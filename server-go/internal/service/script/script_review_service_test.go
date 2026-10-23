// step1→step2 审核 gate 的服务层测试（对应 tests/test_script_review.py 的
// 状态流转 / 适用范围 / 迁移收编 / 编辑校验 / 手动预拆分自愈各测试类）。
//
// 只测外部可观察行为：状态流转（step1 产出 → pending → 阻塞 → 确认 → confirmed → 放行）、
// 结构校验拒绝、基线指纹冲突、隔离草稿阻塞、迁移收编与确认平移。
package script

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/reference"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/provider"
)

// --- 替身与夹具 ---

// stubReviewCaps 是审阅门的视频能力查询替身：按用例注入档位表，默认零值等价
// 「解析不到型号」（caps 无档位且项目未配置后端时两条档位来源都取不到）。
type stubReviewCaps struct {
	view       config.VideoCapabilityView
	resolveErr error
	i2vErr     error
}

func (s *stubReviewCaps) VideoCapabilitiesForProjectBucket(
	_ context.Context, _ *project.ProjectFile, bucket provider.VideoBucket,
) (config.VideoCapabilityView, error) {
	if bucket == provider.BucketI2V && s.i2vErr != nil {
		return config.VideoCapabilityView{}, s.i2vErr
	}
	if s.resolveErr != nil {
		return config.VideoCapabilityView{}, s.resolveErr
	}
	return s.view, nil
}

func (s *stubReviewCaps) VideoGenerateAudioForProject(_ context.Context, _ *project.ProjectFile) (bool, error) {
	return false, nil
}

func (s *stubReviewCaps) ResolveResolution(_ context.Context, _ *project.ProjectFile, _, _ string) (string, error) {
	return "", nil
}

