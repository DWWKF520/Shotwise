package providers

// 目录导出：把 endpoint 注册表以纯数据形式暴露给传输层，并承载 model_id → endpoint 的
// content-first 启发式与声明序列化（端口自 lib/custom_provider/endpoints.py 的 catalog 部分）。

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/shotwise/shotwise/internal/provider"
)

// CustomEndpointDescriptor 是 GET /custom-providers/endpoints 的单条描述，与 Python
// endpoint_spec_to_dict 对齐：剥掉 builder 闭包，image_capabilities 输出排序字符串列表。
type CustomEndpointDescriptor struct {
	Key                 string   `json:"key"`
	MediaType           string   `json:"media_type"`
	Family              string   `json:"family"`
	DisplayNameKey      string   `json:"display_name_key"`
	RequestMethod       string   `json:"request_method"`
	RequestPathTemplate string   `json:"request_path_template"`
	ImageCapabilities   []string `json:"image_capabilities"`
	EndImageCapable     bool     `json:"end_image_capable"`
	Source              string   `json:"source"`
	Version             *string  `json:"version"`
}

// CustomEndpointCatalog 返回全部 endpoint 描述（按 key 排序，输出确定性）。
func CustomEndpointCatalog() []CustomEndpointDescriptor {
	out := make([]CustomEndpointDescriptor, 0, len(customEndpointRegistry))
	for key, spec := range customEndpointRegistry {
		var caps []string
		if spec.hasImageCapabilities {
			caps = spec.imageCapabilities.Names()
			sort.Strings(caps)
		}
		var version *string
		if spec.version != "" {
			v := spec.version
			version = &v
		}
		source := spec.source
		if source == "" {
			source = "builtin"
		}
		out = append(out, CustomEndpointDescriptor{
			Key:                 key,
			MediaType:           string(spec.mediaType),
			Family:              spec.family,
			DisplayNameKey:      spec.displayNameKey,
			RequestMethod:       spec.requestMethod,
			RequestPathTemplate: spec.requestPathTemplate,
			ImageCapabilities:   caps,
			EndImageCapable:     spec.endImageCapable,
			Source:              source,
			Version:             version,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// CustomEndpointDeclarationToMap 把声明序列化回 JSON 形态（响应回显用）。
func CustomEndpointDeclarationToMap(d *CustomEndpointDeclaration) map[string]any {
	if d == nil {
		return nil
	}
	out := map[string]any{
		"method": d.Method,
		"path":   d.Path,
	}
	if len(d.Headers) > 0 {
		out["headers"] = d.Headers
	}
	if len(d.BodyMapping) > 0 {
		out["body_mapping"] = d.BodyMapping
	}
	if len(d.ResponseMapping) > 0 {
		out["response_mapping"] = d.ResponseMapping
	}
	if len(d.CapabilityOverrides) > 0 {
		out["capability_overrides"] = d.CapabilityOverrides
	}
	if d.HasBodyTemplate {
		out["body_template"] = d.BodyTemplate
	}
	if d.Poll != nil {
		out["poll"] = customPollToMap(d.Poll)
	}
	return out
}

// CustomSystemVideoCapabilitiesForModel 导出系统判定给 service 层回显。
func CustomSystemVideoCapabilitiesForModel(endpoint, modelID string) (provider.VideoCapabilities, error) {
	return customSystemVideoCapabilities(endpoint, modelID)
}

// CustomEnsureOpenAIBaseURL / CustomEnsureGoogleBaseURL 导出 URL 归一化给发现与测试链路。
func CustomEnsureOpenAIBaseURL(raw string) string { return customEnsureOpenAIBaseURL(raw) }
func CustomEnsureGoogleBaseURL(raw string) string { return customEnsureGoogleBaseURL(raw) }

// CustomEndpointEndImageCapable 报告该 endpoint 的 delegate 是否真的下传尾帧约束。
// 未知 endpoint 返回 false（调用方在写入侧先行校验 endpoint 合法性）。
func CustomEndpointEndImageCapable(endpoint string) bool {
	if spec, err := CustomEndpointSpecFor(endpoint); err == nil {
		return spec.endImageCapable
	}
	return false
}

// CustomEndpointReferenceAudioCapable 报告该 endpoint 的 delegate 是否真的组装参考音频。
func CustomEndpointReferenceAudioCapable(endpoint string) bool {
	if spec, err := CustomEndpointSpecFor(endpoint); err == nil {
		return spec.referenceAudioCapable
	}
	return false
}

// CustomEndpointImageCapabilities 返回 image 类 endpoint 的能力字符串列表（排序）；
// 非 image 类返回 nil。默认模型互斥校验按 capability 槽位判冲突用。
func CustomEndpointImageCapabilities(endpoint string) []string {
	spec, err := CustomEndpointSpecFor(endpoint)
	if err != nil || !spec.hasImageCapabilities {
		return nil
	}
	names := spec.imageCapabilities.Names()
	sort.Strings(names)
	return names
}

// ── content-first 启发式：model_id + discovery_format → 默认 endpoint ──────────

var (
	customImagePattern = regexp.MustCompile(`image|dall|img|imagen|flux|seedream|jimeng|viduq[12](?:[-_].*)?`)
	customVideoPattern = regexp.MustCompile(`video|sora|kling|wan|seedance|cog|mochi|veo|pika|runway|vidu2(?:\.0)?(?:[-_].*)?|viduq3(?:[-_].*)?`)
	customAudioPattern = regexp.MustCompile(`tts|speech|cosyvoice`)
	customASRPattern   = regexp.MustCompile(`transcribe|speech.?to.?text|recognition`)
)

// InferCustomEndpoint 按 model id 内容优先推默认 endpoint（端口自 infer_endpoint）。
//
// 中转站普遍 discovery_format="openai" 但模型列表夹带 gemini-*/imagen-* 原生 id，
// 必须按内容纠偏，否则被错推到 openai 家族端点。
func InferCustomEndpoint(modelID, discoveryFormat string) string {
	lowered := strings.ToLower(modelID)
	isImage := customImagePattern.MatchString(modelID)
	isWanVersioned := strings.Contains(lowered, "wan2.") || strings.Contains(lowered, "wan3.")

	// 阿里百炼视频先于通用 is_video 拦截到原生异步端点。
	if strings.Contains(lowered, "happyhorse") {
		return "dashscope-async-video"
	}
	if isWanVersioned && !isImage {
		return "dashscope-async-video"
	}
	// MiniMax 原生 token 二级路由（先于通用 is_video/is_image）。
	if strings.Contains(lowered, "hailuo") || strings.Contains(lowered, "s2v") || strings.Contains(lowered, "minimax-h3") {
		return "minimax-video"
	}
	if strings.Contains(lowered, "image-01") {
		return "minimax-image"
	}
	// 可灵二级路由：video 语义优先于 image（image2video 含 image 语义但本质是视频）。
	if strings.Contains(lowered, "kling") {
		if strings.Contains(lowered, "video") {
			return "kling-video"
		}
		if isImage {
			return "kling-image"
		}
		return "kling-video"
	}
	// wan2.x-image / wan3.0-video-image 含 "wan" 会被视频家族误判，显式排除。
	isVideo := customVideoPattern.MatchString(modelID) && !(isWanVersioned && isImage)

	if strings.Contains(lowered, "imagen") {
		return "gemini-image"
	}
	if strings.Contains(lowered, "gemini") && !isVideo {
		if isImage {
			return "gemini-image"
		}
		return "gemini-generate"
	}
	if isVideo {
		if strings.Contains(lowered, "seedance") {
			return "ark-seedance"
		}
		if strings.Contains(lowered, "viduq3") {
			return "vidu-video"
		}
		return "openai-video"
	}
	if isImage {
		if discoveryFormat == "google" {
			return "gemini-image"
		}
		return "openai-images"
	}
	if customAudioPattern.MatchString(modelID) && !customASRPattern.MatchString(modelID) {
		return "openai-tts"
	}
	if discoveryFormat == "google" {
		return "gemini-generate"
	}
	return "openai-chat"
}

// ── 写入侧校验与回显过滤（端口自 capabilities.py 的开放白名单链路）──────────

// customCapabilityAllowlist 是开放给用户覆盖的能力维度（写入与回显两侧同源收窄）。
var customCapabilityAllowlist = map[string]bool{
	"last_frame":                true,
	"reference_audio_mode":      true,
	"max_reference_audio_count": true,
}

// CustomNarrowOverridesToAllowlist 收窄至开放白名单；白名单外的键只能由手工改库产生，
// 剔除是它唯一的出口（对用户静默，落 warning 由调用方负责）。
func CustomNarrowOverridesToAllowlist(overrides map[string]any) map[string]any {
	if len(overrides) == 0 {
		return nil
	}
	out := map[string]any{}
	for k, v := range overrides {
		if customCapabilityAllowlist[k] {
			out[k] = v
		}
	}
	return out
}

// CustomOverridesValidationError 是写入侧覆盖校验的拒绝描述；Code 同时是 i18n key。
type CustomOverridesValidationError struct {
	Code   string
	Params map[string]any
}

// CustomValidateOverridesForWrite 写入侧合法性把关（合成层对脏值只降级不抛，全在这里拒）。
// None / 空字典 = 全部跟随系统判定，一律放行。
func CustomValidateOverridesForWrite(endpoint, modelID string, overrides map[string]any) *CustomOverridesValidationError {
	if len(overrides) == 0 {
		return nil
	}
	if mt, err := CustomEndpointMediaType(endpoint); err != nil || mt != provider.MediaVideo {
		return &CustomOverridesValidationError{
			Code:   "capability_overrides_video_only",
			Params: map[string]any{"model_id": modelID, "endpoint": endpoint},
		}
	}
	for key, value := range overrides {
		field, ok := customCapabilityOverrideFields[key]
		if !ok || !customCapabilityAllowlist[key] {
			continue // 已在上游收窄，未知键静默跳过
		}
		if !customCapabilityValueMatches(value, field) {
			return &CustomOverridesValidationError{
				Code: "capability_override_invalid_value",
				Params: map[string]any{
					"model_id": modelID, "capability": key,
					"expected": customCapabilityTypeName(field),
				},
			}
		}
		if key == "last_frame" && value == true && !CustomEndpointEndImageCapable(endpoint) {
			return &CustomOverridesValidationError{
				Code:   "capability_override_last_frame_unsupported",
				Params: map[string]any{"model_id": modelID, "endpoint": endpoint},
			}
		}
		if key == "reference_audio_mode" && value == string(provider.ReferenceAudioDirect) &&
			!CustomEndpointReferenceAudioCapable(endpoint) {
			return &CustomOverridesValidationError{
				Code:   "capability_override_reference_audio_unsupported",
				Params: map[string]any{"model_id": modelID, "endpoint": endpoint},
			}
		}
	}
	// 合并后不变式：稀疏覆盖只写一维也可能凑出「支持音色输入却限 0 段」，按系统判定补齐后判。
	_, hasMode := overrides["reference_audio_mode"]
	_, hasCount := overrides["max_reference_audio_count"]
	if hasMode || hasCount {
		mode, count := customResolveAudioPair(endpoint, modelID, overrides)
		if !customAudioPairCoherent(mode, count) {
			return &CustomOverridesValidationError{
				Code:   "capability_override_audio_pair_incoherent",
				Params: map[string]any{"model_id": modelID},
			}
		}
	}
	return nil
}

// customResolveAudioPair 按系统判定补齐稀疏覆盖的音频两维。
func customResolveAudioPair(endpoint, modelID string, overrides map[string]any) (provider.ReferenceAudioMode, int) {
	mode := provider.ReferenceAudioNone
	count := 0
	if caps, err := customSystemVideoCapabilities(endpoint, modelID); err == nil {
		mode = caps.ReferenceAudioMode
		count = caps.MaxReferenceAudioCount
	}
	if s, ok := overrides["reference_audio_mode"].(string); ok {
		mode = provider.ReferenceAudioMode(s)
	}
	if n, ok := customAnyInt(overrides["max_reference_audio_count"]); ok {
		count = n
	}
	return mode, count
}

// customCapabilityTypeName 复刻 capability_type_name：错误提示里的期望类型名。
func customCapabilityTypeName(field customCapabilityField) string {
	switch field.kind {
	case customCapBool:
		return "bool"
	case customCapInt, customCapIntOptional:
		return "int"
	case customCapFloatOptional:
		return "float | None"
	case customCapAudioMode:
		return "ReferenceAudioMode"
	default:
		return "unknown"
	}
}

// CustomEffectiveOverridesForResponse 回显前过滤：执行层不会采用的键值剔除 + 白名单收窄 +
// 不一致音频对剔除。返回 nil 表示无有效覆盖。DB 里的原始 JSON 任何形状都吞掉（脏数据不 500）。
func CustomEffectiveOverridesForResponse(endpoint, modelID string, rawJSON string) map[string]any {
	if rawJSON == "" {
		return nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &parsed); err != nil || parsed == nil {
		return nil
	}
	filtered := customFilterValidOverrides(endpoint, modelID, parsed)
	narrowed := CustomNarrowOverridesToAllowlist(filtered)
	if narrowed == nil {
		return nil
	}
	_, hasMode := narrowed["reference_audio_mode"]
	_, hasCount := narrowed["max_reference_audio_count"]
	if hasMode || hasCount {
		mode, count := customResolveAudioPair(endpoint, modelID, narrowed)
		if !customAudioPairCoherent(mode, count) {
			delete(narrowed, "reference_audio_mode")
			delete(narrowed, "max_reference_audio_count")
		}
	}
	if len(narrowed) == 0 {
		return nil
	}
	return narrowed
}

// CustomSystemCapabilitiesForResponse 系统判定能力的响应形态；非 video endpoint 返回 nil。
// 判定失败降级 nil 而非 500：列表端点要能把其余模型正常呈现。
func CustomSystemCapabilitiesForResponse(endpoint, modelID string) *provider.VideoCapabilities {
	mt, err := CustomEndpointMediaType(endpoint)
	if err != nil || mt != provider.MediaVideo {
		return nil
	}
	caps, err := customSystemVideoCapabilities(endpoint, modelID)
	if err != nil {
		return nil
	}
	return &caps
}
