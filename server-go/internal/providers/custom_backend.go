package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// 自定义供应商 backend 包装层与工厂（端口自 lib/custom_provider/backends.py + factory.py）。
//
// 一个自定义供应商（provider_id 形如 custom-<db_id>）按 endpoint 查表：无声明时转发给内置协议
// backend（delegate），有声明时按 EndpointDeclaration 渲染请求。包装层只做三件事——覆写 name
// （=provider_id）与 model（=用户选的 model_id）、记住构造 endpoint、承载合并后的视频能力。
//
// 文本侧（CustomTextBackend / DeclarativeTextDelegate）随文本 backend 契约一并延后：文本
// endpoint 在注册表登记，但工厂按 text endpoint 构造时 fail loud（明确报「text backend not wired」）。
//
// 与 Python 侧的两处已知差异：
//   - 图片请求超时（provider.image_request_timeout_seconds）在 Python 经 _image_request_timeout_kwargs
//     透传给 OpenAIImageBackend；Go 的 NewOpenAIImageBackend 无该构造钩子，故不转发（不由此层兜底
//     默认值，保持“未设置即由 backend 自决”语义）。
//   - JSON 数字在 Go 中统一为 float64，能力覆盖里整数字面量与浮点数字面量无法区分，故 int 维度
//     接受任一整数值的 float64（较 Python 略宽）。

// customProviderConfig 是构造自定义供应商 backend 的输入信封：api_key / base_url 由调用方传入，
// 不从 DB 读，与 Python 侧 create_custom_backend(provider=...) 消费的 provider 属性同义。
type customProviderConfig struct {
	providerID string
	baseURL    string
	apiKey     string
}

// CustomProviderConfig 是 CreateCustomBackend 的导出参数信封。
type CustomProviderConfig struct {
	ProviderID string
	BaseURL    string
	APIKey     string
}

func (c CustomProviderConfig) customConfig() customProviderConfig {
	return customProviderConfig{providerID: c.ProviderID, baseURL: c.BaseURL, apiKey: c.APIKey}
}

// CustomBackend 是工厂的派发结果：MediaType 决定哪个字段非 nil（Go 无 Python 的联合类型）。
type CustomBackend struct {
	MediaType provider.MediaType
	Endpoint  string
	Image     provider.CustomImageBackend
	Video     provider.CustomVideoBackend
	Audio     provider.CustomAudioBackend
}

// ── URL 归一化（复刻 lib/config/url_utils.py 与 endpoints.py 的 _ensure_url_path_suffix）──

var (
	customOpenAIVersionRe = regexp.MustCompile(`/v\d+$`)
	customGoogleVersionRe = regexp.MustCompile(`/v\d+[a-zA-Z]*$`)
)

// customEnsureOpenAIBaseURL 复刻 ensure_openai_base_url：缺版本段则补 /v1。空串短路返回（None 语义）。
func customEnsureOpenAIBaseURL(raw string) string {
	if raw == "" {
		return ""
	}
	stripped := strings.TrimRight(strings.TrimSpace(raw), "/")
	if !customOpenAIVersionRe.MatchString(stripped) {
		stripped += "/v1"
	}
	return stripped
}

// customEnsureGoogleBaseURL 复刻 ensure_google_base_url：剥末尾版本段并补尾部 /。空串返回空串。
func customEnsureGoogleBaseURL(raw string) string {
	if raw == "" {
		return ""
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = strings.TrimRight(s, "/")
	s = customGoogleVersionRe.ReplaceAllString(s, "")
	if !strings.HasSuffix(s, "/") {
		s += "/"
	}
	return s
}

// customEnsureURLPathSuffix 复刻 _ensure_url_path_suffix：补协议已知挂载路径（ark /api/v3、
// vidu /ent/v2、kling /v1），已带对应路径则原样信任。纯域名（无 scheme）先补 https:// 再判定。
func customEnsureURLPathSuffix(baseURL, suffix string) string {
	s := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if s == "" {
		return ""
	}
	normalized := s
	if !strings.Contains(normalized, "://") {
		normalized = "https://" + normalized
	}
	pathSuffix := strings.TrimRight(normalized, "/")
	if parsed, err := url.Parse(normalized); err == nil {
		pathSuffix = strings.TrimRight(parsed.Path, "/")
	}
	if strings.HasSuffix(pathSuffix, suffix) {
		return normalized
	}
	return normalized + suffix
}

// ── 内置 endpoint 的 build_backend 闭包 ────────────────────────────

// 各闭包把已注册的协议 backend 构造器接进来，并复刻 Python 侧各自的 base_url 归一化后再传
// （Go 内置 backend 的归一化与原 builder 不同：ark 不补 /api/v3、vidu 不补 /ent/v2、openai
// 不补 /v1、gemini 不剥版本段；kling 后端内部补 /v1，可幂等）。返回的是协议级 delegate，
// 包装与能力注入由工厂统一完成。

func customBuildOpenAIImages(cfg customProviderConfig, modelID string) (provider.ImageBackend, error) {
	return NewOpenAIImageBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: customEnsureOpenAIBaseURL(cfg.baseURL),
	})
}

