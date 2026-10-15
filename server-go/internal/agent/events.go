package agent

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// EventType 是 SSE 推送的事件种类，与 Python 侧 service.py 的 entry/stream 语义对齐。
type EventType string

const (
	EventDelta      EventType = "delta"       // 文本增量（assistant 流式输出）
	EventToolUse    EventType = "tool_use"    // 模型决定调用某个工具
	EventToolResult EventType = "tool_result" // 工具执行结果
	EventStatus     EventType = "status"      // 运行态（running/done/error/cancelled）
	EventUsage      EventType = "usage"       // token 用量
	EventQuestion   EventType = "question"    // ask_user 挂起，等待用户回答（HITL）
	EventTaskNotice EventType = "task_notice" // 后台任务完成通知注入会话
	EventEntry      EventType = "entry"       // 事件日志条目（冷读回放与断线续传）
)

// Event 是智能体运行时向外推送的最小事件单元。
// seq 单调递增，供前端做去重与乱序重组；sessionID 用于多会话分流。
type Event struct {
	Seq       int64     `json:"seq"`
	SessionID string    `json:"session_id"`
	Type      EventType `json:"type"`
	// Text 用于 delta / status / 文本型 tool_result。
	Text string `json:"text,omitempty"`
	// ToolName 用于 tool_use / tool_result。
	ToolName string `json:"tool_name,omitempty"`
	// IsError 标记工具执行是否失败。
	IsError bool `json:"is_error,omitempty"`
	// Meta 承载结构化附加信息（如 tool 入参 JSON、usage 明细）。
	Meta json.RawMessage `json:"meta,omitempty"`
	// Raw 非空时作为 SSE data: 的载荷原样输出（entry 冷读回放：前端拿到的
	// data 就是条目本身，而不是套一层信封）。
	Raw json.RawMessage `json:"-"`
}

// EncodeSSE 把事件序列化成 SSE 帧：`event: <type>` / `id: <seq>` / `data: <json>`。
// id 仅在 Seq > 0 时输出——entry 派生事件带 seq，EventSource 的 Last-Event-ID
// 即 seq 游标，重连据此续传（设计文档 §14.1）；delta 等无条目事件不带 id。
func (e Event) EncodeSSE() string {
	data := e.Raw
	if len(data) == 0 {
		b, err := json.Marshal(e)
		if err != nil {
			// 序列化兜底：避免单条事件坏掉整个流。
			b = []byte(fmt.Sprintf(`{"type":"error","text":%q}`, err.Error()))
		}
		data = b
	}
	var sb strings.Builder
	sb.WriteString("event: ")
	sb.WriteString(string(e.Type))
	sb.WriteByte('\n')
	if e.Seq > 0 {
		sb.WriteString("id: ")
		sb.WriteString(strconv.FormatInt(e.Seq, 10))
		sb.WriteByte('\n')
	}
	sb.WriteString("data: ")
	sb.Write(data)
	sb.WriteString("\n\n")
	return sb.String()
}
