package gemini

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/providers/provkit"
)

// geminiImageRetryPolicy 与 Python GeminiImageBackend.generate 的
// with_retry_async(max_attempts=5, backoff_seconds=(2, 4, 8, 16, 32)) 同值。
var geminiImageRetryPolicy = provider.RetryPolicy{
	MaxAttempts:    5,
	BackoffSeconds: []int{2, 4, 8, 16, 32},
}

// geminiImageSkipNamePatterns 是名称推断时跳过的文件名前缀（Python SKIP_NAME_PATTERNS）。
var geminiImageSkipNamePatterns = []string{"scene_", "storyboard_", "output_"}

// geminiImageBackend 是 Gemini 图片生成后端，支持 AI Studio 与 Vertex AI（Python GeminiImageBackend）。
type geminiImageBackend struct {
	auth  *geminiAuth
	model string
	caps  provider.ImageCapabilitySet
}

// geminiInlineImage 是响应 / 请求里内联图片的承载结构。
type geminiInlineImage struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

// geminiResponsePart 是 generateContent 响应 content.parts 的一个条目。
type geminiResponsePart struct {
	InlineData *geminiInlineImage `json:"inlineData"`
}

// geminiImageResponse 是 generateContent 的响应形状（只取内含图片的 parts）。
type geminiImageResponse struct {
	Candidates []struct {
		Content struct {
			Parts []geminiResponsePart `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

// NewImageBackend 按通用信封构造。模型名优先取具名参数 image_model，其次取通用 Model 字段。
func NewImageBackend(opts provider.BackendOptions) (*geminiImageBackend, error) {
	auth, err := newGeminiAuth(opts)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(opts.Extra["image_model"])
	if model == "" {
		model = strings.TrimSpace(opts.Model)
	}
	if model == "" {
		model = geminiDefaultImageModel
	}
	return &geminiImageBackend{
		auth:  auth,
		model: model,
		caps:  provider.NewImageCapabilitySet(provider.CapTextToImage, provider.CapImageToImage),
	}, nil
}

// Name 返回 backend 标识（Python name = f"gemini-{backend_type}"）。
func (b *geminiImageBackend) Name() string { return "gemini-" + b.auth.backendType }

// Model 返回实际下发的模型名。
func (b *geminiImageBackend) Model() string { return b.model }

// Capabilities 返回能力位：文生图 + 图生图。
func (b *geminiImageBackend) Capabilities() provider.ImageCapabilitySet { return b.caps }

// Generate 生成一张图片并落盘到 req.OutputPath。
//
// contents 形状：[标签1, 图片1, ..., prompt]，responseModalities=["IMAGE"]。
func (b *geminiImageBackend) Generate(
	ctx context.Context, req provider.ImageGenerationRequest,
) (provider.ImageGenerationResult, error) {
	parts, err := b.buildContents(req)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}

	imageConfig := map[string]any{"aspectRatio": req.AspectRatio}
	if strings.TrimSpace(req.ImageSize) != "" {
		imageConfig["imageSize"] = req.ImageSize
	}
	payload := map[string]any{
		"contents": []any{map[string]any{"parts": parts}},
		"generationConfig": map[string]any{
			"responseModalities": []string{"IMAGE"},
			"imageConfig":        imageConfig,
		},
	}

	headers, err := b.auth.headers(ctx)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	resp, err := geminiRequestJSON(
		ctx, b.auth.client, geminiImageRetryPolicy, b.auth.imageURL(b.model), headers, payload,
	)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}

	var parsed geminiImageResponse
	if err := provkit.DecodeJSON(resp.Body, &parsed); err != nil {
		return provider.ImageGenerationResult{}, err
	}
	if len(parsed.Candidates) == 0 {
		return provider.ImageGenerationResult{}, &provider.NonRetryableError{Err: errors.New("API 未返回图片")}
	}
	written, err := saveGeminiInlineImage(parsed.Candidates[0].Content.Parts, req.OutputPath)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}

	return provider.ImageGenerationResult{
		ImagePath: written,
		Provider:  geminiProviderID,
		Model:     b.model,
	}, nil
}

// buildContents 构建带名称标签的 contents parts：每个参考图前插入其标签（若有），prompt 放最后。
func (b *geminiImageBackend) buildContents(req provider.ImageGenerationRequest) ([]any, error) {
	parts := make([]any, 0, len(req.ReferenceImages)*2+1)
	for _, ref := range req.ReferenceImages {
		label := strings.TrimSpace(ref.Label)
		name := label
		if name == "" {
			name = geminiImageNameFromPath(ref.Path)
		}
		if name != "" {
			parts = append(parts, map[string]any{"text": name})
		}
		imagePart, err := geminiInlineImagePart(ref.Path)
		if err != nil {
			return nil, err
		}
		parts = append(parts, imagePart)
	}
	parts = append(parts, map[string]any{"text": req.Prompt})
	return parts, nil
}

// saveGeminiInlineImage 取第一个带 inlineData 的 part 落盘；无则报错（Python「API 未返回图片」）。
func saveGeminiInlineImage(parts []geminiResponsePart, outputPath string) (string, error) {
	for _, part := range parts {
		if part.InlineData == nil || strings.TrimSpace(part.InlineData.Data) == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(part.InlineData.Data))
		if err != nil {
			return "", fmt.Errorf("decode inline image: %w", err)
		}
		if len(decoded) == 0 {
			continue
		}
		if err := provkit.WriteFileAtomic(outputPath, decoded); err != nil {
			return "", err
		}
		return outputPath, nil
	}
	return "", &provider.NonRetryableError{Err: errors.New("API 未返回图片")}
}

// geminiInlineImagePart 读取本地图片字节，按后缀定 MIME，构造 inlineData part。
func geminiInlineImagePart(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取参考图片失败 %s: %w", path, err)
	}
	return map[string]any{
		"inlineData": map[string]any{
			"mimeType": provkit.MimeForImagePath(path),
			"data":     base64.StdEncoding.EncodeToString(data),
		},
	}, nil
}

// geminiImageNameFromPath 从图片路径推断名称，跳过 scene_/storyboard_/output_ 前缀。
func geminiImageNameFromPath(path string) string {
	base := filepath.Base(path)
	filename := strings.TrimSuffix(base, filepath.Ext(base))
	for _, pattern := range geminiImageSkipNamePatterns {
		if strings.HasPrefix(filename, pattern) {
			return ""
		}
	}
	return filename
}

func init() {
	provider.RegisterImageBackend("gemini", provider.ImageBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.ImageBackend, error) {
			return NewImageBackend(opts)
		},
	})
}
