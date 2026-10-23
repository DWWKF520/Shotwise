package script

// 草稿晋升 + 提示词注册表端点测试（对应 server/routers/draft_promotions.py 9 个端点、
// server/routers/prompt_registry.py 2 个端点）。

import (
	"bytes"
	"encoding/json"
	servicereference "github.com/shotwise/shotwise/internal/service/reference"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/domain/creation"
	domaindraft "github.com/shotwise/shotwise/internal/domain/draft"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/service"
	httpcreative "github.com/shotwise/shotwise/internal/transport/http/creative"
)

// draftTestScript 一份结构合法的 narration 剧本（字段口径见 domain/script_models.go）。
func draftTestScript(desc string) map[string]any {
	return map[string]any{
		"title": "第1集", "content_mode": "narration", "duration_seconds": float64(4),
		"novel":   map[string]any{"title": "小说", "chapter": "第1集"},
		"episode": float64(1), "metadata": map[string]any{"status": "draft"},
		"segments": []any{map[string]any{
			"segment_id": "E1S1", "duration_seconds": float64(4), "novel_text": "原文",
			"characters_in_segment": []any{"阿明"},
			"image_prompt": map[string]any{
				"scene": "桥上的两人",
				"composition": map[string]any{
					"shot_type": "Medium Shot", "lighting": "暖光", "ambiance": "安静",
				},
			},
			"video_prompt": map[string]any{
				"action": desc, "camera_motion": "Static", "ambiance_audio": "风声",
				"dialogue": []any{map[string]any{"speaker": "阿明", "line": "你好"}},
			},
		}},
	}
}

// newDraftTestEngine 构建带注入用户的引擎；userID 为空串时用默认单用户身份。
func newDraftTestEngine(t *testing.T, userID string) (*gin.Engine, *service.ProjectStore) {
	t.Helper()
	root := t.TempDir()
	store := service.NewProjectStore(root)
	repo := servicereference.NewFileDraftPromotionRepository(store)
	svc := domaindraft.NewDraftPromotionService(repo, servicereference.ScriptStructureDraftValidator())

	r := gin.New()
	group := r.Group("/api/v1")
	group.Use(func(c *gin.Context) {
		id := userID
		if id == "" {
			id = "u1"
		}
		c.Set("shotwise.user", &auth.User{ID: id, Sub: id})
		c.Next()
	})
	RegisterDraftPromotions(group, DraftPromotionDeps{Store: store, Drafts: svc})
	httpcreative.RegisterPromptRegistry(group, httpcreative.PromptRegistryDeps{ProfileRoot: func() string { return "" }})
	return r, store
}

// draftRequest 发起任意方法请求；body 为 nil 时不带 body。
func draftRequest(t *testing.T, engine *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// draftDecode 解码响应体；非 2xx 时把响应体带进错误信息方便定位。
func draftDecode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body (%d): %s", rec.Code, rec.Body.String())
	}
	return out
}

