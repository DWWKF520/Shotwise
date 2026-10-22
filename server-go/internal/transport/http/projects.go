package http

import (
	servicescript "github.com/shotwise/shotwise/internal/service/script"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/media"
)

// ProjectDeps 是项目 handler 的依赖。
type ProjectDeps struct {
	Store *service.ProjectStore
	Hub   *service.EventHub
}

// ListProjects 列出项目。
//
// 响应形状对齐 Python list_projects：{projects: [...]}，每项是读时计算的 ProjectSummary
// （含 thumbnail / status / media_summary），不含 total 字段。
func ListProjects(d ProjectDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		names, err := d.Store.List()
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		items := make([]gin.H, 0, len(names))
		for _, name := range names {
			items = append(items, projectSummaryItem(d.Store, name))
		}
		c.JSON(http.StatusOK, gin.H{"projects": items})
	}
}

// projectSummaryItem 计算单个项目的列表行；任何加载失败都退化成基本信息行（不 500）。
func projectSummaryItem(store *service.ProjectStore, name string) gin.H {
	project, err := store.ReadProjectRaw(name)
	if err != nil {
		return gin.H{
			"name":          name,
			"title":         "",
			"style":         "",
			"thumbnail":     nil,
			"status":        gin.H{},
			"media_summary": emptyMediaSummary(),
		}
	}

	preloaded := preloadProjectScripts(store, name, project)
	calculator := servicescript.NewStatusCalculator(store)
	status := calculator.CalculateProjectStatus(name, project, nil, preloaded)
	thumbnail := service.ResolveProjectCover(store, name, project, preloaded)

	title, _ := project["title"].(string)
	style := project["style"]
	if style == nil {
		style = ""
	}
	var createdAt, updatedAt any
	if metadata, ok := project["metadata"].(map[string]any); ok {
		createdAt = metadata["created_at"]
		updatedAt = metadata["updated_at"]
	}

	return gin.H{
		"name":              name,
		"title":             title,
		"style":             style,
		"style_template_id": project["style_template_id"],
		"style_image":       project["style_image"],
		"thumbnail":         thumbnail,
		"created_at":        createdAt,
		"updated_at":        updatedAt,
		"status":            status,
		"media_summary":     projectMediaSummary(store, name),
	}
}

// preloadProjectScripts 一次性加载项目全部剧本（键为 episode.script_file 原值）。
func preloadProjectScripts(store *service.ProjectStore, name string, project map[string]any) map[string]map[string]any {
	preloaded := map[string]map[string]any{}
	episodes, ok := project["episodes"].([]any)
	if !ok {
		return preloaded
	}
	for _, raw := range episodes {
		ep, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		scriptFile, _ := ep["script_file"].(string)
		if scriptFile == "" {
			continue
		}
		if script, err := store.ReadScriptJSON(name, scriptFile); err == nil {
			preloaded[scriptFile] = script
		}
	}
	return preloaded
}

// projectMediaSummary 读取持久化的媒体索引摘要；缺失或损坏时返回空摘要。
func projectMediaSummary(store *service.ProjectStore, name string) media.MediaIndexSummary {
	dir, err := store.Dir(name)
	if err != nil {
		return emptyMediaSummary()
	}
	summary, err := media.ProjectMediaCatalog(dir).Summary()
	if err != nil {
		return emptyMediaSummary()
	}
	return summary
}

// emptyMediaSummary 对应 Python _empty_project_media_summary。
func emptyMediaSummary() media.MediaIndexSummary {
	return media.MediaIndexSummary{Status: "stale", SummaryVersion: 1}
}

// CreateProjectRequest 创建项目的请求体。
type CreateProjectRequest struct {
	Name           string `json:"name"`
	Title          string `json:"title"`
	ContentMode    string `json:"content_mode" binding:"required"`
	GenerationMode string `json:"generation_mode" binding:"required"`
	Style          string `json:"style"`
	AspectRatio    string `json:"aspect_ratio"`
	GridStoryboard bool   `json:"grid_storyboard"`
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
		// 与 Python create_project 同语义：name 缺省时按标题 slug 自动生成；
		// title 与 name 均缺省时报 title_required（Python 400）。
		title := strings.TrimSpace(req.Title)
		name := strings.TrimSpace(req.Name)
		if title == "" && name == "" {
			apperr.Abort(c, apperr.BadRequest("title_required", nil))
			return
		}
		if name == "" {
			name = d.Store.GenerateProjectName(title)
		}
		pf, err := d.Store.Create(name, title, domain.ContentMode(req.ContentMode),
			domain.GenerationMode(req.GenerationMode), req.Style)
		if err != nil {
			switch {
			case errors.Is(err, domain.ErrInvalidProjectName):
				apperr.Abort(c, apperr.BadRequest("invalid_project_name", nil))
			case errors.Is(err, service.ErrProjectExists):
				apperr.Abort(c, apperr.Conflict("project_already_exists", map[string]any{"name": name}))
			default:
				apperr.Abort(c, apperr.Internal(err))
			}
			return
		}
		if req.AspectRatio != "" {
			pf.AspectRatio = req.AspectRatio
		}
		pf.GridStoryboard = req.GridStoryboard
		if err := d.Store.Save(name, pf); err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusCreated, gin.H{"success": true, "name": name, "project": pf})
	}
}

// GetProject 读取项目详情。
//
// 响应形状对齐 Python get_project：{project, scripts, asset_fingerprints}。
// project 是原始 JSON 注入读时计算字段后的结果；scripts 的键去掉 scripts/ 前缀；
// asset_fingerprints 是 {项目相对路径: mtime_ns}。
func GetProject(d ProjectDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		project, err := d.Store.ReadProjectRaw(name)
		if err != nil {
			abortProjectErr(c, err)
			return
		}

		calculator := servicescript.NewStatusCalculator(d.Store)
		project = calculator.EnrichProject(name, project)

		generationMode, _ := project["generation_mode"].(string)
		scripts := gin.H{}
		if episodes, ok := project["episodes"].([]any); ok {
			for _, rawEp := range episodes {
				ep, ok := rawEp.(map[string]any)
				if !ok {
					continue
				}
				scriptFile, _ := ep["script_file"].(string)
				if scriptFile == "" {
					continue
				}
				script, err := d.Store.ReadScriptJSON(name, scriptFile)
				if err != nil {
					// 剧本缺失/损坏不阻断详情：与 Python 只跳过该集一致。
					continue
				}
				key := strings.TrimPrefix(scriptFile, "scripts/")
				scripts[key] = calculator.EnrichScript(script, generationMode)
			}
		}

		fingerprints := map[string]int64{}
		if dir, err := d.Store.Dir(name); err == nil {
			fingerprints = service.ComputeProjectAssetFingerprints(dir)
		}

		c.JSON(http.StatusOK, gin.H{
			"project":            project,
			"scripts":            scripts,
			"asset_fingerprints": fingerprints,
		})
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
		// 走 Update 而不是 Get+Save：写盘守卫是「不更坏」，存量项目自带历史遗留字段问题时
		// 仍可改元数据，只要这次写入没有引入新的结构错误。
		var updated *domain.ProjectFile
		err := d.Store.Update(name, func(pf *domain.ProjectFile) error {
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
			updated = pf
			return nil
		})
		if err != nil {
			var invalid *service.ProjectInvalidError
			if errors.As(err, &invalid) {
				apperr.Abort(c, apperr.BadRequest("invalid_project", map[string]any{"reason": invalid.Error()}))
				return
			}
			abortProjectErr(c, err)
			return
		}
		c.JSON(http.StatusOK, updated)
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
