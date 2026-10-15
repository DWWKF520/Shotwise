package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
)

// fakeScriptStore 在内存剧本上执行 mutate，记录成功写盘次数。
type fakeScriptStore struct {
	script  map[string]any
	writes  int
	forced  error
	inputFn func(filename string)
	last    string
}

func (f *fakeScriptStore) UpdateScript(_, filename string, mutate func(map[string]any) error) error {
	f.last = filename
	if f.forced != nil {
		return f.forced
	}
	if err := mutate(f.script); err != nil {
		return err
	}
	f.writes++
	return nil
}

func scriptFixture() map[string]any {
	return map[string]any{
		"title": "第1集", "content_mode": "narration", "episode": float64(1),
		"segments": []any{
			map[string]any{"segment_id": "E1S1", "image_prompt": map[string]any{"scene": "旧画面"}},
			map[string]any{"segment_id": "E1S2",
				"image_prompt": map[string]any{"scene": "第二镜"},
				"video_prompt": map[string]any{"action": "旧动作"}},
		},
	}
}

func scriptSegment(t *testing.T, store *fakeScriptStore, id string) map[string]any {
	t.Helper()
	for _, item := range store.script["segments"].([]any) {
		segment := item.(map[string]any)
		if segment["segment_id"] == id {
			return segment
		}
	}
	t.Fatalf("分镜 %s 不存在", id)
	return nil
}

func TestPatchEpisodeScriptAppliesBatchAndFlagsRegen(t *testing.T) {
	store := &fakeScriptStore{script: scriptFixture()}
	tl, err := newPatchEpisodeScriptTool("proj", store)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json","edits":{
		"E1S1":{"image_prompt.scene":"新画面","duration_seconds":4},
		"E1S2":{"video_prompt.action":"跑"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if store.writes != 1 {
		t.Fatalf("应写盘一次: %d", store.writes)
	}
	if store.last != "episode_1.json" {
		t.Fatalf("文件名透传有误: %q", store.last)
	}
	if got := scriptSegment(t, store, "E1S1")["image_prompt"].(map[string]any)["scene"]; got != "新画面" {
		t.Fatalf("嵌套字段未写入: %v", got)
	}
	if !strings.Contains(out, "已更新 2 个分镜的字段") {
		t.Fatalf("汇总文本不符: %s", out)
	}
	// 只有改到 image_prompt / video_prompt 的分镜进重生成提醒。
	if !strings.Contains(out, "E1S1") || !strings.Contains(out, "E1S2") {
		t.Fatalf("两条都应命中重生成提醒: %s", out)
	}
}

func TestPatchEpisodeScriptRejectsBadInput(t *testing.T) {
	store := &fakeScriptStore{script: scriptFixture()}
	tl, err := newPatchEpisodeScriptTool("proj", store)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		`{"script":"episode_1.json","edits":{}}`:                                                 "edits 必须是非空",
		`{"script":"episode_1.json","edits":{"E1S1":{}}}`:                                        "非空 { 字段路径: 值 } 映射",
		`{"script":"episode_1.json","edits":{"E9S9":{"title":"x"}}}`:                             "未找到 id",
		`{"script":"episode_1.json","edits":{"E1S1":{"generated_assets.storyboard_image":"x"}}}`: "不可改 generated_assets",
		`{"script":"episode_1.json","edits":{"E1S1":{"segment_id":"E1S9"}}}`:                     "不可改分镜 id 字段",
		`{"script":"sub/episode_1.json","edits":{"E1S1":{"title":"x"}}}`:                         "纯文件名",
	}
	for args, want := range cases {
		out, err := tl.InvokableRun(context.Background(), args)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, want) || !strings.Contains(out, `"is_error":true`) {
			t.Errorf("args %s: 期望拒绝含 %q，实际 %s", args, want, out)
		}
	}
	if store.writes != 0 {
		t.Fatalf("全部被拒时应零落盘: %d", store.writes)
	}

	// 未注入剧本存储时 deny 而不是 panic。
	denied, err := newPatchEpisodeScriptTool("proj", nil)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := denied.InvokableRun(context.Background(), `{"script":"episode_1.json","edits":{"E1S1":{"title":"x"}}}`)
	if !strings.Contains(out, "script editing not configured") {
		t.Fatalf("未注入应 deny: %s", out)
	}
}

