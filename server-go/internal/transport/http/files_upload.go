// 文件上传路由（对应 server/routers/files.py 的 upload 与 style-image 端点）。
//
// 覆盖范围：
//   - POST /projects/:name/upload/source：源文上传，经 SourceLoader 规范化为 UTF-8 .txt。
//   - POST /projects/:name/style-image：风格参考图上传 + Gemini 视觉风格分析。
//   - 角色/场景/道具/产品的图片与音频上传尚未迁移，显式返回 501 而不是静默落盘。
package http

import (
	"errors"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/i18n"
	"github.com/shotwise/shotwise/internal/infra/img"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/textgen"
)

// FileUploadDeps 是上传路由的依赖。
type FileUploadDeps struct {
	Store *service.ProjectStore
	DB    *gorm.DB
}

// styleImageMaxBytes 是风格参考图的请求体上限（与源文上限同值，避免两条上传链路口径分裂）。
const styleImageMaxBytes = service.SourceMaxBytes

// styleImageMediaTypes 是风格参考图允许的扩展名及其 MIME 类型。
var styleImageMediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
}

// uploadTypesPending 是尚未迁移的资产类上传类型（Python UPLOAD_SPECS 中除 source 外的条目）。
var uploadTypesPending = map[string]bool{
	"character": true, "character_ref": true, "character_audio_ref": true,
	"scene": true, "prop": true, "product": true, "product_ref": true,
}

// RegisterFileUploads 注册上传端点（需鉴权）。
func RegisterFileUploads(group *gin.RouterGroup, d FileUploadDeps) {
	if d.Store == nil {
		return
	}
	base := "/projects/:name"
	group.POST(base+"/upload/:type", uploadProjectFile(d))
	group.POST(base+"/style-image", uploadStyleImage(d))
}

// uploadProjectFile 处理 POST /projects/:name/upload/:type。
func uploadProjectFile(d FileUploadDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		if !ensureProject(c, d.Store, name) {
			return
		}
		uploadType := c.Param("type")
		switch {
		case uploadType == "source":
			uploadSource(c, d, name)
		case uploadTypesPending[uploadType]:
			apperr.Abort(c, apperr.NewLiteral(http.StatusNotImplemented, "upload_type_not_migrated"))
		default:
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "invalid_upload_type",
				map[string]any{"upload_type": uploadType}))
		}
	}
}

// uploadSource 是 source 分支：规范化源文并按需备份原始字节。
func uploadSource(c *gin.Context, d FileUploadDeps, name string) {
	header, err := c.FormFile("file")
	if err != nil || header.Filename == "" {
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "missing_filename", nil))
		return
	}
	onConflict := c.DefaultQuery("on_conflict", service.SourceConflictFail)
	switch onConflict {
	case service.SourceConflictFail, service.SourceConflictReplace, service.SourceConflictRename:
	default:
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
			Loc: []string{"query", "on_conflict"}, Msg: "Input should be 'fail', 'replace' or 'rename'",
			Type: "enum",
		}}})
		return
	}
	dir, err := d.Store.Dir(name)
	if err != nil {
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	result, err := saveSourceUpload(header, filepath.Join(dir, "source"), onConflict)
	if err != nil {
		abortSourceError(c, err)
		return
	}
	fileName := filepath.Base(result.NormalizedPath)
	relPath := "source/" + fileName
	var usedEncoding any
	if result.UsedEncoding != "" {
		usedEncoding = result.UsedEncoding
	}
	c.JSON(http.StatusOK, gin.H{
		"success":           true,
		"filename":          fileName,
		"path":              relPath,
		"url":               "/api/v1/files/" + name + "/" + relPath,
		"normalized":        true,
		"original_kept":     result.RawPath != "",
		"original_filename": result.OriginalFilename,
		"used_encoding":     usedEncoding,
		"chapter_count":     result.ChapterCount,
	})
}