func customBuildOpenAIImagesGenerations(cfg customProviderConfig, modelID string) (provider.ImageBackend, error) {
	return NewOpenAIImageBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: customEnsureOpenAIBaseURL(cfg.baseURL),
		Extra:   map[string]string{"mode": "generations_only"},
	})
}

func customBuildOpenAIImagesEdits(cfg customProviderConfig, modelID string) (provider.ImageBackend, error) {
	return NewOpenAIImageBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: customEnsureOpenAIBaseURL(cfg.baseURL),
		Extra:   map[string]string{"mode": "edits_only"},
	})
}

func customBuildGeminiImage(cfg customProviderConfig, modelID string) (provider.ImageBackend, error) {
	return newGeminiImageBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: customEnsureGoogleBaseURL(cfg.baseURL),
		Extra:   map[string]string{"image_model": modelID},
	})
}

func customBuildDashscopeImage(cfg customProviderConfig, modelID string) (provider.ImageBackend, error) {
	// backend 内部由 host 派生 /api/v1（容忍带/不带后缀），传原始 base_url 即可。
	return newDashscopeImageBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: cfg.baseURL,
	})
}

func customBuildMinimaxImage(cfg customProviderConfig, modelID string) (provider.ImageBackend, error) {
	// backend 内部把 base_url 归一化为 {host}/v1（容忍 host 或带 /v1 后缀），传原始 base_url 即可。
	return newMinimaxImageBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: cfg.baseURL,
	})
}

func customBuildKlingImage(cfg customProviderConfig, modelID string) (provider.ImageBackend, error) {
	// bearer 模式旁路 JWT 管理器，用静态 api_key 直发可灵原生异步图像端点。
	return newKlingImageBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: customEnsureURLPathSuffix(cfg.baseURL, "/v1"),
		Extra:   map[string]string{"auth_mode": "bearer"},
	})
}

func customBuildOpenAIVideo(cfg customProviderConfig, modelID string) (provider.VideoBackend, error) {
	return newOpenAIVideoBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: customEnsureOpenAIBaseURL(cfg.baseURL),
	}), nil
}

func customBuildNewAPIVideo(cfg customProviderConfig, modelID string) (provider.VideoBackend, error) {
	baseURL := customEnsureOpenAIBaseURL(cfg.baseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("NewAPI 视频后端需要 base_url")
	}
	return newNewAPIVideoBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: baseURL,
	})
}

func customBuildV2VideoGenerations(cfg customProviderConfig, modelID string) (provider.VideoBackend, error) {
	if strings.TrimSpace(cfg.baseURL) == "" {
		return nil, fmt.Errorf("v2-video-generations 端点需要 base_url")
	}
	// base_url 归一化（去版本段 + 拼 /v2/video/generations）由 backend 内部处理。
	return newV2VideoGenerationsBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: cfg.baseURL,
	})
}

func customBuildArkSeedance(cfg customProviderConfig, modelID string) (provider.VideoBackend, error) {
	return newArkVideoBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: customEnsureURLPathSuffix(cfg.baseURL, "/api/v3"),
	}), nil
}

func customBuildViduVideo(cfg customProviderConfig, modelID string) (provider.VideoBackend, error) {
	return newViduVideoBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: customEnsureURLPathSuffix(cfg.baseURL, "/ent/v2"),
	})
}

func customBuildDashscopeVideo(cfg customProviderConfig, modelID string) (provider.VideoBackend, error) {
	return newDashscopeVideoBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: cfg.baseURL,
	})
}

func customBuildMinimaxVideo(cfg customProviderConfig, modelID string) (provider.VideoBackend, error) {
	return newMinimaxVideoBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: cfg.baseURL,
	})
}

func customBuildKlingVideo(cfg customProviderConfig, modelID string) (provider.VideoBackend, error) {
	return newKlingVideoBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: customEnsureURLPathSuffix(cfg.baseURL, "/v1"),
		Extra:   map[string]string{"auth_mode": "bearer"},
	})
}

func customBuildOpenAIAudio(cfg customProviderConfig, modelID string) (provider.AudioBackend, error) {
	// provider_name 让 delegate 日志与 AudioSynthesisResult.provider 归因到真实 provider，
	// 与包装层 name 的记账身份一致，而非内置 openai。
	return newOpenAIAudioBackend(provider.BackendOptions{
		Model:   modelID,
		APIKey:  cfg.apiKey,
		BaseURL: customEnsureOpenAIBaseURL(cfg.baseURL),
		Extra:   map[string]string{"provider_name": cfg.providerID},
	})
}