// makeReviewProject 建测试项目：登记第 1 集与两个角色（references 派生的解析依赖资产登记）。
func makeReviewProject(t *testing.T, mode project.ContentMode, generationMode project.GenerationMode) (*service.ProjectStore, string) {
	t.Helper()
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("demo", "Demo", mode, generationMode, "Anime"); err != nil {
		t.Fatal(err)
	}
	if err := store.Update("demo", func(pf *project.ProjectFile) error {
		if pf.Characters == nil {
			pf.Characters = project.AssetBucket{}
		}
		pf.Characters["阿离"] = project.WrapAssetEntry(map[string]any{"name": "阿离", "description": "少女"})
		pf.Characters["裴与"] = project.WrapAssetEntry(map[string]any{"name": "裴与", "description": "将军"})
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

// makeManualSplitProject 建手动预拆分场景的项目：账本为空，仅靠派生文件自愈。
func makeManualSplitProject(t *testing.T, mode project.ContentMode) (*service.ProjectStore, string) {
	t.Helper()
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("demo", "Demo", mode, project.GenerationStoryboard, "Anime"); err != nil {
		t.Fatal(err)
	}
	dir, err := store.Dir("demo")
	if err != nil {
		t.Fatal(err)
	}
	return store, dir
}

func writeReviewStep1(t *testing.T, dir, filename string, content map[string]any) string {
	t.Helper()
	drafts := filepath.Join(dir, "drafts", "episode_1")
	if err := os.MkdirAll(drafts, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(drafts, filename)
	if err := fs.WriteJSONAtomic(path, content); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeReviewStep2(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteJSONAtomic(
		filepath.Join(dir, "scripts", "episode_1.json"),
		map[string]any{"title": "第一集", "scenes": []any{}},
	); err != nil {
		t.Fatal(err)
	}
}

func writeReviewSourceText(t *testing.T, dir, filename, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source", filename), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func dramaStep1() map[string]any {
	return map[string]any{
		"title": "第一集",
		"scenes": []any{map[string]any{
			"scene_id":            "E1S01",
			"duration_seconds":    float64(8),
			"segment_break":       false,
			"characters_in_scene": []any{"阿离"},
			"scenes":              []any{},
			"props":               []any{},
			"scene_description":   "雨夜，阿离立于屋檐下",
			"utterances": []any{
				map[string]any{"kind": "voiceover", "speaker": nil, "text": "三年后。"},
				map[string]any{"kind": "dialogue", "speaker": "阿离", "text": "你终于回来了。"},
			},
			"source_text": "三年后，阿离立于屋檐下，轻声道：你终于回来了。",
		}},
	}
}

func narrationStep1() map[string]any {
	return map[string]any{
		"episode": float64(1),
		"segments": []any{map[string]any{
			"segment_id":            "E1S01",
			"novel_text":            "裴与出征后的第二年，送回一个襁褓中的婴儿。",
			"duration_seconds":      float64(6),
			"segment_break":         false,
			"characters_in_segment": []any{"裴与"},
			"scenes":                []any{},
			"props":                 []any{},
		}},
	}
}

func rvStep1() map[string]any {
	return map[string]any{
		"units": []any{map[string]any{
			"unit_id": "E1U01",
			"shots": []any{
				map[string]any{"text": "@[阿离] 立于屋檐下，望向雨幕。"},
				map[string]any{"text": "@[裴与] 策马自远方而来。"},
			},
			"references": []any{
				map[string]any{"type": "character", "name": "阿离"},
				map[string]any{"type": "character", "name": "裴与"},
			},
			"duration_seconds": float64(8),
		}},
	}
}

// legacyRVStep1 构造收编前形状：时长挂在各 shot 上，unit 无 duration_seconds。
func legacyRVStep1() map[string]any {
	legacy := rvStep1()
	unit := legacy["units"].([]any)[0].(map[string]any)
	delete(unit, "duration_seconds")
	unit["shots"].([]any)[0].(map[string]any)["duration"] = float64(5)
	unit["shots"].([]any)[1].(map[string]any)["duration"] = float64(3)
	return legacy
}

// legacyRVStep1WithDurations 按给定 per-shot 时长构造收编前草稿。
func legacyRVStep1WithDurations(t *testing.T, first, second int) map[string]any {
	t.Helper()
	legacy := legacyRVStep1()
	unit := legacy["units"].([]any)[0].(map[string]any)
	unit["shots"].([]any)[0].(map[string]any)["duration"] = float64(first)
	unit["shots"].([]any)[1].(map[string]any)["duration"] = float64(second)
	return legacy
}

// --- 断言辅助 ---

func reviewStatus(t *testing.T, state map[string]any) string {
	t.Helper()
	status, _ := state["status"].(string)
	return status
}

func reviewContent(t *testing.T, state map[string]any) map[string]any {
	t.Helper()
	content, ok := state["content"].(map[string]any)
	if !ok {
		t.Fatalf("state.content 不是对象: %T", state["content"])
	}
	return content
}

func reviewList(t *testing.T, state map[string]any, key string) []any {
	t.Helper()
	list, ok := reviewContent(t, state)[key].([]any)
	if !ok {
		t.Fatalf("state.content.%s 不是数组", key)
	}
	return list
}

func reviewListItem(t *testing.T, state map[string]any, key string, index int) map[string]any {
	t.Helper()
	list := reviewList(t, state, key)
	if index >= len(list) {
		t.Fatalf("content.%s[%d] 越界（共 %d 项）", key, index, len(list))
	}
	item, ok := list[index].(map[string]any)
	if !ok {
		t.Fatalf("content.%s[%d] 不是对象", key, index)
	}
	return item
}

func reviewFingerprint(t *testing.T, state map[string]any) string {
	t.Helper()
	value, ok := state["fingerprint"].(string)
	if !ok {
		t.Fatalf("state.fingerprint 缺失: %v", state["fingerprint"])
	}
	return value
}

func requireReviewCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误码 %s，得到 nil", want)
	}
	var reviewErr *ScriptReviewError
	if !errors.As(err, &reviewErr) {
		t.Fatalf("期望 *ScriptReviewError，得到 %T: %v", err, err)
	}
	if reviewErr.Code != want {
		t.Fatalf("错误码 = %s，期望 %s", reviewErr.Code, want)
	}
}

func reviewRefPairs(t *testing.T, unit map[string]any) [][2]string {
	t.Helper()
	refs, _ := unit["references"].([]any)
	pairs := make([][2]string, 0, len(refs))
	for _, raw := range refs {
		ref, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("reference 不是对象: %T", raw)
		}
		pairs = append(pairs, [2]string{ref["type"].(string), ref["name"].(string)})
	}
	return pairs
}

func readReviewStep1File(t *testing.T, path string) map[string]any {
	t.Helper()
	var data map[string]any
	if err := fs.ReadJSON(path, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func reviewShotsHaveNoDurationKey(t *testing.T, unit map[string]any) {
	t.Helper()
	for index, raw := range unit["shots"].([]any) {
		shot, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("shots[%d] 不是对象", index)
		}
		if _, present := shot["duration"]; present {
			t.Fatalf("shots[%d] 仍携带 duration 字段: %v", index, shot)
		}
	}
}

// --- 状态流转（drama） ---

func TestScriptReviewDramaGateFlow(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationStoryboard)
	svc := NewScriptReviewService(store, &stubReviewCaps{})

	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusNoStep1 {
		t.Fatalf("step1 未产出时状态 = %s", reviewStatus(t, state))
	}

	path := writeReviewStep1(t, dir, "step1_normalized_script.json", dramaStep1())
	state, err = svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("step1 产出后状态 = %s", reviewStatus(t, state))
	}
	scene := reviewListItem(t, state, "scenes", 0)
	if scene["scene_id"] != "E1S01" {
		t.Fatalf("scene_id = %v", scene["scene_id"])
	}
	utterances := scene["utterances"].([]any)
	if utterances[1].(map[string]any)["speaker"] != "阿离" {
		t.Fatalf("utterances[1].speaker = %v", utterances[1].(map[string]any)["speaker"])
	}
	pf, err := store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if !GateBlocksStep2(dir, pf, 1) {
		t.Fatal("pending 状态应阻塞 step2")
	}
	_ = path

	confirmed, err := svc.Confirm(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, confirmed) != ReviewStatusConfirmed {
		t.Fatalf("确认后状态 = %s", reviewStatus(t, confirmed))
	}
	if confirmedAt, _ := confirmed["confirmed_at"].(string); confirmedAt == "" {
		t.Fatal("确认后 confirmed_at 应非空")
	}
	pf, err = store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if GateBlocksStep2(dir, pf, 1) {
		t.Fatal("confirmed 状态应放行 step2")
	}
}

func TestScriptReviewDramaEditingAfterConfirmRepends(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationStoryboard)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	writeReviewStep1(t, dir, "step1_normalized_script.json", dramaStep1())
	if _, err := svc.Confirm(ctx, "demo", 1); err != nil {
		t.Fatal(err)
	}
	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusConfirmed {
		t.Fatalf("确认后状态 = %s", reviewStatus(t, state))
	}

	edited := dramaStep1()
	edited["scenes"].([]any)[0].(map[string]any)["utterances"].([]any)[1].(map[string]any)["text"] = "你怎么才回来。"
	if _, err := svc.SaveContent(ctx, "demo", 1, edited, nil); err != nil {
		t.Fatal(err)
	}

	state, err = svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("内容变更后状态 = %s", reviewStatus(t, state))
	}
	scene := reviewListItem(t, state, "scenes", 0)
	utterances := scene["utterances"].([]any)
	if utterances[1].(map[string]any)["text"] != "你怎么才回来。" {
		t.Fatalf("编辑未落盘: %v", utterances[1])
	}
}

func TestScriptReviewDramaWhitespaceReformatKeepsConfirmed(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationStoryboard)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	path := writeReviewStep1(t, dir, "step1_normalized_script.json", dramaStep1())
	if _, err := svc.Confirm(ctx, "demo", 1); err != nil {
		t.Fatal(err)
	}

	indented, err := json.MarshalIndent(dramaStep1(), "", "    ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, indented, 0o644); err != nil {
		t.Fatal(err)
	}

	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusConfirmed {
		t.Fatalf("纯空白重排不应改变确认状态，状态 = %s", reviewStatus(t, state))
	}
}

// --- 状态流转（narration） ---

