// 同步 Agent 对话端点（对应 server/routers/agent_chat.py）。
//
// 封装流式助手为同步请求-响应模式，供 OpenClaw 等外部 Agent 调用：
// 发送（send_or_create）→ 订阅直播流收集 assistant 文本 → 终态或超时收口 →
// 事件日志兜底补齐（订阅建立前已完成的回复、流异常收尾的截断补偿）。
package http

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/agent"
	"github.com/shotwise/shotwise/internal/agent/eventlog"
	"github.com/shotwise/shotwise/internal/apperr"
)

// syncChatTimeout 与 Python SYNC_CHAT_TIMEOUT 同值：超过后返回已收集的部分响应，
// status 为 "timeout"。
const syncChatTimeout = 120 * time.Second

// agentChatRequest 与 Python AgentChatRequest 同形。
type agentChatRequest struct {
	ProjectName string `json:"project_name"`
	Message     string `json:"message"`
	SessionID   string `json:"session_id"`
}

// agentChatResponse 与 Python AgentChatResponse 同形。
type agentChatResponse struct {
	SessionID string `json:"session_id"`
	Reply     string `json:"reply"`
	Status    string `json:"status"`    // completed | timeout | error | interrupted
	Truncated bool   `json:"truncated"` // 回复可能不完整且未能从事件日志确认补齐
}

// agentChat 处理 POST /agent/chat：同步收集完整回复后一次性返回。
func agentChat(d AssistantDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		a := d.Assistant
		if a == nil {
			apperr.Abort(c, apperr.NewText(http.StatusServiceUnavailable, "session_capacity_exceeded", nil))
			return
		}
		var body agentChatRequest
		if err := c.ShouldBindJSON(&body); err != nil {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
			return
		}
		if body.ProjectName == "" || body.Message == "" {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
			return
		}
		if !d.Store.Exists(body.ProjectName) {
			apperr.Abort(c, apperr.NewText(http.StatusNotFound, "project_not_found",
				map[string]any{"name": body.ProjectName}))
			return
		}
		userID := currentUserID(c)

		// 若传入 session_id，先校验会话归属（跨项目 / 不存在均 404）。
		if body.SessionID != "" {
			meta, err := a.GetSession(c.Request.Context(), body.SessionID, userID)
			if err != nil {
				apperr.Abort(c, apperr.Internal(err))
				return
			}
			if meta == nil || meta.ProjectName != body.ProjectName {
				apperr.Abort(c, apperr.NewText(http.StatusNotFound, "session_not_found",
					map[string]any{"session_id": body.SessionID}))
				return
			}
		}

		// 统一经 send_or_create 受理（幂等键不适用：同步端点每次调用即一次对话）。
		result, err := a.SendCreate(c.Request.Context(), body.ProjectName, body.Message,
			body.SessionID, "", "claude", userID)
		if err != nil {
			abortAssistant(c, err, body.SessionID)
			return
		}
		sessionID := result.SessionID

		// 收集回复（带超时）。
		reply, status := collectAgentChatReply(c.Request.Context(), a, sessionID, syncChatTimeout)
		truncated := false

		// 事件日志是唯一可靠读源，两类情形回读兜底：直播收集为空（订阅前已完成 /
		// 部分广播错过）；流异常收尾（error / interrupted，直播文本可能被截断）。
		streamAborted := status != "completed" && status != "timeout"
		if reply == "" || streamAborted {
			userSeq := int64(-1)
			if result.Entry != nil {
				userSeq = result.Entry.Seq
			}
			if page, perr := a.ListEntries(c.Request.Context(), sessionID, userSeq, userID); perr == nil {
				logReply := extractReplyFromEntries(page.Entries, userSeq)
				sessionRunning := page.Status == "running"
				if reply == "" {
					// 直播收集为空：采用日志内容（日志同样为空则维持空回复）；
					// 异常收尾且会话仍在 running 时，日志内容同样未收全，不能当作已确认完整。
					reply = logReply
					truncated = streamAborted && sessionRunning && logReply != ""
				} else if !sessionRunning && len(logReply) >= len(reply) {
					// 会话已终态且日志不短于直播：确认完整，整体替换；否则保留
					// 直播文本并标记截断，避免被更短、更旧的日志内容静默覆盖。
					reply = logReply
				} else {
					truncated = streamAborted
				}
			} else if reply != "" {
				truncated = streamAborted
			}
		}

		c.JSON(http.StatusOK, agentChatResponse{
			SessionID: sessionID,
			Reply:     reply,
			Status:    status,
			Truncated: truncated,
		})
	}
}

// collectAgentChatReply 消费会话事件流收集 assistant 回复，直到终态或超时。
//
// 终止条件（对应 Python _collect_reply）：
//   - deadline：每轮检查——持续刷屏的会话不能让同步请求无界挂起；
//   - status 终态事件（idle / completed / error / cancelled）；
//   - 流关闭（loop 退出 / bus 关闭）→ 已收即所得。
//
// 返回 (回复文本, status)；status 为 completed / timeout / error / interrupted。
func collectAgentChatReply(ctx context.Context, a *agent.AssistantService, sessionID string, timeout time.Duration) (string, string) {
	// 先建立订阅再检查活跃性：订阅在受理侧 dispatch 之后，此处会话必然在运行或刚收口。
	ch, unsub, err := a.SubscribeLive(ctx, sessionID)
	if err != nil {
		// 会话已不在运行（回复在订阅前完成）：直接走日志兜底路径。
		return "", "completed"
	}
	defer unsub()

	var parts []string
	status := "timeout"
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			return strings.Join(parts, ""), "timeout"
		case <-ctx.Done():
			return strings.Join(parts, ""), "timeout"
		case ev, ok := <-ch:
			if !ok {
				// 流关闭：loop 退出。按已收即所得收尾，终态由日志兜底判定。
				if status == "timeout" {
					status = "completed"
				}
				return strings.Join(parts, ""), status
			}
			if ev.Type == agent.EventDelta && ev.Text != "" {
				parts = append(parts, ev.Text)
				continue
			}
			if ev.Type == agent.EventStatus && ev.Text != "" && ev.Text != "running" {
				switch ev.Text {
				case "idle", "completed", "done":
					return strings.Join(parts, ""), "completed"
				case "error":
					return strings.Join(parts, ""), "error"
				case "cancelled":
					return strings.Join(parts, ""), "interrupted"
				}
			}
		}
	}
}

// extractReplyFromEntries 从事件日志条目提取主线 assistant 回复文本（本轮用户条目之后）。
// 只取无 parent_tool_use_id 的 assistant 条目（subagent 输出不算主线回复）。
func extractReplyFromEntries(entries []*eventlog.Entry, afterSeq int64) string {
	var sb strings.Builder
	for _, e := range entries {
		if e.Type != eventlog.EntryAssistant || e.ParentToolUseID != "" {
			continue
		}
		if e.Seq <= afterSeq {
			continue
		}
		for _, b := range e.Content {
			if b.Type == "text" && b.Text != "" {
				sb.WriteString(b.Text)
			}
		}
	}
	return sb.String()
}
