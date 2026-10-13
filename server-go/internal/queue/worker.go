package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

// 生成任务后台调度器（端口自 lib/generation_worker.py::GenerationWorker）。
//
// 不是一把 Semaphore，而是「容量表 + 占用台账 + 健康账」三件套：容量表是用户配置的并发上限，
// 占用台账是运行时记账，健康账在上限之上做熔断折减。调度粒度是 (provider_id, media_type) 泳道。
//
// 与 Python 的关键差异：Python 侧单线程事件循环天然串行化所有状态变更；Go 侧主循环、执行
// goroutine、API 取消回调分属不同 goroutine，故容量表 / 占用台账 / 健康账内部各自加锁。
// asyncio.shield 的「DB 写入不被取消打断」在 Go 用不带 cancel 的 context 表达。

// DefaultProvider 是解析不出供应商时的限流路由兜底（不落 provider_id 列，仅供泳道路由）。
const DefaultProvider = "gemini-aistudio"

// orphanRescanLeaseLostMult 是「lease 丢失时长」阈值倍数：丢失超过 lease_ttl × 该值才认为是
// 真切换 owner（另一进程曾持 lease 并写入新孤儿），短 flap（续约抖动）不触发重扫。
const orphanRescanLeaseLostMult = 3

// laneOrder 是认领遍历的固定顺序。
var laneOrder = []provider.MediaType{provider.MediaImage, provider.MediaVideo, provider.MediaAudio}

// TaskExecutor 执行一个已认领的生成任务，返回写回 task.result_json 的结果。
//
// 执行身份（provider / model）由 payload 锁定键与 provider_id 列决定，执行层自行解析；
// worker 只负责调度与终态收口。
type TaskExecutor interface {
	Execute(ctx context.Context, task *repository.TaskData) (map[string]any, error)
}

// ProviderExtractor 认领后派生 provider_id，仅供限流槽路由（解析链的薄投影）。
//
// 解析失败回退 DefaultProvider——绝不冒泡阻断认领循环。
type ProviderExtractor func(ctx context.Context, task *repository.TaskData) string

// MediaIndexer 任务产出后登记 MediaAsset；best-effort，失败不得改变生成结果。
type MediaIndexer func(ctx context.Context, task *repository.TaskData, result map[string]any) map[string]any

// Preflight 执行前复检引用资产（准入与执行之间 project.json / 剧本漂移的窗口）。
type Preflight func(ctx context.Context, task *repository.TaskData) error

// FailureEncoder 把失败 code + 参数编码为落库的 error_message（机器码落库，读侧渲染）。
type FailureEncoder func(code string, params map[string]any) string

// TaskErrorEncoder 把执行异常编码为落库的 error_message。
type TaskErrorEncoder func(err error) string

// TransientClassifier 判定异常是否属于上游瞬态失败；只有瞬态失败计健康账。
type TransientClassifier func(err error) bool

// InterruptedSettler 收口中断的记账行（启动收口 + 取消收口）。
type InterruptedSettler interface {
	// SettleInterruptedCalls 把 startedBefore 之前仍 pending 的记账行翻失败，返回条数。
	SettleInterruptedCalls(ctx context.Context, startedBefore time.Time) (int, error)
	// ResumeFailed 按 api_call_id 幂等翻一条 pending 记账行为失败（取消收口，不重扣费）。
	ResumeFailed(ctx context.Context, callID int64) error
}

