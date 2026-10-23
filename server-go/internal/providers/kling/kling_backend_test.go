package kling

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shotwise/shotwise/internal/provider"
)

// TestKlingJWTSignsHS256 校验 JWT 头/载荷/签名与固定时钟下的 exp/nbf。
func TestKlingJWTSignsHS256(t *testing.T) {
	fixed := time.Unix(1_700_000_000, 0)
	mgr := newKlingJWTManager("ak-test", "sk-test")
	mgr.now = func() time.Time { return fixed }

	token := mgr.token()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token should have 3 segments, got %q", token)
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || string(header) != `{"alg":"HS256","typ":"JWT"}` {
		t.Fatalf("unexpected header: %q (%v)", header, err)
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims["iss"] != "ak-test" {
		t.Fatalf("unexpected iss: %v", claims["iss"])
	}
	if int64(claims["exp"].(float64)) != fixed.Unix()+klingTokenTTLSeconds {
		t.Fatalf("unexpected exp: %v", claims["exp"])
	}
	if int64(claims["nbf"].(float64)) != fixed.Unix()-klingTokenNBFSkewSeconds {
		t.Fatalf("unexpected nbf: %v", claims["nbf"])
	}
	mac := hmac.New(sha256.New, []byte("sk-test"))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if parts[2] != want {
		t.Fatalf("signature mismatch: got %q want %q", parts[2], want)
	}
}

// TestKlingJWTRefreshesNearExpiry 校验距过期 >60s 复用缓存，跨过窗口后按需重签。
func TestKlingJWTRefreshesNearExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	mgr := newKlingJWTManager("ak", "sk")
	mgr.now = func() time.Time { return now }

	first := mgr.token()
	now = now.Add(100 * time.Second)
	if got := mgr.token(); got != first {
		t.Fatal("token within refresh margin should be cached")
	}
	now = now.Add(1750 * time.Second)
	if got := mgr.token(); got == first {
		t.Fatal("token past refresh margin should be re-minted")
	}
}

