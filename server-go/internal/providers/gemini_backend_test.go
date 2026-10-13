package providers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/shotwise/shotwise/internal/provider"
)

// geminiJSONResponse 生成一张图片的 generateContent 响应体。
func geminiImageJSON() map[string]any {
	return map[string]any{
		"candidates": []map[string]any{
			{"content": map[string]any{"parts": []map[string]any{
				{"inlineData": map[string]any{"mimeType": "image/png", "data": pngB64()}},
			}}},
		},
	}
}

func geminiDecodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode body: %v (raw=%s)", err, raw)
	}
	return payload
}

// geminiVertexHarness 起一个离线 Vertex 环境：/token 换 access token，其余路径交给 apiHandler。
func geminiVertexHarness(t *testing.T, apiHandler http.HandlerFunc) (serverURL, credPath, accessToken string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	const token = "tok-vertex"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "expires_in": 3600})
			return
		}
		apiHandler(w, r)
	}))
	t.Cleanup(srv.Close)

	creds, _ := json.Marshal(map[string]any{
		"project_id":   "test-project",
		"client_email": "sa@test.iam.gserviceaccount.com",
		"private_key":  string(pemBytes),
		"token_uri":    srv.URL + "/token",
	})
	credPath = filepath.Join(t.TempDir(), "vertex_credentials.json")
	if err := os.WriteFile(credPath, creds, 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	return srv.URL, credPath, token
}

