package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want string // slog.Level.String() 结果
	}{
		{"debug", "DEBUG"},
		{"DEBUG", "DEBUG"},
		{"info", "INFO"},
		{"warn", "WARN"},
		{"warning", "WARN"},
		{"WARNING", "WARN"},
		{"error", "ERROR"},
		{"", "INFO"},
		{"bogus", "INFO"},
		{" warn ", "WARN"},
	}
	for _, c := range cases {
		if got := ParseLevel(c.in).String(); got != c.want {
			t.Errorf("ParseLevel(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestDailyFileAppendAcrossSameDay(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.Local)
	clock := &clock{t: base}
	w, err := openDaily(dir, fileName, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("line1\n")); err != nil {
		t.Fatal(err)
	}
	clock.t = base.Add(5 * time.Hour)
	if _, err := w.Write([]byte("line2\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "line1\nline2\n" {
		t.Fatalf("same-day writes must append to one file, got %q", data)
	}
}

func TestDailyFileRotatesOnDateChange(t *testing.T) {
	dir := t.TempDir()
	day1 := time.Date(2026, 1, 1, 23, 59, 0, 0, time.Local)
	day2 := time.Date(2026, 1, 2, 0, 0, 30, 0, time.Local)
	clock := &clock{t: day1}
	w, err := openDaily(dir, fileName, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("day1\n")); err != nil {
		t.Fatal(err)
	}
	clock.t = day2
	if _, err := w.Write([]byte("day2\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	backup := filepath.Join(dir, fileName+".2026-01-01")
	backupData, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("rotated backup missing: %v", err)
	}
	if string(backupData) != "day1\n" {
		t.Fatalf("backup content = %q, want day1 content", backupData)
	}
	active, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(active) != "day2\n" {
		t.Fatalf("active content = %q, want day2 content", active)
	}
}

func TestDailyFilePrunesOldBackups(t *testing.T) {
	dir := t.TempDir()
	// 预置 8 份轮转副本。
	for i := 1; i <= 8; i++ {
		name := filepath.Join(dir, fmt.Sprintf("%s.2026-01-%02d", fileName, i))
		if err := os.WriteFile(name, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	day9 := time.Date(2026, 1, 9, 8, 0, 0, 0, time.Local)
	clock := &clock{t: day9}
	w, err := openDaily(dir, fileName, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	// openLocked 不会触发轮转（打开当天无旧内容）；写一行并推进日期触发 prune。
	if _, err := w.Write([]byte("today\n")); err != nil {
		t.Fatal(err)
	}
	clock.t = day9.AddDate(0, 0, 1)
	if _, err := w.Write([]byte("next\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var backups []string
	for _, e := range entries {
		if name := e.Name(); len(name) > len(fileName)+1 && name[:len(fileName)+1] == fileName+"." {
			backups = append(backups, name)
		}
	}
	if len(backups) != backupCount {
		t.Fatalf("backup count = %d, want %d (%v)", len(backups), backupCount, backups)
	}
	// 最旧的 2026-01-01 应被删除。
	if _, err := os.Stat(filepath.Join(dir, fileName+".2026-01-01")); err == nil {
		t.Error("oldest backup must be pruned")
	}
}

func TestSetupDirIsFileDegradesToStdout(t *testing.T) {
	dir := t.TempDir()
	fileAsDir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	logger := Setup(Options{Level: "info", Dir: filepath.Join(fileAsDir, "logs")})
	if logger == nil {
		t.Fatal("Setup must return a usable stdout-only logger on file open failure")
	}
	logger.Info("degraded but alive")
}

func TestSetupFileDisabledSkipsDir(t *testing.T) {
	// Dir 非空但禁用文件日志：不触碰目录，返回 stdout logger。
	dir := filepath.Join(t.TempDir(), "logs")
	logger := Setup(Options{Dir: dir, FileDisabled: true})
	if logger == nil {
		t.Fatal("logger must not be nil")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("log dir must not be created when file logging is disabled")
	}
}

// clock 是可推进的测试时钟。
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
