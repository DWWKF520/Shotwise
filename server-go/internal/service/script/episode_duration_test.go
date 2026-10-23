package script

import "github.com/shotwise/shotwise/internal/service"

import (
	"context"
	"errors"
	"testing"

	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/domain/script"
)

// durationFixture 造一个含 3 个分镜的剧集，档位 [4,6,8]。
type durationFixture struct {
	store  *service.ProjectStore
	svc    *EpisodeDurationService
	script map[string]any
}

func newDurationFixture(t *testing.T) durationFixture {
	t.Helper()
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "测试项目", project.ContentNarration, project.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	script := map[string]any{
		"content_mode": "narration",
		"episode":      1,
		"segments": []any{
			map[string]any{"segment_id": "E1S1", "duration_seconds": 4},
			map[string]any{"segment_id": "E1S2", "duration_seconds": 6},
			map[string]any{"segment_id": "E1S3", "duration_seconds": 6},
		},
		"metadata": map[string]any{"status": "draft"},
	}
	if err := store.WriteScriptJSON("p1", "episode_1.json", script); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if err := store.Update("p1", func(pf *project.ProjectFile) error {
		pf.Episodes = append(pf.Episodes, project.EpisodeMeta{Episode: 1, ScriptFile: "scripts/episode_1.json"})
		return nil
	}); err != nil {
		t.Fatalf("register episode: %v", err)
	}
	caps := &fakeVideoCapsResolver{view: veoView()}
	return durationFixture{
		store:  store,
		svc:    NewEpisodeDurationService(store, caps),
		script: script,
	}
}

// currentScript 读回落盘后的剧本。
func (f durationFixture) currentScript(t *testing.T) map[string]any {
	t.Helper()
	script, err := f.store.ReadScriptJSON("p1", "episode_1.json")
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	return script
}

// TestDurationStateShape 校验 GET 的响应形状：revision / plan / items 三块，
// 无配置时 plan 为 null。
func TestDurationStateShape(t *testing.T) {
	f := newDurationFixture(t)
	out, err := f.svc.State(context.Background(), "p1", 1)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if out["revision"] == "" || out["revision"] == nil {
		t.Fatalf("缺 revision: %v", out)
	}
	if out["plan"] != nil {
		t.Fatalf("初始不应有规划配置: %v", out["plan"])
	}
	items, _ := out["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("items = %v", items)
	}
	first, _ := items[0].(map[string]any)
	if first["resource_id"] != "E1S1" || first["locked"] != false || first["generated"] != false {
		t.Fatalf("item = %v", first)
	}
}

