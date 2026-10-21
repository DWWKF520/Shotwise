// 生成任务派发（对应 server/services/generation_tasks.py 的 _TASK_EXECUTORS）：把 worker
// 认领的任务按 task_type 路由到具体生成路径，产出写回 task.result_json 的结果字典。
//
// 迁移边界（fail loud，不静默降级）：
//   - storyboard 已完整接线（prompt 归一化 + 参考直传 + 记账 + 版本 + result 契约）；
//     剧本条目引用收集与 scene 资产写回依赖剧本域（get_storyboard_items），随剧本域迁移补齐。
//   - design 任务的 scene / prop / product 已接线（tasks_design.go）；character 已接线
//     （tasks_character.go，头像裁剪走收敛移植，无人脸检测）。
//   - tts / voice_sample 已接线（tasks_tts.go / tasks_voice_sample.go）；
//     image_edit 已接线（tasks_image_edit.go）；grid 已接线（tasks_grid.go）。
//   - video / reference_video 已接线（tasks_video.go / tasks_reference_video.go）。
//   - canvas_image_* 已接线（tasks_canvas.go）：split / crop 确定性几何，其余八个走
//     项目 i2i（redraw / erase 贴片合成回全图，outpaint 源图回贴画布）。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/queue"
	"github.com/shotwise/shotwise/internal/repository"
)

// TaskDispatcher 实现 queue.TaskExecutor。
type TaskDispatcher struct {
	store  *ProjectStore
	images *ImageGenerator
	audio  *AudioGenerator
	videos *VideoGenerator
}

// NewTaskDispatcher 构造；images 为 nil 时只能处理无需生成的任务类型（当前没有）。
func NewTaskDispatcher(store *ProjectStore, images *ImageGenerator) *TaskDispatcher {
	return &TaskDispatcher{store: store, images: images}
}

// 编译期契约：派发器可直接作为 worker 的执行层注入（queue.NewWorker 的 TaskExecutor 形参）。
var _ queue.TaskExecutor = (*TaskDispatcher)(nil)

// SetVideoGenerator 注入视频生成器；未注入时 video / reference_video 任务 fail loud。
func (d *TaskDispatcher) SetVideoGenerator(videos *VideoGenerator) { d.videos = videos }

// Execute 派发一个已认领任务；返回值写回 task.result_json。
func (d *TaskDispatcher) Execute(ctx context.Context, task *repository.TaskData) (map[string]any, error) {
	if task.ProjectName == "" {
		return nil, fmt.Errorf("task.project_name is required")
	}
	switch task.TaskType {
	case "storyboard":
		return d.executeStoryboard(ctx, task)
	case string(domain.AssetScene), string(domain.AssetProp), string(domain.AssetProduct):
		return d.executeDesign(ctx, task, domain.AssetKind(task.TaskType))
	case string(domain.AssetCharacter):
		return d.executeCharacterTask(ctx, task)
	case "tts":
		return d.executeTTS(ctx, task)
	case "voice_sample":
		return d.executeVoiceSampleTask(ctx, task)
	case "image_edit":
		return d.executeImageEdit(ctx, task)
	case "grid":
		return d.executeGrid(ctx, task)
	case "video":
		return d.executeVideoTask(ctx, task)
	case "reference_video":
		return d.executeReferenceVideoTask(ctx, task)
	case "canvas_image_split", "canvas_image_panorama", "canvas_image_angles", "canvas_image_layers",
		"canvas_image_hd", "canvas_image_outpaint", "canvas_image_redraw", "canvas_image_erase",
		"canvas_image_cutout", "canvas_image_crop":
		return d.executeCanvasImageTask(ctx, task)
	default:
		return nil, fmt.Errorf("unsupported task_type: %s", task.TaskType)
	}
}

