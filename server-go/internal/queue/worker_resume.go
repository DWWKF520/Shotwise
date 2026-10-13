package queue

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

// 重启自愈与断点续传（端口自 lib/generation_worker.py 的孤儿处理与 resume dispatcher）。
//
// 原则——**不主动产生额外扣费**：只要 worker 不能确认能接续供应商已收单的 job，就把孤儿标记
// 为失败丢弃，绝不重新提交（ADR 0007）。

// 不实现 VideoBackend.ResumeVideo 的视频 provider。orphan handler 据此把这些 provider 的
// running 孤儿标记为 [resume_unsupported] 失败，而非主动 requeue 重跑——避免对已提交给供应商
// 的请求二次扣费（Grok 同步型无 job_id；Vidu 的 generate 内联 poll 未抽出独立 resume）。
// 新增不支持 resume 的 backend 时同步在这里登记。
var nonResumableVideoProviders = map[string]bool{
	"grok": true,
	"vidu": true,
}

// ResumeExecutor 是重启自愈的执行入口：只 poll + download，绝不重新 submit（ADR 0007）。
//
// 身份锁定：视频任务的 provider / model 由入队时锁进 payload 能力桶键的执行身份负责，
// 执行层据此按提交时的身份而非当前项目配置解析 backend——否则任务提交后到重启前项目
// provider 配置切换，会拿旧 job_id 去新 provider 轮询，可恢复任务被误判失败。
type ResumeExecutor interface {
	ResumeVideo(ctx context.Context, task *repository.TaskData, jobID string) (map[string]any, error)
}

// ResumeExecutor 的三个可识别终止原因；执行层用 errors.Is 包装返回。
var (
	// ErrResumeUnsupported backend 未实现 resume。
	ErrResumeUnsupported = errors.New("resume unsupported")
	// ErrResumeEndpointChanged 提交端点与当前解析出的端点不一致（协议维度换了）。
	ErrResumeEndpointChanged = errors.New("resume endpoint changed")
	// ErrResumeExpired 供应商侧 job 已过期，无法接续。
	ErrResumeExpired = errors.New("resume expired")
)

// WithResumeExecutor 注入重启自愈执行器。
func WithResumeExecutor(exec ResumeExecutor) WorkerOption {
	return func(w *Worker) { w.resumeExecutor = exec }
}

// orphanMediaType 是孤儿任务的媒体泳道：优先取持久化的 media_type，脏值一律按 video 处理。
//
// 与 MediaLane 的差异：MediaLane 把脏值归 image，这里按 Python 口径归 video——孤儿扫描
// 只把 video 交给 resume 路径，脏 media_type 的行按视频记账避免健康账键与占用台账分裂。
func orphanMediaType(task *repository.TaskData) provider.MediaType {
	if task.MediaType == "image" {
		return provider.MediaImage
	}
	if task.MediaType == "audio" {
		return provider.MediaAudio
	}
	if task.MediaType != "" {
		return provider.MediaVideo
	}
	switch task.TaskType {
	case "tts":
		return provider.MediaAudio
	default:
		return provider.MediaVideo
	}
}

