package http

// 草稿晋升路由（对应 server/routers/draft_promotions.py，9 个端点）。
//
// Agent / Upload / Online-AI 三源草稿的隔离评审流：草稿只存评审记录，正式剧本仍是
// 唯一内容真相源。origin 由服务端按路由指定，客户端不能伪造。
//
// 错误映射（与 Python router 一一对应）：
//   - 项目不存在            → 404 script_not_found {name: script_file}
//   - 剧本文件名非法        → 400 request_invalid
//   - 草稿不存在 / 越权     → 404 draft_not_found {id}
//   - 草稿已作废仍要改内容  → 400 request_invalid
//   - prepare/confirm 期间剧本消失 → 404 script_not_found {name: draft_id}

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
)

// DraftPromotionDeps 是草稿晋升 handler 的依赖。
type DraftPromotionDeps struct {
	Store  *service.ProjectStore
	Drafts *domain.DraftPromotionService
}

// RegisterDraftPromotions 注册草稿晋升路由。
//
// 路由树注意：/draft-promotions/upload 与 /draft-promotions/online-ai 是静态段，
// 与 /draft-promotions/:draft_id 同层，gin 按静态优先匹配（与 Python 侧
// 「先注册具体路径」的实际效果一致）。
func RegisterDraftPromotions(group *gin.RouterGroup, d DraftPromotionDeps) {
	group.POST("/projects/:project_name/draft-promotions", createDraftPromotion(d, domain.DraftOriginAgent))
	group.POST("/projects/:project_name/draft-promotions/upload", createDraftPromotion(d, domain.DraftOriginUpload))
	group.POST("/projects/:project_name/draft-promotions/online-ai", createDraftPromotion(d, domain.DraftOriginOnlineAI))
	group.GET("/projects/:project_name/draft-promotions", listDraftPromotions(d))
	group.GET("/projects/:project_name/draft-promotions/:draft_id", getDraftPromotion(d))
	group.PATCH("/projects/:project_name/draft-promotions/:draft_id", updateDraftPromotion(d))
	group.POST("/projects/:project_name/draft-promotions/:draft_id/abandon", abandonDraftPromotion(d))
	group.POST("/projects/:project_name/draft-promotions/:draft_id/prepare", prepareDraftPromotion(d))
	group.POST("/projects/:project_name/draft-promotions/:draft_id/confirm", confirmDraftPromotion(d))
}

// --- 请求体 ---

type draftCreateRequest struct {
	ScriptFile *string        `json:"script_file"`
	Content    map[string]any `json:"content"`
}

type draftUpdateRequest struct {
	Content map[string]any `json:"content"`
}

type draftConfirmRequest struct {
	ConfirmationToken *string `json:"confirmation_token"`
}

// ---  handlers ---

func createDraftPromotion(d DraftPromotionDeps, origin domain.DraftOrigin) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req draftCreateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		if req.ScriptFile == nil {
			abortMissingField(c, "script_file")
			return
		}
		if err := draftBoundedString(*req.ScriptFile, 512); err != nil {
			abortFieldConstraint(c, "script_file", err.Error())
			return
		}
		if req.Content == nil {
			abortMissingField(c, "content")
			return
		}
		target := domain.DraftTarget{ProjectName: c.Param("project_name"), ScriptFile: *req.ScriptFile}
		draft, err := d.Drafts.Create(target, req.Content, origin, currentUserID(c))
		if err != nil {
			abortDraftErr(c, err, *req.ScriptFile)
			return
		}
		c.JSON(http.StatusCreated, draftResponse(draft))
	}
}

func listDraftPromotions(d DraftPromotionDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectName := c.Param("project_name")
		actorID := currentUserID(c)
		var drafts []*domain.Draft
		var err error
		if scriptFile := c.Query("script_file"); scriptFile != "" {
			drafts, err = d.Drafts.ListDrafts(domain.DraftTarget{ProjectName: projectName, ScriptFile: scriptFile}, actorID)
		} else {
			drafts, err = d.Drafts.Repo().ListAll(projectName, actorID)
		}
		if err != nil {
			abortDraftErr(c, err, "")
			return
		}
		items := make([]gin.H, 0, len(drafts))
		for _, draft := range drafts {
			items = append(items, draftResponse(draft))
		}
		c.JSON(http.StatusOK, gin.H{"drafts": items})
	}
}

func getDraftPromotion(d DraftPromotionDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		draft, ok := loadScopedDraft(c, d)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, draftResponse(draft))
	}
}

func updateDraftPromotion(d DraftPromotionDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		draft, ok := loadScopedDraft(c, d)
		if !ok {
			return
		}
		var req draftUpdateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		if req.Content == nil {
			abortMissingField(c, "content")
			return
		}
		updated, err := d.Drafts.Update(draft.ID, req.Content, currentUserID(c))
		if err != nil {
			abortDraftErr(c, err, draft.Target.ScriptFile)
			return
		}
		c.JSON(http.StatusOK, draftResponse(updated))
	}
}

