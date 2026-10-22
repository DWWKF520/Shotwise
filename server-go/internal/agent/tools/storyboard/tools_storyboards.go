package storyboard

import "github.com/shotwise/shotwise/internal/agent/tools"

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/generation"
)


// --- generate_storyboards ---

type generateStoryboardsIn struct {
	Script     string   `json:"script" jsonschema:"description=剧本文件名（如 episode_1.json），必须是纯文件名"`
	SegmentIDs []string `json:"segment_ids,omitempty" jsonschema:"description=片段或场景 ID 列表；不传则扫描所有缺分镜图的项"`
}

// NewGenerateStoryboardsTool 构造 generate_storyboards。
func NewGenerateStoryboardsTool(project string, q tools.AssetQueue, projects tools.GenerationProjects) (tool.InvokableTool, error) {
	return toolutils.InferTool("generate_storyboards",
		"为 narration/drama 模式剧本生成分镜图。script 为剧本文件名（如 episode_1.json）；"+
			"segment_ids 指定要重生的片段/场景 ID 列表（不传则生成所有缺图项）。",
		func(ctx context.Context, in generateStoryboardsIn) (string, error) {
			if err := tools.ValidateScriptFilename(in.Script); err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			if q == nil || projects == nil {
				return tools.DenyEnvelope("storyboard generation not configured"), nil
			}
			text, failed, err := runGenerateStoryboards(ctx, project, in, q, projects)
			if err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			if failed {
				return tools.DenyEnvelope(text), nil
			}
			return tools.OKEnvelope(text), nil
		})
}

func runGenerateStoryboards(
	ctx context.Context, project string, in generateStoryboardsIn, q tools.AssetQueue, projects tools.GenerationProjects,
) (string, bool, error) {
	pf, err := projects.Get(project)
	if err != nil {
		return "", false, err
	}
	projectDir, err := projects.Dir(project)
	if err != nil {
		return "", false, err
	}
	script, err := projects.ReadScriptJSON(project, in.Script)
	if err != nil {
		return "", false, fmt.Errorf("剧本 %s 不可读取: %w", in.Script, err)
	}
	// 失配剧本在此被拒：按分镜路线该读的数组不在剧本里，继续走下去会落进
	// 「所有片段的分镜图都已生成」的假成功，把成因埋掉。
	if _, err := domain.EnsureRouteSkeleton(script, pf.ContentMode, pf.GenerationMode); err != nil {
		return "", false, err
	}

	items, idField, fields := domain.StoryboardItems(script)
	if len(items) == 0 {
		return "", false, fmt.Errorf("剧本没有可生成分镜图的片段")
	}

	explicit := in.SegmentIDs != nil
	var targetIDs []string
	if explicit {
		selected, unmatched := tools.SelectScriptItemsByID(items, idField, in.SegmentIDs)
		if len(selected) == 0 {
			return fmt.Sprintf("❌ 没有找到匹配的片段/场景：segment_ids=%v", in.SegmentIDs), true, nil
		}
		if len(unmatched) > 0 {
			return fmt.Sprintf("❌ 以下片段不存在: %s", strings.Join(unmatched, ", ")), true, nil
		}
		for _, item := range selected {
			targetIDs = append(targetIDs, fmt.Sprint(item[idField]))
		}
	} else {
		targetIDs = domain.StoryboardItemsMissingImage(items, idField)
		if len(targetIDs) == 0 {
			return "✨ 所有片段的分镜图都已生成", false, nil
		}
	}

	// 风格取自 project.json 原始对象（style / style_description 不在域结构体上）。
	rawProject := map[string]any{}
	if loaded, err := tools.LoadRawProject(projectDir); err == nil {
		rawProject = loaded
	}
	style, _ := rawProject["style"].(string)
	styleDescription, _ := rawProject["style_description"].(string)

	itemsByID := map[string]map[string]any{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemsByID[fmt.Sprint(entry[idField])] = entry
	}

	plans := domain.BuildStoryboardDependencyPlan(items, idField, targetIDs, in.Script)
	var tasks []tools.GenerationTask
	for _, plan := range plans {
		item, ok := itemsByID[plan.ResourceID]
		if !ok {
			continue
		}
		if _, present := item["image_prompt"]; !present {
			return "", false, fmt.Errorf("片段/场景 %v 缺少 image_prompt 字段", plan.ResourceID)
		}
		prompt, err := generation.BuildImagePromptText(item["image_prompt"], style, styleDescription)
		if err != nil {
			return "", false, fmt.Errorf("片段/场景 %v：%w", plan.ResourceID, err)
		}
		tasks = append(tasks, tools.GenerationTask{
			ResourceID:           plan.ResourceID,
			Prompt:               prompt,
			ScriptFile:           in.Script,
			DependencyResourceID: plan.DependencyResourceID,
			DependencyGroup:      plan.DependencyGroup,
			DependencyIndex:      plan.DependencyIndex,
		})
	}

	// 任何一个待入队分镜引用无效都阻断整批：不能让前面的图片已入队后才发现后续资产缺失。
	preflight := service.MergePreflightResults(preflightFor(tasks, itemsByID, pf, projectDir, fields)...)
	if message := service.FormatPreflightError(preflight); message != "" {
		return "", false, fmt.Errorf("%s", message)
	}

	taskIDs, err := tools.EnqueueGenerationTasks(ctx, q, project, "storyboard", tasks)
	if err != nil {
		return "", false, err
	}
	successes, failures, err := tools.AwaitGenerationTasks(ctx, q, tasks, taskIDs)
	if err != nil {
		return "", false, err
	}
	return tools.GenerationSummary("generate_storyboards", successes, failures, "storyboards"), len(failures) > 0, nil
}

