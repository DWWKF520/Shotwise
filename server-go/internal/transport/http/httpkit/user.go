package httpkit

import (
	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/auth"
)

// CurrentUserID 取上下文里的用户标识；缺省回落 default（单用户模式）。
func CurrentUserID(c *gin.Context) string {
	if u := auth.Current(c); u != nil && u.ID != "" {
		return u.ID
	}
	return "default"
}
