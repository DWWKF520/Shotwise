// 生成任务入队服务（对应 server/routers/generate.py 的写侧逻辑）。
//
// 路由函数体只保留 happy path：领域失败一律以 *service.GenerateError 返回（带 HTTP 状态与
// i18n code），由传输层统一翻成 {"detail": ...} 信封。生成本身由 worker 异步执行，
// 这里只做「校验 + 入队 + 返回 task_id」。
package generation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain/media"
	domainproject "github.com/shotwise/shotwise/internal/domain/project"
	domainscript "github.com/shotwise/shotwise/internal/domain/script"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/reference"
)

func generateNotFound(code string, params map[string]any) *service.GenerateError {
	return &service.GenerateError{Status: http.StatusNotFound, Code: code, Params: params}
}

func generateConflict(code string) *service.GenerateError {
	return &service.GenerateError{Status: http.StatusConflict, Code: code}
}

// GenerateQueue 是入队能力（*queue.Queue 满足）。
type GenerateQueue interface {
	EnqueueTask(ctx context.Context, p repository.EnqueueParams) (repository.EnqueueResult, error)
	GetTask(ctx context.Context, taskID string) (*repository.TaskData, error)
}

// GenerateResolver 是生成路径需要的配置解析能力（*config.Resolver 满足）。
type GenerateResolver interface {
	service.VideoCapabilityResolver
	ResolveAudioBackend(ctx context.Context, project *domainproject.ProjectFile, payload map[string]any) (config.ProviderModel, error)
	ResolveImageBackend(
		ctx context.Context, project *domainproject.ProjectFile, payload map[string]any, capability config.ImageCapability,
	) (config.ProviderModel, error)
}

// GenerateService 是生成入队的应用服务。
type GenerateService struct {
	store    *service.ProjectStore
	queue    GenerateQueue
	resolver GenerateResolver
	audio    *AudioGenerator
}

// NewGenerateService 构造。audio 仅用于音色目录查询，可为 nil。
func NewGenerateService(
	store *service.ProjectStore, queue GenerateQueue, resolver GenerateResolver, audio *AudioGenerator,
) *GenerateService {
	return &GenerateService{store: store, queue: queue, resolver: resolver, audio: audio}
}

// 资产类型 → 生成入口的 i18n key（scene 用历史前缀 project_scene_*）。
var assetGenerateKeys = map[domainproject.AssetKind]struct{ NotFound, Submitted string }{
	domainproject.AssetCharacter: {NotFound: "character_not_found", Submitted: "character_task_submitted"},
	domainproject.AssetScene:     {NotFound: "project_scene_not_found", Submitted: "scene_task_submitted"},
	domainproject.AssetProp:      {NotFound: "prop_not_found", Submitted: "prop_task_submitted"},
	domainproject.AssetProduct:   {NotFound: "product_not_found", Submitted: "product_task_submitted"},
}

// GenerateStoryboard 提交分镜图生成任务。
func (s *GenerateService) GenerateStoryboard(
	ctx context.Context, projectName, segmentID string, prompt any, scriptFile, userID string,
) (map[string]any, error) {
	project, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	dir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	script, err := s.store.ReadScriptJSON(projectName, scriptFile)
	if err != nil {
		return nil, err
	}
	items, idField, fields := media.StoryboardItems(script)
	item, _, ok := media.FindStoryboardItem(items, idField, segmentID)
	if !ok {
		return nil, generateNotFound("segment_not_found", map[string]any{"id": segmentID})
	}
	preflight := service.ValidateItemAssetReferences(project, dir, item, fields)
	if len(preflight.Issues) > 0 {
		return nil, service.GenerateBadRequest("generation_asset_preflight_failed", preflight.I18NParams())
	}

	spec, err := BuildTaskSpec("storyboard", "image", segmentID, prompt, &scriptFile, nil)
	if err != nil {
		return nil, err
	}
	result, err := s.enqueue(ctx, projectName, spec, userID, "", nil)
	if err != nil {
		return nil, err
	}
	return submittedResult(result, "storyboard_task_submitted", map[string]any{"segment_id": segmentID}), nil
}

