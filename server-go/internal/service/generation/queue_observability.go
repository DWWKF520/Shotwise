// 队列可观测性只读读层（对应 lib/queue_observability.py）。
//
// 把四个数据源拼成一份诊断快照：Task 表的积压 / 排队名次、ApiCall 表的调用耗时
// p50/p95、worker 的内存泳道快照（可选注入）、WorkerLease 行的进程级在线态。
//
// 只读约束：不写库、不改调度决策、不 requeue（ADR 0007）。查询自带在本模块内，
// 不复用 TaskRepo 的写路径，避免与状态机耦合。
//
// 告警只输出机器可读的 code + 数值/阈值，文案由前端按 code 本地化——本模块不落
// 面向用户的自然语言。分位数按最近秩（nearest-rank）在 Go 侧算：SQLite 没有
// percentile_cont，在 SQL 里算会让两套方言分叉，而这里只需要诊断精度。
package service

import (
	"context"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/queue"
	"github.com/shotwise/shotwise/internal/repository"
)

// 活动 / 终态口径与 repository.ActiveTaskStatuses 一致（本模块独立读，不 import 仓库常量）。
var (
	obsActiveStatuses   = []string{"queued", "running", "cancelling"}
	obsTerminalStatuses = []string{"succeeded", "failed", "cancelled"}
)

const (
	obsDefaultLatencyWindowSeconds  = 3600.0
	obsDefaultLatencySampleLimit    = 5000
	obsDefaultBacklogAlertThreshold = 50
	obsDefaultPendingAlertThreshold = 10
	obsDefaultServiceSampleLimit    = 500
	// obsDefaultWorkerLeaseName 是进程级选主行的 name（worker 每轮续约的就是这一行）。
	obsDefaultWorkerLeaseName = "default"
)

// WorkerSnapshotter 是泳道诊断快照的来源（*queue.Worker 满足）。未注入时省掉泳道
// 与 ETA 容量分母，但 worker 块仍给出选主行在线态。
type WorkerSnapshotter interface {
	ObservabilitySnapshot() queue.ObservabilitySnapshot
}

// WorkerLeaseState 是选主行的在线态。
type WorkerLeaseState struct {
	Name                  string   `json:"name"`
	Online                bool     `json:"online"`
	LeaseRemainingSeconds *float64 `json:"lease_remaining_seconds"`
	InProcess             bool     `json:"in_process"`
}

// DurationSummary 是一组耗时的分位统计。
type DurationSummary struct {
	Samples int     `json:"samples"`
	P50Ms   float64 `json:"p50_ms"`
	P95Ms   float64 `json:"p95_ms"`
	AvgMs   float64 `json:"avg_ms"`
}

// MediaETA 是单条媒体类型的粗排期估计。
type MediaETA struct {
	Queued             int      `json:"queued"`
	Running            int      `json:"running"`
	SampleCount        int      `json:"sample_count"`
	AvgServiceSeconds  *float64 `json:"avg_service_seconds"`
	EffectiveCapacity  *int     `json:"effective_capacity"`
	ETASeconds         *float64 `json:"eta_seconds"`
}

// QueueSnapshot 是完整诊断快照。
type QueueSnapshot struct {
	GeneratedAt            string                            `json:"generated_at"`
	ProjectName            *string                           `json:"project_name"`
	Worker                 WorkerLeaseState                  `json:"worker"`
	Counts                 map[string]int                    `json:"counts"`
	ActiveByMedia          map[string]map[string]int         `json:"active_by_media"`
	QueuedByProvider       map[string]int                    `json:"queued_by_provider"`
	OldestQueuedAt         *string                           `json:"oldest_queued_at"`
	OldestQueuedAgeSeconds *float64                          `json:"oldest_queued_age_seconds"`
	ETA                    map[string]MediaETA               `json:"eta"`
	Providers              map[string]map[string]DurationSummary `json:"providers"`
	Lanes                  any                               `json:"lanes"`
	Health                 any                               `json:"health"`
	Alerts                 []map[string]any                  `json:"alerts"`
}

// ObservabilityOptions 是可选的注入参数（生产路径全部走默认值）。
type ObservabilityOptions struct {
	Now                   time.Time
	LatencyWindowSeconds  *float64
	LatencySampleLimit    *int
	ServiceSampleLimit    int
	BacklogAlertThreshold *int
	PendingAlertThreshold *int
}

