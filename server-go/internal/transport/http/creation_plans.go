package http

// Creation Plan 路由（对应 server/routers/creation_plans.py，13 个端点）。
//
// 双开关：creation_plan 与 official_creation_skills 缺一即整组 404
// （与 Python 侧 router 级 dependencies 等价）。
// 项目一律经 ProjectStore 读取：Python 侧 manager.load_project 的等价物，
// 项目不存在返回 404 project_not_found。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/creative"
)

// CreationPlanDeps 是创作计划 handler 的依赖。
type CreationPlanDeps struct {
	Store *service.ProjectStore
	Plans *creative.CreationPlanService
}

// RegisterCreationPlans 注册创作计划路由。
//
// 路由树注意：/creation-plans/preview 是静态段，与 /creation-plans/:plan_id 同层，
// gin 按静态优先匹配；/creation-compatibility-events/metrics 同理。
func RegisterCreationPlans(group *gin.RouterGroup, d CreationPlanDeps) {
	group.GET("/creation-skills", listCreationSkills(d))
	group.GET("/creation-skills/:skill_id/versions", listCreationSkillVersions(d))
	group.GET("/creation-resources", listCreationResources(d))

	admin := group.Group("", auth.RequireAdmin())
	admin.POST("/creation-skills/:skill_id/deactivate", deactivateCreationSkill(d))

	group.POST("/creation-plans/preview", previewCreationPlan(d))
	group.GET("/creation-plans/:plan_id", getCreationPlan(d))
	group.POST("/creation-plans/:plan_id/start", startCreationPlan(d))
	group.POST("/creation-plans/:plan_id/cancel", cancelCreationPlan(d))
	group.POST("/creation-plans/:plan_id/invalidate", invalidateCreationPlan(d))
	group.POST("/creation-plans/:plan_id/restart", restartCreationPlan(d))
	group.POST("/creation-plans/:plan_id/recompile", recompileCreationPlan(d))

	group.POST("/creation-compatibility-events/:event_id/outcome", recordCompatibilityOutcome(d))
	group.GET("/creation-compatibility-events/metrics", getCompatibilityMetrics(d))
}

// --- 请求体 ---

type creationPlanPreviewRequest struct {
	WorkspaceID            *string          `json:"workspace_id"`
	ProjectID              *string          `json:"project_id"`
	CreationSkillVersionID *string          `json:"creation_skill_version_id"`
	ResourceIDs            []string         `json:"resource_ids"`
	ResourceTypes          []string         `json:"resource_types"`
	ResourceMapping        []map[string]any `json:"resource_mapping"`
	Parameters             map[string]any   `json:"parameters"`
	WorkflowRevision       *string          `json:"workflow_revision"`
	EstimatedCost          *float64         `json:"estimated_cost"`
	Steps                  []string         `json:"steps"`
	ReviewPoints           []string         `json:"review_points"`
	IdempotencyKey         *string          `json:"idempotency_key"`
}

type creationPlanStartRequest struct {
	Mode             *string  `json:"mode"`
	ScriptRevisionID *string  `json:"script_revision_id"`
	EpisodeID        *string  `json:"episode_id"`
	BudgetLimit      *float64 `json:"budget_limit"`
	CostConfirmed    *bool    `json:"cost_confirmed"`
	ReviewConfirmed  *bool    `json:"review_confirmed"`
}

type compatibilityOutcomeRequest struct {
	Outcome *string `json:"outcome"`
}

var creationPlanModes = map[string]bool{"auto": true, "manual": true, "hybrid": true}

// --- 项目读取 ---

// loadProjectMap 读取项目并转成通用映射；不存在时写出 404。
func loadProjectMap(c *gin.Context, store *service.ProjectStore, projectID string) (map[string]any, *domain.ProjectFile, bool) {
	file, err := store.Get(projectID)
	if err != nil || file == nil {
		apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "project_not_found"))
		return nil, nil, false
	}
	encoded, err := json.Marshal(file)
	if err != nil {
		apperr.Abort(c, apperr.Internal(err))
		return nil, nil, false
	}
	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		apperr.Abort(c, apperr.Internal(err))
		return nil, nil, false
	}
	return asMap, file, true
}

// queryProjectID 解析 project_id 查询参数。
func queryProjectID(c *gin.Context) (string, bool) {
	projectID := strings.TrimSpace(c.Query("project_id"))
	if projectID == "" {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"query", "project_id"}, Msg: "field required", Type: "value_error.missing"},
		}})
		return "", false
	}
	return projectID, true
}

// --- 端点 ---

