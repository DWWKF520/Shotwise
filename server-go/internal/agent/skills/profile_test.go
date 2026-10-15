package skills

import (
	"testing"
)

// 回归：真实 profile 的 skill 目录必须通过装配期校验（含 manga-workflow 三 mode 变体）。
func TestRealProfileSkillsValidate(t *testing.T) {
	const root = "../../../../agent_runtime_profile/.claude/skills"
	if err := ValidateDir(root); err != nil {
		t.Fatalf("real profile skills invalid: %v", err)
	}
	entries, err := ListUserInvocable(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("expected user-invocable skills in real profile")
	}
}