// ── 声明式 transport 共享工具 ─────────────────────────────────────

// customDeclarativeHTTPTimeout 是声明式端点单次出站请求的墙钟预算（Python 固定 timeout=120）。
const customDeclarativeHTTPTimeout = 120 * time.Second

func customRequestInputs(model string, values map[string]any) map[string]any {
	inputs := make(map[string]any, len(values)+1)
	inputs["model"] = model
	for name, value := range values {
		inputs[name] = value
	}
	return inputs
}

func customResponseValue(response map[string]any, names ...string) (any, error) {
	for _, name := range names {
		if value, ok := response[name]; ok {
			return value, nil
		}
	}
	return nil, fmt.Errorf("declarative endpoint response must map one of %v", names)
}

// customOptionalString 把空串归一为 nil，与 Python 侧 None 语义一致（缺省不下发该字段）。
func customOptionalString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func customOptionalInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func customOptionalFloat64(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

// customStringList 把字符串切片转成 []any（JSON 数组语义），空切片保留为空数组。
func customStringList(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

// customReferencePaths 取参考图片的本地路径列表（模板 / 映射的 reference_images 输入）。
func customReferencePaths(refs []provider.ReferenceImage) []any {
	out := make([]any, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref.Path)
	}
	return out
}

func customDeclarativeURL(baseURL, path string) string {
	return strings.TrimRight(baseURL, "/") + path
}

// customPostDeclarativeRaw 渲染并发出提交请求，返回解析后的 JSON 文档。
//
// 出站根统一校验形态；headers 缺失时补 Authorization Bearer（声明只允许 accept / content-type，
// 不会与之冲突）。
func customPostDeclarativeRaw(
	ctx context.Context, baseURL, apiKey string,
	declaration *CustomEndpointDeclaration, model string, inputs map[string]any,
) (any, error) {
	effective, err := httpc.ValidateOutboundBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	rendered, err := RenderCustomEndpointDeclaration(declaration, inputs)
	if err != nil {
		return nil, err
	}
	headers := customCopyStringMap(rendered.Headers)
	if _, ok := headers["Authorization"]; !ok {
		headers["Authorization"] = "Bearer " + apiKey
	}
	client := httpc.NewClient(customDeclarativeHTTPTimeout)
	resp, err := client.PostJSON(ctx, customDeclarativeURL(effective, rendered.Path), headers, rendered.Body)
	if err != nil {
		return nil, err
	}
	return customDecodeDeclarativeDocument(resp.Body, model, false)
}

func customPostDeclarative(
	ctx context.Context, baseURL, apiKey string,
	declaration *CustomEndpointDeclaration, model string, inputs map[string]any,
) (map[string]any, error) {
	document, err := customPostDeclarativeRaw(ctx, baseURL, apiKey, declaration, model, inputs)
	if err != nil {
		return nil, err
	}
	return NormalizeCustomEndpointResponse(declaration, document)
}

func customDecodeDeclarativeDocument(body []byte, model string, poll bool) (any, error) {
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		kind := ""
		if poll {
			kind = "poll "
		}
		return nil, fmt.Errorf("declarative endpoint returned non-JSON %sresponse for %s", kind, model)
	}
	return document, nil
}

