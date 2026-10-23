package asset

import (
	"errors"
	"net/http"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/i18n"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/asset"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"
)

// AssetDeps 是资产 handler 的依赖。
type AssetDeps struct {
	Assets *asset.AssetService
	Store  *service.ProjectStore
}

// assetI18nKeys 是各类型的错误文案 key。scene 沿用历史前缀 project_scene_*，
// 与 Python 侧 _I18N_KEYS 一致 —— 前端已按这些 key 出文案，不改名即是契约。
var assetI18nKeys = map[domain.AssetKind][3]string{
	domain.AssetCharacter: {"character_already_exists", "character_not_found", "character_deleted"},
	domain.AssetScene:     {"project_scene_already_exists", "project_scene_not_found", "project_scene_deleted"},
	domain.AssetProp:      {"prop_already_exists", "prop_not_found", "prop_deleted"},
	domain.AssetProduct:   {"product_already_exists", "product_not_found", "product_deleted"},
}

// RegisterAssets 按 spec 注册四类资产的 CRUD 路由。
//
// 全部返回 200（含 DELETE，与 Python 侧一致：装饰器没带 status_code，
// 前端按 200 + {"success":true} 判定，改成 201/204 会破坏既有调用方）。
//
// 路由树注意：/projects/:name/{subdir}/:entry 与 /projects/:name/{subdir}/:entry/rename
// 的段数不同，不冲突；但同层不能再引入与 :entry 竞争静态段的路径。
func RegisterAssets(group *gin.RouterGroup, d AssetDeps) {
	for _, kind := range domain.AssetTypeOrder {
		spec := domain.AssetSpecs[kind]
		base := "/projects/:name/" + spec.Subdir
		group.POST(base, createAsset(d, kind))
		group.PATCH(base+"/:entry", updateAsset(d, kind))
		group.POST(base+"/:entry/rename", renameAsset(d, kind))
		group.DELETE(base+"/:entry", deleteAsset(d, kind))
	}
}

func createAsset(d AssetDeps, kind domain.AssetKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := httpkit.DecodeBody(c)
		if !ok {
			return
		}
		name, ok := httpkit.RequiredString(c, body, "name")
		if !ok {
			return
		}
		description := ""
		if raw, present := body["description"]; present && raw != nil {
			s, ok := raw.(string)
			if !ok {
				apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
					{Loc: []string{"body", "description"}, Msg: "value is not a valid string", Type: "type_error.str"},
				}})
				return
			}
			description = s
		}
		entry, err := d.Assets.Create(c.Param("name"), kind, name, description, body)
		if err != nil {
			abortAssetErr(c, kind, err, name)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, string(kind): entry})
	}
}

func updateAsset(d AssetDeps, kind domain.AssetKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := httpkit.DecodeBody(c)
		if !ok {
			return
		}
		entryName := c.Param("entry")
		entry, err := d.Assets.Patch(c.Param("name"), kind, entryName, body)
		if err != nil {
			abortAssetErr(c, kind, err, entryName)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, string(kind): entry})
	}
}

func renameAsset(d AssetDeps, kind domain.AssetKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := httpkit.DecodeBody(c)
		if !ok {
			return
		}
		newName, ok := httpkit.RequiredString(c, body, "new_name")
		if !ok {
			return
		}
		dryRun := false
		if raw, present := body["dry_run"]; present && raw != nil {
			b, ok := raw.(bool)
			if !ok {
				apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
					{Loc: []string{"body", "dry_run"}, Msg: "value is not a valid boolean", Type: "type_error.bool"},
				}})
				return
			}
			dryRun = b
		}
		entryName := c.Param("entry")
		report, err := d.Assets.Rename(c.Param("name"), kind, entryName, newName, dryRun)
		if err != nil {
			abortAssetErr(c, kind, err, entryName)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"success":    true,
			"dry_run":    report.DryRun,
			"old_name":   report.OldName,
			"new_name":   report.NewName,
			"episodes":   report.Episodes,
			"references": report.References,
			"files":      report.Files,
		})
	}
}

func deleteAsset(d AssetDeps, kind domain.AssetKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		entryName := c.Param("entry")
		if err := d.Assets.Delete(c.Param("name"), kind, entryName); err != nil {
			abortAssetErr(c, kind, err, entryName)
			return
		}
		keys := assetI18nKeys[kind]
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": i18n.T(i18n.FromRequest(c.Request), keys[2], map[string]any{"name": entryName}),
		})
	}
}

// abortAssetErr 把服务层错误映射成统一 {"detail": ...} 信封。
func abortAssetErr(c *gin.Context, kind domain.AssetKind, err error, subject string) {
	keys := assetI18nKeys[kind]
	var (
		nameErr    *asset.AssetNameError
		existsErr  *asset.AssetExistsError
		fieldErr   *asset.AssetFieldError
		collision  *asset.RenameFileCollisionError
		invalid    *service.ProjectInvalidError
		traversal  *fs.PathTraversalError
		statusCode int
		key        string
		params     map[string]any
	)
	switch {
	case errors.As(err, &nameErr):
		statusCode, key, params = http.StatusBadRequest, "asset_invalid_name", map[string]any{"name": nameErr.Raw}
	case errors.As(err, &existsErr):
		statusCode, key, params = http.StatusConflict, keys[0], map[string]any{"name": existsErr.Name}
	case errors.Is(err, service.ErrAssetNotFound):
		statusCode, key, params = http.StatusNotFound, keys[1], map[string]any{"name": subject}
	case errors.As(err, &fieldErr):
		// 类型不匹配是调用方构造出的非法请求，文案与 Python 侧一样是不走翻译的英文原文；
		// voice_notice_dismissed_at 过期则真实用户也会看到，须翻译。
		switch fieldErr.Kind {
		case asset.FieldVoiceNoticeStale:
			statusCode, key = http.StatusUnprocessableEntity, "asset_voice_notice_dismissed_at_stale"
		case asset.FieldMustBeList:
			c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": "field '" + fieldErr.Field + "' must be a list of strings"})
			return
		default:
			c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": "field '" + fieldErr.Field + "' must be a string"})
			return
		}
	case errors.As(err, &collision):
		statusCode, key, params = http.StatusConflict, "asset_rename_file_conflict",
			map[string]any{"filename": filepath.Base(collision.Path)}
	case errors.As(err, &invalid):
		// 结构「不更坏」校验拒绝：整体未落盘，提示用户重试或检查项目数据。
		statusCode, key, params = http.StatusUnprocessableEntity, "asset_rename_rejected", map[string]any{"name": subject}
	case errors.Is(err, fs.ErrNotFound), errors.As(err, &traversal):
		// 项目不存在与路径越界一律 404，且不用错误差异区分「名字非法」和「确实不存在」。
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "project_not_found", map[string]any{"name": c.Param("name")}))
		return
	default:
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	apperr.Abort(c, apperr.NewText(statusCode, key, params))
}
