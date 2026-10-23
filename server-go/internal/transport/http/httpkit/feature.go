package httpkit

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/config"
)

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
