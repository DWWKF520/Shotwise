package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/domain/task"
	"github.com/shotwise/shotwise/internal/infra/db"
)

// 生成任务队列的持久化层（对应 lib/db/repositories/task_repo.py）。
//
// 六条不变式在这里落地：
//  1. 去重是 DB 部分唯一索引（idx_tasks_dedupe_active），冲突后回查既有任务，
//     不是应用层 SELECT-then-INSERT。
//  2. mark_succeeded / mark_failed 返回受影响行数，0 行 = 外部已翻状态，不当错误。
//  3. cancelling 是中间态，必须有收口（finalize_cancelled / finalize_cancelling）。
//  4. 池满过滤用黑名单，provider_id IS NULL 必须放行。
//  5. resume 绝不重新 submit（由执行层保证），这里只提供 provider_job_id 的持久化。
//  6. 无优先级：claim 严格按 queued_at FIFO。

// 任务状态。终态是 succeeded / failed / cancelled；cancelling 是活跃中间态。
const (
	TaskStatusQueued     = "queued"
	TaskStatusRunning    = "running"
	TaskStatusCancelling = "cancelling"
	TaskStatusSucceeded  = "succeeded"
	TaskStatusFailed     = "failed"
	TaskStatusCancelled  = "cancelled"
)

// DefaultUserID 与 Python 侧 DEFAULT_USER_ID 对齐（单用户模式）。
const DefaultUserID = "default"

// ActiveTaskStatuses 是占用去重键的活跃状态集合。
var ActiveTaskStatuses = []string{TaskStatusQueued, TaskStatusRunning, TaskStatusCancelling}

// TerminalTaskStatuses 与前端 isTerminalStatus 同口径。
var TerminalTaskStatuses = map[string]struct{}{
	TaskStatusSucceeded: {},
	TaskStatusFailed:    {},
	TaskStatusCancelled: {},
}

// TerminalEvent 是任务终态迁移的记账，供上层在事务提交后统一发项目事件。
// 各终态方法各自返回事件而非 repo 持有状态：Go 侧避免隐式 per-call 状态。
type TerminalEvent struct {
	TaskID      string
	ProjectName string
	Status      string
	TaskType    string
}

// TaskNotFoundError 表示任务不存在。
type TaskNotFoundError struct{ TaskID string }

func (e *TaskNotFoundError) Error() string { return "task not found: " + e.TaskID }

// ErrTaskNotFound 供 errors.Is 使用。
var ErrTaskNotFound = errors.New("task not found")

// TaskData 是任务的对外投影，字段与 Python _task_to_dict 一一对应。
// payload 里的 prompt 预览快照在此被拆出为独立字段。
type TaskData struct {
	TaskID           string         `json:"task_id"`
	ProjectName      string         `json:"project_name"`
	TaskType         string         `json:"task_type"`
	MediaType        string         `json:"media_type"`
	ResourceID       string         `json:"resource_id"`
	ResourceType     *string        `json:"resource_type"`
	ScriptFile       *string        `json:"script_file"`
	Payload          map[string]any `json:"payload"`
	PromptPreview    map[string]any `json:"prompt_preview"`
	Status           string         `json:"status"`
	Result           map[string]any `json:"result"`
	ErrorMessage     *string        `json:"error_message"`
	Source           string         `json:"source"`
	DependencyTaskID *string        `json:"dependency_task_id"`
	DependencyGroup  *string        `json:"dependency_group"`
	DependencyIndex  *int64         `json:"dependency_index"`
	CancelledBy      *string        `json:"cancelled_by"`
	ProviderID       *string        `json:"provider_id"`
	ProviderJobID    *string        `json:"provider_job_id"`
	ProviderEndpoint *string        `json:"provider_endpoint"`
	Progress         *float64       `json:"progress"`
	ProgressSource   *string        `json:"progress_source"`
	PhaseCode        *string        `json:"phase_code"`
	LeaseUntil       *string        `json:"lease_until"`
	QueuedAt         *string        `json:"queued_at"`
	StartedAt        *string        `json:"started_at"`
	FinishedAt       *string        `json:"finished_at"`
	UpdatedAt        *string        `json:"updated_at"`
	UserID           string         `json:"user_id"`
}

