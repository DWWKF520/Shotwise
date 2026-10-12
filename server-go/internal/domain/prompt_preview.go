package domain

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 入队时抓取的 prompt 预览快照：与 task payload 同存，任务详情 API 直接读取，
// 既不暴露供应商凭证，也不让 UI 从可变的项目数据重建请求。

// PromptPreviewPayloadKey 是内部 payload 键，公开的任务 payload 投影会把它移除。
const PromptPreviewPayloadKey = "__shotwise_prompt_preview"

const (
	maxPromptChars         = 4096
	maxIdentifierChars     = 256
	maxScriptFileChars     = 512
	maxReferenceKindChars  = 64
	maxReferenceLabelChars = 256
	maxReferenceValueChars = 1024
	maxNoteChars           = 512
)

var (
	urlPattern   = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)
	authSchemeRe = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[a-z0-9._~+/=-]+`)
	// Go RE2 没有反向引用，引号成对无法直接表达；拆成「单引号内 / 双引号内 / 裸 token」
	// 三个分支，替换函数回填实际命中的引号形态。
	inlineCredentialRe = regexp.MustCompile(
		`(?i)\b(access[_-]?token|api[_-]?key|authorization|client[_-]?secret|password|refresh[_-]?token|secret|token)(\s*[:=]\s*)(?:'([^']*)'|"([^"]*)"|([^\s,'"&}]+))`)
)

var sensitiveKeyNames = map[string]struct{}{
	"access_token":  {},
	"api_key":       {},
	"apikey":        {},
	"authorization": {},
	"auth":          {},
	"client_secret": {},
	"cookie":        {},
	"credential":    {},
	"credentials":   {},
	"password":      {},
	"private_key":   {},
	"refresh_token": {},
	"secret":        {},
	"set_cookie":    {},
	"signature":     {},
	"token":         {},
	"key":           {},
}

func boundedText(value any, limit int) string {
	var text string
	if s, ok := value.(string); ok {
		text = s
	} else {
		text = fmt.Sprint(value)
	}
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return truncateRunes(text, limit-1) + "…"
}

func isSensitiveKey(key any) bool {
	s := strings.TrimSpace(fmt.Sprint(key))
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' && i > 0 {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	normalized := strings.ToLower(strings.ReplaceAll(b.String(), "-", "_"))
	if _, ok := sensitiveKeyNames[normalized]; ok {
		return true
	}
	for _, suffix := range []string{"key", "token", "secret", "password", "credential"} {
		if strings.HasSuffix(normalized, "_"+suffix) {
			return true
		}
	}
	return false
}

// redactURL 对齐 Python _redact_url：先替换 bearer/basic 认证头与内联凭证（api_key=...），
// 再处理文本里所有 http(s) URL——查询参数值与 fragment 一律替换为 [REDACTED]，
// userinfo 整个丢弃，尾部标点（句读、括号）保留在 URL 之外。
func redactURL(value string) string {
	value = authSchemeRe.ReplaceAllString(value, "${1} "+redactedValue)
	value = inlineCredentialRe.ReplaceAllStringFunc(value, func(match string) string {
		m := inlineCredentialRe.FindStringSubmatch(match)
		key, sep := m[1], m[2]
		switch {
		case m[3] != "":
			return key + sep + "'" + redactedValue + "'"
		case m[4] != "":
			return key + sep + `"` + redactedValue + `"`
		default:
			return key + sep + redactedValue
		}
	})
	return urlPattern.ReplaceAllStringFunc(value, redactURLMatch)
}

func redactURLMatch(match string) string {
	candidate := match
	trailing := ""
	for len(candidate) > 0 {
		last := candidate[len(candidate)-1]
		if strings.IndexByte(".,;:!?)]}", last) < 0 {
			break
		}
		trailing = string(last) + trailing
		candidate = candidate[:len(candidate)-1]
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return match
	}
	netloc := parsed.Host
	if parsed.User != nil {
		hostname := parsed.Hostname()
		if strings.Contains(hostname, ":") && !strings.HasPrefix(hostname, "[") {
			hostname = "[" + hostname + "]"
		}
		netloc = hostname
		if port := parsed.Port(); port != "" {
			netloc += ":" + port
		}
	}
	if netloc == "" {
		// 形如 "http:///path" 的畸形 URL（无主机）：判不清重组语义，原样保留。
		return match
	}
	fragment := ""
	if parsed.Fragment != "" {
		fragment = redactedValue
	}
	return parsed.Scheme + "://" + netloc + parsed.Path + redactQuery(parsed.RawQuery, parsed.ForceQuery) +
		withFragment(fragment) + trailing
}

func withFragment(fragment string) string {
	if fragment == "" {
		return ""
	}
	return "#" + fragment
}

// redactQuery 保持键序与键的原始转义口径（对齐 Python urlencode 对键重新转义的行为），
// 值统一替换为 [REDACTED]；keep_blank_values 语义：只有键没有值也算一对。
func redactQuery(rawQuery string, forceQuery bool) string {
	if rawQuery == "" && !forceQuery {
		return ""
	}
	if rawQuery == "" {
		return "?"
	}
	parts := strings.Split(rawQuery, "&")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		key := part
		if i := strings.IndexByte(part, '='); i >= 0 {
			key = part[:i]
		}
		out = append(out, url.QueryEscape(key)+"="+url.QueryEscape(redactedValue))
	}
	return "?" + strings.Join(out, "&")
}

func sanitizePreviewValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if isSensitiveKey(key) {
				out[fmt.Sprint(key)] = redactedValue
			} else {
				out[fmt.Sprint(key)] = sanitizePreviewValue(item)
			}
		}
		return out
	case []any:
		limit := len(v)
		if limit > 32 {
			limit = 32
		}
		out := make([]any, 0, limit)
		for _, item := range v[:limit] {
			out = append(out, sanitizePreviewValue(item))
		}
		return out
	case string:
		return redactURL(v)
	default:
		return value
	}
}

