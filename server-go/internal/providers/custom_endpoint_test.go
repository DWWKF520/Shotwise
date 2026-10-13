package providers

import (
	"reflect"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/provider"
)

// customParseDeclaration 解析一份声明，解析失败即终止用例。
func customParseDeclaration(t *testing.T, raw map[string]any) *CustomEndpointDeclaration {
	t.Helper()
	declaration, err := ParseCustomEndpointDeclaration(raw)
	if err != nil {
		t.Fatalf("parse declaration: %v", err)
	}
	return declaration
}

// TestParseCustomEndpointDeclarationRejectsInvalid 断言非法声明一律 fail loud。
func TestParseCustomEndpointDeclarationRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{"unknown field", map[string]any{"path": "/x", "bogus": 1}, "unknown fields"},
		{"non-post method", map[string]any{"method": "GET", "path": "/x"}, "not allowed"},
		{"missing path", map[string]any{"method": "POST"}, "non-empty relative path"},
		{"path traversal", map[string]any{"path": "/a/../b"}, "path traversal"},
		{"absolute url", map[string]any{"path": "https://host/x"}, "absolute-path"},
		{"bad path placeholder", map[string]any{"path": "/x/{bad name}"}, "simple field names"},
		{
			"body and template", map[string]any{
				"path": "/x", "body": map[string]any{"/prompt": "prompt"},
				"body_template": map[string]any{"p": "{prompt}"},
			}, "mutually exclusive",
		},
		{"template unknown input", map[string]any{"path": "/x", "body_template": map[string]any{"p": "{bogus}"}}, "not an allowed input"},
		{"template unclosed placeholder", map[string]any{"path": "/x", "body_template": map[string]any{"p": "{prompt"}}, "unclosed"},
		{"body pointer in array", map[string]any{"path": "/x", "body": map[string]any{"/0": "prompt"}}, "named object fields"},
		{"body unsupported tilde", map[string]any{"path": "/x", "body": map[string]any{"/a~2b": "prompt"}}, "unsupported path component"},
		{
			"body parent child", map[string]any{
				"path": "/x", "body": map[string]any{"/a": "model", "/a/b": "prompt"},
			}, "both a field and one of its children",
		},
		{"body value not simple", map[string]any{"path": "/x", "body": map[string]any{"/p": "9bad"}}, "simple input names"},
		{"header not allowlisted", map[string]any{"path": "/x", "headers": map[string]any{"X-Token": "1"}}, "allowlist"},
		{"header line break", map[string]any{"path": "/x", "headers": map[string]any{"accept": "a\nb"}}, "line breaks"},
		{"response bad pointer", map[string]any{"path": "/x", "response": map[string]any{"image_url": "bad"}}, "JSON Pointer"},
		{"response bad key", map[string]any{"path": "/x", "response": map[string]any{"9bad": "/a"}}, "simple field names"},
		{"overrides not object", map[string]any{"path": "/x", "capability_overrides": []any{1}}, "must be an object"},
		{
			"poll unknown field", map[string]any{"path": "/x", "poll": map[string]any{
				"path": "/s", "job_id": "/id", "status": "/st", "done_value": "ok", "bogus": 1,
			}}, "unknown fields",
		},
		{
			"poll missing done value", map[string]any{"path": "/x", "poll": map[string]any{
				"path": "/s", "job_id": "/id", "status": "/st",
			}}, "done_value",
		},
		{
			"poll interval too small", map[string]any{"path": "/x", "poll": map[string]any{
				"path": "/s", "job_id": "/id", "status": "/st", "done_value": "ok", "interval_seconds": 0.001,
			}}, "interval_seconds",
		},
		{
			"poll interval over timeout", map[string]any{"path": "/x", "poll": map[string]any{
				"path": "/s", "job_id": "/id", "status": "/st", "done_value": "ok",
				"interval_seconds": 50, "timeout_seconds": 10,
			}}, "timeout_seconds",
		},
		{
			"poll bad status pointer", map[string]any{"path": "/x", "poll": map[string]any{
				"path": "/s", "job_id": "/id", "status": "st", "done_value": "ok",
			}}, "JSON Pointer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCustomEndpointDeclaration(tc.raw)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestParseCustomEndpointDeclarationRejectsDeepTemplate 断言 body_template 超深被拒。
func TestParseCustomEndpointDeclarationRejectsDeepTemplate(t *testing.T) {
	var deep any = "leaf"
	for i := 0; i < 40; i++ {
		deep = map[string]any{"n": deep}
	}
	_, err := ParseCustomEndpointDeclaration(map[string]any{"path": "/x", "body_template": deep})
	if err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("expected nesting error, got %v", err)
	}
}

