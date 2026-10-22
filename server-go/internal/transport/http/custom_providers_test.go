package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/i18n"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service/admin"
	"github.com/shotwise/shotwise/migrations"
)

// customProviderFixture 起一个自定义供应商路由壳。
//
// authed=true 时挂真实鉴权中间件（认证关闭 → 注入 admin 身份）；authed=false 时完全
// 不挂，中间件上下文里没有用户——RequireAdmin 正是据此返回 403，用来验证管理员闸的覆盖面。
type customProviderFixture struct {
	router *gin.Engine
	db     *gorm.DB
}

func newCustomProviderFixture(t *testing.T, authed bool) customProviderFixture {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "t.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Migrate(gdb, dialect, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if pool, err := gdb.DB(); err == nil {
			_ = pool.Close()
		}
	})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	v1 := r.Group("/api/v1")
	if authed {
		v1.Use(auth.New(nil, false, nil).Middleware())
	}
	RegisterCustomProviders(v1, CustomProviderDeps{
		Providers: admin.NewCustomProviderService(gdb, nil, nil),
	})
	return customProviderFixture{router: r, db: gdb}
}

func (f customProviderFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var payload []byte
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		payload = raw
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

// doRaw 不做 JSON 解码，用于响应是裸数组或空体的路径。
func (f customProviderFixture) doRaw(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var payload []byte
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		payload = raw
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

func customCreatePayload() map[string]any {
	return map[string]any{
		"display_name":     "中转站",
		"discovery_format": "openai",
		"base_url":         "https://example.com/v1",
		"api_key":          "sk-secret-value-1234",
		"models": []any{map[string]any{
			"model_id":            "sora-2",
			"display_name":        "sora-2",
			"endpoint":            "openai-video",
			"is_enabled":          true,
			"supported_durations": []int{5, 10},
		}},
	}
}

// ── 路由表 ───────────────────────────────────────────────────────────────

// TestCustomProviderRoutesRegister 覆盖 gin 的静态段 / 参数段共存：
// /custom-providers/endpoints 与 /custom-providers/:provider_id 同深度，注册冲突会 panic。
func TestCustomProviderRoutesRegister(t *testing.T) {
	r := newCustomProviderFixture(t, true).router
	want := []string{
		"GET /api/v1/custom-providers",
		"GET /api/v1/custom-providers/endpoints",
		"POST /api/v1/custom-providers",
		"GET /api/v1/custom-providers/:provider_id",
		"PATCH /api/v1/custom-providers/:provider_id",
		"PUT /api/v1/custom-providers/:provider_id",
		"PUT /api/v1/custom-providers/:provider_id/models",
		"DELETE /api/v1/custom-providers/:provider_id",
		"GET /api/v1/custom-providers/:provider_id/credentials",
	}
	got := make(map[string]bool)
	for _, route := range r.Routes() {
		got[route.Method+" "+route.Path] = true
	}
	for _, route := range want {
		if !got[route] {
			t.Errorf("route not registered: %s", route)
		}
	}
}

func TestCustomProviderEndpointCatalogIsNotTreatedAsProviderID(t *testing.T) {
	f := newCustomProviderFixture(t, true)
	status, body := f.do(t, http.MethodGet, "/api/v1/custom-providers/endpoints", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	endpoints, _ := body["endpoints"].([]any)
	if len(endpoints) == 0 {
		t.Fatal("expected non-empty endpoint catalog")
	}
	first, _ := endpoints[0].(map[string]any)
	for _, field := range []string{"key", "media_type", "family", "request_method", "image_capabilities"} {
		if _, ok := first[field]; !ok {
			t.Fatalf("catalog entry missing %q: %v", field, first)
		}
	}
}

// ── CRUD 契约 ────────────────────────────────────────────────────────────

func TestCustomProviderHTTPFullLifecycle(t *testing.T) {
	f := newCustomProviderFixture(t, true)

	// POST → 201 + 完整投影
	status, created := f.do(t, http.MethodPost, "/api/v1/custom-providers", customCreatePayload())
	if status != http.StatusCreated {
		t.Fatalf("create status = %d body=%v", status, created)
	}
	id := int(created["id"].(float64))
	if created["api_key_masked"] == "sk-secret-value-1234" {
		t.Fatal("api key leaked in response")
	}
	if created["discovery_format"] != "openai" {
		t.Fatalf("unexpected discovery_format: %v", created["discovery_format"])
	}
	if created["is_enabled"] != true {
		t.Fatalf("is_enabled should default to true: %v", created["is_enabled"])
	}
	models, _ := created["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %v", models)
	}
	first, _ := models[0].(map[string]any)
	durations, _ := first["supported_durations"].([]any)
	if len(durations) != 2 || durations[0].(float64) != 5 {
		t.Fatalf("durations round-trip failed: %v", first["supported_durations"])
	}
	if first["system_capabilities"] == nil {
		t.Fatalf("expected system_capabilities for video model: %v", first)
	}
	if first["global_bucket_refs"] != nil {
		t.Fatalf("expected null bucket refs for unreferenced model: %v", first["global_bucket_refs"])
	}

	// GET /{id}
	status, got := f.do(t, http.MethodGet, "/api/v1/custom-providers/1", nil)
	if status != http.StatusOK {
		t.Fatalf("get status = %d body=%v", status, got)
	}
	if got["id"].(float64) != float64(id) {
		t.Fatalf("unexpected id %v", got["id"])
	}

	// GET / → {providers: [...]}
	status, list := f.do(t, http.MethodGet, "/api/v1/custom-providers", nil)
	if status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	if providers, _ := list["providers"].([]any); len(providers) != 1 {
		t.Fatalf("expected 1 provider, got %v", list["providers"])
	}

	// PATCH → 只改被提供的字段
	status, patched := f.do(t, http.MethodPatch, "/api/v1/custom-providers/1",
		map[string]any{"display_name": "改名"})
	if status != http.StatusOK {
		t.Fatalf("patch status = %d body=%v", status, patched)
	}
	if patched["display_name"] != "改名" {
		t.Fatalf("unexpected display_name %v", patched["display_name"])
	}
	if patched["base_url"] != "https://example.com/v1" {
		t.Fatalf("patch must not clear base_url: %v", patched["base_url"])
	}

	// PUT /{id}/models → 裸数组，整体替换
	_, raw := f.doRaw(t, http.MethodPut, "/api/v1/custom-providers/1/models", map[string]any{
		"models": []any{
			map[string]any{
				"model_id": "sora-2", "display_name": "sora-2", "endpoint": "openai-video",
				"is_enabled": true, "supported_durations": []int{5},
			},
			map[string]any{
				"model_id": "sora-2-pro", "display_name": "pro", "endpoint": "openai-video",
				"is_enabled": true, "supported_durations": []int{10},
			},
		},
	})
	var replaced []map[string]any
	if err := json.Unmarshal(raw, &replaced); err != nil {
		t.Fatalf("replace models must return a bare array, got %s: %v", raw, err)
	}
	if len(replaced) != 2 {
		t.Fatalf("expected 2 models, got %d", len(replaced))
	}

	// DELETE → 204 且此后 GET 404
	status, _ = f.do(t, http.MethodDelete, "/api/v1/custom-providers/1", nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete status = %d", status)
	}
	status, missing := f.do(t, http.MethodGet, "/api/v1/custom-providers/1", nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d body=%v", status, missing)
	}
	// detail 走 i18n 渲染（与 Python 侧 HTTPException(detail=_t(...)) 同款），
	// 故按渲染结果断言而不是比对 key 字面量。
	if missing["detail"] != i18n.T(i18n.ZH, "provider_not_found", nil) {
		t.Fatalf("unexpected detail %v", missing["detail"])
	}
}

func TestCustomProviderCredentialsEndpoint(t *testing.T) {
	f := newCustomProviderFixture(t, true)
	if status, body := f.do(t, http.MethodPost, "/api/v1/custom-providers", customCreatePayload()); status != http.StatusCreated {
		t.Fatalf("create status = %d body=%v", status, body)
	}
	status, creds := f.do(t, http.MethodGet, "/api/v1/custom-providers/1/credentials", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, creds)
	}
	if creds["api_key"] != "sk-secret-value-1234" {
		t.Fatalf("expected plaintext key, got %v", creds["api_key"])
	}
	if creds["base_url"] != "https://example.com/v1" {
		t.Fatalf("unexpected base_url %v", creds["base_url"])
	}
}

func TestCustomProviderValidationErrorBecomes422(t *testing.T) {
	f := newCustomProviderFixture(t, true)
	payload := customCreatePayload()
	models, _ := payload["models"].([]any)
	models[0].(map[string]any)["supported_durations"] = nil // 视频模型必须声明时长

	status, body := f.do(t, http.MethodPost, "/api/v1/custom-providers", payload)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d body=%v", status, body)
	}
	detail, _ := body["detail"].(string)
	if detail == "" {
		t.Fatalf("expected translated detail, got %v", body["detail"])
	}
}

