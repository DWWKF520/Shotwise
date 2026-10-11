package app

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// corsMiddleware 对应 Python 侧的 CORSMiddleware。
// origins 为 ["*"] 时不带 credentials（与 Starlette 行为一致）。
func corsMiddleware(origins []string) gin.HandlerFunc {
	wildcard := len(origins) == 1 && origins[0] == "*"
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[o] = true
	}
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			if wildcard {
				c.Header("Access-Control-Allow-Origin", "*")
			} else if allowed[origin] {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Vary", "Origin")
				c.Header("Access-Control-Allow-Credentials", "true")
			}
		}
		c.Header("Access-Control-Allow-Methods", "*")
		c.Header("Access-Control-Allow-Headers", "*")
		c.Header("Access-Control-Max-Age", "600")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// spaNoCacheMiddleware 给 text/html 响应加 no-store，避免浏览器缓存 SPA 外壳后
// 拿到过期的前端资源引用。对应 Python 侧的 SPAShellNoCacheMiddleware。
func spaNoCacheMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if strings.Contains(c.Writer.Header().Get("Content-Type"), "text/html") {
			c.Writer.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		}
	}
}

// requestLoggingMiddleware 记录 method / path / status / 耗时。
//
// 与 Python 侧一致的降噪规则：
//   - /assets* 与 /health 跳过
//   - GET /api/v1/tasks* 在 2xx 且 <500ms 时降级到 Debug（轮询端点量太大）
func requestLoggingMiddleware(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		skip := path == "/health" || strings.HasPrefix(path, "/assets")
		noisy := c.Request.Method == http.MethodGet &&
			(strings.HasPrefix(path, "/api/v1/tasks") || strings.HasPrefix(path, "/api/v1/queue"))

		start := time.Now()
		c.Next()
		if skip {
			return
		}
		elapsed := time.Since(start)
		status := c.Writer.Status()
		attrs := []any{"method", c.Request.Method, "path", path, "status", status, "elapsed_ms", elapsed.Milliseconds()}
		if noisy && status < 300 && elapsed < 500*time.Millisecond {
			log.Debug("request", attrs...)
			return
		}
		if status >= 500 {
			log.Error("request", attrs...)
			return
		}
		log.Info("request", attrs...)
	}
}
