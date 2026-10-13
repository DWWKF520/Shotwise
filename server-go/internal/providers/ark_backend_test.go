package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/shotwise/shotwise/internal/provider"
)

// newArkTestServer 起一个记录请求的假 Ark 接口。
func newArkTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	return srv
}

// decodeArkBody 读取并解析请求体。
func decodeArkBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode body: %v (%s)", err, string(body))
	}
	return parsed
}

func TestArkImageGenerateTextToImage(t *testing.T) {
	var gotPath, gotAuth string
	var payload map[string]any
	srv := newArkTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		payload = decodeArkBody(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"b64_json": pngB64()}}})
	})

	backend := newArkImageBackend(provider.BackendOptions{APIKey: "sk-test", BaseURL: srv.URL})
	out := filepath.Join(t.TempDir(), "out.png")
	res, err := backend.Generate(context.Background(), provider.ImageGenerationRequest{
		Prompt: "a cat", OutputPath: out, AspectRatio: "16:9",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotPath != "/images/generations" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("unexpected auth header: %q", gotAuth)
	}
	if payload["model"] != arkDefaultImageModel {
		t.Fatalf("unexpected model: %v", payload["model"])
	}
	// 默认走 4.x/5.x 的 2K 表：16:9 → 2848x1600。
	if payload["size"] != "2848x1600" {
		t.Fatalf("unexpected size: %v", payload["size"])
	}
	if res.Provider != arkProvider || res.Model != arkDefaultImageModel {
		t.Fatalf("unexpected identity: %s/%s", res.Provider, res.Model)
	}
	if info, err := os.Stat(out); err != nil || info.Size() == 0 {
		t.Fatalf("output missing: %v", err)
	}
}

func TestArkImageGenerateImageToImage(t *testing.T) {
	var payload map[string]any
	srv := newArkTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		payload = decodeArkBody(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"b64_json": pngB64()}}})
	})

	dir := t.TempDir()
	ref1 := filepath.Join(dir, "r1.png")
	ref2 := filepath.Join(dir, "r2.png")
	for _, p := range []string{ref1, ref2} {
		if err := os.WriteFile(p, []byte("fake"), 0o644); err != nil {
			t.Fatalf("write ref: %v", err)
		}
	}

	backend := newArkImageBackend(provider.BackendOptions{APIKey: "sk", BaseURL: srv.URL})
	if _, err := backend.Generate(context.Background(), provider.ImageGenerationRequest{
		Prompt:          "restyle",
		OutputPath:      filepath.Join(dir, "out.png"),
		AspectRatio:     "1:1",
		ImageSize:       "2K",
		ReferenceImages: []provider.ReferenceImage{{Path: ref1}},
	}); err != nil {
		t.Fatalf("single ref generate: %v", err)
	}
	// 单张参考图传字符串。
	if _, ok := payload["image"].(string); !ok {
		t.Fatalf("single ref should be a string, got %T", payload["image"])
	}
	// 显式 image_size 优先于比例推导。
	if payload["size"] != "2K" {
		t.Fatalf("explicit image_size should win: %v", payload["size"])
	}

	if _, err := backend.Generate(context.Background(), provider.ImageGenerationRequest{
		Prompt:          "restyle",
		OutputPath:      filepath.Join(dir, "out2.png"),
		AspectRatio:     "1:1",
		ReferenceImages: []provider.ReferenceImage{{Path: ref1}, {Path: ref2}},
	}); err != nil {
		t.Fatalf("multi ref generate: %v", err)
	}
	// 多张参考图传列表。
	if arr, ok := payload["image"].([]any); !ok || len(arr) != 2 {
		t.Fatalf("multi ref should be a 2-element list, got %T %v", payload["image"], payload["image"])
	}
}

func TestArkAgentPlanVariantUsesProvidedBasePath(t *testing.T) {
	var gotPath string
	srv := newArkTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"b64_json": pngB64()}}})
	})

	// agent-plan 与 ark 共用实现，差异仅由传入的 base_url 体现（agent-plan 走 /api/plan/v3）。
	backend, err := provider.CreateImageBackend("ark-agent-plan", provider.BackendOptions{
		APIKey:  "sk",
		BaseURL: srv.URL + "/api/plan/v3",
	})
	if err != nil {
		t.Fatalf("create agent-plan backend: %v", err)
	}
	if _, err := backend.Generate(context.Background(), provider.NewImageGenerationRequest("a cat", filepath.Join(t.TempDir(), "o.png"))); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotPath != "/api/plan/v3/images/generations" {
		t.Fatalf("unexpected agent-plan path: %s", gotPath)
	}
}

