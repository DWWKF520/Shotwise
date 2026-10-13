package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/provider"
)

// klingVideoBackend 是可灵 Kling 视频生成后端（JWT 直连 / Bearer 中转双模式，异步轮询）。
//
// 走可灵原生视频端点：submit POST /v1/videos/{text2video|image2video} 取 data.task_id →
// 轮询 GET /v1/videos/{subpath}/{task_id} 至 task_status=succeed 取 task_result.videos[0].url →
// 下载本地。3.0 系模型另走官方根路径（modern/）。鉴权 / base_url 装配 / submit-poll 骨架由
// klingBackendBase 共享；本类型只填视频侧差异：子路径派生、能力位查表、resume 与下载。

const (
	klingVideoDefaultModel = "kling-v2-5-turbo"

	klingText2Video        = "text2video"
	klingImage2Video       = "image2video"
	klingMultiImage2Video  = "multi-image2video"
	klingModern            = "modern"
	klingModernRoutePrefix = "modern/"

	// klingR2VMaxReferenceImages 多图主体（R2V）参考图上限；官方未明确列出，取保守值。
	klingR2VMaxReferenceImages = 4

	klingMinPollTimeoutSeconds = 900
	klingPollTimeoutPerSecond  = 60
	klingVideoPollInterval     = 10 * time.Second
)

// klingResumableSubpaths 可续跑 job_id 里编入的生成类型子路径。
var klingResumableSubpaths = map[string]struct{}{
	klingText2Video:       {},
	klingImage2Video:      {},
	klingMultiImage2Video: {},
	klingModern:           {},
}

// klingVideoModelCaps 单个可灵视频模型的能力位。
type klingVideoModelCaps struct {
	// textToVideo 文生视频闸：为假的 model 不接受无首帧的请求（buildPayload 处拒）。
	textToVideo bool
	lastFrame   bool
	// lastFrameRequiresPro：last_frame=True 但仅 pro 档可用（v2-5-turbo、v2-6 首尾帧均标「仅 pro」）。
	// std 档提交 image_tail 请求体虽会被受理，尾帧约束却不生效——buildPayload 按此位在 std 档拒绝。
	lastFrameRequiresPro bool
	referenceImages      bool
	maxReferenceImages   int
	// generateAudio：能产出视频内人声；该位同时决定请求体是否携带音频开关 sound。
	generateAudio bool
	// audioRequires1080p：有声仅在 pro（即 1080P）档可用（官方 v2-6 约束）。可灵请求体没有分辨率
	// 字段——输出档位只由 mode 决定，故执行期判据落在 mode 上。
	audioRequires1080p bool
}

// klingDefaultVideoCaps turbo / 未登记 model 兜底：文/图生视频、首尾帧，无音频/参考。
var klingDefaultVideoCaps = klingVideoModelCaps{
	textToVideo:          true,
	lastFrame:            true,
	lastFrameRequiresPro: true,
}

// klingVideoCapsByModel 各视频模型能力位（官方一手核实）。
var klingVideoCapsByModel = map[string]klingVideoModelCaps{
	"kling-v2-5-turbo": klingDefaultVideoCaps,
	"kling-v3": {
		textToVideo:   true,
		lastFrame:     true,
		generateAudio: true,
	},
	"kling-v3-omni": {
		textToVideo:        true,
		lastFrame:          true,
		referenceImages:    true,
		maxReferenceImages: klingR2VMaxReferenceImages,
		generateAudio:      true,
	},
	"kling-v3-turbo": {
		textToVideo: true,
	},
	"kling-3.0-turbo": {
		textToVideo: true,
	},
	"kling-v2-6": {
		textToVideo:          true,
		lastFrame:            true,
		lastFrameRequiresPro: true,
		generateAudio:        true,
		audioRequires1080p:   true,
	},
	"kling-video-o1": {
		lastFrame:          true,
		referenceImages:    true,
		maxReferenceImages: klingR2VMaxReferenceImages,
	},
}

