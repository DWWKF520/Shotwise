package providers

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/providers/ark"
	"github.com/shotwise/shotwise/internal/providers/dashscope"
	"github.com/shotwise/shotwise/internal/providers/kling"
	"github.com/shotwise/shotwise/internal/providers/minimax"
	"github.com/shotwise/shotwise/internal/providers/vidu"
)

// 自定义供应商 endpoint 注册表与声明式端点 DSL（端口自 lib/custom_provider/endpoints.py）。
//
// 每条 endpoint 是一个 customEndpointSpec，绑定 media_type、family、HTTP 调用形态与 build 闭包。
// CreateCustomBackend 通过 endpoint 字符串查表派发；无声明时走 spec 的内置协议 backend builder，
// 有声明时由工厂按 EndpointDeclaration 渲染请求。
//
// 声明 DSL 是一份刻意极小的纯数据描述：只做 JSON 结构拼装与字段映射，不接受任意表达式。
// 全部校验规则（正则、字段白名单、poll 字段集合、模板深度上限、poll 间隔/超时上下限、
// 方法白名单、headers 白名单）与原实现一一对应，非法声明一律 fail loud。

// ── EndpointSpec 数据类型 ──────────────────────────────────────────

// customEndpointSpec 是单条 endpoint 的元数据 + backend 构造闭包（文本端点无 builder）。
type customEndpointSpec struct {
	key                 string
	mediaType           provider.MediaType
	family              string
	displayNameKey      string
	requestMethod       string
	requestPathTemplate string

	// imageCapabilities 仅 image 类填写；hasImageCapabilities 区分「空集」与「非 image」。
	imageCapabilities    provider.ImageCapabilitySet
	hasImageCapabilities bool

	// 参考生视频单镜头参考图上限；仅 video 类有意义。nil 表示按 model 读 videoCapsForModel。
	videoMaxReferenceImages *int
	// videoCapsForModel 在 videoMaxReferenceImages 为 nil 时提供 per-model 纯函数；二者恰填其一。
	videoCapsForModel func(modelID string) (provider.VideoCapabilities, error)

	// endImageCapable 表示该 endpoint 的 delegate 真的读取请求尾帧并下传。
	endImageCapable bool
	// referenceAudioCapable 表示该 endpoint 的 delegate 真的组装参考音频。
	referenceAudioCapable bool

	source  string
	version string

	buildImage customImageBuilder
	buildVideo customVideoBuilder
	buildAudio customAudioBuilder
}

// customImageBuilder 构造图片 delegate（内置协议 backend）。
type customImageBuilder func(cfg customProviderConfig, modelID string) (provider.ImageBackend, error)

// customVideoBuilder 构造视频 delegate（内置协议 backend）。
type customVideoBuilder func(cfg customProviderConfig, modelID string) (provider.VideoBackend, error)

// customAudioBuilder 构造音频 delegate（内置协议 backend）。
type customAudioBuilder func(cfg customProviderConfig, modelID string) (provider.AudioBackend, error)

// ── 声明数据类型 ───────────────────────────────────────────────────

// customEndpointDeclarationError 是非法声明被拒时返回的错误（携带出错的字段名）。
type customEndpointDeclarationError struct {
	Field string
	Msg   string
}

func (e *customEndpointDeclarationError) Error() string {
	if e.Field != "" {
		return e.Field + ": " + e.Msg
	}
	return e.Msg
}

func customDeclarationError(field, msg string) *customEndpointDeclarationError {
	return &customEndpointDeclarationError{Field: field, Msg: msg}
}

// CustomEndpointPoll 是异步自定义端点的 submit-and-poll 传输声明。
type CustomEndpointPoll struct {
	Path            string
	JobID           string
	Status          string
	DoneValue       any
	FailedValue     any
	HasFailedValue  bool
	Result          string
	HasResult       bool
	ArtifactURL     string
	HasArtifactURL  bool
	Headers         map[string]string
	IntervalSeconds float64
	TimeoutSeconds  float64
}

// CustomEndpointDeclaration 是一份纯数据的端点描述。
type CustomEndpointDeclaration struct {
	Method              string
	Path                string
	Headers             map[string]string
	BodyMapping         map[string]string
	ResponseMapping     map[string]string
	CapabilityOverrides map[string]any
	BodyTemplate        map[string]any
	HasBodyTemplate     bool
	Poll                *CustomEndpointPoll
}

// RenderedCustomEndpointRequest 是声明渲染出的安全请求。
type RenderedCustomEndpointRequest struct {
	Method  string
	Path    string
	Headers map[string]string
	Body    map[string]any
}

// RenderedCustomEndpointPoll 是声明渲染出的安全 poll 请求。
type RenderedCustomEndpointPoll struct {
	Path    string
	Headers map[string]string
}

// ── 校验常量 ───────────────────────────────────────────────────────

var customDeclarationFields = customStringSet(
	"method", "path", "headers", "body", "body_template", "response", "capability_overrides", "poll",
)

var customDeclarationMethods = customStringSet("POST")

// customDeclarationHeaders 是允许出现的 header 白名单（小写名 → 规范名）。
var customDeclarationHeaders = map[string]string{
	"accept":       "Accept",
	"content-type": "Content-Type",
}

var (
	customSimpleNameRe          = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
	customPathPlaceholderRe     = regexp.MustCompile(`\{[^{}]+\}`)
	customTemplatePlaceholderRe = regexp.MustCompile(`\{[A-Za-z][A-Za-z0-9_-]*\}`)
	customJSONPointerTokenRe    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
	customJSONPointerIndexRe    = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
)

var customPollFields = customStringSet(
	"path", "job_id", "status", "done_value", "failed_value", "result",
	"artifact_url", "headers", "interval_seconds", "timeout_seconds",
)

