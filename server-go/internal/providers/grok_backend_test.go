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

	"github.com/shotwise/shotwise/internal/provider"
)

// 本地起假的 xAI REST 端点，断言请求形态（路径 / 鉴权头 / 关键 payload 字段）与结果映射；离线。

func TestGrokImageGenerateHappyPath(t *testing.T) {
	var gotPath, gotAuth string
	var payload map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/images/generations", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"url": "http://" + r.Host + "/image.png", "respect_moderation": true}},
		})
	})
	mux.HandleFunc("/image.png", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("png-bytes"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	backend, err := newGrokImageBackend(provider.BackendOptions{APIKey: "xai-key", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.png")
	res, err := backend.Generate(context.Background(), provider.NewImageGenerationRequest("a cat", out))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotPath != "/images/generations" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if gotAuth != "Bearer xai-key" {
		t.Fatalf("unexpected auth header: %q", gotAuth)
	}
	if payload["model"] != "grok-imagine-image" {
		t.Fatalf("unexpected model: %v", payload["model"])
	}
	if payload["aspect_ratio"] != "9:16" {
		t.Fatalf("unexpected aspect_ratio: %v", payload["aspect_ratio"])
	}
	if res.Provider != grokProviderID || res.Model != "grok-imagine-image" {
		t.Fatalf("unexpected identity: %s/%s", res.Provider, res.Model)
	}
	if info, err := os.Stat(out); err != nil || info.Size() == 0 {
		t.Fatalf("output file missing/empty: %v", err)
	}
}

func TestGrokImageGenerateWithReferences(t *testing.T) {
	var payload map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/images/generations", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"url": "http://" + r.Host + "/image.png"}},
		})
	})
	mux.HandleFunc("/image.png", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("png-bytes"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ref := filepath.Join(t.TempDir(), "ref.png")
	if err := os.WriteFile(ref, []byte("ref-bytes"), 0o644); err != nil {
		t.Fatalf("write ref: %v", err)
	}
	backend, err := newGrokImageBackend(provider.BackendOptions{APIKey: "xai-key", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	req := provider.NewImageGenerationRequest("edit", filepath.Join(t.TempDir(), "out.png"))
	req.ReferenceImages = []provider.ReferenceImage{{Path: ref}}
	if _, err := backend.Generate(context.Background(), req); err != nil {
		t.Fatalf("generate: %v", err)
	}
	uris, ok := payload["image_urls"].([]any)
	if !ok || len(uris) != 1 {
		t.Fatalf("expected one reference data uri, got %v", payload["image_urls"])
	}
	if uri, _ := uris[0].(string); len(uri) < len("data:image/png;base64,") || uri[:len("data:image/png;base64,")] != "data:image/png;base64," {
		t.Fatalf("unexpected reference uri: %v", uris[0])
	}
}

func TestGrokVideoGenerateHappyPath(t *testing.T) {
	var submitPath, submitAuth string
	var payload map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/videos/generations", func(w http.ResponseWriter, r *http.Request) {
		submitPath = r.URL.Path
		submitAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		_ = json.NewEncoder(w).Encode(map[string]any{"request_id": "req-1"})
	})
	mux.HandleFunc("/videos/req-1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "done",
			"video":  map[string]any{"url": "http://" + r.Host + "/video.mp4", "duration": 8},
		})
	})
	mux.HandleFunc("/video.mp4", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("video-bytes"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	backend, err := newGrokVideoBackend(provider.BackendOptions{APIKey: "xai-key", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.mp4")
	res, err := backend.Generate(context.Background(), provider.NewVideoGenerationRequest("a clip", out))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if submitPath != "/videos/generations" {
		t.Fatalf("unexpected submit path: %s", submitPath)
	}
	if submitAuth != "Bearer xai-key" {
		t.Fatalf("unexpected auth header: %q", submitAuth)
	}
	if payload["model"] != "grok-imagine-video" {
		t.Fatalf("unexpected model: %v", payload["model"])
	}
	if payload["duration"] != float64(5) {
		t.Fatalf("unexpected duration: %v", payload["duration"])
	}
	// 计费时长取自供应商回报的 duration（half-up），而非请求时长。
	if res.DurationSeconds != 8 {
		t.Fatalf("unexpected billed duration: %d", res.DurationSeconds)
	}
	if res.GenerateAudio == nil || !*res.GenerateAudio {
		t.Fatalf("generate_audio should be true: %v", res.GenerateAudio)
	}
}

func TestGrokVideoResumeUnsupported(t *testing.T) {
	backend, err := newGrokVideoBackend(provider.BackendOptions{APIKey: "xai-key"})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	_, err = backend.ResumeVideo(
		context.Background(), "job-1",
		provider.NewVideoGenerationRequest("a clip", filepath.Join(t.TempDir(), "out.mp4")),
	)
	if !errors.Is(err, provider.ErrResumeUnsupported) {
		t.Fatalf("expected ErrResumeUnsupported, got %v", err)
	}
}

func TestGrokBackendsRegistered(t *testing.T) {
	img, err := provider.CreateImageBackend(grokProviderID, provider.BackendOptions{APIKey: "xai-key"})
	if err != nil {
		t.Fatalf("create image backend: %v", err)
	}
	if !img.Capabilities().Has(provider.CapTextToImage) || !img.Capabilities().Has(provider.CapImageToImage) {
		t.Fatalf("image caps should be both: %s", img.Capabilities())
	}
	vid, err := provider.CreateVideoBackend(grokProviderID, provider.BackendOptions{APIKey: "xai-key"})
	if err != nil {
		t.Fatalf("create video backend: %v", err)
	}
	if vid.VideoCapabilities().MaxReferenceImages != 7 {
		t.Fatalf("unexpected max reference images: %d", vid.VideoCapabilities().MaxReferenceImages)
	}
}
