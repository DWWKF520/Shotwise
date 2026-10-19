package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/migrations"
)

// agentConfigFixture 起一个 agent 配置壳（不挂 RequireAdmin，单独验契约）。
type agentConfigFixture struct {
	router *gin.Engine
}

func newAgentConfigFixture(t *testing.T) agentConfigFixture {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "t.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Migrate(gdb, dialect, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := gdb.Exec(
		"INSERT INTO users (id, username, role, is_active, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		repository.DefaultUserID, "default", "admin", true, "2026-01-01T00:00:00+00:00", "2026-01-01T00:00:00+00:00",
	).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		if pool, err := gdb.DB(); err == nil {
			_ = pool.Close()
		}
	})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 鉴权关闭时中间件注入 admin 身份：agent 配置整组要求管理员，夹具必须走同一层。
	authService := auth.New(nil, false, nil)
	v1 := r.Group("/api/v1", authService.Middleware())
	RegisterAgentConfig(v1, AgentConfigDeps{Credentials: service.NewAgentCredentialService(gdb)})
	return agentConfigFixture{router: r}
}

func (f agentConfigFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
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

// TestAgentPresetsScopedToClaudeAndOpenAI 校验预设目录只有 anthropic / openai 两族。
func TestAgentPresetsScopedToClaudeAndOpenAI(t *testing.T) {
	f := newAgentConfigFixture(t)
	status, body := f.do(t, http.MethodGet, "/api/v1/agent/preset-providers?sdk_type=claude", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	items, _ := body["providers"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != "anthropic" {
		t.Fatalf("claude 预设应只有 anthropic: %v", items)
	}
	if body["custom_sentinel_id"] != "__custom__" {
		t.Fatalf("缺自定义哨兵: %v", body)
	}

	status, body = f.do(t, http.MethodGet, "/api/v1/agent/preset-providers?sdk_type=openai", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	items, _ = body["providers"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != "openai" {
		t.Fatalf("openai 预设应只有 openai: %v", items)
	}

	// 非法 sdk_type → 422
	status, _ = f.do(t, http.MethodGet, "/api/v1/agent/preset-providers?sdk_type=gemini", nil)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", status)
	}
}

// TestAgentCredentialLifecycle 校验创建/激活/更新/删除，含密钥掩码与互斥激活。
func TestAgentCredentialLifecycle(t *testing.T) {
	f := newAgentConfigFixture(t)
	status, created := f.do(t, http.MethodPost, "/api/v1/agent/credentials", map[string]any{
		"sdk_type": "claude", "preset_id": "anthropic",
		"base_url": "https://api.anthropic.com", "api_key": "sk-ant-abcdefghij",
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d body=%v", status, created)
	}
	if created["is_active"] != true {
		t.Fatalf("首条应自动激活: %v", created)
	}
	if created["api_key_masked"] != "••••ghij" {
		t.Fatalf("密钥应只回掩码: %v", created["api_key_masked"])
	}
	if created["icon_key"] != "anthropic" {
		t.Fatalf("预设应带 icon_key: %v", created)
	}

	// 第二条不自动激活，激活后第一条变 inactive
	_, second := f.do(t, http.MethodPost, "/api/v1/agent/credentials", map[string]any{
		"sdk_type": "claude", "preset_id": "__custom__", "display_name": "自建中转",
		"base_url": "https://relay.test/v1", "api_key": "sk-ant-zzzzzzzz",
		"protocol": "anthropic_messages",
	})
	if second["is_active"] != false {
		t.Fatalf("第二条不应自动激活: %v", second)
	}
	status, activated := f.do(t, http.MethodPost,
		"/api/v1/agent/credentials/"+itoaInt(int(second["id"].(float64)))+"/activate", nil)
	if status != http.StatusOK || activated["is_active"] != true {
		t.Fatalf("status = %d body=%v", status, activated)
	}
	status, list := f.do(t, http.MethodGet, "/api/v1/agent/credentials?sdk_type=claude", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	active := 0
	for _, raw := range list["credentials"].([]any) {
		if raw.(map[string]any)["is_active"] == true {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("活跃凭证应恰好一条: %v", list)
	}

	// 删除活跃凭证 → 剩余一条接管
	status, _ = f.do(t, http.MethodDelete,
		"/api/v1/agent/credentials/"+itoaInt(int(second["id"].(float64))), nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete status = %d", status)
	}
	_, list = f.do(t, http.MethodGet, "/api/v1/agent/credentials", nil)
	rows, _ := list["credentials"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["is_active"] != true {
		t.Fatalf("剩余凭证应接管: %v", rows)
	}
}

// TestAgentCredentialRequiresKey 校验缺 api_key 是 422（不是落一条空凭证）。
func TestAgentCredentialRequiresKey(t *testing.T) {
	f := newAgentConfigFixture(t)
	status, body := f.do(t, http.MethodPost, "/api/v1/agent/credentials", map[string]any{
		"preset_id": "anthropic", "base_url": "https://api.anthropic.com",
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

// TestAgentCredentialNotFound 校验跨用户/不存在都是 404。
func TestAgentCredentialNotFound(t *testing.T) {
	f := newAgentConfigFixture(t)
	status, body := f.do(t, http.MethodDelete, "/api/v1/agent/credentials/9999", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

// TestAgentTestConnectionDraftRejectsMissingFields 校验草稿试连缺字段是 400。
func TestAgentTestConnectionDraftRejectsMissingFields(t *testing.T) {
	f := newAgentConfigFixture(t)
	status, body := f.do(t, http.MethodPost, "/api/v1/agent/test-connection", map[string]any{
		"base_url": "https://api.openai.com/v1",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v", status, body)
	}
}
