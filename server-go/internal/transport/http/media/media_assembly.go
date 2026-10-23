package media

// 媒体装配计划路由（对应 server/routers/media_assembly.py）。
//
// 本组端点只做确定性的装配意图持久化与校验，不触发 FFmpeg、不建渲染任务、不改媒体文件。

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	domainmedia "github.com/shotwise/shotwise/internal/domain/media"
	"github.com/shotwise/shotwise/internal/service/media"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"
)

// MediaAssemblyDeps 是装配计划 handler 的依赖。
type MediaAssemblyDeps struct {
	Plans *media.MediaAssemblyService
}

// RegisterMediaAssembly 注册装配计划路由。
func RegisterMediaAssembly(group *gin.RouterGroup, d MediaAssemblyDeps) {
	group.POST("/projects/:name/assembly-plans", createAssemblyPlan(d))
	group.GET("/projects/:name/assembly-plans", listAssemblyPlans(d))
	group.GET("/assembly-plans/:plan_id", getAssemblyPlan(d))
	group.POST("/assembly-plans/:plan_id/revisions", createAssemblyPlanRevision(d))
	group.POST("/assembly-plans/:plan_id/status", transitionAssemblyPlan(d))
	group.POST("/assembly-plans/:plan_id/preview-confirm", confirmAssemblyPreview(d))
	group.POST("/assembly-plans/:plan_id/render-confirm", confirmAssemblyRender(d))
	group.POST("/assembly-plans/:plan_id/stale-check", checkAssemblyPlanStale(d))
}

// assemblyDocumentRequest 是装配文档请求体（source_snapshot / timeline 必填）。
type assemblyDocumentRequest struct {
	SourceSnapshot *map[string]any   `json:"source_snapshot"`
	Timeline       *[]map[string]any `json:"timeline"`
	Audio          map[string]any    `json:"audio"`
	Subtitle       map[string]any    `json:"subtitle"`
	Packaging      map[string]any    `json:"packaging"`
	OutputProfile  map[string]any    `json:"output_profile"`
}

type assemblyPlanCreateRequest struct {
	assemblyDocumentRequest
	Name          *string `json:"name"`
	Scope         *string `json:"scope"`
	EpisodeNumber *int64  `json:"episode_number"`
}

type assemblyPlanStatusRequest struct {
	Status *string `json:"status"`
}

type assemblyPreviewConfirmRequest struct {
	RevisionNumber *int64 `json:"revision_number"`
}

type assemblyStaleCheckRequest struct {
	SourceSnapshot *map[string]any `json:"source_snapshot"`
}

// bindAssemblyDocument 解析装配文档请求体，缺失必填字段时写出 422。
func bindAssemblyDocument(c *gin.Context) (map[string]any, []map[string]any, map[string]any, map[string]any,
	map[string]any, map[string]any, bool) {
	var req assemblyDocumentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpkit.AbortBodyBind(c, err)
		return nil, nil, nil, nil, nil, nil, false
	}
	if req.SourceSnapshot == nil {
		httpkit.AbortMissingField(c, "source_snapshot")
		return nil, nil, nil, nil, nil, nil, false
	}
	if req.Timeline == nil {
		httpkit.AbortMissingField(c, "timeline")
		return nil, nil, nil, nil, nil, nil, false
	}
	return *req.SourceSnapshot, *req.Timeline, req.Audio, req.Subtitle, req.Packaging, req.OutputProfile, true
}

func createAssemblyPlan(d MediaAssemblyDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req assemblyPlanCreateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			httpkit.AbortBodyBind(c, err)
			return
		}
		if req.SourceSnapshot == nil {
			httpkit.AbortMissingField(c, "source_snapshot")
			return
		}
		if req.Timeline == nil {
			httpkit.AbortMissingField(c, "timeline")
			return
		}
		if req.Name == nil || *req.Name == "" {
			httpkit.AbortMissingField(c, "name")
			return
		}
		if len(*req.Name) > 200 {
			httpkit.AbortFieldConstraint(c, "name", "string_too_long")
			return
		}
		scope := "episode"
		if req.Scope != nil {
			if *req.Scope == "" || len(*req.Scope) > 32 {
				httpkit.AbortFieldConstraint(c, "scope", "string_too_short")
				return
			}
			scope = *req.Scope
		}
		if req.EpisodeNumber != nil && *req.EpisodeNumber < 1 {
			httpkit.AbortFieldConstraint(c, "episode_number", "greater_than_equal")
			return
		}

		plan, err := d.Plans.CreatePlan(c.Request.Context(), media.CreatePlanParams{
			UserID:         httpkit.CurrentUserID(c),
			ProjectName:    c.Param("name"),
			Name:           *req.Name,
			Scope:          scope,
			EpisodeNumber:  req.EpisodeNumber,
			SourceManifest: *req.SourceSnapshot,
			Timeline:       *req.Timeline,
			Audio:          req.Audio,
			Subtitle:       req.Subtitle,
			Packaging:      req.Packaging,
			OutputProfile:  req.OutputProfile,
		})
		if err != nil {
			abortAssemblyErr(c, err)
			return
		}
		c.JSON(http.StatusCreated, plan)
	}
}

