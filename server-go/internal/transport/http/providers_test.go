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

	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/migrations"
)

// providerFixture 起一个带真实 sqlite 的供应商管理壳。
type providerFixture struct {
	router *gin.Engine
	db     *gorm.DB
}

func newProviderFixture(t *testing.T) providerFixture {
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
	RegisterProviders(v1, ProviderDeps{Providers: service.NewProviderAdminService(gdb)})
	return providerFixture{router: r, db: gdb}
}

func (f providerFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
	}
	return w.Code, out
}

// TestProviderListShowsScopedProviders 校验目录来自注册表且初始全部未配置。
func TestProviderListShowsScopedProviders(t *testing.T) {
	f := newProviderFixture(t)
	status, body := f.do(t, http.MethodGet, "/api/v1/providers", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	items, _ := body["providers"].([]any)
	if len(items) == 0 {
		t.Fatalf("目录为空: %v", body)
	}
	// 用户明确关心的四家必须在目录里：openai（文本/图像/视频）、ark（seedance/seedream）。
	found := map[string]map[string]any{}
	for _, raw := range items {
		item := raw.(map[string]any)
		found[item["id"].(string)] = item
	}
	for _, id := range []string{"openai", "ark"} {
		if _, ok := found[id]; !ok {
			t.Fatalf("缺 %s: %v", id, found)
		}
	}
	openai := found["openai"]
	if openai["status"] != "unconfigured" || openai["enabled"] != true {
		t.Fatalf("openai = %v", openai)
	}
	models, _ := openai["models"].(map[string]any)
	if len(models) == 0 {
		t.Fatalf("openai 应带模型清单: %v", openai)
	}
}

// TestProviderCredentialLifecycle 校验凭证 CRUD：首条自动激活 → ready；密钥只回掩码；
// 删除活跃凭证后由剩余一条接管。
func TestProviderCredentialLifecycle(t *testing.T) {
	f := newProviderFixture(t)
	status, created := f.do(t, http.MethodPost, "/api/v1/providers/openai/credentials", map[string]any{
		"name": "main", "api_key": "sk-test-abcdefgh",
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d body=%v", status, created)
	}
	if created["is_active"] != true {
		t.Fatalf("首条凭证应自动激活: %v", created)
	}
	if created["api_key_masked"] != "••••efgh" {
		t.Fatalf("密钥应只回掩码: %v", created)
	}
	if _, leaked := created["api_key"]; leaked {
		t.Fatalf("不得回传明文密钥: %v", created)
	}

	// 状态变成 ready
	status, list := f.do(t, http.MethodGet, "/api/v1/providers", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	for _, raw := range list["providers"].([]any) {
		item := raw.(map[string]any)
		if item["id"] == "openai" {
			if item["status"] != "ready" || len(item["missing_keys"].([]any)) != 0 {
				t.Fatalf("openai 应 ready: %v", item)
			}
		}
	}

	// 第二条凭证不自动激活
	status, second := f.do(t, http.MethodPost, "/api/v1/providers/openai/credentials", map[string]any{
		"name": "backup", "api_key": "sk-test-ijklmnop",
	})
	if status != http.StatusCreated || second["is_active"] != false {
		t.Fatalf("status = %d body=%v", status, second)
	}

	// 删除活跃凭证 → 剩余一条接管
	firstID := created["id"].(float64)
	status, _ = f.do(t, http.MethodDelete, "/api/v1/providers/openai/credentials/"+itoaInt(int(firstID)), nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete status = %d", status)
	}
	status, creds := f.do(t, http.MethodGet, "/api/v1/providers/openai/credentials", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	rows, _ := creds["credentials"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	if rows[0].(map[string]any)["is_active"] != true {
		t.Fatalf("剩余凭证应接管活跃位: %v", rows[0])
	}
}

// TestProviderActivateCredential 校验激活切换：只有一条活跃。
func TestProviderActivateCredential(t *testing.T) {
	f := newProviderFixture(t)
	_, first := f.do(t, http.MethodPost, "/api/v1/providers/ark/credentials", map[string]any{
		"name": "a", "api_key": "key-1111",
	})
	_, second := f.do(t, http.MethodPost, "/api/v1/providers/ark/credentials", map[string]any{
		"name": "b", "api_key": "key-2222",
	})
	status, _ := f.do(t, http.MethodPost,
		"/api/v1/providers/ark/credentials/"+itoaInt(int(second["id"].(float64)))+"/activate", nil)
	if status != http.StatusNoContent {
		t.Fatalf("activate status = %d", status)
	}
	_, creds := f.do(t, http.MethodGet, "/api/v1/providers/ark/credentials", nil)
	active := 0
	for _, raw := range creds["credentials"].([]any) {
		if raw.(map[string]any)["is_active"] == true {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("活跃凭证应恰好一条: %v", creds)
	}
	_ = first
}

// TestProviderConfigPatchesOptionalKeysOnly 校验配置写只接受非密钥的可选键。
func TestProviderConfigPatchesOptionalKeysOnly(t *testing.T) {
	f := newProviderFixture(t)
	status, _ := f.do(t, http.MethodPatch, "/api/v1/providers/openai/config", map[string]any{
		"base_url": "https://example.test/v1",
	})
	if status != http.StatusNoContent {
		t.Fatalf("status = %d", status)
	}
	_, config := f.do(t, http.MethodGet, "/api/v1/providers/openai/config", nil)
	fields, _ := config["fields"].([]any)
	sawBaseURL := false
	for _, raw := range fields {
		field := raw.(map[string]any)
		if field["key"] == "base_url" {
			sawBaseURL = true
			if field["is_set"] != true {
				t.Fatalf("base_url 应已设置: %v", field)
			}
		}
	}
	if !sawBaseURL {
		t.Fatalf("缺 base_url 字段: %v", config)
	}

	// 密钥不能走 config：必须走凭证端点
	status, body := f.do(t, http.MethodPatch, "/api/v1/providers/openai/config", map[string]any{
		"api_key": "sk-should-not-land-here",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("密钥应被拒: status = %d body=%v", status, body)
	}
}

// TestProviderEnabledToggle 校验启用开关落库并影响目录。
func TestProviderEnabledToggle(t *testing.T) {
	f := newProviderFixture(t)
	status, _ := f.do(t, http.MethodPatch, "/api/v1/providers/openai/enabled", map[string]any{"enabled": false})
	if status != http.StatusNoContent {
		t.Fatalf("status = %d", status)
	}
	_, list := f.do(t, http.MethodGet, "/api/v1/providers", nil)
	for _, raw := range list["providers"].([]any) {
		item := raw.(map[string]any)
		if item["id"] == "openai" && item["enabled"] != false {
			t.Fatalf("openai 应已关闭: %v", item)
		}
	}
}

// TestProviderUnknownID 校验未登记供应商是 404 unknown_provider。
func TestProviderUnknownID(t *testing.T) {
	f := newProviderFixture(t)
	status, body := f.do(t, http.MethodGet, "/api/v1/providers/nope/config", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

// TestProviderTestConnectionWithoutKey 校验未配置密钥时测试返回失败原因而不是 5xx。
func TestProviderTestConnectionWithoutKey(t *testing.T) {
	f := newProviderFixture(t)
	status, body := f.do(t, http.MethodPost, "/api/v1/providers/openai/test", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["success"] != false {
		t.Fatalf("未配置密钥不应通过: %v", body)
	}
}

// TestProviderModelTypesOverrides 校验模型类型覆盖只接受注册表内的模型。
func TestProviderModelTypesOverrides(t *testing.T) {
	f := newProviderFixture(t)
	status, body := f.do(t, http.MethodPatch, "/api/v1/providers/openai/model-types", map[string]any{
		"not-a-model": "image",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("未登记模型应被拒: status = %d body=%v", status, body)
	}
}
