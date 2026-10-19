package http

// Shotwise Flow 路由（对应 server/routers/workflows.py，41 个端点）。
//
// 鉴权：普通端点挂在调用方已加的鉴权组下；/admin/** 额外要求 admin 角色。
// 错误：领域错误统一映射成 404 / 409 / 400 的 {"code": ...} 信封，
// 请求体约束违例沿用 FastAPI 的 422 数组形状。

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
)

// WorkflowDeps 是工作流 handler 的依赖。
type WorkflowDeps struct {
	Workflows *service.WorkflowService
}

// RegisterWorkflows 注册工作流路由。
//
// 路由树注意：/workflow-templates/mine 是静态段，与 /workflow-templates/:template_id
// 同层，gin 按静态优先匹配；/workflows/:definition_id/revisions/:revision_id/revert
// 与 /workflows/:definition_id/revisions 段数不同，不冲突。
func RegisterWorkflows(group *gin.RouterGroup, d WorkflowDeps) {
	group.POST("/workflows", createWorkflow(d))
	group.GET("/workflows/:definition_id", getWorkflow(d))
	group.GET("/workflows/:definition_id/template-upgrade", getTemplateUpgrade(d))
	group.POST("/workflows/:definition_id/template-upgrade", applyTemplateUpgrade(d))
	group.POST("/workflows/:definition_id/revisions", createRevision(d))
	group.GET("/workflows/:definition_id/revisions", listWorkflowRevisions(d))
	group.POST("/workflows/:definition_id/revisions/:revision_id/revert", revertWorkflowRevision(d))
	group.GET("/workflows/:definition_id/export", exportWorkflow(d))

	group.POST("/workflow-revisions/:revision_id/validate", validateRevision(d))
	group.POST("/workflow-revisions/:revision_id/publish", publishRevision(d))
	group.POST("/workflow-revisions/:revision_id/runs", planRun(d))

	group.GET("/workflow-runs/:run_id", getRun(d))
	group.POST("/workflow-runs/:run_id/retry", retryWorkflowRun(d))
	group.POST("/workflow-runs/:run_id/budget/reserve", reserveWorkflowBudget(d))
	group.POST("/workflow-runs/:run_id/budget/settle", settleWorkflowBudget(d))
	group.POST("/workflow-runs/:run_id/patch/validate", validateWorkflowPatch(d))
	group.POST("/workflow-runs/:run_id/patch/apply", applyWorkflowPatch(d))
	group.GET("/workflow-runs/:run_id/nodes/:node_key/log", getNodeLogs(d))
	group.POST("/workflow-runs/:run_id/start", startRun(d, "running"))
	group.POST("/workflow-runs/:run_id/pause", startRun(d, "paused"))
	group.POST("/workflow-runs/:run_id/resume", startRun(d, "running"))
	group.POST("/workflow-runs/:run_id/cancel", startRun(d, "cancelled"))

	group.GET("/workflow-templates", listWorkflowTemplates(d))
	group.POST("/workflow-templates", createWorkflowTemplate(d))
	group.GET("/workflow-templates/mine", listCreatorWorkflowTemplates(d))
	group.PUT("/workflow-templates/:template_id", updateWorkflowTemplate(d))
	group.GET("/workflow-templates/:template_id", getWorkflowTemplate(d))
	group.POST("/workflow-templates/:template_id/view", recordWorkflowTemplateView(d))
	group.POST("/workflow-templates/:template_id/rating", rateWorkflowTemplate(d))
	group.POST("/workflow-templates/:template_id/submit", submitWorkflowTemplate(d))
	group.POST("/workflow-templates/:template_id/withdraw", withdrawWorkflowTemplate(d))
	group.POST("/workflow-templates/:template_id/derive", deriveWorkflowTemplate(d))

	admin := group.Group("", auth.RequireAdmin())
	admin.POST("/admin/workflow-templates/:template_id/review", reviewWorkflowTemplate(d))
	admin.POST("/admin/workflow-templates/:template_id/suspend", suspendWorkflowTemplate(d))
	admin.POST("/admin/workflow-templates/:template_id/restore", restoreWorkflowTemplate(d))
	admin.GET("/admin/workflow-templates/reviews", listWorkflowTemplateReviews(d))

	group.GET("/projects/:project_id/workflow-runs", listRuns(d))
	group.GET("/projects/:project_id/workflows", listWorkflows(d))
	group.POST("/projects/:project_id/workflows/import", importWorkflow(d))
	group.POST("/projects/:project_id/workflows/migrate", migrateProject(d))
	group.GET("/projects/:project_id/event-log", replayProjectEvents(d))
}

