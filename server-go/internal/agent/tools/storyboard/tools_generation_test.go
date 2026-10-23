package storyboard

import (
	"context"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain/project"
)

// 分镜图 / 旁白配音工具的端到端用例；共享替身见 fakes_test.go。

func TestGenerateStoryboardsEnqueuesOnlyMissingImages(t *testing.T) {
	q, projects := newGenerationFixture(t)
	tl, err := NewGenerateStoryboardsTool("proj", q, projects)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 1 || q.enqueued[0].ResourceID != "E1S2" {
		t.Fatalf("只应为缺图项入队: %+v", q.enqueued)
	}
	enqueued := q.enqueued[0]
	if enqueued.TaskType != "storyboard" || enqueued.MediaType != "image" {
		t.Fatalf("任务身份有误: %+v", enqueued)
	}
	if enqueued.ScriptFile == nil || *enqueued.ScriptFile != "episode_1.json" {
		t.Fatalf("剧本文件名应透传: %+v", enqueued.ScriptFile)
	}
	prompt, _ := enqueued.Payload["prompt"].(string)
	// 结构化 prompt：style 写进 YAML 的 Style 字段（带前缀清理），不再重复加 Style: 前缀，
	// 但 style_description 仍作为 Visual style 前缀注入。
	if strings.Contains(prompt, "Style: 画风：") {
		t.Fatalf("存量「画风：」前缀未被清理: %s", prompt)
	}
	if !strings.HasPrefix(prompt, "Visual style: 柔和光线\n\n") {
		t.Fatalf("风格描述前缀不符: %s", prompt)
	}
	if !strings.Contains(prompt, "Style: 写实") || !strings.Contains(prompt, "Scene: 桥上的两人") {
		t.Fatalf("YAML 正文不完整: %s", prompt)
	}
	if !strings.Contains(out, "1 succeeded, 0 failed") {
		t.Fatalf("汇总文案不符: %s", out)
	}
}

func TestGenerateStoryboardsRejectsRouteMismatchAndUnknownIDs(t *testing.T) {
	q, projects := newGenerationFixture(t)
	// 项目路线为参考生视频、剧本却是分镜族骨架 → 跨族失配。
	projects.pf.GenerationMode = "reference_video"
	tl, err := NewGenerateStoryboardsTool("proj", q, projects)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "不属同一族") || !strings.Contains(out, `"is_error":true`) {
		t.Fatalf("跨族剧本应被拒: %s", out)
	}
	if len(q.enqueued) != 0 {
		t.Fatal("被拒时不得入队")
	}

	q2, projects2 := newGenerationFixture(t)
	tl2, _ := NewGenerateStoryboardsTool("proj", q2, projects2)
	out, _ = tl2.InvokableRun(context.Background(), `{"script":"episode_1.json","segment_ids":["E9S9"]}`)
	if !strings.Contains(out, "没有找到匹配的片段") {
		t.Fatalf("显式点名不命中应报错: %s", out)
	}
	if len(q2.enqueued) != 0 {
		t.Fatal("不命中时不得入队")
	}
}

