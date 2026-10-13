package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// Vidu（生数科技）图片 / 视频 backend 端口（对应 Python lib/image_backends/vidu.py、
// lib/video_backends/vidu.py 与共享模块 lib/vidu_shared.py）。
//
// Vidu 图片只有一个端点 /reference2image：images 为空时是文生图，带参考图时是图生图 / 图片
// 编辑。视频按请求字段分派到 /text2video、/img2video、/start-end2video、/reference2video 四个
// 端点，产生/轮询/下载三段的共享逻辑（鉴权头、任务查询、终态判定、URL 提取、请求体上限）收口在
// 本文件，两个 backend 复用。

// Vidu 通用常量：供应商标识、默认端点根、请求体上限、任务轮询接口形态。
const (
	// viduProviderName 与 registry 的 provider_id 同名。
	viduProviderName = "vidu"

	// viduDefaultBaseURL 是 Vidu 开放平台 API 基础地址。
	viduDefaultBaseURL = "https://api.vidu.cn/ent/v2"

	// viduClientTimeout 是单次出网调用的墙钟预算（与 Python create_vidu_client 的 timeout 一致）。
	viduClientTimeout = 60 * time.Second

	// viduMaxBodyBytes 是单次请求体硬上限：文档限制 20MB，base64 自带 ~33% 膨胀，故本地阈值留
	// 2MB 余量给非 images 字段，超限在序列化后提前拒绝。
	viduMaxBodyBytes = 18 * 1024 * 1024

	// viduMaxReferenceImages 是参考图数量上限（图片与视频共用）。
	viduMaxReferenceImages = 7

	// viduDefaultImageModel 是图片默认模型。
	viduDefaultImageModel = "viduq2"
	// viduDefaultVideoModel 是视频默认模型。
	viduDefaultVideoModel = "viduq3-turbo"
)

// Vidu 图片后端专属常量。
const (
	// viduImagePollInterval 是图片任务轮询间隔。
	viduImagePollInterval = 3 * time.Second
	// viduImagePollMaxWait 是图片任务轮询最大等待时长。
	viduImagePollMaxWait = 600 * time.Second
	// viduImagePromptMaxLen 是图片提示词字符数上限（文档：不超过 2000 个字符），超出客户端截断。
	viduImagePromptMaxLen = 2000
)

// viduImageAspectRatioWhitelist 是文档明确的 aspect_ratio 白名单（按 model）。
var viduImageAspectRatioWhitelist = map[string][]string{
	"viduq1": {"16:9", "9:16", "1:1", "3:4", "4:3"},
	"viduq2": {"16:9", "9:16", "1:1", "3:4", "4:3", "21:9", "2:3", "3:2", "auto"},
}

// viduImageResolutionWhitelist 是文档明确的 resolution 白名单（按 model）。
var viduImageResolutionWhitelist = map[string][]string{
	"viduq1": {"1080p"},
	"viduq2": {"1080p", "2K", "4K"},
}

// viduImageModelCapabilities 是各模型的图片能力位；未登记的 model 取「文生图 + 图生图」宽声明。
var viduImageModelCapabilities = map[string]provider.ImageCapabilitySet{
	"viduq2": provider.NewImageCapabilitySet(provider.CapTextToImage, provider.CapImageToImage),
	"viduq1": provider.NewImageCapabilitySet(provider.CapImageToImage),
}

// viduImageBackend 是 Vidu 参考生图 / 文生图后端。
type viduImageBackend struct {
	apiKey  string
	baseURL string
	model   string
	client  *httpc.Client
	caps    provider.ImageCapabilitySet
}

// newViduImageBackend 按通用信封构造；model / base_url 留空时由本后端兜底（默认 model、官方端点），
// api_key 缺失不在此报错——与 Python __init__ 一致，鉴权头在请求期解析时才 fail loud。
func newViduImageBackend(opts provider.BackendOptions) (*viduImageBackend, error) {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = viduDefaultImageModel
	}
	caps, ok := viduImageModelCapabilities[model]
	if !ok {
		caps = provider.NewImageCapabilitySet(provider.CapTextToImage, provider.CapImageToImage)
	}
	return &viduImageBackend{
		apiKey:  opts.APIKey,
		baseURL: viduResolveBaseURL(opts.BaseURL),
		model:   model,
		client:  httpc.NewClient(viduClientTimeout),
		caps:    caps,
	}, nil
}