func TestInsertRemoveSplitSeedgmentTools(t *testing.T) {
	store := &fakeScriptStore{script: scriptFixture()}

	insert, err := newInsertSegmentTool("proj", store)
	if err != nil {
		t.Fatal(err)
	}
	out, err := insert.InvokableRun(context.Background(),
		`{"script":"episode_1.json","after_id":"E1S1","item":{"image_prompt":{"scene":"新镜"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "E1S1, E1S1_1, E1S2") {
		t.Fatalf("插入后顺序不符: %s", out)
	}
	if len(scriptSegment(t, store, "E1S1_1")["generated_assets"].(map[string]any)) != 0 {
		t.Fatal("新分镜资产应为空")
	}

	remove, err := newRemoveSegmentTool("proj", store)
	if err != nil {
		t.Fatal(err)
	}
	out, err = remove.InvokableRun(context.Background(), `{"script":"episode_1.json","id":"E1S1_1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "E1S1, E1S2") {
		t.Fatalf("删除后顺序不符: %s", out)
	}

	split, err := newSplitSegmentTool("proj", store)
	if err != nil {
		t.Fatal(err)
	}
	out, err = split.InvokableRun(context.Background(),
		`{"script":"episode_1.json","id":"E1S2","parts":[{"image_prompt":{"scene":"甲"}},{"image_prompt":{"scene":"乙"}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "拆为 2 份") || !strings.Contains(out, "E1S1, E1S2, E1S2_1") {
		t.Fatalf("拆分结果不符: %s", out)
	}
	// 份数不足应拒绝。
	out, _ = split.InvokableRun(context.Background(),
		`{"script":"episode_1.json","id":"E1S2","parts":[{"image_prompt":{"scene":"甲"}}]}`)
	if !strings.Contains(out, "至少需要 2 个部分") {
		t.Fatalf("份数不足应拒绝: %s", out)
	}
}

func TestPatchEpisodeMetaWhitelistAndTitle(t *testing.T) {
	store := &fakeScriptStore{script: scriptFixture()}
	tl, err := newPatchEpisodeMetaTool("proj", store)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json","field":"title","value":"  新标题  "}`)
	if err != nil {
		t.Fatal(err)
	}
	if store.script["title"] != "新标题" {
		t.Fatalf("title 应裁剪空白后写入: %v", store.script["title"])
	}
	if !strings.Contains(out, "已更新分集title为「新标题」") {
		t.Fatalf("回包文案不符: %s", out)
	}

	for _, args := range []string{
		`{"script":"episode_1.json","field":"episode","value":"2"}`,
		`{"script":"episode_1.json","field":"title","value":"   "}`,
	} {
		out, _ := tl.InvokableRun(context.Background(), args)
		if !strings.Contains(out, `"is_error":true`) {
			t.Errorf("args %s 应被拒: %s", args, out)
		}
	}
}

// fakeRenamer 记录改名请求并回放预置报告。
type fakeRenamer struct {
	calls  int
	kind   domain.AssetKind
	report service.RenameReport
	err    error
}

func (f *fakeRenamer) Rename(_ string, kind domain.AssetKind, _, _ string, _ bool) (service.RenameReport, error) {
	f.calls++
	f.kind = kind
	if f.err != nil {
		return service.RenameReport{}, f.err
	}
	return f.report, nil
}

func TestRenameAssetReportsCascadeAndRejectsUnknownTable(t *testing.T) {
	renamer := &fakeRenamer{report: service.RenameReport{
		Table: "characters", OldName: "阿明", NewName: "小明", Episodes: 3, References: 7, Files: 2,
	}}
	tl, err := newRenameAssetTool("proj", renamer)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(),
		`{"table":"characters","old_name":"阿明","new_name":"小明"}`)
	if err != nil {
		t.Fatal(err)
	}
	if renamer.calls != 1 || renamer.kind != domain.AssetCharacter {
		t.Fatalf("改名请求有误: %+v", renamer)
	}
	if !strings.Contains(out, "更新 3 集共 7 处引用，迁移 2 个关联文件") {
		t.Fatalf("报告文案不符: %s", out)
	}

	out, _ = tl.InvokableRun(context.Background(), `{"table":"weapon","old_name":"a","new_name":"b"}`)
	if !strings.Contains(out, "table 必须是") || !strings.Contains(out, `"is_error":true`) {
		t.Fatalf("未知表应被拒: %s", out)
	}
	if renamer.calls != 1 {
		t.Fatalf("非法请求不应触达服务: %d", renamer.calls)
	}

	// 改名失败以信封回灌。
	failing := &fakeRenamer{err: &service.RenameFileCollisionError{Path: "characters/小明.png"}}
	tl, _ = newRenameAssetTool("proj", failing)
	out, _ = tl.InvokableRun(context.Background(), `{"table":"characters","old_name":"阿明","new_name":"小明"}`)
	if !strings.Contains(out, "rename target occupied") {
		t.Fatalf("改名失败应回灌: %s", out)
	}
}