func TestScriptReviewNarrationPendingThenConfirm(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentNarration, project.GenerationStoryboard)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	writeReviewStep1(t, dir, "step1_segments.json", narrationStep1())

	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("状态 = %s", reviewStatus(t, state))
	}
	segment := reviewListItem(t, state, "segments", 0)
	if novel, _ := segment["novel_text"].(string); !strings.HasPrefix(novel, "裴与") {
		t.Fatalf("novel_text = %v", segment["novel_text"])
	}

	confirmed, err := svc.Confirm(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, confirmed) != ReviewStatusConfirmed {
		t.Fatalf("确认后状态 = %s", reviewStatus(t, confirmed))
	}
}

func TestScriptReviewNarrationEditNovelTextRepends(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentNarration, project.GenerationStoryboard)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	writeReviewStep1(t, dir, "step1_segments.json", narrationStep1())
	if _, err := svc.Confirm(ctx, "demo", 1); err != nil {
		t.Fatal(err)
	}

	edited := narrationStep1()
	edited["segments"].([]any)[0].(map[string]any)["novel_text"] = "裴与出征后的第三年。"
	if _, err := svc.SaveContent(ctx, "demo", 1, edited, nil); err != nil {
		t.Fatal(err)
	}
	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("编辑后状态 = %s", reviewStatus(t, state))
	}
}

// --- 状态流转（reference_video） ---

func TestScriptReviewRVNoStep1ThenPendingThenConfirmed(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	svc := NewScriptReviewService(store, &stubReviewCaps{})

	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusNoStep1 {
		t.Fatalf("step1 未产出时状态 = %s", reviewStatus(t, state))
	}

	writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, rvStep1())
	state, err = svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("step1 产出后状态 = %s", reviewStatus(t, state))
	}
	unit := reviewListItem(t, state, "units", 0)
	if unit["unit_id"] != "E1U01" {
		t.Fatalf("unit_id = %v", unit["unit_id"])
	}
	shotText := unit["shots"].([]any)[0].(map[string]any)["text"]
	if text, _ := shotText.(string); !strings.HasPrefix(text, "@[阿离]") {
		t.Fatalf("shots[0].text = %v", shotText)
	}
	pf, err := store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if !GateBlocksStep2(dir, pf, 1) {
		t.Fatal("pending 状态应阻塞 step2")
	}

	confirmed, err := svc.Confirm(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, confirmed) != ReviewStatusConfirmed {
		t.Fatalf("确认后状态 = %s", reviewStatus(t, confirmed))
	}
	if confirmedAt, _ := confirmed["confirmed_at"].(string); confirmedAt == "" {
		t.Fatal("确认后 confirmed_at 应非空")
	}
	pf, err = store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if GateBlocksStep2(dir, pf, 1) {
		t.Fatal("confirmed 状态应放行 step2")
	}
}

func TestScriptReviewRVEditingShotTextRependsAndRederivesReferences(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	if err := store.Update("demo", func(pf *project.ProjectFile) error {
		if pf.Scenes == nil {
			pf.Scenes = project.AssetBucket{}
		}
		pf.Scenes["屋檐"] = project.WrapAssetEntry(map[string]any{"name": "屋檐", "description": "雨夜屋檐"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, rvStep1())
	if _, err := svc.Confirm(ctx, "demo", 1); err != nil {
		t.Fatal(err)
	}
	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusConfirmed {
		t.Fatalf("确认后状态 = %s", reviewStatus(t, state))
	}

	edited := rvStep1()
	edited["units"].([]any)[0].(map[string]any)["shots"].([]any)[1].(map[string]any)["text"] = "镜头扫过 @[屋檐]。"
	if _, err := svc.SaveContent(ctx, "demo", 1, edited, nil); err != nil {
		t.Fatal(err)
	}

	state, err = svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("编辑后状态 = %s", reviewStatus(t, state))
	}
	got := reviewRefPairs(t, reviewListItem(t, state, "units", 0))
	want := [][2]string{{"character", "阿离"}, {"scene", "屋檐"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("references = %v，期望 %v", got, want)
	}
}

func TestScriptReviewRVQuarantinedStep1BlocksConfirmAndStep2(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, rvStep1())
	if _, err := svc.Confirm(ctx, "demo", 1); err != nil {
		t.Fatal(err)
	}
	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusConfirmed {
		t.Fatalf("确认后状态 = %s", reviewStatus(t, state))
	}

	if _, err := reference.WriteQuarantine(dir, 1, reference.QuarantineKindStep1,
		map[string]any{"units": []any{}},
		[]reference.DraftViolation{{Code: "empty_text", Label: "unit E1U01", Message: "坏"}},
		nil, nil,
	); err != nil {
		t.Fatal(err)
	}

	state, err = svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("隔离草稿在场时状态 = %s", reviewStatus(t, state))
	}
	pf, err := store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if !GateBlocksStep2(dir, pf, 1) {
		t.Fatal("隔离草稿在场应阻塞 step2")
	}
	_, err = svc.Confirm(ctx, "demo", 1)
	requireReviewCode(t, err, ScriptReviewCodeQuarantined)

	if err := reference.ClearQuarantine(dir, 1, reference.QuarantineKindStep1); err != nil {
		t.Fatal(err)
	}
	state, err = svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusConfirmed {
		t.Fatalf("草稿清除后状态 = %s（内容未变，确认仍应有效）", reviewStatus(t, state))
	}
}

func TestScriptReviewRVQuarantineNotApplicableToNonReferencePaths(t *testing.T) {
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationStoryboard)
	pf, err := store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Step1QuarantinePath(dir, pf, 1); ok {
		t.Fatal("drama 无隔离草稿概念，路径解析应返回 false")
	}
	if Step1Quarantined(dir, pf, 1) {
		t.Fatal("drama 不应报告隔离态")
	}
}

func TestScriptReviewRVConfirmRejectsUnitDurationOutOfRange(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	bad := rvStep1()
	bad["units"].([]any)[0].(map[string]any)["duration_seconds"] = float64(9999)
	writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, bad)

	_, err := svc.Confirm(ctx, "demo", 1)
	requireReviewCode(t, err, ScriptReviewCodeInvalidContent)
}

