package task

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEncodeFailurePythonParity(t *testing.T) {
	got, err := EncodeFailure("video_duration_invalid", map[string]any{"duration": 8})
	if err != nil {
		t.Fatal(err)
	}
	// Python: json.dumps({"duration": 8}, ensure_ascii=False, sort_keys=True) → '{"duration": 8}'
	if got != `[video_duration_invalid] {"duration": 8}` {
		t.Fatalf("encoded = %q", got)
	}
	got, err = EncodeFailure("video_duration_invalid", map[string]any{"reason": "中文", "n": 1.5})
	if err != nil {
		t.Fatal(err)
	}
	if got != `[video_duration_invalid] {"n": 1.5, "reason": "中文"}` {
		t.Fatalf("encoded = %q", got)
	}
	got, err = EncodeFailure("restart_lost_image", nil)
	if err != nil || got != "[restart_lost_image]" {
		t.Fatalf("no-params = %q, %v", got, err)
	}
	if _, err := EncodeFailure("no_such_code", nil); err == nil {
		t.Fatal("unknown code must fail fast")
	}
}

func TestEncodeFailureStringifiesNonNativeValues(t *testing.T) {
	// 对应 Python default=str：容器值降级为文本，不得 panic 或静默丢参。
	got, err := EncodeFailure("video_duration_invalid", map[string]any{"duration": []any{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if got != `[video_duration_invalid] {"duration": [1, 2]}` {
		t.Fatalf("encoded = %q", got)
	}
	if !strings.Contains(mustParseCode(t, got), "video_duration_invalid") {
		t.Fatal("round trip failed")
	}
}

func mustParseCode(t *testing.T, msg string) string {
	t.Helper()
	code, _, ok := ParseStructuredFailure(msg)
	if !ok {
		t.Fatalf("not parseable: %q", msg)
	}
	return code
}

func TestParseStructuredFailureRejectsLegacyText(t *testing.T) {
	for _, msg := range []string{
		"plain provider error",
		"[restart_lost] 中文尾巴", // 老格式：非 JSON 尾巴
		"[unknown_code] {\"a\": 1}",
		"[video_duration_invalid] not-json",
	} {
		if _, _, ok := ParseStructuredFailure(msg); ok {
			t.Fatalf("must not parse: %q", msg)
		}
	}
}

func TestSanitizeFailureReasonRedactsCredentialsAndPaths(t *testing.T) {
	out := SanitizeFailureReason("auth failed: Authorization: Bearer sk-abc123 at /home/user/secret.txt")
	if strings.Contains(out, "sk-abc123") || strings.Contains(out, "/home/user/secret.txt") {
		t.Fatalf("leak: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") || !strings.Contains(out, "[REDACTED_PATH]") {
		t.Fatalf("expected redaction markers: %q", out)
	}

	// 结构化信封保留机器码，参数就地清洗。
	out = SanitizeFailureReason(`[video_duration_invalid] {"api_key": "sk-x", "detail": "see C:\\Users\\me\\log.txt"}`)
	code, params, ok := ParseStructuredFailure(out)
	if !ok || code != "video_duration_invalid" {
		t.Fatalf("envelope lost: %q", out)
	}
	if params["api_key"] != "[REDACTED]" {
		t.Fatalf("api_key param = %v", params["api_key"])
	}
	if strings.Contains(params["detail"].(string), `C:\Users`) {
		t.Fatalf("path leak: %v", params["detail"])
	}
}

func TestCollapseCascadeReason(t *testing.T) {
	inner, _ := EncodeFailure("video_duration_invalid", map[string]any{"duration": 99})
	mid, _ := EncodeFailure(CascadeFailureCode, map[string]any{"dependency_task_id": "t2", "reason": inner})
	outer, _ := EncodeFailure(CascadeFailureCode, map[string]any{"dependency_task_id": "t1", "reason": mid})
	collapsed := CollapseCascadeReason(outer)
	if collapsed != inner {
		t.Fatalf("collapsed = %q, want root %q", collapsed, inner)
	}
}

func TestBoundReasonKeepsEnvelopeParseable(t *testing.T) {
	huge := strings.Repeat("长", 3000)
	reason, _ := EncodeFailure("resume_expired_detail", map[string]any{"detail": huge})
	bounded := BoundReason(reason, MaxErrorMessageLen)
	if utf8.RuneCountInString(bounded) > MaxErrorMessageLen {
		t.Fatalf("bounded len = %d", utf8.RuneCountInString(bounded))
	}
	code, params, ok := ParseStructuredFailure(bounded)
	if !ok || code != "resume_expired_detail" {
		t.Fatalf("bounded envelope unparseable: %q", bounded)
	}
	if _, ok := params["detail"].(string); !ok {
		t.Fatalf("detail param lost: %v", params)
	}

	// 非结构化文本退回按字符裁剪。
	if got := BoundReason(strings.Repeat("a", 500), 100); utf8.RuneCountInString(got) != 100 {
		t.Fatalf("plain truncate = %d", utf8.RuneCountInString(got))
	}
	// 未超限原样返回。
	short := `[video_duration_invalid] {"duration": 8}`
	if got := BoundReason(short, MaxErrorMessageLen); got != short {
		t.Fatalf("short reason changed: %q", got)
	}
}

func TestRenderFailureLocalizesAndExpandsCascade(t *testing.T) {
	translate := func(key string, params map[string]any) string {
		if key == "task_fail_cascade_blocked_dependency" {
			return "blocked: " + params["reason"].(string)
		}
		return "rendered:" + key
	}
	leaf, _ := EncodeFailure("video_duration_invalid", nil)
	outer, _ := EncodeFailure(CascadeFailureCode, map[string]any{"dependency_task_id": "t1", "reason": leaf})
	got := RenderFailure(&outer, translate)
	if got == nil || *got != "blocked: rendered:video_duration_invalid" {
		t.Fatalf("render = %v", got)
	}
	legacy := "raw provider text"
	if out := RenderFailure(&legacy, translate); out == nil || *out != legacy {
		t.Fatalf("legacy passthrough = %v", out)
	}
}

func TestBuildEnqueuePromptPreviewRedactsAndExtracts(t *testing.T) {
	scriptFile := "ep1.json"
	providerID := "gemini"
	payload := map[string]any{
		"prompt": map[string]any{
			"action":   "walk",
			"api_key":  "sk-leak",
			"password": "hunter2",
			"website":  "https://ex.com/cb?token=abc&x=1#frag",
		},
		"video_provider_i2v": "gemini/gemini-2.5-pro",
		"duration_seconds":   8,
		"references": []any{
			map[string]any{"kind": "image", "value": "https://ex.com/a.png?key=zzz"},
			"https://ex.com/b.png",
			"",
		},
		"warnings": []any{"bearer sk-999 token in note", 123},
	}
	preview := BuildEnqueuePromptPreview("p1", "video", "video", "s1", &scriptFile, &providerID, payload)
	requests, ok := preview["requests"].([]any)
	if !ok || len(requests) != 1 {
		t.Fatalf("preview shape: %#v", preview)
	}
	req := requests[0].(map[string]any)
	if req["provider"] != "gemini" || req["model"] != "gemini-2.5-pro" {
		t.Fatalf("provider/model = %v / %v", req["provider"], req["model"])
	}
	if req["duration_seconds"] != 8 {
		t.Fatalf("duration = %v", req["duration_seconds"])
	}
	promptText, _ := req["original_prompt"].(string)
	if strings.Contains(promptText, "sk-leak") || strings.Contains(promptText, "hunter2") {
		t.Fatalf("credential leak in prompt preview: %q", promptText)
	}
	if !strings.Contains(promptText, "[REDACTED]") {
		t.Fatalf("expected redaction marker: %q", promptText)
	}
	refs := req["references"].([]any)
	if len(refs) != 2 {
		t.Fatalf("references = %d, want 2 (empty string dropped)", len(refs))
	}
	first := refs[0].(map[string]any)
	if first["kind"] != "image" || !strings.Contains(first["value"].(string), "%5BREDACTED%5D") {
		t.Fatalf("reference redaction = %#v", first)
	}
	if first["label"] != first["value"] {
		t.Fatalf("label should fall back to value: %#v", first)
	}
	warnings := req["warnings"].([]any)
	if len(warnings) != 1 || strings.Contains(warnings[0].(string), "sk-999") {
		t.Fatalf("warnings = %#v", warnings)
	}
	summary := req["request_summary"].(map[string]any)
	if summary["script_file"] != "ep1.json" || summary["project_name"] != "p1" {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestPromptPreviewSensitiveKeyCamelCase(t *testing.T) {
	if !isSensitiveKey("apiKey") || !isSensitiveKey("Access-Token") || !isSensitiveKey("clientSecret") {
		t.Fatal("camel/kebab sensitive keys must be detected")
	}
	if isSensitiveKey("prompt") || isSensitiveKey("action") {
		t.Fatal("benign keys must not be redacted")
	}
}

func TestPyJSONIndentMatchesPythonShape(t *testing.T) {
	got := PyJSONIndent(map[string]any{"b": []any{1, 2}, "a": map[string]any{}}, 2)
	want := "{\n  \"a\": {},\n  \"b\": [\n    1,\n    2\n  ]\n}"
	if got != want {
		t.Fatalf("indent json = %q, want %q", got, want)
	}
}