func listAssemblyPlans(d MediaAssemblyDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := d.Plans.ListPlans(c.Request.Context(), httpkit.CurrentUserID(c), c.Param("name"))
		if err != nil {
			abortAssemblyErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

func getAssemblyPlan(d MediaAssemblyDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		plan, err := d.Plans.GetPlan(c.Request.Context(), c.Param("plan_id"), httpkit.CurrentUserID(c))
		if err != nil {
			abortAssemblyErr(c, err)
			return
		}
		c.JSON(http.StatusOK, plan)
	}
}

func createAssemblyPlanRevision(d MediaAssemblyDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		sourceSnapshot, timeline, audio, subtitle, packaging, outputProfile, ok := bindAssemblyDocument(c)
		if !ok {
			return
		}
		plan, err := d.Plans.CreateRevision(c.Request.Context(), c.Param("plan_id"), media.CreateRevisionParams{
			UserID:         httpkit.CurrentUserID(c),
			SourceManifest: sourceSnapshot,
			Timeline:       timeline,
			Audio:          audio,
			Subtitle:       subtitle,
			Packaging:      packaging,
			OutputProfile:  outputProfile,
		})
		if err != nil {
			abortAssemblyErr(c, err)
			return
		}
		c.JSON(http.StatusOK, plan)
	}
}

func transitionAssemblyPlan(d MediaAssemblyDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req assemblyPlanStatusRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			httpkit.AbortBodyBind(c, err)
			return
		}
		if req.Status == nil || *req.Status == "" || len(*req.Status) > 24 {
			httpkit.AbortFieldConstraint(c, "status", "string_too_short")
			return
		}
		plan, err := d.Plans.TransitionPlan(c.Request.Context(), c.Param("plan_id"), httpkit.CurrentUserID(c), *req.Status)
		if err != nil {
			abortAssemblyErr(c, err)
			return
		}
		c.JSON(http.StatusOK, plan)
	}
}

func confirmAssemblyPreview(d MediaAssemblyDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		revision, ok := bindRevisionNumber(c)
		if !ok {
			return
		}
		plan, err := d.Plans.ConfirmPreview(c.Request.Context(), c.Param("plan_id"), httpkit.CurrentUserID(c), revision)
		if err != nil {
			abortAssemblyErr(c, err)
			return
		}
		c.JSON(http.StatusOK, plan)
	}
}

func confirmAssemblyRender(d MediaAssemblyDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		revision, ok := bindRevisionNumber(c)
		if !ok {
			return
		}
		plan, err := d.Plans.ConfirmRender(c.Request.Context(), c.Param("plan_id"), httpkit.CurrentUserID(c), revision)
		if err != nil {
			abortAssemblyErr(c, err)
			return
		}
		c.JSON(http.StatusOK, plan)
	}
}

func checkAssemblyPlanStale(d MediaAssemblyDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req assemblyStaleCheckRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			httpkit.AbortBodyBind(c, err)
			return
		}
		if req.SourceSnapshot == nil {
			httpkit.AbortMissingField(c, "source_snapshot")
			return
		}
		plan, err := d.Plans.CheckStale(c.Request.Context(), c.Param("plan_id"), httpkit.CurrentUserID(c), *req.SourceSnapshot)
		if err != nil {
			abortAssemblyErr(c, err)
			return
		}
		c.JSON(http.StatusOK, plan)
	}
}

// bindRevisionNumber 解析 preview/render 确认请求体里的 revision_number（≥1）。
func bindRevisionNumber(c *gin.Context) (int64, bool) {
	var req assemblyPreviewConfirmRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpkit.AbortBodyBind(c, err)
		return 0, false
	}
	if req.RevisionNumber == nil {
		httpkit.AbortMissingField(c, "revision_number")
		return 0, false
	}
	if *req.RevisionNumber < 1 {
		httpkit.AbortFieldConstraint(c, "revision_number", "greater_than_equal")
		return 0, false
	}
	return *req.RevisionNumber, true
}

// abortAssemblyErr 把装配计划领域错误映射成统一 {"detail": ...} 信封。
func abortAssemblyErr(c *gin.Context, err error) {
	var (
		notFound   *media.AssemblyPlanNotFoundError
		conflict   *media.AssemblyPlanConflictError
		validation *domainmedia.AssemblyPlanValidationError
	)
	switch {
	case errors.As(err, &validation):
		apperr.Abort(c, apperr.New(http.StatusUnprocessableEntity, "invalid_assembly_plan",
			map[string]any{"errors": validation.Errors}))
	case errors.As(err, &notFound):
		// Python 侧这里是字面量字符串 detail，不走 i18n；保持原样。
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "assembly_plan_not_found", nil))
	case errors.As(err, &conflict):
		params := map[string]any{}
		if conflict.Status != "" {
			params["status"] = conflict.Status
		}
		apperr.Abort(c, apperr.New(http.StatusConflict, conflict.Code, params))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
