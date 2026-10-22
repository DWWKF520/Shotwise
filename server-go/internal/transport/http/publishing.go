package http

// 发布路由（对应 server/routers/publishing.py）。

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/service/publishing"
)

// PublishingDeps 是发布 handler 的依赖。
type PublishingDeps struct {
	Publishing *publishing.PublishingService
	Events     *publishing.PublishJobEvents
}

// RegisterPublishing 注册发布路由。
func RegisterPublishing(group *gin.RouterGroup, d PublishingDeps) {
	group.GET("/publishing/platforms", listPublishingPlatforms(d))
	group.GET("/publishing/accounts", listPublishingAccounts(d))
	group.POST("/publishing/accounts", createPublishingAccount(d))
	group.DELETE("/publishing/accounts/:account_id", revokePublishingAccount(d))

	group.POST("/render-artifacts/:artifact_id/publish", createPublishJob(d))
	group.GET("/publish-jobs", listPublishJobs(d))
	group.GET("/publish-jobs/:job_id", getPublishJob(d))
	group.POST("/publish-jobs/:job_id/retry", retryPublishJob(d))
	group.POST("/publish-jobs/:job_id/cancel", cancelPublishJob(d))
	group.POST("/publish-jobs/:job_id/poll", pollPublishJob(d))
	group.POST("/publish-jobs/:job_id/retract", retractPublishJob(d))
}

func listPublishingPlatforms(d PublishingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, d.Publishing.ListPlatforms())
	}
}

func listPublishingAccounts(d PublishingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		accounts, err := d.Publishing.ListPublishingAccounts(c.Request.Context(), currentUserID(c))
		if err != nil {
			abortPublishingErr(c, err)
			return
		}
		c.JSON(http.StatusOK, accounts)
	}
}

func createPublishingAccount(d PublishingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := decodeBody(c)
		if !ok {
			return
		}
		platform, ok := requiredString(c, body, "platform")
		if !ok {
			return
		}
		platformAccountID, ok := requiredString(c, body, "platform_account_id")
		if !ok {
			return
		}
		accountName, ok := requiredString(c, body, "account_name")
		if !ok {
			return
		}
		accessToken, ok := requiredString(c, body, "access_token")
		if !ok {
			return
		}
		var expiresAt *time.Time
		if raw, present := body["token_expires_at"]; present && raw != nil {
			text, ok := raw.(string)
			if !ok {
				abortFieldConstraint(c, "token_expires_at", "datetime_parsing")
				return
			}
			parsed, err := parseISOTimestamp(text)
			if err != nil {
				abortFieldConstraint(c, "token_expires_at", "datetime_parsing")
				return
			}
			expiresAt = &parsed
		}
		account, err := d.Publishing.CreatePublishingAccount(c.Request.Context(), publishing.CreatePublishingAccountParams{
			UserID:            currentUserID(c),
			Platform:          platform,
			PlatformAccountID: platformAccountID,
			AccountName:       accountName,
			AccessToken:       accessToken,
			RefreshToken:      optionalBodyString(body, "refresh_token"),
			AvatarURL:         optionalBodyString(body, "avatar_url"),
			Scopes:            stringSlice(body["scopes"]),
			TokenExpiresAt:    expiresAt,
		})
		if err != nil {
			abortPublishingErr(c, err)
			return
		}
		c.JSON(http.StatusCreated, account)
	}
}

