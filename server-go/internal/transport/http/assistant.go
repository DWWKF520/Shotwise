// 助手会话路由（对应 server/routers/assistant.py + agent_chat.py）。
//
// 持久会话契约：meta store（agent_sessions）+ 事件日志（agent_session_event_log）
// + client_key 幂等 + rewrite 前缀分叉，编排层在 internal/agent/assistant.go。
// live 执行复用 agent.Runtime（TurnLoop）。
//
// 错误信封：Python assistant 路由的 ApiError 经 app 级 handler 翻成字符串 detail，
// Go 侧对应 apperr.NewText（i18n key 渲染）。
package http

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/agent"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/service"
)

// AssistantDeps 是助手会话路由的依赖。
type AssistantDeps struct {
	Assistant *agent.AssistantService
	Store     *service.ProjectStore
}

// RegisterAssistant 注册助手会话路由。authed 为常规鉴权组；sse 为 flexible
// 鉴权组——entries/stream 是浏览器 EventSource 直发请求，带不了 Authorization
// header，只认 ?token=。
func RegisterAssistant(authed, sse *gin.RouterGroup, d AssistantDeps) {
	base := "/projects/:name/assistant"

	// Python 侧已声明下线的探测接口 → 410 interface_offline。
	authed.GET(base+"/sessions/:session_id/messages", assistantGone())
	authed.GET(base+"/sessions/:session_id/snapshot", assistantGone())
	sse.GET(base+"/sessions/:session_id/stream", assistantGone())

	authed.POST(base+"/sessions/send", assistantSend(d))
	authed.POST(base+"/sessions/:session_id/rewrite", assistantRewrite(d))
	authed.GET(base+"/sessions", assistantListSessions(d))
	authed.GET(base+"/sessions/:session_id", assistantGetSession(d))
	authed.DELETE(base+"/sessions/:session_id", assistantDeleteSession(d))
	authed.GET(base+"/sessions/:session_id/entries", assistantListEntries(d))
	sse.GET(base+"/sessions/:session_id/entries/stream", assistantStreamEntries(d))
	authed.POST(base+"/sessions/:session_id/interrupt", assistantInterrupt(d))
	authed.POST(base+"/sessions/:session_id/switch-agent", assistantSwitchAgent(d))
	authed.POST(base+"/sessions/:session_id/questions/:question_id/answer", assistantAnswerQuestion(d))
	authed.GET(base+"/skills", assistantListSkills(d))

	// 同步 Agent 对话端点（供 OpenClaw 等外部 Agent 调用）。
	authed.POST("/agent/chat", agentChat(d))
}

func assistantGone() gin.HandlerFunc {
	return func(c *gin.Context) {
		apperr.Abort(c, apperr.NewText(http.StatusGone, "interface_offline", nil))
	}
}

// requireAssistantProject 校验项目存在，失败时写出错误并返回空。
func requireAssistantProject(c *gin.Context, d AssistantDeps) *agent.AssistantService {
	if d.Assistant == nil {
		apperr.Abort(c, apperr.NewText(http.StatusServiceUnavailable, "session_capacity_exceeded", nil))
		return nil
	}
	name := c.Param("name")
	if _, err := serviceValidateProjectName(name); err != nil {
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "invalid_project_name", map[string]any{"name": name}))
		return nil
	}
	if !d.Store.Exists(name) {
		// 与 Python send/rewrite 的 FileNotFoundError 映射同 key。
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "session_or_project_not_found", nil))
		return nil
	}
	return d.Assistant
}

func serviceValidateProjectName(name string) (string, error) {
	if name == "" {
		return "", errors.New("empty")
	}
	return name, nil
}

