package http

// 项目媒体库路由（对应 server/routers/media_assets.py），由 feature gate
// media_library 约束。索引只读/维护语义：不改动、不重排物理媒体文件。

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/service"
)

// MediaAssetsDeps 是媒体库 handler 的依赖。
type MediaAssetsDeps struct {
	Store *service.ProjectStore
}

// RegisterMediaAssets 注册项目媒体库路由；调用方负责挂 feature gate。
//
// 路由树注意：静态段（upload / scan / content / reconciliation）与 :media_asset_id
// 参数段同层共存，gin 按静态优先匹配，不冲突。
func RegisterMediaAssets(group *gin.RouterGroup, d MediaAssetsDeps) {
	group.GET("/projects/:name/media-assets", listMediaAssets(d))
	group.POST("/projects/:name/media-assets/upload", uploadMediaAsset(d))
	group.POST("/projects/:name/media-assets/scan", scanMediaAssets(d))
	group.POST("/projects/:name/media-assets/reconciliation/retry", retryMediaAssets(d))
	group.GET("/projects/:name/media-assets/content/:media_asset_id", getMediaAssetContent(d))
	group.GET("/projects/:name/media-assets/:media_asset_id", getMediaAsset(d))
	group.POST("/projects/:name/media-assets/:media_asset_id/bindings", bindMediaAsset(d))
	group.PATCH("/projects/:name/media-assets/:media_asset_id/archive", archiveMediaAsset(d))
	group.DELETE("/projects/:name/media-assets/:media_asset_id", deleteMediaAsset(d))
}

// mediaAssetKinds / origins / bindingKinds 是查询参数的合法枚举取值。
var mediaAssetKinds = map[string]bool{"image": true, "video": true, "audio": true}
var mediaOrigins = map[string]bool{
	"upload": true, "generated": true, "edited": true, "extracted": true, "imported": true,
}
var mediaBindingKinds = map[string]bool{
	"project": true, "character": true, "scene": true, "prop": true, "product": true,
	"episode": true, "shot": true, "style": true, "final": true,
}

// resolveMediaProjectRoot 解析项目根目录；不存在或非法时写出 404 并返回 false。
func resolveMediaProjectRoot(c *gin.Context, store *service.ProjectStore) (string, bool) {
	projectID := c.Param("name")
	if !store.Exists(projectID) {
		apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "project_not_found"))
		return "", false
	}
	root, err := store.Dir(projectID)
	if err != nil {
		apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "project_not_found"))
		return "", false
	}
	return root, true
}

// queryEnum 取可选枚举 query 参数；非法值写出 422。
func queryEnum(c *gin.Context, field string, allowed map[string]bool) (*string, bool) {
	raw, present := c.GetQuery(field)
	if !present || raw == "" {
		return nil, true
	}
	if !allowed[raw] {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"query", field}, Msg: "value is not a valid enumeration member", Type: "type_error.enum"},
		}})
		return nil, false
	}
	return &raw, true
}

// queryMaxLen 取可选有长度上限的 query 参数；超限写出 422。
func queryMaxLen(c *gin.Context, field string, max int) (*string, bool) {
	raw, present := c.GetQuery(field)
	if !present || raw == "" {
		return nil, true
	}
	if len(raw) > max {
		abortFieldConstraint(c, field, "string_too_long")
		return nil, false
	}
	return &raw, true
}

