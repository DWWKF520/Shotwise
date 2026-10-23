package vidu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/provider"
)

// viduTestServer 起一个假的 Vidu API：按路径分派创建 / 任务查询 / 产物下载，并记录请求。
func viduTestServer(t *testing.T, creationBody map[string]any) (*httptest.Server, *string, *string, *map[string]any) {
	t.Helper()
	path := new(string)
	auth := new(string)
	payload := new(map[string]any)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			*path = r.URL.Path
			*auth = r.Header.Get("Authorization")
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, payload)
			_ = json.NewEncoder(w).Encode(creationBody)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tasks/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state":     "success",
				"id":        "task",
				"credits":   7,
				"creations": []map[string]any{{"url": "http://" + r.Host + "/download.bin"}},
			})
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte("artifact-bytes"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, path, auth, payload
}

func TestViduImageGenerateHappyPath(t *testing.T) {
	srv, gotPath, gotAuth, gotPayload := viduTestServer(t, map[string]any{"task_id": "img-1", "credits": 8})

	backend, err := newViduImageBackend(provider.BackendOptions{APIKey: "test-key", BaseURL: srv.URL + "/ent/v2"})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.png")
	res, err := backend.Generate(context.Background(), provider.ImageGenerationRequest{
		Prompt:      "a cat",
		OutputPath:  out,
		AspectRatio: "9:16",
		ImageSize:   "2K",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if *gotPath != "/ent/v2/reference2image" {
		t.Fatalf("unexpected endpoint path: %s", *gotPath)
	}
	if *gotAuth != "Token test-key" {
		t.Fatalf("unexpected auth header: %q", *gotAuth)
	}
	if (*gotPayload)["model"] != "viduq2" {
		t.Fatalf("unexpected model: %v", (*gotPayload)["model"])
	}
	if (*gotPayload)["prompt"] != "a cat" {
		t.Fatalf("unexpected prompt: %v", (*gotPayload)["prompt"])
	}
	if (*gotPayload)["aspect_ratio"] != "9:16" {
		t.Fatalf("unexpected aspect_ratio: %v", (*gotPayload)["aspect_ratio"])
	}
	if (*gotPayload)["resolution"] != "2K" {
		t.Fatalf("unexpected resolution: %v", (*gotPayload)["resolution"])
	}
	if _, ok := (*gotPayload)["images"]; ok {
		t.Fatalf("T2I request must not carry images: %v", (*gotPayload)["images"])
	}
	if res.UsageTokens == nil || *res.UsageTokens != 7 {
		t.Fatalf("expected final credits usage 7, got %v", res.UsageTokens)
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() == 0 {
		t.Fatalf("output image missing or empty: %v", err)
	}
}

func TestViduVideoGenerateHappyPathAndResumeUnsupported(t *testing.T) {
	srv, gotPath, gotAuth, gotPayload := viduTestServer(t, map[string]any{"task_id": "vid-1", "credits": 12})

	backend, err := NewVideoBackend(provider.BackendOptions{APIKey: "test-key", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	req := provider.NewVideoGenerationRequest("a dog running", filepath.Join(t.TempDir(), "out.mp4"))
	res, err := backend.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if *gotPath != "/text2video" {
		t.Fatalf("unexpected endpoint path: %s", *gotPath)
	}
	if *gotAuth != "Token test-key" {
		t.Fatalf("unexpected auth header: %q", *gotAuth)
	}
	if (*gotPayload)["model"] != "viduq3-turbo" {
		t.Fatalf("unexpected model: %v", (*gotPayload)["model"])
	}
	if (*gotPayload)["duration"] != float64(5) {
		t.Fatalf("unexpected duration: %v", (*gotPayload)["duration"])
	}
	if (*gotPayload)["resolution"] != "720p" {
		t.Fatalf("unexpected resolution fallback: %v", (*gotPayload)["resolution"])
	}
	if (*gotPayload)["audio"] != true {
		t.Fatalf("q3 model should send audio=true: %v", (*gotPayload)["audio"])
	}
	if res.DurationSeconds != 5 || res.TaskID != "vid-1" {
		t.Fatalf("unexpected result: %+v", res)
	}

	if _, err := backend.ResumeVideo(context.Background(), "vid-1", req); !errors.Is(err, provider.ErrResumeUnsupported) {
		t.Fatalf("ResumeVideo must be unsupported, got %v", err)
	}
}

func TestViduVideoCapabilitiesForModel(t *testing.T) {
	// viduq3 只支持参考生视频端点，且是「专供 r2v」的 model：prompt 上限取更窄值。
	caps := VideoCapabilitiesForModel("viduq3")
	if caps.FirstFrame || caps.LastFrame {
		t.Fatalf("viduq3 must not support frames: %+v", caps)
	}
	if caps.MaxReferenceImages != 7 {
		t.Fatalf("viduq3 should allow 7 reference images, got %d", caps.MaxReferenceImages)
	}
	if caps.MaxPromptChars == nil || *caps.MaxPromptChars != viduVideoPromptMaxR2V {
		t.Fatalf("viduq3 should declare narrow prompt cap, got %v", caps.MaxPromptChars)
	}

	// viduq2 是多端点 model，静态声明取宽值。
	caps = VideoCapabilitiesForModel("viduq2")
	if caps.MaxPromptChars == nil || *caps.MaxPromptChars != viduVideoPromptMaxT2V {
		t.Fatalf("viduq2 should declare wide prompt cap, got %v", caps.MaxPromptChars)
	}
}