func revokePublishingAccount(d PublishingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Publishing.RevokePublishingAccount(
			c.Request.Context(), c.Param("account_id"), currentUserID(c),
		)
		if err != nil {
			abortPublishingErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func createPublishJob(d PublishingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := decodeBody(c)
		if !ok {
			return
		}
		reviewSnapshotID, ok := requiredString(c, body, "review_snapshot_id")
		if !ok {
			return
		}
		platform, ok := requiredString(c, body, "platform")
		if !ok {
			return
		}
		idempotencyKey, ok := requiredString(c, body, "idempotency_key")
		if !ok {
			return
		}
		maxAttempts := 3
		if v := optionalBodyInt(body, "max_attempts"); v != nil {
			maxAttempts = int(*v)
		}
		result, err := d.Publishing.CreatePublishJob(c.Request.Context(), c.Param("artifact_id"),
			publishing.CreatePublishJobParams{
				UserID:           currentUserID(c),
				ReviewSnapshotID: reviewSnapshotID,
				Platform:         platform,
				IdempotencyKey:   idempotencyKey,
				AccountID:        optionalBodyString(body, "account_id"),
				Destination:      anyMap(body["destination"]),
				MaxAttempts:      maxAttempts,
			})
		if err != nil {
			abortPublishingErr(c, err)
			return
		}
		c.JSON(http.StatusAccepted, result)
	}
}

func listPublishJobs(d PublishingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := 20
		if parsed, err := parseIntQuery(c, "limit", 1, 100); err == nil && parsed != nil {
			limit = int(*parsed)
		} else if err != nil {
			return
		}
		offset := 0
		if parsed, err := parseIntQuery(c, "offset", 0, 1<<31); err == nil && parsed != nil {
			offset = int(*parsed)
		} else if err != nil {
			return
		}
		result, err := d.Publishing.ListPublishJobs(c.Request.Context(), currentUserID(c), limit, offset)
		if err != nil {
			abortPublishingErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func getPublishJob(d PublishingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Publishing.GetPublishJob(c.Request.Context(), c.Param("job_id"), currentUserID(c))
		if err != nil {
			abortPublishingErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func retryPublishJob(d PublishingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Publishing.RetryPublishJob(c.Request.Context(), c.Param("job_id"), currentUserID(c))
		if err != nil {
			abortPublishingErr(c, err)
			return
		}
		c.JSON(http.StatusAccepted, result)
	}
}

func cancelPublishJob(d PublishingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Publishing.CancelPublishJob(c.Request.Context(), c.Param("job_id"), currentUserID(c))
		if err != nil {
			abortPublishingErr(c, err)
			return
		}
		if d.Events != nil {
			if project, ok := result["project_name"].(string); ok {
				d.Events.Emit(project, c.Param("job_id"), "webui")
			}
		}
		c.JSON(http.StatusAccepted, result)
	}
}

func pollPublishJob(d PublishingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Publishing.PollPublishJob(c.Request.Context(), c.Param("job_id"), currentUserID(c))
		if err != nil {
			abortPublishingErr(c, err)
			return
		}
		c.JSON(http.StatusAccepted, result)
	}
}

func retractPublishJob(d PublishingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Publishing.RetractPublishJob(c.Request.Context(), c.Param("job_id"), currentUserID(c))
		if err != nil {
			abortPublishingErr(c, err)
			return
		}
		c.JSON(http.StatusAccepted, result)
	}
}

// --- 小工具 ---

// stringSlice 把 JSON 数组字段转成字符串切片；非数组返回空切片。
func stringSlice(value any) []string {
	list, ok := value.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// parseIntQuery 解析 query 整数并在越界时写出 422；参数缺失返回 (nil, nil)。
func parseIntQuery(c *gin.Context, field string, min, max int) (*int, error) {
	raw, present := c.GetQuery(field)
	if !present || raw == "" {
		return nil, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		abortFieldConstraint(c, field, "greater_than_equal")
		return nil, errors.New("invalid query integer")
	}
	return &value, nil
}

// parseISOTimestamp 解析 ISO8601 时间戳（支持带偏移与不带偏移两种形态）。
func parseISOTimestamp(text string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999", "2006-01-02T15:04:05"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, errors.New("invalid ISO timestamp")
}

// abortPublishingErr 把发布领域错误映射成统一 {"detail": ...} 信封。
func abortPublishingErr(c *gin.Context, err error) {
	var (
		notFound    *publishing.PublishJobNotFoundError
		conflict    *publishing.PublishConflictError
		validation  *publishing.PublishValidationError
		unavailable *publishing.PublishAdapterUnavailableError
	)
	switch {
	case errors.As(err, &notFound):
		apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "publish_resource_not_found"))
	case errors.As(err, &conflict):
		params := map[string]any{}
		if conflict.Status != "" {
			params["status"] = conflict.Status
		}
		apperr.Abort(c, apperr.New(http.StatusConflict, conflict.Code, params))
	case errors.As(err, &validation):
		apperr.Abort(c, apperr.New(http.StatusUnprocessableEntity, validation.Code, nil))
	case errors.As(err, &unavailable):
		apperr.Abort(c, apperr.New(http.StatusServiceUnavailable, unavailable.Code, nil))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