// klingVideoCapsAliases 官方 API 别名 → UI model key。
var klingVideoCapsAliases = map[string]string{
	"kling-3.0":      "kling-v3",
	"kling-3.0-omni": "kling-v3-omni",
}

// klingModelKey 剥厂商前缀：把 ":" 统一成 "/" 后取最后一段 + 去首尾空白 + lower。
//
// 刻意不把 "_" / "." 当分隔符——它们是 model 名合法字符（kling-v3-omni、image-01 都含）。
func klingModelKey(model string) string {
	normalized := strings.ReplaceAll(model, ":", "/")
	parts := strings.Split(normalized, "/")
	return strings.ToLower(strings.TrimSpace(parts[len(parts)-1]))
}

// klingLookupVideoCaps 按 model 取能力位；未登记 model 回落保守默认，绝不按子串猜上限。
func klingLookupVideoCaps(model string) klingVideoModelCaps {
	key := klingModelKey(model)
	if alias, ok := klingVideoCapsAliases[key]; ok {
		key = alias
	}
	if caps, ok := klingVideoCapsByModel[key]; ok {
		return caps
	}
	return klingDefaultVideoCaps
}

// klingVideoCapabilitiesForModel 纯查表声明某 model 的视频能力（不构造实例、无需 api_key）。
//
// first_frame 恒真；last_frame / 参考图上限按 model 读表。last_frame_requires_pro 为真的 model
// 在此保守声明 last_frame=False（无请求档位上下文，无法判断调用方将选哪档），有档位上下文的调用方
// 走 VideoCapabilitiesForTier。
func klingVideoCapabilitiesForModel(modelID string) (provider.VideoCapabilities, error) {
	caps := klingLookupVideoCaps(modelID)
	capsOut := provider.DefaultVideoCapabilities()
	capsOut.LastFrame = caps.lastFrame && !caps.lastFrameRequiresPro
	capsOut.MaxReferenceImages = caps.maxReferenceImages
	return capsOut, nil
}

// klingEffectiveGenerateAudioForModel 默认执行档下真正生效的音频计价参数。
//
// 有声受 1080P 约束的 model（v2-6）无从得知调用方将选哪档质量档，保守返回 false；无约束的有声
// model（v3 / v3-omni）返回 true（方向为高估成本，不低报）。
func klingEffectiveGenerateAudioForModel(modelID string) (bool, error) {
	caps := klingLookupVideoCaps(modelID)
	return caps.generateAudio && !caps.audioRequires1080p, nil
}

// klingVideoBackend 见文件头注释。
type klingVideoBackend struct {
	*klingBackendBase
	apiModelName string
	caps         klingVideoModelCaps
}

// newKlingVideoBackend 按通用信封构造；model / api_model_name 留空时回落到默认模型。
func newKlingVideoBackend(opts provider.BackendOptions) (*klingVideoBackend, error) {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = klingVideoDefaultModel
	}
	base, err := newKlingBackendBase(opts, model)
	if err != nil {
		return nil, err
	}
	base.mediaLabel = "视频"
	apiModelName := strings.TrimSpace(opts.Extra["api_model_name"])
	if apiModelName == "" {
		apiModelName = model
	}
	b := &klingVideoBackend{
		klingBackendBase: base,
		apiModelName:     apiModelName,
		// 按 model 取能力位（归一化前缀 / 大小写后精确命中）；未登记 model 回落保守默认。
		caps: klingLookupVideoCaps(apiModelName),
	}
	// 3.0 系端点走官方根路径：经 resolveURL 钩子覆写，不改鉴权与重试骨架。
	b.resolveURL = b.videoRequestURL
	return b, nil
}

// VideoCapabilities 返回本实例的静态能力声明（按 registry 键名 model）。
func (b *klingVideoBackend) VideoCapabilities() provider.VideoCapabilities {
	caps, _ := klingVideoCapabilitiesForModel(b.model)
	return caps
}

