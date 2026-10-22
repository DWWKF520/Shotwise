// 宫格图路由（对应 server/routers/grids.py）。
//
// Mount prefix: /api/v1/projects/:name
//
// 路由树注意：/grid-capability 是静态段，必须与 /grids/:grid_id 同级注册且 gin 按静态
// 优先匹配；它刻意不挂在 /grids/ 之下（否则会被 :grid_id 抢走）。
//
// 错误信封：全部走 apperr.NewText（Python 侧是 ApiError 家族 → app 级 handler 翻字符串），
// 只有 grid_generation_in_progress 是 409。
package http

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/i18n"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/infra/img"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/generation"
)

// GridDeps 是宫格路由的依赖。
type GridDeps struct {
	Store *service.ProjectStore
	Grids *generation.GridTaskService
}

// RegisterGrids 注册宫格路由。
func RegisterGrids(group *gin.RouterGroup, d GridDeps) {
	if d.Grids == nil {
		return
	}
	base := "/projects/:name"
	group.POST(base+"/generate/grid/:episode", generateGrid(d))
	group.GET(base+"/grid-capability", gridCapability(d))
	group.GET(base+"/grids", listGrids(d))
	group.GET(base+"/grids/:grid_id", getGrid(d))
	group.POST(base+"/grids/:grid_id/regenerate", regenerateGrid(d))
	group.POST(base+"/grids/:grid_id/split", splitGrid(d))
	group.POST(base+"/grids/:grid_id/upload", uploadGridImage(d))
}

// generateGrid 按分镜分组提交宫格生成任务。
func generateGrid(d GridDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		var body struct {
			ScriptFile *string  `json:"script_file"`
			SceneIDs   []string `json:"scene_ids"`
		}
		if !decodeJSONBody(c, &body) {
			return
		}
		if body.ScriptFile == nil {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"body", "script_file"}, Msg: "field required", Type: "missing",
			}}})
			return
		}
		out, err := d.Grids.Generate(
			c.Request.Context(), c.Param("name"), episode, *body.ScriptFile, body.SceneIDs, userID(c))
		writeGridResult(c, out, err)
	}
}

// gridCapability 返回当前项目的宫格档位上限。
func gridCapability(d GridDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		out, err := d.Grids.Capability(c.Request.Context(), c.Param("name"))
		writeGridResult(c, out, err)
	}
}

// listGrids 列出项目下全部宫格记录。
func listGrids(d GridDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		out, err := d.Grids.List(c.Param("name"))
		if err != nil {
			abortGridErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// getGrid 读单条宫格记录。
func getGrid(d GridDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		grid, err := d.Grids.GridData(c.Param("name"), c.Param("grid_id"))
		if err != nil {
			abortGridErr(c, err)
			return
		}
		c.JSON(http.StatusOK, grid)
	}
}

// regenerateGrid 重置宫格状态并重新入队。
func regenerateGrid(d GridDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		out, err := d.Grids.Regenerate(
			c.Request.Context(), c.Param("name"), c.Param("grid_id"), userID(c))
		writeGridResult(c, out, err)
	}
}

// splitGrid 按当前联合图切分并覆写各分镜格。
func splitGrid(d GridDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		out, err := d.Grids.Split(c.Request.Context(), c.Param("name"), c.Param("grid_id"))
		writeGridResult(c, out, err)
	}
}

// uploadGridImage 上传联合图替换当前宫格图（仅归一化，不缩放不校验布局）。
func uploadGridImage(d GridDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		fileHeader, err := c.FormFile("file")
		if err != nil {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"body", "file"}, Msg: "field required", Type: "missing",
			}}})
			return
		}
		opened, err := fileHeader.Open()
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		defer func() { _ = opened.Close() }()

		content, err := img.ReadAllBounded(opened, maxImageUploadBytes)
		if err != nil {
			if errors.Is(err, img.ErrTooLarge) {
				apperr.Abort(c, apperr.NewText(http.StatusRequestEntityTooLarge, "upload_too_large", map[string]any{
					"max_megabytes": maxImageUploadBytes / (1024 * 1024),
				}))
				return
			}
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		// 联合图分辨率即信息量：不缩放，只做格式/方向归一化。
		pngBytes, err := img.NormalizeToPNG(content, nil)
		if err != nil {
			if errors.Is(err, img.ErrPixelLimit) {
				apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "image_pixels_too_large", map[string]any{
					"max_megapixels": img.MaxUploadPixels / 1_000_000,
				}))
				return
			}
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "invalid_image_file", nil))
			return
		}
		out, err := d.Grids.UploadGridImage(
			c.Request.Context(), c.Param("name"), c.Param("grid_id"), fileHeader.Filename, pngBytes)
		writeGridResult(c, out, err)
	}
}

// maxImageUploadBytes 是图片上传的字节上限（与 Python validate_upload 的 image 档一致）。
const maxImageUploadBytes = 30 << 20

// writeGridResult 统一渲染 message 并写响应。
func writeGridResult(c *gin.Context, out map[string]any, err error) {
	if err != nil {
		abortGridErr(c, err)
		return
	}
	loc := i18n.FromRequest(c.Request)
	if key, ok := out["message_key"].(string); ok {
		params, _ := out["message_params"].(map[string]any)
		out["message"] = i18n.T(loc, key, params)
	}
	delete(out, "message_key")
	delete(out, "message_params")
	c.JSON(http.StatusOK, out)
}

// abortGridErr 把宫格域错误映射成统一信封。
func abortGridErr(c *gin.Context, err error) {
	var (
		generateErr *service.GenerateError
		traversal   *fs.PathTraversalError
	)
	switch {
	case errors.As(err, &generateErr):
		apperr.Abort(c, apperr.NewText(generateErr.Status, generateErr.Code, generateErr.Params))
	case errors.As(err, &traversal):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "invalid_project_name",
			map[string]any{"name": c.Param("name")}))
	case errors.Is(err, io.ErrUnexpectedEOF):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "invalid_image_file", nil))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
