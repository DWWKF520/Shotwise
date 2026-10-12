// Package ffmpeg 封装 ffmpeg/ffprobe 子进程调用：可用性探测、分级超时、stderr 连续排空与进度解析、产物后置校验。
//
// 调用一律走 argv 列表（不经 shell）。缺工具时按用途分两类口径：探测类（缩略图/抽帧/时长）
// 返回零值降级跳过，渲染类返回 RenderToolError 阻断。
package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// 分级超时：读容器元数据瞬时返回，全量解码统计帧数最慢，抽帧、缩略图居中；
// 超时一律 kill 回收，不等已僵死的子进程。
const (
	// MetadataTimeout 覆盖 ffprobe 读容器元数据与音频时长探测。
	MetadataTimeout = 10 * time.Second
	// CountFramesTimeout 覆盖 -count_frames 全量解码统计帧数。
	CountFramesTimeout = 120 * time.Second
	// FrameExtractTimeout 覆盖单帧抽取。
	FrameExtractTimeout = 60 * time.Second
	// ThumbnailTimeout 覆盖首帧缩略图。
	ThumbnailTimeout = 30 * time.Second
	// RenderTimeout 覆盖拼接渲染等长任务，Run 的 timeout <= 0 时取该值。
	RenderTimeout = 900 * time.Second
)

const (
	// cancelPollInterval 是取消检查与进度上报的轮询间隔。
	cancelPollInterval = 500 * time.Millisecond
	// failureTailBytes 限制失败消息保留的 stderr 尾部长度。
	failureTailBytes = 4000
	// positionScanOverlap 是增量扫描 time= 位置时回看的字节数，避免匹配被分块截断。
	positionScanOverlap = 32
)

// 机器码与 Python 侧 RenderToolError.code 口径一致，供上层映射任务失败码。
const (
	CodeFFmpegUnavailable        = "ffmpeg_unavailable"
	CodeFFprobeUnavailable       = "ffprobe_unavailable"
	CodeProcessUnavailable       = "media_process_unavailable"
	CodeProcessTimeout           = "media_process_timeout"
	CodeProcessCancelled         = "media_process_cancelled"
	CodeProcessFailed            = "media_process_failed"
	CodeSourceRefInvalid         = "source_ref_invalid"
	CodeSourceProbeInvalid       = "source_probe_invalid"
	CodeSourceDurationInvalid    = "source_duration_invalid"
	CodeRenderDimensionsInvalid  = "render_dimensions_invalid"
	CodeRenderQualityInvalid     = "render_quality_invalid"
	CodeRenderOutputMissing      = "render_output_missing"
	CodeRenderProbeInvalid       = "render_probe_invalid"
	CodePreviewProbeInvalid      = "preview_probe_invalid"
	CodePackagingDurationInvalid = "packaging_duration_invalid"
	CodePackagingSourceInvalid   = "packaging_source_invalid"
	CodePackagingTextInvalid     = "packaging_text_invalid"
	CodePackagingOutputMissing   = "packaging_output_missing"
	CodeSubtitleOutputMissing    = "subtitle_output_missing"
)

// RenderToolError 是渲染路径的失败：工具缺失、子进程超时/取消/非零退出、产物校验不通过。
// Err 保留底层错误，errors.As/errors.Is 可穿透到 *TimeoutError 或 context 取消。
type RenderToolError struct {
	Code    string
	Message string
	Err     error
}

func (e *RenderToolError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *RenderToolError) Unwrap() error { return e.Err }

// ErrTimeout 供 errors.Is 判别超时分类。
var ErrTimeout = errors.New("media process timeout")

// TimeoutError 是子进程超过分级超时被 kill 回收的可判别类型。
type TimeoutError struct {
	Tool    string
	Timeout time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("%s timed out after %s", e.Tool, e.Timeout)
}

// Is 让 errors.Is(err, ErrTimeout) 命中超时分类，调用方不必认识工具名。
func (e *TimeoutError) Is(target error) bool { return target == ErrTimeout }

