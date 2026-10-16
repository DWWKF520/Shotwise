// 视频任务执行（对应 server/services/generation_tasks.py::execute_video_task 与
// _finalize_video_task）：分镜路线的 i2v，首帧取该镜头分镜图、尾帧取镜头持久属性快照。
//
// 与工具侧（internal/agent/tools_generate_video.go）同口径：能力闸门、时长守卫与桶全部由
// VideoGenerator 门面统一承担，执行层不各写一份。
package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/ffmpeg"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

// executeVideoTask 执行一个分镜镜头的视频生成并把产物路径/缩略图写回剧本。
func (d *TaskDispatcher) executeVideoTask(ctx context.Context, task *repository.TaskData) (map[string]any, error) {
	if d.videos == nil {
		return nil, fmt.Errorf("video task requires a video generator")
	}
	scriptFile := payloadString(task.Payload, "script_file")
	if scriptFile == "" {
		return nil, fmt.Errorf("script_file is required for video task")
	}
	rawPrompt, present := task.Payload["prompt"]
	if !present || rawPrompt == nil {
		return nil, fmt.Errorf("prompt is required for video task")
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
	items, idField, _, err := domain.ResolveScriptItems(script)
	if err != nil {
		return nil, err
	}
	item, _, _ := domain.FindStoryboardItem(items, idField, task.ResourceID)
	contentMode := scriptContentMode(script, project)

	// lane 归桶按项目路线求值，与提交入口 generate_video 同源：入口挡掉参考路线后到达这里的
	// 项目恒为 i2v，但桶不在两处各硬编码一次，避免路线口径分叉。
	target, err := d.videos.Resolve(ctx, project, task.Payload,
		provider.VideoBucketForGenerationMode(project.GenerationMode), task.TaskID)
	if err != nil {
		return nil, err
	}

	// 优先读取 generated_assets.storyboard_image，回退默认路径。校验口径与路由入队预检、
	// SDK 工具入队预检共用同一份 resolveStoryboardImageRef。
	storyboardRel := domain.GeneratedAssets(item)["storyboard_image"]
	storyboardFile, err := ResolveStoryboardImageRef(projectDir, storyboardRel)
	if err != nil {
		return nil, err
	}
	if storyboardFile == "" {
		storyboardFile = filepath.Join(projectDir, "storyboards", "scene_"+task.ResourceID+".png")
	}
	// is_file 而非 exists：字段被外部编辑指向目录（如 storyboards 本身）时 exists 仍为真，
	// 目录会被当作 start_image 传给视频后端，在编码阶段才失败且原因不可读。
	if !pathIsFile(storyboardFile) {
		return nil, fmt.Errorf("storyboard not found: %s", filepath.Base(storyboardFile))
	}

	prompt := rawPrompt
	// Voice_Profiles 声明段唯一来源是本函数下方的戏剧台词派生：调用方（WebUI 请求体 / 剧本 JSON
	// 残留）自带的 voice_profiles 一律先剥离，不因 utterances 门控不触发而绕过真无声门控直达 YAML。
	if m, ok := prompt.(map[string]any); ok {
		prompt = StripVoiceProfiles(m)
	}

	// drama 口型台词单一真相源在场景级有序 utterances：从 dialogue-kind 条目取台词注入 video YAML
	// 的 dialogue 出口。narration / ad 的 item 无 utterances 字段，payload.dialogue 原样透传；
	// SDK 路径 prompt 已是渲染好的字符串、跳过。条目不在剧本中时按空条目处理（与 Python 的
	// 「找不到分镜回退空 dict」同口径），戏剧路径照常走台词派生。
	if m, ok := prompt.(map[string]any); ok && contentMode == string(domain.ContentDrama) {
		voiceCharacters := d.dramaVoiceCharacters(ctx, project, target)
		utterances, hasUtterances := any(nil), false
		if item != nil {
			utterances, hasUtterances = item["utterances"]
		}
		if hasUtterances {
			prompt = BuildDramaVideoPrompt(m, utterances, voiceCharacters)
		} else {
			// utterances 迁移前的存量剧本：load_script 按原始 JSON 读盘不过校验，不会被自动
			// 补齐，台词仍留在 video_prompt.dialogue。
			prompt = BuildDramaVideoPromptFromLegacyDialogue(m, voiceCharacters)
		}
	}

	promptText, err := normalizeVideoPrompt(prompt)
	if err != nil {
		return nil, err
	}
	aspectRatio := aspectRatioFor(project, "videos")
	seed := payloadInt64(task.Payload, "seed")
	serviceTier := payloadNestedString(task.Payload, "video_provider_settings", "service_tier")
	if serviceTier == "" {
		serviceTier = "default"
	}

	// provider / model / 能力 / 分辨率均取自单次解析的 video lane：能力按 backend 实际身份
	// 查询，与实际要调用的 model 对齐——历史任务 payload 携带 provider 覆盖、或自定义供应商
	// 目标 model 被禁用回退时，二者一致避免 duration 守卫误判。
	registryProviderID := target.ProviderModel.ProviderID
	modelName := target.BackendModel()
	supportedDurations := target.SupportedDurations()
	resolution, err := d.videos.Resolution(ctx, project, target)
	if err != nil {
		return nil, err
	}

	durationSeconds, err := d.resolveVideoDuration(task, project, registryProviderID, modelName, supportedDurations, resolution)
	if err != nil {
		return nil, err
	}
	if err := assertDurationSupported(durationSeconds, supportedDurations); err != nil {
		return nil, err
	}

	endImage, err := resolveEndFrameImage(projectDir, item, task.ResourceID)
	if err != nil {
		return nil, err
	}

	out, err := d.videos.Generate(ctx, target, VideoGenerateParams{
		ProjectName:     task.ProjectName,
		ProjectDir:      projectDir,
		UserID:          task.UserID,
		TaskID:          task.TaskID,
		ResourceType:    "videos",
		ResourceID:      task.ResourceID,
		Prompt:          promptText,
		StartImage:      storyboardFile,
		EndImage:        endImage,
		AspectRatio:     aspectRatio,
		DurationSeconds: durationSeconds,
		Resolution:      resolution,
		Seed:            seed,
		ServiceTier:     serviceTier,
	})
	if err != nil {
		return nil, err
	}

	return d.finalizeVideoTask(task, projectDir, scriptFile, out)
}

// dramaVoiceCharacters 取戏剧 Voice_Profiles 注入的角色表：无声（模型不产音或本集关音频）返回 nil。
func (d *TaskDispatcher) dramaVoiceCharacters(
	ctx context.Context, project *domain.ProjectFile, target *VideoTarget,
) *domain.AssetBucket {
	requested, err := d.videos.RequestedGenerateAudio(ctx, project)
	if err != nil {
		// 开关解析失败按 Python 侧独立解析失败的降级口径处理：有信号才判定为真无声。
		requested = true
	}
	if voiceConsistencyForCaps(target.Caps(), target.ProviderModel, project.GenerationMode) == "none" || !requested {
		return nil
	}
	return &project.Characters
}

// resolveVideoDuration 解析申请的时长：payload > project.default_duration > 收窄后档位首项。
//
// 显式指定的时长（payload 真值）不做「取首项」回退，直接交给时长守卫：安全解析与合法性由
// 守卫统一把关。payload 显式为假值（0 / "" 等）时按未设置处理，回退到项目默认；项目默认同样
// 为假值时取收窄后档位首项。
func (d *TaskDispatcher) resolveVideoDuration(
	task *repository.TaskData, project *domain.ProjectFile,
	registryProviderID, modelName string, supportedDurations []int, resolution string,
) (int, error) {
	if raw, present := task.Payload["duration_seconds"]; present && raw != nil && refPresenceTruthy(raw) {
		return videoDurationSeconds(raw)
	}
	if project.DefaultDuration != nil && *project.DefaultDuration != 0 {
		return *project.DefaultDuration, nil
	}
	// 取首项前先按当前分辨率的联动约束收窄：否则默认（Auto）设置会取到 backend 在该分辨率下
	// 必然拒绝的秒数。显式指定的时长不经此收窄，其合法性由 assertDurationSupported 与 backend
	// 的执行期校验把关。
	candidates := constrainVideoDurations(registryProviderID, modelName, supportedDurations, resolution)
	if len(candidates) == 0 {
		return 0, provider.NewVideoCapabilityError(provider.FailureVideoCapabilitiesUnresolved, map[string]any{
			"name": registryProviderID + "/" + modelName,
		})
	}
	return candidates[0], nil
}

// finalizeVideoTask 是 normal + resume 共用的 finalize：写 scene 资产 + 抽缩略图 + 返回 result dict。
func (d *TaskDispatcher) finalizeVideoTask(
	task *repository.TaskData, projectDir, scriptFile string, out VideoGenerateResult,
) (map[string]any, error) {
	clipRel := "videos/scene_" + task.ResourceID + ".mp4"
	if err := d.writeScriptGeneratedAsset(task.ProjectName, scriptFile, task.ResourceID, "video_clip", clipRel); err != nil {
		return nil, err
	}
	if out.VideoURI != "" {
		if err := d.writeScriptGeneratedAsset(task.ProjectName, scriptFile, task.ResourceID, "video_uri", out.VideoURI); err != nil {
			return nil, err
		}
	}

	videoFile := filepath.Join(projectDir, "videos", "scene_"+task.ResourceID+".mp4")
	thumbRel := "thumbnails/scene_" + task.ResourceID + ".jpg"
	thumbFile := filepath.Join(projectDir, filepath.FromSlash(thumbRel))
	if ffmpeg.MakeThumbnail(context.Background(), videoFile, thumbFile) {
		if err := d.writeScriptGeneratedAsset(task.ProjectName, scriptFile, task.ResourceID, "video_thumbnail", thumbRel); err != nil {
			return nil, err
		}
	} else {
		_ = os.Remove(thumbFile)
	}

	createdAt, err := NewVersions(projectDir).LatestCreatedAt("videos", task.ResourceID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"version":       out.Version,
		"file_path":     clipRel,
		"created_at":    createdAt,
		"resource_type": "videos",
		"resource_id":   task.ResourceID,
		"video_uri":     out.VideoURI,
	}, nil
}

// normalizeVideoPrompt 归一化视频 prompt 并在末尾追加统一文本化的反向提示词（对应
// generation_tasks.py::_normalize_video_prompt）。
func normalizeVideoPrompt(raw any) (string, error) {
	switch p := raw.(type) {
	case string:
		if strings.TrimSpace(p) == "" {
			return "", fmt.Errorf("prompt must not be empty")
		}
		return appendVideoNegativeTail(p), nil
	case map[string]any:
		if !IsStructuredVideoPrompt(p) {
			return "", fmt.Errorf("prompt must be a string or include action/camera_motion")
		}
		action, _ := p["action"].(string)
		if strings.TrimSpace(action) == "" {
			return "", fmt.Errorf("prompt.action must not be empty")
		}
		dialogueRaw, hasDialogue := p["dialogue"]
		if hasDialogue && dialogueRaw != nil {
			if _, ok := dialogueRaw.([]any); !ok {
				return "", fmt.Errorf("prompt.dialogue must be an array")
			}
		}
		normalized := map[string]any{
			"action":         strings.TrimSpace(action),
			"camera_motion":  stringOr(p["camera_motion"], ""),
			"ambiance_audio": stringOr(p["ambiance_audio"], ""),
			"dialogue":       normalizeDialogue(dialogueRaw),
			"voice_profiles": voiceProfilesValue(p["voice_profiles"]),
		}
		if normalized["camera_motion"] == "" {
			normalized["camera_motion"] = "Static"
		}
		body, err := VideoPromptToYAML(normalized)
		if err != nil {
			return "", err
		}
		return appendVideoNegativeTail(body), nil
	default:
		return "", fmt.Errorf("prompt must be a string or object")
	}
}

// normalizeDialogue 归一化 dialogue 数组：保留 speaker 或 line 非空（strip 后）的对象条目。
func normalizeDialogue(raw any) []any {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		speaker := strings.TrimSpace(stringOr(m["speaker"], ""))
		line := strings.TrimSpace(stringOr(m["line"], ""))
		if speaker != "" || line != "" {
			out = append(out, map[string]any{"speaker": speaker, "line": line})
		}
	}
	return out
}

