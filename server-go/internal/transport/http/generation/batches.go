// 批量生成路由（对应 server/routers/batches.py）。
//
// Mount prefix: /api/v1/projects/:name/batches
//
// 语义要点：
//   - 创建成功是 201，返回体是投影后的状态（status 由任务状态派生，不单独存储）；
//   - 入参是严格模式（extra="forbid"）：未知字段直接 422，避免客户端以为某个字段生效了；
//   - 越权/不存在的批次统一 404 batch_not_found（不区分，避免探测他人批次号）。
package generation

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	domaintask "github.com/shotwise/shotwise/internal/domain/task"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/generation"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"
)

// BatchDeps 是批次路由的依赖。
type BatchDeps struct {
	Store *service.ProjectStore
	// NewOrchestrator 按「项目 + 调用者」构造编排器（仓储带 user/project 围栏）。
	NewOrchestrator func(projectName, userID string) *generation.BatchOrchestrator
}

// RegisterBatches 注册批量生成路由。
func RegisterBatches(group *gin.RouterGroup, d BatchDeps) {
	if d.NewOrchestrator == nil {
		return
	}
	base := "/projects/:name/batches"
	group.POST(base, createBatch(d))
	group.GET(base+"/:batch_id", getBatch(d))
	group.POST(base+"/:batch_id/cancel", cancelBatch(d))
	group.POST(base+"/:batch_id/retry-failed", retryFailedBatch(d))
}

// batchTaskBody 是批次里单个任务的提交体（字段与 queue task 对齐）。
type batchTaskBody struct {
	TaskType         string         `json:"task_type"`
	MediaType        string         `json:"media_type"`
	ResourceID       string         `json:"resource_id"`
	Payload          map[string]any `json:"payload"`
	ScriptFile       *string        `json:"script_file"`
	ResourceType     *string        `json:"resource_type"`
	Source           *string        `json:"source"`
	DependencyTaskID *string        `json:"dependency_task_id"`
	DependencyGroup  *string        `json:"dependency_group"`
	DependencyIndex  *int           `json:"dependency_index"`
	ProviderID       *string        `json:"provider_id"`
}

// batchItemBody 是批次条目。
type batchItemBody struct {
	ItemID string        `json:"item_id"`
	Task   batchTaskBody `json:"task"`
}

// createBatchBody 是创建批次的提交体。
type createBatchBody struct {
	Items []batchItemBody `json:"items"`
}

// batchTaskAllowedFields 对应 pydantic 的 extra="forbid"：未知字段一律 422。
var batchTaskAllowedFields = map[string]bool{
	"task_type": true, "media_type": true, "resource_id": true, "payload": true,
	"script_file": true, "resource_type": true, "source": true, "dependency_task_id": true,
	"dependency_group": true, "dependency_index": true, "provider_id": true,
}

// batchTaskRequired 是必填字段（长度校验另有下限 1）。
var batchTaskRequired = []string{"task_type", "media_type", "resource_id"}

// batchSourceValues 对应 Literal["webui", "agent", "api"]。
var batchSourceValues = map[string]bool{"webui": true, "agent": true, "api": true}

// createBatch 创建并原子准入一批任务。
func createBatch(d BatchDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		raw, ok := readRawJSON(c)
		if !ok {
			return
		}
		items, items_ := decodeBatchItems(c, raw)
		if !items_ {
			return
		}
		orchestrator := d.NewOrchestrator(c.Param("name"), httpkit.CurrentUserID(c))
		batch, err := orchestrator.Admit(c.Request.Context(), c.Param("name"), items, httpkit.CurrentUserID(c))
		if err != nil {
			abortBatchErr(c, err, "batch_admission_failed")
			return
		}
		out, err := batchResponse(c, orchestrator, batch)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusCreated, out)
	}
}

