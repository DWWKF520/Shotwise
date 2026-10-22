// 批量生成的控制面（对应 lib/generation_batches.py + lib/generation_queue.py 的批次适配器
// + server/routers/batches.py）。
//
// 设计要点（照抄 Python 的三条约束）：
//   - 批次不持有任何 worker 状态；执行真相源仍是 generation_tasks，批次只投影出展示状态；
//   - 准入是原子的：所有任务在**一个事务**里插入，任一去重冲突则整批回滚；
//   - 入参归一先于事务：TaskSpec 校验与载荷构造都在开事务之前完成，失败不留半批数据。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
)

// BatchStatus 是由任务状态派生出的控制面状态。
type BatchStatus string

const (
	BatchAdmitted         BatchStatus = "admitted"
	BatchRunning          BatchStatus = "running"
	BatchSucceeded        BatchStatus = "succeeded"
	BatchPartiallySuccess BatchStatus = "partially_succeeded"
	BatchFailed           BatchStatus = "failed"
	BatchCancelled        BatchStatus = "cancelled"
)

// BatchNotFoundError 表示批次不存在（404 batch_not_found）。
type BatchNotFoundError struct{}

func (e *BatchNotFoundError) Error() string { return "batch_not_found" }

// BatchAdmissionError 表示批次无法原子准入（409 batch_admission_failed / batch_retry_failed）。
type BatchAdmissionError struct{ Detail string }

func (e *BatchAdmissionError) Error() string { return e.Detail }

// GenerationBatchItem 是批次里的一个任务请求，item_id 与队列 task_id 解耦。
type GenerationBatchItem struct {
	ItemID string         `json:"item_id"`
	Task   map[string]any `json:"task"`
}

// GenerationBatch 是可持久化的批次元数据（不含任何执行态）。
type GenerationBatch struct {
	BatchID         string
	ProjectName     string
	Items           []GenerationBatchItem
	TaskIDs         []string
	CancelRequested bool
	UserID          string
	AdmissionState  string
	ErrorMessage    *string
}

// Tasks 把 item_id 与 task_id 一一配对（长度必然一致）。
func (b GenerationBatch) Tasks() []map[string]any {
	out := make([]map[string]any, 0, len(b.Items))
	for index, item := range b.Items {
		taskID := ""
		if index < len(b.TaskIDs) {
			taskID = b.TaskIDs[index]
		}
		out = append(out, map[string]any{"item": item, "task_id": taskID})
	}
	return out
}

// BatchTaskQueue 是批次需要的队列能力（*queue.Queue 满足）。
type BatchTaskQueue interface {
	GetTask(ctx context.Context, taskID string) (*repository.TaskData, error)
	CancelTask(ctx context.Context, taskID string) (repository.CancelResult, error)
}

// BatchTaskAdapter 把控制面接到既有任务/队列面。
type BatchTaskAdapter struct {
	repo        *repository.TaskRepo
	queue       BatchTaskQueue
	projectName string
}

// NewBatchTaskAdapter 构造适配器；projectName 用于补齐条目里省略的重复项目字段。
func NewBatchTaskAdapter(repo *repository.TaskRepo, queue BatchTaskQueue, projectName string) *BatchTaskAdapter {
	return &BatchTaskAdapter{repo: repo, queue: queue, projectName: projectName}
}