// executeStoryboard 执行分镜图生成。
//
// payload 契约（与 Python 侧入队载荷一致）：prompt（string 或 {scene, composition} 结构）、
// script_file、extra_reference_images（项目内相对路径列表）。参考收集当前只吃直传列表；
// 剧本条目（角色/场景/道具 sheet）引用与生成后的 scene 资产写回待剧本域迁移补齐。
func (d *TaskDispatcher) executeStoryboard(ctx context.Context, task *repository.TaskData) (map[string]any, error) {
	rawPrompt, ok := task.Payload["prompt"]
	if !ok || rawPrompt == nil {
		return nil, fmt.Errorf("prompt is required for storyboard task")
	}
	project, err := d.store.Get(task.ProjectName)
	if err != nil {
		return nil, err
	}
	projectDir, err := d.store.Dir(task.ProjectName)
	if err != nil {
		return nil, err
	}

	promptText, err := normalizeStoryboardPrompt(rawPrompt, project.Style)
	if err != nil {
		return nil, err
	}
	refs, err := collectPayloadReferenceImages(projectDir, task.Payload)
	if err != nil {
		return nil, err
	}

	outRel, err := resourceRelativePath("storyboards", task.ResourceID)
	if err != nil {
		return nil, err
	}
	outPath, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(outRel))
	if err != nil {
		return nil, fmt.Errorf("非法资源 ID: %s", task.ResourceID)
	}

	request := provider.NewImageGenerationRequest(promptText, outPath)
	request.ReferenceImages = refs
	request.AspectRatio = aspectRatioFor(project, "storyboards")
	if size := payloadString(task.Payload, "image_size"); size != "" {
		request.ImageSize = size
	}
	request.ProjectName = task.ProjectName

	capability := config.ImageCapT2I
	if len(refs) > 0 {
		capability = config.ImageCapI2I
	}
	if _, err := d.images.Generate(ctx, ImageGenerateParams{
		ProjectName: task.ProjectName,
		Project:     project,
		Capability:  capability,
		Request:     request,
		UserID:      task.UserID,
		SegmentID:   segmentIDFor("image", "storyboards", task.ResourceID),
	}); err != nil {
		return nil, err
	}

	return finishImageResult(projectDir, "storyboards", task.ResourceID, promptText, request.AspectRatio)
}

// finishImageResult 登记版本并组装 result 契约（对应各 execute_*_task 的返回字典）。
func finishImageResult(projectDir, resourceType, resourceID, prompt, aspectRatio string) (map[string]any, error) {
	outRel, err := resourceRelativePath(resourceType, resourceID)
	if err != nil {
		return nil, err
	}
	// 版本登记把生成产物复制进 versions/ 快照；created_at 取刚写入的这条。
	versions := NewVersions(projectDir)
	absolute := filepath.Join(projectDir, filepath.FromSlash(outRel))
	if _, _, err := versions.AddVersion(resourceType, resourceID, prompt, absolute, map[string]any{
		"aspect_ratio": aspectRatio,
	}); err != nil {
		return nil, err
	}
	createdAt, err := versions.LatestCreatedAt(resourceType, resourceID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"file_path":     outRel,
		"created_at":    createdAt,
		"resource_type": resourceType,
		"resource_id":   resourceID,
	}, nil
}

// segmentIDFor 按记账通道白名单判定 segment_id（对应 lib/media_generator.py 同名函数）。
func segmentIDFor(callType, resourceType, resourceID string) string {
	switch callType {
	case "audio":
		return resourceID
	case "image":
		if resourceType == "storyboards" || resourceType == "videos" || resourceType == "grids" {
			return resourceID
		}
	case "video":
		if resourceType == "storyboards" || resourceType == "videos" || resourceType == "reference_videos" {
			return resourceID
		}
	default:
		return ""
	}
	return ""
}

// collectPayloadReferenceImages 解析 payload["extra_reference_images"]：项目内相对路径
// 逐项 SafeJoin，越界或不存在（含指向目录）的条目跳过——与 Python 侧 try_safe_join +
// is_file 的容错口径一致。
func collectPayloadReferenceImages(projectDir string, payload map[string]any) ([]provider.ReferenceImage, error) {
	raw, ok := payload["extra_reference_images"]
	if !ok || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("extra_reference_images must be a list")
	}
	var refs []provider.ReferenceImage
	for _, item := range list {
		path, ok := item.(string)
		if !ok || strings.TrimSpace(path) == "" {
			continue
		}
		full := fs.TrySafeJoin(projectDir, false, filepath.FromSlash(strings.TrimSpace(path)))
		if full == "" {
			continue
		}
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			continue
		}
		refs = append(refs, provider.ReferenceImage{Path: full})
	}
	return refs, nil
}

