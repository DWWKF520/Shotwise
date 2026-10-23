package httpkit

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/agent"
)

// StreamAgentEvents 把事件流写成 SSE 帧，直到流关闭或客户端断开。
func StreamAgentEvents(c *gin.Context, evCh <-chan agent.Event, unsub func()) {
	w := c.Writer
	defer unsub()
	ticker := time.NewTicker(agentHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case ev, ok := <-evCh:
			if !ok {
				return // loop 退出（bus 关闭）或已退订
			}
			if _, err := w.WriteString(ev.EncodeSSE()); err != nil {
				return
			}
			w.Flush()
		case <-ticker.C:
			if _, err := w.WriteString(": keepalive\n\n"); err != nil {
				return
			}
			w.Flush()
		case <-c.Request.Context().Done():
			// 客户端断开：仅退订本连接；已入队的轮次由 Runtime 空闲策略收口。
			return
		}
	}
}

// agentHeartbeatInterval 是 SSE 心跳间隔：连接空闲时用注释帧保活，防止代理断连。
const agentHeartbeatInterval = 15 * time.Second
