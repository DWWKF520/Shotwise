package http

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
)

// currentUserID 取上下文里的用户标识；缺省回落 default（单用户模式）。
func currentUserID(c *gin.Context) string {
	if u := auth.Current(c); u != nil && u.ID != "" {
		return u.ID
	}
	return "default"
}

// ensureProject 对应 Python 路由里的 `_ensure_project`：项目名非法 → 400，
// 项目不存在 → 404。返回 false 时已写出错误响应。
func ensureProject(c *gin.Context, store *service.ProjectStore, name string) bool {
	if _, err := domain.ValidateProjectName(name); err != nil {
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "invalid_project_name", map[string]any{"name": name}))
		return false
	}
	if !store.Exists(name) {
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "project_not_found", map[string]any{"name": name}))
		return false
	}
	return true
}

// RequireFeature 是功能开关守卫中间件：未开启时返回 404
// {"code":"feature_disabled","feature":name}（与 Python 侧 _require_feature 一致）。
func RequireFeature(name string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !config.FeatureEnabled(name) {
			apperr.Abort(c, apperr.New(http.StatusNotFound, "feature_disabled", map[string]any{"feature": name}))
			return
		}
		c.Next()
	}
}

// isoTimestamp 近似 Python datetime.isoformat()：小数秒按微秒输出，为 0 时省略。
func isoTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.999999-07:00")
}
