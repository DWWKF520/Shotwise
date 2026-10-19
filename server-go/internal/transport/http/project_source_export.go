// 项目源文上传、概述生成与导出端点。对应 projects.py 的
// POST /source、POST /generate-overview、POST /export/token、GET /export。
package http

import (
	"archive/zip"
	"errors"
	"fmt"
	stdiofs "io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/service"
	"gorm.io/gorm"
)

// sourceExportDeps 聚合依赖。
type sourceExportDeps struct {
	Store    *service.ProjectStore
	Source   *service.ProjectSource
	Auth     *auth.Service
	Jianying *service.JianyingDraftService
}

// RegisterProjectSourceExport 注册四个端点。export/token 在 authed 组内；
// GET /export 走短时效 download_token 认证（浏览器原生下载带不了 header）。
func RegisterProjectSourceExport(group *gin.RouterGroup, authed *gin.RouterGroup, deps sourceExportDeps) {
	h := &projectSourceHandlers{deps: deps}
	authed.POST("/projects/:name/source", h.setSource)
	authed.POST("/projects/:name/generate-overview", h.generateOverview)
	authed.POST("/projects/:name/export/token", h.createExportToken)
	group.GET("/projects/:name/export", h.exportArchive)
	group.GET("/projects/:name/export/jianying-draft", h.exportJianyingDraft)
}

type projectSourceHandlers struct{ deps sourceExportDeps }

// setSource 处理 multipart：content 与 file 二选一，可选触发概述生成。
func (h *projectSourceHandlers) setSource(c *gin.Context) {
	generateOverview := c.PostForm("generate_overview") != "false"
	contentForm := c.PostForm("content")
	hasFile := false
	if _, err := c.FormFile("file"); err == nil {
		hasFile = true
	}
	// Python 判 truthy：content 非空且 file 存在 → 400；空 content + file 合法。
	if contentForm != "" && hasFile {
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "one_of_content_or_file", nil))
		return
	}
	var fileContent []byte
	originalName := ""
	if hasFile {
		fileHeader, _ := c.FormFile("file")
		file, err := fileHeader.Open()
		if err != nil {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "content_or_file_required", nil))
			return
		}
		defer func() { _ = file.Close() }()
		// 上限按字符数×4（UTF-8 汉字 3 字节 + 余量），读阶段截断。
		content, err := readBounded(file, int64(service.SourceMaxChars)*4+1)
		if err != nil {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "file_too_large",
				map[string]any{"max_chars": service.SourceMaxChars}))
			return
		}
		fileContent = content
		originalName = fileHeader.Filename
	} else if contentForm == "" {
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "content_or_file_required", nil))
		return
	}
	var contentPtr *string
	if contentForm != "" && len(fileContent) == 0 {
		contentPtr = &contentForm
	}

	result, err := h.deps.Source.SetSource(c.Request.Context(), c.Param("name"), service.SetSourceInput{
		Content:          contentPtr,
		FileContent:      fileContent,
		OriginalName:     originalName,
		GenerateOverview: generateOverview,
	})
	if err != nil {
		abortSourceErr(c, err)
		return
	}
	response := gin.H{"success": true, "filename": result.Filename, "chars": result.Chars}
	if result.Overview != nil {
		response["overview"] = result.Overview
	}
	if result.OverviewError != "" {
		response["overview"] = nil
		response["overview_error"] = result.OverviewError
	}
	c.JSON(http.StatusOK, response)
}

func (h *projectSourceHandlers) generateOverview(c *gin.Context) {
	overview, err := h.deps.Source.GenerateOverview(c.Request.Context(), c.Param("name"))
	if err != nil {
		abortSourceErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "overview": overview})
}

// createExportToken 签发短时效下载 token 并带回导出诊断。
func (h *projectSourceHandlers) createExportToken(c *gin.Context) {
	scope := c.DefaultQuery("scope", "full")
	if scope != "full" && scope != "current" {
		apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "scope_invalid", nil))
		return
	}
	name := c.Param("name")
	if _, err := h.deps.Store.Get(name); err != nil {
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "project_not_found", map[string]any{"name": name}))
		return
	}
	user := auth.Current(c)
	sub := "default"
	userID := "default"
	if user != nil {
		sub = user.ID
		userID = user.ID
	}
	token, err := h.deps.Auth.CreateDownloadToken(sub, name, userID)
	if err != nil {
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"download_token": token,
		"expires_in":     auth.DownloadTokenExpirySeconds,
		"diagnostics": gin.H{
			// 诊断面只给事实（文件计数与总量），不给路径细节。
			"file_count":  countProjectFiles(h.deps.Store.Root(), name),
			"total_bytes": projectBytes(h.deps.Store.Root(), name),
		},
	})
}

