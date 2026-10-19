// 版本查询 / 还原路由（对应 server/routers/versions.py）。
//
// Mount prefix: /api/v1/projects/:name/versions/:resource_type/:resource_id
//
// 只有白名单内的资源类型可还原（storyboards / videos / characters / scenes / props /
// products / reference_videos / grids）；路径形状委托 service.resourcePatterns，本层
// 只做参数解析与错误映射。
package http

import (
	"errors"
	"net/http"
	"os"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/service"
)

// VersionDeps 是版本路由的依赖。
type VersionDeps struct {
	Versions *service.VersionService
}

// RegisterVersions 注册版本查询与还原路由。
func RegisterVersions(group *gin.RouterGroup, d VersionDeps) {
	if d.Versions == nil {
		return
	}
	base := "/projects/:name/versions/:resource_type/:resource_id"
	group.GET(base, getVersions(d))
	group.POST(base+"/restore/:version", restoreVersion(d))
}

// getVersions 返回资源的所有版本列表。
func getVersions(d VersionDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := d.Versions.List(c.Param("name"), c.Param("resource_type"), c.Param("resource_id"))
		if err != nil {
			abortVersionErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// restoreVersion 把资源切回指定版本。
func restoreVersion(d VersionDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		version, err := strconv.Atoi(c.Param("version"))
		if err != nil {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"path", "version"}, Msg: "value is not a valid integer", Type: "int_parsing"},
			}})
			return
		}
		out, err := d.Versions.Restore(c.Param("name"), c.Param("resource_type"), c.Param("resource_id"), version)
		if err != nil {
			abortVersionErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// abortVersionErr 把版本域错误映射成统一 {"detail": ...} 信封。
//
// 非法项目名 / 越界 resource_id / 损坏的 versions.json 在 Python 侧都是 ValueError，
// 统一翻成 400 request_invalid（原始 str(e) 可能含路径，只进日志）；只有版本文件
// 实际缺失（safe_join require_file）才冒到 500。
func abortVersionErr(c *gin.Context, err error) {
	var (
		resourceMissing *service.VersionResourceNotFoundError
		versionMissing  *service.VersionNotFoundError
		traversal       *fs.PathTraversalError
	)
	// unsupported_resource_type / invalid_resource_id 的文案里带占位符，params 只能由
	// 路径参数补齐——服务层的哨兵错误不带值。
	resourceType := c.Param("resource_type")
	resourceID := c.Param("resource_id")
	switch {
	case errors.Is(err, service.ErrUnsupportedResourceType):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "unsupported_resource_type", map[string]any{"resource_type": resourceType}))
	case errors.Is(err, service.ErrAdGridNotSupported):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "ad_grid_not_supported", nil))
	case errors.Is(err, service.ErrGridStoryboardNotEnabled):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "grid_storyboard_not_enabled", nil))
	case errors.Is(err, service.ErrInvalidResourceID):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "invalid_resource_id", map[string]any{"resource_id": resourceID}))
	case errors.As(err, &resourceMissing):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "version_resource_not_found", map[string]any{
			"resource_type": resourceMissing.ResourceType,
			"resource_id":   resourceMissing.ResourceID,
		}))
	case errors.As(err, &versionMissing):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "version_not_found", map[string]any{"version": versionMissing.Version}))
	case errors.As(err, &traversal):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
	case errors.Is(err, os.ErrNotExist):
		// 版本记录在账本里但快照文件已被删除：Python 侧 FileNotFoundError 直接 5xx。
		apperr.Abort(c, apperr.Internal(err))
	default:
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
	}
}