// handleOrphanTasksOnStart 扫 running + cancelling 孤儿，按「是否可安全 resume」分流。
//
// 启动期 fast path 只做终结类处理，立刻返回；可 resume 的视频孤儿交后台 dispatcher 分批处理，
// 避免 N 个长轮询孤儿把启动期阻塞数十分钟。
func (w *Worker) handleOrphanTasksOnStart(ctx context.Context) {
	orphans, err := w.queue.ListOrphanTasksOnStart(ctx)
	if err != nil {
		slog.Warn("列出启动孤儿失败", "err", err)
		return
	}
	if len(orphans) == 0 {
		return
	}
	slog.Info("开始扫孤儿", "count", len(orphans), "lease_ttl", w.leaseTTL)

	// self-active 防 self-preemption：lease flap 后重扫时 DB 里的 running 可能包含本进程仍
	// inflight 的 task。不排除会把还在跑的任务标失败，或让同一 provider job 被并发 resume
	// （违反 ADR 0007 红线）。
	selfActive := w.slots.ActiveTaskIDs()

	resumable := map[string][]*repository.TaskData{}

	for i := range orphans {
		task := &orphans[i]
		if selfActive[task.TaskID] {
			slog.Info("孤儿扫到本进程仍 active 的 task，跳过避免 self-preemption", "task_id", task.TaskID)
			continue
		}
		if task.Status == repository.TaskStatusCancelling {
			w.finalizeCancelled(ctx, task.TaskID)
			slog.Info("孤儿 cancelling → cancelled", "task_id", task.TaskID)
			continue
		}

		media := orphanMediaType(task)
		if media == provider.MediaImage {
			// image 不持久化 job_id 也无 resume 入口——已提交的请求无法回收，重跑=双重扣费。
			slog.Warn("孤儿 image running → [restart_lost]", "task_id", task.TaskID)
			w.markFailedResume(ctx, task, "restart_lost_image", nil)
			continue
		}
		if media == provider.MediaAudio {
			slog.Warn("孤儿 audio running → [restart_lost]", "task_id", task.TaskID)
			w.markFailedResume(ctx, task, "restart_lost_audio", nil)
			continue
		}

		// video：优先用持久化的 provider_id——否则项目配置在重启前后切换时，重解析可能把
		// 原本 Grok/Vidu 孤儿误判成可 resume，或把可 resume 任务路由到错池。
		providerID := ""
		if task.ProviderID != nil {
			providerID = *task.ProviderID
		}
		if providerID == "" {
			providerID = w.extractProviderID(ctx, task)
		}
		if nonResumableVideoProviders[providerID] {
			slog.Warn("孤儿 video running（provider 不支持 resume）→ [resume_unsupported]",
				"provider", providerID, "task_id", task.TaskID)
			w.markFailedResume(ctx, task, "resume_unsupported_provider", map[string]any{"provider_id": providerID})
			continue
		}
		if task.ProviderJobID == nil || *task.ProviderJobID == "" {
			slog.Warn("孤儿 video running 无 job_id → [restart_lost]", "task_id", task.TaskID)
			w.markFailedResume(ctx, task, "restart_lost_no_job_id", nil)
			continue
		}

		task.ProviderID = &providerID
		resumable[providerID] = append(resumable[providerID], task)
	}

	if len(resumable) == 0 {
		slog.Info("孤儿扫描完成，无可 resume 任务")
		return
	}
	total := 0
	for _, tasks := range resumable {
		total += len(tasks)
	}
	slog.Info("孤儿扫描 fast path 完成，可 resume video 任务交后台分批 dispatch", "count", total)
	w.orphanWG.Add(1)
	go w.dispatchResumeOrphans(ctx, resumable)
}

// dispatchResumeOrphans 是后台 dispatcher：按 provider 分桶并发，各桶内受 video 容量约束分批。
func (w *Worker) dispatchResumeOrphans(ctx context.Context, resumable map[string][]*repository.TaskData) {
	defer w.orphanWG.Done()
	if w.stopped() {
		return
	}
	var sub sync.WaitGroup
	for providerID, tasks := range resumable {
		sub.Add(1)
		go func(providerID string, tasks []*repository.TaskData) {
			defer sub.Done()
			w.dispatchProviderBucket(ctx, providerID, tasks)
		}(providerID, tasks)
	}
	sub.Wait()
	slog.Info("孤儿后台 dispatcher 完成")
}

// dispatchProviderBucket 跑同 provider 桶内的 resume 任务，用信号量精确卡并发。
//
//   - cap <= 0：reload 一次兜底（DB 加载可能晚于首次孤儿扫描）后仍 <= 0 → fail-fast
//     mark_failed[resume_unsupported_capacity_zero]，绝不进容量 0 的信号量死锁。
//   - 子任务由父 goroutine 同步预先以 PENDING 登记（避免异步调度尚未触发时 has_room 误判）；
//     拿到信号量后 promote 成 INFLIGHT；finally release。
//   - 信号量容量在 dispatch 顶部读一次定型：reload 期间改的容量表不影响本批 dispatch 上限。
func (w *Worker) dispatchProviderBucket(ctx context.Context, providerID string, tasks []*repository.TaskData) {
	cap := w.capacity.Get(providerID, provider.MediaVideo)
	if cap <= 0 {
		w.ReloadLimits(ctx)
		cap = w.capacity.Get(providerID, provider.MediaVideo)
	}
	if cap <= 0 {
		for _, task := range tasks {
			w.markFailedResume(ctx, task, "resume_unsupported_capacity_zero", map[string]any{"provider_id": providerID})
		}
		return
	}

	sem := make(chan struct{}, cap)
	var sub sync.WaitGroup
	for _, task := range tasks {
		if w.stopped() {
			break
		}
		taskCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		handle := newRunHandle(cancel)
		// 先同步登记 PENDING，再起 goroutine：避免「goroutine 尚未被调度时 has_room 看占用=0
		// 误判可有容量」的瞬时竞态。
		w.slots.Register(providerID, provider.MediaVideo, task.TaskID, handle, true)
		w.registerCancel(task.TaskID, cancel)
		sub.Add(1)
		go func(task *repository.TaskData, handle *runHandle, taskCtx context.Context) {
			defer sub.Done()
			defer cancel()
			acquired := false
			defer func() {
				if acquired {
					<-sem
				}
				w.unregisterCancel(task.TaskID)
				w.slots.Release(providerID, provider.MediaVideo, task.TaskID)
			}()
			select {
			case sem <- struct{}{}:
				acquired = true
			case <-w.stopCh:
				return
			}
			if w.stopped() {
				return
			}
			w.slots.Promote(providerID, provider.MediaVideo, task.TaskID)
			slog.Info("已派发 resume video orphan", "task_id", task.TaskID, "provider", providerID)
			handle.setErr(w.processResumeTask(taskCtx, task))
			handle.finish()
		}(task, handle, taskCtx)
	}
	sub.Wait()
}

