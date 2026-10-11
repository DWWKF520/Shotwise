package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// maxIterations 限制单轮对话里模型生成循环的上限，防止工具递归失控。
const maxIterations = 20

// Engine 是自建的透明 ReAct 循环：model → 流式输出 → 工具调用 → 回填 → 再生成。
// 这正是 eino adk.ChatModelAgent 内部所做的，抽出来是为了在工具调用前后
// 插入 GuardedTool 的权限/hook 裁决（文档 4.1 节 ★核心）。
type Engine struct {
	model  model.ToolCallingChatModel
	tools  *ToolSet
	system string
}

// NewEngine 构造引擎；会一次性把工具 schema 绑到模型上。
func NewEngine(m model.ToolCallingChatModel, tools *ToolSet, systemPrompt string) (*Engine, error) {
	if len(tools.Infos()) > 0 {
		bound, err := m.WithTools(tools.Infos())
		if err != nil {
			return nil, err
		}
		m = bound
	}
	return &Engine{model: m, tools: tools, system: systemPrompt}, nil
}

// Run 执行一次完整对话轮，把事件写入返回的 channel，调用方负责按 SSE 消费。
// 同一 session 的多轮历史由调用方通过 messages 参数注入。
// finalCh 在对话结束时投递完整消息列表（含 assistant 与 tool 消息），供会话持久化。
func (e *Engine) Run(ctx context.Context, sessionID string, messages []*schema.Message) (<-chan Event, <-chan []*schema.Message, error) {
	ch := make(chan Event, 64)
	finalCh := make(chan []*schema.Message, 1)
	seq := int64(0)
	nextSeq := func() int64 { seq++; return seq }

	go func() {
		defer close(ch)
		defer func() { finalCh <- messages }()
		var lastErr error
		for iter := 0; iter < maxIterations; iter++ {
			select {
			case <-ctx.Done():
				ch <- Event{Seq: nextSeq(), SessionID: sessionID, Type: EventStatus, Text: "aborted"}
				return
			default:
			}

			assistant, tcDeltas, err := e.streamOnce(ctx, sessionID, nextSeq, ch, messages)
			if err != nil {
				lastErr = err
				ch <- Event{Seq: nextSeq(), SessionID: sessionID, Type: EventStatus, Text: "error", Meta: errMeta(err)}
				return
			}
			messages = append(messages, assistant)

			if len(assistant.ToolCalls) == 0 {
				ch <- Event{Seq: nextSeq(), SessionID: sessionID, Type: EventStatus, Text: "done"}
				return
			}

			// 执行本轮所有工具调用，回填 tool 消息。
			for _, tc := range assistant.ToolCalls {
				ev := Event{Seq: nextSeq(), SessionID: sessionID, Type: EventToolUse, ToolName: tc.Function.Name,
					Meta: mustJSON(map[string]any{"arguments": tc.Function.Arguments})}
				ch <- ev

				res, runErr := e.tools.Run(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
				isErr := runErr != nil
				if isErr {
					res = denyResult(runErr.Error())
				}
				// 解析工具回包里的 is_error 标记（与 Python 侧约定一致）。
				if parsed := parseToolError(res); parsed {
					isErr = true
				}
				text := extractToolText(res)
				ch <- Event{Seq: nextSeq(), SessionID: sessionID, Type: EventToolResult, ToolName: tc.Function.Name, IsError: isErr, Text: text}
				messages = append(messages, &schema.Message{
					Role:       schema.Tool,
					ToolCallID: tc.ID,
					ToolName:   tc.Function.Name,
					Content:    string(res),
				})
				_ = tcDeltas
			}
		}
		if lastErr == nil {
			ch <- Event{Seq: nextSeq(), SessionID: sessionID, Type: EventStatus, Text: "done"}
		}
	}()

	return ch, finalCh, nil
}

// streamOnce 调用模型流式生成，并实时把文本增量作为 delta 事件推出。
// 返回组装完整的 assistant 消息（含合并后的 tool_calls）。
func (e *Engine) streamOnce(ctx context.Context, sessionID string, nextSeq func() int64, ch chan<- Event, messages []*schema.Message) (*schema.Message, []toolCallDelta, error) {
	sr, err := e.model.Stream(ctx, messages)
	if err != nil {
		return nil, nil, err
	}
	defer sr.Close()

	acc := &schema.Message{Role: schema.Assistant}
	var deltas []toolCallDelta
	for {
		chunk, err := sr.Recv()
		if err != nil {
			if isEOF(err) {
				break
			}
			return nil, nil, err
		}
		if chunk == nil {
			continue
		}
		if chunk.Content != "" {
			acc.Content += chunk.Content
			ch <- Event{Seq: nextSeq(), SessionID: sessionID, Type: EventDelta, Text: chunk.Content}
		}
		for _, tc := range chunk.ToolCalls {
			mergeToolCall(&deltas, tc)
		}
		if chunk.ResponseMeta != nil && chunk.ResponseMeta.Usage != nil {
			u := chunk.ResponseMeta.Usage
			ch <- Event{Seq: nextSeq(), SessionID: sessionID, Type: EventUsage,
				Meta: mustJSON(map[string]any{"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens, "total_tokens": u.TotalTokens})}
		}
	}
	acc.ToolCalls = finalizeToolCalls(deltas)
	return acc, deltas, nil
}

// toolCallDelta 用于合并流式工具调用分片（OpenAI 按 index 追加）。
type toolCallDelta struct {
	index    *int
	id       string
	typ      string
	name     string
	args     strings.Builder
}

func mergeToolCall(dst *[]toolCallDelta, tc schema.ToolCall) {
	idx := 0
	if tc.Index != nil {
		idx = *tc.Index
	}
	for len(*dst) <= idx {
		*dst = append(*dst, toolCallDelta{})
	}
	d := &(*dst)[idx]
	d.index = tc.Index
	if tc.ID != "" {
		d.id = tc.ID
	}
	if tc.Type != "" {
		d.typ = tc.Type
	}
	if tc.Function.Name != "" {
		d.name = tc.Function.Name
	}
	if tc.Function.Arguments != "" {
		d.args.WriteString(tc.Function.Arguments)
	}
}

func finalizeToolCalls(deltas []toolCallDelta) []schema.ToolCall {
	out := make([]schema.ToolCall, 0, len(deltas))
	for _, d := range deltas {
		if d.name == "" && d.id == "" && d.args.Len() == 0 {
			continue
		}
		out = append(out, schema.ToolCall{
			ID:   d.id,
			Type: d.typ,
			Function: schema.FunctionCall{
				Name:      d.name,
				Arguments: d.args.String(),
			},
		})
	}
	return out
}

func isEOF(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "stream closed"))
}

func errMeta(err error) json.RawMessage { return mustJSON(map[string]any{"message": err.Error()}) }

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func parseToolError(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var probe struct {
		IsError bool `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return probe.IsError
}

func extractToolText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var probe struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return string(raw)
	}
	var sb strings.Builder
	for _, c := range probe.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}
