package domain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 任务失败原因的结构化编码：worker 落库的是机器稳定的 ``[code] {json params}``，
// 而不是被语言锁死的文案；读侧（tasks API）再用请求的 Translator 渲染成 zh/en/vi。
// 识别不了的字符串（裸异常文本、历史行）原样透传，绝不丢原因或误解析。

// MaxErrorMessageLen 是 error_message 落库的长度上限（字符数，与 Python len() 同口径）。
const MaxErrorMessageLen = 2000

// CascadeFailureCode 是级联失败的机器码，payload 里的 reason 参数可能又是同码信封。
const CascadeFailureCode = "cascade_blocked_dependency"

// CapabilityFailureCodes 是后端能力拒绝类失败码（ImageCapabilityError /
// VideoCapabilityError / ReferencePayloadFloorError）。这些码本身就是 i18n errors
// 目录的 key，映射为恒等；逐个枚举而非推导，新码未登记就在测试里失败。
var CapabilityFailureCodes = map[string]struct{}{
	"image_capability_missing_i2i":                   {},
	"image_capability_missing_t2i":                   {},
	"image_dashscope_4k_t2i_only":                    {},
	"image_endpoint_mismatch_no_i2i":                 {},
	"image_endpoint_mismatch_no_t2i":                 {},
	"image_reference_images_unreadable":              {},
	"ref_payload_floor_exceeded":                     {},
	"video_capabilities_unresolved":                  {},
	"video_capability_missing_t2v":                   {},
	"video_duration_invalid":                         {},
	"video_duration_not_supported":                   {},
	"video_end_image_requires_start_image":           {},
	"video_end_image_unreadable":                     {},
	"video_last_frame_requires_pro":                  {},
	"video_last_frame_unsupported":                   {},
	"video_prompt_too_long":                          {},
	"video_reference_images_duration_unsupported":    {},
	"video_reference_images_exceeded":                {},
	"video_reference_images_required":                {},
	"video_reference_images_unreadable":              {},
	"video_reference_images_unsupported":             {},
	"video_resolution_not_supported":                 {},
	"video_kling_model_endpoint_unsupported":         {},
	"video_reference_images_with_frames_unsupported": {},
	"video_reference_audio_duration_exceeded":        {},
	"video_reference_audio_exceeded":                 {},
	"video_reference_audio_format_unsupported":       {},
	"video_reference_audio_slots_insufficient":       {},
	"video_reference_audio_unreadable":               {},
	"video_reference_audio_unsupported":              {},
	"video_resolution_duration_unsupported":          {},
	"video_start_image_unreadable":                   {},
}

