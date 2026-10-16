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

		// 全局资产库文案与 Python 侧 lib/i18n/*/assets.py 逐条对齐。
		"asset_not_found":                 "资产「{name}」不存在",
		"asset_already_exists":            "同类型下已有同名资产「{name}」",
		"asset_invalid_type":              "资产类型必须为 character / scene / prop",
		"asset_upload_too_large":          "图片大小超过限制（5MB）",
		"asset_unsupported_format":        "仅支持 png/jpg/jpeg/webp",
		"asset_source_resource_not_found": "项目「{project}」中不存在{kind}「{name}」",
		"asset_target_project_not_found":  "目标项目「{project}」不存在",
		"asset_load_project_failed":       "加载目标项目失败",
		"asset_invalid_conflict_policy":   "冲突策略必须为 skip / overwrite / rename",
		"asset_type_character":            "角色",
		"asset_type_scene":                "场景",
		"asset_type_prop":                 "道具",
		"asset_type_product":              "产品",

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

		// 长期记忆文案与 Python 侧 lib/i18n/*/errors.py 逐条对齐。
		"memory_scope_invalid":       "记忆作用域无效，只能是 user 或 project",
		"memory_project_required":    "项目记忆必须指定项目",
		"memory_project_invalid":     "项目记忆的项目标识无效",
		"memory_category_invalid":    "记忆分类无效",
		"memory_source_invalid":      "记忆来源无效",
		"memory_content_invalid":     "记忆内容无效",
		"memory_content_empty":       "记忆内容不能为空",
		"memory_content_too_long":    "记忆内容过长，最多 {max_length} 个字符",
		"memory_sensitive_content":   "记忆内容疑似包含密钥、密码或敏感路径，无法保存",
		"memory_prompt_injection":    "记忆内容疑似包含提示注入指令，无法保存",
		"memory_duplicate":           "相同记忆已存在",
		"memory_update_empty":        "至少提供一个需要更新的字段",
		"memory_not_found":           "记忆不存在",
		"memory_candidate_not_found": "记忆候选不存在",
		"memory_candidate_resolved":  "记忆候选已处理",
		"memory_scope_required":      "指定项目时必须同时指定记忆作用域",
		"memory_import_invalid":      "记忆导入数据无效",

		// step1→step2 审核门文案与 Python 侧 lib/i18n/*/errors.py 逐条对齐。
		"script_review_not_applicable":                "该集不适用 step1 审核确认（该模式无结构化 step1 中间态）",
		"script_review_no_step1":                      "尚无 step1 结构化中间态可确认，请先完成预处理",
		"script_review_quarantined":                   "本集 step1 有违约产物待处置，请让智能体修复并晋升后再确认",
		"script_review_conflict":                      "step1 内容在编辑期间已被其他编辑方修改，本次保存未覆盖；请刷新查看最新内容，合并后再保存",
		"script_review_invalid_content":               "step1 中间态结构校验失败：{details}",
		"script_review_quarantine_unreadable":         "隔离草稿文件已损坏或格式不符，无法读取，请让智能体重新拆分该集",
		"script_review_quarantine_schema_unsupported": "隔离草稿使用了不支持的 schema 版本 {version}，请先升级应用后再继续",
		"episode_not_found":                           "第 {episode} 集不存在或尚无剧本文件",
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

		"asset_not_found":                 "Asset '{name}' does not exist",
		"asset_already_exists":            "An asset named '{name}' already exists under the same type",
		"asset_invalid_type":              "Asset type must be character / scene / prop",
		"asset_upload_too_large":          "Image size exceeds the limit (5MB)",
		"asset_unsupported_format":        "Only png/jpg/jpeg/webp are supported",
		"asset_source_resource_not_found": "Project '{project}' does not contain {kind} '{name}'",
		"asset_target_project_not_found":  "Target project '{project}' does not exist",
		"asset_load_project_failed":       "failed to load target project",
		"asset_invalid_conflict_policy":   "conflict_policy must be skip / overwrite / rename",
		"asset_type_character":            "character",
		"asset_type_scene":                "scene",
		"asset_type_prop":                 "prop",
		"asset_type_product":              "product",

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

		"memory_scope_invalid":       "Invalid memory scope; expected user or project",
		"memory_project_required":    "A project is required for project-scoped memory",
		"memory_project_invalid":     "The memory project identifier is invalid",
		"memory_category_invalid":    "Invalid memory category",
		"memory_source_invalid":      "Invalid memory source",
		"memory_content_invalid":     "Invalid memory content",
		"memory_content_empty":       "Memory content cannot be empty",
		"memory_content_too_long":    "Memory content is too long; the maximum is {max_length} characters",
		"memory_sensitive_content":   "Memory content appears to contain a key, password, or sensitive path and cannot be saved",
		"memory_prompt_injection":    "Memory content appears to contain a prompt injection and cannot be saved",
		"memory_duplicate":           "The same memory already exists",
		"memory_update_empty":        "Provide at least one field to update",
		"memory_not_found":           "Memory was not found",
		"memory_candidate_not_found": "Memory candidate was not found",
		"memory_candidate_resolved":  "Memory candidate has already been resolved",
		"memory_scope_required":      "A memory scope is required when filtering by project",
		"memory_import_invalid":      "Invalid memory import payload",

		"script_review_not_applicable": "Step 1 review does not apply to this episode (this mode has no structured Step 1 intermediate)",
		"script_review_no_step1":       "No Step 1 structured draft to confirm yet; please finish preprocessing first",
		"script_review_quarantined":    "This episode has a rejected Step 1 draft awaiting repair; let the agent fix and promote it before confirming",
		"script_review_conflict": "The Step 1 draft was modified by another editor while you were editing; your save was not applied. " +
			"Refresh to see the latest content, merge your changes, then save again",
		"script_review_invalid_content":               "Step 1 draft structure validation failed: {details}",
		"script_review_quarantine_unreadable":         "The quarantined draft file is corrupted or malformed and can't be read; ask the agent to re-split this episode",
		"script_review_quarantine_schema_unsupported": "This quarantined draft uses unsupported schema version {version}; upgrade the application before continuing",
		"episode_not_found":                           "Episode {episode} not found or has no script file yet",
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

		"asset_not_found":                 "Tài nguyên '{name}' không tồn tại",
		"asset_already_exists":            "Đã tồn tại tài nguyên cùng loại có tên '{name}'",
		"asset_invalid_type":              "Loại tài nguyên phải là character / scene / prop",
		"asset_upload_too_large":          "Kích thước ảnh vượt giới hạn (5MB)",
		"asset_unsupported_format":        "Chỉ hỗ trợ các định dạng png/jpg/jpeg/webp",
		"asset_source_resource_not_found": "Dự án '{project}' không chứa {kind} '{name}'",
		"asset_target_project_not_found":  "Dự án đích '{project}' không tồn tại",
		"asset_load_project_failed":       "Không tải được dự án đích",
		"asset_invalid_conflict_policy":   "conflict_policy phải là skip / overwrite / rename",
		"asset_type_character":            "nhân vật",
		"asset_type_scene":                "cảnh",
		"asset_type_prop":                 "đạo cụ",
		"asset_type_product":              "sản phẩm",

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

		"memory_scope_invalid":       "Phạm vi bộ nhớ không hợp lệ, chỉ chấp nhận user hoặc project",
		"memory_project_required":    "Bộ nhớ theo dự án phải có dự án",
		"memory_project_invalid":     "Định danh dự án của bộ nhớ không hợp lệ",
		"memory_category_invalid":    "Danh mục bộ nhớ không hợp lệ",
		"memory_source_invalid":      "Nguồn bộ nhớ không hợp lệ",
		"memory_content_invalid":     "Nội dung bộ nhớ không hợp lệ",
		"memory_content_empty":       "Nội dung bộ nhớ không được để trống",
		"memory_content_too_long":    "Nội dung bộ nhớ quá dài, tối đa {max_length} ký tự",
		"memory_sensitive_content":   "Nội dung bộ nhớ có vẻ chứa khóa, mật khẩu hoặc đường dẫn nhạy cảm nên không thể lưu",
		"memory_prompt_injection":    "Nội dung bộ nhớ có vẻ chứa lệnh chèn prompt nên không thể lưu",
		"memory_duplicate":           "Bộ nhớ giống hệt đã tồn tại",
		"memory_update_empty":        "Hãy cung cấp ít nhất một trường cần cập nhật",
		"memory_not_found":           "Không tìm thấy bộ nhớ",
		"memory_candidate_not_found": "Không tìm thấy ứng viên bộ nhớ",
		"memory_candidate_resolved":  "Ứng viên bộ nhớ đã được xử lý",
		"memory_scope_required":      "Khi lọc theo dự án phải chỉ định phạm vi bộ nhớ",
		"memory_import_invalid":      "Dữ liệu nhập bộ nhớ không hợp lệ",

		"script_review_not_applicable": "Tập này không áp dụng xác nhận Step 1 (chế độ này không có bản trung gian Step 1 có cấu trúc)",
		"script_review_no_step1":       "Chưa có bản nháp có cấu trúc Step 1 để xác nhận; vui lòng hoàn tất tiền xử lý trước",
		"script_review_quarantined":    "Tập này có bản nháp Step 1 vi phạm đang chờ xử lý; hãy để tác nhân sửa và thăng cấp trước khi xác nhận",
		"script_review_conflict": "Bản nháp Step 1 đã bị người chỉnh sửa khác thay đổi trong lúc bạn đang chỉnh sửa; lần lưu này chưa được áp dụng. " +
			"Hãy tải lại để xem nội dung mới nhất, hợp nhất thay đổi của bạn rồi lưu lại",
		"script_review_invalid_content":               "Xác thực cấu trúc bản nháp Step 1 thất bại: {details}",
		"script_review_quarantine_unreadable":         "Tệp bản nháp bị cách ly đã hỏng hoặc sai định dạng, không thể đọc được; hãy để tác nhân chia lại tập này",
		"script_review_quarantine_schema_unsupported": "Bản nháp bị cách ly sử dụng phiên bản schema {version} không được hỗ trợ; hãy nâng cấp ứng dụng trước khi tiếp tục",
		"episode_not_found":                           "Không tìm thấy tập {episode} hoặc tập chưa có tệp kịch bản",
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
