package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/url"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRetryConstants(t *testing.T) {
	if DefaultMaxAttempts != 3 || DownloadMaxAttempts != 5 || JitterUpperSeconds != 2.0 {
		t.Fatalf("重试常量异常：%d %d %v", DefaultMaxAttempts, DownloadMaxAttempts, JitterUpperSeconds)
	}
	if !reflect.DeepEqual(DefaultBackoffSeconds, []int{2, 4, 8}) {
		t.Fatalf("默认退避序列 = %v", DefaultBackoffSeconds)
	}
	if !reflect.DeepEqual(DownloadBackoffSeconds, []int{5, 10, 20, 40}) {
		t.Fatalf("下载退避序列 = %v", DownloadBackoffSeconds)
	}
	if DefaultRetryPolicy.MaxAttempts != DefaultMaxAttempts ||
		!reflect.DeepEqual(DefaultRetryPolicy.BackoffSeconds, DefaultBackoffSeconds) {
		t.Fatalf("默认策略 = %+v", DefaultRetryPolicy)
	}
	if DownloadRetryPolicy.MaxAttempts != DownloadMaxAttempts ||
		!reflect.DeepEqual(DownloadRetryPolicy.BackoffSeconds, DownloadBackoffSeconds) {
		t.Fatalf("下载策略 = %+v", DownloadRetryPolicy)
	}
	if len(RetryableStatusPatterns) != 14 {
		t.Fatalf("瞬态文本模式应为 14 条，实得 %d：%v", len(RetryableStatusPatterns), RetryableStatusPatterns)
	}
}

func TestWaitForAttempt(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	cases := []struct {
		name    string
		attempt int
		backoff []int
		lo, hi  int // 秒区间 [lo, hi)，hi = 退避 + 抖动上界
	}{
		{"首试", 0, DefaultBackoffSeconds, 2, 4},
		{"第二次", 1, DefaultBackoffSeconds, 4, 6},
		{"第三次", 2, DefaultBackoffSeconds, 8, 10},
		{"超出序列取最后一项", 5, DefaultBackoffSeconds, 8, 10},
		{"负下标钳到首项", -1, DefaultBackoffSeconds, 2, 4},
		{"下载首试", 0, DownloadBackoffSeconds, 5, 7},
		{"下载末次", 3, DownloadBackoffSeconds, 40, 42},
		{"空序列只有抖动", 0, nil, 0, 2},
	}
	for _, c := range cases {
		got := WaitForAttempt(c.attempt, c.backoff, rnd)
		if got < time.Duration(c.lo)*time.Second || got >= time.Duration(c.hi)*time.Second {
			t.Errorf("%s: WaitForAttempt = %v，期望落在 [%ds, %ds)", c.name, got, c.lo, c.hi)
		}
	}
	// 同一随机源下两次调用应给出不同抖动但都落在界内（抖动确实在起作用）。
	first := WaitForAttempt(0, DefaultBackoffSeconds, rand.New(rand.NewSource(1)))
	second := WaitForAttempt(0, DefaultBackoffSeconds, rand.New(rand.NewSource(2)))
	if first == second {
		t.Log("不同种子给出相同等待（概率极低但非错误）")
	}
}

func TestIsRetryableHTTPStatus(t *testing.T) {
	cases := []struct {
		status        int
		retryNotFound bool
		want          bool
	}{
		{200, false, false},
		{400, false, false},
		{401, false, false},
		{403, false, false},
		{404, false, false},
		{404, true, true},
		{408, false, true},
		{422, false, false},
		{425, false, true},
		{429, false, true},
		{499, false, false},
		{500, false, true},
		{502, false, true},
		{503, false, true},
		{504, false, true},
		{599, false, true},
		{600, false, false},
	}
	for _, c := range cases {
		if got := IsRetryableHTTPStatus(c.status, c.retryNotFound); got != c.want {
			t.Errorf("IsRetryableHTTPStatus(%d, %v) = %v，期望 %v", c.status, c.retryNotFound, got, c.want)
		}
	}
}

func transportFixtures() (dialErr, readErr, resetErr, eofErr error) {
	dialErr = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	readErr = &url.Error{Op: "Post", URL: "https://api.example.com/v1", Err: &net.OpError{
		Op: "read", Net: "tcp", Err: errors.New("i/o timeout"),
	}}
	resetErr = &url.Error{Op: "Get", URL: "https://cdn.example.com/x", Err: syscall.ECONNRESET}
	eofErr = io.ErrUnexpectedEOF
	return
}

