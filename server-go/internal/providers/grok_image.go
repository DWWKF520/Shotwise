package providers

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// Grok（xAI）图片生成后端。对齐 Python lib/image_backends/grok.py 的行为：走 xAI 的
// REST 图片接口（POST {base}/images/generations），支持 T2I 与 I2I；I2I 时把存在的参考图
// 转成 base64 data URI 放进 image_urls 一并提交，图生图与文生图共用同一端点。

const (
	// grokProviderID 与 registry 的 provider_id 同名。
	grokProviderID = "grok"

	grokDefaultImageModel = "grok-imagine-image"
	// grokDefaultBaseURL 是 xAI REST API 的默认根：供应商未提供 base_url 时使用。
	grokDefaultBaseURL = "https://api.x.ai/v1"

	// grokImageRequestTimeout 单次图片生成请求的墙钟预算。图片是「一次调用 = 一次计费」，
	// 超时后重试等于二次计费，故给整个请求一个显式上界。
	grokImageRequestTimeout = 600 * time.Second
)

// grokImageSupportedAspectRatios 是 xAI 文档列出的合法比例集合；不在集合内只告警仍透传给 API。
var grokImageSupportedAspectRatios = map[string]struct{}{
	"1:1": {}, "16:9": {}, "9:16": {}, "4:3": {}, "3:4": {}, "3:2": {}, "2:3": {},
	"2:1": {}, "1:2": {}, "19.5:9": {}, "9:19.5": {}, "20:9": {}, "9:20": {}, "auto": {},
}

// grokImageBackend 是 xAI 图片生成后端。
type grokImageBackend struct {
	model   string
	baseURL string
	apiKey  string
	client  *httpc.Client
}

// newGrokImageBackend 按通用信封构造；model / base_url 留空时由本后端兜底（默认 model、官方端点）。
func newGrokImageBackend(opts provider.BackendOptions) (*grokImageBackend, error) {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = grokDefaultImageModel
	}
	baseURL := strings.TrimSpace(opts.BaseURL)
	if baseURL == "" {
		baseURL = grokDefaultBaseURL
	}
	return &grokImageBackend{
		model:   model,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  opts.APIKey,
		client:  httpc.NewClient(grokImageRequestTimeout),
	}, nil
}

// Name 返回供应商标识。
func (b *grokImageBackend) Name() string { return grokProviderID }

// Model 返回实际下发的模型名。
func (b *grokImageBackend) Model() string { return b.model }

// Capabilities 返回本后端的能力位：T2I 与 I2I 均支持。
func (b *grokImageBackend) Capabilities() provider.ImageCapabilitySet {
	return imageCapabilitySet(provider.CapTextToImage, provider.CapImageToImage)
}

// grokImageResponse 是图片接口的响应形状。取 OpenAI 兼容的 data[]，只解析用得上的字段。
type grokImageResponse struct {
	Data []struct {
		URL string `json:"url"`
		// 内容审核结果；缺省（nil）时不据此拒绝，仅显式 false 才判为被审核拦截。
		RespectModeration *bool `json:"respect_moderation"`
	} `json:"data"`
}

// Generate 生成一张图片并落盘到 req.OutputPath。
func (b *grokImageBackend) Generate(
	ctx context.Context, req provider.ImageGenerationRequest,
) (provider.ImageGenerationResult, error) {
	payload := map[string]any{
		"prompt":       req.Prompt,
		"model":        b.model,
		"aspect_ratio": grokValidateAspectRatio(req.AspectRatio),
	}
	if req.ImageSize != "" {
		payload["resolution"] = req.ImageSize
	}
	if uris := grokReferenceDataURIs(req.ReferenceImages); len(uris) > 0 {
		payload["image_urls"] = uris
	}

	target := httpc.JoinURL(b.baseURL, "/images/generations")
	var resp *httpc.Response
	// 创建即计费，按「请求是否确定送达」分流：连接类失败可重试，歧义态终态失败不重复计费。
	err := provider.RunWithRetry(ctx, provider.DefaultRetryPolicy, provider.SubmitRetryable, func(ctx context.Context) error {
		created, err := SubmitPost(ctx, grokProviderID, func(ctx context.Context) (*httpc.Response, error) {
			return b.client.PostJSON(ctx, target, b.authHeaders(), payload)
		})
		if err != nil {
			return err
		}
		resp = created
		return nil
	})
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}

	var parsed grokImageResponse
	if err := decodeJSON(resp.Body, &parsed); err != nil {
		return provider.ImageGenerationResult{}, err
	}
	if len(parsed.Data) == 0 {
		return provider.ImageGenerationResult{}, &provider.NonRetryableError{
			Err: fmt.Errorf("grok image response has empty data (model=%s)", b.model),
		}
	}
	item := parsed.Data[0]
	if item.RespectModeration != nil && !*item.RespectModeration {
		return provider.ImageGenerationResult{}, fmt.Errorf("Grok 图片生成被内容审核拒绝")
	}
	if strings.TrimSpace(item.URL) == "" {
		return provider.ImageGenerationResult{}, fmt.Errorf("grok image response item carries no url")
	}
	if err := downloadImageToPath(ctx, b.client, strings.TrimSpace(item.URL), req.OutputPath); err != nil {
		return provider.ImageGenerationResult{}, err
	}

	return provider.ImageGenerationResult{
		ImagePath: req.OutputPath,
		Provider:  grokProviderID,
		Model:     b.model,
		ImageURI:  item.URL,
	}, nil
}

// authHeaders 构造鉴权头。
func (b *grokImageBackend) authHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + b.apiKey}
}

// grokValidateAspectRatio 校验比例是否在 xAI 支持列表内；不支持只告警并原样透传。
func grokValidateAspectRatio(aspectRatio string) string {
	if _, ok := grokImageSupportedAspectRatios[aspectRatio]; !ok {
		slog.Warn("Grok 可能不支持该 aspect_ratio，将透传给 API", "aspect_ratio", aspectRatio)
	}
	return aspectRatio
}

// grokReferenceDataURIs 把存在的参考图转成 base64 data URI 列表；打不开的参考图跳过。
func grokReferenceDataURIs(refs []provider.ReferenceImage) []string {
	if len(refs) == 0 {
		return nil
	}
	uris := make([]string, 0, len(refs))
	for _, ref := range refs {
		if _, err := os.Stat(ref.Path); err != nil {
			continue
		}
		uri, err := imageToBase64DataURI(ref.Path)
		if err != nil {
			continue
		}
		uris = append(uris, uri)
	}
	return uris
}

func init() {
	provider.RegisterImageBackend(grokProviderID, provider.ImageBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.ImageBackend, error) {
			return newGrokImageBackend(opts)
		},
	})
}