func listMediaAssets(d MediaAssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		root, ok := resolveMediaProjectRoot(c, d.Store)
		if !ok {
			return
		}
		kind, ok := queryEnum(c, "kind", mediaAssetKinds)
		if !ok {
			return
		}
		origin, ok := queryEnum(c, "origin", mediaOrigins)
		if !ok {
			return
		}
		bindingKind, ok := queryEnum(c, "binding_kind", mediaBindingKinds)
		if !ok {
			return
		}
		workflowRunID, ok := queryMaxLen(c, "workflow_run_id", 128)
		if !ok {
			return
		}
		targetID, ok := queryMaxLen(c, "target_id", 256)
		if !ok {
			return
		}
		purpose, ok := queryMaxLen(c, "purpose", 128)
		if !ok {
			return
		}
		var archived *bool
		if raw, present := c.GetQuery("archived"); present && raw != "" {
			parsed, err := parseQueryBool(raw)
			if err != nil {
				abortFieldConstraint(c, "archived", "bool_parsing")
				return
			}
			archived = &parsed
		}
		projectID := c.Param("name")
		result, err := service.ListProjectMediaAssets(service.ListProjectMediaAssetsParams{
			ProjectID:     &projectID,
			ProjectRoot:   root,
			Kind:          kind,
			Origin:        origin,
			WorkflowRunID: workflowRunID,
			BindingKind:   bindingKind,
			TargetID:      targetID,
			Purpose:       purpose,
			Archived:      archived,
		})
		if err != nil {
			abortMediaAssetErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func uploadMediaAsset(d MediaAssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		root, ok := resolveMediaProjectRoot(c, d.Store)
		if !ok {
			return
		}
		header, err := c.FormFile("file")
		if err != nil {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"body", "file"}, Msg: "field required", Type: "value_error.missing"},
			}})
			return
		}
		filename := header.Filename
		if filename == "" {
			filename = "upload"
		}
		if service.ClassifyMediaPath(filename) == "" {
			apperr.Abort(c, apperr.NewLiteral(http.StatusUnsupportedMediaType, "unsupported_media"))
			return
		}
		suffix := strings.ToLower(filepath.Ext(filename))
		relativePath := "uploads/" + strings.ReplaceAll(uuid.NewString(), "-", "") + suffix
		destination := filepath.Join(root, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		if !writeUploadedMedia(c, header, destination) {
			return
		}
		originalName := filename
		asset, err := service.RegisterMediaAsset(service.RegisterMediaAssetParams{
			ProjectID:    c.Param("name"),
			ProjectRoot:  root,
			RelativePath: relativePath,
			Origin:       service.MediaOriginUpload,
			OriginalName: &originalName,
		})
		if err != nil {
			abortMediaAssetErr(c, err)
			return
		}
		if asset == nil {
			catalog := service.ProjectMediaCatalog(root)
			reason := "upload_registration_unavailable"
			origin := service.MediaOriginUpload
			if _, enqErr := catalog.EnqueueReconciliation(service.EnqueueMediaReconciliationParams{
				ProjectID:    c.Param("name"),
				RelativePath: relativePath,
				Reason:       reason,
				Origin:       origin,
			}); enqErr != nil {
				apperr.Abort(c, apperr.Internal(enqErr))
				return
			}
			apperr.Abort(c, apperr.NewLiteral(http.StatusServiceUnavailable, "media_index_unavailable"))
			return
		}
		result, err := service.GetProjectMediaAsset(root, asset.ID)
		if err != nil {
			abortMediaAssetErr(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	}
}

// writeUploadedMedia 把上传流写入目标文件；失败时写出错误并返回 false。
func writeUploadedMedia(c *gin.Context, header *multipart.FileHeader, destination string) bool {
	source, err := header.Open()
	if err != nil {
		apperr.Abort(c, apperr.Internal(err))
		return false
	}
	defer source.Close()
	target, err := os.Create(destination)
	if err != nil {
		apperr.Abort(c, apperr.Internal(err))
		return false
	}
	if _, err := io.Copy(target, source); err != nil {
		target.Close()
		apperr.Abort(c, apperr.Internal(err))
		return false
	}
	if err := target.Close(); err != nil {
		apperr.Abort(c, apperr.Internal(err))
		return false
	}
	return true
}

func getMediaAssetContent(d MediaAssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		root, ok := resolveMediaProjectRoot(c, d.Store)
		if !ok {
			return
		}
		asset, err := service.GetProjectMediaAsset(root, c.Param("media_asset_id"))
		if err != nil {
			abortMediaAssetErr(c, err)
			return
		}
		physicalPath, _ := asset["physical_path"].(string)
		safePath, safe := mediaContentPath(root, physicalPath)
		if !safe {
			apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "media_file_not_found"))
			return
		}
		info, statErr := os.Stat(safePath)
		if statErr != nil || info.IsDir() {
			apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "media_file_not_found"))
			return
		}
		file, openErr := os.Open(safePath)
		if openErr != nil {
			apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "media_file_not_found"))
			return
		}
		defer file.Close()

		originalName, _ := asset["original_name"].(string)
		c.Header("X-Content-Type-Options", "nosniff")
		if mimeType, isString := asset["mime_type"].(string); isString && mimeType != "" {
			c.Header("Content-Type", mimeType)
		}
		c.Header("Content-Disposition", contentDisposition(originalName))
		http.ServeContent(c.Writer, c.Request, originalName, info.ModTime(), file)
	}
}

