package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/shotwise/shotwise/internal/provider"
)

// openAIImageServer 起一个假的 OpenAI 图片接口，记录收到的请求并回一张 1x1 PNG。
func openAIImageServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, func() string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	return srv, func() string { return srv.URL }
}

func pngB64() string {
	// 最小合法 PNG（1x1 透明）。
	return base64.StdEncoding.EncodeToString([]byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4,
		0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44,
		0xAE, 0x42, 0x60, 0x82,
	})
}

func TestOpenAIImageGenerateTextToImage(t *testing.T) {
	var gotPath string
	var payload map[string]any
	srv, url := openAIImageServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"b64_json": pngB64()}},
			"usage": map[string]any{
				"input_tokens":          120,
				"output_tokens":         800,
				"input_tokens_details":  map[string]any{"text_tokens": 120},
				"output_tokens_details": map[string]any{"image_tokens": 800},
			},
		})
	})

	backend, err := NewOpenAIImageBackend(provider.BackendOptions{
		Model:  "gpt-image-2",
		APIKey: "sk-test",
		BaseURL: func() string {
			_ = url
			return srv.URL
		}(),
	})
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
	if gotPath != "/images/generations" {
		t.Fatalf("expected generations endpoint, got %s", gotPath)
	}
	// 比例优先：9:16 + 2K 短边 1440 → t=round(1440/(16*9))=10 → 1440x2560。
	if payload["size"] != "1440x2560" {
		t.Fatalf("unexpected size: %v", payload["size"])
	}
	if payload["quality"] != "high" {
		t.Fatalf("unexpected quality: %v", payload["quality"])
	}
	if payload["n"] != float64(1) {
		t.Fatalf("n should be 1, got %v", payload["n"])
	}
	if res.Provider != ProviderOpenAI || res.Model != "gpt-image-2" {
		t.Fatalf("unexpected identity: %s/%s", res.Provider, res.Model)
	}
	if res.Quality != "high" {
		t.Fatalf("unexpected result quality: %s", res.Quality)
	}
	if res.TextInputTokens == nil || *res.TextInputTokens != 120 {
		t.Fatalf("text input tokens not parsed: %v", res.TextInputTokens)
	}
	if res.ImageOutputTokens == nil || *res.ImageOutputTokens != 800 {
		t.Fatalf("image output tokens not parsed: %v", res.ImageOutputTokens)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("output file missing: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("output file is empty")
	}
	if res.ImagePath != out {
		t.Fatalf("unexpected image path: %s", res.ImagePath)
	}
}

func TestOpenAIImageGenerateImageToImage(t *testing.T) {
	var gotPath, contentType string
	var fieldModel, fieldSize string
	var fileCount int
	srv, _ := openAIImageServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		contentType = r.Header.Get("Content-Type")
		mediaType, params, err := mime.ParseMediaType(contentType)
		if err == nil && mediaType == "multipart/form-data" {
			reader := multipart.NewReader(r.Body, params["boundary"])
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					break
				}
				if part.FormName() == "image" {
					fileCount++
				}
				if part.FormName() == "model" {
					raw, _ := io.ReadAll(part)
					fieldModel = string(raw)
				}
				if part.FormName() == "size" {
					raw, _ := io.ReadAll(part)
					fieldSize = string(raw)
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"b64_json": pngB64()}}})
	})

	refPath := filepath.Join(t.TempDir(), "ref.png")
	if err := os.WriteFile(refPath, []byte("fake-png-bytes"), 0o644); err != nil {
		t.Fatalf("write ref: %v", err)
	}
	backend, err := NewOpenAIImageBackend(provider.BackendOptions{Model: "gpt-image-2", APIKey: "sk", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.png")
	if _, err = backend.Generate(context.Background(), provider.ImageGenerationRequest{
		Prompt:          "make it night",
		OutputPath:      out,
		AspectRatio:     "16:9",
		ReferenceImages: []provider.ReferenceImage{{Path: refPath}},
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotPath != "/images/edits" {
		t.Fatalf("expected edits endpoint, got %s", gotPath)
	}
	if fileCount != 1 {
		t.Fatalf("expected 1 reference file part, got %d", fileCount)
	}
	if fieldModel != "gpt-image-2" {
		t.Fatalf("unexpected model field: %q", fieldModel)
	}
	if fieldSize != "1280x720" {
		t.Fatalf("unexpected size field: %q", fieldSize)
	}
}

func TestOpenAIImageCapabilityGate(t *testing.T) {
	backend, err := NewOpenAIImageBackend(provider.BackendOptions{
		Model: "gpt-image-2", APIKey: "sk", Extra: map[string]string{"mode": "edits_only"},
	})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	_, err = backend.Generate(context.Background(), provider.ImageGenerationRequest{
		Prompt: "a cat", OutputPath: filepath.Join(t.TempDir(), "out.png"), AspectRatio: "1:1",
	})
	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("expected capability error, got %v", err)
	}
	if capErr.Code != provider.FailureImageEndpointMismatchNoT2I {
		t.Fatalf("unexpected code: %s", capErr.Code)
	}
}

func TestOpenAIImageUpstreamErrorClassification(t *testing.T) {
	srv, _ := openAIImageServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	backend, err := NewOpenAIImageBackend(provider.BackendOptions{Model: "gpt-image-2", APIKey: "sk", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	_, err = backend.Generate(context.Background(), provider.ImageGenerationRequest{
		Prompt: "a cat", OutputPath: filepath.Join(t.TempDir(), "out.png"), AspectRatio: "1:1",
	})
	var statusErr *provider.HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != 500 {
		t.Fatalf("expected HTTPStatusError(500), got %v", err)
	}
	if !provider.SubmitRetryable(err) {
		t.Fatal("5xx should be retryable on submit")
	}
}

func TestOpenAIImageEmptyDataFails(t *testing.T) {
	srv, _ := openAIImageServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
	})
	backend, err := NewOpenAIImageBackend(provider.BackendOptions{Model: "gpt-image-2", APIKey: "sk", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	_, err = backend.Generate(context.Background(), provider.ImageGenerationRequest{
		Prompt: "a cat", OutputPath: filepath.Join(t.TempDir(), "out.png"), AspectRatio: "1:1",
	})
	if err == nil {
		t.Fatal("empty data should fail")
	}
	if provider.SubmitRetryable(err) {
		t.Fatal("empty data must not be retried")
	}
}

func TestOpenAIImageRegisteredInRegistry(t *testing.T) {
	found := false
	for _, key := range provider.RegisteredImageBackendKeys() {
		if key == "openai" {
			found = true
		}
	}
	if !found {
		t.Fatal("openai image backend should be registered")
	}
	backend, err := provider.CreateImageBackend("openai", provider.BackendOptions{Model: "gpt-image-2", APIKey: "sk"})
	if err != nil {
		t.Fatalf("create from registry: %v", err)
	}
	if !backend.Capabilities().Has(provider.CapTextToImage) || !backend.Capabilities().Has(provider.CapImageToImage) {
		t.Fatalf("default mode should support both: %s", backend.Capabilities())
	}
}
