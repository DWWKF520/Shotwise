package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/agent"
)

// RegisterAgentRoutes 注册智能体运行时相关路由。
// POST /api/v1/agent/query  —— SSE 流式问答
// GET  /api/v1/agent/sessions —— 列出活跃会话
func RegisterAgentRoutes(rg *gin.RouterGroup, rt *agent.Runtime) {
	rg.POST("/agent/query", func(c *gin.Context) {
		var body struct {
			SessionID string `json:"session_id"`
			Project   string `json:"project"`
			Message   string `json:"message"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			abortBadRequest(c, "invalid_body", "请求体必须是 {session_id?, project?, message}")
			return
		}
		if body.Message == "" {
			abortBadRequest(c, "empty_message", "message 不能为空")
			return
		}

		sessionID, evCh, unsub, err := rt.Query(c.Request.Context(), body.SessionID, body.Project, body.Message)
		if err != nil {
			abortInternal(c, "agent_start_failed", err.Error())
			return
		}

		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Session-Id", sessionID)

		// 首帧下发 session_id，便于前端建立关联。
		c.Writer.WriteString(agent.Event{SessionID: sessionID, Type: agent.EventStatus, Text: "running"}.EncodeSSE())
		c.Writer.Flush()

		flusher := c.Writer
		defer unsub()
		for {
			select {
			case ev, ok := <-evCh:
				if !ok {
					return // loop 退出（bus 关闭）或已退订
				}
				flusher.WriteString(ev.EncodeSSE())
				flusher.Flush()
			case <-c.Request.Context().Done():
				// 客户端断开：仅退订本连接；query 已入队，loop 由 Runtime 空闲策略收口。
				return
			}
		}
	})

	rg.GET("/agent/sessions", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"sessions": rt.ListSessions()})
	})
}

// abortBadRequest 是 agent 路由内的便捷错误响应。
func abortBadRequest(c *gin.Context, code, msg string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": code, "message": msg}})
}

func abortInternal(c *gin.Context, code, msg string) {
	c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": code, "message": msg}})
}
