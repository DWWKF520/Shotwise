package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// 任务失败码表与结构化编码（对应 Python 侧 lib/task_failure.py）。
//
// Worker 在 Task.error_message 里落机器稳定的失败原因而不是锁定语言的文案：一个 [code] 记号，
// 可选跟一个 JSON 参数对象，整体不超过 MaxFailureMessageChars。读侧按 Accept-Language 渲染。
// 认不出 [code] 形态的字符串（供应商原始异常文本、旧格式遗留行）原样透传，任何已存的失败原因
// 都不会丢失或被误解析。

// MaxFailureMessageChars 是 error_message 落库的字符上限。
const MaxFailureMessageChars = 2000

// FailureCode 是任务失败机器码。
type FailureCode string

// 能力类失败码：backend 的能力拒绝（ImageCapabilityError / VideoCapabilityError /
// ReferencePayloadFloorError）。码本身就是 i18n errors 目录的 key，映射为身份映射。
// 逐项枚举而非从抛点派生，未登记的码 fail-fast。
const (
	FailureImageCapabilityMissingI2I      = FailureCode("image_capability_missing_i2i")
	FailureImageCapabilityMissingT2I      = FailureCode("image_capability_missing_t2i")
	FailureImageDashscope4KT2IOnly        = FailureCode("image_dashscope_4k_t2i_only")
	FailureImageEndpointMismatchNoI2I     = FailureCode("image_endpoint_mismatch_no_i2i")
	FailureImageEndpointMismatchNoT2I     = FailureCode("image_endpoint_mismatch_no_t2i")
	FailureImageReferenceImagesUnreadable = FailureCode("image_reference_images_unreadable")

	FailureRefPayloadFloorExceeded = FailureCode("ref_payload_floor_exceeded")

	FailureVideoCapabilitiesUnresolved     = FailureCode("video_capabilities_unresolved")
	FailureVideoCapabilityMissingT2V       = FailureCode("video_capability_missing_t2v")
	FailureVideoDurationInvalid            = FailureCode("video_duration_invalid")
	FailureVideoDurationNotSupported       = FailureCode("video_duration_not_supported")
	FailureVideoEndImageRequiresStartImage = FailureCode("video_end_image_requires_start_image")
	FailureVideoEndImageUnreadable         = FailureCode("video_end_image_unreadable")
	FailureVideoLastFrameRequiresPro       = FailureCode("video_last_frame_requires_pro")
	FailureVideoLastFrameUnsupported       = FailureCode("video_last_frame_unsupported")
	FailureVideoPromptTooLong              = FailureCode("video_prompt_too_long")

	FailureVideoReferenceImagesDurationUnsupported   = FailureCode("video_reference_images_duration_unsupported")
	FailureVideoReferenceImagesExceeded              = FailureCode("video_reference_images_exceeded")
	FailureVideoReferenceImagesRequired              = FailureCode("video_reference_images_required")
	FailureVideoReferenceImagesUnreadable            = FailureCode("video_reference_images_unreadable")
	FailureVideoReferenceImagesUnsupported           = FailureCode("video_reference_images_unsupported")
	FailureVideoResolutionNotSupported               = FailureCode("video_resolution_not_supported")
	FailureVideoKlingModelEndpointUnsupported        = FailureCode("video_kling_model_endpoint_unsupported")
	FailureVideoReferenceImagesWithFramesUnsupported = FailureCode("video_reference_images_with_frames_unsupported")

	FailureVideoReferenceAudioDurationExceeded  = FailureCode("video_reference_audio_duration_exceeded")
	FailureVideoReferenceAudioExceeded          = FailureCode("video_reference_audio_exceeded")
	FailureVideoReferenceAudioFormatUnsupported = FailureCode("video_reference_audio_format_unsupported")
	FailureVideoReferenceAudioSlotsInsufficient = FailureCode("video_reference_audio_slots_insufficient")
	FailureVideoReferenceAudioUnreadable        = FailureCode("video_reference_audio_unreadable")
	FailureVideoReferenceAudioUnsupported       = FailureCode("video_reference_audio_unsupported")

	FailureVideoResolutionDurationUnsupported = FailureCode("video_resolution_duration_unsupported")
	FailureVideoStartImageUnreadable          = FailureCode("video_start_image_unreadable")
)