// QueueObservability 是快照查询入口。
type QueueObservability struct {
	db     *gorm.DB
	worker WorkerSnapshotter
}

// NewQueueObservability 构造；worker 允许为 nil（省掉泳道与 ETA 容量分母）。
func NewQueueObservability(gdb *gorm.DB, worker WorkerSnapshotter) *QueueObservability {
	return &QueueObservability{db: gdb, worker: worker}
}

// BuildSnapshot 拼出完整诊断快照。
func (o *QueueObservability) BuildSnapshot(
	ctx context.Context, projectName *string, opts ObservabilityOptions,
) (QueueSnapshot, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	counts, err := o.taskStatusCounts(ctx, projectName)
	if err != nil {
		return QueueSnapshot{}, err
	}
	activeByMedia, err := o.activeByMedia(ctx, projectName)
	if err != nil {
		return QueueSnapshot{}, err
	}
	queuedByProvider, err := o.queuedByProvider(ctx, projectName)
	if err != nil {
		return QueueSnapshot{}, err
	}
	oldestQueuedAt, err := o.oldestQueuedAt(ctx, projectName)
	if err != nil {
		return QueueSnapshot{}, err
	}

	lanes := map[string]queue.LaneObservability{}
	health := map[string]queue.LaneHealthSnapshot{}
	if o.worker != nil {
		snapshot := o.worker.ObservabilitySnapshot()
		if snapshot.Lanes != nil {
			lanes = snapshot.Lanes
		}
		if snapshot.Health != nil {
			health = snapshot.Health
		}
	}

	serviceLimit := opts.ServiceSampleLimit
	if serviceLimit <= 0 {
		serviceLimit = obsDefaultServiceSampleLimit
	}
	serviceSamples, err := o.serviceTimeSamples(ctx, projectName, serviceLimit)
	if err != nil {
		return QueueSnapshot{}, err
	}
	backlogThreshold := obsDefaultBacklogAlertThreshold
	if opts.BacklogAlertThreshold != nil {
		backlogThreshold = *opts.BacklogAlertThreshold
	} else {
		backlogThreshold = readObsIntEnv("QUEUE_BACKLOG_ALERT_THRESHOLD", obsDefaultBacklogAlertThreshold, 1)
	}
	pendingThreshold := obsDefaultPendingAlertThreshold
	if opts.PendingAlertThreshold != nil {
		pendingThreshold = *opts.PendingAlertThreshold
	} else {
		pendingThreshold = readObsIntEnv("QUEUE_PENDING_ALERT_THRESHOLD", obsDefaultPendingAlertThreshold, 1)
	}

	workerState, err := o.WorkerLeaseState(ctx, obsDefaultWorkerLeaseName, float64(now.UnixNano())/1e9)
	if err != nil {
		return QueueSnapshot{}, err
	}
	// 「本进程挂没挂 worker」与「有没有进程持有选主行」是两件事，一起给出才不会误读空泳道。
	workerState.InProcess = o.worker != nil

	providers, err := o.providerLatencies(ctx, projectName, opts.LatencyWindowSeconds, opts.LatencySampleLimit, now)
	if err != nil {
		return QueueSnapshot{}, err
	}

	snapshot := QueueSnapshot{
		GeneratedAt:       pyISO(now),
		ProjectName:       projectName,
		Worker:            workerState,
		Counts:            counts,
		ActiveByMedia:     activeByMedia,
		QueuedByProvider:  queuedByProvider,
		ETA:               etaByMedia(activeByMedia, serviceSamples, lanes),
		Providers:         providers,
		Lanes:             lanes,
		Health:            health,
	}
	if oldestQueuedAt != nil {
		iso := pyISO(*oldestQueuedAt)
		age := pyRound1(now.Sub(*oldestQueuedAt).Seconds())
		snapshot.OldestQueuedAt = &iso
		snapshot.OldestQueuedAgeSeconds = &age
	}
	snapshot.Alerts = buildQueueAlerts(counts, workerState.Online, lanes, backlogThreshold, pendingThreshold)
	return snapshot, nil
}