// GenerateVideo 提交分镜图生视频任务。
//
// 路线闸门前置于分镜图存在性检查：参考生视频路线本无分镜图步骤，落到后面会拿到
// 「请先生成分镜图」的误导指引；路线以 project.json 为唯一真相源，磁盘产物不投票。
func (s *GenerateService) GenerateVideo(
	ctx context.Context, projectName, segmentID string, prompt any, scriptFile string,
	durationSeconds, seed *int64, userID string,
) (map[string]any, error) {
	project, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	if project.GenerationMode == string(domainproject.GenerationReferenceVideo) {
		return nil, generateConflict("video_route_is_reference_video")
	}
	dir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	script, err := s.store.ReadScriptJSON(projectName, scriptFile)
	if err != nil {
		return nil, err
	}
	items, idField, fields := media.StoryboardItems(script)
	item, _, ok := media.FindStoryboardItem(items, idField, segmentID)
	if !ok {
		return nil, generateNotFound("segment_not_found", map[string]any{"id": segmentID})
	}
	if preflight := service.ValidateItemAssetReferences(project, dir, item, fields); len(preflight.Issues) > 0 {
		return nil, service.GenerateBadRequest("generation_asset_preflight_failed", preflight.I18NParams())
	}

	// 分镜图引用来自磁盘剧本 JSON，不可信任：脏类型 / 越界路径都在这里拦下。
	storyboardRel := media.GeneratedAssets(item)["storyboard_image"]
	storyboardFile, err := service.ResolveStoryboardImageRef(dir, storyboardRel)
	if err != nil {
		return nil, service.GenerateBadRequest("invalid_storyboard_image_path", map[string]any{"segment_id": segmentID})
	}
	if storyboardFile == "" {
		storyboardFile = filepath.Join(dir, "storyboards", "scene_"+segmentID+".png")
	}
	if info, statErr := os.Stat(storyboardFile); statErr != nil || info.IsDir() {
		return nil, service.GenerateBadRequest("generate_storyboard_first", map[string]any{"segment_id": segmentID})
	}

	// 归桶按项目路线求值，与执行层 lane 声明同源；上面的路线闸门已挡掉参考路线。
	bucket := provider.VideoBucketForGenerationMode(project.GenerationMode)
	if _, err := s.resolver.VideoCapabilitiesForProjectBucket(ctx, project, bucket); err != nil {
		var capabilityErr *config.VideoBucketCapabilityError
		if errors.As(err, &capabilityErr) {
			params := map[string]any{}
			for k, v := range capabilityErr.Params() {
				params[k] = v
			}
			return nil, service.GenerateBadRequest(string(capabilityErr.Code), params)
		}
		return nil, err
	}
	if err := service.AssertAudioSwitchSupported(ctx, s.resolver, project, bucket); err != nil {
		var conflict *service.AudioSwitchConflictError
		if errors.As(err, &conflict) {
			return nil, service.GenerateBadRequest("video_audio_switch_not_supported", map[string]any{
				"provider": conflict.ProviderID, "model": conflict.ModelID,
			})
		}
		return nil, err
	}

	requested := s.resolveEpisodeDurationRequest(ctx, projectName, project, script, item, segmentID, durationSeconds)
	extra := map[string]any{"duration_seconds": durationSeconds, "seed": seed}
	if requested != nil {
		// duration_seconds 仍是执行契约；requested_seconds 单独标记该值来自规划而非剧本。
		extra["requested_seconds"] = *requested
		extra["duration_seconds"] = *requested
	}
	spec, err := BuildTaskSpec("video", "video", segmentID, prompt, &scriptFile, extra)
	if err != nil {
		return nil, err
	}
	result, err := s.enqueue(ctx, projectName, spec, userID, "", nil)
	if err != nil {
		return nil, err
	}
	return submittedResult(result, "video_task_submitted", map[string]any{"segment_id": segmentID}), nil
}