// Validate 校验单条任务（对应 GenerationQueueBatchAdapter.validate）。
func (a *BatchTaskAdapter) Validate(task map[string]any) []string {
	issues := []string{}
	project := strings.TrimSpace(taskString(task, "project_name"))
	if project == "" {
		project = a.projectName
	}
	if project == "" {
		issues = append(issues, "project_name is required")
	} else if a.projectName != "" && project != a.projectName {
		issues = append(issues, "project_name does not match adapter")
	}
	for _, field := range []string{"task_type", "media_type", "resource_id"} {
		if strings.TrimSpace(taskString(task, field)) == "" {
			issues = append(issues, field+" is required")
		}
	}
	rawPayload, hasPayload := task["payload"]
	if hasPayload && rawPayload != nil {
		if _, ok := rawPayload.(map[string]any); !ok {
			issues = append(issues, "payload must be an object")
		}
	}
	scriptFile := taskString(task, "script_file")
	if payload, ok := rawPayload.(map[string]any); ok && scriptFile == "" {
		scriptFile = taskString(payload, "script_file")
	}
	switch taskString(task, "task_type") {
	case "storyboard", "video", "tts":
		if strings.TrimSpace(scriptFile) == "" {
			issues = append(issues, fmt.Sprintf("script_file is required for %s batch task", taskString(task, "task_type")))
		}
	}
	return issues
}

// PrepareAll 把批次条目归一为 EnqueueParams（事务外完成，失败不留半批）。
func (a *BatchTaskAdapter) PrepareAll(tasks []map[string]any, userID string) ([]repository.EnqueueParams, error) {
	if len(tasks) == 0 {
		return nil, &BatchAdmissionError{Detail: "batch must contain at least one task"}
	}
	out := make([]repository.EnqueueParams, 0, len(tasks))
	for _, task := range tasks {
		if issues := a.Validate(task); len(issues) > 0 {
			return nil, &BatchAdmissionError{Detail: strings.Join(issues, "; ")}
		}
		params, err := a.prepareTask(task, userID)
		if err != nil {
			return nil, err
		}
		out = append(out, params)
	}
	return out, nil
}

// prepareTask 复用单任务入队的同一套结构守卫（TaskSpec），规则不分叉。
func (a *BatchTaskAdapter) prepareTask(task map[string]any, userID string) (repository.EnqueueParams, error) {
	project := taskString(task, "project_name")
	if project == "" {
		project = a.projectName
	}
	if project == "" {
		return repository.EnqueueParams{}, &BatchAdmissionError{Detail: "project_name is required"}
	}
	if a.projectName != "" && project != a.projectName {
		return repository.EnqueueParams{}, &BatchAdmissionError{Detail: "project_name does not match adapter"}
	}
	rawPayload := task["payload"]
	var payload map[string]any
	if rawPayload != nil {
		typed, ok := rawPayload.(map[string]any)
		if !ok {
			return repository.EnqueueParams{}, &BatchAdmissionError{Detail: "payload must be an object"}
		}
		payload = typed
	}
	topScriptFile := taskString(task, "script_file")
	payloadScriptFile := taskString(payload, "script_file")
	if topScriptFile != "" && payloadScriptFile != "" && topScriptFile != payloadScriptFile {
		return repository.EnqueueParams{}, &BatchAdmissionError{Detail: "script_file differs between task and payload"}
	}
	scriptFile := topScriptFile
	if scriptFile == "" {
		scriptFile = payloadScriptFile
	}
	extra := map[string]any{}
	for key, value := range payload {
		if key == "prompt" || key == "script_file" {
			continue
		}
		extra[key] = value
	}
	var scriptPtr *string
	if scriptFile != "" {
		scriptPtr = &scriptFile
	}
	spec, err := BuildTaskSpec(
		taskString(task, "task_type"), taskString(task, "media_type"), taskString(task, "resource_id"),
		payload["prompt"], scriptPtr, extra)
	if err != nil {
		return repository.EnqueueParams{}, &BatchAdmissionError{Detail: err.Error()}
	}
	params := repository.EnqueueParams{
		ProjectName:      project,
		TaskType:         spec.TaskType,
		MediaType:        spec.MediaType,
		ResourceID:       spec.ResourceID,
		Payload:          spec.Payload,
		ScriptFile:       spec.ScriptFile,
		Source:           taskStringOr(task, "source", "webui"),
		UserID:           userID,
		DependencyTaskID: optionalTaskString(task, "dependency_task_id"),
		DependencyGroup:  optionalTaskString(task, "dependency_group"),
		ResourceType:     optionalTaskString(task, "resource_type"),
		ProviderID:       optionalTaskString(task, "provider_id"),
	}
	if index, ok := taskInt(task, "dependency_index"); ok {
		value := int64(index)
		params.DependencyIndex = &value
	}
	return params, nil
}

