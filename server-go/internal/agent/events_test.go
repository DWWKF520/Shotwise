package agent

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestEvent_EncodeSSE_Frame(t *testing.T) {
	ev := Event{Seq: 7, SessionID: "s1", Type: EventEntry, Raw: []byte(`{"seq":7,"type":"user"}`)}
	got := ev.EncodeSSE()
	want := "event: entry\nid: 7\ndata: {\"seq\":7,\"type\":\"user\"}\n\n"
	if got != want {
		t.Fatalf("EncodeSSE() = %q, want %q", got, want)
	}
}

// 无条目事件（delta 等）不带 id 字段，避免污染 Last-Event-ID 游标。
func TestEvent_EncodeSSE_NoIDWithoutSeq(t *testing.T) {
	got := Event{Seq: 0, SessionID: "s1", Type: EventDelta, Text: "hi"}.EncodeSSE()
	if strings.Contains(got, "\nid:") {
		t.Fatalf("frame carries id for seq-less event: %q", got)
	}
	if !strings.HasPrefix(got, "event: delta\n") {
		t.Fatalf("frame missing event name: %q", got)
	}
	if !strings.Contains(got, `"text":"hi"`) {
		t.Fatalf("frame missing payload: %q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("frame not terminated by blank line: %q", got)
	}
}

// 字符串工具的信封回包：is_error 与正文按信封约定透出。
func TestParseToolResultMessage_Envelope(t *testing.T) {
	msg := schema.ToolMessage(okEnvelope("完成"), "call-1")
	if isErr, text := parseToolResultMessage(msg); isErr || text != "完成" {
		t.Fatalf("isErr=%v text=%q", isErr, text)
	}
	msg = schema.ToolMessage(denyEnvelope("被围栏拒绝"), "call-2")
	if isErr, text := parseToolResultMessage(msg); !isErr || text != "被围栏拒绝" {
		t.Fatalf("isErr=%v text=%q", isErr, text)
	}
}

// 多模态工具的回包落在 UserInputMultiContent：取文本部件、丢弃图片部件（base64 不入库也不进 SSE）。
func TestParseToolResultMessage_MultimodalDropsImageParts(t *testing.T) {
	base64Frame := strings.Repeat("A", 64)
	msg := schema.ToolMessage("", "call-3")
	msg.UserInputMultiContent = []schema.MessageInputPart{
		{Type: schema.ChatMessagePartTypeText, Text: okEnvelope("审阅完成，发现 1 个问题")},
		{Type: schema.ChatMessagePartTypeText, Text: `{"summary":{"units":3}}`},
		{Type: schema.ChatMessagePartTypeText, Text: "episode_1.json / U1 / frame 0"},
		{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
			MessagePartCommon: schema.MessagePartCommon{Base64Data: &base64Frame, MIMEType: "image/jpeg"},
		}},
	}
	gotErr, text := parseToolResultMessage(msg)
	if gotErr {
		t.Fatalf("信封 is_error=false，却被判为错误: %q", text)
	}
	for _, want := range []string{"审阅完成，发现 1 个问题", `{"summary":{"units":3}}`, "U1 / frame 0"} {
		if !strings.Contains(text, want) {
			t.Errorf("文本部件缺失 %q: %s", want, text)
		}
	}
	if strings.Contains(text, base64Frame) {
		t.Error("图片 base64 不得进入 tool_result entry")
	}

	// 信封 is_error=true 时同样只取文本部件。
	msg.UserInputMultiContent[0] = schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: denyEnvelope("抽帧失败")}
	if gotErr, text = parseToolResultMessage(msg); !gotErr || !strings.Contains(text, "抽帧失败") {
		t.Fatalf("isErr=%v text=%q", gotErr, text)
	}
}

// 非信封的任意 JSON 回包不得被误吞成信封（否则正文会丢空）。
func TestParseToolResultMessage_KeepsPlainJSON(t *testing.T) {
	msg := schema.ToolMessage(`{"summary":{"units":3}}`, "call-4")
	if isErr, text := parseToolResultMessage(msg); isErr || text != `{"summary":{"units":3}}` {
		t.Fatalf("isErr=%v text=%q", isErr, text)
	}
}