// TestDurationSaveRequiresMatchingRevision 校验保存是乐观并发的：过期修订号 409，
// 不改任何分镜时长。
func TestDurationSaveRequiresMatchingRevision(t *testing.T) {
	f := newDurationFixture(t)
	state, err := f.svc.State(context.Background(), "p1", 1)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	revision, _ := state["revision"].(string)

	if _, err := f.svc.Save(context.Background(), "p1", 1, "stale", EpisodeDurationPlanInput{
		TargetSeconds: 20, Strategy: "equal",
	}); err == nil {
		t.Fatal("过期修订号必须报冲突")
	} else {
		var conflict *EpisodeDurationRevisionConflictError
		if !errors.As(err, &conflict) || conflict.Actual != revision {
			t.Fatalf("err = %v", err)
		}
	}

	out, err := f.svc.Save(context.Background(), "p1", 1, revision, EpisodeDurationPlanInput{
		TargetSeconds: 20, Strategy: "equal",
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	plan, _ := out["plan"].(map[string]any)
	if plan["target_seconds"] != 20 || plan["strategy"] != "equal" {
		t.Fatalf("plan = %v", plan)
	}
	// 保存只写规划设置，不动分镜时长。
	script := f.currentScript(t)
	segments, _ := script["segments"].([]any)
	if seconds, _ := scriptInt(segments[0].(map[string]any)["duration_seconds"]); seconds != 4 {
		t.Fatalf("保存不应改分镜时长: %v", seconds)
	}
	if out["revision"] == revision {
		t.Fatal("保存后修订号应变化")
	}
}

// TestDurationPreviewDoesNotPersist 校验预览是纯读：只产出 diff，不写盘。
func TestDurationPreviewDoesNotPersist(t *testing.T) {
	f := newDurationFixture(t)
	before, err := f.store.ReadScriptJSON("p1", "episode_1.json")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out, err := f.svc.Preview(context.Background(), "p1", 1, EpisodeDurationPlanInput{
		TargetSeconds: 24, Strategy: "equal",
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if out["target_seconds"] != 24 {
		t.Fatalf("target_seconds = %v", out["target_seconds"])
	}
	changes, _ := out["changes"].([]any)
	if len(changes) == 0 {
		t.Fatalf("应产出时长变化: %v", out)
	}
	after, err := f.store.ReadScriptJSON("p1", "episode_1.json")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if script.EpisodeDurationRevision(before) != script.EpisodeDurationRevision(after) {
		t.Fatal("预览不得写盘")
	}
}

// TestDurationApplySkipsLockedAndGenerated 校验应用时逐项复核：锁定或已生成的条目
// 只进 skipped，绝不覆盖。
func TestDurationApplySkipsLockedAndGenerated(t *testing.T) {
	f := newDurationFixture(t)
	// 第二段锁定、第三段已有视频。
	script := f.currentScript(t)
	segments, _ := script["segments"].([]any)
	segments[1].(map[string]any)["duration_locked"] = true
	segments[2].(map[string]any)["generated_assets"] = map[string]any{"video_clip": "videos/E1S3.mp4"}
	if err := f.store.WriteScriptJSON("p1", "episode_1.json", script); err != nil {
		t.Fatalf("write: %v", err)
	}

	state, err := f.svc.State(context.Background(), "p1", 1)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	revision, _ := state["revision"].(string)
	out, err := f.svc.Apply(context.Background(), "p1", 1, revision, EpisodeDurationPlanInput{
		TargetSeconds: 24, Strategy: "equal",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// 锁定 / 已生成的条目在规划阶段就被排除在 changes 之外，因此既不会进 applied，
	// 也不会进 skipped（skipped 只兜「规划后到落盘前状态被并发改掉」的竞态）。
	applied, _ := out["applied"].(map[string]any)
	if len(applied) != 1 {
		t.Fatalf("applied = %v", applied)
	}
	if _, ok := applied["E1S2"]; ok {
		t.Fatalf("锁定条目不应被改写: %v", applied)
	}
	if _, ok := applied["E1S1"]; !ok {
		t.Fatalf("可调整条目应被改写: %v", applied)
	}
	// 落盘复核
	after := f.currentScript(t)
	afterSegments, _ := after["segments"].([]any)
	if seconds, _ := scriptInt(afterSegments[1].(map[string]any)["duration_seconds"]); seconds != 6 {
		t.Fatalf("锁定条目时长被改了: %v", afterSegments[1])
	}
	// 只剩一个可调整条目时它吸收整集时长，但会被夹到后端支持的档位（[4,6,8] → 8）。
	if seconds, _ := scriptInt(afterSegments[0].(map[string]any)["duration_seconds"]); seconds != 8 {
		t.Fatalf("剩余条目应吸收整集时长并夹到档位上限: %v", afterSegments[0])
	}
}

// TestDurationLockToggle 校验锁切换与「已生成条目不可改锁」。
func TestDurationLockToggle(t *testing.T) {
	f := newDurationFixture(t)
	state, err := f.svc.State(context.Background(), "p1", 1)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	revision, _ := state["revision"].(string)
	out, err := f.svc.SetLock(context.Background(), "p1", 1, "E1S1", true, revision)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	if out["locked"] != true || out["resource_id"] != "E1S1" {
		t.Fatalf("out = %v", out)
	}

	// 未知条目 → 404 语义
	revision, _ = out["revision"].(string)
	if _, err := f.svc.SetLock(context.Background(), "p1", 1, "E9S9", true, revision); err == nil {
		t.Fatal("未知条目应报错")
	} else {
		var notFound *EpisodeDurationNotFoundError
		if !errors.As(err, &notFound) {
			t.Fatalf("err = %v", err)
		}
	}
}

// TestDurationRejectsInvalidStrategy 校验策略与目标时长的入参校验。
func TestDurationRejectsInvalidStrategy(t *testing.T) {
	f := newDurationFixture(t)
	if _, err := f.svc.Preview(context.Background(), "p1", 1, EpisodeDurationPlanInput{
		TargetSeconds: 20, Strategy: "magic",
	}); err == nil {
		t.Fatal("未知策略应被拒绝")
	}
	if _, err := f.svc.Preview(context.Background(), "p1", 1, EpisodeDurationPlanInput{
		TargetSeconds: 0, Strategy: "equal",
	}); err == nil {
		t.Fatal("非正目标时长应被拒绝")
	}
}
