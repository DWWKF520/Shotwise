// 参考生视频 unit 任务执行（对应 server/services/reference_video_tasks.py::execute_reference_video_task
// 与 _finalize_reference_video_unit）。
//
// resource_id 即 unit_id（E{集}U{序号}）。narration/drama 的 unit 来自 video_units（内容自包含）；
// ad 剧本骨架唯一，unit 来自 reference_units 轻量索引，成员镜头执行期从 shots 水合。
package generation

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/domain/media"
	domainproject "github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/ffmpeg"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/reference"
)

// executeReferenceVideoTask 处理一个 reference_video unit 的生成。
func (d *TaskDispatcher) executeReferenceVideoTask(ctx context.Context, task *repository.TaskData) (map[string]any, error) {
	if d.videos == nil {
		return nil, fmt.Errorf("reference_video task requires a video generator")
	}
	scriptFile := payloadString(task.Payload, "script_file")
	if scriptFile == "" {
		return nil, fmt.Errorf("script_file is required for reference_video task")
	}

	project, err := d.store.Get(task.ProjectName)
	if err != nil {
		return nil, err
	}
	projectDir, err := d.store.Dir(task.ProjectName)
	if err != nil {
		return nil, err
	}
	script, err := d.store.ReadScriptJSON(task.ProjectName, scriptFile)
	if err != nil {
		return nil, err
	}
	isAd := project.ContentMode == string(domainproject.ContentAd)
	unit, ok := media.FindReferenceUnit(script, task.ResourceID, isAd)
	if !ok {
		return nil, fmt.Errorf("unit not found: %s", task.ResourceID)
	}
	var adShots []map[string]any
	var sourceSignature string
	if isAd {
		// 索引悬空（镜头被删/改 ID 后未重新派生）在此 fail-loud，提示重新派生。
		adShots, err = media.ResolveAdUnitShots(script, unit)
		if err != nil {
			return nil, err
		}
		// 来源签名按执行期剧本快照计算：prompt 与签名同源于此快照。
		sourceSignature = media.AdUnitSourceSignature(script, unit)
	}

	// 解析 references：narration/drama 缺图直接失败；ad 软口径跳过 + warning。
	var sourceRefs []string
	var warnings []map[string]any
	var adEntries []map[string]any
	if isAd {
		adReferences := adUnitReferenceMaps(adShots)
		var adWarnings []map[string]any
		adEntries, adWarnings = reference.ResolveAdUnitReferenceEntries(project, projectDir, adReferences)
		for _, entry := range adEntries {
			sourceRefs = append(sourceRefs, entry["image"].(string))
		}
		warnings = append(warnings, adWarnings...)
	} else {
		sourceRefs, err = reference.ResolveUnitReferences(project, projectDir, stringMapList(unit["references"]))
		if err != nil {
			return nil, err
		}
	}

	// 桶按本 unit 解析后的实际参考图分流：有参考图 → r2v；无参考图的退化镜头降级 → i2v。
	executionCapability := provider.ReferenceVideoBucket(len(sourceRefs) > 0)
	target, err := d.videos.Resolve(ctx, project, task.Payload, executionCapability, task.TaskID)
	if err != nil {
		return nil, err
	}
	providerName := ""
	if target.Backend != nil {
		providerName = target.Backend.Name()
	}
	modelName := target.BackendModel()
	caps := target.Caps()

	// model 粒度能力上限（单一真相源：backend 声明的参考图上限）。
	var maxRefs *int
	if caps.MaxReferenceImages > 0 {
		value := caps.MaxReferenceImages
		maxRefs = &value
	}
	supportedDurations := target.SupportedDurations()

	// 参考视频是唯一需要非空 resolution 档位的调用方：lane 已按 registry provider_id 兜底。
	resolution := target.FallbackResolution()
	if saved, err := d.videos.Resolution(ctx, project, target); err == nil && saved != "" {
		resolution = saved
	}
	registryProviderID := target.ProviderModel.ProviderID

	baseDuration, err := service.UnitScriptDuration(unit, adShots)
	if err != nil {
		return nil, err
	}

	var constrainedRefs []string
	var effectiveDuration int
	if isAd {
		adEntries, clampWarnings := reference.ClampAdReferenceEntries(adEntries, maxRefs, providerName, modelName)
		entryImages := make([]string, 0, len(adEntries))
		for _, entry := range adEntries {
			entryImages = append(entryImages, entry["image"].(string))
		}
		var durationWarnings []map[string]any
		constrainedRefs, effectiveDuration, durationWarnings = reference.ApplyProviderConstraints(
			providerName, modelName, nil, supportedDurations, entryImages, baseDuration,
			registryProviderID, resolution,
		)
		warnings = append(warnings, clampWarnings...)
		warnings = append(warnings, durationWarnings...)
	} else {
		constrainedRefs, effectiveDuration, warnings = reference.ApplyProviderConstraints(
			providerName, modelName, maxRefs, supportedDurations, sourceRefs, baseDuration,
			registryProviderID, resolution,
		)
	}

	// 把实际申请的秒数与执行身份写回 task：resume 路径据此重建申请参数、锁定 backend。
	if task.TaskID != "" {
		if err := d.videos.PersistEffectiveDuration(ctx, task.TaskID, effectiveDuration); err != nil {
			slog.Warn("effective_duration 写回 payload 失败", "task_id", task.TaskID, "error", err)
		}
		// 入队按 unit 声明近似定桶、执行按解析后的实际参考图定桶，二者分裂时陈旧桶键在 resume
		// 解析里优先于 provider_id 列注入——故连同锁定键一起改写：清掉其它桶的键、把实际执行
		// 身份写进实际桶的键。非分裂场景等值改写，幂等。
		identityPatch := map[string]any{}
		for _, cap := range provider.VideoBucketOrder {
			if cap == executionCapability {
				continue
			}
			identityPatch["video_provider_"+string(cap)] = nil
		}
		identityPatch["video_provider_"+string(executionCapability)] = target.ProviderModel.PairKey()
		if err := d.videos.PersistExecutionIdentity(ctx, task.TaskID, registryProviderID, identityPatch); err != nil {
			return nil, err
		}
	}

	// 渲染 prompt。
	audioPaths := reference.ResolveReferenceAudioPaths(project, projectDir)
	requestedAudio, err := d.videos.RequestedGenerateAudio(ctx, project)
	if err != nil {
		requestedAudio = true
	}
	voiceSettings := reference.VoiceRenderSettings{
		VoiceConsistency:       service.VoiceConsistencyForCaps(caps, target.ProviderModel, project.GenerationMode),
		RequestedGenerateAudio: requestedAudio,
		MaxReferenceAudio:      caps.MaxReferenceAudioCount,
		ModelID:                modelName,
		// 参考音频路径先解析再渲染：渲染层按「确实可用」判定绑定，@音频N 的编号与随请求发出的
		// 段数因此严格等长（字段指向已删文件时不会留下指向不存在段的编号）。
		AudioReady:             reference.AudioPathSet(audioPaths),
		RequiresReferenceImage: caps.ReferenceAudioPerImage,
	}

	var rendered reference.RenderedUnitPrompt
	if isAd {
		rendered, err = reference.RenderAdBackendPrompt(adShots, adEntries, project, voiceSettings, project.Style)
		if err != nil {
			return nil, err
		}
		productNames := []string{}
		seen := map[string]bool{}
		for _, entry := range adEntries {
			kind, _ := entry["kind"].(string)
			if kind != "sheet" && kind != "original" {
				continue
			}
			name, _ := entry["name"].(string)
			if name != "" && !seen[name] {
				seen[name] = true
				productNames = append(productNames, name)
			}
		}
		rendered.Prompt = reference.AppendProductFidelityTail(rendered.Prompt, productNames)
	} else {
		rendered, err = renderUnitPromptForUnit(unit, project, constrainedRefs, voiceSettings, project.Style)
		if err != nil {
			return nil, err
		}
	}
	warnings = append(warnings, rendered.Warnings...)

	referenceAudioFiles, referenceAudioTargets := reference.BuildReferenceAudioWiring(
		rendered, audioPaths, voiceSettings.RequiresReferenceImage,
	)

	aspectRatio := project.AspectRatio
	if aspectRatio == "" {
		aspectRatio = "9:16"
	}
	out, err := d.videos.Generate(ctx, target, VideoGenerateParams{
		ProjectName:           task.ProjectName,
		ProjectDir:            projectDir,
		UserID:                task.UserID,
		TaskID:                task.TaskID,
		ResourceType:          "reference_videos",
		ResourceID:            task.ResourceID,
		Prompt:                rendered.Prompt,
		ReferenceImages:       constrainedRefs,
		ReferenceAudioFiles:   referenceAudioFiles,
		ReferenceAudioTargets: referenceAudioTargets,
		AspectRatio:           aspectRatio,
		DurationSeconds:       effectiveDuration,
		Resolution:            resolution,
	})
	if err != nil {
		return nil, err
	}

	signatureArg := service.SignatureCompute
	if isAd {
		signatureArg = service.SignatureValue
	}
	return d.finalizeReferenceVideoUnit(task, projectDir, scriptFile, out, warnings, signatureArg, sourceSignature)
}

