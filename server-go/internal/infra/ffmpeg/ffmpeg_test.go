package ffmpeg

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTimeoutTiersMatchPipelineContract(t *testing.T) {
	cases := map[string]struct {
		got  time.Duration
		want time.Duration
	}{
		"metadata":     {MetadataTimeout, 10 * time.Second},
		"count_frames": {CountFramesTimeout, 120 * time.Second},
		"抽帧":           {FrameExtractTimeout, 60 * time.Second},
		"缩略图":          {ThumbnailTimeout, 30 * time.Second},
		"渲染":           {RenderTimeout, 900 * time.Second},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if testCase.got != testCase.want {
				t.Errorf("timeout = %s, want %s", testCase.got, testCase.want)
			}
		})
	}
}

func TestAvailabilityFollowsPath(t *testing.T) {
	removeTools(t)
	if Available() {
		t.Error("Available() = true, want false when ffmpeg is not on PATH")
	}
	if ProbeAvailable() {
		t.Error("ProbeAvailable() = true, want false when ffprobe is not on PATH")
	}

	installStub(t)
	if !Available() {
		t.Error("Available() = false, want true after installing stub")
	}
	if !ProbeAvailable() {
		t.Error("ProbeAvailable() = false, want true after installing stub")
	}
}

func TestAvailabilityCachesLookPath(t *testing.T) {
	installStub(t)
	if !Available() {
		t.Fatal("Available() = false, want true with stub on PATH")
	}
	// 缓存生效：把 PATH 换成空目录、不清缓存时结论保持不变。
	t.Setenv("PATH", t.TempDir())
	if !Available() {
		t.Error("Available() = false, want cached true")
	}
	// 清缓存后重新探测。
	resetAvailabilityForTests()
	if Available() {
		t.Error("Available() = true, want false after cache reset with empty PATH")
	}
}

func TestRunRejectsEmptyArgv(t *testing.T) {
	err := Run(context.Background(), time.Second, nil)
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeProcessUnavailable {
		t.Fatalf("err = %v, want RenderToolError with code %s", err, CodeProcessUnavailable)
	}
}

func TestRunDrainsStdoutWithoutDeadlock(t *testing.T) {
	installStub(t)
	const size = 512 * 1024
	stubEnv(t, map[string]string{stubRepeatEnv: strconv.Itoa(size)})

	var stdout []byte
	err := Run(context.Background(), 30*time.Second, []string{"ffmpeg", "-i", "in.mp4"}, WithStdout(&stdout))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(stdout) != size || strings.Trim(string(stdout), "o") != "" {
		t.Errorf("stdout length = %d, want %d bytes of %q", len(stdout), size, "o")
	}
}

func TestRunCapturesStdoutOnSuccessOnly(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{stubStdoutEnv: "payload", stubExitEnv: "1"})

	var stdout []byte
	if err := Run(context.Background(), 30*time.Second, []string{"ffmpeg"}, WithStdout(&stdout)); err == nil {
		t.Fatal("Run error = nil, want a process failure")
	}
	if len(stdout) != 0 {
		t.Errorf("stdout = %q, want nothing captured from a failed process", stdout)
	}
}

func TestRunClassifiesTimeout(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{stubSleepMsEnv: "5000"})

	start := time.Now()
	err := Run(context.Background(), 100*time.Millisecond, []string{"ffmpeg", "-i", "slow.mp4"})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout did not kill the process promptly: %s", elapsed)
	}
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeProcessTimeout {
		t.Fatalf("err = %v, want RenderToolError with code %s", err, CodeProcessTimeout)
	}
	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("err = %v, want distinguishable *TimeoutError", err)
	}
	if timeoutErr.Timeout != 100*time.Millisecond {
		t.Errorf("TimeoutError.Timeout = %s, want 100ms", timeoutErr.Timeout)
	}
	if timeoutErr.Tool != "ffmpeg" {
		t.Errorf("TimeoutError.Tool = %q, want ffmpeg", timeoutErr.Tool)
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, want errors.Is(err, ErrTimeout)", err)
	}
}