// resolveEpisodeDurationRequest 解析剧集时长规划请求，不修改剧本里已持久化的时长。
//
// 显式请求时长优先；分镜已锁时长或已有成片时返回 nil。规划是可选层：配置缺失、
// 能力不可解析或畸形数据都返回 nil，把时长交还给既有路径。
func (s *GenerateService) resolveEpisodeDurationRequest(
	ctx context.Context, projectName string, project *domainproject.ProjectFile, script map[string]any,
	item map[string]any, resourceID string, explicit *int64,
) *int {
	if explicit != nil || domainscript.IsItemDurationLocked(item) || domainscript.IsItemVideoGenerated(item) {
		return nil
	}
	projectRaw, err := s.store.ReadProjectRaw(projectName)
	if err != nil {
		return nil
	}
	planConfig := domainscript.ReadEpisodeDurationPlan(projectRaw, script)
	if planConfig == nil {
		return nil
	}
	bucket := provider.VideoBucketForGenerationMode(project.GenerationMode)
	view, err := s.resolver.VideoCapabilitiesForProjectBucket(ctx, project, bucket)
	if err != nil {
		return nil
	}
	supported := make([]int, 0, len(view.SupportedDurations))
	for _, value := range view.SupportedDurations {
		if value > 0 {
			supported = append(supported, value)
		}
	}
	if len(supported) == 0 {
		return nil
	}
	items, idField, _ := media.StoryboardItems(script)
	inputs := make([]domainscript.ShotDurationInput, 0, len(items))
	for _, raw := range items {
		candidate, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		candidateID := strings.TrimSpace(fmt.Sprint(candidate[idField]))
		if candidateID == "" {
			continue
		}
		var current *int
		if seconds, ok := pyIntValue(candidate["duration_seconds"]); ok && seconds > 0 {
			value := seconds
			current = &value
		}
		weight := 1.0
		if raw, ok := candidate["duration_weight"].(float64); ok && raw > 0 {
			weight = raw
		}
		inputs = append(inputs, domainscript.ShotDurationInput{
			ShotID:             candidateID,
			CurrentSeconds:     current,
			SupportedDurations: supported,
			Locked:             domainscript.IsItemDurationLocked(candidate),
			Generated:          domainscript.IsItemVideoGenerated(candidate),
			Weight:             weight,
		})
	}
	planner := domainscript.EpisodeDurationPlanner{}
	plan, err := planner.BuildPlan(
		planConfig.TargetSeconds, inputs, planConfig.Strategy, planConfig.ManualAllocations, nil)
	if err != nil {
		return nil
	}
	return planner.RequestedSecondsFor(resourceID, plan)
}

// GenerateTTS 提交单段旁白配音任务。
func (s *GenerateService) GenerateTTS(
	ctx context.Context, projectName, segmentID, scriptFile, userID string,
) (map[string]any, error) {
	project, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	script, err := s.store.ReadScriptJSON(projectName, scriptFile)
	if err != nil {
		return nil, err
	}
	items, idField, _ := media.StoryboardItems(script)
	item, _, ok := media.FindStoryboardItem(items, idField, segmentID)
	if !ok {
		return nil, generateNotFound("segment_not_found", map[string]any{"id": segmentID})
	}
	if narrationText(item) == "" {
		return nil, service.GenerateBadRequest("tts_novel_text_missing", map[string]any{"segment_id": segmentID})
	}
	providerID, err := s.audioProviderID(ctx, project)
	if err != nil {
		return nil, err
	}
	result, err := s.enqueueTTSSegment(ctx, projectName, segmentID, scriptFile, userID, providerID)
	if err != nil {
		return nil, err
	}
	return submittedResult(result, "tts_task_submitted", map[string]any{"segment_id": segmentID}), nil
}

