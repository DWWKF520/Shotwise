package project

import "github.com/shotwise/shotwise/internal/agent/tools"

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
)

func writeTextFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func newTextToolsFixture(t *testing.T) (list, read tool.InvokableTool, root string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "proj")
	writeTextFixture(t, root, map[string]string{
		"project.json":           `{"name":"proj"}`,
		"source/novel.txt":       "第一行\n第二行\n第三行\n",
		"source/raw/novel.bin":   "raw backup",
		"source/.hidden.md":      "hidden",
		"drafts/episode_1.md":    "草稿",
		"scripts/episode_1.json": `{"a":1}`,
		"scripts/image.png":      "binary",
		"notes.txt":              "outside categories",
	})
	gate := &tools.TextFileGate{}
	l, err := NewListProjectTextFilesTool("proj", root, gate)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReadProjectTextTool("proj", root, gate)
	if err != nil {
		t.Fatal(err)
	}
	return l, r, root
}

func TestListProjectTextFilesOrdersCategoriesAndSkipsRawAndHidden(t *testing.T) {
	list, _, _ := newTextToolsFixture(t)
	out, err := list.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"当前首选文稿候选（source/）", "source/novel.txt", "备用项目文本（drafts/", "scripts/episode_1.json", "项目元数据"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"raw", ".hidden", "image.png", "notes.txt"} {
		if strings.Contains(out, banned) {
			t.Errorf("list output must not contain %q:\n%s", banned, out)
		}
	}
	if strings.Index(out, "source/novel.txt") > strings.Index(out, "drafts/episode_1.md") {
		t.Errorf("source/ must precede drafts/ in priority order:\n%s", out)
	}
}

func TestReadProjectTextRequiresFreshListing(t *testing.T) {
	list, read, root := newTextToolsFixture(t)
	args := `{"path":"source/novel.txt"}`

	out, err := read.InvokableRun(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "请先调用 list_project_text_files") || !strings.Contains(out, `"is_error":true`) {
		t.Fatalf("read before list must be denied: %s", out)
	}

	if _, err := list.InvokableRun(context.Background(), `{}`); err != nil {
		t.Fatal(err)
	}
	out, _ = read.InvokableRun(context.Background(), args)
	if !strings.Contains(out, "行范围: 1-3 / 3") {
		t.Fatalf("read after list should succeed: %s", out)
	}

	writeTextFixture(t, root, map[string]string{"drafts/new.md": "新文件"})
	out, _ = read.InvokableRun(context.Background(), args)
	if !strings.Contains(out, "请先调用 list_project_text_files") {
		t.Fatalf("stale listing after file change must be denied: %s", out)
	}
}

func TestReadProjectTextPaginationAndRestrictions(t *testing.T) {
	list, read, _ := newTextToolsFixture(t)
	if _, err := list.InvokableRun(context.Background(), `{}`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	out, _ := read.InvokableRun(ctx, `{"path":"source/novel.txt","start_line":2,"max_lines":1}`)
	if !strings.Contains(out, "行范围: 2-2 / 3") || !strings.Contains(out, "第二行") || strings.Contains(out, "第一行") {
		t.Fatalf("pagination wrong: %s", out)
	}

	cases := map[string]string{
		`{"path":"source/raw/novel.bin"}`:             "source/raw/",
		`{"path":"../project.json"}`:                  "",
		`{"path":"scripts/image.png"}`:                "只允许读取",
		`{"path":"notes.txt"}`:                        "只能读取",
		`{"path":"source/novel.txt","max_lines":401}`: "不能超过",
		`{"path":"  "}`:                               "非空",
	}
	for args, want := range cases {
		out, err := read.InvokableRun(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `"is_error":true`) {
			t.Errorf("args %s must be denied: %s", args, out)
		}
		if want != "" && !strings.Contains(out, want) {
			t.Errorf("args %s: want %q in %s", args, want, out)
		}
	}
}

func TestSplitTextLinesMatchesPythonSplitlines(t *testing.T) {
	cases := map[string]int{"": 0, "a": 1, "a\n": 1, "a\n\n": 2, "a\r\nb\rc": 3}
	for in, want := range cases {
		if got := len(splitTextLines(in)); got != want {
			t.Errorf("splitTextLines(%q) = %d lines, want %d", in, got, want)
		}
	}
}
