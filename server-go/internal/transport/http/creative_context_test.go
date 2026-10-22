package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/creative"
)

func TestRegisterCreativeContextRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterCreativeContext(r.Group("/api/v1"), CreativeContextDeps{})

	registered := map[string]bool{}
	for _, route := range r.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	for _, want := range []string{
		http.MethodPost + " /api/v1/projects/:name/creative-context/resolve",
		http.MethodPost + " /api/v1/projects/:name/creative-context/resolve-references",
	} {
		if !registered[want] {
			t.Errorf("route not registered: %s", want)
		}
	}
}

// newCreativeContextHandlerEnv 构造真实服务 + DB + 项目，并返回已挂载路由的
// 引擎与板上带 document 条目的 boardID。
func newCreativeContextHandlerEnv(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gdb := newJianyingHTTPTestDB(t)
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "Project One", domain.ContentDrama, domain.GenerationStoryboard, ""); err != nil {
		t.Fatalf("create project: %v", err)
	}
	ctx := context.Background()
	user := repository.DefaultUserID
	boards := creative.NewCreativeBoardService(gdb)
	board, err := boards.CreateBoard(ctx, user, "p1", "Board", nil, nil)
	if err != nil {
		t.Fatalf("create board: %v", err)
	}
	boardID, _ := board["id"].(string)
	if _, err := boards.AddItem(ctx, boardID, user, "document", "document", "doc-1",
		nil, nil, nil, map[string]any{"name": "原始文稿"}, nil); err != nil {
		t.Fatalf("add item: %v", err)
	}
	plan := repository.CreationPlan{
		ID: "plan-1", UserID: user, ProjectID: "p1",
		SkillID: "novel-to-drama", Status: "previewed", Fingerprint: "fp-plan-1",
	}
	if err := gdb.Create(&plan).Error; err != nil {
		t.Fatalf("insert plan: %v", err)
	}

	r := gin.New()
	RegisterCreativeContext(r.Group("/api/v1"), CreativeContextDeps{
		Context: creative.NewCreativeContextService(gdb, store),
		Store:   store,
	})
	return r, boardID
}

func postCreativeContext(t *testing.T, r *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestCreativeContextResolveHandlerSuccess(t *testing.T) {
	r, boardID := newCreativeContextHandlerEnv(t)
	rec := postCreativeContext(t, r, "/api/v1/projects/p1/creative-context/resolve", map[string]any{
		"creative_board_id": boardID,
		"creation_plan_id":  "plan-1",
		"selected_resources": []map[string]any{
			{"id": "doc-1", "resource_type": "document"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["current_skill_id"] != "novel-to-drama" {
		t.Errorf("current_skill_id = %v", out["current_skill_id"])
	}
	if out["generation_mode"] != "storyboard" {
		t.Errorf("generation_mode = %v", out["generation_mode"])
	}
}

func TestCreativeContextResolveHandlerErrors(t *testing.T) {
	r, boardID := newCreativeContextHandlerEnv(t)

	// 项目不存在 → 404 project_not_found。
	rec := postCreativeContext(t, r, "/api/v1/projects/none/creative-context/resolve", map[string]any{
		"creative_board_id": boardID, "creation_plan_id": "plan-1",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("project 404: status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "project_not_found") {
		t.Errorf("project 404 body = %s", rec.Body.String())
	}

	// 缺 creation_plan_id → 422。
	rec = postCreativeContext(t, r, "/api/v1/projects/p1/creative-context/resolve", map[string]any{
		"creative_board_id": boardID,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing plan: status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "creation_plan_id") {
		t.Errorf("missing plan body = %s", rec.Body.String())
	}

	// plan 不存在 → 409 creation_plan_not_found。
	rec = postCreativeContext(t, r, "/api/v1/projects/p1/creative-context/resolve", map[string]any{
		"creative_board_id": boardID, "creation_plan_id": "plan-404",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("plan 404: status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "creation_plan_not_found") {
		t.Errorf("plan 404 body = %s", rec.Body.String())
	}
}

func TestCreativeContextResolveReferencesHandler(t *testing.T) {
	r, boardID := newCreativeContextHandlerEnv(t)

	rec := postCreativeContext(t, r, "/api/v1/projects/p1/creative-context/resolve-references", map[string]any{
		"creative_board_id": boardID,
		"context_references": []map[string]any{
			{"text": "原始文稿"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	references, _ := out["references"].([]any)
	if len(references) != 1 {
		t.Fatalf("references = %v", references)
	}
	resolution, _ := references[0].(map[string]any)
	if resolution["id"] != "doc-1" {
		t.Errorf("resolution id = %v", resolution["id"])
	}

	// 引用无法解析 → 409 context_reference_unresolved。
	rec = postCreativeContext(t, r, "/api/v1/projects/p1/creative-context/resolve-references", map[string]any{
		"creative_board_id": boardID,
		"context_references": []map[string]any{
			{"text": "不存在的引用"},
		},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("unresolved: status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "context_reference_unresolved") {
		t.Errorf("unresolved body = %s", rec.Body.String())
	}

	// references 端点 context_references 至少 1 条 → 422。
	rec = postCreativeContext(t, r, "/api/v1/projects/p1/creative-context/resolve-references", map[string]any{
		"creative_board_id":  boardID,
		"context_references": []map[string]any{},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty refs: status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreativeContextFeatureDisabled(t *testing.T) {
	t.Setenv("SHOTWISE_FEATURE_CONTEXT_AGENT", "0")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	gated := r.Group("/api/v1", RequireFeature("context_agent"))
	RegisterCreativeContext(gated, CreativeContextDeps{})

	rec := postCreativeContext(t, r, "/api/v1/projects/p1/creative-context/resolve", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "feature_disabled") {
		t.Errorf("body = %s", rec.Body.String())
	}
}