func TestRunZeroTimeoutFallsBackToRenderTimeout(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{stubSleepMsEnv: "300"})

	if err := Run(context.Background(), 0, []string{"ffmpeg"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunCancelCheckKillsProcess(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{stubSleepMsEnv: "5000"})

	err := Run(context.Background(), 30*time.Second, []string{"ffmpeg", "-i", "in.mp4"},
		WithCancelCheck(func() bool { return true }))
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeProcessCancelled {
		t.Fatalf("err = %v, want RenderToolError with code %s", err, CodeProcessCancelled)
	}
}

func TestRunCancelCheckRunsWhileProcessIsAlive(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{stubSleepMsEnv: "700"})

	checks := 0
	err := Run(context.Background(), 30*time.Second, []string{"ffmpeg"},
		WithCancelCheck(func() bool { checks++; return false }))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 进程存活约 700ms，只应落在 500ms 那一次轮询上。
	if checks != 1 {
		t.Errorf("CancelCheck calls = %d, want 1", checks)
	}
}

func TestRunContextCancellation(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{stubSleepMsEnv: "5000"})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	err := Run(ctx, 30*time.Second, []string{"ffmpeg", "-i", "in.mp4"})
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeProcessCancelled {
		t.Fatalf("err = %v, want RenderToolError with code %s", err, CodeProcessCancelled)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
}

func TestRunNonZeroExitReportsStderrTail(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{
		stubStderrEnv: "boom: invalid input\n",
		stubExitEnv:   "3",
	})

	err := Run(context.Background(), 30*time.Second, []string{"ffmpeg", "-i", "in.mp4"})
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeProcessFailed {
		t.Fatalf("err = %v, want RenderToolError with code %s", err, CodeProcessFailed)
	}
	if !strings.Contains(toolErr.Message, "boom: invalid input") {
		t.Errorf("message = %q, want stderr text", toolErr.Message)
	}
}

func TestRunExitedProcessWithoutStderrFallsBackToDefaultMessage(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{stubExitEnv: "1"})

	err := Run(context.Background(), 30*time.Second, []string{"ffmpeg"})
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Message != "media process failed" {
		t.Fatalf("err = %v, want default media process failure message", err)
	}
}

func TestRunTruncatesLongStderrTail(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{stubRepeatEnv: strconv.Itoa(64 * 1024), stubExitEnv: "1"})

	err := Run(context.Background(), 30*time.Second, []string{"ffmpeg"})
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("err = %v, want RenderToolError", err)
	}
	if len(toolErr.Message) > failureTailBytes {
		t.Errorf("message length = %d, want at most %d", len(toolErr.Message), failureTailBytes)
	}
}

func TestRunParsesProgressFromStderr(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{
		stubModeEnv:    stubModeEcho,
		stubStderrEnv:  "frame= 10 time=00:00:00.40 speed=1x\nframe= 20 time=00:00:00.80\nframe= 25 time=00:00:01.00\n",
		stubSleepMsEnv: "700",
	})

	var fractions []float64
	err := Run(context.Background(), 30*time.Second, []string{"ffmpeg", "-i", "in.mp4"},
		WithProgress(4, func(fraction float64) { fractions = append(fractions, fraction) }))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fractions) != 1 || fractions[0] != 0.25 {
		t.Fatalf("progress fractions = %v, want [0.25] (last position 1.00s of 4s)", fractions)
	}
}

func TestRunSkipsProgressWithoutTotal(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{stubStderrEnv: "time=00:00:02.00\n"})

	called := false
	err := Run(context.Background(), 30*time.Second, []string{"ffmpeg"},
		WithProgress(0, func(float64) { called = true }))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called {
		t.Error("progress reported without a positive total duration")
	}
}

func TestRunClampsProgressAboveOne(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{stubStderrEnv: "time=00:00:10.00\n"})

	var fraction float64
	err := Run(context.Background(), 30*time.Second, []string{"ffmpeg"},
		WithProgress(2, func(value float64) { fraction = value }))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fraction != 1 {
		t.Errorf("fraction = %v, want clamped to 1", fraction)
	}
}

func TestRunSpawnFailureIsProcessUnavailable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	err := Run(context.Background(), time.Second, []string{missing})
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeProcessUnavailable {
		t.Fatalf("err = %v, want RenderToolError with code %s", err, CodeProcessUnavailable)
	}
}

func TestResolveToolReportsMissingBinary(t *testing.T) {
	removeTools(t)
	_, err := resolveTool("ffmpeg")
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeFFmpegUnavailable {
		t.Fatalf("err = %v, want RenderToolError with code %s", err, CodeFFmpegUnavailable)
	}
	if !strings.Contains(toolErr.Message, "not on PATH") {
		t.Errorf("message = %q, want PATH hint", toolErr.Message)
	}
}