// WorkerLeaseState 读进程级选主行的在线态：lease_until 是 epoch 秒，缺行或已过期都算离线。
//
// 快照需要这个跨进程信号，是因为 lanes 只反映本进程注入的 worker：泳道为空既可能是
// 「空闲」，也可能是「这个进程根本没挂 worker」，少了这一位两种情况会被读成同一种。
func (o *QueueObservability) WorkerLeaseState(
	ctx context.Context, name string, nowEpoch float64,
) (WorkerLeaseState, error) {
	var rows []float64
	if err := o.db.WithContext(ctx).Model(&repository.WorkerLease{}).
		Where("name = ?", name).Pluck("lease_until", &rows).Error; err != nil {
		return WorkerLeaseState{}, err
	}
	state := WorkerLeaseState{Name: name}
	if len(rows) == 0 {
		return state, nil
	}
	state.Online = rows[0] > nowEpoch
	remaining := pyRound1(rows[0] - nowEpoch)
	state.LeaseRemainingSeconds = &remaining
	return state, nil
}

// TaskQueuePosition 是单个任务在所属泳道里的排队名次。
type TaskQueuePosition struct {
	TaskID     string  `json:"task_id"`
	ProjectName string `json:"project_name"`
	Status     string  `json:"status"`
	MediaType  string  `json:"media_type"`
	ProviderID *string `json:"provider_id"`
	Position   *int    `json:"position"`
	LaneQueued int     `json:"lane_queued"`
	QueuedAt   *string `json:"queued_at"`
}

// TaskQueuePosition 返回任务在其媒体类型泳道里的 0 基名次；任务不存在返回 nil, nil。
//
// position 按 claim 的排序键 (queued_at, task_id) 计算，等价于 claim_next 的
// ORDER BY queued_at ASC。依赖未就绪的任务也计入，因此它是等待时间的上界而非精确值。
// 非 queued 状态不排名次（position 为空），但仍给出所在泳道的排队规模。
func (o *QueueObservability) TaskQueuePosition(ctx context.Context, taskID string) (*TaskQueuePosition, error) {
	var task repository.Task
	err := o.db.WithContext(ctx).Where("task_id = ?", taskID).Take(&task).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var laneQueued int64
	if err := o.db.WithContext(ctx).Model(&repository.Task{}).
		Where("status = ? AND media_type = ?", repository.TaskStatusQueued, task.MediaType).
		Count(&laneQueued).Error; err != nil {
		return nil, err
	}
	out := &TaskQueuePosition{
		TaskID:      task.TaskID,
		ProjectName: task.ProjectName,
		Status:      task.Status,
		MediaType:   task.MediaType,
		ProviderID:  task.ProviderID,
		LaneQueued:  int(laneQueued),
	}
	if task.QueuedAt.Valid {
		iso := pyISO(task.QueuedAt.Time)
		out.QueuedAt = &iso
	}
	if task.Status == repository.TaskStatusQueued {
		var ahead int64
		if err := o.db.WithContext(ctx).Model(&repository.Task{}).
			Where("status = ? AND media_type = ? AND (queued_at < ? OR (queued_at = ? AND task_id < ?))",
				repository.TaskStatusQueued, task.MediaType,
				task.QueuedAt, task.QueuedAt, task.TaskID).
			Count(&ahead).Error; err != nil {
			return nil, err
		}
		position := int(ahead)
		out.Position = &position
	}
	return out, nil
}

// ---- 查询 ----

func obsProjectFilter(q *gorm.DB, projectName *string) *gorm.DB {
	if projectName == nil {
		return q
	}
	return q.Where("project_name = ?", *projectName)
}

// taskStatusCounts 按状态分组计数；缺失的状态补 0，形状与 /tasks/stats 一致。
func (o *QueueObservability) taskStatusCounts(ctx context.Context, projectName *string) (map[string]int, error) {
	var rows []struct {
		Status string
		Cnt    int64
	}
	q := obsProjectFilter(o.db.WithContext(ctx).Model(&repository.Task{}), projectName)
	if err := q.Select("status, COUNT(*) AS cnt").Group("status").Scan(&rows).Error; err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, status := range append(append([]string{}, obsActiveStatuses...), obsTerminalStatuses...) {
		counts[status] = 0
	}
	total := 0
	for _, row := range rows {
		if _, known := counts[row.Status]; known {
			counts[row.Status] = int(row.Cnt)
		}
		total += int(row.Cnt)
	}
	counts["total"] = total
	return counts, nil
}

