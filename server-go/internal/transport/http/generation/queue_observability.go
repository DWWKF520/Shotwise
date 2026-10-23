// 队列可观测性只读路由（对应 server/routers/queue_observability.py）。
//
// 两个端点只读，不含调度副作用（ADR 0007：孤儿任务不自动重排）。告警文案由前端按
// code 本地化，本层不输出面向用户的自然语言。
package generation

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/service/generation"
)

// QueueObservabilityDeps 是可观测性路由的依赖。
type QueueObservabilityDeps struct {
	Obs *generation.QueueObservability
}

// RegisterQueueObservability 注册队列诊断路由。
func RegisterQueueObservability(group *gin.RouterGroup, d QueueObservabilityDeps) {
	if d.Obs == nil {
		return
	}
	group.GET("/queue/observability", getQueueObservability(d))
	group.GET("/tasks/:task_id/queue-position", getTaskQueuePosition(d))
}

// getQueueObservability 返回队列诊断快照；未启动 worker 时省掉泳道与 ETA 容量分母。
func getQueueObservability(d QueueObservabilityDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		snapshot, err := d.Obs.BuildSnapshot(c.Request.Context(), optionalQuery(c, "project_name"),
			generation.ObservabilityOptions{})
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusOK, snapshot)
	}
}

// getTaskQueuePosition 返回任务排队名次（0 基）；任务不存在时 404。
func getTaskQueuePosition(d QueueObservabilityDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		taskID := c.Param("task_id")
		position, err := d.Obs.TaskQueuePosition(c.Request.Context(), taskID)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		if position == nil {
			apperr.Abort(c, apperr.NewText(http.StatusNotFound, "task_not_found", map[string]any{"id": taskID}))
			return
		}
		c.JSON(http.StatusOK, position)
	}
}