// draftCreate 建草稿并返回 draft_id。
func draftCreate(t *testing.T, engine *gin.Engine, path string, scriptFile string, content map[string]any) map[string]any {
	t.Helper()
	rec := draftRequest(t, engine, http.MethodPost, path, map[string]any{
		"script_file": scriptFile,
		"content":     content,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s: %d %s", path, rec.Code, rec.Body.String())
	}
	return draftDecode(t, rec)
}

// TestDraftPromotionEndpointInventory 端点数与 Python router 对齐：9 + 2 = 11。
func TestDraftPromotionEndpointInventory(t *testing.T) {
	engine, _ := newDraftTestEngine(t, "")
	routes := engine.Routes()
	draft, prompts := 0, 0
	for _, route := range routes {
		switch {
		case strings.Contains(route.Path, "/draft-promotions"):
			draft++
		case strings.Contains(route.Path, "/prompts/registry"):
			prompts++
		}
	}
	if draft != 9 {
		t.Fatalf("draft-promotion endpoints = %d, want 9:\n%v", draft, routePaths(routes))
	}
	if prompts != 2 {
		t.Fatalf("prompt registry endpoints = %d, want 2:\n%v", prompts, routePaths(routes))
	}
}

func routePaths(routes []gin.RouteInfo) []string {
	out := make([]string, 0, len(routes))
	for _, route := range routes {
		out = append(out, route.Method+" "+route.Path)
	}
	return out
}

// TestDraftPromotionFullLifecycle 建 → 列 → 读 → 改 → 准备 → 确认 全链路。
func TestDraftPromotionFullLifecycle(t *testing.T) {
	engine, store := newDraftTestEngine(t, "")
	if _, err := store.Create("p1", "项目", project.ContentDrama, project.GenerationStoryboard, "写实"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := store.SaveScript("p1", "episode_1.json", draftTestScript("并肩前行")); err != nil {
		t.Fatalf("seed script: %v", err)
	}

	// 三个 origin 各建一条。
	agentDraft := draftCreate(t, engine, "/api/v1/projects/p1/draft-promotions", "episode_1.json", draftTestScript("改后A"))
	if agentDraft["origin"] != "agent" || agentDraft["status"] != "active" || agentDraft["prepared"] != false {
		t.Fatalf("agent draft = %v", agentDraft)
	}
	uploadDraft := draftCreate(t, engine, "/api/v1/projects/p1/draft-promotions/upload", "episode_1.json", draftTestScript("改后U"))
	if uploadDraft["origin"] != "upload" {
		t.Fatalf("upload draft origin = %v", uploadDraft["origin"])
	}
	onlineDraft := draftCreate(t, engine, "/api/v1/projects/p1/draft-promotions/online-ai", "episode_1.json", draftTestScript("改后O"))
	if onlineDraft["origin"] != "online_ai" {
		t.Fatalf("online draft origin = %v", onlineDraft["origin"])
	}
	draftID, _ := agentDraft["draft_id"].(string)
	if draftID == "" {
		t.Fatal("draft_id missing")
	}
	if agentDraft["base_revision"].(float64) <= 0 {
		t.Fatalf("base_revision = %v", agentDraft["base_revision"])
	}

	// 列表：不带 script_file 走 list_all，带则按剧本过滤。
	rec := draftRequest(t, engine, http.MethodGet, "/api/v1/projects/p1/draft-promotions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if items := draftDecode(t, rec)["drafts"].([]any); len(items) != 3 {
		t.Fatalf("list_all = %d, want 3", len(items))
	}
	rec = draftRequest(t, engine, http.MethodGet, "/api/v1/projects/p1/draft-promotions?script_file=episode_1.json", nil)
	if items := draftDecode(t, rec)["drafts"].([]any); len(items) != 3 {
		t.Fatalf("list_drafts = %d, want 3", len(items))
	}
	rec = draftRequest(t, engine, http.MethodGet, "/api/v1/projects/p1/draft-promotions?script_file=other.json", nil)
	if items := draftDecode(t, rec)["drafts"].([]any); len(items) != 0 {
		t.Fatalf("list_drafts(other) = %d, want 0", len(items))
	}

	// 单条读取。
	rec = draftRequest(t, engine, http.MethodGet, "/api/v1/projects/p1/draft-promotions/"+draftID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	if got := draftDecode(t, rec)["draft_id"]; got != draftID {
		t.Fatalf("get draft_id = %v", got)
	}

	// 改内容。
	rec = draftRequest(t, engine, http.MethodPatch, "/api/v1/projects/p1/draft-promotions/"+draftID,
		map[string]any{"content": draftTestScript("改后B")})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}

	// 准备：官方未变 → ready_for_confirmation + 令牌 + 预览。
	rec = draftRequest(t, engine, http.MethodPost, "/api/v1/projects/p1/draft-promotions/"+draftID+"/prepare", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("prepare: %d %s", rec.Code, rec.Body.String())
	}
	prep := draftDecode(t, rec)
	if prep["status"] != "ready_for_confirmation" {
		t.Fatalf("prepare status = %v", prep["status"])
	}
	token, _ := prep["confirmation_token"].(string)
	if token == "" {
		t.Fatal("confirmation_token missing")
	}
	if issues := prep["validation_issues"].([]any); len(issues) != 0 {
		t.Fatalf("validation_issues = %v", issues)
	}
	if _, ok := prep["conflicts"].([]any); !ok {
		t.Fatalf("conflicts must be a list: %v", prep["conflicts"])
	}

	// 错误令牌 → invalid_confirmation（200，不是 4xx）。
	rec = draftRequest(t, engine, http.MethodPost, "/api/v1/projects/p1/draft-promotions/"+draftID+"/confirm",
		map[string]any{"confirmation_token": "wrong"})
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm bad token: %d %s", rec.Code, rec.Body.String())
	}
	if draftDecode(t, rec)["status"] != "invalid_confirmation" {
		t.Fatalf("bad token status = %v", draftDecode(t, rec)["status"])
	}

	// 正确令牌 → promoted，且正式剧本被原子写盘。
	rec = draftRequest(t, engine, http.MethodPost, "/api/v1/projects/p1/draft-promotions/"+draftID+"/confirm",
		map[string]any{"confirmation_token": token})
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}
	result := draftDecode(t, rec)
	if result["status"] != "promoted" {
		t.Fatalf("confirm status = %v", result["status"])
	}
	if revision, ok := result["promoted_revision"].(float64); !ok || revision <= 0 {
		t.Fatalf("promoted_revision = %v", result["promoted_revision"])
	}
	official, err := store.ReadScriptJSON("p1", "episode_1.json")
	if err != nil {
		t.Fatalf("read official: %v", err)
	}
	action := official["segments"].([]any)[0].(map[string]any)["video_prompt"].(map[string]any)["action"]
	if action != "改后B" {
		t.Fatalf("official script action = %v, want 改后B", action)
	}

	// 已消费的令牌不能二次确认。
	rec = draftRequest(t, engine, http.MethodPost, "/api/v1/projects/p1/draft-promotions/"+draftID+"/confirm",
		map[string]any{"confirmation_token": token})
	if draftDecode(t, rec)["status"] != "invalid_confirmation" {
		t.Fatalf("re-confirm status = %v", draftDecode(t, rec)["status"])
	}
	// 另两条草稿不受影响。
	rec = draftRequest(t, engine, http.MethodGet, "/api/v1/projects/p1/draft-promotions", nil)
	if items := draftDecode(t, rec)["drafts"].([]any); len(items) != 3 {
		t.Fatalf("list after confirm = %d", len(items))
	}
	_ = uploadDraft
	_ = onlineDraft
}