// TestKlingImageGenerateHappyPath 走 JWT 模式，断言 URL 路径、JWT 鉴权头与关键 payload 字段。
func TestKlingImageGenerateHappyPath(t *testing.T) {
	var base string
	var gotPath, gotAuth string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/images/generations":
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &payload)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"task_id": "task-1"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/images/generations/task-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"task_status": "succeed",
					"task_result": map[string]any{"images": []map[string]any{{"url": base + "/img.png"}}},
				},
			})
		case r.URL.Path == "/img.png":
			_, _ = w.Write([]byte("PNGDATA"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	base = srv.URL

	backend, err := NewImageBackend(provider.BackendOptions{
		Model:   "kling-image-o1",
		BaseURL: srv.URL,
		Extra:   map[string]string{"access_key": "ak-test", "secret_key": "sk-test"},
	})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.png")
	res, err := backend.Generate(context.Background(), provider.ImageGenerationRequest{
		Prompt: "a cat", OutputPath: out, AspectRatio: "9:16",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotPath != "/v1/images/generations" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Fatalf("unexpected auth header: %q", gotAuth)
	}
	tokenClaims := strings.Split(strings.TrimPrefix(gotAuth, "Bearer "), ".")
	if len(tokenClaims) != 3 {
		t.Fatalf("auth header should carry a JWT, got %q", gotAuth)
	}
	claimsJSON, _ := base64.RawURLEncoding.DecodeString(tokenClaims[1])
	var claims map[string]any
	_ = json.Unmarshal(claimsJSON, &claims)
	if claims["iss"] != "ak-test" {
		t.Fatalf("jwt iss should be access_key, got %v", claims["iss"])
	}
	if payload["model_name"] != "kling-image-o1" || payload["prompt"] != "a cat" ||
		payload["aspect_ratio"] != "9:16" || payload["n"] != float64(1) {
		t.Fatalf("unexpected payload: %v", payload)
	}
	if _, hasResolution := payload["resolution"]; hasResolution {
		t.Fatalf("non-omni model must not send resolution: %v", payload)
	}
	content, err := os.ReadFile(out)
	if err != nil || string(content) != "PNGDATA" {
		t.Fatalf("output file not written: %q (%v)", content, err)
	}
	if res.Provider != "kling" || res.Model != "kling-image-o1" || res.ImageURI != base+"/img.png" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// TestKlingVideoGenerateHappyPath 走 Bearer 模式，断言旧版视频请求形态与 job_id 持久化。
func TestKlingVideoGenerateHappyPath(t *testing.T) {
	var base string
	var gotPath, gotAuth string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/videos/text2video":
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &payload)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"task_id": "video-1"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/text2video/video-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"task_status": "succeed",
					"task_result": map[string]any{"videos": []map[string]any{{"url": base + "/clip.mp4"}}},
				},
			})
		case r.URL.Path == "/clip.mp4":
			_, _ = w.Write([]byte("MP4DATA"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	base = srv.URL

	var persistedJobID string
	backend, err := NewVideoBackend(provider.BackendOptions{
		Model:   "kling-v2-5-turbo",
		APIKey:  "key-123",
		BaseURL: srv.URL,
		PersistJobID: func(_ context.Context, _, jobID string, _ *string) error {
			persistedJobID = jobID
			return nil
		},
	})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.mp4")
	req := provider.NewVideoGenerationRequest("do it", out)
	req.TaskID = "task-xyz"
	req.GenerateAudio = false
	res, err := backend.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotPath != "/v1/videos/text2video" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if gotAuth != "Bearer key-123" {
		t.Fatalf("unexpected auth header: %q", gotAuth)
	}
	if payload["model_name"] != "kling-v2-5-turbo" || payload["mode"] != "std" ||
		payload["duration"] != "5" || payload["aspect_ratio"] != "9:16" {
		t.Fatalf("unexpected payload: %v", payload)
	}
	if _, hasSound := payload["sound"]; hasSound {
		t.Fatalf("turbo model without audio capability must not send sound: %v", payload)
	}
	if persistedJobID != "text2video:video-1:0" {
		t.Fatalf("unexpected persisted job id: %q", persistedJobID)
	}
	if res.TaskID != "video-1" || res.Provider != "kling" || res.GenerateAudio == nil || *res.GenerateAudio {
		t.Fatalf("unexpected result: %+v", res)
	}
	content, err := os.ReadFile(out)
	if err != nil || string(content) != "MP4DATA" {
		t.Fatalf("output file not written: %q (%v)", content, err)
	}
}

// TestKlingVideoCapabilities 校验静态与档位感知的能力声明。
func TestKlingVideoCapabilities(t *testing.T) {
	staticCaps, err := VideoCapabilitiesForModel("kling-v2-5-turbo")
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if !staticCaps.FirstFrame || staticCaps.LastFrame {
		t.Fatalf("turbo static caps should be first_frame only: %+v", staticCaps)
	}
	v3Caps, _ := VideoCapabilitiesForModel("kling-v3")
	if !v3Caps.LastFrame {
		t.Fatalf("kling-v3 static caps should allow last_frame: %+v", v3Caps)
	}

	backend, err := NewVideoBackend(provider.BackendOptions{Model: "kling-v2-5-turbo", APIKey: "key"})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	if !backend.VideoCapabilitiesForTier("pro", "1080p").LastFrame {
		t.Fatal("pro tier should allow last_frame on turbo")
	}
	if backend.VideoCapabilitiesForTier("std", "720p").LastFrame {
		t.Fatal("std tier must not allow last_frame on turbo")
	}
	// 4k 档优先于 service_tier，不应被 pro 误判放行。
	if backend.VideoCapabilitiesForTier("pro", "4k").LastFrame {
		t.Fatal("4k tier must not allow last_frame on turbo")
	}
}

// TestKlingRegisteredInRegistry 校验注册键与档位感知接口实现。
func TestKlingRegisteredInRegistry(t *testing.T) {
	video, err := provider.CreateVideoBackend("kling", provider.BackendOptions{Model: "kling-v2-5-turbo", APIKey: "key"})
	if err != nil {
		t.Fatalf("create kling video backend: %v", err)
	}
	if _, ok := video.(provider.TierAwareVideoCapabilityProbe); !ok {
		t.Fatal("kling video backend should implement TierAwareVideoCapabilityProbe")
	}
	image, err := provider.CreateImageBackend("kling", provider.BackendOptions{Model: "kling-image-o1", APIKey: "key"})
	if err != nil {
		t.Fatalf("create kling image backend: %v", err)
	}
	if !image.Capabilities().Has(provider.CapTextToImage) || !image.Capabilities().Has(provider.CapImageToImage) {
		t.Fatalf("unexpected kling image capabilities: %s", image.Capabilities())
	}
}