func promptText(value any) (string, string) {
	switch v := value.(type) {
	case string:
		return boundedText(redactURL(v), maxPromptChars), "plain_text"
	case map[string]any:
		sanitized := sanitizePreviewValue(v)
		return boundedText(pyJSONIndent(sanitized, 2), maxPromptChars), "structured"
	case nil:
		return "", "unknown"
	default:
		return boundedText(redactURL(fmt.Sprint(value)), maxPromptChars), "unknown"
	}
}

func providerAndModel(payload map[string]any, provider string) (string, string) {
	model := ""
	for _, key := range []string{"video_provider_i2v", "video_provider_r2v", "image_provider_t2i", "image_provider_i2i"} {
		value, ok := payload[key].(string)
		if !ok || !strings.Contains(value, "/") {
			continue
		}
		candidateProvider, candidateModel, _ := strings.Cut(value, "/")
		if candidateProvider != "" && candidateModel != "" {
			if provider == "" {
				provider = candidateProvider
			}
			model = candidateModel
			break
		}
	}
	return provider, model
}

func firstTruthy(m map[string]any, keys ...string) any {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			switch t := v.(type) {
			case nil:
				continue
			case string:
				if t == "" {
					continue
				}
			}
			return v
		}
	}
	return nil
}

func previewReferences(value any) []any {
	items, ok := value.([]any)
	if !ok {
		return []any{}
	}
	limit := len(items)
	if limit > 32 {
		limit = 32
	}
	result := make([]any, 0, limit)
	for _, item := range items[:limit] {
		if m, ok := item.(map[string]any); ok {
			label := firstTruthy(m, "label", "name", "id")
			raw := firstTruthy(m, "value", "url", "path", "id")
			rawStr, ok := raw.(string)
			if !ok || rawStr == "" {
				continue
			}
			if label == nil {
				label = rawStr
			}
			kind := firstTruthy(m, "kind", "type")
			if kind == nil {
				kind = "other"
			}
			result = append(result, map[string]any{
				"kind":  boundedText(redactURL(fmt.Sprint(kind)), maxReferenceKindChars),
				"label": boundedText(redactURL(fmt.Sprint(label)), maxReferenceLabelChars),
				"value": boundedText(redactURL(rawStr), maxReferenceValueChars),
			})
		} else if s, ok := item.(string); ok && s != "" {
			sanitized := boundedText(redactURL(s), maxReferenceValueChars)
			result = append(result, map[string]any{
				"kind":  "other",
				"label": truncateRunes(sanitized, maxReferenceLabelChars),
				"value": sanitized,
			})
		}
	}
	return result
}

func boundedStringList(value any, limit int) []any {
	items, ok := value.([]any)
	if !ok {
		return []any{}
	}
	n := len(items)
	if n > 32 {
		n = 32
	}
	out := make([]any, 0, n)
	for _, item := range items[:n] {
		if s, ok := item.(string); ok {
			out = append(out, boundedText(redactURL(s), limit))
		}
	}
	return out
}

// BuildEnqueuePromptPreview 为一条刚入队的任务构造有界、供应商无关的预览快照。
// 这不是供应商载荷的序列化器：供应商侧的归一化仍发生在执行期，快照只记录不可变的
// 入队输入与准入时已知的安全元数据。
func BuildEnqueuePromptPreview(
	projectName, taskType, mediaType, resourceID string,
	scriptFile *string,
	providerID *string,
	payload map[string]any,
) map[string]any {
	originalPrompt, shape := promptText(payload["prompt"])
	provider := ""
	if providerID != nil {
		provider = *providerID
	}
	provider, model := providerAndModel(payload, provider)
	provider = boundedText(redactURL(provider), maxIdentifierChars)
	model = boundedText(redactURL(model), maxIdentifierChars)

	summary := map[string]any{
		"project_name": boundedText(redactURL(projectName), maxIdentifierChars),
		"task_type":    boundedText(redactURL(taskType), maxIdentifierChars),
		"media_type":   boundedText(redactURL(mediaType), maxIdentifierChars),
		"resource_id":  boundedText(redactURL(resourceID), maxIdentifierChars),
	}
	if scriptFile != nil && *scriptFile != "" {
		summary["script_file"] = boundedText(redactURL(*scriptFile), maxScriptFileChars)
	}

	var duration any
	switch payload["duration_seconds"].(type) {
	case bool:
		duration = nil
	default:
		switch payload["duration_seconds"].(type) {
		case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			duration = payload["duration_seconds"]
		default:
			duration = nil
		}
	}

	var resolution any
	if s, ok := payload["resolution"].(string); ok {
		resolution = boundedText(redactURL(s), maxIdentifierChars)
	} else {
		resolution = nil
	}

	request := map[string]any{
		"id":                     boundedText(redactURL(resourceID), maxIdentifierChars),
		"label":                  boundedText(redactURL(taskType+":"+resourceID), maxIdentifierChars),
		"original_prompt":        originalPrompt,
		"effective_prompt":       nilIfEmpty(originalPrompt),
		"shape":                  shape,
		"provider":               nilIfEmpty(provider),
		"model":                  nilIfEmpty(model),
		"references":             previewReferences(payload["references"]),
		"duration_seconds":       duration,
		"resolution":             resolution,
		"capability_adjustments": boundedStringList(payload["capability_adjustments"], maxNoteChars),
		"warnings":               boundedStringList(payload["warnings"], maxNoteChars),
		"request_summary":        summary,
	}
	return map[string]any{
		"source":   "enqueue_snapshot",
		"requests": []any{request},
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
