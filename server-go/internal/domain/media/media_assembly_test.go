package media

import (
	"strings"
	"testing"
)

func minimalTimeline() []map[string]any {
	return []map[string]any{{
		"id": "E1U1:0", "source_unit_id": "E1U1", "order": 0, "kind": "video_unit",
		"source_ref": "videos/E1U1.mp4", "duration_seconds": 5.0,
		"trim_start_seconds": 0.0, "trim_end_seconds": 0.0,
		"audio_policy": "duck", "transition": nil,
	}}
}

func minimalAudio() map[string]any {
	return map[string]any{"unit_audio_policy": "duck", "tracks": []any{}, "ducking_db": -12.0}
}

func minimalSubtitle() map[string]any {
	return map[string]any{"mode": "burn_in", "cues": []any{}, "source": "script"}
}

func minimalPackaging() map[string]any {
	return map[string]any{
		"director_review": map[string]any{"status": "pending"},
		"intro":           map[string]any{"enabled": false},
		"outro":           map[string]any{"enabled": false},
		"cover":           map[string]any{"enabled": false, "source": nil},
	}
}

func minimalOutputProfile() map[string]any {
	return map[string]any{
		"preview": map[string]any{"width": 640, "height": 360},
		"final":   map[string]any{"width": 1920, "height": 1080},
		"format":  "mp4",
	}
}

func TestValidatePlanDocumentAcceptsMinimalDocument(t *testing.T) {
	snapshot := BuildSourceSnapshot("p1", map[string]any{"items": []any{}})
	validation, err := ValidatePlanDocument(
		snapshot, minimalTimeline(), minimalAudio(), minimalSubtitle(), minimalPackaging(), minimalOutputProfile(),
	)
	if err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
	if valid, _ := validation["valid"].(bool); !valid {
		t.Fatalf("validation = %v, want valid", validation)
	}
}

func TestValidatePlanDocumentRejectsEmptyTimelineAndTrim(t *testing.T) {
	snapshot := BuildSourceSnapshot("p1", map[string]any{"items": []any{}})

	_, err := ValidatePlanDocument(
		snapshot, []any{}, minimalAudio(), minimalSubtitle(), minimalPackaging(), minimalOutputProfile(),
	)
	if !assertValidationCode(t, err, "timeline", "not_empty") {
		t.Fatal("empty timeline must be rejected with not_empty")
	}

	trimmed := minimalTimeline()
	trimmed[0]["trim_start_seconds"] = 5.0
	_, err = ValidatePlanDocument(
		snapshot, trimmed, minimalAudio(), minimalSubtitle(), minimalPackaging(), minimalOutputProfile(),
	)
	if !assertValidationCode(t, err, "timeline[0]", "empty_after_trim") {
		t.Fatal("trim consuming the whole clip must be rejected")
	}
}

func TestValidatePlanDocumentRejectsNonContiguousOrder(t *testing.T) {
	snapshot := BuildSourceSnapshot("p1", map[string]any{"items": []any{}})
	timeline := minimalTimeline()
	timeline = append(timeline, map[string]any{
		"id": "E1U2:1", "order": 2, "kind": "video_unit", "source_ref": "videos/E1U2.mp4",
		"duration_seconds": 5.0, "audio_policy": "duck",
	})
	_, err := ValidatePlanDocument(
		snapshot, timeline, minimalAudio(), minimalSubtitle(), minimalPackaging(), minimalOutputProfile(),
	)
	if !assertValidationCode(t, err, "timeline", "non_contiguous_order") {
		t.Fatal("order gap must be rejected")
	}
}

func TestBuildSourceSnapshotNormalizesItems(t *testing.T) {
	manifest := map[string]any{
		"contract_version": "episode-media-manifest/v1",
		"fingerprint":      "abc",
		"script":           "episode_1.json",
		"collection":       "scenes",
		"items": []any{
			map[string]any{
				"order": 1, "unit_id": "E1U1",
				"source":      map[string]any{"path": "videos/E1U1.mp4", "kind": "generated_video"},
				"media_probe": map[string]any{"exists": true, "has_video_stream": true},
			},
		},
	}
	snapshot := BuildSourceSnapshot("p1", manifest)
	if snapshot["snapshot_version"] != "media-assembly-source/v1" {
		t.Fatalf("snapshot_version = %v", snapshot["snapshot_version"])
	}
	if snapshot["manifest_contract_version"] != "episode-media-manifest/v1" {
		t.Fatalf("manifest_contract_version = %v", snapshot["manifest_contract_version"])
	}
	project, _ := snapshot["project"].(map[string]any)
	if project["name"] != "p1" {
		t.Fatalf("project = %v", project)
	}
	items, _ := anyList(snapshot["items"])
	item, _ := items[0].(map[string]any)
	fingerprint, _ := item["file_fingerprint"].(map[string]any)
	if fingerprint["exists"] != true {
		t.Fatalf("file_fingerprint should inherit media_probe.exists, got %v", fingerprint)
	}
}

func TestSourceFingerprintIsDeterministic(t *testing.T) {
	snapshot := map[string]any{"b": 1.0, "a": []any{"x"}, "items": []any{}}
	first, err := SourceFingerprint(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	second, err := SourceFingerprint(map[string]any{"items": []any{}, "a": []any{"x"}, "b": 1.0})
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 64 {
		t.Fatalf("fingerprint not stable: %q vs %q", first, second)
	}
	changed, err := SourceFingerprint(map[string]any{"b": 2.0, "a": []any{"x"}, "items": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("fingerprint must change when content changes")
	}
}

func TestAssertTransitionAndRunningStatus(t *testing.T) {
	if err := AssertTransition("draft", "confirmed"); err != nil {
		t.Fatalf("draft→confirmed must be allowed: %v", err)
	}
	if err := AssertTransition("completed", "render_pending"); err == nil {
		t.Fatal("completed→render_pending must be rejected")
	}
	if err := AssertTransition("draft", "bogus"); err == nil {
		t.Fatal("unknown status must be rejected")
	}
	if !IsRunningStatus("rendering") || IsRunningStatus("draft") {
		t.Fatal("running status set mismatch")
	}
}

func assertValidationCode(t *testing.T, err error, path, code string) bool {
	t.Helper()
	validationErr, ok := err.(*AssemblyPlanValidationError)
	if !ok {
		return false
	}
	for _, issue := range validationErr.Errors {
		if issue["path"] == path && issue["code"] == code {
			return true
		}
	}
	var found []string
	for _, issue := range validationErr.Errors {
		found = append(found, strings.Join([]string{issue["path"].(string), issue["code"].(string)}, ":"))
	}
	t.Logf("validation errors = %v", found)
	return false
}