func TestIsDefinitelyNotDelivered(t *testing.T) {
	dialErr, readErr, _, _ := transportFixtures()
	dnsErr := &net.DNSError{Err: "no such host", Name: "api.example.com"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"拨号失败", dialErr, true},
		{"DNS 解析失败", dnsErr, true},
		{"连接被拒绝", fmt.Errorf("connect: %w", syscall.ECONNREFUSED), true},
		{"读超时（请求可能已送达）", readErr, false},
		{"本地协议错误", &LocalProtocolError{Err: ErrUnsupportedProtocol}, false},
		{"普通错误", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := IsDefinitelyNotDelivered(c.err); got != c.want {
			t.Errorf("%s: IsDefinitelyNotDelivered = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestSubmitRetryable(t *testing.T) {
	dialErr, readErr, _, _ := transportFixtures()
	dnsErr := &net.DNSError{Err: "no such host", Name: "api.example.com"}
	ambiguous := &AmbiguousSubmitError{Provider: "ark", Err: readErr}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"用户取消", context.Canceled, false},
		{"包裹的用户取消", fmt.Errorf("任务取消：%w", context.Canceled), false},
		{"不可重试标记", &NonRetryableError{Err: dialErr}, false},
		{"拨号失败可重试（请求确定未送达）", dialErr, true},
		{"DNS 失败可重试", dnsErr, true},
		{"连接被拒绝可重试", fmt.Errorf("connect: %w", syscall.ECONNREFUSED), true},
		{"读超时不可重试（歧义态）", readErr, false},
		{"裸 EOF 不可重试", io.EOF, false},
		{"歧义态封装即使包着可重试错误也终态失败", ambiguous, false},
		{"HTTP 500 可重试", &HTTPStatusError{StatusCode: 500}, true},
		{"HTTP 502 可重试", &HTTPStatusError{StatusCode: 502}, true},
		{"HTTP 503 可重试", &HTTPStatusError{StatusCode: 503}, true},
		{"HTTP 504 可重试", &HTTPStatusError{StatusCode: 504}, true},
		{"HTTP 408 可重试", &HTTPStatusError{StatusCode: 408}, true},
		{"HTTP 425 可重试", &HTTPStatusError{StatusCode: 425}, true},
		{"HTTP 429 可重试", &HTTPStatusError{StatusCode: 429}, true},
		{"HTTP 404 快速失败（创建端点不存在）", &HTTPStatusError{StatusCode: 404}, false},
		{"HTTP 400 快速失败", &HTTPStatusError{StatusCode: 400}, false},
		{"HTTP 401 快速失败", &HTTPStatusError{StatusCode: 401}, false},
		{"HTTP 403 快速失败", &HTTPStatusError{StatusCode: 403}, false},
		{"HTTP 422 快速失败", &HTTPStatusError{StatusCode: 422}, false},
		{"本地协议错误快速失败", &LocalProtocolError{Err: ErrUnsupportedProtocol}, false},
		{"业务错误快速失败", errors.New("prompt 不合规"), false},
	}
	for _, c := range cases {
		if got := SubmitRetryable(c.err); got != c.want {
			t.Errorf("%s: SubmitRetryable = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestPollRetryable(t *testing.T) {
	dialErr, readErr, resetErr, eofErr := transportFixtures()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"用户取消", context.Canceled, false},
		{"不可重试标记", &NonRetryableError{Err: readErr}, false},
		{"拨号失败可重试", dialErr, true},
		{"读超时可重试（幂等 GET）", readErr, true},
		{"连接重置可重试", resetErr, true},
		{"意外 EOF 可重试", eofErr, true},
		{"HTTP 404 可重试（任务短暂未就绪）", &HTTPStatusError{StatusCode: 404}, true},
		{"HTTP 500 可重试", &HTTPStatusError{StatusCode: 500}, true},
		{"HTTP 429 可重试", &HTTPStatusError{StatusCode: 429}, true},
		{"HTTP 400 快速失败", &HTTPStatusError{StatusCode: 400}, false},
		{"HTTP 401 快速失败", &HTTPStatusError{StatusCode: 401}, false},
		{"本地协议错误快速失败（重试不会变好）", &LocalProtocolError{Err: ErrUnsupportedProtocol}, false},
		{"不支持的 scheme 直接判定为本地错误", ErrUnsupportedProtocol, false},
		{"业务错误快速失败", errors.New("任务已过期"), false},
	}
	for _, c := range cases {
		if got := PollRetryable(c.err); got != c.want {
			t.Errorf("%s: PollRetryable = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestDownloadRetryable(t *testing.T) {
	dialErr, readErr, resetErr, _ := transportFixtures()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"用户取消", context.Canceled, false},
		{"不可重试标记", &NonRetryableError{Err: readErr}, false},
		{"HTTP 404 快速失败（provider 签发的 URL 不存在）", &HTTPStatusError{StatusCode: 404}, false},
		{"HTTP 403 快速失败（签名过期）", &HTTPStatusError{StatusCode: 403}, false},
		{"HTTP 413 快速失败", &HTTPStatusError{StatusCode: 413}, false},
		{"HTTP 503 可重试", &HTTPStatusError{StatusCode: 503}, true},
		{"HTTP 429 可重试", &HTTPStatusError{StatusCode: 429}, true},
		{"读超时可重试（幂等 GET）", readErr, true},
		{"连接重置可重试", resetErr, true},
		{"拨号失败可重试", dialErr, true},
		{"本地协议错误快速失败", &LocalProtocolError{Err: ErrUnsupportedProtocol}, false},
		{"业务错误快速失败", errors.New("文件校验失败"), false},
	}
	for _, c := range cases {
		if got := DownloadRetryable(c.err); got != c.want {
			t.Errorf("%s: DownloadRetryable = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestIsLocalProtocolError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"不支持的 scheme", ErrUnsupportedProtocol, true},
		{"包裹的不支持 scheme", fmt.Errorf("出站失败：%w", ErrUnsupportedProtocol), true},
		{"本地协议错误包装", &LocalProtocolError{Err: errors.New("bad request line")}, true},
		{"普通传输错误", &net.OpError{Op: "dial"}, false},
	}
	for _, c := range cases {
		if got := IsLocalProtocolError(c.err); got != c.want {
			t.Errorf("%s: IsLocalProtocolError = %v，期望 %v", c.name, got, c.want)
		}
	}
	if !IsNonRetryableError(&NonRetryableError{Err: errors.New("x")}) {
		t.Fatal("不可重试标记判定失效")
	}
	if IsNonRetryableError(errors.New("x")) {
		t.Fatal("普通错误不应被判定为不可重试标记")
	}
}

func TestIsTransientUpstreamError(t *testing.T) {
	dialErr, readErr, _, _ := transportFixtures()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"用户取消不是上游不健康信号", context.Canceled, false},
		{"不可重试标记恒 false", &NonRetryableError{Err: errors.New("503")}, false},
		{"传输错误", readErr, true},
		{"拨号失败", dialErr, true},
		{"消息含 503", errors.New("upstream returned 503"), true},
		{"消息含 resource_exhausted", errors.New("RESOURCE_EXHAUSTED: quota"), true},
		{"消息含 service unavailable", errors.New("Service Unavailable"), true},
		{"消息含 timed out", errors.New("connection timed out"), true},
		{"消息含 timeout", errors.New("request timeout"), true},
		{"消息含 bad gateway", errors.New("502 Bad Gateway"), true},
		{"消息含 internal server error", errors.New("INTERNAL SERVER ERROR"), true},
		{"无瞬态迹象", errors.New("prompt 不合规"), false},
	}
	for _, c := range cases {
		if got := IsTransientUpstreamError(c.err); got != c.want {
			t.Errorf("%s: IsTransientUpstreamError = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestClassifySubmitError(t *testing.T) {
	dialErr, readErr, _, _ := transportFixtures()
	localErr := &LocalProtocolError{Err: ErrUnsupportedProtocol}
	statusErr := &HTTPStatusError{StatusCode: 503}

	if got := ClassifySubmitError("ark", nil); got != nil {
		t.Fatalf("nil 应原样返回：%v", got)
	}
	if got := ClassifySubmitError("ark", context.Canceled); got != context.Canceled {
		t.Fatalf("用户取消应原样返回：%v", got)
	}
	if got := ClassifySubmitError("ark", dialErr); got != dialErr {
		t.Fatalf("连接类失败应原样返回（交谓词重试）：%v", got)
	}
	if got := ClassifySubmitError("ark", localErr); got != localErr {
		t.Fatalf("本地协议错误应原样返回：%v", got)
	}
	if got := ClassifySubmitError("ark", statusErr); got != statusErr {
		t.Fatalf("HTTP 状态错误应原样返回：%v", got)
	}
	business := errors.New("参数非法")
	if got := ClassifySubmitError("ark", business); got != business {
		t.Fatalf("业务错误应原样返回：%v", got)
	}

	classified := ClassifySubmitError("ark", readErr)
	var ambiguous *AmbiguousSubmitError
	if !errors.As(classified, &ambiguous) {
		t.Fatalf("读超时应包成 AmbiguousSubmitError，实得 %T：%v", classified, classified)
	}
	if ambiguous.Provider != "ark" || ambiguous.Err != readErr {
		t.Fatalf("歧义态字段错误：%+v", ambiguous)
	}
	if SubmitRetryable(classified) {
		t.Fatal("歧义态终态失败，绝不重试")
	}
	// 裸 EOF 同样归歧义态（可能已送达）。
	if classified := ClassifySubmitError("ark", io.EOF); !errors.As(classified, &ambiguous) {
		t.Fatalf("裸 EOF 应归歧义态：%v", classified)
	}
}

func TestHTTPStatusErrorFormatting(t *testing.T) {
	withStatus := &HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}
	if withStatus.Error() != "HTTP 503 Service Unavailable" {
		t.Fatalf("Error() = %q", withStatus.Error())
	}
	withoutStatus := &HTTPStatusError{StatusCode: 404}
	if withoutStatus.Error() != "HTTP status 404" {
		t.Fatalf("Error() = %q", withoutStatus.Error())
	}
}

func TestRunWithRetry(t *testing.T) {
	// 零等待策略：负退避使定时器立即触发（抖动为正也盖不过），测试不实际睡眠。
	fast := func(maxAttempts int) RetryPolicy {
		return RetryPolicy{MaxAttempts: maxAttempts, BackoffSeconds: []int{-10}}
	}

	t.Run("失败后成功", func(t *testing.T) {
		attempts := 0
		err := RunWithRetry(context.Background(), fast(3), func(error) bool { return true },
			func(context.Context) error {
				attempts++
				if attempts < 2 {
					return errors.New("临时失败")
				}
				return nil
			})
		if err != nil || attempts != 2 {
			t.Fatalf("err = %v，attempts = %d", err, attempts)
		}
	})

	t.Run("全失败返回最后一次错误", func(t *testing.T) {
		attempts := 0
		last := errors.New("最后一次")
		err := RunWithRetry(context.Background(), fast(3), func(error) bool { return true },
			func(context.Context) error {
				attempts++
				if attempts == 3 {
					return last
				}
				return errors.New("临时失败")
			})
		if err != last || attempts != 3 {
			t.Fatalf("err = %v，attempts = %d", err, attempts)
		}
	})

	t.Run("谓词拒绝不重试", func(t *testing.T) {
		attempts := 0
		rejected := errors.New("业务错误")
		err := RunWithRetry(context.Background(), fast(3), func(error) bool { return false },
			func(context.Context) error { attempts++; return rejected })
		if err != rejected || attempts != 1 {
			t.Fatalf("err = %v，attempts = %d", err, attempts)
		}
	})

	t.Run("不可重试标记不重试", func(t *testing.T) {
		attempts := 0
		err := RunWithRetry(context.Background(), fast(3), func(error) bool { return true },
			func(context.Context) error {
				attempts++
				return &NonRetryableError{Err: errors.New("能力不支持")}
			})
		if !IsNonRetryableError(err) || attempts != 1 {
			t.Fatalf("err = %v，attempts = %d", err, attempts)
		}
	})

	t.Run("用户取消不重试", func(t *testing.T) {
		attempts := 0
		err := RunWithRetry(context.Background(), fast(3), func(error) bool { return true },
			func(context.Context) error { attempts++; return context.Canceled })
		if !errors.Is(err, context.Canceled) || attempts != 1 {
			t.Fatalf("err = %v，attempts = %d", err, attempts)
		}
	})

	t.Run("取消的上下文中断等待", func(t *testing.T) {
		// 正退避 + 已取消的 ctx：首试失败后等待分支立即被 ctx 打断。
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		attempts := 0
		last := errors.New("临时失败")
		err := RunWithRetry(ctx, RetryPolicy{MaxAttempts: 3, BackoffSeconds: []int{10}},
			func(error) bool { return true },
			func(context.Context) error { attempts++; return last })
		if err != last || attempts != 1 {
			t.Fatalf("err = %v，attempts = %d", err, attempts)
		}
	})

	t.Run("单次尝试上限", func(t *testing.T) {
		attempts := 0
		err := RunWithRetry(context.Background(), fast(1), func(error) bool { return true },
			func(context.Context) error { attempts++; return errors.New("x") })
		if err == nil || attempts != 1 {
			t.Fatalf("err = %v，attempts = %d", err, attempts)
		}
	})
}

func TestRetryableStatusPatternsAreLowercase(t *testing.T) {
	for _, pattern := range RetryableStatusPatterns {
		if pattern != strings.ToLower(pattern) {
			t.Errorf("模式 %q 应为小写（判定前统一转小写比对）", pattern)
		}
	}
	joined := strings.Join(RetryableStatusPatterns, ",")
	for _, want := range []string{"429", "500", "502", "503", "504", "resource_exhausted", "timed out"} {
		if !strings.Contains(joined, want) {
			t.Errorf("模式表缺 %q", want)
		}
	}
}
