package creative

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/creative"
	"github.com/shotwise/shotwise/migrations"
)

// TestRegisterCreationPlanRoutes 逐条核对 13 个端点（对应 server/routers/creation_plans.py）。
func TestRegisterCreationPlanRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterCreationPlans(r.Group("/api/v1"), CreationPlanDeps{})

	registered := map[string]bool{}
	for _, route := range r.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	want := []string{
		http.MethodGet + " /api/v1/creation-skills",
		http.MethodGet + " /api/v1/creation-skills/:skill_id/versions",
		http.MethodGet + " /api/v1/creation-resources",
		http.MethodPost + " /api/v1/creation-skills/:skill_id/deactivate",
		http.MethodPost + " /api/v1/creation-plans/preview",
		http.MethodGet + " /api/v1/creation-plans/:plan_id",
		http.MethodPost + " /api/v1/creation-plans/:plan_id/start",
		http.MethodPost + " /api/v1/creation-plans/:plan_id/cancel",
		http.MethodPost + " /api/v1/creation-plans/:plan_id/invalidate",
		http.MethodPost + " /api/v1/creation-plans/:plan_id/restart",
		http.MethodPost + " /api/v1/creation-plans/:plan_id/recompile",
		http.MethodPost + " /api/v1/creation-compatibility-events/:event_id/outcome",
		http.MethodGet + " /api/v1/creation-compatibility-events/metrics",
	}
	if len(want) != 13 {
		t.Fatalf("endpoint list must stay at 13, got %d", len(want))
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

func newCreationPlanTestEngine(t *testing.T) (*gin.Engine, *service.ProjectStore) {
	t.Helper()
	return newCreationPlanTestEngineWithRole(t, "admin")
}

// newCreationPlanTestEngineWithRole 构建带注入用户的引擎；role 用于校验 admin 围栏。
func newCreationPlanTestEngineWithRole(t *testing.T, role string) (*gin.Engine, *service.ProjectStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "creation.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Migrate(gdb, dialect, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedWorkflowUsers(t, gdb)
	pool, err := gdb.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	projects := t.TempDir()
	store := service.NewProjectStore(projects)
	if _, err := store.Create("p1", "项目一", project.ContentDrama, project.GenerationStoryboard, ""); err != nil {
		t.Fatalf("create project: %v", err)
	}
	plans := creative.NewCreationPlanService(gdb, dialect, creative.NewWorkflowService(gdb), nil)
	// 对应 Python 启动期的 sync_official_creation_skills：把代码拥有的清单物化到库里。
	if _, err := plans.Catalog().SyncOfficialCreationSkills(context.Background()); err != nil {
		t.Fatalf("sync official skills: %v", err)
	}

	r := gin.New()
	group := r.Group("/api/v1")
	group.Use(func(c *gin.Context) {
		c.Set("shotwise.user", &auth.User{ID: "u1", Sub: "u1", Role: role})
		c.Next()
	})
	RegisterCreationPlans(group, CreationPlanDeps{Store: store, Plans: plans})
	return r, store
}

func creationJSON(t *testing.T, engine *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// firstCompatibleSkillVersion 取当前项目第一个兼容的官方 Skill 版本 ID。
//
// 目录按 skill_id 字典序返回，第一个未必与当前项目兼容——用例必须挑兼容的那一个，
// 否则预览会直接落到"不兼容"分支。
func firstCompatibleSkillVersion(t *testing.T, engine *gin.Engine) string {
	t.Helper()
	rec := creationGet(t, engine, "/api/v1/creation-skills?project_id=p1&resource_types=document")
	if rec.Code != http.StatusOK {
		t.Fatalf("list skills: %d %s", rec.Code, rec.Body.String())
	}
	items, ok := workflowDecodeBody(t, rec)["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatal("official skills must be synced into the catalog")
	}
	for _, raw := range items {
		item, isMap := raw.(map[string]any)
		if !isMap {
			continue
		}
		report, isMap := item["compatibility"].(map[string]any)
		if !isMap || report["compatible"] != true {
			continue
		}
		// 官方 revision 的 generation_mode 取 Skill 声明集合排序后的第一个：
		// 只声明 storyboard 的 Skill 才保证 revision 与本项目同路线。
		modes, _ := report["supported_generation_modes"].([]any)
		if len(modes) != 1 || modes[0] != "storyboard" {
			continue
		}
		return item["version_id"].(string)
	}
	t.Fatalf("no compatible official skill for project: %s", rec.Body.String())
	return ""
}

// TestCreationPlanPreviewStartCancel 走通 预览 → 启动 → 取消 的主链路。
func TestCreationPlanPreviewStartCancel(t *testing.T) {
	engine, store := newCreationPlanTestEngine(t)
	_ = store

	versionID := firstCompatibleSkillVersion(t, engine)

	rec := creationJSON(t, engine, http.MethodPost, "/api/v1/creation-plans/preview", map[string]any{
		"workspace_id":              "ws",
		"project_id":                "p1",
		"creation_skill_version_id": versionID,
		"resource_ids":              []string{"doc-1", "doc-1", "doc-2"},
		"resource_types":            []string{"document"},
		"parameters":                map[string]any{"k": "v"},
		"idempotency_key":           "idem-1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	preview := workflowDecodeBody(t, rec)
	planID := preview["plan_id"].(string)
	if preview["deduped"] != false {
		t.Fatalf("first preview must not be deduped: %s", rec.Body.String())
	}
	if preview["status"] != "previewed" {
		t.Fatalf("unexpected status: %s", rec.Body.String())
	}
	report := preview["compatibility_report"].(map[string]any)
	if report["compatible"] != true {
		t.Fatalf("drama project must be compatible with a drama skill: %s", rec.Body.String())
	}

	// 同 idempotency_key 重放：原样返回且 deduped=true。
	rec = creationJSON(t, engine, http.MethodPost, "/api/v1/creation-plans/preview", map[string]any{
		"workspace_id":              "ws",
		"project_id":                "p1",
		"creation_skill_version_id": versionID,
		"resource_ids":              []string{"doc-1"},
		"resource_types":            []string{"document"},
		"idempotency_key":           "idem-1",
	})
	if workflowDecodeBody(t, rec)["deduped"] != true {
		t.Fatalf("replay must be deduped: %s", rec.Body.String())
	}

	rec = creationJSON(t, engine, http.MethodPost, "/api/v1/creation-plans/"+planID+"/start", map[string]any{
		"mode":             "hybrid",
		"cost_confirmed":   true,
		"review_confirmed": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	started := workflowDecodeBody(t, rec)
	if started["workflow_run_id"] == nil || started["workflow_run_id"] == "" {
		t.Fatalf("start must create a run: %s", rec.Body.String())
	}
	if started["status"] != "running" && started["status"] != "started" {
		t.Fatalf("unexpected plan status: %s", rec.Body.String())
	}

	rec = creationJSON(t, engine, http.MethodPost, "/api/v1/creation-plans/"+planID+"/cancel", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	if workflowDecodeBody(t, rec)["status"] != "cancelled" {
		t.Fatalf("cancel must flip status: %s", rec.Body.String())
	}

	// 已取消的计划不得再启动。
	rec = creationJSON(t, engine, http.MethodPost, "/api/v1/creation-plans/"+planID+"/start", map[string]any{
		"cost_confirmed": true,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("start after cancel must conflict: %d %s", rec.Code, rec.Body.String())
	}
	if workflowDecodeBody(t, rec)["detail"].(map[string]any)["code"] != "creation_plan_cancelled" {
		t.Fatalf("unexpected code: %s", rec.Body.String())
	}
}

// TestCreationPlanStartRejectsChangedSnapshot 项目快照变了必须先重编译，不能直接启动。
func TestCreationPlanStartRejectsChangedSnapshot(t *testing.T) {
	engine, store := newCreationPlanTestEngine(t)
	versionID := firstCompatibleSkillVersion(t, engine)

	rec := creationJSON(t, engine, http.MethodPost, "/api/v1/creation-plans/preview", map[string]any{
		"workspace_id":              "ws",
		"project_id":                "p1",
		"creation_skill_version_id": versionID,
		"resource_ids":              []string{"doc-1"},
		"resource_types":            []string{"document"},
		"idempotency_key":           "idem-snapshot",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	planID := workflowDecodeBody(t, rec)["plan_id"].(string)

	if err := store.Update("p1", func(pf *project.ProjectFile) error {
		pf.Style = "水墨"
		return nil
	}); err != nil {
		t.Fatalf("update project: %v", err)
	}

	rec = creationJSON(t, engine, http.MethodPost, "/api/v1/creation-plans/"+planID+"/start", map[string]any{
		"cost_confirmed":   true,
		"review_confirmed": true,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale snapshot must conflict: %d %s", rec.Code, rec.Body.String())
	}
	if workflowDecodeBody(t, rec)["detail"].(map[string]any)["code"] != "project_snapshot_changed" {
		t.Fatalf("unexpected code: %s", rec.Body.String())
	}
}

// TestCreationPlanInvalidateThenRecompile 作废后用当前快照重编译出一条新计划。
func TestCreationPlanInvalidateThenRecompile(t *testing.T) {
	engine, store := newCreationPlanTestEngine(t)
	_ = store

	versionID := firstCompatibleSkillVersion(t, engine)

	rec := creationJSON(t, engine, http.MethodPost, "/api/v1/creation-plans/preview", map[string]any{
		"workspace_id":              "ws",
		"project_id":                "p1",
		"creation_skill_version_id": versionID,
		"resource_ids":              []string{"doc-1"},
		"resource_types":            []string{"document"},
		"idempotency_key":           "idem-recompile",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	planID := workflowDecodeBody(t, rec)["plan_id"].(string)

	// 重编译的意义在于「项目快照已变 → 用新快照重出一条不可变计划」：
	// 快照不变时新计划与旧计划完全同构（plan_id 由快照决定），这里先改项目风格。
	if err := store.Update("p1", func(pf *project.ProjectFile) error {
		pf.Style = "水墨"
		return nil
	}); err != nil {
		t.Fatalf("update project: %v", err)
	}

	rec = creationJSON(t, engine, http.MethodPost, "/api/v1/creation-plans/"+planID+"/invalidate", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("invalidate: %d %s", rec.Code, rec.Body.String())
	}
	if workflowDecodeBody(t, rec)["status"] != "invalidated" {
		t.Fatalf("unexpected status: %s", rec.Body.String())
	}

	rec = creationJSON(t, engine, http.MethodPost, "/api/v1/creation-plans/"+planID+"/recompile", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("recompile: %d %s", rec.Code, rec.Body.String())
	}
	recompiled := workflowDecodeBody(t, rec)
	if recompiled["recompiled_from"] != planID {
		t.Fatalf("recompile must point back: %s", rec.Body.String())
	}
	if recompiled["plan_id"] == planID {
		t.Fatalf("recompile must produce a new plan id: %s", rec.Body.String())
	}

	// 已作废的计划不能再启动。
	rec = creationJSON(t, engine, http.MethodPost, "/api/v1/creation-plans/"+planID+"/start", map[string]any{
		"cost_confirmed": true,
	})
	if workflowDecodeBody(t, rec)["detail"].(map[string]any)["code"] != "creation_plan_invalidated" {
		t.Fatalf("unexpected code: %s", rec.Body.String())
	}
}

// TestCreationPlanEmptyResourcesRejected 空资源选择直接 400。
func TestCreationPlanEmptyResourcesRejected(t *testing.T) {
	engine, store := newCreationPlanTestEngine(t)
	_ = store
	versionID := firstCompatibleSkillVersion(t, engine)

	rec := creationJSON(t, engine, http.MethodPost, "/api/v1/creation-plans/preview", map[string]any{
		"workspace_id":              "ws",
		"project_id":                "p1",
		"creation_skill_version_id": versionID,
		"resource_ids":              []string{"   "},
		"resource_types":            []string{"document"},
		"idempotency_key":           "idem-empty",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty resources must be rejected: %d %s", rec.Code, rec.Body.String())
	}
	if workflowDecodeBody(t, rec)["detail"] != "at least one real resource must be selected" {
		t.Fatalf("unexpected detail: %s", rec.Body.String())
	}
}

// TestCreationPlanUnknownSkillVersion 未知 Skill 版本回 400（同步后仍找不到）。
func TestCreationPlanUnknownSkillVersion(t *testing.T) {
	engine, store := newCreationPlanTestEngine(t)
	_ = store
	rec := creationJSON(t, engine, http.MethodPost, "/api/v1/creation-plans/preview", map[string]any{
		"workspace_id":              "ws",
		"project_id":                "p1",
		"creation_skill_version_id": "ghost:v1",
		"resource_ids":              []string{"doc-1"},
		"resource_types":            []string{"document"},
		"idempotency_key":           "idem-ghost",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown skill version must be rejected: %d %s", rec.Code, rec.Body.String())
	}
}

// TestCreationPlanNotFoundEnvelope 计划不存在 / 不属于当前用户 → 404。
func TestCreationPlanNotFoundEnvelope(t *testing.T) {
	engine, store := newCreationPlanTestEngine(t)
	_ = store
	rec := creationGet(t, engine, "/api/v1/creation-plans/cp_missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d %s", rec.Code, rec.Body.String())
	}
	if workflowDecodeBody(t, rec)["detail"] != "creation_plan_not_found" {
		t.Fatalf("unexpected detail: %s", rec.Body.String())
	}
}

// TestCreationPlanProjectMissing 项目不存在 → 404 project_not_found。
func TestCreationPlanProjectMissing(t *testing.T) {
	engine, store := newCreationPlanTestEngine(t)
	_ = store
	rec := creationGet(t, engine, "/api/v1/creation-skills?project_id=nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d %s", rec.Code, rec.Body.String())
	}
	if workflowDecodeBody(t, rec)["detail"] != "project_not_found" {
		t.Fatalf("unexpected detail: %s", rec.Body.String())
	}
}

// TestCreationPlanCompatibilityMetrics 兼容性指标是空数组而不是 null。
func TestCreationPlanCompatibilityMetrics(t *testing.T) {
	engine, store := newCreationPlanTestEngine(t)
	_ = store
	rec := creationGet(t, engine, "/api/v1/creation-compatibility-events/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := workflowDecodeBody(t, rec)["items"].([]any); !ok {
		t.Fatalf("items must be an array: %s", rec.Body.String())
	}
}

// TestListCreationResourcesExposesProjectEntities 资源清单只暴露 id / label / type。
func TestListCreationResourcesExposesProjectEntities(t *testing.T) {
	engine, store := newCreationPlanTestEngine(t)
	_ = store
	rec := creationGet(t, engine, "/api/v1/creation-resources?project_id=p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("resources: %d %s", rec.Code, rec.Body.String())
	}
	items, ok := workflowDecodeBody(t, rec)["items"].([]any)
	if !ok {
		t.Fatalf("items must be an array: %s", rec.Body.String())
	}
	for _, raw := range items {
		item, isMap := raw.(map[string]any)
		if !isMap {
			t.Fatalf("item must be an object: %v", raw)
		}
		for _, key := range []string{"id", "label", "type"} {
			if _, exists := item[key]; !exists {
				t.Fatalf("resource missing %s: %v", key, item)
			}
		}
	}
}

// TestDeactivateCreationSkillRequiresAdmin 非 admin 停用 Skill 被 403 拦下。
func TestDeactivateCreationSkillRequiresAdmin(t *testing.T) {
	engine, _ := newCreationPlanTestEngineWithRole(t, "user")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/creation-skills/novel-to-drama/deactivate", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin must be forbidden: %d %s", rec.Code, rec.Body.String())
	}
}

// TestDeactivateCreationSkillByAdmin admin 停用后该 Skill 从目录消失。
func TestDeactivateCreationSkillByAdmin(t *testing.T) {
	engine, store := newCreationPlanTestEngine(t)
	_ = store
	req := httptest.NewRequest(http.MethodPost, "/api/v1/creation-skills/novel-to-drama/deactivate", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin deactivate: %d %s", rec.Code, rec.Body.String())
	}
	body := workflowDecodeBody(t, rec)
	if body["active"] != false {
		t.Fatalf("expected active=false: %s", rec.Body.String())
	}
}

// creationGet 发起 GET 请求。
func creationGet(t *testing.T, engine *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}
