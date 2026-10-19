package domain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// promptRegistryRoot 定位仓库内的 agent_runtime_profile（测试 cwd 在 internal/domain）。
func promptRegistryRoot(t *testing.T) string {
	t.Helper()
	root := AgentProfileRoot()
	if root == "" {
		t.Skip("agent_runtime_profile not found from test cwd")
	}
	if _, err := os.Stat(filepath.Join(root, "CLAUDE.drama.md")); err != nil {
		t.Fatalf("profile root %s missing CLAUDE.drama.md: %v", root, err)
	}
	return root
}

// TestBuildPromptRegistryParityWithPython 与 lib/prompt_registry.py 的实算输出对拍：
// 条目数、顺序、分组、内容模式、标题、字符数（Python len(str) = 码点数）。
func TestBuildPromptRegistryParityWithPython(t *testing.T) {
	entries := BuildPromptRegistry(promptRegistryRoot(t))
	type row struct{ group, path, mode, title string }
	want := []row{
		{"system", "CLAUDE.ad.md", "ad", "AI 视频生成工作空间"},
		{"system", "CLAUDE.drama.md", "drama", "AI 视频生成工作空间"},
		{"system", "CLAUDE.narration.md", "narration", "AI 视频生成工作空间"},
		{"agent", ".claude/agents/analyze-assets.md", "", "analyze-assets"},
		{"agent", ".claude/agents/create-episode-script.md", "", "create-episode-script"},
		{"agent", ".claude/agents/generate-assets.md", "", "generate-assets"},
		{"agent", ".claude/agents/normalize-drama-script.md", "", "normalize-drama-script"},
		{"agent", ".claude/agents/split-narration-segments.md", "", "split-narration-segments"},
		{"agent", ".claude/agents/split-reference-video-units.md", "", "split-reference-video-units"},
		{"skill", ".claude/skills/compose-video/SKILL.md", "", "合成视频"},
		{"skill", ".claude/skills/director-review/SKILL.md", "", "导演审阅"},
		{"skill", ".claude/skills/episode-editing/SKILL.md", "", "Episode editing contract"},
		{"skill", ".claude/skills/generate-assets/SKILL.md", "", "生成资产设计图"},
		{"skill", ".claude/skills/generate-grid/SKILL.md", "", "生成宫格分镜图"},
		{"skill", ".claude/skills/generate-narration-audio/SKILL.md", "", "生成旁白配音"},
		{"skill", ".claude/skills/generate-script/SKILL.md", "", "generate-script"},
		{"skill", ".claude/skills/generate-storyboard/SKILL.md", "", "生成分镜图"},
		{"skill", ".claude/skills/generate-video/SKILL.md", "", "生成视频"},
		{"skill", ".claude/skills/manage-project/SKILL.md", "", "项目管理工具集"},
		{"skill", ".claude/skills/manga-workflow/SKILL.ad.md", "ad", "广告/短片工作流"},
		{"skill", ".claude/skills/manga-workflow/SKILL.drama.md", "drama", "视频工作流编排"},
		{"skill", ".claude/skills/manga-workflow/SKILL.narration.md", "narration", "视频工作流编排"},
		{"reference", ".claude/references/generation-modes.md", "", "生成模式参考"},
	}
	if len(entries) != len(want) {
		t.Fatalf("entry count = %d, want %d", len(entries), len(want))
	}
	for i, expect := range want {
		got := entries[i]
		if got.Group != expect.group || got.Path != expect.path || got.Title != expect.title {
			t.Fatalf("entry %d = %s/%s/%q, want %s/%s/%q", i, got.Group, got.Path, got.Title, expect.group, expect.path, expect.title)
		}
		mode := ""
		if got.ContentMode != nil {
			mode = *got.ContentMode
		}
		if mode != expect.mode {
			t.Fatalf("entry %d (%s) content_mode = %q, want %q", i, got.Path, mode, expect.mode)
		}
		if got.CharCount <= 0 {
			t.Fatalf("entry %d (%s) char_count = %d", i, got.Path, got.CharCount)
		}
	}
	// Python 实算 char_count：CLAUDE.drama.md = 9625。
	for _, entry := range entries {
		if entry.Path == "CLAUDE.drama.md" && entry.CharCount != 9625 {
			t.Fatalf("CLAUDE.drama.md char_count = %d, want 9625", entry.CharCount)
		}
	}
}

