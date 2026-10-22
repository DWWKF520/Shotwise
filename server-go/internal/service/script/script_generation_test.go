package script

import "github.com/shotwise/shotwise/internal/service"

import (
	"github.com/shotwise/shotwise/internal/service/reference"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
)

func TestParseNarrationStep1DraftAndValidate(t *testing.T) {
	segments, err := ParseNarrationStep1Draft("```json\n{\"segments\":[{\"segment_id\":\"E1S01\",\"novel_text\":\"他推开门。\"," +
		"\"duration_seconds\":4,\"characters_in_segment\":[\"林清\"],\"scenes\":[],\"props\":[]}]}\n```")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(segments) != 1 {
		t.Fatalf("want 1 segment, got %d", len(segments))
	}
	if err := ValidateNarrationSegments(segments, []int{4, 6, 8}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	// 时长不在档位内 fail loud。
	segments[0]["duration_seconds"] = 5
	if err := ValidateNarrationSegments(segments, []int{4, 6, 8}); err == nil {
		t.Fatal("want duration error")
	}
	// 空白 novel_text fail loud。
	segments[0]["duration_seconds"] = 4
	segments[0]["novel_text"] = "  "
	if err := ValidateNarrationSegments(segments, []int{4, 6, 8}); err == nil {
		t.Fatal("want blank novel_text error")
	}
}

func TestValidateNarrationAssetReferencesAndCoverage(t *testing.T) {
	segments := []map[string]any{
		{"segment_id": "E1S01", "novel_text": "他推开门。", "duration_seconds": 4, "characters_in_segment": []any{"林清"}, "scenes": []any{}, "props": []any{}},
		{"segment_id": "E1S02", "novel_text": "风灌了进来。", "duration_seconds": 4, "characters_in_segment": []any{}, "scenes": []any{"旧宅"}, "props": []any{}},
	}
	if err := ValidateNarrationAssetReferences(segments, []string{"林清"}, []string{"旧宅"}, nil); err != nil {
		t.Fatalf("asset refs: %v", err)
	}
	segments[1]["props"] = []any{"未登记道具"}
	if err := ValidateNarrationAssetReferences(segments, []string{"林清"}, []string{"旧宅"}, nil); err == nil {
		t.Fatal("want unregistered asset error")
	}
	segments[1]["props"] = []any{}
	if err := ValidateNarrationNovelTextCoverage(segments, "他推开门。风灌了进来。"); err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if err := ValidateNarrationNovelTextCoverage(segments, "他推开门。风吹来。"); err == nil {
		t.Fatal("want coverage mismatch error")
	}
}

func TestParseDramaNormalizedScript(t *testing.T) {
	durations := []int{4, 6, 8}
	valid := `{"title":"第一集","scenes":[{"scene_id":"E1S01","duration_seconds":"6","characters_in_scene":["林清"],` +
		`"scene_description":"他推门","utterances":[{"kind":"dialogue","speaker":"林清","text":"我回来了"},` +
		`{"kind":"voiceover","text":"风起"}],"source_text":"原文"}]}`
	content, err := ParseDramaNormalizedScript(valid, durations)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	scenes, _ := content["scenes"].([]any)
	if len(scenes) != 1 {
		t.Fatalf("want 1 scene, got %d", len(scenes))
	}
	scene := scenes[0].(map[string]any)
	if scene["duration_seconds"] != 6 {
		t.Fatalf("digit string duration should coerce to int, got %v", scene["duration_seconds"])
	}

	// 时长越界。
	if _, err := ParseDramaNormalizedScript(`{"title":"x","scenes":[{"scene_id":"E1S01","duration_seconds":5,"characters_in_scene":[],"scene_description":"d"}]}`, durations); err == nil {
		t.Fatal("want duration enum error")
	}
	// 不允许的字段。
	if _, err := ParseDramaNormalizedScript(`{"title":"x","scenes":[{"scene_id":"E1S01","duration_seconds":6,"characters_in_scene":[],"scene_description":"d","bogus":1}]}`, durations); err == nil {
		t.Fatal("want unknown field error")
	}
	// dialogue 缺 speaker。
	if _, err := ParseDramaNormalizedScript(`{"title":"x","scenes":[{"scene_id":"E1S01","duration_seconds":6,"characters_in_scene":[],"scene_description":"d","utterances":[{"kind":"dialogue","text":"hi"}]}]}`, durations); err == nil {
		t.Fatal("want dialogue speaker error")
	}
}

func TestMergeVisualLayers(t *testing.T) {
	step1 := []map[string]any{
		{"segment_id": "E1S01", "novel_text": "他推开门。", "duration_seconds": 4, "segment_break": false},
	}
	visual := map[string]any{"title": "第一集", "segments": []any{
		map[string]any{"segment_id": "E1S01", "image_prompt": map[string]any{"scene": "s"}, "video_prompt": map[string]any{"action": "a"}},
	}}
	merged, err := MergeNarrationVisual(step1, visual, 1)
	if err != nil {
		t.Fatalf("merge narration: %v", err)
	}
	segs, _ := merged["segments"].([]any)
	entry := segs[0].(map[string]any)
	if entry["novel_text"] != "他推开门。" || entry["image_prompt"] == nil {
		t.Fatalf("merge should keep step1 content and add visual layer: %v", entry)
	}

	// 缺覆盖 fail loud。
	if _, err := MergeNarrationVisual(step1, map[string]any{"title": "t", "segments": []any{}}, 1); err == nil {
		t.Fatal("want missing coverage error")
	}

	content := []map[string]any{{"scene_id": "E1S01", "scene_description": "d", "utterances": []any{}}}
	dramaVisual := []map[string]any{{"scene_id": "E1S01", "image_prompt": map[string]any{"scene": "s"}, "video_prompt": map[string]any{"action": "a"}}}
	scenes, err := MergeDramaVisualIntoScenes(content, dramaVisual)
	if err != nil {
		t.Fatalf("merge drama: %v", err)
	}
	scene := scenes[0].(map[string]any)
	if _, ok := scene["scene_description"]; ok {
		t.Fatal("scene_description is content-only and should be dropped")
	}
	if scene["image_prompt"] == nil {
		t.Fatal("visual layer should be merged in")
	}
}

func TestContentFingerprintIgnoresKeyOrder(t *testing.T) {
	a := map[string]any{"b": 1, "a": []any{1, 2}}
	b := map[string]any{"a": []any{1, 2}, "b": 1}
	if service.ContentFingerprintOfData(a) != service.ContentFingerprintOfData(b) {
		t.Fatal("fingerprint should be stable across key order")
	}
}

func TestReviewStatusAndConfirmation(t *testing.T) {
	dir := t.TempDir()
	episode := 1
	drafts := service.EpisodeDraftsDir(dir, episode)
	if err := os.MkdirAll(drafts, 0o755); err != nil {
		t.Fatal(err)
	}
	pf := &domain.ProjectFile{
		ContentMode: "narration",
		Episodes:    []domain.EpisodeMeta{{Episode: 1, ScriptFile: "scripts/episode_1.json"}},
	}
	if got := ReviewStatus(dir, pf, episode); got != ReviewStatusNoStep1 {
		t.Fatalf("want no_step1, got %s", got)
	}
	name, _ := Step1Filename("narration")
	step1Path := filepath.Join(drafts, name)
	raw := `{"segments":[{"segment_id":"E1S01","novel_text":"x","duration_seconds":4,` +
		`"characters_in_segment":[],"scenes":[],"props":[]}]}`
	if err := os.WriteFile(step1Path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReviewStatus(dir, pf, episode); got != ReviewStatusPendingReview {
		t.Fatalf("want pending_review, got %s", got)
	}
	if !GateBlocksStep2(dir, pf, episode) {
		t.Fatal("gate should block before confirmation")
	}
	var content map[string]any
	if err := json.Unmarshal([]byte(raw), &content); err != nil {
		t.Fatal(err)
	}
	fingerprint := service.ContentFingerprintOfData(content)
	if !service.ApplyConfirmation(pf, episode, fingerprint, "2026-01-01T00:00:00Z") {
		t.Fatal("apply confirmation should succeed")
	}
	if got := ReviewStatus(dir, pf, episode); got != ReviewStatusConfirmed {
		t.Fatalf("want confirmed, got %s", got)
	}
	// step1 内容改动 → 指纹漂移 → 回到 pending。
	updated := `{"segments":[{"segment_id":"E1S01","novel_text":"x 改","duration_seconds":4,` +
		`"characters_in_segment":[],"scenes":[],"props":[]}]}`
	if err := os.WriteFile(step1Path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReviewStatus(dir, pf, episode); got != ReviewStatusPendingReview {
		t.Fatalf("want pending after drift, got %s", got)
	}
}

func TestQuarantineRoundTrip(t *testing.T) {
	dir := t.TempDir()
	episode := 2
	content := map[string]any{"units": []any{map[string]any{"text": "镜头1：他推开门"}}}
	violations := []reference.DraftViolation{{Code: "schema_invalid", Label: "unit E2U01", Message: "坏"}}
	path, err := reference.WriteQuarantine(dir, episode, reference.QuarantineKindStep1, content, violations, map[string]any{"source": nil}, nil)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if path == "" {
		t.Fatal("quarantine path should not be empty")
	}
	if !reference.QuarantineExists(dir, episode, reference.QuarantineKindStep1) {
		t.Fatal("quarantine should exist")
	}
	draft, err := reference.ReadQuarantine(dir, episode, reference.QuarantineKindStep1)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if draft == nil || draft.Content["units"] == nil || len(draft.Violations) != 1 {
		t.Fatalf("unexpected draft: %+v", draft)
	}
	if _, hasSource := draft.Meta["source"]; !hasSource {
		t.Fatal("meta.source 键应一律写出（未指定时 null）")
	}
	if _, err := os.Stat(filepath.Join(service.EpisodeDraftsDir(dir, episode), service.ReferenceVideoStep1QuarantineName)); err != nil {
		t.Fatalf("expected quarantine file at fixed name: %v", err)
	}
	if err := reference.ClearQuarantine(dir, episode, reference.QuarantineKindStep1); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if reference.QuarantineExists(dir, episode, reference.QuarantineKindStep1) {
		t.Fatal("quarantine should be cleared")
	}
}
