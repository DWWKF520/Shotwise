// Package httpc 是出网 HTTP 的薄封装。
//
// 存在的理由不是「省几行 net/http」，而是把出站错误的分类收口在一处：连接类失败（确定未送达，
// 可重试）与读/写阶段失败（请求可能已送达，属歧义态）在传输层长得几乎一样，靠 provider 的
// 重试谓词按类型判别。每个 backend 各自猜一次的后果就是同一类故障在不同供应商下有的重试、
// 有的重复计费。本层只做三件事：scheme 白名单、状态码 → *provider.HTTPStatusError、其余传输
// 错误原样上抛。
package httpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/provider"
)

// maxDiagnosticsBody 是错误信息里携带的响应体片段上限（供 413 / 能力类错误诊断，
// 不做全文回显：上游报错体可能很长且含用户提示词）。
const maxDiagnosticsBody = 512

// Response 是一次出站调用的结果。Body 已完整读入内存（生成接口的响应体都是小载荷；
// 大文件下载走 DownloadTo）。
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// FilePart 是 multipart 表单里的一个文件字段。
type FilePart struct {
	FieldName string
	FileName  string
	Content   []byte
}

// Client 持有一个 http.Client。零值不可用，用 NewClient 构造。
type Client struct {
	HTTP *http.Client
}

// NewClient 构造客户端；timeout <= 0 表示不设整体超时（调用方自行用 context 控时）。
func NewClient(timeout time.Duration) *Client {
	return &Client{HTTP: &http.Client{Timeout: timeout}}
}

// Do 执行请求并把错误分类成重试谓词认得出的形态。
//
// 传输错误原样上抛（provider.IsDefinitelyNotDelivered 据此区分「确定未送达」）；
// 上游 >=400 统一包成 *provider.HTTPStatusError，谓词按状态码判定，不看消息文本。
func (c *Client) Do(req *http.Request) (*Response, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// 响应头已收到、体读取中断：请求已送达服务端，属歧义态，原样上抛不伪装成状态错误。
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &provider.HTTPStatusError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       truncate(string(body), maxDiagnosticsBody),
			URL:        req.URL.String(),
		}
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// PostJSON 发送 JSON 请求体。headers 通常只有 Authorization / Content-Type。
func (c *Client) PostJSON(ctx context.Context, target string, headers map[string]string, payload any) (*Response, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, &provider.LocalProtocolError{Err: fmt.Errorf("marshal request: %w", err)}
	}
	req, err := newRequest(ctx, http.MethodPost, target, headers, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.Do(req)
}

// Get 发起幂等 GET（轮询 / 下载前的探测）。
func (c *Client) Get(ctx context.Context, target string, headers map[string]string) (*Response, error) {
	req, err := newRequest(ctx, http.MethodGet, target, headers, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// PostMultipart 发送 multipart/form-data（图片编辑等带文件的接口）。
func (c *Client) PostMultipart(
	ctx context.Context, target string, headers map[string]string,
	fields map[string]string, files []FilePart,
) (*Response, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for _, file := range files {
		part, err := writer.CreateFormFile(file.FieldName, file.FileName)
		if err != nil {
			return nil, fmt.Errorf("create form file: %w", err)
		}
		if _, err := part.Write(file.Content); err != nil {
			return nil, fmt.Errorf("write form file: %w", err)
		}
	}
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return nil, fmt.Errorf("write form field: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close multipart: %w", err)
	}
	req, err := newRequest(ctx, http.MethodPost, target, headers, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return c.Do(req)
}

// newRequest 构造请求并在发出前校验 scheme：非 http/https 一律本地协议错误，
// 不进网（provider.ErrUnsupportedProtocol 供重试谓词快速失败）。
func newRequest(ctx context.Context, method, target string, headers map[string]string, body io.Reader) (*http.Request, error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, &provider.LocalProtocolError{Err: fmt.Errorf("%w: %q", provider.ErrUnsupportedProtocol, target)}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, &provider.LocalProtocolError{Err: fmt.Errorf("%w: %q", provider.ErrUnsupportedProtocol, parsed.Scheme)}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, &provider.LocalProtocolError{Err: err}
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return req, nil
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// IsRetryableStatus 是给调用方的便捷判定：该状态码是否值得重试（404 由调用方按场景决定）。
func IsRetryableStatus(statusCode int, retryNotFound bool) bool {
	return provider.IsRetryableHTTPStatus(statusCode, retryNotFound)
}

// JoinURL 拼接 base 与路径，避免手工拼串出现双斜杠或缺斜杠。
func JoinURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	if path == "" {
		return base
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}