// GetTask 读取任务的对外投影；不存在返回 (nil, nil)。
func (a *BatchTaskAdapter) GetTask(ctx context.Context, taskID string) (map[string]any, error) {
	task, err := a.queue.GetTask(ctx, taskID)
	if err != nil {
		var notFound *repository.TaskNotFoundError
		if errors.As(err, &notFound) {
			return nil, nil
		}
		return nil, err
	}
	return taskProjection(task), nil
}

// CancelTask 取消单个任务。
func (a *BatchTaskAdapter) CancelTask(ctx context.Context, taskID string) error {
	_, err := a.queue.CancelTask(ctx, taskID)
	return err
}

// AdmitAll 原子入队（整批一个事务）。
func (a *BatchTaskAdapter) AdmitAll(ctx context.Context, params []repository.EnqueueParams) ([]string, error) {
	results, err := a.repo.EnqueueBatch(ctx, params)
	if err != nil {
		return nil, &BatchAdmissionError{Detail: "batch contains an active task conflict"}
	}
	ids := make([]string, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.TaskID)
	}
	return ids, nil
}

// BatchRepository 是批次元数据的持久化端口。
type BatchRepository interface {
	Save(ctx context.Context, batch GenerationBatch) error
	Get(ctx context.Context, batchID string) (*GenerationBatch, error)
	AdmitWithTasks(ctx context.Context, batch GenerationBatch, params []repository.EnqueueParams) (GenerationBatch, error)
	ReplaceTasks(
		ctx context.Context, batch GenerationBatch, replacements map[int]repository.EnqueueParams,
	) (GenerationBatch, error)
}

// GormBatchRepository 是 generation_batches 表的仓储，按 user_id + project_name 收敛可见性。
type GormBatchRepository struct {
	db          *gorm.DB
	userID      string
	projectName string
}

// NewGormBatchRepository 构造。
func NewGormBatchRepository(db *gorm.DB, userID, projectName string) *GormBatchRepository {
	return &GormBatchRepository{db: db, userID: userID, projectName: projectName}
}

// Save 写入/更新批次元数据。
func (r *GormBatchRepository) Save(ctx context.Context, batch GenerationBatch) error {
	itemsJSON, err := json.Marshal(batch.Items)
	if err != nil {
		return err
	}
	taskIDs := batch.TaskIDs
	if taskIDs == nil {
		taskIDs = []string{}
	}
	taskIDsJSON, err := json.Marshal(taskIDs)
	if err != nil {
		return err
	}
	row := repository.GenerationBatch{
		BatchID:         batch.BatchID,
		ProjectName:     batch.ProjectName,
		AdmissionState:  batch.AdmissionState,
		ItemsJSON:       string(itemsJSON),
		TaskIdsJSON:     string(taskIDsJSON),
		CancelRequested: batch.CancelRequested,
		ErrorMessage:    batch.ErrorMessage,
		UserID:          r.userID,
	}
	return upsertBatchRow(r.db.WithContext(ctx), row)
}

