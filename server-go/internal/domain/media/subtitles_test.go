package media

import (
	"strings"
	"testing"
)

func TestGenerateSRT(t *testing.T) {
	cues := []any{
		map[string]any{"start_seconds": 0.5, "end_seconds": 2, "text": "你好"},
		map[string]any{"start_seconds": 3, "end_seconds": 4.25, "text": "line1\r\nline2"},
	}
	out, err := GenerateSRT(cues)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	want := "1\n00:00:00,500 --> 00:00:02,000\n你好\n\n2\n00:00:03,000 --> 00:00:04,250\nline1\nline2\n"
	if out != want {
		t.Errorf("srt = %q, want %q", out, want)
	}
}

func TestGenerateSRTEmpty(t *testing.T) {
	out, err := GenerateSRT(nil)
	if err != nil || out != "" {
		t.Errorf("empty cues: out=%q err=%v", out, err)
	}
}

func TestGenerateVTT(t *testing.T) {
	id := "cue-1"
	cues := []any{
		map[string]any{"start_seconds": 0, "end_seconds": 1, "text": "a", "identifier": id},
		map[string]any{"start_seconds": 1, "end_seconds": 2.5, "text": "b"},
	}
	out, err := GenerateVTT(cues)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	want := "WEBVTT\n\ncue-1\n00:00:00.000 --> 00:00:01.000\na\n\n00:00:01.000 --> 00:00:02.500\nb\n"
	if out != want {
		t.Errorf("vtt = %q, want %q", out, want)
	}
}

func TestSubtitleTimestampUsesHalfEvenRounding(t *testing.T) {
	if got := formatSubtitleTimestamp(0.0005, ","); got != "00:00:00,000" {
		t.Errorf("half-even 0.0005s = %q, want 00:00:00,000", got)
	}
	if got := formatSubtitleTimestamp(1.5, ","); got != "00:00:01,500" {
		t.Errorf("1.5s = %q, want 00:00:01,500", got)
	}
	if got := formatSubtitleTimestamp(3661500.4, "."); got != "1017:05:00.400" {
		t.Errorf("large = %q", got)
	}
}

func TestNormalizeSubtitleCuesRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		cue  map[string]any
		want string
	}{
		{"missing_start", map[string]any{"end_seconds": 1, "text": "a"}, "is missing start_seconds"},
		{"start_negative", map[string]any{"start_seconds": -1, "end_seconds": 1, "text": "a"}, "must be non-negative"},
		{"start_not_number", map[string]any{"start_seconds": "0", "end_seconds": 1, "text": "a"}, "must be a finite number"},
		{"start_bool", map[string]any{"start_seconds": true, "end_seconds": 1, "text": "a"}, "must be a finite number"},
		{"end_before_start", map[string]any{"start_seconds": 2, "end_seconds": 2, "text": "a"}, "greater than start_seconds"},
		{"empty_text", map[string]any{"start_seconds": 0, "end_seconds": 1, "text": "  "}, "non-empty string"},
		{"nul_text", map[string]any{"start_seconds": 0, "end_seconds": 1, "text": "a\x00b"}, "must not contain NUL"},
		{
			"blank_identifier",
			map[string]any{"start_seconds": 0, "end_seconds": 1, "text": "a", "identifier": "  "},
			"identifier must be a non-empty string",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeSubtitleCues([]any{tc.cue})
			if err == nil {
				t.Fatalf("want error %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want contains %q", err.Error(), tc.want)
			}
		})
	}
	if _, err := NormalizeSubtitleCues([]any{"not-a-cue"}); err == nil {
		t.Errorf("non-object cue should fail")
	}
}

func TestNormalizeSubtitleCuesRejectsOverlap(t *testing.T) {
	cues := []any{
		map[string]any{"start_seconds": 0, "end_seconds": 2, "text": "a"},
		map[string]any{"start_seconds": 1, "end_seconds": 3, "text": "b"},
	}
	_, err := NormalizeSubtitleCues(cues)
	if err == nil || !strings.Contains(err.Error(), "overlaps the previous cue") {
		t.Errorf("err = %v, want overlap error", err)
	}
	// 相邻（start == previous end）不算重叠。
	adjacent := []any{
		map[string]any{"start_seconds": 0, "end_seconds": 2, "text": "a"},
		map[string]any{"start_seconds": 2, "end_seconds": 3, "text": "b"},
	}
	if _, err := NormalizeSubtitleCues(adjacent); err != nil {
		t.Errorf("adjacent cues should pass: %v", err)
	}
}
