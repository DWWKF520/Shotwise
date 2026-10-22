package media

import "github.com/shotwise/shotwise/internal/service"

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/repository"
)

// newJianyingExportEnv 准备真实 DB + 项目根目录，返回导出服务与项目根。
func newJianyingExportEnv(t *testing.T) (*JianyingExportService, string, *MediaAssemblyService) {
	t.Helper()
	gdb := newAssemblyPlanTestDB(t)
	root := t.TempDir()
	store := service.NewProjectStore(root)
	dir := filepath.Join(root, "p1")
	if err := os.MkdirAll(filepath.Join(dir, "videos"), 0o755); err != nil {
		t.Fatalf("mkdir videos: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "audio"), 0o755); err != nil {
		t.Fatalf("mkdir audio: %v", err)
	}
	videoBytes := []byte("video-bytes-0001")
	coverBytes := []byte("cover-bytes")
	audioBytes := []byte("audio-bytes")
	if err := os.WriteFile(filepath.Join(dir, "videos", "E1U1.mp4"), videoBytes, 0o644); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cover.jpg"), coverBytes, 0o644); err != nil {
		t.Fatalf("seed cover: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "audio", "a1.mp3"), audioBytes, 0o644); err != nil {
		t.Fatalf("seed audio: %v", err)
	}
	videoSHA := sha256.Sum256(videoBytes)
	manifest := assemblyTestManifest()
	items := manifest["items"].([]any)
	items[0].(map[string]any)["file_fingerprint"] = map[string]any{
		"exists": true, "sha256": hex.EncodeToString(videoSHA[:]),
	}

	assembly := NewMediaAssemblyService(gdb)
	payload, err := assembly.CreatePlan(context.Background(), CreatePlanParams{
		UserID:         repository.DefaultUserID,
		ProjectName:    "p1",
		Name:           "jianying export plan",
		Scope:          "episode",
		EpisodeNumber:  nil,
		SourceManifest: manifest,
		Timeline:       assemblyTestTimeline(),
		Audio: map[string]any{
			"unit_audio_policy": "duck",
			"tracks": []any{map[string]any{
				"id": "a1", "kind": "music", "source_ref": "audio/a1.mp3",
				"start_seconds": 0.0, "volume": 0.8,
			}},
			"ducking_db": -12.0,
		},
		Subtitle: map[string]any{
			"mode": "burn_in", "source": "script",
			"cues": []any{
				map[string]any{"id": "c1", "start_seconds": 0.0, "end_seconds": 2.0, "text": "第一句"},
				map[string]any{"id": "c2", "start_seconds": 2.0, "end_seconds": 4.0, "text": "第二句"},
			},
		},
		Packaging: map[string]any{
			"cover": map[string]any{"enabled": true, "source_ref": "cover.jpg", "duration_seconds": 2.0},
		},
		OutputProfile: map[string]any{"format": "mp4"},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	_ = payload

	exports := NewJianyingExportService(gdb, store)
	return exports, dir, assembly
}

func readZipEntries(t *testing.T, path string) map[string][]byte {
	t.Helper()
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer reader.Close()
	out := map[string][]byte{}
	for _, f := range reader.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open entry %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read entry %s: %v", f.Name, err)
		}
		out[f.Name] = data
	}
	return out
}

func TestJianyingExportBuildsBundle(t *testing.T) {
	exports, _, assembly := newJianyingExportEnv(t)
	plans, err := assembly.ListPlans(context.Background(), repository.DefaultUserID, "p1")
	if err != nil || len(plans) != 1 {
		t.Fatalf("list plans: %v (len=%d)", err, len(plans))
	}
	planID, _ := plans[0]["id"].(string)

	bundle, err := exports.ExportCurrentRevision(context.Background(), planID, repository.DefaultUserID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	defer os.RemoveAll(bundle.TempDir)
	if bundle.RevisionNumber != 1 {
		t.Errorf("revision = %d, want 1", bundle.RevisionNumber)
	}
	if !strings.HasSuffix(bundle.Filename, "-r1.zip") || !strings.Contains(bundle.Filename, planID) {
		t.Errorf("filename = %q", bundle.Filename)
	}

	entries := readZipEntries(t, bundle.Path)
	for _, name := range []string{
		"README.txt", "manifest.json", "media/001-clip.mp4", "media/audio-001.mp3",
		"media/cover.jpg", "subtitles.srt", "subtitles.vtt", "timeline.json",
	} {
		if _, ok := entries[name]; !ok {
			t.Errorf("zip missing entry %s (have %v)", name, keysOf(entries))
		}
	}
	if string(entries["media/001-clip.mp4"]) != "video-bytes-0001" {
		t.Errorf("clip content mismatch")
	}

	var manifest map[string]any
	if err := json.Unmarshal(entries["manifest.json"], &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest["schema_version"] != "shotwise-jianying-draft/v1" {
		t.Errorf("schema_version = %v", manifest["schema_version"])
	}
	if manifest["plan_id"] != planID {
		t.Errorf("plan_id = %v", manifest["plan_id"])
	}
	if manifest["revision_number"] != float64(1) {
		t.Errorf("revision_number = %v", manifest["revision_number"])
	}
	assets, _ := manifest["assets"].([]any)
	if len(assets) != 3 {
		t.Fatalf("assets = %d, want 3", len(assets))
	}
	roles := map[string]bool{}
	for _, raw := range assets {
		asset, _ := raw.(map[string]any)
		roles[asset["role"].(string)] = true
		if asset["fingerprint"] == "" || asset["size_bytes"] == nil {
			t.Errorf("asset fingerprint/size missing: %v", asset)
		}
	}
	if !roles["video"] || !roles["audio"] || !roles["cover"] {
		t.Errorf("roles = %v", roles)
	}
	timeline, _ := manifest["timeline"].([]any)
	first, _ := timeline[0].(map[string]any)
	if first["source_ref"] != "media/001-clip.mp4" || first["media_path"] != "media/001-clip.mp4" {
		t.Errorf("timeline rewrite = %v", first)
	}
	subtitle, _ := manifest["subtitle"].(map[string]any)
	if subtitle["cue_count"] != float64(2) {
		t.Errorf("cue_count = %v", subtitle["cue_count"])
	}
	audioSection, _ := manifest["audio"].(map[string]any)
	tracks, _ := audioSection["tracks"].([]any)
	track, _ := tracks[0].(map[string]any)
	if track["source_ref"] != "media/audio-001.mp3" {
		t.Errorf("audio rewrite = %v", track)
	}
	if !strings.Contains(string(entries["subtitles.srt"]), "00:00:00,000 --> 00:00:02,000") {
		t.Errorf("srt = %q", entries["subtitles.srt"])
	}
	if !strings.HasPrefix(string(entries["subtitles.vtt"]), "WEBVTT") {
		t.Errorf("vtt head missing: %q", entries["subtitles.vtt"])
	}
}

func TestJianyingExportPlanNotFound(t *testing.T) {
	exports, _, _ := newJianyingExportEnv(t)
	_, err := exports.ExportCurrentRevision(context.Background(), "missing", repository.DefaultUserID)
	var notFound *AssemblyPlanNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestJianyingExportSourceFileChanged(t *testing.T) {
	gdb := newAssemblyPlanTestDB(t)
	root := t.TempDir()
	dir := filepath.Join(root, "p1")
	if err := os.MkdirAll(filepath.Join(dir, "videos"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "videos", "E1U1.mp4"), []byte("v1"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sha := sha256.Sum256([]byte("v1"))
	manifest := assemblyTestManifest()
	items := manifest["items"].([]any)
	items[0].(map[string]any)["file_fingerprint"] = map[string]any{
		"exists": true, "sha256": hex.EncodeToString(sha[:]),
	}
	assembly := NewMediaAssemblyService(gdb)
	if _, err := assembly.CreatePlan(context.Background(), CreatePlanParams{
		UserID: repository.DefaultUserID, ProjectName: "p1", Name: "n", Scope: "episode",
		SourceManifest: manifest, Timeline: assemblyTestTimeline(),
		Audio: map[string]any{"tracks": []any{}}, Subtitle: map[string]any{"cues": []any{}},
		Packaging: map[string]any{"cover": map[string]any{"enabled": false}},
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "videos", "E1U1.mp4"), []byte("v2"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	exports := NewJianyingExportService(gdb, service.NewProjectStore(root))
	plans, _ := assembly.ListPlans(context.Background(), repository.DefaultUserID, "p1")
	_, err := exports.ExportCurrentRevision(context.Background(), plans[0]["id"].(string), repository.DefaultUserID)
	var exportErr *JianyingExportError
	if !errors.As(err, &exportErr) {
		t.Fatalf("err = %v, want export error", err)
	}
	if exportErr.Code != "source_fingerprint_conflict" || exportErr.Status != http.StatusConflict {
		t.Errorf("code=%s status=%d", exportErr.Code, exportErr.Status)
	}
}

func TestJianyingExportSourceFileMissing(t *testing.T) {
	gdb := newAssemblyPlanTestDB(t)
	root := t.TempDir()
	dir := filepath.Join(root, "p1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	assembly := NewMediaAssemblyService(gdb)
	if _, err := assembly.CreatePlan(context.Background(), CreatePlanParams{
		UserID: repository.DefaultUserID, ProjectName: "p1", Name: "n", Scope: "episode",
		SourceManifest: assemblyTestManifest(), Timeline: assemblyTestTimeline(),
		Audio: map[string]any{"tracks": []any{}}, Subtitle: map[string]any{"cues": []any{}},
		Packaging: map[string]any{"cover": map[string]any{"enabled": false}},
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	exports := NewJianyingExportService(gdb, service.NewProjectStore(root))
	plans, _ := assembly.ListPlans(context.Background(), repository.DefaultUserID, "p1")
	_, err := exports.ExportCurrentRevision(context.Background(), plans[0]["id"].(string), repository.DefaultUserID)
	var exportErr *JianyingExportError
	if !errors.As(err, &exportErr) {
		t.Fatalf("err = %v, want export error", err)
	}
	if exportErr.Code != "source_file_missing" || exportErr.Status != http.StatusConflict {
		t.Errorf("code=%s status=%d", exportErr.Code, exportErr.Status)
	}
}

func TestJianyingExportRevisionConflicts(t *testing.T) {
	gdb := newAssemblyPlanTestDB(t)
	root := t.TempDir()
	assembly := NewMediaAssemblyService(gdb)
	dir := filepath.Join(root, "p1")
	if err := os.MkdirAll(filepath.Join(dir, "videos"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "videos", "E1U1.mp4"), []byte("v"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := assembly.CreatePlan(context.Background(), CreatePlanParams{
		UserID: repository.DefaultUserID, ProjectName: "p1", Name: "n", Scope: "episode",
		SourceManifest: assemblyTestManifest(), Timeline: assemblyTestTimeline(),
		Audio: map[string]any{"tracks": []any{}}, Subtitle: map[string]any{"cues": []any{}},
		Packaging: map[string]any{"cover": map[string]any{"enabled": false}},
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	exports := NewJianyingExportService(gdb, service.NewProjectStore(root))
	plans, _ := assembly.ListPlans(context.Background(), repository.DefaultUserID, "p1")
	planID := plans[0]["id"].(string)

	// current_revision_missing：删除 revision 行。
	if err := gdb.Exec("DELETE FROM assembly_plan_revisions WHERE plan_id = ?", planID).Error; err != nil {
		t.Fatalf("delete revisions: %v", err)
	}
	_, err := exports.ExportCurrentRevision(context.Background(), planID, repository.DefaultUserID)
	var conflict *AssemblyPlanConflictError
	if !errors.As(err, &conflict) || conflict.Code != "current_revision_missing" {
		t.Fatalf("err = %v, want current_revision_missing", err)
	}
	// revision_conflict 是「读 plan 与读 revision 之间指针被并发推进」的防御分支
	//（单线程下指针与查到的 revision 恒一致），不做单线程断言。
}

func TestJianyingExportSubtitleInvalid(t *testing.T) {
	gdb := newAssemblyPlanTestDB(t)
	root := t.TempDir()
	assembly := NewMediaAssemblyService(gdb)
	dir := filepath.Join(root, "p1")
	if err := os.MkdirAll(filepath.Join(dir, "videos"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "videos", "E1U1.mp4"), []byte("v"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := assembly.CreatePlan(context.Background(), CreatePlanParams{
		UserID: repository.DefaultUserID, ProjectName: "p1", Name: "n", Scope: "episode",
		SourceManifest: assemblyTestManifest(), Timeline: assemblyTestTimeline(),
		Audio: map[string]any{"tracks": []any{}}, Subtitle: map[string]any{"cues": []any{}},
		Packaging: map[string]any{"cover": map[string]any{"enabled": false}},
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	exports := NewJianyingExportService(gdb, service.NewProjectStore(root))
	plans, _ := assembly.ListPlans(context.Background(), repository.DefaultUserID, "p1")
	planID := plans[0]["id"].(string)

	// 直接改库里的 subtitle_json：塞一条带 NUL 的 cue（创建校验会拦，只能事后注入）。
	badSubtitle, _ := json.Marshal(map[string]any{
		"cues": []any{map[string]any{"start_seconds": 0, "end_seconds": 1, "text": "a\x00b"}},
	})
	if err := gdb.Exec(
		"UPDATE assembly_plan_revisions SET subtitle_json = ? WHERE plan_id = ?", string(badSubtitle), planID,
	).Error; err != nil {
		t.Fatalf("patch subtitle: %v", err)
	}
	_, err := exports.ExportCurrentRevision(context.Background(), planID, repository.DefaultUserID)
	var exportErr *JianyingExportError
	if !errors.As(err, &exportErr) || exportErr.Code != "subtitle_invalid" {
		t.Fatalf("err = %v, want subtitle_invalid", err)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
