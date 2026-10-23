// 镜头尾帧路由（对应 server/routers/end_frames.py）。
//
// Mount prefix: /api/v1/projects/:name/shots/:shot_id/end-frame
//
// upload / delete 的 script_file 是查询参数（Python 侧签名即 `script_file: str`），
// 只有 select 走 JSON body。三个端点共用同一套领域错误映射。
package media

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/infra/img"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/asset"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"
)

// EndFrameDeps 是尾帧路由的依赖。
type EndFrameDeps struct {
	Store    *service.ProjectStore
	EndFrame *asset.EndFrameService
}

// RegisterEndFrames 注册尾帧路由。
func RegisterEndFrames(group *gin.RouterGroup, d EndFrameDeps) {
	if d.EndFrame == nil {
		return
	}
	base := "/projects/:name/shots/:shot_id/end-frame"
	group.POST(base+"/upload", uploadEndFrame(d))
	group.POST(base+"/select", selectEndFrame(d))
	group.DELETE(base, deleteEndFrame(d))
}

// uploadEndFrame 上传任意图片作为该镜头的尾帧。
func uploadEndFrame(d EndFrameDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		scriptFile := c.Query("script_file")
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
		content, err := img.ReadAllBounded(opened, asset.EndFrameUploadMaxBytes)
		if err != nil {
			if errors.Is(err, img.ErrTooLarge) {
				apperr.Abort(c, apperr.NewText(http.StatusRequestEntityTooLarge, "upload_too_large", map[string]any{
					"max_megabytes": asset.EndFrameUploadMaxBytes / (1024 * 1024),
				}))
				return
			}
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		relative, err := d.EndFrame.SetFromBytes(
			c.Request.Context(), c.Param("name"), scriptFile, c.Param("shot_id"), content)
		if err != nil {
			abortEndFrameErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "end_frame_image": relative})
	}
}

// selectEndFrame 指定项目内已有图片的相对路径作为尾帧（快照复制，不建立引用）。
func selectEndFrame(d EndFrameDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			ScriptFile *string `json:"script_file"`
			SourcePath *string `json:"source_path"`
		}
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		var items []apperr.ValidationItem
		if body.ScriptFile == nil {
			items = append(items, apperr.ValidationItem{
				Loc: []string{"body", "script_file"}, Msg: "field required", Type: "missing"})
		}
		if body.SourcePath == nil {
			items = append(items, apperr.ValidationItem{
				Loc: []string{"body", "source_path"}, Msg: "field required", Type: "missing"})
		}
		if len(items) > 0 {
			apperr.Abort(c, &apperr.Validation{Items: items})
			return
		}
		relative, err := d.EndFrame.SetFromProjectImage(
			c.Request.Context(), c.Param("name"), *body.ScriptFile, c.Param("shot_id"), *body.SourcePath)
		if err != nil {
			abortEndFrameErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "end_frame_image": relative})
	}
}

// deleteEndFrame 清除该镜头的尾帧：删快照文件并把字段置空。
func deleteEndFrame(d EndFrameDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := d.EndFrame.Clear(
			c.Request.Context(), c.Param("name"), c.Query("script_file"), c.Param("shot_id"),
		); err != nil {
			abortEndFrameErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// abortEndFrameErr 把尾帧领域错误映射成统一 {"detail": ...} 信封。
func abortEndFrameErr(c *gin.Context, err error) {
	var endFrameErr *asset.EndFrameError
	if !errors.As(err, &endFrameErr) {
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	apperr.Abort(c, apperr.NewText(endFrameErr.Status, endFrameErr.Key, endFrameErr.Params))
}