// Name 返回供应商标识。
func (b *viduImageBackend) Name() string { return viduProviderName }

// Model 返回实际下发的模型名。
func (b *viduImageBackend) Model() string { return b.model }

// Capabilities 返回本实例的能力位（由 model 决定）。
func (b *viduImageBackend) Capabilities() provider.ImageCapabilitySet { return b.caps }

// Generate 生成一张图片并落盘到 req.OutputPath。
//
// 能力闸门先于请求：不支持的路径返回 *provider.CapabilityError，不发请求、不计费。
func (b *viduImageBackend) Generate(
	ctx context.Context, req provider.ImageGenerationRequest,
) (provider.ImageGenerationResult, error) {
	headers, err := viduAuthHeaders(b.apiKey)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}

	hasRefs := len(req.ReferenceImages) > 0
	if hasRefs && !b.caps.Has(provider.CapImageToImage) {
		return provider.ImageGenerationResult{}, provider.NewImageCapabilityError(
			provider.FailureImageEndpointMismatchNoI2I, map[string]any{"model": b.model},
		)
	}
	if !hasRefs && !b.caps.Has(provider.CapTextToImage) {
		return provider.ImageGenerationResult{}, provider.NewImageCapabilityError(
			provider.FailureImageEndpointMismatchNoT2I, map[string]any{"model": b.model},
		)
	}

	// viduq1 必须 1-7 张；viduq2 允许 0-7 张。这里截断到上限。
	refs := req.ReferenceImages
	if len(refs) > viduMaxReferenceImages {
		slog.Warn("Vidu 参考图数量超过上限，截断", "count", len(refs), "limit", viduMaxReferenceImages)
		refs = refs[:viduMaxReferenceImages]
	}

	body := map[string]any{
		"model":  b.model,
		"prompt": viduTruncateRunes(req.Prompt, viduImagePromptMaxLen),
	}
	if len(refs) > 0 {
		images := make([]string, 0, len(refs))
		for _, ref := range refs {
			uri, err := imageToBase64DataURI(ref.Path)
			if err != nil {
				return provider.ImageGenerationResult{}, err
			}
			images = append(images, uri)
		}
		body["images"] = images
	}

	// aspect_ratio / resolution 白名单兜底：不在白名单的取值丢弃（不静默降级成别的比例）。
	if req.AspectRatio != "" {
		if allowed := viduImageAspectRatioWhitelist[b.model]; allowed != nil {
			if viduContains(allowed, req.AspectRatio) {
				body["aspect_ratio"] = req.AspectRatio
			} else {
				slog.Warn("Vidu image aspect_ratio 不在白名单，丢弃", "aspect_ratio", req.AspectRatio, "model", b.model)
			}
		}
	}
	if req.ImageSize != "" {
		if allowed := viduImageResolutionWhitelist[b.model]; allowed != nil {
			if viduContains(allowed, req.ImageSize) {
				body["resolution"] = req.ImageSize
			} else {
				slog.Warn("Vidu image resolution 不在白名单，使用默认", "resolution", req.ImageSize, "model", b.model)
			}
		}
	}
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}

	payload, err := viduCreateTask(ctx, b.client, b.baseURL, "/reference2image", headers, body, "图片")
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	taskID, _ := viduTaskID(payload)

	final, err := viduPollTask(ctx, b.client, b.baseURL, headers, taskID, viduImagePollInterval, viduImagePollMaxWait)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	url, err := viduExtractURL(final)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}

	// creations 内 credits 可能也带（兜底取顶层）；credits=0 合法，必须显式判 nil。
	actual := viduCredits(final)
	if actual == nil {
		actual = viduCredits(payload)
	}

	if err := downloadImageToPath(ctx, b.client, url, req.OutputPath); err != nil {
		return provider.ImageGenerationResult{}, err
	}

	result := provider.ImageGenerationResult{
		ImagePath: req.OutputPath,
		Provider:  viduProviderName,
		Model:     b.model,
		Seed:      req.Seed,
	}
	if actual != nil {
		tokens := int64(*actual)
		result.UsageTokens = &tokens
	}
	return result, nil
}

