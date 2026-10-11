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

		"asset_invalid_name": "资产名不合法：{name}",
		// 关闭声音过渡横幅时用的确认戳已过期：横幅渲染后声音又被更新过。
		"asset_voice_notice_dismissed_at_stale": "声音已再次更新，请刷新后重新确认",
		"asset_rename_file_conflict":            "改名目标文件已存在：{filename}",
		"asset_rename_rejected":                 "资产改名未通过结构校验，未做任何改动：{name}",

		"character_already_exists": "角色已存在：{name}",
		"character_not_found":      "角色不存在：{name}",
		"character_deleted":        "角色已删除：{name}",

		// scene 用历史前缀 project_scene_*，与 Python 侧 _I18N_KEYS 一致，前端按此出文案。
		"project_scene_already_exists": "场景已存在：{name}",
		"project_scene_not_found":      "场景不存在：{name}",
		"project_scene_deleted":        "场景已删除：{name}",

		"prop_already_exists": "道具已存在：{name}",
		"prop_not_found":      "道具不存在：{name}",
		"prop_deleted":        "道具已删除：{name}",

		"product_already_exists": "产品已存在：{name}",
		"product_not_found":      "产品不存在：{name}",
		"product_deleted":        "产品已删除：{name}",
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

		"asset_invalid_name":                    "Invalid asset name: {name}",
		"asset_voice_notice_dismissed_at_stale": "The voice changed again; reload and confirm.",
		"asset_rename_file_conflict":            "Rename target already exists: {filename}",
		"asset_rename_rejected":                 "Rename rejected by structure validation: {name}",

		"character_already_exists": "Character already exists: {name}",
		"character_not_found":      "Character not found: {name}",
		"character_deleted":        "Character deleted: {name}",

		"project_scene_already_exists": "Scene already exists: {name}",
		"project_scene_not_found":      "Scene not found: {name}",
		"project_scene_deleted":        "Scene deleted: {name}",

		"prop_already_exists": "Prop already exists: {name}",
		"prop_not_found":      "Prop not found: {name}",
		"prop_deleted":        "Prop deleted: {name}",

		"product_already_exists": "Product already exists: {name}",
		"product_not_found":      "Product not found: {name}",
		"product_deleted":        "Product deleted: {name}",
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

		"asset_invalid_name":                    "Tên tài nguyên không hợp lệ: {name}",
		"asset_voice_notice_dismissed_at_stale": "Giọng đã thay đổi lại; hãy tải lại và xác nhận.",
		"asset_rename_file_conflict":            "Tên tệp đích đã tồn tại: {filename}",
		"asset_rename_rejected":                 "Đổi tên bị từ chối bởi kiểm tra cấu trúc: {name}",

		"character_already_exists": "Nhân vật đã tồn tại: {name}",
		"character_not_found":      "Không tìm thấy nhân vật: {name}",
		"character_deleted":        "Đã xóa nhân vật: {name}",

		"project_scene_already_exists": "Cảnh đã tồn tại: {name}",
		"project_scene_not_found":      "Không tìm thấy cảnh: {name}",
		"project_scene_deleted":        "Đã xóa cảnh: {name}",

		"prop_already_exists": "Đạo cụ đã tồn tại: {name}",
		"prop_not_found":      "Không tìm thấy đạo cụ: {name}",
		"prop_deleted":        "Đã xóa đạo cụ: {name}",

		"product_already_exists": "Sản phẩm đã tồn tại: {name}",
		"product_not_found":      "Không tìm thấy sản phẩm: {name}",
		"product_deleted":        "Đã xóa sản phẩm: {name}",
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
