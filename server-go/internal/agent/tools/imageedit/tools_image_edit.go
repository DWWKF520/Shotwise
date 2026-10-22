package imageedit

import "github.com/shotwise/shotwise/internal/agent/tools"

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/service"
)

// edit_images（对应 Python sdk_tools/enqueue_image_edits.py）：对已生成的设计图/分镜图做
// 指令式局部编辑，批量下发后逐个等待终态。
//
// 编辑与重新生成的区别由 prompt 语义承担（不改 image_prompt、只按指令微调当前图）；
// 资源解析与「当前图是否存在」的校验复用执行层 service.ResolveCurrentImageRel，
// 工具层与 worker 层不各判一次。

// ImageEditCapabilityReader 判定项目图片 i2i 槽是否解析出可用供应商
// （*config.Resolver 满足；与 get_video_capabilities 的能力读取注入同构）。
//
// 批量编辑只要一次「是否可用」的项目级判断，不像 HTTP 端点那样需要拿到 provider_id
// 传给入队，故直接读解析结果、不落 provider 字段。
type ImageEditCapabilityReader interface {
	ResolveImageBackend(
		ctx context.Context, project *domain.ProjectFile, payload map[string]any, capability config.ImageCapability,
	) (config.ProviderModel, error)
}


// editImageTargets 是编辑目标的展示用中文标签（仅工具输出；不参与 i18n）。
var editImageTargets = map[string]string{
	"character":  "角色",
	"scene":      "场景",
	"prop":       "道具",
	"product":    "产品",
	"storyboard": "分镜图",
}

type editImagesIn struct {
	ResourceType string `json:"resource_type" jsonschema:"description=编辑目标类型：character/scene/prop/product/storyboard"`
	Edits        []struct {
		ID          string `json:"id" jsonschema:"description=资产名称，或 storyboard 的 segment_id/scene_id"`
		Instruction string `json:"instruction" jsonschema:"description=编辑指令（自然语言，描述要修改的部分）"`
	} `json:"edits" jsonschema:"description=批量编辑列表，每项一个 id + instruction"`
	ScriptFile string `json:"script_file,omitempty" jsonschema:"description=剧本文件名（如 episode_1.json）；resource_type=storyboard 时必填，必须是纯文件名"`
}

// NewEditImagesTool 构造 edit_images。
func NewEditImagesTool(
	project string, q tools.AssetQueue, projects tools.GenerationProjects, caps ImageEditCapabilityReader,
) (tool.InvokableTool, error) {
	return toolutils.InferTool("edit_images",
		"对已生成的设计图/分镜图做指令式局部编辑：保持原图大体不变，仅按指令修改不满意的部分"+
			"（如换发色、去掉背景杂物、调整光线氛围），支持同类型批量下发。"+
			"与「重新生成」的区别：编辑=保底图微调、不改变原 image_prompt，重生成会作废本次编辑效果"+
			"（仍按原 prompt 重画）；重新生成=按原 prompt 整图重画，会推翻已满意的部分。"+
			"用户只想改局部时用编辑；用户想推翻构图/内容重来、或原 image_prompt 本身要改时用重新生成。"+
			"resource_type 支持 character/scene/prop/product/storyboard 五类，storyboard 必须带 script_file。"+
			"编辑必然走图生图（i2i）；当前项目图片供应商不支持 i2i 时直接返回错误，不创建任何任务。",
		func(ctx context.Context, in editImagesIn) (string, error) {
			if !service.IsEditableResourceType(in.ResourceType) {
				return tools.DenyEnvelope(fmt.Sprintf("resource_type 必须是以下之一: %s",
					strings.Join(service.EditableResourceTypes, ", "))), nil
			}
			if len(in.Edits) == 0 {
				return tools.DenyEnvelope("edits 不能为空"), nil
			}
			if q == nil || projects == nil || caps == nil {
				return tools.DenyEnvelope("image editing not configured"), nil
			}
			text, failed, err := runEditImages(ctx, project, in, q, projects, caps)
			if err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			if failed {
				return tools.DenyEnvelope(text), nil
			}
			return tools.OKEnvelope(text), nil
		})
}