// customBodyTemplateInputs 是允许出现在 body_template 中的声明式输入名。
var customBodyTemplateInputs = customStringSet(
	"model", "prompt", "system_prompt", "max_output_tokens", "aspect_ratio", "image_size",
	"seed", "reference_images", "text", "voice", "language_type", "speed", "duration",
	"duration_seconds", "resolution", "start_image", "end_image", "reference_audio_files", "generate_audio",
)

var customPollPointerPlaceholders = customStringSet("job_id", "model")

const (
	customMaxTemplateDepth        = 32
	customDefaultPollIntervalSecs = 2.0
	customDefaultPollTimeoutSecs  = 300.0
	customMinPollIntervalSecs     = 0.05
	customMaxPollIntervalSecs     = 60.0
	customMaxPollTimeoutSecs      = 3600.0
)

func customStringSet(items ...string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}

// ── 路径 / pointer 校验 ────────────────────────────────────────────

func customValidatePath(raw any, field string, placeholders map[string]bool, restrictPlaceholders, allowQuery bool) (string, error) {
	path, ok := raw.(string)
	if !ok || path == "" {
		return "", customDeclarationError(field, "must be a non-empty relative path")
	}
	parsed, err := url.Parse(path)
	if err != nil {
		return "", customDeclarationError(field, "must be an absolute-path reference without a URL or query")
	}
	if !strings.HasPrefix(path, "/") || parsed.Scheme != "" || parsed.Host != "" ||
		parsed.Fragment != "" || (parsed.RawQuery != "" && !allowQuery) {
		return "", customDeclarationError(field, "must be an absolute-path reference without a URL or query")
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return "", customDeclarationError(field, "must not contain path traversal")
		}
	}
	for _, placeholder := range customPathPlaceholderRe.FindAllString(path, -1) {
		name := placeholder[1 : len(placeholder)-1]
		if !customSimpleNameRe.MatchString(name) {
			return "", customDeclarationError(field, "placeholder names must be simple field names")
		}
		if restrictPlaceholders && !placeholders[name] {
			return "", customDeclarationError(field, "only the "+customAllowedPlaceholders(placeholders)+" placeholder(s) are allowed")
		}
	}
	return path, nil
}

func customAllowedPlaceholders(placeholders map[string]bool) string {
	names := make([]string, 0, len(placeholders))
	for name := range placeholders {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, "{"+name+"}")
	}
	return strings.Join(parts, ", ")
}

func customValidateObjectPointer(raw any, field string, allowArray bool, placeholders map[string]bool) (string, error) {
	pointer, ok := raw.(string)
	if !ok || !strings.HasPrefix(pointer, "/") {
		return "", customDeclarationError(field, "must be a JSON Pointer-like path starting with '/'")
	}
	tokens := strings.Split(pointer[1:], "/")
	if len(tokens) == 0 {
		return "", customDeclarationError(field, "must not contain empty path components")
	}
	for _, token := range tokens {
		if token == "" {
			return "", customDeclarationError(field, "must not contain empty path components")
		}
		if customHasUnsupportedTilde(token) {
			return "", customDeclarationError(field, "contains an unsupported path component")
		}
		decoded := strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		if customTemplatePlaceholderRe.MatchString(decoded) && customTemplatePlaceholderRe.FindString(decoded) == decoded {
			placeholder := decoded[1 : len(decoded)-1]
			if !placeholders[placeholder] {
				allowed := customAllowedPlaceholders(placeholders)
				if allowed == "" {
					allowed = "none"
				}
				return "", customDeclarationError(field, "placeholder {"+placeholder+"} is not allowed; allowed: "+allowed)
			}
			decoded = "job_id"
		}
		if allowArray {
			if !customJSONPointerTokenRe.MatchString(decoded) && !customJSONPointerIndexRe.MatchString(decoded) {
				return "", customDeclarationError(field, "contains an unsupported response path component")
			}
		} else if !customJSONPointerTokenRe.MatchString(decoded) {
			return "", customDeclarationError(field, "body paths only support named object fields")
		}
	}
	return pointer, nil
}

// customHasUnsupportedTilde 检测 RE2 不支持 lookahead 的 `~(?![01])`：出现非 ~0 / ~1 的转义。
func customHasUnsupportedTilde(token string) bool {
	for i := 0; i < len(token); i++ {
		if token[i] != '~' {
			continue
		}
		if i+1 >= len(token) || (token[i+1] != '0' && token[i+1] != '1') {
			return true
		}
	}
	return false
}

// ── header / mapping / template 校验 ───────────────────────────────

func customToAnyMap(raw any) (map[string]any, bool) {
	switch typed := raw.(type) {
	case map[string]any:
		return typed, true
	case map[string]string:
		out := make(map[string]any, len(typed))
		for key, value := range typed {
			out[key] = value
		}
		return out, true
	default:
		return nil, false
	}
}

func customValidateHeaders(raw any) (map[string]string, error) {
	mapping, ok := customToAnyMap(raw)
	if !ok {
		return nil, customDeclarationError("header", "must be an object")
	}
	headers := map[string]string{}
	for name, value := range mapping {
		canonical := customDeclarationHeaders[strings.ToLower(name)]
		if canonical == "" {
			return nil, customDeclarationError("header", fmt.Sprintf("header %q is not declared in the allowlist", name))
		}
		valueStr, isStr := value.(string)
		if !isStr || strings.ContainsAny(valueStr, "\r\n") {
			return nil, customDeclarationError("header", "must be a static string without line breaks")
		}
		headers[canonical] = valueStr
	}
	return headers, nil
}