func TestScriptReviewRVConfirmRederivesReferencesWhenEditedOutsideSave(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	if err := store.Update("demo", func(pf *project.ProjectFile) error {
		if pf.Scenes == nil {
			pf.Scenes = project.AssetBucket{}
		}
		pf.Scenes["屋檐"] = project.WrapAssetEntry(map[string]any{"name": "屋檐", "description": "雨夜屋檐"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewScriptReviewService(store, &stubReviewCaps{})

	stale := rvStep1()
	stale["units"].([]any)[0].(map[string]any)["shots"].([]any)[1].(map[string]any)["text"] = "镜头扫过 @[屋檐]。"
	path := writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, stale)

	confirmed, err := svc.Confirm(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, confirmed) != ReviewStatusConfirmed {
		t.Fatalf("确认后状态 = %s", reviewStatus(t, confirmed))
	}
	want := [][2]string{{"character", "阿离"}, {"scene", "屋檐"}}
	if got := reviewRefPairs(t, reviewListItem(t, confirmed, "units", 0)); !reflect.DeepEqual(got, want) {
		t.Fatalf("确认后的 references = %v，期望 %v", got, want)
	}

	onDisk := readReviewStep1File(t, path)
	if got := reviewRefPairs(t, onDisk["units"].([]any)[0].(map[string]any)); !reflect.DeepEqual(got, want) {
		t.Fatalf("落盘 references = %v，期望 %v", got, want)
	}
}

// --- 时长档位（reference_video 专属） ---

func TestScriptReviewDurationTiersNarrowsByResolutionConstraint(t *testing.T) {
	ctx := context.Background()
	store, _ := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	resolver := &stubReviewCaps{
		view: config.VideoCapabilityView{
			ProviderID:         "gemini-aistudio",
			ModelID:            "veo-3.1-generate-preview",
			SupportedDurations: []int{4, 6, 8},
		},
		i2vErr: errors.New("i2v bucket unresolvable in this test"),
	}
	svc := NewScriptReviewService(store, resolver)

	tiers, err := svc.GetReferenceDurationTiers(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if tiers == nil {
		t.Fatal("档位表不应为 nil")
	}
	if got := tiers["with_references"].([]int); !reflect.DeepEqual(got, []int{8}) {
		t.Fatalf("with_references = %v", got)
	}
	if got := tiers["without_references"].([]int); !reflect.DeepEqual(got, []int{8}) {
		t.Fatalf("without_references = %v", got)
	}
}

func TestScriptReviewDurationTiersNoneWhenCapsAndRawUnresolved(t *testing.T) {
	ctx := context.Background()
	store, _ := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	resolver := &stubReviewCaps{resolveErr: errors.New("video_capabilities backend unreachable")}
	svc := NewScriptReviewService(store, resolver)

	tiers, err := svc.GetReferenceDurationTiers(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if tiers != nil {
		t.Fatalf("两条档位来源都失效时应返回 nil，得到 %v", tiers)
	}
}

func TestScriptReviewDurationTiersNoneForNonReferenceVideo(t *testing.T) {
	ctx := context.Background()
	store, _ := makeReviewProject(t, project.ContentDrama, project.GenerationStoryboard)
	resolver := &stubReviewCaps{
		view: config.VideoCapabilityView{
			ProviderID:         "custom-acme",
			ModelID:            "acme-video",
			SupportedDurations: []int{5, 10},
		},
	}
	svc := NewScriptReviewService(store, resolver)

	tiers, err := svc.GetReferenceDurationTiers(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if tiers != nil {
		t.Fatalf("非 reference_video 变体应返回 nil，得到 %v", tiers)
	}
}

func TestScriptReviewDurationTiersUsesCapsForCustomProvider(t *testing.T) {
	ctx := context.Background()
	store, _ := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	resolver := &stubReviewCaps{
		view: config.VideoCapabilityView{
			ProviderID:         "custom-acme",
			ModelID:            "acme-video",
			SupportedDurations: []int{5, 10},
		},
	}
	svc := NewScriptReviewService(store, resolver)

	tiers, err := svc.GetReferenceDurationTiers(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if tiers == nil {
		t.Fatal("自定义供应商经 caps 应解析到档位表")
	}
	if got := tiers["with_references"].([]int); !reflect.DeepEqual(got, []int{5, 10}) {
		t.Fatalf("with_references = %v", got)
	}
	if got := tiers["without_references"].([]int); !reflect.DeepEqual(got, []int{5, 10}) {
		t.Fatalf("without_references = %v", got)
	}
}

// --- 存量迁移（read 时收编 + 确认平移） ---

func TestScriptReviewMigrationTakesSlotFromCaps(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	resolver := &stubReviewCaps{
		view: config.VideoCapabilityView{
			ProviderID:         "custom-acme",
			ModelID:            "acme-video",
			SupportedDurations: []int{4, 8, 12},
		},
	}
	svc := NewScriptReviewService(store, resolver)
	path := writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, legacyRVStep1WithDurations(t, 6, 4))

	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := service.IntValueOrZero(reviewListItem(t, state, "units", 0)["duration_seconds"]); got != 12 {
		t.Fatalf("迁移后 duration_seconds = %d，期望取档 12", got)
	}
	onDisk := readReviewStep1File(t, path)
	if got := service.IntValueOrZero(onDisk["units"].([]any)[0].(map[string]any)["duration_seconds"]); got != 12 {
		t.Fatalf("落盘 duration_seconds = %d，期望 12", got)
	}
}

func TestScriptReviewMigrationCustomProviderTakesSlotFromCaps(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	resolver := &stubReviewCaps{
		view: config.VideoCapabilityView{
			ProviderID:         "custom-acme",
			ModelID:            "acme-video",
			SupportedDurations: []int{5, 10},
		},
	}
	svc := NewScriptReviewService(store, resolver)
	path := writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, legacyRVStep1WithDurations(t, 4, 3))

	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := service.IntValueOrZero(reviewListItem(t, state, "units", 0)["duration_seconds"]); got != 10 {
		t.Fatalf("迁移后 duration_seconds = %d，期望取档 10", got)
	}
	if got := state["supported_durations"].([]int); !reflect.DeepEqual(got, []int{5, 10}) {
		t.Fatalf("supported_durations = %v，期望 [5 10]", got)
	}
	onDisk := readReviewStep1File(t, path)
	if got := service.IntValueOrZero(onDisk["units"].([]any)[0].(map[string]any)["duration_seconds"]); got != 10 {
		t.Fatalf("落盘 duration_seconds = %d，期望 10", got)
	}
}

func TestScriptReviewMigrationCustomProviderDirectConfirmTakesSlot(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	resolver := &stubReviewCaps{
		view: config.VideoCapabilityView{
			ProviderID:         "custom-acme",
			ModelID:            "acme-video",
			SupportedDurations: []int{5, 10},
		},
	}
	svc := NewScriptReviewService(store, resolver)
	writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, legacyRVStep1WithDurations(t, 4, 3))

	state, err := svc.Confirm(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusConfirmed {
		t.Fatalf("直连确认后状态 = %s", reviewStatus(t, state))
	}
	if got := service.IntValueOrZero(reviewListItem(t, state, "units", 0)["duration_seconds"]); got != 10 {
		t.Fatalf("确认收编后 duration_seconds = %d，期望 10", got)
	}
}

func TestScriptReviewMigrationBuiltinFallsBackToRegistryWhenCapsUnavailable(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	if err := store.Update("demo", func(pf *project.ProjectFile) error {
		pf.VideoBackend = "gemini-aistudio/veo-3.1-generate-preview"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	resolver := &stubReviewCaps{resolveErr: errors.New("video_capabilities backend unreachable")}
	svc := NewScriptReviewService(store, resolver)
	writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, legacyRVStep1WithDurations(t, 4, 3))

	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := state["supported_durations"].([]int); !reflect.DeepEqual(got, []int{4, 6, 8}) {
		t.Fatalf("supported_durations = %v，期望 registry 档位 [4 6 8]", got)
	}
	if got := service.IntValueOrZero(reviewListItem(t, state, "units", 0)["duration_seconds"]); got != 8 {
		t.Fatalf("迁移后 duration_seconds = %d，期望按 registry 档位取 8", got)
	}
}

func TestScriptReviewMigrationFallsBackToStructuralClamp(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, legacyRVStep1WithDurations(t, 6, 4))

	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := service.IntValueOrZero(reviewListItem(t, state, "units", 0)["duration_seconds"]); got != 10 {
		t.Fatalf("无档位表时应按结构区间保留 10s，得到 %d", got)
	}
}

func TestScriptReviewLegacyDraftMigratedOnReadWrittenBackIdempotent(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	path := writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, legacyRVStep1())

	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	unit := reviewListItem(t, state, "units", 0)
	if got := service.IntValueOrZero(unit["duration_seconds"]); got != 8 {
		t.Fatalf("收编后 duration_seconds = %d，期望求和 8", got)
	}
	reviewShotsHaveNoDurationKey(t, unit)

	onDisk := readReviewStep1File(t, path)
	diskUnit := onDisk["units"].([]any)[0].(map[string]any)
	if got := service.IntValueOrZero(diskUnit["duration_seconds"]); got != 8 {
		t.Fatalf("落盘 duration_seconds = %d", got)
	}
	reviewShotsHaveNoDurationKey(t, diskUnit)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetState(ctx, "demo", 1); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("二次读不应再改写落盘内容")
	}
}

func TestScriptReviewLegacyDraftCanBeConfirmedAndSaved(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, legacyRVStep1())

	confirmed, err := svc.Confirm(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, confirmed) != ReviewStatusConfirmed {
		t.Fatalf("确认后状态 = %s", reviewStatus(t, confirmed))
	}

	edited := reviewContent(t, confirmed)
	edited["units"].([]any)[0].(map[string]any)["shots"].([]any)[0].(map[string]any)["text"] = "@[阿离] 收伞。"
	state, err := svc.SaveContent(ctx, "demo", 1, edited, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("编辑保存后状态 = %s", reviewStatus(t, state))
	}
}

func TestScriptReviewConfirmSurvivesMigrationWithoutReopening(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	path := writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, legacyRVStep1())

	before, err := service.ContentFingerprint(path)
	if err != nil || before == nil {
		t.Fatalf("取迁移前指纹失败: %v", err)
	}
	if err := store.Update("demo", func(pf *project.ProjectFile) error {
		service.ApplyConfirmation(pf, 1, *before, "2026-01-01T00:00:00Z")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusConfirmed {
		t.Fatalf("纯格式收编不应回退确认，状态 = %s", reviewStatus(t, state))
	}
	state, err = svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusConfirmed {
		t.Fatalf("二次读状态 = %s", reviewStatus(t, state))
	}
	if confirmedAt, _ := state["confirmed_at"].(string); confirmedAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("confirmed_at = %v", state["confirmed_at"])
	}
	if got := service.IntValueOrZero(reviewListItem(t, state, "units", 0)["duration_seconds"]); got != 8 {
		t.Fatalf("收编后 duration_seconds = %d", got)
	}
}

func TestScriptReviewConfirmReopensWhenMigrationClampsDuration(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	resolver := &stubReviewCaps{
		view: config.VideoCapabilityView{
			ProviderID:         "custom-acme",
			ModelID:            "acme-video",
			SupportedDurations: []int{4, 8, 12},
		},
	}
	svc := NewScriptReviewService(store, resolver)
	path := writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, legacyRVStep1WithDurations(t, 60, 30))

	before, err := service.ContentFingerprint(path)
	if err != nil || before == nil {
		t.Fatalf("取迁移前指纹失败: %v", err)
	}
	if err := store.Update("demo", func(pf *project.ProjectFile) error {
		service.ApplyConfirmation(pf, 1, *before, "2026-01-01T00:00:00Z")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("clamp 改写时长后应退回待审，状态 = %s", reviewStatus(t, state))
	}
	if got := service.IntValueOrZero(reviewListItem(t, state, "units", 0)["duration_seconds"]); got != 12 {
		t.Fatalf("clamp 后 duration_seconds = %d，期望 12", got)
	}
}

func TestScriptReviewClampingMigrationReopensGrandfatheredEpisode(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	resolver := &stubReviewCaps{
		view: config.VideoCapabilityView{
			ProviderID:         "custom-acme",
			ModelID:            "acme-video",
			SupportedDurations: []int{4, 8, 12},
		},
	}
	svc := NewScriptReviewService(store, resolver)
	writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, legacyRVStep1WithDurations(t, 60, 30))
	writeReviewStep2(t, dir)

	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("grandfather 存量集被 clamp 后应退回待审，状态 = %s", reviewStatus(t, state))
	}
	state, err = svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("幂等重读不应放回 grandfather 放行，状态 = %s", reviewStatus(t, state))
	}
}