// voiceProfilesValue 读 voice_profiles 字段；非列表按空列表处理（对应 Python `or []`）。
func voiceProfilesValue(raw any) []any {
	if list, ok := raw.([]any); ok {
		return list
	}
	return []any{}
}

// resolveEndFrameImage 解析镜头持久属性 end_frame_image 快照，返回经校验的绝对路径（未设置返回空串）。
//
// 剧本是磁盘上的 JSON，字段值不可信任：只接受「当前镜头自己的」end_frames/ 快照——不是随便
// 一个存在的 end_frames/ 内文件，字段指向别的镜头会静默生成/扣费错镜头的尾帧。裸文件名（无路径
// 分隔符）按校验侧 default_dir 回退口径补 end_frames/ 前缀重试。
func resolveEndFrameImage(projectDir string, item map[string]any, resourceID string) (string, error) {
	if item == nil {
		return "", nil
	}
	raw, present := item["end_frame_image"]
	if !present || raw == nil {
		return "", nil
	}
	// 只把 nil / "" 视为「未设置」；其余 falsy 脏数据必须继续走下面的硬失败，不被静默吞成未设置。
	rel, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("invalid end frame snapshot path: %v", raw)
	}
	if rel == "" {
		return "", nil
	}
	normalized := strings.ReplaceAll(strings.TrimSpace(rel), "\\", "/")
	candidate := normalized
	if !strings.Contains(normalized, "/") {
		candidate = "end_frames/" + normalized
	}
	expectedRel, err := resourceRelativePath("end_frames", resourceID)
	if err != nil {
		return "", err
	}
	// try_safe_join / safe_join 都走 realpath，会展开符号链接：若字段值恰是当前镜头的 canonical
	// 相对路径，但磁盘上那个位置被替换成指向别处（如另一镜头快照、分镜图）的符号链接，两次解析
	// 会算出同一个被展开的真实目标，让相等比较失去意义。这里逐段检查 canonical 路径每个组件
	// （文件名与父目录），挡住「路径字符串正确但磁盘对象被调包」。边界：Windows 目录联接不置
	// ModeSymlink，本检查在 Windows 上只覆盖符号链接。
	endFrameFile := fs.TrySafeJoin(projectDir, false, filepath.FromSlash(candidate))
	expectedFile, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(expectedRel))
	if err != nil {
		return "", fmt.Errorf("invalid end frame snapshot path: %v", raw)
	}
	if endFrameFile == "" || endFrameFile != expectedFile || canonicalPathTampered(projectDir, expectedRel) {
		return "", fmt.Errorf("invalid end frame snapshot path: %v", raw)
	}
	if !pathIsFile(endFrameFile) {
		return "", fmt.Errorf("end frame snapshot not found: %s", filepath.Base(endFrameFile))
	}
	return endFrameFile, nil
}

