package media

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/service"
)

func TestRegisterMediaAssetsRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterMediaAssets(r.Group("/api/v1"), MediaAssetsDeps{})

	registered := map[string]bool{}
	for _, route := range r.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	want := []string{
		http.MethodGet + " /api/v1/projects/:name/media-assets",
		http.MethodPost + " /api/v1/projects/:name/media-assets/upload",
		http.MethodPost + " /api/v1/projects/:name/media-assets/scan",
		http.MethodPost + " /api/v1/projects/:name/media-assets/reconciliation/retry",
		http.MethodGet + " /api/v1/projects/:name/media-assets/content/:media_asset_id",
		http.MethodGet + " /api/v1/projects/:name/media-assets/:media_asset_id",
		http.MethodPost + " /api/v1/projects/:name/media-assets/:media_asset_id/bindings",
		http.MethodPatch + " /api/v1/projects/:name/media-assets/:media_asset_id/archive",
		http.MethodDelete + " /api/v1/projects/:name/media-assets/:media_asset_id",
	}
	for _, route := range want {
		if !registered[route] {
			t.Errorf("route not registered: %s", route)
		}
	}
}

func newMediaAssetsRouter(t *testing.T) (*gin.Engine, *service.ProjectStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	store := service.NewProjectStore(root)
	if _, err := store.Create("demo", "Demo", project.ContentDrama, project.GenerationStoryboard, "写实"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	r := gin.New()
	RegisterMediaAssets(r.Group("/api/v1"), MediaAssetsDeps{Store: store})
	return r, store
}

func TestMediaAssetsScanHandler(t *testing.T) {
	t.Setenv("SHOTWISE_MEDIA_ASSET_INDEX", "1")
	r, store := newMediaAssetsRouter(t)
	dir, err := store.Dir("demo")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	if err := writeFile(filepath.Join(dir, "uploads", "u.png"), "u"); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/demo/media-assets/scan?dry_run=true", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["dry_run"] != true {
		t.Errorf("dry_run = %v", body["dry_run"])
	}
}

func TestMediaAssetsProjectNotFound(t *testing.T) {
	r, _ := newMediaAssetsRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/missing/media-assets", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestMediaAssetsUploadHandler(t *testing.T) {
	t.Setenv("SHOTWISE_MEDIA_ASSET_INDEX", "1")
	r, _ := newMediaAssetsRouter(t)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "clip.png")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := part.Write([]byte("png-bytes")); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/demo/media-assets/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var asset map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &asset); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if asset["origin"] != "upload" {
		t.Errorf("origin = %v, want upload", asset["origin"])
	}
	if asset["kind"] != "image" {
		t.Errorf("kind = %v, want image", asset["kind"])
	}
}

func TestMediaAssetsUploadUnsupported(t *testing.T) {
	r, _ := newMediaAssetsRouter(t)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "notes.txt")
	_, _ = part.Write([]byte("text"))
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/demo/media-assets/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
}

func TestParseQueryBool(t *testing.T) {
	trueCases := []string{"1", "true", "ON", "yes"}
	for _, raw := range trueCases {
		got, err := parseQueryBool(raw)
		if err != nil || !got {
			t.Errorf("parseQueryBool(%q) = (%v, %v), want true", raw, got, err)
		}
	}
	falseCases := []string{"0", "false", "off", "no"}
	for _, raw := range falseCases {
		got, err := parseQueryBool(raw)
		if err != nil || got {
			t.Errorf("parseQueryBool(%q) = (%v, %v), want false", raw, got, err)
		}
	}
	if _, err := parseQueryBool("maybe"); err == nil {
		t.Error("expected error for invalid boolean")
	}
}

func TestMediaContentPath(t *testing.T) {
	root := filepath.Join(string(filepath.Separator)+"data", "projects", "demo")
	if _, ok := mediaContentPath(root, filepath.Join(root, "uploads", "a.png")); !ok {
		t.Error("expected in-root path to be allowed")
	}
	outside := filepath.Join(string(filepath.Separator)+"data", "other", "a.png")
	if _, ok := mediaContentPath(root, outside); ok {
		t.Error("expected out-of-root path to be rejected")
	}
	if _, ok := mediaContentPath(root, ""); ok {
		t.Error("expected empty path to be rejected")
	}
}

func TestContentDisposition(t *testing.T) {
	if got := contentDisposition("a.png"); got != `attachment; filename="a.png"` {
		t.Errorf("ascii dispos = %q", got)
	}
	got := contentDisposition("图.png")
	if !bytes.Contains([]byte(got), []byte("filename*=UTF-8''")) {
		t.Errorf("non-ascii dispos should carry filename*: %q", got)
	}
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
