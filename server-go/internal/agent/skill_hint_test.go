package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestUserTurnTextAddsSkillHintPrefix(t *testing.T) {
	plain := userTurnText(InboxItem{Kind: InboxUserMessage, Text: "开始"})
	if plain != "开始" {
		t.Fatalf("plain = %q", plain)
	}
	hinted := userTurnText(InboxItem{Kind: InboxUserMessage, Text: "开始", SkillHint: "generate-video"})
	if hinted != "[用户请求触发 skill: generate-video] 开始" {
		t.Fatalf("hinted = %q", hinted)
	}
}

func TestQueryWithSkillRejectsUnknownSkill(t *testing.T) {
	r := newTestRuntime(t, &scriptedModel{})
	writeProjectClaude(t, r.dataDir, "proj")

	_, _, _, err := r.QueryWithSkill(context.Background(), "hint-s1", "proj", "no-such-skill", "go")
	if !errors.Is(err, ErrSkillNotInvocable) {
		t.Fatalf("expected ErrSkillNotInvocable, got %v", err)
	}
}

func TestQueryWithSkillAcceptsInvocableSkill(t *testing.T) {
	r := newTestRuntime(t, &scriptedModel{script: []*schema.Message{textMsg("ok")}})
	writeProjectClaude(t, r.dataDir, "proj")

	_, ch, unsub, err := r.QueryWithSkill(context.Background(), "hint-s2", "proj", "demo", "go")
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	drainEvents(t, ch)
}
