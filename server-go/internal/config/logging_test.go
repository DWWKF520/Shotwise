package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveLogDirDefault(t *testing.T) {
	t.Setenv("SHOTWISE_LOG_DIR", "")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got := ResolveLogDir(); got != filepath.Join(wd, "logs") {
		t.Errorf("ResolveLogDir() = %q, want %q", got, filepath.Join(wd, "logs"))
	}
}

func TestResolveLogDirAbsolute(t *testing.T) {
	t.Setenv("SHOTWISE_LOG_DIR", "/var/log/shotwise")
	if got := ResolveLogDir(); got != "/var/log/shotwise" {
		t.Errorf("ResolveLogDir() = %q, want /var/log/shotwise", got)
	}
}

func TestResolveLogDirRelativeJoinsCwd(t *testing.T) {
	t.Setenv("SHOTWISE_LOG_DIR", "  rel/logs ")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(wd, "rel", "logs")
	if got := ResolveLogDir(); got != want {
		t.Errorf("ResolveLogDir() = %q, want %q（相对路径须基于 cwd 且容忍空白）", got, want)
	}
}

func TestLogFileDisabled(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"0", false},
		{"no", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"Yes", true},
		{"  yes  ", true},
	}
	for _, c := range cases {
		t.Setenv("SHOTWISE_LOG_FILE_DISABLED", c.in)
		if got := logFileDisabled(); got != c.want {
			t.Errorf("logFileDisabled() with %q = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestLoadPopulatesLogConfig(t *testing.T) {
	t.Setenv("SHOTWISE_LOG_DIR", "/tmp/shotwise-logs")
	t.Setenv("SHOTWISE_LOG_FILE_DISABLED", "1")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogDir != "/tmp/shotwise-logs" {
		t.Errorf("cfg.LogDir = %q, want /tmp/shotwise-logs", cfg.LogDir)
	}
	if !cfg.LogFileDisabled {
		t.Error("cfg.LogFileDisabled must be true when SHOTWISE_LOG_FILE_DISABLED=1")
	}
}
