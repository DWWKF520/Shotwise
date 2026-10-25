package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// writeProjectClaude 在 dataDir/<project>/.claude 下落一个 skill 与一个 subagent。
func writeProjectClaude(t *testing.T, dataDir, project string) {
	t.Helper()
	skillDir := filepath.Join(dataDir, project, ".claude", "skills", "demo")
	agentDir := filepath.Join(dataDir, project, ".claude", "agents")
	for _, d := range []string{skillDir, agentDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	skillBody := "---\nname: demo\ndescription: demo skill\n---\n# Demo\nstep one\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillBody), 0o644); err != nil {
		t.Fatal(err)
	}
	agentBody := "---\nname: generate-assets\ndescription: 资产生成子代理\n---\n你是资产生成执行器。\n"
	if err := os.WriteFile(filepath.Join(agentDir, "generate-assets.md"), []byte(agentBody), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 模型调 skill 工具 → 轮末事件日志出现 skill_invocation 系统条目（只记 skill 名）。
func TestSkillToolInvocationRecordedAsEntry(t *testing.T) {
	m := &scriptedModel{script: []*schema.Message{
		toolCallMsg("k1", "skill", `{"skill":"demo"}`),
		textMsg("loaded"),
	}}
	r := newTestRuntime(t, m)
	writeProjectClaude(t, r.dataDir, "proj")

	sid, ch, unsub, err := r.Query(context.Background(), "skill-s1", "proj", "use demo skill")
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	drainEvents(t, ch)

	entries, err := r.Entries(context.Background(), sid, -1)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.Subtype == "skill_invocation" && len(e.Content) == 1 && e.Content[0].Text == "demo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("skill_invocation entry for demo not found in %d entries", len(entries))
	}
}

// 模型调子代理 AgentTool → 子代理消息带 parent_tool_use_id 指向该调用。
func TestSubagentEntriesAttributedToAgentToolCall(t *testing.T) {
	m := &scriptedModel{script: []*schema.Message{
		toolCallMsg("g1", "generate-assets", `{"request":"做角色设计"}`),
		textMsg("sub-answer"), // 子代理自己的回答
		textMsg("final"),      // 主代理收尾
	}}
	r := newTestRuntime(t, m)
	writeProjectClaude(t, r.dataDir, "proj")

	sid, ch, unsub, err := r.Query(context.Background(), "sub-s1", "proj", "generate")
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	drainEvents(t, ch)

	entries, err := r.Entries(context.Background(), sid, -1)
	if err != nil {
		t.Fatal(err)
	}
	var attributed, director bool
	for _, e := range entries {
		if e.Type != "assistant" || len(e.Content) == 0 {
			continue
		}
		switch e.Content[0].Text {
		case "sub-answer":
			attributed = e.ParentToolUseID == "g1"
		case "final":
			director = e.ParentToolUseID == ""
		}
	}
	if !attributed {
		t.Fatal("subagent assistant entry should carry parent_tool_use_id=g1")
	}
	if !director {
		t.Fatal("director assistant entry must not carry parent_tool_use_id")
	}
}

func TestParseSkillName(t *testing.T) {
	cases := map[string]string{
		// 官方默认格式（英文 / 中文语言）
		"Launching skill: demo\nBase directory for this skill: C:\\p\\.claude\\skills\\demo\n\nbody": "demo",
		"正在启动 Skill：demo\n此 Skill 的目录：/data/p/.claude/skills/demo\nbody":                             "demo",
		// 旧格式：只有基目录行（路径末段）
		"Base directory for this skill: C:\\p\\.claude\\skills\\generate-video\\SKILL.md\n\nbody": "generate-video",
		"Base directory for this skill: /data/p/.claude/skills/manga-workflow/\nbody":             "manga-workflow",
		"not a skill injection": "",
	}
	for in, want := range cases {
		if got := parseSkillName(in); got != want {
			t.Errorf("parseSkillName(%q) = %q, want %q", in, got, want)
		}
	}
}
