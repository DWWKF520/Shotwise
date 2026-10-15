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
//	assistant 流式 chunk                → delta（chunk.Content 直透）+ 本条消息合并后落库
//	assistant 整消息                    → assistant entry + 每 ToolCall 一条 tool_use
//	tool 消息事件                       → tool_result entry + tool_result 事件
//	skill tool 的 tool_result           → 额外一条 system/skill_invocation entry（§17.2）
//	subagent 内部事件                   → entry 带 parent_tool_use_id（§17.4）
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
	// skillTool 是 skill 中间件的工具名：其 tool_result 额外定型为 skill_invocation 条目。
	skillTool string
	// subagents 是 subagent（AgentTool）名字集合：其内部事件归属到对应的 AgentTool 调用。
	subagents map[string]bool
	// pendingCalls 是 subagent 名 → 已发出、尚未返回的 AgentTool 调用 tool_use id（FIFO）。
	// 子 agent 的事件先于父 tool_result 到达，据此解析 parent_tool_use_id。
	pendingCalls map[string][]string
}

// consume 消费一个 TurnLoop 轮的事件流；返回值即 TurnLoop OnAgentEvents 的返回值。
// 约定：错误（含 CancelError）在此转换为 entry + status 事件后返回 nil——
// 错误已上报给调用方，不应终止 TurnLoop 生命周期（Stop / interrupt 由 Runtime 显式控制）。
func (a *eventAdapter) consume(ctx context.Context, events *adk.AsyncIterator[*adk.AgentEvent]) error {
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
		parent := a.parentFor(ev.AgentName)
		switch mo.Role {
		case schema.Assistant:
			a.handleAssistantMessage(ctx, mo, parent)
		case schema.Tool:
			a.handleToolMessage(ctx, mo, parent)
		default:
			// user / system 消息不由事件流产生（输入侧由 Runtime 落库），忽略。
		}
	}
	return nil
}

// parentFor 返回子 agent 事件所归属的 AgentTool 调用 id；主 agent 事件返回空。
func (a *eventAdapter) parentFor(agentName string) string {
	if !a.subagents[agentName] {
		return ""
	}
	if ids := a.pendingCalls[agentName]; len(ids) > 0 {
		return ids[0]
	}
	return ""
}

// handleAssistantMessage 处理一条 assistant 消息事件：流式时逐 chunk 直透 delta，
// 读尽后把本条消息合并为整消息落库。每个事件即一条完整消息，不跨事件合并。
func (a *eventAdapter) handleAssistantMessage(ctx context.Context, mo *adk.TypedMessageVariant[*schema.Message], parent string) {
	var chunks []*schema.Message
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
			chunks = append(chunks, chunk)
			if chunk.Content != "" {
				a.emit(Event{SessionID: a.sessionID, Type: EventDelta, Text: chunk.Content})
			}
			if chunk.ResponseMeta != nil && chunk.ResponseMeta.Usage != nil {
				a.emitUsage(chunk.ResponseMeta.Usage)
			}
		}
	} else if mo.Message != nil {
		chunks = append(chunks, mo.Message)
	}
	a.flushAssistant(ctx, chunks, parent)
}

