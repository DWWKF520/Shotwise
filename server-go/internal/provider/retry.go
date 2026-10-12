package provider

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// 通用重试策略：指数退避 + 随机抖动，不依赖任何供应商 SDK（对应 Python 侧 lib/retry.py）。
//
// 三套重试谓词分别对应提交（非幂等 POST）、轮询（幂等 GET）、下载（幂等 GET）三种调用形态：
// 提交阶段只有「请求确定未送达」的连接类失败可重试，歧义态（可能已送达）直接终态失败；轮询把
// 404 视为「任务短暂未就绪」重试；下载由 provider 直接签发结果 URL，404 是确定性错误，随其余
// 4xx 快速失败。

const (
	// DefaultMaxAttempts 是默认重试次数（生成 / 提交阶段）。
	DefaultMaxAttempts = 3
	// DownloadMaxAttempts 是下载阶段重试次数（比生成阶段更宽容，下载失败不浪费生成额度）。
	DownloadMaxAttempts = 5
	// JitterUpperSeconds 是随机抖动的上界（秒）：等待 = 退避 + U(0, 2)。
	JitterUpperSeconds = 2.0
)

// DefaultBackoffSeconds 是默认退避序列（秒）。
var DefaultBackoffSeconds = []int{2, 4, 8}

// DownloadBackoffSeconds 是下载阶段退避序列（秒）。
var DownloadBackoffSeconds = []int{5, 10, 20, 40}

// RetryableStatusPatterns 覆盖异常类型不在类型列表里但属于瞬态的情况（大小写不敏感子串比对）。
// 仅用于 IsTransientUpstreamError（供应商泳道健康度）；三个重试谓词按错误类型与状态码显式判定，
// 不走文本兜底——异常消息里的 URL/task_id 含 "500"/"503" 子串时会被误判为瞬态。
var RetryableStatusPatterns = []string{
	"429",
	"resource_exhausted",
	"500",
	"502",
	"503",
	"504",
	"internalservererror",
	"internal server error",
	"serviceunavailable",
	"service unavailable",
	"bad gateway",
	"gateway timeout",
	"timed out",
	"timeout",
}

// RetryPolicy 描述一次重试序列的尝试次数与退避。
type RetryPolicy struct {
	MaxAttempts    int
	BackoffSeconds []int
}

// DefaultRetryPolicy 是生成 / 提交阶段的默认策略。
var DefaultRetryPolicy = RetryPolicy{MaxAttempts: DefaultMaxAttempts, BackoffSeconds: DefaultBackoffSeconds}

// DownloadRetryPolicy 是下载阶段的策略。
var DownloadRetryPolicy = RetryPolicy{MaxAttempts: DownloadMaxAttempts, BackoffSeconds: DownloadBackoffSeconds}

// HTTPStatusError 是上游返回 >=400 状态码的错误（对应 httpx.HTTPStatusError）。
// 谓词按 StatusCode 显式闸门判定，不看消息文本。
type HTTPStatusError struct {
	StatusCode int
	Status     string // 形如 "503 Service Unavailable"，可空
	Body       string // 响应体前若干字符，供 413 等诊断；可空
	URL        string
}

func (e *HTTPStatusError) Error() string {
	if e.Status != "" {
		return "HTTP " + e.Status
	}
	return "HTTP status " + strconv.Itoa(e.StatusCode)
}

// LocalProtocolError 是请求字节发出前就确定失败的本地/协议错误（URL scheme 不受支持、
// 本地请求构造违反协议）。既无重复计费风险，重试到超时也不会变好——poll / download 路径
// 快速失败，submit 路径原样抛出（非歧义态，不套「请求可能已送达」）。
type LocalProtocolError struct{ Err error }

func (e *LocalProtocolError) Error() string {
	if e.Err == nil {
		return "local protocol error"
	}
	return "local protocol error: " + e.Err.Error()
}

func (e *LocalProtocolError) Unwrap() error { return e.Err }

// ErrUnsupportedProtocol 表示出站 scheme 不受支持（HTTP 层在发出请求前判定）。
var ErrUnsupportedProtocol = errors.New("unsupported protocol scheme")