// VideoCapabilitiesForTier 按实际请求档位收窄 last_frame 声明。
//
// 实际解析出的 mode（4k 优先于 service_tier）为 pro 才放行；std/4k 档仍保守拒绝，与 buildPayload
// 的 fail-loud 护栏放行条件对齐。
func (b *klingVideoBackend) VideoCapabilitiesForTier(serviceTier string, resolution string) provider.VideoCapabilities {
	caps := klingLookupVideoCaps(b.model)
	mode := klingResolveModeFrom(resolution, serviceTier)
	capsOut := provider.DefaultVideoCapabilities()
	capsOut.LastFrame = caps.lastFrame && (!caps.lastFrameRequiresPro || mode == "pro")
	capsOut.MaxReferenceImages = caps.maxReferenceImages
	return capsOut
}

// ── 端点 / 模型名派生 ────────────────────────────────────────────────────

// modernModelName 3.0 系官方路由使用的模型名；非 3.0 系回空串。
func (b *klingVideoBackend) modernModelName() string {
	key := klingModelKey(b.apiModelName)
	switch key {
	case "kling-v3", "kling-3.0":
		return "kling-3.0"
	case "kling-v3-omni":
		return "kling-3.0-omni"
	case "kling-v3-turbo", "kling-3.0-turbo":
		return "kling-3.0-turbo"
	}
	return ""
}

// isModernModel 是否走 3.0 系官方根路径。
func (b *klingVideoBackend) isModernModel() bool { return b.modernModelName() != "" }

// modernBaseURL 官方根路径：剥掉结尾的 /v1。
func (b *klingVideoBackend) modernBaseURL() string {
	base := strings.TrimRight(b.baseURL, "/")
	if strings.HasSuffix(strings.ToLower(base), "/v1") {
		return strings.TrimRight(base[:len(base)-3], "/")
	}
	return base
}

// videoRequestURL 覆写 resolveURL：modern/ 前缀走官方根路径，其余走默认 base_url。
func (b *klingVideoBackend) videoRequestURL(endpointPath string) string {
	if strings.HasPrefix(endpointPath, klingModernRoutePrefix) {
		rest := strings.TrimLeft(endpointPath[len(klingModernRoutePrefix):], "/")
		return b.modernBaseURL() + "/" + rest
	}
	return b.defaultRequestURL(endpointPath)
}

// modernStandardModelName 普通 v3 文本 / 图像路由使用的模型名（Omni 仅在带参考内容时走独有路由）。
func (b *klingVideoBackend) modernStandardModelName() string {
	modelName := b.modernModelName()
	if modelName == "kling-3.0-omni" {
		return "kling-3.0"
	}
	return modelName
}

// modernEndpoint 3.0 系端点路径（带 modern/ 前缀供 resolveURL 识别）。
func (b *klingVideoBackend) modernEndpoint(req provider.VideoGenerationRequest, hasReferences bool) (string, error) {
	modelName := b.modernModelName()
	if hasReferences {
		if modelName != "kling-3.0-omni" {
			return "", provider.NewVideoCapabilityError(provider.FailureVideoReferenceImagesUnsupported, map[string]any{
				"model": b.model,
			})
		}
		return klingModernRoutePrefix + "omni-video/" + modelName, nil
	}
	route := "text-to-video"
	if req.StartImage != "" {
		route = "image-to-video"
	}
	return klingModernRoutePrefix + route + "/" + b.modernStandardModelName(), nil
}

