package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/schema"
	jsonschema "github.com/eino-contrib/jsonschema"
)

// Kind 标识一次工具调用的访问性质，供 GuardedTool 的权限策略分派。
type Kind int

const (
	KindRead  Kind = iota // 只读（Read/Glob/Grep/list_*）
	KindWrite             // 写文件（Write/Edit/patch_*）
	KindBash              // 执行命令（Bash）
	KindAction            // 业务动作（generate_* 等，不需文件系统围栏）
)

// Invocation 是一次工具调用的上下文，交给 Guard 做权限裁决。
type Invocation struct {
	ToolName string
	Kind     Kind
	Args     json.RawMessage
}

// Guard 承担原 Claude Agent SDK 的权限链（PreToolUse / canUseTool / 沙箱投影）。
// Check 返回 error 即拒绝；返回的 error message 会原样回灌给模型作为工具失败反馈。
type Guard interface {
	Check(ctx context.Context, inv Invocation) error
}

// Tool 是一个可被模型调用的 Eino 工具。对应文档里 37 个进程内 MCP 工具的形态。
type Tool interface {
	Name() string
	Kind() Kind
	Info() *schema.ToolInfo
	Run(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

// GuardedTool 装饰任意 Tool，在 Run 前后调用 Guard 做权限/hook 裁决。
// 这是文档 4.1 节 ★核心 的落点：替代 hooks + canUseTool。
type GuardedTool struct {
	inner Tool
	guard Guard
}

// NewGuardedTool 包裹一个工具并绑定权限策略。
func NewGuardedTool(inner Tool, guard Guard) *GuardedTool {
	return &GuardedTool{inner: inner, guard: guard}
}

func (g *GuardedTool) Name() string         { return g.inner.Name() }
func (g *GuardedTool) Kind() Kind           { return g.inner.Kind() }
func (g *GuardedTool) Info() *schema.ToolInfo { return g.inner.Info() }

func (g *GuardedTool) Run(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if g.guard != nil {
		if err := g.guard.Check(ctx, Invocation{ToolName: g.inner.Name(), Kind: g.inner.Kind(), Args: args}); err != nil {
			// 拒绝信息以工具失败形式回灌模型，与 Python 侧 is_error 约定一致。
			return denyResult(err.Error()), nil
		}
	}
	return g.inner.Run(ctx, args)
}

// ToolSet 聚合一组工具，并产出 BindTools 所需的 []*schema.ToolInfo。
type ToolSet struct {
	tools []Tool
	byName map[string]Tool
}

// NewToolSet 构造空工具集。
func NewToolSet() *ToolSet {
	return &ToolSet{byName: map[string]Tool{}}
}

// Register 注册一个工具（自动包一层 GuardedTool，guard 为 nil 时仅透传）。
func (ts *ToolSet) Register(t Tool, guard Guard) {
	wrapped := NewGuardedTool(t, guard)
	ts.tools = append(ts.tools, wrapped)
	ts.byName[wrapped.Name()] = wrapped
}

// Get 按名取工具。
func (ts *ToolSet) Get(name string) (Tool, bool) {
	t, ok := ts.byName[name]
	return t, ok
}

// Infos 返回所有工具的 schema（用于 model.BindTools）。
func (ts *ToolSet) Infos() []*schema.ToolInfo {
	out := make([]*schema.ToolInfo, 0, len(ts.tools))
	for _, t := range ts.tools {
		out = append(out, t.Info())
	}
	return out
}

// Run 按名执行工具；未知工具返回结构化错误。
func (ts *ToolSet) Run(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	t, ok := ts.byName[name]
	if !ok {
		return denyResult(fmt.Sprintf("unknown tool: %s", name)), nil
	}
	return t.Run(ctx, args)
}

// toolInfo 用 JSON Schema 构造 *schema.ToolInfo。
func toolInfo(name, desc string, schemaObj any) *schema.ToolInfo {
	js := toJSONSchema(schemaObj)
	return &schema.ToolInfo{
		Name:        name,
		Desc:        desc,
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js),
	}
}

// toJSONSchema 把任意结构体转成 jsonschema.Schema。
func toJSONSchema(v any) *jsonschema.Schema {
	// Reflect 反射生成 schema；v 为 nil 表示无参数。
	if v == nil {
		return &jsonschema.Schema{Type: "object"}
	}
	return jsonschema.Reflect(v)
}

// denyResult 生成与 Python 侧 {"is_error":true,...} 一致的工具失败回包。
func denyResult(msg string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"is_error": true,
	})
	return b
}

// okResult 生成工具成功回包（content 文本）。
func okResult(text string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"is_error": false,
	})
	return b
}