// 通用与解析层失败码。
const (
	// 视频解析闸（能力桶判定）：码即 errors 目录 key，与后端能力异常同为身份映射，
	// 但抛点在解析层而非 backend。
	FailureVideoCapabilityMissingI2V           = FailureCode("video_capability_missing_i2v")
	FailureVideoCapabilityMissingR2V           = FailureCode("video_capability_missing_r2v")
	FailureVideoCapabilityReferenceUnavailable = FailureCode("video_capability_reference_unavailable")

	FailureProviderUnsupportedMedia = FailureCode("provider_unsupported_media")

	FailureRestartLostImage         = FailureCode("restart_lost_image")
	FailureRestartLostAudio         = FailureCode("restart_lost_audio")
	FailureRestartLostNoJobID       = FailureCode("restart_lost_no_job_id")
	FailureRestartLostResumeNoJobID = FailureCode("restart_lost_resume_no_job_id")

	FailureResumeUnsupportedProvider     = FailureCode("resume_unsupported_provider")
	FailureResumeUnsupportedCapacityZero = FailureCode("resume_unsupported_capacity_zero")
	FailureResumeUnsupportedDetail       = FailureCode("resume_unsupported_detail")
	FailureResumeExpiredDetail           = FailureCode("resume_expired_detail")
	FailureResumeEndpointChangedDetail   = FailureCode("resume_endpoint_changed_detail")

	// 剧本编辑错误（ScriptEditError.key 本身即 errors 目录 key，无需前缀间接层）。
	FailureScriptEditError                  = FailureCode("script_edit_error")
	FailureScriptEditItemsNotList           = FailureCode("script_edit_items_not_list")
	FailureScriptEditUnitListsInvalid       = FailureCode("script_edit_unit_lists_invalid")
	FailureScriptEditGeneratedAssetsInvalid = FailureCode("script_edit_generated_assets_invalid")

	// 级联失败：reason 参数嵌套存储被级联依赖任务自身的失败原因（可能又是一个结构化编码串），
	// 渲染时递归展开。
	FailureCascadeBlockedDependency = FailureCode("cascade_blocked_dependency")
)

// FailureCodeSet 是失败码集合。
type FailureCodeSet map[FailureCode]struct{}

// Has 判断集合是否含该码。
func (s FailureCodeSet) Has(code FailureCode) bool {
	_, ok := s[code]
	return ok
}

// CapabilityFailureCodes 是能力类失败码集合。
var CapabilityFailureCodes = FailureCodeSet{
	FailureImageCapabilityMissingI2I:                 {},
	FailureImageCapabilityMissingT2I:                 {},
	FailureImageDashscope4KT2IOnly:                   {},
	FailureImageEndpointMismatchNoI2I:                {},
	FailureImageEndpointMismatchNoT2I:                {},
	FailureImageReferenceImagesUnreadable:            {},
	FailureRefPayloadFloorExceeded:                   {},
	FailureVideoCapabilitiesUnresolved:               {},
	FailureVideoCapabilityMissingT2V:                 {},
	FailureVideoDurationInvalid:                      {},
	FailureVideoDurationNotSupported:                 {},
	FailureVideoEndImageRequiresStartImage:           {},
	FailureVideoEndImageUnreadable:                   {},
	FailureVideoLastFrameRequiresPro:                 {},
	FailureVideoLastFrameUnsupported:                 {},
	FailureVideoPromptTooLong:                        {},
	FailureVideoReferenceImagesDurationUnsupported:   {},
	FailureVideoReferenceImagesExceeded:              {},
	FailureVideoReferenceImagesRequired:              {},
	FailureVideoReferenceImagesUnreadable:            {},
	FailureVideoReferenceImagesUnsupported:           {},
	FailureVideoResolutionNotSupported:               {},
	FailureVideoKlingModelEndpointUnsupported:        {},
	FailureVideoReferenceImagesWithFramesUnsupported: {},
	FailureVideoReferenceAudioDurationExceeded:       {},
	FailureVideoReferenceAudioExceeded:               {},
	FailureVideoReferenceAudioFormatUnsupported:      {},
	FailureVideoReferenceAudioSlotsInsufficient:      {},
	FailureVideoReferenceAudioUnreadable:             {},
	FailureVideoReferenceAudioUnsupported:            {},
	FailureVideoResolutionDurationUnsupported:        {},
	FailureVideoStartImageUnreadable:                 {},
}