func TestScriptReviewConfirmDirectCallConfirmsMigratedContent(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	resolver := &stubReviewCaps{
		view: config.VideoCapabilityView{
			ProviderID:         "custom-acme",
			ModelID:            "acme-video",
			SupportedDurations: []int{4, 8, 12},
		},
	}
	svc := NewScriptReviewService(store, resolver)
	writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, legacyRVStep1WithDurations(t, 60, 30))

	state, err := svc.Confirm(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusConfirmed {
		t.Fatalf("直连确认后状态 = %s", reviewStatus(t, state))
	}
	if got := service.IntValueOrZero(reviewListItem(t, state, "units", 0)["duration_seconds"]); got != 12 {
		t.Fatalf("确认收编后 duration_seconds = %d，期望 12", got)
	}
}

func TestScriptReviewMigrationDoesNotConfirmUnconfirmedEpisode(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, legacyRVStep1())

	if err := store.Update("demo", func(pf *project.ProjectFile) error {
		service.ApplyConfirmation(pf, 1, strings.Repeat("0", 64), "2026-01-01T00:00:00Z")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("指纹本就对不上时照常待审，状态 = %s", reviewStatus(t, state))
	}
}

// --- 适用范围 ---

func TestScriptReviewReferenceVideoApplicable(t *testing.T) {
	ctx := context.Background()
	store, _ := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	pf, err := store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if Step1Kind(pf) != step1KindReferenceVideo {
		t.Fatalf("step1 变体 = %s", Step1Kind(pf))
	}
	if !IsReviewApplicable(pf) {
		t.Fatal("reference_video 应纳入 gate")
	}
	state, err := NewScriptReviewService(store, &stubReviewCaps{}).GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusNoStep1 {
		t.Fatalf("未产 step1 时状态 = %s", reviewStatus(t, state))
	}
}

