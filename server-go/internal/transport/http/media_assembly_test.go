package http

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRegisterMediaAssemblyRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterMediaAssembly(r.Group("/api/v1"), MediaAssemblyDeps{})

	registered := map[string]bool{}
	for _, route := range r.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	want := []string{
		http.MethodPost + " /api/v1/projects/:name/assembly-plans",
		http.MethodGet + " /api/v1/projects/:name/assembly-plans",
		http.MethodGet + " /api/v1/assembly-plans/:plan_id",
		http.MethodPost + " /api/v1/assembly-plans/:plan_id/revisions",
		http.MethodPost + " /api/v1/assembly-plans/:plan_id/status",
		http.MethodPost + " /api/v1/assembly-plans/:plan_id/preview-confirm",
		http.MethodPost + " /api/v1/assembly-plans/:plan_id/render-confirm",
		http.MethodPost + " /api/v1/assembly-plans/:plan_id/stale-check",
	}
	for _, route := range want {
		if !registered[route] {
			t.Errorf("route not registered: %s", route)
		}
	}
}
