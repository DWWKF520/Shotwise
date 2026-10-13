package httpc

import (
	"errors"
	"net/url"
	"strings"
)

// 出站 URL 的统一形态校验（端口自 lib/outbound_url.py）。
//
// 探针、模型发现、声明式运行时请求与产物下载在真正发起 HTTP 之前都经过这里，避免同一份用户
// 配置在不同入口得到不同结论（「发现模型能过、真跑生成被拒」）。
//
// 刻意不限制私网 / 环回地址：自建服务、局域网中转与 http://localhost 上的本地网关都是受支持的
// 端点形态。本层只做形态校验——scheme / host / userinfo / 端口 / query / fragment——把非法形态
// 挡在 net/http 之前，不代替用户判断某个地址是否可信。

// ErrOutboundURL 是出站地址形态非法。消息为固定文案，不回显输入：地址可能内嵌凭证
// （用户把 key 拼进 URL），原文外流会把凭证写进日志与接口响应。
var ErrOutboundURL = errors.New("outbound url is not a parsable absolute http(s) URL")

// 本包只在发出请求前校验 scheme，这里补齐 Python 侧更严格的形态校验。

// ValidateOutboundURL 校验产物下载 / 已拼好路径的出站 URL，返回去空白后的原值。
//
// query 放行：供应商签发的产物地址通常把签名参数挂在 query 上，收窄会导致下载断裂。
// fragment 仍拦——它在链路中不可见，只会让「存储值即调用值」出现无意义的偏差。
func ValidateOutboundURL(raw string) (string, error) {
	return validateOutbound(raw, true)
}

// ValidateOutboundBaseURL 校验用户配置的端点根（base_url），返回去空白后的原值。
//
// 比 ValidateOutboundURL 多拦 query：调用方按字符串拼接路径，带 query 会让拼出的地址与
// 「存储值即调用值」分叉（探测通过但运行时 404）。
func ValidateOutboundBaseURL(raw string) (string, error) {
	return validateOutbound(raw, false)
}

func validateOutbound(raw string, allowQuery bool) (string, error) {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return "", ErrOutboundURL
	}
	// 逐字符拦而不只看 url.Parse 的 query / fragment 是否非空：空 query 的
	// "https://x/a?" 同样会让拼接结果与存储值分叉。
	if strings.Contains(candidate, "#") {
		return "", ErrOutboundURL
	}
	if !allowQuery && strings.Contains(candidate, "?") {
		return "", ErrOutboundURL
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return "", ErrOutboundURL
	}
	if parsed.User != nil {
		return "", ErrOutboundURL
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", ErrOutboundURL
	}
	if parsed.Hostname() == "" {
		return "", ErrOutboundURL
	}
	return candidate, nil
}
