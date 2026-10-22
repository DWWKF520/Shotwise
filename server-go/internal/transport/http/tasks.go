// 任务队列与取消路由（对应 server/routers/tasks.py）。
//
// Mount prefix: /api/v1
//
// 读侧统一做 loc 化：error_message 是 `[code] {params}` 信封、result.warnings 是
// {key, params} 条目，两个出口（列表 / 详情 / 取消响应）必须同源渲染，否则前端会在
// 某一个出口看到裸 code。写侧只有取消，语义由 repository 的状态机给定。
package http

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/i18n"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service/generation"
)

// TaskQueue 是任务路由依赖的队列能力（*queue.Queue 满足）。
type TaskQueue interface {
	ListTasks(ctx context.Context, p repository.ListTasksParams) (repository.TaskListResult, error)
	GetTaskStats(ctx context.Context, projectName *string) (repository.TaskStats, error)
	GetTask(ctx context.Context, taskID string) (*repository.TaskData, error)
	GetCancelPreview(ctx context.Context, taskID string) (repository.CancelPreview, error)
	CancelTask(ctx context.Context, taskID string) (repository.CancelResult, error)
	GetCancelAllPreview(ctx context.Context, projectName string) (int64, error)
	CancelAllActive(ctx context.Context, projectName string) (repository.CancelResult, error)
}

// TaskDeps 是任务路由的依赖。
type TaskDeps struct {
	Queue TaskQueue
}

// RegisterTasks 注册任务队列与取消路由。
//
// 路由树注意：/tasks/stats 是静态段，必须在 /tasks/:task_id 之前注册，否则 gin 会把
// stats 当成 task_id（同一位置上静态与参数并存时 gin 要求静态优先）。
func RegisterTasks(group *gin.RouterGroup, d TaskDeps) {
	group.GET("/tasks/stats", getTaskStats(d))
	group.GET("/tasks", listTasks(d))
	group.GET("/tasks/:task_id/cancel-preview", cancelTaskPreview(d))
	group.POST("/tasks/:task_id/cancel", cancelTask(d))
	group.GET("/tasks/:task_id", getTask(d))

	group.GET("/projects/:name/tasks", listProjectTasks(d))
	group.GET("/projects/:name/tasks/cancel-all-preview", cancelAllPreview(d))
	group.POST("/projects/:name/tasks/cancel-all", cancelAll(d))
}

// getTaskStats 返回按状态聚合的任务统计。
func getTaskStats(d TaskDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectName := optionalQuery(c, "project_name")
		stats, err := d.Queue.GetTaskStats(c.Request.Context(), projectName)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"stats": stats})
	}
}

// listTasks 分页列出任务（可跨项目）。
func listTasks(d TaskDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		params, ok := taskListParams(c, optionalQuery(c, "project_name"))
		if !ok {
			return
		}
		writeTaskList(c, d, params)
	}
}

// listProjectTasks 分页列出单项目任务（项目名取路径参数）。
func listProjectTasks(d TaskDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectName := c.Param("name")
		params, ok := taskListParams(c, &projectName)
		if !ok {
			return
		}
		writeTaskList(c, d, params)
	}
}

func writeTaskList(c *gin.Context, d TaskDeps, params repository.ListTasksParams) {
	result, err := d.Queue.ListTasks(c.Request.Context(), params)
	if err != nil {
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	loc := i18n.FromRequest(c.Request)
	items := make([]map[string]any, 0, len(result.Items))
	for _, task := range result.Items {
		items = append(items, generation.LocalizeTask(task, loc))
	}
	c.JSON(http.StatusOK, gin.H{
		"items":     items,
		"total":     result.Total,
		"page":      result.Page,
		"page_size": result.PageSize,
	})
}

// cancelTaskPreview 预览取消某个任务的影响范围。
func cancelTaskPreview(d TaskDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		taskID := c.Param("task_id")
		preview, err := d.Queue.GetCancelPreview(c.Request.Context(), taskID)
		if err != nil {
			abortTaskErr(c, err, taskID)
			return
		}
		c.JSON(http.StatusOK, preview)
	}
}