// FailureCodeKeys 是失败码 → i18n errors 目录 key 的映射。code 是 agent-facing 且落库，
// key 在读取时解析为 zh/en/vi 模板。能力类码、解析闸码与剧本编辑码本身即 errors 目录 key
// （身份映射）；其余通用码带 task_fail_ 前缀间接一层。
var FailureCodeKeys = map[FailureCode]string{
	FailureImageCapabilityMissingI2I:                 "image_capability_missing_i2i",
	FailureImageCapabilityMissingT2I:                 "image_capability_missing_t2i",
	FailureImageDashscope4KT2IOnly:                   "image_dashscope_4k_t2i_only",
	FailureImageEndpointMismatchNoI2I:                "image_endpoint_mismatch_no_i2i",
	FailureImageEndpointMismatchNoT2I:                "image_endpoint_mismatch_no_t2i",
	FailureImageReferenceImagesUnreadable:            "image_reference_images_unreadable",
	FailureRefPayloadFloorExceeded:                   "ref_payload_floor_exceeded",
	FailureVideoCapabilitiesUnresolved:               "video_capabilities_unresolved",
	FailureVideoCapabilityMissingT2V:                 "video_capability_missing_t2v",
	FailureVideoDurationInvalid:                      "video_duration_invalid",
	FailureVideoDurationNotSupported:                 "video_duration_not_supported",
	FailureVideoEndImageRequiresStartImage:           "video_end_image_requires_start_image",
	FailureVideoEndImageUnreadable:                   "video_end_image_unreadable",
	FailureVideoLastFrameRequiresPro:                 "video_last_frame_requires_pro",
	FailureVideoLastFrameUnsupported:                 "video_last_frame_unsupported",
	FailureVideoPromptTooLong:                        "video_prompt_too_long",
	FailureVideoReferenceImagesDurationUnsupported:   "video_reference_images_duration_unsupported",
	FailureVideoReferenceImagesExceeded:              "video_reference_images_exceeded",
	FailureVideoReferenceImagesRequired:              "video_reference_images_required",
	FailureVideoReferenceImagesUnreadable:            "video_reference_images_unreadable",
	FailureVideoReferenceImagesUnsupported:           "video_reference_images_unsupported",
	FailureVideoResolutionNotSupported:               "video_resolution_not_supported",
	FailureVideoKlingModelEndpointUnsupported:        "video_kling_model_endpoint_unsupported",
	FailureVideoReferenceImagesWithFramesUnsupported: "video_reference_images_with_frames_unsupported",
	FailureVideoReferenceAudioDurationExceeded:       "video_reference_audio_duration_exceeded",
	FailureVideoReferenceAudioExceeded:               "video_reference_audio_exceeded",
	FailureVideoReferenceAudioFormatUnsupported:      "video_reference_audio_format_unsupported",
	FailureVideoReferenceAudioSlotsInsufficient:      "video_reference_audio_slots_insufficient",
	FailureVideoReferenceAudioUnreadable:             "video_reference_audio_unreadable",
	FailureVideoReferenceAudioUnsupported:            "video_reference_audio_unsupported",
	FailureVideoResolutionDurationUnsupported:        "video_resolution_duration_unsupported",
	FailureVideoStartImageUnreadable:                 "video_start_image_unreadable",

	FailureVideoCapabilityMissingI2V:           "video_capability_missing_i2v",
	FailureVideoCapabilityMissingR2V:           "video_capability_missing_r2v",
	FailureVideoCapabilityReferenceUnavailable: "video_capability_reference_unavailable",
	FailureProviderUnsupportedMedia:            "task_fail_provider_unsupported_media",
	FailureRestartLostImage:                    "task_fail_restart_lost_image",
	FailureRestartLostAudio:                    "task_fail_restart_lost_audio",
	FailureRestartLostNoJobID:                  "task_fail_restart_lost_no_job_id",
	FailureRestartLostResumeNoJobID:            "task_fail_restart_lost_resume_no_job_id",
	FailureResumeUnsupportedProvider:           "task_fail_resume_unsupported_provider",
	FailureResumeUnsupportedCapacityZero:       "task_fail_resume_unsupported_capacity_zero",
	FailureResumeUnsupportedDetail:             "task_fail_resume_unsupported_detail",
	FailureResumeExpiredDetail:                 "task_fail_resume_expired_detail",
	FailureResumeEndpointChangedDetail:         "task_fail_resume_endpoint_changed_detail",
	FailureScriptEditError:                     "script_edit_error",
	FailureScriptEditItemsNotList:              "script_edit_items_not_list",
	FailureScriptEditUnitListsInvalid:          "script_edit_unit_lists_invalid",
	FailureScriptEditGeneratedAssetsInvalid:    "script_edit_generated_assets_invalid",
	FailureCascadeBlockedDependency:            "task_fail_cascade_blocked_dependency",
}

