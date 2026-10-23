// 项目 ZIP 导入与导入前校验端点。对应 projects.py 的 POST /projects/import 与
// POST /projects/import/preflight。
//
// 错误体形态与 Python 同形：归档校验失败回 {detail, errors, warnings, ...extra}
// （不是标准 {detail: string} 信封）——前端导入面板要按 errors/warnings 列表渲染。
package project

import (
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/service"
)

// ArchiveDeps 聚合导入依赖。
type ArchiveDeps struct {
	Archive *service.ProjectArchiveService
}

// RegisterProjectArchive 注册导入端点（authed 组内）。
func RegisterProjectArchive(group *gin.RouterGroup, deps ArchiveDeps) {
	h := &archiveHandlers{deps: deps}
	group.POST("/projects/import", h.importProject)
	group.POST("/projects/import/preflight", h.preflightProject)
}

type archiveHandlers struct{ deps ArchiveDeps }

func (h *archiveHandlers) importProject(c *gin.Context) {
	h.handle(c, false)
}

func (h *archiveHandlers) preflightProject(c *gin.Context) {
	h.handle(c, true)
}

// handle 处理两个端点：preflight 只校验不安装。
func (h *archiveHandlers) handle(c *gin.Context, preflight bool) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "content_or_file_required", nil))
		return
	}
	// Python 用临时文件承接上传再解析（zip.NewReader 需要 ReaderAt）；Go 同样先落盘。
	tmp, err := os.CreateTemp("", "shotwise-upload-*.zip")
	if err != nil {
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	source, err := fileHeader.Open()
	if err != nil {
		_ = tmp.Close()
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	_, copyErr := io.Copy(tmp, source)
	closeErr := tmp.Close()
	_ = source.Close()
	if copyErr != nil {
		apperr.Abort(c, apperr.Internal(copyErr))
		return
	}
	if closeErr != nil {
		apperr.Abort(c, apperr.Internal(closeErr))
		return
	}

	if preflight {
		result, err := h.deps.Archive.PreflightArchive(tmpPath, fileHeader.Filename)
		if err != nil {
			abortArchiveErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"project_name":          result.ProjectName,
			"project_title":         result.ProjectTitle,
			"conflict_project_name": result.ConflictProjectName,
			"diagnostics":           result.Diagnostics,
		})
		return
	}

	conflictPolicy := c.DefaultPostForm("conflict_policy", "prompt")
	result, err := h.deps.Archive.ImportArchive(tmpPath, fileHeader.Filename, conflictPolicy)
	if err != nil {
		abortArchiveErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":             true,
		"project_name":        result.ProjectName,
		"project":             result.Project,
		"warnings":            result.Warnings,
		"conflict_resolution": result.ConflictResolution,
		"diagnostics":         result.Diagnostics,
		// project memory 按收敛口径不参与（memory 服务未迁），恒回零计数。
		"project_memories": gin.H{"imported": 0, "skipped": 0},
	})
}

// abortArchiveErr 把归档域错误映射为 Python 同形响应体。
func abortArchiveErr(c *gin.Context, err error) {
	var invalid *service.ArchiveValidationError
	if !errors.As(err, &invalid) {
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	body := gin.H{
		"detail":   invalid.Detail,
		"errors":   invalid.Errors,
		"warnings": invalid.Warnings,
	}
	for key, value := range invalid.Extra {
		body[key] = value
	}
	c.AbortWithStatusJSON(invalid.Status, body)
}
