package http

// step1→step2 web 审核 gate 路由（对应 server/routers/script_review.py）：结构化中间态的
// 审阅 / 编辑 / 确认 + 只读的待编写（pending authoring）推导。
//
// step1 产出后中间态在 web 可见可改，用户显式确认后才放行 step2 视觉生成（step2 由 agent 的
// generate_episode_script 工具执行，读时经 gate 校验阻塞到确认）。

import (
	servicescript "github.com/shotwise/shotwise/internal/service/script"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/i18n"
	"github.com/shotwise/shotwise/internal/service"
)

// ScriptReviewDeps 是审核 gate handler 的依赖。
type ScriptReviewDeps struct {
	Store   *service.ProjectStore
	Reviews *servicescript.ScriptReviewService
}

// RegisterScriptReview 注册审核 gate 路由；调用方负责鉴权。
func RegisterScriptReview(group *gin.RouterGroup, d ScriptReviewDeps) {
	group.GET("/projects/:name/episodes/:episode/script-review", getScriptReview(d))
	group.GET("/projects/:name/episodes/:episode/pending-authoring", getPendingAuthoring(d))
	group.PUT("/projects/:name/episodes/:episode/script-review/content", updateScriptReviewContent(d))
	group.POST("/projects/:name/episodes/:episode/script-review/confirm", confirmScriptReview(d))
}

// episodeFromPath 解析路径里的集号（对应 FastAPI 的 {episode:int} 参数校验）：非整数写出
// FastAPI 风格的 422。
func episodeFromPath(c *gin.Context) (int, bool) {
	episode, err := strconv.Atoi(c.Param("episode"))
	if err != nil {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"path", "episode"},
				Msg:  "Input should be a valid integer, unable to parse string as an integer",
				Type: "int_parsing"},
		}})
		return 0, false
	}
	return episode, true
}

// reviewErrorStatus / reviewErrorI18N 是 gate 领域错误码 → HTTP 状态 / i18n key 的共享映射
// （对应 server/routers/_script_review_errors.py）。invalid_content / episode_not_found 带参数，
// 在 abortReviewErr 单独处理。
var (
	reviewErrorStatus = map[string]int{
		servicescript.ScriptReviewCodeNotApplicable: http.StatusConflict,
		servicescript.ScriptReviewCodeNoStep1:       http.StatusConflict,
		servicescript.ScriptReviewCodeQuarantined:   http.StatusConflict,
		servicescript.ScriptReviewCodeConflict:      http.StatusConflict,
	}
	reviewErrorI18N = map[string]string{
		servicescript.ScriptReviewCodeNotApplicable: "script_review_not_applicable",
		servicescript.ScriptReviewCodeNoStep1:       "script_review_no_step1",
		servicescript.ScriptReviewCodeQuarantined:   "script_review_quarantined",
		servicescript.ScriptReviewCodeConflict:      "script_review_conflict",
	}
)