// activeByMedia 把活动任务按媒体类型 × 状态分桶（终态不进积压视图）。
func (o *QueueObservability) activeByMedia(ctx context.Context, projectName *string) (map[string]map[string]int, error) {
	var rows []struct {
		MediaType string
		Status    string
		Cnt       int64
	}
	q := obsProjectFilter(o.db.WithContext(ctx).Model(&repository.Task{}).Where("status IN ?", obsActiveStatuses), projectName)
	if err := q.Select("media_type, status, COUNT(*) AS cnt").
		Group("media_type, status").Order("media_type").Scan(&rows).Error; err != nil {
		return nil, err
	}
	buckets := map[string]map[string]int{}
	for _, row := range rows {
		bucket := buckets[row.MediaType]
		if bucket == nil {
			bucket = map[string]int{"queued": 0, "running": 0, "cancelling": 0}
			buckets[row.MediaType] = bucket
		}
		bucket[row.Status] = int(row.Cnt)
	}
	return buckets, nil
}

// queuedByProvider 把排队中的任务按 provider_id 分桶；未落 provider 的老数据显示为 unknown。
func (o *QueueObservability) queuedByProvider(ctx context.Context, projectName *string) (map[string]int, error) {
	var rows []struct {
		ProviderID *string
		Cnt        int64
	}
	q := obsProjectFilter(o.db.WithContext(ctx).Model(&repository.Task{}).
		Where("status = ?", repository.TaskStatusQueued), projectName)
	if err := q.Select("provider_id, COUNT(*) AS cnt").Group("provider_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, row := range rows {
		key := "unknown"
		if row.ProviderID != nil && *row.ProviderID != "" {
			key = *row.ProviderID
		}
		out[key] += int(row.Cnt)
	}
	return out, nil
}