// flushAssistant 合并一条消息的 chunk 成整消息落库（含 tool_use 事件）。
func (a *eventAdapter) flushAssistant(ctx context.Context, chunks []*schema.Message, parent string) {
	if len(chunks) == 0 {
		return
	}
	msg, err := schema.ConcatMessages(chunks)
	if err != nil || msg == nil {
		return
	}
	entry := &eventlog.Entry{
		UUID:            newEntryUUID(),
		SessionID:       a.sessionID,
		Type:            eventlog.EntryAssistant,
		Timestamp:       nowUTC(),
		MessageID:       messageID(msg),
		ParentToolUseID: parent,
	}
	if msg.Content != "" {
		entry.Content = append(entry.Content, eventlog.Block{Type: "text", Text: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		entry.Content = append(entry.Content, eventlog.Block{
			Type: "tool_use", ID: tc.ID, Name: tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
		// 登记 AgentTool 调用：其子 agent 事件据此归属到该 tool_use id。
		if a.subagents[tc.Function.Name] {
			a.pendingCalls[tc.Function.Name] = append(a.pendingCalls[tc.Function.Name], tc.ID)
		}
	}
	// 先落库拿 seq，再广播：tool_use 事件带条目 seq，前端可据此续传/去重。
	seq := a.appendOne(ctx, entry)
	for _, tc := range msg.ToolCalls {
		a.emit(Event{SessionID: a.sessionID, Seq: seq, Type: EventToolUse, ToolName: tc.Function.Name,
			Meta: mustJSON(map[string]any{"arguments": tc.Function.Arguments})})
	}
}

// handleToolMessage 处理 Role=tool 的完整消息事件：tool_result entry + 事件。
func (a *eventAdapter) handleToolMessage(ctx context.Context, mo *adk.TypedMessageVariant[*schema.Message], parent string) {
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
	// AgentTool 调用返回：出队对应的 pending 调用。
	if a.subagents[mo.ToolName] {
		if ids := a.pendingCalls[mo.ToolName]; len(ids) > 0 {
			a.pendingCalls[mo.ToolName] = ids[1:]
		}
	}
	isErr, text := parseToolResultMessage(msg)
	entry := &eventlog.Entry{
		UUID:            newEntryUUID(),
		SessionID:       a.sessionID,
		Type:            eventlog.EntryToolResult,
		Timestamp:       nowUTC(),
		MessageID:       messageID(msg),
		ToolUseID:       msg.ToolCallID,
		ParentToolUseID: parent,
	}
	entry.Content = append(entry.Content, eventlog.Block{Type: "tool_result", Text: text})
	seq := a.appendOne(ctx, entry)
	a.emit(Event{SessionID: a.sessionID, Seq: seq, Type: EventToolResult, ToolName: mo.ToolName,
		IsError: isErr, Text: text})

	if a.skillTool != "" && mo.ToolName == a.skillTool {
		if name := parseSkillName(text); name != "" {
			a.appendEntries(ctx, &eventlog.Entry{
				UUID:            newEntryUUID(),
				SessionID:       a.sessionID,
				Type:            eventlog.EntrySystem,
				Subtype:         eventlog.SubtypeSkillInvocation,
				Content:         []eventlog.Block{{Type: "text", Text: name}},
				Timestamp:       nowUTC(),
				ParentToolUseID: parent,
				ToolUseID:       msg.ToolCallID,
			})
		}
	}
}

// parseSkillName 从 skill 注入正文首行的基目录路径提取 skill 名（与 Python _parse_skill_name_from_injection 同语义）。
// 注入正文全文不进日志，只记名字。
func parseSkillName(text string) string {
	const prefix = "Base directory for this skill:"
	firstLine := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	if !strings.HasPrefix(firstLine, prefix) {
		return ""
	}
	path := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(firstLine[len(prefix):]), "\\", "/"), "/")
	var segments []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}
	if n := len(segments); n > 0 && strings.EqualFold(segments[n-1], "SKILL.md") {
		segments = segments[:n-1]
	}
	if len(segments) == 0 {
		return ""
	}
	return segments[len(segments)-1]
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
//
// 多模态工具（EnhancedInvokableTool）的回包只落在 UserInputMultiContent 上：图片部件不进日志、
// 不进 SSE（抽帧 base64 既不宜整段落库，也不该随事件流推给前端），只取文本部件。
// 首个文本部件是信封时据其判定 is_error，并把信封替换为其中的正文。
func parseToolResultMessage(msg *schema.Message) (isErr bool, text string) {
	if msg == nil {
		return false, ""
	}
	chunks := textChunks(msg)
	if len(chunks) == 0 {
		return false, ""
	}
	if parsedErr, inner, ok := parseToolEnvelope(chunks[0]); ok {
		isErr = parsedErr
		chunks[0] = inner
	}
	return isErr, strings.Join(chunks, "\n")
}

// textChunks 取工具回包的文本块：普通工具在 Content，多模态工具在 UserInputMultiContent。
func textChunks(msg *schema.Message) []string {
	if msg.Content != "" {
		return []string{msg.Content}
	}
	var chunks []string
	for _, part := range msg.UserInputMultiContent {
		if part.Type == schema.ChatMessagePartTypeText && part.Text != "" {
			chunks = append(chunks, part.Text)
		}
	}
	return chunks
}

// parseToolEnvelope 尝试把文本解析为工具信封，返回其中的 is_error 与拼接后的正文。
// 仅当文本是对象且带 content 数组时才认作信封——普通工具回包可能是任意 JSON，不能误吞。
func parseToolEnvelope(raw string) (isErr bool, text string, ok bool) {
	var probe map[string]any
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return false, "", false
	}
	list, found := probe["content"].([]any)
	if !found {
		return false, "", false
	}
	var sb strings.Builder
	for _, item := range list {
		m, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		if s, isStr := m["text"].(string); isStr {
			sb.WriteString(s)
		}
	}
	isErr, _ = probe["is_error"].(bool)
	return isErr, sb.String(), true
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
