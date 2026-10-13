package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shotwise/shotwise/internal/provider"
)

// TestAgnesImageGenerateHappyPath 校验 OpenAI 兼容 images/generations 端点的请求形态与落盘。
func TestAgnesImageGenerateHappyPath(t *testing.T) {
	var gotPath, gotAuth string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"b64_json": pngB64()}}})
	}))
	t.Cleanup(srv.Close)

	backend, err := newAgnesImageBackend(provider.BackendOptions{APIKey: "ag-key", BaseURL: srv.URL, Model: agnesImageDefaultModel})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.png")
	result, err := backend.Generate(context.Background(), provider.ImageGenerationRequest{
		Prompt: "a castle", OutputPath: out, AspectRatio: "9:16",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if gotPath != "/v1/images/generations" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if gotAuth != "Bearer ag-key" {
		t.Fatalf("unexpected auth header: %q", gotAuth)
	}
	if payload["model"] != agnesImageDefaultModel || payload["size"] != "2K" || payload["ratio"] != "9:16" {
		t.Fatalf("unexpected payload: %v", payload)
	}
	if extra, ok := payload["extra_body"].(map[string]any); !ok || extra["response_format"] != "url" {
		t.Fatalf("unexpected extra_body: %v", payload["extra_body"])
	}
	if result.Provider != "agnes" || result.Model != agnesImageDefaultModel {
		t.Fatalf("unexpected result: %+v", result)
	}
	if info, err := os.Stat(out); err != nil || info.Size() == 0 {
		t.Fatalf("output not written: %v", err)
	}
}

// TestAgnesVideoGenerateHappyPath 校验异步 /v1/videos 提交、轮询与成片下载。
func TestAgnesVideoGenerateHappyPath(t *testing.T) {
	var submitPath, pollPath, gotAuth string
	var submitPayload map[string]any
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/videos":
			submitPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &submitPayload)
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "ag-1"})
		case "/v1/videos/ag-1":
			pollPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "completed", "url": srv.URL + "/ag.mp4", "seconds": "8",
			})
		case "/ag.mp4":
			_, _ = w.Write([]byte("agnes-video"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	backend, err := newAgnesVideoBackend(provider.BackendOptions{APIKey: "ag-key", BaseURL: srv.URL, Model: agnesVideoDefaultModel})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	req := provider.NewVideoGenerationRequest("a robot", filepath.Join(t.TempDir(), "out.mp4"))
	req.DurationSeconds = 8
	req.Resolution = "720p"
	result, err := backend.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if submitPath != "/v1/videos" || pollPath != "/v1/videos/ag-1" || gotAuth != "Bearer ag-key" {
		t.Fatalf("unexpected paths/auth: submit=%s poll=%s auth=%s", submitPath, pollPath, gotAuth)
	}
	// 无素材即 text 模式；size 由比例优先算出（9:16 @720p → 720x1280）。
	if submitPayload["mode"] != agnesVideoTextMode || submitPayload["seconds"] != "8" ||
		submitPayload["size"] != "720x1280" || submitPayload["aspect_ratio"] != "9:16" {
		t.Fatalf("unexpected submit payload: %v", submitPayload)
	}
	if result.TaskID != "ag-1" || result.DurationSeconds != 8 || result.VideoURI != srv.URL+"/ag.mp4" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.GenerateAudio == nil || *result.GenerateAudio {
		t.Fatalf("agnes 视频应恒无声: %v", result.GenerateAudio)
	}
	if data, err := os.ReadFile(req.OutputPath); err != nil || string(data) != "agnes-video" {
		t.Fatalf("video not downloaded: %v %q", err, data)
	}
}

// TestAgnesVideoFailureStatuses 校验失败态枚举：failed / error / cancelled / canceled 均快速失败。
func TestAgnesVideoFailureStatuses(t *testing.T) {
	for _, status := range []string{"failed", "error", "cancelled", "canceled"} {
		state := map[string]any{"status": status, "error": map[string]any{"message": "boom"}}
		if reason := agnesVideoFailureReason(state); reason == "" {
			t.Fatalf("status %q 应判为失败", status)
		}
	}
	for _, status := range []string{"queued", "in_progress", "completed"} {
		if reason := agnesVideoFailureReason(map[string]any{"status": status}); reason != "" {
			t.Fatalf("status %q 不应判为失败: %s", status, reason)
		}
	}
}

// TestAgnesVideoRequestValidation 校验互斥通道与型号专属时长 / 分辨率拒绝。
func TestAgnesVideoRequestValidation(t *testing.T) {
	backend, err := newAgnesVideoBackend(provider.BackendOptions{APIKey: "ag-key"})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.mp4")

	// 参考图与首帧叠加被拒。
	req := provider.NewVideoGenerationRequest("x", out)
	req.DurationSeconds = 5
	req.StartImage = "start.png"
	req.ReferenceImages = []string{"ref.png"}
	if _, err := backend.buildPayload(req); !hasCapabilityCode(err, provider.FailureVideoReferenceImagesWithFramesUnsupported) {
		t.Fatalf("expected frames+references rejection, got %v", err)
	}

	// 普通型号时长越界。
	req = provider.NewVideoGenerationRequest("x", out)
	req.DurationSeconds = 19
	if _, err := backend.buildPayload(req); !hasCapabilityCode(err, provider.FailureVideoDurationNotSupported) {
		t.Fatalf("expected duration rejection, got %v", err)
	}

	// Flash：时长越界与分辨率越界。
	flash, err := newAgnesVideoBackend(provider.BackendOptions{APIKey: "ag-key", Model: "agnes-video-2.5-flash"})
	if err != nil {
		t.Fatalf("construct flash backend: %v", err)
	}
	req = provider.NewVideoGenerationRequest("x", out)
	req.DurationSeconds = 18
	if _, err := flash.buildPayload(req); !hasCapabilityCode(err, provider.FailureVideoDurationNotSupported) {
		t.Fatalf("expected flash duration rejection, got %v", err)
	}
	req = provider.NewVideoGenerationRequest("x", out)
	req.DurationSeconds = 6
	req.Resolution = "1080p"
	if _, err := flash.buildPayload(req); !hasCapabilityCode(err, provider.FailureVideoResolutionNotSupported) {
		t.Fatalf("expected flash resolution rejection, got %v", err)
	}
}

// TestAgnesBillingAndCapabilities 校验能力声明、计费时长 half-up 取整与轮询上限。
func TestAgnesBillingAndCapabilities(t *testing.T) {
	flash, err := provider.VideoCapabilitiesForModel("agnes", "agnes-video-2.5-flash")
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if flash.MaxReferenceImages != agnesVideoFlashMaxReferenceImages {
		t.Fatalf("unexpected flash capabilities: %+v", flash)
	}
	standard, err := provider.VideoCapabilitiesForModel("agnes", agnesVideoDefaultModel)
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if standard.MaxReferenceImages != agnesVideoMaxReferenceImages {
		t.Fatalf("unexpected standard capabilities: %+v", standard)
	}

	foundImage, foundVideo := false, false
	for _, key := range provider.RegisteredImageBackendKeys() {
		foundImage = foundImage || key == "agnes"
	}
	for _, key := range provider.RegisteredVideoBackendKeys() {
		foundVideo = foundVideo || key == "agnes"
	}
	if !foundImage || !foundVideo {
		t.Fatalf("agnes should be registered: image=%v video=%v", foundImage, foundVideo)
	}

	// half-up 取整（4.5→5）；超 24h 上限回落；上限内保留。
	if v, ok := agnesCoerceDuration("4.5"); !ok || v != 5 {
		t.Fatalf("expected half-up 5, got %v %v", v, ok)
	}
	if _, ok := agnesCoerceDuration(99999); ok {
		t.Fatalf("超上限应回落")
	}
	if v, ok := agnesCoerceDuration(float64(86400)); !ok || v != 86400 {
		t.Fatalf("上限内应保留, got %v %v", v, ok)
	}

	// max_wait = max(900, duration*60)。
	if agnesVideoMaxWait(5) != 900*time.Second || agnesVideoMaxWait(18) != 18*60*time.Second {
		t.Fatalf("unexpected max wait: %s / %s", agnesVideoMaxWait(5), agnesVideoMaxWait(18))
	}
}

// hasCapabilityCode 判断错误是否为指定失败码的能力错误。
func hasCapabilityCode(err error, code provider.FailureCode) bool {
	var capErr *provider.CapabilityError
	return errors.As(err, &capErr) && capErr.Code == code
}