// --- 请求体 ---

type workflowDefinitionCreateRequest struct {
	WorkspaceID *string `json:"workspace_id"`
	ProjectID   *string `json:"project_id"`
	Name        *string `json:"name"`
}

type workflowNodeInput struct {
	NodeKey             *string        `json:"node_key"`
	NodeType            *string        `json:"node_type"`
	NodeTypeVersion     *string        `json:"node_type_version"`
	ConfigSchemaVersion *string        `json:"config_schema_version"`
	Config              map[string]any `json:"config"`
	UiPosition          any            `json:"ui_position"`
	Weight              *float64       `json:"weight"`
	RetryPolicy         any            `json:"retry_policy"`
	ApprovalPolicy      any            `json:"approval_policy"`
}

type workflowEdgeInput struct {
	EdgeKey       *string `json:"edge_key"`
	SourceNodeKey *string `json:"source_node_key"`
	TargetNodeKey *string `json:"target_node_key"`
	Condition     any     `json:"condition"`
	OnFailure     *string `json:"on_failure"`
	Priority      *int    `json:"priority"`
}

type workflowRevisionCreateRequest struct {
	Nodes          []workflowNodeInput `json:"nodes"`
	Edges          []workflowEdgeInput `json:"edges"`
	TemplateLock   any                 `json:"template_lock"`
	ContentMode    *string             `json:"content_mode"`
	GenerationMode *string             `json:"generation_mode"`
	InputSchema    any                 `json:"input_schema"`
}

type workflowRunCreateRequest struct {
	WorkspaceID      *string        `json:"workspace_id"`
	ProjectID        *string        `json:"project_id"`
	ScriptRevisionID *string        `json:"script_revision_id"`
	Mode             *string        `json:"mode"`
	InputSnapshot    map[string]any `json:"input_snapshot"`
	EpisodeID        *string        `json:"episode_id"`
	BudgetLimit      *float64       `json:"budget_limit"`
}

type workflowRunTransitionRequest struct {
	ExpectedVersion *int64 `json:"expected_version"`
}

type workflowRetryNodeRequest struct {
	NodeKey *string `json:"node_key"`
	Start   *bool   `json:"start"`
}

type workflowTemplateUpgradeRequest struct {
	Confirmed *bool `json:"confirmed"`
}

type workflowTemplateDraftRequest struct {
	Name           *string             `json:"name"`
	Description    *string             `json:"description"`
	TemplateType   *string             `json:"template_type"`
	Contract       map[string]any      `json:"contract"`
	Nodes          []workflowNodeInput `json:"nodes"`
	Edges          []workflowEdgeInput `json:"edges"`
	ContentMode    *string             `json:"content_mode"`
	GenerationMode *string             `json:"generation_mode"`
	CoverRef       *string             `json:"cover_ref"`
}

type workflowTemplateRatingRequest struct {
	Rating *float64 `json:"rating"`
}

type workflowTemplateReviewRequest struct {
	Decision *string `json:"decision"`
	Comment  *string `json:"comment"`
}

type workflowTemplateDeriveRequest struct {
	WorkspaceID *string `json:"workspace_id"`
	ProjectID   *string `json:"project_id"`
	Name        *string `json:"name"`
}

type workflowPatchRequest struct {
	Patch     map[string]any `json:"patch"`
	Confirmed *bool          `json:"confirmed"`
	Start     *bool          `json:"start"`
}

type workflowBudgetReserveRequest struct {
	Amount *float64 `json:"amount"`
}

type workflowBudgetSettleRequest struct {
	ReservationID *string  `json:"reservation_id"`
	Amount        *float64 `json:"amount"`
}

type workflowWorkspaceScopeRequest struct {
	WorkspaceID *string `json:"workspace_id"`
}

type workflowImportRequest struct {
	WorkspaceID    *string             `json:"workspace_id"`
	Name           *string             `json:"name"`
	Nodes          []workflowNodeInput `json:"nodes"`
	Edges          []workflowEdgeInput `json:"edges"`
	TemplateLock   any                 `json:"template_lock"`
	ContentMode    *string             `json:"content_mode"`
	GenerationMode *string             `json:"generation_mode"`
	InputSchema    any                 `json:"input_schema"`
}

// --- 节点 / 边解码 ---