// TestDraftPromotionPrepareInvalidContent 结构非法的草稿 prepare 返回 invalid。
func TestDraftPromotionPrepareInvalidContent(t *testing.T) {
	engine, store := newDraftTestEngine(t, "")
	if _, err := store.Create("p1", "项目", project.ContentDrama, project.GenerationStoryboard, "写实"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := store.SaveScript("p1", "episode_1.json", draftTestScript("a")); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	broken := draftTestScript("a")
	broken["segments"] = "not-a-list"
	draft := draftCreate(t, engine, "/api/v1/projects/p1/draft-promotions", "episode_1.json", broken)
	draftID := draft["draft_id"].(string)

	rec := draftRequest(t, engine, http.MethodPost, "/api/v1/projects/p1/draft-promotions/"+draftID+"/prepare", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("prepare: %d %s", rec.Code, rec.Body.String())
	}
	prep := draftDecode(t, rec)
	if prep["status"] != "invalid" {
		t.Fatalf("status = %v", prep["status"])
	}
	issues, ok := prep["validation_issues"].([]any)
	if !ok || len(issues) == 0 {
		t.Fatalf("validation_issues = %v", prep["validation_issues"])
	}
	first := issues[0].(map[string]any)
	if first["code"] != "script_structure_invalid" {
		t.Fatalf("issue code = %v", first["code"])
	}
}

// TestDraftPromotionAutoMergeOnOfficialChange 官方变了 → 自动合并后仍可确认。
func TestDraftPromotionAutoMergeOnOfficialChange(t *testing.T) {
	engine, store := newDraftTestEngine(t, "")
	if _, err := store.Create("p1", "项目", project.ContentDrama, project.GenerationStoryboard, "写实"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := store.SaveScript("p1", "episode_1.json", draftTestScript("原始")); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	draft := draftCreate(t, engine, "/api/v1/projects/p1/draft-promotions", "episode_1.json", draftTestScript("草稿改"))
	draftID := draft["draft_id"].(string)

	// 外部写者改了官方剧本的另一个字段（title）。
	official := draftTestScript("原始")
	official["title"] = "第1集·官方改"
	if err := store.SaveScript("p1", "episode_1.json", official); err != nil {
		t.Fatalf("rewrite official: %v", err)
	}

	rec := draftRequest(t, engine, http.MethodPost, "/api/v1/projects/p1/draft-promotions/"+draftID+"/prepare", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("prepare: %d %s", rec.Code, rec.Body.String())
	}
	prep := draftDecode(t, rec)
	if prep["status"] != "ready_for_confirmation" {
		t.Fatalf("status = %v (%s)", prep["status"], rec.Body.String())
	}
	paths, ok := prep["auto_merged_paths"].([]any)
	if !ok || len(paths) == 0 {
		t.Fatalf("auto_merged_paths = %v", prep["auto_merged_paths"])
	}
	preview := prep["preview_content"].(map[string]any)
	if preview["title"] != "第1集·官方改" {
		t.Fatalf("preview title = %v（官方改动应被自动合并进来）", preview["title"])
	}
	token := prep["confirmation_token"].(string)

	rec = draftRequest(t, engine, http.MethodPost, "/api/v1/projects/p1/draft-promotions/"+draftID+"/confirm",
		map[string]any{"confirmation_token": token})
	if draftDecode(t, rec)["status"] != "promoted" {
		t.Fatalf("confirm = %s", rec.Body.String())
	}
	final, err := store.ReadScriptJSON("p1", "episode_1.json")
	if err != nil {
		t.Fatalf("read official: %v", err)
	}
	action := final["segments"].([]any)[0].(map[string]any)["video_prompt"].(map[string]any)["action"]
	if action != "草稿改" || final["title"] != "第1集·官方改" {
		t.Fatalf("merged official = %v / %v", final["title"], action)
	}
}

// TestDraftPromotionConflicted 同一路径双改 → conflicted，确认拿不到令牌。
func TestDraftPromotionConflicted(t *testing.T) {
	engine, store := newDraftTestEngine(t, "")
	if _, err := store.Create("p1", "项目", project.ContentDrama, project.GenerationStoryboard, "写实"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := store.SaveScript("p1", "episode_1.json", draftTestScript("原始")); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	draft := draftCreate(t, engine, "/api/v1/projects/p1/draft-promotions", "episode_1.json", draftTestScript("草稿改"))
	draftID := draft["draft_id"].(string)

	official := draftTestScript("官方改")
	if err := store.SaveScript("p1", "episode_1.json", official); err != nil {
		t.Fatalf("rewrite official: %v", err)
	}
	rec := draftRequest(t, engine, http.MethodPost, "/api/v1/projects/p1/draft-promotions/"+draftID+"/prepare", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("prepare: %d %s", rec.Code, rec.Body.String())
	}
	prep := draftDecode(t, rec)
	if prep["status"] != "conflicted" {
		t.Fatalf("status = %v", prep["status"])
	}
	conflicts := prep["conflicts"].([]any)
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %v", conflicts)
	}
	conflict := conflicts[0].(map[string]any)
	if conflict["path"] != "/segments/E1S1/video_prompt/action" {
		t.Fatalf("conflict path = %v", conflict["path"])
	}
	// 冲突后没有令牌，确认恒 invalid_confirmation。
	rec = draftRequest(t, engine, http.MethodPost, "/api/v1/projects/p1/draft-promotions/"+draftID+"/confirm",
		map[string]any{"confirmation_token": "token-1"})
	if draftDecode(t, rec)["status"] != "invalid_confirmation" {
		t.Fatalf("confirm after conflict = %s", rec.Body.String())
	}
}

// TestDraftPromotionErrorMapping 404 / 400 口径与 Python router 对齐。
func TestDraftPromotionErrorMapping(t *testing.T) {
	engine, store := newDraftTestEngine(t, "")
	if _, err := store.Create("p1", "项目", project.ContentDrama, project.GenerationStoryboard, "写实"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := store.SaveScript("p1", "episode_1.json", draftTestScript("a")); err != nil {
		t.Fatalf("seed script: %v", err)
	}

	// 项目不存在 → 404 script_not_found。
	rec := draftRequest(t, engine, http.MethodPost, "/api/v1/projects/nope/draft-promotions",
		map[string]any{"script_file": "episode_1.json", "content": draftTestScript("a")})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing project: %d %s", rec.Code, rec.Body.String())
	}
	if detail := draftDecode(t, rec)["detail"]; detail != "剧本 'episode_1.json' 不存在" {
		t.Fatalf("missing project detail = %v", detail)
	}

	// 剧本文件名穿越 → 400 request_invalid。
	rec = draftRequest(t, engine, http.MethodPost, "/api/v1/projects/p1/draft-promotions",
		map[string]any{"script_file": "../escape.json", "content": draftTestScript("a")})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal: %d %s", rec.Code, rec.Body.String())
	}

	// 缺字段 → 422。
	rec = draftRequest(t, engine, http.MethodPost, "/api/v1/projects/p1/draft-promotions",
		map[string]any{"content": draftTestScript("a")})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing script_file: %d %s", rec.Code, rec.Body.String())
	}
	rec = draftRequest(t, engine, http.MethodPost, "/api/v1/projects/p1/draft-promotions",
		map[string]any{"script_file": "episode_1.json", "content": "not-an-object"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("content not object: %d %s", rec.Code, rec.Body.String())
	}

	// 草稿不存在 / 项目 scope 不符 → 404 draft_not_found。
	rec = draftRequest(t, engine, http.MethodGet, "/api/v1/projects/p1/draft-promotions/nope", nil)
	if rec.Code != http.StatusNotFound || draftDecode(t, rec)["detail"] != "draft_not_found" {
		t.Fatalf("unknown draft: %d %s", rec.Code, rec.Body.String())
	}
	draft := draftCreate(t, engine, "/api/v1/projects/p1/draft-promotions", "episode_1.json", draftTestScript("a"))
	draftID := draft["draft_id"].(string)
	rec = draftRequest(t, engine, http.MethodGet, "/api/v1/projects/other/draft-promotions/"+draftID, nil)
	if rec.Code != http.StatusNotFound || draftDecode(t, rec)["detail"] != "draft_not_found" {
		t.Fatalf("scope mismatch: %d %s", rec.Code, rec.Body.String())
	}

	// 作废后不能再改内容 → 400 request_invalid；作废幂等。
	rec = draftRequest(t, engine, http.MethodPost, "/api/v1/projects/p1/draft-promotions/"+draftID+"/abandon", nil)
	if rec.Code != http.StatusOK || draftDecode(t, rec)["status"] != "abandoned" {
		t.Fatalf("abandon: %d %s", rec.Code, rec.Body.String())
	}
	rec = draftRequest(t, engine, http.MethodPatch, "/api/v1/projects/p1/draft-promotions/"+draftID,
		map[string]any{"content": draftTestScript("b")})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch abandoned: %d %s", rec.Code, rec.Body.String())
	}
	rec = draftRequest(t, engine, http.MethodPost, "/api/v1/projects/p1/draft-promotions/"+draftID+"/abandon", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("abandon twice: %d %s", rec.Code, rec.Body.String())
	}
	// 作废后 prepare → invalid + draft_abandoned。
	rec = draftRequest(t, engine, http.MethodPost, "/api/v1/projects/p1/draft-promotions/"+draftID+"/prepare", nil)
	if rec.Code != http.StatusOK || draftDecode(t, rec)["status"] != "invalid" {
		t.Fatalf("prepare abandoned: %d %s", rec.Code, rec.Body.String())
	}

	// 别的用户看不到这条草稿（属主隔离）。
	otherEngine, _ := newDraftTestEngine(t, "u2")
	rec = draftRequest(t, otherEngine, http.MethodGet, "/api/v1/projects/p1/draft-promotions/"+draftID, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("actor isolation: %d %s", rec.Code, rec.Body.String())
	}
}

