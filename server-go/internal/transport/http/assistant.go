// 助手会话路由（对应 server/routers/assistant.py）——仅覆盖与本实现语义一致的端点。
//
// **范围与边界**：Python 的 assistant 是「持久会话」模型（meta store + 事件日志 +
// client_key 幂等 + rewrite 前缀分叉），Go 侧 internal/agent 是「在程会话」模型
// （TurnLoop + 内存会话表 + eventlog）。两者的 sessions/entries 形状不同，不能靠改前缀
// 直接对齐——那会让前端拿到结构错误的 200。因此本文件只注册两类端点：
//   1. Python 侧已声明下线的接口（messages / snapshot → 410 interface_offline）；
//   2. 未来随会话存储迁移补齐的端点占位（send / rewrite / switch-agent / 列表 / 详情 /
//      删除）**刻意不注册**：半兼容的 200 比诚实的 404 更伤前端。
//
// 其余端点随 P5 会话存储迁移一并落地（见 00-overview.md P5 行）。
package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
)

// AssistantDeps 是助手会话路由的依赖。
type AssistantDeps struct{}

// RegisterAssistant 注册助手会话路由（当前仅下线接口的 410 响应）。
func RegisterAssistant(group *gin.RouterGroup, d AssistantDeps) {
	base := "/projects/:name/assistant"
	group.GET(base+"/sessions/:session_id/messages", assistantGone(d))
	group.GET(base+"/sessions/:session_id/snapshot", assistantGone(d))
}

// assistantGone 返回 410 interface_offline：这两个接口在 Python 侧也已声明下线，
// 前端保留调用路径只是为了探测。
func assistantGone(d AssistantDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		apperr.Abort(c, apperr.NewText(http.StatusGone, "interface_offline", nil))
	}
}