// abortAssistant 把编排层错误映射为 HTTP 响应（与 Python 异常 → 状态码同口径）。
func abortAssistant(c *gin.Context, err error, sessionID string) {
	var (
		status int
		key    string
		params map[string]any
	)
	switch {
	case errors.Is(err, agent.ErrSessionNotFound):
		status, key, params = http.StatusNotFound, "session_not_found", map[string]any{"session_id": sessionID}
	case errors.Is(err, agent.ErrSessionBusy):
		status, key = http.StatusConflict, "session_busy"
	case errors.Is(err, agent.ErrSessionSuperseded):
		status, key = http.StatusConflict, "session_already_superseded"
	case errors.Is(err, agent.ErrRewriteAnchor):
		status, key = http.StatusBadRequest, "rewrite_anchor_invalid"
	case errors.Is(err, agent.ErrPendingQuestion):
		status, key = http.StatusConflict, "rewrite_blocked_by_question"
	case errors.Is(err, agent.ErrInterruptSettle):
		status, key = http.StatusGatewayTimeout, "rewrite_interrupt_timeout"
	case errors.Is(err, agent.ErrMessageEmpty):
		status, key = http.StatusBadRequest, "request_invalid"
	case errors.Is(err, agent.ErrSDKTypeUnknown):
		status, key, params = http.StatusBadRequest, "agent_sdk_type_unknown", map[string]any{"sdk_type": sdkTypeFromErr(err)}
	default:
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	apperr.Abort(c, apperr.NewText(status, key, params))
}

func sdkTypeFromErr(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ":"); i >= 0 {
		return strings.TrimSpace(msg[i+1:])
	}
	return ""
}

type imageAttachmentBody struct {
	Data      string `json:"data"`
	MediaType string `json:"media_type"`
}

type sendRequestBody struct {
	Content   string               `json:"content"`
	Images    []imageAttachmentBody `json:"images"`
	SessionID string               `json:"session_id"`
	ClientKey string               `json:"client_key"`
	SDKType   string               `json:"sdk_type"`
}

func assistantSend(d AssistantDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		a := requireAssistantProject(c, d)
		if a == nil {
			return
		}
		var body sendRequestBody
		if err := c.ShouldBindJSON(&body); err != nil {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
			return
		}
		// 图片附件（多模态）暂不在 Go 运行时支持范围内：解析字段但显式拒绝，
		// 不静默丢弃用户图片。
		if len(body.Images) > 0 {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
			return
		}
		if body.SDKType == "" {
			body.SDKType = "claude"
		}
		result, err := a.SendCreate(c.Request.Context(), c.Param("name"), body.Content,
			body.SessionID, body.ClientKey, body.SDKType, currentUserID(c))
		if err != nil {
			abortAssistant(c, err, body.SessionID)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func assistantRewrite(d AssistantDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		a := requireAssistantProject(c, d)
		if a == nil {
			return
		}
		sessionID := c.Param("session_id")
		var body struct {
			AnchorEntryUUID string                `json:"anchor_entry_uuid"`
			Content         string                `json:"content"`
			Images          []imageAttachmentBody `json:"images"`
			ClientKey       string                `json:"client_key"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || body.AnchorEntryUUID == "" {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
			return
		}
		if len(body.Images) > 0 {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
			return
		}
		result, err := a.Rewrite(c.Request.Context(), c.Param("name"), sessionID,
			body.AnchorEntryUUID, body.Content, body.ClientKey, currentUserID(c))
		if err != nil {
			abortAssistant(c, err, sessionID)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func assistantListSessions(d AssistantDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		a := requireAssistantProject(c, d)
		if a == nil {
			return
		}
		var status *service.SessionStatus
		if raw := strings.TrimSpace(c.Query("status")); raw != "" {
			switch service.SessionStatus(raw) {
			case service.SessionStatusIdle, service.SessionStatusRunning,
				service.SessionStatusCompleted, service.SessionStatusError,
				service.SessionStatusInterrupted:
				s := service.SessionStatus(raw)
				status = &s
			default:
				apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
				return
			}
		}
		limit := queryIntDefault(c, "limit", 50)
		if limit < 1 {
			limit = 1
		} else if limit > 200 {
			limit = 200
		}
		offset := queryIntDefault(c, "offset", 0)
		if offset < 0 {
			offset = 0
		}
		sessions, err := a.ListSessions(c.Request.Context(), c.Param("name"), status, limit, offset, currentUserID(c))
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		if sessions == nil {
			sessions = []*service.SessionMeta{}
		}
		c.JSON(http.StatusOK, gin.H{"sessions": sessions})
	}
}

// requireSessionOwnership 校验会话存在且属于当前项目，返回编排层句柄；
// 失败时已写出 404。
func requireSessionOwnership(c *gin.Context, d AssistantDeps) (*agent.AssistantService, *service.SessionMeta) {
	a := requireAssistantProject(c, d)
	if a == nil {
		return nil, nil
	}
	sessionID := c.Param("session_id")
	meta, err := a.GetSession(c.Request.Context(), sessionID, currentUserID(c))
	if err != nil {
		apperr.Abort(c, apperr.Internal(err))
		return nil, nil
	}
	if meta == nil || meta.ProjectName != c.Param("name") {
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "session_not_found", map[string]any{"session_id": sessionID}))
		return nil, nil
	}
	return a, meta
}

func assistantGetSession(d AssistantDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, meta := requireSessionOwnership(c, d); meta != nil {
			c.JSON(http.StatusOK, meta)
		}
	}
}

func assistantDeleteSession(d AssistantDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		a, _ := requireSessionOwnership(c, d)
		if a == nil {
			return
		}
		deleted, err := a.DeleteSession(c.Request.Context(), c.Param("session_id"), currentUserID(c))
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		if !deleted {
			apperr.Abort(c, apperr.NewText(http.StatusNotFound, "session_not_found",
				map[string]any{"session_id": c.Param("session_id")}))
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

func assistantListEntries(d AssistantDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		a, _ := requireSessionOwnership(c, d)
		if a == nil {
			return
		}
		page, err := a.ListEntries(c.Request.Context(), c.Param("session_id"), queryAfterSeq(c), currentUserID(c))
		if err != nil {
			if errors.Is(err, agent.ErrSessionNotFound) {
				apperr.Abort(c, apperr.NewText(http.StatusNotFound, "session_not_found",
					map[string]any{"session_id": c.Param("session_id")}))
				return
			}
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusOK, page)
	}
}

func assistantStreamEntries(d AssistantDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		a, _ := requireSessionOwnership(c, d)
		if a == nil {
			return
		}
		sessionID := c.Param("session_id")
		cursor := queryAfterSeq(c)
		// 游标优先级：EventSource 自动重连携带的 Last-Event-ID > ?after=<seq>（冷订阅）。
		if raw := strings.TrimSpace(c.GetHeader("Last-Event-ID")); raw != "" {
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
				cursor = parsed
			}
		}
		evCh, unsub, err := a.Stream(c.Request.Context(), sessionID, cursor)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		c.Header("X-Session-Id", sessionID)
		streamAgentEvents(c, evCh, unsub)
	}
}

func assistantInterrupt(d AssistantDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		a, _ := requireSessionOwnership(c, d)
		if a == nil {
			return
		}
		sessionID := c.Param("session_id")
		status, err := a.Interrupt(c.Request.Context(), sessionID, currentUserID(c))
		if err != nil {
			abortAssistant(c, err, sessionID)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": status, "session_id": sessionID})
	}
}

func assistantSwitchAgent(d AssistantDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		a, _ := requireSessionOwnership(c, d)
		if a == nil {
			return
		}
		sessionID := c.Param("session_id")
		var body struct {
			SDKType string `json:"sdk_type"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
			return
		}
		if body.SDKType == "" {
			body.SDKType = "claude"
		}
		meta, err := a.SwitchAgent(c.Request.Context(), sessionID, body.SDKType, currentUserID(c))
		if err != nil {
			abortAssistant(c, err, sessionID)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "accepted", "session_id": sessionID, "sdk_type": meta.SDKType})
	}
}

