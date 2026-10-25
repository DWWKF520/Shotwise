package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile 在 root/rel 写入内容（自动建目录）。
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// 变体 skill 按 content_mode 投影成项目侧 SKILL.md；不匹配的 mode 不落地。
func TestSyncProfileProjectsVariantSkillByMode(t *testing.T) {
	profile := t.TempDir()
	project := t.TempDir()
	for _, mode := range ContentModes {
		writeFile(t, profile, ".claude/skills/manga/SKILL."+mode+".md", "body of "+mode)
	}
	writeFile(t, profile, "CLAUDE.drama.md", "drama prompt")

	created, err := SyncProfile(profile, project, "drama")
	if err != nil {
		t.Fatal(err)
	}
	if created != 2 {
		t.Fatalf("created = %d, want 2 (skill + CLAUDE.md)", created)
	}
	if got := readFile(t, project, ".claude/skills/manga/SKILL.md"); got != "body of drama" {
		t.Fatalf("SKILL.md = %q, want drama variant", got)
	}
	if got := readFile(t, project, "CLAUDE.md"); got != "drama prompt" {
		t.Fatalf("CLAUDE.md = %q", got)
	}
	// 其他 mode 的变体不落地。
	for _, mode := range []string{"ad", "narration"} {
		if _, err := os.Stat(filepath.Join(project, ".claude", "skills", "manga", "SKILL."+mode+".md")); !os.IsNotExist(err) {
			t.Fatalf("variant %s must not be projected (stat err=%v)", mode, err)
		}
	}
}

// 已有项目侧文件（用户修改）不覆盖；重复同步不产生新文件。
func TestSyncProfileKeepsExistingAndIsIdempotent(t *testing.T) {
	profile := t.TempDir()
	project := t.TempDir()
	writeFile(t, profile, ".claude/skills/plain/SKILL.md", "builtin body")
	writeFile(t, profile, ".claude/agents/reviewer.md", "agent body")
	writeFile(t, project, ".claude/skills/plain/SKILL.md", "user edited")

	created, err := SyncProfile(profile, project, "narration")
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1 (仅 agent)", created)
	}
	if got := readFile(t, project, ".claude/skills/plain/SKILL.md"); got != "user edited" {
		t.Fatalf("user edit overwritten: %q", got)
	}
	if got := readFile(t, project, ".claude/agents/reviewer.md"); got != "agent body" {
		t.Fatalf("agent = %q", got)
	}

	created, err = SyncProfile(profile, project, "narration")
	if err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatalf("second sync created = %d, want 0", created)
	}
}

// skill 目录附带的脚本等非变体文件原样复制。
func TestSyncProfileCopiesSkillAttachments(t *testing.T) {
	profile := t.TempDir()
	project := t.TempDir()
	writeFile(t, profile, ".claude/skills/gen/SKILL.md", "body")
	writeFile(t, profile, ".claude/skills/gen/scripts/run.py", "print('hi')")

	if _, err := SyncProfile(profile, project, "drama"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, project, ".claude/skills/gen/scripts/run.py"); got != "print('hi')" {
		t.Fatalf("script = %q", got)
	}
}

// 未修改的内置文件随 profile 升级更新；用户改过的不覆盖；用户删除的不复活。
func TestSyncProfileManifestStates(t *testing.T) {
	profile := t.TempDir()
	project := t.TempDir()
	writeFile(t, profile, ".claude/skills/a/SKILL.md", "v1")
	writeFile(t, profile, ".claude/skills/b/SKILL.md", "v1")
	writeFile(t, profile, ".claude/skills/c/SKILL.md", "v1")

	if _, err := SyncProfile(profile, project, "drama"); err != nil {
		t.Fatal(err)
	}

	// profile 升级 v2。
	writeFile(t, profile, ".claude/skills/a/SKILL.md", "v2")
	writeFile(t, profile, ".claude/skills/b/SKILL.md", "v2")
	writeFile(t, profile, ".claude/skills/c/SKILL.md", "v2")
	// b 用户改过；c 用户删除。
	writeFile(t, project, ".claude/skills/b/SKILL.md", "user edit")
	if err := os.Remove(filepath.Join(project, ".claude", "skills", "c", "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	changed, err := SyncProfile(profile, project, "drama")
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1 (仅 a 升级)", changed)
	}
	if got := readFile(t, project, ".claude/skills/a/SKILL.md"); got != "v2" {
		t.Fatalf("a = %q, want v2", got)
	}
	if got := readFile(t, project, ".claude/skills/b/SKILL.md"); got != "user edit" {
		t.Fatalf("b = %q, user edit must be preserved", got)
	}
	if _, err := os.Stat(filepath.Join(project, ".claude", "skills", "c", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("c must stay deleted (tombstone), stat err=%v", err)
	}

	// manifest 三态落盘：a 记录新哈希，b 保留旧记录，c 为 tombstone。
	raw, err := os.ReadFile(filepath.Join(project, manifestFile))
	if err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	var saved struct {
		Files map[string]struct {
			Source string `json:"source"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Files[".claude/skills/c/SKILL.md"].Source != "tombstone" {
		t.Fatalf("c source = %q, want tombstone", saved.Files[".claude/skills/c/SKILL.md"].Source)
	}
	if saved.Files[".claude/skills/a/SKILL.md"].Source != "profile" {
		t.Fatalf("a source = %q, want profile", saved.Files[".claude/skills/a/SKILL.md"].Source)
	}
}

// 通用与变体并存 / 同一逻辑文件多变体 → fail loud（与 Python 配置校验同口径）。
func TestSyncProfileRejectsConflictingLayout(t *testing.T) {
	profile := t.TempDir()
	writeFile(t, profile, ".claude/skills/dup/SKILL.md", "common")
	writeFile(t, profile, ".claude/skills/dup/SKILL.drama.md", "variant")

	if _, err := SyncProfile(profile, t.TempDir(), "drama"); err == nil {
		t.Fatal("want conflict error for common+variant skill")
	} else if !strings.Contains(err.Error(), "投影到同一目标") {
		t.Fatalf("err = %v", err)
	}

	profile2 := t.TempDir()
	writeFile(t, profile2, "CLAUDE.md", "common")
	writeFile(t, profile2, "CLAUDE.drama.md", "variant")
	if _, err := SyncProfile(profile2, t.TempDir(), "drama"); err == nil {
		t.Fatal("want conflict error for common+variant CLAUDE")
	}
}

// profile 目录缺失时静默跳过（部署未带 profile 不阻断）。
func TestSyncProfileMissingProfileDir(t *testing.T) {
	for _, dir := range []string{"", filepath.Join(t.TempDir(), "nope")} {
		created, err := SyncProfile(dir, t.TempDir(), "drama")
		if err != nil || created != 0 {
			t.Fatalf("SyncProfile(%q) = (%d, %v), want (0, nil)", dir, created, err)
		}
	}
}

// 非法 content_mode 报错（与 Python ValueError 同口径）。
func TestSyncProfileRejectsInvalidMode(t *testing.T) {
	profile := t.TempDir()
	writeFile(t, profile, ".claude/skills/plain/SKILL.md", "body")
	if _, err := SyncProfile(profile, t.TempDir(), "nope"); err == nil {
		t.Fatal("want error for invalid content_mode")
	}
}