// abortReviewErr 把 ScriptReviewError 写成对应的错误响应；未登记的错误码落 400
// （对应 Python raise_review_error）。非领域错误按 500 处理。
func abortReviewErr(c *gin.Context, err error, episode int) {
	var reviewErr *servicescript.ScriptReviewError
	if !errors.As(err, &reviewErr) {
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	switch reviewErr.Code {
	case servicescript.ScriptReviewCodeInvalidContent:
		apperr.Abort(c, apperr.NewText(
			http.StatusUnprocessableEntity, "script_review_invalid_content",
			map[string]any{"details": reviewErr.Message},
		))
	case servicescript.ScriptReviewCodeEpisodeNotFound:
		apperr.Abort(c, apperr.NewText(
			http.StatusNotFound, "episode_not_found", map[string]any{"episode": episode},
		))
	default:
		status := http.StatusBadRequest
		if mapped, ok := reviewErrorStatus[reviewErr.Code]; ok {
			status = mapped
		}
		key := "internal_server_error"
		if mapped, ok := reviewErrorI18N[reviewErr.Code]; ok {
			key = mapped
		}
		apperr.Abort(c, apperr.NewText(status, key, nil))
	}
}

// attachDurationTiers 把收窄后的逐 unit 时长档位挂到 state 上；三个改动 step1 内容的端点
// （GET/PUT/POST）都要走这一步——否则保存 / 确认后 adopt() 用不带 duration_tiers 的响应覆盖
// GET 读到的收窄结果，面板退回未收窄的 supported_durations，与 GET 首次加载时的呈现不一致。
//
// 是否调用交给 GetReferenceDurationTiers 自己按 step1 变体判断，不靠
// state["supported_durations"] 非空短路——那是另一个方法的返回值，型号解析不到时同样为空，
// 靠它短路会让这类项目连未收窄的档位都拿不到。
func attachDurationTiers(c *gin.Context, d ScriptReviewDeps, name string, episode int, state map[string]any) bool {
	tiers, err := d.Reviews.GetReferenceDurationTiers(c.Request.Context(), name, episode)
	if err != nil {
		apperr.Abort(c, apperr.Internal(err))
		return false
	}
	state["duration_tiers"] = tiers
	return true
}

// localizeQuarantineViolations 把 quarantine_unreadable / quarantine_schema_unsupported 违约的
// 固定文案换成按请求语言渲染的本地化文本（对应 Python _localize_quarantine_violations）。
//
// 这两个 code 只由 service 层两处产出（隔离草稿信封本身损坏 / 重算所需的 meta 缺失损坏），
// 都是固定字符串；其余违约类型在产出时已渲染好插值，不在此牵动。
func localizeQuarantineViolations(quarantine map[string]any, loc i18n.Locale) map[string]any {
	if quarantine == nil {
		return nil
	}
	violations, ok := quarantine["violations"].([]map[string]any)
	if !ok {
		return quarantine
	}
	for _, violation := range violations {
		switch violation["code"] {
		case "quarantine_unreadable":
			violation["message"] = i18n.T(loc, "script_review_quarantine_unreadable", nil)
		case "quarantine_schema_unsupported":
			version, present := violation["schema_version"]
			if !present || version == nil {
				version = "?"
			}
			violation["message"] = i18n.T(loc, "script_review_quarantine_schema_unsupported", map[string]any{"version": version})
		}
	}
	return quarantine
}

// getScriptReview 读取该集 step1 结构化中间态 + 审核状态（供 web 渲染与编辑）。
//
// quarantine 字段单独合并（reference_video 变体、隔离草稿在场时才非 nil）：它按产出时那套
// 校验器做读时重算，与 GetState 的落盘读写彼此独立。先取 quarantine 再取 state：agent 的晋升
// 工具在两次读之间把隔离草稿清掉、正式 step1 写成新内容时，这个顺序让响应落在「content 已是
// 新的、quarantine 却还带着晋升前的违约报告」这一侧——面板会误判成仍在隔离态、阻塞确认，
// 下一轮轮询自然纠正；反过来的顺序会让响应落在「content 仍是旧的、quarantine 已经是 nil」
// 这一侧，面板会误判成干净态放行确认，用户点下确认时实际晋升的是他从未看过的那份新内容。
func getScriptReview(d ScriptReviewDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		episode, ok := episodeFromPath(c)
		if !ok {
			return
		}
		if !ensureProject(c, d.Store, name) {
			return
		}
		quarantine, err := d.Reviews.GetQuarantineInfo(c.Request.Context(), name, episode)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		state, err := d.Reviews.GetState(c.Request.Context(), name, episode)
		if err != nil {
			abortReviewErr(c, err, episode)
			return
		}
		if !attachDurationTiers(c, d, name, episode, state) {
			return
		}
		state["quarantine"] = localizeQuarantineViolations(quarantine, i18n.FromRequest(c.Request))
		c.JSON(http.StatusOK, state)
	}
}

