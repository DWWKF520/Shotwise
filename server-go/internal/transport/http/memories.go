package http

// 长期记忆路由（对应 server/routers/memories.py）。

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/service"
)

// MemoryDeps 是记忆 handler 的依赖。
type MemoryDeps struct {
	Store    *service.ProjectStore
	Memories *service.MemoryService
}

// RegisterMemories 注册记忆路由。
//
// 注意 gin 路由树：/memories/user、/memories/export 与 /memories/import 是同层静态段，
// 与 /memories/:memory_id 的参数段只在不同方法（PATCH/DELETE）上重合。
func RegisterMemories(group *gin.RouterGroup, d MemoryDeps) {
	group.GET("/memories/user", listUserMemories(d))
	group.POST("/memories/user", createUserMemory(d))
	group.DELETE("/memories/user", clearUserMemories(d))
	group.GET("/memories/export", exportMemories(d))
	group.POST("/memories/import", importMemories(d))
	group.PATCH("/memories/:memory_id", updateMemory(d))
	group.DELETE("/memories/:memory_id", deleteMemory(d))

	group.GET("/memory-candidates", listMemoryCandidates(d))
	group.POST("/memory-candidates", createMemoryCandidate(d))
	group.POST("/memory-candidates/:candidate_id/accept", acceptMemoryCandidate(d))
	group.POST("/memory-candidates/:candidate_id/reject", rejectMemoryCandidate(d))

	group.GET("/projects/:name/memories", listProjectMemories(d))
	group.POST("/projects/:name/memories", createProjectMemory(d))
	group.DELETE("/projects/:name/memories", clearProjectMemories(d))
	group.POST("/projects/:name/memories/import", importProjectMemories(d))
}

type memoryCreateRequest struct {
	Category *string        `json:"category"`
	Content  *string        `json:"content"`
	Source   *string        `json:"source"`
	Metadata map[string]any `json:"metadata"`
}

type memoryUpdateRequest struct {
	Category *string        `json:"category"`
	Content  *string        `json:"content"`
	Metadata map[string]any `json:"metadata"`
}

type memoryCandidateCreateRequest struct {
	Scope           *string        `json:"scope"`
	ProjectName     *string        `json:"project_name"`
	Category        *string        `json:"category"`
	Content         *string        `json:"content"`
	SourceSessionID *string        `json:"source_session_id"`
	SourceMessageID *string        `json:"source_message_id"`
	Metadata        map[string]any `json:"metadata"`
}

func listUserMemories(d MemoryDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := d.Memories.ListMemories(c.Request.Context(), currentUserID(c), "user", nil)
		if err != nil {
			abortMemoryErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

func listProjectMemories(d MemoryDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		if !ensureProject(c, d.Store, name) {
			return
		}
		items, err := d.Memories.ListMemories(c.Request.Context(), currentUserID(c), "project", &name)
		if err != nil {
			abortMemoryErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

func createUserMemory(d MemoryDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		createMemoryCommon(c, d, nil)
	}
}

func createProjectMemory(d MemoryDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		if !ensureProject(c, d.Store, name) {
			return
		}
		createMemoryCommon(c, d, &name)
	}
}

// createMemoryCommon 是用户级与项目级记忆创建共用的实现。
func createMemoryCommon(c *gin.Context, d MemoryDeps, projectName *string) {
	var req memoryCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortBodyBind(c, err)
		return
	}
	if req.Content == nil {
		abortMissingField(c, "content")
		return
	}
	scope := "user"
	if projectName != nil {
		scope = "project"
	}
	category, source := "other", "user"
	if req.Category != nil {
		category = *req.Category
	}
	if req.Source != nil {
		source = *req.Source
	}
	item, err := d.Memories.CreateMemory(c.Request.Context(), service.CreateMemoryParams{
		UserID:      currentUserID(c),
		Scope:       scope,
		ProjectName: projectName,
		Category:    category,
		Content:     *req.Content,
		Source:      source,
		Metadata:    req.Metadata,
	})
	if err != nil {
		abortMemoryErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, item)
}

func clearUserMemories(d MemoryDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		clearMemoriesCommon(c, d, nil)
	}
}

func clearProjectMemories(d MemoryDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		if !ensureProject(c, d.Store, name) {
			return
		}
		clearMemoriesCommon(c, d, &name)
	}
}

func clearMemoriesCommon(c *gin.Context, d MemoryDeps, projectName *string) {
	scope := "user"
	if projectName != nil {
		scope = "project"
	}
	deleted, err := d.Memories.ClearMemories(c.Request.Context(), currentUserID(c), scope, projectName)
	if err != nil {
		abortMemoryErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": deleted})
}

func updateMemory(d MemoryDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req memoryUpdateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		item, err := d.Memories.UpdateMemory(
			c.Request.Context(), currentUserID(c), c.Param("memory_id"), req.Category, req.Content, req.Metadata,
		)
		if err != nil {
			abortMemoryErr(c, err)
			return
		}
		c.JSON(http.StatusOK, item)
	}
}

