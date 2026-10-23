package httpkit

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/agent"
	"github.com/shotwise/shotwise/internal/apperr"
)

// AbortBodyBind 把 JSON 绑定失败写成 FastAPI 风格的 422。
func AbortBodyBind(c *gin.Context, err error) {
	apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
		{Loc: []string{"body"}, Msg: err.Error(), Type: "value_error"},
	}})
}

// AbortMissingField 把缺失的必填字段写成 FastAPI 风格的 422。
func AbortMissingField(c *gin.Context, field string) {
	apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
		{Loc: []string{"body", field}, Msg: "field required", Type: "value_error.missing"},
	}})
}

// AbortFieldConstraint 把字段越界写成 FastAPI 风格的 422。
func AbortFieldConstraint(c *gin.Context, field, typ string) {
	apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
		{Loc: []string{"body", field}, Msg: "invalid value", Type: typ},
	}})
}

// AbortAssistant 把编排层错误映射为 HTTP 响应（与 Python 异常 → 状态码同口径）。
func AbortAssistant(c *gin.Context, err error, sessionID string) {
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
	case errors.Is(err, agent.ErrSkillNotInvocable):
		status = http.StatusBadRequest
		key, params = SkillNotInvocableMessage(err)
	default:
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	apperr.Abort(c, apperr.NewText(status, key, params))
}

// SkillNotInvocableMessage 把 skill 不可触发错误映射成 i18n key 与渲染参数。
// 项目里有可用 skill 时用带列表的 key（文案末尾列出全部可用 skill，便于直接补全）。
func SkillNotInvocableMessage(err error) (string, map[string]any) {
	var e *agent.SkillNotInvocableError
	if !errors.As(err, &e) {
		return "skill_not_invocable", map[string]any{"skill": ""}
	}
	if len(e.Available) == 0 {
		return "skill_not_invocable", map[string]any{"skill": e.Skill}
	}
	return "skill_not_invocable_available", map[string]any{
		"skill":     e.Skill,
		"available": strings.Join(e.Available, ", "),
	}
}

func sdkTypeFromErr(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ":"); i >= 0 {
		return strings.TrimSpace(msg[i+1:])
	}
	return ""
}