func TestScriptReviewAdNotApplicable(t *testing.T) {
	ctx := context.Background()
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("addemo", "Ad", project.ContentAd, project.GenerationStoryboard, "Anime"); err != nil {
		t.Fatal(err)
	}
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	pf, err := store.Get("addemo")
	if err != nil {
		t.Fatal(err)
	}
	if Step1Kind(pf) != "" {
		t.Fatalf("ad 无结构化 step1，step1 变体应为空，得到 %q", Step1Kind(pf))
	}
	state, err := svc.GetState(ctx, "addemo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusNotApplicable {
		t.Fatalf("ad 状态 = %s", reviewStatus(t, state))
	}
}

// --- 编辑校验 + 确认前置错误 ---

func TestScriptReviewSaveInvalidContentRejected(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationStoryboard)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	writeReviewStep1(t, dir, "step1_normalized_script.json", dramaStep1())

	bad := dramaStep1()
	bad["scenes"].([]any)[0].(map[string]any)["utterances"].([]any)[1] =
		map[string]any{"kind": "dialogue", "speaker": nil, "text": "无人"}
	_, err := svc.SaveContent(ctx, "demo", 1, bad, nil)
	requireReviewCode(t, err, ScriptReviewCodeInvalidContent)
}

func TestScriptReviewConfirmWithoutStep1Rejected(t *testing.T) {
	ctx := context.Background()
	store, _ := makeReviewProject(t, project.ContentDrama, project.GenerationStoryboard)
	svc := NewScriptReviewService(store, &stubReviewCaps{})

	_, err := svc.Confirm(ctx, "demo", 1)
	requireReviewCode(t, err, ScriptReviewCodeNoStep1)
}

func TestScriptReviewSaveNotApplicableRejected(t *testing.T) {
	ctx := context.Background()
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("addemo", "Ad", project.ContentAd, project.GenerationStoryboard, "Anime"); err != nil {
		t.Fatal(err)
	}
	svc := NewScriptReviewService(store, &stubReviewCaps{})

	_, err := svc.SaveContent(ctx, "addemo", 1, dramaStep1(), nil)
	requireReviewCode(t, err, ScriptReviewCodeNotApplicable)
}

func TestScriptReviewGetStateUnregisteredEpisodeRejected(t *testing.T) {
	ctx := context.Background()
	store, _ := makeReviewProject(t, project.ContentDrama, project.GenerationStoryboard)
	svc := NewScriptReviewService(store, &stubReviewCaps{})

	_, err := svc.GetState(ctx, "demo", 99)
	requireReviewCode(t, err, ScriptReviewCodeEpisodeNotFound)
}

func TestScriptReviewSaveUnregisteredEpisodeWritesNoOrphan(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationStoryboard)
	svc := NewScriptReviewService(store, &stubReviewCaps{})

	_, err := svc.SaveContent(ctx, "demo", 99, dramaStep1(), nil)
	requireReviewCode(t, err, ScriptReviewCodeEpisodeNotFound)
	orphan := filepath.Join(dir, "drafts", "episode_99", "step1_normalized_script.json")
	if _, statErr := os.Stat(orphan); !os.IsNotExist(statErr) {
		t.Fatalf("不得落孤儿 step1 文件: %v", statErr)
	}
}

func TestScriptReviewSaveStaleFingerprintConflictsReferenceVideo(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	path := writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, rvStep1())
	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	stale := reviewFingerprint(t, state)

	other := rvStep1()
	other["units"].([]any)[0].(map[string]any)["shots"].([]any)[0].(map[string]any)["text"] = "@[阿离] 转身离开。"
	if _, err := svc.SaveContent(ctx, "demo", 1, other, nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	mine := rvStep1()
	mine["units"].([]any)[0].(map[string]any)["shots"].([]any)[1].(map[string]any)["text"] = "@[裴与] 下马。"
	_, err = svc.SaveContent(ctx, "demo", 1, mine, &stale)
	requireReviewCode(t, err, ScriptReviewCodeConflict)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("冲突时不得落盘覆盖")
	}

	state, err = svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	fresh := reviewFingerprint(t, state)
	state, err = svc.SaveContent(ctx, "demo", 1, mine, &fresh)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("最新基线重试后状态 = %s", reviewStatus(t, state))
	}
	onDisk := readReviewStep1File(t, path)
	shotText := onDisk["units"].([]any)[0].(map[string]any)["shots"].([]any)[1].(map[string]any)["text"]
	if shotText != "@[裴与] 下马。" {
		t.Fatalf("落盘 shots[1].text = %v", shotText)
	}
}