// upsertBatchRow 是批次行写入的唯一入口。
//
// 不用 Session.Save：行已存在时 Save 会把 created_at 一起写成零值，而该列 NOT NULL，
// 「只改一个标志」的写（取消 / 重试）会直接失败。这里先 UPDATE 再按需 INSERT。
func upsertBatchRow(tx *gorm.DB, row repository.GenerationBatch) error {
	updates := map[string]any{
		"project_name":     row.ProjectName,
		"admission_state":  row.AdmissionState,
		"items_json":       row.ItemsJSON,
		"task_ids_json":    row.TaskIdsJSON,
		"cancel_requested": row.CancelRequested,
		"error_message":    row.ErrorMessage,
		"updated_at":       db.NewFlexibleTime(time.Now()),
	}
	res := tx.Model(&repository.GenerationBatch{}).Where("batch_id = ?", row.BatchID).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	now := db.NewFlexibleTime(time.Now())
	row.CreatedAt = now
	row.UpdatedAt = now
	return tx.Create(&row).Error
}

// Get 按批次号读取；越权（他人 / 他项目）视同不存在。
func (r *GormBatchRepository) Get(ctx context.Context, batchID string) (*GenerationBatch, error) {
	var row repository.GenerationBatch
	query := r.db.WithContext(ctx).Where("batch_id = ?", batchID)
	if r.userID != "" {
		query = query.Where("user_id = ?", r.userID)
	}
	if r.projectName != "" {
		query = query.Where("project_name = ?", r.projectName)
	}
	if err := query.First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return batchFromRow(row, r.userID)
}

// AdmitWithTasks 在同一事务里写批次元数据与全部任务行：任务入队冲突即整批回滚。
func (r *GormBatchRepository) AdmitWithTasks(
	ctx context.Context, batch GenerationBatch, params []repository.EnqueueParams,
) (GenerationBatch, error) {
	var admitted GenerationBatch
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ids, err := repository.NewTaskRepo(tx).EnqueueBatch(ctx, params)
		if err != nil {
			return &BatchAdmissionError{Detail: "batch contains an active task conflict"}
		}
		taskIDs := make([]string, 0, len(ids))
		for _, result := range ids {
			taskIDs = append(taskIDs, result.TaskID)
		}
		admitted = batch
		admitted.TaskIDs = taskIDs
		admitted.AdmissionState = "admitted"
		itemsJSON, marshalErr := json.Marshal(admitted.Items)
		if marshalErr != nil {
			return marshalErr
		}
		taskIDsJSON, marshalErr := json.Marshal(taskIDs)
		if marshalErr != nil {
			return marshalErr
		}
		row := repository.GenerationBatch{
			BatchID:         admitted.BatchID,
			ProjectName:     admitted.ProjectName,
			AdmissionState:  admitted.AdmissionState,
			ItemsJSON:       string(itemsJSON),
			TaskIdsJSON:     string(taskIDsJSON),
			CancelRequested: admitted.CancelRequested,
			ErrorMessage:    admitted.ErrorMessage,
			UserID:          r.userID,
		}
		return upsertBatchRow(tx, row)
	})
	if err != nil {
		return GenerationBatch{}, err
	}
	return admitted, nil
}

// ReplaceTasks 在同一事务里替换失败条目的任务行（重试用）。
func (r *GormBatchRepository) ReplaceTasks(
	ctx context.Context, batch GenerationBatch, replacements map[int]repository.EnqueueParams,
) (GenerationBatch, error) {
	indexes := make([]int, 0, len(replacements))
	for index := range replacements {
		indexes = append(indexes, index)
	}
	sortInts(indexes)
	specs := make([]repository.EnqueueParams, 0, len(indexes))
	for _, index := range indexes {
		specs = append(specs, replacements[index])
	}
	var out GenerationBatch
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		results, err := repository.NewTaskRepo(tx).EnqueueBatch(ctx, specs)
		if err != nil {
			return &BatchAdmissionError{Detail: "batch contains an active task conflict"}
		}
		if len(results) != len(indexes) {
			return &BatchAdmissionError{Detail: "batch task adapter returned an unexpected number of task IDs"}
		}
		out = batch
		taskIDs := append([]string(nil), batch.TaskIDs...)
		for position, index := range indexes {
			if index < len(taskIDs) {
				taskIDs[index] = results[position].TaskID
			}
		}
		out.TaskIDs = taskIDs
		out.CancelRequested = false
		itemsJSON, marshalErr := json.Marshal(out.Items)
		if marshalErr != nil {
			return marshalErr
		}
		taskIDsJSON, marshalErr := json.Marshal(taskIDs)
		if marshalErr != nil {
			return marshalErr
		}
		row := repository.GenerationBatch{
			BatchID:         out.BatchID,
			ProjectName:     out.ProjectName,
			AdmissionState:  out.AdmissionState,
			ItemsJSON:       string(itemsJSON),
			TaskIdsJSON:     string(taskIDsJSON),
			CancelRequested: false,
			ErrorMessage:    out.ErrorMessage,
			UserID:          r.userID,
		}
		return upsertBatchRow(tx, row)
	})
	if err != nil {
		return GenerationBatch{}, err
	}
	return out, nil
}