// customPollDeclarative 从提交响应读 job_id，轮询声明的状态端点直到完成，返回结果值。
func customPollDeclarative(
	ctx context.Context, baseURL, apiKey string,
	declaration *CustomEndpointDeclaration, model string, submission any,
) (any, error) {
	poll := declaration.Poll
	if poll == nil {
		return nil, nil
	}
	submissionMap, ok := submission.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("declarative endpoint returned non-object submission for %s", model)
	}
	effective, err := httpc.ValidateOutboundBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	jobPointer, err := RenderCustomPollPointer(poll.JobID, map[string]any{"model": model})
	if err != nil {
		return nil, err
	}
	jobValue, err := GetCustomJSONPointer(submissionMap, jobPointer)
	if err != nil {
		return nil, fmt.Errorf("declarative endpoint submission is missing job id at %q", jobPointer)
	}
	jobID, ok := customWantJobID(jobValue)
	if !ok {
		return nil, fmt.Errorf("declarative endpoint job id must be a string or integer, got %v", jobValue)
	}
	pointerInputs := map[string]any{"model": model, "job_id": jobID}
	pollRequest, err := RenderCustomEndpointPoll(declaration, pointerInputs)
	if err != nil {
		return nil, err
	}
	if pollRequest == nil {
		return nil, nil
	}
	statusPointer, err := RenderCustomPollPointer(poll.Status, pointerInputs)
	if err != nil {
		return nil, err
	}
	resultPointer := ""
	if poll.HasResult {
		resultPointer, err = RenderCustomPollPointer(poll.Result, pointerInputs)
		if err != nil {
			return nil, err
		}
	}
	headers := customCopyStringMap(pollRequest.Headers)
	if _, ok := headers["Authorization"]; !ok {
		headers["Authorization"] = "Bearer " + apiKey
	}
	client := httpc.NewClient(customDeclarativeHTTPTimeout)
	deadline := time.Now().Add(time.Duration(poll.TimeoutSeconds * float64(time.Second)))
	var lastStatus any
	for {
		resp, err := client.Get(ctx, customDeclarativeURL(effective, pollRequest.Path), headers)
		if err != nil {
			return nil, err
		}
		document, err := customDecodeDeclarativeDocument(resp.Body, model, true)
		if err != nil {
			return nil, err
		}
		status, statusErr := GetCustomJSONPointer(document, statusPointer)
		if statusErr == nil {
			if poll.FailedValue != nil && customJSONScalarEqual(status, poll.FailedValue) {
				return nil, fmt.Errorf("declarative endpoint job %s failed with status %v", jobID, status)
			}
			if customJSONScalarEqual(status, poll.DoneValue) {
				if !poll.HasResult {
					return nil, nil
				}
				result, err := GetCustomJSONPointer(document, resultPointer)
				if err != nil {
					return nil, fmt.Errorf("declarative endpoint completed without a result at %q", resultPointer)
				}
				return result, nil
			}
		}
		lastStatus = status
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf(
				"declarative endpoint job %s did not finish within %gs (last status %v)",
				jobID, poll.TimeoutSeconds, lastStatus,
			)
		}
		if err := sleepWithContext(ctx, time.Duration(poll.IntervalSeconds*float64(time.Second))); err != nil {
			return nil, err
		}
	}
}

// ── 图片 / 音频产物落盘 ───────────────────────────────────────────

// customStoreImageValue 按「data URI / base64 内联，否则下载」落盘（Python _store_image_value）。
func customStoreImageValue(ctx context.Context, value any, outputPath string) error {
	text, isString := value.(string)
	if isString && strings.HasPrefix(text, "data:") {
		if idx := strings.Index(text, ","); idx >= 0 {
			text = text[idx+1:]
		}
	}
	if isString && !strings.HasPrefix(text, "http") {
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return fmt.Errorf("decode declarative image payload: %w", err)
		}
		return writeFileAtomic(outputPath, decoded)
	}
	raw := text
	if !isString {
		raw = fmt.Sprint(value)
	}
	return downloadImageToPath(ctx, httpc.NewClient(customDeclarativeHTTPTimeout), raw, outputPath)
}

// customStorePolledImage 渲染产物 URL 后落盘（无 artifact_url 声明时回退内联值）。
func customStorePolledImage(
	ctx context.Context, baseURL string, poll *CustomEndpointPoll, result any, outputPath string,
) error {
	renderedURL, hasURL, err := RenderCustomEndpointArtifactURL(poll, result)
	if err != nil {
		return err
	}
	if !hasURL {
		return customStoreImageValue(ctx, result, outputPath)
	}
	if !strings.HasPrefix(renderedURL, "http") && !strings.HasPrefix(renderedURL, "data:") {
		effective, err := httpc.ValidateOutboundBaseURL(baseURL)
		if err != nil {
			return err
		}
		renderedURL = customDeclarativeURL(effective, renderedURL)
	}
	if strings.HasPrefix(renderedURL, "data:") {
		return customStoreImageValue(ctx, renderedURL, outputPath)
	}
	validated, err := httpc.ValidateOutboundURL(renderedURL)
	if err != nil {
		return err
	}
	client := httpc.NewClient(customDeclarativeHTTPTimeout)
	resp, err := client.Get(ctx, validated, nil)
	if err != nil {
		return err
	}
	if len(resp.Body) == 0 {
		return fmt.Errorf("downloaded image is empty: %s", validated)
	}
	return writeFileAtomic(outputPath, resp.Body)
}

