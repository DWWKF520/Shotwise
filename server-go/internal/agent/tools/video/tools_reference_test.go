package video

import (
	serviceref "github.com/shotwise/shotwise/internal/service/reference"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/agent/tools/script"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/service"
)

// fakeReferenceCaps 是参考工具链的完整能力解析替身（*config.Resolver 的最小模拟）。
type fakeReferenceCaps struct {
	durations    []int
	maxRefImages int
}

func (f fakeReferenceCaps) view() config.VideoCapabilityView {
	return config.VideoCapabilityView{
		ProviderID:         "p",
		ModelID:            "m",
		SupportedDurations: f.durations,
		Caps: provider.VideoCapabilities{
			MaxReferenceImages: f.maxRefImages,
			ReferenceAudioMode: provider.ReferenceAudioNone,
		},
	}
}

func (f fakeReferenceCaps) VideoCapabilitiesForProject(
	context.Context, *domain.ProjectFile,
) (config.VideoCapabilityView, error) {
	return f.view(), nil
}

func (f fakeReferenceCaps) VideoCapabilitiesForProjectBucket(
	context.Context, *domain.ProjectFile, provider.VideoBucket,
) (config.VideoCapabilityView, error) {
	return f.view(), nil
}

func (f fakeReferenceCaps) VideoGenerateAudioForProject(context.Context, *domain.ProjectFile) (bool, error) {
	return true, nil
}

func (f fakeReferenceCaps) ResolveResolution(context.Context, *domain.ProjectFile, string, string) (string, error) {
	return "", nil
}

// newReferenceProject 造一个参考生视频项目（narration 内容、reference_video 路线）。
func newReferenceProject(t *testing.T) (*fakeGenerationProjects, string) {
	t.Helper()
	pf := &domain.ProjectFile{
		ContentMode:    "narration",
		GenerationMode: string(domain.GenerationReferenceVideo),
		Title:          "测试项目",
		Style:          "写实",
		Episodes:       []domain.EpisodeMeta{{Episode: 1, ScriptFile: "scripts/episode_1.json"}},
	}
	projects, dir := newTextGenProject(t, "narration", pf)
	if err := os.WriteFile(filepath.Join(dir, "source", "episode_1.txt"), []byte("他推开门。"), 0o644); err != nil {
		t.Fatal(err)
	}
	return projects, dir
}

