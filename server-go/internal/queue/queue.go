package queue

import (
	"context"
	"log/slog"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

// Queue 是任务队列门面（端口自 lib/generation_queue.py）：包装 TaskRepo，承载入队派生执行
// 身份、取消的进程内信号分发、worker 租约，以及在写操作返回之后统一发布任务终态事件。
//
// 终态事件在 repo 调用返回之后发布——repo 的每次写都已提交，此时发布才与「已落库的状态」
// 一致；发布失败不上抛（状态本身已落库，事件只是实时性优化）。

// 租约与轮询参数（与 Python 常量化值同口径）。
const (
	// TaskWorkerLeaseTTLSec 租约 TTL（秒）。
	TaskWorkerLeaseTTLSec = 10.0
	// TaskWorkerHeartbeatSec 心跳间隔（秒）。
	TaskWorkerHeartbeatSec = 3.0
	// TaskPollIntervalSec 空轮询间隔（秒）。
	TaskPollIntervalSec = 1.0
	// DefaultLeaseName 默认租约名。
	DefaultLeaseName = "default"
)

// ProjectLoader 按名读项目文件（*service.ProjectStore 满足）。读不到时返回 error。
type ProjectLoader interface {
	Get(name string) (*domain.ProjectFile, error)
}

// BucketResolver 为入队任务定视频能力桶；返回 nil 表示该任务不定桶。
type BucketResolver func(
	projectName string, project *domain.ProjectFile, taskType string, payload map[string]any, resourceID string,
) *provider.VideoBucket

// TerminalEmitter 发布任务终态事件；nil 时 no-op。
type TerminalEmitter func(events []repository.TerminalEvent)

// CancelCallback 同步请求中断一个运行中的任务，命中返回 true（进程内取消信号）。
type CancelCallback func(taskID string) bool

// Queue 是任务队列门面。
type Queue struct {
	db             *gorm.DB
	projects       ProjectLoader
	bucketResolver BucketResolver
	emitTerminal   TerminalEmitter
	cancelCallback CancelCallback
}

// QueueOption 是门面构造选项。
type QueueOption func(*Queue)

// WithProjectLoader 注入项目读取器（入队派生执行身份用）。
func WithProjectLoader(loader ProjectLoader) QueueOption {
	return func(q *Queue) { q.projects = loader }
}

// WithBucketResolver 注入视频定桶器（reference_video 的逐镜头分流用）。
func WithBucketResolver(resolver BucketResolver) QueueOption {
	return func(q *Queue) { q.bucketResolver = resolver }
}

// WithTerminalEmitter 注入终态事件发布器。
func WithTerminalEmitter(emitter TerminalEmitter) QueueOption {
	return func(q *Queue) { q.emitTerminal = emitter }
}

// WithCancelCallback 注入进程内取消回调。
func WithCancelCallback(cb CancelCallback) QueueOption {
	return func(q *Queue) { q.cancelCallback = cb }
}

// NewQueue 构造队列门面。
func NewQueue(gdb *gorm.DB, opts ...QueueOption) *Queue {
	q := &Queue{db: gdb}
	for _, opt := range opts {
		opt(q)
	}
	return q
}

// SetCancelCallback 后置注入进程内取消回调（worker 启动前设置，让取消 API 能同步投递信号）。
func (q *Queue) SetCancelCallback(cb CancelCallback) { q.cancelCallback = cb }

func (q *Queue) repo() *repository.TaskRepo { return repository.NewTaskRepo(q.db) }

// ----------------------------------------------------------------------
// 入队（含执行身份派生与视频定桶）
// ----------------------------------------------------------------------

// derivedExecution 是入队派生的执行身份；Bucket 仅视频任务非空。
type derivedExecution struct {
	Model  config.ProviderModel
	Bucket *provider.VideoBucket
}

// EnqueueTask 入队单条任务：caller 未传 ProviderID 时主动派生一次，让 claim 走 SQL 池过滤
// 快路径；派生失败留 NULL 走 IS NULL 兜底，由 worker claim 后二次校验。派生成功时视频任务
// 同时把执行 model 锁进 payload，中断续跑据此沿用同一 model。
func (q *Queue) EnqueueTask(ctx context.Context, p repository.EnqueueParams) (repository.EnqueueResult, error) {
	if p.ProviderID == nil {
		derived := q.deriveExecutionModelForEnqueue(ctx, p.ProjectName, p.Payload, p.TaskType, p.MediaType, p.ResourceID)
		if derived != nil {
			providerID := derived.Model.ProviderID
			p.ProviderID = &providerID
			p.Payload = pinVideoExecutionModel(p.Payload, derived.Bucket, derived.Model)
		}
	}
	result, err := q.repo().Enqueue(ctx, p)
	if err != nil {
		return result, err
	}
	if result.Deduped {
		slog.Debug("任务去重", "task_id", result.TaskID)
	} else {
		slog.Info("任务入队", "task_id", result.TaskID, "type", p.TaskType)
	}
	return result, nil
}

// deriveExecutionModelForEnqueue 按项目 + payload 派生该任务的执行身份，视频任务连同定桶
// 结果一并返回。
//
// provider_id 落 task 行供 claim SQL 池过滤；视频任务的完整身份另锁进 payload。与 worker 的
// 解析投影同套逻辑，但失败时返回 nil（不强行回默认供应商）——让任务走 provider_id IS NULL
// 兜底分支，由 worker claim 后二次校验，比硬塞一个可能错误的 provider 安全。
func (q *Queue) deriveExecutionModelForEnqueue(
	ctx context.Context, projectName string, payload map[string]any, taskType, mediaType, resourceID string,
) *derivedExecution {
	isVideo := mediaType == "video" || taskType == "video" || taskType == "reference_video"
	isAudio := mediaType == "audio" || taskType == "tts"

	var project *domain.ProjectFile
	if projectName != "" {
		if q.projects == nil {
			return nil
		}
		loaded, err := q.projects.Get(projectName)
		if err != nil {
			slog.Debug("入队时读取项目失败，留 NULL 由 worker 兜底", "project", projectName, "err", err)
			return nil
		}
		project = loaded
	}

	resolver := config.NewResolver(q.db)
	var (
		resolved config.ProviderModel
		bucket   *provider.VideoBucket
		err      error
	)
	switch {
	case isVideo:
		bucket = q.resolveQueuedBucket(projectName, project, taskType, payload, resourceID)
		resolved, err = resolver.ResolveVideoBackend(ctx, project, payload, bucket)
	case isAudio:
		resolved, err = resolver.ResolveAudioBackend(ctx, project, payload)
	default:
		// image_edit 必然 i2i 且入队即知（唯一例外，见 docs/adr/0001），按 i2i 槽解析；
		// 其余 image 任务 capability 执行时才定，取 t2i 作代表性 provider。
		capability := config.ImageCapT2I
		if taskType == "image_edit" {
			capability = config.ImageCapI2I
		}
		resolved, err = resolver.ResolveImageBackend(ctx, project, payload, capability)
	}
	if err != nil {
		slog.Debug("入队时派生执行身份失败，留 NULL 由 worker 兜底", "task_type", taskType, "err", err)
		return nil
	}
	if resolved.ProviderID == "" {
		return nil
	}
	return &derivedExecution{Model: resolved, Bucket: bucket}
}

// resolveQueuedBucket 定桶：有注入的定桶器就用它，否则用默认（任务类型表 + 参考路线退代表桶）。
func (q *Queue) resolveQueuedBucket(
	projectName string, project *domain.ProjectFile, taskType string, payload map[string]any, resourceID string,
) *provider.VideoBucket {
	if q.bucketResolver != nil {
		return q.bucketResolver(projectName, project, taskType, payload, resourceID)
	}
	return defaultBucketForQueuedTask(taskType)
}

// defaultBucketForQueuedTask 是入队定桶的默认口径。
//
// 图生视频 → i2v；参考生视频按 unit 的参考集分流（有参考图 → r2v、无参考图退化镜头 → i2v）。
// 逐镜头分流依赖剧本 / unit 的加载层，此处退代表桶 r2v——与 Python「剧本 / unit 读不到时回退
// 代表桶」同口径，只影响 claim 池过滤与限流槽路由的精度，执行层会按解析后的实际参考图独立
// 精确定桶。表外任务类型返回 nil（不定桶）。
func defaultBucketForQueuedTask(taskType string) *provider.VideoBucket {
	bucket, ok := provider.VideoBucketForTaskType(taskType)
	if !ok {
		return nil
	}
	return &bucket
}

// pinVideoExecutionModel 把入队解析出的执行身份锁进视频任务 payload 的能力桶键，返回新 payload
// （不改调用方的 map）。
//
// 锁定的是「任务真正会执行的 model」，执行与中断续跑据此走同一身份：task 行只存 provider_id，
// 锁不住 model（docs/adr/0054「不静默换模型」）。桶键与复合值形态和解析侧 payload 层同源。
// 非视频任务与不定桶的任务、以及解析不出 model 时原样返回。
func pinVideoExecutionModel(
	payload map[string]any, bucket *provider.VideoBucket, model config.ProviderModel,
) map[string]any {
	if bucket == nil || model.ModelID == "" {
		return payload
	}
	out := make(map[string]any, len(payload)+1)
	for key, value := range payload {
		out[key] = value
	}
	out["video_provider_"+string(*bucket)] = model.PairKey()
	return out
}

// MediaLane 返回任务所属媒体泳道（image / video / audio）。
//
// 单一映射点——解析分流与健康账的泳道键共用它：两条路径对「同一任务属于哪条 lane」必须给出
// 同一个答案，否则熔断记在一条泳道上、限流却按另一条走。reference_video 等 task_type 与
// media_type="video" 同属 video lane，tts 归 audio。
func MediaLane(mediaType, taskType string) provider.MediaType {
	if mediaType == "video" || taskType == "video" || taskType == "reference_video" {
		return provider.MediaVideo
	}
	if mediaType == "audio" || taskType == "tts" {
		return provider.MediaAudio
	}
	return provider.MediaImage
}

// ----------------------------------------------------------------------
// 查询与认领
// ----------------------------------------------------------------------

// GetActiveTasksForResources 列出同一去重键下仍活跃的任务。
func (q *Queue) GetActiveTasksForResources(
	ctx context.Context, projectName, taskType string, resourceIDs []string, scriptFile, resourceType *string,
) ([]repository.TaskData, error) {
	return q.repo().GetActiveTasksForResources(ctx, projectName, taskType, resourceIDs, scriptFile, resourceType)
}

// ClaimNextTask 领取下一个 queued 任务（FIFO，黑名单排除池满 provider）。
func (q *Queue) ClaimNextTask(
	ctx context.Context, mediaType string, poolFullProviders []string,
) (*repository.TaskData, error) {
	task, err := q.repo().ClaimNext(ctx, mediaType, poolFullProviders)
	if err != nil {
		return nil, err
	}
	if task != nil {
		slog.Debug("任务被领取", "task_id", task.TaskID)
	}
	return task, nil
}

// RequeueRunningTasks 救援扳手：批量把 running 任务回队。
func (q *Queue) RequeueRunningTasks(ctx context.Context, limit int) (int64, error) {
	recovered, err := q.repo().RequeueRunning(ctx, limit)
	if err != nil {
		return 0, err
	}
	if recovered > 0 {
		slog.Warn("回收 running 任务", "count", recovered)
	}
	return recovered, nil
}

// ListOrphanTasksOnStart 列出启动时需要自愈的 running + cancelling 任务。
func (q *Queue) ListOrphanTasksOnStart(ctx context.Context) ([]repository.TaskData, error) {
	return q.repo().ListOrphanTasksOnStart(ctx)
}

// RequeueTask 把单条 running 任务放回 queued（worker 池满 / 熔断回队）。
func (q *Queue) RequeueTask(ctx context.Context, taskID string) (int64, error) {
	return q.repo().RequeueTask(ctx, taskID)
}

// GetTask 按 ID 取任务；不存在返回 nil。
func (q *Queue) GetTask(ctx context.Context, taskID string) (*repository.TaskData, error) {
	return q.repo().Get(ctx, taskID)
}

// ListTasks 分页列出任务。
func (q *Queue) ListTasks(ctx context.Context, p repository.ListTasksParams) (repository.TaskListResult, error) {
	return q.repo().ListTasks(ctx, p)
}

// GetTaskStats 聚合任务统计。
func (q *Queue) GetTaskStats(ctx context.Context, projectName *string) (repository.TaskStats, error) {
	return q.repo().GetStats(ctx, projectName)
}

// ----------------------------------------------------------------------
// 执行期投影回写
// ----------------------------------------------------------------------

// PersistProviderJobID 回写 provider_job_id（+可选提交端点）。
func (q *Queue) PersistProviderJobID(ctx context.Context, taskID, jobID string, endpoint *string) error {
	return q.repo().PersistProviderJobID(ctx, taskID, jobID, endpoint)
}

// PersistAPICallID 把 api_calls.id 写回 payload，供 resume 精准翻 pending。
func (q *Queue) PersistAPICallID(ctx context.Context, taskID string, callID int64) error {
	return q.repo().PersistAPICallID(ctx, taskID, callID)
}

// PersistEffectiveDuration 把实际申请秒数写回 payload["duration_seconds"]。
func (q *Queue) PersistEffectiveDuration(ctx context.Context, taskID string, durationSeconds int64) error {
	return q.repo().PersistEffectiveDuration(ctx, taskID, durationSeconds)
}

// PersistExecutionProviderID 把执行期实际解析出的 provider 写回 provider_id 列。
func (q *Queue) PersistExecutionProviderID(ctx context.Context, taskID, providerID string) error {
	return q.repo().PersistExecutionProviderID(ctx, taskID, providerID)
}

// PersistExecutionIdentity 执行前把实际执行身份写回投影列与 payload 锁定键。
//
// 入队按 unit 声明近似定桶锁定身份，执行按解析后的实际参考图定桶，二者分裂时陈旧桶键在 resume
// 解析里优先于 provider_id 列注入，只刷新列锁不住轮询 backend——故连同锁定键一起改写：清掉
// 其它桶的键、把实际执行身份写进实际桶的键。非分裂场景等值改写，幂等。
func (q *Queue) PersistExecutionIdentity(
	ctx context.Context, taskID string, model config.ProviderModel, capability provider.VideoBucket,
) error {
	patch := make(map[string]any, len(provider.VideoBucketOrder))
	for _, bucket := range provider.VideoBucketOrder {
		if bucket != capability {
			patch["video_provider_"+string(bucket)] = nil
		}
	}
	patch["video_provider_"+string(capability)] = model.PairKey()
	return q.repo().PersistExecutionIdentity(ctx, taskID, model.ProviderID, patch)
}

// ----------------------------------------------------------------------
// 终态写回
// ----------------------------------------------------------------------

// MarkTaskSucceeded 把 running 任务翻 succeeded，返回受影响行数（0 = 已被外部翻状态，
// worker 走 0-rows-cancelled 协议）。
func (q *Queue) MarkTaskSucceeded(ctx context.Context, taskID string, result map[string]any) (int64, error) {
	affected, event, err := q.repo().MarkSucceeded(ctx, taskID, result)
	if err != nil {
		return 0, err
	}
	if affected > 0 {
		slog.Info("任务成功", "task_id", taskID)
	} else {
		slog.Info("mark_succeeded 0 行（已被外部翻状态）", "task_id", taskID)
	}
	if event != nil {
		q.emitTerminalEvents([]repository.TerminalEvent{*event})
	}
	return affected, nil
}

// MarkTaskFailed 把 running 任务翻 failed 并级联下游，返回受影响行数。
func (q *Queue) MarkTaskFailed(ctx context.Context, taskID, errorMessage string) (int64, error) {
	affected, events, err := q.repo().MarkFailed(ctx, taskID, errorMessage)
	if err != nil {
		return 0, err
	}
	if affected > 0 {
		slog.Warn("任务失败", "task_id", taskID, "error", truncate(errorMessage, 200))
	} else {
		slog.Info("mark_failed 0 行（已被外部翻状态）", "task_id", taskID)
	}
	q.emitTerminalEvents(events)
	return affected, nil
}

// MarkTaskCancelled 是 worker finally 的 0-rows-cancelled 协议兜底入口：把
// queued/cancelling/running 任务落 cancelled，并级联下游。级联打到的 running 下游经
// worker callback 同步分发进程内取消信号。
func (q *Queue) MarkTaskCancelled(ctx context.Context, taskID, cancelledBy string) (int64, error) {
	result, err := q.repo().FinalizeCancelled(ctx, taskID, cancelledBy)
	if err != nil {
		return 0, err
	}
	q.dispatchCancelSignals(result.Cancelling, "finalize cascade")
	if result.Rows > 0 {
		// FinalizeCancelled 不返回终态事件；补一次读拿 project / task_type 重建事件。
		if task, err := q.repo().Get(ctx, taskID); err == nil && task != nil {
			q.emitTerminalEvents(terminalEventsFromTaskData([]repository.TaskData{*task}))
		} else if err != nil {
			slog.Warn("读取已取消任务失败，跳过终态事件", "task_id", taskID, "err", err)
		}
	}
	return result.Rows, nil
}

// ----------------------------------------------------------------------
// 取消
// ----------------------------------------------------------------------

// CancelTask 按状态分发取消单个任务，并同步通知仍在 worker 中运行的任务。
//
// Repository 返回 cancelling 意图列表 → 门面同步分发进程内信号。callback 不命中（task 已不在
// inflight）是 best-effort 失败：DB 已是 cancelling，worker finally 走最终取消兜底接住。
func (q *Queue) CancelTask(ctx context.Context, taskID string) (repository.CancelResult, error) {
	result, err := q.repo().CancelTask(ctx, taskID)
	if err != nil {
		return result, err
	}
	q.dispatchCancelSignals(result.Cancelling, "cancel")
	q.emitTerminalEvents(terminalEventsFromTaskData(result.Cancelled))
	return result, nil
}

// CancelEpisodeTasks 取消指定分集的活动任务，并同步通知仍在 worker 中运行的任务。
func (q *Queue) CancelEpisodeTasks(ctx context.Context, projectName, scriptFile string) (repository.CancelResult, error) {
	result, err := q.repo().CancelEpisodeTasks(ctx, projectName, scriptFile)
	if err != nil {
		return result, err
	}
	q.dispatchCancelSignals(result.Cancelling, "episode delete")
	q.emitTerminalEvents(terminalEventsFromTaskData(result.Cancelled))
	return result, nil
}

// CancelAllActive 停止项目内全部活动任务，并在 worker 可用时同步通知它中断运行中调用。
//
// worker 回调不存在或 lease 已过期时，running 任务无法等待进程内 finally 收口，直接落
// cancelled；否则停服务或 worker 崩溃后的僵尸任务会永久停在 cancelling。
func (q *Queue) CancelAllActive(ctx context.Context, projectName string) (repository.CancelResult, error) {
	repo := q.repo()
	result, err := repo.CancelAllActive(ctx, projectName)
	if err != nil {
		return result, err
	}
	cancelling := append([]string(nil), result.Cancelling...)
	if len(cancelling) > 0 {
		online := false
		if q.cancelCallback != nil {
			if online, err = repo.IsWorkerOnline(ctx, DefaultLeaseName); err != nil {
				// 探测失败按「离线」处理：宁可多兜底落终态，也不留僵尸在 cancelling。
				slog.Warn("探测 worker 在线状态失败，按离线兜底", "err", err)
				online = false
			}
		}
		if q.cancelCallback == nil || !online {
			finalized, err := repo.FinalizeCancelling(ctx, cancelling, "user")
			if err != nil {
				return result, err
			}
			result.Cancelled = append(result.Cancelled, finalized...)
			result.Cancelling = []string{}
			cancelling = nil
		}
	}
	q.dispatchCancelSignals(cancelling, "cancel all")
	q.emitTerminalEvents(terminalEventsFromTaskData(result.Cancelled))
	return result, nil
}

// GetCancelPreview 预览取消某个任务的影响范围。
func (q *Queue) GetCancelPreview(ctx context.Context, taskID string) (repository.CancelPreview, error) {
	return q.repo().GetCancelPreview(ctx, taskID)
}

// GetCancelAllPreview 返回项目中当前活动任务数量。
func (q *Queue) GetCancelAllPreview(ctx context.Context, projectName string) (int64, error) {
	return q.repo().GetCancelAllPreview(ctx, projectName)
}

// ----------------------------------------------------------------------
// worker 租约
// ----------------------------------------------------------------------

// AcquireOrRenewWorkerLease 获取或续约 worker 租约，返回是否持有。
func (q *Queue) AcquireOrRenewWorkerLease(ctx context.Context, name, ownerID string, ttl float64) (bool, error) {
	return q.repo().AcquireOrRenewLease(ctx, name, ownerID, ttl)
}

// ReleaseWorkerLease 释放租约（仅 owner 匹配时生效）。
func (q *Queue) ReleaseWorkerLease(ctx context.Context, name, ownerID string) error {
	return q.repo().ReleaseLease(ctx, name, ownerID)
}

// IsWorkerOnline 判断默认租约是否在线。
func (q *Queue) IsWorkerOnline(ctx context.Context, name string) (bool, error) {
	return q.repo().IsWorkerOnline(ctx, name)
}

// GetWorkerLease 返回租约详情；无租约行返回 nil。
func (q *Queue) GetWorkerLease(ctx context.Context, name string) (*repository.WorkerLeaseInfo, error) {
	return q.repo().GetWorkerLease(ctx, name)
}

// ----------------------------------------------------------------------
// 内部
// ----------------------------------------------------------------------

// dispatchCancelSignals 同步分发进程内取消信号：callback 必须同步调用（worker 侧是 O(1) 的
// context 取消），否则 API 立刻返回但信号延迟到下次调度，破坏秒级响应。单个回调 panic 不能
// 拖垮其余信号分发，逐条 recover。
func (q *Queue) dispatchCancelSignals(taskIDs []string, reason string) {
	cb := q.cancelCallback
	if cb == nil {
		return
	}
	for _, taskID := range taskIDs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("worker 取消回调 panic", "task_id", taskID, "reason", reason, "panic", r)
				}
			}()
			cb(taskID)
		}()
	}
}

func (q *Queue) emitTerminalEvents(events []repository.TerminalEvent) {
	if q.emitTerminal == nil || len(events) == 0 {
		return
	}
	q.emitTerminal(events)
}

// terminalEventsFromTaskData 从终态任务投影构造事件；非终态任务被过滤。
func terminalEventsFromTaskData(tasks []repository.TaskData) []repository.TerminalEvent {
	events := make([]repository.TerminalEvent, 0, len(tasks))
	for _, task := range tasks {
		if _, ok := repository.TerminalTaskStatuses[task.Status]; !ok {
			continue
		}
		events = append(events, repository.TerminalEvent{
			TaskID:      task.TaskID,
			ProjectName: task.ProjectName,
			Status:      task.Status,
			TaskType:    task.TaskType,
		})
	}
	return events
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