func customValidateMapping(raw any, field string, response bool) (map[string]string, error) {
	mapping, ok := customToAnyMap(raw)
	if !ok {
		return nil, customDeclarationError(field, "must be an object")
	}
	result := map[string]string{}
	seenValues := map[string]bool{}
	keys := make([]string, 0, len(mapping))
	for key := range mapping {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := mapping[key]
		var pointer string
		var err error
		if response {
			if !customSimpleNameRe.MatchString(key) {
				return nil, customDeclarationError(field, "response names must be simple field names")
			}
			pointer, err = customValidateObjectPointer(value, field, true, nil)
			if err != nil {
				return nil, err
			}
		} else {
			pointer, err = customValidateObjectPointer(key, field, false, nil)
			if err != nil {
				return nil, err
			}
			valueStr, isStr := value.(string)
			if !isStr || !customSimpleNameRe.MatchString(valueStr) {
				return nil, customDeclarationError(field, "body values must be simple input names")
			}
		}
		if seenValues[pointer] {
			return nil, customDeclarationError(field, "contains duplicate mapping paths")
		}
		if response {
			result[key] = pointer
			seenValues[pointer] = true
		} else {
			valueStr, _ := value.(string)
			result[key] = valueStr
			seenValues[valueStr] = true
		}
	}
	if !response {
		paths := make([]string, 0, len(result))
		for path := range result {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, parent := range paths {
			for _, child := range paths {
				if child != parent && strings.HasPrefix(child, parent+"/") {
					return nil, customDeclarationError(field, "cannot map both a field and one of its children")
				}
			}
		}
	}
	return result, nil
}

func customIterTemplatePlaceholders(value, field string) ([]string, error) {
	var names []string
	index := 0
	for index < len(value) {
		if value[index] != '{' {
			index++
			continue
		}
		end := strings.IndexByte(value[index+1:], '}')
		if end == -1 {
			return nil, customDeclarationError(field, "contains an unclosed template placeholder")
		}
		end += index + 1
		name := value[index+1 : end]
		if !customSimpleNameRe.MatchString(name) {
			return nil, customDeclarationError(field, "placeholder names must be simple field names")
		}
		names = append(names, name)
		index = end + 1
	}
	return names, nil
}

func customValidateTemplate(value any, field string, depth int) (any, error) {
	if depth > customMaxTemplateDepth {
		return nil, customDeclarationError(field, "nesting is too deep")
	}
	switch typed := value.(type) {
	case string:
		names, err := customIterTemplatePlaceholders(typed, field)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if !customBodyTemplateInputs[name] {
				return nil, customDeclarationError(field, "placeholder {"+name+"} is not an allowed input")
			}
		}
		return typed, nil
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			if key == "" {
				return nil, customDeclarationError(field, "object keys must be non-empty strings")
			}
			validated, err := customValidateTemplate(item, field, depth+1)
			if err != nil {
				return nil, err
			}
			result[key] = validated
		}
		return result, nil
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			validated, err := customValidateTemplate(item, field, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, validated)
		}
		return result, nil
	case bool, int, int64, float64, nil:
		return value, nil
	default:
		return nil, customDeclarationError(field, "must contain only JSON-compatible values")
	}
}

func customValidateScalar(value any, field string) error {
	switch value.(type) {
	case string, bool, int, int64, float64, nil:
		return nil
	default:
		return customDeclarationError(field, "must be a scalar JSON value")
	}
}

// ── poll 校验 ──────────────────────────────────────────────────────