// modernResolution 3.0 系解析并校验分辨率档位。
func (b *klingVideoBackend) modernResolution(req provider.VideoGenerationRequest, modelName string, hasReferences bool) (string, error) {
	resolution := strings.ToLower(req.Resolution)
	if resolution == "" {
		resolution = "720p"
	}
	// 普通 v3 路由声明 4K；Omni 专属参考路由在官方明确声明前保守只放 720p/1080p。
	standardV3 := modelName == "kling-3.0" || (modelName == "kling-3.0-omni" && !hasReferences)
	supported := []string{"720p", "1080p"}
	if standardV3 {
		supported = []string{"720p", "1080p", "4k"}
	}
	for _, s := range supported {
		if resolution == s {
			return resolution, nil
		}
	}
	displayResolution := req.Resolution
	if displayResolution == "" {
		displayResolution = "720p"
	}
	sorted := append([]string(nil), supported...)
	sort.Strings(sorted)
	return "", provider.NewVideoCapabilityError(provider.FailureVideoResolutionDurationUnsupported, map[string]any{
		"model":      b.model,
		"resolution": displayResolution,
		"duration":   req.DurationSeconds,
		"supported":  strings.Join(sorted, ", "),
	})
}

// buildModernPayload 构建 3.0 系 contents/settings/options 请求体，返回 (端点路径, 请求体)。
func (b *klingVideoBackend) buildModernPayload(req provider.VideoGenerationRequest) (string, map[string]any, error) {
	modelName := b.modernModelName()
	references := klingValidFrames(req.ReferenceImages)
	startImage := req.StartImage
	endImage := req.EndImage
	if len(references) > 0 && modelName != "kling-3.0-omni" {
		return "", nil, provider.NewVideoCapabilityError(provider.FailureVideoReferenceImagesUnsupported, map[string]any{
			"model": b.model,
		})
	}
	if len(references) > 0 && (startImage != "" || endImage != "") {
		return "", nil, provider.NewVideoCapabilityError(provider.FailureVideoReferenceImagesWithFramesUnsupported, map[string]any{
			"model": b.model,
		})
	}
	if endImage != "" && startImage == "" {
		return "", nil, provider.NewVideoCapabilityError(provider.FailureVideoEndImageRequiresStartImage, map[string]any{
			"model": b.model,
		})
	}
	if endImage != "" && !b.caps.lastFrame {
		return "", nil, provider.NewVideoCapabilityError(provider.FailureVideoLastFrameUnsupported, map[string]any{
			"provider": b.name(),
			"model":    b.model,
		})
	}
	if startImage == "" && len(references) == 0 && !b.caps.textToVideo {
		return "", nil, provider.NewVideoCapabilityError(provider.FailureVideoCapabilityMissingT2V, map[string]any{
			"provider": b.name(),
			"model":    b.model,
		})
	}

	contents := []map[string]any{{"type": "prompt", "text": req.Prompt}}
	if startImage != "" {
		encoded, err := b.encodeFrame(startImage)
		if err != nil {
			return "", nil, err
		}
		contents = append(contents, map[string]any{"type": "first_frame", "url": encoded})
	}
	if endImage != "" {
		encoded, err := b.encodeFrame(endImage)
		if err != nil {
			return "", nil, err
		}
		contents = append(contents, map[string]any{"type": "last_frame", "url": encoded})
	}
	if len(references) > 0 {
		if len(references) > b.caps.maxReferenceImages {
			return "", nil, provider.NewVideoCapabilityError(provider.FailureVideoReferenceImagesExceeded, map[string]any{
				"model": b.model,
				"count": len(references),
				"limit": b.caps.maxReferenceImages,
			})
		}
		for _, path := range references {
			encoded, err := b.encodeFrame(path)
			if err != nil {
				return "", nil, err
			}
			contents = append(contents, map[string]any{"type": "refer_image", "url": encoded})
		}
	}

	resolution, err := b.modernResolution(req, modelName, len(references) > 0)
	if err != nil {
		return "", nil, err
	}
	settings := map[string]any{
		"resolution": resolution,
		"duration":   req.DurationSeconds,
	}
	if startImage == "" {
		// Omni 参考图不会被当作首帧；无首帧时官方要求显式提供宽高比。
		aspect := req.AspectRatio
		if aspect == "" {
			aspect = "9:16"
		}
		settings["aspect_ratio"] = aspect
	}
	if modelName != "kling-3.0-turbo" {
		audio := "off"
		if req.GenerateAudio && b.caps.generateAudio {
			audio = "native"
		}
		settings["audio"] = audio
		settings["multi_shot"] = false
	}
	payload := map[string]any{
		"contents": contents,
		"settings": settings,
		"options":  map[string]any{"watermark_info": map[string]any{"enabled": false}},
	}
	endpoint, err := b.modernEndpoint(req, len(references) > 0)
	if err != nil {
		return "", nil, err
	}
	return endpoint, payload, nil
}

