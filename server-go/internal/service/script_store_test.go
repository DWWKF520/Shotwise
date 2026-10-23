package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain/project"
	domainscript "github.com/shotwise/shotwise/internal/domain/script"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

const serviceScriptFixture = "episode_1.json"

func validScriptPayload(title string, episode int) map[string]any {
	return map[string]any{
		"title": title, "content_mode": "narration", "episode": float64(episode),
		"segments": []any{map[string]any{
			"segment_id": "E1S1", "duration_seconds": float64(4), "novel_text": "原文",
			"characters_in_segment": []any{"阿明"},
			"image_prompt": map[string]any{"scene": "画面",
				"composition": map[string]any{"shot_type": "Medium Shot", "lighting": "暖", "ambiance": "静"}},
			"video_prompt": map[string]any{"action": "动作", "camera_motion": "Static", "ambiance_audio": "风声"},
		}},
	}
}

// newScriptFixture 建项目骨架，并把剧本以原始 JSON 落盘（不经守卫，便于构造「改前已非法」样本）。
func newScriptFixture(t *testing.T, filename string, script map[string]any) (*ProjectStore, string) {
	t.Helper()
	store := NewProjectStore(t.TempDir())
	if _, err := store.Create("proj", "测试项目", project.ContentNarration, project.GenerationStoryboard, "写实"); err != nil {
		t.Fatal(err)
	}
	dir, err := store.Dir("proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if script != nil {
		if err := fs.WriteJSONAtomic(filepath.Join(dir, "scripts", filename), script); err != nil {
			t.Fatal(err)
		}
	}
	return store, dir
}