// customResolveAudioContent 把音频字段取值还原成字节：http(s) 下载，其余按 base64 解码。
func customResolveAudioContent(ctx context.Context, value any) ([]byte, error) {
	text, isString := value.(string)
	if isString && strings.HasPrefix(text, "http") {
		validated, err := httpc.ValidateOutboundURL(text)
		if err != nil {
			return nil, err
		}
		client := httpc.NewClient(customDeclarativeHTTPTimeout)
		resp, err := client.Get(ctx, validated, nil)
		if err != nil {
			return nil, err
		}
		return resp.Body, nil
	}
	payload := text
	if isString && strings.HasPrefix(payload, "data:") {
		if idx := strings.Index(payload, ","); idx >= 0 {
			payload = payload[idx+1:]
		}
	}
	if !isString {
		payload = fmt.Sprint(value)
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("decode declarative audio payload: %w", err)
	}
	return decoded, nil
}

// ── 标量比较与任务 id 归一 ────────────────────────────────────────

// customJSONScalarEqual 比较两个 JSON 标量（数字按数值比较，与 Python == 同义）。
func customJSONScalarEqual(a, b any) bool {
	switch typed := a.(type) {
	case string:
		other, ok := b.(string)
		return ok && typed == other
	case bool:
		other, ok := b.(bool)
		return ok && typed == other
	default:
		left, lok := customAnyFloat(a)
		right, rok := customAnyFloat(b)
		return lok && rok && left == right
	}
}

// customWantJobID 校验并归一化提交响应里的 job_id：仅接受字符串或整数（Python str/int，
// 显式排除 bool）。
func customWantJobID(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) {
			return "", false
		}
		return strconv.FormatInt(int64(typed), 10), true
	case int:
		return strconv.Itoa(typed), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	default:
		return "", false
	}
}

// customTruthy 复刻 Python 的 `x or y` 真值判定（0 / "" / false / nil 为假）。
func customTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return typed != ""
	case bool:
		return typed
	case float64:
		return typed != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	default:
		return true
	}
}

func customOptionalTaskID(response map[string]any) string {
	for _, key := range []string{"task_id", "job_id"} {
		if value, ok := response[key]; ok && customTruthy(value) {
			return customScalarString(value)
		}
	}
	return ""
}

// ── 声明式 delegate ──────────────────────────────────────────────

// customDeclarativeImageDelegate 按声明渲染图片请求；支持可选的 submit-and-poll 传输。
type customDeclarativeImageDelegate struct {
	providerID  string
	baseURL     string
	apiKey      string
	model       string
	declaration *CustomEndpointDeclaration
}

func (d *customDeclarativeImageDelegate) Name() string  { return d.providerID }
func (d *customDeclarativeImageDelegate) Model() string { return d.model }

// Capabilities 返回文生图能力位（与 Python 声明一致）。
func (d *customDeclarativeImageDelegate) Capabilities() provider.ImageCapabilitySet {
	return provider.NewImageCapabilitySet(provider.CapTextToImage)
}

func (d *customDeclarativeImageDelegate) Generate(
	ctx context.Context, req provider.ImageGenerationRequest,
) (provider.ImageGenerationResult, error) {
	inputs := customRequestInputs(d.model, map[string]any{
		"prompt":           req.Prompt,
		"aspect_ratio":     req.AspectRatio,
		"image_size":       customOptionalString(req.ImageSize),
		"seed":             customOptionalInt64(req.Seed),
		"reference_images": customReferencePaths(req.ReferenceImages),
	})
	if d.declaration.Poll != nil {
		submission, err := customPostDeclarativeRaw(ctx, d.baseURL, d.apiKey, d.declaration, d.model, inputs)
		if err != nil {
			return provider.ImageGenerationResult{}, err
		}
		result, err := customPollDeclarative(ctx, d.baseURL, d.apiKey, d.declaration, d.model, submission)
		if err != nil {
			return provider.ImageGenerationResult{}, err
		}
		if err := customStorePolledImage(ctx, d.baseURL, d.declaration.Poll, result, req.OutputPath); err != nil {
			return provider.ImageGenerationResult{}, err
		}
	} else {
		response, err := customPostDeclarative(ctx, d.baseURL, d.apiKey, d.declaration, d.model, inputs)
		if err != nil {
			return provider.ImageGenerationResult{}, err
		}
		value, err := customResponseValue(response, "image_url", "result_url", "url", "image_base64", "b64_json")
		if err != nil {
			return provider.ImageGenerationResult{}, err
		}
		if err := customStoreImageValue(ctx, value, req.OutputPath); err != nil {
			return provider.ImageGenerationResult{}, err
		}
	}
	return provider.ImageGenerationResult{
		ImagePath: req.OutputPath,
		Provider:  d.providerID,
		Model:     d.model,
	}, nil
}

// customDeclarativeVideoDelegate 按声明渲染视频请求（同步取回 URL 后下载）。
type customDeclarativeVideoDelegate struct {
	providerID  string
	baseURL     string
	apiKey      string
	model       string
	declaration *CustomEndpointDeclaration
}

