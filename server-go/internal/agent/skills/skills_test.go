package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/agent/workspace"
)

func writeSkill(t *testing.T, dir, file, name, invocable, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: desc of " + name + "\n"
	if invocable != "" {
		content += "user-invocable: " + invocable + "\n"
	}
	content += "---\n" + body
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 官方 backend + 官方 middleware：能发现并加载 <root>/<name>/SKILL.md，
// BaseDirectory 指向 skill 目录、正文不含 frontmatter。
func TestFilesystemMiddlewareLoadsSkill(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "demo"), "SKILL.md", "demo", "", "# body\nhello world")

	fsb, err := workspace.NewBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	mw, backend, err := NewFilesystemMiddleware(context.Background(), fsb, root)
	if err != nil {
		t.Fatal(err)
	}
	if mw == nil {
		t.Fatal("middleware must be registered for an existing skills dir")
	}

	list, err := backend.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "demo" {
		t.Fatalf("list = %+v, want [demo]", list)
	}

	s, err := backend.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Content, "hello world") || strings.Contains(s.Content, "user-invocable") {
		t.Fatalf("content = %q", s.Content)
	}
	if !filepath.IsAbs(s.BaseDirectory) || filepath.Base(s.BaseDirectory) != "demo" {
		t.Fatalf("base dir = %q, want absolute .../demo", s.BaseDirectory)
	}

	if _, err := backend.Get(context.Background(), "nope"); err == nil {
		t.Fatal("unknown skill must error")
	}
}

// 空 skills 目录也能装配（quickstart 的"没配置 skill 也能跑"语义），列表为空。
func TestFilesystemMiddlewareEmptyDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills") // 尚不存在
	fsb, err := workspace.NewBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, backend, err := NewFilesystemMiddleware(context.Background(), fsb, root); err != nil {
		t.Fatal(err)
	} else if list, err := backend.List(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("empty dir list = %+v, err = %v", list, err)
	}
}

// 变体 skill 在 profile 同步后已投影为项目侧 SKILL.md，官方 backend 只认该入口。
func TestFilesystemMiddlewareIgnoresVariantOnlySkill(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "manga"), "SKILL.drama.md", "manga", "true", "body")

	fsb, err := workspace.NewBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	_, backend, err := NewFilesystemMiddleware(context.Background(), fsb, root)
	if err != nil {
		t.Fatal(err)
	}
	if list, err := backend.List(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("variant-only skill must be invisible (sync 负责投影), list = %+v, err = %v", list, err)
	}
}

func TestValidateDirRejectsPartialVariants(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "partial"), "SKILL.drama.md", "partial", "true", "x")
	if err := ValidateDir(root); err == nil {
		t.Fatal("expected error for partial variants")
	}
}

func TestValidateDirRejectsCommonAndVariantConflict(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "both")
	writeSkill(t, dir, "SKILL.md", "both", "", "x")
	writeSkill(t, dir, "SKILL.drama.md", "both", "", "x")
	if err := ValidateDir(root); err == nil {
		t.Fatal("expected conflict error")
	}
}

func TestValidateDirRejectsInconsistentInvocable(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "mixed")
	writeSkill(t, dir, "SKILL.drama.md", "mixed", "true", "x")
	writeSkill(t, dir, "SKILL.narration.md", "mixed", "false", "x")
	writeSkill(t, dir, "SKILL.ad.md", "mixed", "true", "x")
	if err := ValidateDir(root); err == nil {
		t.Fatal("expected user-invocable inconsistency error")
	}
}

func TestValidateDirMissingRootIsEmpty(t *testing.T) {
	if err := ValidateDir(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("missing root should be empty, got %v", err)
	}
}

func TestParseFileUserInvocableAndFallbackDescription(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(p, []byte("---\nname: x\nuser-invocable: no\n---\n# title\nfirst line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ParseFile(p, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if m.UserInvocable {
		t.Fatal("user-invocable: no must be false")
	}
	if m.Description != "first line" {
		t.Fatalf("description fallback = %q", m.Description)
	}
}

func TestListUserInvocableFiltersAndIcons(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "generate-video"), "SKILL.md", "generate-video", "", "x")
	writeSkill(t, filepath.Join(root, "hidden"), "SKILL.md", "hidden", "false", "x")

	entries, err := ListUserInvocable(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "generate-video" || entries[0].Icon != "film" {
		t.Fatalf("entries = %+v", entries)
	}
}