// aspectRatioFor 移植 server/services/generation_tasks.py::get_aspect_ratio：
// 角色与多视图资产固定横版；其余读项目顶层字段（字符串或按资源类型分字典），
// 缺失时按 content_mode 推导（narration/ad 竖屏，drama 横屏）。
func aspectRatioFor(project *domain.ProjectFile, resourceType string) string {
	if resourceType == "characters" || resourceType == "scenes" || resourceType == "props" || resourceType == "products" {
		return "16:9"
	}
	if project.AspectRatio != "" {
		return project.AspectRatio
	}
	if project.ContentMode == string(domain.ContentNarration) || project.ContentMode == string(domain.ContentAd) {
		return "9:16"
	}
	return "16:9"
}

// normalizeStoryboardPrompt 归一化分镜图 prompt：正文组装 + 统一反向提示词尾巴（尾巴幂等）。
func normalizeStoryboardPrompt(raw any, style string) (string, error) {
	body, err := BuildImagePromptBody(raw, style)
	if err != nil {
		return "", err
	}
	return appendImageNegativeTail(body), nil
}

// BuildImagePromptBody 返回不含风格前缀、不含反向尾巴的提示词正文。
//
// 结构化对象转 YAML（Style/Scene/Composition/Avoid 键序）；纯字符串原样返回。工具层与
// worker 层共用这一份正文组装，避免入队时与执行时的提示词口径分叉。
func BuildImagePromptBody(raw any, style string) (string, error) {
	switch p := raw.(type) {
	case string:
		if strings.TrimSpace(p) == "" {
			return "", fmt.Errorf("prompt must not be empty")
		}
		return p, nil
	case map[string]any:
		scene, _ := p["scene"].(string)
		if strings.TrimSpace(scene) == "" {
			return "", fmt.Errorf("prompt must be a string or include scene/composition")
		}
		composition, _ := p["composition"].(map[string]any)
		normalized := map[string]string{
			"scene":     strings.TrimSpace(scene),
			"shot_type": stringOr(composition["shot_type"], "Medium Shot"),
			"lighting":  stringOr(composition["lighting"], ""),
			"ambiance":  stringOr(composition["ambiance"], ""),
		}
		return imagePromptToYAML(normalized, style), nil
	default:
		return "", fmt.Errorf("prompt must be a string or object")
	}
}

// BuildImagePromptText 按 Python sdk_tools/enqueue_storyboards.py::_build_prompt 的口径组装入队提示词：
// 结构化 prompt 的 style 已写进 YAML 的 Style 字段，故不再重复加 "Style:" 前缀；非结构化（纯字符串）
// prompt 不含 Style，前缀补上。反向尾巴由执行层追加，此处不加。
func BuildImagePromptText(raw any, style, styleDescription string) (string, error) {
	structured := false
	if m, ok := raw.(map[string]any); ok {
		_, structured = m["scene"]
	}
	var parts []string
	normalizedStyle := normalizeStylePrefix(style)
	if normalizedStyle != "" && !structured {
		parts = append(parts, "Style: "+normalizedStyle)
	}
	if strings.TrimSpace(styleDescription) != "" {
		parts = append(parts, "Visual style: "+strings.TrimSpace(styleDescription))
	}
	prefix := ""
	if len(parts) > 0 {
		prefix = strings.Join(parts, "\n") + "\n\n"
	}
	body, err := BuildImagePromptBody(raw, normalizedStyle)
	if err != nil {
		return "", err
	}
	return prefix + body, nil
}