// oldestQueuedAt 返回最老一条 queued 任务的入队时间。
func (o *QueueObservability) oldestQueuedAt(ctx context.Context, projectName *string) (*time.Time, error) {
	var rows []db.FlexibleTime
	q := obsProjectFilter(o.db.WithContext(ctx).Model(&repository.Task{}).
		Where("status = ?", repository.TaskStatusQueued), projectName)
	if err := q.Select("MIN(queued_at) AS min_queued").Pluck("min_queued", &rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 || !rows[0].Valid {
		return nil, nil
	}
	value := rows[0].Time.UTC()
	return &value, nil
}

// serviceTimeSamples 是最近完成的成功任务按媒体类型给一组服务时长（秒），用于 ETA 的均值。
//
// 取 finished_at - started_at（含重试与排队后的真实执行时长），而不是 ApiCall.duration_ms：
// 后者是一次上游调用的耗时，不含重试与本地落盘。
func (o *QueueObservability) serviceTimeSamples(
	ctx context.Context, projectName *string, limit int,
) (map[string][]float64, error) {
	var rows []repository.Task
	q := obsProjectFilter(o.db.WithContext(ctx).Model(&repository.Task{}).
		Where("status = ? AND started_at IS NOT NULL AND finished_at IS NOT NULL", repository.TaskStatusSucceeded),
		projectName)
	if err := q.Order("finished_at DESC").Limit(limit).
		Select("media_type, started_at, finished_at").Find(&rows).Error; err != nil {
		return nil, err
	}
	samples := map[string][]float64{}
	for i := range rows {
		if !rows[i].StartedAt.Valid || !rows[i].FinishedAt.Valid {
			continue
		}
		seconds := rows[i].FinishedAt.Time.Sub(rows[i].StartedAt.Time).Seconds()
		if seconds > 0 {
			samples[rows[i].MediaType] = append(samples[rows[i].MediaType], seconds)
		}
	}
	return samples, nil
}

// providerLatencies 按 provider → call_type 汇总最近完成调用的 p50 / p95。
//
// 只统计 status == 'success' 且有 duration_ms 的调用：失败调用的耗时长尾（超时被掐断）
// 会把 p95 带偏，失败率另有 usage 端点负责。
func (o *QueueObservability) providerLatencies(
	ctx context.Context, projectName *string, windowSeconds *float64, sampleLimit *int, now time.Time,
) (map[string]map[string]DurationSummary, error) {
	window := obsDefaultLatencyWindowSeconds
	if windowSeconds != nil {
		window = *windowSeconds
	} else {
		window = readObsFloatEnv("QUEUE_LATENCY_WINDOW_SECONDS", obsDefaultLatencyWindowSeconds, 60.0)
	}
	limit := obsDefaultLatencySampleLimit
	if sampleLimit != nil {
		limit = *sampleLimit
	} else {
		limit = readObsIntEnv("QUEUE_LATENCY_SAMPLE_LIMIT", obsDefaultLatencySampleLimit, 1)
	}
	cutoff := now.Add(-time.Duration(window * float64(time.Second)))
	var rows []repository.APICall
	q := obsProjectFilter(o.db.WithContext(ctx).Model(&repository.APICall{}).
		Where("status = ? AND duration_ms IS NOT NULL AND finished_at IS NOT NULL", "success").
		Where("finished_at >= ?", db.NewFlexibleTime(cutoff)), projectName)
	if err := q.Order("finished_at DESC").Limit(limit).
		Select("provider, call_type, duration_ms").Find(&rows).Error; err != nil {
		return nil, err
	}
	grouped := map[string]map[string][]int64{}
	for i := range rows {
		provider := rows[i].Provider
		if provider == "" {
			provider = "unknown"
		}
		if grouped[provider] == nil {
			grouped[provider] = map[string][]int64{}
		}
		if rows[i].DurationMs == nil {
			continue
		}
		grouped[provider][rows[i].CallType] = append(grouped[provider][rows[i].CallType], *rows[i].DurationMs)
	}
	out := make(map[string]map[string]DurationSummary, len(grouped))
	for provider, callTypes := range grouped {
		summaries := make(map[string]DurationSummary, len(callTypes))
		for callType, durations := range callTypes {
			summaries[callType] = summarizeDurations(durations)
		}
		out[provider] = summaries
	}
	return out, nil
}

// ---- 纯函数 ----

// obsPercentile 是最近秩（nearest-rank）分位数，values 必��升序且非空。
func obsPercentile(values []float64, quantile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	rank := int(math.Ceil(quantile * float64(len(values))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(values) {
		rank = len(values)
	}
	return values[rank-1]
}

// summarizeDurations 把一组 duration_ms 收成 p50 / p95 / 均值；空样本返回全 0 计数。
func summarizeDurations(durations []int64) DurationSummary {
	if len(durations) == 0 {
		return DurationSummary{}
	}
	ordered := make([]float64, len(durations))
	var sum float64
	for i, d := range durations {
		ordered[i] = float64(d)
		sum += float64(d)
	}
	sort.Float64s(ordered)
	return DurationSummary{
		Samples: len(ordered),
		P50Ms:   pyRound1(obsPercentile(ordered, 0.5)),
		P95Ms:   pyRound1(obsPercentile(ordered, 0.95)),
		AvgMs:   pyRound1(sum / float64(len(ordered))),
	}
}

// estimateETASeconds 是粗排期估计：排队数 × 平均服务时长 / 有效并发。
// 样本不足、没有排队、或该媒体类型下所有泳道都被熔断（有效并发 0）时返回 nil。
func estimateETASeconds(queued int, avgServiceSeconds *float64, effectiveCapacity int) *float64 {
	if queued <= 0 || effectiveCapacity <= 0 {
		return nil
	}
	if avgServiceSeconds == nil || *avgServiceSeconds <= 0 {
		return nil
	}
	value := pyRound1(float64(queued) * *avgServiceSeconds / float64(effectiveCapacity))
	return &value
}

// etaByMedia 按媒体类型汇总 ETA：排队数、平均服务时长、该类型下全部泳道的有效并发之和。
func etaByMedia(
	activeByMedia map[string]map[string]int,
	serviceSamples map[string][]float64,
	lanes map[string]queue.LaneObservability,
) map[string]MediaETA {
	capacityByMedia := map[string]int{}
	for laneKey, lane := range lanes {
		_, mediaType := splitLaneKey(laneKey)
		capacityByMedia[mediaType] += lane.EffectiveCapacity
	}
	eta := make(map[string]MediaETA, len(activeByMedia))
	for mediaType, buckets := range activeByMedia {
		samples := serviceSamples[mediaType]
		var avg *float64
		if len(samples) > 0 {
			var sum float64
			for _, sample := range samples {
				sum += sample
			}
			value := pyRound1(sum / float64(len(samples)))
			avg = &value
		}
		entry := MediaETA{
			Queued:            buckets["queued"],
			Running:           buckets["running"],
			SampleCount:       len(samples),
			AvgServiceSeconds: avg,
		}
		// 没有泳道快照时（未注入 worker）容量未知，ETA 记空而不是假装 0 并发。
		if capacity, known := capacityByMedia[mediaType]; known {
			capacityCopy := capacity
			entry.EffectiveCapacity = &capacityCopy
			entry.ETASeconds = estimateETASeconds(buckets["queued"], avg, capacity)
		}
		eta[mediaType] = entry
	}
	return eta
}

// splitLaneKey 拆 "provider:media" —— provider 里可能含冒号，故按最后一个冒号切。
func splitLaneKey(key string) (providerID, mediaType string) {
	index := strings.LastIndex(key, ":")
	if index < 0 {
		return key, ""
	}
	return key[:index], key[index+1:]
}

// buildQueueAlerts 从积压计数与泳道快照派生告警条目（纯函数，无 I/O）。
//
// 停滞拆成两种：worker_offline（没有进程持有选主行，活干不了，要重启/查服务）与
// queue_stalled（worker 在线却没任务在跑，通常是泳道全熔断或容量为 0）。两者的
// 处置动作不同，合成一个 code 会把运维引到错的方向。
func buildQueueAlerts(
	counts map[string]int, workerOnline bool, lanes map[string]queue.LaneObservability,
	backlogThreshold, pendingThreshold int,
) []map[string]any {
	alerts := []map[string]any{}
	queued := counts["queued"]
	running := counts["running"]
	if queued >= backlogThreshold {
		alerts = append(alerts, map[string]any{
			"code": "queue_backlog", "severity": "warning",
			"value": queued, "threshold": backlogThreshold,
		})
	}
	if queued+running > 0 && !workerOnline {
		// 有活却没人持有选主行：重启空档与崩溃后残留的 running 都落在这里。
		alerts = append(alerts, map[string]any{
			"code": "worker_offline", "severity": "critical", "value": queued, "running": running,
		})
	} else if queued > 0 && running == 0 {
		// worker 在线却一个都没在跑：所有泳道都被熔断、容量为 0，或依赖尚未就绪。
		alerts = append(alerts, map[string]any{
			"code": "queue_stalled", "severity": "critical", "value": queued,
		})
	}
	keys := make([]string, 0, len(lanes))
	for key := range lanes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, laneKey := range keys {
		lane := lanes[laneKey]
		providerID, mediaType := splitLaneKey(laneKey)
		if lane.Health == "open" {
			alerts = append(alerts, map[string]any{
				"code": "provider_tripped", "severity": "warning",
				"provider_id": providerID, "media_type": mediaType,
			})
		}
		if lane.EffectiveCapacity > 0 && lane.Occupied >= lane.EffectiveCapacity {
			alerts = append(alerts, map[string]any{
				"code": "lane_saturated", "severity": "info",
				"provider_id": providerID, "media_type": mediaType,
				"value": lane.Occupied, "threshold": lane.EffectiveCapacity,
			})
		}
		if lane.Pending >= pendingThreshold {
			// 视频通道的 sem 限流子任务：占着槽位却还没真正发起上游调用。
			alerts = append(alerts, map[string]any{
				"code": "pending_wait_high", "severity": "warning",
				"provider_id": providerID, "media_type": mediaType,
				"value": lane.Pending, "threshold": pendingThreshold,
			})
		}
	}
	return alerts
}

// ---- 助手 ----

// pyISO 输出 Python datetime.isoformat() 形态的 UTC 串（微秒精度 + 时区偏移）。
func pyISO(t time.Time) string {
	t = t.UTC().Truncate(time.Microsecond)
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += "." + strings.Repeat("0", 6-len(strconv.Itoa(us))) + strconv.Itoa(us)
	}
	_, offset := t.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	return s + sign + pad2(offset/3600) + ":" + pad2((offset%3600)/60)
}

func pad2(v int) string {
	if v < 10 {
		return "0" + strconv.Itoa(v)
	}
	return strconv.Itoa(v)
}

// pyRound1 保留一位小数（诊断精度足够，不必复刻 Python 的银行家舍入）。
func pyRound1(value float64) float64 {
	return math.Round(value*10) / 10
}

// readObsFloatEnv 读取浮点环境变量，缺失或低于下界时回落默认值。
func readObsFloatEnv(name string, fallback, minimum float64) float64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < minimum {
		return fallback
	}
	return value
}

// readObsIntEnv 读取整型环境变量，缺失或低于下界时回落默认值。
func readObsIntEnv(name string, fallback, minimum int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum {
		return fallback
	}
	return value
}
