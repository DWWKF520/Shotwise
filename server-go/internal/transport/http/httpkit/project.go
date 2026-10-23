package httpkit

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/service"
)

// EnsureProject 对应 Python 路由里的 `_ensure_project`：项目名非法 → 400，
// 项目不存在 → 404。返回 false 时已写出错误响应。
func EnsureProject(c *gin.Context, store *service.ProjectStore, name string) bool {
	if _, err := project.ValidateProjectName(name); err != nil {
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "invalid_project_name", map[string]any{"name": name}))
		return false
	}
	if !store.Exists(name) {
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "project_not_found", map[string]any{"name": name}))
		return false
	}
	return true
}

// EpisodeParam 解析 :episode；非法时写出 FastAPI 风格的 422。
func EpisodeParam(c *gin.Context) (int, bool) {
	raw := c.Param("episode")
	value, err := strconv.Atoi(raw)
	if err != nil {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"path", "episode"}, Msg: "value is not a valid integer", Type: "int_parsing"},
		}})
		return 0, false
	}
	return value, true
}

// RequireExistingProject 项目不存在时写出 404 并返回 false。
func RequireExistingProject(c *gin.Context, store *service.ProjectStore) bool {
	if !store.Exists(c.Param("name")) {
		apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "project_not_found"))
		return false
	}
	return true
}