// NonRetryableError 是标记包装：命中此类型的异常始终不重试。
//
// 需要绝对不可重试语义的错误（能力不支持、参数非法等）用它包裹，在一切谓词判定之前短路，
// 不走任何后续匹配。
type NonRetryableError struct{ Err error }

func (e *NonRetryableError) Error() string {
	if e.Err == nil {
		return "non-retryable error"
	}
	return e.Err.Error()
}

func (e *NonRetryableError) Unwrap() error { return e.Err }

// IsNonRetryableError 判断错误链上是否有不可重试标记。
func IsNonRetryableError(err error) bool {
	var marker *NonRetryableError
	return errors.As(err, &marker)
}

// IsLocalProtocolError 判断是否本地/协议错误。
func IsLocalProtocolError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUnsupportedProtocol) {
		return true
	}
	var local *LocalProtocolError
	return errors.As(err, &local)
}

// IsDefinitelyNotDelivered 判断传输错误是否属于「请求确定未送达」的连接类子集：连接建立阶段
// 失败 / 从未取得连接 / 拨号失败。重试安全——服务端不可能已建任务 / 已计费。
//
// 读 / 写阶段及之后的传输错误（读超时、写失败、连接中途断开）请求可能已抵达服务端，归歧义态，
// 不在此列。Go 侧连接类失败统一表现为 *net.OpError 且 Op == "dial"（含其包裹的 DNS 解析失败）；
// 对应 Python httpx 的 ConnectError / ConnectTimeout / PoolTimeout / ProxyError。
func IsDefinitelyNotDelivered(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	// 连接被对端拒绝：请求字节从未发出。
	return errors.Is(err, syscall.ECONNREFUSED)
}

// IsRetryableHTTPStatus 按状态码判定是否可重试。
//
// 瞬态错误恒重试：408 / 425 / 429 / 5xx。404 默认快速失败（确定性的「不存在」，如端点拼错）；
// 轮询 / 下载场景传 retryNotFound=true 时按「任务提交后短暂未就绪 / 资源未传播」重试。
// 其余 4xx（400/401/403/422 等）确定性客户端错误一律快速失败——重试只会拖到超时，白占 worker 槽。
func IsRetryableHTTPStatus(statusCode int, retryNotFound bool) bool {
	if statusCode == 408 || statusCode == 425 || statusCode == 429 {
		return true
	}
	if statusCode >= 500 && statusCode <= 599 {
		return true
	}
	if statusCode == 404 {
		return retryNotFound
	}
	return false
}

// SubmitRetryable 是创建 / 提交阶段（非幂等「创建 + 计费」POST）的重试谓词。
//
// 与 PollRetryable 的关键区别：传输错误只重试「请求确定未送达」的连接类子集，重试不会重复
// 建任务、不会重复计费。歧义态（读超时等「请求可能已被服务端处理」，由 ClassifySubmitError
// 包成 AmbiguousSubmitError）终态失败。HTTP 状态错误按状态码闸门：5xx/408/425/429 重试，
// 404 与确定性 4xx 快速失败。
func SubmitRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || IsNonRetryableError(err) {
		return false
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return IsRetryableHTTPStatus(statusErr.StatusCode, false)
	}
	var ambiguous *AmbiguousSubmitError
	if errors.As(err, &ambiguous) {
		return false
	}
	return IsDefinitelyNotDelivered(err)
}

// PollRetryable 是轮询阶段（幂等 GET）的重试谓词。
//
// 幂等查询重试无副作用，故传输 / 网络错误一律重试；唯本地 / 协议错误在请求发出前就确定失败，
// 重试不会变好，快速失败。HTTP 状态错误按状态码闸门，404 视为「任务提交后短暂未就绪 /
// 资源未传播」重试。业务异常一律快速失败。
func PollRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || IsNonRetryableError(err) {
		return false
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return IsRetryableHTTPStatus(statusErr.StatusCode, true)
	}
	if IsLocalProtocolError(err) {
		return false
	}
	return isTransportError(err)
}