func customValidatePoll(raw any) (*CustomEndpointPoll, error) {
	if raw == nil {
		return nil, nil
	}
	mapping, ok := customToAnyMap(raw)
	if !ok {
		return nil, customDeclarationError("poll", "must be an object")
	}
	var unknown []string
	for key := range mapping {
		if !customPollFields[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, customDeclarationError("poll", fmt.Sprintf("unknown fields: %v", unknown))
	}

	path, err := customValidatePath(
		mapping["path"], "poll.path", customStringSet("model", "job_id"), true, false,
	)
	if err != nil {
		return nil, err
	}
	jobID, err := customValidateObjectPointer(mapping["job_id"], "poll.job_id", true, customPollPointerPlaceholders)
	if err != nil {
		return nil, err
	}
	status, err := customValidateObjectPointer(mapping["status"], "poll.status", true, customPollPointerPlaceholders)
	if err != nil {
		return nil, err
	}
	doneValue := mapping["done_value"]
	if err := customValidateScalar(doneValue, "poll.done_value"); err != nil {
		return nil, err
	}
	if doneValue == nil {
		return nil, customDeclarationError("poll.done_value", "is required and must not be null")
	}
	failedValue := mapping["failed_value"]
	if err := customValidateScalar(failedValue, "poll.failed_value"); err != nil {
		return nil, err
	}
	var result string
	hasResult := false
	if rawResult, ok := mapping["result"]; ok && rawResult != nil {
		result, err = customValidateObjectPointer(rawResult, "poll.result", true, customPollPointerPlaceholders)
		if err != nil {
			return nil, err
		}
		hasResult = true
	}
	var artifactURL string
	hasArtifactURL := false
	if rawArtifact, ok := mapping["artifact_url"]; ok && rawArtifact != nil {
		artifactURL, err = customValidatePath(rawArtifact, "poll.artifact_url", nil, false, true)
		if err != nil {
			return nil, err
		}
		hasArtifactURL = true
	}
	rawHeaders := any(map[string]any{})
	if headers, ok := mapping["headers"]; ok {
		rawHeaders = headers
	}
	headers, err := customValidateHeaders(rawHeaders)
	if err != nil {
		return nil, err
	}
	interval := any(customDefaultPollIntervalSecs)
	if value, ok := mapping["interval_seconds"]; ok {
		interval = value
	}
	timeout := any(customDefaultPollTimeoutSecs)
	if value, ok := mapping["timeout_seconds"]; ok {
		timeout = value
	}
	intervalSeconds, ok := customToFloat(interval)
	if !ok {
		return nil, customDeclarationError("poll.interval_seconds", "must be a number")
	}
	timeoutSeconds, ok := customToFloat(timeout)
	if !ok {
		return nil, customDeclarationError("poll.timeout_seconds", "must be a number")
	}
	if intervalSeconds < customMinPollIntervalSecs || intervalSeconds > customMaxPollIntervalSecs {
		return nil, customDeclarationError(
			"poll.interval_seconds",
			fmt.Sprintf("must be between %v and %v seconds", customMinPollIntervalSecs, customMaxPollIntervalSecs),
		)
	}
	if intervalSeconds > timeoutSeconds || timeoutSeconds > customMaxPollTimeoutSecs {
		return nil, customDeclarationError(
			"poll.timeout_seconds",
			fmt.Sprintf("must be between the poll interval and %v seconds", customMaxPollTimeoutSecs),
		)
	}
	return &CustomEndpointPoll{
		Path:            path,
		JobID:           jobID,
		Status:          status,
		DoneValue:       doneValue,
		FailedValue:     failedValue,
		HasFailedValue:  failedValue != nil,
		Result:          result,
		HasResult:       hasResult,
		ArtifactURL:     artifactURL,
		HasArtifactURL:  hasArtifactURL,
		Headers:         headers,
		IntervalSeconds: intervalSeconds,
		TimeoutSeconds:  timeoutSeconds,
	}, nil
}

func customToFloat(value any) (float64, bool) {
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

// ── 声明 validate / parse ─────────────────────────────────────────

// ValidateCustomEndpointDeclaration 校验一个已解析的声明并原样返回。
func ValidateCustomEndpointDeclaration(declaration *CustomEndpointDeclaration) error {
	if declaration == nil {
		return customDeclarationError("declaration", "must be an EndpointDeclaration")
	}
	if !customDeclarationMethods[declaration.Method] {
		return customDeclarationError("method", fmt.Sprintf("method %q is not allowed", declaration.Method))
	}
	if _, err := customValidatePath(declaration.Path, "path", nil, false, false); err != nil {
		return err
	}
	if _, err := customValidateHeaders(declaration.Headers); err != nil {
		return err
	}
	if _, err := customValidateMapping(declaration.BodyMapping, "body", false); err != nil {
		return err
	}
	if _, err := customValidateMapping(declaration.ResponseMapping, "response", true); err != nil {
		return err
	}
	if len(declaration.BodyMapping) > 0 && declaration.HasBodyTemplate {
		return customDeclarationError("body_template", "body and body_template are mutually exclusive")
	}
	if declaration.HasBodyTemplate {
		if _, err := customValidateTemplate(declaration.BodyTemplate, "body_template", 0); err != nil {
			return err
		}
	}
	if declaration.Poll != nil {
		if _, err := customValidatePoll(customPollToMap(declaration.Poll)); err != nil {
			return err
		}
	}
	for key, value := range declaration.CapabilityOverrides {
		if !customSimpleNameRe.MatchString(key) {
			return customDeclarationError("capability_overrides", "capability names must be simple field names")
		}
		if err := customValidateScalar(value, "capability_overrides"); err != nil {
			return customDeclarationError("capability_overrides", "values must be scalar JSON values")
		}
	}
	return nil
}

func customPollToMap(poll *CustomEndpointPoll) map[string]any {
	out := map[string]any{
		"path":             poll.Path,
		"job_id":           poll.JobID,
		"status":           poll.Status,
		"done_value":       poll.DoneValue,
		"interval_seconds": poll.IntervalSeconds,
		"timeout_seconds":  poll.TimeoutSeconds,
	}
	if poll.HasFailedValue {
		out["failed_value"] = poll.FailedValue
	} else {
		out["failed_value"] = nil
	}
	if poll.HasResult {
		out["result"] = poll.Result
	}
	if poll.HasArtifactURL {
		out["artifact_url"] = poll.ArtifactURL
	}
	if poll.Headers != nil {
		out["headers"] = poll.Headers
	}
	return out
}

// ParseCustomEndpointDeclaration 解析并校验一份纯数据声明。
func ParseCustomEndpointDeclaration(raw map[string]any) (*CustomEndpointDeclaration, error) {
	if raw == nil {
		return nil, customDeclarationError("declaration", "must be an object")
	}
	var unknown []string
	for key := range raw {
		if !customDeclarationFields[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, customDeclarationError("declaration", fmt.Sprintf("unknown fields: %v", unknown))
	}
	method := any("POST")
	if value, ok := raw["method"]; ok {
		method = value
	}
	methodStr, ok := method.(string)
	if !ok {
		return nil, customDeclarationError("method", "must be a string")
	}
	methodStr = strings.ToUpper(methodStr)
	if !customDeclarationMethods[methodStr] {
		return nil, customDeclarationError("method", fmt.Sprintf("method %q is not allowed", methodStr))
	}
	path, err := customValidatePath(raw["path"], "path", customStringSet("model"), true, false)
	if err != nil {
		return nil, err
	}
	bodyRaw := any(map[string]any{})
	if value, ok := raw["body"]; ok {
		bodyRaw = value
	}
	bodyTemplateRaw, hasBodyTemplateRaw := raw["body_template"]
	bodyMap, _ := customToAnyMap(bodyRaw)
	if len(bodyMap) > 0 && hasBodyTemplateRaw && bodyTemplateRaw != nil {
		return nil, customDeclarationError("body_template", "body and body_template are mutually exclusive")
	}
	var bodyTemplate map[string]any
	hasBodyTemplate := false
	if hasBodyTemplateRaw && bodyTemplateRaw != nil {
		tmpl, ok := customToAnyMap(bodyTemplateRaw)
		if !ok {
			return nil, customDeclarationError("body_template", "must be an object")
		}
		if _, err := customValidateTemplate(tmpl, "body_template", 0); err != nil {
			return nil, err
		}
		bodyTemplate = tmpl
		hasBodyTemplate = true
	}

	headers, err := customValidateHeaders(customOrDefault(raw, "headers", map[string]any{}))
	if err != nil {
		return nil, err
	}
	bodyMapping, err := customValidateMapping(bodyRaw, "body", false)
	if err != nil {
		return nil, err
	}
	responseMapping, err := customValidateMapping(customOrDefault(raw, "response", map[string]any{}), "response", true)
	if err != nil {
		return nil, err
	}
	capabilityOverrides := map[string]any{}
	if rawOverrides, ok := raw["capability_overrides"]; ok {
		if mapped, isMap := customToAnyMap(rawOverrides); isMap {
			capabilityOverrides = mapped
		}
	}
	poll, err := customValidatePoll(raw["poll"])
	if err != nil {
		return nil, err
	}
	if rawOverrides, ok := raw["capability_overrides"]; ok && rawOverrides != nil {
		if _, isMap := customToAnyMap(rawOverrides); !isMap {
			return nil, customDeclarationError("capability_overrides", "must be an object")
		}
	}

	declaration := &CustomEndpointDeclaration{
		Method:              methodStr,
		Path:                path,
		Headers:             headers,
		BodyMapping:         bodyMapping,
		ResponseMapping:     responseMapping,
		CapabilityOverrides: capabilityOverrides,
		BodyTemplate:        bodyTemplate,
		HasBodyTemplate:     hasBodyTemplate,
		Poll:                poll,
	}
	if err := ValidateCustomEndpointDeclaration(declaration); err != nil {
		return nil, err
	}
	return declaration, nil
}

func customOrDefault(raw map[string]any, key string, fallback any) any {
	if value, ok := raw[key]; ok {
		return value
	}
	return fallback
}

// ── 渲染 ───────────────────────────────────────────────────────────

// customQuote 对字符串做 Python urllib.parse.quote(safe="-._~") 等价的百分号编码。
func customQuote(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '-' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
}

func customRenderPath(path string, inputs map[string]any, allowed map[string]bool) (string, error) {
	var renderErr error
	rendered := customPathPlaceholderRe.ReplaceAllStringFunc(path, func(match string) string {
		if renderErr != nil {
			return match
		}
		name := match[1 : len(match)-1]
		if !allowed[name] {
			renderErr = customDeclarationError("path", "placeholder {"+name+"} is not allowed")
			return match
		}
		value, ok := inputs[name].(string)
		if !ok || value == "" {
			renderErr = customDeclarationError("input", "missing non-empty "+name+" value")
			return match
		}
		return customQuote(value)
	})
	if renderErr != nil {
		return "", renderErr
	}
	return rendered, nil
}

// customScalarString 把内联插值的标量转成字符串，bool 按 Python str() 输出 True/False。
func customScalarString(value any) string {
	switch typed := value.(type) {
	case bool:
		if typed {
			return "True"
		}
		return "False"
	default:
		return fmt.Sprint(value)
	}
}

func customRenderTemplate(value any, inputs map[string]any, depth int) (any, error) {
	if depth > customMaxTemplateDepth {
		return nil, customDeclarationError("body_template", "nesting is too deep")
	}
	switch typed := value.(type) {
	case string:
		if customTemplatePlaceholderRe.MatchString(typed) && customTemplatePlaceholderRe.FindString(typed) == typed {
			name := typed[1 : len(typed)-1]
			item, ok := inputs[name]
			if !ok {
				return nil, customDeclarationError("input", fmt.Sprintf("missing input %q", name))
			}
			return item, nil
		}
		var renderErr error
		rendered := customTemplatePlaceholderRe.ReplaceAllStringFunc(typed, func(match string) string {
			if renderErr != nil {
				return match
			}
			name := match[1 : len(match)-1]
			item, ok := inputs[name]
			if !ok {
				renderErr = customDeclarationError("input", fmt.Sprintf("missing input %q", name))
				return match
			}
			switch item.(type) {
			case map[string]any, []any, nil:
				renderErr = customDeclarationError("input", fmt.Sprintf("input %q must be a scalar for inline interpolation", name))
				return match
			}
			return customScalarString(item)
		})
		if renderErr != nil {
			return nil, renderErr
		}
		return rendered, nil
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			rendered, err := customRenderTemplate(item, inputs, depth+1)
			if err != nil {
				return nil, err
			}
			result[key] = rendered
		}
		return result, nil
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			rendered, err := customRenderTemplate(item, inputs, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, rendered)
		}
		return result, nil
	default:
		return value, nil
	}
}

func customSetBodyValue(body map[string]any, pointer string, value any) error {
	tokens := strings.Split(pointer[1:], "/")
	for i := range tokens {
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(tokens[i], "~1", "/"), "~0", "~")
	}
	current := body
	for _, token := range tokens[:len(tokens)-1] {
		child, exists := current[token]
		if !exists || child == nil {
			next := map[string]any{}
			current[token] = next
			current = next
			continue
		}
		mapped, ok := child.(map[string]any)
		if !ok {
			return customDeclarationError("body", "body path collides with a scalar")
		}
		current = mapped
	}
	current[tokens[len(tokens)-1]] = value
	return nil
}

