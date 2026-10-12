package repository

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/infra/db"
)

// 取消语义（两级：DB 状态 + 进程内取消信号）与 worker 租约。
//
// 取消按状态分发：
//   - queued  → 直接终态 cancelled，并级联下游（含 grandchildren）；
//   - running → 落 cancelling 中间态，等 worker finally 兜底；下游级联推迟到
//     finalize_cancelled，避免「running 父尚未终止，下游 queued 已永久 cancelled」；
//   - cancelling → 幂等（视为已取消，不重复发信号）；
//   - 终态 → skipped_terminal。
//
// Repository 只更新 DB；cancelling 列表由上层（GenerationQueue）拿到后同步分发
// 进程内取消信号，Repository 不持有 worker callback。

// CancelResult 是取消操作的三个桶。
type CancelResult struct {
	Cancelled       []TaskData `json:"cancelled"`
	Cancelling      []string   `json:"cancelling"`
	SkippedTerminal []TaskData `json:"skipped_terminal"`
}

// FinalizeResult 是 worker finally 收口单任务的结果：cancelling 是级联打到的
// running 下游（上层需补发取消信号）。
type FinalizeResult struct {
	Rows       int64    `json:"rows"`
	Cancelling []string `json:"cancelling"`
}

// CancelPreviewTask 是取消预览里的任务摘要。
type CancelPreviewTask struct {
	TaskID     string `json:"task_id"`
	TaskType   string `json:"task_type"`
	ResourceID string `json:"resource_id"`
	Status     string `json:"status"`
}

// CancelPreviewDependent 是取消预览里的下游任务摘要。
type CancelPreviewDependent struct {
	TaskID     string `json:"task_id"`
	TaskType   string `json:"task_type"`
	ResourceID string `json:"resource_id"`
}

// CancelPreview 预览取消影响范围：只列「队列中的下游」，running/cancelling 下游数量
// 不稳定，由实际 cancel 触发后再经 SSE 反映。
type CancelPreview struct {
	Task     CancelPreviewTask        `json:"task"`
	Cascaded []CancelPreviewDependent `json:"cascaded"`
}

func (r *TaskRepo) getTask(ctx context.Context, taskID string) (*Task, error) {
	var row Task
	if err := r.db.WithContext(ctx).Where("task_id = ?", taskID).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &TaskNotFoundError{TaskID: taskID}
		}
		return nil, err
	}
	return &row, nil
}

// GetCancelPreview 预览取消某个任务的影响范围。
func (r *TaskRepo) GetCancelPreview(ctx context.Context, taskID string) (CancelPreview, error) {
	row, err := r.getTask(ctx, taskID)
	if err != nil {
		return CancelPreview{}, err
	}
	deps, err := r.collectQueuedDependents(ctx, taskID)
	if err != nil {
		return CancelPreview{}, err
	}
	return CancelPreview{
		Task:     CancelPreviewTask{TaskID: row.TaskID, TaskType: row.TaskType, ResourceID: row.ResourceID, Status: row.Status},
		Cascaded: deps,
	}, nil
}

func (r *TaskRepo) collectQueuedDependents(ctx context.Context, taskID string) ([]CancelPreviewDependent, error) {
	var rows []Task
	if err := r.db.WithContext(ctx).Model(&Task{}).
		Where("dependency_task_id = ? AND status = ?", taskID, TaskStatusQueued).
		Order("queued_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	dependents := make([]CancelPreviewDependent, 0, len(rows))
	for i := range rows {
		summary := CancelPreviewDependent{TaskID: rows[i].TaskID, TaskType: rows[i].TaskType, ResourceID: rows[i].ResourceID}
		dependents = append(dependents, summary)
		nested, err := r.collectQueuedDependents(ctx, rows[i].TaskID)
		if err != nil {
			return nil, err
		}
		dependents = append(dependents, nested...)
	}
	return dependents, nil
}

// CancelTask 按状态分发取消单个任务。
func (r *TaskRepo) CancelTask(ctx context.Context, taskID string) (CancelResult, error) {
	row, err := r.getTask(ctx, taskID)
	if err != nil {
		return CancelResult{}, err
	}
	result := CancelResult{Cancelled: []TaskData{}, Cancelling: []string{}, SkippedTerminal: []TaskData{}}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return dispatchCancel(tx, row, "user", &result)
	})
	if err != nil {
		return CancelResult{}, err
	}
	return result, nil
}