func readScriptFile(t *testing.T, dir, filename string) map[string]any {
	t.Helper()
	var script map[string]any
	if err := fs.ReadJSON(filepath.Join(dir, "scripts", filename), &script); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestUpdateScriptRecomputesMetadataAndSyncsEpisode(t *testing.T) {
	store, dir := newScriptFixture(t, serviceScriptFixture, validScriptPayload("旧标题", 1))
	// 注入一条更长的时长用于断言 metadata 重算；同时改标题以触发分集镜像。
	err := store.UpdateScript("proj", "scripts/"+serviceScriptFixture, func(script map[string]any) error {
		script["title"] = "新标题"
		segment := script["segments"].([]any)[0].(map[string]any)
		segment["duration_seconds"] = float64(12)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	written := readScriptFile(t, dir, serviceScriptFixture)
	if written["title"] != "新标题" {
		t.Fatalf("标题未写入: %v", written["title"])
	}
	metadata, _ := written["metadata"].(map[string]any)
	if metadata == nil {
		t.Fatal("metadata 未生成")
	}
	if metadata["status"] != "draft" {
		t.Fatalf("status = %v", metadata["status"])
	}
	if _, ok := metadata["created_at"].(string); !ok {
		t.Fatalf("created_at 缺失: %v", metadata["created_at"])
	}
	if metadata["total_scenes"] != float64(1) {
		t.Fatalf("total_scenes = %v", metadata["total_scenes"])
	}
	if metadata["estimated_duration_seconds"] != float64(12) {
		t.Fatalf("estimated_duration_seconds = %v", metadata["estimated_duration_seconds"])
	}

	// 分集元数据镜像到 project.json（WebUI 分集列表的数据来源）。
	pf, err := store.Get("proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(pf.Episodes) != 1 {
		t.Fatalf("episodes = %+v", pf.Episodes)
	}
	if pf.Episodes[0].Title != "新标题" || pf.Episodes[0].ScriptFile != "scripts/"+serviceScriptFixture {
		t.Fatalf("分集元数据未同步: %+v", pf.Episodes[0])
	}
}

func TestUpdateScriptGuardRejectsNewStructuralError(t *testing.T) {
	store, dir := newScriptFixture(t, serviceScriptFixture, validScriptPayload("标题", 1))
	err := store.UpdateScript("proj", serviceScriptFixture, func(script map[string]any) error {
		delete(script, "title")
		return nil
	})
	if err == nil {
		t.Fatal("引入新的结构错误应被守卫拒绝")
	}
	var validationErr *ScriptStructureValidationError
	if !asStructureError(err, &validationErr) {
		t.Fatalf("错误类型不符: %T %v", err, err)
	}
	if !strings.Contains(strings.Join(validationErr.Errors, ";"), "title") {
		t.Fatalf("错误应点名 title: %v", validationErr.Errors)
	}

	// 拒绝必须整批零落盘：磁盘上的剧本保持未改写。
	written := readScriptFile(t, dir, serviceScriptFixture)
	if written["title"] != "标题" {
		t.Fatalf("非法写入落了盘: %+v", written)
	}
	if _, ok := written["metadata"]; ok {
		t.Fatal("被拒的写入不应重算 metadata")
	}
}

func TestUpdateScriptGuardAllowsAlreadyInvalidScript(t *testing.T) {
	broken := validScriptPayload("标题", 1)
	delete(broken, "title")
	store, dir := newScriptFixture(t, serviceScriptFixture, broken)

	// 改前已非法 → 放行（不为历史遗留背锅），且其余编辑照常生效。
	err := store.UpdateScript("proj", serviceScriptFixture, func(script map[string]any) error {
		segment := script["segments"].([]any)[0].(map[string]any)
		segment["novel_text"] = "改过的原文"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	written := readScriptFile(t, dir, serviceScriptFixture)
	if written["segments"].([]any)[0].(map[string]any)["novel_text"] != "改过的原文" {
		t.Fatalf("既有非法剧本的合法编辑应放行: %+v", written)
	}
}

func TestUpdateScriptRejectsFilenameEpisodeMismatch(t *testing.T) {
	store, dir := newScriptFixture(t, "episode_2.json", validScriptPayload("标题", 1))
	err := store.UpdateScript("proj", "episode_2.json", func(script map[string]any) error {
		script["title"] = "改过"
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("集号不一致应被拒: %v", err)
	}
	written := readScriptFile(t, dir, "episode_2.json")
	if written["title"] != "标题" {
		t.Fatalf("不一致的写入不应落盘: %+v", written)
	}
}

func TestUpdateScriptRejectsCorruptedSkeletonKey(t *testing.T) {
	// 改前合法、改后把骨架数组写成 null → 守卫以「不更坏」语义拒绝。
	store, dir := newScriptFixture(t, serviceScriptFixture, validScriptPayload("标题", 1))
	err := store.UpdateScript("proj", serviceScriptFixture, func(script map[string]any) error {
		script["segments"] = nil
		return nil
	})
	if err == nil {
		t.Fatal("会话骨架数组类型损坏应被拒")
	}
	if !strings.Contains(err.Error(), "segments") {
		t.Fatalf("错误应点名 segments: %v", err)
	}
	written := readScriptFile(t, dir, serviceScriptFixture)
	if written["segments"] == nil {
		t.Fatalf("损坏写入不应落盘: %+v", written)
	}
}

// 改前就已损坏的剧本仍可编辑（不为历史遗留背锅），但统计不重算、旧值保留。
func TestUpdateScriptKeepsOldMetadataOnCorruptedData(t *testing.T) {
	broken := validScriptPayload("标题", 1)
	broken["segments"] = nil
	broken["metadata"] = map[string]any{"total_scenes": float64(99), "status": "draft"}
	store, dir := newScriptFixture(t, serviceScriptFixture, broken)
	if err := store.UpdateScript("proj", serviceScriptFixture, func(script map[string]any) error {
		script["title"] = "改过"
		return nil
	}); err != nil {
		t.Fatalf("改前已损坏应放行: %v", err)
	}
	written := readScriptFile(t, dir, serviceScriptFixture)
	metadata, _ := written["metadata"].(map[string]any)
	if metadata["total_scenes"] != float64(99) {
		t.Fatalf("损坏数据的旧统计应保留: %+v", metadata)
	}
	if written["title"] != "改过" {
		t.Fatalf("合法字段编辑应生效: %+v", written)
	}
}

func TestUpdateScriptMutationErrorSkipsWrite(t *testing.T) {
	store, dir := newScriptFixture(t, serviceScriptFixture, validScriptPayload("标题", 1))
	err := store.UpdateScript("proj", serviceScriptFixture, func(script map[string]any) error {
		script["title"] = "半途而废"
		return &domainscript.ScriptEditError{Message: "id 未命中", Key: domainscript.ScriptEditErrorKey}
	})
	if err == nil {
		t.Fatal("mutate 报错应上抛")
	}
	written := readScriptFile(t, dir, serviceScriptFixture)
	if written["title"] != "标题" {
		t.Fatalf("mutate 报错应整批零落盘: %+v", written)
	}
}

func TestNormalizeScriptFilenameAliases(t *testing.T) {
	cases := map[string]string{
		"episode_1.json":           "episode_1.json",
		"scripts/episode_1.json":   "episode_1.json",
		"./episode_1.json":         "episode_1.json",
		"./scripts/episode_1.json": "episode_1.json",
		"scripts/./episode_1.json": "episode_1.json",
	}
	for in, want := range cases {
		if got := NormalizeScriptFilename(in); got != want {
			t.Errorf("NormalizeScriptFilename(%q) = %q，期望 %q", in, got, want)
		}
	}
	// 空化输入被拒。
	store, _ := newScriptFixture(t, serviceScriptFixture, validScriptPayload("标题", 1))
	if err := store.UpdateScript("proj", "", func(map[string]any) error { return nil }); err == nil {
		t.Fatal("空文件名应被拒")
	}
	if err := store.UpdateScript("proj", "sub/episode_1.json", func(map[string]any) error { return nil }); err == nil {
		t.Fatal("路径分隔符应被拒")
	}
}

func asStructureError(err error, target **ScriptStructureValidationError) bool {
	e, ok := err.(*ScriptStructureValidationError)
	if ok {
		*target = e
	}
	return ok
}