// IsKnownFailureCode 判断码是否已登记（未登记的码不得落库、不参与渲染）。
func IsKnownFailureCode(code FailureCode) bool {
	_, ok := FailureCodeKeys[code]
	return ok
}

// FailureI18nKey 返回码对应的 i18n errors 目录 key。
func FailureI18nKey(code FailureCode) (string, bool) {
	key, ok := FailureCodeKeys[code]
	return key, ok
}

// NonResumableVideoProviders 是不实现 resume 的视频 provider 集合：orphan 处理据此把这些
// provider 的 running 孤儿标成 [resume_unsupported] 失败，而非重新 submit 造成二次扣费
// （Grok 同步型无 job_id；Vidu 未抽出独立 resume）。新增不支持 resume 的 backend 时同步登记。
var NonResumableVideoProviders = map[string]struct{}{
	"grok": {},
	"vidu": {},
}

// IsNonResumableVideoProvider 判断该 provider 是否不支持视频续跑。
func IsNonResumableVideoProvider(providerID string) bool {
	_, ok := NonResumableVideoProviders[providerID]
	return ok
}

// 结构化失败串：[code] 可选跟随一个空格与 JSON 参数对象，两端锚定，
// 使旧格式的 [restart_lost] 中文尾巴与任意异常文本都不会命中。DOTALL 让参数值含转义换行时
// JSON 分组仍能匹配完整。
var structuredFailureRe = regexp.MustCompile(`(?s)^\[(\w+)\](?: (\{.*\}))?$`)

// EncodeFailureMessage 把已知失败码（+参数）编码为落库机器串：[code]，有参数时为
// [code] {排序 JSON}，并裁剪到 MaxFailureMessageChars 以内。
//
// 未登记的码是调用点笔误，直接 panic（等价于 Python 侧抛 KeyError 的 fail-fast），
// 不让一个无法渲染的原因静默落库。参数中非 JSON 原生值按文本编码（等价 default=str）：
// 能力码常回显被拒的原始配置值，失败路径不得因编码失败而卡住。
func EncodeFailureMessage(code FailureCode, params map[string]any) string {
	if !IsKnownFailureCode(code) {
		panic(fmt.Sprintf("unknown failure code: %q", code))
	}
	return BoundFailureReason(encodeFailureRaw(code, params), MaxFailureMessageChars)
}

// encodeFailureRaw 编码但不裁剪、不校验（供裁剪与脱敏路径复用，避免递归互调）。
func encodeFailureRaw(code FailureCode, params map[string]any) string {
	if len(params) == 0 {
		return "[" + string(code) + "]"
	}
	return "[" + string(code) + "] " + encodeParams(params)
}

// encodeParams 按 key 排序编码参数对象，冒号与逗号后带一个空格，与 Python
// json.dumps(sort_keys=True, ensure_ascii=False) 的默认分隔符一致。
func encodeParams(params map[string]any) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(marshalJSONValue(k))
		b.WriteString(": ")
		b.WriteString(marshalParamValue(params[k]))
	}
	b.WriteByte('}')
	return b.String()
}

// marshalParamValue 编码单个参数值；不可编码的值降级为文本（等价 Python default=str）。
func marshalParamValue(v any) string {
	switch v.(type) {
	case nil, bool, string, float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, json.Number, map[string]any, []any:
		if encoded, ok := tryMarshal(v); ok {
			return encoded
		}
	}
	if encoded, ok := tryMarshal(v); ok {
		return encoded
	}
	return marshalJSONValue(fmt.Sprint(v))
}

// marshalJSONValue 编码单个字符串值，不转义 HTML 字符（< > & 保持原样，与
// ensure_ascii=False 的输出一致）。
func marshalJSONValue(s string) string {
	encoded, _ := tryMarshal(s)
	return encoded
}

func tryMarshal(v any) (string, bool) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", false
	}
	return strings.TrimSuffix(buf.String(), "\n"), true
}