// preflightFor 逐条校验待入队分镜的资产引用。
func preflightFor(
	tasks []tools.GenerationTask, itemsByID map[string]map[string]any, pf *domain.ProjectFile,
	projectDir string, fields domain.StoryboardItemFields,
) []service.PreflightResult {
	results := make([]service.PreflightResult, 0, len(tasks))
	for _, task := range tasks {
		item, ok := itemsByID[task.ResourceID]
		if !ok {
			continue
		}
		results = append(results, service.ValidateItemAssetReferences(pf, projectDir, item, fields))
	}
	return results
}

// scriptContentMode 取剧本内容模式，缺失时回落到项目配置（与读侧既定口径一致）。
func scriptContentMode(script map[string]any, pf *domain.ProjectFile) string {
	if mode, ok := script["content_mode"].(string); ok && mode != "" {
		return mode
	}
	return pf.ContentMode
}

type generateNarrationAudioIn struct {
	Script     string   `json:"script" jsonschema:"description=剧本文件名（如 episode_1.json），必须是纯文件名"`
	SegmentIDs []string `json:"segment_ids,omitempty" jsonschema:"description=片段 ID 列表；不传则扫描所有缺旁白音频的段"`
}

// NewGenerateNarrationAudioTool 构造 generate_narration_audio。
func NewGenerateNarrationAudioTool(project string, q tools.AssetQueue, projects tools.GenerationProjects) (tool.InvokableTool, error) {
	return toolutils.InferTool("generate_narration_audio",
		"为说书（narration）模式剧本逐段生成旁白配音（TTS），入队并等待完成。"+
			"script 为剧本文件名（如 episode_1.json）；segment_ids 指定片段 ID 列表（不传则扫描所有缺旁白音频的段）。"+
			"音频以各段 novel_text 原文合成，只依赖剧本，不依赖分镜图/视频。",
		func(ctx context.Context, in generateNarrationAudioIn) (string, error) {
			if err := tools.ValidateScriptFilename(in.Script); err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			if q == nil || projects == nil {
				return tools.DenyEnvelope("narration audio generation not configured"), nil
			}
			text, failed, err := runGenerateNarrationAudio(ctx, project, in, q, projects)
			if err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			if failed {
				return tools.DenyEnvelope(text), nil
			}
			return tools.OKEnvelope(text), nil
		})
}

