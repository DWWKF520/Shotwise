package media

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRegisterPublishingRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterPublishing(r.Group("/api/v1"), PublishingDeps{})

	registered := map[string]bool{}
	for _, route := range r.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	want := []string{
		http.MethodGet + " /api/v1/publishing/platforms",
		http.MethodGet + " /api/v1/publishing/accounts",
		http.MethodPost + " /api/v1/publishing/accounts",
		http.MethodDelete + " /api/v1/publishing/accounts/:account_id",
		http.MethodPost + " /api/v1/render-artifacts/:artifact_id/publish",
		http.MethodGet + " /api/v1/publish-jobs",
		http.MethodGet + " /api/v1/publish-jobs/:job_id",
		http.MethodPost + " /api/v1/publish-jobs/:job_id/retry",
		http.MethodPost + " /api/v1/publish-jobs/:job_id/cancel",
		http.MethodPost + " /api/v1/publish-jobs/:job_id/poll",
		http.MethodPost + " /api/v1/publish-jobs/:job_id/retract",
	}
	for _, route := range want {
		if !registered[route] {
			t.Errorf("route not registered: %s", route)
		}
	}
}