// DecodeFailureMessage 把 [code] {params} 拆成 (code, params)；识别不了、参数畸形或参数非对象时
// 返回 ok=false，调用方原样透传原文。
func DecodeFailureMessage(message string) (FailureCode, map[string]any, bool) {
	m := structuredFailureRe.FindStringSubmatch(message)
	if m == nil {
		return "", nil, false
	}
	code := FailureCode(m[1])
	if !IsKnownFailureCode(code) {
		return "", nil, false
	}
	if m[2] == "" {
		return code, map[string]any{}, true
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(m[2]), &params); err != nil || params == nil {
		return "", nil, false
	}
	return code, params, true
}

// BoundFailureReason 把 reason 裁剪到 limit 字符内。
//
// 直接按字符截断合法的 [code] {params} 结构化串会切断 JSON 尾部，使读侧无法解析、原文未翻译地
// 泄露给界面（如 resume_expired_detail 的 detail 参数源自远端错误响应，可能长达上千字符）。
// 这里优先裁剪结构化串里最长的字符串参数，保持重新编码后仍是合法的 [code] {params}；
// 非结构化文本或裁剪后仍超限时退回按原始字符裁剪。
//
// 超长的非字符串参数先降级成自身的 JSON 文本再参与裁剪：容器值同样可能撑爆预算——
// video_duration_invalid 回显的是调用方拒绝的原始配置值，导入或手工编辑的 project.json 能把
// 它写成一个大数组——而 JSON 原生标量不经降级，没有字符串参数可收窄时就会退到裸切片，把信封
// 切在 JSON 中途。裁剪按重新编码后的实际长度迭代收敛，不用一次算准的差值：转义字符占两列，
// 超额长度与原始字符数不等长。
func BoundFailureReason(reason string, limit int) string {
	if utf8.RuneCountInString(reason) <= limit {
		return reason
	}
	m := structuredFailureRe.FindStringSubmatch(reason)
	if m == nil {
		return truncateRunes(reason, limit)
	}
	code := FailureCode(m[1])
	if !IsKnownFailureCode(code) || m[2] == "" {
		return truncateRunes(reason, limit)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(m[2]), &raw); err != nil || raw == nil {
		return truncateRunes(reason, limit)
	}
	params := make(map[string]any, len(raw))
	for k, v := range raw {
		params[k] = asShrinkable(v)
	}
	stringKeys := make([]string, 0, len(params))
	for k, v := range params {
		if _, ok := v.(string); ok {
			stringKeys = append(stringKeys, k)
		}
	}
	if len(stringKeys) == 0 {
		return truncateRunes(reason, limit)
	}
	sort.Strings(stringKeys)
	encoded := encodeFailureRaw(code, params)
	for utf8.RuneCountInString(encoded) > limit {
		longestKey := stringKeys[0]
		for _, k := range stringKeys[1:] {
			if utf8.RuneCountInString(params[k].(string)) > utf8.RuneCountInString(params[longestKey].(string)) {
				longestKey = k
			}
		}
		current := params[longestKey].(string)
		if current == "" {
			// 字符串参数全被削空仍超限，说明预算连信封骨架都装不下，退回按字符裁剪。
			return truncateRunes(reason, limit)
		}
		// 超额长度以编码后的字符计，原始字符经 JSON 转义可能占多列，两者不等长：拿它当要砍的
		// 原始字符数只是下界估算，砍完重编码再看，直到真正装下。估算超过参数自身长度时改砍
		// 一半而非削空——一次削空会丢掉本可保留的诊断内容。至少砍一个字符保证收敛。
		deficit := utf8.RuneCountInString(encoded) - limit
		currentLen := utf8.RuneCountInString(current)
		drop := deficit
		if drop >= currentLen {
			drop = currentLen / 2
			if drop < 1 {
				drop = 1
			}
		}
		params[longestKey] = truncateRunes(current, currentLen-drop)
		encoded = encodeFailureRaw(code, params)
	}
	return encoded
}