// adUnitReferenceMaps 把 ad 成员镜头的参考集渲染成 “[{type,name}]“ 结构。
func adUnitReferenceMaps(shots []map[string]any) []map[string]any {
	refs := media.AdUnitReferences(shots)
	out := make([]map[string]any, 0, len(refs))
	for _, ref := range refs {
		out = append(out, map[string]any{"type": ref.Type, "name": ref.Name})
	}
	return out
}

// stringMapList 把 []any 过滤成 []map[string]any（非对象条目跳过，与 Python 对脏数据的跳过同口径）。
func stringMapList(value any) []map[string]any {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, raw := range list {
		if m, ok := raw.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// finalizeReferenceVideoUnit 是 normal + resume + 上传共用：抽缩略图、写 unit.generated_assets、
// 返回 result dict。
func (d *TaskDispatcher) finalizeReferenceVideoUnit(
	task *repository.TaskData, projectDir, scriptFile string, out VideoGenerateResult,
	warnings []map[string]any, mode service.SignatureMode, signature string,
) (map[string]any, error) {
	thumbRel := ""
	thumbDir := filepath.Join(projectDir, "reference_videos", "thumbnails")
	if err := os.MkdirAll(thumbDir, 0o755); err != nil {
		return nil, err
	}
	thumbPath := filepath.Join(thumbDir, task.ResourceID+".jpg")
	if ffmpeg.MakeThumbnail(context.Background(), out.OutputPath, thumbPath) {
		thumbRel = "reference_videos/thumbnails/" + task.ResourceID + ".jpg"
	} else {
		_ = os.Remove(thumbPath)
	}

	versions := service.NewVersions(projectDir)
	var writtenSignature *string
	if err := d.store.UpdateScript(task.ProjectName, scriptFile, func(script map[string]any) error {
		written, err := applyUnitVideoAssets(script, task.ResourceID, out.VideoURI, thumbRel, service.NowISO(), mode, signature)
		if err != nil {
			return err
		}
		writtenSignature = written
		return nil
	}); err != nil {
		return nil, err
	}

	if writtenSignature != nil && *writtenSignature != "" {
		// 版本记录是签名的还原档案：补写失败只降级还原时的基准精度，不足以推翻已成功的 finalize。
		if _, err := versions.UpdateVersionMetadata("reference_videos", task.ResourceID, out.Version, map[string]any{
			"source_signature": *writtenSignature,
		}); err != nil {
			slog.Warn("来源签名补写版本记录失败", "resource_id", task.ResourceID, "version", out.Version, "error", err)
		}
	}

	// 与上面的档案补写读同一个 versions.json，同样在产物与剧本落盘之后：读失败只让结果少一个
	// 时间戳，而不是把已成功的付费生成报成失败。
	var createdAt any
	if value, err := versions.LatestCreatedAt("reference_videos", task.ResourceID); err == nil {
		createdAt = value
	} else {
		slog.Warn("读取版本记录取入库时间失败", "resource_id", task.ResourceID, "error", err)
		createdAt = nil
	}

	if warnings == nil {
		warnings = []map[string]any{}
	}
	return map[string]any{
		"version":       out.Version,
		"file_path":     "reference_videos/" + task.ResourceID + ".mp4",
		"created_at":    createdAt,
		"resource_type": "reference_videos",
		"resource_id":   task.ResourceID,
		"video_uri":     out.VideoURI,
		"warnings":      warnings,
	}, nil
}

// applyUnitVideoAssets 在剧本 dict 上写回 unit.generated_assets（对应 apply_unit_video_assets）。
//
// unit 在 narration/drama 剧本中位于 video_units、在 ad 剧本中位于 reference_units 派生索引——
// 两处都查。新结果不含 video_uri / 缩略图时清空旧值，避免指向过期 URI / 已删除文件。unit 不存在
// 返回错误（还原侧把它当正常跳过），unit 列表结构损坏同样返回错误。
//
// mode 为 service.SignatureCompute 时按本剧本现算来源签名（finalize 兜底口径）；service.SignatureValue 时沿用
// 给定值（生成 finalize 传执行期快照的签名），空串表示清除。返回写入的签名（narration/drama
// 的 video_units 条目不承载签名，返回 nil）。
func applyUnitVideoAssets(
	script map[string]any, resourceID, videoURI, thumbRel, generatedAt string, mode service.SignatureMode, signature string,
) (*string, error) {
	type unitList struct {
		key   string
		units []any
	}
	var candidates []unitList
	for _, key := range []string{"video_units", "reference_units"} {
		raw, present := script[key]
		if !present || raw == nil {
			continue
		}
		units, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("video_units / reference_units 必须是 list")
		}
		candidates = append(candidates, unitList{key: key, units: units})
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("video_units / reference_units 必须是 list")
	}
	for _, candidate := range candidates {
		for _, raw := range candidate.units {
			u, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := u["unit_id"].(string); id != resourceID {
				continue
			}
			ga, ok := u["generated_assets"].(map[string]any)
			if !ok {
				if _, present := u["generated_assets"]; present {
					return nil, fmt.Errorf("generated_assets 必须是 dict")
				}
				ga = map[string]any{}
				u["generated_assets"] = ga
			}
			ga["video_clip"] = "reference_videos/" + resourceID + ".mp4"
			if generatedAt != "" {
				ga["video_generated_at"] = generatedAt
			}
			if videoURI != "" {
				ga["video_uri"] = videoURI
			} else {
				delete(ga, "video_uri")
			}
			if thumbRel != "" {
				ga["video_thumbnail"] = thumbRel
			} else {
				delete(ga, "video_thumbnail")
			}
			ga["status"] = "completed"
			delete(u, "stale")
			if candidate.key != "reference_units" {
				return nil, nil
			}
			resolved := signature
			if mode == service.SignatureCompute {
				resolved = media.AdUnitSourceSignature(script, u)
			}
			if resolved != "" {
				ga["source_signature"] = resolved
			} else {
				delete(ga, "source_signature")
			}
			out := resolved
			return &out, nil
		}
	}
	return nil, &reference.ReferenceUnitNotFoundError{UnitID: resourceID}
}

func init() {
	// 依赖倒置注册：版本还原侧（根包 version_service）经 Hook 写回 unit 产物。
	service.ApplyUnitVideoAssetsHook = applyUnitVideoAssets
}

// renderUnitPromptForUnit 把 unit 渲染成三段论 prompt（对应 reference_video_tasks.py::_render_unit_prompt）。
//
// 空提示词的结构校验落在书写层文本上：三段论渲染恒产出非空第三段，渲染后已无从判别书写层是否为空。
// 渲染用 reference.AssembleShotsTextForRender（按数组位置重新注入规范 header），因为经解析预览面板编辑回写的
// unit 其 shots[*].text 普遍已不带 header。
func renderUnitPromptForUnit(
	unit map[string]any, project *domainproject.ProjectFile, constrainedRefs []string,
	settings reference.VoiceRenderSettings, style string,
) (reference.RenderedUnitPrompt, error) {
	shotsAny, _ := unit["shots"].([]any)
	if strings.TrimSpace(media.AssembleShotsText(shotsAny)) == "" {
		return reference.RenderedUnitPrompt{}, fmt.Errorf("reference video unit prompt is empty: all shots[*].text are blank")
	}
	// 与 constrained_refs 同口径裁 references：保证「图片N」的 1-based 索引与 backend 实际收到的
	// reference_images 一一对应；跳过去重会让编号错位到错误的资产上。
	rawRefs := stringMapList(unit["references"])
	promptRefs := reference.DedupeTypedReferences(rawRefs)
	if len(promptRefs) > len(constrainedRefs) {
		promptRefs = promptRefs[:len(constrainedRefs)]
	}
	text := reference.AssembleShotsTextForRender(shotsAny)
	return reference.RenderUnitPrompt(text, project, promptRefs, settings, style), nil
}