// CancelEpisodeTasks 取消指定项目、指定原始剧本绑定下的全部活动任务。
// script_file 是分集的稳定身份：删除或调整显示编号不会波及其他分集。
func (r *TaskRepo) CancelEpisodeTasks(ctx context.Context, projectName, scriptFile string) (CancelResult, error) {
	var rows []Task
	if err := r.db.WithContext(ctx).Model(&Task{}).
		Where("project_name = ? AND script_file = ? AND status IN ?", projectName, scriptFile, ActiveTaskStatuses).
		Order("queued_at ASC, task_id ASC").Find(&rows).Error; err != nil {
		return CancelResult{}, err
	}
	result := CancelResult{Cancelled: []TaskData{}, Cancelling: []string{}, SkippedTerminal: []TaskData{}}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i := range rows {
			if err := dispatchCancel(tx, &rows[i], "episode_deleted", &result); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return CancelResult{}, err
	}
	return result, nil
}

// GetCancelAllPreview 返回项目中当前活动任务数量。
func (r *TaskRepo) GetCancelAllPreview(ctx context.Context, projectName string) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&Task{}).
		Where("project_name = ? AND status IN ?", projectName, ActiveTaskStatuses).
		Count(&count).Error
	return count, err
}

// CancelAllActive 取消项目中全部活动任务（图片、视频通道一并停止，不区分 media_type）。
func (r *TaskRepo) CancelAllActive(ctx context.Context, projectName string) (CancelResult, error) {
	var rows []Task
	if err := r.db.WithContext(ctx).Model(&Task{}).
		Where("project_name = ? AND status IN ?", projectName, ActiveTaskStatuses).
		Order("queued_at ASC, task_id ASC").Find(&rows).Error; err != nil {
		return CancelResult{}, err
	}
	result := CancelResult{Cancelled: []TaskData{}, Cancelling: []string{}, SkippedTerminal: []TaskData{}}
	if len(rows) == 0 {
		return result, nil
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i := range rows {
			if err := dispatchCancel(tx, &rows[i], "user", &result); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return CancelResult{}, err
	}
	return result, nil
}

// dispatchCancel 按 task.status 分发到对应的 DB 状态转移。
func dispatchCancel(tx *gorm.DB, task *Task, cancelledBy string, result *CancelResult) error {
	switch task.Status {
	case TaskStatusQueued:
		_, err := markCancelled(tx, task.TaskID, cancelledBy, result, true)
		return err
	case TaskStatusRunning:
		affected, err := markCancelling(tx, task.TaskID, cancelledBy)
		if err != nil {
			return err
		}
		if affected > 0 {
			result.Cancelling = append(result.Cancelling, task.TaskID)
			return nil
		}
		// 竞态：UPDATE 失败说明 status 已变；刷新后分发到对应桶。
		var refreshed Task
		if err := tx.Where("task_id = ?", task.TaskID).Take(&refreshed).Error; err != nil {
			return err
		}
		switch refreshed.Status {
		case TaskStatusCancelling:
			result.Cancelling = append(result.Cancelling, refreshed.TaskID)
		case TaskStatusSucceeded, TaskStatusFailed, TaskStatusCancelled:
			// worker 已抢先落终态，让 API 响应里有迹可循（避免前端转圈不停）。
			result.SkippedTerminal = append(result.SkippedTerminal, taskToData(&refreshed))
		}
		return nil
	case TaskStatusCancelling:
		// 幂等：已发起取消，不重复加信号。
		return nil
	default:
		result.SkippedTerminal = append(result.SkippedTerminal, taskToData(task))
		return nil
	}
}

// markCancelled 把 queued / cancelling / running 任务标为 cancelled（终态）。
//
// WHERE 守卫承接三条路径：cancel API 直接取消 queued；worker finally 从 cancelling
// 落地；进程级取消（SIGTERM / 超时）直接打到 running——漏掉这条会把任务永久卡在
// running，每次重启都被孤儿处理当成待恢复任务。终态仍被 IN 子句排除，保持幂等。
//
// cancelled_by 用 COALESCE 写入：上游已写过的（cascade）保留，没写过的用 caller
// 提供的值兜底，避免级联归因丢失。
func markCancelled(tx *gorm.DB, taskID, cancelledBy string, result *CancelResult, cascade bool) (*TaskData, error) {
	now := time.Now()
	res := tx.Model(&Task{}).
		Where("task_id = ? AND status IN ?", taskID, []string{TaskStatusQueued, TaskStatusCancelling, TaskStatusRunning}).
		Updates(map[string]any{
			"status":       TaskStatusCancelled,
			"cancelled_by": gorm.Expr("COALESCE(cancelled_by, ?)", cancelledBy),
			"finished_at":  db.NewFlexibleTime(now),
			"updated_at":   db.NewFlexibleTime(now),
		})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, nil
	}
	var row Task
	if err := tx.Where("task_id = ?", taskID).Take(&row).Error; err != nil {
		return nil, err
	}
	data := taskToData(&row)
	if result != nil {
		// 先记自身再级联——保证响应体里父先于子。
		result.Cancelled = append(result.Cancelled, data)
	}
	if cascade {
		if err := cascadeCancelDependents(tx, taskID, result); err != nil {
			return nil, err
		}
	}
	return &data, nil
}