// klingResolveModeFrom 质量档 → mode：resolution=4k 独立成 4k 档，否则 service_tier→std/pro。
//
// 与 per_second_tiered 定价的档位派生一致（4k 优先于 std/pro），保证请求档与计费档同源。
func klingResolveModeFrom(resolution, serviceTier string) string {
	if strings.ToLower(resolution) == "4k" {
		return "4k"
	}
	if strings.ToLower(serviceTier) == "pro" {
		return "pro"
	}
	return "std"
}

// resolveMode 按请求解析实际质量档。
func (b *klingVideoBackend) resolveMode(req provider.VideoGenerationRequest) string {
	return klingResolveModeFrom(req.Resolution, req.ServiceTier)
}

// effectiveAudio 实际是否产出视频内人声：请求要 + model 有 generate_audio 能力 + 走得到音频开关的
// 子路径 + 满足该 model 的分辨率约束。无能力的 model 恒 false。
func (b *klingVideoBackend) effectiveAudio(req provider.VideoGenerationRequest, subpath string) bool {
	if b.isModernModel() {
		return req.GenerateAudio && b.caps.generateAudio
	}
	if !(req.GenerateAudio && b.caps.generateAudio) {
		return false
	}
	// multi-image2video 原生 schema 不含音频开关，成片必然无声。
	if subpath == klingMultiImage2Video {
		return false
	}
	if b.caps.audioRequires1080p {
		// 官方约束维度是分辨率，但请求体没有分辨率字段：档位只由 mode 决定，判据落在实际发出的 mode。
		return b.resolveMode(req) == "pro"
	}
	return true
}

// klingValidFrames 过滤出非空参考图路径。
func klingValidFrames(images []string) []string {
	if len(images) == 0 {
		return nil
	}
	out := make([]string, 0, len(images))
	for _, img := range images {
		if img != "" {
			out = append(out, img)
		}
	}
	return out
}

