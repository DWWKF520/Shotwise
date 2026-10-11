package http

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/service"
)

// ProjectDeps 是项目 handler 的依赖。
type ProjectDeps struct {
	Store *service.ProjectStore
	Hub   *service.EventHub
}

// ListProjects 列出项目。
func ListProjects(d ProjectDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		names, err := d.Store.List()
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		items := make([]gin.H, 0, len(names))
		for _, n := range names {
			item := gin.H{"name": n}
			if pf, err := d.Store.Get(n); err == nil {
				item["title"] = pf.Title
				item["content_mode"] = pf.ContentMode
				item["generation_mode"] = pf.GenerationMode
				item["episodes_count"] = len(pf.Episodes)
			}
			items = append(items, item)
		}
		c.JSON(http.StatusOK, gin.H{"projects": items, "total": len(items)})
	}
}

// CreateProjectRequest 创建项目的请求体。
type CreateProjectRequest struct {
	Name            string `json:"name" binding:"required"`
	Title           string `json:"title"`
	ContentMode     string `json:"content_mode" binding:"required"`
	GenerationMode  string `json:"generation_mode" binding:"required"`
	Style           string `json:"style"`
	AspectRatio     string `json:"aspect_ratio"`
	GridStoryboard  bool   `json:"grid_storyboard"`
}

// CreateProject 创建项目。
func CreateProject(d ProjectDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateProjectRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"body"}, Msg: err.Error(), Type: "value_error"},
			}})
			return
		}
		if !domain.IsValidContentMode(req.ContentMode) {
			apperr.Abort(c, apperr.BadRequest("invalid_content_mode", map[string]any{"mode": req.ContentMode}))
			return
		}
		if !domain.IsValidGenerationMode(req.GenerationMode) {
			apperr.Abort(c, apperr.BadRequest("invalid_generation_mode", map[string]any{"mode": req.GenerationMode}))
			return
		}
		title := req.Title
		if title == "" {
			title = req.Name
		}
		pf, err := d.Store.Create(req.Name, title, domain.ContentMode(req.ContentMode),
			domain.GenerationMode(req.GenerationMode), req.Style)
		if err != nil {
			switch {
			case errors.Is(err, domain.ErrInvalidProjectName):
				apperr.Abort(c, apperr.BadRequest("invalid_project_name", nil))
			case errors.Is(err, service.ErrProjectExists):
				apperr.Abort(c, apperr.Conflict("project_already_exists", map[string]any{"name": req.Name}))
			default:
				apperr.Abort(c, apperr.Internal(err))
			}
			return
		}
		if req.AspectRatio != "" {
			pf.AspectRatio = req.AspectRatio
		}
		pf.GridStoryboard = req.GridStoryboard
		if err := d.Store.Save(req.Name, pf); err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusCreated, pf)
	}
}

// GetProject 读取项目详情。
func GetProject(d ProjectDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		pf, err := d.Store.Get(name)
		if err != nil {
			abortProjectErr(c, err)
			return
		}
		c.JSON(http.StatusOK, pf)
	}
}

// UpdateProjectRequest 更新项目的请求体（仅允许可变字段）。
type UpdateProjectRequest struct {
	Title       *string `json:"title"`
	Style       *string `json:"style"`
	AspectRatio *string `json:"aspect_ratio"`
	Brief       *string `json:"brief"`
}

// UpdateProject 更新项目元数据。
func UpdateProject(d ProjectDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		var req UpdateProjectRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"body"}, Msg: err.Error(), Type: "value_error"},
			}})
			return
		}
		pf, err := d.Store.Get(name)
		if err != nil {
			abortProjectErr(c, err)
			return
		}
		if req.Title != nil {
			pf.Title = *req.Title
		}
		if req.Style != nil {
			pf.Style = *req.Style
		}
		if req.AspectRatio != nil {
			pf.AspectRatio = *req.AspectRatio
		}
		if req.Brief != nil {
			pf.Brief = *req.Brief
		}
		if err := d.Store.Save(name, pf); err != nil {
			apperr.Abort(c, apperr.BadRequest("invalid_project", map[string]any{"reason": err.Error()}))
			return
		}
		c.JSON(http.StatusOK, pf)
	}
}

// DeleteProject 删除项目并广播 project_deleted。
func DeleteProject(d ProjectDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		if err := d.Store.Delete(name); err != nil {
			abortProjectErr(c, err)
			return
		}
		d.Hub.PublishDeleted(name)
		c.Status(http.StatusNoContent)
	}
}

// ProjectEventStream 是项目变更 SSE 通道。
//
// 首帧必发 snapshot，随后推增量 changes；project_deleted 后流结束。
// 心跳用注释行（客户端不可见），Python 侧的 _idle 哨兵同样不下发给客户端。
func ProjectEventStream(d ProjectDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		dir, err := d.Store.Dir(name)
		if err != nil {
			abortProjectErr(c, err)
			return
		}
		if _, err := os.Stat(filepath.Join(dir, "project.json")); err != nil {
			abortProjectErr(c, fs.ErrNotFound)
			return
		}

		ch, cancel := d.Hub.Subscribe(name)
		defer cancel()

		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")

		w := c.Writer
		// 首帧 snapshot：订阅建立后立刻下发，保证客户端拿到基线指纹。
		_, _ = w.Write(service.SnapshotFrame(service.Snapshot{
			ProjectName: name,
			Fingerprint: projectFingerprint(dir),
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		}))
		w.Flush()

		clientGone := c.Request.Context().Done()
		for {
			select {
			case frame, ok := <-ch:
				if !ok {
					// PublishDeleted 关闭通道 → 流结束。
					return
				}
				if _, err := w.Write(frame); err != nil {
					return
				}
				w.Flush()
			case <-time.After(time.Second):
				// 心跳：Python 侧的 _idle 哨兵同样不下发给客户端，这里用注释行。
				if _, err := w.Write(service.HeartbeatFrame(time.Now())); err != nil {
					return
				}
				w.Flush()
			case <-clientGone:
				return
			}
		}
	}
}

func projectFingerprint(dir string) string {
	st, err := os.Stat(filepath.Join(dir, "project.json"))
	if err != nil {
		return ""
	}
	return st.ModTime().UTC().Format(time.RFC3339Nano)
}

// abortProjectErr 映射项目相关错误。
//
// 注意：路径越界与项目不存在一律对外表现为 404 —— 不能让攻击者用错误差异
// 区分「名字非法」和「确实不存在」，也不回传任何服务器绝对路径。
func abortProjectErr(c *gin.Context, err error) {
	var traversal *fs.PathTraversalError
	switch {
	case errors.Is(err, fs.ErrNotFound), errors.Is(err, domain.ErrInvalidProjectName):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "project_not_found", nil))
	case errors.As(err, &traversal):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "project_not_found", nil))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
