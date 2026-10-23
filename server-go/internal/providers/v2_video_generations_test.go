package providers

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

// v2TestVideoBytes 是假成片字节（内容无关，仅验证落盘链路）。
var v2TestVideoBytes = []byte("fake-mp4-bytes")

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
		_, _ = w.Write(v2TestVideoBytes)
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
