package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"

	"github.com/shotwise/shotwise/internal/agent/eventlog"
)

// eventAdapter 把一个会话的 adk.AgentEvent 流转换为两个出口
// （设计文档 03-agent-runtime-eino.md §14）：
//   - event_log append（权威 transcript，seq 由 Store 单调分配）
//   - SSE Event（经 emit 回调推给订阅者）
//
// 转换表（§14.1）：
//
//	assistant 流式 chunk                → delta（chunk.Content 直透）+ 累积
//	轮末 assistant 整消息               → assistant entry + 每 ToolCall 一条 tool_use
//	tool 消息事件                       → tool_result entry + tool_result 事件
//	guard deny（CustomizedOutput）      → system entry + tool_result(is_error)
//	Action.Interrupted                  → entry(subtype=question) + question 事件
//	Err = *adk.CancelError              → entry(subtype=interrupt) + status=cancelled
//	Err 其他                            → entry(subtype=agent_turn_failure) + status=error
//	ResponseMeta.Usage                  → usage 事件
type eventAdapter struct {
	sessionID string
	store     eventlog.Store
	emit      func(Event)
	// onInterrupted 在捕获业务中断（ask_user）时回调，把待决问题交给
	// Runtime 登记（恢复时作为 ResumeParams.Targets 的 key）。
	onInterrupted func(*PendingQuestion)
}

// assistantDraft 累积一轮 assistant 流式 chunk，轮末合并成整消息落库。
// Eino 的流式 chunk 语义与 Claude SDK 不同：分片合并交给 schema.ConcatMessages。
type assistantDraft struct {
	chunks []*schema.Message
}

// consume 消费一个 TurnLoop 轮的事件流；返回值即 TurnLoop OnAgentEvents 的返回值。
// 约定：错误（含 CancelError）在此转换为 entry + status 事件后返回 nil——
// 错误已上报给调用方，不应终止 TurnLoop 生命周期（Stop / interrupt 由 Runtime 显式控制）。
func (a *eventAdapter) consume(ctx context.Context, events *adk.AsyncIterator[*adk.AgentEvent]) error {
	draft := &assistantDraft{}
	for {
		ev, ok := events.Next()
		if !ok {
			break
		}
		if ev == nil {
			continue
		}
		// 业务中断（ask_user 等）：登记 interruptID，交由框架落 checkpoint。
		if ev.Action != nil && ev.Action.Interrupted != nil {
			a.handleInterrupted(ctx, ev.Action.Interrupted)
			continue
		}
		if ev.Err != nil {
			a.handleError(ctx, ev.Err)
			continue
		}
		if ev.Output == nil || ev.Output.MessageOutput == nil {
			continue
		}
		mo := ev.Output.MessageOutput
		switch mo.Role {
		case schema.Assistant:
			a.handleAssistantChunk(ctx, draft, mo)
		case schema.Tool:
			// tool 消息意味着上一个 assistant 迭代（发出 tool_calls）已结束：
			// 在此处 flush，避免跨迭代 concat 把 tool_call 消息与最终消息混成一条。
			a.flushAssistantDraft(ctx, draft)
			a.handleToolMessage(ctx, mo)
		default:
			// user / system 消息不由事件流产生（输入侧由 Runtime 落库），忽略。
		}
	}
	// 轮末：合并 assistant 整消息落库。
	a.flushAssistantDraft(ctx, draft)
	return nil
}

// handleAssistantChunk 处理 assistant 消息输出：流式时逐 chunk 直透 delta 并累积；
// 非流式（Generate 路径）时整消息直落 draft。
func (a *eventAdapter) handleAssistantChunk(_ context.Context, draft *assistantDraft, mo *adk.TypedMessageVariant[*schema.Message]) {
	if mo.IsStreaming {
		if mo.MessageStream == nil {
			return
		}
		defer mo.MessageStream.Close()
		for {
			chunk, err := mo.MessageStream.Recv()
			if err != nil {
				break // EOF 或流错误：累积到此为止
			}
			if chunk == nil {
				continue
			}
			draft.chunks = append(draft.chunks, chunk)
			if chunk.Content != "" {
				a.emit(Event{SessionID: a.sessionID, Type: EventDelta, Text: chunk.Content})
			}
			if chunk.ResponseMeta != nil && chunk.ResponseMeta.Usage != nil {
				a.emitUsage(chunk.ResponseMeta.Usage)
			}
		}
		return
	}
	if mo.Message != nil {
		draft.chunks = append(draft.chunks, mo.Message)
	}
}