func (d *customDeclarativeVideoDelegate) Name() string  { return d.providerID }
func (d *customDeclarativeVideoDelegate) Model() string { return d.model }

// VideoCapabilities 返回默认视频能力声明（与 Python VideoCapabilities() 默认同值）。
func (d *customDeclarativeVideoDelegate) VideoCapabilities() provider.VideoCapabilities {
	return provider.DefaultVideoCapabilities()
}

func (d *customDeclarativeVideoDelegate) Generate(
	ctx context.Context, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	inputs := customRequestInputs(d.model, map[string]any{
		"prompt":                req.Prompt,
		"duration":              req.DurationSeconds,
		"duration_seconds":      req.DurationSeconds,
		"aspect_ratio":          req.AspectRatio,
		"resolution":            customOptionalString(req.Resolution),
		"start_image":           customOptionalString(req.StartImage),
		"end_image":             customOptionalString(req.EndImage),
		"reference_images":      customStringList(req.ReferenceImages),
		"reference_audio_files": customStringList(req.ReferenceAudioFiles),
		"generate_audio":        req.GenerateAudio,
	})
	response, err := customPostDeclarative(ctx, d.baseURL, d.apiKey, d.declaration, d.model, inputs)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	rawURL, err := customResponseValue(response, "video_url", "result_url", "url")
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	videoURL := fmt.Sprint(rawURL)
	if err := DownloadVideo(ctx, videoURL, req.OutputPath, customDeclarativeHTTPTimeout); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	return provider.VideoGenerationResult{
		VideoPath:       req.OutputPath,
		Provider:        d.providerID,
		Model:           d.model,
		DurationSeconds: req.DurationSeconds,
		VideoURI:        videoURL,
		TaskID:          customOptionalTaskID(response),
	}, nil
}

// ResumeVideo 声明式视频端点无轮询传输，续跑不支持（orphan 处理据此走 [resume_unsupported]）。
func (d *customDeclarativeVideoDelegate) ResumeVideo(
	_ context.Context, _ string, _ provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	return provider.VideoGenerationResult{}, provider.ErrResumeUnsupported
}

// customDeclarativeAudioDelegate 按声明渲染语音合成请求并落盘音频。
type customDeclarativeAudioDelegate struct {
	providerID  string
	baseURL     string
	apiKey      string
	model       string
	declaration *CustomEndpointDeclaration
}

func (d *customDeclarativeAudioDelegate) Name() string  { return d.providerID }
func (d *customDeclarativeAudioDelegate) Model() string { return d.model }

// Capabilities 返回文本转语音能力位。
func (d *customDeclarativeAudioDelegate) Capabilities() provider.AudioCapabilitySet {
	return provider.NewAudioCapabilitySet(provider.CapTextToSpeech)
}

// ListVoices 声明式端点无音色目录，返回空（Python 侧同名方法返回 []）。
func (d *customDeclarativeAudioDelegate) ListVoices() []provider.VoiceOption { return nil }

func (d *customDeclarativeAudioDelegate) Synthesize(
	ctx context.Context, req provider.AudioSynthesisRequest,
) (provider.AudioSynthesisResult, error) {
	inputs := customRequestInputs(d.model, map[string]any{
		"text":          req.Text,
		"voice":         req.Voice,
		"language_type": req.LanguageType,
		"speed":         customOptionalFloat64(req.Speed),
	})
	response, err := customPostDeclarative(ctx, d.baseURL, d.apiKey, d.declaration, d.model, inputs)
	if err != nil {
		return provider.AudioSynthesisResult{}, err
	}
	value, err := customResponseValue(response, "audio_url", "result_url", "url", "audio_base64", "b64_json")
	if err != nil {
		return provider.AudioSynthesisResult{}, err
	}
	content, err := customResolveAudioContent(ctx, value)
	if err != nil {
		return provider.AudioSynthesisResult{}, err
	}
	if err := writeFileAtomic(req.OutputPath, content); err != nil {
		return provider.AudioSynthesisResult{}, err
	}
	return provider.AudioSynthesisResult{
		Provider:   d.providerID,
		Model:      d.model,
		Characters: utf8.RuneCountInString(req.Text),
		OutputPath: req.OutputPath,
	}, nil
}

// ── 包装层 ───────────────────────────────────────────────────────

// customWrappedImageBackend 覆写 name / model，其余转发被包装 delegate。
type customWrappedImageBackend struct {
	providerID string
	model      string
	delegate   provider.ImageDelegate
}

func (b *customWrappedImageBackend) Name() string  { return b.providerID }
func (b *customWrappedImageBackend) Model() string { return b.model }

func (b *customWrappedImageBackend) Capabilities() provider.ImageCapabilitySet {
	return b.delegate.Capabilities()
}