func TestScriptReviewSaveStaleFingerprintConflictsDrama(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationStoryboard)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	path := writeReviewStep1(t, dir, "step1_normalized_script.json", dramaStep1())
	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	stale := reviewFingerprint(t, state)

	other := dramaStep1()
	other["title"] = "另一方改的标题"
	if _, err := svc.SaveContent(ctx, "demo", 1, other, nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.SaveContent(ctx, "demo", 1, dramaStep1(), &stale)
	requireReviewCode(t, err, ScriptReviewCodeConflict)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("冲突时不得落盘覆盖")
	}
}

func TestScriptReviewSaveWithoutFingerprintSkipsBaselineCheck(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, rvStep1())
	other := rvStep1()
	other["units"].([]any)[0].(map[string]any)["shots"].([]any)[0].(map[string]any)["text"] = "@[阿离] 转身离开。"
	if _, err := svc.SaveContent(ctx, "demo", 1, other, nil); err != nil {
		t.Fatal(err)
	}

	state, err := svc.SaveContent(ctx, "demo", 1, rvStep1(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("无基线直连保存后状态 = %s", reviewStatus(t, state))
	}
}

func TestScriptReviewRVSaveClearsStaleStep2QuarantineOnChange(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationReferenceVideo)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	writeReviewStep1(t, dir, service.ReferenceVideoStep1Filename, rvStep1())
	// 先经一次保存把归一化形状落盘，「内容未变」的比较才有同一基准。
	if _, err := svc.SaveContent(ctx, "demo", 1, rvStep1(), nil); err != nil {
		t.Fatal(err)
	}

	if _, err := reference.WriteQuarantine(dir, 1, reference.QuarantineKindStep2,
		map[string]any{"units": []any{map[string]any{"text": "旧基底"}}}, nil, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveContent(ctx, "demo", 1, rvStep1(), nil); err != nil {
		t.Fatal(err)
	}
	if !reference.QuarantineExists(dir, 1, reference.QuarantineKindStep2) {
		t.Fatal("内容未变的保存不应清 step2 隔离草稿")
	}

	edited := rvStep1()
	edited["units"].([]any)[0].(map[string]any)["shots"].([]any)[0].(map[string]any)["text"] = "@[阿离] 转身离开。"
	if _, err := svc.SaveContent(ctx, "demo", 1, edited, nil); err != nil {
		t.Fatal(err)
	}
	if reference.QuarantineExists(dir, 1, reference.QuarantineKindStep2) {
		t.Fatal("内容变更的保存应清 step2 隔离草稿")
	}
}

func TestScriptReviewConfirmCorruptStep1Rejected(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationStoryboard)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	path := writeReviewStep1(t, dir, "step1_normalized_script.json", dramaStep1())
	if err := os.WriteFile(path, []byte("\x00\x01 not json at all {"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := svc.Confirm(ctx, "demo", 1)
	requireReviewCode(t, err, ScriptReviewCodeInvalidContent)
}

func TestScriptReviewConfirmNonObjectStep1Rejected(t *testing.T) {
	ctx := context.Background()
	for _, payload := range []string{"null", "[]", "\"not an object\"", "{}"} {
		store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationStoryboard)
		svc := NewScriptReviewService(store, &stubReviewCaps{})
		path := writeReviewStep1(t, dir, "step1_normalized_script.json", dramaStep1())
		if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := svc.Confirm(ctx, "demo", 1)
		requireReviewCode(t, err, ScriptReviewCodeInvalidContent)
	}
}

// --- 存量穷举 ---

func TestScriptReviewGrandfatheredConfirmedWhenStep2Exists(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationStoryboard)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	writeReviewStep1(t, dir, "step1_normalized_script.json", dramaStep1())
	writeReviewStep2(t, dir)

	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusConfirmed {
		t.Fatalf("grandfather 存量集状态 = %s", reviewStatus(t, state))
	}
	pf, err := store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if GateBlocksStep2(dir, pf, 1) {
		t.Fatal("grandfather 放行不应阻塞 step2")
	}
}

func TestScriptReviewFingerprintMismatchAfterEditRepends(t *testing.T) {
	ctx := context.Background()
	store, dir := makeReviewProject(t, project.ContentDrama, project.GenerationStoryboard)
	svc := NewScriptReviewService(store, &stubReviewCaps{})
	writeReviewStep1(t, dir, "step1_normalized_script.json", dramaStep1())
	writeReviewStep2(t, dir)
	if _, err := svc.Confirm(ctx, "demo", 1); err != nil {
		t.Fatal(err)
	}

	edited := dramaStep1()
	edited["scenes"].([]any)[0].(map[string]any)["source_text"] = "改写后的原文锚"
	if _, err := svc.SaveContent(ctx, "demo", 1, edited, nil); err != nil {
		t.Fatal(err)
	}
	state, err := svc.GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("指纹失配应优先于 grandfather 判据，状态 = %s", reviewStatus(t, state))
	}
}

// --- 手动预拆分自愈 ---

func TestScriptReviewGetStateSelfHealsOrphan(t *testing.T) {
	ctx := context.Background()
	store, dir := makeManualSplitProject(t, project.ContentNarration)
	writeReviewSourceText(t, dir, "episode_1.txt", "裴与出征后的第二年。")
	writeReviewStep1(t, dir, "step1_segments.json", narrationStep1())

	state, err := NewScriptReviewService(store, &stubReviewCaps{}).GetState(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, state) != ReviewStatusPendingReview {
		t.Fatalf("自愈后状态 = %s", reviewStatus(t, state))
	}
	pf, err := store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	entry := service.FindEpisodeEntry(pf, 1)
	if entry == nil {
		t.Fatal("自愈应登记第 1 集条目")
	}
	if entry.LedgerStatus != project.LedgerStatusConsumed {
		t.Fatalf("ledger_status = %s，已有 step1 中间文件应为 consumed", entry.LedgerStatus)
	}
	if entry.SourceRange != nil {
		t.Fatalf("自愈只登记不锚定，source_range 应为空: %v", entry.SourceRange)
	}
}

