package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/shotwise/shotwise/internal/agent/policy"
)

// review 工具与 guard middleware 的组合行为：deny 必须回信封而非 error，
// allow 透传且 Post 只收到文本部件（原 tools_review_test.go 用例，随包重组迁入）。

// reviewAllowGuard 放行所有调用并记录 Post 收到的结果文本。
type reviewAllowGuard struct{ posted string }

func (g *reviewAllowGuard) Pre(context.Context, policy.ToolCall) (json.RawMessage, error) {
	return nil, nil
}

func (g *reviewAllowGuard) Post(_ context.Context, _ policy.ToolCall, result string) error {
	g.posted = result
	return nil
}

// reviewDenyGuard 拒绝所有调用。
type reviewDenyGuard struct{}

func (reviewDenyGuard) Pre(context.Context, policy.ToolCall) (json.RawMessage, error) {
	return nil, errors.New("blocked by policy")
}

func (reviewDenyGuard) Post(context.Context, policy.ToolCall, string) error { return nil }

func TestReviewGuardWrapsEnhancedToolCall(t *testing.T) {
	ctx := context.Background()
	tc := &adk.ToolContext{Name: "inspect_director_review"}
	called := false
	ep := func(context.Context, *schema.ToolArgument, ...tool.Option) (*schema.ToolResult, error) {
		called = true
		return &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: "ok"}}}, nil
	}

	deny, err := NewGuardMiddleware(reviewDenyGuard{}).WrapEnhancedInvokableToolCall(ctx, ep, tc)
	if err != nil {
		t.Fatal(err)
	}
	res, err := deny(ctx, &schema.ToolArgument{Text: `{}`})
	if err != nil {
		t.Fatalf("deny must return envelope, not error: %v", err)
	}
	if called {
		t.Fatal("denied call must not reach the tool")
	}
	if len(res.Parts) != 1 || !strings.Contains(res.Parts[0].Text, `"is_error":true`) ||
		!strings.Contains(res.Parts[0].Text, "blocked by policy") {
		t.Fatalf("deny envelope malformed: %+v", res.Parts)
	}

	guard := &reviewAllowGuard{}
	allow, err := NewGuardMiddleware(guard).WrapEnhancedInvokableToolCall(ctx, ep, tc)
	if err != nil {
		t.Fatal(err)
	}
	res, err = allow(ctx, &schema.ToolArgument{Text: `{}`})
	if err != nil || !called || res.Parts[0].Text != "ok" {
		t.Fatalf("allowed call should pass through: err=%v called=%v parts=%+v", err, called, res.Parts)
	}
	if guard.posted != "ok" {
		t.Fatalf("Post received %q, want text parts only", guard.posted)
	}
}