func TestGenerateStoryboardsBlocksOnPreflightFailure(t *testing.T) {
	q, projects := newGenerationFixture(t)
	// 角色条目的设计图指向不存在的文件 → preflight 应阻断整批。
	entry := project.NewAssetEntry()
	entry.SetString("description", "少年")
	entry.SetString("character_sheet", "characters/缺图.png")
	projects.pf.Characters = project.AssetBucket{"阿明": entry}

	tl, err := NewGenerateStoryboardsTool("proj", q, projects)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"is_error":true`) {
		t.Fatalf("引用无效应阻断: %s", out)
	}
	if len(q.enqueued) != 0 {
		t.Fatal("阻断时不得入队")
	}
}

func TestGenerateNarrationAudioSelectsByMissingAudio(t *testing.T) {
	q, projects := newGenerationFixture(t)
	script := projects.scripts["episode_1.json"]
	segments := script["segments"].([]any)
	segments[1].(map[string]any)["novel_text"] = "第二段原文"
	segments[0].(map[string]any)["generated_assets"].(map[string]any)["narration_audio"] = "audio/segment_E1S1.wav"

	tl, err := NewGenerateNarrationAudioTool("proj", q, projects)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 1 || q.enqueued[0].ResourceID != "E1S2" {
		t.Fatalf("只应为缺旁白音频的段入队: %+v", q.enqueued)
	}
	if q.enqueued[0].TaskType != "tts" || q.enqueued[0].MediaType != "audio" {
		t.Fatalf("任务身份有误: %+v", q.enqueued[0])
	}
	if got, _ := q.enqueued[0].Payload["prompt"].(string); got != "第二段原文" {
		t.Fatalf("合成文本应取 novel_text 原文: %q", got)
	}
	if !strings.Contains(out, "1 succeeded, 0 failed") {
		t.Fatalf("汇总文案不符: %s", out)
	}

	// 全部已生成时走「真无事可做」分支，不入队。
	q2, projects2 := newGenerationFixture(t)
	segments2 := projects2.scripts["episode_1.json"]["segments"].([]any)
	for _, item := range segments2 {
		item.(map[string]any)["generated_assets"] = map[string]any{"narration_audio": "audio/x.wav"}
	}
	tl2, _ := NewGenerateNarrationAudioTool("proj", q2, projects2)
	out, _ = tl2.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if !strings.Contains(out, "所有片段的旁白音频都已生成") || len(q2.enqueued) != 0 {
		t.Fatalf("全部已生成不应入队: %s", out)
	}
}

func TestGenerateNarrationAudioModeAndRouteGates(t *testing.T) {
	// drama 内容模式：scenes 没有 novel_text，直接拒绝。
	q, projects := newGenerationFixture(t)
	projects.pf.ContentMode = "drama"
	projects.scripts["episode_1.json"]["content_mode"] = "drama"
	tl, _ := NewGenerateNarrationAudioTool("proj", q, projects)
	out, _ := tl.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if !strings.Contains(out, "仅适用说书") || len(q.enqueued) != 0 {
		t.Fatalf("drama 应被拒: %s", out)
	}

	// 参考生视频路线：剧本没有 segments，不适用旁白配音。
	q2, projects2 := newGenerationFixture(t)
	projects2.pf.GenerationMode = "reference_video"
	projects2.scripts["episode_1.json"] = map[string]any{
		"title": "第1集", "content_mode": "narration",
		"video_units": []any{map[string]any{"unit_id": "E1U1", "duration_seconds": float64(8),
			"shots": []any{map[string]any{"text": "x"}}}},
	}
	tl2, _ := NewGenerateNarrationAudioTool("proj", q2, projects2)
	out, _ = tl2.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if !strings.Contains(out, "不适用旁白配音") || len(q2.enqueued) != 0 {
		t.Fatalf("参考路线应被拒: %s", out)
	}
}

func TestGenerateNarrationAudioBlankTextCountsAsFailureWhenExplicit(t *testing.T) {
	q, projects := newGenerationFixture(t)
	tl, err := NewGenerateNarrationAudioTool("proj", q, projects)
	if err != nil {
		t.Fatal(err)
	}
	// E1S2 的 novel_text 是空白；显式点名时按失败处理，且不给它入队。
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json","segment_ids":["E1S2"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 0 {
		t.Fatalf("空白文本不应入队: %+v", q.enqueued)
	}
	if !strings.Contains(out, "novel_text 为空，无法配音") || !strings.Contains(out, "1 failed") {
		t.Fatalf("显式点名空白段应计失败: %s", out)
	}
	if !strings.Contains(out, `"is_error":true`) {
		t.Fatalf("有失败时应带 is_error: %s", out)
	}
}

func TestGenerationToolsDenyWithoutQueue(t *testing.T) {
	_, projects := newGenerationFixture(t)

	tl, err := NewGenerateStoryboardsTool("proj", nil, projects)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "storyboard generation not configured") {
		t.Fatalf("未注入队列应 deny: %s", out)
	}

	audio, err := NewGenerateNarrationAudioTool("proj", nil, projects)
	if err != nil {
		t.Fatal(err)
	}
	out, err = audio.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "narration audio generation not configured") {
		t.Fatalf("未注入队列应 deny: %s", out)
	}
}
