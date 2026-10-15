package textgen

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

const (
	// geminiDefaultBaseURL 是 AI Studio（Gemini Developer API）默认端点根，与 providers 包同值。
	geminiDefaultBaseURL = "https://generativelanguage.googleapis.com/"
	geminiAPIVersion     = "v1beta"
	// geminiHTTPTimeout 与 providers 包 Gemini 族的出站超时同值。
	geminiHTTPTimeout = 600 * time.Second
	// geminiFinishMaxTokens 是 generateContent 因输出上限结束时的 finishReason。
	geminiFinishMaxTokens = "MAX_TOKENS"
)

// geminiAistudio 是 AI Studio 形态的 Gemini 文本后端，经 REST generateContent 调用。
type geminiAistudio struct {
	apiKey  string
	baseURL string
	model   string
	client  *httpc.Client
}

// newGeminiAistudio 构造后端。apiKey 缺失时 fail loud，不回落任何默认值。
// baseURL 为空时使用 AI Studio 默认端点。
func newGeminiAistudio(apiKey, baseURL, model string) (*geminiAistudio, error) {
	key := strings.TrimSpace(apiKey)
	if key == "" {
		return nil, errors.New("gemini-aistudio 未配置 api_key")
	}
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = geminiDefaultBaseURL
	}
	return &geminiAistudio{
		apiKey:  key,
		baseURL: base,
		model:   model,
		client:  httpc.NewClient(geminiHTTPTimeout),
	}, nil
}

// geminiGenerateResponse 是 generateContent 响应中本实现用到的字段。
type geminiGenerateResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text    string `json:"text"`
				Thought bool   `json:"thought"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
}

// firstCandidate 返回首个候选的文本（去首尾空白，跳过 thought 片段）与结束原因。无候选时均为空。
func (r geminiGenerateResponse) firstCandidate() (string, string) {
	if len(r.Candidates) == 0 {
		return "", ""
	}
	first := r.Candidates[0]
	var b strings.Builder
	for _, part := range first.Content.Parts {
		if !part.Thought {
			b.WriteString(part.Text)
		}
	}
	return strings.TrimSpace(b.String()), first.FinishReason
}

// Generate 实现 Generator。schema 非 nil 时以 responseMimeType=application/json + responseSchema
// 请求结构化输出，并在返回前校验其为合法 JSON。
//
// 提交阶段沿用非幂等 POST 的重试语义：只重试确定未送达的连接类失败与 5xx/429 等瞬态状态码，
// 避免重复计费。
func (g *geminiAistudio) Generate(
	ctx context.Context, prompt string, schema map[string]any, maxOutputTokens int,
) (string, error) {
	generationConfig := map[string]any{}
	if schema != nil {
		responseSchema, err := geminiResponseSchema(schema)
		if err != nil {
			return "", err
		}
		generationConfig["responseMimeType"] = "application/json"
		generationConfig["responseSchema"] = responseSchema
	}
	if maxOutputTokens > 0 {
		generationConfig["maxOutputTokens"] = maxOutputTokens
	}
	payload := map[string]any{
		"contents": []any{map[string]any{
			"role":  "user",
			"parts": []any{map[string]any{"text": prompt}},
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
		if schema != nil {
			return "", fmt.Errorf("gemini-aistudio/%s 的结构化输出被输出上限截断，内容不完整", g.model)
		}
		slog.Warn("gemini-aistudio 文本输出被输出上限截断", "model", g.model, "max_output_tokens", maxOutputTokens)
	}
	if schema != nil && !json.Valid([]byte(text)) {
		return "", &provider.NonRetryableError{Err: fmt.Errorf("gemini-aistudio/%s 的结构化输出不是合法 JSON", g.model)}
	}
	return text, nil
}

// geminiInstanceKeywords 的值是实例数据，原样保留、不递归（与 Python _INSTANCE_KEYWORDS 同值）。
var geminiInstanceKeywords = map[string]bool{"const": true, "enum": true, "default": true, "examples": true}

// geminiSubschemaMapKeys 的值是「名字 → 子 schema」映射，其 key 不是 schema 关键字（与 Python _SUBSCHEMA_MAP_KEYS 同值）。
var geminiSubschemaMapKeys = map[string]bool{
	"properties": true, "patternProperties": true, "$defs": true, "definitions": true, "dependentSchemas": true,
}

// geminiResponseSchema 把 JSON Schema 归一为 Gemini responseSchema 可表达的形态（与 Python _to_response_schema 同语义）：
//   - 标量 const 改写为单值 enum；
//   - 含非字符串成员的 enum 改写为字符串 enum 并置 type=string；
//   - 顶层 $defs 丢弃。
//
// 输入先经 JSON 往返，统一成 JSON 解码后的形态（整数即 float64），调用方可传任意 Go 类型的 map。
// 含 $ref 时报错：responseSchema 不支持引用，调用方须传内联 schema。
func geminiResponseSchema(schema map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("序列化 response schema: %w", err)
	}
	var canonical any
	if err := json.Unmarshal(raw, &canonical); err != nil {
		return nil, fmt.Errorf("解析 response schema: %w", err)
	}
	node, err := normalizeSchemaNode(canonical, false)
	if err != nil {
		return nil, err
	}
	normalized, ok := node.(map[string]any)
	if !ok {
		return nil, errors.New("response schema 必须是 JSON 对象")
	}
	delete(normalized, "$defs")
	return normalized, nil
}

// normalizeSchemaNode 递归归一 schema 节点。subschemaMap 为 true 时，node 的 key 是属性名 / 定义名，
// 其值才是子 schema，因此 key 不按关键字解释（属性名恰为 const 时也不误改）。
func normalizeSchemaNode(node any, subschemaMap bool) (any, error) {
	switch v := node.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			n, err := normalizeSchemaNode(item, false)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if subschemaMap {
				n, err := normalizeSchemaNode(item, false)
				if err != nil {
					return nil, err
				}
				out[key] = n
				continue
			}
			if key == "$ref" {
				return nil, errors.New("response schema 不支持 $ref，调用方须传入内联 schema")
			}
			if geminiInstanceKeywords[key] {
				out[key] = item
				continue
			}
			n, err := normalizeSchemaNode(item, geminiSubschemaMapKeys[key])
			if err != nil {
				return nil, err
			}
			out[key] = n
		}
		if c, ok := out["const"]; ok && isJSONScalar(c) {
			delete(out, "const")
			out["enum"] = []any{c}
		}
		if values, ok := out["enum"].([]any); ok && hasNonStringMember(values) {
			strs := make([]any, len(values))
			for i, x := range values {
				strs[i] = fmt.Sprint(x)
			}
			out["enum"] = strs
			out["type"] = "string"
		}
		return out, nil
	default:
		return node, nil
	}
}

// isJSONScalar 判断 JSON 解码后的值是否为标量（null / 字符串 / 布尔 / 数字）。
func isJSONScalar(v any) bool {
	switch v.(type) {
	case nil, string, bool, float64:
		return true
	default:
		return false
	}
}

// hasNonStringMember 判断 enum 成员中是否存在非字符串。
func hasNonStringMember(values []any) bool {
	for _, x := range values {
		if _, ok := x.(string); !ok {
			return true
		}
	}
	return false
}
