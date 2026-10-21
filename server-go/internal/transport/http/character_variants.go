// 角色头像上传与变体管理路由。对应 server/routers/characters.py 的 6 个手写端点
// （角色 CRUD 由资产工厂另行生成）。
package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/infra/img"
	"github.com/shotwise/shotwise/internal/service"
)

// CharacterDeps 聚合角色扩展端点依赖。
type CharacterDeps struct {
	Variants *service.CharacterVariants
}

// variantAllowedKeys 是两个变体请求的合法字段集（pydantic extra="forbid"）。
var variantAllowedKeys = map[string]bool{
	"slug": true, "display_name": true, "description": true, "image_path": true,
	"status": true, "metadata": true, "image_asset_id": true,
}

// RegisterCharacterExtras 注册头像与变体端点（挂在 /api/v1 之下，authed 组内）。
func RegisterCharacterExtras(group *gin.RouterGroup, deps CharacterDeps) {
	h := &characterHandlers{deps: deps}
	group.POST("/projects/:name/characters/:entry/avatar", h.uploadAvatar)
	// 旧版别名（Python include_in_schema=False）：不带 /projects 前缀，仅兼容存量客户端。
	group.POST("/:name/characters/:entry/avatar", h.uploadAvatar)
	group.GET("/projects/:name/characters/:entry/variants", h.listVariants)
	group.POST("/projects/:name/characters/:entry/variants", h.createVariant)
	group.PATCH("/projects/:name/characters/:entry/variants/:variant_id", h.updateVariant)
	group.DELETE("/projects/:name/characters/:entry/variants/:variant_id", h.deleteVariant)
}

type characterHandlers struct{ deps CharacterDeps }

// uploadAvatar 处理 multipart 上传：空文件 400、超字节 413、像素超限 413、格式非法 400。
func (h *characterHandlers) uploadAvatar(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "character_avatar_file_empty", nil))
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "character_avatar_file_empty", nil))
		return
	}
	defer func() { _ = file.Close() }()
	// 有界读取：读阶段就截断，不按 Content-Length 分配内存。
	content, err := img.ReadAllBounded(file, service.AvatarMaxBytes)
	if err != nil {
		if errors.Is(err, img.ErrTooLarge) {
			apperr.Abort(c, apperr.NewText(http.StatusRequestEntityTooLarge, "character_avatar_too_large",
				map[string]any{"max_mb": service.AvatarMaxBytes / (1024 * 1024)}))
			return
		}
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	if len(content) == 0 {
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "character_avatar_file_empty", nil))
		return
	}
	if len(content) > service.AvatarMaxBytes {
		apperr.Abort(c, apperr.NewText(http.StatusRequestEntityTooLarge, "character_avatar_too_large",
			map[string]any{"max_mb": service.AvatarMaxBytes / (1024 * 1024)}))
		return
	}

	relativePath, err := h.deps.Variants.UploadCharacterAvatar(c.Param("name"), c.Param("entry"), content)
	if err != nil {
		abortCharacterErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "path": relativePath})
}

func (h *characterHandlers) listVariants(c *gin.Context) {
	variants, err := h.deps.Variants.ListVariants(c.Param("name"), c.Param("entry"))
	if err != nil {
		abortCharacterErr(c, err)
		return
	}
	if variants == nil {
		variants = []map[string]any{}
	}
	c.JSON(http.StatusOK, gin.H{"variants": variants})
}

func (h *characterHandlers) createVariant(c *gin.Context) {
	raw, ok := decodeVariantBody(c)
	if !ok {
		return
	}
	if _, present := raw["slug"]; !present {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
			Loc: []string{"body", "slug"}, Msg: "Field required", Type: "missing",
		}}})
		return
	}
	var body struct {
		Slug         *string `json:"slug"`
		DisplayName  *string `json:"display_name"`
		Description  *string `json:"description"`
		ImagePath    *string `json:"image_path"`
		Status       *string `json:"status"`
		Metadata     map[string]any
		ImageAssetID *string `json:"image_asset_id"`
	}
	if err := strictDecode(raw, &body); err != nil {
		abortInvalidVariantBody(c, err)
		return
	}
	// Python Literal[...] 在校验层就 422，不能等服务层 422 混在 slug 校验里。
	if body.Status != nil && !isKnownVariantStatus(*body.Status) {
		abortVariantError(c, http.StatusUnprocessableEntity, "character_variant_invalid_status", nil)
		return
	}
	variant, err := h.deps.Variants.CreateVariant(c.Param("name"), c.Param("entry"), service.VariantCreateInput{
		Slug:         derefVariantString(body.Slug),
		DisplayName:  body.DisplayName,
		Description:  body.Description,
		ImagePath:    body.ImagePath,
		Status:       body.Status,
		Metadata:     body.Metadata,
		ImageAssetID: body.ImageAssetID,
	})
	if err != nil {
		abortCharacterErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "variant": variant})
}

