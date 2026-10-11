// Package agent 是用 Eino 重建的 Shotwise 智能体运行时。
//
// 设计要点（与 docs/go-migration/03-agent-runtime-eino.md 对齐）：
//   - 取代 Claude Agent SDK 的子进程模型：Eino 是纯 Go 库，Agent 就是 goroutine，
//     不再需要 SessionActor 串行化子进程调用（ADR 0028 的约束消失）。
//   - ReAct 循环透明自建（这正是 eino adk.ChatModelAgent 内部所做），便于在
//     工具调用前后插入 GuardedTool 权限校验与 hooks。adk.NewChatModelAgent 是
//     后续可平滑升级的官方封装，本包先以自研循环换取透明与可控。
//   - 工具以 tool.InvokableTool 形态注册（对应文档里的 37 个进程内 MCP 工具），
//     统一包一层 GuardedTool 装饰器承担权限与 hook（替代 PreToolUse/PostToolUse + canUseTool）。
//   - SSE 事件面保留 Python 侧 entry/delta/tool_use/tool_result/status 语义。
package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
)

// ModelConfig 描述一个可工具调用的 ChatModel 连接。
// 生产环境这些值来自 DB 的 provider_config / provider_credential；dev 来自 env。
type ModelConfig struct {
	APIKey  string // 可为空（本地/网关免密）
	Model   string
	BaseURL string // 空则走 OpenAI 默认网关；Anthropic Claude 可填 OpenAI 兼容网关
}

// NewChatModel 构造一个支持工具调用的 Eino ChatModel。
// 返回的 model.ToolCallingChatModel 可直接 BindTools / Stream / Generate。
func NewChatModel(ctx context.Context, cfg ModelConfig) (model.ToolCallingChatModel, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("agent: model name is required")
	}
	oc := &openai.ChatModelConfig{
		Model:   cfg.Model,
		APIKey:  cfg.APIKey,
		BaseURL: cfg.BaseURL,
		Timeout: 5 * time.Minute,
	}
	cm, err := openai.NewChatModel(ctx, oc)
	if err != nil {
		return nil, fmt.Errorf("agent: create eino chat model: %w", err)
	}
	return cm, nil
}
