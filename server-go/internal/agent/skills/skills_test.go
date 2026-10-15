package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestBackendCommonSkillVisibleInAnyMode(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "plain"), "SKILL.md", "plain", "", "# body\nhello")

	for _, mode := range []string{"", "drama"} {
		b := NewModeAwareBackend(root, mode)
		list, err := b.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].Name != "plain" {
			t.Fatalf("mode %q: list = %+v", mode, list)
		}
	}
	s, err := NewModeAwareBackend(root, "drama").Get(context.Background(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Content, "hello") || !filepath.IsAbs(s.BaseDirectory) {
		t.Fatalf("unexpected skill: %+v", s)
	}
}

func TestBackendVariantFilteredByMode(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "manga")
	for _, m := range ContentModes {
		writeSkill(t, dir, "SKILL."+m+".md", "manga", "true", "body-"+m)
	}

	b := NewModeAwareBackend(root, "narration")
	s, err := b.Get(context.Background(), "manga")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Content, "body-narration") || strings.Contains(s.Content, "body-drama") {
		t.Fatalf("wrong variant body: %q", s.Content)
	}
	if _, err := NewModeAwareBackend(root, "").List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if list, _ := NewModeAwareBackend(root, "").List(context.Background()); len(list) != 0 {
		t.Fatalf("variant-only skill must be hidden without mode, got %+v", list)
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