func TestCustomProviderInvalidPathParamBecomes422(t *testing.T) {
	f := newCustomProviderFixture(t, true)
	status, body := f.do(t, http.MethodGet, "/api/v1/custom-providers/not-a-number", nil)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d body=%v", status, body)
	}
}

// ── 管理员闸 ─────────────────────────────────────────────────────────────

// TestCustomProviderAdminGateScope 验证闸只覆盖删除与明文凭证读取，不误伤普通读写。
func TestCustomProviderAdminGateScope(t *testing.T) {
	f := newCustomProviderFixture(t, false) // 无鉴权中间件 → 上下文里没有用户
	if status, body := f.do(t, http.MethodPost, "/api/v1/custom-providers", customCreatePayload()); status != http.StatusCreated {
		t.Fatalf("create status = %d body=%v", status, body)
	}
	cases := []struct {
		name   string
		method string
		path   string
		gated  bool
	}{
		{"list", http.MethodGet, "/api/v1/custom-providers", false},
		{"get", http.MethodGet, "/api/v1/custom-providers/1", false},
		{"endpoints", http.MethodGet, "/api/v1/custom-providers/endpoints", false},
		{"patch", http.MethodPatch, "/api/v1/custom-providers/1", false},
		{"replace models", http.MethodPut, "/api/v1/custom-providers/1/models", false},
		{"credentials", http.MethodGet, "/api/v1/custom-providers/1/credentials", true},
		{"delete", http.MethodDelete, "/api/v1/custom-providers/1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := f.do(t, tc.method, tc.path, map[string]any{"display_name": "x", "models": []any{}})
			if tc.gated {
				if status != http.StatusForbidden {
					t.Fatalf("expected 403 for %s %s, got %d body=%v", tc.method, tc.path, status, body)
				}
				return
			}
			if status == http.StatusForbidden {
				t.Fatalf("%s %s must not require admin", tc.method, tc.path)
			}
		})
	}
}

// TestCustomProviderDeleteSucceedsForAdmin 与上一个用例互补：挂上鉴权（注入 admin）后
// 删除放行。
func TestCustomProviderDeleteSucceedsForAdmin(t *testing.T) {
	f := newCustomProviderFixture(t, true)
	if status, body := f.do(t, http.MethodPost, "/api/v1/custom-providers", customCreatePayload()); status != http.StatusCreated {
		t.Fatalf("create status = %d body=%v", status, body)
	}
	if status, _ := f.do(t, http.MethodDelete, "/api/v1/custom-providers/1", nil); status != http.StatusNoContent {
		t.Fatalf("admin delete status = %d", status)
	}
	var count int64
	if err := f.db.Model(&repository.CustomProviderModel{}).Count(&count).Error; err != nil {
		t.Fatalf("count models: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected cascaded model deletion, %d remain", count)
	}
}
