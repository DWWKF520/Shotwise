// 首次使用引导状态与 API Key 管理路由（对应 server/routers/onboarding.py 与 api_keys.py）。
//
// 两个路由组共用一个文件：它们都是「实例级小配置面」，且都只服务管理端 UI。
//
// 语义要点：
//   - 引导标记刻意不放进 /system/config 的读写白名单：它不是用户可配置项，也不该被配置页
//     的批量读写带上；
//   - API Key 的管理操作不允许由 API Key 本身执行（必须是 JWT 身份），否则一把泄漏的 key
//     能自我续期、自我提权；
//   - 完整 key 只在创建响应里出现一次，之后只暴露前缀。
package system

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"
	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
)

// OnboardingSeenKey 是「引导已看过」的设置项键。
const OnboardingSeenKey = "onboarding_seen"

// OnboardingDeps 是引导状态路由的依赖。
type OnboardingDeps struct {
	Config *config.ConfigService
}

// RegisterOnboarding 注册引导状态路由。
func RegisterOnboarding(group *gin.RouterGroup, d OnboardingDeps) {
	if d.Config == nil {
		return
	}
	group.GET("/onboarding/status", getOnboardingStatus(d))
	group.POST("/onboarding/seen", markOnboardingSeen(d))
}

// getOnboardingStatus 返回引导是否已看过；未设置视为未看过。
func getOnboardingStatus(d OnboardingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		value, err := d.Config.GetSetting(c.Request.Context(), OnboardingSeenKey, "")
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"seen": value == "true"})
	}
}

// markOnboardingSeen 标记引导已看过（幂等：跳过 / 看完 / 关闭三条退出路径都打到这里）。
func markOnboardingSeen(d OnboardingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := d.Config.SetSetting(c.Request.Context(), OnboardingSeenKey, "true"); err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"seen": true})
	}
}

// APIKeyDeps 是 API Key 管理路由的依赖。
type APIKeyDeps struct {
	DB *gorm.DB
}

// apiKeyDefaultExpiryDays 是未指定过期天数时的默认有效期。
const apiKeyDefaultExpiryDays = 30

// RegisterAPIKeys 注册 API Key 管理路由（整组要求管理员身份）。
func RegisterAPIKeys(group *gin.RouterGroup, d APIKeyDeps) {
	if d.DB == nil {
		return
	}
	admin := group.Group("", auth.RequireAdmin())
	admin.POST("/api-keys", createAPIKey(d))
	admin.GET("/api-keys", listAPIKeys(d))
	admin.DELETE("/api-keys/:key_id", deleteAPIKey(d))
}

// requireJWTAuth 确保请求不是用 API Key 认证的：管理操作不允许由 key 本身执行。
func requireJWTAuth(c *gin.Context) bool {
	if user := auth.Current(c); user != nil && user.AuthVia == "apikey" {
		apperr.Abort(c, apperr.NewText(http.StatusForbidden, "jwt_auth_required", nil))
		return false
	}
	return true
}