// decodeWorkflowNodes 把请求节点还原成 Python model_dump 的键集合。
//
// 键集合必须与 Python 的 WorkflowNodeInput 完全一致（含显式 null 的
// ui_position / retry_policy / approval_policy），否则 graph_hash 会与 Python
// 对同一张图算出不同的值。
func decodeWorkflowNodes(c *gin.Context, inputs []workflowNodeInput) ([]map[string]any, bool) {
	nodes := make([]map[string]any, 0, len(inputs))
	for index, input := range inputs {
		key, ok := requiredWorkflowString(c, input.NodeKey, "nodes.node_key", 128)
		if !ok {
			return nil, false
		}
		nodeType, ok := requiredWorkflowString(c, input.NodeType, "nodes.node_type", 128)
		if !ok {
			return nil, false
		}
		config := input.Config
		if config == nil {
			config = map[string]any{}
		}
		weight := 1.0
		if input.Weight != nil {
			if *input.Weight <= 0 {
				abortWorkflowField(c, "nodes.weight", "greater_than")
				return nil, false
			}
			weight = *input.Weight
		}
		nodes = append(nodes, map[string]any{
			"node_key":              key,
			"node_type":             nodeType,
			"node_type_version":     stringOrDefault(input.NodeTypeVersion, "1"),
			"config_schema_version": stringOrDefault(input.ConfigSchemaVersion, "1"),
			"config":                config,
			"ui_position":           input.UiPosition,
			"weight":                weight,
			"retry_policy":          input.RetryPolicy,
			"approval_policy":       input.ApprovalPolicy,
		})
		_ = index
	}
	return nodes, true
}

func decodeWorkflowEdges(c *gin.Context, inputs []workflowEdgeInput) ([]map[string]any, bool) {
	edges := make([]map[string]any, 0, len(inputs))
	for _, input := range inputs {
		key, ok := requiredWorkflowString(c, input.EdgeKey, "edges.edge_key", 128)
		if !ok {
			return nil, false
		}
		source, ok := requiredWorkflowString(c, input.SourceNodeKey, "edges.source_node_key", 128)
		if !ok {
			return nil, false
		}
		target, ok := requiredWorkflowString(c, input.TargetNodeKey, "edges.target_node_key", 128)
		if !ok {
			return nil, false
		}
		onFailure := "stop"
		if input.OnFailure != nil {
			if !workflowOnFailures[*input.OnFailure] {
				abortWorkflowField(c, "edges.on_failure", "string_pattern_mismatch")
				return nil, false
			}
			onFailure = *input.OnFailure
		}
		priority := 0
		if input.Priority != nil {
			priority = *input.Priority
		}
		edges = append(edges, map[string]any{
			"edge_key":        key,
			"source_node_key": source,
			"target_node_key": target,
			"condition":       input.Condition,
			"on_failure":      onFailure,
			"priority":        priority,
		})
	}
	return edges, true
}

var workflowOnFailures = map[string]bool{"stop": true, "skip": true, "fallback": true}

func stringOrDefault(value *string, fallback string) string {
	if value == nil || *value == "" {
		return fallback
	}
	return *value
}

// requiredWorkflowString 校验必填字符串（非空且不超过 maxLen）。
func requiredWorkflowString(c *gin.Context, value *string, field string, maxLen int) (string, bool) {
	if value == nil || strings.TrimSpace(*value) == "" {
		abortMissingField(c, field)
		return "", false
	}
	if len(*value) > maxLen {
		abortWorkflowField(c, field, "string_too_long")
		return "", false
	}
	return *value, true
}

func abortWorkflowField(c *gin.Context, field, typ string) {
	apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
		{Loc: []string{"body", field}, Msg: "invalid value", Type: typ},
	}})
}

// workflowString 校验可选字符串的上下界（minLen=1 表示非空）。
func workflowString(c *gin.Context, raw string, field string, minLen, maxLen int) (string, bool) {
	if len(raw) < minLen {
		abortWorkflowField(c, field, "string_too_short")
		return "", false
	}
	if len(raw) > maxLen {
		abortWorkflowField(c, field, "string_too_long")
		return "", false
	}
	return raw, true
}

// workflowQueryInt 解析整型查询参数并校验区间。
func workflowQueryInt(c *gin.Context, name string, def, min, max int) (int, bool) {
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
	if value < min || value > max {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"query", name}, Msg: "invalid value", Type: "greater_than_equal"},
		}})
		return 0, false
	}
	return value, true
}

// --- 错误映射 ---