// DownloadRetryable 是下载阶段（幂等 GET 取 provider 签发的结果 URL）的重试谓词。
//
// 与 PollRetryable 的唯一区别在 404：下载 URL 由 provider 直接签发，4xx（404 不存在 /
// 403 过期 / 413 等）是确定性错误，重试也不会变好，故 404 随其余 4xx 一并快速失败——
// 轮询的 404 才是「任务短暂未就绪」该重试。5xx/408/425/429 与传输错误重试。
func DownloadRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || IsNonRetryableError(err) {
		return false
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return IsRetryableHTTPStatus(statusErr.StatusCode, false)
	}
	if IsLocalProtocolError(err) {
		return false
	}
	return isTransportError(err)
}

// IsTransientUpstreamError 判断错误是否代表「上游暂时不健康」——与重试判定同源的词汇表。
//
// 供应商泳道健康度直接复用这份词汇表：「值得重试」与「算上游不健康」是同一个判断，分两份实现
// 迟早漂移。NonRetryableError 子类（能力不支持、参数非法等）恒为 false；用户取消不是上游不
// 健康信号，同样恒为 false。
func IsTransientUpstreamError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || IsNonRetryableError(err) {
		return false
	}
	if isTransportError(err) {
		return true
	}
	lower := strings.ToLower(err.Error())
	for _, pattern := range RetryableStatusPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// ClassifySubmitError 把提交阶段（非幂等 POST）的传输错误分流，等价 Python 的 submit_post 包装：
//
//   - 本地 / 协议错误：请求发出前就确定失败、无计费风险 → 原样返回，由谓词快速失败；
//   - 连接类失败（dial）：请求确定未送达 → 原样返回，交 SubmitRetryable 重试；
//   - 其余传输错误（读超时 / 写失败 / 连接中途断开）：请求可能已被服务端处理 → 包成
//     AmbiguousSubmitError 终态失败，不重试，避免重复建任务 + 重复计费；
//   - 非传输错误（HTTP 状态错误、业务错误、用户取消）原样返回。
func ClassifySubmitError(provider string, err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	if IsLocalProtocolError(err) || IsDefinitelyNotDelivered(err) {
		return err
	}
	if isTransportError(err) {
		return &AmbiguousSubmitError{Provider: provider, Err: err}
	}
	return err
}

// isTransportError 判断错误是否来自传输层（请求可能已发出）。net/http 把所有传输错误包在
// *url.Error 里，底层多为 net.Error；另有连接重置 / 管道破裂与裸 EOF 形态。
// context.Canceled 不是传输失败，调用方须先排除。
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)
}

// WaitForAttempt 计算第 attempt 次重试（0-based）的等待时长：backoff 序列按 attempt 取，
// 超出序列长度时取最后一项，再加 U(0, JitterUpperSeconds) 的抖动。
// rnd 为 nil 时用全局随机源（goroutine 安全）。
func WaitForAttempt(attempt int, backoffSeconds []int, rnd *rand.Rand) time.Duration {
	backoff := 0
	if len(backoffSeconds) > 0 {
		idx := attempt
		if idx >= len(backoffSeconds) {
			idx = len(backoffSeconds) - 1
		}
		if idx < 0 {
			idx = 0
		}
		backoff = backoffSeconds[idx]
	}
	jitter := rand.Float64()
	if rnd != nil {
		jitter = rnd.Float64()
	}
	return time.Duration((float64(backoff) + jitter*JitterUpperSeconds) * float64(time.Second))
}

// RunWithRetry 按 policy 重试 fn：最后一次失败、NonRetryableError、谓词拒绝的错误与
// context 取消立即返回；每次重试前按退避 + 抖动等待（可被 ctx 取消）。
// 对应 Python 的 with_retry_async 装饰器。
func RunWithRetry(ctx context.Context, policy RetryPolicy, retryIf func(error) bool, fn func(ctx context.Context) error) error {
	var lastErr error
	for attempt := 0; attempt < policy.MaxAttempts; attempt++ {
		lastErr = fn(ctx)
		if lastErr == nil {
			return nil
		}
		isLast := attempt >= policy.MaxAttempts-1
		if isLast || errors.Is(lastErr, context.Canceled) || IsNonRetryableError(lastErr) ||
			(retryIf != nil && !retryIf(lastErr)) {
			return lastErr
		}
		timer := time.NewTimer(WaitForAttempt(attempt, policy.BackoffSeconds, nil))
		select {
		case <-ctx.Done():
			timer.Stop()
			return lastErr
		case <-timer.C:
		}
	}
	return lastErr
}
