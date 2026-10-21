package http

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRegisterCreativeBoardsRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterCreativeBoards(r.Group("/api/v1"), CreativeBoardDeps{})

	registered := map[string]bool{}
	for _, route := range r.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	want := []string{
		http.MethodPost + " /api/v1/projects/:name/creative-boards",
		http.MethodGet + " /api/v1/projects/:name/creative-boards",
		http.MethodGet + " /api/v1/creative-boards/:board_id",
		http.MethodPut + " /api/v1/creative-boards/:board_id/snapshot",
		http.MethodPatch + " /api/v1/creative-boards/:board_id",
		http.MethodDelete + " /api/v1/creative-boards/:board_id",
		http.MethodPost + " /api/v1/creative-boards/:board_id/copy",
		http.MethodPost + " /api/v1/creative-boards/:board_id/versions",
		http.MethodGet + " /api/v1/creative-boards/:board_id/versions",
		http.MethodGet + " /api/v1/creative-boards/:board_id/versions/:version_id",
		http.MethodPost + " /api/v1/creative-boards/:board_id/versions/:version_id/restore",
		http.MethodPost + " /api/v1/creative-boards/:board_id/items",
		http.MethodPatch + " /api/v1/creative-boards/:board_id/items/:item_id",
		http.MethodDelete + " /api/v1/creative-boards/:board_id/items/:item_id",
		http.MethodPost + " /api/v1/creative-boards/:board_id/edges",
		http.MethodDelete + " /api/v1/creative-boards/:board_id/edges/:edge_id",
	}
	for _, route := range want {
		if !registered[route] {
			t.Errorf("route not registered: %s", route)
		}
	}
}