// abortWorkflowErr 把工作流领域错误映射成统一 {"detail": ...} 信封。
func abortWorkflowErr(c *gin.Context, err error) {
	var (
		notFound   *service.WorkflowNotFoundError
		conflict   *service.WorkflowConflictError
		validation *domain.WorkflowValidationError
	)
	switch {
	case errors.As(err, &notFound):
		apperr.Abort(c, apperr.New(http.StatusNotFound, notFound.Code, notFound.Params))
	case errors.As(err, &conflict):
		apperr.Abort(c, apperr.New(http.StatusConflict, conflict.Code, conflict.Params))
	case errors.As(err, &validation):
		apperr.Abort(c, apperr.New(http.StatusBadRequest, validation.Code, validation.Params))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}

// --- 定义 / 版本 ---

func createWorkflow(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req workflowDefinitionCreateRequest
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
		name, ok := requiredWorkflowString(c, req.Name, "name", 200)
		if !ok {
			return
		}
		result, err := d.Workflows.CreateDefinition(c.Request.Context(), service.WorkflowDefinitionParams{
			WorkspaceID: workspaceID, ProjectID: projectID, Name: name, ActorID: currentUserID(c),
		})
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func getWorkflow(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Workflows.GetWorkflow(c.Request.Context(), c.Param("definition_id"), currentUserID(c))
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func getTemplateUpgrade(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Workflows.GetTemplateUpgrade(c.Request.Context(), c.Param("definition_id"), currentUserID(c))
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func applyTemplateUpgrade(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req workflowTemplateUpgradeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		confirmed := req.Confirmed != nil && *req.Confirmed
		result, err := d.Workflows.UpgradeWorkflowTemplate(
			c.Request.Context(), c.Param("definition_id"), currentUserID(c), confirmed,
		)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func createRevision(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req workflowRevisionCreateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		if len(req.Nodes) == 0 {
			abortWorkflowField(c, "nodes", "too_short")
			return
		}
		if len(req.Nodes) > 1000 || len(req.Edges) > 5000 {
			abortWorkflowField(c, "nodes", "too_long")
			return
		}
		nodes, ok := decodeWorkflowNodes(c, req.Nodes)
		if !ok {
			return
		}
		edges, ok := decodeWorkflowEdges(c, req.Edges)
		if !ok {
			return
		}
		contentMode := "drama"
		if req.ContentMode != nil {
			if !domain.WorkflowContentModes[*req.ContentMode] {
				abortWorkflowField(c, "content_mode", "string_pattern_mismatch")
				return
			}
			contentMode = *req.ContentMode
		}
		generationMode := "storyboard"
		if req.GenerationMode != nil {
			if !domain.WorkflowGenerationModes[*req.GenerationMode] {
				abortWorkflowField(c, "generation_mode", "string_pattern_mismatch")
				return
			}
			generationMode = *req.GenerationMode
		}
		inputSchema := any(map[string]any{})
		if req.InputSchema != nil {
			inputSchema = req.InputSchema
		}
		result, err := d.Workflows.CreateRevision(c.Request.Context(), service.WorkflowRevisionParams{
			DefinitionID:   c.Param("definition_id"),
			Nodes:          nodes,
			Edges:          edges,
			TemplateLock:   req.TemplateLock,
			ActorID:        currentUserID(c),
			ContentMode:    contentMode,
			GenerationMode: generationMode,
			InputSchema:    inputSchema,
		})
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func validateRevision(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Workflows.ValidateRevision(c.Request.Context(), c.Param("revision_id"), currentUserID(c))
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func publishRevision(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Workflows.PublishRevision(c.Request.Context(), c.Param("revision_id"), currentUserID(c))
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func listWorkflowRevisions(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Workflows.ListRevisions(c.Request.Context(), c.Param("definition_id"), currentUserID(c))
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func revertWorkflowRevision(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Workflows.RevertRevision(
			c.Request.Context(), c.Param("definition_id"), c.Param("revision_id"), currentUserID(c),
		)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

// --- 运行 ---

func planRun(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req workflowRunCreateRequest
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
		mode := "hybrid"
		if req.Mode != nil {
			if !workflowRunModes[*req.Mode] {
				abortWorkflowField(c, "mode", "string_pattern_mismatch")
				return
			}
			mode = *req.Mode
		}
		if req.EpisodeID != nil && len(*req.EpisodeID) > 128 {
			abortWorkflowField(c, "episode_id", "string_too_long")
			return
		}
		if req.BudgetLimit != nil && *req.BudgetLimit < 0 {
			abortWorkflowField(c, "budget_limit", "greater_than_equal")
			return
		}
		inputSnapshot := req.InputSnapshot
		if inputSnapshot == nil {
			inputSnapshot = map[string]any{}
		}
		result, err := d.Workflows.PlanRun(c.Request.Context(), service.WorkflowPlanRunParams{
			RevisionID:       c.Param("revision_id"),
			WorkspaceID:      workspaceID,
			ProjectID:        projectID,
			Mode:             mode,
			InputSnapshot:    inputSnapshot,
			ScriptRevisionID: req.ScriptRevisionID,
			ActorID:          currentUserID(c),
			EpisodeID:        req.EpisodeID,
			BudgetLimit:      req.BudgetLimit,
		})
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

var workflowRunModes = map[string]bool{"auto": true, "manual": true, "hybrid": true}

func getRun(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Workflows.GetRun(c.Request.Context(), c.Param("run_id"), currentUserID(c))
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func retryWorkflowRun(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req workflowRetryNodeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		nodeKey, ok := requiredWorkflowString(c, req.NodeKey, "node_key", 128)
		if !ok {
			return
		}
		start := true
		if req.Start != nil {
			start = *req.Start
		}
		result, err := d.Workflows.RetryRunFromNode(
			c.Request.Context(), c.Param("run_id"), nodeKey, currentUserID(c), start,
		)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

// startRun 是 start / pause / resume / cancel 四个跃迁端点的共用实现。
func startRun(d WorkflowDeps, target string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req workflowRunTransitionRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		if req.ExpectedVersion == nil {
			abortMissingField(c, "expected_version")
			return
		}
		if *req.ExpectedVersion < 1 {
			abortWorkflowField(c, "expected_version", "greater_than_equal")
			return
		}
		result, err := d.Workflows.TransitionWorkflowRun(
			c.Request.Context(), c.Param("run_id"), target, *req.ExpectedVersion, currentUserID(c),
		)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func getNodeLogs(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit, ok := workflowQueryInt(c, "limit", 500, 1, 2000)
		if !ok {
			return
		}
		result, err := d.Workflows.ListNodeLogs(
			c.Request.Context(), c.Param("run_id"), c.Param("node_key"), currentUserID(c), limit,
		)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

// --- 模板市场 ---

func listWorkflowTemplates(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawType := strings.TrimSpace(c.Query("template_type"))
		if rawType != "" && !workflowTemplateTypes[rawType] {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"query", "template_type"}, Msg: "invalid value", Type: "string_pattern_mismatch"},
			}})
			return
		}
		user := auth.Current(c)
		marketplace := map[string]any{"items": []any{}}
		if config.MarketplacePublicEnabled() || (user != nil && user.IsAdmin()) {
			var err error
			marketplace, err = d.Workflows.ListMarketplace(c.Request.Context(), rawType, 50)
			if err != nil {
				abortWorkflowErr(c, err)
				return
			}
		}
		builtin := service.ListBuiltinTemplates()
		builtinItems, _ := builtin["items"].([]any)
		marketItems, _ := marketplace["items"].([]any)
		items := make([]any, 0, len(builtinItems)+len(marketItems))
		items = append(items, builtinItems...)
		items = append(items, marketItems...)
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

var workflowTemplateTypes = map[string]bool{"manga": true, "short_drama": true}

func createWorkflowTemplate(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		user := auth.Current(c)
		if (user == nil || !user.IsAdmin()) && !config.TemplateUploadEnabled() {
			apperr.Abort(c, apperr.NewLiteral(http.StatusForbidden, "workflow_template_upload_disabled"))
			return
		}
		params, ok := bindWorkflowTemplateDraft(c)
		if !ok {
			return
		}
		params.ActorID = currentUserID(c)
		result, err := d.Workflows.CreateTemplateDraft(c.Request.Context(), params)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func updateWorkflowTemplate(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		params, ok := bindWorkflowTemplateDraft(c)
		if !ok {
			return
		}
		params.ActorID = currentUserID(c)
		result, err := d.Workflows.UpdateTemplateDraft(c.Request.Context(), c.Param("template_id"), params)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func bindWorkflowTemplateDraft(c *gin.Context) (service.WorkflowTemplateParams, bool) {
	var req workflowTemplateDraftRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortBodyBind(c, err)
		return service.WorkflowTemplateParams{}, false
	}
	name, ok := requiredWorkflowString(c, req.Name, "name", 200)
	if !ok {
		return service.WorkflowTemplateParams{}, false
	}
	description := ""
	if req.Description != nil {
		description, ok = workflowString(c, *req.Description, "description", 0, 5000)
		if !ok {
			return service.WorkflowTemplateParams{}, false
		}
	}
	templateType, ok := requiredWorkflowString(c, req.TemplateType, "template_type", 32)
	if !ok {
		return service.WorkflowTemplateParams{}, false
	}
	if !workflowTemplateTypes[templateType] {
		abortWorkflowField(c, "template_type", "string_pattern_mismatch")
		return service.WorkflowTemplateParams{}, false
	}
	if len(req.Nodes) == 0 {
		abortWorkflowField(c, "nodes", "too_short")
		return service.WorkflowTemplateParams{}, false
	}
	if len(req.Nodes) > 1000 || len(req.Edges) > 5000 {
		abortWorkflowField(c, "nodes", "too_long")
		return service.WorkflowTemplateParams{}, false
	}
	nodes, ok := decodeWorkflowNodes(c, req.Nodes)
	if !ok {
		return service.WorkflowTemplateParams{}, false
	}
	edges, ok := decodeWorkflowEdges(c, req.Edges)
	if !ok {
		return service.WorkflowTemplateParams{}, false
	}
	contentMode := "drama"
	if req.ContentMode != nil {
		if !domain.WorkflowContentModes[*req.ContentMode] {
			abortWorkflowField(c, "content_mode", "string_pattern_mismatch")
			return service.WorkflowTemplateParams{}, false
		}
		contentMode = *req.ContentMode
	}
	generationMode := "storyboard"
	if req.GenerationMode != nil {
		if !domain.WorkflowGenerationModes[*req.GenerationMode] {
			abortWorkflowField(c, "generation_mode", "string_pattern_mismatch")
			return service.WorkflowTemplateParams{}, false
		}
		generationMode = *req.GenerationMode
	}
	if req.CoverRef != nil {
		if _, ok := workflowString(c, *req.CoverRef, "cover_ref", 0, 500); !ok {
			return service.WorkflowTemplateParams{}, false
		}
	}
	contract := req.Contract
	if contract == nil {
		contract = map[string]any{}
	}
	return service.WorkflowTemplateParams{
		Name:           name,
		Description:    description,
		TemplateType:   templateType,
		Contract:       contract,
		Nodes:          nodes,
		Edges:          edges,
		ContentMode:    contentMode,
		GenerationMode: generationMode,
		CoverRef:       req.CoverRef,
	}, true
}

func listCreatorWorkflowTemplates(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Workflows.ListCreatorTemplates(c.Request.Context(), currentUserID(c))
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func getWorkflowTemplate(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		user := auth.Current(c)
		public := config.MarketplacePublicEnabled() || (user != nil && user.IsAdmin())
		result, err := d.Workflows.GetTemplate(c.Request.Context(), c.Param("template_id"), currentUserID(c), public)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func recordWorkflowTemplateView(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Workflows.RecordTemplateView(c.Request.Context(), c.Param("template_id"))
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func rateWorkflowTemplate(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req workflowTemplateRatingRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		if req.Rating == nil {
			abortMissingField(c, "rating")
			return
		}
		if *req.Rating < 1 || *req.Rating > 5 {
			abortWorkflowField(c, "rating", "greater_than_equal")
			return
		}
		result, err := d.Workflows.RateTemplate(
			c.Request.Context(), c.Param("template_id"), currentUserID(c), *req.Rating,
		)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func submitWorkflowTemplate(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Workflows.SubmitTemplate(c.Request.Context(), c.Param("template_id"), currentUserID(c))
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func withdrawWorkflowTemplate(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Workflows.WithdrawTemplate(c.Request.Context(), c.Param("template_id"), currentUserID(c))
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func deriveWorkflowTemplate(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req workflowTemplateDeriveRequest
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
		name, ok := requiredWorkflowString(c, req.Name, "name", 200)
		if !ok {
			return
		}
		result, err := d.Workflows.DeriveTemplate(c.Request.Context(), service.DeriveTemplateParams{
			TemplateID:  c.Param("template_id"),
			WorkspaceID: workspaceID,
			ProjectID:   projectID,
			Name:        name,
			ActorID:     currentUserID(c),
		})
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func reviewWorkflowTemplate(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req workflowTemplateReviewRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		decision, ok := requiredWorkflowString(c, req.Decision, "decision", 32)
		if !ok {
			return
		}
		if !workflowTemplateDecisions[decision] {
			abortWorkflowField(c, "decision", "string_pattern_mismatch")
			return
		}
		comment, ok := requiredWorkflowString(c, req.Comment, "comment", 5000)
		if !ok {
			return
		}
		result, err := d.Workflows.ReviewTemplate(c.Request.Context(), service.ReviewTemplateParams{
			TemplateID: c.Param("template_id"),
			ReviewerID: currentUserID(c),
			Decision:   decision,
			Comment:    comment,
		})
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

var workflowTemplateDecisions = map[string]bool{
	"start": true, "approve": true, "reject": true, "changes_requested": true,
}

func suspendWorkflowTemplate(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Workflows.SetTemplateSuspended(
			c.Request.Context(), c.Param("template_id"), currentUserID(c), true,
		)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func restoreWorkflowTemplate(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Workflows.SetTemplateSuspended(
			c.Request.Context(), c.Param("template_id"), currentUserID(c), false,
		)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func listWorkflowTemplateReviews(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawType := strings.TrimSpace(c.Query("template_type"))
		if rawType != "" && !workflowTemplateTypes[rawType] {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"query", "template_type"}, Msg: "invalid value", Type: "string_pattern_mismatch"},
			}})
			return
		}
		riskTag := strings.TrimSpace(c.Query("risk_tag"))
		if len(riskTag) > 64 {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"query", "risk_tag"}, Msg: "invalid value", Type: "string_too_long"},
			}})
			return
		}
		limit, ok := workflowQueryInt(c, "limit", 100, 1, 200)
		if !ok {
			return
		}
		result, err := d.Workflows.ListPendingTemplateReviews(c.Request.Context(), rawType, riskTag, limit)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

// --- 预算 / patch ---

func reserveWorkflowBudget(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req workflowBudgetReserveRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		if req.Amount == nil {
			abortMissingField(c, "amount")
			return
		}
		if *req.Amount <= 0 {
			abortWorkflowField(c, "amount", "greater_than")
			return
		}
		result, err := d.Workflows.ReserveRunBudget(
			c.Request.Context(), c.Param("run_id"), currentUserID(c), *req.Amount,
		)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func settleWorkflowBudget(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req workflowBudgetSettleRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		reservationID, ok := requiredWorkflowString(c, req.ReservationID, "reservation_id", 36)
		if !ok {
			return
		}
		if req.Amount == nil {
			abortMissingField(c, "amount")
			return
		}
		if *req.Amount < 0 {
			abortWorkflowField(c, "amount", "greater_than_equal")
			return
		}
		// 对应 Python 侧：先确认 reservation 存在且属于该 run，再进服务层。
		reservation, err := d.Workflows.ReservationByID(c.Request.Context(), reservationID)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		if reservation.WorkflowRunID != c.Param("run_id") {
			apperr.Abort(c, apperr.New(http.StatusNotFound, "workflow_budget_reservation_not_found", nil))
			return
		}
		result, err := d.Workflows.SettleRunBudget(
			c.Request.Context(), reservationID, currentUserID(c), *req.Amount,
		)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func validateWorkflowPatch(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		req, ok := bindWorkflowPatchRequest(c)
		if !ok {
			return
		}
		patch, ok := decodeWorkflowPatch(c, req.Patch)
		if !ok {
			return
		}
		if !config.AutoOptimizationEnabled() {
			// 与 Python 侧一致：自动优化未开启时，逐项标记需要确认。
			for i := range patch.Operations {
				patch.Operations[i].RequiresConfirmation = true
			}
		}
		result, err := d.Workflows.ValidatePatchForRun(
			c.Request.Context(), c.Param("run_id"), patch, currentUserID(c),
		)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func applyWorkflowPatch(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		req, ok := bindWorkflowPatchRequest(c)
		if !ok {
			return
		}
		confirmed := req.Confirmed != nil && *req.Confirmed
		if !config.AutoOptimizationEnabled() && !confirmed {
			apperr.Abort(c, apperr.NewLiteral(http.StatusConflict, "workflow_patch_confirmation_required"))
			return
		}
		patch, ok := decodeWorkflowPatch(c, req.Patch)
		if !ok {
			return
		}
		start := req.Start != nil && *req.Start
		result, err := d.Workflows.ApplyPatchForRun(
			c.Request.Context(), c.Param("run_id"), patch, currentUserID(c), confirmed, start,
		)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func bindWorkflowPatchRequest(c *gin.Context) (workflowPatchRequest, bool) {
	var req workflowPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortBodyBind(c, err)
		return workflowPatchRequest{}, false
	}
	if req.Patch == nil {
		abortMissingField(c, "patch")
		return workflowPatchRequest{}, false
	}
	return req, true
}

// decodeWorkflowPatch 把 patch 对象解码成领域模型。
//
// Python 侧用 Pydantic 校验（违例会冒泡成 500），这里按请求体约束写成 422，
// 客户端拿到的是可修正的错误而不是服务端异常。
func decodeWorkflowPatch(c *gin.Context, raw map[string]any) (domain.WorkflowPatch, bool) {
	patch := domain.WorkflowPatch{Scope: "episode"}
	if value, ok := raw["base_revision_id"].(string); ok {
		patch.BaseRevisionID = value
	}
	if value, ok := raw["scope"].(string); ok {
		patch.Scope = value
	}
	if value, ok := raw["rerun"].(bool); ok {
		patch.Rerun = value
	}
	if value, ok := raw["reason"].(string); ok {
		patch.Reason = value
	}
	operations, ok := raw["operations"].([]any)
	if !ok {
		abortWorkflowField(c, "patch.operations", "list_type")
		return domain.WorkflowPatch{}, false
	}
	for _, item := range operations {
		entry, isMap := item.(map[string]any)
		if !isMap {
			abortWorkflowField(c, "patch.operations", "model_attributes_type")
			return domain.WorkflowPatch{}, false
		}
		operation := domain.WorkflowPatchOperation{}
		if value, isString := entry["operation"].(string); isString {
			operation.Operation = value
		}
		if value, isString := entry["target_node"].(string); isString {
			operation.TargetNode = &value
		}
		if value, isString := entry["path"].(string); isString {
			operation.Path = &value
		}
		operation.Before = entry["before"]
		operation.After = entry["after"]
		if value, isNumber := entry["estimated_cost_delta"].(float64); isNumber {
			operation.EstimatedCostDelta = value
		}
		if value, isBool := entry["requires_confirmation"].(bool); isBool {
			operation.RequiresConfirmation = value
		}
		patch.Operations = append(patch.Operations, operation)
	}
	if err := patch.Validate(); err != nil {
		abortWorkflowErr(c, err)
		return domain.WorkflowPatch{}, false
	}
	return patch, true
}

// --- 项目作用域 ---

func listRuns(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit, ok := workflowQueryInt(c, "limit", 50, 1, 200)
		if !ok {
			return
		}
		result, err := d.Workflows.ListRuns(c.Request.Context(), c.Param("project_id"), currentUserID(c), limit)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func listWorkflows(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Workflows.ListDefinitions(c.Request.Context(), c.Param("project_id"), currentUserID(c))
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func exportWorkflow(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Workflows.ExportDefinition(c.Request.Context(), c.Param("definition_id"), currentUserID(c))
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func importWorkflow(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req workflowImportRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		workspaceID, ok := requiredWorkflowString(c, req.WorkspaceID, "workspace_id", 128)
		if !ok {
			return
		}
		name, ok := requiredWorkflowString(c, req.Name, "name", 200)
		if !ok {
			return
		}
		if len(req.Nodes) == 0 {
			abortWorkflowField(c, "nodes", "too_short")
			return
		}
		if len(req.Nodes) > 1000 || len(req.Edges) > 5000 {
			abortWorkflowField(c, "nodes", "too_long")
			return
		}
		nodes, ok := decodeWorkflowNodes(c, req.Nodes)
		if !ok {
			return
		}
		edges, ok := decodeWorkflowEdges(c, req.Edges)
		if !ok {
			return
		}
		contentMode := "drama"
		if req.ContentMode != nil {
			if !domain.WorkflowContentModes[*req.ContentMode] {
				abortWorkflowField(c, "content_mode", "string_pattern_mismatch")
				return
			}
			contentMode = *req.ContentMode
		}
		generationMode := "storyboard"
		if req.GenerationMode != nil {
			if !domain.WorkflowGenerationModes[*req.GenerationMode] {
				abortWorkflowField(c, "generation_mode", "string_pattern_mismatch")
				return
			}
			generationMode = *req.GenerationMode
		}
		inputSchema := any(map[string]any{})
		if req.InputSchema != nil {
			inputSchema = req.InputSchema
		}
		result, err := d.Workflows.ImportDefinition(c.Request.Context(), service.WorkflowImportParams{
			WorkspaceID:    workspaceID,
			ProjectID:      c.Param("project_id"),
			Name:           name,
			Nodes:          nodes,
			Edges:          edges,
			TemplateLock:   req.TemplateLock,
			ActorID:        currentUserID(c),
			ContentMode:    contentMode,
			GenerationMode: generationMode,
			InputSchema:    inputSchema,
		})
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func migrateProject(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req workflowWorkspaceScopeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortBodyBind(c, err)
			return
		}
		workspaceID, ok := requiredWorkflowString(c, req.WorkspaceID, "workspace_id", 128)
		if !ok {
			return
		}
		result, err := d.Workflows.MigrateProject(
			c.Request.Context(), workspaceID, c.Param("project_id"), currentUserID(c),
		)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func replayProjectEvents(d WorkflowDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		after, ok := workflowQueryInt(c, "after", 0, 0, 1<<31-1)
		if !ok {
			return
		}
		limit, ok := workflowQueryInt(c, "limit", 500, 1, 1000)
		if !ok {
			return
		}
		result, err := d.Workflows.ListProjectEvents(
			c.Request.Context(), c.Param("project_id"), currentUserID(c), int64(after), limit,
		)
		if err != nil {
			abortWorkflowErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}