// markFailedResume 落 resume 路径的失败终态；0 行说明外部已翻 cancelling，补落 cancelled。
func (w *Worker) markFailedResume(ctx context.Context, task *repository.TaskData, code string, params map[string]any) {
	w.markFailed(ctx, task.TaskID, w.encodeFailure(code, params))
}

// processResumeTask 重启自愈入口：直接调 resume executor，绕过常规生成流水线。
func (w *Worker) processResumeTask(ctx context.Context, task *repository.TaskData) error {
	taskID := task.TaskID
	jobID := ""
	if task.ProviderJobID != nil {
		jobID = *task.ProviderJobID
	}
	if jobID == "" {
		// 防御：本不该被派发到这里（扫描期已 mark_failed [restart_lost]）。
		w.markFailedResume(ctx, task, "restart_lost_resume_no_job_id", nil)
		return nil
	}

	// 非视频媒体退到把持久化的 provider 注入 payload["image_provider"]（只锁 provider、锁不
	// 住 model）。孤儿扫描只把 video 交到这里，该分支只在 media_type 为脏数据时可达。
	isVideo := task.MediaType == "video" || task.TaskType == "video" || task.TaskType == "reference_video"
	if task.ProviderID != nil && *task.ProviderID != "" && !isVideo {
		if task.Payload == nil {
			task.Payload = map[string]any{}
		}
		task.Payload["image_provider"] = *task.ProviderID
	}

	providerID := w.extractProviderID(ctx, task)
	slog.Info("重启自愈处理任务", "task_id", taskID, "type", task.TaskType, "provider", providerID, "job", jobID)

	if w.resumeExecutor == nil {
		// 未注入 resume 执行层：显式拒绝而非静默放行（AGENTS「依赖沙箱内才有的能力需显式拒绝」）。
		slog.Warn("未注入 resume 执行器，任务标记失败", "task_id", taskID)
		w.markFailedResume(ctx, task, "resume_unsupported_detail", map[string]any{"detail": "resume executor not configured"})
		return nil
	}

	result, err := w.resumeExecutor.ResumeVideo(ctx, task, jobID)
	if err != nil {
		if ctx.Err() != nil {
			w.settleCancelledLedgerCall(ctx, task)
			w.finalizeCancelled(ctx, taskID)
			return err
		}
		switch {
		case errors.Is(err, ErrResumeUnsupported):
			slog.Warn("resume 不支持", "task_id", taskID, "err", err)
			w.markFailedResume(ctx, task, "resume_unsupported_detail", map[string]any{"detail": err.Error()})
		case errors.Is(err, ErrResumeEndpointChanged):
			slog.Warn("resume endpoint 已变更", "task_id", taskID, "err", err)
			w.markFailedResume(ctx, task, "resume_endpoint_changed_detail", map[string]any{"detail": err.Error()})
		case errors.Is(err, ErrResumeExpired):
			slog.Warn("resume 已过期", "task_id", taskID, "err", err)
			w.markFailedResume(ctx, task, "resume_expired_detail", map[string]any{"detail": err.Error()})
		default:
			slog.Error("resume 失败", "task_id", taskID, "provider", providerID, "err", err)
			// 泳道固定 video：这条路径只服务视频孤儿，脏数据行也照 video 记账，避免健康账键
			// 与占用台账分裂。
			w.health.RecordFailure(providerID, provider.MediaVideo, w.isTransient(err))
			w.markFailed(ctx, taskID, w.encodeTaskError(err))
		}
		return err
	}

	if w.indexMedia != nil {
		result = w.indexMedia(ctx, task, result)
	}
	rows, err := w.queue.MarkTaskSucceeded(writeContext(ctx), taskID, result)
	if err != nil {
		slog.Error("重启自愈标记成功失败", "task_id", taskID, "err", err)
		return err
	}
	if rows == 0 {
		w.finalizeCancelled(ctx, taskID)
		return nil
	}
	w.health.RecordSuccess(providerID, provider.MediaVideo)
	slog.Info("重启自愈完成", "task_id", taskID)
	return nil
}