// getBatch 读取批次状态。
func getBatch(d BatchDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		orchestrator := d.NewOrchestrator(c.Param("name"), httpkit.CurrentUserID(c))
		out, err := loadBatchResponse(c, orchestrator, c.Param("batch_id"))
		if err != nil {
			abortBatchErr(c, err, "")
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// cancelBatch 取消批次里所有非终态任务。
func cancelBatch(d BatchDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		orchestrator := d.NewOrchestrator(c.Param("name"), httpkit.CurrentUserID(c))
		batch, err := orchestrator.Cancel(c.Request.Context(), c.Param("batch_id"))
		if err != nil {
			abortBatchErr(c, err, "")
			return
		}
		out, err := batchResponse(c, orchestrator, batch)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// retryFailedBatch 重提批次里失败的任务。
func retryFailedBatch(d BatchDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		orchestrator := d.NewOrchestrator(c.Param("name"), httpkit.CurrentUserID(c))
		batch, err := orchestrator.RetryFailed(c.Request.Context(), c.Param("batch_id"), httpkit.CurrentUserID(c))
		if err != nil {
			abortBatchErr(c, err, "batch_retry_failed")
			return
		}
		out, err := batchResponse(c, orchestrator, batch)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// ---- 助手 ----

// readRawJSON 读出请求体原文；畸形 JSON 写 422。
func readRawJSON(c *gin.Context) (map[string]json.RawMessage, bool) {
	var envelope map[string]json.RawMessage
	if err := json.NewDecoder(c.Request.Body).Decode(&envelope); err != nil {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"body"}, Msg: "invalid request body", Type: "json_invalid"},
		}})
		return nil, false
	}
	return envelope, true
}

// decodeBatchItems 逐条解析 items，并复刻 pydantic 的严格校验（extra=forbid / 长度 / 枚举）。
func decodeBatchItems(c *gin.Context, envelope map[string]json.RawMessage) ([]generation.GenerationBatchItem, bool) {
	rawItems, present := envelope["items"]
	if !present {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"body", "items"}, Msg: "field required", Type: "missing"},
		}})
		return nil, false
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(rawItems, &entries); err != nil {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"body", "items"}, Msg: "Input should be a valid list", Type: "list_type"},
		}})
		return nil, false
	}
	var items []apperr.ValidationItem
	add := func(loc []string, msg, typ string) {
		items = append(items, apperr.ValidationItem{Loc: loc, Msg: msg, Type: typ})
	}
	if len(entries) == 0 {
		add([]string{"body", "items"}, "List should have at least 1 item after validation, not 0", "too_short")
	}
	if len(entries) > 500 {
		add([]string{"body", "items"}, "List should have at most 500 items after validation", "too_long")
	}

	out := make([]generation.GenerationBatchItem, 0, len(entries))
	for index, entry := range entries {
		itemLoc := []string{"body", "items", httpkit.ItoaInt(index)}
		for key := range entry {
			if key != "item_id" && key != "task" {
				add(append(append([]string{}, itemLoc...), key), "Extra inputs are not permitted", "extra_forbidden")
			}
		}
		var itemID string
		if raw, ok := entry["item_id"]; ok {
			if err := json.Unmarshal(raw, &itemID); err != nil {
				add(append(append([]string{}, itemLoc...), "item_id"), "Input should be a valid string", "string_type")
			} else if itemID == "" {
				add(append(append([]string{}, itemLoc...), "item_id"),
					"String should have at least 1 character", "string_too_short")
			}
		} else {
			add(append(append([]string{}, itemLoc...), "item_id"), "field required", "missing")
		}
		rawTask, hasTask := entry["task"]
		if !hasTask {
			add(append(append([]string{}, itemLoc...), "task"), "field required", "missing")
			continue
		}
		var taskFields map[string]json.RawMessage
		if err := json.Unmarshal(rawTask, &taskFields); err != nil {
			add(append(append([]string{}, itemLoc...), "task"), "Input should be a valid dictionary", "dict_type")
			continue
		}
		taskLoc := append(append([]string{}, itemLoc...), "task")
		for key := range taskFields {
			if !batchTaskAllowedFields[key] {
				add(append(append([]string{}, taskLoc...), key), "Extra inputs are not permitted", "extra_forbidden")
			}
		}
		for _, field := range batchTaskRequired {
			raw, ok := taskFields[field]
			if !ok {
				add(append(append([]string{}, taskLoc...), field), "field required", "missing")
				continue
			}
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				add(append(append([]string{}, taskLoc...), field), "Input should be a valid string", "string_type")
			} else if value == "" {
				add(append(append([]string{}, taskLoc...), field),
					"String should have at least 1 character", "string_too_short")
			}
		}
		if raw, ok := taskFields["source"]; ok {
			var source string
			if err := json.Unmarshal(raw, &source); err != nil || !batchSourceValues[source] {
				add(append(append([]string{}, taskLoc...), "source"),
					"Input should be 'webui', 'agent' or 'api'", "literal_error")
			}
		}
		if raw, ok := taskFields["dependency_index"]; ok {
			var idx int
			if err := json.Unmarshal(raw, &idx); err != nil {
				add(append(append([]string{}, taskLoc...), "dependency_index"),
					"Input should be a valid integer", "int_parsing")
			} else if idx < 0 {
				add(append(append([]string{}, taskLoc...), "dependency_index"),
					"Input should be greater than or equal to 0", "greater_than_equal")
			}
		}

		// 组装原始任务 map：批次服务层与单任务入队共用同一套字段口径。
		var task batchTaskBody
		decoder := json.NewDecoder(bytes.NewReader(rawTask))
		decoder.UseNumber()
		if err := decoder.Decode(&task); err != nil {
			add(append(append([]string{}, taskLoc...), ""), "invalid task body", "value_error")
			continue
		}
		out = append(out, generation.GenerationBatchItem{ItemID: itemID, Task: taskBodyToMap(task)})
	}
	if len(items) > 0 {
		apperr.Abort(c, &apperr.Validation{Items: items})
		return nil, false
	}
	return out, true
}

