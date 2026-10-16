package ffmpeg

// 审查类子进程调用：需要在进程成功退出后同时拿到 stdout 与 stderr
// （blackdetect / silencedetect / volumedetect 的统计信息都打在 stderr）。

import (
	"context"
	"os/exec"
	"path/filepath"
	"time"
)

// ResolveTool 解析媒体工具的可执行路径；缺失时返回 *RenderToolError
// （对应 Python lib/media_assembly/rendering.py 的 resolve_tool，
// 媒体审查路径不降级）。
func ResolveTool(name string) (string, error) { return resolveTool(name) }

// RunCapture 执行 argv 列表并在退出后同时返回 stdout 与 stderr。
// 超时（timeout <= 0 时取 RenderTimeout）先 kill 再回收；err 非 nil 时
// stderr 仍尽力返回，供调用方截取尾部构造失败详情。
func RunCapture(ctx context.Context, timeout time.Duration, args []string) ([]byte, []byte, error) {
	if len(args) == 0 {
		return nil, nil, &RenderToolError{Code: CodeProcessUnavailable, Message: "media process argv is empty"}
	}
	if timeout <= 0 {
		timeout = RenderTimeout
	}
	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, &RenderToolError{Code: CodeProcessUnavailable, Message: err.Error(), Err: err}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case waitErr := <-done:
		if ctx.Err() != nil {
			return nil, stderr.Bytes(), cancelledError(ctx.Err())
		}
		if waitErr != nil {
			return nil, stderr.Bytes(), &RenderToolError{Code: CodeProcessFailed, Message: "media process failed", Err: waitErr}
		}
		return stdout.Bytes(), stderr.Bytes(), nil
	case <-ctx.Done():
		killAndWait(cmd, done)
		return nil, stderr.Bytes(), cancelledError(ctx.Err())
	case <-timer.C:
		killAndWait(cmd, done)
		return nil, stderr.Bytes(), &RenderToolError{
			Code:    CodeProcessTimeout,
			Message: "media process timed out",
			Err:     &TimeoutError{Tool: filepath.Base(args[0]), Timeout: timeout},
		}
	}
}