// getPendingAuthoring 列出该集官方剧本里「正文变了、视觉层待重新编写」的条目（只读推导，
// 不落盘）。判定口径与范围见 service/pending_authoring.go 的模块注释。
func getPendingAuthoring(d ScriptReviewDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		episode, ok := episodeFromPath(c)
		if !ok {
			return
		}
		if !ensureProject(c, d.Store, name) {
			return
		}
		pf, err := d.Store.Get(name)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		dir, err := d.Store.Dir(name)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusOK, servicescript.PendingAuthoringReportFor(dir, pf, episode).ToDict())
	}
}

// updateScriptReviewContent 保存手动 / agent 编辑后的结构化中间态，并使该集重新进入待审。
//
// base_fingerprint（query）是编辑方 GET 时拿到的内容指纹：给定时服务端在锁内比对，编辑期间
// step1 被另一写入方改过则 409 冲突、不落盘；缺省不比对（无基线的直连调用）。
//
// quarantine 同 GET 一并合并：保存作用于正式草稿，与隔离草稿是两份独立文件，保存在途时
// agent 可能已经另外产出一份新的隔离草稿——响应缺这个字段的话 adopt() 会把它当作「无隔离
// 草稿」，面板显示干净态、放行确认，而 confirm() 仍会按隔离文件存在性 409。
//
// 保存完成后立即取 quarantine，早于 attachDurationTiers 的能力解析：晋升工具若恰好在这段
// 时间内把隔离草稿清掉，越晚读 quarantine 越可能读到「已清除」而不是晋升前那份，响应就会
// 落在「本次保存的内容 + quarantine: nil」这一侧，使没看过的、晋升后的内容被当作可放行
// 确认——同 GET 端点的顺序取舍，读时序错位只会让确认被多余地拦一轮，不会误放行。
func updateScriptReviewContent(d ScriptReviewDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		episode, ok := episodeFromPath(c)
		if !ok {
			return
		}
		if !ensureProject(c, d.Store, name) {
			return
		}
		var content map[string]any
		if err := c.ShouldBindJSON(&content); err != nil {
			abortBodyBind(c, err)
			return
		}
		if content == nil {
			abortBodyBind(c, errors.New("content must be a JSON object"))
			return
		}
		var baseFingerprint *string
		if raw := c.Query("base_fingerprint"); raw != "" {
			baseFingerprint = &raw
		}
		state, err := d.Reviews.SaveContent(c.Request.Context(), name, episode, content, baseFingerprint)
		if err != nil {
			abortReviewErr(c, err, episode)
			return
		}
		quarantine, err := d.Reviews.GetQuarantineInfo(c.Request.Context(), name, episode)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		if !attachDurationTiers(c, d, name, episode, state) {
			return
		}
		state["quarantine"] = localizeQuarantineViolations(quarantine, i18n.FromRequest(c.Request))
		c.JSON(http.StatusOK, state)
	}
}

// confirmScriptReview 用户显式确认 step1 内容，放行 step2 视觉生成。
//
// quarantine 同 GET / PUT 一并合并，保持三个端点响应形状一致——Confirm 内部虽已按隔离文件
// 存在性拒绝确认，但响应仍应如实反映确认完成那一刻的隔离态，而不是让这个字段在三个端点里
// 时有时无。
func confirmScriptReview(d ScriptReviewDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		episode, ok := episodeFromPath(c)
		if !ok {
			return
		}
		if !ensureProject(c, d.Store, name) {
			return
		}
		state, err := d.Reviews.Confirm(c.Request.Context(), name, episode)
		if err != nil {
			abortReviewErr(c, err, episode)
			return
		}
		if !attachDurationTiers(c, d, name, episode, state) {
			return
		}
		quarantine, err := d.Reviews.GetQuarantineInfo(c.Request.Context(), name, episode)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		state["quarantine"] = localizeQuarantineViolations(quarantine, i18n.FromRequest(c.Request))
		c.JSON(http.StatusOK, state)
	}
}
