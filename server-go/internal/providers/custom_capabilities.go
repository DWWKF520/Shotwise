package providers

import (
	"fmt"
	"math"

	"github.com/shotwise/shotwise/internal/provider"
)

// 自定义供应商视频能力的唯一合成点（端口自 lib/custom_provider/capabilities.py）：
// endpoint spec 系统判定 ⊕ 声明提示 ⊕ 模型级用户覆盖 → 生效能力。
//
// 合成语义只此一份：需要生效能力的调用方一律走这里，不得自行合并覆盖，否则「界面允许的
// 操作在执行期反悔」。系统判定本身不落库，随注册表升级自动变化；用户覆盖是稀疏字典
// （capability_overrides JSON 列），键缺席即该维度跟随系统判定。

// customCapabilityKind 是覆盖维度的期望类型。
type customCapabilityKind int

const (
	customCapBool customCapabilityKind = iota
	customCapInt
	customCapIntOptional
	customCapFloatOptional
	customCapAudioMode
)

// customCapabilityField 描述一个可覆盖能力维度的类型约束。
type customCapabilityField struct {
	kind     customCapabilityKind
	optional bool
}

// customCapabilityOverrideFields 是覆盖键 → 值类型，与 VideoCapabilities 字段一一对应。
var customCapabilityOverrideFields = map[string]customCapabilityField{
	"first_frame":                       {kind: customCapBool},
	"last_frame":                        {kind: customCapBool},
	"max_reference_images":              {kind: customCapInt},
	"reference_audio_mode":              {kind: customCapAudioMode},
	"max_reference_audio_count":         {kind: customCapInt},
	"max_reference_audio_total_seconds": {kind: customCapFloatOptional, optional: true},
	"reference_audio_per_image":         {kind: customCapBool},
	"max_prompt_chars":                  {kind: customCapIntOptional, optional: true},
	"first_frame_ratio_adaptive_only":   {kind: customCapBool},
}

// customSystemVideoCapabilities 读 endpoint spec 得出系统对该模型视频能力的判定。
//
// 两条声明形式（注册表不变式保证恰填其一）：per-model 纯函数，或 endpoint 维度 int 硬上限
// （其余维度取 VideoCapabilities 默认）。不构造 SDK client、不查 DB。
func customSystemVideoCapabilities(endpoint, modelID string) (provider.VideoCapabilities, error) {
	spec, err := CustomEndpointSpecFor(endpoint)
	if err != nil {
		return provider.VideoCapabilities{}, err
	}
	if spec.mediaType != provider.MediaVideo {
		return provider.VideoCapabilities{}, fmt.Errorf("endpoint %q is %s, not video", endpoint, spec.mediaType)
	}
	if spec.videoCapsForModel != nil {
		caps, err := spec.videoCapsForModel(modelID)
		if err != nil {
			return provider.VideoCapabilities{}, err
		}
		if caps.MaxReferenceImages < 0 {
			return provider.VideoCapabilities{}, fmt.Errorf(
				"invalid backend max_reference_images: endpoint=%q model=%q value=%d",
				endpoint, modelID, caps.MaxReferenceImages,
			)
		}
		return caps.Normalized(), nil
	}
	if spec.videoMaxReferenceImages == nil {
		return provider.VideoCapabilities{}, fmt.Errorf(
			"video endpoint %q declares neither video_max_reference_images nor video_caps_for_model", endpoint,
		)
	}
	if *spec.videoMaxReferenceImages < 0 {
		return provider.VideoCapabilities{}, fmt.Errorf(
			"invalid video_max_reference_images on endpoint %q: %d", endpoint, *spec.videoMaxReferenceImages,
		)
	}
	caps := provider.DefaultVideoCapabilities()
	caps.MaxReferenceImages = *spec.videoMaxReferenceImages
	return caps, nil
}

// customCapabilityValueMatches 判定覆盖值是否可直接落入该能力维度。
//
// bool 与 int 互斥（Go 里已按类型区分）；数值维度拒负数；可选维度额外接受 nil。
// JSON 数字在 Go 中统一为 float64，整数字面量与浮点字面量无法区分，故 int 维度接受任一
// 整数值的 float64（较 Python 略宽），浮点维度接受任意非负数值。
func customCapabilityValueMatches(value any, field customCapabilityField) bool {
	if value == nil {
		return field.optional
	}
	switch field.kind {
	case customCapBool:
		_, ok := value.(bool)
		return ok
	case customCapInt, customCapIntOptional:
		n, ok := customAnyInt(value)
		return ok && n >= 0
	case customCapFloatOptional:
		f, ok := customAnyFloat(value)
		return ok && f >= 0
	case customCapAudioMode:
		s, ok := value.(string)
		return ok && (s == string(provider.ReferenceAudioNone) || s == string(provider.ReferenceAudioDirect))
	default:
		return false
	}
}

