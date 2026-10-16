package http

// 剪映草稿导出路由（对应 server/routers/jianying_assembly.py）。

import (
	"errors"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/service"
)

// JianyingAssemblyDeps 是剪映导出 handler 的依赖。
type JianyingAssemblyDeps struct {
	Exports *service.JianyingExportService
}

// RegisterJianyingAssembly 注册剪映草稿导出路由。
func RegisterJianyingAssembly(group *gin.RouterGroup, d JianyingAssemblyDeps) {
	group.GET("/assembly-plans/:plan_id/jianying-draft", exportJianyingDraft(d))
}

func exportJianyingDraft(d JianyingAssemblyDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		bundle, err := d.Exports.ExportCurrentRevision(c.Request.Context(), c.Param("plan_id"), currentUserID(c))
		if err != nil {
			abortJianyingErr(c, err)
			return
		}
		// 对应 Python 的 BackgroundTasks：响应写出完成后清理临时目录。
		// FileAttachment 内部的 http.ServeContent 同步写完响应并关闭文件句柄，
		// handler 返回时数据已与磁盘文件解耦，删除安全。
		defer os.RemoveAll(bundle.TempDir)
		// 显式声明 media_type（对应 FileResponse(media_type="application/zip")），
		// 避免落进 http.ServeContent 的扩展名嗅探（Windows 注册表会给 .zip
		// 返回 application/x-zip-compressed）。
		c.Header("Content-Type", "application/zip")
		c.FileAttachment(bundle.Path, bundle.Filename)
	}
}

// abortJianyingErr 把导出领域的错误映射成统一 {"detail": ...} 信封：
// 404 / 409 {"code","status"} / 4xx {"code","message"}，与 Python handler 逐支对齐。
func abortJianyingErr(c *gin.Context, err error) {
	var (
		notFound *service.AssemblyPlanNotFoundError
		conflict *service.AssemblyPlanConflictError
		export   *service.JianyingExportError
	)
	switch {
	case errors.As(err, &notFound):
		apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "assembly_plan_not_found"))
	case errors.As(err, &conflict):
		params := map[string]any{}
		if conflict.Status != "" {
			params["status"] = conflict.Status
		}
		apperr.Abort(c, apperr.New(http.StatusConflict, conflict.Code, params))
	case errors.As(err, &export):
		apperr.Abort(c, apperr.New(export.Status, export.Code, map[string]any{"message": export.Message}))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
