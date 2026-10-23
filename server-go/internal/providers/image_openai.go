// Package providers 承载各内置供应商 backend 的实现（对应 Python lib/{image,video,audio}_backends）。
//
// 契约（接口、能力声明、注册表、失败码、重试谓词）在 internal/provider，本包只做「怎么跟某家
// 供应商说话」。实现在 init 时把自己注册进 provider 的 backend registry，装配方 import 本包
// 即完成装载——注册表不反向依赖本包。
package providers

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// OpenAI 供应商标识与默认端点。
const (
	// ProviderOpenAI 与 registry 的 provider_id 同名。
	ProviderOpenAI = "openai"

	openAIDefaultImageModel = "gpt-image-2"
	openAIDefaultBaseURL    = "https://api.openai.com/v1"

	// openAIImageRequestTimeout 单次生成请求的墙钟预算。
	//
	// 图片请求是「一次调用 = 一次计费」的形态，超时后重试等于二次计费，故这里给整个 generate
	// 一个显式上界，而不是让内层重试各自重新获得完整超时。
	openAIImageRequestTimeout = 600 * time.Second
)

// gpt-image-2 的像素约束：size 接受任意 WxH，宽高均被 16 整除，比例 1:3~3:1；
// ≤2560 长边稳定，~3840 实验性，最大 3840。
const (
	openAIImageRoundTo        = 16
	openAIImageMaxLongEdge    = 3840
	openAIImageStableLongEdge = 2560
	openAIImageMaxRatio       = 3.0
	openAIMaxReferenceImages  = 16
)

// openAIImageQuality 档位 → quality（大小写不敏感：用户自定义档位可能写成 "2k" / "4K "，
// 不归一会让 size 解析成功但 quality 静默丢失）。
var openAIImageQuality = map[string]string{
	"512px": "low",
	"1k":    "medium",
	"2k":    "high",
	"4k":    "high",
}

// imageBackendMode 决定一个实例支持 T2I / I2I / 两者（Python ImageBackendMode 同义），
// 自定义供应商按 endpoint 只开通其一。
type imageBackendMode string

const (
	modeBoth            imageBackendMode = "both"
	modeGenerationsOnly imageBackendMode = "generations_only"
	modeEditsOnly       imageBackendMode = "edits_only"
)

// modeCapabilities 模式 → 能力位。
var modeCapabilities = map[imageBackendMode]provider.ImageCapabilitySet{
	modeBoth:            provider.NewImageCapabilitySet(provider.CapTextToImage, provider.CapImageToImage),
	modeGenerationsOnly: provider.NewImageCapabilitySet(provider.CapTextToImage),
	modeEditsOnly:       provider.NewImageCapabilitySet(provider.CapImageToImage),
}

// OpenAIImageBackend 是 OpenAI 图片生成后端：无参考图走 /images/generations（T2I），
// 有参考图走 /images/edits（I2I）。直接走 REST，不引第三方 SDK——官方 Go SDK 不覆盖
// 图片编辑的多文件表单形态，且自建能精确控制请求形态与错误分类。
type OpenAIImageBackend struct {
	model   string
	baseURL string
	apiKey  string
	client  *httpc.Client
	caps    provider.ImageCapabilitySet
}

// NewOpenAIImageBackend 按通用信封构造；model / base_url 留空时由本后端自行兜底
// （默认 model、官方端点），api_key 缺失不在此报错——由装配方决定是否 fail loud。
func NewOpenAIImageBackend(opts provider.BackendOptions) (*OpenAIImageBackend, error) {
	mode := modeBoth
	if raw := strings.TrimSpace(opts.Extra["mode"]); raw != "" {
		mode = imageBackendMode(raw)
	}
	caps, ok := modeCapabilities[mode]
	if !ok {
		return nil, fmt.Errorf("unknown openai image backend mode: %q", mode)
	}
	baseURL := strings.TrimSpace(opts.BaseURL)
	if baseURL == "" {
		baseURL = openAIDefaultBaseURL
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = openAIDefaultImageModel
	}
	return &OpenAIImageBackend{
		model:   model,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  opts.APIKey,
		client:  httpc.NewClient(openAIImageRequestTimeout),
		caps:    caps,
	}, nil
}

// Name 返回供应商标识。
func (b *OpenAIImageBackend) Name() string { return ProviderOpenAI }

// Model 返回实际下发的模型名。
func (b *OpenAIImageBackend) Model() string { return b.model }

// Capabilities 返回本实例的能力位（由 mode 决定）。
func (b *OpenAIImageBackend) Capabilities() provider.ImageCapabilitySet { return b.caps }

