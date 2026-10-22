// 发布平台适配器契约与能力声明（对应 server/services/publishing_adapters.py）。
//
// 这里不实现任何网络客户端：内置注册表刻意留空，直到官方平台 API、应用凭据与
// 授权范围确认后才注册真实适配器；在适配器接入前，调用方 fail closed。
package service

import "strings"

// PlatformCapabilities 是单个平台的能力声明（JSON 字段名与 Python asdict 对齐）。
type PlatformCapabilities struct {
	Platform              string  `json:"platform"`
	DisplayName           string  `json:"display_name"`
	AdapterConnected      bool    `json:"adapter_connected"`
	SupportsOAuth         bool    `json:"supports_oauth"`
	SupportsPublish       bool    `json:"supports_publish"`
	SupportsSchedule      bool    `json:"supports_schedule"`
	SupportsStatusPolling bool    `json:"supports_status_polling"`
	SupportsRetract       bool    `json:"supports_retract"`
	SupportsCoverUpdate   bool    `json:"supports_cover_update"`
	UnavailableReason     *string `json:"unavailable_reason"`
}

// publishingPlatformOrder 固定能力列表的输出顺序（Python dict 保留插入序）。
var publishingPlatformOrder = []string{"douyin", "hongguo"}

var publishingCapabilities = map[string]PlatformCapabilities{
	"douyin": {
		Platform:          "douyin",
		DisplayName:       "抖音",
		UnavailableReason: publishReasonPtr("official_adapter_not_connected"),
	},
	"hongguo": {
		Platform:          "hongguo",
		DisplayName:       "红果",
		UnavailableReason: publishReasonPtr("official_adapter_not_connected"),
	},
}

func publishReasonPtr(value string) *string { return &value }

// ListPlatformCapabilities 按固定顺序返回全部平台能力声明。
func ListPlatformCapabilities() []PlatformCapabilities {
	out := make([]PlatformCapabilities, 0, len(publishingPlatformOrder))
	for _, platform := range publishingPlatformOrder {
		out = append(out, publishingCapabilities[platform])
	}
	return out
}

// GetPlatformCapabilities 按归一化平台名取能力声明；未登记返回 (zero, false)。
func GetPlatformCapabilities(platform string) (PlatformCapabilities, bool) {
	capability, ok := publishingCapabilities[normalizePublishingPlatform(platform)]
	return capability, ok
}

func normalizePublishingPlatform(platform string) string {
	return strings.ToLower(strings.TrimSpace(platform))
}