func TestSplitReferenceVideoUnitsTool(t *testing.T) {
	projects, dir := newReferenceProject(t)
	flat := `{"units":[{"duration_seconds":6,"source_text":"他推开门。","text":"镜头1：他推开门走进屋。"}]}`
	tl, err := newSplitReferenceVideoUnitsTool("p1", staticGenFactory(flat), projects, fakeReferenceCaps{durations: []int{4, 6, 8}, maxRefImages: 3})
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.InvokableRun(context.Background(), `{"episode":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "参考视频单元拆分") {
		t.Fatalf("unexpected result: %s", res)
	}
	step1 := filepath.Join(service.EpisodeDraftsDir(dir, 1), service.ReferenceVideoStep1Filename)
	raw, err := os.ReadFile(step1)
	if err != nil {
		t.Fatalf("step1 not written: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	units, _ := parsed["units"].([]any)
	if len(units) != 1 {
		t.Fatalf("want 1 unit, got %v", parsed["units"])
	}
	unit := units[0].(map[string]any)
	if unit["unit_id"] != "E1U01" || unit["duration_seconds"] != float64(6) {
		t.Fatalf("machine-derived unit fields wrong: %v", unit)
	}
	if serviceref.QuarantineExists(dir, 1, serviceref.QuarantineKindStep1) {
		t.Fatal("quarantine should be cleared after successful split")
	}
}

func TestSplitReferenceVideoUnitsQuarantinesViolation(t *testing.T) {
	projects, dir := newReferenceProject(t)
	// source_text 不是原文逐字子串 → 违约 → 隔离草稿 + deny。
	flat := `{"units":[{"duration_seconds":6,"source_text":"杜撰的原文","text":"镜头1：他推开门走进屋。"}]}`
	tl, err := newSplitReferenceVideoUnitsTool("p1", staticGenFactory(flat), projects, fakeReferenceCaps{durations: []int{4, 6, 8}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.InvokableRun(context.Background(), `{"episode":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "已隔离到草稿") {
		t.Fatalf("violation should quarantine: %s", res)
	}
	if !serviceref.QuarantineExists(dir, 1, serviceref.QuarantineKindStep1) {
		t.Fatal("quarantine should exist after violation")
	}
	if _, err := os.Stat(filepath.Join(service.EpisodeDraftsDir(dir, 1), service.ReferenceVideoStep1Filename)); err == nil {
		t.Fatal("official step1 must not be written on violation")
	}
}

func TestOpenReferenceStep1ForEditTool(t *testing.T) {
	projects, dir := newReferenceProject(t)
	step1 := filepath.Join(service.EpisodeDraftsDir(dir, 1), service.ReferenceVideoStep1Filename)
	if err := os.MkdirAll(filepath.Dir(step1), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"units":[{"unit_id":"E1U01","shots":[{"text":"他推开门走进屋。"}],"duration_seconds":6,"references":[],"source_text":"他推开门。"}]}`
	if err := os.WriteFile(step1, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	tl, err := newOpenReferenceStep1ForEditTool("p1", projects)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.InvokableRun(context.Background(), `{"episode":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "已取回可编辑草稿") {
		t.Fatalf("unexpected result: %s", res)
	}
	draft, err := serviceref.ReadQuarantine(dir, 1, serviceref.QuarantineKindStep1)
	if err != nil {
		t.Fatal(err)
	}
	if draft == nil {
		t.Fatal("draft should be created")
	}
	if _, hasFP := draft.Meta["base_fingerprint"]; !hasFP {
		t.Fatal("meta.base_fingerprint should be recorded")
	}
}

func TestValidateAndPromoteReferenceDraftStep1(t *testing.T) {
	projects, dir := newReferenceProject(t)
	if _, err := serviceref.WriteQuarantine(dir, 1, serviceref.QuarantineKindStep1,
		map[string]any{"units": []any{map[string]any{
			"duration_seconds": 6, "source_text": "他推开门。", "text": "镜头1：他推开门走进屋。",
		}}}, nil, map[string]any{"source": nil, "base_fingerprint": nil}, nil); err != nil {
		t.Fatal(err)
	}
	tl, err := newValidateAndPromoteReferenceDraftTool("p1", staticGenFactory("{}"), projects, fakeReferenceCaps{durations: []int{4, 6, 8}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.InvokableRun(context.Background(), `{"episode":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "晋升") {
		t.Fatalf("unexpected result: %s", res)
	}
	step1 := filepath.Join(service.EpisodeDraftsDir(dir, 1), service.ReferenceVideoStep1Filename)
	if _, err := os.Stat(step1); err != nil {
		t.Fatalf("official step1 should be written: %v", err)
	}
	if serviceref.QuarantineExists(dir, 1, serviceref.QuarantineKindStep1) {
		t.Fatal("quarantine should be cleared after promotion")
	}
}

func TestGenerateEpisodeScriptToolReference(t *testing.T) {
	projects, dir := newReferenceProject(t)
	// grandfather 放行：step2 产物已存在 → gate 不阻塞。
	if err := os.WriteFile(filepath.Join(dir, "scripts", "episode_1.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	step1 := filepath.Join(service.EpisodeDraftsDir(dir, 1), service.ReferenceVideoStep1Filename)
	if err := os.MkdirAll(filepath.Dir(step1), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"units":[{"unit_id":"E1U01","shots":[{"text":"他推开门走进屋。"}],"duration_seconds":6,"references":[],"source_text":"他推开门。"}]}`
	if err := os.WriteFile(step1, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	flat := `{"title":"第一集","units":[{"text":"镜头1：他推开门走进屋。"}]}`
	tl, err := script.NewGenerateEpisodeScriptTool("p1", staticGenFactory(flat), projects, fakeReferenceCaps{durations: []int{4, 6, 8}})
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
	units, _ := script["video_units"].([]any)
	if len(units) != 1 {
		t.Fatalf("script should carry video_units: %v", script)
	}
	unit := units[0].(map[string]any)
	if unit["duration_seconds"] != float64(6) {
		t.Fatalf("duration should be carried from step1: %v", unit)
	}
}

func TestGenerateEpisodeScriptToolAD(t *testing.T) {
	pf := &domain.ProjectFile{ContentMode: "ad", GenerationMode: string(domain.GenerationStoryboard), TargetDuration: intPtr(30)}
	projects, _ := newTextGenProject(t, "ad", pf)
	out := `{"title":"广告","shots":[{"shot_id":"E1S01","section":"hook","duration_seconds":6,"voiceover_text":"来",` +
		`"products_in_shot":[],"characters_in_shot":[],"scenes":[],"props":[],` +
		`"image_prompt":{"scene":"s","composition":{"shot_type":"Medium Shot","lighting":"l","ambiance":"a"}},` +
		`"video_prompt":{"action":"a","camera_motion":"Static","ambiance_audio":"","dialogue":[]}}]}`
	tl, err := script.NewGenerateEpisodeScriptTool("p1", staticGenFactory(out), projects, fakeReferenceCaps{durations: []int{4, 6, 8}})
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
}

func intPtr(v int) *int { return &v }