// RenderCustomEndpointDeclaration 用声明显式给出的字段渲染请求。
func RenderCustomEndpointDeclaration(declaration *CustomEndpointDeclaration, inputs map[string]any) (RenderedCustomEndpointRequest, error) {
	if err := ValidateCustomEndpointDeclaration(declaration); err != nil {
		return RenderedCustomEndpointRequest{}, err
	}
	if _, ok := inputs["headers"]; ok {
		return RenderedCustomEndpointRequest{}, customDeclarationError("header", "runtime headers are not permitted")
	}
	body := map[string]any{}
	if declaration.HasBodyTemplate {
		rendered, err := customRenderTemplate(declaration.BodyTemplate, inputs, 0)
		if err != nil {
			return RenderedCustomEndpointRequest{}, err
		}
		renderedMap, ok := rendered.(map[string]any)
		if !ok {
			return RenderedCustomEndpointRequest{}, customDeclarationError("body_template", "must render to a JSON object")
		}
		body = renderedMap
	} else {
		for pointer, inputName := range declaration.BodyMapping {
			value, ok := inputs[inputName]
			if !ok {
				return RenderedCustomEndpointRequest{}, customDeclarationError("input", fmt.Sprintf("missing input %q", inputName))
			}
			if err := customSetBodyValue(body, pointer, value); err != nil {
				return RenderedCustomEndpointRequest{}, err
			}
		}
	}
	path, err := customRenderPath(declaration.Path, inputs, customStringSet("model"))
	if err != nil {
		return RenderedCustomEndpointRequest{}, err
	}
	return RenderedCustomEndpointRequest{
		Method:  declaration.Method,
		Path:    path,
		Headers: customCopyStringMap(declaration.Headers),
		Body:    body,
	}, nil
}