// Worker 是任务队列的后台调度器。
type Worker struct {
	queue     *Queue
	leaseName string
	ownerID   string

	capacity *CapacityTable
	slots    *SlotTable
	health   *ProviderHealthTable

	leaseTTL          float64
	heartbeatInterval float64
	pollInterval      float64

	executor        TaskExecutor
	resumeExecutor  ResumeExecutor
	extractProvider ProviderExtractor
	indexMedia      MediaIndexer
	preflight       Preflight
	encodeFailure   FailureEncoder
	encodeTaskError TaskErrorEncoder
	isTransient     TransientClassifier
	settler         InterruptedSettler

	// constructedAt 是启动收口的时间下界：本进程构造之前发起的 pending 调用不可能由本进程
	// 接续（单进程 server+worker 捆绑），据此断定它们已中断。
	constructedAt time.Time

	stopCh   chan struct{}
	stopOnce sync.Once

	execWG   sync.WaitGroup // 在途执行
	orphanWG sync.WaitGroup // 孤儿后台 dispatcher

	// runHandle 保护 cancelFuncs：API 侧 RequestCancel 与主循环 dispatch / drain 并发访问。
	runHandle   sync.Mutex
	cancelFuncs map[string]context.CancelFunc

	ownsLease         atomic.Bool
	orphanHandledOnce bool
	startupSettled    bool
	leaseLostAt       time.Time
}

// WorkerOption 是 worker 构造选项。
type WorkerOption func(*Worker)

// WithLeaseName 覆盖租约名。
func WithLeaseName(name string) WorkerOption { return func(w *Worker) { w.leaseName = name } }

// WithCapacityTable 注入容量表。
func WithCapacityTable(table *CapacityTable) WorkerOption {
	return func(w *Worker) { w.capacity = table }
}

// WithSlotTable 注入占用台账。
func WithSlotTable(slots *SlotTable) WorkerOption { return func(w *Worker) { w.slots = slots } }

// WithHealthTable 注入健康账。
func WithHealthTable(health *ProviderHealthTable) WorkerOption {
	return func(w *Worker) { w.health = health }
}

// WithProviderExtractor 注入限流路由用的 provider 解析投影。
func WithProviderExtractor(fn ProviderExtractor) WorkerOption {
	return func(w *Worker) { w.extractProvider = fn }
}

// WithMediaIndexer 注入 MediaAsset 登记器。
func WithMediaIndexer(fn MediaIndexer) WorkerOption { return func(w *Worker) { w.indexMedia = fn } }

// WithPreflight 注入执行前资产复检。
func WithPreflight(fn Preflight) WorkerOption { return func(w *Worker) { w.preflight = fn } }

// WithFailureEncoder 注入失败 code 编码器。
func WithFailureEncoder(fn FailureEncoder) WorkerOption {
	return func(w *Worker) { w.encodeFailure = fn }
}

// WithTaskErrorEncoder 注入执行异常编码器。
func WithTaskErrorEncoder(fn TaskErrorEncoder) WorkerOption {
	return func(w *Worker) { w.encodeTaskError = fn }
}

// WithTransientClassifier 注入瞬态失败判定器。
func WithTransientClassifier(fn TransientClassifier) WorkerOption {
	return func(w *Worker) { w.isTransient = fn }
}

// WithInterruptedSettler 注入中断记账收口器。
func WithInterruptedSettler(s InterruptedSettler) WorkerOption {
	return func(w *Worker) { w.settler = s }
}

// NewWorker 构造 worker。executor 为必填——没有执行层的 worker 无法处理任何任务。
func NewWorker(q *Queue, executor TaskExecutor, opts ...WorkerOption) *Worker {
	w := &Worker{
		queue:             q,
		leaseName:         DefaultLeaseName,
		ownerID:           "worker-" + repository.NewTaskID()[:10],
		capacity:          CapacityTableFromEnv(),
		slots:             NewSlotTable(),
		health:            NewProviderHealthTable(HealthSettingsFromEnv(), nil),
		executor:          executor,
		encodeFailure:     defaultFailureEncoder,
		encodeTaskError:   func(err error) string { return err.Error() },
		isTransient:       func(error) bool { return false },
		constructedAt:     time.Now().UTC(),
		stopCh:            make(chan struct{}),
		heartbeatInterval: TaskWorkerHeartbeatSec,
		pollInterval:      TaskPollIntervalSec,
	}
	// 构造时做下界 clamp（租约过短会让心跳追不上过期）。
	w.leaseTTL = max(1.0, TaskWorkerLeaseTTLSec)
	w.heartbeatInterval = max(0.5, w.heartbeatInterval)
	w.pollInterval = max(0.1, w.pollInterval)
	for _, opt := range opts {
		opt(w)
	}
	slog.Info("Worker 初始容量表", "limits", w.capacity.Limits())
	return w
}