func markCancelling(tx *gorm.DB, taskID, cancelledBy string) (int64, error) {
	now := time.Now()
	res := tx.Model(&Task{}).
		Where("task_id = ? AND status = ?", taskID, TaskStatusRunning).
		Updates(map[string]any{
			"status":       TaskStatusCancelling,
			"cancelled_by": cancelledBy,
			"updated_at":   db.NewFlexibleTime(now),
		})
	return res.RowsAffected, res.Error
}

// cascadeCancelDependents 级联取消直接下游；递归由 markCancelled 驱动，
// 即使下游是 running（落 cancelling、不在本帧级联），其 worker finally 也会兜底。
func cascadeCancelDependents(tx *gorm.DB, taskID string, result *CancelResult) error {
	var rows []Task
	if err := tx.Model(&Task{}).
		Where("dependency_task_id = ?", taskID).
		Order("queued_at ASC").Find(&rows).Error; err != nil {
		return err
	}
	for i := range rows {
		if err := dispatchCancel(tx, &rows[i], "cascade", result); err != nil {
			return err
		}
	}
	return nil
}

// FinalizeCancelled 是 worker finally 的「0 行 = 已取消」协议入口：把
// queued/cancelling/running 任务落 cancelled，并级联下游。返回级联打到的
// running 下游列表，上层据此补发进程内取消信号。
func (r *TaskRepo) FinalizeCancelled(ctx context.Context, taskID, cancelledBy string) (FinalizeResult, error) {
	out := FinalizeResult{Cancelling: []string{}}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := CancelResult{Cancelled: []TaskData{}, Cancelling: out.Cancelling, SkippedTerminal: []TaskData{}}
		data, err := markCancelled(tx, taskID, cancelledBy, &result, true)
		if err != nil {
			return err
		}
		if data != nil {
			out.Rows = 1
		}
		out.Cancelling = result.Cancelling
		return nil
	})
	if err != nil {
		return FinalizeResult{}, err
	}
	return out, nil
}