// openAIImageResponse 是 images 接口的响应形状。只取用得上的字段，未知字段不解析。
type openAIImageResponse struct {
	Data []struct {
		B64JSON       string `json:"b64_json"`
		URL           string `json:"url"`
		RevisedPrompt string `json:"revised_prompt"`
	} `json:"data"`
	Usage *struct {
		InputTokens        *int64 `json:"input_tokens"`
		OutputTokens       *int64 `json:"output_tokens"`
		InputTokensDetails *struct {
			ImageTokens *int64 `json:"image_tokens"`
			TextTokens  *int64 `json:"text_tokens"`
		} `json:"input_tokens_details"`
		OutputTokensDetails *struct {
			ImageTokens *int64 `json:"image_tokens"`
			TextTokens  *int64 `json:"text_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

// Generate 生成一张图片并落盘到 req.OutputPath。
//
// 能力闸门先于请求：不支持的路径直接返回 *provider.CapabilityError，不发请求、不计费。
func (b *OpenAIImageBackend) Generate(
	ctx context.Context, req provider.ImageGenerationRequest,
) (provider.ImageGenerationResult, error) {
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

	var (
		resp *httpc.Response
		err  error
	)
	if hasRefs {
		resp, err = b.generateEdit(ctx, req)
	} else {
		resp, err = b.generateCreate(ctx, req)
	}
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}

	var parsed openAIImageResponse
	if err := decodeJSON(resp.Body, &parsed); err != nil {
		return provider.ImageGenerationResult{}, err
	}
	if len(parsed.Data) == 0 {
		// 空 data 通常是内容安全过滤命中或上游网关异常；不是可重试的瞬态错误——
		// 重试同一 prompt 只会再撞一次同一道过滤。
		return provider.ImageGenerationResult{}, &provider.NonRetryableError{
			Err: fmt.Errorf("openai image response has empty data (model=%s)", b.model),
		}
	}
	item := parsed.Data[0]
	written, err := saveOpenAIImage(ctx, b.client, item.B64JSON, item.URL, req.OutputPath)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}

	result := provider.ImageGenerationResult{
		ImagePath: written,
		Provider:  ProviderOpenAI,
		Model:     b.model,
		Quality:   qualityFor(req.ImageSize),
	}
	if parsed.Usage != nil {
		// 必须拿到 input 拆分（image_tokens / text_tokens 至少一项有值）才填明细，否则留空
		// 让计费侧走静态 fallback——部分字段缺失时漏算 input 费用比不算更糟。
		if details := parsed.Usage.InputTokensDetails; details != nil &&
			(details.ImageTokens != nil || details.TextTokens != nil) {
			result.ImageInputTokens = details.ImageTokens
			result.TextInputTokens = details.TextTokens
			var imgOut, txtOut *int64
			if out := parsed.Usage.OutputTokensDetails; out != nil {
				imgOut, txtOut = out.ImageTokens, out.TextTokens
			}
			if imgOut == nil {
				// 部分模型只在顶层暴露 output_tokens（GPT Image 输出基本为 image token）。
				imgOut = parsed.Usage.OutputTokens
			}
			if imgOut != nil || txtOut != nil {
				result.ImageOutputTokens = imgOut
				result.TextOutputTokens = txtOut
			}
			// 输入拆分到手但输出完全拿不到 → 数据残缺，撤回让计费侧走静态 fallback。
			if result.ImageOutputTokens == nil && result.TextOutputTokens == nil {
				result.ImageInputTokens, result.TextInputTokens = nil, nil
			}
		}
		if result.ImageInputTokens == nil && result.ImageOutputTokens == nil && parsed.Usage.InputTokens != nil {
			// 无拆分明细时保留总 token 数，供计费侧按总量估算。
			total := parsed.Usage.InputTokens
			result.UsageTokens = total
		}
	}
	return result, nil
}

// generateCreate 走 /images/generations（T2I）。
func (b *OpenAIImageBackend) generateCreate(
	ctx context.Context, req provider.ImageGenerationRequest,
) (*httpc.Response, error) {
	payload := map[string]any{
		"model":  b.model,
		"prompt": req.Prompt,
		"n":      1,
	}
	for key, value := range resolveOpenAIImageParams(req.ImageSize, req.AspectRatio) {
		payload[key] = value
	}
	return b.client.PostJSON(ctx, httpc.JoinURL(b.baseURL, "/images/generations"), b.authHeaders(), payload)
}

// generateEdit 走 /images/edits（I2I，multipart 多文件）。
func (b *OpenAIImageBackend) generateEdit(
	ctx context.Context, req provider.ImageGenerationRequest,
) (*httpc.Response, error) {
	refs := req.ReferenceImages
	if len(refs) > openAIMaxReferenceImages {
		refs = refs[:openAIMaxReferenceImages]
	}
	files := make([]httpc.FilePart, 0, len(refs))
	for _, ref := range refs {
		content, err := os.ReadFile(ref.Path)
		if err != nil {
			// 参考图打不开就跳过（文件已删除 / 权限），全部打不开才有后续的能力错误。
			continue
		}
		files = append(files, httpc.FilePart{
			FieldName: "image",
			FileName:  filepath.Base(ref.Path),
			Content:   content,
		})
	}
	if len(files) == 0 {
		// 所有参考图都打不开：抛错而非降级到 T2I——用户提交的是 i2i 请求但没有有效素材，
		// 静默降级会产出与意图完全无关、却照常扣费的图。
		return nil, provider.NewImageCapabilityError(
			provider.FailureImageEndpointMismatchNoI2I, map[string]any{"model": b.model},
		)
	}
	fields := map[string]string{"model": b.model, "prompt": req.Prompt}
	for key, value := range resolveOpenAIImageParams(req.ImageSize, req.AspectRatio) {
		fields[key] = value
	}
	return b.client.PostMultipart(ctx, httpc.JoinURL(b.baseURL, "/images/edits"), b.authHeaders(), fields, files)
}

// authHeaders 构造鉴权头。
func (b *OpenAIImageBackend) authHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + b.apiKey}
}

// resolveOpenAIImageParams 按「比例优先、清晰度其次」算出 {size, quality}。
//
// 比例永远来自 aspect_ratio；image_size（档位 / 自定义 WxH / 空）只决定清晰度短边，自定义
// WxH 剥离其自带比例（取 min 当短边）。image_size 为空时按默认短边兜底，仍下传精确比例
// size——不回退上游默认（否则中转网关会用自家默认比例，丢掉项目比例）。
func resolveOpenAIImageParams(imageSize, aspectRatio string) map[string]string {
	short := project.ResolutionToShortEdge(imageSize, project.ImageTierShortEdge, project.DefaultShortEdge)
	maxLong := openAIImageMaxLongEdge
	ratio := openAIImageMaxRatio
	w, h := project.AspectSize(aspectRatio, short, project.AspectSizeOptions{
		RoundTo:     openAIImageRoundTo,
		MaxLongEdge: &maxLong,
		MaxRatio:    &ratio,
	})
	params := map[string]string{"size": fmt.Sprintf("%dx%d", w, h)}
	if quality := qualityFor(imageSize); quality != "" {
		params["quality"] = quality
	}
	return params
}

// qualityFor 档位 → quality（大小写不敏感）；自定义 WxH / 空 → 空串。
func qualityFor(imageSize string) string {
	trimmed := strings.TrimSpace(strings.ToLower(imageSize))
	if trimmed == "" {
		return ""
	}
	return openAIImageQuality[trimmed]
}

// saveOpenAIImage 把响应里的图片落到 outputPath：优先 b64（无需二次出网），否则下载 url。
//
// 落盘用「临时文件 + rename」：直接写目标路径会在写入中途崩溃时留下半截文件，而版本库与缩略图
// 都会把它当成一张有效图片读进来。
func saveOpenAIImage(ctx context.Context, client *httpc.Client, b64JSON, imageURL, outputPath string) (string, error) {
	var content []byte
	switch {
	case strings.TrimSpace(b64JSON) != "":
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64JSON))
		if err != nil {
			return "", fmt.Errorf("decode b64_json: %w", err)
		}
		content = decoded
	case strings.TrimSpace(imageURL) != "":
		resp, err := client.Get(ctx, strings.TrimSpace(imageURL), nil)
		if err != nil {
			return "", err
		}
		content = resp.Body
	default:
		return "", fmt.Errorf("openai image response item carries neither b64_json nor url")
	}
	if len(content) == 0 {
		return "", fmt.Errorf("openai image response item is empty")
	}
	if err := writeFileAtomic(outputPath, content); err != nil {
		return "", err
	}
	return outputPath, nil
}

// writeFileAtomic 先写同目录临时文件再 rename，避免中途失败留下半截产物。
func writeFileAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create output dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".shotwise-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// rename 成功后该文件已不存在，删除失败无需处理。
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp image: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp image: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename image into place: %w", err)
	}
	return nil
}

func init() {
	provider.RegisterImageBackend("openai", provider.ImageBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.ImageBackend, error) {
			return NewOpenAIImageBackend(opts)
		},
	})
}