// GenerateTTSBatch 批量提交旁白配音任务：只入队缺 narration_audio 且有小说原文的段。
func (s *GenerateService) GenerateTTSBatch(
	ctx context.Context, projectName, scriptFile, userID string,
) (map[string]any, error) {
	project, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	script, err := s.store.ReadScriptJSON(projectName, scriptFile)
	if err != nil {
		return nil, err
	}
	items, idField, _ := media.StoryboardItems(script)
	missing := make([]string, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if narrationText(item) == "" {
			continue
		}
		if service.PyTruthy(media.GeneratedAssets(item)["narration_audio"]) {
			continue
		}
		if id := strings.TrimSpace(fmt.Sprint(item[idField])); id != "" {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return map[string]any{
			"success": true, "task_ids": []string{}, "deduped": false, "message_key": "tts_batch_none_missing",
		}, nil
	}
	providerID, err := s.audioProviderID(ctx, project)
	if err != nil {
		return nil, err
	}
	taskIDs := make([]string, 0, len(missing))
	allDeduped := true
	for _, segmentID := range missing {
		result, err := s.enqueueTTSSegment(ctx, projectName, segmentID, scriptFile, userID, providerID)
		if err != nil {
			return nil, err
		}
		taskIDs = append(taskIDs, result.TaskID)
		if !result.Deduped {
			allDeduped = false
		}
	}
	return map[string]any{
		"success":  true,
		"task_ids": taskIDs,
		// 批量语义：全部入队都命中既有任务（本次一个新任务都没建）才算 deduped。
		"deduped":     len(taskIDs) > 0 && allDeduped,
		"message_key": "tts_batch_submitted",
		"message_params": map[string]any{
			"count": len(taskIDs),
		},
	}, nil
}

// AudioBackendVoices 返回项目实际生效的 audio backend 音色枚举。
//
// 未配置任何 audio 供应商时返回 configured=false + 空列表，不报错——前端据此禁用
// 生成入口而非报错。
func (s *GenerateService) AudioBackendVoices(
	ctx context.Context, projectName string,
) (map[string]any, error) {
	project, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	if s.audio == nil {
		return map[string]any{"configured": false, "provider_id": nil, "model": nil, "voices": []any{}}, nil
	}
	voices, err := s.audio.Voices(ctx, project)
	if err != nil || !voices.Configured {
		return map[string]any{"configured": false, "provider_id": nil, "model": nil, "voices": []any{}}, nil
	}
	items := make([]map[string]any, 0, len(voices.Voices))
	for _, voice := range voices.Voices {
		items = append(items, map[string]any{"id": voice.ID, "label_key": voice.Label})
	}
	return map[string]any{
		"configured":  true,
		"provider_id": voices.ProviderID,
		"model":       voices.Model,
		"voices":      items,
	}, nil
}

// VoiceSampleTextMaxLength 是试听文案长度粗护栏（与前端同值）。
const VoiceSampleTextMaxLength = 200

// GenerateVoiceSample 提交角色 TTS 试听样本生成任务。
func (s *GenerateService) GenerateVoiceSample(
	ctx context.Context, projectName, name, text, voice, userID string,
) (map[string]any, error) {
	text = strings.TrimSpace(text)
	voice = strings.TrimSpace(voice)
	if text == "" {
		return nil, service.GenerateBadRequest("prompt_text_empty", nil)
	}
	if len([]rune(text)) > VoiceSampleTextMaxLength {
		return nil, service.GenerateBadRequest("voice_sample_text_too_long", map[string]any{
			"max_length": VoiceSampleTextMaxLength,
		})
	}
	if voice == "" {
		return nil, service.GenerateBadRequest("voice_sample_voice_required", nil)
	}
	project, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	charName, err := domainproject.ValidateAssetName(name)
	if err != nil {
		return nil, service.GenerateBadRequest("asset_invalid_name", map[string]any{"name": name})
	}
	bucket, ok := project.BucketRef(domainproject.AssetCharacter)
	if !ok || bucket == nil {
		return nil, generateNotFound("character_not_found", map[string]any{"name": charName})
	}
	if _, found := bucket.ResolveKey(charName); !found {
		return nil, generateNotFound("character_not_found", map[string]any{"name": charName})
	}
	providerID, err := s.audioProviderID(ctx, project)
	if err != nil {
		return nil, err
	}
	spec, err := BuildTaskSpec("voice_sample", "audio", charName, text, nil, map[string]any{"voice": voice})
	if err != nil {
		return nil, err
	}
	result, err := s.enqueue(ctx, projectName, spec, userID, providerID, nil)
	if err != nil {
		return nil, err
	}
	return submittedResult(result, "voice_sample_task_submitted", map[string]any{"name": charName}), nil
}

// ConfirmVoiceSample 把已生成、已试听的样本提升为角色 reference_audio。
//
// 只信一个 task_id 指向的 voice_sample 任务结果，不接受客户端直传路径——产物已在执行层
// 过一遍与上传同口径的格式 / 时长 / 大小校验，此处只做归属校验后原样落盘。
func (s *GenerateService) ConfirmVoiceSample(
	ctx context.Context, projectName, name, taskID string,
) (map[string]any, error) {
	charName, err := domainproject.ValidateAssetName(name)
	if err != nil {
		return nil, service.GenerateBadRequest("asset_invalid_name", map[string]any{"name": name})
	}
	task, err := s.queue.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil || task.ProjectName != projectName || task.TaskType != "voice_sample" || task.ResourceID != charName {
		return nil, generateNotFound("task_not_found", map[string]any{"id": taskID})
	}
	if task.Status != repository.TaskStatusSucceeded {
		return nil, service.GenerateBadRequest("voice_sample_not_ready", nil)
	}
	sampleRel, _ := task.Result["file_path"].(string)
	if sampleRel == "" {
		return nil, service.GenerateBadRequest("voice_sample_not_ready", nil)
	}

	project, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	bucket, ok := project.BucketRef(domainproject.AssetCharacter)
	if !ok || bucket == nil {
		return nil, generateNotFound("character_not_found", map[string]any{"name": charName})
	}
	charKey, found := bucket.ResolveKey(charName)
	if !found {
		return nil, generateNotFound("character_not_found", map[string]any{"name": charName})
	}
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	if !fs.SafeExists(projectDir, filepath.FromSlash(sampleRel)) {
		return nil, generateNotFound("voice_sample_file_missing", nil)
	}
	samplePath, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(sampleRel))
	if err != nil {
		return nil, generateNotFound("voice_sample_file_missing", nil)
	}
	content, err := os.ReadFile(samplePath)
	if err != nil {
		return nil, generateNotFound("voice_sample_file_missing", nil)
	}

	refsAudioDir := filepath.Join(projectDir, "characters", "refs_audio")
	if err := os.MkdirAll(refsAudioDir, 0o755); err != nil {
		return nil, err
	}
	filename := charName + ".wav"
	targetPath := filepath.Join(refsAudioDir, filename)
	refAudioRel := "characters/refs_audio/" + filename
	oldAudio := (*bucket)[charKey].MustString("reference_audio")
	staleAudio := resolveStaleReferenceAudio(projectDir, oldAudio, targetPath)
	if err := os.WriteFile(targetPath, content, 0o644); err != nil {
		return nil, err
	}
	if err := s.store.Update(projectName, func(pf *domainproject.ProjectFile) error {
		update, ok := pf.BucketRef(domainproject.AssetCharacter)
		if !ok || update == nil {
			return service.ErrResourceMissingInScript
		}
		key, ok := update.ResolveKey(charName)
		if !ok {
			return service.ErrResourceMissingInScript
		}
		entry := (*update)[key]
		entry.SetString("reference_audio", refAudioRel)
		// 机械戳 voice_updated_at：与资产 PATCH 侧同口径，存量过渡横幅据此失效。
		entry.SetString("voice_updated_at", service.NowISO())
		return nil
	}); err != nil {
		if errors.Is(err, service.ErrResourceMissingInScript) {
			return nil, generateNotFound("character_not_found", map[string]any{"name": charName})
		}
		return nil, err
	}
	discardStaleReferenceAudio(staleAudio)
	return map[string]any{
		"success": true,
		"path":    refAudioRel,
		"url":     "/api/v1/files/" + projectName + "/" + refAudioRel,
	}, nil
}

