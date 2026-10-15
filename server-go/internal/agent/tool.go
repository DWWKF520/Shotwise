package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
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
		if runErr != nil && !isControlFlow(runErr) {
			// 工具执行失败与 deny 一致回灌模型（is_error 信封），而非中断整轮。
			return denyEnvelope(runErr.Error()), nil
		}
		if perr := m.guard.Post(ctx, call, res); perr != nil {
			return denyEnvelope(perr.Error()), nil
		}
		return res, runErr
	}, nil
}

// WrapEnhancedInvokableToolCall 对多模态工具（EnhancedInvokableTool）实现与 WrapInvokableToolCall 同语义的包裹：
// 框架只把 InvokableTool 交给 WrapInvokableToolCall，不实现这里则多模态工具会绕过 ToolGuard。
// deny 与执行失败都回灌为文本 is_error 信封的 ToolResult，结构与字符串工具一致。
func (m *GuardMiddleware) WrapEnhancedInvokableToolCall(ctx context.Context, ep adk.EnhancedInvokableToolCallEndpoint, tc *adk.ToolContext) (adk.EnhancedInvokableToolCallEndpoint, error) {
	if m.guard == nil {
		return ep, nil
	}
	return func(ctx context.Context, arg *schema.ToolArgument, opts ...tool.Option) (*schema.ToolResult, error) {
		args := ""
		if arg != nil {
			args = arg.Text
		}
		call := policy.ToolCall{Name: tc.Name, Args: json.RawMessage(args)}
		rewritten, err := m.guard.Pre(ctx, call)
		if err != nil {
			return denyResult(err.Error()), nil
		}
		argIn := args
		if rewritten != nil {
			argIn = string(rewritten)
		}
		res, runErr := ep(ctx, &schema.ToolArgument{Text: argIn}, opts...)
		if runErr != nil && !isControlFlow(runErr) {
			return denyResult(runErr.Error()), nil
		}
		if perr := m.guard.Post(ctx, call, toolResultText(res)); perr != nil {
			return denyResult(perr.Error()), nil
		}
		return res, runErr
	}, nil
}

// denyResult 把失败信封封装为多模态结果的唯一文本部件。
func denyResult(msg string) *schema.ToolResult {
	return &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: denyEnvelope(msg)}}}
}

// toolResultText 拼接多模态结果中的文本部件，供 Post 校验使用（图片部件不参与文本校验）。
func toolResultText(res *schema.ToolResult) string {
	if res == nil {
		return ""
	}
	var parts []string
	for _, p := range res.Parts {
		if p.Type == schema.ToolPartTypeText {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// isControlFlow 判定错误是否为框架控制信号（HITL 中断、取消），这类错误必须原样上抛。
func isControlFlow(err error) bool {
	if _, ok := compose.IsInterruptRerunError(err); ok {
		return true
	}
	var cancelErr *adk.CancelError
	return errors.As(err, &cancelErr)
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