// buildPayload 返回 (端点子路径, 请求体)，3.0 系与旧版协议并行。
func (b *klingVideoBackend) buildPayload(req provider.VideoGenerationRequest) (string, map[string]any, error) {
	if b.isModernModel() {
		return b.buildModernPayload(req)
	}

	payload := map[string]any{
		"model_name":   b.model,
		"prompt":       req.Prompt,
		"mode":         b.resolveMode(req),
		"duration":     fmt.Sprintf("%d", req.DurationSeconds),
		"aspect_ratio": req.AspectRatio,
	}

	references := klingValidFrames(req.ReferenceImages)
	if len(references) > 0 {
		// 生成时防御（fail-loud）：未声明多图主体能力的 model 不得升级到 R2V 子路径。
		if !b.caps.referenceImages {
			return "", nil, provider.NewVideoCapabilityError(provider.FailureVideoReferenceImagesUnsupported, map[string]any{
				"model": b.model,
			})
		}
		if len(references) > b.caps.maxReferenceImages {
			return "", nil, provider.NewVideoCapabilityError(provider.FailureVideoReferenceImagesExceeded, map[string]any{
				"model": b.model,
				"count": len(references),
				"limit": b.caps.maxReferenceImages,
			})
		}
		imageList := make([]map[string]any, 0, len(references))
		for _, path := range references {
			encoded, err := b.encodeFrame(path)
			if err != nil {
				return "", nil, err
			}
			imageList = append(imageList, map[string]any{"image": encoded})
		}
		payload["image_list"] = imageList
		return klingMultiImage2Video, payload, nil
	}

	subpath := klingText2Video
	if req.StartImage == "" {
		// 无首帧/无参考 = 文生视频意图；不支持 t2v 的 model（如 kling-video-o1）即拒绝。
		if !b.caps.textToVideo {
			return "", nil, provider.NewVideoCapabilityError(provider.FailureVideoCapabilityMissingT2V, map[string]any{
				"provider": b.name(),
				"model":    b.model,
			})
		}
	} else {
		encoded, err := b.encodeFrame(req.StartImage)
		if err != nil {
			return "", nil, err
		}
		payload["image"] = encoded
		if req.EndImage != "" {
			// 首尾帧仅 pro 档生效时，std/4k 档提交 image_tail 虽会被受理，尾帧约束却不生效——
			// fail loud 拒绝而非放行一个调用方以为已生效实则被忽略的请求。
			if b.caps.lastFrameRequiresPro && b.resolveMode(req) != "pro" {
				return "", nil, provider.NewVideoCapabilityError(provider.FailureVideoLastFrameRequiresPro, map[string]any{
					"provider": b.name(),
					"model":    b.model,
				})
			}
			encodedTail, err := b.encodeFrame(req.EndImage)
			if err != nil {
				return "", nil, err
			}
			payload["image_tail"] = encodedTail
		}
		subpath = klingImage2Video
	}

	// 音频开关 sound 仅 text2video / image2video 子路径携带；无音频能力的 model 不发该字段。
	if b.caps.generateAudio {
		if b.effectiveAudio(req, subpath) {
			payload["sound"] = "on"
		} else {
			payload["sound"] = "off"
		}
	}
	return subpath, payload, nil
}

// encodeFrame 本地帧图 → 纯 base64；缺失 / 不可读 fail-loud 中止。
func (b *klingVideoBackend) encodeFrame(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", provider.NewVideoCapabilityError(provider.FailureVideoStartImageUnreadable, map[string]any{
			"model": b.model,
			"name":  klingBaseName(path),
		})
	}
	encoded, err := klingImageToBase64(path)
	if err != nil {
		return "", provider.NewVideoCapabilityError(provider.FailureVideoStartImageUnreadable, map[string]any{
			"model": b.model,
			"name":  klingBaseName(path),
		})
	}
	return encoded, nil
}

// name 返回供应商标识（供错误参数使用）。
func (b *klingVideoBackend) name() string { return b.klingBackendBase.Name() }

// klingVideoSafeLogView 预脱敏标量视图：base64 帧图 / prompt 一律不展开。
func klingVideoSafeLogView(subpath string, payload map[string]any) []any {
	contents, _ := payload["contents"].([]map[string]any)
	modernPromptLen := 0
	modernReferenceCount := 0
	hasFirstFrame := false
	hasLastFrame := false
	for _, item := range contents {
		switch item["type"] {
		case "prompt":
			if text := klingAsStr(item["text"]); text != "" {
				modernPromptLen = len(text)
			}
		case "refer_image":
			modernReferenceCount++
		case "first_frame":
			hasFirstFrame = true
		case "last_frame":
			hasLastFrame = true
		}
	}
	settings := klingAsDict(payload["settings"])
	duration := payload["duration"]
	if duration == nil {
		duration = settings["duration"]
	}
	aspect := payload["aspect_ratio"]
	if aspect == nil {
		aspect = settings["aspect_ratio"]
	}
	sound := payload["sound"]
	if sound == nil {
		sound = settings["audio"]
	}
	referenceCount := modernReferenceCount
	if list, ok := payload["image_list"].([]map[string]any); ok {
		referenceCount = len(list)
	}
	promptLen := modernPromptLen
	if prompt := klingAsStr(payload["prompt"]); prompt != "" {
		promptLen = len(prompt)
	}
	_, hasImage := payload["image"]
	_, hasImageTail := payload["image_tail"]
	return []any{
		"endpoint", subpath,
		"model_name", payload["model_name"],
		"mode", payload["mode"],
		"duration", duration,
		"aspect_ratio", aspect,
		"sound", klingAsStr(sound),
		"has_image", hasImage || hasFirstFrame,
		"has_image_tail", hasImageTail || hasLastFrame,
		"reference_count", referenceCount,
		"prompt_len", promptLen,
	}
}