// TaskRepo 承载 tasks / worker_lease 两张表的操作。
type TaskRepo struct {
	db *gorm.DB
}

func NewTaskRepo(gdb *gorm.DB) *TaskRepo { return &TaskRepo{db: gdb} }

// NewTaskID 生成 32 位十六进制任务 ID（与 Python uuid4().hex 同形）。
func NewTaskID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand 失败在进程级已经不可继续；退化为纳秒计数保证唯一性仍可用。
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// ---- 投影 ----

func taskToData(row *Task) TaskData {
	payload := map[string]any{}
	if row.PayloadJSON != nil && *row.PayloadJSON != "" {
		_ = json.Unmarshal([]byte(*row.PayloadJSON), &payload)
	}
	var preview map[string]any
	if v, ok := payload[task.PromptPreviewPayloadKey]; ok {
		if m, ok := v.(map[string]any); ok {
			preview = m
		}
		delete(payload, task.PromptPreviewPayloadKey)
	}
	result := map[string]any{}
	if row.ResultJSON != nil && *row.ResultJSON != "" {
		_ = json.Unmarshal([]byte(*row.ResultJSON), &result)
	}
	return TaskData{
		TaskID:           row.TaskID,
		ProjectName:      row.ProjectName,
		TaskType:         row.TaskType,
		MediaType:        row.MediaType,
		ResourceID:       row.ResourceID,
		ResourceType:     row.ResourceType,
		ScriptFile:       row.ScriptFile,
		Payload:          payload,
		PromptPreview:    preview,
		Status:           row.Status,
		Result:           result,
		ErrorMessage:     row.ErrorMessage,
		Source:           row.Source,
		DependencyTaskID: row.DependencyTaskID,
		DependencyGroup:  row.DependencyGroup,
		DependencyIndex:  row.DependencyIndex,
		CancelledBy:      row.CancelledBy,
		ProviderID:       row.ProviderID,
		ProviderJobID:    row.ProviderJobID,
		ProviderEndpoint: row.ProviderEndpoint,
		Progress:         row.Progress,
		ProgressSource:   row.ProgressSource,
		PhaseCode:        row.PhaseCode,
		LeaseUntil:       isoOrNil(row.LeaseUntil),
		QueuedAt:         isoOrNil(row.QueuedAt),
		StartedAt:        isoOrNil(row.StartedAt),
		FinishedAt:       isoOrNil(row.FinishedAt),
		UpdatedAt:        isoOrNil(row.UpdatedAt),
		UserID:           row.UserID,
	}
}