// defaultFailureEncoder 是 `[code] {json params}` 的兜底编码；params 为空时只留 code。
func defaultFailureEncoder(code string, params map[string]any) string {
	if len(params) == 0 {
		return "[" + code + "]"
	}
	payload := make(map[string]any, len(params))
	for k, v := range params {
		payload[k] = v
	}
	return "[" + code + "] " + mustJSONString(payload)
}

// ----------------------------------------------------------------------
// 生命周期
// ----------------------------------------------------------------------

// Run 跑主循环直到 Stop 或 ctx 取消；退出前等在途执行收尾。阻塞，调用方自行起 goroutine。
func (w *Worker) Run(ctx context.Context) {
	slog.Info("worker 主循环启动", "owner", w.ownerID, "lease", w.leaseName)
	defer w.shutdown(ctx)
	for {
		if w.stopped() || ctx.Err() != nil {
			break
		}
		w.tick(ctx)
	}
}

// tick 是主循环的一圈：续约 → drain → 孤儿处理 → 认领 → 休眠。
func (w *Worker) tick(ctx context.Context) {
	hadLease := w.ownsLease.Load()
	owns := false
	acquired, err := w.queue.AcquireOrRenewWorkerLease(ctx, w.leaseName, w.ownerID, w.leaseTTL)
	if err != nil {
		slog.Warn("续约 worker lease 失败", "owner", w.ownerID, "err", err)
	} else {
		owns = acquired
	}
	w.ownsLease.Store(owns)

	if owns && !hadLease {
		slog.Info("获得 worker lease", "owner", w.ownerID)
	}
	if hadLease && !owns {
		slog.Warn("失去 worker lease", "owner", w.ownerID)
	}

	w.drainFinished(ctx)

	if hadLease && !owns && w.leaseLostAt.IsZero() {
		w.leaseLostAt = time.Now()
	}
	if owns && !w.leaseLostAt.IsZero() {
		lost := time.Since(w.leaseLostAt).Seconds()
		if lost > w.leaseTTL*orphanRescanLeaseLostMult {
			slog.Info("lease 长时间丢失，认为另一进程曾持 lease，重扫孤儿",
				"lost_seconds", lost, "threshold_seconds", w.leaseTTL*orphanRescanLeaseLostMult)
			w.orphanHandledOnce = false
		}
		w.leaseLostAt = time.Time{}
	}

	if owns && !w.orphanHandledOnce {
		w.handleOrphanTasksOnStart(ctx)
		if !w.startupSettled {
			w.settleInterruptedCallsOnStart(ctx)
			w.startupSettled = true
		}
		w.orphanHandledOnce = true
	}

	if !owns {
		w.sleep(w.heartbeatInterval)
		return
	}

	if w.claimTasks(ctx) {
		w.sleep(0.05)
	} else {
		w.sleep(w.pollInterval)
	}
}

