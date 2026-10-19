// 镜头级分镜图 / 视频上传路由（对应 server/routers/shot_uploads.py）。
//
// Mount prefix: /api/v1/projects/:name/shots/:shot_id/upload/:kind
//
// upload 的 script_file 是查询参数（与 Python 签名一致）；kind 只接受 storyboard / video，
// 其余按 FastAPI 的 Literal 校验给 422。
package http

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/service"
)

// ShotUploadDeps 是镜头上传路由的依赖。
type ShotUploadDeps struct {
	Store   *service.ProjectStore
	Uploads *service.ShotUploadService
}

// RegisterShotUploads 注册镜头上传路由。
func RegisterShotUploads(group *gin.RouterGroup, d ShotUploadDeps) {
	if d.Uploads == nil {
		return
	}
	group.POST("/projects/:name/shots/:shot_id/upload/:kind", uploadShotMedia(d))
}

// uploadShotMedia 上传分镜图或镜头视频，替换该镜头的 AI 生成资产。
func uploadShotMedia(d ShotUploadDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		kind := service.ShotUploadKind(c.Param("kind"))
		if kind != service.ShotUploadStoryboard && kind != service.ShotUploadVideo {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"path", "kind"}, Msg: "Input should be 'storyboard' or 'video'", Type: "literal_error",
			}}})
			return
		}
		scriptFile := c.Query("script_file")
		fileHeader, err := c.FormFile("file")
		if err != nil {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"body", "file"}, Msg: "field required", Type: "missing",
			}}})
			return
		}
		declared := fileHeader.Size
		maxBytes, err := service.ValidateUpload(fileHeader.Filename, &declared, kind)
		if err != nil {
			abortShotUploadErr(c, err)
			return
		}

		var (
			content []byte
			stream  service.TargetWriter
		)
		if kind == service.ShotUploadStoryboard {
			opened, openErr := fileHeader.Open()
			if openErr != nil {
				apperr.Abort(c, apperr.Internal(openErr))
				return
			}
			defer func() { _ = opened.Close() }()
			// 限定读入内存的字节数：Content-Length 缺失/被绕过时不至于 OOM。
			content, openErr = readBounded(opened, maxBytes)
			if openErr != nil {
				abortShotUploadErr(c, openErr)
				return
			}
		} else {
			// 视频不进内存：边读边写到目标文件，超限即中断并清掉半截文件。
			stream = func(target string) error {
				opened, openErr := fileHeader.Open()
				if openErr != nil {
					return openErr
				}
				defer func() { _ = opened.Close() }()
				return writeStreamBounded(opened, target, maxBytes)
			}
		}

		out, err := d.Uploads.Upload(
			c.Request.Context(), c.Param("name"), kind, scriptFile, c.Param("shot_id"),
			fileHeader.Filename, content, stream)
		if err != nil {
			abortShotUploadErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// readBounded 读满上限内的全部字节；超限返回上传超限错误。
func readBounded(r io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, &service.UploadValidationError{Status: 413, Key: "upload_too_large", Params: map[string]any{
			"max_mb": maxBytes / (1024 * 1024),
		}}
	}
	return data, nil
}

// writeStreamBounded 把 reader 流式写到 target，超限即报错并删除半截文件。
func writeStreamBounded(r io.Reader, target string, maxBytes int64) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmp := target + ".part"
	file, err := os.Create(tmp)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(r, maxBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if written > maxBytes {
		_ = os.Remove(tmp)
		return &service.UploadValidationError{Status: 413, Key: "upload_too_large", Params: map[string]any{
			"max_mb": maxBytes / (1024 * 1024),
		}}
	}
	return os.Rename(tmp, target)
}

// abortShotUploadErr 映射上传域错误（校验 / 超限 / 路径穿越）。
func abortShotUploadErr(c *gin.Context, err error) {
	var (
		validation *service.UploadValidationError
		traversal  *fs.PathTraversalError
	)
	switch {
	case errors.As(err, &validation):
		apperr.Abort(c, apperr.NewText(validation.Status, validation.Key, validation.Params))
	case errors.As(err, &traversal):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "invalid_resource_id",
			map[string]any{"resource_id": c.Param("shot_id")}))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