// isoOrNil 输出 Python datetime.isoformat() 形态的 UTC 串（微秒精度、+00:00 时区）；
// 空值输出 nil（JSON null）。
func isoOrNil(ft db.FlexibleTime) *string {
	if !ft.Valid {
		return nil
	}
	t := ft.Time.UTC().Truncate(time.Microsecond)
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	_, offset := t.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	s += fmt.Sprintf("%s%02d:%02d", sign, offset/3600, (offset%3600)/60)
	return &s
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func jsonMap(raw *string) map[string]any {
	out := map[string]any{}
	if raw != nil && *raw != "" {
		_ = json.Unmarshal([]byte(*raw), &out)
	}
	return out
}

// ---- 入队 ----

// EnqueueParams 是入队参数。
type EnqueueParams struct {
	ProjectName      string
	TaskType         string
	MediaType        string
	ResourceID       string
	Payload          map[string]any
	ScriptFile       *string
	ResourceType     *string
	Source           string
	DependencyTaskID *string
	DependencyGroup  *string
	DependencyIndex  *int64
	UserID           string
	ProviderID       *string
}

// EnqueueResult 是入队结果；冲突去重时 Deduped=true 并带上既有任务 ID。
type EnqueueResult struct {
	TaskID         string  `json:"task_id"`
	Status         string  `json:"status"`
	Deduped        bool    `json:"deduped"`
	ExistingTaskID *string `json:"existing_task_id"`
}

// coerce 把可空字符串归一为空串（与索引表达式里的 COALESCE 对齐）。
func coerce(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// activeDedupeQuery 是 “idx_tasks_dedupe_active“ 去重键中与 resource_id 无关的部分，
// enqueue 的冲突回查与 GetActiveTasksForResources 共用这一份，去重口径只有一处定义。
func (r *TaskRepo) activeDedupeQuery(ctx context.Context, projectName, taskType, scriptFile, resourceType string) *gorm.DB {
	return r.db.WithContext(ctx).Model(&Task{}).Where(
		"project_name = ? AND task_type = ? AND COALESCE(script_file, '') = ? AND COALESCE(resource_type, '') = ? AND status IN ?",
		projectName, taskType, scriptFile, resourceType, ActiveTaskStatuses)
}

func (r *TaskRepo) insertTaskRow(ctx context.Context, row *Task) error {
	return r.db.WithContext(ctx).Create(row).Error
}

// Enqueue 单条入队。去重冲突时按 queued_at DESC 回查 1 条既有活跃任务返回，
// 而不是报错——活跃态占用去重键，落到终态后同键可再次入队。
func (r *TaskRepo) Enqueue(ctx context.Context, p EnqueueParams) (EnqueueResult, error) {
	payload := make(map[string]any, len(p.Payload)+1)
	for k, v := range p.Payload {
		payload[k] = v
	}
	payload[task.PromptPreviewPayloadKey] = task.BuildEnqueuePromptPreview(
		p.ProjectName, p.TaskType, p.MediaType, p.ResourceID, p.ScriptFile, p.ProviderID, payload)
	payloadJSON := mustJSON(payload)

	source := p.Source
	if source == "" {
		source = "webui"
	}
	userID := p.UserID
	if userID == "" {
		userID = DefaultUserID
	}
	now := db.NewFlexibleTime(time.Now())
	row := &Task{
		TaskID:           NewTaskID(),
		ProjectName:      p.ProjectName,
		TaskType:         p.TaskType,
		MediaType:        p.MediaType,
		ResourceID:       p.ResourceID,
		ScriptFile:       p.ScriptFile,
		ResourceType:     p.ResourceType,
		PayloadJSON:      &payloadJSON,
		Status:           TaskStatusQueued,
		Source:           source,
		DependencyTaskID: p.DependencyTaskID,
		DependencyGroup:  p.DependencyGroup,
		DependencyIndex:  p.DependencyIndex,
		ProviderID:       p.ProviderID,
		QueuedAt:         now,
		UpdatedAt:        now,
		UserID:           userID,
	}
	if err := r.insertTaskRow(ctx, row); err != nil {
		if !db.IsUniqueViolation(err) {
			return EnqueueResult{}, err
		}
		var existing Task
		q := r.activeDedupeQuery(ctx, p.ProjectName, p.TaskType, coerce(p.ScriptFile), coerce(p.ResourceType)).
			Where("resource_id = ?", p.ResourceID).
			Order("queued_at DESC").Limit(1)
		if findErr := q.Take(&existing).Error; findErr != nil {
			// 回查不到说明冲突不是去重索引造成的（例如理论上的 task_id 撞车），原样抛。
			return EnqueueResult{}, err
		}
		existingID := existing.TaskID
		return EnqueueResult{TaskID: existingID, Status: existing.Status, Deduped: true, ExistingTaskID: &existingID}, nil
	}
	return EnqueueResult{TaskID: row.TaskID, Status: TaskStatusQueued}, nil
}

// EnqueueBatch 原子入队一批任务：冲突即整批回滚，**不**回退到既有任务。
// 这是批量准入（prepare_all/admit_all）的边界语义。
func (r *TaskRepo) EnqueueBatch(ctx context.Context, specs []EnqueueParams) ([]EnqueueResult, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	now := db.NewFlexibleTime(time.Now())
	rows := make([]*Task, 0, len(specs))
	for _, spec := range specs {
		missing := ""
		for name, value := range map[string]string{
			"project_name": spec.ProjectName,
			"task_type":    spec.TaskType,
			"media_type":   spec.MediaType,
			"resource_id":  spec.ResourceID,
		} {
			if strings.TrimSpace(value) == "" {
				if missing != "" {
					missing += ", "
				}
				missing += name
			}
		}
		if missing != "" {
			return nil, fmt.Errorf("batch task missing required fields: %s", missing)
		}
		payload := make(map[string]any, len(spec.Payload)+1)
		for k, v := range spec.Payload {
			payload[k] = v
		}
		payload[task.PromptPreviewPayloadKey] = task.BuildEnqueuePromptPreview(
			spec.ProjectName, spec.TaskType, spec.MediaType, spec.ResourceID, spec.ScriptFile, spec.ProviderID, payload)
		payloadJSON := mustJSON(payload)
		source := spec.Source
		if source == "" {
			source = "webui"
		}
		userID := spec.UserID
		if userID == "" {
			userID = DefaultUserID
		}
		rows = append(rows, &Task{
			TaskID:           NewTaskID(),
			ProjectName:      spec.ProjectName,
			TaskType:         spec.TaskType,
			MediaType:        spec.MediaType,
			ResourceID:       spec.ResourceID,
			ScriptFile:       spec.ScriptFile,
			ResourceType:     spec.ResourceType,
			PayloadJSON:      &payloadJSON,
			Status:           TaskStatusQueued,
			Source:           source,
			DependencyTaskID: spec.DependencyTaskID,
			DependencyGroup:  spec.DependencyGroup,
			DependencyIndex:  spec.DependencyIndex,
			ProviderID:       spec.ProviderID,
			QueuedAt:         now,
			UpdatedAt:        now,
			UserID:           userID,
		})
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return tx.Create(&rows).Error
	})
	if err != nil {
		return nil, err
	}
	results := make([]EnqueueResult, 0, len(rows))
	for _, row := range rows {
		results = append(results, EnqueueResult{TaskID: row.TaskID, Status: TaskStatusQueued})
	}
	return results, nil
}

// GetActiveTasksForResources 查询命中去重键、当前处于活动态的任务，
// 供调用方在真正入队前探测冲突并拒绝。结果按入队时间定序。
func (r *TaskRepo) GetActiveTasksForResources(
	ctx context.Context, projectName, taskType string, resourceIDs []string, scriptFile, resourceType *string,
) ([]TaskData, error) {
	if len(resourceIDs) == 0 {
		return nil, nil
	}
	var rows []Task
	q := r.activeDedupeQuery(ctx, projectName, taskType, coerce(scriptFile), coerce(resourceType)).
		Where("resource_id IN ?", resourceIDs).
		Order("queued_at ASC, task_id ASC")
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]TaskData, 0, len(rows))
	for i := range rows {
		out = append(out, taskToData(&rows[i]))
	}
	return out, nil
}

