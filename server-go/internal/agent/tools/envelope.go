package tools

import (
	"encoding/json"

	"github.com/cloudwego/eino/schema"
)

// 智能体工具的公共底座：信封、队列等待、生成任务编排、文本清单门闸、
// 项目/剧本读取等横切设施，供 internal/agent/tools 下各分类子包与 agent 运行时共用。
//
// 本文件实现与 Python 侧约定一致的工具回包信封：
// {"content":[{"type":"text","text":<msg>}],"is_error":<bool>}

// Envelope 生成工具回包；序列化失败时退化为纯文本，保证工具调用闭环不断。
func Envelope(text string, isError bool) string {
	b, err := json.Marshal(map[string]any{
		"content":  []map[string]any{{"type": "text", "text": text}},
		"is_error": isError,
	})
	if err != nil {
		return text
	}
	return string(b)
}

// OKEnvelope 生成工具成功回包（content 文本）。
func OKEnvelope(text string) string {
	return Envelope(text, false)
}

// DenyEnvelope 生成工具失败回包（is_error=true）。
func DenyEnvelope(msg string) string {
	return Envelope(msg, true)
}

// DenyResult 把失败信封封装为多模态结果的唯一文本部件。
func DenyResult(msg string) *schema.ToolResult {
	return &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: DenyEnvelope(msg)}}}
}
