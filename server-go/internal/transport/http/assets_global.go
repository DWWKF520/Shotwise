// 全局资产库路由（对应 server/routers/assets.py，8 端点）。
//
// 前缀：/api/v1/assets
//
// create 与 replace-image 走 multipart（Python 的 Form + File）；from-project 与
// apply-to-project 是项目 ↔ 全局库的搬运通道（文件拷贝 + DB 写入共用失败边界）。
package http

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/asset"
)

// AssetsDeps 是全局资产路由的依赖。
type AssetsDeps struct {
	Assets *asset.GlobalAssetService
}

// RegisterAssetsGlobal 注册全局资产库路由。
func RegisterAssetsGlobal(group *gin.RouterGroup, d AssetsDeps) {
	if d.Assets == nil {
		return
	}
	base := "/assets"
	group.GET(base, listGlobalAssets(d))
	group.POST(base, createGlobalAsset(d))
	group.POST(base+"/from-project", globalAssetFromProject(d))
	group.POST(base+"/apply-to-project", globalAssetApplyToProject(d))
	group.GET(base+"/:asset_id", getGlobalAsset(d))
	group.PATCH(base+"/:asset_id", patchGlobalAsset(d))
	group.DELETE(base+"/:asset_id", deleteGlobalAsset(d))
	group.POST(base+"/:asset_id/image", replaceGlobalAssetImage(d))
}

// listGlobalAssets 列出资产（type / q / limit / offset）。
func listGlobalAssets(d AssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit, offset := paginationParams(c)
		items, err := d.Assets.List(c.Query("type"), c.Query("q"), limit, offset)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

// paginationParams 解析 limit/offset（默认 100 / 0）。
func paginationParams(c *gin.Context) (int, int) {
	limit, err := strconv.Atoi(c.Query("limit"))
	if err != nil || limit <= 0 {
		limit = 100
	}
	offset, err := strconv.Atoi(c.Query("offset"))
	if err != nil || offset < 0 {
		offset = 0
	}
	return limit, offset
}

// getGlobalAsset 读单个资产。
func getGlobalAsset(d AssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := d.Assets.Get(c.Param("asset_id"))
		if err != nil {
			abortAssetsErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"asset": out})
	}
}

// createGlobalAsset 创建资产（multipart：type/name/description/voice_style + 可选 image）。
func createGlobalAsset(d AssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		assetType := c.PostForm("type")
		name := c.PostForm("name")
		if assetType == "" || name == "" {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"body", "type"}, Msg: "field required", Type: "missing"},
				{Loc: []string{"body", "name"}, Msg: "field required", Type: "missing"},
			}})
			return
		}
		var imageReader, imageFilename = multipartFileOrNil(c, "image")
		out, err := d.Assets.Create(
			assetType, name, c.PostForm("description"), c.PostForm("voice_style"),
			imageReader, imageFilename)
		if err != nil {
			abortAssetsErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"asset": out})
	}
}

// multipartFileOrNil 取可选上传文件；不存在返回 (nil, "")。
func multipartFileOrNil(c *gin.Context, field string) (interface{ Read([]byte) (int, error) }, string) {
	header, err := c.FormFile(field)
	if err != nil || header == nil {
		return nil, ""
	}
	opened, openErr := header.Open()
	if openErr != nil {
		return nil, ""
	}
	return opened, header.Filename
}

// patchGlobalAsset 更新资产元数据。
func patchGlobalAsset(d AssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
			VoiceStyle  *string `json:"voice_style"`
		}
		if !decodeJSONBody(c, &body) {
			return
		}
		out, err := d.Assets.Patch(c.Param("asset_id"), body.Name, body.Description, body.VoiceStyle)
		if err != nil {
			abortAssetsErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"asset": out})
	}
}

// deleteGlobalAsset 删除资产（204）。
func deleteGlobalAsset(d AssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := d.Assets.Delete(c.Param("asset_id")); err != nil {
			abortAssetsErr(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// replaceGlobalAssetImage 替换资产图片。
func replaceGlobalAssetImage(d AssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		reader, filename := multipartFileOrNil(c, "image")
		if reader == nil {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"body", "image"}, Msg: "field required", Type: "missing",
			}}})
			return
		}
		out, err := d.Assets.ReplaceImage(c.Param("asset_id"), reader, filename)
		if err != nil {
			abortAssetsErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"asset": out})
	}
}