func (b *customWrappedImageBackend) Generate(
	ctx context.Context, req provider.ImageGenerationRequest,
) (provider.ImageGenerationResult, error) {
	return b.delegate.Generate(ctx, req)
}

// Delegate 返回被包装的协议级执行体。
func (b *customWrappedImageBackend) Delegate() provider.ImageDelegate { return b.delegate }

// customWrappedAudioBackend 覆写 name / model，其余转发被包装 delegate。
type customWrappedAudioBackend struct {
	providerID string
	model      string
	delegate   provider.AudioDelegate
}

func (b *customWrappedAudioBackend) Name() string  { return b.providerID }
func (b *customWrappedAudioBackend) Model() string { return b.model }

func (b *customWrappedAudioBackend) Capabilities() provider.AudioCapabilitySet {
	return b.delegate.Capabilities()
}

func (b *customWrappedAudioBackend) ListVoices() []provider.VoiceOption {
	return b.delegate.ListVoices()
}

func (b *customWrappedAudioBackend) Synthesize(
	ctx context.Context, req provider.AudioSynthesisRequest,
) (provider.AudioSynthesisResult, error) {
	return b.delegate.Synthesize(ctx, req)
}

// Delegate 返回被包装的协议级执行体。
func (b *customWrappedAudioBackend) Delegate() provider.AudioDelegate { return b.delegate }

// customWrappedVideoBackend 覆写 name / model、记住构造 endpoint，并承载合并后的生效能力
// （系统判定 ⊕ 声明提示 ⊕ 用户覆盖）：执行层据此门控，不再读被包装 backend 的静态声明。
type customWrappedVideoBackend struct {
	providerID string
	model      string
	delegate   provider.VideoDelegate
	endpoint   string
	videoCaps  *provider.VideoCapabilities
	// overrides 是过滤后的稀疏用户覆盖，供档位查询叠加到档位感知基底上（与 videoCaps 分开传递：
	// 后者是 context-free 的完整合成结果，短路回它会让档位查询失去意义）。
	overrides map[string]any
}

func (b *customWrappedVideoBackend) Name() string  { return b.providerID }
func (b *customWrappedVideoBackend) Model() string { return b.model }

// Endpoint 返回构造本 backend 所用的 endpoint 键（续跑据此比对协议是否被换掉）。
func (b *customWrappedVideoBackend) Endpoint() string { return b.endpoint }

// Delegate 返回被包装的协议级执行体。
func (b *customWrappedVideoBackend) Delegate() provider.VideoDelegate { return b.delegate }

func (b *customWrappedVideoBackend) VideoCapabilities() provider.VideoCapabilities {
	if b.videoCaps != nil {
		return *b.videoCaps
	}
	return b.delegate.VideoCapabilities()
}

// VideoCapabilitiesForTier 按请求档位收窄能力：以被包装 backend 的档位感知查询为基底（未实现则
// 回落其静态声明），再叠加稀疏用户覆盖。
func (b *customWrappedVideoBackend) VideoCapabilitiesForTier(serviceTier, resolution string) provider.VideoCapabilities {
	base := provider.ResolveVideoCapabilities(b.delegate, serviceTier, resolution)
	if len(b.overrides) == 0 {
		return base
	}
	return customEnforceAudioCapabilityInvariant(customMergeOverrides(base, b.overrides))
}

func (b *customWrappedVideoBackend) Generate(
	ctx context.Context, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	// 注入本次执行的 endpoint，让下游协议 backend 在 submit 后与 job_id 一并持久化——续跑据此
	// 判定协议是否已被换掉（docs/adr/0054）。
	if b.endpoint != "" {
		req.ExecutionEndpoint = b.endpoint
	}
	return b.delegate.Generate(ctx, req)
}

func (b *customWrappedVideoBackend) ResumeVideo(
	ctx context.Context, jobID string, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	return b.delegate.ResumeVideo(ctx, jobID, req)
}

// ── 工厂 ─────────────────────────────────────────────────────────

