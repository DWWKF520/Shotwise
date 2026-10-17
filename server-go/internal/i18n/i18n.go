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

		// 参考生视频文案与 Python 侧 lib/i18n/*/errors.py 逐条对齐。
		"script_validation_failed":         "剧本结构校验失败：{details}",
		"ref_episode_not_found":            "第 {episode} 集不存在",
		"ref_not_reference_video_mode":     "该集剧本不是参考生视频模式",
		"ref_not_registered":               "引用的资产未注册：{missing}",
		"ref_unit_not_found":               "视频单元「{unit_id}」不存在",
		"ref_unit_ids_length_mismatch":     "unit_ids 数量与现有单元不一致",
		"ref_duplicate_unit_ids":           "unit_ids 存在重复",
		"ref_unit_ids_mismatch":            "unit_ids 与现有单元不匹配",
		"ref_script_missing":               "该集绑定的剧本文件不存在",
		"ref_script_rebound":               "该集剧本绑定已变更，请重试",
		"ref_ad_units_derived":             "广告/短片项目的视频单元由镜头自动派生，不支持手动增删改；请编辑镜头后重新派生分组",
		"ref_derive_ad_only":               "仅广告/短片项目支持派生分组",
		"ref_ad_stale_index":               "分组索引与镜头不一致，请重新派生分组",
		"generation_asset_preflight_failed": "生成前检查失败。未登记资产：{unregistered}；缺少资产图：{missing_images}；缺少角色衍生形态：{missing_variants}",
		"prompt_text_empty":                 "prompt 不能为空",
		"video_audio_switch_not_supported":  "{provider}/{model} 的成片恒有声，无法关闭音频；请在设置中把音频开关改回开启后重试",
		"invalid_resource_id":               "非法的资源 ID: {resource_id}",
		"missing_filename":                  "上传的文件缺少文件名",
		"unsupported_video_type":            "不支持的视频类型 {ext}，允许的类型: {allowed}",
		"upload_too_large":                  "上传文件超过大小上限（{max_mb} MB）",
		"video_capability_missing_i2v":      "{provider}/{model} 不支持图生视频；请在设置中为「图生视频」单独指定支持该用途的模型，或更换默认视频模型",
		"video_capability_missing_r2v":      "{provider}/{model} 不支持参考生视频；请在设置中为「参考生视频」单独指定支持该用途的模型，或更换默认视频模型",
		"video_capability_reference_unavailable": "配置的视频模型 {provider}/{model} 已不可用（模型被删除、能力已变更或供应商被移除）；请在设置中重新选择视频模型",

		// 分镜文稿解析预览的降级可见性提示。
		"ref_warn_unregistered_mention":     "@[{name}] 未在角色/场景/道具中登记：不会附带参考图，请检查名称或先创建资产",
		"ref_warn_unclosed_brace":           "镜头{shot}：台词花括号未闭合，未识别为台词，该行文本将原样发送：{excerpt}…",
		"ref_warn_dialogue_inline":          "镜头{shot}：台词与描述写在同一行，未识别为台词；如需声音参考请将台词单独成行（@[角色]：{{台词}}）",
		"ref_warn_unregistered_speaker":     "@[{name}] 未在角色中登记：无法确认说话人，该行按原文发送",
		"ref_warn_speaker_without_audio":    "角色「{name}」未设置参考音频：台词声音将由模型自行决定",
		"ref_warn_speaker_audio_unavailable": "角色「{name}」已设置参考音频，但该音频当前不可用：台词声音将由模型自行决定",
		"ref_warn_reference_audio_overflow": "参考音频最多 {limit} 段：角色「{name}」的台词声音将由模型自行决定",
		"ref_warn_speaker_audio_needs_image": "角色「{name}」没有参考图（纯画外）：当前视频模型要求参考音频逐段挂在参考图上，该角色的台词声音将由模型自行决定",
		"ref_warn_silent_model":             "当前视频模型「{model}」不生成音频，台词仅用于提示词参考",
		"ref_warn_silent_episode":           "本集已关闭参考音频：不会上传参考音频，声音一致性不生效，台词仅用于提示词参考；成片是否有声音由所选模型决定",
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

		"script_validation_failed":         "Script structure validation failed: {details}",
		"ref_episode_not_found":            "Episode {episode} not found",
		"ref_not_reference_video_mode":     "Episode script is not in reference-video mode",
		"ref_not_registered":               "Referenced assets are not registered: {missing}",
		"ref_unit_not_found":               "Video unit '{unit_id}' not found",
		"ref_unit_ids_length_mismatch":     "unit_ids count does not match existing units",
		"ref_duplicate_unit_ids":           "unit_ids contains duplicates",
		"ref_unit_ids_mismatch":            "unit_ids do not match existing units",
		"ref_script_missing":               "The script file bound to this episode does not exist",
		"ref_script_rebound":               "This episode's script binding changed, please retry",
		"ref_ad_units_derived":             "Video units of an ad/short-video project are derived from shots automatically and cannot be edited manually; edit the shots and re-derive the grouping instead",
		"ref_derive_ad_only":               "Deriving video unit groups is only available for ad/short-video projects",
		"ref_ad_stale_index":               "The unit grouping index no longer matches the shots, please re-derive the grouping",
		"generation_asset_preflight_failed": "Generation preflight failed. Unregistered assets: {unregistered}; missing asset images: {missing_images}; missing character variants: {missing_variants}",
		"prompt_text_empty":                 "prompt must not be empty",
		"video_audio_switch_not_supported":  "{provider}/{model} always produces audio and cannot be muted; turn the audio switch back on in settings and try again",
		"invalid_resource_id":               "Invalid resource ID: {resource_id}",
		"missing_filename":                  "Uploaded file is missing a filename",
		"unsupported_video_type":            "Unsupported video type {ext}. Allowed types: {allowed}",
		"upload_too_large":                  "Uploaded file exceeds the size limit ({max_mb} MB)",
		"video_capability_missing_i2v":      "{provider}/{model} does not support image-to-video generation; assign a model that supports it for image-to-video in Settings, or change the default video model",
		"video_capability_missing_r2v":      "{provider}/{model} does not support reference-to-video generation; assign a model that supports it for reference-to-video in Settings, or change the default video model",
		"video_capability_reference_unavailable": "The configured video model {provider}/{model} is no longer available (model deleted, capabilities changed, or provider removed); re-select a video model in Settings",

		"ref_warn_unregistered_mention": "@[{name}] is not registered as a character, scene, or prop: no reference image will be attached. Check the name or create the asset first",
		"ref_warn_unclosed_brace":       "Shot {shot}: unclosed dialogue braces, not recognized as dialogue. The line is sent verbatim: {excerpt}…",
		"ref_warn_dialogue_inline":      "Shot {shot}: dialogue and description are on the same line, so it is not recognized as dialogue. For voice reference, put the dialogue on its own line (@[character]: {{dialogue}})",
		"ref_warn_unregistered_speaker": "@[{name}] is not registered as a character: the speaker cannot be confirmed, so the line is sent verbatim",
		"ref_warn_speaker_without_audio":    "Character '{name}' has no reference audio: the model decides the dialogue voice",
		"ref_warn_speaker_audio_unavailable": "Character '{name}' has reference audio set, but it is currently unavailable: the model decides the dialogue voice",
		"ref_warn_reference_audio_overflow":  "At most {limit} reference audio clips: the model decides the dialogue voice for character '{name}'",
		"ref_warn_speaker_audio_needs_image": "Character '{name}' has no reference image (off-screen only): the current video model requires reference audio to be attached per reference image, so the model decides this character's dialogue voice",
		"ref_warn_silent_model":              "The current video model '{model}' generates no audio; dialogue is used only as prompt context",
		"ref_warn_silent_episode":            "Reference audio is disabled for this episode: no reference audio is uploaded and voice consistency does not apply; dialogue is used only as prompt context. Whether the rendered video actually has sound depends on the selected model",
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

		"script_validation_failed":         "Xác thực cấu trúc kịch bản thất bại: {details}",
		"ref_episode_not_found":            "Không tìm thấy tập {episode}",
		"ref_not_reference_video_mode":     "Kịch bản của tập này không ở chế độ video tham chiếu",
		"ref_not_registered":               "Các tài nguyên được tham chiếu chưa được đăng ký: {missing}",
		"ref_unit_not_found":               "Không tìm thấy đơn vị video '{unit_id}'",
		"ref_unit_ids_length_mismatch":     "Số lượng unit_ids không khớp với các đơn vị hiện có",
		"ref_duplicate_unit_ids":           "unit_ids bị trùng lặp",
		"ref_unit_ids_mismatch":            "unit_ids không khớp với các đơn vị hiện có",
		"ref_script_missing":               "Tệp kịch bản gắn với tập này không tồn tại",
		"ref_script_rebound":               "Liên kết kịch bản của tập này đã thay đổi, vui lòng thử lại",
		"ref_ad_units_derived":             "Các đơn vị video của dự án quảng cáo/video ngắn được tự động suy ra từ các cảnh quay, không thể chỉnh sửa thủ công; hãy chỉnh sửa cảnh quay rồi suy ra lại nhóm",
		"ref_derive_ad_only":               "Chỉ dự án quảng cáo/video ngắn mới hỗ trợ suy ra nhóm đơn vị video",
		"ref_ad_stale_index":               "Chỉ mục nhóm không còn khớp với các cảnh quay, vui lòng suy ra lại nhóm",
		"generation_asset_preflight_failed": "Kiểm tra trước khi tạo thất bại. Tài nguyên chưa đăng ký: {unregistered}; ảnh tài nguyên bị thiếu: {missing_images}; biến thể nhân vật bị thiếu: {missing_variants}",
		"prompt_text_empty":                 "prompt không được để trống",
		"video_audio_switch_not_supported":  "{provider}/{model} luôn tạo ra âm thanh và không thể tắt tiếng; hãy bật lại công tắc âm thanh trong cài đặt rồi thử lại",
		"invalid_resource_id":               "ID tài nguyên không hợp lệ: {resource_id}",
		"missing_filename":                  "Tệp tải lên thiếu tên tệp",
		"unsupported_video_type":            "Định dạng video không hỗ trợ {ext}. Các loại cho phép: {allowed}",
		"upload_too_large":                  "Tệp tải lên vượt quá giới hạn dung lượng ({max_mb} MB)",
		"video_capability_missing_i2v":      "{provider}/{model} không hỗ trợ tạo video từ ảnh; hãy chỉ định mô hình hỗ trợ tính năng này cho mục tạo video từ ảnh trong Cài đặt, hoặc đổi mô hình video mặc định",
		"video_capability_missing_r2v":      "{provider}/{model} không hỗ trợ tạo video theo tham chiếu; hãy chỉ định mô hình hỗ trợ tính năng này cho mục tạo video theo tham chiếu trong Cài đặt, hoặc đổi mô hình video mặc định",
		"video_capability_reference_unavailable": "Mô hình video đã cấu hình {provider}/{model} không còn khả dụng (mô hình bị xóa, năng lực đã thay đổi hoặc nhà cung cấp bị gỡ bỏ); hãy chọn lại mô hình video trong Cài đặt",

		"ref_warn_unregistered_mention": "@[{name}] chưa được đăng ký trong nhân vật/bối cảnh/đạo cụ: sẽ không kèm ảnh tham chiếu, hãy kiểm tra tên hoặc tạo tài sản trước",
		"ref_warn_unclosed_brace":       "Cảnh {shot}: dấu ngoặc nhọn của lời thoại chưa đóng nên không được nhận là lời thoại, dòng này sẽ được gửi nguyên văn: {excerpt}…",
		"ref_warn_dialogue_inline":      "Cảnh {shot}: lời thoại và mô tả nằm cùng một dòng nên không được nhận là lời thoại; nếu cần tham chiếu giọng nói, hãy để lời thoại thành dòng riêng (@[nhân vật]: {{lời thoại}})",
		"ref_warn_unregistered_speaker": "@[{name}] chưa được đăng ký trong nhân vật: không xác định được người nói, dòng này sẽ được gửi nguyên văn",
		"ref_warn_speaker_without_audio":     "Nhân vật '{name}' chưa đặt âm thanh tham chiếu: giọng của lời thoại sẽ do mô hình tự quyết định",
		"ref_warn_speaker_audio_unavailable": "Nhân vật '{name}' đã đặt âm thanh tham chiếu nhưng hiện không dùng được: giọng của lời thoại sẽ do mô hình tự quyết định",
		"ref_warn_reference_audio_overflow":  "Tối đa {limit} đoạn âm thanh tham chiếu: giọng lời thoại của nhân vật '{name}' sẽ do mô hình tự quyết định",
		"ref_warn_speaker_audio_needs_image": "Nhân vật '{name}' không có ảnh tham chiếu (chỉ xuất hiện ngoài hình): mô hình video hiện tại yêu cầu âm thanh tham chiếu phải gắn theo từng ảnh tham chiếu, giọng lời thoại của nhân vật này sẽ do mô hình tự quyết định",
		"ref_warn_silent_model":              "Mô hình video hiện tại '{model}' không tạo âm thanh, lời thoại chỉ dùng làm gợi ý cho prompt",
		"ref_warn_silent_episode":            "Tập này đã tắt âm thanh tham chiếu: không tải lên âm thanh tham chiếu, tính nhất quán giọng nói không có hiệu lực, lời thoại chỉ dùng làm gợi ý cho prompt; video hoàn chỉnh có tiếng hay không tùy thuộc vào mô hình đã chọn",
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