// BatchOrchestrator 是批次的公开接口：准入、状态、取消、重试。
type BatchOrchestrator struct {
	repo  BatchRepository
	tasks *BatchTaskAdapter
}

// NewBatchOrchestrator 构造。
func NewBatchOrchestrator(repo BatchRepository, tasks *BatchTaskAdapter) *BatchOrchestrator {
	return &BatchOrchestrator{repo: repo, tasks: tasks}
}

// Admit 校验并原子准入一批任务。
func (o *BatchOrchestrator) Admit(
	ctx context.Context, projectName string, items []GenerationBatchItem, userID string,
) (GenerationBatch, error) {
	issues := []string{}
	if strings.TrimSpace(projectName) == "" {
		issues = append(issues, "project_name is required")
	}
	if len(items) == 0 {
		issues = append(issues, "batch must contain at least one item")
	}
	seen := map[string]struct{}{}
	for _, item := range items {
		if strings.TrimSpace(item.ItemID) == "" {
			issues = append(issues, "item_id is required")
		} else if _, ok := seen[item.ItemID]; ok {
			issues = append(issues, "duplicate item_id: "+item.ItemID)
		}
		seen[item.ItemID] = struct{}{}
		if declared := taskString(item.Task, "project_name"); declared != "" && declared != projectName {
			issues = append(issues, fmt.Sprintf("item '%s': project_name does not match batch", item.ItemID))
		}
		for _, field := range []string{"task_type", "media_type", "resource_id"} {
			if strings.TrimSpace(taskString(item.Task, field)) == "" {
				issues = append(issues, fmt.Sprintf("item '%s': %s is required", item.ItemID, field))
			}
		}
		for _, issue := range o.tasks.Validate(item.Task) {
			issues = append(issues, fmt.Sprintf("item '%s': %s", item.ItemID, issue))
		}
	}
	if len(issues) > 0 {
		return GenerationBatch{}, &BatchAdmissionError{Detail: strings.Join(issues, "; ")}
	}

	raw := make([]map[string]any, 0, len(items))
	for _, item := range items {
		raw = append(raw, item.Task)
	}
	params, err := o.tasks.PrepareAll(raw, userID)
	if err != nil {
		return GenerationBatch{}, err
	}
	batch := GenerationBatch{
		BatchID:        newBatchID(),
		ProjectName:    projectName,
		Items:          items,
		UserID:         userID,
		AdmissionState: "admitting",
	}
	return o.repo.AdmitWithTasks(ctx, batch, params)
}

// GetBatch 读取批次元数据。
func (o *BatchOrchestrator) GetBatch(ctx context.Context, batchID string) (GenerationBatch, error) {
	batch, err := o.repo.Get(ctx, batchID)
	if err != nil {
		return GenerationBatch{}, err
	}
	if batch == nil {
		return GenerationBatch{}, &BatchNotFoundError{}
	}
	return *batch, nil
}

// GetTask 透出单任务投影。
func (o *BatchOrchestrator) GetTask(ctx context.Context, taskID string) (map[string]any, error) {
	return o.tasks.GetTask(ctx, taskID)
}