func runGenerateNarrationAudio(
	ctx context.Context, project string, in generateNarrationAudioIn, q tools.AssetQueue, projects tools.GenerationProjects,
) (string, bool, error) {
	pf, err := projects.Get(project)
	if err != nil {
		return "", false, err
	}
	script, err := projects.ReadScriptJSON(project, in.Script)
	if err != nil {
		return "", false, fmt.Errorf("剧本 %s 不可读取: %w", in.Script, err)
	}
	// 内容模式与路线都以项目为兜底真相源：只读剧本自身的 content_mode 会让缺该字段的剧本
	// 在 drama 项目下绕过模式拒绝，落进「0 succeeded, 0 failed」的空转（drama 的 scenes 没有 novel_text）。
	contentMode := scriptContentMode(script, pf)
	if contentMode == string(domain.ContentDrama) {
		return "", false, fmt.Errorf("旁白配音仅适用说书（narration）模式剧本；drama 模式的 scenes 没有 novel_text")
	}
	if _, err := domain.EnsureRouteSkeleton(script, contentMode, pf.GenerationMode); err != nil {
		return "", false, err
	}
	if pf.IsReferenceVideo() {
		return "", false, fmt.Errorf("参考生视频（reference_video）路线的剧本没有 segments，不适用旁白配音")
	}

	items, idField, _ := domain.StoryboardItems(script)
	if len(items) == 0 {
		return "", false, fmt.Errorf("剧本没有可配音的片段")
	}

	explicit := in.SegmentIDs != nil
	var selected []map[string]any
	var unmatched []string
	if explicit {
		selected, unmatched = tools.SelectScriptItemsByID(items, idField, in.SegmentIDs)
	} else {
		for _, id := range tools.MissingGeneratedAsset(items, idField, "narration_audio") {
			picked, _ := tools.SelectScriptItemsByID(items, idField, []string{id})
			selected = append(selected, picked...)
		}
	}
	if len(selected) == 0 {
		if explicit {
			return fmt.Sprintf("❌ 没有找到匹配的片段：segment_ids=%v", in.SegmentIDs), true, nil
		}
		return "✨ 所有片段的旁白音频都已生成", false, nil
	}

	// 缺 id 的片段（损坏/手改剧本）不能让整批中断，跳过并告警。
	var tasks []tools.GenerationTask
	var blank []string
	missingIDCount := 0
	for _, item := range selected {
		id := strings.TrimSpace(fmt.Sprint(item[idField]))
		if id == "" || id == "<nil>" {
			missingIDCount++
			continue
		}
		text, _ := item["novel_text"].(string)
		if strings.TrimSpace(text) == "" {
			blank = append(blank, id)
			continue
		}
		tasks = append(tasks, tools.GenerationTask{ResourceID: id, Prompt: text, ScriptFile: in.Script})
	}

	var successes, failures []tools.GenerationOutcome
	if len(tasks) > 0 {
		taskIDs, err := tools.EnqueueGenerationTasks(ctx, q, project, "tts", tasks)
		if err != nil {
			return "", false, err
		}
		if successes, failures, err = tools.AwaitGenerationTasks(ctx, q, tasks, taskIDs); err != nil {
			return "", false, err
		}
	}

	lines := []string{fmt.Sprintf("generate_narration_audio summary: %d succeeded, %d failed",
		len(successes), len(failures)+len(unmatched)+blankFailureCount(blank, explicit))}
	for _, s := range successes {
		lines = append(lines, fmt.Sprintf("  ✓ %s → %s%s", s.ResourceID, s.FilePath("audio"), s.VersionSuffix()))
	}
	for _, f := range failures {
		lines = append(lines, fmt.Sprintf("  ✗ %s: %s", f.ResourceID, f.Message))
	}
	// 空白段不能静默丢弃：扫描模式下是告警（该段永远无法配音，不阻塞其余段）；
	// 显式点名时按失败处理——调用方明确要这一段，给「成功 0 失败 0」会误导。
	mark := "⚠️"
	if explicit {
		mark = "✗"
	}
	for _, id := range blank {
		lines = append(lines, fmt.Sprintf("  %s %s: novel_text 为空，无法配音", mark, id))
	}
	for _, id := range unmatched {
		lines = append(lines, fmt.Sprintf("  ✗ %s: 片段不存在", id))
	}
	if missingIDCount > 0 {
		lines = append(lines, fmt.Sprintf("  ⚠️ 跳过 %d 个缺少 %s 的片段", missingIDCount, idField))
	}
	failedCount := len(failures) + len(unmatched) + blankFailureCount(blank, explicit)
	return strings.Join(lines, "\n"), failedCount > 0, nil
}

func blankFailureCount(blank []string, explicit bool) int {
	if explicit {
		return len(blank)
	}
	return 0
}
