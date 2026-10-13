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

// openAIVideoTestVideoBytes 是假成片字节（内容无关，仅验证落盘链路）。
var openAIVideoTestVideoBytes = []byte("fake-mp4-bytes")

// TestOpenAIVideoGenerateHappyPath 断言 OpenAI Sora 视频后端 happy path 的请求形态：
// create 走 POST /videos（JSON，无参考图），轮询 GET /videos/{id}，内容 GET /videos/{id}/content。
func TestOpenAIVideoGenerateHappyPath(t *testing.T) {
	var createPath, authHeader string
	var createPayload map[string]any
	var pollPath, contentPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/videos":
			createPath = r.URL.Path
			authHeader = r.Header.Get("Authorization")
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &createPayload)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "video_1", "status": "in_progress"})
		case r.Method == http.MethodGet && r.URL.Path == "/videos/video_1":
			pollPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "video_1", "status": "completed", "seconds": 4})
		case r.Method == http.MethodGet && r.URL.Path == "/videos/video_1/content":
			contentPath = r.URL.Path
			_, _ = w.Write(openAIVideoTestVideoBytes)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	backend := newOpenAIVideoBackend(provider.BackendOptions{APIKey: "sk-test", BaseURL: srv.URL})
	req := provider.NewVideoGenerationRequest("a cat surfing", filepath.Join(t.TempDir(), "out.mp4"))
	res, err := backend.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if createPath != "/videos" {
		t.Fatalf("expected create path /videos, got %q", createPath)
	}
	if authHeader != "Bearer sk-test" {
		t.Fatalf("unexpected auth header: %q", authHeader)
	}
	if createPayload["model"] != "sora-2" {
		t.Fatalf("unexpected model: %v", createPayload["model"])
	}
	if createPayload["prompt"] != "a cat surfing" {
		t.Fatalf("unexpected prompt: %v", createPayload["prompt"])
	}
	if createPayload["seconds"] != "5" {
		t.Fatalf("unexpected seconds: %v", createPayload["seconds"])
	}
	// 9:16 无分辨率 → 720p 档吸附 720x1280。
	if createPayload["size"] != "720x1280" {
		t.Fatalf("unexpected size: %v", createPayload["size"])
	}
	if pollPath != "/videos/video_1" || contentPath != "/videos/video_1/content" {
		t.Fatalf("unexpected poll/content paths: %q %q", pollPath, contentPath)
	}
	if res.DurationSeconds != 4 || res.TaskID != "video_1" || res.Provider != ProviderOpenAI {
		t.Fatalf("unexpected result: %+v", res)
	}
	data, err := os.ReadFile(res.VideoPath)
	if err != nil || string(data) != string(openAIVideoTestVideoBytes) {
		t.Fatalf("output not written: %v", err)
	}
}

// TestOpenAIVideoCapabilities 断言 Sora 参考图上限为 1（首帧与参考共享单槽）。
func TestOpenAIVideoCapabilities(t *testing.T) {
	caps := openAIVideoCapabilitiesForModel("sora-2-pro")
	if caps.MaxReferenceImages != 1 || !caps.FirstFrame || caps.LastFrame {
		t.Fatalf("unexpected caps: %+v", caps)
	}
}

