package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/auth"
)

// AuthStatus 暴露 AUTH_ENABLED 供前端 bootstrap 判断是否拦截登录。
// 响应形状与 Python 侧一致：{"enabled": bool}。
func AuthStatus(svc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"enabled": svc.Enabled()})
	}
}

// AuthToken 是 OAuth2 密码表单登录。
// 认证关闭时跳过凭据校验直接签发，让前端即便打开登录页也能进主界面。
func AuthToken(svc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		username := c.PostForm("username")
		password := c.PostForm("password")

		if svc.Enabled() && !svc.CheckCredentials(username, password) {
			c.Header("WWW-Authenticate", "Bearer")
			apperr.Abort(c, apperr.NewText(http.StatusUnauthorized, "invalid_credentials", nil))
			return
		}
		token, err := svc.CreateToken(username)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		role := "admin"
		if svc.Enabled() {
			role = svc.RoleOf(username)
		}
		c.JSON(http.StatusOK, gin.H{
			"access_token": token,
			"token_type":   "bearer",
			"role":         role,
		})
	}
}

// AuthVerify 校验 token 并回填 username/role。
func AuthVerify() gin.HandlerFunc {
	return func(c *gin.Context) {
		u := auth.Current(c)
		if u == nil {
			c.Header("WWW-Authenticate", "Bearer")
			apperr.Abort(c, apperr.NewText(http.StatusUnauthorized, "unauthorized", nil))
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"valid":    true,
			"username": u.Sub,
			"role":     u.Role,
		})
	}
}

// AuthLogout 登出。JWT 无状态，这里只返回 204（Python 侧同样只记日志）。
func AuthLogout() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	}
}