// FinalizeCancelling 把已进入 cancelling 的任务批量落终态，供无可用 worker 时兜底
// （worker 已退出或租约过期时没人会调用 FinalizeCancelled，任务会永久停在中间态）。
func (r *TaskRepo) FinalizeCancelling(ctx context.Context, taskIDs []string, cancelledBy string) ([]TaskData, error) {
	cancelled := []TaskData{}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, taskID := range taskIDs {
			result := CancelResult{Cancelled: cancelled, Cancelling: []string{}, SkippedTerminal: []TaskData{}}
			if _, err := markCancelled(tx, taskID, cancelledBy, &result, false); err != nil {
				return err
			}
			cancelled = result.Cancelled
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cancelled, nil
}

// ---- 孤儿与救援 ----

// ListOrphanTasksOnStart 返回 running + cancelling 任务用于重启自愈。
func (r *TaskRepo) ListOrphanTasksOnStart(ctx context.Context) ([]TaskData, error) {
	var rows []Task
	if err := r.db.WithContext(ctx).Model(&Task{}).
		Where("status IN ?", []string{TaskStatusRunning, TaskStatusCancelling}).
		Order("updated_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]TaskData, 0, len(rows))
	for i := range rows {
		out = append(out, taskToData(&rows[i]))
	}
	return out, nil
}

// RequeueRunning 救援扳手：批量把 running 任务回队（ops 手动执行；启动自愈不自动调）。
func (r *TaskRepo) RequeueRunning(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 5000 {
		limit = 5000
	}
	var ids []string
	if err := r.db.WithContext(ctx).Model(&Task{}).
		Where("status = ?", TaskStatusRunning).
		Order("updated_at ASC").Limit(limit).Pluck("task_id", &ids).Error; err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	res := r.db.WithContext(ctx).Model(&Task{}).
		Where("task_id IN ? AND status = ?", ids, TaskStatusRunning).
		Updates(map[string]any{
			"status":        TaskStatusQueued,
			"started_at":    nil,
			"finished_at":   nil,
			"updated_at":    db.NewFlexibleTime(time.Now()),
			"result_json":   nil,
			"error_message": nil,
		})
	return res.RowsAffected, res.Error
}

// ---- 执行期身份持久化 ----

// PersistProviderJobID 单独事务持久化 provider_job_id（+提交端点），不带状态守卫
// （worker 内调用，确定是 running）。endpoint 与 job_id 同一次 UPDATE 落地：
// 两者必须同时可见，否则续跑会拿到 job_id 却判不出协议是否被换掉、也无从回放原域名。
// None 时不写该列——保留既有值比清空更安全。
func (r *TaskRepo) PersistProviderJobID(ctx context.Context, taskID, jobID string, endpoint *string) error {
	values := map[string]any{"provider_job_id": jobID, "updated_at": db.NewFlexibleTime(time.Now())}
	if endpoint != nil {
		values["provider_endpoint"] = *endpoint
	}
	return r.db.WithContext(ctx).Model(&Task{}).Where("task_id = ?", taskID).Updates(values).Error
}

// mergePayloadField 把单个字段并入 task.payload 并提交。payload_json 是 TEXT 列存
// JSON 字符串，故用 read-modify-write。并发前提：写 payload 的路径在同一 task 的
// 执行协程内串行（提交前各写一次身份字段，互不并发）。
func (r *TaskRepo) mergePayloadField(ctx context.Context, taskID, key string, value any, raiseIfMissing bool) error {
	var payloads []*string
	// 用 Pluck 而非结构性查询：payload_json 允许 NULL（迁移历史行），
	// 按「行存在但列为 NULL」处理，而不是误判成无行。
	if err := r.db.WithContext(ctx).Model(&Task{}).
		Where("task_id = ?", taskID).Pluck("payload_json", &payloads).Error; err != nil {
		return err
	}
	if len(payloads) == 0 {
		if raiseIfMissing {
			return &TaskNotFoundError{TaskID: taskID}
		}
		return nil
	}
	data := jsonMap(payloads[0])
	data[key] = value
	res := r.db.WithContext(ctx).Model(&Task{}).Where("task_id = ?", taskID).Updates(map[string]any{
		"payload_json": mustJSON(data),
		"updated_at":   db.NewFlexibleTime(time.Now()),
	})
	if res.Error != nil {
		return res.Error
	}
	if raiseIfMissing && res.RowsAffected == 0 {
		return &TaskNotFoundError{TaskID: taskID}
	}
	return nil
}

// PersistAPICallID 把 api_calls.id 写回 payload["api_call_id"]，供 resume 路径精准
// 翻 pending。task 不存在或 0 行命中时报错——避免静默 commit 让上层以为已持久化。
func (r *TaskRepo) PersistAPICallID(ctx context.Context, taskID string, callID int64) error {
	return r.mergePayloadField(ctx, taskID, "api_call_id", callID, true)
}

// PersistEffectiveDuration 把取档后实际申请的秒数写回 payload["duration_seconds"]；
// resume 路径读的就是这个字段。task 不存在时静默跳过（仅是 resume 元数据）。
func (r *TaskRepo) PersistEffectiveDuration(ctx context.Context, taskID string, durationSeconds int64) error {
	return r.mergePayloadField(ctx, taskID, "duration_seconds", durationSeconds, false)
}

// PersistExecutionProviderID 把执行期实际解析出的 provider 写回 provider_id 列。
// 参考路线退化镜头在执行期按实际参考图分桶，投影可能与入队值不同；resume 按该列
// 锁定 backend 轮询，不写回会拿投影 backend 的 job_id 去实际 backend 轮询。
func (r *TaskRepo) PersistExecutionProviderID(ctx context.Context, taskID, providerID string) error {
	return r.db.WithContext(ctx).Model(&Task{}).Where("task_id = ?", taskID).Updates(map[string]any{
		"provider_id": providerID,
		"updated_at":  db.NewFlexibleTime(time.Now()),
	}).Error
}

// PersistExecutionIdentity 同一次提交内写 provider_id 列并按 patch 改写 payload：
// 值为 nil 表示删除该键，其余键覆盖写入。task 不存在时静默返回。
func (r *TaskRepo) PersistExecutionIdentity(ctx context.Context, taskID, providerID string, patch map[string]any) error {
	var payloads []*string
	if err := r.db.WithContext(ctx).Model(&Task{}).
		Where("task_id = ?", taskID).Pluck("payload_json", &payloads).Error; err != nil {
		return err
	}
	values := map[string]any{"provider_id": providerID, "updated_at": db.NewFlexibleTime(time.Now())}
	if len(payloads) > 0 {
		data := jsonMap(payloads[0])
		for key, value := range patch {
			if value == nil {
				delete(data, key)
			} else {
				data[key] = value
			}
		}
		values["payload_json"] = mustJSON(data)
	}
	return r.db.WithContext(ctx).Model(&Task{}).Where("task_id = ?", taskID).Updates(values).Error
}

// ---- 查询 ----

// Get 按 ID 取任务；不存在返回 nil, nil。
func (r *TaskRepo) Get(ctx context.Context, taskID string) (*TaskData, error) {
	var row Task
	err := r.db.WithContext(ctx).Where("task_id = ?", taskID).Take(&row).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data := taskToData(&row)
	return &data, nil
}

// ListTasksParams 是列表过滤与分页参数。
type ListTasksParams struct {
	ProjectName *string
	Status      *string
	TaskType    *string
	Source      *string
	Page        int
	PageSize    int
}

// TaskListResult 是分页结果。
type TaskListResult struct {
	Items    []TaskData `json:"items"`
	Total    int64      `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
}

// ListTasks 分页列出任务，按 updated_at DESC, queued_at DESC 定序。
func (r *TaskRepo) ListTasks(ctx context.Context, p ListTasksParams) (TaskListResult, error) {
	page := p.Page
	if page < 1 {
		page = 1
	}
	pageSize := p.PageSize
	if pageSize < 1 {
		pageSize = 1
	}
	if pageSize > 500 {
		pageSize = 500
	}
	q := r.db.WithContext(ctx).Model(&Task{})
	if p.ProjectName != nil {
		q = q.Where("project_name = ?", *p.ProjectName)
	}
	if p.Status != nil {
		q = q.Where("status = ?", *p.Status)
	}
	if p.TaskType != nil {
		q = q.Where("task_type = ?", *p.TaskType)
	}
	if p.Source != nil {
		q = q.Where("source = ?", *p.Source)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return TaskListResult{}, err
	}
	var rows []Task
	if err := q.Order("updated_at DESC, queued_at DESC").
		Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error; err != nil {
		return TaskListResult{}, err
	}
	items := make([]TaskData, 0, len(rows))
	for i := range rows {
		items = append(items, taskToData(&rows[i]))
	}
	return TaskListResult{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// TaskStats 是按状态聚合的任务统计。
type TaskStats struct {
	Queued     int64 `json:"queued"`
	Running    int64 `json:"running"`
	Cancelling int64 `json:"cancelling"`
	Succeeded  int64 `json:"succeeded"`
	Failed     int64 `json:"failed"`
	Cancelled  int64 `json:"cancelled"`
	Total      int64 `json:"total"`
}

// GetStats 聚合任务统计；total 覆盖所有状态（含未知状态行）。
func (r *TaskRepo) GetStats(ctx context.Context, projectName *string) (TaskStats, error) {
	q := r.db.WithContext(ctx).Model(&Task{})
	if projectName != nil {
		q = q.Where("project_name = ?", *projectName)
	}
	var rows []struct {
		Status string
		Cnt    int64
	}
	if err := q.Select("status, COUNT(*) AS cnt").Group("status").Scan(&rows).Error; err != nil {
		return TaskStats{}, err
	}
	var stats TaskStats
	for _, row := range rows {
		switch row.Status {
		case TaskStatusQueued:
			stats.Queued = row.Cnt
		case TaskStatusRunning:
			stats.Running = row.Cnt
		case TaskStatusCancelling:
			stats.Cancelling = row.Cnt
		case TaskStatusSucceeded:
			stats.Succeeded = row.Cnt
		case TaskStatusFailed:
			stats.Failed = row.Cnt
		case TaskStatusCancelled:
			stats.Cancelled = row.Cnt
		}
		stats.Total += row.Cnt
	}
	return stats, nil
}

// ---- Worker 租约 ----

// AcquireOrRenewLease 抢占或续约 worker 租约（ttl 秒）。fast path 只在「自己持有
// 或已过期」时更新；慢路径 INSERT 抢锁，撞唯一约束说明竞争失败。
func (r *TaskRepo) AcquireOrRenewLease(ctx context.Context, name, ownerID string, ttl float64) (bool, error) {
	nowEpoch := float64(time.Now().UnixNano()) / 1e9
	if ttl < 1.0 {
		ttl = 1.0
	}
	leaseUntil := nowEpoch + ttl
	res := r.db.WithContext(ctx).Model(&WorkerLease{}).
		Where("name = ? AND (owner_id = ? OR lease_until <= ?)", name, ownerID, nowEpoch).
		Updates(map[string]any{
			"owner_id":    ownerID,
			"lease_until": leaseUntil,
			"updated_at":  db.NewFlexibleTime(time.Now()),
		})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected > 0 {
		return true, nil
	}
	lease := WorkerLease{Name: name, OwnerID: ownerID, LeaseUntil: leaseUntil, UpdatedAt: db.NewFlexibleTime(time.Now())}
	if err := r.db.WithContext(ctx).Create(&lease).Error; err != nil {
		if db.IsUniqueViolation(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ReleaseLease 释放自己持有的租约。
func (r *TaskRepo) ReleaseLease(ctx context.Context, name, ownerID string) error {
	return r.db.WithContext(ctx).
		Where("name = ? AND owner_id = ?", name, ownerID).
		Delete(&WorkerLease{}).Error
}

// IsWorkerOnline 判断 default worker 是否在线（租约未过期）。
func (r *TaskRepo) IsWorkerOnline(ctx context.Context, name string) (bool, error) {
	var rows []float64
	if err := r.db.WithContext(ctx).Model(&WorkerLease{}).
		Where("name = ?", name).Pluck("lease_until", &rows).Error; err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	return rows[0] > float64(time.Now().UnixNano())/1e9, nil
}

// WorkerLeaseInfo 是租约的查询投影。
type WorkerLeaseInfo struct {
	Name       string  `json:"name"`
	OwnerID    string  `json:"owner_id"`
	LeaseUntil float64 `json:"lease_until"`
	UpdatedAt  *string `json:"updated_at"`
	IsOnline   bool    `json:"is_online"`
}

// GetWorkerLease 返回租约详情；无租约行返回 nil, nil。
func (r *TaskRepo) GetWorkerLease(ctx context.Context, name string) (*WorkerLeaseInfo, error) {
	var row WorkerLease
	err := r.db.WithContext(ctx).Where("name = ?", name).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	isOnline := row.LeaseUntil > float64(time.Now().UnixNano())/1e9
	return &WorkerLeaseInfo{
		Name:       row.Name,
		OwnerID:    row.OwnerID,
		LeaseUntil: row.LeaseUntil,
		UpdatedAt:  isoOrNil(row.UpdatedAt),
		IsOnline:   isOnline,
	}, nil
}