// TestNewAPIVideoGenerateHappyPath 断言 NewAPI 视频后端 happy path 的请求形态。
func TestNewAPIVideoGenerateHappyPath(t *testing.T) {
	var createPath, authHeader string
	var createPayload map[string]any

	var downloadURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/video/generations", func(w http.ResponseWriter, r *http.Request) {
		createPath = r.URL.Path
		authHeader = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &createPayload)
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task_1"})
	})
	mux.HandleFunc("/video/generations/task_1", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "completed",
			"url":      downloadURL,
			"metadata": map[string]any{"duration": "5", "seed": 7},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	downloadURL = srv.URL + "/download"
	mux.HandleFunc("/download", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(openAIVideoTestVideoBytes)
	})

	backend, err := newNewAPIVideoBackend(provider.BackendOptions{
		APIKey: "sk-newapi", BaseURL: srv.URL, Model: "kling-v1",
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	req := provider.NewVideoGenerationRequest("a dog", filepath.Join(t.TempDir(), "out.mp4"))
	res, err := backend.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if createPath != "/video/generations" {
		t.Fatalf("unexpected create path: %q", createPath)
	}
	if authHeader != "Bearer sk-newapi" {
		t.Fatalf("unexpected auth header: %q", authHeader)
	}
	if createPayload["model"] != "kling-v1" || createPayload["duration"] != float64(5) {
		t.Fatalf("unexpected payload: %v", createPayload)
	}
	// 9:16 + 无分辨率 → 短边 720，对齐 8 的倍数 → 720x1280。
	if createPayload["width"] != float64(720) || createPayload["height"] != float64(1280) {
		t.Fatalf("unexpected size: %vx%v", createPayload["width"], createPayload["height"])
	}
	if createPayload["n"] != float64(1) {
		t.Fatalf("unexpected n: %v", createPayload["n"])
	}
	if res.Provider != newAPIVideoProvider || res.TaskID != "task_1" || res.DurationSeconds != 5 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Seed == nil || *res.Seed != 7 {
		t.Fatalf("unexpected seed: %v", res.Seed)
	}
}

// TestV2VideoGenerationsGenerateHappyPath 断言 v2「流派 C」视频后端 happy path 的请求形态与多路径解析。
func TestV2VideoGenerationsGenerateHappyPath(t *testing.T) {
	var createPath string
	var createPayload map[string]any
	var pollQuery string

	var downloadURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/video/generations", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			createPath = r.URL.Path
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &createPayload)
			_ = json.NewEncoder(w).Encode(map[string]any{"generation_id": "g1"})
			return
		}
		pollQuery = r.URL.Query().Get("generation_id")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "succeeded",
			"video":  map[string]any{"url": downloadURL},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	downloadURL = srv.URL + "/dl"
	mux.HandleFunc("/dl", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(openAIVideoTestVideoBytes)
	})

	backend, err := newV2VideoGenerationsBackend(provider.BackendOptions{
		APIKey: "sk-v2", BaseURL: srv.URL, Model: "veo-3",
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	req := provider.NewVideoGenerationRequest("a bird", filepath.Join(t.TempDir(), "out.mp4"))
	res, err := backend.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if createPath != "/v2/video/generations" {
		t.Fatalf("unexpected create path: %q", createPath)
	}
	if createPayload["model"] != "veo-3" || createPayload["duration"] != float64(5) {
		t.Fatalf("unexpected payload: %v", createPayload)
	}
	if createPayload["aspect_ratio"] != "9:16" {
		t.Fatalf("aspect_ratio should always be sent, got %v", createPayload["aspect_ratio"])
	}
	if pollQuery != "g1" {
		t.Fatalf("poll should carry generation_id, got %q", pollQuery)
	}
	if res.Provider != v2VideoProvider || res.TaskID != "g1" || res.VideoURI != downloadURL {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// TestOpenAIAudioSynthesizeAndVoices 断言 OpenAI 兼容语音后端：请求体字段、字符计费与音色目录。
func TestOpenAIAudioSynthesizeAndVoices(t *testing.T) {
	var payload map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		_, _ = w.Write([]byte("audio-bytes"))
	}))
	t.Cleanup(srv.Close)

	backend, err := newOpenAIAudioBackend(provider.BackendOptions{
		Model: "gpt-4o-mini-tts", APIKey: "sk", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if len(backend.ListVoices()) != 13 {
		t.Fatalf("expected full voice catalog (13), got %d", len(backend.ListVoices()))
	}

	req := provider.NewAudioSynthesisRequest("你好", filepath.Join(t.TempDir(), "out.wav"), "nova")
	res, err := backend.Synthesize(context.Background(), req)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if gotPath != "/audio/speech" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if payload["model"] != "gpt-4o-mini-tts" || payload["input"] != "你好" || payload["voice"] != "nova" {
		t.Fatalf("unexpected payload: %v", payload)
	}
	if payload["response_format"] != "wav" {
		t.Fatalf("unexpected response_format: %v", payload["response_format"])
	}
	if res.Characters != 2 || res.Provider != ProviderOpenAI {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// TestOpenAIAudioLegacyVoiceNarrowing 断言官方 legacy 模型下收窄音色子集。
func TestOpenAIAudioLegacyVoiceNarrowing(t *testing.T) {
	backend, err := newOpenAIAudioBackend(provider.BackendOptions{Model: "tts-1", APIKey: "sk"})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if len(backend.ListVoices()) != 9 {
		t.Fatalf("legacy model should expose 9 voices, got %d", len(backend.ListVoices()))
	}

	// 覆写 provider_name 的自定义供应商即使模型同名也不收窄（维持全量目录）。
	custom, err := newOpenAIAudioBackend(provider.BackendOptions{
		Model: "tts-1", APIKey: "sk", Extra: map[string]string{"provider_name": "openai-tts"},
	})
	if err != nil {
		t.Fatalf("construct custom: %v", err)
	}
	if len(custom.ListVoices()) != 13 {
		t.Fatalf("custom provider should expose full catalog, got %d", len(custom.ListVoices()))
	}
	if custom.Name() != "openai-tts" {
		t.Fatalf("unexpected provider name: %s", custom.Name())
	}
}
