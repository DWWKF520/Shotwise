package http

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestRegisterMemoriesRoutes 校验路由树能容纳 /memories/user 等静态段与
// /memories/:memory_id 参数段并存（gin httprouter 对同层静态/参数混合有严格规则；
// 注册期不 panic 即证明契约可落地）。
func TestRegisterMemoriesRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 依赖为空：注册只构造闭包，不触发服务调用。
	RegisterMemories(r.Group("/api/v1"), MemoryDeps{})

	registered := map[string]bool{}
	for _, route := range r.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	want := []string{
		http.MethodGet + " /api/v1/memories/user",
		http.MethodPost + " /api/v1/memories/user",
		http.MethodDelete + " /api/v1/memories/user",
		http.MethodGet + " /api/v1/memories/export",
		http.MethodPost + " /api/v1/memories/import",
		http.MethodPatch + " /api/v1/memories/:memory_id",
		http.MethodDelete + " /api/v1/memories/:memory_id",
		http.MethodGet + " /api/v1/memory-candidates",
		http.MethodPost + " /api/v1/memory-candidates",
		http.MethodPost + " /api/v1/memory-candidates/:candidate_id/accept",
		http.MethodPost + " /api/v1/memory-candidates/:candidate_id/reject",
		http.MethodGet + " /api/v1/projects/:name/memories",
		http.MethodPost + " /api/v1/projects/:name/memories",
		http.MethodDelete + " /api/v1/projects/:name/memories",
		http.MethodPost + " /api/v1/projects/:name/memories/import",
	}
	for _, route := range want {
		if !registered[route] {
			t.Errorf("route not registered: %s", route)
		}
	}
}
