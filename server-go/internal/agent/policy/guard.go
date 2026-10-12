// Package policy 承载智能体工具调用的权限围栏，对应 Python 侧
// agent_access_policy.py + PreToolUse/PostToolUse hooks 的裁决逻辑。
//
// 本包是纯逻辑层：不 import Eino 类型，可独立单测。
// 与 Eino 的对接（middleware WrapInvokableToolCall → ToolGuard）在主包 guard.go。
package policy

import (
	"context"
	"encoding/json"
)

// ToolCall 是一次工具调用的抽象快照，交给 ToolGuard 做权限裁决。
type ToolCall struct {
	// Name 是工具名（模型视角，小写归一由实现方处理）。
	Name string
	// Args 是模型给出的 JSON 入参。
	Args json.RawMessage
}

// ToolGuard 对应设计文档 03-agent-runtime-eino.md §12.2 的统一裁决接口：
//   - Pre 返回 (args, error)：error 非 nil → 拒绝，error message 回灌模型（deny 信封）；
//     args 非 nil → 改写后传给真实工具（如 Bash 的 env 剥离）。
//   - Post 校验/审计：返回 error 时工具回包被替换为失败信封。
type ToolGuard interface {
	Pre(ctx context.Context, call ToolCall) (json.RawMessage, error)
	Post(ctx context.Context, call ToolCall, result string) error
}