// globalAssetFromProject 项目资产入库。
func globalAssetFromProject(d AssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			ProjectName  *string `json:"project_name"`
			ResourceType *string `json:"resource_type"`
			ResourceID   *string `json:"resource_id"`
			OverrideName *string `json:"override_name"`
			Overwrite    *bool   `json:"overwrite"`
		}
		if !decodeJSONBody(c, &body) {
			return
		}
		var items []apperr.ValidationItem
		for _, field := range []struct {
			name  string
			value *string
		}{
			{"project_name", body.ProjectName},
			{"resource_type", body.ResourceType},
			{"resource_id", body.ResourceID},
		} {
			if field.value == nil {
				items = append(items, apperr.ValidationItem{
					Loc: []string{"body", field.name}, Msg: "field required", Type: "missing"})
			}
		}
		if len(items) > 0 {
			apperr.Abort(c, &apperr.Validation{Items: items})
			return
		}
		overwrite := false
		if body.Overwrite != nil {
			overwrite = *body.Overwrite
		}
		out, err := d.Assets.FromProject(c.Request.Context(), asset.FromProjectInput{
			ProjectName:  *body.ProjectName,
			ResourceType: *body.ResourceType,
			ResourceID:   *body.ResourceID,
			OverrideName: body.OverrideName,
			Overwrite:    overwrite,
		})
		if err != nil {
			abortAssetsErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"asset": out})
	}
}

// globalAssetApplyToProject 全局资产批量应用到项目。
func globalAssetApplyToProject(d AssetsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			AssetIDs       []string `json:"asset_ids"`
			TargetProject  *string  `json:"target_project"`
			ConflictPolicy *string  `json:"conflict_policy"`
		}
		if !decodeJSONBody(c, &body) {
			return
		}
		var items []apperr.ValidationItem
		if body.TargetProject == nil {
			items = append(items, apperr.ValidationItem{
				Loc: []string{"body", "target_project"}, Msg: "field required", Type: "missing"})
		}
		if body.AssetIDs == nil {
			items = append(items, apperr.ValidationItem{
				Loc: []string{"body", "asset_ids"}, Msg: "field required", Type: "missing"})
		}
		if len(items) > 0 {
			apperr.Abort(c, &apperr.Validation{Items: items})
			return
		}
		policy := "skip"
		if body.ConflictPolicy != nil {
			policy = *body.ConflictPolicy
		}
		out, err := d.Assets.ApplyToProject(c.Request.Context(), asset.ApplyToProjectInput{
			AssetIDs:       body.AssetIDs,
			TargetProject:  *body.TargetProject,
			ConflictPolicy: policy,
		})
		if err != nil {
			abortAssetsErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// abortAssetsErr 映射全局资产域错误。
func abortAssetsErr(c *gin.Context, err error) {
	var (
		exists    *asset.GlobalAssetExistsError
		conflict  *asset.GlobalAssetConflictError
		valueErr  *service.ValueError
		missing   *asset.GlobalSourceMissingError
	)
	switch {
	case errors.As(err, &conflict):
		// 对象形 detail：前端要读 existing 展示冲突面板（Python 侧同形状）。
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"detail": gin.H{
			"message":  "asset already exists",
			"existing": conflict.Existing,
		}})
	case errors.As(err, &exists):
		apperr.Abort(c, apperr.NewText(http.StatusConflict, "asset_already_exists",
			map[string]any{"name": exists.Name}))
	case errors.As(err, &missing):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "asset_source_resource_not_found",
			map[string]any{"project": missing.Project, "name": missing.Resource}))
	case errors.Is(err, asset.ErrGlobalTargetProjectNotFound):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "asset_target_project_not_found", nil))
	case errors.Is(err, asset.ErrGlobalAssetNotFound):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "asset_not_found",
			map[string]any{"name": c.Param("asset_id")}))
	case errors.Is(err, asset.ErrGlobalInvalidType):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "asset_invalid_type", nil))
	case errors.As(err, &valueErr):
		code := "request_invalid"
		switch valueErr.Msg {
		case "asset_invalid_conflict_policy":
			code = "asset_invalid_conflict_policy"
		case "unsupported image format":
			code = "unsupported_image_type"
		}
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, code, nil))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