// CreateCustomBackend 按 endpoint 查表构造自定义供应商 backend。
//
// endpoint_declaration / capability_overrides 以调用方从 DB 读到的原始 JSON 字符串传入（空串表示
// 未设置）。视频后端额外注入生效能力（endpoint spec 系统判定 ⊕ 声明提示 ⊕ 模型级用户覆盖）。
func CreateCustomBackend(
	cfg CustomProviderConfig, modelID, endpoint string, endpointDeclarationJSON, capabilityOverridesJSON string,
) (CustomBackend, error) {
	spec, err := CustomEndpointSpecFor(endpoint)
	if err != nil {
		return CustomBackend{}, err
	}
	declaration, err := customParseDeclarationJSON(endpointDeclarationJSON)
	if err != nil {
		return CustomBackend{}, err
	}
	// poll 传输目前只在图片委托里接线；其它媒体类型声明 poll 会被静默忽略，故在构造期 fail loud。
	if declaration != nil && declaration.Poll != nil && spec.mediaType != provider.MediaImage {
		return CustomBackend{}, fmt.Errorf("poll is only supported on image endpoints, not %q", spec.mediaType)
	}
	if spec.mediaType == provider.MediaText {
		// 文本侧契约延后：text endpoint 已登记，但构造时明确 fail loud，避免静默降级。
		return CustomBackend{}, fmt.Errorf("text backend not wired for endpoint %q", endpoint)
	}
	overrides, err := customParseOverridesJSON(capabilityOverridesJSON)
	if err != nil {
		return CustomBackend{}, err
	}
	inner := cfg.customConfig()
	switch spec.mediaType {
	case provider.MediaImage:
		delegate, err := customResolveImageDelegate(inner, modelID, declaration, spec)
		if err != nil {
			return CustomBackend{}, err
		}
		return CustomBackend{
			MediaType: provider.MediaImage,
			Endpoint:  endpoint,
			Image: &customWrappedImageBackend{
				providerID: cfg.ProviderID, model: modelID, delegate: delegate,
			},
		}, nil
	case provider.MediaVideo:
		delegate, err := customResolveVideoDelegate(inner, modelID, declaration, spec)
		if err != nil {
			return CustomBackend{}, err
		}
		caps, applied, err := CustomSynthesizeVideoCapabilitiesWithDeclaration(endpoint, modelID, declaration, overrides)
		if err != nil {
			return CustomBackend{}, err
		}
		return CustomBackend{
			MediaType: provider.MediaVideo,
			Endpoint:  endpoint,
			Video: &customWrappedVideoBackend{
				providerID: cfg.ProviderID, model: modelID, delegate: delegate,
				endpoint: endpoint, videoCaps: &caps, overrides: applied,
			},
		}, nil
	case provider.MediaAudio:
		delegate, err := customResolveAudioDelegate(inner, modelID, declaration, spec)
		if err != nil {
			return CustomBackend{}, err
		}
		return CustomBackend{
			MediaType: provider.MediaAudio,
			Endpoint:  endpoint,
			Audio: &customWrappedAudioBackend{
				providerID: cfg.ProviderID, model: modelID, delegate: delegate,
			},
		}, nil
	default:
		return CustomBackend{}, fmt.Errorf("unsupported media type %q for endpoint %q", spec.mediaType, endpoint)
	}
}

func customParseDeclarationJSON(raw string) (*CustomEndpointDeclaration, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse endpoint declaration: %w", err)
	}
	return ParseCustomEndpointDeclaration(parsed)
}

func customParseOverridesJSON(raw string) (any, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse capability overrides: %w", err)
	}
	return parsed, nil
}

func customResolveImageDelegate(
	cfg customProviderConfig, modelID string, declaration *CustomEndpointDeclaration, spec customEndpointSpec,
) (provider.ImageBackend, error) {
	if declaration != nil {
		return &customDeclarativeImageDelegate{
			providerID: cfg.providerID, baseURL: cfg.baseURL, apiKey: cfg.apiKey,
			model: modelID, declaration: declaration,
		}, nil
	}
	if spec.buildImage == nil {
		return nil, fmt.Errorf("image endpoint %q has no builder", spec.key)
	}
	return spec.buildImage(cfg, modelID)
}

func customResolveVideoDelegate(
	cfg customProviderConfig, modelID string, declaration *CustomEndpointDeclaration, spec customEndpointSpec,
) (provider.VideoBackend, error) {
	if declaration != nil {
		return &customDeclarativeVideoDelegate{
			providerID: cfg.providerID, baseURL: cfg.baseURL, apiKey: cfg.apiKey,
			model: modelID, declaration: declaration,
		}, nil
	}
	if spec.buildVideo == nil {
		return nil, fmt.Errorf("video endpoint %q has no builder", spec.key)
	}
	return spec.buildVideo(cfg, modelID)
}

func customResolveAudioDelegate(
	cfg customProviderConfig, modelID string, declaration *CustomEndpointDeclaration, spec customEndpointSpec,
) (provider.AudioBackend, error) {
	if declaration != nil {
		return &customDeclarativeAudioDelegate{
			providerID: cfg.providerID, baseURL: cfg.baseURL, apiKey: cfg.apiKey,
			model: modelID, declaration: declaration,
		}, nil
	}
	if spec.buildAudio == nil {
		return nil, fmt.Errorf("audio endpoint %q has no builder", spec.key)
	}
	return spec.buildAudio(cfg, modelID)
}
