// 项目文件管理路由（对应 server/routers/files.py 的轻量端点组，10 端点）。
//
// 挂载口径与 Python 一致：
//   - GET /files/:project/:path 与 GET /global-assets/:type/:filename 是**公开**端点
//     （前端经 <img>/<video> 直发请求带不了 Authorization header），但都有 SafeJoin 防护；
//   - 其余端点走标准鉴权。
//
// 上传端点（POST /upload/{type} 与 POST /style-image）见 files_upload.go。
package http

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/service"
)

// FilesDeps 是文件管理路由的依赖。
type FilesDeps struct {
	Store  *service.ProjectStore
	Files  *service.FileService
	Review *service.ScriptReviewService
}

// RegisterPublicFiles 注册公开静态文件端点（无鉴权，前端 <img>/<video> 直发）。
func RegisterPublicFiles(group *gin.RouterGroup, d FilesDeps) {
	if d.Files == nil {
		return
	}
	group.GET("/files/:project_name/*path", serveProjectFile(d))
	group.GET("/global-assets/:asset_type/:filename", serveGlobalAsset(d))
}

// RegisterFiles 注册需鉴权的文件管理端点。
func RegisterFiles(group *gin.RouterGroup, d FilesDeps) {
	if d.Files == nil {
		return
	}
	base := "/projects/:name"
	group.GET(base+"/files", listProjectFiles(d))
	group.GET(base+"/source/:filename", getSourceFile(d))
	group.PUT(base+"/source/:filename", putSourceFile(d))
	group.DELETE(base+"/source/:filename", deleteSourceFile(d))
	group.GET(base+"/drafts/:episode/step:step", getDraftContent(d))
	group.PUT(base+"/drafts/:episode/step:step", putDraftContent(d))
	group.DELETE(base+"/drafts/:episode/step:step", deleteDraftContent(d))
	group.DELETE(base+"/characters/:char_name/reference-audio", deleteCharacterRefAudio(d))
}

// serveProjectFile 服务项目内静态文件。
func serveProjectFile(d FilesDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		path, ok, err := d.Files.ResolveProjectFile(c.Param("project_name"), c.Param("path"))
		if err != nil {
			apperr.Abort(c, apperr.NewText(http.StatusNotFound, "project_not_found",
				map[string]any{"name": c.Param("project_name")}))
			return
		}
		if !ok {
			// 越界一律 403：不让 404/403 的差异成为项目目录外的存在性探针。
			apperr.Abort(c, apperr.NewText(http.StatusForbidden, "forbidden_access", nil))
			return
		}
		info, statErr := os.Stat(path)
		if statErr != nil || info.IsDir() {
			apperr.Abort(c, apperr.NewText(http.StatusNotFound, "file_not_found",
				map[string]any{"path": c.Param("path")}))
			return
		}
		// 内容寻址缓存：带 ?v= 或 versions/ 路径时设 immutable；nosniff 防内容嗅探成脚本。
		c.Header("X-Content-Type-Options", "nosniff")
		if c.Query("v") != "" || strings.HasPrefix(c.Param("path"), "versions/") {
			c.Header("Cache-Control", "public, max-age=31536000, immutable")
		}
		c.File(path)
	}
}

// serveGlobalAsset 服务全局资产图片。
func serveGlobalAsset(d FilesDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		path, ok, err := d.Files.ResolveGlobalAsset(c.Param("asset_type"), c.Param("filename"))
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		if !ok {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "invalid_asset_type", nil))
			return
		}
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() {
			apperr.Abort(c, apperr.NewText(http.StatusNotFound, "file_not_found",
				map[string]any{"path": c.Param("filename")}))
			return
		}
		c.Header("X-Content-Type-Options", "nosniff")
		c.File(path)
	}
}

// listProjectFiles 列出各桶可见文件。
func listProjectFiles(d FilesDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := d.Files.ListProjectFiles(c.Param("name"))
		if err != nil {
			abortFilesErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"files": out})
	}
}

// getSourceFile 读 source 文本文件。
func getSourceFile(d FilesDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		content, err := d.Files.ReadSourceFile(c.Param("name"), c.Param("filename"))
		if err != nil {
			abortFilesErr(c, err)
			return
		}
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusOK, "%s", content)
	}
}