// FailureCodeKeys 是「机器码 → i18n errors key」。能力类码恒等映射；解析层与队列
// 自造的码走 task_fail_* 前缀。EncodeFailure 只接受在此登记的码，拼错在调用点即失败，
// 而不是悄悄落一条渲染不出来的原因。
var FailureCodeKeys = map[string]string{
	"image_capability_missing_i2i":                   "image_capability_missing_i2i",
	"image_capability_missing_t2i":                   "image_capability_missing_t2i",
	"image_dashscope_4k_t2i_only":                    "image_dashscope_4k_t2i_only",
	"image_endpoint_mismatch_no_i2i":                 "image_endpoint_mismatch_no_i2i",
	"image_endpoint_mismatch_no_t2i":                 "image_endpoint_mismatch_no_t2i",
	"image_reference_images_unreadable":              "image_reference_images_unreadable",
	"ref_payload_floor_exceeded":                     "ref_payload_floor_exceeded",
	"video_capabilities_unresolved":                  "video_capabilities_unresolved",
	"video_capability_missing_t2v":                   "video_capability_missing_t2v",
	"video_duration_invalid":                         "video_duration_invalid",
	"video_duration_not_supported":                   "video_duration_not_supported",
	"video_end_image_requires_start_image":           "video_end_image_requires_start_image",
	"video_end_image_unreadable":                     "video_end_image_unreadable",
	"video_last_frame_requires_pro":                  "video_last_frame_requires_pro",
	"video_last_frame_unsupported":                   "video_last_frame_unsupported",
	"video_prompt_too_long":                          "video_prompt_too_long",
	"video_reference_images_duration_unsupported":    "video_reference_images_duration_unsupported",
	"video_reference_images_exceeded":                "video_reference_images_exceeded",
	"video_reference_images_required":                "video_reference_images_required",
	"video_reference_images_unreadable":              "video_reference_images_unreadable",
	"video_reference_images_unsupported":             "video_reference_images_unsupported",
	"video_resolution_not_supported":                 "video_resolution_not_supported",
	"video_kling_model_endpoint_unsupported":         "video_kling_model_endpoint_unsupported",
	"video_reference_images_with_frames_unsupported": "video_reference_images_with_frames_unsupported",
	"video_reference_audio_duration_exceeded":        "video_reference_audio_duration_exceeded",
	"video_reference_audio_exceeded":                 "video_reference_audio_exceeded",
	"video_reference_audio_format_unsupported":       "video_reference_audio_format_unsupported",
	"video_reference_audio_slots_insufficient":       "video_reference_audio_slots_insufficient",
	"video_reference_audio_unreadable":               "video_reference_audio_unreadable",
	"video_reference_audio_unsupported":              "video_reference_audio_unsupported",
	"video_resolution_duration_unsupported":          "video_resolution_duration_unsupported",
	"video_start_image_unreadable":                   "video_start_image_unreadable",
	// 视频解析闸（lib/config/resolver.py 的 VideoBucketCapabilityError）：抛点在解析层，
	// 码即 errors 目录 key。
	"video_capability_missing_i2v":           "video_capability_missing_i2v",
	"video_capability_missing_r2v":           "video_capability_missing_r2v",
	"video_capability_reference_unavailable": "video_capability_reference_unavailable",
	"provider_unsupported_media":             "task_fail_provider_unsupported_media",
	"restart_lost_image":                     "task_fail_restart_lost_image",
	"restart_lost_audio":                     "task_fail_restart_lost_audio",
	"restart_lost_no_job_id":                 "task_fail_restart_lost_no_job_id",
	"restart_lost_resume_no_job_id":          "task_fail_restart_lost_resume_no_job_id",
	"resume_unsupported_provider":            "task_fail_resume_unsupported_provider",
	"resume_unsupported_capacity_zero":       "task_fail_resume_unsupported_capacity_zero",
	"resume_unsupported_detail":              "task_fail_resume_unsupported_detail",
	"resume_expired_detail":                  "task_fail_resume_expired_detail",
	"resume_endpoint_changed_detail":         "task_fail_resume_endpoint_changed_detail",
	// ScriptEditError.key 本身就是 errors 目录 key。
	"script_edit_error":                    "script_edit_error",
	"script_edit_items_not_list":           "script_edit_items_not_list",
	"script_edit_unit_lists_invalid":       "script_edit_unit_lists_invalid",
	"script_edit_generated_assets_invalid": "script_edit_generated_assets_invalid",
	"cascade_blocked_dependency":           "task_fail_cascade_blocked_dependency",
}

// MaxCascadeUnwrap 是 collapseCascadeReason 的解包上限，纯空转防线。
const maxCascadeUnwrap = 100

const redactedValue = "[REDACTED]"

var (
	sensitiveKeyRe  = regexp.MustCompile(`(?i)^(?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|signature|secret|password|passwd|authorization)$`)
	sensitiveTextRe = regexp.MustCompile(
		`(?i)(\b(?:authorization\s*:\s*bearer|x[-_]?api[-_]?key|api[_-]?key|access[_-]?token|refresh[_-]?token|token|signature|secret|password|passwd)\b\s*[:=]\s*)(["']?)([^"'\s,;&}]+)`)
	bearerTextRe     = regexp.MustCompile(`(?i)(\bbearer\s+)([^\s,;"'{}]+)`)
	sensitiveQueryRe = regexp.MustCompile(
		`(?i)([?&](?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|signature|secret|password|passwd|key)(?:=|%3d))([^&#\s"'<>;,)}\]]+)`)
	// Go 的 RE2 没有 lookbehind，用「捕获前导字符再原样吐回」替代 (?<!...)：语义等价，
	// 只是被前导字符分隔的相邻路径会各自独立匹配。
	windowsAbsPathRe = regexp.MustCompile(`(^|[^A-Za-z0-9_/:])((?:[A-Za-z]:[\\/])(?:[^\\/\s"'<>|,;)}\]\]]+[\\/])*[^\\/\s"'<>|,;)}\]\]]*)`)
	posixAbsPathRe   = regexp.MustCompile(`(^|[^A-Za-z0-9_/])(/(?:Users|home|var|tmp)(?:/[^\s"'<>|,;)}\]\]]*)?)`)
	// DOTALL（(?s)）保证参数值里的转义换行不会截断匹配。
	structuredRe = regexp.MustCompile(`(?s)^\[(\w+)\](?: (\{.*\}))?$`)
)

