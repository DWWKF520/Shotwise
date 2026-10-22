package imageedit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
)

// fakeImageCaps 是 edit_images 的能力解析替身：err 非空时模拟 i2i 槽解析失败。
type fakeImageCaps struct{ err error }

func (f fakeImageCaps) ResolveImageBackend(
	context.Context, *domain.ProjectFile, map[string]any, config.ImageCapability,
) (config.ProviderModel, error) {
	if f.err != nil {
		return config.ProviderModel{}, f.err
	}
	return config.ProviderModel{ProviderID: "gemini-aistudio", ModelID: "gemini-image"}, nil
}

// ResolveResolution 让 fakeImageCaps 同时满足 service.ImageCapabilityResolver（宫格门控用）。
func (f fakeImageCaps) ResolveResolution(context.Context, *domain.ProjectFile, string, string) (string, error) {
	return "2K", nil
}

func TestEditImagesEnqueuesAssetEdit(t *testing.T) {
	q, projects := newGenerationFixture(t)
	tl, err := NewEditImagesTool("proj", q, projects, fakeImageCaps{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(),
		`{"resource_type":"character","edits":[{"id":"阿明","instruction":"把发色改成红色"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 1 {
		t.Fatalf("应入队一条编辑任务: %+v", q.enqueued)
	}
	enqueued := q.enqueued[0]
	if enqueued.TaskType != "image_edit" || enqueued.MediaType != "image" {
		t.Fatalf("任务身份有误: %+v", enqueued)
	}
	if enqueued.ResourceID != "阿明" {
		t.Fatalf("resource_id 应为资产名: %+v", enqueued)
	}
	// image_edit 的四类资产与 storyboard 共用 task_type，resource_type 是去重键维度，必须带上。
	if enqueued.ResourceType == nil || *enqueued.ResourceType != "character" {
		t.Fatalf("resource_type 去重维度缺失: %+v", enqueued.ResourceType)
	}
	if enqueued.ScriptFile != nil {
		t.Fatalf("资产编辑不应带 script_file: %+v", enqueued.ScriptFile)
	}
	if got, _ := enqueued.Payload["resource_type"].(string); got != "character" {
		t.Fatalf("载荷应带 resource_type: %+v", enqueued.Payload)
	}
	if got, _ := enqueued.Payload["prompt"].(string); got != "把发色改成红色" {
		t.Fatalf("编辑指令应作为 prompt: %q", got)
	}
	if !strings.Contains(out, "edit_images summary: 1 succeeded, 0 failed") {
		t.Fatalf("汇总文案不符: %s", out)
	}
}

func TestEditImagesStoryboardRequiresScriptAndResolvesItem(t *testing.T) {
	q, projects := newGenerationFixture(t)
	tl, _ := NewEditImagesTool("proj", q, projects, fakeImageCaps{})

	// 缺 script_file：storyboard 分支直接拒绝。
	out, _ := tl.InvokableRun(context.Background(),
		`{"resource_type":"storyboard","edits":[{"id":"E1S1","instruction":"换光线"}]}`)
	if !strings.Contains(out, "script_file 必填") || len(q.enqueued) != 0 {
		t.Fatalf("storyboard 缺剧本应被拒: %s", out)
	}

	// E1S1 的 generated_assets 指向 storyboards/scene_E1S1.png，该文件在 fixture 里不存在
	// → 视为「没有可编辑的当前图」跳过；E1S2 无 storyboard_image，回退 canonical 路径，
	// 同样不存在 → 也跳过。两者都不入队，最终无任务可执行。
	out, _ = tl.InvokableRun(context.Background(),
		`{"resource_type":"storyboard","script_file":"episode_1.json","edits":[{"id":"E1S1","instruction":"换光线"}]}`)
	if !strings.Contains(out, "没有可编辑的当前图") || !strings.Contains(out, "没有可执行的编辑任务") {
		t.Fatalf("缺当前图应跳过并空转: %s", out)
	}
	if len(q.enqueued) != 0 {
		t.Fatalf("缺当前图不应入队: %+v", q.enqueued)
	}
}

func TestEditImagesRejectsUnknownTypeAndUnavailableI2I(t *testing.T) {
	q, projects := newGenerationFixture(t)
	tl, _ := NewEditImagesTool("proj", q, projects, fakeImageCaps{})
	out, _ := tl.InvokableRun(context.Background(),
		`{"resource_type":"clue","edits":[{"id":"x","instruction":"y"}]}`)
	if !strings.Contains(out, "resource_type 必须是以下之一") {
		t.Fatalf("未知类型应被拒: %s", out)
	}

	// i2i 槽解析失败 → 不创建任何任务。
	capped := &fakeImageCaps{err: errors.New("no usable provider")}
	tl2, _ := NewEditImagesTool("proj", q, projects, capped)
	out, _ = tl2.InvokableRun(context.Background(),
		`{"resource_type":"character","edits":[{"id":"阿明","instruction":"换发色"}]}`)
	if !strings.Contains(out, "不支持图生图") {
		t.Fatalf("i2i 不可用应被拒: %s", out)
	}
	if len(q.enqueued) != 0 {
		t.Fatalf("i2i 不可用不应入队: %+v", q.enqueued)
	}
}

func TestEditImagesSkipsMissingAssetAndDeduplicates(t *testing.T) {
	q, projects := newGenerationFixture(t)
	tl, _ := NewEditImagesTool("proj", q, projects, fakeImageCaps{})
	out, _ := tl.InvokableRun(context.Background(),
		`{"resource_type":"character","edits":[{"id":"不存在","instruction":"x"},{"id":"阿明","instruction":"y"},{"id":"阿明","instruction":"z"}]}`)
	if !strings.Contains(out, "'不存在' 不存在，跳过") {
		t.Fatalf("未登记资产应告警: %s", out)
	}
	if !strings.Contains(out, "重复出现，仅保留第一条编辑指令") {
		t.Fatalf("重复条目应告警: %s", out)
	}
	if len(q.enqueued) != 1 || q.enqueued[0].ResourceID != "阿明" {
		t.Fatalf("重复应只入队一条: %+v", q.enqueued)
	}
}

func TestEditImagesDenyWhenNotConfigured(t *testing.T) {
	_, projects := newGenerationFixture(t)
	tl, _ := NewEditImagesTool("proj", nil, projects, fakeImageCaps{})
	out, _ := tl.InvokableRun(context.Background(),
		`{"resource_type":"character","edits":[{"id":"阿明","instruction":"x"}]}`)
	if !strings.Contains(out, "image editing not configured") {
		t.Fatalf("未注入依赖应 deny: %s", out)
	}
}