func TestArkVideoGenerateHappyPath(t *testing.T) {
	var gotCreatePath, gotAuth, gotPollPath string
	var payload map[string]any
	var srv *httptest.Server
	srv = newArkTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/contents/generations/tasks":
			gotCreatePath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			payload = decodeArkBody(t, r)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "cgt-test-1"})
		case r.Method == http.MethodGet && r.URL.Path == "/contents/generations/tasks/cgt-test-1":
			gotPollPath = r.URL.Path
			// 直接返回 succeeded，避免测试等待轮询间隔。
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "succeeded",
				"content": map[string]any{"video_url": srv.URL + "/out.mp4"},
				"seed":    42,
				"usage":   map[string]any{"completion_tokens": 123},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/out.mp4":
			_, _ = w.Write([]byte("video-bytes"))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	backend := newArkVideoBackend(provider.BackendOptions{APIKey: "sk-v", BaseURL: srv.URL})
	out := filepath.Join(t.TempDir(), "out.mp4")
	req := provider.NewVideoGenerationRequest("a cat runs", out)
	req.Resolution = "720p"
	seed := int64(7)
	req.Seed = &seed

	res, err := backend.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotCreatePath != "/contents/generations/tasks" {
		t.Fatalf("unexpected create path: %s", gotCreatePath)
	}
	if gotPollPath != "/contents/generations/tasks/cgt-test-1" {
		t.Fatalf("unexpected poll path: %s", gotPollPath)
	}
	if gotAuth != "Bearer sk-v" {
		t.Fatalf("unexpected auth header: %q", gotAuth)
	}
	if payload["model"] != arkDefaultVideoModel {
		t.Fatalf("unexpected model: %v", payload["model"])
	}
	if payload["ratio"] != "9:16" || payload["duration"] != float64(5) {
		t.Fatalf("unexpected ratio/duration: %v/%v", payload["ratio"], payload["duration"])
	}
	if payload["generate_audio"] != true || payload["watermark"] != false {
		t.Fatalf("unexpected audio/watermark: %v/%v", payload["generate_audio"], payload["watermark"])
	}
	if payload["resolution"] != "720p" || payload["service_tier"] != "default" {
		t.Fatalf("unexpected resolution/tier: %v/%v", payload["resolution"], payload["service_tier"])
	}
	if payload["seed"] != float64(7) {
		t.Fatalf("unexpected seed: %v", payload["seed"])
	}

	if res.Provider != arkProvider || res.Model != arkDefaultVideoModel {
		t.Fatalf("unexpected identity: %s/%s", res.Provider, res.Model)
	}
	if res.DurationSeconds != 5 || res.VideoURI != srv.URL+"/out.mp4" || res.TaskID != "cgt-test-1" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Seed == nil || *res.Seed != 42 {
		t.Fatalf("seed not parsed: %v", res.Seed)
	}
	if res.UsageTokens == nil || *res.UsageTokens != 123 {
		t.Fatalf("usage tokens not parsed: %v", res.UsageTokens)
	}
	if res.GenerateAudio == nil || !*res.GenerateAudio {
		t.Fatalf("generate_audio not carried: %v", res.GenerateAudio)
	}
	if info, err := os.Stat(out); err != nil || info.Size() == 0 {
		t.Fatalf("output missing: %v", err)
	}
}

func TestArkVideoCapabilitiesForModel(t *testing.T) {
	seedance15 := arkVideoCapabilitiesForModel("doubao-seedance-1-5-pro-251215")
	if !seedance15.FirstFrame || !seedance15.LastFrame || seedance15.MaxReferenceImages != 9 {
		t.Fatalf("unexpected 1.5 caps: %+v", seedance15)
	}
	if seedance15.ReferenceAudioMode != provider.ReferenceAudioNone {
		t.Fatalf("1.5 should have no reference audio: %s", seedance15.ReferenceAudioMode)
	}

	seedance2 := arkVideoCapabilitiesForModel("doubao-seedance-2-0-mini-260615")
	if !seedance2.LastFrame || seedance2.ReferenceAudioMode != provider.ReferenceAudioDirect {
		t.Fatalf("unexpected 2.0 caps: %+v", seedance2)
	}
	if seedance2.MaxReferenceAudioCount != 3 ||
		seedance2.MaxReferenceAudioTotalSeconds == nil || *seedance2.MaxReferenceAudioTotalSeconds != 15 {
		t.Fatalf("unexpected 2.0 audio caps: %+v", seedance2)
	}

	seedance25 := arkVideoCapabilitiesForModel("doubao-seedance-2-5-260628")
	if seedance25.MaxReferenceImages != 30 || !seedance25.FirstFrameRatioAdaptiveOnly {
		t.Fatalf("unexpected 2.5 caps: %+v", seedance25)
	}
	if seedance25.MaxReferenceAudioCount != 10 ||
		seedance25.MaxReferenceAudioTotalSeconds == nil || *seedance25.MaxReferenceAudioTotalSeconds != 30 {
		t.Fatalf("unexpected 2.5 audio caps: %+v", seedance25)
	}

	// 未上表的未知型号一律保守判定。
	unknown := arkVideoCapabilitiesForModel("doubao-seedance-9-9-future")
	if !unknown.FirstFrame || unknown.LastFrame || unknown.MaxReferenceImages != 0 {
		t.Fatalf("unknown model should be conservative: %+v", unknown)
	}
}

func TestArkBackendsRegistered(t *testing.T) {
	for _, key := range []string{"ark", "ark-agent-plan"} {
		if _, err := provider.CreateImageBackend(key, provider.BackendOptions{APIKey: "sk"}); err != nil {
			t.Fatalf("image backend %q not registered: %v", key, err)
		}
		if _, err := provider.CreateVideoBackend(key, provider.BackendOptions{APIKey: "sk"}); err != nil {
			t.Fatalf("video backend %q not registered: %v", key, err)
		}
	}
	caps, err := provider.VideoCapabilitiesForModel("ark", "doubao-seedance-2-0-mini-260615")
	if err != nil {
		t.Fatalf("capabilities lookup: %v", err)
	}
	if caps.MaxReferenceAudioCount != 3 {
		t.Fatalf("unexpected caps from registry: %+v", caps)
	}
}
