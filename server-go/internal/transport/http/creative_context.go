package http

// 创建上下文解析路由（对应 server/routers/creative_context.py）。
// fail-closed 语义：引用必须恰好命中一个稳定 ID，歧义/缺失一律 409。

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/creative"
)

// CreativeContextDeps 是创建上下文 handler 的依赖。
type CreativeContextDeps struct {
	Context *creative.CreativeContextService
	Store   *service.ProjectStore
}

// RegisterCreativeContext 注册创建上下文路由；调用方负责挂 feature gate。
func RegisterCreativeContext(group *gin.RouterGroup, d CreativeContextDeps) {
	group.POST("/projects/:name/creative-context/resolve", resolveCreativeContext(d))
	group.POST("/projects/:name/creative-context/resolve-references", resolveCreativeReferences(d))
}

func resolveCreativeContext(d CreativeContextDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID := c.Param("name")
		if !d.Store.Exists(projectID) {
			apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "project_not_found"))
			return
		}
		req, ok := parseCreativeContextBody(c, true)
		if !ok {
			return
		}
		out, err := d.Context.ResolveCreationContext(c.Request.Context(), projectID, currentUserID(c), req)
		if err != nil {
			abortCreativeContextErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func resolveCreativeReferences(d CreativeContextDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID := c.Param("name")
		if !d.Store.Exists(projectID) {
			apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "project_not_found"))
			return
		}
		req, ok := parseCreativeContextBody(c, false)
		if !ok {
			return
		}
		out, err := d.Context.ResolveContextReferences(c.Request.Context(), projectID, currentUserID(c), req)
		if err != nil {
			abortCreativeContextErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// parseCreativeContextBody 解析两个端点共用的请求体；withPlan 为 true 时
// 校验 resolve 端点的必填 plan 字段，false 时校验 references 端点的
// context_references min_length=1。
func parseCreativeContextBody(c *gin.Context, withPlan bool) (creative.CreativeContextRequest, bool) {
	body, ok := decodeBody(c)
	if !ok {
		return creative.CreativeContextRequest{}, false
	}
	creativeBoardID, ok := requiredString(c, body, "creative_board_id")
	if !ok {
		return creative.CreativeContextRequest{}, false
	}
	creationPlanID := ""
	if withPlan {
		creationPlanID, ok = requiredString(c, body, "creation_plan_id")
		if !ok {
			return creative.CreativeContextRequest{}, false
		}
	}
	selected, ok := parseSelectedResources(c, body)
	if !ok {
		return creative.CreativeContextRequest{}, false
	}
	references, ok := parseContextReferences(c, body)
	if !ok {
		return creative.CreativeContextRequest{}, false
	}
	if !withPlan && len(references) == 0 {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"body", "context_references"}, Msg: "List should have at least 1 item after validation, not 0",
				Type: "too_short"},
		}})
		return creative.CreativeContextRequest{}, false
	}
	return creative.CreativeContextRequest{
		CreativeBoardID:        creativeBoardID,
		EpisodeID:              optionalBodyString(body, "episode_id"),
		ShotID:                 optionalBodyString(body, "shot_id"),
		SelectedResources:      selected,
		ContextReferences:      references,
		CurrentSkillID:         optionalBodyString(body, "current_skill_id"),
		CreationPlanID:         creationPlanID,
		WorkflowRunID:          optionalBodyString(body, "workflow_run_id"),
		Previewed:              optionalBodyBool(body, "previewed"),
		Confirmed:              bodyBoolOr(body, "confirmed"),
		QualityGatePassed:      optionalBodyBool(body, "quality_gate_passed"),
		ApprovalStatus:         optionalBodyString(body, "approval_status"),
		AllowGateBypass:        bodyBoolOr(body, "allow_gate_bypass"),
		GenerationMode:         optionalBodyString(body, "generation_mode"),
		GenerationModeOverride: optionalBodyString(body, "generation_mode_override"),
	}, true
}

func parseSelectedResources(c *gin.Context, body map[string]any) ([]domain.SelectedResource, bool) {
	raw, present := body["selected_resources"]
	if !present || raw == nil {
		return nil, true
	}
	list, ok := raw.([]any)
	if !ok {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"body", "selected_resources"}, Msg: "Input should be a valid list", Type: "list_type"},
		}})
		return nil, false
	}
	out := make([]domain.SelectedResource, 0, len(list))
	for i, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"body", "selected_resources", intLoc(i)}, Msg: "Input should be a valid dictionary",
					Type: "dict_type"},
			}})
			return nil, false
		}
		id, ok := entry["id"].(string)
		if !ok || id == "" {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"body", "selected_resources", intLoc(i), "id"}, Msg: "field required",
					Type: "value_error.missing"},
			}})
			return nil, false
		}
		resourceType, ok := entry["resource_type"].(string)
		if !ok || resourceType == "" {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"body", "selected_resources", intLoc(i), "resource_type"}, Msg: "field required",
					Type: "value_error.missing"},
			}})
			return nil, false
		}
		out = append(out, domain.SelectedResource{ID: id, ResourceType: resourceType})
	}
	return out, true
}

func parseContextReferences(c *gin.Context, body map[string]any) ([]domain.ContextReference, bool) {
	raw, present := body["context_references"]
	if !present || raw == nil {
		return nil, true
	}
	list, ok := raw.([]any)
	if !ok {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"body", "context_references"}, Msg: "Input should be a valid list", Type: "list_type"},
		}})
		return nil, false
	}
	out := make([]domain.ContextReference, 0, len(list))
	for i, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"body", "context_references", intLoc(i)}, Msg: "Input should be a valid dictionary",
					Type: "dict_type"},
			}})
			return nil, false
		}
		text, ok := entry["text"].(string)
		if !ok || text == "" {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"body", "context_references", intLoc(i), "text"}, Msg: "field required",
					Type: "value_error.missing"},
			}})
			return nil, false
		}
		var expectedType *string
		if s, ok := entry["expected_type"].(string); ok && s != "" {
			expectedType = &s
		}
		out = append(out, domain.ContextReference{Text: text, ExpectedType: expectedType})
	}
	return out, true
}

// intLoc 把列表下标渲染成 pydantic loc 里的字符串形式。
func intLoc(i int) string { return strconv.Itoa(i) }

// optionalBodyBool 取可选布尔字段；缺失 / null / 类型不对返回 nil。
func optionalBodyBool(body map[string]any, field string) *bool {
	raw, present := body[field]
	if !present || raw == nil {
		return nil
	}
	if b, ok := raw.(bool); ok {
		return &b
	}
	return nil
}

// bodyBoolOr 取带默认值的布尔字段；缺失 / null / 类型不对返回 def。
func bodyBoolOr(body map[string]any, field string) bool {
	if b := optionalBodyBool(body, field); b != nil {
		return *b
	}
	return false
}

// abortCreativeContextErr 把解析错误映射成 409 {"code", **details}
// （对应 except CreativeContextResolutionError as exc）。
func abortCreativeContextErr(c *gin.Context, err error) {
	var resolutionErr *domain.CreativeContextResolutionError
	if errors.As(err, &resolutionErr) {
		apperr.Abort(c, apperr.New(http.StatusConflict, resolutionErr.Code, resolutionErr.Details))
		return
	}
	apperr.Abort(c, apperr.Internal(err))
}