// promptYAMLScalar 输出单个 YAML 纯量：取值安全（无结构歧义字符）时保持明文，
// 否则用 JSON 双引号串——YAML 的双引号纯量与 JSON 兼容，等价 PyYAML 的转义行为。
func promptYAMLScalar(value string) string {
	if value == "" {
		return `""`
	}
	unsafe := strings.Contains(value, ": ") || strings.Contains(value, " #") ||
		value != strings.TrimSpace(value) || strings.ContainsAny(value, "\n\r\t") ||
		strings.HasPrefix(value, "-") || strings.HasPrefix(value, "?") ||
		strings.HasPrefix(value, "&") || strings.HasPrefix(value, "*") ||
		strings.HasPrefix(value, "!") || strings.HasPrefix(value, "|") ||
		strings.HasPrefix(value, ">") || strings.HasPrefix(value, "%") ||
		strings.HasPrefix(value, "@") || strings.HasPrefix(value, "`") ||
		strings.HasPrefix(value, "'") || strings.HasPrefix(value, "\"") ||
		strings.HasPrefix(value, "[") || strings.HasPrefix(value, "{")
	if !unsafe {
		return value
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

// imagePromptToYAML 把 imagePrompt 结构渲染为 YAML，键序 Style/Scene/Composition/Avoid
// （对应 lib/prompt_utils.py::image_prompt_to_yaml；PyYAML 宽行不折行，此处逐行手写同形）。
func imagePromptToYAML(p map[string]string, projectStyle string) string {
	var b strings.Builder
	b.WriteString("Style: " + promptYAMLScalar(normalizeStylePrefix(projectStyle)) + "\n")
	b.WriteString("Scene: " + promptYAMLScalar(p["scene"]) + "\n")
	b.WriteString("Composition:\n")
	b.WriteString("  shot_type: " + promptYAMLScalar(p["shot_type"]) + "\n")
	b.WriteString("  lighting: " + promptYAMLScalar(p["lighting"]) + "\n")
	b.WriteString("  ambiance: " + promptYAMLScalar(p["ambiance"]) + "\n")
	b.WriteString("Avoid: " + promptYAMLScalar(storyboardAvoidItems) + "\n")
	return b.String()
}

// appendImageNegativeTail 给分镜图 prompt 追加统一反向提示词（幂等）。
func appendImageNegativeTail(prompt string) string {
	if strings.TrimSpace(prompt) == "" {
		return storyboardNegativeTail
	}
	if strings.Contains(prompt, storyboardNegativeTail) {
		return prompt
	}
	return strings.TrimRight(prompt, " \t\n\r") + "\n\n" + storyboardNegativeTail
}

func stringOr(value any, fallback string) string {
	if s, ok := value.(string); ok && s != "" {
		return s
	}
	return fallback
}

// payloadString 读 payload 里的字符串字段，非字符串或空串返回空。
func payloadString(payload map[string]any, key string) string {
	if v, ok := payload[key].(string); ok {
		return v
	}
	return ""
}

// ---------------------------------------------------------------------------
// prompt 构造常量与风格归一化（对应 lib/prompt_utils.py / lib/prompt_builders.py 的
// 分镜图相关面；资产图 build_*_prompt 随 design 任务迁移一并补齐）。
// ---------------------------------------------------------------------------

// 反向提示词：分镜图与视频各自定义，内容相同也不合并（同 Python 侧口径）。
const (
	storyboardAvoidItems   = "水印、多余文字、Logo"
	videoAvoidItems        = "BGM、文字字幕、水印"
	storyboardNegativeTail = "Avoid: " + storyboardAvoidItems
	videoNegativeTail      = "Avoid: " + videoAvoidItems
)

// appendVideoNegativeTail 给视频 prompt 追加统一反向提示词（幂等）。
func appendVideoNegativeTail(prompt string) string {
	if strings.TrimSpace(prompt) == "" {
		return videoNegativeTail
	}
	if strings.Contains(prompt, videoNegativeTail) {
		return prompt
	}
	return strings.TrimRight(prompt, " \t\n\r") + "\n\n" + videoNegativeTail
}

// normalizeStylePrefix 去掉风格值开头的「画风：」前缀并 strip 两端空白（幂等）。
// 存量项目的 style 取自旧版风格模版，不清理会渲染成 Style: 画风：... 的中英混叠。
func normalizeStylePrefix(style string) string {
	s := strings.TrimSpace(style)
	for _, prefix := range []string{"画风：", "画风:"} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
			break
		}
	}
	return s
}