// RenderCustomEndpointPoll 渲染声明里的 poll 路径与 headers（无 poll 时返回 nil）。
func RenderCustomEndpointPoll(declaration *CustomEndpointDeclaration, inputs map[string]any) (*RenderedCustomEndpointPoll, error) {
	if err := ValidateCustomEndpointDeclaration(declaration); err != nil {
		return nil, err
	}
	if declaration.Poll == nil {
		return nil, nil
	}
	path, err := customRenderPath(declaration.Poll.Path, inputs, customStringSet("model", "job_id"))
	if err != nil {
		return nil, err
	}
	return &RenderedCustomEndpointPoll{Path: path, Headers: customCopyStringMap(declaration.Poll.Headers)}, nil
}

// RenderCustomPollPointer 替换 poll JSON pointer 中允许的占位符，转义 pointer token。
func RenderCustomPollPointer(pointer string, inputs map[string]any) (string, error) {
	var renderErr error
	rendered := customTemplatePlaceholderRe.ReplaceAllStringFunc(pointer, func(match string) string {
		if renderErr != nil {
			return match
		}
		name := match[1 : len(match)-1]
		if !customPollPointerPlaceholders[name] {
			renderErr = customDeclarationError("poll", "placeholder {"+name+"} is not allowed")
			return match
		}
		value, ok := inputs[name].(string)
		if !ok || value == "" {
			renderErr = customDeclarationError("input", "missing non-empty "+name+" value")
			return match
		}
		return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
	})
	if renderErr != nil {
		return "", renderErr
	}
	return rendered, nil
}

// RenderCustomEndpointArtifactURL 从已完成的 poll 结果渲染产物 URL 模板。
func RenderCustomEndpointArtifactURL(poll *CustomEndpointPoll, result any) (string, bool, error) {
	if poll == nil || !poll.HasArtifactURL {
		return "", false, nil
	}
	inputs := map[string]any{}
	switch typed := result.(type) {
	case map[string]any:
		for key, value := range typed {
			inputs[key] = value
		}
	case string:
		inputs["result"] = typed
	}
	var renderErr error
	rendered := customTemplatePlaceholderRe.ReplaceAllStringFunc(poll.ArtifactURL, func(match string) string {
		if renderErr != nil {
			return match
		}
		name := match[1 : len(match)-1]
		value := inputs[name]
		switch typed := value.(type) {
		case string:
			return customQuote(typed)
		case int:
			return customQuote(strconv.Itoa(typed))
		case int64:
			return customQuote(strconv.FormatInt(typed, 10))
		case float64:
			return customQuote(strconv.FormatFloat(typed, 'g', -1, 64))
		default:
			renderErr = customDeclarationError("poll.artifact_url", fmt.Sprintf("artifact URL is missing scalar %q", name))
			return match
		}
	})
	if renderErr != nil {
		return "", false, renderErr
	}
	return rendered, true, nil
}

// ── JSON pointer / 响应归一化 ─────────────────────────────────────

// GetCustomJSONPointer 从供应商文档中读取一个 JSON Pointer 形态的路径。
func GetCustomJSONPointer(document any, pointer string) (any, error) {
	return customPointerGet(document, pointer)
}

func customPointerGet(document any, pointer string) (any, error) {
	current := document
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("pointer must start with '/': %q", pointer)
	}
	for _, rawToken := range strings.Split(pointer[1:], "/") {
		token := strings.ReplaceAll(strings.ReplaceAll(rawToken, "~1", "/"), "~0", "~")
		switch typed := current.(type) {
		case map[string]any:
			value, ok := typed[token]
			if !ok {
				return nil, fmt.Errorf("key %q not found", token)
			}
			current = value
		case []any:
			if !customJSONPointerIndexRe.MatchString(token) {
				return nil, fmt.Errorf("key %q not found", token)
			}
			index, _ := strconv.Atoi(token)
			if index >= len(typed) {
				return nil, fmt.Errorf("index %d out of range", index)
			}
			current = typed[index]
		default:
			return nil, fmt.Errorf("key %q not found", token)
		}
	}
	return current, nil
}