// viduResolveBaseURL 返回配置的端点根，留空回落官方默认。
func viduResolveBaseURL(baseURL string) string {
	if strings.TrimSpace(baseURL) == "" {
		return viduDefaultBaseURL
	}
	return strings.TrimSpace(baseURL)
}

// viduAuthHeaders 构造鉴权头；Vidu 用 "Token <key>" 方案，api_key 缺失即 fail loud（不静默空跑）。
func viduAuthHeaders(apiKey string) (map[string]string, error) {
	token := strings.TrimSpace(apiKey)
	if token == "" {
		return nil, errors.New("请到系统配置页填写 Vidu API Key")
	}
	return map[string]string{"Authorization": "Token " + token}, nil
}

// viduCreateTask 提交创建请求（非幂等的「创建 + 计费」POST）并校验响应带 task_id。
//
// 走 SubmitPost + SubmitRetryable：连接类失败（请求确定未送达）重试，歧义态终态失败，避免重复
// 建任务 + 重复计费；4xx 由 SubmitRetryable 按状态码闸门分流。
func viduCreateTask(
	ctx context.Context, client *httpc.Client, baseURL, endpoint string,
	headers map[string]string, body map[string]any, label string,
) (map[string]any, error) {
	if err := viduAssertBodySize(body); err != nil {
		return nil, err
	}
	target := httpc.JoinURL(baseURL, endpoint)
	slog.Info("调用 Vidu "+label+" API", "endpoint", endpoint, "kwargs", viduSafeBodyForLog(body))

	var payload map[string]any
	err := provider.RunWithRetry(ctx, provider.DefaultRetryPolicy, provider.SubmitRetryable, func(ctx context.Context) error {
		resp, err := SubmitPost(ctx, viduProviderName, func(ctx context.Context) (*httpc.Response, error) {
			return client.PostJSON(ctx, target, headers, body)
		})
		if err != nil {
			return err
		}
		payload = nil
		if err := decodeJSON(resp.Body, &payload); err != nil {
			return err
		}
		if id, ok := viduTaskID(payload); !ok || id == "" {
			return &provider.NonRetryableError{
				Err: fmt.Errorf("Vidu %s任务创建响应缺少 task_id: %v", label, payload),
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// viduPollTask 轮询任务至终态，返回最终响应体。
func viduPollTask(
	ctx context.Context, client *httpc.Client, baseURL string,
	headers map[string]string, taskID string, interval, maxWait time.Duration,
) (map[string]any, error) {
	return PollWithRetry(ctx, PollConfig[map[string]any]{
		PollFn: func(ctx context.Context) (map[string]any, error) {
			return viduFetchTask(ctx, client, baseURL, headers, taskID)
		},
		IsDone:       viduTaskDone,
		IsFailed:     viduTaskFailure,
		PollInterval: interval,
		MaxWait:      maxWait,
		Label:        "Vidu",
		OnProgress: func(v map[string]any, elapsed time.Duration) {
			slog.Info("Vidu 生成中...", "state", v["state"], "elapsed_seconds", int(elapsed.Seconds()))
		},
	})
}

// viduFetchTask 查询 Vidu 任务状态（GET /tasks/{id}/creations）。
func viduFetchTask(
	ctx context.Context, client *httpc.Client, baseURL string, headers map[string]string, taskID string,
) (map[string]any, error) {
	resp, err := client.Get(ctx, httpc.JoinURL(baseURL, "/tasks/"+taskID+"/creations"), headers)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := decodeJSON(resp.Body, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// viduTaskID 提取 task_id；缺失 / 空串视为未取到。
func viduTaskID(payload map[string]any) (string, bool) {
	value, ok := payload["task_id"]
	if !ok || value == nil {
		return "", false
	}
	if s, ok := value.(string); ok {
		if s == "" {
			return "", false
		}
		return s, true
	}
	return fmt.Sprint(value), true
}

// viduCredits 提取 credits；缺失视为 nil。credits=0 是合法值，不能被当成「无 credits」。
func viduCredits(payload map[string]any) *float64 {
	value, ok := payload["credits"]
	if !ok || value == nil {
		return nil
	}
	switch v := value.(type) {
	case float64:
		return &v
	case int:
		f := float64(v)
		return &f
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return &f
		}
	}
	return nil
}

// viduTaskDone 判断任务是否成功终态。
func viduTaskDone(payload map[string]any) bool {
	state, _ := payload["state"].(string)
	return state == "success"
}

// viduTaskFailure 判断任务是否失败终态，返回错误信息（空串表示未失败）。
func viduTaskFailure(payload map[string]any) string {
	state, _ := payload["state"].(string)
	if state != "failed" {
		return ""
	}
	errCode := payload["err_code"]
	if errCode == nil || errCode == "" || errCode == float64(0) {
		errCode = "unknown"
	}
	return fmt.Sprintf("Vidu 任务失败: err_code=%v", errCode)
}

// viduExtractURL 从 success 响应中提取生成物 URL。
func viduExtractURL(payload map[string]any) (string, error) {
	creations, _ := payload["creations"].([]any)
	if len(creations) == 0 {
		return "", fmt.Errorf("Vidu 任务无 creations: task_id=%v", payload["id"])
	}
	first, ok := creations[0].(map[string]any)
	if !ok {
		return "", fmt.Errorf("Vidu creations[0] 形态异常: task_id=%v", payload["id"])
	}
	url, _ := first["url"].(string)
	if url == "" {
		return "", fmt.Errorf("Vidu creations[0].url 为空: task_id=%v", payload["id"])
	}
	return url, nil
}

// viduAssertBodySize 在序列化后估算请求体大小，超限提前拒绝（20MB 文档上限，本地留 2MB 余量）。
func viduAssertBodySize(body map[string]any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal vidu request body: %w", err)
	}
	if len(encoded) > viduMaxBodyBytes {
		return fmt.Errorf(
			"Vidu 请求体大小 %.1fMB 超过上限 %dMB"+
				"（base64 编码自带 ~33%% 膨胀，请减少参考图数量或压缩图片后重试）",
			float64(len(encoded))/1024/1024, viduMaxBodyBytes/1024/1024,
		)
	}
	return nil
}

// viduSafeLogKeys 是日志输出允许的字段白名单（避免日志回显 base64 图片或敏感字段）。
var viduSafeLogKeys = []string{"model", "duration", "resolution", "aspect_ratio", "seed", "audio", "off_peak"}

// viduSafeBodyForLog 生成安全的日志视图：仅保留白名单字段 + prompt 截断 + images 仅计数。
func viduSafeBodyForLog(body map[string]any) map[string]any {
	view := make(map[string]any, len(viduSafeLogKeys)+2)
	for _, key := range viduSafeLogKeys {
		if value, ok := body[key]; ok {
			view[key] = value
		}
	}
	if prompt, ok := body["prompt"].(string); ok {
		view["prompt"] = viduTruncateRunes(prompt, 120)
		if len([]rune(prompt)) > 120 {
			view["prompt"] = view["prompt"].(string) + "…"
		}
	}
	if images, ok := body["images"].([]string); ok && len(images) > 0 {
		view["images"] = fmt.Sprintf("<%d data uri>", len(images))
	}
	return view
}

// viduTruncateRunes 按字符（code point）截断到上限，计量口径与 Python 字符串切片一致。
func viduTruncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

// viduContains 判断字符串切片是否含某值。
func viduContains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

func init() {
	provider.RegisterImageBackend(viduProviderName, provider.ImageBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.ImageBackend, error) {
			return newViduImageBackend(opts)
		},
	})
}
