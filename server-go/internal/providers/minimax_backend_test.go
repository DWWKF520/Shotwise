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
	"time"

	"github.com/shotwise/shotwise/internal/provider"
)

// TestMinimaxImageGenerateHappyPath 校验 image-01 单步同步端点的请求形态与结果落盘。
func TestMinimaxImageGenerateHappyPath(t *testing.T) {
	var gotPath, gotAuth string
	var payload map[string]any
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/image_generation":
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &payload)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"base_resp": map[string]any{"status_code": 0},
				"data":      map[string]any{"image_urls": []string{srv.URL + "/result.png"}},
			})
		case "/result.png":
			_, _ = w.Write([]byte("png-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	backend, err := newMinimaxImageBackend(provider.BackendOptions{APIKey: "mm-key", BaseURL: srv.URL, Model: "image-01"})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.png")
	result, err := backend.Generate(context.Background(), provider.ImageGenerationRequest{
		Prompt: "a cat", OutputPath: out, AspectRatio: "9:16", ImageSize: "2K",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if gotPath != "/v1/image_generation" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if gotAuth != "Bearer mm-key" {
		t.Fatalf("unexpected auth header: %q", gotAuth)
	}
	if payload["model"] != "image-01" || payload["response_format"] != "url" || payload["n"] != float64(1) {
		t.Fatalf("unexpected payload: %v", payload)
	}
	if payload["prompt_optimizer"] != false {
		t.Fatalf("prompt_optimizer 应关闭: %v", payload["prompt_optimizer"])
	}
	// 竖屏 2K 经长边收口后为 1152x2048。
	if payload["width"] != float64(1152) || payload["height"] != float64(2048) {
		t.Fatalf("unexpected size: %v x %v", payload["width"], payload["height"])
	}
	if result.Provider != "minimax" || result.Model != "image-01" || result.ImageURI != srv.URL+"/result.png" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if data, err := os.ReadFile(out); err != nil || string(data) != "png-bytes" {
		t.Fatalf("output not written: %v %q", err, data)
	}
}

// TestMinimaxVideoGenerateV1HappyPath 校验海螺 v1 两步取 URL 流程（提交 → 轮询 → 取件 → 下载）。
func TestMinimaxVideoGenerateV1HappyPath(t *testing.T) {
	var submitPath, gotAuth, queryRaw, retrieveRaw string
	var submitPayload map[string]any
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/video_generation":
			submitPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &submitPayload)
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-1", "base_resp": map[string]any{"status_code": 0}})
		case "/v1/query/video_generation":
			queryRaw = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "Success", "file_id": "file-1"})
		case "/v1/files/retrieve":
			retrieveRaw = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(map[string]any{"file": map[string]any{"download_url": srv.URL + "/video.mp4"}})
		case "/video.mp4":
			_, _ = w.Write([]byte("video-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	backend, err := newMinimaxVideoBackend(provider.BackendOptions{APIKey: "mm-key", BaseURL: srv.URL, Model: minimaxVideoHailuo})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	req := provider.NewVideoGenerationRequest("a dog", filepath.Join(t.TempDir(), "out.mp4"))
	req.DurationSeconds = 6
	req.Resolution = "768p"
	result, err := backend.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if submitPath != "/v1/video_generation" || gotAuth != "Bearer mm-key" {
		t.Fatalf("unexpected submit: path=%s auth=%s", submitPath, gotAuth)
	}
	if submitPayload["model"] != minimaxVideoHailuo || submitPayload["resolution"] != "768P" || submitPayload["duration"] != float64(6) {
		t.Fatalf("unexpected submit payload: %v", submitPayload)
	}
	if queryRaw != "task_id=task-1" || retrieveRaw != "file_id=file-1" {
		t.Fatalf("unexpected query strings: %s / %s", queryRaw, retrieveRaw)
	}
	if result.TaskID != "task-1" || result.VideoURI != srv.URL+"/video.mp4" || result.DurationSeconds != 6 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if data, err := os.ReadFile(req.OutputPath); err != nil || string(data) != "video-bytes" {
		t.Fatalf("video not downloaded: %v %q", err, data)
	}
}

// TestMinimaxVideoGenerateV2HappyPath 校验 H3 v2 单步取 URL 流程与多模态 content[] 请求体。
func TestMinimaxVideoGenerateV2HappyPath(t *testing.T) {
	var submitPath string
	var submitPayload map[string]any
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/video_generation":
			submitPath = r.URL.Path
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &submitPayload)
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "h3-1"})
		case "/v2/query/video_generation/h3-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"task": map[string]any{"status": "succeeded", "content": map[string]any{"url": srv.URL + "/v2.mp4"}},
			})
		case "/v2.mp4":
			_, _ = w.Write([]byte("v2-video"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	backend, err := newMinimaxVideoBackend(provider.BackendOptions{APIKey: "mm-key", BaseURL: srv.URL, Model: minimaxVideoH3})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	startImage := filepath.Join(t.TempDir(), "frame.png")
	if err := os.WriteFile(startImage, []byte("frame-bytes"), 0o644); err != nil {
		t.Fatalf("write start image: %v", err)
	}
	req := provider.NewVideoGenerationRequest("a bird", filepath.Join(t.TempDir(), "out.mp4"))
	req.DurationSeconds = 8
	req.Resolution = "768p"
	req.StartImage = startImage
	result, err := backend.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if submitPath != "/v2/video_generation" {
		t.Fatalf("unexpected path: %s", submitPath)
	}
	if submitPayload["resolution"] != "768P" || submitPayload["duration"] != float64(8) || submitPayload["ratio"] != "9:16" {
		t.Fatalf("unexpected submit payload: %v", submitPayload)
	}
	content, ok := submitPayload["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("expected text + first_frame content, got %v", submitPayload["content"])
	}
	if first := content[0].(map[string]any); first["type"] != "text" || first["text"] != "a bird" {
		t.Fatalf("unexpected first content item: %v", first)
	}
	if frame := content[1].(map[string]any); frame["role"] != "first_frame" {
		t.Fatalf("unexpected frame content item: %v", frame)
	}
	if result.TaskID != "h3-1" || result.DurationSeconds != 8 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

// TestMinimaxVideoCapabilitiesAndRegistration 校验能力声明（含注入的静态查询）与轮询上限。
func TestMinimaxVideoCapabilitiesAndRegistration(t *testing.T) {
	s2v, err := provider.VideoCapabilitiesForModel("minimax", minimaxVideoS2V)
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if s2v.FirstFrame || s2v.MaxReferenceImages != 1 {
		t.Fatalf("unexpected S2V-01 capabilities: %+v", s2v)
	}
	h3, err := provider.VideoCapabilitiesForModel("minimax", minimaxVideoH3)
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if !h3.LastFrame || h3.ReferenceAudioMode != provider.ReferenceAudioDirect || h3.MaxReferenceAudioCount != 3 {
		t.Fatalf("unexpected H3 capabilities: %+v", h3)
	}
	if h3.MaxPromptChars == nil || *h3.MaxPromptChars != minimaxH3MaxPromptChars {
		t.Fatalf("unexpected H3 prompt limit: %v", h3.MaxPromptChars)
	}

	foundImage, foundVideo := false, false
	for _, key := range provider.RegisteredImageBackendKeys() {
		foundImage = foundImage || key == "minimax"
	}
	for _, key := range provider.RegisteredVideoBackendKeys() {
		foundVideo = foundVideo || key == "minimax"
	}
	if !foundImage || !foundVideo {
		t.Fatalf("minimax should be registered: image=%v video=%v", foundImage, foundVideo)
	}

	// max_wait = max(900, duration*60)。
	if minimaxVideoMaxWait(6) != 900*time.Second || minimaxVideoMaxWait(30) != 30*60*time.Second {
		t.Fatalf("unexpected max wait: %s / %s", minimaxVideoMaxWait(6), minimaxVideoMaxWait(30))
	}
}