// ---- 认领 ----

// ClaimNext 领取下一个 queued 任务（严格 queued_at FIFO，无优先级）。
//
// poolFullProviders 是本轮已知池满的 provider 集合（黑名单语义）：只排除这些
// provider 的任务；provider_id IS NULL 的老数据与未知 provider 照常 claim，
// 交 worker 二次解析（解析失败走 mark_failed 兜底，不会静默堆积）。
func (r *TaskRepo) ClaimNext(ctx context.Context, mediaType string, poolFullProviders []string) (*TaskData, error) {
	now := time.Now()
	var sb strings.Builder
	sb.WriteString("SELECT tasks.task_id FROM tasks " +
		"LEFT JOIN tasks AS dependency ON dependency.task_id = tasks.dependency_task_id " +
		"WHERE tasks.status = 'queued' AND tasks.media_type = ?")
	args := []any{mediaType}
	if len(poolFullProviders) > 0 {
		sb.WriteString(" AND (tasks.provider_id IS NULL OR tasks.provider_id NOT IN ?)")
		args = append(args, poolFullProviders)
	}
	sb.WriteString(" AND (tasks.dependency_task_id IS NULL OR dependency.status = 'succeeded') " +
		"ORDER BY tasks.queued_at ASC LIMIT 1")

	var picked []struct{ TaskID string }
	if err := r.db.WithContext(ctx).Raw(sb.String(), args...).Scan(&picked).Error; err != nil {
		return nil, err
	}
	if len(picked) == 0 {
		return nil, nil
	}
	target := picked[0].TaskID

	res := r.db.WithContext(ctx).Model(&Task{}).
		Where("task_id = ? AND status = ?", target, TaskStatusQueued).
		Updates(map[string]any{
			"status":     TaskStatusRunning,
			"started_at": db.NewFlexibleTime(now),
			"updated_at": db.NewFlexibleTime(now),
		})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		// 竞态：SELECT 与 UPDATE 之间被其他 worker 抢先认领。
		return nil, nil
	}
	var task Task
	if err := r.db.WithContext(ctx).Where("task_id = ?", target).Take(&task).Error; err != nil {
		return nil, err
	}
	data := taskToData(&task)
	return &data, nil
}

