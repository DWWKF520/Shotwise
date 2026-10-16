package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
)

// fakeTextGenCaps 是剧本生成类工具的时长能力替身。
type fakeTextGenCaps struct{ durations []int }

func (f fakeTextGenCaps) VideoCapabilitiesForProject(
	context.Context, *domain.ProjectFile,
) (config.VideoCapabilityView, error) {
	return config.VideoCapabilityView{ProviderID: "p", ModelID: "m", SupportedDurations: f.durations}, nil
}

// SaveScript 给共享替身 fakeGenerationProjects 补上剧本落盘能力（写入项目 scripts/ 目录）。
func (f *fakeGenerationProjects) SaveScript(_, scriptFilename string, script map[string]any) error {
	scriptsDir := filepath.Join(f.dir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(script, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(scriptsDir, scriptFilename), raw, 0o644)
}

func staticGenFactory(out string) service.TextCompletionFactory {
	return func(context.Context, *domain.ProjectFile) (service.TextCompletion, error) {
		return &fakeTextCompletion{outputs: []string{out}}, nil
	}
}

func newTextGenProject(t *testing.T, contentMode string, pf *domain.ProjectFile) (*fakeGenerationProjects, string) {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"source", "scripts"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pf.ContentMode = contentMode
	pf.Style = "写实"
	pf.Title = "测试项目"
	return &fakeGenerationProjects{pf: pf, dir: dir}, dir
}

func TestSplitNarrationSegmentsTool(t *testing.T) {
	pf := &domain.ProjectFile{}
	projects, dir := newTextGenProject(t, "narration", pf)
	novel := "他推开门。风灌了进来。"
	if err := os.WriteFile(filepath.Join(dir, "source", "episode_1.txt"), []byte(novel), 0o644); err != nil {
		t.Fatal(err)
	}
	out := `{"segments":[{"segment_id":"E1S01","novel_text":"他推开门。","duration_seconds":4,` +
		`"characters_in_segment":[],"scenes":[],"props":[]},` +
		`{"segment_id":"E1S02","novel_text":"风灌了进来。","duration_seconds":6,` +
		`"characters_in_segment":[],"scenes":[],"props":[]}]}`
	tl, err := newSplitNarrationSegmentsTool("p1", staticGenFactory(out), projects, fakeTextGenCaps{durations: []int{4, 6, 8}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.InvokableRun(context.Background(), `{"episode":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "说书片段拆分") {
		t.Fatalf("unexpected result: %s", res)
	}
	step1 := filepath.Join(service.EpisodeDraftsDir(dir, 1), "step1_segments.json")
	if _, err := os.Stat(step1); err != nil {
		t.Fatalf("step1 not written: %v", err)
	}
}

func TestNormalizeDramaScriptTool(t *testing.T) {
	pf := &domain.ProjectFile{}
	projects, dir := newTextGenProject(t, "drama", pf)
	if err := os.WriteFile(filepath.Join(dir, "source", "episode_1.txt"), []byte("他推开门。"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := `{"title":"第一集","scenes":[{"scene_id":"E1S01","duration_seconds":6,` +
		`"characters_in_scene":[],"scene_description":"他推开门","utterances":[],"source_text":"他推开门。"}]}`
	tl, err := newNormalizeDramaScriptTool("p1", staticGenFactory(out), projects, fakeTextGenCaps{durations: []int{4, 6, 8}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.InvokableRun(context.Background(), `{"episode":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "规范化剧本") {
		t.Fatalf("unexpected result: %s", res)
	}
	step1 := filepath.Join(service.EpisodeDraftsDir(dir, 1), "step1_normalized_script.json")
	if _, err := os.Stat(step1); err != nil {
		t.Fatalf("step1 not written: %v", err)
	}
}

func TestConfirmScriptReviewTool(t *testing.T) {
	pf := &domain.ProjectFile{Episodes: []domain.EpisodeMeta{{Episode: 1, ScriptFile: "scripts/episode_1.json"}}}
	projects, dir := newTextGenProject(t, "narration", pf)
	step1 := filepath.Join(service.EpisodeDraftsDir(dir, 1), "step1_segments.json")
	if err := os.MkdirAll(filepath.Dir(step1), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"segments":[{"segment_id":"E1S01","novel_text":"他推开门。","duration_seconds":4,` +
		`"characters_in_segment":[],"scenes":[],"props":[]}]}`
	if err := os.WriteFile(step1, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	tl, err := newConfirmScriptReviewTool("p1", projects)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.InvokableRun(context.Background(), `{"episode":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "已确认") {
		t.Fatalf("unexpected result: %s", res)
	}
	if pf.Episodes[0].Step1Review == nil {
		t.Fatal("confirmation record should be written")
	}
}

func TestGenerateEpisodeScriptToolNarration(t *testing.T) {
	pf := &domain.ProjectFile{Episodes: []domain.EpisodeMeta{{Episode: 1, ScriptFile: "scripts/episode_1.json"}}}
	projects, dir := newTextGenProject(t, "narration", pf)
	// step1 片段表。
	step1 := filepath.Join(service.EpisodeDraftsDir(dir, 1), "step1_segments.json")
	if err := os.MkdirAll(filepath.Dir(step1), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"segments":[{"segment_id":"E1S01","novel_text":"他推开门。","duration_seconds":4,` +
		`"segment_break":false,"characters_in_segment":[],"scenes":[],"props":[]}]}`
	if err := os.WriteFile(step1, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	// 已有 step2 产物 → grandfather 放行（避免待审 gate 阻塞）。
	if err := os.WriteFile(filepath.Join(dir, "scripts", "episode_1.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	visual := `{"title":"第一集","segments":[{"segment_id":"E1S01",` +
		`"image_prompt":{"scene":"他在门口","composition":{"shot_type":"Medium Shot","lighting":"冷光","ambiance":"雨"}},` +
		`"video_prompt":{"action":"推门","camera_motion":"Static","ambiance_audio":"雨声","dialogue":[]}}]}`
	tl, err := newGenerateEpisodeScriptTool("p1", staticGenFactory(visual), projects, fakeTextGenCaps{durations: []int{4, 6, 8}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.InvokableRun(context.Background(), `{"episode":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "剧本生成完成") {
		t.Fatalf("unexpected result: %s", res)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "scripts", "episode_1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var script map[string]any
	if err := json.Unmarshal(saved, &script); err != nil {
		t.Fatal(err)
	}
	segments, _ := script["segments"].([]any)
	if len(segments) != 1 {
		t.Fatalf("want 1 segment, got %v", script["segments"])
	}
	entry := segments[0].(map[string]any)
	if entry["novel_text"] != "他推开门。" {
		t.Fatalf("novel_text should be passed through from step1: %v", entry)
	}
	if _, ok := entry["image_prompt"]; !ok {
		t.Fatalf("visual layer should be merged in: %v", entry)
	}
	if script["episode"] != float64(1) {
		t.Fatalf("episode should be injected: %v", script["episode"])
	}
}

func TestGenerateEpisodeScriptToolReferenceNotMigrated(t *testing.T) {
	pf := &domain.ProjectFile{GenerationMode: string(domain.GenerationReferenceVideo)}
	projects, dir := newTextGenProject(t, "narration", pf)
	step1 := filepath.Join(service.EpisodeDraftsDir(dir, 1), "step1_reference_units.json")
	if err := os.MkdirAll(filepath.Dir(step1), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(step1, []byte(`{"units":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	tl, err := newGenerateEpisodeScriptTool("p1", staticGenFactory("{}"), projects, fakeTextGenCaps{durations: []int{4, 6, 8}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.InvokableRun(context.Background(), `{"episode":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "参考路径需要完整视频能力解析器") {
		t.Fatalf("reference path without full resolver should fail loud: %s", res)
	}
}
