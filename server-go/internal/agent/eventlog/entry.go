// Package eventlog 承载智能体事件日志（权威 transcript）的定型契约与存储。
//
// 设计文档 03-agent-runtime-eino.md §4.4：不再伪造 Claude SDK transcript，
// 事件日志 = 权威 transcript，append-only + per-session 单调 seq。
// 分叉（前缀复制）基于 entries 复制到新 session_id（后续增量）。
//
// AgentEvent 流 → Entry 的转换逻辑在主包 agent/eventlog_adapter.go
// （转换需要消费 adk.AgentEvent 并产出 SSE Event，归属主包避免循环依赖）。
package eventlog

import (
	"encoding/json"
	"time"
)

// EntryType 是 entry 的一级类型（type 契约）。
type EntryType string

const (
	EntryUser      EntryType = "user"
	EntryAssistant EntryType = "assistant"
	EntryToolResult EntryType = "tool_result"
	EntrySystem    EntryType = "system"
)

// EntrySubtype 是 entry 的二级类型（subtype 契约）。
type EntrySubtype string

const (
	SubtypeInterrupt        EntrySubtype = "interrupt"
	SubtypeQuestion         EntrySubtype = "question"
	SubtypeQuestionAnswer   EntrySubtype = "question_answer"
	SubtypeTaskNotification EntrySubtype = "task_notification"
	SubtypeAgentTurnFailure EntrySubtype = "agent_turn_failure"
	SubtypeSkillInvocation  EntrySubtype = "skill_invocation"
)

// Block 是 content[] 内的归一化内容块（对应 Python turn_schema 的 block 归一化）。
type Block struct {
	Type string          `json:"type"` // text / tool_use / tool_result / image
	Text string          `json:"text,omitempty"`
	// tool_use 块的附属字段。
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// Entry 是事件日志的单条记录。
// parent_tool_use_id 采用 snake_case 单一形态写入；读侧三形态归一
// （parent_tool_use_id / parentToolUseID / parentToolUseId）由消费方兼容。
type Entry struct {
	Seq             int64          `json:"seq"`
	UUID            string         `json:"uuid"`
	SessionID       string         `json:"session_id"`
	Type            EntryType      `json:"type"`
	Subtype         EntrySubtype   `json:"subtype,omitempty"`
	Content         []Block        `json:"content,omitempty"`
	Timestamp       time.Time      `json:"timestamp"`
	ParentToolUseID string         `json:"parent_tool_use_id,omitempty"`
	MessageID       string         `json:"message_id,omitempty"`
	ToolUseID       string         `json:"tool_use_id,omitempty"`
}
