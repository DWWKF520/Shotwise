package agent

import (
	"context"
	"encoding/json"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/shotwise/shotwise/internal/agent/policy"
)

// GuardMiddleware 把 policy.ToolGuard 接入 Eino 的 middleware 链
// （设计文档 03-agent-runtime-eino.md §12）：一个包裹点覆盖全部工具——
// 业务工具、filesystem 注入的文件/Shell 工具、Subagent AgentTool。
//
// 语义对应：
//   - Pre error → deny：错误信息以 is_error 信封回灌模型（对应 canUseTool deny）
//   - Pre 返回改写 args → updated_input（对应 Bash env 剥离）
//   - Post error → 工具回包被替换为失败信封（对应 PostToolUse 校验）
type GuardMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
	guard policy.ToolGuard
}

// NewGuardMiddleware 用给定围栏策略构造 middleware。
func NewGuardMiddleware(guard policy.ToolGuard) *GuardMiddleware {
	return &GuardMiddleware{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}, guard: guard}
}

// WrapInvokableToolCall 实现 middleware：在真实工具执行前后调用 ToolGuard。
func (m *GuardMiddleware) WrapInvokableToolCall(ctx context.Context, ep adk.InvokableToolCallEndpoint, tc *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	if m.guard == nil {
		return ep, nil
	}
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		call := policy.ToolCall{Name: tc.Name, Args: json.RawMessage(args)}
		rewritten, err := m.guard.Pre(ctx, call)
		if err != nil {
			// deny：错误信息以工具失败信封回灌模型，而非抛错中断轮次。
			return denyEnvelope(err.Error()), nil
		}
		argIn := args
		if rewritten != nil {
			argIn = string(rewritten)
		}
		res, runErr := ep(ctx, argIn, opts...)
		if perr := m.guard.Post(ctx, call, res); perr != nil {
			return denyEnvelope(perr.Error()), nil
		}
		return res, runErr
	}, nil
}

// denyEnvelope 生成与 Python 侧约定一致的工具失败回包：
// {"content":[{"type":"text","text":<msg>}],"is_error":true}
func denyEnvelope(msg string) string {
	return toolEnvelope(msg, true)
}

// okEnvelope 生成工具成功回包（content 文本）。
func okEnvelope(text string) string {
	return toolEnvelope(text, false)
}

func toolEnvelope(text string, isError bool) string {
	b, err := json.Marshal(map[string]any{
		"content":  []map[string]any{{"type": "text", "text": text}},
		"is_error": isError,
	})
	if err != nil {
		// 序列化失败时退化为纯文本，保证工具调用闭环不断。
		return text
	}
	return string(b)
}
