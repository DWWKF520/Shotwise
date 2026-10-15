package policy

import (
	"context"
	"testing"
)

func TestPreExecuteWhitelist(t *testing.T) {
	p := NewAgentAccessPolicy(t.TempDir(), nil)
	ctx := context.Background()

	if _, err := p.Pre(ctx, ToolCall{Name: "execute", Args: mustJSON(map[string]any{"command": "ls -la"})}); err == nil {
		t.Fatal("execute with non-whitelisted command must be denied")
	}
	if _, err := p.Pre(ctx, ToolCall{Name: "execute", Args: mustJSON(map[string]any{"command": "python .claude/skills/compose-video/scripts/compose_video.py"})}); err != nil {
		t.Fatalf("whitelisted execute denied: %v", err)
	}
}
