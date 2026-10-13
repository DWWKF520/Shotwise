package providers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// ArkImageBackend —— 火山方舟 Seedream 图片生成后端（端口自 lib/image_backends/ark.py）。
//
// 走 Ark 原生 REST 端点 POST /images/generations；T2I 与 I2I 同端点，I2I 时把参考图内联为
// base64 data URI 放进 image 字段（单张传字符串、多张传列表）。

const (
	// arkDefaultImageModel 是 model 缺省时的 Seedream 型号。
	arkDefaultImageModel = "doubao-seedream-5-0-lite-260128"
	// arkImageRequestTimeout 单次生成请求的墙钟预算。
	//
	// 图片请求是「一次调用 = 一次计费」的形态，超时后重试等于二次计费，故给整个 generate 一个
	// 显式上界，而不是让内层重试各自重新获得完整超时。
	arkImageRequestTimeout = 600 * time.Second
)

// Seedream size 参数：不传时 4.x/5.x 默认 2048x2048（1:1），3.0-t2i 默认 1024x1024（1:1）。
// 不显式传 size 会让项目设置的 aspect_ratio 完全失效。
//
// 官方推荐宽高像素值（方式 2）：4.x/5.x 系列 2K 档（总像素须 ≥ 3_686_400）；3.0-t2i 系列
// 1K 档（单边 ∈ [512, 2048]）。
var (
	arkSeedream2KSize = map[string]string{
		"1:1":  "2048x2048",
		"4:3":  "2304x1728",
		"3:4":  "1728x2304",
		"16:9": "2848x1600",
		"9:16": "1600x2848",
		"3:2":  "2496x1664",
		"2:3":  "1664x2496",
		"21:9": "3136x1344",
	}
	arkSeedream1KSize = map[string]string{
		"1:1":  "1024x1024",
		"4:3":  "1152x864",
		"3:4":  "864x1152",
		"16:9": "1280x720",
		"9:16": "720x1280",
		"3:2":  "1248x832",
		"2:3":  "832x1248",
		"21:9": "1512x648",
	}
)

// arkResolveSeedreamSize 按模型族选尺寸表；未识别比例时回退到分辨率 keyword（方式 1，由模型
// 按 prompt 自适应）。
func arkResolveSeedreamSize(modelID, aspectRatio string) string {
	mid := strings.ToLower(modelID)
	if strings.Contains(mid, "seedream-3") {
		if size, ok := arkSeedream1KSize[aspectRatio]; ok {
			return size
		}
		return "1K"
	}
	// 默认按 4.x/5.x 处理（含 lite 与未来兼容版本）。
	if size, ok := arkSeedream2KSize[aspectRatio]; ok {
		return size
	}
	return "2K"
}

// arkImageBackend 是 Seedream 图片生成后端实例。
type arkImageBackend struct {
	model   string
	baseURL string
	apiKey  string
	client  *httpc.Client
}

// newArkImageBackend 按通用信封构造；model / base_url 留空时由本后端自行兜底（默认 model、
// 官方端点）。api_key 缺失不在此报错——由装配方决定是否 fail loud。
func newArkImageBackend(opts provider.BackendOptions) *arkImageBackend {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = arkDefaultImageModel
	}
	return &arkImageBackend{
		model:   model,
		baseURL: arkEffectiveBaseURL(opts.BaseURL),
		apiKey:  opts.APIKey,
		client:  httpc.NewClient(arkImageRequestTimeout),
	}
}

// Name 返回供应商标识。
func (b *arkImageBackend) Name() string { return arkProvider }

// Model 返回实际下发的模型名。
func (b *arkImageBackend) Model() string { return b.model }

// Capabilities 返回本实例的能力位：Seedream 同时支持文生图与图生图。
func (b *arkImageBackend) Capabilities() provider.ImageCapabilitySet {
	return provider.NewImageCapabilitySet(provider.CapTextToImage, provider.CapImageToImage)
}

// arkImageResponse 是 images 接口的响应形状。只取用得上的字段，未知字段不解析。
type arkImageResponse struct {
	Data []struct {
		B64JSON string `json:"b64_json"`
		URL     string `json:"url"`
	} `json:"data"`
}

// Generate 生成一张图片并落盘到 req.OutputPath。
func (b *arkImageBackend) Generate(
	ctx context.Context, req provider.ImageGenerationRequest,
) (provider.ImageGenerationResult, error) {
	payload := map[string]any{
		"model":  b.model,
		"prompt": req.Prompt,
	}

	// 优先使用 caller 显式传入的 image_size（如 grid 路径会传 "2K"/"4K"），否则按官方推荐表
	// 从 aspect_ratio 推导宽高。
	size := strings.TrimSpace(req.ImageSize)
	if size == "" {
		size = arkResolveSeedreamSize(b.model, req.AspectRatio)
	}
	payload["size"] = size

	// I2I：读取参考图并转为 base64 data URI。单张传字符串，多张传列表。
	if len(req.ReferenceImages) > 0 {
		dataURIs := make([]string, 0, len(req.ReferenceImages))
		for _, ref := range req.ReferenceImages {
			dataURI, err := imageToBase64DataURI(ref.Path)
			if err != nil {
				return provider.ImageGenerationResult{}, err
			}
			dataURIs = append(dataURIs, dataURI)
		}
		if len(dataURIs) == 1 {
			payload["image"] = dataURIs[0]
		} else {
			payload["image"] = dataURIs
		}
	}

	if req.Seed != nil {
		payload["seed"] = *req.Seed
	}

	resp, err := b.client.PostJSON(
		ctx, httpc.JoinURL(b.baseURL, "/images/generations"), arkAuthHeaders(b.apiKey), payload,
	)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}

	var parsed arkImageResponse
	if err := decodeJSON(resp.Body, &parsed); err != nil {
		return provider.ImageGenerationResult{}, err
	}
	if len(parsed.Data) == 0 {
		// 空 data 通常是内容安全过滤命中或上游网关异常，不是可重试的瞬态错误——重试同一 prompt
		// 只会再撞一次同一道过滤。
		return provider.ImageGenerationResult{}, &provider.NonRetryableError{
			Err: fmt.Errorf(
				"Ark 图片生成响应 data 为空 (model=%s)，可能触发内容安全过滤或上游服务异常", b.model,
			),
		}
	}

	written, err := saveImageItem(ctx, b.client, parsed.Data[0].B64JSON, parsed.Data[0].URL, req.OutputPath)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}

	return provider.ImageGenerationResult{
		ImagePath: written,
		Provider:  arkProvider,
		Model:     b.model,
	}, nil
}

func init() {
	create := func(opts provider.BackendOptions) (provider.ImageBackend, error) {
		return newArkImageBackend(opts), nil
	}
	// ark 与 ark-agent-plan 同用本实现；两键的域名差异由装配层经 BackendOptions.BaseURL 注入。
	provider.RegisterImageBackend("ark", provider.ImageBackendRegistration{Create: create})
	provider.RegisterImageBackend("ark-agent-plan", provider.ImageBackendRegistration{Create: create})
}
