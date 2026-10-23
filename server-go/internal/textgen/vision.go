package textgen

// 视觉输入路径（对应 Python 风格分析经 Gemini generateContent 的图像请求）：
// parts 同时携带文本 prompt 与 inline_data 图像。复用 geminiAistudio 的
// 端点/鉴权/重试语义，仅请求载荷多一个图像部件。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
	"gorm.io/gorm"
)

// StyleAnalysisPrompt 与 Python lib/text_backends/prompts.py 的
// STYLE_ANALYSIS_PROMPT 同文：只分析视觉风格，不描述主体内容。
const StyleAnalysisPrompt = "Analyze the visual style of this image. Describe the lighting, " +
	"color palette, medium (e.g., oil painting, digital art, photography), " +
	"texture, and overall mood. Do NOT describe the subject matter " +
	"(e.g., people, objects) or specific content. Focus ONLY on the " +
	"artistic style. Provide a concise comma-separated list of descriptors " +
	"suitable for an image generation prompt."

// GenerateWithImage 发起带图像输入的文本生成（风格分析用）。
// mediaType 形如 "image/png"；maxOutputTokens <= 0 表示不设上限。
func (g *geminiAistudio) GenerateWithImage(
	ctx context.Context, prompt string, imageData []byte, mediaType string, maxOutputTokens int,
) (string, error) {
	if mediaType == "" {
		mediaType = "image/png"
	}
	generationConfig := map[string]any{}
	if maxOutputTokens > 0 {
		generationConfig["maxOutputTokens"] = maxOutputTokens
	}
	payload := map[string]any{
		"contents": []any{map[string]any{
			"role": "user",
			"parts": []any{
				map[string]any{"text": prompt},
				map[string]any{
					"inline_data": map[string]any{
						"mime_type": mediaType,
						"data":      base64.StdEncoding.EncodeToString(imageData),
					},
				},
			},
		}},
	}
	if len(generationConfig) > 0 {
		payload["generationConfig"] = generationConfig
	}

	target := httpc.JoinURL(g.baseURL, geminiAPIVersion+"/models/"+g.model+":generateContent")
	headers := map[string]string{"x-goog-api-key": g.apiKey}
	var resp *httpc.Response
	err := provider.RunWithRetry(ctx, provider.DefaultRetryPolicy, provider.SubmitRetryable,
		func(ctx context.Context) error {
			result, err := g.client.PostJSON(ctx, target, headers, payload)
			if err != nil {
				return provider.ClassifySubmitError(providerGeminiAistudio, err)
			}
			resp = result
			return nil
		})
	if err != nil {
		return "", err
	}

	var parsed geminiGenerateResponse
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return "", &provider.NonRetryableError{Err: fmt.Errorf("decode gemini response: %w", err)}
	}
	text, finishReason := parsed.firstCandidate()
	if finishReason == geminiFinishMaxTokens {
		return "", fmt.Errorf("%w: gemini-aistudio/%s 的风格分析被输出上限截断", ErrOutputTruncated, g.model)
	}
	return strings.TrimSpace(text), nil
}

// VisionGenerator 是带图像输入的文本生成接口。
type VisionGenerator interface {
	GenerateWithImage(ctx context.Context, prompt string, imageData []byte, mediaType string, maxOutputTokens int) (string, error)
}

// NewVisionGenerator 按项目解析出的文本 provider 构造视觉生成器。
// 解析到的 provider 不支持图像输入时返回 ErrUnsupportedProvider。
func NewVisionGenerator(ctx context.Context, db *gorm.DB, project *project.ProjectFile, task TaskType) (VisionGenerator, error) {
	gen, err := NewGenerator(ctx, db, project, task)
	if err != nil {
		return nil, err
	}
	vg, ok := gen.(VisionGenerator)
	if !ok {
		return nil, fmt.Errorf("%w: 当前文本 provider 不支持图像输入", ErrUnsupportedProvider)
	}
	return vg, nil
}