// TestDraftPromotionStoreFile 草稿记录落盘在 <数据目录>/.draft_promotions.json，
// 重启进程（换仓储实例）后仍可读回——对应 Python 的 JSON store。
func TestDraftPromotionStoreFile(t *testing.T) {
	engine, store := newDraftTestEngine(t, "")
	if _, err := store.Create("p1", "项目", project.ContentDrama, project.GenerationStoryboard, "写实"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := store.SaveScript("p1", "episode_1.json", draftTestScript("a")); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	draft := draftCreate(t, engine, "/api/v1/projects/p1/draft-promotions", "episode_1.json", draftTestScript("a"))
	draftID := draft["draft_id"].(string)

	storePath := filepath.Join(store.Root(), ".draft_promotions.json")
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("store file: %v", err)
	}

	// 用同一个数据目录重建仓储（等价于进程重启）。
	reloaded := domaindraft.NewDraftPromotionService(
		servicereference.NewFileDraftPromotionRepository(service.NewProjectStore(store.Root())),
		servicereference.ScriptStructureDraftValidator(),
	)
	loaded, err := reloaded.GetDraft(draftID, "u1")
	if err != nil {
		t.Fatalf("reload draft: %v", err)
	}
	if loaded.Target.ScriptFile != "episode_1.json" || loaded.Origin != domaindraft.DraftOriginAgent {
		t.Fatalf("reloaded draft = %+v", loaded)
	}
}

