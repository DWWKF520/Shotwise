package subagents

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// profileAgentsDir 指向仓库内真实的 agent 源目录（与 Python 侧同源）。
const profileAgentsDir = "../../../../agent_runtime_profile/.claude/agents"

func TestParseFileWithSkillsList(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.md")
	body := "---\nname: create-episode-script\ndescription: \"单集剧本\"\nskills:\n  - generate-script\n  - other\n---\n\n正文\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	def, err := ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if def.Name != "create-episode-script" || def.Description != "单集剧本" {
		t.Fatalf("def = %+v", def)
	}
	if !reflect.DeepEqual(def.Skills, []string{"generate-script", "other"}) {
		t.Fatalf("skills = %v", def.Skills)
	}
	if def.Instruction != "正文" {
		t.Fatalf("instruction = %q", def.Instruction)
	}
}

func TestParseFileRequiresNameAndDescription(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.md")
	if err := os.WriteFile(p, []byte("---\nname: only-name\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFile(p); err == nil {
		t.Fatal("expected error when description missing")
	}
}

func TestLoadDirMissingIsEmpty(t *testing.T) {
	defs, err := LoadDir(filepath.Join(t.TempDir(), "none"))
	if err != nil || len(defs) != 0 {
		t.Fatalf("defs=%v err=%v", defs, err)
	}
}

// 回归：真实 profile 的 6 个 subagent 必须全部可解析且登记了工具白名单。
func TestRealProfileAgentsAllWhitelisted(t *testing.T) {
	defs, err := LoadDir(profileAgentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 6 {
		t.Fatalf("expected 6 subagents, got %d", len(defs))
	}
	for _, d := range defs {
		if _, ok := toolWhitelist[d.Name]; !ok {
			t.Errorf("subagent %s missing tool whitelist", d.Name)
		}
	}
}

func TestBuildRejectsUnwhitelistedSubagent(t *testing.T) {
	b := &Builder{}
	_, err := b.Build(context.Background(), Definition{Name: "rogue", Description: "x", Instruction: "y"})
	if err == nil || !strings.Contains(err.Error(), "白名单") {
		t.Fatalf("expected whitelist error, got %v", err)
	}
}

func TestBuildRequiresSkillBodyWhenSkillsDeclared(t *testing.T) {
	b := &Builder{}
	_, err := b.withSkills(Definition{Name: "create-episode-script", Skills: []string{"generate-script"}})
	if err == nil {
		t.Fatal("expected error when SkillBody is nil")
	}
}
