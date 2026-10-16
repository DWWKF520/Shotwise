package repository

// 发布任务 / 发布账号 / 渲染产物的归属查询与写回（对应
// server/services/publishing.py 内使用的 SQLAlchemy 读写原语）。

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

// PublishingRepo 读写 publishing_accounts / publish_jobs 及其绑定的渲染产物。
type PublishingRepo struct{ db *gorm.DB }

// NewPublishingRepo 构造仓储。
func NewPublishingRepo(gdb *gorm.DB) *PublishingRepo { return &PublishingRepo{db: gdb} }

func takeOwned[T any](db *gorm.DB, ctx context.Context, id, userID string) (*T, error) {
	var row T
	err := db.WithContext(ctx).Where("id = ? AND user_id = ?", id, userID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// GetOwnedJob 仅在发布任务属于请求用户时返回。
func (r *PublishingRepo) GetOwnedJob(ctx context.Context, jobID, userID string) (*PublishJob, error) {
	return takeOwned[PublishJob](r.db, ctx, jobID, userID)
}

// GetOwnedArtifact 仅在渲染产物属于请求用户时返回。
func (r *PublishingRepo) GetOwnedArtifact(ctx context.Context, artifactID, userID string) (*RenderArtifact, error) {
	return takeOwned[RenderArtifact](r.db, ctx, artifactID, userID)
}

// GetOwnedAccount 仅在发布账号属于请求用户时返回。
func (r *PublishingRepo) GetOwnedAccount(ctx context.Context, accountID, userID string) (*PublishingAccount, error) {
	return takeOwned[PublishingAccount](r.db, ctx, accountID, userID)
}

// GetOwnedPlan 仅在装配计划属于请求用户时返回。
func (r *PublishingRepo) GetOwnedPlan(ctx context.Context, planID, userID string) (*AssemblyPlan, error) {
	return takeOwned[AssemblyPlan](r.db, ctx, planID, userID)
}

// GetOwnedSnapshot 仅在复核快照属于请求用户时返回。
func (r *PublishingRepo) GetOwnedSnapshot(ctx context.Context, snapshotID, userID string) (*RenderReviewSnapshot, error) {
	return takeOwned[RenderReviewSnapshot](r.db, ctx, snapshotID, userID)
}

// ListAccounts 按 created_at 升序列出用户的发布账号。
func (r *PublishingRepo) ListAccounts(ctx context.Context, userID string) ([]PublishingAccount, error) {
	var accounts []PublishingAccount
	err := r.db.WithContext(ctx).Where("user_id = ?", userID).Order("created_at").Find(&accounts).Error
	return accounts, err
}

// CreateAccount 写入发布账号。
func (r *PublishingRepo) CreateAccount(ctx context.Context, account *PublishingAccount) error {
	return r.db.WithContext(ctx).Create(account).Error
}

// SaveAccount 以显式列写回账号的可变字段（含把令牌清空为 NULL）。
func (r *PublishingRepo) SaveAccount(ctx context.Context, account *PublishingAccount) error {
	return r.db.WithContext(ctx).Model(&PublishingAccount{}).Where("id = ?", account.ID).Updates(map[string]any{
		"status":                  account.Status,
		"encrypted_access_token":  account.EncryptedAccessToken,
		"encrypted_refresh_token": account.EncryptedRefreshToken,
		"token_expires_at":        account.TokenExpiresAt,
		"last_refresh_at":         account.LastRefreshAt,
		"last_error_code":         account.LastErrorCode,
		"updated_at":              account.UpdatedAt,
	}).Error
}

// FindJobByIDempotencyKey 按幂等键查重；不存在返回 (nil, nil)。
func (r *PublishingRepo) FindJobByIDempotencyKey(ctx context.Context, userID, key string) (*PublishJob, error) {
	var job PublishJob
	err := r.db.WithContext(ctx).Where("user_id = ? AND idempotency_key = ?", userID, key).Take(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// CreateJob 写入发布任务。
func (r *PublishingRepo) CreateJob(ctx context.Context, job *PublishJob) error {
	return r.db.WithContext(ctx).Create(job).Error
}

// SaveJob 以显式列写回发布任务的可变字段（含把错误 / 轮询时间清空为 NULL）。
func (r *PublishingRepo) SaveJob(ctx context.Context, job *PublishJob) error {
	return r.db.WithContext(ctx).Model(&PublishJob{}).Where("id = ?", job.ID).Updates(map[string]any{
		"account_id":          job.AccountID,
		"status":              job.Status,
		"attempt":             job.Attempt,
		"max_attempts":        job.MaxAttempts,
		"error_code":          job.ErrorCode,
		"error_message":       job.ErrorMessage,
		"external_content_id": job.ExternalContentID,
		"external_status":     job.ExternalStatus,
		"next_poll_at":        job.NextPollAt,
		"last_polled_at":      job.LastPolledAt,
		"started_at":          job.StartedAt,
		"completed_at":        job.CompletedAt,
		"worker_id":           job.WorkerID,
		"lease_until":         job.LeaseUntil,
		"updated_at":          job.UpdatedAt,
	}).Error
}

// ListJobs 按 created_at 倒序分页，返回 (items, total)。
func (r *PublishingRepo) ListJobs(ctx context.Context, userID string, limit, offset int) ([]PublishJob, int64, error) {
	var total int64
	if err := r.db.WithContext(ctx).Model(&PublishJob{}).Where("user_id = ?", userID).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var jobs []PublishJob
	err := r.db.WithContext(ctx).Where("user_id = ?", userID).
		Order("created_at DESC, id DESC").Offset(offset).Limit(limit).Find(&jobs).Error
	return jobs, total, err
}