// NormalizeCustomEndpointResponse 从供应商 JSON 文档中抽取声明映射的响应字段。
func NormalizeCustomEndpointResponse(declaration *CustomEndpointDeclaration, response any) (map[string]any, error) {
	if err := ValidateCustomEndpointDeclaration(declaration); err != nil {
		return nil, err
	}
	normalized := map[string]any{}
	for name, pointer := range declaration.ResponseMapping {
		value, err := customPointerGet(response, pointer)
		if err != nil {
			return nil, customDeclarationError("response", fmt.Sprintf("missing response field at %q", pointer))
		}
		normalized[name] = value
	}
	return normalized, nil
}

func customCopyStringMap(source map[string]string) map[string]string {
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

// ── ENDPOINT_REGISTRY 注册表 ──────────────────────────────────────

// customEndpointRegistry 是 endpoint 键 → spec 的单一真相源。
var customEndpointRegistry = map[string]customEndpointSpec{
	"openai-chat": {
		key: "openai-chat", mediaType: provider.MediaText, family: "openai",
		displayNameKey: "endpoint_openai_chat_display",
		requestMethod:  "POST", requestPathTemplate: "/v1/chat/completions",
		source: "builtin",
	},
	"gemini-generate": {
		key: "gemini-generate", mediaType: provider.MediaText, family: "google",
		displayNameKey: "endpoint_gemini_generate_display",
		requestMethod:  "POST", requestPathTemplate: "/v1beta/models/{model}:generateContent",
		source: "builtin",
	},
	"openai-images": {
		key: "openai-images", mediaType: provider.MediaImage, family: "openai",
		displayNameKey: "endpoint_openai_images_display",
		requestMethod:  "POST", requestPathTemplate: "/v1/images/{generations,edits}",
		imageCapabilities:    provider.NewImageCapabilitySet(provider.CapTextToImage, provider.CapImageToImage),
		hasImageCapabilities: true,
		source:               "builtin",
		buildImage:           customBuildOpenAIImages,
	},
	"openai-images-generations": {
		key: "openai-images-generations", mediaType: provider.MediaImage, family: "openai",
		displayNameKey: "endpoint_openai_images_generations_display",
		requestMethod:  "POST", requestPathTemplate: "/v1/images/generations",
		imageCapabilities:    provider.NewImageCapabilitySet(provider.CapTextToImage),
		hasImageCapabilities: true,
		source:               "builtin",
		buildImage:           customBuildOpenAIImagesGenerations,
	},
	"openai-images-edits": {
		key: "openai-images-edits", mediaType: provider.MediaImage, family: "openai",
		displayNameKey: "endpoint_openai_images_edits_display",
		requestMethod:  "POST", requestPathTemplate: "/v1/images/edits",
		imageCapabilities:    provider.NewImageCapabilitySet(provider.CapImageToImage),
		hasImageCapabilities: true,
		source:               "builtin",
		buildImage:           customBuildOpenAIImagesEdits,
	},
	"gemini-image": {
		key: "gemini-image", mediaType: provider.MediaImage, family: "google",
		displayNameKey: "endpoint_gemini_image_display",
		requestMethod:  "POST", requestPathTemplate: "/v1beta/models/{model}:generateContent",
		imageCapabilities:    provider.NewImageCapabilitySet(provider.CapTextToImage, provider.CapImageToImage),
		hasImageCapabilities: true,
		source:               "builtin",
		buildImage:           customBuildGeminiImage,
	},
	"openai-video": {
		key: "openai-video", mediaType: provider.MediaVideo, family: "openai",
		displayNameKey: "endpoint_openai_video_display",
		requestMethod:  "POST", requestPathTemplate: "/v1/videos",
		videoMaxReferenceImages: customIntPtr(1),
		source:                  "builtin",
		buildVideo:              customBuildOpenAIVideo,
	},
	"newapi-video": {
		key: "newapi-video", mediaType: provider.MediaVideo, family: "newapi",
		displayNameKey: "endpoint_newapi_video_display",
		requestMethod:  "POST", requestPathTemplate: "/v1/video/generations",
		videoMaxReferenceImages: customIntPtr(0),
		source:                  "builtin",
		buildVideo:              customBuildNewAPIVideo,
	},
	"v2-video-generations": {
		key: "v2-video-generations", mediaType: provider.MediaVideo, family: "v2",
		displayNameKey: "endpoint_v2_video_generations_display",
		requestMethod:  "POST", requestPathTemplate: "/v2/video/generations",
		videoCapsForModel: customCapsFunc(v2VideoGenerationsCapabilitiesForModel),
		endImageCapable:   true,
		source:            "builtin",
		buildVideo:        customBuildV2VideoGenerations,
	},
	"ark-seedance": {
		key: "ark-seedance", mediaType: provider.MediaVideo, family: "ark",
		displayNameKey: "endpoint_ark_seedance_display",
		requestMethod:  "POST", requestPathTemplate: "/api/v3/contents/generations/tasks",
		videoCapsForModel:     customCapsFunc(ark.VideoCapabilitiesForModel),
		endImageCapable:       true,
		referenceAudioCapable: true,
		source:                "builtin",
		buildVideo:            customBuildArkSeedance,
	},
	"vidu-video": {
		key: "vidu-video", mediaType: provider.MediaVideo, family: "vidu",
		displayNameKey: "endpoint_vidu_video_display",
		requestMethod:  "POST", requestPathTemplate: "/ent/v2/img2video",
		videoCapsForModel: customCapsFunc(vidu.VideoCapabilitiesForModel),
		endImageCapable:   true,
		source:            "builtin",
		buildVideo:        customBuildViduVideo,
	},
	"dashscope-image": {
		key: "dashscope-image", mediaType: provider.MediaImage, family: "dashscope",
		displayNameKey: "endpoint_dashscope_image_display",
		requestMethod:  "POST", requestPathTemplate: "/api/v1/services/aigc/multimodal-generation/generation",
		imageCapabilities:    provider.NewImageCapabilitySet(provider.CapTextToImage, provider.CapImageToImage),
		hasImageCapabilities: true,
		source:               "builtin",
		buildImage:           customBuildDashscopeImage,
	},
	"openai-tts": {
		key: "openai-tts", mediaType: provider.MediaAudio, family: "openai",
		displayNameKey: "endpoint_openai_tts_display",
		requestMethod:  "POST", requestPathTemplate: "/v1/audio/speech",
		source:     "builtin",
		buildAudio: customBuildOpenAIAudio,
	},
	"dashscope-async-video": {
		key: "dashscope-async-video", mediaType: provider.MediaVideo, family: "dashscope",
		displayNameKey: "endpoint_dashscope_async_video_display",
		requestMethod:  "POST", requestPathTemplate: "/api/v1/services/aigc/video-generation/video-synthesis",
		videoCapsForModel:     customCapsFunc(dashscope.ProfileForModel),
		referenceAudioCapable: true,
		source:                "builtin",
		buildVideo:            customBuildDashscopeVideo,
	},
	"minimax-image": {
		key: "minimax-image", mediaType: provider.MediaImage, family: "minimax",
		displayNameKey: "endpoint_minimax_image_display",
		requestMethod:  "POST", requestPathTemplate: "/image_generation",
		imageCapabilities:    provider.NewImageCapabilitySet(provider.CapTextToImage, provider.CapImageToImage),
		hasImageCapabilities: true,
		source:               "builtin",
		buildImage:           customBuildMinimaxImage,
	},
	"minimax-video": {
		key: "minimax-video", mediaType: provider.MediaVideo, family: "minimax",
		displayNameKey: "endpoint_minimax_video_display",
		requestMethod:  "POST", requestPathTemplate: "/video_generation",
		videoCapsForModel: customCapsFuncErrless(minimax.VideoCapabilitiesForModel),
		source:            "builtin",
		buildVideo:        customBuildMinimaxVideo,
	},
	"kling-image": {
		key: "kling-image", mediaType: provider.MediaImage, family: "kling",
		displayNameKey: "endpoint_kling_image_display",
		requestMethod:  "POST", requestPathTemplate: "/v1/images/generations",
		imageCapabilities:    provider.NewImageCapabilitySet(provider.CapTextToImage, provider.CapImageToImage),
		hasImageCapabilities: true,
		source:               "builtin",
		buildImage:           customBuildKlingImage,
	},
	"kling-video": {
		key: "kling-video", mediaType: provider.MediaVideo, family: "kling",
		displayNameKey: "endpoint_kling_video_display",
		requestMethod:  "POST", requestPathTemplate: "/v1/videos/{text2video,image2video,multi-image2video}",
		videoCapsForModel: customCapsFuncErrless(kling.VideoCapabilitiesForModel),
		endImageCapable:   true,
		source:            "builtin",
		buildVideo:        customBuildKlingVideo,
	},
}

func customIntPtr(value int) *int { return &value }

// customCapsFunc 适配无 error 的 per-model caps 纯函数。
func customCapsFunc(fn func(modelID string) provider.VideoCapabilities) func(string) (provider.VideoCapabilities, error) {
	return func(modelID string) (provider.VideoCapabilities, error) { return fn(modelID), nil }
}

// customCapsFuncErrless 同上，供签名已返回 error 的函数直接使用。
func customCapsFuncErrless(fn func(modelID string) (provider.VideoCapabilities, error)) func(string) (provider.VideoCapabilities, error) {
	return fn
}

// CustomEndpointSpecFor 返回 endpoint 的规格；未知 endpoint 报错（fail loud）。
func CustomEndpointSpecFor(endpoint string) (customEndpointSpec, error) {
	spec, ok := customEndpointRegistry[endpoint]
	if !ok {
		return customEndpointSpec{}, fmt.Errorf("unknown endpoint: %q", endpoint)
	}
	return spec, nil
}

// CustomEndpointMediaType 返回 endpoint 的媒体类型；未知 endpoint 报错。
func CustomEndpointMediaType(endpoint string) (provider.MediaType, error) {
	spec, err := CustomEndpointSpecFor(endpoint)
	if err != nil {
		return "", err
	}
	return spec.mediaType, nil
}

// customValidateEndpointRegistry 复刻 Python import 期不变式校验，misconfig 立即 panic。
func customValidateEndpointRegistry() {
	for key, spec := range customEndpointRegistry {
		hasInt := spec.videoMaxReferenceImages != nil
		hasFn := spec.videoCapsForModel != nil
		if spec.mediaType != provider.MediaVideo && spec.referenceAudioCapable {
			panic(fmt.Sprintf("non-video endpoint %q must not declare reference_audio_capable", key))
		}
		if spec.mediaType == provider.MediaVideo {
			if hasInt == hasFn {
				panic(fmt.Sprintf(
					"video endpoint %q must declare exactly one of video_max_reference_images or video_caps_for_model", key,
				))
			}
			if spec.videoMaxReferenceImages != nil && *spec.videoMaxReferenceImages < 0 {
				panic(fmt.Sprintf("video endpoint %q declares negative video_max_reference_images", key))
			}
		} else if hasInt || hasFn {
			panic(fmt.Sprintf("non-video endpoint %q must not declare video caps", key))
		}
	}
}

func init() {
	customValidateEndpointRegistry()
}