// ── job_id 编解码 ────────────────────────────────────────────────────────

// klingEncodeJobID 把生成类型子路径 + 有声标志编进持久化 job_id（subpath:task_id:audio）。
//
// resume 时请求已无 start_image 可推断子路径，必须随 task_id 一起持久化；有声标志同理，避免
// resume 时按可能已漂移的配置重算导致有声/无声计费漂移。
func klingEncodeJobID(subpath, taskID string, generateAudio bool) string {
	audio := "0"
	if generateAudio {
		audio = "1"
	}
	return subpath + ":" + taskID + ":" + audio
}

// klingDecodeJobID 从持久化 job_id 复原 (子路径, task_id, 有声标志)。
//
// 3 段（subpath:task_id:audio，audio 为 0/1）带有声标志；2 段（subpath:task_id）不带，返回 nil；
// 无已知前缀回落 text2video、整串作 task_id。
func klingDecodeJobID(jobID string) (string, string, *bool) {
	parts := strings.Split(jobID, ":")
	if len(parts) == 3 && isKlingResumableSubpath(parts[0]) && (parts[2] == "0" || parts[2] == "1") {
		audio := parts[2] == "1"
		return parts[0], parts[1], &audio
	}
	prefix, rest, found := strings.Cut(jobID, ":")
	if found && isKlingResumableSubpath(prefix) {
		return prefix, rest, nil
	}
	return klingText2Video, jobID, nil
}

// isKlingResumableSubpath 是否已知的生成类型子路径。
func isKlingResumableSubpath(subpath string) bool {
	_, ok := klingResumableSubpaths[subpath]
	return ok
}

