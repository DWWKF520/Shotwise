// Package logging 构造进程级 slog logger：stdout 始终保留，另按天轮转写日志文件。
//
// 语义对齐 Python 侧 lib/logging_config.py：
//   - 文件名 shotwise.log，本地日期切换时轮转
//   - 轮转副本命名 shotwise.log.YYYY-MM-DD（日期取副本内容所写的那一天）
//   - 副本最多保留 7 份，超出删除最旧（副本名自带日期，字典序即时间序）
//   - 目录或文件打开失败不阻断启动，降级为仅 stdout 并告警
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// fileName 是日志文件名，与 Python 侧一致。
	fileName = "shotwise.log"
	// backupCount 是轮转副本保留份数。
	backupCount = 7
	dateLayout = "2006-01-02"
)

// Options 控制进程级 logger 的构造。
type Options struct {
	// Level 是日志级别字符串（debug/info/warn/warning/error，不区分大小写），
	// 非法值回落 info（对齐 Python 侧 getattr 回落）。
	Level string
	// Dir 是日志目录；空串表示仅 stdout。
	Dir string
	// FileDisabled 关闭文件日志，仅 stdout。
	FileDisabled bool
	// Now 可注入时钟，测试轮转用；nil 用 time.Now。
	Now func() time.Time
}

// Setup 构造双写 stdout 与日志文件的 logger。文件日志打开失败时
// 告警并降级为仅 stdout（对齐 Python 侧 attach_file_handler 的容错）。
func Setup(o Options) *slog.Logger {
	level := ParseLevel(o.Level)
	if o.Dir == "" || o.FileDisabled {
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	}
	file, err := openDaily(o.Dir, fileName, o.Now)
	if err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
		logger.Warn("file logging disabled", "error", err)
		return logger
	}
	return slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stdout, file), &slog.HandlerOptions{Level: level}))
}

// ParseLevel 把 LOG_LEVEL 字符串解析为 slog 级别，非法值回落 info。
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// dailyFile 是按本地日期轮转的追加写文件，轮转在 Write 时惰性触发。
// 进程退出不依赖显式 Close：os.File 无用户态缓冲，append 写直达内核。
type dailyFile struct {
	mu   sync.Mutex
	path string // 目录内完整路径
	dir  string
	f    *os.File
	day  string // 当前打开文件对应的本地日期
	now  func() time.Time
}

func openDaily(dir, name string, now func() time.Time) (*dailyFile, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	w := &dailyFile{path: filepath.Join(dir, name), dir: dir, now: now}
	if err := w.openLocked(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *dailyFile) openLocked() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w.f = f
	w.day = w.now().Format(dateLayout)
	return nil
}

func (w *dailyFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if day := w.now().Format(dateLayout); day != w.day {
		w.rotateLocked(day)
	}
	if w.f == nil {
		// 上次轮转打开失败：每次 Write 重试，避免一次错误永久关闭文件日志。
		if err := w.openLocked(); err != nil {
			return 0, err
		}
	}
	return w.f.Write(p)
}

// rotateLocked 尽力而为：重命名或重开失败时打 stderr 继续运行，
// 已写入的内容不会丢——重开失败后仍会在下次 Write 时重试。
func (w *dailyFile) rotateLocked(day string) {
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
	if _, err := os.Stat(w.path); err == nil {
		if err := os.Rename(w.path, w.path+"."+w.day); err != nil {
			fmt.Fprintf(os.Stderr, "logging: rotate %s failed: %v\n", w.path, err)
		}
	}
	w.pruneBackupsLocked()
	if err := w.openLocked(); err != nil {
		fmt.Fprintf(os.Stderr, "logging: reopen %s failed: %v\n", w.path, err)
	}
}

// pruneBackupsLocked 删除超出保留份数的最旧副本。
func (w *dailyFile) pruneBackupsLocked() {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	prefix := filepath.Base(w.path) + "."
	var backups []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if name := e.Name(); strings.HasPrefix(name, prefix) {
			backups = append(backups, name)
		}
	}
	sort.Strings(backups)
	for len(backups) > backupCount {
		if err := os.Remove(filepath.Join(w.dir, backups[0])); err != nil {
			return // 删不动就停，下轮再试
		}
		backups = backups[1:]
	}
}

// Close 关闭当前打开的文件；仅供测试与显式收尾，进程退出不必调用。
func (w *dailyFile) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