// Stop 通知主循环退出，并等在途执行收尾（幂等）。
func (w *Worker) Stop(ctx context.Context) {
	w.stopOnce.Do(func() { close(w.stopCh) })
	done := make(chan struct{})
	go func() {
		w.execWG.Wait()
		w.orphanWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (w *Worker) stopped() bool {
	select {
	case <-w.stopCh:
		return true
	default:
		return false
	}
}

// sleep 是可被 Stop 打断的定时等待。
func (w *Worker) sleep(seconds float64) {
	timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-w.stopCh:
	}
}

// shutdown 释放租约并等在途执行收尾。
func (w *Worker) shutdown(ctx context.Context) {
	if w.ownsLease.Load() {
		if err := w.queue.ReleaseWorkerLease(ctx, w.leaseName, w.ownerID); err != nil {
			slog.Warn("释放 worker lease 失败", "owner", w.ownerID, "err", err)
		}
	}
	w.ownsLease.Store(false)
	w.waitInflightCompletion()
}

// waitInflightCompletion 等孤儿 dispatcher 与全部在途执行收尾，然后清空占用台账。
func (w *Worker) waitInflightCompletion() {
	w.orphanWG.Wait()
	w.execWG.Wait()
	w.slots.Clear()
}

// ----------------------------------------------------------------------
// 容量 / 健康
// ----------------------------------------------------------------------

// ReloadLimits 从 DB 重载容量表数字（占用台账与健康账不受影响）。
func (w *Worker) ReloadLimits(ctx context.Context) {
	table, err := CapacityTableFromDB(ctx, w.queue.db)
	if err != nil {
		slog.Warn("从 DB 加载供应商配置失败，保持当前配置", "err", err)
		return
	}
	w.capacity.Replace(table.Limits())
	slog.Info("已更新供应商容量表", "limits", w.capacity.Limits())
}

// effectiveCapacity 返回容量表上限经健康折减后的有效并发；熔断打开时为 0。
//
// 与容量表的 0（不支持该 lane）语义不同：调用方必须分开处理——不支持是 fail-fast 终态，
// 熔断是回队等探针。
func (w *Worker) effectiveCapacity(providerID string, media provider.MediaType) int {
	base := w.capacity.Get(providerID, media)
	if base <= 0 {
		return base
	}
	return w.health.Capacity(providerID, media, base)
}

// poolFullProviders 返回当前 cycle 该 media 不该 claim 的 provider 黑名单。
//
// 两类来源：熔断打开（与占用无关，持续故障的供应商哪怕空着槽也不该再放任务）+ 池满
// （占用已达健康折减后的有效上限）。池满的源是「有占用的 provider」而非容量表全集：空
// provider 本就 has_room，不该进黑名单；「未知但有占用」的 provider（启动窗口 / 运行中
// 被删）照旧进黑名单，避免其池满任务每 cycle 被 claim→requeue 刷屏。
//
// 守卫 base > 0：has_room 在 cap == 0 时也返回 false，不加守卫会把「不支持该 lane 的
// provider」也归入黑名单，让 SQL 静默 drop，而不是走 worker 二次校验的 cap == 0 fail-fast。
func (w *Worker) poolFullProviders(media provider.MediaType) []string {
	blocked := make(map[string]bool)
	for _, pid := range w.health.BlockedProviders(media) {
		if w.capacity.Get(pid, media) > 0 {
			blocked[pid] = true
		}
	}
	for _, pid := range w.slots.OccupiedProviders(media) {
		if w.capacity.Get(pid, media) <= 0 {
			continue
		}
		cap := w.effectiveCapacity(pid, media)
		if cap <= 0 || !w.slots.HasRoom(pid, media, cap) {
			blocked[pid] = true
		}
	}
	out := make([]string, 0, len(blocked))
	for pid := range blocked {
		out = append(out, pid)
	}
	sort.Strings(out)
	return out
}

// ----------------------------------------------------------------------
// 认领
// ----------------------------------------------------------------------

// claimTasks 按泳道认领任务并派发。返回本圈是否认领到任务（决定下一圈休眠时长）。
//
// 池满 / 熔断任务不被 claim → requeue 反复刷屏，而是在 SQL 层按黑名单过滤，保持 queued；
// provider_id IS NULL 老数据与未知 provider 不被过滤，claim 后由 worker 二次解析再校验容量。
func (w *Worker) claimTasks(ctx context.Context) bool {
	claimedAny := false
	for _, media := range laneOrder {
		for {
			if w.stopped() || ctx.Err() != nil {
				return claimedAny
			}
			poolFull := w.poolFullProviders(media)
			task, err := w.queue.ClaimNextTask(ctx, string(media), poolFull)
			if err != nil {
				slog.Warn("认领任务失败", "media", media, "err", err)
				break
			}
			if task == nil {
				break
			}

			providerID := w.extractProviderID(ctx, task)
			laneCap := w.capacity.Get(providerID, media)
			if laneCap <= 0 {
				slog.Warn("供应商不支持该媒体类型，任务标记失败",
					"provider", providerID, "media", media, "task_id", task.TaskID)
				_, err := w.queue.MarkTaskFailed(writeContext(ctx), task.TaskID, w.encodeFailure(
					"provider_unsupported_media",
					map[string]any{"provider_id": providerID, "media_type": string(media)},
				))
				if err != nil {
					slog.Warn("标记不支持媒体类型失败", "task_id", task.TaskID, "err", err)
				}
				claimedAny = true
				continue
			}

			if cap := w.effectiveCapacity(providerID, media); cap <= 0 {
				// 健康熔断：回队等半开探针，绝不 mark_failed——上游抖动是暂时的。
				slog.Info("供应商泳道熔断中，任务回队等半开探针",
					"provider", providerID, "media", media, "task_id", task.TaskID)
				w.deferClaimedTask(ctx, task, providerID)
				break
			}

			budget := w.effectiveCapacity(providerID, media)
			if !w.slots.HasRoom(providerID, media, budget) {
				// NULL 老数据 / 未知 provider 经 SQL 兜底走到这里：二次校验仍满 → 回队下次再试。
				slog.Info("供应商池满，任务回队等待下一 cycle",
					"provider", providerID, "media", media, "task_id", task.TaskID)
				w.deferClaimedTask(ctx, task, providerID)
				break
			}

			claimedAny = true
			w.dispatch(ctx, providerID, media, task)
		}
	}
	return claimedAny
}

// dispatch 起一个执行 goroutine 并同步登记占用（避免异步调度尚未触发时 has_room 误判）。
func (w *Worker) dispatch(ctx context.Context, providerID string, media provider.MediaType, task *repository.TaskData) {
	execCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	handle := newRunHandle(cancel)
	w.registerCancel(task.TaskID, cancel)
	w.slots.Register(providerID, media, task.TaskID, handle, false)
	w.execWG.Add(1)
	go func() {
		defer w.execWG.Done()
		handle.setErr(w.processTask(execCtx, task, providerID))
		handle.finish()
	}()
}

// deferClaimedTask 把已 claim 但当前不能跑的任务放回 queued，等下次 cycle 重试。
//
// 回队前把重派生的 provider 刷回投影列：走到这里说明存量投影与现值分裂（NULL 兜底，或
// 入队后剧本参考集 / 供应商配置被改），不刷新的话存量值躲过 SQL 过滤，之后每 cycle 重复
// claim → requeue → break，同 lane 其他可跑任务被持续排头阻塞。best-effort。
func (w *Worker) deferClaimedTask(ctx context.Context, task *repository.TaskData, providerID string) {
	if err := w.queue.PersistExecutionProviderID(writeContext(ctx), task.TaskID, providerID); err != nil {
		slog.Warn("回队前投影刷新失败", "task_id", task.TaskID, "provider", providerID, "err", err)
	}
	if _, err := w.queue.RequeueTask(writeContext(ctx), task.TaskID); err != nil {
		slog.Warn("回队任务失败", "task_id", task.TaskID, "err", err)
	}
}

// ----------------------------------------------------------------------
// 执行与终态收口
// ----------------------------------------------------------------------

// processTask 执行一个任务并按 0-rows-cancelled 协议收口（ADR 0006）。返回执行错误供占用
// 台账的诊断读取（终态本身已在函数内落库）。
//
// 所有 DB 写入用不带 cancel 的 context（等价 asyncio.shield）：取消信号在写入期间到达时
// 让写入跑完再传播，避免任务停在 cancelling / running 中间态。
func (w *Worker) processTask(ctx context.Context, task *repository.TaskData, providerID string) error {
	taskID := task.TaskID
	taskType := task.TaskType
	media := MediaLane(task.MediaType, task.TaskType)
	slog.Info("开始处理任务", "task_id", taskID, "type", taskType, "provider", providerID)

	result, execErr := w.runExecutor(ctx, task)

	if execErr != nil && ctx.Err() != nil {
		// 用户 / 级联取消：取消不是上游不健康的信号，健康账不动。
		w.settleCancelledLedgerCall(ctx, task)
		w.finalizeCancelled(ctx, taskID)
		return execErr
	}
	if execErr != nil {
		slog.Error("任务失败", "task_id", taskID, "type", taskType, "provider", providerID, "err", execErr)
		w.health.RecordFailure(providerID, media, w.isTransient(execErr))
		w.markFailed(ctx, taskID, w.encodeTaskError(execErr))
		return execErr
	}
	w.health.RecordSuccess(providerID, media)

	rows, err := w.queue.MarkTaskSucceeded(writeContext(ctx), taskID, result)
	if err != nil {
		slog.Error("标记任务成功失败", "task_id", taskID, "err", err)
		return err
	}
	if rows == 0 {
		// 0-rows-cancelled 协议：execute 跑赢但 DB 已被外部翻 cancelling。
		w.finalizeCancelled(ctx, taskID)
		return nil
	}
	slog.Info("任务完成", "task_id", taskID, "type", taskType, "provider", providerID)
	return nil
}

// runExecutor 跑 preflight → 执行 → 媒资登记；panic 收敛为错误，不让单个任务拖垮 worker。
func (w *Worker) runExecutor(ctx context.Context, task *repository.TaskData) (result map[string]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("task executor panic: %v", r)
		}
	}()
	if w.preflight != nil {
		if err := w.preflight(ctx, task); err != nil {
			return nil, err
		}
	}
	result, err = w.executor.Execute(ctx, task)
	if err != nil {
		return nil, err
	}
	if w.indexMedia != nil {
		result = w.indexMedia(ctx, task, result)
	}
	return result, nil
}