// Status 派生批次状态（批次未准入时报错）。
func (o *BatchOrchestrator) Status(ctx context.Context, batch GenerationBatch) (BatchStatus, error) {
	if batch.AdmissionState != "admitted" {
		return "", &BatchAdmissionError{Detail: "batch is not admitted"}
	}
	statuses := make([]string, 0, len(batch.TaskIDs))
	for _, taskID := range batch.TaskIDs {
		task, err := o.tasks.GetTask(ctx, taskID)
		if err != nil {
			return "", err
		}
		statuses = append(statuses, taskStatusOf(task))
	}
	return deriveBatchStatus(statuses, batch.CancelRequested), nil
}

// Cancel 取消批次里所有非终态任务。
func (o *BatchOrchestrator) Cancel(ctx context.Context, batchID string) (GenerationBatch, error) {
	batch, err := o.GetBatch(ctx, batchID)
	if err != nil {
		return GenerationBatch{}, err
	}
	for _, taskID := range batch.TaskIDs {
		task, err := o.tasks.GetTask(ctx, taskID)
		if err != nil {
			return GenerationBatch{}, err
		}
		switch taskStatusOf(task) {
		case "succeeded", "failed", "cancelled":
			continue
		}
		if cancelErr := o.tasks.CancelTask(ctx, taskID); cancelErr != nil {
			var notFound *repository.TaskNotFoundError
			if errors.As(cancelErr, &notFound) {
				continue
			}
			return GenerationBatch{}, cancelErr
		}
	}
	if !batch.CancelRequested {
		batch.CancelRequested = true
		if saveErr := o.repo.Save(ctx, batch); saveErr != nil {
			return GenerationBatch{}, saveErr
		}
	}
	return batch, nil
}

// RetryFailed 重提批次里失败的任务，返回替换后的批次。
func (o *BatchOrchestrator) RetryFailed(ctx context.Context, batchID string, userID string) (GenerationBatch, error) {
	batch, err := o.GetBatch(ctx, batchID)
	if err != nil {
		return GenerationBatch{}, err
	}
	indexes := make([]int, 0, len(batch.Items))
	specs := make([]map[string]any, 0, len(batch.Items))
	for index, paired := range batch.Tasks() {
		taskID, _ := paired["task_id"].(string)
		task, getErr := o.tasks.GetTask(ctx, taskID)
		if getErr != nil {
			return GenerationBatch{}, getErr
		}
		if taskStatusOf(task) != "failed" {
			continue
		}
		indexes = append(indexes, index)
		specs = append(specs, batch.Items[index].Task)
	}
	if len(indexes) == 0 {
		return batch, nil
	}
	prepared, err := o.tasks.PrepareAll(specs, userID)
	if err != nil {
		return GenerationBatch{}, err
	}
	if len(prepared) != len(indexes) {
		return GenerationBatch{}, &BatchAdmissionError{
			Detail: "batch task adapter returned an unexpected number of prepared retry tasks"}
	}
	replacements := make(map[int]repository.EnqueueParams, len(indexes))
	for position, index := range indexes {
		replacements[index] = prepared[position]
	}
	return o.repo.ReplaceTasks(ctx, batch, replacements)
}

// ---- 内部 ----

// batchFromRow 把表行还原成领域对象。
func batchFromRow(row repository.GenerationBatch, fallbackUserID string) (*GenerationBatch, error) {
	items := []GenerationBatchItem{}
	if row.ItemsJSON != "" {
		if err := json.Unmarshal([]byte(row.ItemsJSON), &items); err != nil {
			return nil, err
		}
	}
	taskIDs := []string{}
	if row.TaskIdsJSON != "" {
		if err := json.Unmarshal([]byte(row.TaskIdsJSON), &taskIDs); err != nil {
			return nil, err
		}
	}
	userID := row.UserID
	if userID == "" {
		userID = fallbackUserID
	}
	return &GenerationBatch{
		BatchID:         row.BatchID,
		ProjectName:     row.ProjectName,
		Items:           items,
		TaskIDs:         taskIDs,
		CancelRequested: row.CancelRequested,
		UserID:          userID,
		AdmissionState:  row.AdmissionState,
		ErrorMessage:    row.ErrorMessage,
	}, nil
}

