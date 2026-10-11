// Package i18n 提供面向用户的文案渲染。
//
// 关键约束：业务代码只传 key + params，绝不硬编码文案。
// 面向 agent 的字符串（工具返回、日志、内部异常）不走这里。
package i18n

import (
	"fmt"
	"net/http"
	"strings"
)

// Locale 支持的语言。与 Python 侧 zh/en/vi 三语保持一致。
type Locale string

const (
	ZH Locale = "zh"
	EN Locale = "en"
	VI Locale = "vi"
)

// bundles 是内置文案表。迁移期按需增量补充，key 与 Python 侧 i18n 表同名。
var bundles = map[Locale]map[string]string{
	ZH: {
		"internal_server_error":  "服务器内部错误",
		"resource_not_found":     "资源不存在",
		"not_found":              "资源不存在",
		"unauthorized":           "未认证",
		"forbidden":              "无权限",
		"invalid_credentials":    "用户名或密码错误",
		"project_not_found":      "项目不存在",
		"project_already_exists": "项目已存在",
		"invalid_project_name":   "项目名只能包含字母、数字和连字符",
		"invalid_request_body":   "请求体格式错误",
		"feature_disabled":       "功能未开启：{feature}",
		"validation_failed":      "参数校验失败",
	},
	EN: {
		"internal_server_error":  "Internal server error",
		"resource_not_found":     "Resource not found",
		"not_found":              "Resource not found",
		"unauthorized":           "Not authenticated",
		"forbidden":              "Permission denied",
		"invalid_credentials":    "Invalid username or password",
		"project_not_found":      "Project not found",
		"project_already_exists": "Project already exists",
		"invalid_project_name":   "Project name may only contain letters, digits and hyphens",
		"invalid_request_body":   "Invalid request body",
		"feature_disabled":       "Feature disabled: {feature}",
		"validation_failed":      "Validation failed",
	},
	VI: {
		"internal_server_error":  "Lỗi máy chủ nội bộ",
		"resource_not_found":     "Không tìm thấy tài nguyên",
		"not_found":              "Không tìm thấy tài nguyên",
		"unauthorized":           "Chưa xác thực",
		"forbidden":              "Không có quyền",
		"invalid_credentials":    "Tên người dùng hoặc mật khẩu không đúng",
		"project_not_found":      "Không tìm thấy dự án",
		"project_already_exists": "Dự án đã tồn tại",
		"invalid_project_name":   "Tên dự án chỉ được chứa chữ cái, chữ số và dấu gạch nối",
		"invalid_request_body":   "Nội dung yêu cầu không hợp lệ",
		"feature_disabled":       "Tính năng chưa bật: {feature}",
		"validation_failed":      "Xác thực không thành công",
	},
}

// FromRequest 按 Accept-Language 解析请求语言，未知一律回落 zh。
func FromRequest(r *http.Request) Locale {
	header := r.Header.Get("Accept-Language")
	for _, part := range strings.Split(header, ",") {
		tag := strings.TrimSpace(part)
		if i := strings.Index(tag, ";"); i >= 0 {
			tag = strings.TrimSpace(tag[:i])
		}
		switch strings.ToLower(tag) {
		case "en", "en-us", "en-gb":
			return EN
		case "vi", "vi-vn":
			return VI
		case "zh", "zh-cn", "zh-hans", "zh-tw", "zh-hk":
			return ZH
		}
	}
	return ZH
}

// T 渲染文案。占位符形如 {name}，未提供的占位符原样保留（便于发现漏传）。
func T(loc Locale, key string, params map[string]any) string {
	tmpl, ok := bundles[loc][key]
	if !ok {
		if tmpl, ok = bundles[ZH][key]; !ok {
			return key
		}
	}
	for k, v := range params {
		tmpl = strings.ReplaceAll(tmpl, "{"+k+"}", fmtString(v))
	}
	return tmpl
}

func fmtString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case error:
		return t.Error()
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}