// markFailed 落 failed；0 行说明外部已抢先翻状态（cancelling），落地 cancelled 终态。
func (w *Worker) markFailed(ctx context.Context, taskID, message string) {
	rows, err := w.queue.MarkTaskFailed(writeContext(ctx), taskID, message)
	if err != nil {
		slog.Warn("标记任务失败出错", "task_id", taskID, "err", err)
		return
	}
	if rows == 0 {
		w.finalizeCancelled(ctx, taskID)
	}
}

// finalizeCancelled 走 0-rows-cancelled 协议兜底落 cancelled 终态。
func (w *Worker) finalizeCancelled(ctx context.Context, taskID string) {
	if _, err := w.queue.MarkTaskCancelled(writeContext(ctx), taskID, "user"); err != nil {
		slog.Warn("落 cancelled 终态失败", "task_id", taskID, "err", err)
	}
}

// settleCancelledLedgerCall 取消收尾：翻掉该 task 已持久化的 pending 记账行，避免永久挂账。
//
// 取消是终态，任务不会再被 resume，pending 行因而永远不会被补。缺锚点或结算失败只记日志，
// 绝不阻断 mark_task_cancelled。
func (w *Worker) settleCancelledLedgerCall(ctx context.Context, task *repository.TaskData) {
	if w.settler == nil {
		return
	}
	raw, ok := task.Payload["api_call_id"]
	if !ok || raw == nil {
		return
	}
	callID, ok := toInt64(raw)
	if !ok {
		slog.Warn("取消收尾跳过非法 api_call_id", "task_id", task.TaskID, "value", raw)
		return
	}
	if err := w.settler.ResumeFailed(writeContext(ctx), callID); err != nil {
		slog.Error("取消收口 pending 记账失败", "task_id", task.TaskID, "call_id", callID, "err", err)
	}
}