// TestParseCustomEndpointDeclarationAcceptsFull 断言合法声明解析后各字段落位正确。
func TestParseCustomEndpointDeclarationAcceptsFull(t *testing.T) {
	declaration := customParseDeclaration(t, map[string]any{
		"method": "post",
		"path":   "/v1/images/{model}",
		"headers": map[string]any{
			"accept": "application/json", "content-type": "application/json",
		},
		"body":                 map[string]any{"/prompt": "prompt", "/aspect_ratio": "aspect_ratio"},
		"response":             map[string]any{"image_url": "/data/0/url"},
		"capability_overrides": map[string]any{"max_reference_images": 2},
		"poll": map[string]any{
			"path":             "/v1/jobs/{job_id}",
			"job_id":           "/task/id",
			"status":           "/task/state",
			"done_value":       "SUCCEEDED",
			"failed_value":     "FAILED",
			"result":           "/output/images/0/url",
			"artifact_url":     "/download?file={result}",
			"interval_seconds": 0.25,
			"timeout_seconds":  30,
		},
	})
	if declaration.Method != "POST" {
		t.Fatalf("method: %q", declaration.Method)
	}
	if declaration.Path != "/v1/images/{model}" {
		t.Fatalf("path: %q", declaration.Path)
	}
	if declaration.Headers["Accept"] != "application/json" || declaration.Headers["Content-Type"] != "application/json" {
		t.Fatalf("headers: %v", declaration.Headers)
	}
	if declaration.BodyMapping["/prompt"] != "prompt" {
		t.Fatalf("body mapping: %v", declaration.BodyMapping)
	}
	if declaration.ResponseMapping["image_url"] != "/data/0/url" {
		t.Fatalf("response mapping: %v", declaration.ResponseMapping)
	}
	if declaration.CapabilityOverrides["max_reference_images"] != 2 {
		t.Fatalf("overrides: %v", declaration.CapabilityOverrides)
	}
	poll := declaration.Poll
	if poll == nil {
		t.Fatalf("poll must be parsed")
	}
	if poll.IntervalSeconds != 0.25 || poll.TimeoutSeconds != 30 {
		t.Fatalf("poll timing: %+v", poll)
	}
	if !poll.HasFailedValue || !poll.HasResult || !poll.HasArtifactURL {
		t.Fatalf("poll optional flags: %+v", poll)
	}
}

