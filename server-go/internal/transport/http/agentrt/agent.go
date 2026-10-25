package agentrt

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/agent"
	"github.com/shotwise/shotwise/internal/agent/eventlog"
	"github.com/shotwise/shotwise/internal/i18n"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"
)

// RegisterAgentRoutes 注册智能体运行时路由。
// POST /agent/query                              —— SSE 流式问答（提交一条用户消息）
// GET  /agent/skills?project=<name>              —— 项目内 user-invocable skill 列表（/ 前缀补全）
// GET  /agent/sessions                           —— 列出活跃会话
// GET  /agent/sessions/:session_id/entries       —— 冷读事件日志（?after=<seq> 游标）
// POST /agent/sessions/:session_id/interrupt     —— 中断当前轮
// POST /agent/sessions/:session_id/questions/:question_id/answer —— 回答待决问题（HITL）
func RegisterAgentRoutes(rg *gin.RouterGroup, rt *agent.Runtime) {
	rg.POST("/agent/query", func(c *gin.Context) {
		var body struct {
			SessionID string `json:"session_id"`
			Project   string `json:"project"`
			Message   string `json:"message"`
			// Skill 可选：显式触发的用户 skill 名（§17.5）。
			Skill string `json:"skill"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			abortBadRequest(c, "invalid_body", "请求体必须是 {session_id?, project?, message, skill?}")
			return
		}
		if body.Message == "" {
			abortBadRequest(c, "empty_message", "message 不能为空")
			return
		}

		// / 前缀识别：显式 skill 字段缺省且首 token 形如 /name 时，转成显式 skill 调用
		// （Claude Code slash command 语义）；正文取 /name 之后的参数部分。
		skill, message := body.Skill, body.Message
		if skill == "" {
			skill, message = agent.ParseSlashCommand(body.Message)
		}

		sessionID, evCh, unsub, err := rt.QueryWithSkill(c.Request.Context(), body.SessionID, body.Project, skill, message)
		if errors.Is(err, agent.ErrSkillNotInvocable) {
			abortSkillNotInvocable(c, err)
			return
		}
		if err != nil {
			abortInternal(c, "agent_start_failed", err.Error())
			return
		}

		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		c.Header("X-Session-Id", sessionID)

		// 首帧下发 session_id，便于前端建立关联。
		c.Writer.WriteString(agent.Event{SessionID: sessionID, Type: agent.EventStatus, Text: "running"}.EncodeSSE())
		c.Writer.Flush()

		httpkit.StreamAgentEvents(c, evCh, unsub)
	})

	rg.GET("/agent/skills", func(c *gin.Context) {
		project := strings.TrimSpace(c.Query("project"))
		if project == "" {
			abortBadRequest(c, "project_required", "缺少 project 查询参数")
			return
		}
		entries, err := rt.ListSkills(project)
		if err != nil {
			abortInternal(c, "skills_list_failed", err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"skills": entries})
	})

	rg.GET("/agent/sessions", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"sessions": rt.ListSessions()})
	})

	rg.GET("/agent/sessions/:session_id/entries", func(c *gin.Context) {
		sessionID := c.Param("session_id")
		after := httpkit.QueryAfterSeq(c)
		entries, err := rt.Entries(c.Request.Context(), sessionID, after)
		if err != nil {
			abortInternal(c, "entries_read_failed", err.Error())
			return
		}
		if entries == nil {
			entries = []*eventlog.Entry{}
		}
		status := "idle"
		if rt.IsActive(sessionID) {
			status = "running"
		}
		c.JSON(http.StatusOK, gin.H{
			"session_id": sessionID,
			"status":     status,
			"entries":    entries,
		})
	})

	rg.POST("/agent/sessions/:session_id/interrupt", func(c *gin.Context) {
		sessionID := c.Param("session_id")
		if err := rt.Interrupt(c.Request.Context(), sessionID); err != nil {
			// 会话未在运行：中断是幂等操作，返回当前状态而不是报错。
			c.JSON(http.StatusOK, gin.H{"status": "idle", "session_id": sessionID})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "interrupted", "session_id": sessionID})
	})

	rg.POST("/agent/sessions/:session_id/questions/:question_id/answer", func(c *gin.Context) {
		var body struct {
			Answers map[string]string `json:"answers"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			abortBadRequest(c, "invalid_body", "请求体必须是 {answers: {question_id: text}}")
			return
		}
		if len(body.Answers) == 0 {
			abortBadRequest(c, "answers_required", "answers 不能为空")
			return
		}
		sessionID := c.Param("session_id")
		questionID := c.Param("question_id")
		evCh, unsub, err := rt.AnswerQuestion(c.Request.Context(), sessionID, questionID, body.Answers)
		if err != nil {
			abortBadRequest(c, "session_question_unavailable", err.Error())
			return
		}
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		httpkit.StreamAgentEvents(c, evCh, unsub)
	})
}

// RegisterAgentStreamRoutes 注册需要 EventSource 访问的 SSE 路由。
// EventSource 无法自定义 header，只能走 flexible 鉴权（?token=）。
func RegisterAgentStreamRoutes(rg *gin.RouterGroup, rt *agent.Runtime) {
	// GET /agent/sessions/:session_id/entries/stream
	//
	// 游标优先级：EventSource 自动重连携带的 Last-Event-ID > ?after=<seq>。
	rg.GET("/agent/sessions/:session_id/entries/stream", func(c *gin.Context) {
		sessionID := c.Param("session_id")
		cursor := httpkit.QueryAfterSeq(c)
		if raw := strings.TrimSpace(c.GetHeader("Last-Event-ID")); raw != "" {
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
				cursor = parsed
			}
		}
		evCh, unsub, err := rt.Stream(c.Request.Context(), sessionID, cursor)
		if err != nil {
			abortInternal(c, "entries_stream_failed", err.Error())
			return
		}
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		c.Header("X-Session-Id", sessionID)
		httpkit.StreamAgentEvents(c, evCh, unsub)
	})
}

// abortBadRequest 是 agent 路由内的便捷错误响应。
func abortBadRequest(c *gin.Context, code, msg string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": code, "message": msg}})
}

// abortSkillNotInvocable 返回 400：本地化文案 + 项目内可用 skill 列表，客户端据此做补全提示。
func abortSkillNotInvocable(c *gin.Context, err error) {
	key, params := httpkit.SkillNotInvocableMessage(err)
	available := []string{}
	var skillErr *agent.SkillNotInvocableError
	if errors.As(err, &skillErr) && skillErr.Available != nil {
		available = skillErr.Available
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
		"code":      "skill_not_invocable",
		"message":   i18n.T(i18n.FromRequest(c.Request), key, params),
		"available": available,
	}})
}

func abortInternal(c *gin.Context, code, msg string) {
	c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": code, "message": msg}})
}