// TestPromptRegistryEndpoints 注册表两个只读端点。
func TestPromptRegistryEndpoints(t *testing.T) {
	root := creation.AgentProfileRoot()
	if root == "" {
		t.Skip("agent_runtime_profile not found from test cwd")
	}
	r := gin.New()
	group := r.Group("/api/v1")
	group.Use(func(c *gin.Context) {
		c.Set("shotwise.user", &auth.User{ID: "u1", Sub: "u1"})
		c.Next()
	})
	httpcreative.RegisterPromptRegistry(group, httpcreative.PromptRegistryDeps{ProfileRoot: func() string { return root }})

	rec := draftRequest(t, r, http.MethodGet, "/api/v1/prompts/registry", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	templates := draftDecode(t, rec)["templates"].([]any)
	if len(templates) != 23 {
		t.Fatalf("templates = %d, want 23", len(templates))
	}
	first := templates[0].(map[string]any)
	if first["path"] != "CLAUDE.ad.md" || first["group"] != "system" || first["content_mode"] != "ad" {
		t.Fatalf("first template = %v", first)
	}

	rec = draftRequest(t, r, http.MethodGet, "/api/v1/prompts/registry/CLAUDE.drama.md", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	body := draftDecode(t, rec)
	if got := utf8.RuneCountInString(body["content"].(string)); got != 9625 {
		t.Fatalf("content runes = %d, want 9625", got)
	}
	if body["template"].(map[string]any)["title"] != "AI 视频生成工作空间" {
		t.Fatalf("template = %v", body["template"])
	}

	// 多段路径（skill 模板）也要能取到。
	rec = draftRequest(t, r, http.MethodGet, "/api/v1/prompts/registry/.claude/skills/compose-video/SKILL.md", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("skill detail: %d %s", rec.Code, rec.Body.String())
	}

	// 未知路径与穿越输入 → 404 prompt_template_not_found。
	for _, path := range []string{
		"/api/v1/prompts/registry/CLAUDE.nope.md",
		"/api/v1/prompts/registry/../etc/passwd",
	} {
		rec = draftRequest(t, r, http.MethodGet, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		if detail := draftDecode(t, rec)["detail"]; detail != "提示词模板不存在" {
			t.Fatalf("%s detail = %v", path, detail)
		}
	}
}