// ---- 终态 ----

func terminalEvent(row *Task, status string) TerminalEvent {
	if _, ok := TerminalTaskStatuses[status]; !ok {
		// 非终态是空操作：状态守卫兜住未来新增的非终态调用点。
		return TerminalEvent{}
	}
	return TerminalEvent{TaskID: row.TaskID, ProjectName: row.ProjectName, Status: status, TaskType: row.TaskType}
}

// MarkSucceeded 把 running 任务翻 succeeded；返回受影响行数与终态事件。
// rows=0 表示外部已把 DB 翻成 cancelling/cancelled/failed 等状态，
// worker finally 应据此走「0 行 = 已取消」协议，而不是当错误处理。
func (r *TaskRepo) MarkSucceeded(ctx context.Context, taskID string, result map[string]any) (int64, *TerminalEvent, error) {
	now := time.Now()
	updates := map[string]any{
		"status":        TaskStatusSucceeded,
		"result_json":   mustJSON(result),
		"error_message": nil,
		"finished_at":   db.NewFlexibleTime(now),
		"updated_at":    db.NewFlexibleTime(now),
	}
	res := r.db.WithContext(ctx).Model(&Task{}).
		Where("task_id = ? AND status = ?", taskID, TaskStatusRunning).Updates(updates)
	if res.Error != nil {
		return 0, nil, res.Error
	}
	if res.RowsAffected == 0 {
		return 0, nil, nil
	}
	var row Task
	if err := r.db.WithContext(ctx).Where("task_id = ?", taskID).Take(&row).Error; err != nil {
		return res.RowsAffected, nil, err
	}
	event := terminalEvent(&row, TaskStatusSucceeded)
	return res.RowsAffected, &event, nil
}

// MarkFailed 把 running 任务翻 failed，并级联把依赖它的 queued 下游一并标失败。
// error_message 先清洗（凭证/绝对路径）再裁剪；级联原因折叠后按预算裁剪，
// 保证落库的始终是合法信封。
func (r *TaskRepo) MarkFailed(ctx context.Context, taskID, errorMessage string) (int64, []TerminalEvent, error) {
	sanitized := task.SanitizeFailureReason(errorMessage)
	var events []TerminalEvent
	var affected int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		affected, err = markFailedRunning(tx, taskID, sanitized)
		if err != nil || affected == 0 {
			return err
		}
		if ev, err := terminalEventByTask(tx, taskID, TaskStatusFailed); err != nil {
			return err
		} else if ev != nil {
			events = append(events, *ev)
		}
		cascaded, err := cascadeFailedQueued(tx, taskID, sanitized)
		if err != nil {
			return err
		}
		events = append(events, cascaded...)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return affected, events, nil
}

