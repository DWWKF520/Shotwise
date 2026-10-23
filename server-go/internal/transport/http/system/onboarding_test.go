package system

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/migrations"
)

// adminFixture 起一个只有管理面路由的壳（引导状态 + API Key）。
type adminFixture struct {
	router *gin.Engine
}

func newAdminFixture(t *testing.T) adminFixture {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "t.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Migrate(gdb, dialect, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// api_keys.user_id 有外键约束，先落默认用户。
	if err := gdb.Exec(
		"INSERT INTO users (id, username, role, is_active, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		repository.DefaultUserID, "default", "user", true, "2026-01-01T00:00:00+00:00", "2026-01-01T00:00:00+00:00",
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
	v1 := r.Group("/api/v1")
	RegisterOnboarding(v1, OnboardingDeps{Config: config.NewConfigService(gdb)})
	// API Key 路由在真实装配里挂 RequireAdmin；这里直接注册以单独验契约。
	admin := v1.Group("")
	admin.POST("/api-keys", createAPIKey(APIKeyDeps{DB: gdb}))
	admin.GET("/api-keys", listAPIKeys(APIKeyDeps{DB: gdb}))
	admin.DELETE("/api-keys/:key_id", deleteAPIKey(APIKeyDeps{DB: gdb}))
	return adminFixture{router: r}
}

func (f adminFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
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

// TestOnboardingSeenRoundTrip 校验引导标记：初始未看过，标记后幂等返回 true。
func TestOnboardingSeenRoundTrip(t *testing.T) {
	f := newAdminFixture(t)
	status, body := f.do(t, http.MethodGet, "/api/v1/onboarding/status", nil)
	if status != http.StatusOK || body["seen"] != false {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if status, body = f.do(t, http.MethodPost, "/api/v1/onboarding/seen", nil); status != http.StatusOK || body["seen"] != true {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if status, body = f.do(t, http.MethodGet, "/api/v1/onboarding/status", nil); status != http.StatusOK || body["seen"] != true {
		t.Fatalf("status = %d body=%v", status, body)
	}
	// 幂等
	if status, body = f.do(t, http.MethodPost, "/api/v1/onboarding/seen", nil); status != http.StatusOK || body["seen"] != true {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

// TestAPIKeyLifecycle 校验创建（201 且返回完整 key 与前缀）、列表（不含 key）、删除（204）。
func TestAPIKeyLifecycle(t *testing.T) {
	f := newAdminFixture(t)
	status, created := f.do(t, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "ci"})
	if status != http.StatusCreated {
		t.Fatalf("status = %d body=%v", status, created)
	}
	fullKey, _ := created["key"].(string)
	if len(fullKey) == 0 || created["key_prefix"] == "" {
		t.Fatalf("created = %v", created)
	}
	// 默认 30 天有效期
	if created["expires_at"] == nil {
		t.Fatalf("默认应有有效期: %v", created)
	}

	// 列表直接返回数组（与 Python 一致）：单独取原文解析。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/api-keys", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list %q: %v", rec.Body.String(), err)
	}
	if len(rows) != 1 || rows[0]["name"] != "ci" {
		t.Fatalf("rows = %v", rows)
	}
	// 列表不得回传完整 key
	if _, leaked := rows[0]["key"]; leaked {
		t.Fatalf("列表不应包含完整 key: %v", rows[0])
	}

	// 删除
	id := created["id"].(float64)
	status, _ = f.do(t, http.MethodDelete, "/api/v1/api-keys/"+httpkit.ItoaInt(int(id)), nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete status = %d", status)
	}
	// 再删一次：404
	status, body := f.do(t, http.MethodDelete, "/api/v1/api-keys/"+httpkit.ItoaInt(int(id)), nil)
	if status != http.StatusNotFound {
		t.Fatalf("第二次删除应 404: status = %d body=%v", status, body)
	}
}

// TestAPIKeyNoExpiry 校验 expires_days=0 表示不过期。
func TestAPIKeyNoExpiry(t *testing.T) {
	f := newAdminFixture(t)
	status, created := f.do(t, http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "permanent", "expires_days": 0,
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d body=%v", status, created)
	}
	if created["expires_at"] != nil {
		t.Fatalf("expires_days=0 应不过期: %v", created["expires_at"])
	}
}

// TestAPIKeyRejectsNegativeExpiry 校验负数过期天数是 422。
func TestAPIKeyRejectsNegativeExpiry(t *testing.T) {
	f := newAdminFixture(t)
	status, body := f.do(t, http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "bad", "expires_days": -1,
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

var _ = repository.APIKey{}