// asShrinkable 把容器参数值换成自身的 JSON 文本，好让裁剪逻辑能收窄它：截断容器的字符串形态
// 比截断 JSON 字面量安全（后者会留下不闭合的括号）。标量本就撑不爆预算也无从收窄，且转成
// 文本会在重新编码时被加上引号，把落库信封里的参数类型改掉，故只动容器。
func asShrinkable(value any) any {
	switch value.(type) {
	case map[string]any, []any:
		if encoded, ok := tryMarshal(value); ok {
			return encoded
		}
		return "…"
	}
	return value
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

// CollapseCascadeReason 把嵌套的级联原因折叠成最内层的根本原因，供级联编码前调用。
//
// 逐层包裹会让串近指数增长：每一层都把上一层的整个信封重新编码进 JSON，转义使长度翻倍，
// 多层后就会撑破落库预算，裁剪只能从尾部切字符，把内层信封切在 JSON 中途——外层仍可解析，
// 读侧却把残缺的内层当普通文本原样嵌进本地化文案里。中间层携带的只是「另一个同样被阻塞的
// 任务 id」，用户能据以行动的是直接依赖与根本原因两项，前者由外层自己的 dependency_task_id
// 保留。折叠后串长与依赖链深度无关。
func CollapseCascadeReason(reason string) string {
	for seen := 0; ; seen++ {
		if seen > 100 {
			// 正常链路远达不到这个深度。真撞上说明遇到了畸形或人造的深层串，就地收手，
			// 不在落库路径上空转。
			return reason
		}
		code, params, ok := DecodeFailureMessage(reason)
		if !ok || code != FailureCascadeBlockedDependency {
			return reason
		}
		nested, ok := params["reason"].(string)
		if !ok {
			return reason
		}
		reason = nested
	}
}

// 敏感键名：整键命中即整体遮蔽（含常见凭证字段名）。
var sensitiveKeyRe = regexp.MustCompile(`(?i)^(?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|signature|secret|password|passwd|authorization)$`)

// 文本中的凭证形态（含 query 参数、Bearer 头）。
var (
	sensitiveTextRe = regexp.MustCompile(
		`(?i)(\b(?:authorization\s*:\s*bearer|x[-_]?api[-_]?key|api[_-]?key|access[_-]?token|refresh[_-]?token|token|signature|secret|password|passwd)\b\s*[:=]\s*)(["']?)([^"'\s,;&}]+)`)
	bearerTextRe     = regexp.MustCompile(`(?i)(\bbearer\s+)([^\s,;"'{}]+)`)
	sensitiveQueryRe = regexp.MustCompile(
		`(?i)([?&](?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|signature|secret|password|passwd|key)(?:=|%3d))([^&#\s"'<>;,)}\]]+)`)
)

// 本地绝对路径（Windows 盘符形态与常见 POSIX 前缀）：外流会带出机器目录结构。
var (
	windowsAbsPathRe = regexp.MustCompile(`(^|[^A-Za-z0-9_/:])([A-Za-z]:[\\/](?:[^\\/\s"'<>|,;)}\]]+[\\/])*[^\\/\s"'<>|,;)}\]]*)`)
	posixAbsPathRe   = regexp.MustCompile(`(^|[^A-Za-z0-9_/])(/(?:Users|home|var|tmp)(?:/[^\s"'<>|,;)}\]]*)?)`)
)

// SanitizeFailureReason 在失败原因落库或经接口返回前做脱敏。
//
// 已知的结构化信封保持可解析且保留机器码；旧格式或供应商原始文本就地脱敏。与 BoundFailureReason
// 分离：本函数只管凭证与本地路径，长度限制归裁剪函数。
func SanitizeFailureReason(reason string) string {
	code, params, ok := DecodeFailureMessage(reason)
	if !ok {
		return sanitizeFailureText(reason)
	}
	sanitized := make(map[string]any, len(params))
	for k, v := range params {
		sanitized[k] = sanitizeFailureValue(v, k)
	}
	return encodeFailureRaw(code, sanitized)
}

func sanitizeFailureValue(value any, key string) any {
	if sensitiveKeyRe.MatchString(key) {
		return "[REDACTED]"
	}
	switch v := value.(type) {
	case string:
		return sanitizeFailureText(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = sanitizeFailureValue(item, k)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = sanitizeFailureValue(item, "")
		}
		return out
	}
	return value
}

func sanitizeFailureText(reason string) string {
	sanitized := sensitiveQueryRe.ReplaceAllString(reason, `${1}[REDACTED]`)
	sanitized = sensitiveTextRe.ReplaceAllString(sanitized, `${1}${2}[REDACTED]`)
	sanitized = bearerTextRe.ReplaceAllString(sanitized, `${1}[REDACTED]`)
	sanitized = windowsAbsPathRe.ReplaceAllString(sanitized, `${1}[REDACTED_PATH]`)
	return posixAbsPathRe.ReplaceAllString(sanitized, `${1}[REDACTED_PATH]`)
}