// EncodeFailure 把已知失败码（+参数）编码成落库的机器串：无参数时是 “[code]“，
// 否则 “[code] {python 风格 JSON, 键排序}“。未登记的码返回错误（fail fast）。
// 参数里的非 JSON 原生值转成文本（对应 Python 的 default=str）：参数往往回显被拒绝的
// 原始配置值，编码不因载荷奇怪而在失败路径上再抛一次。
func EncodeFailure(code string, params map[string]any) (string, error) {
	if _, ok := FailureCodeKeys[code]; !ok {
		return "", fmt.Errorf("unknown failure code: %q", code)
	}
	if len(params) == 0 {
		return "[" + code + "]", nil
	}
	return "[" + code + "] " + pyJSONSpaced(params), nil
}

// ParseStructuredFailure 把 “[code] {params}“ 拆成 (code, params)；识别不了或
// params 畸形时返回 false。
func ParseStructuredFailure(msg string) (string, map[string]any, bool) {
	m := structuredRe.FindStringSubmatch(msg)
	if m == nil {
		return "", nil, false
	}
	code := m[1]
	if _, ok := FailureCodeKeys[code]; !ok {
		return "", nil, false
	}
	if m[2] == "" {
		return code, map[string]any{}, true
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(m[2]), &parsed); err != nil {
		// 畸形 JSON 按无法识别处理：调用方原样透传，原因不丢。
		return "", nil, false
	}
	return code, parsed, true
}

// SanitizeFailureReason 在落库/回传前清掉凭证与本地绝对路径。可解析的结构化信封
// 保留机器码（逐参数清洗后重新编码）；裸文本就地清洗。与 BoundReason 的分工：
// 这里只管内容安全，长度收窄归 BoundReason。
func SanitizeFailureReason(reason string) string {
	code, params, ok := ParseStructuredFailure(reason)
	if !ok {
		return sanitizeFailureText(reason)
	}
	sanitized := make(map[string]any, len(params))
	for k, v := range params {
		sanitized[k] = sanitizeFailureValue(v, k, true)
	}
	encoded, err := EncodeFailure(code, sanitized)
	if err != nil {
		return sanitizeFailureText(reason)
	}
	return encoded
}

func sanitizeFailureValue(value any, key string, hasKey bool) any {
	if hasKey && sensitiveKeyRe.MatchString(key) {
		return redactedValue
	}
	switch v := value.(type) {
	case string:
		return sanitizeFailureText(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = sanitizeFailureValue(item, k, true)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = sanitizeFailureValue(item, "", false)
		}
		return out
	default:
		return value
	}
}

func sanitizeFailureText(reason string) string {
	s := sensitiveQueryRe.ReplaceAllString(reason, "${1}"+redactedValue)
	s = sensitiveTextRe.ReplaceAllString(s, "${1}${2}"+redactedValue)
	s = bearerTextRe.ReplaceAllString(s, "${1}"+redactedValue)
	s = windowsAbsPathRe.ReplaceAllString(s, "${1}[REDACTED_PATH]")
	return posixAbsPathRe.ReplaceAllString(s, "${1}[REDACTED_PATH]")
}

