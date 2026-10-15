package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 回归（§17.6）：每个用户可触发的 skill 都必须在前端 dashboard i18n 三语中有 skill_name_<id> 键，
// 与 Python 侧 tests/test_frontend_skill_i18n.py 的约束等价。
func TestUserInvocableSkillsHaveI18nKeys(t *testing.T) {
	const skillsRoot = "../../../../agent_runtime_profile/.claude/skills"
	entries, err := ListUserInvocable(skillsRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, lang := range []string{"zh", "en", "vi"} {
		raw, err := os.ReadFile(filepath.Join("../../../../frontend/src/i18n", lang, "dashboard.ts"))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, e := range entries {
			key := "'skill_name_" + strings.ReplaceAll(e.Name, "-", "_") + "'"
			if !strings.Contains(text, key) {
				t.Errorf("lang %s: missing i18n key %s for skill %s", lang, key, e.Name)
			}
		}
	}
}