// resolveStaleReferenceAudio 识别「换掉后就没有指针指向」的旧文件。
//
// 返回空串表示无需清理：旧指针为空、指向 refs_audio 之外，或新旧文件在同一 inode 上
// （大小写不敏感文件系统）——此时新内容即将原地覆盖，不能当孤儿删掉。
func resolveStaleReferenceAudio(projectDir, oldAudio, newPath string) string {
	if strings.TrimSpace(oldAudio) == "" {
		return ""
	}
	resolved := reference.ResolveAudioRefPath(projectDir, oldAudio)
	if resolved == "" {
		return ""
	}
	oldInfo, err := os.Stat(resolved)
	if err == nil {
		if newInfo, newErr := os.Stat(newPath); newErr == nil && os.SameFile(oldInfo, newInfo) {
			return ""
		}
	}
	return resolved
}

// discardStaleReferenceAudio 删除已无指针指向的旧参考音频；失败只记日志不抛。
func discardStaleReferenceAudio(stalePath string) {
	if stalePath == "" {
		return
	}
	if err := os.Remove(stalePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		// 新文件与字段都已提交，此处再抛会让调用方误以为整次替换失败并重试。
		fmt.Printf("旧参考音频物理删除失败，可能残留孤儿文件：%s (%v)\n", stalePath, err)
	}
}