func TestFindPromptTemplate(t *testing.T) {
	root := promptRegistryRoot(t)
	detail := FindPromptTemplate(root, "CLAUDE.drama.md")
	if detail == nil {
		t.Fatal("CLAUDE.drama.md must be found")
	}
	if got := utf8.RuneCountInString(detail.Content); got != 9625 {
		t.Fatalf("content length = %d, want 9625 (Python len(str))", got)
	}
	if detail.Template.Title != "AI 视频生成工作空间" {
		t.Fatalf("title = %q", detail.Template.Title)
	}
	// 未知路径与穿越输入一律 nil：查表不走文件系统拼接。
	if FindPromptTemplate(root, "../etc/passwd") != nil {
		t.Fatal("traversal path must not resolve")
	}
	if FindPromptTemplate(root, "CLAUDE.nope.md") != nil {
		t.Fatal("unknown path must not resolve")
	}
}

func TestBuildPromptRegistryEmptyRoot(t *testing.T) {
	if entries := BuildPromptRegistry(""); entries != nil {
		t.Fatalf("empty root must return nil, got %d", len(entries))
	}
	if entries := BuildPromptRegistry(filepath.Join(t.TempDir(), "missing")); entries != nil {
		t.Fatalf("missing root must return nil, got %d", len(entries))
	}
}

// TestPromptFrontmatterAndModeParsing 覆盖 frontmatter / mode 注释 / 截断口径。
func TestPromptFrontmatterAndModeParsing(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	long := strings.Repeat("标", 250)
	write("CLAUDE.drama.md", "---\nname: \"标题\"\nDescription: 第一行说明\n---\n# 文档标题\n\n## 第一节\n\n## 第二节\n正文。\n")
	write(".claude/agents/a.md", "# a\n\n<!-- mode: narration -->\n\n## S1\n")
	write(".claude/skills/s/SKILL.ad.md", "# s\n")
	write(".claude/references/r.md", "---\nname: r\nunterminated: yes\n正文\n")
	// 符号链接必须被拒绝（不越出 profile 投影）。
	if err := os.Symlink(filepath.Join(root, "CLAUDE.drama.md"), filepath.Join(root, "CLAUDE.link.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	entries := BuildPromptRegistry(root)
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want 4 (symlink rejected): %+v", len(entries), entries)
	}
	byPath := map[string]PromptTemplate{}
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	system := byPath["CLAUDE.drama.md"]
	if system.Title != "文档标题" {
		t.Fatalf("heading must win over frontmatter name: %q", system.Title)
	}
	if system.Description != "第一行说明" {
		t.Fatalf("description = %q, want frontmatter value", system.Description)
	}
	if system.ContentMode == nil || *system.ContentMode != "drama" {
		t.Fatalf("content_mode = %v, want drama", system.ContentMode)
	}
	if len(system.Sections) != 2 {
		t.Fatalf("sections = %v", system.Sections)
	}
	agent := byPath[".claude/agents/a.md"]
	if agent.ContentMode == nil || *agent.ContentMode != "narration" {
		t.Fatalf("mode comment not honored: %v", agent.ContentMode)
	}
	if agent.Title != "a" {
		t.Fatalf("fallback title = %q, want file stem", agent.Title)
	}
	skill := byPath[".claude/skills/s/SKILL.ad.md"]
	if skill.Group != PromptGroupSkill || skill.ContentMode == nil || *skill.ContentMode != "ad" {
		t.Fatalf("skill entry = %+v", skill)
	}
	// 未闭合 frontmatter 按空表处理，且首段解析整个跳过围栏后的内容
	// （Python _first_paragraph 的 in_frontmatter 永不闭合）→ 描述为空。
	ref := byPath[".claude/references/r.md"]
	if ref.Description != "" {
		t.Fatalf("unterminated frontmatter description = %q, want empty", ref.Description)
	}
	// 长标题按 200 码点截断并补省略号。
	write("CLAUDE.long.md", "# "+long+"\n")
	entries = BuildPromptRegistry(root)
	for _, entry := range entries {
		if entry.Path == "CLAUDE.long.md" {
			runes := []rune(entry.Title)
			if len(runes) != promptMaxTitleChars || !strings.HasSuffix(entry.Title, "…") {
				t.Fatalf("bounded title runes = %d suffix=%q", len(runes), entry.Title[len(entry.Title)-3:])
			}
			return
		}
	}
	t.Fatal("CLAUDE.long.md missing")
}
