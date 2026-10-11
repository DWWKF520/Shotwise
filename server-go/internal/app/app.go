// Package app 装配 gin 引擎、进程级单例与 HTTP 路由。
package app

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/agent"
	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/service"
	transport "github.com/shotwise/shotwise/internal/transport/http"
	"gorm.io/gorm"
)

// Runtime 持有进程级单例，对应 Python 侧挂在 app.state 上的那批对象。
type Runtime struct {
	Config  *config.Config
	Log     *slog.Logger
	DB      *gorm.DB
	Dialect db.Dialect
	Store   *service.ProjectStore
	Assets  *service.AssetService
	Hub     *service.EventHub
	Auth    *auth.Service
	Agent   *agent.Runtime
}

// New 构造引擎并完成路由注册。
//
// 中间件注册顺序即执行顺序（gin 的 Use 是尾插），与 Python 侧「注册顺序相反」
// 的 add_middleware 语义不同，这里按最终外层→内层排列：
//
//	Recovery → SPA no-cache → 请求日志 → CORS → 路由
func New(rt *Runtime) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()

	r.Use(gin.Recovery())
	r.Use(spaNoCacheMiddleware())
	r.Use(requestLoggingMiddleware(rt.Log))
	r.Use(corsMiddleware(rt.Config.CORSOrigins))

	r.NoRoute(notFound)
	r.NoMethod(methodNotAllowed)

	r.GET("/health", transport.Health)

	v1 := r.Group("/api/v1")
	registerAuth(v1, rt)
	registerProjects(v1, rt)
	registerAgent(v1, rt)

	return r
}

// registerAuth 认证路由。status/token 必须公开（拿 token 之前就要可达）。
func registerAuth(v1 *gin.RouterGroup, rt *Runtime) {
	v1.GET("/auth/status", transport.AuthStatus(rt.Auth))
	v1.POST("/auth/token", transport.AuthToken(rt.Auth))

	authed := v1.Group("", rt.Auth.Middleware())
	authed.POST("/auth/logout", transport.AuthLogout())
	authed.GET("/auth/verify", transport.AuthVerify())
}

// registerProjects 项目路由。
//
// 注意 gin 的路由树限制：/projects/:name/events/stream 与 /projects/:name
// 前缀不冲突，但未来加入 /projects/:name/script-scenes/:id 时必须核对
// 与 /projects/:name/scenes/:entry 的匹配优先级（Python 侧 projects.py:1050
// 明确注释过这个陷阱）。
func registerProjects(v1 *gin.RouterGroup, rt *Runtime) {
	deps := transport.ProjectDeps{Store: rt.Store, Hub: rt.Hub}

	authed := v1.Group("", rt.Auth.Middleware())
	authed.GET("/projects", transport.ListProjects(deps))
	authed.POST("/projects", transport.CreateProject(deps))
	authed.GET("/projects/:name", transport.GetProject(deps))
	authed.PATCH("/projects/:name", transport.UpdateProject(deps))
	authed.DELETE("/projects/:name", transport.DeleteProject(deps))

	transport.RegisterAssets(authed, transport.AssetDeps{Assets: rt.Assets, Store: rt.Store})

	// SSE 走 flexible 鉴权：EventSource 无法自定义 header，仅支持 ?token=。
	sse := v1.Group("", rt.Auth.FlexibleMiddleware())
	sse.GET("/projects/:name/events/stream", transport.ProjectEventStream(deps))
}

// registerAgent 智能体运行时路由（含 SSE 流式问答）。
func registerAgent(v1 *gin.RouterGroup, rt *Runtime) {
	if rt.Agent == nil {
		return
	}
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterAgentRoutes(authed, rt.Agent)
}

func notFound(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"detail": "Not Found"})
}

func methodNotAllowed(c *gin.Context) {
	c.JSON(http.StatusMethodNotAllowed, gin.H{"detail": "Method Not Allowed"})
}