// klingEscapeTaskID 按 Python quote(safe=”) 口径百分号编码 task_id。
func klingEscapeTaskID(taskID string) string {
	var b strings.Builder
	for i := 0; i < len(taskID); i++ {
		c := taskID[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteString(fmt.Sprintf("%%%02X", c))
	}
	return b.String()
}

// klingMaxWait 最大等待时长：max(900, duration*60) 秒。
func klingMaxWait(durationSeconds int) time.Duration {
	seconds := durationSeconds * klingPollTimeoutPerSecond
	if seconds < klingMinPollTimeoutSeconds {
		seconds = klingMinPollTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

// ── generate / resume ───────────────────────────────────────────────────

// translateMultiImageSubmitError 将 Kling 对多图模型/端点组合的确定性 400 转为可操作的配置错误。
func (b *klingVideoBackend) translateMultiImageSubmitError(err error, subpath string) error {
	if subpath != klingMultiImage2Video || strings.ToLower(strings.TrimSpace(b.model)) != "kling-v3-omni" {
		return nil
	}
	var statusErr *provider.HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != 400 {
		return nil
	}
	var body any
	if jsonErr := json.Unmarshal([]byte(statusErr.Body), &body); jsonErr != nil {
		return nil
	}
	bodyMap := klingAsDict(body)
	if klingFormatCodeValue(bodyMap["code"]) != "1201" {
		return nil
	}
	if strings.ToLower(strings.TrimSpace(klingAsStr(bodyMap["message"]))) != "model is not supported" {
		return nil
	}
	return provider.NewVideoCapabilityError(provider.FailureVideoKlingModelEndpointUnsupported, map[string]any{
		"provider": b.name(),
		"model":    b.model,
		"endpoint": b.baseURL + "/videos/" + subpath,
	})
}

// Generate 提交并轮询至完成，下载成片到 req.OutputPath。
func (b *klingVideoBackend) Generate(
	ctx context.Context, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	endpoint, payload, err := b.buildPayload(req)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	generateAudio := b.effectiveAudio(req, endpoint)
	slog.Info("调用 Kling 视频 API", klingVideoSafeLogView(endpoint, payload)...)

	var taskID, jobSubpath string
	if b.isModernModel() {
		taskID, err = b.submitTask(ctx, endpoint, payload)
		jobSubpath = klingModern
	} else {
		taskID, err = b.submitTask(ctx, "videos/"+endpoint, payload)
		jobSubpath = endpoint
	}
	if err != nil {
		if translated := b.translateMultiImageSubmitError(err, endpoint); translated != nil {
			return provider.VideoGenerationResult{}, translated
		}
		return provider.VideoGenerationResult{}, err
	}
	slog.Info("Kling 视频任务已创建", "task_id", taskID, "model", b.model)

	if err := PersistProviderJobIDFor(ctx, b.opts, req, klingEncodeJobID(jobSubpath, taskID, generateAudio), ""); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	return b.pollAndBuild(ctx, endpoint, taskID, req, generateAudio)
}

// ResumeVideo 接续已 submit 的 Kling task：仅轮询 + 取 url + 下载，不重新提交。
//
// 查询子路径从持久化 job_id 复原；有声标志同样优先取持久化值（submit 时算定），2 段 job_id 一律
// 判无声——这类 job_id 只由不具备音频能力的实现产出，成片必然无声。
func (b *klingVideoBackend) ResumeVideo(
	ctx context.Context, jobID string, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	subpath, taskID, persistedAudio := klingDecodeJobID(jobID)
	generateAudio := persistedAudio != nil && *persistedAudio
	return b.pollAndBuild(ctx, subpath, taskID, req, generateAudio)
}

// pollAndBuild 轮询至终态、取结果 URL 并下载。
func (b *klingVideoBackend) pollAndBuild(
	ctx context.Context, subpath, taskID string, req provider.VideoGenerationRequest, generateAudio bool,
) (provider.VideoGenerationResult, error) {
	pollPath := "videos/" + subpath + "/" + taskID
	if subpath == klingModern || strings.HasPrefix(subpath, klingModernRoutePrefix) {
		pollPath = klingModernRoutePrefix + "tasks?task_ids=" + klingEscapeTaskID(taskID)
	}
	final, err := b.pollUntilTerminal(ctx, func(ctx context.Context) (map[string]any, error) {
		return b.pollQuery(ctx, pollPath)
	}, klingVideoPollInterval, klingMaxWait(req.DurationSeconds))
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}

	downloadURL, err := klingExtractVideoURL(final)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	if err := DownloadVideo(ctx, downloadURL, req.OutputPath, 120*time.Second); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	slog.Info("Kling 视频下载完成", "path", req.OutputPath)

	audio := generateAudio
	return provider.VideoGenerationResult{
		VideoPath:       req.OutputPath,
		Provider:        klingProvider,
		Model:           b.model,
		DurationSeconds: req.DurationSeconds,
		VideoURI:        downloadURL,
		TaskID:          taskID,
		GenerateAudio:   &audio,
	}, nil
}

func init() {
	provider.RegisterVideoBackend("kling", provider.VideoBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.VideoBackend, error) {
			return newKlingVideoBackend(opts)
		},
		CapabilitiesForModel:           klingVideoCapabilitiesForModel,
		EffectiveGenerateAudioForModel: klingEffectiveGenerateAudioForModel,
	})
}