// settleInterruptedCallsOnStart 启动收口孤儿记账行；失败只记日志，绝不打断任务处理主循环。
func (w *Worker) settleInterruptedCallsOnStart(ctx context.Context) {
	if w.settler == nil {
		return
	}
	settled, err := w.settler.SettleInterruptedCalls(writeContext(ctx), w.constructedAt)
	if err != nil {
		slog.Error("启动收口孤儿记账行失败（不影响任务处理）", "err", err)
		return
	}
	if settled > 0 {
		slog.Info("启动收口孤儿记账行", "count", settled)
	}
}

// ----------------------------------------------------------------------
// 占用台账收尾 / 取消
// ----------------------------------------------------------------------

// drainFinished 清掉已完成的占用；被取消的子任务由 drain 端兜底 mark_cancelled。
//
// 正常路径 processTask 已 mark_cancelled；但取消可能落在 processTask 进入取消分支之前，
// 那一刻来不及落终态。SQL 守卫 status IN (queued, cancelling, running) 保证幂等。
func (w *Worker) drainFinished(ctx context.Context) {
	for _, finished := range w.slots.DrainFinished() {
		w.unregisterCancel(finished.TaskID)
		if finished.Task.Cancelled() {
			if _, err := w.queue.MarkTaskCancelled(writeContext(ctx), finished.TaskID, "user"); err != nil {
				slog.Warn("drain 兜底 mark_cancelled 失败", "task_id", finished.TaskID, "err", err)
			}
			continue
		}
		if err := finished.Task.Err(); err != nil {
			slog.Debug("已处理的任务异常已在 processTask 中记录", "task_id", finished.TaskID)
		}
	}
}

