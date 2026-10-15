package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk/filesystem"
)

func newTestBackend(t *testing.T) (*Backend, string) {
	t.Helper()
	root := t.TempDir()
	b, err := NewBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	return b, root
}

func TestWriteReadEditRoundTrip(t *testing.T) {
	b, _ := newTestBackend(t)
	ctx := context.Background()

	if err := b.Write(ctx, &filesystem.WriteRequest{FilePath: "drafts/ep1.txt", Content: "alpha\nbeta\ngamma\n"}); err != nil {
		t.Fatal(err)
	}
	got, err := b.Read(ctx, &filesystem.ReadRequest{FilePath: "drafts/ep1.txt"})
	if err != nil || got.Content != "alpha\nbeta\ngamma\n" {
		t.Fatalf("read = %q, %v", got.Content, err)
	}
	if err := b.Edit(ctx, &filesystem.EditRequest{FilePath: "drafts/ep1.txt", OldString: "beta", NewString: "BETA"}); err != nil {
		t.Fatal(err)
	}
	got, _ = b.Read(ctx, &filesystem.ReadRequest{FilePath: "drafts/ep1.txt"})
	if !strings.Contains(got.Content, "BETA") {
		t.Fatalf("edit not applied: %q", got.Content)
	}
}

func TestEditRequiresUniqueMatchUnlessReplaceAll(t *testing.T) {
	b, root := newTestBackend(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(root, "dup.txt"), []byte("x x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.Edit(ctx, &filesystem.EditRequest{FilePath: "dup.txt", OldString: "x", NewString: "y"}); err == nil {
		t.Fatal("expected error for non-unique match")
	}
	if err := b.Edit(ctx, &filesystem.EditRequest{FilePath: "dup.txt", OldString: "x", NewString: "y", ReplaceAll: true}); err != nil {
		t.Fatal(err)
	}
	if err := b.Edit(ctx, &filesystem.EditRequest{FilePath: "dup.txt", OldString: "nope", NewString: "y"}); err == nil {
		t.Fatal("expected error for missing match")
	}
}

func TestReadOffsetLimit(t *testing.T) {
	b, root := newTestBackend(t)
	if err := os.WriteFile(filepath.Join(root, "n.txt"), []byte("1\n2\n3\n4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := b.Read(context.Background(), &filesystem.ReadRequest{FilePath: "n.txt", Offset: 2, Limit: 2})
	if err != nil || got.Content != "2\n3\n" {
		t.Fatalf("offset/limit = %q, %v", got.Content, err)
	}
}

func TestPathConfinement(t *testing.T) {
	b, root := newTestBackend(t)
	ctx := context.Background()
	outside := filepath.Join(filepath.Dir(root), "escape.txt")

	if err := b.Write(ctx, &filesystem.WriteRequest{FilePath: "../escape.txt", Content: "x"}); err == nil {
		t.Fatal("relative escape must be rejected")
	}
	if err := b.Write(ctx, &filesystem.WriteRequest{FilePath: outside, Content: "x"}); err == nil {
		t.Fatal("absolute path outside root must be rejected")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("file escaped the root (stat err=%v)", err)
	}
}

func TestLsGlobGrep(t *testing.T) {
	b, root := newTestBackend(t)
	ctx := context.Background()
	must := func(p, c string) {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("a/one.md", "hello world\nsecond")
	must("a/b/two.md", "nothing here")
	must(".hidden/x.md", "hello hidden")

	ls, err := b.LsInfo(ctx, &filesystem.LsInfoRequest{Path: "a"})
	if err != nil || len(ls) != 2 {
		t.Fatalf("ls = %+v, %v", ls, err)
	}

	gl, err := b.GlobInfo(ctx, &filesystem.GlobInfoRequest{Pattern: "**/*.md", Path: "."})
	if err != nil || len(gl) != 3 {
		t.Fatalf("glob = %+v, %v", gl, err)
	}

	gr, err := b.GrepRaw(ctx, &filesystem.GrepRequest{Pattern: "hello", Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	if len(gr) != 1 || gr[0].Path != "a/one.md" || gr[0].Line != 1 {
		t.Fatalf("grep should skip hidden dirs and report one match, got %+v", gr)
	}
}

func TestShellRejectsNonWhitelistedCommand(t *testing.T) {
	s := &Shell{root: t.TempDir()}
	if _, err := s.Execute(context.Background(), &filesystem.ExecuteRequest{Command: "ls -la"}); err == nil {
		t.Fatal("non-whitelisted command must be rejected")
	}
	if _, err := s.Execute(context.Background(), &filesystem.ExecuteRequest{Command: "python x.py; rm -rf /"}); err == nil {
		t.Fatal("metacharacter injection must be rejected")
	}
}

func TestScrubEnvDropsSecretKeys(t *testing.T) {
	env := []string{"PATH=/bin", "ANTHROPIC_API_KEY=sk-1", "OTHER=2"}
	out := scrubEnv(env, []string{"anthropic_api_key"})
	for _, kv := range out {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Fatalf("secret key leaked: %v", out)
		}
	}
	if len(out) != 2 {
		t.Fatalf("unexpected env: %v", out)
	}
}