func TestGeminiImageAistudioHappyPath(t *testing.T) {
	var gotPath, gotKey string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		payload = geminiDecodeBody(t, r)
		_ = json.NewEncoder(w).Encode(geminiImageJSON())
	}))
	t.Cleanup(srv.Close)

	backend, err := newGeminiImageBackend(provider.BackendOptions{
		APIKey:  "k-ai",
		BaseURL: srv.URL,
		Extra:   map[string]string{"image_model": "gemini-3.1-flash-image-preview"},
	})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	refPath := filepath.Join(t.TempDir(), "hero.png")
	if err := os.WriteFile(refPath, []byte("ref-bytes"), 0o644); err != nil {
		t.Fatalf("write ref: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.png")
	result, err := backend.Generate(context.Background(), provider.ImageGenerationRequest{
		Prompt:          "a castle",
		OutputPath:      out,
		AspectRatio:     "16:9",
		ImageSize:       "2K",
		ReferenceImages: []provider.ReferenceImage{{Path: refPath, Label: "Hero"}},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotPath != "/v1beta/models/gemini-3.1-flash-image-preview:generateContent" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if gotKey != "k-ai" {
		t.Fatalf("unexpected api key header: %q", gotKey)
	}

	// 断言 contents 形状：[标签, 图片, prompt]。
	contents := payload["contents"].([]any)[0].(map[string]any)
	parts := contents["parts"].([]any)
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d: %v", len(parts), parts)
	}
	if text := parts[0].(map[string]any)["text"]; text != "Hero" {
		t.Fatalf("expected label text, got %v", text)
	}
	inline := parts[1].(map[string]any)["inlineData"].(map[string]any)
	if inline["mimeType"] != "image/png" || inline["data"] == "" {
		t.Fatalf("unexpected inline image: %v", inline)
	}
	if text := parts[2].(map[string]any)["text"]; text != "a castle" {
		t.Fatalf("expected prompt last, got %v", text)
	}

	genConfig := payload["generationConfig"].(map[string]any)
	if modes := genConfig["responseModalities"].([]any); modes[0] != "IMAGE" {
		t.Fatalf("unexpected response modalities: %v", modes)
	}
	imageConfig := genConfig["imageConfig"].(map[string]any)
	if imageConfig["aspectRatio"] != "16:9" || imageConfig["imageSize"] != "2K" {
		t.Fatalf("unexpected image config: %v", imageConfig)
	}

	if result.Provider != "gemini" || result.Model != "gemini-3.1-flash-image-preview" {
		t.Fatalf("unexpected identity: %s/%s", result.Provider, result.Model)
	}
	if info, err := os.Stat(out); err != nil || info.Size() == 0 {
		t.Fatalf("output not written: %v", err)
	}
}

func TestGeminiImageVertexOAuth(t *testing.T) {
	var gotPath, gotAuth string
	serverURL, credPath, token := geminiVertexHarness(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(geminiImageJSON())
	})

	backend, err := newGeminiImageBackend(provider.BackendOptions{
		BaseURL: serverURL,
		Model:   "gemini-3.1-flash-image-preview",
		Extra:   map[string]string{"backend_type": "vertex", "credentials_path": credPath},
	})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	if backend.Name() != "gemini-vertex" {
		t.Fatalf("unexpected name: %s", backend.Name())
	}
	out := filepath.Join(t.TempDir(), "out.png")
	if _, err := backend.Generate(context.Background(), provider.ImageGenerationRequest{
		Prompt: "a dog", OutputPath: out, AspectRatio: "1:1",
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	wantPath := "/v1beta1/projects/test-project/locations/global/publishers/google/models/gemini-3.1-flash-image-preview:generateContent"
	if gotPath != wantPath {
		t.Fatalf("unexpected path:\n got %s\nwant %s", gotPath, wantPath)
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("unexpected auth header: %q", gotAuth)
	}
	if info, err := os.Stat(out); err != nil || info.Size() == 0 {
		t.Fatalf("output not written: %v", err)
	}
}

func TestGeminiVideoAistudioGenerate(t *testing.T) {
	var gotPath string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v.mp4" {
			_, _ = w.Write([]byte("fake-video"))
			return
		}
		gotPath = r.URL.Path
		payload = geminiDecodeBody(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "models/veo/operations/op1",
			"done": true,
			"response": map[string]any{
				"generateVideoResponse": map[string]any{
					"generatedSamples": []map[string]any{{"video": map[string]any{"uri": serverURL(r)}}},
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	backend, err := newGeminiVideoBackend(provider.BackendOptions{
		APIKey:  "k-ai",
		BaseURL: srv.URL,
		Extra:   map[string]string{"video_model": "veo-3.1-lite-generate-preview"},
	})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	req := provider.NewVideoGenerationRequest("a cat", filepath.Join(t.TempDir(), "out.mp4"))
	req.DurationSeconds = 8
	req.Resolution = "1080p"
	result, err := backend.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotPath != "/v1beta/models/veo-3.1-lite-generate-preview:predictLongRunning" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	instance := payload["instances"].([]any)[0].(map[string]any)
	if instance["prompt"] != "a cat" {
		t.Fatalf("unexpected prompt: %v", instance["prompt"])
	}
	params := payload["parameters"].(map[string]any)
	if params["aspectRatio"] != "9:16" || params["durationSeconds"] != float64(8) || params["resolution"] != "1080p" {
		t.Fatalf("unexpected parameters: %v", params)
	}
	if _, ok := params["generateAudio"]; ok {
		t.Fatalf("aistudio 不应下发 generateAudio: %v", params)
	}
	if result.Provider != "gemini" || result.GenerateAudio == nil || !*result.GenerateAudio {
		t.Fatalf("unexpected result: %+v", result)
	}
	if data, err := os.ReadFile(req.OutputPath); err != nil || string(data) != "fake-video" {
		t.Fatalf("video not downloaded: %v %q", err, data)
	}
}

func TestGeminiVideoVertexGenerate(t *testing.T) {
	var gotAuth, gotPath string
	var params map[string]any
	serverURL, credPath, token := geminiVertexHarness(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		payload := geminiDecodeBody(t, r)
		params = payload["parameters"].(map[string]any)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "projects/test-project/locations/global/operations/op1",
			"done": true,
			"response": map[string]any{
				"videos": []map[string]any{{
					"bytesBase64Encoded": base64.StdEncoding.EncodeToString([]byte("vertex-video")),
				}},
			},
		})
	})

	backend, err := newGeminiVideoBackend(provider.BackendOptions{
		BaseURL: serverURL,
		Extra:   map[string]string{"backend_type": "vertex", "credentials_path": credPath, "video_model": "veo-3.1-fast-generate-001"},
	})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	req := provider.NewVideoGenerationRequest("a cat", filepath.Join(t.TempDir(), "out.mp4"))
	req.DurationSeconds = 8
	if _, err := backend.Generate(context.Background(), req); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("unexpected auth header: %q", gotAuth)
	}
	if gotPath != "/v1beta1/projects/test-project/locations/global/publishers/google/models/veo-3.1-fast-generate-001:predictLongRunning" {
		t.Errorf("unexpected path: %s", gotPath)
	}
	if params["generateAudio"] != true {
		t.Errorf("vertex 应下发 generateAudio: %v", params)
	}
	if data, err := os.ReadFile(req.OutputPath); err != nil || string(data) != "vertex-video" {
		t.Fatalf("video not written from inline bytes: %v %q", err, data)
	}
}

