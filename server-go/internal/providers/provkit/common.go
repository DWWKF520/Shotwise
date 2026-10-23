package provkit

import (
	"encoding/json"
	"fmt"

	"github.com/shotwise/shotwise/internal/provider"
)

// maxJSONErrorContext 是解码失败时回显的响应体片段上限。上游网关常在 200 里塞 HTML 错误页，
// 不回显一段就无从判断是协议变更还是代理拦截；全文回显则可能把用户提示词写进日志。
const maxJSONErrorContext = 256

// DecodeJSON 解析上游响应体；失败时带一段原文，便于诊断网关 / 协议问题。
//
// 解码失败不重试：响应已完整收到，重试只会再收一次同样的畸形体。
func DecodeJSON(body []byte, out any) error {
	if err := json.Unmarshal(body, out); err != nil {
		return &provider.NonRetryableError{Err: fmt.Errorf("decode upstream response: %w (%q)", err, prefix(body))}
	}
	return nil
}

// prefix 截取诊断用前缀。
func prefix(body []byte) string {
	limit := min(len(body), maxJSONErrorContext)
	return string(body[:limit])
}
