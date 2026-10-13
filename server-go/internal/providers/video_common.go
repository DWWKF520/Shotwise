package providers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// 视频 backend 共享基础设施（端口自 lib/video_backends/base.py）：
//
//   - PollWithRetry —— 通用轮询：pollFn / isDone / isFailed / interval / maxWait / retryIf / onProgress；
//   - SubmitPost    —— create/提交阶段（非幂等 POST）统一分流「确定未送达」与歧义态；
//   - DownloadVideo —— 从 provider 签发的 URL 流式下载结果（幂等 GET，自带退避重试）；
//   - PersistProviderJobIDFor —— submit 后立即落库 job_id（+端点），fail-fast 防幽灵任务。
//
// 各家 backend 只在「怎么跟某家供应商说话」上分叉，上述四件事收口在本文件，避免同一类降级
// 在不同供应商下有的重试、有的重复计费。

// PollConfig 是一次轮询序列的参数。
type PollConfig[T any] struct {
	// PollFn 每次轮询调用（幂等 GET）。
	PollFn func(ctx context.Context) (T, error)
	// IsDone 判断结果是否表示任务完成。
	IsDone func(T) bool
	// IsFailed 判断结果是否表示任务失败，返回非空错误信息即失败（空串表示未失败）。
	IsFailed func(T) string
	// PollInterval 两次轮询之间的间隔。
	PollInterval time.Duration
	// MaxWait 最大等待时间，超时返回错误。
	MaxWait time.Duration
	// RetryIf 自定义重试谓词；nil 时默认按 provider.PollRetryable（幂等 GET 口径）。
	RetryIf func(error) bool
	// Label 日志前缀（如 "Ark"、"Gemini"）。
	Label string
	// OnProgress 每次非终态轮询后回调（已耗时）。
	OnProgress func(T, time.Duration)
}

// PollWithRetry 通用轮询：先查询再等待，已完成/缓存命中的任务立刻返回，不被 interval 白等一轮。
func PollWithRetry[T any](ctx context.Context, cfg PollConfig[T]) (T, error) {
	var zero T
	start := time.Now()
	prefix := ""
	if cfg.Label != "" {
		prefix = cfg.Label + " "
	}
	predicate := cfg.RetryIf
	if predicate == nil {
		predicate = provider.PollRetryable
	}
	for {
		result, err := cfg.PollFn(ctx)
		if err != nil {
			// 用户取消 / 超时不是「上游暂时失败」，直接冒泡。
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return zero, err
			}
			if !predicate(err) {
				return zero, err
			}
			slog.Warn("轮询异常（将重试）", "label", cfg.Label, "error", err)
		} else {
			if msg := cfg.IsFailed(result); msg != "" {
				return zero, errors.New(msg)
			}
			if cfg.IsDone(result) {
				return result, nil
			}
			if cfg.OnProgress != nil {
				cfg.OnProgress(result, time.Since(start))
			}
		}
		if time.Since(start) >= cfg.MaxWait {
			return zero, fmt.Errorf("%s任务超时（%.0f秒）", prefix, cfg.MaxWait.Seconds())
		}
		if err := sleepWithContext(ctx, cfg.PollInterval); err != nil {
			return zero, err
		}
	}
}

// sleepWithContext 可被取消打断的等待。
func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SubmitPost 是 create/提交阶段（非幂等「创建 + 计费」POST）的统一包装。
//
// httpc 已把 >=400 收敛为 *provider.HTTPStatusError、传输错误原样上抛；这里再经
// provider.ClassifySubmitError 把「请求可能已送达」的传输错误包成 *AmbiguousSubmitError
// 终态失败（不重试），连接类失败原样上抛交重试谓词。与 with_retry(retry_if=SubmitRetryable)
// 配套使用。
func SubmitPost(
	ctx context.Context, providerName string, do func(context.Context) (*httpc.Response, error),
) (*httpc.Response, error) {
	resp, err := do(ctx)
	if err != nil {
		return nil, provider.ClassifySubmitError(providerName, err)
	}
	return resp, nil
}

// DownloadVideo 从 provider 签发的 URL 下载视频到本地路径（幂等 GET，自带退避重试）。
//
// 出站形态先校验：非法 URL 是确定性错误，DownloadRetryable 不会把形态错误拖进重试窗口。
func DownloadVideo(ctx context.Context, rawURL, outputPath string, timeout time.Duration) error {
	policy := provider.DownloadRetryPolicy
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	client := httpc.NewClient(timeout)
	return provider.RunWithRetry(ctx, policy, provider.DownloadRetryable, func(ctx context.Context) error {
		validated, err := httpc.ValidateOutboundURL(rawURL)
		if err != nil {
			return err
		}
		resp, err := client.Get(ctx, validated, nil)
		if err != nil {
			return err
		}
		if len(resp.Body) == 0 {
			return fmt.Errorf("downloaded video is empty: %s", validated)
		}
		return writeFileAtomic(outputPath, resp.Body)
	})
}

// PersistProviderJobIDFor 在 submit 成功后立即把 job_id（+提交端点）落库，供重启续跑定位远端 job。
//
// 非 worker 路径（req.TaskID 为空 / 未注入 persister）跳过。端点取值：自定义供应商包装层注入的
// req.ExecutionEndpoint 优先（它是续跑比对协议的依据，不能被下游 backend 的域名顶掉），缺席时落到
// 调用方传入的 endpoint（内置供应商传实际请求域名，供续跑回放）。
//
// 持久化失败原样上抛（由 worker finally 兜底 mark_failed），保持 fail-fast 语义（ADR 0007）。
func PersistProviderJobIDFor(
	ctx context.Context, opts provider.BackendOptions, req provider.VideoGenerationRequest,
	jobID, endpoint string,
) error {
	if opts.PersistJobID == nil || req.TaskID == "" {
		return nil
	}
	effective := req.ExecutionEndpoint
	if effective == "" {
		effective = endpoint
	}
	var endpointPtr *string
	if effective != "" {
		endpointPtr = &effective
	}
	return opts.PersistJobID(ctx, req.TaskID, jobID, endpointPtr)
}