// taskProjection 把任务投影成批次响应需要的字段（与 Python _response 一致）。
func taskProjection(task *repository.TaskData) map[string]any {
	if task == nil {
		return nil
	}
	return map[string]any{
		"task_id":         task.TaskID,
		"status":          task.Status,
		"progress":        task.Progress,
		"progress_source": task.ProgressSource,
		"phase_code":      task.PhaseCode,
		"error_message":   task.ErrorMessage,
		"resource_id":     task.ResourceID,
	}
}

// taskStatusOf 读任务状态；任务缺失按 failed 计（与 Python 一致）。
func taskStatusOf(task map[string]any) string {
	if task == nil {
		return "failed"
	}
	status, _ := task["status"].(string)
	if status == "" {
		return "failed"
	}
	return status
}

// deriveBatchStatus 从任务状态派生批次状态。
func deriveBatchStatus(statuses []string, cancelRequested bool) BatchStatus {
	if len(statuses) == 0 {
		return BatchFailed
	}
	active := map[string]bool{"queued": true, "running": true, "cancelling": true}
	succeeded, failed, cancelled := 0, 0, 0
	for _, status := range statuses {
		if active[status] {
			return BatchRunning
		}
		switch status {
		case "succeeded":
			succeeded++
		case "failed":
			failed++
		case "cancelled":
			cancelled++
		}
	}
	switch {
	case succeeded == len(statuses):
		return BatchSucceeded
	case cancelled == len(statuses) || (cancelRequested && succeeded == 0 && failed == 0):
		return BatchCancelled
	case succeeded > 0:
		return BatchPartiallySuccess
	default:
		return BatchFailed
	}
}

// taskString 读 map 里的字符串字段。
func taskString(task map[string]any, key string) string {
	if task == nil {
		return ""
	}
	value, _ := task[key].(string)
	return value
}

// taskStringOr 读字符串字段，缺省取 fallback。
func taskStringOr(task map[string]any, key, fallback string) string {
	if value := taskString(task, key); value != "" {
		return value
	}
	return fallback
}

// optionalTaskString 读可空字符串字段（空串 → nil）。
func optionalTaskString(task map[string]any, key string) *string {
	value, ok := task[key].(string)
	if !ok || value == "" {
		return nil
	}
	return &value
}

// taskInt 读整数（JSON 解出为 float64）。
func taskInt(task map[string]any, key string) (int, bool) {
	value, present := task[key]
	if !present || value == nil {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if float64(int(typed)) == typed {
			return int(typed), true
		}
	}
	return 0, false
}

// sortInts 小数组插入排序（避免为几个下标引入额外依赖）。
func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// newBatchID 生成 32 位十六进制批次号（与 Python uuid4().hex 同形）。
func newBatchID() string {
	id, err := uuid.NewRandom()
	if err != nil {
		// 随机源不可用是环境级故障：时间戳兜底让调用方拿到可用的批次号，
		// 而不是把一个「提交成功但没有批次号」的批次留在库里。
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return strings.ReplaceAll(id.String(), "-", "")
}

// batchNow 供时间无关的确定性测试断言使用。
var batchNow = func() time.Time { return time.Now().UTC() }

// DeriveBatchStatusForTest 暴露派生规则给跨包测试，避免测试另写一份实现。
func DeriveBatchStatusForTest(statuses []string, cancelRequested bool) string {
	return string(deriveBatchStatus(statuses, cancelRequested))
}