// RequestCancel 是进程内取消信号：取消 task 对应的执行 context，返回是否命中。
//
// 由取消 API 同步调用（秒级响应）。未命中是 best-effort 失败——worker finally 走
// mark_cancelled 兜底（SQL 守卫 IN queued|cancelling|running 接住）。
func (w *Worker) RequestCancel(taskID string) bool {
	handle := w.slots.FindByTask(taskID)
	if handle != nil && !handle.Done() {
		handle.Cancel()
		slog.Info("已对任务发出 in-process cancel 信号", "task_id", taskID)
		return true
	}
	slog.Info("request_cancel: 任务不在 inflight（worker finally 兜底）", "task_id", taskID)
	return false
}

// ----------------------------------------------------------------------
// 诊断
// ----------------------------------------------------------------------

// LaneObservability 是单条泳道的诊断视图。
type LaneObservability struct {
	Capacity          int     `json:"capacity"`
	EffectiveCapacity int     `json:"effective_capacity"`
	Occupied          int     `json:"occupied"`
	Inflight          int     `json:"inflight"`
	Pending           int     `json:"pending"`
	Utilization       float64 `json:"utilization"`
	Health            string  `json:"health"`
}

// ObservabilitySnapshot 是只读诊断快照。
type ObservabilitySnapshot struct {
	Lanes  map[string]LaneObservability  `json:"lanes"`
	Health map[string]LaneHealthSnapshot `json:"health"`
}