func listCreationSkills(d CreationPlanDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID, ok := queryProjectID(c)
		if !ok {
			return
		}
		project, _, ok := loadProjectMap(c, d.Store, projectID)
		if !ok {
			return
		}
		available := map[string]bool{}
		for _, value := range c.QueryArray("resource_types") {
			trimmed := strings.TrimSpace(value)
			if trimmed != "" {
				available[trimmed] = true
			}
		}
		items, err := d.Plans.Catalog().ListCreationSkills(c.Request.Context(), project, available)
		if err != nil {
			abortCreationErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

func listCreationSkillVersions(d CreationPlanDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := d.Plans.Catalog().ListCreationSkillVersions(c.Request.Context(), c.Param("skill_id"))
		if err != nil {
			abortCreationErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

func listCreationResources(d CreationPlanDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID, ok := queryProjectID(c)
		if !ok {
			return
		}
		project, _, ok := loadProjectMap(c, d.Store, projectID)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": creative.ListCreationResources(project)})
	}
}

func deactivateCreationSkill(d CreationPlanDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		user := auth.Current(c)
		role := "user"
		if user != nil {
			role = user.Role
		}
		ok, err := d.Plans.Catalog().DeactivateCreationSkill(c.Request.Context(), c.Param("skill_id"), role)
		if err != nil {
			abortCreationErr(c, err)
			return
		}
		if !ok {
			apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "creation_skill_not_found"))
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": c.Param("skill_id"), "active": false})
	}
}

func previewCreationPlan(d CreationPlanDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req creationPlanPreviewRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		workspaceID, ok := requiredWorkflowString(c, req.WorkspaceID, "workspace_id", 128)
		if !ok {
			return
		}
		projectID, ok := requiredWorkflowString(c, req.ProjectID, "project_id", 200)
		if !ok {
			return
		}
		skillVersionID, ok := requiredWorkflowString(c, req.CreationSkillVersionID, "creation_skill_version_id", 128)
		if !ok {
			return
		}
		idempotencyKey, ok := requiredWorkflowString(c, req.IdempotencyKey, "idempotency_key", 256)
		if !ok {
			return
		}
		if len(req.ResourceIDs) > 1000 || len(req.ResourceTypes) > 32 || len(req.ResourceMapping) > 1000 {
			abortWorkflowField(c, "resource_ids", "too_long")
			return
		}
		if req.WorkflowRevision != nil && (*req.WorkflowRevision == "" || len(*req.WorkflowRevision) > 128) {
			abortWorkflowField(c, "workflow_revision", "string_too_short")
			return
		}
		if req.EstimatedCost != nil && *req.EstimatedCost < 0 {
			abortWorkflowField(c, "estimated_cost", "greater_than_equal")
			return
		}
		if len(req.Steps) > 256 || len(req.ReviewPoints) > 256 {
			abortWorkflowField(c, "steps", "too_long")
			return
		}
		project, file, ok := loadProjectMap(c, d.Store, projectID)
		if !ok {
			return
		}
		parameters := req.Parameters
		if parameters == nil {
			parameters = map[string]any{}
		}
		result, err := d.Plans.CreateCreationPlanPreview(c.Request.Context(), creative.CreationPlanPreviewParams{
			UserID:                 currentUserID(c),
			WorkspaceID:            workspaceID,
			ProjectID:              projectID,
			CreationSkillVersionID: skillVersionID,
			Project:                project,
			ProjectFile:            file,
			ResourceIDs:            req.ResourceIDs,
			ResourceTypes:          req.ResourceTypes,
			ResourceMapping:        req.ResourceMapping,
			Parameters:             parameters,
			WorkflowRevision:       req.WorkflowRevision,
			EstimatedCost:          req.EstimatedCost,
			Steps:                  req.Steps,
			ReviewPoints:           req.ReviewPoints,
			IdempotencyKey:         idempotencyKey,
		})
		if err != nil {
			abortCreationErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func getCreationPlan(d CreationPlanDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Plans.GetCreationPlan(c.Request.Context(), c.Param("plan_id"), currentUserID(c))
		if err != nil {
			abortCreationErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

// bindStartRequest 解析 start / restart 的公共请求体。
func bindStartRequest(c *gin.Context) (creative.StartCreationPlanParams, bool) {
	var req creationPlanStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortBodyBind(c, err)
		return creative.StartCreationPlanParams{}, false
	}
	mode := "hybrid"
	if req.Mode != nil {
		if !creationPlanModes[*req.Mode] {
			abortWorkflowField(c, "mode", "string_pattern_mismatch")
			return creative.StartCreationPlanParams{}, false
		}
		mode = *req.Mode
	}
	if req.BudgetLimit != nil && *req.BudgetLimit < 0 {
		abortWorkflowField(c, "budget_limit", "greater_than_equal")
		return creative.StartCreationPlanParams{}, false
	}
	return creative.StartCreationPlanParams{
		Mode:             mode,
		ScriptRevisionID: req.ScriptRevisionID,
		EpisodeID:        req.EpisodeID,
		BudgetLimit:      req.BudgetLimit,
		CostConfirmed:    req.CostConfirmed != nil && *req.CostConfirmed,
		ReviewConfirmed:  req.ReviewConfirmed != nil && *req.ReviewConfirmed,
	}, true
}

// planProject 由计划快照里的 project_context.project_id 反查项目。
//
// Python 侧先 get_creation_plan 再按快照里的 project_id 读项目；这里复用同一顺序，
// 顺带把「快照非法」翻译成 500 creation_plan_snapshot_invalid（与 Python 一致）。
func planProject(c *gin.Context, d CreationPlanDeps) (map[string]any, bool) {
	result, err := d.Plans.GetCreationPlan(c.Request.Context(), c.Param("plan_id"), currentUserID(c))
	if err != nil {
		abortCreationErr(c, err)
		return nil, false
	}
	snapshot, ok := result["project_context"].(map[string]any)
	if !ok {
		apperr.Abort(c, apperr.NewLiteral(http.StatusInternalServerError, "creation_plan_snapshot_invalid"))
		return nil, false
	}
	projectID, _ := snapshot["project_id"].(string)
	project, _, ok := loadProjectMap(c, d.Store, projectID)
	return project, ok
}

func startCreationPlan(d CreationPlanDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		params, ok := bindStartRequest(c)
		if !ok {
			return
		}
		project, ok := planProject(c, d)
		if !ok {
			return
		}
		params.PlanID = c.Param("plan_id")
		params.UserID = currentUserID(c)
		params.Project = project
		result, err := d.Plans.StartCreationPlan(c.Request.Context(), params)
		if err != nil {
			abortCreationErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func cancelCreationPlan(d CreationPlanDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Plans.CancelCreationPlan(c.Request.Context(), c.Param("plan_id"), currentUserID(c))
		if err != nil {
			abortCreationErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func invalidateCreationPlan(d CreationPlanDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Plans.InvalidateCreationPlan(c.Request.Context(), c.Param("plan_id"), currentUserID(c))
		if err != nil {
			abortCreationErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func restartCreationPlan(d CreationPlanDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		params, ok := bindStartRequest(c)
		if !ok {
			return
		}
		project, ok := planProject(c, d)
		if !ok {
			return
		}
		params.PlanID = c.Param("plan_id")
		params.UserID = currentUserID(c)
		params.Project = project
		result, err := d.Plans.RestartCreationPlan(c.Request.Context(), params)
		if err != nil {
			abortCreationErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func recompileCreationPlan(d CreationPlanDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		project, ok := planProject(c, d)
		if !ok {
			return
		}
		result, err := d.Plans.RecompileCreationPlan(
			c.Request.Context(), c.Param("plan_id"), currentUserID(c), project,
		)
		if err != nil {
			abortCreationErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func recordCompatibilityOutcome(d CreationPlanDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req compatibilityOutcomeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		outcome, ok := requiredWorkflowString(c, req.Outcome, "outcome", 32)
		if !ok {
			return
		}
		if !creative.CreationCompatibilityOutcomes[outcome] {
			abortWorkflowField(c, "outcome", "string_pattern_mismatch")
			return
		}
		result, err := d.Plans.RecordCompatibilityOutcome(c.Request.Context(), c.Param("event_id"), outcome)
		if err != nil {
			var notFound *creative.CreationPlanNotFoundError
			switch {
			case errors.As(err, &notFound):
				apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "compatibility_event_not_found"))
			default:
				abortCreationErr(c, err)
			}
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func getCompatibilityMetrics(d CreationPlanDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Plans.CompatibilityMetrics(c.Request.Context())
		if err != nil {
			abortCreationErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

// --- 错误映射 ---

// abortCreationErr 把创作计划领域的错误映射成统一 {"detail": ...} 信封。
func abortCreationErr(c *gin.Context, err error) {
	var (
		notFound       *creative.CreationPlanNotFoundError
		startError     *creative.CreationPlanStartError
		planError      *domain.CreationPlanError
		forbidden      *creative.CreationSkillForbiddenError
		invalidOutcome *creative.CreationPlanInvalidOutcomeError
		validation     *domain.WorkflowValidationError
		workflowNF     *creative.WorkflowNotFoundError
		workflowCF     *creative.WorkflowConflictError
		manifest       *creative.CreationSkillManifestConflictError
	)
	switch {
	case errors.As(err, &notFound):
		apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "creation_plan_not_found"))
	case errors.As(err, &startError):
		params := map[string]any{"code": startError.Code}
		for key, value := range startError.Report {
			if key != "code" {
				params[key] = value
			}
		}
		apperr.Abort(c, apperr.New(http.StatusConflict, startError.Code, params))
	case errors.As(err, &invalidOutcome):
		apperr.Abort(c, apperr.NewLiteral(http.StatusBadRequest, "invalid_compatibility_outcome"))
	case errors.As(err, &planError):
		apperr.Abort(c, apperr.NewLiteral(http.StatusBadRequest, planError.Message))
	case errors.As(err, &forbidden):
		apperr.Abort(c, apperr.NewLiteral(http.StatusForbidden, forbidden.Code))
	case errors.As(err, &validation):
		apperr.Abort(c, apperr.New(http.StatusBadRequest, validation.Code, validation.Params))
	case errors.As(err, &workflowNF):
		apperr.Abort(c, apperr.New(http.StatusNotFound, workflowNF.Code, workflowNF.Params))
	case errors.As(err, &workflowCF):
		apperr.Abort(c, apperr.New(http.StatusConflict, workflowCF.Code, workflowCF.Params))
	case errors.As(err, &manifest):
		apperr.Abort(c, apperr.NewLiteral(http.StatusInternalServerError, manifest.Message))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
