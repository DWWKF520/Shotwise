package media

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/media"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit/httpkittest"
)

func TestRegisterJianyingAssemblyRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterJianyingAssembly(r.Group("/api/v1"), JianyingAssemblyDeps{})

	registered := map[string]bool{}
	for _, route := range r.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	want := http.MethodGet + " /api/v1/assembly-plans/:plan_id/jianying-draft"
	if !registered[want] {
		t.Errorf("route not registered: %s", want)
	}
}

// newJianyingHandlerEnv 构造真实服务 + DB + 项目根，并创建一份可导出的计划。
// 返回引擎、planID、项目根目录。
func newJianyingHandlerEnv(t *testing.T) (*gin.Engine, string, string) {
	t.Helper()
	gdb := httpkittest.NewJianyingHTTPTestDB(t)
	root := t.TempDir()
	store := service.NewProjectStore(root)
	dir := filepath.Join(root, "p1")
	if err := os.MkdirAll(filepath.Join(dir, "videos"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	video := []byte("clip-bytes")
	if err := os.WriteFile(filepath.Join(dir, "videos", "E1U1.mp4"), video, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sha := sha256.Sum256(video)
	manifest := map[string]any{
		"contract_version": "episode-media-manifest/v1",
		"persistence":      "read_only",
		"script":           "episode_1.json",
		"episode":          1,
		"content_mode":     "drama",
		"generation_mode":  "storyboard",
		"collection":       "scenes",
		"fingerprint":      "manifest-fingerprint",
		"items": []any{map[string]any{
			"order": 1, "unit_id": "E1U1", "planned_duration_seconds": 5.0,
			"source":           map[string]any{"path": "videos/E1U1.mp4", "kind": "generated_video"},
			"media_probe":      map[string]any{"exists": true, "probe_available": false, "has_video_stream": nil},
			"file_fingerprint": map[string]any{"exists": true, "sha256": hex.EncodeToString(sha[:])},
		}},
	}
	timeline := []map[string]any{{
		"id": "E1U1:0", "source_unit_id": "E1U1", "order": 0, "kind": "video_unit",
		"source_ref": "videos/E1U1.mp4", "duration_seconds": 5.0,
		"trim_start_seconds": 0.0, "trim_end_seconds": 0.0,
		"audio_policy": "duck", "transition": nil,
	}}
	assembly := media.NewMediaAssemblyService(gdb)
	payload, err := assembly.CreatePlan(context.Background(), media.CreatePlanParams{
		UserID: repository.DefaultUserID, ProjectName: "p1", Name: "plan", Scope: "episode",
		SourceManifest: manifest, Timeline: timeline,
		Audio:     map[string]any{"tracks": []any{}},
		Subtitle:  map[string]any{"cues": []any{}},
		Packaging: map[string]any{"cover": map[string]any{"enabled": false}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	planID, _ := payload["id"].(string)

	r := gin.New()
	RegisterJianyingAssembly(r.Group("/api/v1"), JianyingAssemblyDeps{
		Exports: media.NewJianyingExportService(gdb, store),
	})
	return r, planID, dir
}

func TestJianyingDraftHandlerServesZip(t *testing.T) {
	r, planID, _ := newJianyingHandlerEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/assembly-plans/"+planID+"/jianying-draft", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/zip") {
		t.Errorf("content-type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, planID) {
		t.Errorf("content-disposition = %q", cd)
	}
	reader, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	names := map[string]bool{}
	for _, f := range reader.File {
		names[f.Name] = true
	}
	for _, want := range []string{"manifest.json", "media/001-clip.mp4", "subtitles.srt", "timeline.json"} {
		if !names[want] {
			t.Errorf("zip missing %s", want)
		}
	}
}

func TestJianyingDraftHandlerNotFound(t *testing.T) {
	r, _, _ := newJianyingHandlerEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/assembly-plans/missing/jianying-draft", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "assembly_plan_not_found") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestJianyingDraftHandlerSourceMissing(t *testing.T) {
	r, planID, dir := newJianyingHandlerEnv(t)
	if err := os.Remove(filepath.Join(dir, "videos", "E1U1.mp4")); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/assembly-plans/"+planID+"/jianying-draft", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "source_file_missing") {
		t.Errorf("body = %s", rec.Body.String())
	}
}