// flushAssistantDraft 合并当前累积的 assistant chunk 成整消息落库（含 tool_use 事件），
// 并清空草稿以便下一个迭代重新累积。
func (a *eventAdapter) flushAssistantDraft(ctx context.Context, draft *assistantDraft) {
	if len(draft.chunks) == 0 {
		return
	}
	chunks := draft.chunks
	draft.chunks = nil
	msg, err := schema.ConcatMessages(chunks)
	if err != nil || msg == nil {
		return
	}
	entry := &eventlog.Entry{
		UUID:      newEntryUUID(),
		SessionID: a.sessionID,
		Type:      eventlog.EntryAssistant,
		Timestamp: nowUTC(),
		MessageID: messageID(msg),
	}
	if msg.Content != "" {
		entry.Content = append(entry.Content, eventlog.Block{Type: "text", Text: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		entry.Content = append(entry.Content, eventlog.Block{
			Type: "tool_use", ID: tc.ID, Name: tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}
	// 先落库拿 seq，再广播：tool_use 事件带条目 seq，前端可据此续传/去重。
	seq := a.appendOne(ctx, entry)
	for _, tc := range msg.ToolCalls {
		a.emit(Event{SessionID: a.sessionID, Seq: seq, Type: EventToolUse, ToolName: tc.Function.Name,
			Meta: mustJSON(map[string]any{"arguments": tc.Function.Arguments})})
	}
}

// handleToolMessage 处理 Role=tool 的完整消息事件：tool_result entry + 事件。
func (a *eventAdapter) handleToolMessage(ctx context.Context, mo *adk.TypedMessageVariant[*schema.Message]) {
	msg := mo.Message
	if msg == nil && !mo.IsStreaming {
		return
	}
	if msg == nil {
		// 工具回包理论上是整消息；流式兜底消费。
		defer mo.MessageStream.Close()
		for {
			chunk, err := mo.MessageStream.Recv()
			if err != nil {
				break
			}
			msg = concatToolChunk(msg, chunk)
		}
	}
	if msg == nil {
		return
	}
	isErr, text := parseToolResultMessage(msg)
	entry := &eventlog.Entry{
		UUID:      newEntryUUID(),
		SessionID: a.sessionID,
		Type:      eventlog.EntryToolResult,
		Timestamp: nowUTC(),
		MessageID: messageID(msg),
		ToolUseID: msg.ToolCallID,
	}
	entry.Content = append(entry.Content, eventlog.Block{Type: "tool_result", Text: text})
	seq := a.appendOne(ctx, entry)
	a.emit(Event{SessionID: a.sessionID, Seq: seq, Type: EventToolResult, ToolName: mo.ToolName,
		IsError: isErr, Text: text})
}

func concatToolChunk(acc, chunk *schema.Message) *schema.Message {
	if chunk == nil {
		return acc
	}
	if acc == nil {
		return chunk
	}
	acc.Content += chunk.Content
	return acc
}

// handleInterrupted 处理业务中断：question entry + question 事件 + 待决问题登记。
func (a *eventAdapter) handleInterrupted(ctx context.Context, info *adk.InterruptInfo) {
	var interruptID string
	var questions []AskQuestion
	for _, ic := range info.InterruptContexts {
		if ic == nil {
			continue
		}
		if interruptID == "" {
			interruptID = ic.ID
		}
		// ask_user 挂起时 Info 即 AskUserInput；类型不符时只登记 ID，
		// 问题结构退回事件日志里的原始 JSON。
		if in, ok := ic.Info.(AskUserInput); ok {
			questions = append(questions, in.Questions...)
		}
	}
	entry := &eventlog.Entry{
		UUID:      newEntryUUID(),
		SessionID: a.sessionID,
		Type:      eventlog.EntrySystem,
		Subtype:   eventlog.SubtypeQuestion,
		Timestamp: nowUTC(),
	}
	for _, ic := range info.InterruptContexts {
		if ic != nil {
			entry.Content = append(entry.Content, eventlog.Block{Type: "text", Text: interruptPayloadText(ic.Info)})
		}
	}
	seq := a.appendOne(ctx, entry)
	if interruptID != "" && a.onInterrupted != nil {
		a.onInterrupted(&PendingQuestion{
			SessionID: a.sessionID,
			ID:        interruptID,
			Seq:       seq,
			Questions: questions,
		})
	}
	a.emit(Event{SessionID: a.sessionID, Seq: seq, Type: EventQuestion,
		Raw: mustJSON(questionPayload{SessionID: a.sessionID, QuestionID: interruptID, Questions: questions})})
}

// questionPayload 是 question 事件的 data 载荷（与 Python 侧 ask_user_question 事件同构）。
type questionPayload struct {
	SessionID  string        `json:"session_id"`
	QuestionID string        `json:"question_id"`
	Questions  []AskQuestion `json:"questions"`
}

// handleError 处理事件流错误：CancelError → interrupt / cancelled；其他 → 失败。
func (a *eventAdapter) handleError(ctx context.Context, err error) {
	var cancelErr *adk.CancelError
	if errors.As(err, &cancelErr) {
		a.appendEntries(ctx, &eventlog.Entry{
			UUID:      newEntryUUID(),
			SessionID: a.sessionID,
			Type:      eventlog.EntrySystem,
			Subtype:   eventlog.SubtypeInterrupt,
			Content:   []eventlog.Block{{Type: "text", Text: "turn interrupted"}},
			Timestamp: nowUTC(),
		})
		a.emit(Event{SessionID: a.sessionID, Type: EventStatus, Text: "cancelled"})
		return
	}
	a.appendEntries(ctx, &eventlog.Entry{
		UUID:      newEntryUUID(),
		SessionID: a.sessionID,
		Type:      eventlog.EntrySystem,
		Subtype:   eventlog.SubtypeAgentTurnFailure,
		Content:   []eventlog.Block{{Type: "text", Text: err.Error()}},
		Timestamp: nowUTC(),
	})
	a.emit(Event{SessionID: a.sessionID, Type: EventStatus, Text: "error",
		Meta: mustJSON(map[string]any{"message": err.Error()})})
}

// appendUserEntries 落用户消息 entry（输入侧，由 Runtime 在 Push 时调用）。
func (a *eventAdapter) appendUserEntries(ctx context.Context, text string) {
	a.appendEntries(ctx, &eventlog.Entry{
		UUID:      newEntryUUID(),
		SessionID: a.sessionID,
		Type:      eventlog.EntryUser,
		Content:   []eventlog.Block{{Type: "text", Text: text}},
		Timestamp: nowUTC(),
	})
}

// appendSystemNotice 落系统通知 entry（task_notification 注入、question_answer 等）。
func (a *eventAdapter) appendSystemNotice(ctx context.Context, subtype eventlog.EntrySubtype, text string) {
	a.appendEntries(ctx, &eventlog.Entry{
		UUID:      newEntryUUID(),
		SessionID: a.sessionID,
		Type:      eventlog.EntrySystem,
		Subtype:   subtype,
		Content:   []eventlog.Block{{Type: "text", Text: text}},
		Timestamp: nowUTC(),
	})
}

// appendEntries 批量追加条目（忽略写失败：事件日志不可用时不阻断运行）。
func (a *eventAdapter) appendEntries(ctx context.Context, entries ...*eventlog.Entry) {
	if len(entries) == 0 {
		return
	}
	_, _ = a.store.Append(ctx, entries)
}

// appendOne 追加条目并返回第一条的 seq（落库失败时 seq 为 0，事件不带 id）。
func (a *eventAdapter) appendOne(ctx context.Context, entry *eventlog.Entry) int64 {
	seqs, err := a.store.Append(ctx, []*eventlog.Entry{entry})
	if err != nil || len(seqs) == 0 {
		// 事件日志写失败不阻断运行；SSE 出口仍在。
		return 0
	}
	return seqs[0]
}

func (a *eventAdapter) emitUsage(u *schema.TokenUsage) {
	a.emit(Event{SessionID: a.sessionID, Type: EventUsage,
		Meta: mustJSON(map[string]any{
			"prompt_tokens":     u.PromptTokens,
			"completion_tokens": u.CompletionTokens,
			"total_tokens":      u.TotalTokens,
		})})
}

// parseToolResultMessage 解析工具回包里的 is_error 信封与文本（与 Python 侧约定一致）。
func parseToolResultMessage(msg *schema.Message) (isErr bool, text string) {
	if msg == nil {
		return false, ""
	}
	var probe struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"is_error"`
	}
	if err := json.Unmarshal([]byte(msg.Content), &probe); err == nil {
		var sb strings.Builder
		for _, c := range probe.Content {
			sb.WriteString(c.Text)
		}
		return probe.IsError, sb.String()
	}
	return false, msg.Content
}

// interruptPayloadText 把 ask_user 的中断信息转成可读文本入 entry。
func interruptPayloadText(info any) string {
	b, err := json.Marshal(info)
	if err != nil {
		return ""
	}
	return string(b)
}

// --- helpers ---

func newEntryUUID() string { return uuid.NewString() }

func nowUTC() time.Time { return time.Now().UTC() }

// messageID 从 Extra 里取模型侧消息 id（Eino Message 无独立 ID 字段，openai 实现会写入 Extra["id"]）。
func messageID(msg *schema.Message) string {
	if msg == nil || msg.Extra == nil {
		return ""
	}
	id, _ := msg.Extra["id"].(string)
	return id
}
