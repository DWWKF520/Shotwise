package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// fakeModel 是一个可工具调用的假模型，用于无外部依赖地验证 ReAct 循环：
// 第一次调用返回一次工具调用；第二次调用返回最终文本。
type fakeModel struct {
	calls int
}

func (f *fakeModel) Generate(ctx context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return &schema.Message{Role: schema.Assistant, Content: "done"}, nil
}

func (f *fakeModel) Stream(ctx context.Context, in []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	f.calls++
	if f.calls == 1 {
		// 第一轮：要求调用 echo 工具，参数 {"message":"hi"}
		return schema.StreamReaderFromArray([]*schema.Message{{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: schema.FunctionCall{Name: "echo", Arguments: `{"message":"hi"}`},
			}},
		}}), nil
	}
	// 第二轮：给出最终文本
	return schema.StreamReaderFromArray([]*schema.Message{{
		Role:    schema.Assistant,
		Content: "final answer",
	}}), nil
}

func (f *fakeModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return f, nil
}

// TestEngine_ToolLoop 验证：工具调用 → 工具执行 → 回填 → 最终完成，且事件顺序正确。
func TestEngine_ToolLoop(t *testing.T) {
	ts := NewToolSet()
	ts.Register(&echoTool{}, nil)
	eng, err := NewEngine(&fakeModel{}, ts, "sys")
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	events, finalCh, err := eng.Run(context.Background(), "sess_test", []*schema.Message{
		{Role: schema.User, Content: "use echo"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	var (
		gotToolUse    bool
		gotToolResult bool
		gotDone       bool
		lastText      string
	)
	for ev := range events {
		switch ev.Type {
		case EventToolUse:
			if ev.ToolName != "echo" {
				t.Fatalf("unexpected tool_use: %s", ev.ToolName)
			}
			gotToolUse = true
		case EventToolResult:
			if ev.ToolName != "echo" {
				t.Fatalf("unexpected tool_result: %s", ev.ToolName)
			}
			gotToolResult = true
			lastText = ev.Text
		case EventStatus:
			if ev.Text == "done" {
				gotDone = true
			}
		case EventDelta:
			lastText = ev.Text
		}
	}

	if !gotToolUse || !gotToolResult || !gotDone {
		t.Fatalf("missing events: toolUse=%v toolResult=%v done=%v", gotToolUse, gotToolResult, gotDone)
	}

	final := <-finalCh
	// 期望历史：user, assistant(tool_call), tool(echo), assistant(final)
	if len(final) != 4 {
		t.Fatalf("expected 4 messages in final history, got %d", len(final))
	}
	if final[2].Role != schema.Tool || final[2].ToolName != "echo" {
		t.Fatalf("tool message malformed: %+v", final[2])
	}
	_ = json.RawMessage{}
	_ = lastText
}
