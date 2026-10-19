package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/migrations"
)

// TestRegisterWorkflowRoutes 逐条核对 41 个端点（对应 server/routers/workflows.py）。
func TestRegisterWorkflowRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterWorkflows(r.Group("/api/v1"), WorkflowDeps{})

	registered := map[string]bool{}
	for _, route := range r.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	want := []string{
		http.MethodPost + " /api/v1/workflows",
		http.MethodGet + " /api/v1/workflows/:definition_id",
		http.MethodGet + " /api/v1/workflows/:definition_id/template-upgrade",
		http.MethodPost + " /api/v1/workflows/:definition_id/template-upgrade",
		http.MethodPost + " /api/v1/workflows/:definition_id/revisions",
		http.MethodGet + " /api/v1/workflows/:definition_id/revisions",
		http.MethodPost + " /api/v1/workflows/:definition_id/revisions/:revision_id/revert",
		http.MethodGet + " /api/v1/workflows/:definition_id/export",
		http.MethodPost + " /api/v1/workflow-revisions/:revision_id/validate",
		http.MethodPost + " /api/v1/workflow-revisions/:revision_id/publish",
		http.MethodPost + " /api/v1/workflow-revisions/:revision_id/runs",
		http.MethodGet + " /api/v1/workflow-runs/:run_id",
		http.MethodPost + " /api/v1/workflow-runs/:run_id/retry",
		http.MethodPost + " /api/v1/workflow-runs/:run_id/budget/reserve",
		http.MethodPost + " /api/v1/workflow-runs/:run_id/budget/settle",
		http.MethodPost + " /api/v1/workflow-runs/:run_id/patch/validate",
		http.MethodPost + " /api/v1/workflow-runs/:run_id/patch/apply",
		http.MethodGet + " /api/v1/workflow-runs/:run_id/nodes/:node_key/log",
		http.MethodPost + " /api/v1/workflow-runs/:run_id/start",
		http.MethodPost + " /api/v1/workflow-runs/:run_id/pause",
		http.MethodPost + " /api/v1/workflow-runs/:run_id/resume",
		http.MethodPost + " /api/v1/workflow-runs/:run_id/cancel",
		http.MethodGet + " /api/v1/workflow-templates",
		http.MethodPost + " /api/v1/workflow-templates",
		http.MethodGet + " /api/v1/workflow-templates/mine",
		http.MethodPut + " /api/v1/workflow-templates/:template_id",
		http.MethodGet + " /api/v1/workflow-templates/:template_id",
		http.MethodPost + " /api/v1/workflow-templates/:template_id/view",
		http.MethodPost + " /api/v1/workflow-templates/:template_id/rating",
		http.MethodPost + " /api/v1/workflow-templates/:template_id/submit",
		http.MethodPost + " /api/v1/workflow-templates/:template_id/withdraw",
		http.MethodPost + " /api/v1/workflow-templates/:template_id/derive",
		http.MethodPost + " /api/v1/admin/workflow-templates/:template_id/review",
		http.MethodPost + " /api/v1/admin/workflow-templates/:template_id/suspend",
		http.MethodPost + " /api/v1/admin/workflow-templates/:template_id/restore",
		http.MethodGet + " /api/v1/admin/workflow-templates/reviews",
		http.MethodGet + " /api/v1/projects/:project_id/workflow-runs",
		http.MethodGet + " /api/v1/projects/:project_id/workflows",
		http.MethodPost + " /api/v1/projects/:project_id/workflows/import",
		http.MethodPost + " /api/v1/projects/:project_id/workflows/migrate",
		http.MethodGet + " /api/v1/projects/:project_id/event-log",
	}
	if len(want) != 41 {
		t.Fatalf("endpoint list must stay at 41, got %d", len(want))
	}
	for _, route := range want {
		if !registered[route] {
			t.Errorf("route not registered: %s", route)
		}
	}
	if len(registered) != len(want) {
		t.Errorf("registered %d routes, want %d", len(registered), len(want))
	}
}

// --- 端到端冒烟 ---

func newWorkflowTestEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "workflow.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Migrate(gdb, dialect, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// workflow_definitions.user_id 有指向 users(id) 的外键，单用户模式下由
	// EnsureDefaultUser 播种；这里显式播种请求用户与 system（官方 Skill 铸造用）。
	seedWorkflowUsers(t, gdb)
	pool, err := gdb.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	r := gin.New()
	group := r.Group("/api/v1")
	// 注入一个固定用户，绕开 JWT。
	group.Use(func(c *gin.Context) {
		c.Set("shotwise.user", &auth.User{ID: "u1", Sub: "u1", Role: "admin"})
		c.Next()
	})
	RegisterWorkflows(group, WorkflowDeps{Workflows: service.NewWorkflowService(gdb)})
	return r
}

func workflowPost(t *testing.T, engine *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func workflowGet(t *testing.T, engine *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func workflowDecodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body (%d): %s", rec.Code, rec.Body.String())
	}
	return out
}

// TestWorkflowDefinitionRevisionRunLifecycle 走通 定义 → 版本 → 发布 → 运行 → 跃迁。
func TestWorkflowDefinitionRevisionRunLifecycle(t *testing.T) {
	engine := newWorkflowTestEngine(t)

	rec := workflowPost(t, engine, "/api/v1/workflows", map[string]any{
		"workspace_id": "ws", "project_id": "p1", "name": "main",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create definition: %d %s", rec.Code, rec.Body.String())
	}
	definitionID := workflowDecodeBody(t, rec)["id"].(string)

	nodes := []any{
		map[string]any{"node_key": "script_generate", "node_type": "script_generate", "weight": 2.0},
		map[string]any{"node_key": "export", "node_type": "export"},
	}
	edges := []any{map[string]any{
		"edge_key":        "script_generate-export",
		"source_node_key": "script_generate",
		"target_node_key": "export",
	}}
	rec = workflowPost(t, engine, "/api/v1/workflows/"+definitionID+"/revisions", map[string]any{
		"nodes": nodes, "edges": edges, "content_mode": "drama", "generation_mode": "storyboard",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create revision: %d %s", rec.Code, rec.Body.String())
	}
	revisionID := workflowDecodeBody(t, rec)["id"].(string)

	rec = workflowPost(t, engine, "/api/v1/workflow-revisions/"+revisionID+"/publish", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("publish revision: %d %s", rec.Code, rec.Body.String())
	}

	rec = workflowPost(t, engine, "/api/v1/workflow-revisions/"+revisionID+"/runs", map[string]any{
		"workspace_id": "ws", "project_id": "p1", "mode": "hybrid",
		"input_snapshot": map[string]any{"k": "v"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("plan run: %d %s", rec.Code, rec.Body.String())
	}
	run := workflowDecodeBody(t, rec)
	runID := run["id"].(string)
	if run["deduped"] != false {
		t.Fatalf("first run must not be deduped: %v", run)
	}

	// 相同输入再次规划：按指纹去重，不新建运行。
	rec = workflowPost(t, engine, "/api/v1/workflow-revisions/"+revisionID+"/runs", map[string]any{
		"workspace_id": "ws", "project_id": "p1", "mode": "hybrid",
		"input_snapshot": map[string]any{"k": "v"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("plan duplicate run: %d %s", rec.Code, rec.Body.String())
	}
	if workflowDecodeBody(t, rec)["deduped"] != true {
		t.Fatalf("duplicate run must be deduped: %s", rec.Body.String())
	}

	// planned → running：乐观并发按 expected_version。
	rec = workflowPost(t, engine, "/api/v1/workflow-runs/"+runID+"/start", map[string]any{"expected_version": 1})
	if rec.Code != http.StatusOK {
		t.Fatalf("start run: %d %s", rec.Code, rec.Body.String())
	}
	if workflowDecodeBody(t, rec)["status"] != "running" {
		t.Fatalf("run not running: %s", rec.Body.String())
	}

	// 版本不符 → 409 workflow_version_conflict。
	rec = workflowPost(t, engine, "/api/v1/workflow-runs/"+runID+"/cancel", map[string]any{"expected_version": 1})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale version must conflict: %d %s", rec.Code, rec.Body.String())
	}
	detail := workflowDecodeBody(t, rec)["detail"].(map[string]any)
	if detail["code"] != "workflow_version_conflict" {
		t.Fatalf("unexpected conflict code: %v", detail)
	}

	rec = workflowPost(t, engine, "/api/v1/workflow-runs/"+runID+"/cancel", map[string]any{"expected_version": 2})
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel run: %d %s", rec.Code, rec.Body.String())
	}

	// 已发布版本不可重复发布 → 409 workflow_revision_immutable。
	rec = workflowPost(t, engine, "/api/v1/workflow-revisions/"+revisionID+"/publish", map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("republish must conflict: %d %s", rec.Code, rec.Body.String())
	}
}

// TestWorkflowNodeTypeAndModeValidation 校验节点类型与模式围栏。
func TestWorkflowNodeTypeAndModeValidation(t *testing.T) {
	engine := newWorkflowTestEngine(t)
	rec := workflowPost(t, engine, "/api/v1/workflows", map[string]any{
		"workspace_id": "ws", "project_id": "p1", "name": "main",
	})
	definitionID := workflowDecodeBody(t, rec)["id"].(string)

	rec = workflowPost(t, engine, "/api/v1/workflows/"+definitionID+"/revisions", map[string]any{
		"nodes": []any{map[string]any{"node_key": "a", "node_type": "custom_node"}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown node type must be rejected: %d %s", rec.Code, rec.Body.String())
	}
	if workflowDecodeBody(t, rec)["detail"].(map[string]any)["code"] != "workflow_node_not_allowed" {
		t.Fatalf("unexpected code: %s", rec.Body.String())
	}

	rec = workflowPost(t, engine, "/api/v1/workflows/"+definitionID+"/revisions", map[string]any{
		"nodes":           []any{map[string]any{"node_key": "a", "node_type": "export"}},
		"generation_mode": "telepathy",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad generation_mode must be 422: %d %s", rec.Code, rec.Body.String())
	}
}

// TestWorkflowRunNotFoundEnvelope 未知运行返回 404 + 机读码。
func TestWorkflowRunNotFoundEnvelope(t *testing.T) {
	engine := newWorkflowTestEngine(t)
	rec := workflowGet(t, engine, "/api/v1/workflow-runs/does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d %s", rec.Code, rec.Body.String())
	}
	if workflowDecodeBody(t, rec)["detail"].(map[string]any)["code"] != "workflow_run_not_found" {
		t.Fatalf("unexpected detail: %s", rec.Body.String())
	}
}

// TestWorkflowBudgetReserveAndSettle 预算预留 → 结算 → 剩余额度。
func TestWorkflowBudgetReserveAndSettle(t *testing.T) {
	engine := newWorkflowTestEngine(t)
	rec := workflowPost(t, engine, "/api/v1/workflows", map[string]any{
		"workspace_id": "ws", "project_id": "p1", "name": "main",
	})
	definitionID := workflowDecodeBody(t, rec)["id"].(string)
	rec = workflowPost(t, engine, "/api/v1/workflows/"+definitionID+"/revisions", map[string]any{
		"nodes": []any{map[string]any{"node_key": "export", "node_type": "export"}},
	})
	revisionID := workflowDecodeBody(t, rec)["id"].(string)
	rec = workflowPost(t, engine, "/api/v1/workflow-revisions/"+revisionID+"/publish", map[string]any{})
	rec = workflowPost(t, engine, "/api/v1/workflow-revisions/"+revisionID+"/runs", map[string]any{
		"workspace_id": "ws", "project_id": "p1", "budget_limit": 10,
	})
	runID := workflowDecodeBody(t, rec)["id"].(string)

	rec = workflowPost(t, engine, "/api/v1/workflow-runs/"+runID+"/budget/reserve", map[string]any{"amount": 4})
	if rec.Code != http.StatusOK {
		t.Fatalf("reserve: %d %s", rec.Code, rec.Body.String())
	}
	reservationID := workflowDecodeBody(t, rec)["reservation_id"].(string)
	if workflowDecodeBody(t, rec)["remaining_amount"].(float64) != 6 {
		t.Fatalf("unexpected remaining: %s", rec.Body.String())
	}

	rec = workflowPost(t, engine, "/api/v1/workflow-runs/"+runID+"/budget/settle", map[string]any{
		"reservation_id": reservationID, "amount": 3,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("settle: %d %s", rec.Code, rec.Body.String())
	}
	body := workflowDecodeBody(t, rec)
	if body["spent_amount"].(float64) != 3 || body["reserved_amount"].(float64) != 1 {
		t.Fatalf("unexpected settlement: %s", rec.Body.String())
	}

	// 超预留额度结算 → 400。
	rec = workflowPost(t, engine, "/api/v1/workflow-runs/"+runID+"/budget/settle", map[string]any{
		"reservation_id": reservationID, "amount": 99,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversettle must be rejected: %d %s", rec.Code, rec.Body.String())
	}
}

// TestWorkflowLegacyMigrationIsIdempotent 项目迁移第二次不再建新定义。
func TestWorkflowLegacyMigrationIsIdempotent(t *testing.T) {
	engine := newWorkflowTestEngine(t)
	first := workflowPost(t, engine, "/api/v1/projects/p1/workflows/migrate", map[string]any{"workspace_id": "ws"})
	if first.Code != http.StatusOK {
		t.Fatalf("migrate: %d %s", first.Code, first.Body.String())
	}
	if workflowDecodeBody(t, first)["migrated"] != true {
		t.Fatalf("first migration must report migrated=true: %s", first.Body.String())
	}
	second := workflowPost(t, engine, "/api/v1/projects/p1/workflows/migrate", map[string]any{"workspace_id": "ws"})
	if workflowDecodeBody(t, second)["migrated"] != false {
		t.Fatalf("second migration must be a no-op: %s", second.Body.String())
	}
}

// TestWorkflowBuiltinTemplatesReturned 内置模板始终可见（市场关闭时也不为空）。
func TestWorkflowBuiltinTemplatesReturned(t *testing.T) {
	engine := newWorkflowTestEngine(t)
	rec := workflowGet(t, engine, "/api/v1/workflow-templates")
	if rec.Code != http.StatusOK {
		t.Fatalf("list templates: %d %s", rec.Code, rec.Body.String())
	}
	items, ok := workflowDecodeBody(t, rec)["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("builtin templates missing: %s", rec.Body.String())
	}
}

// TestWorkflowLegacyGraphMatchesPythonShape 复刻 Python legacy_linear_graph 的形态。
func TestWorkflowLegacyGraphMatchesPythonShape(t *testing.T) {
	nodes, edges := service.LegacyLinearGraph()
	if len(nodes) != len(service.LegacyFlowNodeTypes) {
		t.Fatalf("node count mismatch: %d", len(nodes))
	}
	if len(edges) != len(service.LegacyFlowNodeTypes)-1 {
		t.Fatalf("edge count mismatch: %d", len(edges))
	}
	if domain.WorkflowMapFloat(nodes[0], "weight", 0) != 1.0 {
		t.Fatalf("source_import weight must stay 1.0: %v", nodes[0])
	}
	if err := domain.WorkflowValidateGraph(nodes, edges); err != nil {
		t.Fatalf("legacy graph must validate: %v", err)
	}
}

// TestWorkflowRepositoryTablesRegistered 保证迁移里有工作流全表（模型不被 AutoMigrate 漏掉）。
func TestWorkflowRepositoryTablesRegistered(t *testing.T) {
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "tables.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Migrate(gdb, dialect, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := gdb.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	for _, table := range []string{
		"workflow_definitions", "workflow_revisions", "workflow_nodes", "workflow_edges",
		"workflow_runs", "workflow_node_runs", "workflow_node_run_items", "workflow_templates",
		"workflow_marketplace_reviews", "workflow_usage_stats", "workflow_approvals",
		"budget_reservations", "project_event_log",
	} {
		var name string
		err := gdb.Raw("SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).
			Scan(&name).Error
		if err != nil || name != table {
			t.Errorf("table missing: %s (%v)", table, err)
		}
	}
	var _ = repository.WorkflowRun{}
}

// seedWorkflowUsers 播种外键所需的 users 行。
func seedWorkflowUsers(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	now := db.NewFlexibleTime(time.Now())
	for _, id := range []string{"u1", "system"} {
		row := repository.User{ID: id, Username: id, Role: "admin", IsActive: true, CreatedAt: now, UpdatedAt: now}
		if err := gdb.Where(repository.User{ID: id}).FirstOrCreate(&row).Error; err != nil {
			t.Fatalf("seed user %s: %v", id, err)
		}
	}
}
