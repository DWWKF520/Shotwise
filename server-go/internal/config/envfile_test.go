package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newEnvFileTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

func TestGeneratePasswordAlphabet(t *testing.T) {
	pwd, err := generatePassword(16)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(pwd) != 16 {
		t.Fatalf("length = %d, want 16", len(pwd))
	}
	for _, r := range pwd {
		if !strings.ContainsRune(passwordAlphabet, r) {
			t.Fatalf("unexpected rune %q", r)
		}
	}
	// 两次生成不应相同（概率性，失败概率可忽略）。
	other, _ := generatePassword(16)
	if other == pwd {
		t.Fatalf("two passwords identical")
	}
}

func TestEnsureAuthPasswordDisabled(t *testing.T) {
	t.Setenv("AUTH_PASSWORD", "leftover")
	pwd, err := EnsureAuthPassword(false, filepath.Join(t.TempDir(), ".env"), newEnvFileTestLogger())
	if err != nil || pwd != "" {
		t.Fatalf("disabled: pwd=%q err=%v", pwd, err)
	}
}

func TestEnsureAuthPasswordExisting(t *testing.T) {
	t.Setenv("AUTH_PASSWORD", "existing-pwd")
	pwd, err := EnsureAuthPassword(true, filepath.Join(t.TempDir(), ".env"), newEnvFileTestLogger())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if pwd != "existing-pwd" {
		t.Fatalf("pwd = %q, want existing-pwd", pwd)
	}
}

func TestEnsureAuthPasswordGenerateCreatesFile(t *testing.T) {
	t.Setenv("AUTH_PASSWORD", "")
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")

	pwd, err := EnsureAuthPassword(true, envPath, newEnvFileTestLogger())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(pwd) != 16 {
		t.Fatalf("pwd length = %d", len(pwd))
	}
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "AUTH_PASSWORD="+pwd {
		t.Fatalf("env content = %q, want AUTH_PASSWORD=%s", got, pwd)
	}
	if os.Getenv("AUTH_PASSWORD") != pwd {
		t.Fatalf("process env not updated")
	}
}

func TestEnsureAuthPasswordReplacesExistingLine(t *testing.T) {
	t.Setenv("AUTH_PASSWORD", "")
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	seed := "AUTH_TOKEN_SECRET=keep\n# 注释\nAUTH_PASSWORD=\nAUTH_USERNAME=admin\n"
	if err := os.WriteFile(envPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	pwd, err := EnsureAuthPassword(true, envPath, newEnvFileTestLogger())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "AUTH_PASSWORD="+pwd+"\n") {
		t.Fatalf("replaced line missing: %q", content)
	}
	if !strings.Contains(content, "AUTH_TOKEN_SECRET=keep") || !strings.Contains(content, "AUTH_USERNAME=admin") {
		t.Fatalf("other lines lost: %q", content)
	}
	if strings.Count(content, "AUTH_PASSWORD=") != 1 {
		t.Fatalf("duplicate AUTH_PASSWORD lines: %q", content)
	}
}

func TestEnsureAuthPasswordSkipsNonUTF8(t *testing.T) {
	t.Setenv("AUTH_PASSWORD", "")
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	// cp936 编码的中文注释字节（非 UTF-8）。
	if err := os.WriteFile(envPath, append([]byte("AUTH_PASSWORD=\n"), 0xD6, 0xD0, 0xCE, 0xC4), 0o600); err != nil {
		t.Fatal(err)
	}

	pwd, err := EnsureAuthPassword(true, envPath, newEnvFileTestLogger())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// 进程内密码仍生效，文件保持原样。
	if len(pwd) != 16 || os.Getenv("AUTH_PASSWORD") != pwd {
		t.Fatalf("process env not set")
	}
	data, _ := os.ReadFile(envPath)
	if !strings.Contains(string(data), "AUTH_PASSWORD=\n") {
		t.Fatalf("non-UTF-8 file must not be rewritten: %q", data)
	}
}

func TestLoadDotenv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := "# comment\n" +
		"LOADDOTENV_TEST_A=from-file\n" +
		"LOADDOTENV_TEST_B=\"quoted value\"\n" +
		"LOADDOTENV_TEST_C='single'\n" +
		"export LOADDOTENV_TEST_D=exported\n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LOADDOTENV_TEST_A", "from-process")
	t.Setenv("LOADDOTENV_TEST_B", "")
	t.Setenv("LOADDOTENV_TEST_C", "")
	t.Setenv("LOADDOTENV_TEST_D", "")

	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	LoadDotenv()

	if got := os.Getenv("LOADDOTENV_TEST_A"); got != "from-process" {
		t.Fatalf("existing env must win: %q", got)
	}
	if got := os.Getenv("LOADDOTENV_TEST_B"); got != "quoted value" {
		t.Fatalf("double-quoted value: %q", got)
	}
	if got := os.Getenv("LOADDOTENV_TEST_C"); got != "single" {
		t.Fatalf("single-quoted value: %q", got)
	}
	if got := os.Getenv("LOADDOTENV_TEST_D"); got != "exported" {
		t.Fatalf("export prefix: %q", got)
	}
}

func TestUnquoteEnvValue(t *testing.T) {
	cases := map[string]string{
		`"a b"`:       "a b",
		`'a b'`:       "a b",
		`plain`:       "plain",
		`"unbalanced`: `"unbalanced`,
		`""`:          "",
	}
	for in, want := range cases {
		if got := unquoteEnvValue(in); got != want {
			t.Errorf("unquote(%q) = %q, want %q", in, got, want)
		}
	}
}