func TestScriptReviewConfirmSelfHealsAndUnblocksStep2(t *testing.T) {
	ctx := context.Background()
	store, dir := makeManualSplitProject(t, project.ContentDrama)
	writeReviewSourceText(t, dir, "episode_1.txt", "任意派生内容")
	writeReviewStep1(t, dir, "step1_normalized_script.json", dramaStep1())

	confirmed, err := NewScriptReviewService(store, &stubReviewCaps{}).Confirm(ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if reviewStatus(t, confirmed) != ReviewStatusConfirmed {
		t.Fatalf("自愈后确认状态 = %s", reviewStatus(t, confirmed))
	}
	pf, err := store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if GateBlocksStep2(dir, pf, 1) {
		t.Fatal("确认后应放行 step2")
	}
}

func TestScriptReviewSelfHealNeverAnchorsEvenWhenSourceTextMatches(t *testing.T) {
	ctx := context.Background()
	store, dir := makeManualSplitProject(t, project.ContentNarration)
	writeReviewSourceText(t, dir, "novel.txt", "裴与出征后的第二年，送回一个襁褓中的婴儿。后续内容在此。")
	writeReviewSourceText(t, dir, "episode_1.txt", "裴与出征后的第二年，送回一个襁褓中的婴儿。")

	if _, err := NewScriptReviewService(store, &stubReviewCaps{}).GetState(ctx, "demo", 1); err != nil {
		t.Fatal(err)
	}
	pf, err := store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	entry := service.FindEpisodeEntry(pf, 1)
	if entry == nil {
		t.Fatal("自愈应登记条目")
	}
	if entry.SourceRange != nil {
		t.Fatalf("自愈只登记不锚定，source_range 应为空: %v", entry.SourceRange)
	}
}

func TestScriptReviewSelfHealRegistersAllOrphans(t *testing.T) {
	ctx := context.Background()
	store, dir := makeManualSplitProject(t, project.ContentNarration)
	writeReviewSourceText(t, dir, "episode_1.txt", "第一集内容")
	writeReviewSourceText(t, dir, "episode_2.txt", "第二集内容")

	if _, err := NewScriptReviewService(store, &stubReviewCaps{}).GetState(ctx, "demo", 1); err != nil {
		t.Fatal(err)
	}
	pf, err := store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if service.FindEpisodeEntry(pf, 1) == nil || service.FindEpisodeEntry(pf, 2) == nil {
		t.Fatalf("自愈应登记全部孤儿集号: %+v", pf.Episodes)
	}
}

func TestScriptReviewSelfHealPreservesExistingLedgerEntries(t *testing.T) {
	ctx := context.Background()
	store, dir := makeManualSplitProject(t, project.ContentNarration)
	if err := store.Update("demo", func(pf *project.ProjectFile) error {
		pf.Episodes = append(pf.Episodes, project.EpisodeMeta{
			Episode: 1, Title: "第一集", ScriptFile: "scripts/episode_1.json",
			LedgerStatus: project.LedgerStatusPlanned,
			SourceRange:  &project.SourceRange{SourceFile: "source/novel.txt", Start: 0, End: 5},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	writeReviewSourceText(t, dir, "episode_2.txt", "第二集派生内容")

	if _, err := NewScriptReviewService(store, &stubReviewCaps{}).GetState(ctx, "demo", 2); err != nil {
		t.Fatal(err)
	}
	pf, err := store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	entry := service.FindEpisodeEntry(pf, 1)
	if entry == nil {
		t.Fatal("第 1 集条目应保留")
	}
	if entry.LedgerStatus != project.LedgerStatusPlanned {
		t.Fatalf("已登记条目的 ledger_status 被改写: %s", entry.LedgerStatus)
	}
	want := project.SourceRange{SourceFile: "source/novel.txt", Start: 0, End: 5}
	if entry.SourceRange == nil || *entry.SourceRange != want {
		t.Fatalf("已登记条目的 source_range 被改写: %+v", entry.SourceRange)
	}
	if service.FindEpisodeEntry(pf, 2) == nil {
		t.Fatal("孤儿集 2 应被登记")
	}
}

func TestScriptReviewSelfHealNotAppliedWhenDerivativeMissing(t *testing.T) {
	ctx := context.Background()
	store, _ := makeManualSplitProject(t, project.ContentNarration)
	svc := NewScriptReviewService(store, &stubReviewCaps{})

	_, err := svc.GetState(ctx, "demo", 1)
	requireReviewCode(t, err, ScriptReviewCodeEpisodeNotFound)
	pf, err := store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(pf.Episodes) != 0 {
		t.Fatalf("真正缺失的集号不得触发自愈登记: %+v", pf.Episodes)
	}
}

func TestScriptReviewSelfHealIdempotentNoDuplicates(t *testing.T) {
	ctx := context.Background()
	store, dir := makeManualSplitProject(t, project.ContentNarration)
	writeReviewSourceText(t, dir, "episode_1.txt", "第一集派生内容")
	svc := NewScriptReviewService(store, &stubReviewCaps{})

	if _, err := svc.GetState(ctx, "demo", 1); err != nil {
		t.Fatal(err)
	}
	first, err := store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	firstEntry := service.FindEpisodeEntry(first, 1)
	if firstEntry == nil {
		t.Fatal("首次自愈应登记条目")
	}

	if _, err := svc.GetState(ctx, "demo", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetState(ctx, "demo", 1); err != nil {
		t.Fatal(err)
	}
	pf, err := store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	matches := 0
	for index := range pf.Episodes {
		if pf.Episodes[index].Episode == 1 {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("重复自愈产生 %d 条集号 1 条目", matches)
	}
	if entry := service.FindEpisodeEntry(pf, 1); entry.LedgerStatus != firstEntry.LedgerStatus {
		t.Fatalf("重复自愈改写了已登记条目: %s", entry.LedgerStatus)
	}
}