func runEditImages(
	ctx context.Context, project string, in editImagesIn, q tools.AssetQueue,
	projects tools.GenerationProjects, caps ImageEditCapabilityReader,
) (string, bool, error) {
	isStoryboard := in.ResourceType == service.StoryboardResourceType
	scriptFile := ""
	var script map[string]any
	if isStoryboard {
		if strings.TrimSpace(in.ScriptFile) == "" {
			return "resource_type=storyboard 时 script_file 必填", true, nil
		}
		scriptFile = strings.TrimSpace(in.ScriptFile)
		if err := tools.ValidateScriptFilename(scriptFile); err != nil {
			return "", false, err
		}
		loaded, err := projects.ReadScriptJSON(project, scriptFile)
		if err != nil {
			return "", false, fmt.Errorf("剧本 %s 不可读取: %w", scriptFile, err)
		}
		script = loaded
	}

	pf, err := projects.Get(project)
	if err != nil {
		return "", false, err
	}
	projectDir, err := projects.Dir(project)
	if err != nil {
		return "", false, err
	}

	// 与 HTTP 端点入队前的 fail-fast 同一判断点：i2i 槽解析不出可用供应商时直接拒绝，
	// 不创建任何编辑任务。
	if _, err := caps.ResolveImageBackend(ctx, pf, nil, config.ImageCapI2I); err != nil {
		return "❌ 当前项目图片供应商不支持图生图（i2i），无法执行编辑；请提示用户前往设置更换支持 i2i 的供应商", true, nil
	}

	label := editImageTargets[in.ResourceType]
	var warnings []string
	var tasks []tools.GenerationTask
	seen := map[string]bool{}
	for _, edit := range in.Edits {
		resourceID := strings.TrimSpace(edit.ID)
		instruction := strings.TrimSpace(edit.Instruction)
		if resourceID == "" {
			warnings = append(warnings, "⚠️  edits 中存在缺少 id 的条目，跳过")
			continue
		}
		if seen[resourceID] {
			warnings = append(warnings, fmt.Sprintf("⚠️  %s '%s' 在 edits 中重复出现，仅保留第一条编辑指令", label, resourceID))
			continue
		}
		seen[resourceID] = true
		if instruction == "" {
			warnings = append(warnings, fmt.Sprintf("⚠️  %s '%s' 缺少编辑指令，跳过", label, resourceID))
			continue
		}
		currentRel, found := service.ResolveCurrentImageRel(pf, in.ResourceType, resourceID, script)
		if !found {
			warnings = append(warnings, fmt.Sprintf("⚠️  %s '%s' 不存在，跳过", label, resourceID))
			continue
		}
		if currentRel == "" || !fs.SafeExists(projectDir, currentRel) {
			warnings = append(warnings, fmt.Sprintf("⚠️  %s '%s' 没有可编辑的当前图，跳过", label, resourceID))
			continue
		}
		task := tools.GenerationTask{
			ResourceID:        resourceID,
			Prompt:            instruction,
			ExtraPayload:      map[string]any{"resource_type": in.ResourceType},
			DedupResourceType: in.ResourceType,
		}
		if isStoryboard {
			task.ScriptFile = scriptFile
		}
		tasks = append(tasks, task)
	}
	if len(tasks) == 0 {
		return strings.Join(append(warnings, "没有可执行的编辑任务"), "\n"), true, nil
	}

	taskIDs, err := tools.EnqueueGenerationTasks(ctx, q, project, "image_edit", tasks)
	if err != nil {
		return "", false, err
	}
	successes, failures, err := tools.AwaitGenerationTasks(ctx, q, tasks, taskIDs)
	if err != nil {
		return "", false, err
	}

	lines := append([]string{}, warnings...)
	lines = append(lines, fmt.Sprintf("edit_images summary: %d succeeded, %d failed", len(successes), len(failures)))
	for _, s := range successes {
		lines = append(lines, fmt.Sprintf("  ✓ %s → %s%s", s.ResourceID, editFilePathOf(s), s.VersionSuffix()))
	}
	for _, f := range failures {
		lines = append(lines, fmt.Sprintf("  ✗ %s: %s", f.ResourceID, f.Message))
	}
	return strings.Join(lines, "\n"), len(failures) > 0, nil
}

// editFilePathOf 取编辑产物的展示路径；结果里缺 file_path 时回落到资源 id（对应 Python
// `result.get("file_path") or br.resource_id`）。
func editFilePathOf(o tools.GenerationOutcome) string {
	if path, ok := o.Details["file_path"].(string); ok && path != "" {
		return path
	}
	return o.ResourceID
}
