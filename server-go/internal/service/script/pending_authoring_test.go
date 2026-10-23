// 待编写（pending authoring）读时推导测试（对应 tests/test_pending_authoring.py）：
// 三变体的内容投影、id 对齐与三种待编写原因。
//
// step1 与官方剧本两侧都经磁盘 JSON 走一轮（PendingAuthoringReportFor 读的是落盘内容），
// 数字统一经 JSON 反序列化成 float64，与线上读取路径同构。
package script

import (
	"encoding/json"
	"github.com/shotwise/shotwise/internal/service"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/shotwise/shotwise/internal/domain/project"
)

// makePendingProject 建测试项目并登记第 1 集（剧本路径 scripts/episode_1.json）。
func makePendingProject(t *testing.T, mode project.ContentMode, generationMode project.GenerationMode) (*service.ProjectStore, string) {
	t.Helper()
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("demo", "Demo", mode, generationMode, "Anime"); err != nil {
		t.Fatal(err)
	}
	if err := store.Update("demo", func(pf *project.ProjectFile) error {
		pf.Episodes = append(pf.Episodes, project.EpisodeMeta{
			Episode: 1, Title: "第一集", ScriptFile: "scripts/episode_1.json",
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dir, err := store.Dir("demo")
	if err != nil {
		t.Fatal(err)
	}
	return store, dir
}

func writePendingJSON(t *testing.T, path string, payload map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writePendingStep1(t *testing.T, dir, filename string, payload map[string]any) {
	t.Helper()
	writePendingJSON(t, filepath.Join(dir, "drafts", "episode_1", filename), payload)
}

func writePendingScript(t *testing.T, dir string, payload map[string]any) {
	t.Helper()
	writePendingJSON(t, filepath.Join(dir, "scripts", "episode_1.json"), payload)
}

func pendingReport(t *testing.T, store *service.ProjectStore, dir string) PendingAuthoringReport {
	t.Helper()
	pf, err := store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	return PendingAuthoringReportFor(dir, pf, 1)
}

// pendingDramaScene 官方剧本里的一个 drama 场景：step1 内容投影 + 视觉层。
func pendingDramaScene(sceneID, text string) map[string]any {
	return map[string]any{
		"scene_id":            sceneID,
		"duration_seconds":    8,
		"segment_break":       false,
		"characters_in_scene": []any{"阿离"},
		"scenes":              []any{},
		"props":               []any{},
		"utterances": []any{
			map[string]any{"kind": "voiceover", "speaker": nil, "text": "三年后。"},
			map[string]any{"kind": "dialogue", "speaker": "阿离", "text": text},
		},
		"source_text":  "三年后，阿离立于屋檐下：你终于回来了。",
		"image_prompt": map[string]any{"scene": "雨夜屋檐"},
		"video_prompt": map[string]any{"action": "定格"},
	}
}

// pendingDramaStep1Scene step1 里的一个 drama 场景：与官方剧本同源，另带 step1-only 的
// scene_description，不带视觉层。
func pendingDramaStep1Scene(sceneID, text string) map[string]any {
	return map[string]any{
		"scene_id":            sceneID,
		"duration_seconds":    8,
		"segment_break":       false,
		"characters_in_scene": []any{"阿离"},
		"scenes":              []any{},
		"props":               []any{},
		"utterances": []any{
			map[string]any{"kind": "voiceover", "speaker": nil, "text": "三年后。"},
			map[string]any{"kind": "dialogue", "speaker": "阿离", "text": text},
		},
		"source_text":       "三年后，阿离立于屋檐下：你终于回来了。",
		"scene_description": "雨夜，阿离立于屋檐下",
	}
}

// pendingNarrationStep1Segment narration step1 的一个 segment。
func pendingNarrationStep1Segment(segmentID, novelText string) map[string]any {
	return map[string]any{
		"segment_id":            segmentID,
		"novel_text":            novelText,
		"duration_seconds":      5,
		"segment_break":         false,
		"characters_in_segment": []any{"阿离"},
		"scenes":                []any{},
		"props":                 []any{},
	}
}

// pendingNarrationSegment 官方剧本里的 narration segment：step1 同源 + 视觉层。
func pendingNarrationSegment(segmentID, novelText string) map[string]any {
	segment := pendingNarrationStep1Segment(segmentID, novelText)
	segment["image_prompt"] = map[string]any{"scene": "雨夜屋檐"}
	segment["video_prompt"] = map[string]any{"action": "定格"}
	return segment
}

// pendingRVStep1Unit reference_video step1 的一个 unit。
func pendingRVStep1Unit(unitID string, duration int) map[string]any {
	return map[string]any{
		"unit_id":          unitID,
		"shots":            []any{map[string]any{"text": "@[阿离] 立于屋檐下。"}},
		"duration_seconds": duration,
		"references":       []any{map[string]any{"type": "character", "name": "阿离"}},
		"source_text":      "三年后，阿离立于屋檐下。",
	}
}

// pendingRVUnit 官方剧本里的 video_unit：shots 已被 step2 视觉展开改写。
func pendingRVUnit(unitID string, duration int) map[string]any {
	return map[string]any{
		"unit_id":          unitID,
		"shots":            []any{map[string]any{"text": "@[阿离] 全景定格，雨丝掠过屋檐。"}},
		"duration_seconds": duration,
		"references":       []any{map[string]any{"type": "character", "name": "阿离"}},
	}
}

// --- 适用范围 ---

func TestPendingAuthoringAdModeNotApplicable(t *testing.T) {
	store, dir := makePendingProject(t, project.ContentAd, project.GenerationStoryboard)
	report := pendingReport(t, store, dir)
	if report.Applicable {
		t.Fatal("ad 模式不适用待编写推导")
	}
	if report.Kind != "" {
		t.Fatalf("kind = %q，期望空", report.Kind)
	}
	if len(report.Items) != 0 {
		t.Fatalf("items = %v，期望空", report.Items)
	}
}

func TestPendingAuthoringScriptAbsentReportsNothingPending(t *testing.T) {
	// 「还没生成过剧本」不等于「整集待编写」，不把未产出报成待编写。
	store, dir := makePendingProject(t, project.ContentDrama, project.GenerationStoryboard)
	writePendingStep1(t, dir, "step1_normalized_script.json", map[string]any{
		"title": "第一集", "scenes": []any{pendingDramaStep1Scene("E1S01", "你终于回来了。")},
	})
	report := pendingReport(t, store, dir)
	if !report.Applicable || report.Kind != "drama" {
		t.Fatalf("applicable/kind = %v/%q", report.Applicable, report.Kind)
	}
	if report.ScriptPresent {
		t.Fatal("剧本未产出时 script_present 应为 false")
	}
	if len(report.Items) != 0 || report.HasPending() {
		t.Fatalf("items = %v，期望空", report.Items)
	}
}

func TestPendingAuthoringStep1AbsentReportsNothingPending(t *testing.T) {
	store, dir := makePendingProject(t, project.ContentDrama, project.GenerationStoryboard)
	writePendingScript(t, dir, map[string]any{
		"title": "第一集", "scenes": []any{pendingDramaScene("E1S01", "你终于回来了。")},
	})
	report := pendingReport(t, store, dir)
	if !report.ScriptPresent {
		t.Fatal("剧本在场时 script_present 应为 true")
	}
	if len(report.Items) != 0 {
		t.Fatalf("items = %v，期望空", report.Items)
	}
}

// --- drama 投影 ---

func TestPendingAuthoringDramaMatchingContentIsClean(t *testing.T) {
	store, dir := makePendingProject(t, project.ContentDrama, project.GenerationStoryboard)
	writePendingStep1(t, dir, "step1_normalized_script.json", map[string]any{
		"title": "第一集", "scenes": []any{pendingDramaStep1Scene("E1S01", "你终于回来了。")},
	})
	writePendingScript(t, dir, map[string]any{
		"title": "第一集", "scenes": []any{pendingDramaScene("E1S01", "你终于回来了。")},
	})
	if report := pendingReport(t, store, dir); len(report.Items) != 0 {
		t.Fatalf("items = %v，期望空", report.Items)
	}
}

func TestPendingAuthoringDramaContentChangeMarksStaleVisual(t *testing.T) {
	store, dir := makePendingProject(t, project.ContentDrama, project.GenerationStoryboard)
	writePendingStep1(t, dir, "step1_normalized_script.json", map[string]any{
		"title": "第一集", "scenes": []any{pendingDramaStep1Scene("E1S01", "我回来了。")},
	})
	writePendingScript(t, dir, map[string]any{
		"title": "第一集", "scenes": []any{pendingDramaScene("E1S01", "你终于回来了。")},
	})
	report := pendingReport(t, store, dir)
	if !reflect.DeepEqual(report.ItemIDs(), []string{"E1S01"}) {
		t.Fatalf("item_ids = %v", report.ItemIDs())
	}
	if report.Items[0].Reason != PendingReasonStaleVisual {
		t.Fatalf("reason = %s", report.Items[0].Reason)
	}
}

func TestPendingAuthoringDramaSceneDescriptionIgnored(t *testing.T) {
	// scene_description 是 step1-only、合并时被剔除，两侧取值不同不构成待编写。
	store, dir := makePendingProject(t, project.ContentDrama, project.GenerationStoryboard)
	step1 := pendingDramaStep1Scene("E1S01", "你终于回来了。")
	step1["scene_description"] = "完全不同的视觉改编描述"
	writePendingStep1(t, dir, "step1_normalized_script.json", map[string]any{
		"title": "第一集", "scenes": []any{step1},
	})
	writePendingScript(t, dir, map[string]any{
		"title": "第一集", "scenes": []any{pendingDramaScene("E1S01", "你终于回来了。")},
	})
	if report := pendingReport(t, store, dir); len(report.Items) != 0 {
		t.Fatalf("items = %v，期望空", report.Items)
	}
}

func TestPendingAuthoringDramaScriptOnlyFieldsIgnored(t *testing.T) {
	// note / transition_to_next / generated_assets 是剧本侧 UI / 运行时字段，不参与判定。
	store, dir := makePendingProject(t, project.ContentDrama, project.GenerationStoryboard)
	writePendingStep1(t, dir, "step1_normalized_script.json", map[string]any{
		"title": "第一集", "scenes": []any{pendingDramaStep1Scene("E1S01", "你终于回来了。")},
	})
	scene := pendingDramaScene("E1S01", "你终于回来了。")
	scene["note"] = "人工备注"
	scene["transition_to_next"] = "fade"
	scene["generated_assets"] = map[string]any{"storyboard_image": "a.png"}
	writePendingScript(t, dir, map[string]any{"title": "第一集", "scenes": []any{scene}})
	if report := pendingReport(t, store, dir); len(report.Items) != 0 {
		t.Fatalf("items = %v，期望空", report.Items)
	}
}

func TestPendingAuthoringDramaMissingAndOrphanItems(t *testing.T) {
	store, dir := makePendingProject(t, project.ContentDrama, project.GenerationStoryboard)
	writePendingStep1(t, dir, "step1_normalized_script.json", map[string]any{
		"title": "第一集",
		"scenes": []any{
			pendingDramaStep1Scene("E1S01", "你终于回来了。"),
			pendingDramaStep1Scene("E1S02", "你终于回来了。"),
		},
	})
	writePendingScript(t, dir, map[string]any{
		"title": "第一集",
		"scenes": []any{
			pendingDramaScene("E1S01", "你终于回来了。"),
			pendingDramaScene("E1S99", "你终于回来了。"),
		},
	})
	report := pendingReport(t, store, dir)
	got := make([][2]string, 0, len(report.Items))
	for _, item := range report.Items {
		got = append(got, [2]string{item.ItemID, item.Reason})
	}
	want := [][2]string{{"E1S02", PendingReasonMissingVisual}, {"E1S99", PendingReasonOrphanItem}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %v，期望 %v", got, want)
	}
}

func TestPendingAuthoringDramaMissingContentFieldIsStale(t *testing.T) {
	// 剧本缺一个 step1 声明过的字段时判为不一致，不把缺失静默当成通过。
	store, dir := makePendingProject(t, project.ContentDrama, project.GenerationStoryboard)
	writePendingStep1(t, dir, "step1_normalized_script.json", map[string]any{
		"title": "第一集", "scenes": []any{pendingDramaStep1Scene("E1S01", "你终于回来了。")},
	})
	scene := pendingDramaScene("E1S01", "你终于回来了。")
	delete(scene, "segment_break")
	writePendingScript(t, dir, map[string]any{"title": "第一集", "scenes": []any{scene}})
	report := pendingReport(t, store, dir)
	if len(report.Items) != 1 || report.Items[0].Reason != PendingReasonStaleVisual {
		t.Fatalf("items = %v，期望 [E1S01 stale_visual]", report.Items)
	}
}

func TestPendingAuthoringDramaMalformedScriptListsDegradeToEmpty(t *testing.T) {
	store, dir := makePendingProject(t, project.ContentDrama, project.GenerationStoryboard)
	writePendingStep1(t, dir, "step1_normalized_script.json", map[string]any{
		"title": "第一集", "scenes": []any{pendingDramaStep1Scene("E1S01", "你终于回来了。")},
	})
	writePendingScript(t, dir, map[string]any{"title": "第一集", "scenes": "not-a-list"})
	report := pendingReport(t, store, dir)
	if !report.ScriptPresent {
		t.Fatal("剧本在场时 script_present 应为 true")
	}
	if len(report.Items) != 0 {
		t.Fatalf("items = %v，期望空（列表键脏时不判定）", report.Items)
	}
}

func TestPendingAuthoringDramaReportToDictShape(t *testing.T) {
	store, dir := makePendingProject(t, project.ContentDrama, project.GenerationStoryboard)
	writePendingStep1(t, dir, "step1_normalized_script.json", map[string]any{
		"title": "第一集", "scenes": []any{pendingDramaStep1Scene("E1S01", "改了")},
	})
	writePendingScript(t, dir, map[string]any{
		"title": "第一集", "scenes": []any{pendingDramaScene("E1S01", "你终于回来了。")},
	})
	got := pendingReport(t, store, dir).ToDict()
	want := map[string]any{
		"applicable":     true,
		"kind":           "drama",
		"script_present": true,
		"has_pending":    true,
		"items": []map[string]string{
			{"item_id": "E1S01", "reason": PendingReasonStaleVisual},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("to_dict = %v，期望 %v", got, want)
	}
}

// --- narration 投影 ---

func TestPendingAuthoringNarrationCleanWhenNovelTextMatches(t *testing.T) {
	store, dir := makePendingProject(t, project.ContentNarration, project.GenerationStoryboard)
	writePendingStep1(t, dir, "step1_segments.json", map[string]any{
		"segments": []any{pendingNarrationStep1Segment("E1S01", "三年后。")},
	})
	writePendingScript(t, dir, map[string]any{
		"title": "第一集", "segments": []any{pendingNarrationSegment("E1S01", "三年后。")},
	})
	report := pendingReport(t, store, dir)
	if report.Kind != "narration" {
		t.Fatalf("kind = %q", report.Kind)
	}
	if len(report.Items) != 0 {
		t.Fatalf("items = %v，期望空", report.Items)
	}
}

func TestPendingAuthoringNarrationNovelTextChangeMarksStale(t *testing.T) {
	store, dir := makePendingProject(t, project.ContentNarration, project.GenerationStoryboard)
	writePendingStep1(t, dir, "step1_segments.json", map[string]any{
		"segments": []any{pendingNarrationStep1Segment("E1S01", "三年后，他回来了。")},
	})
	writePendingScript(t, dir, map[string]any{
		"title": "第一集", "segments": []any{pendingNarrationSegment("E1S01", "三年后。")},
	})
	report := pendingReport(t, store, dir)
	if len(report.Items) != 1 || report.Items[0].Reason != PendingReasonStaleVisual {
		t.Fatalf("items = %v，期望 [E1S01 stale_visual]", report.Items)
	}
}

// --- reference_video 投影 ---

func TestPendingAuthoringRVShotsTextExpansionIsNotStale(t *testing.T) {
	// shots 正文由 step2 视觉展开改写，两侧必然不同，不构成待编写。
	store, dir := makePendingProject(t, project.ContentNarration, project.GenerationReferenceVideo)
	writePendingStep1(t, dir, service.ReferenceVideoStep1Filename, map[string]any{
		"units": []any{pendingRVStep1Unit("E1U01", 4)},
	})
	writePendingScript(t, dir, map[string]any{
		"title": "第一集", "video_units": []any{pendingRVUnit("E1U01", 4)},
	})
	report := pendingReport(t, store, dir)
	if report.Kind != "reference_video" {
		t.Fatalf("kind = %q", report.Kind)
	}
	if len(report.Items) != 0 {
		t.Fatalf("items = %v，期望空", report.Items)
	}
}

func TestPendingAuthoringRVDurationChangeMarksStale(t *testing.T) {
	store, dir := makePendingProject(t, project.ContentNarration, project.GenerationReferenceVideo)
	writePendingStep1(t, dir, service.ReferenceVideoStep1Filename, map[string]any{
		"units": []any{pendingRVStep1Unit("E1U01", 8)},
	})
	writePendingScript(t, dir, map[string]any{
		"title": "第一集", "video_units": []any{pendingRVUnit("E1U01", 4)},
	})
	report := pendingReport(t, store, dir)
	if len(report.Items) != 1 || report.Items[0].Reason != PendingReasonStaleVisual {
		t.Fatalf("items = %v，期望 [E1U01 stale_visual]", report.Items)
	}
}

func TestPendingAuthoringRVReferenceChangeMarksStale(t *testing.T) {
	store, dir := makePendingProject(t, project.ContentNarration, project.GenerationReferenceVideo)
	unit := pendingRVStep1Unit("E1U01", 4)
	unit["references"] = []any{
		map[string]any{"type": "character", "name": "阿离"},
		map[string]any{"type": "scene", "name": "屋檐"},
	}
	writePendingStep1(t, dir, service.ReferenceVideoStep1Filename, map[string]any{"units": []any{unit}})
	writePendingScript(t, dir, map[string]any{
		"title": "第一集", "video_units": []any{pendingRVUnit("E1U01", 4)},
	})
	report := pendingReport(t, store, dir)
	if len(report.Items) != 1 || report.Items[0].Reason != PendingReasonStaleVisual {
		t.Fatalf("items = %v，期望 [E1U01 stale_visual]", report.Items)
	}
}

func TestPendingAuthoringRVSourceTextIgnored(t *testing.T) {
	// source_text 是 step1-only，落地时被剔除，两侧取值不同不构成待编写。
	store, dir := makePendingProject(t, project.ContentNarration, project.GenerationReferenceVideo)
	unit := pendingRVStep1Unit("E1U01", 4)
	unit["source_text"] = "完全不同的原文摘录"
	writePendingStep1(t, dir, service.ReferenceVideoStep1Filename, map[string]any{"units": []any{unit}})
	writePendingScript(t, dir, map[string]any{
		"title": "第一集", "video_units": []any{pendingRVUnit("E1U01", 4)},
	})
	if report := pendingReport(t, store, dir); len(report.Items) != 0 {
		t.Fatalf("items = %v，期望空", report.Items)
	}
}