// TestRenderCustomEndpointDeclarationMapping 断言 body 映射与 path 占位符渲染。
func TestRenderCustomEndpointDeclarationMapping(t *testing.T) {
	declaration := customParseDeclaration(t, map[string]any{
		"path":    "/v1/images/{model}",
		"headers": map[string]any{"accept": "application/json"},
		"body":    map[string]any{"/prompt": "prompt", "/config/seed": "seed"},
	})
	rendered, err := RenderCustomEndpointDeclaration(declaration, map[string]any{
		"model": "m1", "prompt": "hi", "seed": 7,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if rendered.Path != "/v1/images/m1" {
		t.Fatalf("path: %q", rendered.Path)
	}
	if rendered.Headers["Accept"] != "application/json" {
		t.Fatalf("headers: %v", rendered.Headers)
	}
	if rendered.Body["prompt"] != "hi" {
		t.Fatalf("body prompt: %v", rendered.Body)
	}
	config, ok := rendered.Body["config"].(map[string]any)
	if !ok || config["seed"] != 7 {
		t.Fatalf("body nested: %v", rendered.Body)
	}
}

// TestRenderCustomEndpointDeclarationMissingInput 断言缺输入 fail loud。
func TestRenderCustomEndpointDeclarationMissingInput(t *testing.T) {
	declaration := customParseDeclaration(t, map[string]any{
		"path": "/x",
		"body": map[string]any{"/prompt": "prompt"},
	})
	_, err := RenderCustomEndpointDeclaration(declaration, map[string]any{"model": "m"})
	if err == nil || !strings.Contains(err.Error(), "missing input") {
		t.Fatalf("expected missing input error, got %v", err)
	}
}

// TestRenderCustomEndpointDeclarationRejectsRuntimeHeaders 断言运行期不可注入 headers。
func TestRenderCustomEndpointDeclarationRejectsRuntimeHeaders(t *testing.T) {
	declaration := customParseDeclaration(t, map[string]any{"path": "/x"})
	_, err := RenderCustomEndpointDeclaration(declaration, map[string]any{
		"model": "m", "headers": map[string]any{"accept": "application/json"},
	})
	if err == nil || !strings.Contains(err.Error(), "runtime headers") {
		t.Fatalf("expected runtime headers error, got %v", err)
	}
}

// TestRenderCustomEndpointDeclarationTemplate 断言模板渲染：整串占位取原值、内联拼接取标量。
func TestRenderCustomEndpointDeclarationTemplate(t *testing.T) {
	declaration := customParseDeclaration(t, map[string]any{
		"path": "/gen",
		"body_template": map[string]any{
			"model":  "{model}",
			"prompt": "prompt: {prompt}",
			"images": "{reference_images}",
			"count":  3,
			"flag":   true,
		},
	})
	rendered, err := RenderCustomEndpointDeclaration(declaration, map[string]any{
		"model": "m1", "prompt": "hi", "reference_images": []any{"a.png", "b.png"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body := rendered.Body
	if body["model"] != "m1" || body["prompt"] != "prompt: hi" {
		t.Fatalf("body strings: %v", body)
	}
	if !reflect.DeepEqual(body["images"], []any{"a.png", "b.png"}) {
		t.Fatalf("whole-string placeholder should carry the raw value: %v", body["images"])
	}
	if body["count"] != 3 || body["flag"] != true {
		t.Fatalf("body scalars: %v", body)
	}
}

// TestRenderCustomEndpointDeclarationInlineNonScalar 断言内联插值拒绝非标量。
func TestRenderCustomEndpointDeclarationInlineNonScalar(t *testing.T) {
	declaration := customParseDeclaration(t, map[string]any{
		"path":          "/gen",
		"body_template": map[string]any{"images": "prefix {reference_images}"},
	})
	_, err := RenderCustomEndpointDeclaration(declaration, map[string]any{
		"model": "m", "reference_images": []any{"a"},
	})
	if err == nil || !strings.Contains(err.Error(), "scalar") {
		t.Fatalf("expected scalar error, got %v", err)
	}
}

// TestRenderCustomPollPointer 断言 poll pointer 占位符替换与 token 转义。
func TestRenderCustomPollPointer(t *testing.T) {
	got, err := RenderCustomPollPointer("/jobs/{job_id}", map[string]any{"job_id": "a/b~c"})
	if err != nil {
		t.Fatalf("render poll pointer: %v", err)
	}
	if got != "/jobs/a~1b~0c" {
		t.Fatalf("escaped pointer: %q", got)
	}
	if _, err := RenderCustomPollPointer("/x/{status}", map[string]any{"status": "s"}); err == nil {
		t.Fatalf("expected disallowed placeholder error")
	}
}

// TestRenderCustomEndpointArtifactURL 断言产物 URL 模板渲染与缺值报错。
func TestRenderCustomEndpointArtifactURL(t *testing.T) {
	poll := &CustomEndpointPoll{ArtifactURL: "/dl/{image_id}", HasArtifactURL: true}
	got, ok, err := RenderCustomEndpointArtifactURL(poll, map[string]any{"image_id": "abc 1"})
	if err != nil || !ok || got != "/dl/abc%201" {
		t.Fatalf("artifact url: %q ok=%v err=%v", got, ok, err)
	}
	if _, _, err := RenderCustomEndpointArtifactURL(poll, map[string]any{"other": "x"}); err == nil {
		t.Fatalf("expected missing scalar error")
	}
	if _, ok, err := RenderCustomEndpointArtifactURL(&CustomEndpointPoll{}, nil); err != nil || ok {
		t.Fatalf("absent artifact_url must be a no-op: ok=%v err=%v", ok, err)
	}
}

// TestGetCustomJSONPointer 断言 JSON Pointer 取值：对象、数组下标与 ~0/~1 转义。
func TestGetCustomJSONPointer(t *testing.T) {
	document := map[string]any{
		"data": []any{
			map[string]any{"url": "u1", "m~n": "tilde", "a/b": "slash"},
		},
	}
	cases := []struct {
		pointer string
		want    any
	}{
		{"/data/0/url", "u1"},
		{"/data/0/m~0n", "tilde"},
		{"/data/0/a~1b", "slash"},
	}
	for _, tc := range cases {
		got, err := GetCustomJSONPointer(document, tc.pointer)
		if err != nil || got != tc.want {
			t.Fatalf("pointer %q: got %v err %v", tc.pointer, got, err)
		}
	}
	for _, pointer := range []string{"no-slash", "/nope", "/data/5/url", "/data/url"} {
		if _, err := GetCustomJSONPointer(document, pointer); err == nil {
			t.Fatalf("pointer %q should fail", pointer)
		}
	}
}

// TestNormalizeCustomEndpointResponse 断言按 response 映射抽取字段，缺失即报错。
func TestNormalizeCustomEndpointResponse(t *testing.T) {
	declaration := customParseDeclaration(t, map[string]any{
		"path":     "/x",
		"response": map[string]any{"image_url": "/data/0/url"},
	})
	normalized, err := NormalizeCustomEndpointResponse(declaration, map[string]any{
		"data": []any{map[string]any{"url": "u1"}},
	})
	if err != nil || normalized["image_url"] != "u1" {
		t.Fatalf("normalize: %v err %v", normalized, err)
	}
	if _, err := NormalizeCustomEndpointResponse(declaration, map[string]any{"data": []any{}}); err == nil {
		t.Fatalf("expected missing response field error")
	}
}

// TestCustomEndpointRegistryShape 断言注册表规模、媒体分布与不变式。
func TestCustomEndpointRegistryShape(t *testing.T) {
	byMedia := map[provider.MediaType]int{}
	for _, spec := range customEndpointRegistry {
		byMedia[spec.mediaType]++
	}
	if len(customEndpointRegistry) != 18 {
		t.Fatalf("registry size: %d", len(customEndpointRegistry))
	}
	if byMedia[provider.MediaText] != 2 || byMedia[provider.MediaImage] != 7 ||
		byMedia[provider.MediaVideo] != 8 || byMedia[provider.MediaAudio] != 1 {
		t.Fatalf("media distribution: %v", byMedia)
	}
	spec, err := CustomEndpointSpecFor("openai-chat")
	if err != nil || spec.mediaType != provider.MediaText {
		t.Fatalf("openai-chat spec: %+v err %v", spec, err)
	}
	if spec.buildImage != nil || spec.buildVideo != nil || spec.buildAudio != nil {
		t.Fatalf("text endpoint must not declare a builder")
	}
	if mediaType, err := CustomEndpointMediaType("openai-images"); err != nil || mediaType != provider.MediaImage {
		t.Fatalf("openai-images media type: %q err %v", mediaType, err)
	}
	if _, err := CustomEndpointMediaType("nope"); err == nil {
		t.Fatalf("unknown endpoint must fail loud")
	}
	// init 期已跑过一次，这里再显式确认不变式不 panic。
	customValidateEndpointRegistry()
}