// CollapseCascadeReason 把嵌套的级联原因折叠成最内层的根本原因，供级联编码前调用。
// 逐层包裹会让串近指数增长：每层都把上层的整个信封重新编码进 JSON，转义使长度翻倍，
// 七层左右就撑破落库预算，裁剪只能从尾部切字符、把内层信封切在 JSON 中途。
// 折叠后串长与依赖链深度无关。
func CollapseCascadeReason(reason string) string {
	for seen := 0; ; seen++ {
		if seen > maxCascadeUnwrap {
			// 正常链路远达不到这个深度；真撞上说明遇到了畸形或人造的深层串，
			// 就地收手，不在落库路径上空转。
			return reason
		}
		code, params, ok := ParseStructuredFailure(reason)
		if !ok || code != CascadeFailureCode {
			return reason
		}
		nested, ok := params["reason"].(string)
		if !ok {
			return reason
		}
		reason = nested
	}
}

// BoundReason 把 reason 裁剪到 limit 个字符内。直接按字符截断合法的 “[code] {params}“
// 会切断 JSON 尾部，使读侧无法解析、原文未翻译地泄露给界面（resume_expired_detail 的
// detail 参数源自远端错误响应，可能上千字符）。这里优先收窄结构化串里最长的字符串参数，
// 保持重新编码后仍是合法信封；非结构化文本或裁剪后仍超限时退回按原始字符裁剪。
//
// 超长的非字符串参数先降级成自身的 JSON 文本再参与裁剪：容器值（video_duration_invalid
// 回显的原始配置值）同样可能撑爆预算，而 JSON 原生类型转成字符串会在重编码时被加上引号，
// 所以只对容器做降级。
//
// 裁剪按重新编码后的实际长度迭代收敛，不用一次算准的差值：引号与反斜杠经 JSON 转义后
// 占两列，超额长度与原始字符数不等长。
func BoundReason(reason string, limit int) string {
	if utf8.RuneCountInString(reason) <= limit {
		return reason
	}
	m := structuredRe.FindStringSubmatch(reason)
	if m == nil {
		return truncateRunes(reason, limit)
	}
	code := m[1]
	if _, ok := FailureCodeKeys[code]; !ok {
		return truncateRunes(reason, limit)
	}
	if m[2] == "" {
		return truncateRunes(reason, limit)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(m[2]), &raw); err != nil {
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

	encoded, err := EncodeFailure(code, params)
	if err != nil {
		return truncateRunes(reason, limit)
	}
	for utf8.RuneCountInString(encoded) > limit {
		longest := stringKeys[0]
		for _, k := range stringKeys[1:] {
			if utf8.RuneCountInString(params[k].(string)) > utf8.RuneCountInString(params[longest].(string)) {
				longest = k
			}
		}
		current := params[longest].(string)
		if current == "" {
			// 字符串参数全被削空仍超限，说明预算连信封骨架都装不下，退回按字符裁剪。
			return truncateRunes(reason, limit)
		}
		deficit := utf8.RuneCountInString(encoded) - limit
		drop := deficit
		if deficit >= utf8.RuneCountInString(current) {
			// 估算超过参数自身长度时改砍一半而非削空：转义参数的超额长度常大于原始长度，
			// 一次削空会把本可保留的诊断内容全丢掉。至少砍一个字符保证收敛。
			drop = utf8.RuneCountInString(current) / 2
			if drop < 1 {
				drop = 1
			}
		}
		params[longest] = truncateRunes(current, utf8.RuneCountInString(current)-drop)
		encoded, err = EncodeFailure(code, params)
		if err != nil {
			return truncateRunes(reason, limit)
		}
	}
	return encoded
}

// asShrinkable 把容器参数值换成自身的 JSON 文本，好让裁剪逻辑能收窄它——截断容器的
// 字符串形态比截断 JSON 字面量安全，后者会留下不闭合的括号。只动容器，标量保持原样。
func asShrinkable(value any) any {
	switch value.(type) {
	case map[string]any, []any:
		return pyJSONSpaced(value)
	default:
		return value
	}
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	n := 0
	for i := range s {
		if n == limit {
			return s[:i]
		}
		n++
	}
	return s
}

// FailureTranslator 是渲染失败文案的翻译函数（由 transport 层用请求的 Translator 提供）。
type FailureTranslator func(key string, params map[string]any) string

// RenderFailure 把存储的失败原因渲染成本地化文案：“[code]“ / “[code] {params}“
// 走 Translator；级联原因把上游 reason 参数递归展开，一并本地化。识别不了的
// （裸异常文本、历史行、畸形载荷）原样返回。
func RenderFailure(errorMessage *string, translate FailureTranslator) *string {
	if errorMessage == nil || *errorMessage == "" {
		return errorMessage
	}
	code, params, ok := ParseStructuredFailure(*errorMessage)
	if !ok {
		return errorMessage
	}
	if code == CascadeFailureCode {
		if nested, ok := params["reason"].(string); ok {
			merged := make(map[string]any, len(params))
			for k, v := range params {
				merged[k] = v
			}
			if rendered := RenderFailure(&nested, translate); rendered != nil {
				merged["reason"] = *rendered
			}
			params = merged
		}
	}
	out := translate(FailureCodeKeys[code], params)
	return &out
}

// ---- Python 风格 JSON 序列化 ----
//
// error_message 要与 Python json.dumps(sort_keys=True, ensure_ascii=False, default=str)
// 的字节形态对齐：分隔符带空格（", " / ": "），键排序，非 ASCII 不转义。读侧解析对
// 空格不敏感，但两套实现共写的库要能逐字节对账。

// pyJSONSpaced 输出单行、带空格分隔符的 JSON。
func pyJSONSpaced(value any) string {
	return pyJSONValue(value, 0, 0)
}

// pyJSONIndent 输出 Python json.dumps(indent=...) 风格的多行 JSON（空容器保持单行）。
func pyJSONIndent(value any, indent int) string {
	return pyJSONValue(value, indent, 0)
}

func pyJSONValue(value any, indent, depth int) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case bool:
		if v {
			return "true"
		}
		return "false"
	case string:
		return pyJSONString(v)
	case int:
		return strconv.FormatInt(int64(v), 10)
	case int8:
		return strconv.FormatInt(int64(v), 10)
	case int16:
		return strconv.FormatInt(int64(v), 10)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint:
		return strconv.FormatUint(uint64(v), 10)
	case uint8:
		return strconv.FormatUint(uint64(v), 10)
	case uint16:
		return strconv.FormatUint(uint64(v), 10)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case float32:
		return formatPyFloat(float64(v))
	case float64:
		return formatPyFloat(v)
	case json.Number:
		return v.String()
	case map[string]any:
		return pyJSONObject(v, indent, depth)
	case []any:
		return pyJSONArray(v, indent, depth)
	default:
		// default=str：剩余类型转成文本再作为 JSON 字符串输出。
		return pyJSONString(fmt.Sprint(value))
	}
}

