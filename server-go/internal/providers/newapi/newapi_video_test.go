package newapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/shotwise/shotwise/internal/provider"
)

// testVideoBytes 是假成片字节（内容无关，仅验证落盘链路）。
var testVideoBytes = []byte("fake-mp4-bytes")

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
		_, _ = w.Write(testVideoBytes)
	})

	backend, err := NewVideoBackend(provider.BackendOptions{
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