func lookupFFmpeg() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

func lookupFFprobe() bool {
	_, err := exec.LookPath("ffprobe")
	return err == nil
}

var (
	ffmpegOnce  = sync.OnceValue(lookupFFmpeg)
	ffprobeOnce = sync.OnceValue(lookupFFprobe)
)

// Available 报告 PATH 中是否存在 ffmpeg，结果缓存。
func Available() bool { return ffmpegOnce() }

// ProbeAvailable 报告 PATH 中是否存在 ffprobe，与 ffmpeg 独立检查：
// 精简容器可能只装其中一个。
func ProbeAvailable() bool { return ffprobeOnce() }

// resetAvailabilityForTests 清缓存，让测试改动 PATH 后立即生效。
func resetAvailabilityForTests() {
	ffmpegOnce = sync.OnceValue(lookupFFmpeg)
	ffprobeOnce = sync.OnceValue(lookupFFprobe)
}

// resolveTool 解析工具绝对路径；缺失即返回对应的 RenderToolError（渲染路径不降级）。
func resolveTool(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", &RenderToolError{
			Code:    name + "_unavailable",
			Message: name + " is not installed or is not on PATH",
			Err:     err,
		}
	}
	return path, nil
}

// RunOption 定制一次子进程调用的输出捕获、进度上报与取消检查。
type RunOption func(*runConfig)

type runConfig struct {
	stdout      *[]byte
	total       float64
	onProgress  func(fraction float64)
	cancelCheck func() bool
}

// WithStdout 把子进程 stdout 追加到 dst 指向的切片，供调用方解析 ffprobe 输出。
func WithStdout(dst *[]byte) RunOption {
	return func(cfg *runConfig) { cfg.stdout = dst }
}

// WithProgress 在 stderr 出现新的 time= 位置时按 totalSeconds 折算 0..1 比例回调；
// totalSeconds <= 0 时不上报。
func WithProgress(totalSeconds float64, report func(fraction float64)) RunOption {
	return func(cfg *runConfig) {
		cfg.total = totalSeconds
		cfg.onProgress = report
	}
}

// WithCancelCheck 每 0.5s 轮询一次，返回 true 即 kill 子进程并返回取消错误。
func WithCancelCheck(check func() bool) RunOption {
	return func(cfg *runConfig) { cfg.cancelCheck = check }
}

// Run 执行 argv 列表并持续排空 stdout/stderr：管道写满会让长任务死锁，
// 因此读取交给 exec 的拷贝 goroutine，而不是先等待退出再读。
//
// timeout <= 0 时取 RenderTimeout。超时先 kill 再回收，返回的 *TimeoutError
// 可经 errors.As 提取、errors.Is(err, ErrTimeout) 判别。
func Run(ctx context.Context, timeout time.Duration, args []string, opts ...RunOption) error {
	if len(args) == 0 {
		return &RenderToolError{Code: CodeProcessUnavailable, Message: "media process argv is empty"}
	}
	var cfg runConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	if timeout <= 0 {
		timeout = RenderTimeout
	}
	stdout := &syncBuffer{}
	stderr := &stderrSink{}

	// Stdin 保持 nil：子进程读到 EOF 而不是继承父进程终端，避免交互式等待挂死调用方。
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return &RenderToolError{Code: CodeProcessUnavailable, Message: err.Error(), Err: err}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(cancelPollInterval)
	defer ticker.Stop()

	var lastReported float64
	hasReported := false
	reportProgress := func() {
		if cfg.onProgress == nil || cfg.total <= 0 {
			return
		}
		position, ok := stderr.latestPosition()
		if !ok || (hasReported && position == lastReported) {
			return
		}
		lastReported, hasReported = position, true
		cfg.onProgress(clamp01(position / cfg.total))
	}

	var (
		stdoutBytes []byte
		runErr      error
	)
loop:
	for {
		select {
		case waitErr := <-done:
			// 进程退出后不会再有写入，补报最后一次位置。
			reportProgress()
			switch {
			case ctx.Err() != nil:
				runErr = cancelledError(ctx.Err())
			case waitErr != nil:
				detail := string(tailBytes(stderr.bytes(), failureTailBytes))
				if detail == "" {
					detail = "media process failed"
				}
				runErr = &RenderToolError{Code: CodeProcessFailed, Message: detail, Err: waitErr}
			default:
				stdoutBytes = stdout.Bytes()
			}
			break loop
		case <-ctx.Done():
			killAndWait(cmd, done)
			runErr = cancelledError(ctx.Err())
			break loop
		case <-timer.C:
			killAndWait(cmd, done)
			runErr = &RenderToolError{
				Code:    CodeProcessTimeout,
				Message: "media process timed out",
				Err:     &TimeoutError{Tool: filepath.Base(args[0]), Timeout: timeout},
			}
			break loop
		case <-ticker.C:
			if cfg.cancelCheck != nil && cfg.cancelCheck() {
				killAndWait(cmd, done)
				runErr = &RenderToolError{Code: CodeProcessCancelled, Message: "media process cancelled"}
				break loop
			}
			reportProgress()
		}
	}
	if cfg.stdout != nil {
		*cfg.stdout = append(*cfg.stdout, stdoutBytes...)
	}
	return runErr
}