// exportArchive 打包项目目录为 ZIP。download_token 认证（purpose/project 双校验）。
func (h *projectSourceHandlers) exportArchive(c *gin.Context) {
	name := c.Param("name")
	if c.Query("scope") != "" && c.Query("scope") != "full" && c.Query("scope") != "current" {
		apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "scope_invalid", nil))
		return
	}
	if _, err := h.deps.Auth.VerifyDownloadToken(c.Query("download_token"), name); err != nil {
		apperr.Abort(c, apperr.NewText(http.StatusUnauthorized, "download_token_invalid", nil))
		return
	}
	projectDir, err := h.deps.Store.Dir(name)
	if err != nil {
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "project_not_found", map[string]any{"name": name}))
		return
	}
	c.Header("Content-Disposition", "attachment; filename=\""+name+".zip\"")
	c.Header("Content-Type", "application/zip")
	zipWriter := zip.NewWriter(c.Writer)
	// 稳定顺序：目录遍历排序，保证同内容两次导出字节一致（利于校验与缓存）。
	var paths []string
	_ = filepath.WalkDir(projectDir, func(path string, entry stdiofs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		// 跳过 symlink（防软链把导出包变成任意文件读取）与运行期临时文件。
		if !entry.Type().IsRegular() {
			return nil
		}
		base := filepath.Base(path)
		if strings.HasPrefix(base, ".") && strings.HasSuffix(base, ".tmp") {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	sort.Strings(paths)
	for _, path := range paths {
		rel, relErr := filepath.Rel(projectDir, path)
		if relErr != nil {
			continue
		}
		writer, err := zipWriter.Create(filepath.ToSlash(rel))
		if err != nil {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		_, _ = writer.Write(content)
	}
	_ = zipWriter.Close()
}

// exportJianyingDraft 导出指定集的剪映草稿 ZIP（download_token 认证，浏览器原生下载）。
func (h *projectSourceHandlers) exportJianyingDraft(c *gin.Context) {
	name := c.Param("name")
	if _, err := h.deps.Auth.VerifyDownloadToken(c.Query("download_token"), name); err != nil {
		apperr.Abort(c, apperr.NewText(http.StatusUnauthorized, "download_token_invalid", nil))
		return
	}
	episode, err := strconv.Atoi(c.Query("episode"))
	if err != nil {
		apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "request_invalid", nil))
		return
	}
	draftPath := c.Query("draft_path")
	if strings.TrimSpace(draftPath) == "" {
		apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "request_invalid", nil))
		return
	}
	useDraftInfoName := c.DefaultQuery("jianying_version", "6") != "5"
	zipPath, draftName, exportErr := h.deps.Jianying.ExportEpisodeDraft(
		c.Request.Context(), name, episode, draftPath, useDraftInfoName)
	if exportErr != nil {
		abortJianyingExportErr(c, exportErr)
		return
	}
	defer func() { _ = os.RemoveAll(filepath.Dir(zipPath)) }()
	c.Header("Content-Disposition",
		fmt.Sprintf("attachment; filename=\"%s_episode_%d_jianying_draft.zip\"", name, episode))
	c.Header("Content-Type", "application/zip")
	c.File(zipPath)
	_ = draftName
}

// abortJianyingExportErr 把剪映导出域错误映射到 Python 同形状态码（与拼装面的 abortJianyingErr 区分）。
func abortJianyingExportErr(c *gin.Context, err error) {
	var jyErr *service.JianyingError
	var notFound *service.SourceReadError
	switch {
	case errors.As(err, &jyErr):
		if jyErr.Key == "jianying_no_completed_segments" {
			apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "jianying_no_completed_segments",
				map[string]any{"episode": c.Query("episode")}))
			return
		}
		apperr.Abort(c, apperr.Internal(err))
	case errors.Is(err, service.ErrNoCompletedSegments):
		apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "jianying_no_completed_segments",
			map[string]any{"episode": c.Query("episode")}))
	case errors.Is(err, os.ErrNotExist), errors.As(err, &notFound):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "project_not_found",
			map[string]any{"name": c.Param("name")}))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}

// abortSourceErr 把源文/概述域错误映射到 Python 同形状态码。
func abortSourceErr(c *gin.Context, err error) {
	var (
		value   *service.SourceValueError
		readErr *service.SourceReadError
	)
	switch {
	case errors.As(err, &value):
		key := value.Msg
		if key == "file_too_large" || key == "unsupported_file_type" {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, key, value.Params))
			return
		}
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, key, nil))
	case errors.As(err, &readErr):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "invalid_encoding", nil))
	case errors.Is(err, service.ErrSourceDecode):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "invalid_encoding", nil))
	case errors.Is(err, service.ErrSourceEmpty):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "overview_source_empty", nil))
	case errors.Is(err, service.ErrOverviewInvalid):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "overview_ai_response_invalid", nil))
	case errors.Is(err, service.ErrTextProviderMissing):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "text_provider_not_configured", nil))
	case errors.Is(err, service.ErrAssetNotFound):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "project_not_found",
			map[string]any{"name": c.Param("name")}))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}

// countProjectFiles / projectBytes 是导出诊断的粗粒度统计（越界由 Store.Dir 保证）。
func countProjectFiles(root, name string) int {
	dir, err := fsSafeDir(root, name)
	if err != nil {
		return 0
	}
	count := 0
	_ = filepath.WalkDir(dir, func(path string, entry stdiofs.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() {
			count++
		}
		return nil
	})
	return count
}

func projectBytes(root, name string) int64 {
	dir, err := fsSafeDir(root, name)
	if err != nil {
		return 0
	}
	var total int64
	_ = filepath.WalkDir(dir, func(path string, entry stdiofs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return nil
		}
		if info, statErr := entry.Info(); statErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// fsSafeDir 用项目存储的 SafeJoin 语义兜底越界（正常路径 Store.Dir 已保证）。
func fsSafeDir(root, name string) (string, error) {
	if strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return "", errors.New("invalid project name")
	}
	return fs.SafeJoin(root, false, name)
}

// NewSourceExportDeps 是 app 层的构造入口。
func NewSourceExportDeps(store *service.ProjectStore, authService *auth.Service, gdb *gorm.DB) sourceExportDeps {
	return sourceExportDeps{
		Store:    store,
		Source:   service.NewProjectSource(store, gdb),
		Auth:     authService,
		Jianying: service.NewJianyingDraftService(store),
	}
}