func assistantAnswerQuestion(d AssistantDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		a, _ := requireSessionOwnership(c, d)
		if a == nil {
			return
		}
		sessionID := c.Param("session_id")
		questionID := c.Param("question_id")
		var body struct {
			Answers map[string]string `json:"answers"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
			return
		}
		if len(body.Answers) == 0 {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "answers_required", nil))
			return
		}
		if err := a.AnswerQuestion(c.Request.Context(), sessionID, questionID, body.Answers, currentUserID(c)); err != nil {
			// 会话未运行或无待回答问题。
			abortAssistant(c, err, sessionID)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "accepted", "session_id": sessionID, "question_id": questionID})
	}
}

func assistantListSkills(d AssistantDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		a := requireAssistantProject(c, d)
		if a == nil {
			return
		}
		entries, err := a.ListSkills(c.Param("name"))
		if err != nil {
			if errors.Is(err, agent.ErrSessionNotFound) {
				apperr.Abort(c, apperr.NewText(http.StatusNotFound, "project_not_found",
					map[string]any{"name": c.Param("name")}))
				return
			}
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"skills": entries})
	}
}

// queryIntDefault 解析整型 query 参数，缺省 / 非法回落默认值。
func queryIntDefault(c *gin.Context, name string, def int) int {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return def
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return parsed
}