// ObservabilitySnapshot 返回每条泳道的占用 / 有效并发 / 健康态，键为 "provider:media"。
//
// 只收容量 > 0 的登记泳道，外加任何有占用的未知 provider（首次 reload 前的启动窗口 /
// 运行中被删的 custom provider），避免它们从诊断视图里凭空消失。纯查询、不写库。
func (w *Worker) ObservabilitySnapshot() ObservabilitySnapshot {
	slots := w.slots.Snapshot()
	keys := make(map[laneKey]bool)
	for providerID, lanes := range w.capacity.Snapshot() {
		for media, limit := range lanes {
			if limit > 0 {
				keys[laneKey{providerID, provider.MediaType(media)}] = true
			}
		}
	}
	for key := range slots {
		keys[key] = true
	}

	lanes := make(map[string]LaneObservability, len(keys))
	for key := range keys {
		occupied := slots[key]
		effective := w.effectiveCapacity(key.ProviderID, key.MediaType)
		utilization := 0.0
		if effective > 0 {
			utilization = float64(occupied.Occupied) / float64(effective)
		}
		lanes[key.ProviderID+":"+string(key.MediaType)] = LaneObservability{
			Capacity:          w.capacity.Get(key.ProviderID, key.MediaType),
			EffectiveCapacity: effective,
			Occupied:          occupied.Occupied,
			Inflight:          occupied.Inflight,
			Pending:           occupied.Pending,
			Utilization:       utilization,
			Health:            string(w.health.State(key.ProviderID, key.MediaType)),
		}
	}
	return ObservabilitySnapshot{Lanes: lanes, Health: w.health.Snapshot()}
}

// ----------------------------------------------------------------------
// 内部
// ----------------------------------------------------------------------

func (w *Worker) extractProviderID(ctx context.Context, task *repository.TaskData) string {
	if w.extractProvider == nil {
		return DefaultProvider
	}
	providerID := w.extractProvider(ctx, task)
	if providerID == "" {
		return DefaultProvider
	}
	return providerID
}

func (w *Worker) registerCancel(taskID string, cancel context.CancelFunc) {
	w.runHandle.Lock()
	defer w.runHandle.Unlock()
	w.cancels()[taskID] = cancel
}

func (w *Worker) unregisterCancel(taskID string) {
	w.runHandle.Lock()
	defer w.runHandle.Unlock()
	delete(w.cancels(), taskID)
}

// cancels 返回取消表；调用方须持 runHandle 锁。首次访问懒初始化。
func (w *Worker) cancels() map[string]context.CancelFunc {
	if w.cancelFuncs == nil {
		w.cancelFuncs = map[string]context.CancelFunc{}
	}
	return w.cancelFuncs
}

// writeContext 返回不带 cancel 的 context：DB 写入不被取消打断（等价 asyncio.shield）。
func writeContext(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// mustJSONString 是 repository 侧 JSON 序列化在本包的轻量复用。
func mustJSONString(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// toInt64 把 JSON 解码出的数值（float64 / json.Number / 整数）归一为 int64。
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case float64:
		if n != float64(int64(n)) {
			return 0, false
		}
		return int64(n), true
	case float32:
		f := float64(n)
		if f != float64(int64(f)) {
			return 0, false
		}
		return int64(f), true
	default:
		return 0, false
	}
}

// runHandle 是占用台账里的执行体句柄：Done / Cancel / Cancelled / Err 的 Go 实现。
type runHandle struct {
	cancel    context.CancelFunc
	done      chan struct{}
	cancelled atomic.Bool
	errMu     sync.Mutex
	err       error
	once      sync.Once
}

func newRunHandle(cancel context.CancelFunc) *runHandle {
	return &runHandle{cancel: cancel, done: make(chan struct{})}
}

// finish 标记执行结束（由执行 goroutine 在返回前调用）。
func (h *runHandle) finish() { close(h.done) }

func (h *runHandle) setErr(err error) {
	h.errMu.Lock()
	defer h.errMu.Unlock()
	h.err = err
}

func (h *runHandle) Done() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

func (h *runHandle) Cancel() {
	h.cancelled.Store(true)
	h.once.Do(h.cancel)
}

func (h *runHandle) Cancelled() bool { return h.cancelled.Load() }

func (h *runHandle) Err() error {
	h.errMu.Lock()
	defer h.errMu.Unlock()
	return h.err
}