func TestGeminiVideoDurationConstraintReject(t *testing.T) {
	backend, err := newGeminiVideoBackend(provider.BackendOptions{
		APIKey: "k-ai",
		Extra:  map[string]string{"video_model": "veo-3.1-lite-generate-preview"},
	})
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	// 参考图路径：lite 声明允许时长 [8]。
	req := provider.NewVideoGenerationRequest("x", filepath.Join(t.TempDir(), "out.mp4"))
	req.DurationSeconds = 5
	req.ReferenceImages = []string{"a.png"}
	_, err = backend.Generate(context.Background(), req)
	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) || capErr.Code != provider.FailureVideoReferenceImagesDurationUnsupported {
		t.Fatalf("expected reference image duration rejection, got %v", err)
	}
	// 分辨率路径：1080p 只允许 8 秒。
	req = provider.NewVideoGenerationRequest("x", filepath.Join(t.TempDir(), "out.mp4"))
	req.DurationSeconds = 4
	req.Resolution = "1080p"
	_, err = backend.Generate(context.Background(), req)
	if !errors.As(err, &capErr) || capErr.Code != provider.FailureVideoResolutionDurationUnsupported {
		t.Fatalf("expected resolution duration rejection, got %v", err)
	}
}

func TestGeminiBackendsRegistered(t *testing.T) {
	caps, err := provider.VideoCapabilitiesForModel("gemini", "veo-3.1-lite-generate-preview")
	if err != nil {
		t.Fatalf("video capabilities: %v", err)
	}
	if !caps.FirstFrame || !caps.LastFrame || caps.MaxReferenceImages != 3 {
		t.Fatalf("unexpected capabilities: %+v", caps)
	}

	foundImage, foundVideo := false, false
	for _, key := range provider.RegisteredImageBackendKeys() {
		if key == "gemini" {
			foundImage = true
		}
	}
	for _, key := range provider.RegisteredVideoBackendKeys() {
		if key == "gemini" {
			foundVideo = true
		}
	}
	if !foundImage || !foundVideo {
		t.Fatalf("gemini should be registered: image=%v video=%v", foundImage, foundVideo)
	}

	backend, err := provider.CreateImageBackend("gemini", provider.BackendOptions{APIKey: "k-ai"})
	if err != nil {
		t.Fatalf("create image backend: %v", err)
	}
	if backend.Name() != "gemini-aistudio" || !backend.Capabilities().Has(provider.CapTextToImage) ||
		!backend.Capabilities().Has(provider.CapImageToImage) {
		t.Fatalf("unexpected image backend: %s %s", backend.Name(), backend.Capabilities())
	}
}

// serverURL 供 handler 自引用当前测试服务器地址。
func serverURL(r *http.Request) string { return "http://" + r.Host + "/v.mp4" }
