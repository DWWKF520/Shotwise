// 系统配置端点测试。
package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/service/admin"
	"github.com/shotwise/shotwise/migrations"
	"gorm.io/gorm/logger"
)

func newSystemConfigFixture(t *testing.T) *gin.Engine {
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
	// 生产注册里 GET 无鉴权、PATCH 管理员限定；夹具统一过鉴权中间件
	// （单用户模式注入 admin 身份），让 PATCH 的管理员分支可达。
	authService := auth.New(nil, false, nil)
	RegisterSystemConfig(r.Group("/api/v1", authService.Middleware()), SystemConfigDeps{
		Config: admin.NewSystemConfigService(gdb, config.NewConfigService(gdb)),
	})
	return r
}

func sysConfigCall(t *testing.T, r *gin.Engine, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(payload)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

// TestSystemConfigGetDefaults 校验默认值形态（narration_speed null、布尔默认、整数默认）。
func TestSystemConfigGetDefaults(t *testing.T) {
	r := newSystemConfigFixture(t)
	status, body := sysConfigCall(t, r, http.MethodGet, "/api/v1/system/config", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	settings := body["settings"].(map[string]any)
	if _, set := settings["narration_speed"]; !set {
		t.Fatalf("narration_speed 键缺失: %v", settings)
	}
	if settings["narration_speed"] != nil {
		t.Fatalf("narration_speed 默认应为 null: %v", settings["narration_speed"])
	}
	if settings["video_generate_audio"] != true {
		t.Fatalf("video_generate_audio 默认应为 true: %v", settings["video_generate_audio"])
	}
	if settings["agent_session_cleanup_delay_seconds"] != float64(300) {
		t.Fatalf("cleanup 默认应为 300: %v", settings["agent_session_cleanup_delay_seconds"])
	}
	key := settings["anthropic_api_key"].(map[string]any)
	if key["is_set"] != false || key["masked"] != nil {
		t.Fatalf("anthropic key 默认视图: %v", key)
	}
	options := body["options"].(map[string]any)
	for _, listKey := range []string{"video_backends", "image_backends", "text_backends", "audio_backends"} {
		if _, ok := options[listKey]; !ok {
			t.Fatalf("options 缺 %s: %v", listKey, options)
		}
	}
}

// TestSystemConfigPatchValidation 校验语速卫生校验、整数范围、未知字段拒绝。
func TestSystemConfigPatchValidation(t *testing.T) {
	r := newSystemConfigFixture(t)

	// 语速 0 / 负数 → 422
	status, _ := sysConfigCall(t, r, http.MethodPatch, "/api/v1/system/config",
		map[string]any{"narration_speed": 0})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("zero speed status = %d", status)
	}
	status, _ = sysConfigCall(t, r, http.MethodPatch, "/api/v1/system/config",
		map[string]any{"narration_speed": -1.5})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("negative speed status = %d", status)
	}

	// 整数越界 → 422
	status, _ = sysConfigCall(t, r, http.MethodPatch, "/api/v1/system/config",
		map[string]any{"agent_max_concurrent_sessions": 99})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("sessions range status = %d", status)
	}

	// 未知字段 → 422
	status, _ = sysConfigCall(t, r, http.MethodPatch, "/api/v1/system/config",
		map[string]any{"totally_unknown": 1})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("unknown field status = %d", status)
	}

	// 合法补丁：显式置空 + 布尔
	status, body := sysConfigCall(t, r, http.MethodPatch, "/api/v1/system/config", map[string]any{
		"narration_speed":      1.25,
		"video_generate_audio": false,
		"default_text_backend": "openai/gpt-4o-mini",
	})
	if status != http.StatusOK {
		t.Fatalf("patch status = %d body=%v", status, body)
	}
	settings := body["settings"].(map[string]any)
	if settings["narration_speed"] != float64(1.25) {
		t.Fatalf("narration_speed = %v", settings["narration_speed"])
	}
	if settings["video_generate_audio"] != false {
		t.Fatalf("video_generate_audio = %v", settings["video_generate_audio"])
	}
	if settings["default_text_backend"] != "openai/gpt-4o-mini" {
		t.Fatalf("default_text_backend = %v", settings["default_text_backend"])
	}
}

// TestModelCandidatesShape 校验候选响应四块结构（无 ready 供应商时全空但不缺键）。
func TestModelCandidatesShape(t *testing.T) {
	r := newSystemConfigFixture(t)
	status, body := sysConfigCall(t, r, http.MethodGet, "/api/v1/system/config/model-candidates", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	for _, key := range []string{"image", "video", "provider_names"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("缺 %s: %v", key, body)
		}
	}
	image := body["image"].(map[string]any)
	if _, ok := image["default"]; !ok {
		t.Fatalf("image 缺 default: %v", image)
	}
	buckets, _ := image["buckets"].(map[string]any)
	for _, bucket := range []string{"t2i", "i2i"} {
		if _, ok := buckets[bucket]; !ok {
			t.Fatalf("image 缺桶 %s: %v", bucket, buckets)
		}
	}
}

// TestSystemVersionShape 校验版本端点结构（GitHub 检查失败也不 500）。
func TestSystemVersionShape(t *testing.T) {
	r := newSystemConfigFixture(t)
	status, body := sysConfigCall(t, r, http.MethodGet, "/api/v1/system/version", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	current, _ := body["current"].(map[string]any)
	if current["version"] == "" {
		t.Fatalf("缺 current.version: %v", body)
	}
	if _, ok := body["has_update"]; !ok {
		t.Fatalf("缺 has_update: %v", body)
	}
}
