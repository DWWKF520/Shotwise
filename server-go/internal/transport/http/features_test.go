package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// featuresRouter 只挂放量诊断三端点（不依赖 DB）。
func featuresRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	v1 := r.Group("/api/v1")
	RegisterFeatures(v1, FeaturesDeps{})
	return r
}

func featuresGet(t *testing.T, r *gin.Engine, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
	}
	return w.Code, out
}

// TestFeatureFlagsSnapshot 校验公开状态只含布尔值，且默认值与 Python 一致。
func TestFeatureFlagsSnapshot(t *testing.T) {
	r := featuresRouter()
	status, body := featuresGet(t, r, "/api/v1/feature-flags")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	features, _ := body["features"].(map[string]any)
	if features["media_asset_index"] != false {
		t.Fatalf("media_asset_index 默认应关闭: %v", features)
	}
	if features["media_library"] != true || features["creation_plan"] != true {
		t.Fatalf("features = %v", features)
	}
}

// TestFeatureFlagsValidateMarksInvalidEnv 校验非法布尔值被标记且不回传原始值。
func TestFeatureFlagsValidateMarksInvalidEnv(t *testing.T) {
	t.Setenv("SHOTWISE_FEATURE_CREATION_PLAN", "maybe")
	r := featuresRouter()
	status, body := featuresGet(t, r, "/api/v1/feature-flags/validate")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["valid"] != false {
		t.Fatalf("非法取值应判无效: %v", body)
	}
	flags, _ := body["flags"].(map[string]any)
	plan, _ := flags["creation_plan"].(map[string]any)
	if plan["valid"] != false || plan["enabled"] != false {
		t.Fatalf("creation_plan = %v", plan)
	}
	// 诊断面不得出现原始 env 值
	raw, _ := json.Marshal(body)
	if string(raw) == "" || containsSub(string(raw), "maybe") {
		t.Fatalf("不得回传原始值: %s", raw)
	}
}

// TestFeatureFlagsValidateUnknownEnv 校验未知开关变量被报出来。
func TestFeatureFlagsValidateUnknownEnv(t *testing.T) {
	t.Setenv("SHOTWISE_FEATURE_TYPO", "on")
	r := featuresRouter()
	_, body := featuresGet(t, r, "/api/v1/feature-flags/validate")
	errs, _ := body["errors"].([]any)
	if len(errs) == 0 {
		t.Fatalf("应报未知变量: %v", body)
	}
}

// TestFeatureMetricsShape 校验诊断响应四块齐全（无创建计划服务时 compatibility 为空集）。
func TestFeatureMetricsShape(t *testing.T) {
	r := featuresRouter()
	status, body := featuresGet(t, r, "/api/v1/feature-flags/metrics")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	for _, key := range []string{"features", "configuration", "creation", "compatibility"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("缺 %s: %v", key, body)
		}
	}
}

// containsSub 是子串判断的小助手（避免为一个断言引入 strings 依赖链）。
func containsSub(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