// GenerateAsset 提交项目级资产（character / scene / prop / product）设计图生成任务。
func (s *GenerateService) GenerateAsset(
	ctx context.Context, kind domainproject.AssetKind, projectName, resourceName string, prompt string,
	regenerate bool, userID string,
) (map[string]any, error) {
	keys, ok := assetGenerateKeys[kind]
	if !ok {
		return nil, service.GenerateBadRequest("asset_invalid_type", map[string]any{"type": string(kind)})
	}
	project, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	bucket, ok := project.BucketRef(kind)
	if !ok || bucket == nil {
		return nil, generateNotFound(keys.NotFound, map[string]any{"name": resourceName})
	}
	resourceKey, found := bucket.ResolveKey(resourceName)
	if !found {
		return nil, generateNotFound(keys.NotFound, map[string]any{"name": resourceName})
	}
	assetSpec, specOK := domainproject.Spec(kind)
	if !specOK {
		return nil, service.GenerateBadRequest("asset_invalid_type", map[string]any{"type": string(kind)})
	}
	dir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	sheet := ""
	if entry := (*bucket)[resourceKey]; entry != nil {
		sheet = entry.MustString(assetSpec.SheetKey)
	}
	// 只把项目内、真实存在的普通文件视为可复用资产：失败任务的临时文件、已删除但仍
	// 残留在 project.json 的路径以及越界路径都不会命中。
	hasExisting := sheet != "" && fs.SafeExists(dir, filepath.FromSlash(sheet))
	if hasExisting && !regenerate {
		return map[string]any{
			"success": true, "task_id": nil, "deduped": false, "reused": true,
			"message_key": "asset_generation_reused",
			"message_params": map[string]any{
				"name": resourceName,
			},
		}, nil
	}
	taskSpec, err := BuildTaskSpec(string(kind), "image", resourceKey, prompt, nil, nil)
	if err != nil {
		return nil, err
	}
	result, err := s.enqueue(ctx, projectName, taskSpec, userID, "", nil)
	if err != nil {
		return nil, err
	}
	out := submittedResult(result, keys.Submitted, map[string]any{"name": resourceName})
	out["reused"] = false
	return out, nil
}

// EditImage 提交图片指令式编辑任务。
func (s *GenerateService) EditImage(
	ctx context.Context, projectName, resourceType, resourceID, instruction string,
	scriptFile, referenceMediaAssetID *string, userID string,
) (map[string]any, error) {
	if !IsEditableResourceType(resourceType) {
		return nil, service.GenerateBadRequest("image_edit_resource_type_invalid", map[string]any{
			"resource_type": resourceType,
		})
	}
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return nil, service.GenerateBadRequest("image_edit_instruction_required", nil)
	}
	isStoryboard := resourceType == StoryboardResourceType
	var scriptName *string
	if scriptFile != nil {
		if trimmed := strings.TrimSpace(*scriptFile); trimmed != "" {
			scriptName = &trimmed
		}
	}
	if isStoryboard && scriptName == nil {
		return nil, service.GenerateBadRequest("image_edit_script_file_required", nil)
	}

	project, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	dir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	var script map[string]any
	if isStoryboard {
		if script, err = s.store.ReadScriptJSON(projectName, *scriptName); err != nil {
			return nil, err
		}
	}
	currentRel, found := ResolveCurrentImageRel(project, resourceType, resourceID, script)
	if !found {
		if isStoryboard {
			return nil, generateNotFound("segment_not_found", map[string]any{"id": resourceID})
		}
		keys := assetGenerateKeys[domainproject.AssetKind(resourceType)]
		return nil, generateNotFound(keys.NotFound, map[string]any{"name": resourceID})
	}
	if currentRel == "" || !fs.SafeExists(dir, filepath.FromSlash(currentRel)) {
		return nil, service.GenerateBadRequest("image_edit_no_current_image", map[string]any{"id": resourceID})
	}
	extra := map[string]any{"resource_type": resourceType}
	referenceID := ""
	if referenceMediaAssetID != nil {
		if trimmed := strings.TrimSpace(*referenceMediaAssetID); trimmed != "" {
			referenceID = trimmed
			extra["reference_media_asset_id"] = referenceID
			if !s.referenceMediaUsable(dir, referenceID) {
				return nil, service.GenerateBadRequest("image_edit_reference_invalid", map[string]any{"id": referenceID})
			}
		}
	}
	// 图片编辑必然 i2i 且入队即知，故解析前置到入队；执行层的 capability gating 保留兜底。
	if _, err := s.resolver.ResolveImageBackend(ctx, project, nil, config.ImageCapI2I); err != nil {
		return nil, service.GenerateBadRequest("image_edit_i2i_unavailable", nil)
	}
	var taskScriptFile *string
	if isStoryboard {
		taskScriptFile = scriptName
	}
	spec, err := BuildTaskSpec("image_edit", "image", resourceID, instruction, taskScriptFile, extra)
	if err != nil {
		return nil, err
	}
	providerID := s.imageI2IProviderID(ctx, project)
	// image_edit 跨四类资产 + storyboard 共用同一 task_type，resource_id 命名空间不互斥
	// （角色和道具可能同名），纳入去重键避免跨类型误判为重复任务。
	result, err := s.enqueue(ctx, projectName, spec, userID, providerID, &resourceType)
	if err != nil {
		return nil, err
	}
	return submittedResult(result, "image_edit_task_submitted", map[string]any{"id": resourceID}), nil
}