// killAndWait 杀掉子进程并回收，避免留下孤儿进程。
func killAndWait(cmd *exec.Cmd, done <-chan error) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	<-done
}

// cancelledError 把上下文取消归类为可判别的取消错误，cause 供 errors.Is 判断。
func cancelledError(cause error) error {
	return &RenderToolError{Code: CodeProcessCancelled, Message: "media process cancelled", Err: cause}
}

// syncBuffer 是 exec 拷贝 goroutine 与调用方并发访问的字节缓冲。
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf...)
}

// ffmpegPositionPattern 匹配 ffmpeg 进度行里的 time= 位置。
var ffmpegPositionPattern = regexp.MustCompile(`time=(\d+):(\d{2}):(\d{2}(\.\d+)?)`)

// stderrSink 持续接收 stderr：累积正文（失败时取尾部）并解析最新的 time= 位置。
type stderrSink struct {
	mu       sync.Mutex
	buf      []byte
	scanned  int
	position float64
	hasPos   bool
}

func (s *stderrSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	return len(p), nil
}

// latestPosition 返回 stderr 中最新的 time= 位置；增量扫描只处理新到达的字节，
// 回看一小段保证跨块匹配不被漏掉（重复解析同一匹配是幂等的）。
func (s *stderrSink) latestPosition() (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := s.scanned - positionScanOverlap
	if start < 0 {
		start = 0
	}
	for _, match := range ffmpegPositionPattern.FindAllSubmatch(s.buf[start:], -1) {
		hours, errHours := strconv.Atoi(string(match[1]))
		minutes, errMinutes := strconv.Atoi(string(match[2]))
		seconds, errSeconds := strconv.ParseFloat(string(match[3]), 64)
		if errHours != nil || errMinutes != nil || errSeconds != nil {
			continue
		}
		s.position = float64(hours)*3600 + float64(minutes)*60 + seconds
		s.hasPos = true
	}
	s.scanned = len(s.buf)
	return s.position, s.hasPos
}

func (s *stderrSink) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf...)
}

// tailBytes 取字节尾部，用于把超长 stderr 收进有上限的错误消息。
func tailBytes(b []byte, limit int) []byte {
	if len(b) > limit {
		return b[len(b)-limit:]
	}
	return b
}

func clamp01(value float64) float64 {
	if math.IsNaN(value) || value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

// fileExists 报告路径存在且是普通文件。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// nonEmptyFile 报告路径存在、是普通文件且非空，用于渲染产物的后置校验。
func nonEmptyFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 0
}

// logSkip 记录一次降级跳过，避免缺工具时静默无输出。
func logSkip(reason, path string) {
	slog.Info(reason, "path", path)
}