// createAPIKey 创建新 API Key；完整 key 仅在本次响应出现。
func createAPIKey(d APIKeyDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireJWTAuth(c) {
			return
		}
		var body struct {
			Name        string `json:"name"`
			ExpiresDays *int   `json:"expires_days"`
		}
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		var items []apperr.ValidationItem
		if strings.TrimSpace(body.Name) == "" {
			// Python 的 name: str 无长度约束，空串能过 pydantic；但空名会让列表无法分辨，
			// 这里按「必填」口径拒绝（与前端表单一致）。
			items = append(items, apperr.ValidationItem{
				Loc: []string{"body", "name"}, Msg: "field required", Type: "missing"})
		}
		if body.ExpiresDays != nil && *body.ExpiresDays < 0 {
			items = append(items, apperr.ValidationItem{
				Loc: []string{"body", "expires_days"}, Msg: "Input should be greater than or equal to 0",
				Type: "greater_than_equal"})
		}
		if len(items) > 0 {
			apperr.Abort(c, &apperr.Validation{Items: items})
			return
		}

		plaintext, keyHash, err := auth.GenerateAPIKey()
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		keyPrefix := plaintext[:len(auth.APIKeyPrefix)+4]

		var expiresAt *time.Time
		if body.ExpiresDays == nil {
			value := time.Now().UTC().AddDate(0, 0, apiKeyDefaultExpiryDays)
			expiresAt = &value
		} else if *body.ExpiresDays > 0 {
			value := time.Now().UTC().AddDate(0, 0, *body.ExpiresDays)
			expiresAt = &value
		}
		// expires_days == 0 表示不过期：expires_at 留空。

		now := time.Now().UTC()
		row := repository.APIKey{
			Name:      body.Name,
			KeyHash:   keyHash,
			KeyPrefix: keyPrefix,
			CreatedAt: db.NewFlexibleTime(now),
			UpdatedAt: db.NewFlexibleTime(now),
			UserID:    httpkit.CurrentUserID(c),
		}
		if expiresAt != nil {
			row.ExpiresAt = db.NewFlexibleTime(*expiresAt)
		}
		if err := d.DB.WithContext(c.Request.Context()).Create(&row).Error; err != nil {
			// 名称唯一约束冲突：429/409 都不可接受，Python 用 409 api_key_name_exists。
			if isUniqueViolation(err) {
				apperr.Abort(c, apperr.NewText(http.StatusConflict, "api_key_name_exists",
					map[string]any{"name": body.Name}))
				return
			}
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusCreated, gin.H{
			"id":         row.ID,
			"name":       row.Name,
			"key":        plaintext,
			"key_prefix": row.KeyPrefix,
			"created_at": isoOrEmpty(row.CreatedAt),
			"expires_at": nullableISO(row.ExpiresAt),
		})
	}
}

// listAPIKeys 查询所有 API Key 的元数据（不含完整 key）。
func listAPIKeys(d APIKeyDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireJWTAuth(c) {
			return
		}
		var rows []repository.APIKey
		if err := d.DB.WithContext(c.Request.Context()).
			Order("id ASC").Find(&rows).Error; err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		out := make([]any, 0, len(rows))
		for _, row := range rows {
			out = append(out, gin.H{
				"id":           row.ID,
				"name":         row.Name,
				"key_prefix":   row.KeyPrefix,
				"created_at":   isoOrEmpty(row.CreatedAt),
				"expires_at":   nullableISO(row.ExpiresAt),
				"last_used_at": nullableISO(row.LastUsedAt),
			})
		}
		c.JSON(http.StatusOK, out)
	}
}

// deleteAPIKey 吊销指定 API Key。
func deleteAPIKey(d APIKeyDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireJWTAuth(c) {
			return
		}
		keyID, ok := httpkit.QueryInt64(c, "key_id")
		if !ok {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"path", "key_id"}, Msg: "Input should be a valid integer", Type: "int_parsing",
			}}})
			return
		}
		res := d.DB.WithContext(c.Request.Context()).Where("id = ?", keyID).Delete(&repository.APIKey{})
		if res.Error != nil {
			apperr.Abort(c, apperr.Internal(res.Error))
			return
		}
		if res.RowsAffected == 0 {
			apperr.Abort(c, apperr.NewText(http.StatusNotFound, "api_key_not_found",
				map[string]any{"key_id": keyID}))
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// ---- 助手 ----

// isUniqueViolation 判定唯一约束冲突（SQLite / Postgres 文案都覆盖）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique") || strings.Contains(message, "duplicate")
}

// isoOrEmpty 把时间列渲染成 Python isoformat 形态；零值渲染空串。
func isoOrEmpty(t db.FlexibleTime) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.999999+00:00")
}

// nullableISO 把可空时间列渲染成 isoformat 或 null。
func nullableISO(t db.FlexibleTime) any {
	if t.IsZero() {
		return nil
	}
	return isoOrEmpty(t)
}