// imageI2IProviderID 取 i2i 槽解析出的 provider_id（限流池路由按 i2i 槽精确记账）。
func (s *GenerateService) imageI2IProviderID(ctx context.Context, project *domainproject.ProjectFile) string {
	selected, err := s.resolver.ResolveImageBackend(ctx, project, nil, config.ImageCapI2I)
	if err != nil {
		return ""
	}
	return selected.ProviderID
}

// referenceMediaUsable 校验参考素材是项目内的图片且文件存在。
func (s *GenerateService) referenceMediaUsable(projectDir, mediaAssetID string) bool {
	asset, err := service.GetProjectMediaAssetHook(projectDir, mediaAssetID)
	if err != nil || asset == nil {
		return false
	}
	kind, _ := asset["kind"].(string)
	if kind != "image" {
		return false
	}
	physical, _ := asset["physical_path"].(string)
	if physical == "" {
		return false
	}
	return fs.SafeExists(projectDir, filepath.FromSlash(physical))
}

// ---- 内部 ----

// enqueue 统一入队：spec → EnqueueParams。
func (s *GenerateService) enqueue(
	ctx context.Context, projectName string, spec TaskSpec, userID, providerID string, resourceType *string,
) (repository.EnqueueResult, error) {
	var providerPtr *string
	if providerID != "" {
		providerPtr = &providerID
	}
	return s.queue.EnqueueTask(ctx, repository.EnqueueParams{
		ProjectName:  projectName,
		TaskType:     spec.TaskType,
		MediaType:    spec.MediaType,
		ResourceID:   spec.ResourceID,
		Payload:      spec.Payload,
		ScriptFile:   spec.ScriptFile,
		ResourceType: resourceType,
		Source:       "webui",
		UserID:       userID,
		ProviderID:   providerPtr,
	})
}

// enqueueTTSSegment 入队单段旁白配音任务（复用已解析的 provider，避免每段重复解析）。
func (s *GenerateService) enqueueTTSSegment(
	ctx context.Context, projectName, segmentID, scriptFile, userID, providerID string,
) (repository.EnqueueResult, error) {
	script := scriptFile
	spec, err := BuildTaskSpec("tts", "audio", segmentID, nil, &script, nil)
	if err != nil {
		return repository.EnqueueResult{}, err
	}
	return s.enqueue(ctx, projectName, spec, userID, providerID, nil)
}

// audioProviderID 解析 audio 供应商；解析失败视为未配置（400）。
func (s *GenerateService) audioProviderID(ctx context.Context, project *domainproject.ProjectFile) (string, error) {
	selected, err := s.resolver.ResolveAudioBackend(ctx, project, nil)
	if err != nil {
		return "", service.GenerateBadRequest("audio_provider_not_configured", nil)
	}
	return selected.ProviderID, nil
}

// narrationText 取分段的旁白文本（缺省为空串）。
func narrationText(item map[string]any) string {
	text, ok := item["novel_text"].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

// submittedResult 是入队成功的统一响应体。
func submittedResult(
	result repository.EnqueueResult, messageKey string, params map[string]any,
) map[string]any {
	return map[string]any{
		"success":        true,
		"task_id":        result.TaskID,
		"deduped":        result.Deduped,
		"message_key":    messageKey,
		"message_params": params,
	}
}

// pyIntValue 复刻 Python 的 int 判定（bool 不算 int）。
func pyIntValue(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if v != float64(int(v)) {
			return 0, false
		}
		return int(v), true
	}
	return 0, false
}
