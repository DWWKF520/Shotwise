package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/provider"
)

// 读取请求体并记录，供断言请求形态。
func readJSONBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	body, _ := io.ReadAll(r.Body)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return parsed
}

func TestDashscopeImageGenerateHappyPath(t *testing.T) {
	var (
		gotPath string
		gotAuth string
		payload map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			payload = readJSONBody(t, r)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"output": map[string]any{
					"choices": []map[string]any{{
						"message": map[string]any{
							"content": []map[string]any{{"image": "http://" + r.Host + "/img.png"}},
						},
					}},
				},
			})
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte("img-bytes"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	backend, err := newDashscopeImageBackend(provider.BackendOptions{APIKey: "test-key", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("construct image backend: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.png")
	res, err := backend.Generate(context.Background(), provider.NewImageGenerationRequest("a cat", out))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotPath != "/api/v1/services/aigc/multimodal-generation/generation" {
		t.Fatalf("unexpected endpoint path: %s", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("unexpected auth header: %q", gotAuth)
	}
	if payload["model"] != "qwen-image-2.0" {
		t.Fatalf("unexpected model: %v", payload["model"])
	}
	params, _ := payload["parameters"].(map[string]any)
	if _, ok := params["size"].(string); !ok {
		t.Fatalf("parameters.size missing or not string: %v", params["size"])
	}
	if res.ImageURI == "" {
		t.Fatalf("image_uri not set")
	}
	if content, err := os.ReadFile(out); err != nil || string(content) != "img-bytes" {
		t.Fatalf("output file wrong: %q (%v)", content, err)
	}
}

func TestDashscopeVideoWan3BaseURL(t *testing.T) {
	var (
		submitPath  string
		submitAuth  string
		asyncHeader string
		submitBody  map[string]any
		pollPath    string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "video-synthesis"):
			submitPath = r.URL.Path
			submitAuth = r.Header.Get("Authorization")
			asyncHeader = r.Header.Get("X-DashScope-Async")
			submitBody = readJSONBody(t, r)
			_ = json.NewEncoder(w).Encode(map[string]any{"output": map[string]any{"task_id": "t1"}})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tasks/"):
			pollPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]any{
				"output": map[string]any{
					"task_status": "SUCCEEDED",
					"video_url":   "http://" + r.Host + "/v.mp4",
				},
				"usage": map[string]any{"duration": 6},
			})
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte("video-bytes"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	backend, err := newDashscopeVideoBackend(provider.BackendOptions{
		APIKey: "test-key",
		// 通用 base_url 指向不可达地址，验证 wan3.0 走独立 maas 域名。
		BaseURL: "http://general.invalid",
		Model:   "wan3.0-video",
		Extra:   map[string]string{"wan3_base_url": srv.URL},
	})
	if err != nil {
		t.Fatalf("construct video backend: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.mp4")
	res, err := backend.Generate(context.Background(), provider.NewVideoGenerationRequest("a shot", out))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if submitPath != "/services/aigc/video-generation/video-synthesis" {
		t.Fatalf("unexpected submit path: %s", submitPath)
	}
	if submitAuth != "Bearer test-key" {
		t.Fatalf("unexpected auth header: %q", submitAuth)
	}
	if asyncHeader != "enable" {
		t.Fatalf("expected async header enable, got %q", asyncHeader)
	}
	if submitBody["model"] != "wan3.0-video" {
		t.Fatalf("unexpected model: %v", submitBody["model"])
	}
	params, _ := submitBody["parameters"].(map[string]any)
	if params["resolution"] != "720P" {
		t.Fatalf("unexpected resolution: %v", params["resolution"])
	}
	// 无首帧时应下发 ratio，wan3.0 应下发 audio 开关。
	if params["ratio"] != "9:16" {
		t.Fatalf("unexpected ratio: %v", params["ratio"])
	}
	if params["audio"] != true {
		t.Fatalf("expected audio=true, got %v", params["audio"])
	}
	input, _ := submitBody["input"].(map[string]any)
	if _, hasMedia := input["media"]; hasMedia {
		t.Fatalf("wan3.0 text request should not carry media")
	}
	// 轮询应回放提交时的 maas 域名。
	if pollPath != "/tasks/t1" {
		t.Fatalf("unexpected poll path: %s", pollPath)
	}
	if res.DurationSeconds != 6 {
		t.Fatalf("expected billing duration 6, got %d", res.DurationSeconds)
	}
	if content, err := os.ReadFile(out); err != nil || string(content) != "video-bytes" {
		t.Fatalf("output file wrong: %q (%v)", content, err)
	}

	// 静态能力查询：wan3.0 声明尾帧 + 参考图上限 10。
	caps, err := provider.VideoCapabilitiesForModel("dashscope", "wan3.0-video")
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if !caps.LastFrame || caps.MaxReferenceImages != 10 || caps.ReferenceAudioMode != provider.ReferenceAudioDirect {
		t.Fatalf("unexpected wan3 capabilities: %+v", caps)
	}
}

func TestDashscopeAudioSynthesizeAndVoices(t *testing.T) {
	var (
		gotPath     string
		gotAuth     string
		asyncHeader string
		payload     map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			asyncHeader = r.Header.Get("X-DashScope-Async")
			payload = readJSONBody(t, r)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"output": map[string]any{"audio": map[string]any{"url": "http://" + r.Host + "/a.wav"}},
			})
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte("wav-bytes"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	backend, err := newDashscopeAudioBackend(provider.BackendOptions{APIKey: "test-key", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("construct audio backend: %v", err)
	}
	voices := backend.ListVoices()
	if len(voices) != 19 || voices[0].ID != "Cherry" || voices[0].Label != "voice_label_dashscope_cherry" {
		t.Fatalf("unexpected voice catalog: len=%d first=%+v", len(voices), voices[0])
	}

	out := filepath.Join(t.TempDir(), "out.wav")
	res, err := backend.Synthesize(context.Background(), provider.NewAudioSynthesisRequest("hello", out, "Cherry"))
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if gotPath != "/api/v1/services/aigc/multimodal-generation/generation" {
		t.Fatalf("unexpected endpoint path: %s", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("unexpected auth header: %q", gotAuth)
	}
	// 同步 TTS 不带异步头。
	if asyncHeader != "" {
		t.Fatalf("audio TTS must not set async header, got %q", asyncHeader)
	}
	if payload["model"] != "qwen3-tts-flash" {
		t.Fatalf("unexpected model: %v", payload["model"])
	}
	input, _ := payload["input"].(map[string]any)
	if input["voice"] != "Cherry" || input["language_type"] != "Chinese" || input["text"] != "hello" {
		t.Fatalf("unexpected input payload: %v", input)
	}
	if res.Characters != 5 {
		t.Fatalf("expected 5 characters, got %d", res.Characters)
	}
	if content, err := os.ReadFile(out); err != nil || string(content) != "wav-bytes" {
		t.Fatalf("output file wrong: %q (%v)", content, err)
	}
}