package textgen

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/provider"
)

// capturedRequest 记录离线 Gemini 端点收到的请求，供断言使用。
type capturedRequest struct {
	path   string
	apiKey string
	body   map[string]any
	calls  int
}

// newGeminiTestServer 起一个返回固定响应的 generateContent 端点。
func newGeminiTestServer(t *testing.T, status int, respBody any) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		captured.calls++
		captured.path = r.URL.Path
		captured.apiKey = r.Header.Get("x-goog-api-key")
		_ = json.Unmarshal(raw, &captured.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

// geminiTextBody 构造只含单个文本 part 的 generateContent 响应。
func geminiTextBody(text, finishReason string) map[string]any {
	return map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": []any{map[string]any{"text": text}}},
			"finishReason": finishReason,
		}},
	}
}

func TestGeminiAistudioStructuredRequest(t *testing.T) {
	srv, captured := newGeminiTestServer(t, http.StatusOK, geminiTextBody(`{"kind":"a"}`, "STOP"))
	gen, err := newGeminiAistudio("k-test", srv.URL, "gemini-3-flash-preview")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind":  map[string]any{"const": "a"},
			"level": map[string]any{"type": "integer", "enum": []int{4, 6, 8}},
		},
		"required": []string{"kind"},
		"$defs":    map[string]any{"unused": map[string]any{"type": "string"}},
	}

	got, err := gen.Generate(context.Background(), "分析关系", schema, 1234)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got != `{"kind":"a"}` {
		t.Fatalf("text = %q", got)
	}
	if captured.path != "/v1beta/models/gemini-3-flash-preview:generateContent" {
		t.Fatalf("path = %s", captured.path)
	}
	if captured.apiKey != "k-test" {
		t.Fatalf("x-goog-api-key = %q", captured.apiKey)
	}
	gc, ok := captured.body["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig 缺失: %v", captured.body)
	}
	if gc["responseMimeType"] != "application/json" {
		t.Fatalf("responseMimeType = %v", gc["responseMimeType"])
	}
	if gc["maxOutputTokens"] != float64(1234) {
		t.Fatalf("maxOutputTokens = %v", gc["maxOutputTokens"])
	}
	rs, ok := gc["responseSchema"].(map[string]any)
	if !ok {
		t.Fatalf("responseSchema 缺失: %v", gc)
	}
	if _, has := rs["$defs"]; has {
		t.Fatalf("顶层 $defs 应被丢弃: %v", rs)
	}
	props := rs["properties"].(map[string]any)
	kind := props["kind"].(map[string]any)
	if _, has := kind["const"]; has {
		t.Fatalf("const 应改写为 enum: %v", kind)
	}
	if enum := kind["enum"].([]any); len(enum) != 1 || enum[0] != "a" {
		t.Fatalf("kind.enum = %v", enum)
	}
	level := props["level"].(map[string]any)
	if level["type"] != "string" {
		t.Fatalf("level.type = %v", level["type"])
	}
	if enum := level["enum"].([]any); len(enum) != 3 || enum[0] != "4" || enum[2] != "8" {
		t.Fatalf("level.enum = %v", enum)
	}
}

func TestGeminiAistudioFreeTextOmitsGenerationConfig(t *testing.T) {
	body := map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{
				map[string]any{"text": "内部推理", "thought": true},
				map[string]any{"text": "  你好  "},
			}},
			"finishReason": "STOP",
		}},
	}
	srv, captured := newGeminiTestServer(t, http.StatusOK, body)
	gen, err := newGeminiAistudio("k", srv.URL, "m")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	got, err := gen.Generate(context.Background(), "hi", nil, 0)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got != "你好" {
		t.Fatalf("text = %q（thought 片段应被跳过并去除首尾空白）", got)
	}
	if _, has := captured.body["generationConfig"]; has {
		t.Fatalf("自由文本且无上限时不应带 generationConfig: %v", captured.body)
	}
}

func TestGeminiAistudioStructuredTruncatedIsError(t *testing.T) {
	srv, _ := newGeminiTestServer(t, http.StatusOK, geminiTextBody(`{"kind":`, "MAX_TOKENS"))
	gen, _ := newGeminiAistudio("k", srv.URL, "m")

	if _, err := gen.Generate(context.Background(), "p", map[string]any{"type": "object"}, 10); err == nil {
		t.Fatal("结构化输出被截断应报错")
	}
}

func TestGeminiAistudioStructuredNonJSONIsError(t *testing.T) {
	srv, _ := newGeminiTestServer(t, http.StatusOK, geminiTextBody("好的，结果如下", "STOP"))
	gen, _ := newGeminiAistudio("k", srv.URL, "m")

	if _, err := gen.Generate(context.Background(), "p", map[string]any{"type": "object"}, 0); err == nil {
		t.Fatal("结构化输出非 JSON 应报错")
	}
}

func TestGeminiAistudioClientErrorNotRetried(t *testing.T) {
	srv, captured := newGeminiTestServer(t, http.StatusBadRequest, map[string]any{"error": "bad"})
	gen, _ := newGeminiAistudio("k", srv.URL, "m")

	_, err := gen.Generate(context.Background(), "p", nil, 0)
	var statusErr *provider.HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v，期望 400 的 HTTPStatusError", err)
	}
	if captured.calls != 1 {
		t.Fatalf("400 不应重试，实际请求 %d 次", captured.calls)
	}
}

func TestNewGeminiAistudioRequiresAPIKey(t *testing.T) {
	if _, err := newGeminiAistudio("  ", "", "m"); err == nil {
		t.Fatal("api_key 缺失应报错")
	}
}

func TestGeminiResponseSchemaRejectsRef(t *testing.T) {
	_, err := geminiResponseSchema(map[string]any{
		"type":       "object",
		"properties": map[string]any{"x": map[string]any{"$ref": "#/$defs/x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "$ref") {
		t.Fatalf("err = %v，期望 $ref 报错", err)
	}
}

func TestGeminiResponseSchemaKeepsPropertyNamedConst(t *testing.T) {
	// 属性名恰为 const 时，其子 schema 仍需归一，而 const 本身不按关键字改写。
	got, err := geminiResponseSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"const": map[string]any{"enum": []any{1, 2}},
		},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	prop := got["properties"].(map[string]any)["const"].(map[string]any)
	if prop["type"] != "string" {
		t.Fatalf("属性 const 的子 schema 未归一: %v", prop)
	}
}