// customFilterValidOverrides 按写入侧同一判定过滤 overrides，返回真正生效的子集。
//
// 不被识别的键、类型不符的值一律忽略：合成是执行链路的最后一道，一条脏配置不该让整个生成
// 路径不可用。last_frame / reference_audio_mode 另按 endpoint 的 delegate 是否真的下传收窄。
func customFilterValidOverrides(endpoint, modelID string, overrides map[string]any) map[string]any {
	if overrides == nil {
		return map[string]any{}
	}
	applied := map[string]any{}
	for key, value := range overrides {
		field, ok := customCapabilityOverrideFields[key]
		if !ok {
			continue
		}
		if !customCapabilityValueMatches(value, field) {
			continue
		}
		if key == "last_frame" && value == true {
			capable := false
			if spec, err := CustomEndpointSpecFor(endpoint); err == nil {
				capable = spec.endImageCapable
			}
			if !capable {
				continue
			}
		}
		if key == "reference_audio_mode" && value == string(provider.ReferenceAudioDirect) {
			capable := false
			if spec, err := CustomEndpointSpecFor(endpoint); err == nil {
				capable = spec.referenceAudioCapable
			}
			if !capable {
				continue
			}
		}
		applied[key] = value
	}
	return applied
}

// customMergeOverrides 把稀疏覆盖并进能力对象，枚举维度还原成枚举成员、浮点维度还原成 float。
func customMergeOverrides(caps provider.VideoCapabilities, applied map[string]any) provider.VideoCapabilities {
	if len(applied) == 0 {
		return caps
	}
	for key, value := range applied {
		switch key {
		case "first_frame":
			caps.FirstFrame, _ = value.(bool)
		case "last_frame":
			caps.LastFrame, _ = value.(bool)
		case "max_reference_images":
			if n, ok := customAnyInt(value); ok {
				caps.MaxReferenceImages = n
			}
		case "reference_audio_mode":
			if s, ok := value.(string); ok {
				caps.ReferenceAudioMode = provider.ReferenceAudioMode(s)
			}
		case "max_reference_audio_count":
			if n, ok := customAnyInt(value); ok {
				caps.MaxReferenceAudioCount = n
			}
		case "max_reference_audio_total_seconds":
			if f, ok := customAnyFloat(value); ok {
				caps.MaxReferenceAudioTotalSeconds = &f
			}
		case "reference_audio_per_image":
			caps.ReferenceAudioPerImage, _ = value.(bool)
		case "max_prompt_chars":
			if n, ok := customAnyInt(value); ok {
				caps.MaxPromptChars = &n
			}
		case "first_frame_ratio_adaptive_only":
			caps.FirstFrameRatioAdaptiveOnly, _ = value.(bool)
		}
	}
	return caps
}

// customAudioPairCoherent 是音频两维的合并后不变式：声明支持音色输入就必须给出正的段数上限。
func customAudioPairCoherent(mode provider.ReferenceAudioMode, count int) bool {
	return mode == provider.ReferenceAudioNone || count > 0
}

// customEnforceAudioCapabilityInvariant 把违反音频两维不变式的合并结果降到 none。
func customEnforceAudioCapabilityInvariant(caps provider.VideoCapabilities) provider.VideoCapabilities {
	if customAudioPairCoherent(caps.ReferenceAudioMode, caps.MaxReferenceAudioCount) {
		return caps
	}
	caps.ReferenceAudioMode = provider.ReferenceAudioNone
	return caps
}

// CustomSynthesizeVideoCapabilitiesWithOverrides 系统判定 ⊕ 用户覆盖 → 生效能力（附带过滤后的稀疏覆盖）。
func CustomSynthesizeVideoCapabilitiesWithOverrides(
	endpoint, modelID string, overrides map[string]any,
) (provider.VideoCapabilities, map[string]any, error) {
	caps, err := customSystemVideoCapabilities(endpoint, modelID)
	if err != nil {
		return provider.VideoCapabilities{}, nil, err
	}
	applied := customFilterValidOverrides(endpoint, modelID, overrides)
	merged := customMergeOverrides(caps, applied)
	return customEnforceAudioCapabilityInvariant(merged), applied, nil
}

// CustomSynthesizeVideoCapabilitiesWithDeclaration 同合成，但把声明里的 capability_overrides
// 作为低优先级默认值：声明提示是运输元数据，刻意稀疏，不替换端点的系统能力；同一字段两处都
// 给出时显式用户覆盖获胜。
func CustomSynthesizeVideoCapabilitiesWithDeclaration(
	endpoint, modelID string, declaration *CustomEndpointDeclaration, overrides any,
) (provider.VideoCapabilities, map[string]any, error) {
	combined := map[string]any{}
	if declaration != nil {
		if err := ValidateCustomEndpointDeclaration(declaration); err != nil {
			return provider.VideoCapabilities{}, nil, err
		}
		for key, value := range declaration.CapabilityOverrides {
			combined[key] = value
		}
	}
	if overrides == nil {
		// 保持声明提示。
	} else if mapped, ok := overrides.(map[string]any); ok {
		for key, value := range mapped {
			combined[key] = value
		}
	} else {
		combined = map[string]any{}
	}
	return CustomSynthesizeVideoCapabilitiesWithOverrides(endpoint, modelID, combined)
}

// customAnyInt 把 JSON 数值还原成 int（整数值的 float64 亦可）。
func customAnyInt(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) {
			return 0, false
		}
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	default:
		return 0, false
	}
}

// customAnyFloat 把 JSON 数值还原成 float64。
func customAnyFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	default:
		return 0, false
	}
}