// cancelTask 按状态分发取消单个任务；终态任务原样回给调用方。
//
// cancelled / skipped_terminal 里的任务必须与列表 / 详情同源本地化，否则会在这一个
// 出口泄露裸 [code] {params}。
func cancelTask(d TaskDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		taskID := c.Param("task_id")
		result, err := d.Queue.CancelTask(c.Request.Context(), taskID)
		if err != nil {
			abortTaskErr(c, err, taskID)
			return
		}
		loc := i18n.FromRequest(c.Request)
		c.JSON(http.StatusOK, gin.H{
			"cancelled":        localizeTasks(result.Cancelled, loc),
			"cancelling":       result.Cancelling,
			"skipped_terminal": localizeTasks(result.SkippedTerminal, loc),
		})
	}
}

// cancelAllPreview 返回项目中当前活动任务数量。
func cancelAllPreview(d TaskDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		count, err := d.Queue.GetCancelAllPreview(c.Request.Context(), c.Param("name"))
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"active_count": count})
	}
}

// cancelAll 取消项目中全部活动任务，返回三个桶的计数。
//
// cancelled_count 含级联打到的 queued 下游（取消父任务会连带取消其下游），与
// active_count 不等是正常现象。
func cancelAll(d TaskDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Queue.CancelAllActive(c.Request.Context(), c.Param("name"))
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"cancelled_count":       len(result.Cancelled),
			"cancelling_count":      len(result.Cancelling),
			"skipped_terminal_count": len(result.SkippedTerminal),
		})
	}
}

// getTask 返回单个任务详情。
func getTask(d TaskDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		taskID := c.Param("task_id")
		task, err := d.Queue.GetTask(c.Request.Context(), taskID)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		if task == nil {
			apperr.Abort(c, apperr.NotFound("task_not_found", map[string]any{"id": taskID}))
			return
		}
		c.JSON(http.StatusOK, gin.H{"task": generation.LocalizeTask(*task, i18n.FromRequest(c.Request))})
	}
}

// ---- 助手 ----

// taskListParams 解析列表过滤与分页参数（page ge=1、page_size le=500，越界即 422）。
func taskListParams(c *gin.Context, projectName *string) (repository.ListTasksParams, bool) {
	page, ok := taskQueryInt(c, "page", 1, 1, 0)
	if !ok {
		return repository.ListTasksParams{}, false
	}
	pageSize, ok := taskQueryInt(c, "page_size", 50, 1, 500)
	if !ok {
		return repository.ListTasksParams{}, false
	}
	return repository.ListTasksParams{
		ProjectName: projectName,
		Status:      optionalQuery(c, "status"),
		TaskType:    optionalQuery(c, "task_type"),
		Source:      optionalQuery(c, "source"),
		Page:        page,
		PageSize:    pageSize,
	}, true
}

// taskQueryInt 解析整型查询参数；max 为 0 表示不设上界。
func taskQueryInt(c *gin.Context, name string, def, min, max int) (int, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return def, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"query", name}, Msg: "value is not a valid integer", Type: "int_parsing"},
		}})
		return 0, false
	}
	if value < min || (max > 0 && value > max) {
		typ := "greater_than_equal"
		if max > 0 && value > max {
			typ = "less_than_equal"
		}
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"query", name}, Msg: "invalid value", Type: typ},
		}})
		return 0, false
	}
	return value, true
}

// optionalQuery 返回非空查询参数，空串归为缺省。
func optionalQuery(c *gin.Context, name string) *string {
	value := strings.TrimSpace(c.Query(name))
	if value == "" {
		return nil
	}
	return &value
}

// localizeTasks 批量本地化任务投影。
func localizeTasks(tasks []repository.TaskData, loc i18n.Locale) []map[string]any {
	out := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, generation.LocalizeTask(task, loc))
	}
	return out
}

// abortTaskErr 把「任务不存在」翻成 400（与 Python 侧 cancel 两个端点的口径一致）。
func abortTaskErr(c *gin.Context, err error, taskID string) {
	var notFound *repository.TaskNotFoundError
	switch {
	case errors.As(err, &notFound):
		// tasks.py 抛 BadRequestError("task_not_found")：400 且 detail 是翻译后的字符串。
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "task_not_found", map[string]any{"id": taskID}))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