func (h *characterHandlers) updateVariant(c *gin.Context) {
	raw, ok := decodeVariantBody(c)
	if !ok {
		return
	}
	var body struct {
		Slug         *string `json:"slug"`
		DisplayName  *string `json:"display_name"`
		Description  *string `json:"description"`
		ImagePath    *string `json:"image_path"`
		Status       *string `json:"status"`
		Metadata     map[string]any
		ImageAssetID *string `json:"image_asset_id"`
	}
	if err := strictDecode(raw, &body); err != nil {
		abortInvalidVariantBody(c, err)
		return
	}
	if body.Status != nil && !isKnownVariantStatus(*body.Status) {
		abortVariantError(c, http.StatusUnprocessableEntity, "character_variant_invalid_status", nil)
		return
	}
	_, metadataSet := raw["metadata"]
	variant, err := h.deps.Variants.UpdateVariant(c.Param("name"), c.Param("entry"), c.Param("variant_id"),
		service.VariantUpdateInput{
			Slug:         body.Slug,
			DisplayName:  body.DisplayName,
			Description:  body.Description,
			ImagePath:    body.ImagePath,
			Status:       body.Status,
			Metadata:     body.Metadata,
			MetadataSet:  metadataSet,
			ImageAssetID: body.ImageAssetID,
		})
	if err != nil {
		abortCharacterErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "variant": variant})
}

func (h *characterHandlers) deleteVariant(c *gin.Context) {
	variant, err := h.deps.Variants.DeleteVariant(c.Param("name"), c.Param("entry"), c.Param("variant_id"))
	if err != nil {
		abortCharacterErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "variant": variant})
}

// decodeVariantBody 读请求体为原始键值表：同时完成「严格字段集」检查
// （extra="forbid" → 422 extra_forbidden）与 metadata 存在性探测（PATCH 要区分
// 「显式置空」与「未提供」，结构体绑定表达不了这个差异）。
func decodeVariantBody(c *gin.Context) (map[string]json.RawMessage, bool) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
			Loc: []string{"body"}, Msg: "invalid request body", Type: "json_invalid",
		}}})
		return nil, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
			Loc: []string{"body"}, Msg: "invalid request body", Type: "json_invalid",
		}}})
		return nil, false
	}
	for key := range raw {
		if !variantAllowedKeys[key] {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"body", key}, Msg: "Extra inputs are not permitted", Type: "extra_forbidden",
			}}})
			return nil, false
		}
	}
	return raw, true
}

// strictDecode 把原始键值表解进结构体（键集已校验，不会再有未知字段）。
func strictDecode(raw map[string]json.RawMessage, dst any) error {
	merged, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(merged, dst)
}

// abortInvalidVariantBody 把结构化解码失败转成 FastAPI 风格 422。
func abortInvalidVariantBody(c *gin.Context, err error) {
	apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
		Loc: []string{"body"}, Msg: "invalid request body: " + err.Error(), Type: "json_invalid",
	}}})
}

// derefVariantString 创建必填 slug 的值拷贝（nil 已在调用前排除）。
func derefVariantString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// abortCharacterErr 把变体域错误映射到 Python 侧同形的状态码。
func abortCharacterErr(c *gin.Context, err error) {
	var (
		slugExists *service.CharacterVariantSlugExistsError
		tooLarge   *service.CharacterAvatarTooLargeError
	)
	switch {
	case errors.As(err, &slugExists):
		abortVariantError(c, http.StatusConflict, "character_variant_slug_exists",
			map[string]any{"slug": slugExists.Slug})
	case errors.As(err, &tooLarge):
		abortVariantError(c, http.StatusRequestEntityTooLarge, "character_avatar_pixels_too_large",
			map[string]any{"max_megapixels": tooLarge.MaxMegapixels})
	case errors.Is(err, service.ErrVariantNotFound):
		abortVariantError(c, http.StatusNotFound, "character_variant_not_found",
			map[string]any{"name": c.Param("variant_id")})
	case errors.Is(err, service.ErrVariantInvalidSlug):
		abortVariantError(c, http.StatusUnprocessableEntity, "character_variant_invalid_slug", nil)
	case errors.Is(err, service.ErrVariantInvalidStatus):
		abortVariantError(c, http.StatusUnprocessableEntity, "character_variant_invalid_status", nil)
	case errors.Is(err, service.ErrVariantInvalidPath):
		abortVariantError(c, http.StatusUnprocessableEntity, "character_variant_invalid_image_path", nil)
	case errors.Is(err, service.ErrAvatarInvalidFile):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "character_avatar_invalid_file", nil))
	case errors.Is(err, service.ErrAssetNotFound):
		// FileNotFoundError→project_not_found / KeyError→character_not_found 的可观察
		// 差异在这里收敛：角色缺失更常见，统一按 character_not_found。
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "character_not_found",
			map[string]any{"name": c.Param("entry")}))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}

// abortVariantError 与 Python _variant_error 同形：detail 是翻译后的字符串。
func abortVariantError(c *gin.Context, status int, key string, params map[string]any) {
	apperr.Abort(c, apperr.NewText(status, key, params))
}

// isKnownVariantStatus 校验状态枚举（与域层集合一致，用于请求边界 422）。
func isKnownVariantStatus(status string) bool {
	switch status {
	case "draft", "ready", "missing", "archived":
		return true
	}
	return false
}