func abandonDraftPromotion(d DraftPromotionDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		draft, ok := loadScopedDraft(c, d)
		if !ok {
			return
		}
		abandoned, err := d.Drafts.Abandon(draft.ID, currentUserID(c))
		if err != nil {
			abortDraftErr(c, err, draft.Target.ScriptFile)
			return
		}
		c.JSON(http.StatusOK, draftResponse(abandoned))
	}
}

func prepareDraftPromotion(d DraftPromotionDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		draft, ok := loadScopedDraft(c, d)
		if !ok {
			return
		}
		preparation, err := d.Drafts.PreparePromotion(draft.ID, currentUserID(c))
		if err != nil {
			abortDraftErr(c, err, draft.ID)
			return
		}
		// Python 侧返回 **_draft_response(draft) + preparation 字段：
		// preparation 里的草稿快照是 prepare 之后的最新状态。
		refreshed, err := d.Drafts.GetDraft(draft.ID, currentUserID(c))
		if err != nil {
			abortDraftErr(c, err, draft.ID)
			return
		}
		body := draftResponse(refreshed)
		body["status"] = preparation.Status
		body["validation_issues"] = preparation.ValidationIssues
		body["conflicts"] = preparation.Conflicts
		body["auto_merged_paths"] = preparation.AutoMergedPaths
		body["confirmation_token"] = preparation.ConfirmationToken
		body["preview_content"] = preparation.PreviewContent
		c.JSON(http.StatusOK, body)
	}
}

func confirmDraftPromotion(d DraftPromotionDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		draft, ok := loadScopedDraft(c, d)
		if !ok {
			return
		}
		var req draftConfirmRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		if req.ConfirmationToken == nil {
			abortMissingField(c, "confirmation_token")
			return
		}
		if err := draftBoundedString(*req.ConfirmationToken, 512); err != nil {
			abortFieldConstraint(c, "confirmation_token", err.Error())
			return
		}
		result, err := d.Drafts.ConfirmPromotion(draft.ID, *req.ConfirmationToken, currentUserID(c))
		if err != nil {
			abortDraftErr(c, err, draft.ID)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"status":            result.Status,
			"validation_issues": result.ValidationIssues,
			"promoted_revision": result.PromotedRevision,
		})
	}
}

// loadScopedDraft 读取草稿并校验项目 / 属主scope；越权与不存在同报 404 draft_not_found
// （对应 Python _ensure_project_scope / _ensure_actor_scope）。
func loadScopedDraft(c *gin.Context, d DraftPromotionDeps) (*domain.Draft, bool) {
	draft, err := d.Drafts.GetDraft(c.Param("draft_id"), currentUserID(c))
	if err != nil {
		abortDraftErr(c, err, "")
		return nil, false
	}
	if draft.Target.ProjectName != c.Param("project_name") {
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "draft_not_found", map[string]any{"id": draft.ID}))
		return nil, false
	}
	return draft, true
}

// draftResponse 对应 Python _draft_response。
func draftResponse(draft *domain.Draft) gin.H {
	return gin.H{
		"draft_id":         draft.ID,
		"project_name":     draft.Target.ProjectName,
		"script_file":      draft.Target.ScriptFile,
		"origin":           draft.Origin,
		"base_revision":    draft.BaseRevision,
		"base_fingerprint": draft.BaseFingerprint,
		"content":          draft.Content,
		"prepared":         draft.Prepared != nil,
		"status":           draft.Status,
	}
}

// abortDraftErr 把领域 / 仓储错误映射成统一 {"detail": ...} 信封。
// scriptFile 用于 404 script_not_found 的 name 字段（create 用剧本名，
// prepare/confirm 用草稿 id——与 Python 两处的入参保持一致）。
func abortDraftErr(c *gin.Context, err error, scriptFile string) {
	switch {
	case errors.Is(err, domain.ErrDraftNotFound):
		// Python 侧 NotFoundError("draft_not_found", id=...) 经全局 handler 渲染成
		// {"detail": "<文案>"}；该 key 不在三语目录里，回落成 key 本身（与 Python 一致）。
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "draft_not_found", map[string]any{"id": c.Param("draft_id")}))
	case errors.Is(err, domain.ErrDraftAbandoned):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
	case errors.Is(err, service.ErrDraftProjectNotFound):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "script_not_found", map[string]any{"name": scriptFile}))
	case errors.Is(err, service.ErrDraftScriptNameInvalid):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}

// draftBoundedString 复刻 Pydantic Field(min_length=1, max_length=max) 的约束，
// 返回的字符串直接作为 422 的 type 字段。
func draftBoundedString(value string, max int) error {
	if value == "" {
		return errors.New("string_too_short")
	}
	if len([]rune(value)) > max {
		return errors.New("string_too_long")
	}
	return nil
}