// canonicalPathTampered 逐段检查 canonical 相对路径上的每个组件是否为符号链接。
func canonicalPathTampered(projectDir, rel string) bool {
	current := projectDir
	for _, component := range strings.Split(filepath.ToSlash(rel), "/") {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// scriptContentMode 取剧本内容模式，缺失时回退项目配置，最终默认 narration
// （对应 resolve_content_mode：键存在即取该值，即便为空串）。
func scriptContentMode(script map[string]any, project *domain.ProjectFile) string {
	if mode, present := script["content_mode"]; present {
		if s, ok := mode.(string); ok {
			return s
		}
	}
	if project != nil && project.ContentMode != "" {
		return project.ContentMode
	}
	return string(domain.ContentNarration)
}

// payloadInt64 读 payload 里的整数（JSON 解出为 float64），非整数/非数值返回 nil。
func payloadInt64(payload map[string]any, key string) *int64 {
	switch v := payload[key].(type) {
	case float64:
		if v != float64(int64(v)) {
			return nil
		}
		out := int64(v)
		return &out
	case int64:
		out := v
		return &out
	case int:
		out := int64(v)
		return &out
	}
	return nil
}

// payloadNestedString 读 payload[key][subKey] 的字符串值。
func payloadNestedString(payload map[string]any, key, subKey string) string {
	nested, ok := payload[key].(map[string]any)
	if !ok {
		return ""
	}
	return stringOr(nested[subKey], "")
}
