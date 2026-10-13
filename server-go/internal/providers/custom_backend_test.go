package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/provider"
)

// customTestConfig 构造自定义供应商信封。
func customTestConfig(providerID, baseURL, apiKey string) CustomProviderConfig {
	return CustomProviderConfig{ProviderID: providerID, BaseURL: baseURL, APIKey: apiKey}
}

// TestCreateCustomBackendBuiltinOpenAIImages 断言图片 endpoint 无声明时转发内置协议 backend：
// 请求落到 /v1/images/generations，包装层覆写 name / model，产物按 b64 落盘。
func TestCreateCustomBackendBuiltinOpenAIImages(t *testing.T) {
	imageBytes := []byte("custom-img-bytes")
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString(imageBytes)}},
		})
	}))
	t.Cleanup(srv.Close)

	backend, err := CreateCustomBackend(
		customTestConfig("custom-7", srv.URL, "sk-custom"), "gpt-image-2", "openai-images", "", "",
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if backend.MediaType != provider.MediaImage || backend.Image == nil || backend.Video != nil || backend.Audio != nil {
		t.Fatalf("unexpected dispatch: %+v", backend)
	}
	if backend.Image.Name() != "custom-7" || backend.Image.Model() != "gpt-image-2" {
		t.Fatalf("wrapper identity: %s / %s", backend.Image.Name(), backend.Image.Model())
	}
	if backend.Image.Delegate() == nil {
		t.Fatalf("delegate must be non-nil")
	}

	out := filepath.Join(t.TempDir(), "out.png")
	res, err := backend.Image.Generate(context.Background(), provider.NewImageGenerationRequest("a cat", out))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotPath != "/v1/images/generations" {
		t.Fatalf("path: %q", gotPath)
	}
	if gotAuth != "Bearer sk-custom" {
		t.Fatalf("auth: %q", gotAuth)
	}
	if gotBody["model"] != "gpt-image-2" || gotBody["prompt"] != "a cat" {
		t.Fatalf("body: %v", gotBody)
	}
	if res.Provider != ProviderOpenAI {
		t.Fatalf("result provider: %q", res.Provider)
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != string(imageBytes) {
		t.Fatalf("output not written: %v", err)
	}
}

// TestCreateCustomBackendBuiltinOpenAITTS 断言音频 endpoint 无声明时转发内置语音 backend，
// 且记账 provider 归因到自定义供应商。
func TestCreateCustomBackendBuiltinOpenAITTS(t *testing.T) {
	audioBytes := []byte("audio-bytes")
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write(audioBytes)
	}))
	t.Cleanup(srv.Close)

	backend, err := CreateCustomBackend(
		customTestConfig("custom-3", srv.URL, "sk-a"), "tts-custom", "openai-tts", "", "",
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if backend.MediaType != provider.MediaAudio || backend.Audio == nil {
		t.Fatalf("unexpected dispatch: %+v", backend)
	}
	if backend.Audio.Name() != "custom-3" || backend.Audio.Model() != "tts-custom" {
		t.Fatalf("wrapper identity: %s / %s", backend.Audio.Name(), backend.Audio.Model())
	}
	out := filepath.Join(t.TempDir(), "out.wav")
	res, err := backend.Audio.Synthesize(context.Background(), provider.NewAudioSynthesisRequest("你好", out, "nova"))
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if gotPath != "/v1/audio/speech" {
		t.Fatalf("path: %q", gotPath)
	}
	if gotAuth != "Bearer sk-a" {
		t.Fatalf("auth: %q", gotAuth)
	}
	if gotBody["model"] != "tts-custom" || gotBody["input"] != "你好" || gotBody["voice"] != "nova" {
		t.Fatalf("body: %v", gotBody)
	}
	if res.Provider != "custom-3" || res.Characters != 2 {
		t.Fatalf("result: %+v", res)
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != string(audioBytes) {
		t.Fatalf("output not written: %v", err)
	}
}

// TestCreateCustomBackendBuiltinOpenAIVideo 断言视频 endpoint 无声明时转发内置 backend 并注入 endpoint。
func TestCreateCustomBackendBuiltinOpenAIVideo(t *testing.T) {
	videoBytes := []byte("custom-video-bytes")
	var createPath, pollPath, contentPath string
	var createBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/videos":
			createPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "video_1", "status": "in_progress"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/video_1":
			pollPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "video_1", "status": "completed", "seconds": 4})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/video_1/content":
			contentPath = r.URL.Path
			_, _ = w.Write(videoBytes)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	backend, err := CreateCustomBackend(
		customTestConfig("custom-5", srv.URL, "sk-v"), "sora-2", "openai-video", "", "",
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if backend.MediaType != provider.MediaVideo || backend.Video == nil {
		t.Fatalf("unexpected dispatch: %+v", backend)
	}
	if backend.Video.Endpoint() != "openai-video" || backend.Video.Name() != "custom-5" {
		t.Fatalf("wrapper identity: %s endpoint %s", backend.Video.Name(), backend.Video.Endpoint())
	}
	out := filepath.Join(t.TempDir(), "out.mp4")
	res, err := backend.Video.Generate(context.Background(), provider.NewVideoGenerationRequest("a cat surfing", out))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if createPath != "/v1/videos" || pollPath != "/v1/videos/video_1" || contentPath != "/v1/videos/video_1/content" {
		t.Fatalf("paths: %q %q %q", createPath, pollPath, contentPath)
	}
	if createBody["model"] != "sora-2" {
		t.Fatalf("body model: %v", createBody["model"])
	}
	if res.TaskID != "video_1" || res.DurationSeconds != 4 {
		t.Fatalf("result: %+v", res)
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != string(videoBytes) {
		t.Fatalf("output not written: %v", err)
	}
}

// TestCreateCustomBackendVideoCapabilitySynthesis 断言视频生效能力 = 系统判定 ⊕ 声明 ⊕ 用户覆盖，
// 且 endpoint 未声明的维度（尾帧 / 参考音色）覆盖被收窄丢弃。
func TestCreateCustomBackendVideoCapabilitySynthesis(t *testing.T) {
	backend, err := CreateCustomBackend(
		customTestConfig("custom-5", "https://example.invalid", "sk"),
		"sora-2", "openai-video", "",
		`{"max_reference_images":3,"last_frame":true,"reference_audio_mode":"direct"}`,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if backend.Video == nil || backend.Video.Endpoint() != "openai-video" {
		t.Fatalf("unexpected dispatch: %+v", backend)
	}
	caps := backend.Video.VideoCapabilities()
	if caps.MaxReferenceImages != 3 {
		t.Fatalf("max_reference_images override not applied: %+v", caps)
	}
	if caps.LastFrame {
		t.Fatalf("last_frame must be dropped for a non end-image endpoint: %+v", caps)
	}
	if caps.ReferenceAudioMode != provider.ReferenceAudioNone {
		t.Fatalf("reference_audio_mode must stay none: %+v", caps)
	}
	if !caps.FirstFrame {
		t.Fatalf("first_frame default must hold: %+v", caps)
	}
	tierAware, ok := backend.Video.(interface {
		VideoCapabilitiesForTier(serviceTier, resolution string) provider.VideoCapabilities
	})
	if !ok {
		t.Fatalf("wrapped video backend must implement VideoCapabilitiesForTier")
	}
	if tierCaps := tierAware.VideoCapabilitiesForTier("default", "720p"); tierCaps.MaxReferenceImages != 3 {
		t.Fatalf("tier query must carry overrides: %+v", tierCaps)
	}
}

// TestCreateCustomBackendDeclarativeImage 断言声明式图片请求：模板渲染 path / body / headers。
func TestCreateCustomBackendDeclarativeImage(t *testing.T) {
	imageBytes := []byte("decl-img")
	var gotPath, gotAccept, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"payload": map[string]any{"b64": base64.StdEncoding.EncodeToString(imageBytes)},
		})
	}))
	t.Cleanup(srv.Close)

	declaration := `{"path":"/v1/custom/image","headers":{"accept":"application/json"},` +
		`"body":{"/prompt":"prompt","/aspect_ratio":"aspect_ratio"},` +
		`"response":{"b64_json":"/payload/b64"}}`
	backend, err := CreateCustomBackend(
		customTestConfig("custom-1", srv.URL, "sk-x"), "my-model", "openai-images", declaration, "",
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.png")
	res, err := backend.Image.Generate(context.Background(), provider.NewImageGenerationRequest("draw", out))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotPath != "/v1/custom/image" {
		t.Fatalf("path: %q", gotPath)
	}
	if gotAccept != "application/json" || gotAuth != "Bearer sk-x" {
		t.Fatalf("headers: accept=%q auth=%q", gotAccept, gotAuth)
	}
	if gotBody["prompt"] != "draw" || gotBody["aspect_ratio"] != "9:16" {
		t.Fatalf("body: %v", gotBody)
	}
	if res.Provider != "custom-1" || res.Model != "my-model" {
		t.Fatalf("result: %+v", res)
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != string(imageBytes) {
		t.Fatalf("output not written: %v", err)
	}
}

// TestCreateCustomBackendDeclarativeImagePoll 断言声明式 submit-and-poll 图片链路：
// 提交读 job_id、轮询状态、渲染 artifact_url 后下载落盘。
func TestCreateCustomBackendDeclarativeImagePoll(t *testing.T) {
	imageBytes := []byte("polled-img")
	var submitPath, pollPath, downloadPath string
	var submitBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/submit":
			submitPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&submitBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"job": map[string]any{"id": "j1"}})
		case r.Method == http.MethodGet && r.URL.Path == "/jobs/j1":
			pollPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "SUCCEEDED", "output": map[string]any{"image_id": "abc"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/download/abc":
			downloadPath = r.URL.Path
			_, _ = w.Write(imageBytes)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	declaration := `{"path":"/submit","body":{"/prompt":"prompt"},` +
		`"poll":{"path":"/jobs/{job_id}","job_id":"/job/id","status":"/state",` +
		`"done_value":"SUCCEEDED","result":"/output","artifact_url":"/download/{image_id}",` +
		`"interval_seconds":0.05,"timeout_seconds":5}}`
	backend, err := CreateCustomBackend(
		customTestConfig("custom-2", srv.URL, "sk"), "m", "openai-images", declaration, "",
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.png")
	if _, err := backend.Image.Generate(context.Background(), provider.NewImageGenerationRequest("draw", out)); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if submitPath != "/submit" || pollPath != "/jobs/j1" || downloadPath != "/download/abc" {
		t.Fatalf("paths: %q %q %q", submitPath, pollPath, downloadPath)
	}
	if submitBody["prompt"] != "draw" {
		t.Fatalf("submit body: %v", submitBody)
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != string(imageBytes) {
		t.Fatalf("output not written: %v", err)
	}
}

// TestCreateCustomBackendDeclarativeVideo 断言声明式视频请求：响应映射取 video_url 后下载落盘。
func TestCreateCustomBackendDeclarativeVideo(t *testing.T) {
	videoBytes := []byte("decl-video")
	var genPath string
	var genBody map[string]any
	var downloadURL, downloadPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/gen":
			genPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&genBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"video": downloadURL}})
		case r.Method == http.MethodGet && r.URL.Path == "/out.mp4":
			downloadPath = r.URL.Path
			_, _ = w.Write(videoBytes)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	downloadURL = srv.URL + "/out.mp4"

	declaration := `{"path":"/v1/gen","body":{"/prompt":"prompt","/duration":"duration"},` +
		`"response":{"video_url":"/data/video"}}`
	backend, err := CreateCustomBackend(
		customTestConfig("custom-9", srv.URL, "sk"), "veo-x", "openai-video", declaration, "",
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if backend.Video == nil || backend.Video.Endpoint() != "openai-video" {
		t.Fatalf("unexpected dispatch: %+v", backend)
	}
	out := filepath.Join(t.TempDir(), "out.mp4")
	res, err := backend.Video.Generate(context.Background(), provider.NewVideoGenerationRequest("a bird", out))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if genPath != "/v1/gen" || downloadPath != "/out.mp4" {
		t.Fatalf("paths: %q %q", genPath, downloadPath)
	}
	if genBody["prompt"] != "a bird" || genBody["duration"] != float64(5) {
		t.Fatalf("body: %v", genBody)
	}
	if res.Provider != "custom-9" || res.Model != "veo-x" || res.VideoURI != downloadURL {
		t.Fatalf("result: %+v", res)
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != string(videoBytes) {
		t.Fatalf("output not written: %v", err)
	}
}

// TestCreateCustomBackendTextEndpointFailsLoud 断言文本 endpoint 已登记但构造时 fail loud。
func TestCreateCustomBackendTextEndpointFailsLoud(t *testing.T) {
	_, err := CreateCustomBackend(customTestConfig("custom-1", "", ""), "gpt-x", "openai-chat", "", "")
	if err == nil || !strings.Contains(err.Error(), "text backend not wired") {
		t.Fatalf("expected text fail-loud, got %v", err)
	}
}

// TestCreateCustomBackendUnknownEndpoint 断言未知 endpoint 报错。
func TestCreateCustomBackendUnknownEndpoint(t *testing.T) {
	_, err := CreateCustomBackend(customTestConfig("custom-1", "", ""), "m", "nope", "", "")
	if err == nil || !strings.Contains(err.Error(), "unknown endpoint") {
		t.Fatalf("expected unknown endpoint error, got %v", err)
	}
}

// TestCreateCustomBackendPollOnVideoFailsLoud 断言 poll 传输只支持图片 endpoint。
func TestCreateCustomBackendPollOnVideoFailsLoud(t *testing.T) {
	declaration := `{"path":"/x","poll":{"path":"/s","job_id":"/id","status":"/st","done_value":"ok"}}`
	_, err := CreateCustomBackend(
		customTestConfig("custom-1", "https://example.invalid", "sk"), "m", "openai-video", declaration, "",
	)
	if err == nil || !strings.Contains(err.Error(), "poll is only supported on image endpoints") {
		t.Fatalf("expected poll-on-video error, got %v", err)
	}
}

// TestCreateCustomBackendRejectsMalformedJSON 断言非法声明 / 覆盖 JSON 被拒。
func TestCreateCustomBackendRejectsMalformedJSON(t *testing.T) {
	cfg := customTestConfig("custom-1", "https://example.invalid", "sk")
	if _, err := CreateCustomBackend(cfg, "m", "openai-images", "{not json", ""); err == nil ||
		!strings.Contains(err.Error(), "parse endpoint declaration") {
		t.Fatalf("expected declaration parse error, got %v", err)
	}
	if _, err := CreateCustomBackend(cfg, "m", "openai-images", "", "not-json"); err == nil ||
		!strings.Contains(err.Error(), "parse capability overrides") {
		t.Fatalf("expected overrides parse error, got %v", err)
	}
}