// taskBodyToMap 把严格提交体转成任务 map（nil 字段不出现，与 model_dump(exclude_none=True) 一致）。
func taskBodyToMap(body batchTaskBody) map[string]any {
	task := map[string]any{
		"task_type":   body.TaskType,
		"media_type":  body.MediaType,
		"resource_id": body.ResourceID,
	}
	if body.Payload != nil {
		task["payload"] = body.Payload
	}
	put := func(key string, value *string) {
		if value != nil {
			task[key] = *value
		}
	}
	put("script_file", body.ScriptFile)
	put("resource_type", body.ResourceType)
	put("source", body.Source)
	put("dependency_task_id", body.DependencyTaskID)
	put("dependency_group", body.DependencyGroup)
	put("provider_id", body.ProviderID)
	if body.DependencyIndex != nil {
		task["dependency_index"] = *body.DependencyIndex
	}
	return task
}

// loadBatchResponse 读批次并投影。
func loadBatchResponse(c *gin.Context, o *generation.BatchOrchestrator, batchID string) (map[string]any, error) {
	batch, err := o.GetBatch(c.Request.Context(), batchID)
	if err != nil {
		return nil, err
	}
	return batchResponse(c, o, batch)
}

// batchResponse 组装响应：状态由任务状态派生，逐条任务带 occupied / reason。
func batchResponse(
	c *gin.Context, o *generation.BatchOrchestrator, batch generation.GenerationBatch,
) (map[string]any, error) {
	status, err := o.Status(c.Request.Context(), batch)
	if err != nil {
		return nil, err
	}
	tasks := make([]any, 0, len(batch.Items))
	for index, item := range batch.Items {
		taskID := ""
		if index < len(batch.TaskIDs) {
			taskID = batch.TaskIDs[index]
		}
		task, err := o.GetTask(c.Request.Context(), taskID)
		if err != nil {
			return nil, err
		}
		taskStatus := "failed"
		if task != nil {
			if value, ok := task["status"].(string); ok && value != "" {
				taskStatus = value
			}
		}
		entry := map[string]any{
			"item_id":         item.ItemID,
			"resource_id":     taskStringOf(item.Task["resource_id"]),
			"task_id":         taskID,
			"status":          taskStatus,
			"occupied":        taskStatus == "queued" || taskStatus == "running" || taskStatus == "cancelling",
			"progress":        nil,
			"progress_source": nil,
			"phase_code":      nil,
			"reason":          nil,
		}
		if task != nil {
			entry["progress"] = task["progress"]
			entry["progress_source"] = task["progress_source"]
			entry["phase_code"] = task["phase_code"]
			if message, ok := task["error_message"].(string); ok && message != "" {
				entry["reason"] = domaintask.SanitizeFailureReason(message)
			}
		}
		tasks = append(tasks, entry)
	}
	return map[string]any{
		"batch_id":         batch.BatchID,
		"project_name":     batch.ProjectName,
		"status":           string(status),
		"cancel_requested": batch.CancelRequested,
		"tasks":            tasks,
	}, nil
}

// abortBatchErr 把批次错误映射成统一信封。
//
// 准入与重试的失败是字符串 detail（Python 侧直接交给 HTTPException）；批次不存在也是
// 字符串 detail，避免把内部错误文本透给客户端。
func abortBatchErr(c *gin.Context, err error, admissionCode string) {
	var (
		notFound  *generation.BatchNotFoundError
		admission *generation.BatchAdmissionError
	)
	switch {
	case errors.As(err, &notFound):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "batch_not_found", nil))
	case errors.As(err, &admission) && admissionCode != "":
		apperr.Abort(c, apperr.NewLiteral(http.StatusConflict, admissionCode))
	case errors.As(err, &admission):
		apperr.Abort(c, apperr.NewText(http.StatusConflict, "batch_admission_failed", nil))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}

// taskStringOf 把任意值取成字符串（缺省空串）。
func taskStringOf(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}
