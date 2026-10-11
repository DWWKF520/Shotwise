package agent

import (
	"encoding/json"
	"fmt"
)

// EventType 是 SSE 推送的事件种类，与 Python 侧 service.py 的 entry/stream 语义对齐。
type EventType string

const (
	EventDelta      EventType = "delta"       // 文本增量（assistant 流式输出）
	EventToolUse    EventType = "tool_use"    // 模型决定调用某个工具
	EventToolResult EventType = "tool_result" // 工具执行结果
	EventStatus     EventType = "status"      // 运行态（running/done/error）
	EventUsage      EventType = "usage"       // token 用量
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
}

// EncodeSSE 把事件序列化成 SSE 帧：`data: <json>\n\n`。供 transport 层调用。
func (e Event) EncodeSSE() string {
	b, err := json.Marshal(e)
	if err != nil {
		// 序列化兜底：避免单条事件坏掉整个流。
		b = []byte(fmt.Sprintf(`{"type":"error","text":%q}`, err.Error()))
	}
	return "data: " + string(b) + "\n\n"
}

// encodeSSE 包内别名（保留旧调用点兼容）。
func (e Event) encodeSSE() string { return e.EncodeSSE() }
