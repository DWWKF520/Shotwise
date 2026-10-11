// Package http 是 gin handler 层。
package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Health 返回服务健康状态。无前缀端点，不鉴权。
func Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"service": "shotwise",
	})
}