func deleteMemory(d MemoryDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := d.Memories.DeleteMemory(c.Request.Context(), currentUserID(c), c.Param("memory_id")); err != nil {
			abortMemoryErr(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func listMemoryCandidates(d MemoryDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var scope, projectName *string
		if v, ok := c.GetQuery("scope"); ok {
			scope = &v
		}
		if v, ok := c.GetQuery("project_name"); ok {
			if !ensureProject(c, d.Store, v) {
				return
			}
			projectName = &v
		}
		items, err := d.Memories.ListCandidates(c.Request.Context(), currentUserID(c), scope, projectName)
		if err != nil {
			abortMemoryErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

func createMemoryCandidate(d MemoryDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req memoryCandidateCreateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		if req.Scope == nil {
			abortMissingField(c, "scope")
			return
		}
		if req.Content == nil {
			abortMissingField(c, "content")
			return
		}
		if req.ProjectName != nil && !ensureProject(c, d.Store, *req.ProjectName) {
			return
		}
		category := "other"
		if req.Category != nil {
			category = *req.Category
		}
		item, err := d.Memories.CreateCandidate(c.Request.Context(), service.CreateMemoryCandidateParams{
			UserID:          currentUserID(c),
			Scope:           *req.Scope,
			ProjectName:     req.ProjectName,
			Category:        category,
			Content:         *req.Content,
			SourceSessionID: req.SourceSessionID,
			SourceMessageID: req.SourceMessageID,
			Metadata:        req.Metadata,
		})
		if err != nil {
			abortMemoryErr(c, err)
			return
		}
		c.JSON(http.StatusCreated, item)
	}
}

func acceptMemoryCandidate(d MemoryDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Memories.AcceptCandidate(c.Request.Context(), currentUserID(c), c.Param("candidate_id"))
		if err != nil {
			abortMemoryErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func rejectMemoryCandidate(d MemoryDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Memories.RejectCandidate(c.Request.Context(), currentUserID(c), c.Param("candidate_id"))
		if err != nil {
			abortMemoryErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func exportMemories(d MemoryDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var projectName *string
		if v, ok := c.GetQuery("project_name"); ok {
			if !ensureProject(c, d.Store, v) {
				return
			}
			projectName = &v
		}
		result, err := d.Memories.ExportMemories(c.Request.Context(), currentUserID(c), projectName)
		if err != nil {
			abortMemoryErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func importMemories(d MemoryDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var projectName *string
		if v, ok := c.GetQuery("project_name"); ok {
			if !ensureProject(c, d.Store, v) {
				return
			}
			projectName = &v
		}
		body, ok := decodeBody(c)
		if !ok {
			return
		}
		result, err := d.Memories.ImportMemories(c.Request.Context(), currentUserID(c), body, projectName)
		if err != nil {
			abortMemoryErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func importProjectMemories(d MemoryDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		if !ensureProject(c, d.Store, name) {
			return
		}
		body, ok := decodeBody(c)
		if !ok {
			return
		}
		result, err := d.Memories.ImportProjectMemories(c.Request.Context(), currentUserID(c), name, body)
		if err != nil {
			abortMemoryErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

// abortBodyBind 把 JSON 绑定失败写成 FastAPI 风格的 422。
func abortBodyBind(c *gin.Context, err error) {
	apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
		{Loc: []string{"body"}, Msg: err.Error(), Type: "value_error"},
	}})
}

// abortMissingField 把缺失的必填字段写成 FastAPI 风格的 422。
func abortMissingField(c *gin.Context, field string) {
	apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
		{Loc: []string{"body", field}, Msg: "field required", Type: "value_error.missing"},
	}})
}

// abortMemoryErr 把记忆领域错误映射成统一 {"detail": ...} 信封。
func abortMemoryErr(c *gin.Context, err error) {
	var (
		validation *service.MemoryValidationError
		notFound   *service.MemoryNotFoundError
		conflict   *service.MemoryConflictError
	)
	switch {
	case errors.As(err, &validation):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, validation.Code, memoryErrorParams(validation.Code)))
	case errors.As(err, &notFound):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, notFound.Code, nil))
	case errors.As(err, &conflict):
		apperr.Abort(c, apperr.NewText(http.StatusConflict, conflict.Code, nil))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}

func memoryErrorParams(code string) map[string]any {
	if code == "memory_content_too_long" {
		return map[string]any{"max_length": service.MemoryMaxLength}
	}
	return nil
}