// mediaContentPath 把物理路径限定在项目根内（对应 Python 的 relative_to 检查）。
func mediaContentPath(projectRoot, physicalPath string) (string, bool) {
	if physicalPath == "" {
		return "", false
	}
	relative, err := filepath.Rel(projectRoot, physicalPath)
	if err != nil {
		return "", false
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return filepath.Join(projectRoot, relative), true
}

// contentDisposition 构造 attachment 头；非 ASCII 文件名附带 RFC 5987 形态。
func contentDisposition(filename string) string {
	base := filepath.Base(filename)
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(base)
	ascii := true
	for _, r := range base {
		if r > 127 {
			ascii = false
			break
		}
	}
	if ascii {
		return `attachment; filename="` + escaped + `"`
	}
	return `attachment; filename="` + escaped + `"; filename*=UTF-8''` + url.PathEscape(base)
}

func getMediaAsset(d MediaAssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		root, ok := resolveMediaProjectRoot(c, d.Store)
		if !ok {
			return
		}
		result, err := service.GetProjectMediaAsset(root, c.Param("media_asset_id"))
		if err != nil {
			abortMediaAssetErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func bindMediaAsset(d MediaAssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		root, ok := resolveMediaProjectRoot(c, d.Store)
		if !ok {
			return
		}
		body, ok := decodeBody(c)
		if !ok {
			return
		}
		bindingKind, ok := requiredString(c, body, "binding_kind")
		if !ok {
			return
		}
		if !mediaBindingKinds[bindingKind] {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"body", "binding_kind"}, Msg: "value is not a valid enumeration member", Type: "type_error.enum"},
			}})
			return
		}
		purpose, ok := requiredString(c, body, "purpose")
		if !ok {
			return
		}
		if purpose == "" {
			abortFieldConstraint(c, "purpose", "string_too_short")
			return
		}
		if len(purpose) > 128 {
			abortFieldConstraint(c, "purpose", "string_too_long")
			return
		}
		targetID := optionalBodyString(body, "target_id")
		if targetID != nil && len(*targetID) > 256 {
			abortFieldConstraint(c, "target_id", "string_too_long")
			return
		}
		result, err := service.BindProjectMediaAsset(
			root, c.Param("name"), c.Param("media_asset_id"), bindingKind, targetID, purpose,
		)
		if err != nil {
			abortMediaAssetErr(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	}
}

func archiveMediaAsset(d MediaAssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		root, ok := resolveMediaProjectRoot(c, d.Store)
		if !ok {
			return
		}
		body, ok := decodeBody(c)
		if !ok {
			return
		}
		raw, present := body["archived"]
		if !present || raw == nil {
			abortMissingField(c, "archived")
			return
		}
		archived, isBool := raw.(bool)
		if !isBool {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"body", "archived"}, Msg: "value is not a valid boolean", Type: "type_error.bool"},
			}})
			return
		}
		result, err := service.ArchiveProjectMediaAsset(root, c.Param("media_asset_id"), archived)
		if err != nil {
			abortMediaAssetErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func scanMediaAssets(d MediaAssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		root, ok := resolveMediaProjectRoot(c, d.Store)
		if !ok {
			return
		}
		dryRun := false
		if raw, present := c.GetQuery("dry_run"); present && raw != "" {
			parsed, err := parseQueryBool(raw)
			if err != nil {
				abortFieldConstraint(c, "dry_run", "bool_parsing")
				return
			}
			dryRun = parsed
		}
		result, err := service.ScanProjectMediaAssets(c.Param("name"), root, dryRun)
		if err != nil {
			abortMediaAssetErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func retryMediaAssets(d MediaAssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		root, ok := resolveMediaProjectRoot(c, d.Store)
		if !ok {
			return
		}
		itemID, ok := queryMaxLen(c, "item_id", 64)
		if !ok {
			return
		}
		result, err := service.RetryProjectMediaReconciliation(c.Param("name"), root, itemID)
		if err != nil {
			abortMediaAssetErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func deleteMediaAsset(d MediaAssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		root, ok := resolveMediaProjectRoot(c, d.Store)
		if !ok {
			return
		}
		result, err := service.DeleteProjectMediaAsset(root, c.Param("media_asset_id"))
		if err != nil {
			abortMediaAssetErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

// parseQueryBool 解析 FastAPI 风格的 query 布尔值。
func parseQueryBool(raw string) (bool, error) {
	switch strings.ToLower(raw) {
	case "1", "true", "on", "yes":
		return true, nil
	case "0", "false", "off", "no":
		return false, nil
	default:
		return false, errors.New("invalid boolean")
	}
}

// abortMediaAssetErr 把媒体库领域错误映射成统一 {"detail": ...} 信封。
func abortMediaAssetErr(c *gin.Context, err error) {
	var (
		notFound   *service.MediaAssetNotFoundError
		referenced *service.MediaAssetReferencedError
		valueErr   *service.ValueError
	)
	switch {
	case errors.As(err, &notFound):
		apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "media_asset_not_found"))
	case errors.As(err, &referenced):
		apperr.Abort(c, apperr.New(http.StatusConflict, "media_asset_still_referenced", map[string]any{
			"references": referenced.References,
		}))
	case errors.As(err, &valueErr):
		apperr.Abort(c, apperr.NewLiteral(http.StatusBadRequest, valueErr.Msg))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