// putSourceFile 写 source 文本文件。
func putSourceFile(d FilesDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		content, err := readPlainTextBody(c)
		if err != nil {
			return
		}
		path, err := d.Files.WriteSourceFile(c.Param("name"), c.Param("filename"), content)
		if err != nil {
			abortFilesErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "path": path})
	}
}

// deleteSourceFile 删除 source 文件（级联删 raw 备份）。
func deleteSourceFile(d FilesDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := d.Files.DeleteSourceFile(c.Param("name"), c.Param("filename")); err != nil {
			abortFilesErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// readPlainTextBody 读 text/plain 请求体（FastAPI 的 Body(..., media_type="text/plain")）。
func readPlainTextBody(c *gin.Context) (string, error) {
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		apperr.Abort(c, apperr.Internal(err))
		return "", err
	}
	return string(raw), nil
}

// getDraftContent 读草稿文本。
func getDraftContent(d FilesDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		episode, step, ok := draftParams(c)
		if !ok {
			return
		}
		content, err := d.Files.ReadDraft(c.Param("name"), episode, step)
		if err != nil {
			abortFilesErr(c, err)
			return
		}
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusOK, "%s", content)
	}
}

// putDraftContent 写草稿文本。
func putDraftContent(d FilesDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		episode, step, ok := draftParams(c)
		if !ok {
			return
		}
		content, err := readPlainTextBody(c)
		if err != nil {
			return
		}
		path, isNew, err := d.Files.WriteDraft(
			c.Request.Context(), c.Param("name"), episode, step, content, d.Review)
		if err != nil {
			abortFilesErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "path": path, "created": isNew})
	}
}

// deleteDraftContent 删除草稿。
func deleteDraftContent(d FilesDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		episode, step, ok := draftParams(c)
		if !ok {
			return
		}
		if err := d.Files.DeleteDraft(c.Param("name"), episode, step); err != nil {
			abortFilesErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// deleteCharacterRefAudio 删除角色参考音频。
func deleteCharacterRefAudio(d FilesDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := d.Files.DeleteCharacterReferenceAudio(
			c.Param("name"), c.Param("char_name")); err != nil {
			abortFilesErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// draftParams 解析剧集与步骤号（非法即 FastAPI 风格 422）。
func draftParams(c *gin.Context) (int, int, bool) {
	episode, err := strconv.Atoi(c.Param("episode"))
	if err != nil {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
			Loc: []string{"path", "episode"}, Msg: "Input should be a valid integer", Type: "int_parsing",
		}}})
		return 0, 0, false
	}
	// 路由参数是 step:step 形式（gin catch-all 前缀），值形如 "1"。
	stepRaw := strings.TrimPrefix(c.Param("step"), "step")
	step, err := strconv.Atoi(stepRaw)
	if err != nil {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
			Loc: []string{"path", "step"}, Msg: "Input should be a valid integer", Type: "int_parsing",
		}}})
		return 0, 0, false
	}
	return episode, step, true
}

// abortFilesErr 映射文件域错误。
func abortFilesErr(c *gin.Context, err error) {
	var (
		valueErr *service.ValueError
		missing  *service.MediaAssetNotFoundError
	)
	switch {
	case errors.As(err, &missing) && c.Param("char_name") != "":
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "character_not_found",
			map[string]any{"name": c.Param("char_name")}))
	case errors.Is(err, os.ErrNotExist):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "file_not_found",
			map[string]any{"path": c.Param("filename")}))
	case errors.As(err, &valueErr):
		code := "request_invalid"
		switch {
		case strings.HasPrefix(valueErr.Msg, "invalid step_num"):
			code = "invalid_step_num"
		case valueErr.Msg == "draft_invalid_json":
			code = "draft_invalid_json"
		case strings.HasPrefix(valueErr.Msg, "invalid json"):
			code = "script_review_invalid_content"
		case valueErr.Msg == "forbidden path":
			apperr.Abort(c, apperr.NewText(http.StatusForbidden, "forbidden_access", nil))
			return
		}
		params := map[string]any{"details": valueErr.Msg}
		if strings.HasPrefix(valueErr.Msg, "invalid step_num") {
			params = map[string]any{"step_num": strings.TrimPrefix(valueErr.Msg, "invalid step_num: ")}
		}
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, code, params))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