func formatPyFloat(v float64) string {
	if v == float64(int64(v)) && v < 1e16 && v > -1e16 {
		return strconv.FormatInt(int64(v), 10)
	}
	s := strconv.FormatFloat(v, 'g', -1, 64)
	// Python 的 repr 为指数形式补 "+"，如 1e+21。
	if i := strings.IndexByte(s, 'e'); i >= 0 && i+1 < len(s) && s[i+1] != '-' && s[i+1] != '+' {
		s = s[:i+1] + "+" + s[i+1:]
	}
	return s
}

func pyJSONObject(obj map[string]any, indent, depth int) string {
	if len(obj) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
			if indent == 0 {
				b.WriteByte(' ')
			}
		}
		if indent > 0 {
			b.WriteByte('\n')
			b.WriteString(strings.Repeat(" ", indent*(depth+1)))
		}
		b.WriteString(pyJSONString(k))
		b.WriteString(": ")
		b.WriteString(pyJSONValue(obj[k], indent, depth+1))
	}
	if indent > 0 {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat(" ", indent*depth))
	}
	b.WriteByte('}')
	return b.String()
}

func pyJSONArray(arr []any, indent, depth int) string {
	if len(arr) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, item := range arr {
		if i > 0 {
			b.WriteByte(',')
			if indent == 0 {
				b.WriteByte(' ')
			}
		}
		if indent > 0 {
			b.WriteByte('\n')
			b.WriteString(strings.Repeat(" ", indent*(depth+1)))
		}
		b.WriteString(pyJSONValue(item, indent, depth+1))
	}
	if indent > 0 {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat(" ", indent*depth))
	}
	b.WriteByte(']')
	return b.String()
}

// pyJSONString 按 Python ensure_ascii=False 的转义口径输出字符串：只转义引号、
// 反斜杠与控制字符，非 ASCII 原样保留。
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
