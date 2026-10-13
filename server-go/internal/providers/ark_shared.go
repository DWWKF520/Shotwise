package providers

import (
	"os"
	"strings"
)

// Ark（火山方舟）图片 / 视频 backend 的共享常量与工具。
//
// 端口自 lib/ark_shared.py 的模块级常量与客户端工厂：两家供货形态同源（同一基址、同一 Bearer
// 鉴权、同一缺省域名兜底）。key 与 ark-agent-plan 两个 registry 键共用同一份实现，二者差异
// （默认域名走 /api/plan/v3）由装配层经 BackendOptions.BaseURL 传入的取值体现，本文件不区分。

const (
	// arkProvider 是对外声明的供应商标识，与 registry 的 provider_id 同名。
	arkProvider = "ark"
	// arkDefaultBaseURL 是 base_url 缺省时的火山方舟国内站端点（即 /api/v3）。
	arkDefaultBaseURL = "https://ark.cn-beijing.volces.com/api/v3"
)

// arkAuthHeaders 构造 Ark 的 Bearer 鉴权头。
func arkAuthHeaders(apiKey string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + apiKey}
}

// arkEffectiveBaseURL 解析生效基址：留空回落 arkDefaultBaseURL，否则去尾斜杠。
// 去尾斜杠避免与 JoinURL 拼接出双斜杠。
func arkEffectiveBaseURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return arkDefaultBaseURL
	}
	return strings.TrimRight(trimmed, "/")
}

// arkFileExists 判断路径是否为一个已存在的普通文件（对应 Python Path.is_file()）。
// 目录与符号链接失配一律按不存在处理——缺失素材要 fail loud，不能静默跳过。
func arkFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