func markFailedRunning(tx *gorm.DB, taskID, errorMessage string) (int64, error) {
	now := time.Now()
	res := tx.Model(&Task{}).
		Where("task_id = ? AND status = ?", taskID, TaskStatusRunning).
		Updates(map[string]any{
			"status":        TaskStatusFailed,
			"error_message": task.BoundReason(task.SanitizeFailureReason(errorMessage), task.MaxErrorMessageLen),
			"finished_at":   db.NewFlexibleTime(now),
			"updated_at":    db.NewFlexibleTime(now),
		})
	return res.RowsAffected, res.Error
}

func markFailedQueuedDep(tx *gorm.DB, taskID, errorMessage string) (int64, error) {
	now := time.Now()
	res := tx.Model(&Task{}).
		Where("task_id = ? AND status = ?", taskID, TaskStatusQueued).
		Updates(map[string]any{
			"status":        TaskStatusFailed,
			"error_message": task.BoundReason(task.SanitizeFailureReason(errorMessage), task.MaxErrorMessageLen),
			"finished_at":   db.NewFlexibleTime(now),
			"updated_at":    db.NewFlexibleTime(now),
		})
	return res.RowsAffected, res.Error
}

// cascadeFailedQueued 按依赖链级联标失败：rootReason 沿链条原样传递（根因，不随层数
// 重新嵌套），每层只把自己的直接阻塞方 task_id 写进编码。
func cascadeFailedQueued(tx *gorm.DB, taskID, rootReason string) ([]TerminalEvent, error) {
	var dependentIDs []string
	if err := tx.Model(&Task{}).
		Where("dependency_task_id = ? AND status = ?", taskID, TaskStatusQueued).
		Order("queued_at ASC").Pluck("task_id", &dependentIDs).Error; err != nil {
		return nil, err
	}
	var events []TerminalEvent
	for _, depID := range dependentIDs {
		blocked := encodeBoundedCascadeFailure(taskID, rootReason)
		affected, err := markFailedQueuedDep(tx, depID, blocked)
		if err != nil {
			return nil, err
		}
		if affected == 0 {
			continue
		}
		if ev, err := terminalEventByTask(tx, depID, TaskStatusFailed); err != nil {
			return nil, err
		} else if ev != nil {
			events = append(events, *ev)
		}
		nested, err := cascadeFailedQueued(tx, depID, rootReason)
		if err != nil {
			return nil, err
		}
		events = append(events, nested...)
	}
	return events, nil
}

// encodeBoundedCascadeFailure 生成级联失败信封并保证落库长度不超上限。
// 逐层包裹近指数增长，先折叠到根本原因；re-encode 循环按实际超额收敛，
// 避免转义（引号/反斜杠）导致一次估算砍不准。
func encodeBoundedCascadeFailure(dependencyTaskID, reason string) string {
	reason = task.SanitizeFailureReason(reason)
	reason = task.CollapseCascadeReason(reason)
	params := map[string]any{"dependency_task_id": dependencyTaskID, "reason": reason}
	encoded, err := task.EncodeFailure(task.CascadeFailureCode, params)
	if err != nil {
		return ""
	}
	if utf8.RuneCountInString(encoded) <= task.MaxErrorMessageLen {
		return encoded
	}
	overhead, _ := task.EncodeFailure(task.CascadeFailureCode,
		map[string]any{"dependency_task_id": dependencyTaskID, "reason": ""})
	budget := task.MaxErrorMessageLen - utf8.RuneCountInString(overhead)
	if budget < 0 {
		budget = 0
	}
	for range 5 {
		reason = task.BoundReason(reason, budget)
		params["reason"] = reason
		encoded, err = task.EncodeFailure(task.CascadeFailureCode, params)
		if err != nil {
			return ""
		}
		overflow := utf8.RuneCountInString(encoded) - task.MaxErrorMessageLen
		if overflow <= 0 || budget <= 0 {
			break
		}
		budget -= overflow
		if budget < 0 {
			budget = 0
		}
	}
	return encoded
}

func terminalEventByTask(tx *gorm.DB, taskID, status string) (*TerminalEvent, error) {
	var row Task
	if err := tx.Where("task_id = ?", taskID).Take(&row).Error; err != nil {
		return nil, err
	}
	event := terminalEvent(&row, status)
	return &event, nil
}