// saveSourceUpload 把上传体落到临时文件（有字节上限），交给 SourceLoader 规范化。
// 临时文件在返回前删除，任何路径都不泄漏。
func saveSourceUpload(h *multipart.FileHeader, sourceDir, onConflict string) (*service.SourceNormalizeResult, error) {
	in, err := h.Open()
	if err != nil {
		return nil, err
	}
	defer in.Close()

	tmp, err := os.CreateTemp("", "shotwise-source-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	// 多读 1 字节：超限时 LoadSource 的 stat 检查能据实报 413。
	if _, err := io.Copy(tmp, io.LimitReader(in, service.SourceMaxBytes+1)); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	return service.LoadSource(tmpPath, sourceDir, h.Filename, onConflict, service.SourceMaxBytes)
}

// abortSourceError 把 SourceLoader 的错误映射为与 Python 同形的 HTTP 响应。
func abortSourceError(c *gin.Context, err error) {
	var se *service.SourceError
	if !errors.As(err, &se) {
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	switch se.Kind {
	case service.SourceErrUnsupportedFormat:
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "source_unsupported_format",
			map[string]any{"ext": se.Ext}))
	case service.SourceErrTooLarge:
		apperr.Abort(c, apperr.NewText(http.StatusRequestEntityTooLarge, "source_too_large", map[string]any{
			"filename": se.Filename,
			"size_mb":  roundMB(se.SizeBytes),
			"limit_mb": roundMB(se.LimitBytes),
		}))
	case service.SourceErrDecodeFailed:
		apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "source_decode_failed", map[string]any{
			"filename": se.Filename,
			"tried":    strings.Join(se.Tried, ", "),
		}))
	case service.SourceErrCorruptFile:
		apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "source_corrupt_file", map[string]any{
			"filename": se.Filename,
			"reason":   se.Reason,
		}))
	case service.SourceErrConflict:
		// Python 此处的 detail 是对象形：existing / suggested_name / message 三个字段。
		loc := i18n.FromRequest(c.Request)
		c.JSON(http.StatusConflict, gin.H{"detail": gin.H{
			"existing":       se.Existing,
			"suggested_name": se.Suggested,
			"message": i18n.T(loc, "source_conflict", map[string]any{
				"existing":  se.Existing,
				"suggested": se.Suggested,
			}),
		}})
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}

// roundMB 与 Python round(bytes / 1024 / 1024, 1) 同口径。
func roundMB(b int64) float64 {
	return math.Round(float64(b)/1024/1024*10) / 10
}

// uploadStyleImage 处理 POST /projects/:name/style-image：保存参考图、调用视觉模型分析风格、
// 回写 project.json（风格图与描述二选一地覆盖模板与手写 style）。
func uploadStyleImage(d FileUploadDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		if !ensureProject(c, d.Store, name) {
			return
		}
		header, err := c.FormFile("file")
		if err != nil || header.Filename == "" {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "missing_filename", nil))
			return
		}
		ext := strings.ToLower(filepath.Ext(header.Filename))
		mediaType, ok := styleImageMediaTypes[ext]
		if !ok {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "unsupported_image_type", map[string]any{
				"ext":     ext,
				"allowed": ".png, .jpg, .jpeg, .webp",
			}))
			return
		}
		content, err := readUploadBounded(header, styleImageMaxBytes)
		if err != nil {
			if errors.Is(err, img.ErrTooLarge) {
				apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "upload_too_large",
					map[string]any{"max_mb": styleImageMaxBytes / (1024 * 1024)}))
				return
			}
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		if _, _, derr := img.DecodeConfig(content); derr != nil {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "invalid_image_file", nil))
			return
		}

		dir, err := d.Store.Dir(name)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		fileName := "style_reference" + ext
		if err := os.WriteFile(filepath.Join(dir, fileName), content, 0o644); err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}

		pf, err := d.Store.Get(name)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		gen, err := textgen.NewVisionGenerator(c.Request.Context(), d.DB, pf, textgen.TaskStyleAnalysis)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		description, err := gen.GenerateWithImage(c.Request.Context(), textgen.StyleAnalysisPrompt, content, mediaType, 0)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}

		if err := d.Store.Update(name, func(p *domain.ProjectFile) error {
			p.StyleImage = fileName
			p.StyleDescription = description
			// 强互斥：自定义参考图与模板二选一，同时清掉模板展开写入的 style prompt。
			p.StyleTemplateID = ""
			p.Style = ""
			return nil
		}); err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"success":           true,
			"style_image":       fileName,
			"style_description": description,
			"url":               "/api/v1/files/" + name + "/" + fileName,
		})
	}
}

// readUploadBounded 读取上传体并施加字节上限，超限返回 400 upload_too_large。
func readUploadBounded(h *multipart.FileHeader, maxBytes int64) ([]byte, error) {
	in, err := h.Open()
	if err != nil {
		return nil, err
	}
	defer in.Close()
	return img.ReadAllBounded(in, maxBytes)
}
