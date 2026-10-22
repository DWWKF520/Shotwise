// 与平台无关的发布任务持久化与生命周期服务（对应 server/services/publishing.py）。
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
)

// PublishJobEvents 发出发布任务的项目变更刷新信号（对应 server/services/publishing_events.py）。
type PublishJobEvents struct {
	hub   *EventHub
	store *ProjectStore
}

// NewPublishJobEvents 构造发布事件发射器。
func NewPublishJobEvents(hub *EventHub, store *ProjectStore) *PublishJobEvents {
	return &PublishJobEvents{hub: hub, store: store}
}

// Emit 尽力发出一次发布任务刷新信号；缺项目名 / 任务 id 时直接跳过。
func (e *PublishJobEvents) Emit(projectName, jobID, source string) {
	project := strings.TrimSpace(projectName)
	id := strings.TrimSpace(jobID)
	if project == "" || id == "" || e.hub == nil {
		return
	}
	fingerprint := ""
	if e.store != nil {
		if dir, err := e.store.Dir(project); err == nil {
			fingerprint = ProjectFingerprint(dir)
		}
	}
	e.hub.Publish(ChangeBatch{
		ProjectName: project,
		BatchID:     uuid.NewString(),
		Fingerprint: fingerprint,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Source:      source,
		Changes: []Change{{
			EntityType: "publish_job",
			Action:     "publish_job_updated",
			EntityID:   id,
			Label:      id,
			Important:  false,
		}},
	})
}

// PublishJobNotFoundError 表示发布任务或其归属资源不存在（HTTP 404）。
type PublishJobNotFoundError struct{ Ref string }

func (e *PublishJobNotFoundError) Error() string { return e.Ref }

// PublishConflictError 表示请求与不可变绑定或状态冲突（HTTP 409）。
type PublishConflictError struct {
	Code   string
	Status string
}

func (e *PublishConflictError) Error() string { return e.Code }

// PublishValidationError 表示发布请求的平台 / 目的地数据非法（HTTP 422）。
type PublishValidationError struct{ Code string }

func (e *PublishValidationError) Error() string { return e.Code }

// PublishAdapterUnavailableError 表示真实平台适配器未接入（HTTP 503）。
type PublishAdapterUnavailableError struct{ Code string }

func (e *PublishAdapterUnavailableError) Error() string { return e.Code }

// PublishingService 承载发布账号与发布任务的读写与生命周期。
type PublishingService struct {
	db    *gorm.DB
	repo  *repository.PublishingRepo
	plans *repository.AssemblyPlanRepo
}

// NewPublishingService 构造服务。
func NewPublishingService(gdb *gorm.DB) *PublishingService {
	return &PublishingService{
		db:    gdb,
		repo:  repository.NewPublishingRepo(gdb),
		plans: repository.NewAssemblyPlanRepo(gdb),
	}
}

// --- 载荷渲染 ---

func publishJobPayload(job *repository.PublishJob) map[string]any {
	return map[string]any{
		"id":                   job.ID,
		"user_id":              job.UserID,
		"plan_id":              job.PlanID,
		"project_name":         job.ProjectName,
		"artifact_id":          job.ArtifactID,
		"review_snapshot_id":   job.ReviewSnapshotID,
		"account_id":           job.AccountID,
		"revision_number":      job.RevisionNumber,
		"source_fingerprint":   job.SourceFingerprint,
		"artifact_fingerprint": job.ArtifactFingerprint,
		"platform":             job.Platform,
		"destination":          parseJSONValue(job.DestinationJSON),
		"idempotency_key":      job.IdempotencyKey,
		"status":               job.Status,
		"attempt":              job.Attempt,
		"max_attempts":         job.MaxAttempts,
		"error_code":           job.ErrorCode,
		"error_message":        job.ErrorMessage,
		"external_content_id":  job.ExternalContentID,
		"external_status":      job.ExternalStatus,
		"next_poll_at":         nullableISO(job.NextPollAt),
		"last_polled_at":       nullableISO(job.LastPolledAt),
		"created_at":           isoformatTime(job.CreatedAt.Time),
		"started_at":           nullableISO(job.StartedAt),
		"completed_at":         nullableISO(job.CompletedAt),
		"updated_at":           isoformatTime(job.UpdatedAt.Time),
	}
}

func publishingAccountPayload(account *repository.PublishingAccount) map[string]any {
	var scopes []string
	if err := json.Unmarshal([]byte(account.ScopesJSON), &scopes); err != nil {
		scopes = []string{}
	}
	return map[string]any{
		"id":                     account.ID,
		"user_id":                account.UserID,
		"platform":               account.Platform,
		"platform_account_id":    account.PlatformAccountID,
		"account_name":           account.AccountName,
		"avatar_url":             account.AvatarURL,
		"scopes":                 scopes,
		"encryption_key_version": account.EncryptionKeyVersion,
		"status":                 account.Status,
		"token_expires_at":       nullableISO(account.TokenExpiresAt),
		"last_refresh_at":        nullableISO(account.LastRefreshAt),
		"last_error_code":        account.LastErrorCode,
		"created_at":             isoformatTime(account.CreatedAt.Time),
		"updated_at":             isoformatTime(account.UpdatedAt.Time),
	}
}

// --- 平台与账号 ---

// ListPlatforms 返回内置平台能力声明（真实适配器尚未接入）。
func (s *PublishingService) ListPlatforms() []PlatformCapabilities {
	return ListPlatformCapabilities()
}

// ListPublishingAccounts 列出用户的发布账号。
func (s *PublishingService) ListPublishingAccounts(ctx context.Context, userID string) ([]map[string]any, error) {
	accounts, err := s.repo.ListAccounts(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(accounts))
	for i := range accounts {
		out = append(out, publishingAccountPayload(&accounts[i]))
	}
	return out, nil
}

// CreatePublishingAccountParams 是账号创建的入参。
type CreatePublishingAccountParams struct {
	UserID            string
	Platform          string
	PlatformAccountID string
	AccountName       string
	AccessToken       string
	RefreshToken      *string
	AvatarURL         *string
	Scopes            []string
	TokenExpiresAt    *time.Time
}

// CreatePublishingAccount 保存一个已完成的 OAuth 账号（令牌加密存储，永不回传）。
func (s *PublishingService) CreatePublishingAccount(
	ctx context.Context, p CreatePublishingAccountParams,
) (map[string]any, error) {
	platform, err := normalizePublishPlatform(p.Platform)
	if err != nil {
		return nil, err
	}
	if _, ok := GetPlatformCapabilities(platform); !ok {
		return nil, &PublishValidationError{Code: "platform_not_supported"}
	}
	if strings.TrimSpace(p.PlatformAccountID) == "" || strings.TrimSpace(p.AccountName) == "" || p.AccessToken == "" {
		return nil, &PublishValidationError{Code: "account_fields_invalid"}
	}
	encryptedAccess, err := EncryptPublishingSecret(p.AccessToken)
	if err != nil {
		return nil, err
	}
	var encryptedRefresh *string
	if p.RefreshToken != nil && *p.RefreshToken != "" {
		value, err := EncryptPublishingSecret(*p.RefreshToken)
		if err != nil {
			return nil, err
		}
		encryptedRefresh = &value
	}
	scopesJSON, err := json.Marshal(dedupSortedStrings(p.Scopes))
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	account := &repository.PublishingAccount{
		ID:                    uuid.NewString(),
		UserID:                p.UserID,
		Platform:              platform,
		PlatformAccountID:     strings.TrimSpace(p.PlatformAccountID),
		AccountName:           strings.TrimSpace(p.AccountName),
		AvatarURL:             p.AvatarURL,
		EncryptedAccessToken:  &encryptedAccess,
		EncryptedRefreshToken: encryptedRefresh,
		ScopesJSON:            string(scopesJSON),
		EncryptionKeyVersion:  publishingKeyVersion,
		Status:                "active",
		CreatedAt:             db.NewFlexibleTime(now),
		UpdatedAt:             db.NewFlexibleTime(now),
	}
	if p.TokenExpiresAt != nil {
		account.TokenExpiresAt = db.NewFlexibleTime(*p.TokenExpiresAt)
	}
	if err := s.repo.CreateAccount(ctx, account); err != nil {
		if db.IsUniqueViolation(err) {
			return nil, &PublishConflictError{Code: "publishing_account_already_exists"}
		}
		return nil, err
	}
	return publishingAccountPayload(account), nil
}

// RevokePublishingAccount 撤销账号并清空其令牌。
func (s *PublishingService) RevokePublishingAccount(ctx context.Context, accountID, userID string) (map[string]any, error) {
	account, err := s.repo.GetOwnedAccount(ctx, accountID, userID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, &PublishJobNotFoundError{Ref: accountID}
	}
	account.Status = "revoked"
	account.EncryptedAccessToken = nil
	account.EncryptedRefreshToken = nil
	account.UpdatedAt = db.NewFlexibleTime(time.Now().UTC())
	if err := s.repo.SaveAccount(ctx, account); err != nil {
		return nil, err
	}
	return publishingAccountPayload(account), nil
}

// --- 发布任务 ---

// CreatePublishJobParams 是发布任务创建的入参。
type CreatePublishJobParams struct {
	UserID           string
	ReviewSnapshotID string
	Platform         string
	IdempotencyKey   string
	AccountID        *string
	Destination      map[string]any
	MaxAttempts      int
}

// CreatePublishJob 校验绑定后入队一条发布任务（幂等键冲突按指纹判定复用或拒绝）。
func (s *PublishingService) CreatePublishJob(
	ctx context.Context, artifactID string, p CreatePublishJobParams,
) (map[string]any, error) {
	platform, err := normalizePublishPlatform(p.Platform)
	if err != nil {
		return nil, err
	}
	idempotencyKey := strings.TrimSpace(p.IdempotencyKey)
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return nil, &PublishValidationError{Code: "idempotency_key_invalid"}
	}
	if p.MaxAttempts < 1 || p.MaxAttempts > 10 {
		return nil, &PublishValidationError{Code: "max_attempts_invalid"}
	}
	if p.AccountID != nil {
		account, err := s.repo.GetOwnedAccount(ctx, *p.AccountID, p.UserID)
		if err != nil {
			return nil, err
		}
		if account == nil {
			return nil, &PublishJobNotFoundError{Ref: *p.AccountID}
		}
		if account.Platform != platform {
			return nil, &PublishConflictError{Code: "account_platform_mismatch", Status: account.Status}
		}
		if account.Status != "active" {
			return nil, &PublishConflictError{Code: "publishing_account_inactive", Status: account.Status}
		}
	}
	artifact, err := s.repo.GetOwnedArtifact(ctx, artifactID, p.UserID)
	if err != nil {
		return nil, err
	}
	if artifact == nil {
		return nil, &PublishJobNotFoundError{Ref: artifactID}
	}
	plan, revision, snapshot, err := s.validateBinding(ctx, artifact, p.ReviewSnapshotID, p.UserID)
	if err != nil {
		return nil, err
	}
	destination := p.Destination
	if destination == nil {
		destination = map[string]any{}
	}
	destinationJSON, err := canonicalString(destination)
	if err != nil {
		return nil, err
	}
	fingerprint := publishRequestFingerprint(artifact.ID, snapshot.ID, platform, destinationJSON, p.MaxAttempts)

	existing, err := s.repo.FindJobByIDempotencyKey(ctx, p.UserID, idempotencyKey)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.RequestFingerprint != fingerprint {
			return nil, &PublishConflictError{Code: "idempotency_key_reused", Status: existing.Status}
		}
		return publishJobPayload(existing), nil
	}

	now := time.Now().UTC()
	job := &repository.PublishJob{
		ID:                  uuid.NewString(),
		UserID:              p.UserID,
		PlanID:              plan.ID,
		ProjectName:         plan.ProjectName,
		ArtifactID:          artifact.ID,
		ReviewSnapshotID:    snapshot.ID,
		AccountID:           p.AccountID,
		RevisionNumber:      revision.VersionNumber,
		SourceFingerprint:   revision.SourceFingerprint,
		ArtifactFingerprint: artifact.Fingerprint,
		Platform:            platform,
		DestinationJSON:     destinationJSON,
		IdempotencyKey:      idempotencyKey,
		RequestFingerprint:  fingerprint,
		Status:              "queued",
		Attempt:             0,
		MaxAttempts:         int64(p.MaxAttempts),
		CreatedAt:           db.NewFlexibleTime(now),
		UpdatedAt:           db.NewFlexibleTime(now),
	}
	if err := s.repo.CreateJob(ctx, job); err != nil {
		if !db.IsUniqueViolation(err) {
			return nil, err
		}
		existing, findErr := s.repo.FindJobByIDempotencyKey(ctx, p.UserID, idempotencyKey)
		if findErr != nil {
			return nil, findErr
		}
		if existing == nil {
			return nil, err
		}
		if existing.RequestFingerprint != fingerprint {
			return nil, &PublishConflictError{Code: "idempotency_key_reused", Status: existing.Status}
		}
		return publishJobPayload(existing), nil
	}
	return publishJobPayload(job), nil
}

// GetPublishJob 读取单条发布任务。
func (s *PublishingService) GetPublishJob(ctx context.Context, jobID, userID string) (map[string]any, error) {
	job, err := s.ownedJob(ctx, jobID, userID)
	if err != nil {
		return nil, err
	}
	return publishJobPayload(job), nil
}

// ListPublishJobs 按创建时间倒序分页列出用户的发布历史。
func (s *PublishingService) ListPublishJobs(ctx context.Context, userID string, limit, offset int) (map[string]any, error) {
	safeLimit := limit
	if safeLimit < 1 {
		safeLimit = 1
	}
	if safeLimit > 100 {
		safeLimit = 100
	}
	safeOffset := offset
	if safeOffset < 0 {
		safeOffset = 0
	}
	jobs, total, err := s.repo.ListJobs(ctx, userID, safeLimit, safeOffset)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(jobs))
	for i := range jobs {
		items = append(items, publishJobPayload(&jobs[i]))
	}
	return map[string]any{
		"items":  items,
		"total":  total,
		"limit":  safeLimit,
		"offset": safeOffset,
	}, nil
}

// RetryPublishJob 在绑定仍成立时把失败任务重新入队。
func (s *PublishingService) RetryPublishJob(ctx context.Context, jobID, userID string) (map[string]any, error) {
	job, err := s.ownedJob(ctx, jobID, userID)
	if err != nil {
		return nil, err
	}
	switch job.Status {
	case "failed", "failed_retryable", "retract_failed":
	default:
		return nil, &PublishConflictError{Code: "retry_requires_failed", Status: job.Status}
	}
	if job.Attempt >= job.MaxAttempts {
		return nil, &PublishConflictError{Code: "retry_limit_reached", Status: job.Status}
	}
	artifact, err := s.repo.GetOwnedArtifact(ctx, job.ArtifactID, userID)
	if err != nil {
		return nil, err
	}
	if artifact == nil {
		return nil, &PublishJobNotFoundError{Ref: job.ArtifactID}
	}
	if _, _, _, err := s.validateBinding(ctx, artifact, job.ReviewSnapshotID, userID); err != nil {
		return nil, err
	}
	job.Status = "queued"
	job.ErrorCode = nil
	job.ErrorMessage = nil
	job.CompletedAt = db.FlexibleTime{}
	job.NextPollAt = db.FlexibleTime{}
	job.UpdatedAt = db.NewFlexibleTime(time.Now().UTC())
	if err := s.repo.SaveJob(ctx, job); err != nil {
		return nil, err
	}
	return publishJobPayload(job), nil
}

// CancelPublishJob 取消一条活跃发布请求（不回撤已发布内容）。
func (s *PublishingService) CancelPublishJob(ctx context.Context, jobID, userID string) (map[string]any, error) {
	job, err := s.ownedJob(ctx, jobID, userID)
	if err != nil {
		return nil, err
	}
	switch job.Status {
	case "queued", "publishing", "submitted", "processing", "publish_pending", "failed_retryable":
	default:
		return nil, &PublishConflictError{Code: "cancel_requires_active", Status: job.Status}
	}
	now := time.Now().UTC()
	code := "canceled_by_user"
	job.Status = "canceled"
	job.ErrorCode = &code
	job.ErrorMessage = &code
	job.NextPollAt = db.FlexibleTime{}
	job.WorkerID = nil
	job.LeaseUntil = db.FlexibleTime{}
	job.CompletedAt = db.NewFlexibleTime(now)
	job.UpdatedAt = db.NewFlexibleTime(now)
	if err := s.repo.SaveJob(ctx, job); err != nil {
		return nil, err
	}
	return publishJobPayload(job), nil
}

// PollPublishJob 轮询外部发布状态；未接入适配器时 fail closed。
func (s *PublishingService) PollPublishJob(ctx context.Context, jobID, userID string) (map[string]any, error) {
	job, err := s.ownedJob(ctx, jobID, userID)
	if err != nil {
		return nil, err
	}
	switch job.Status {
	case "submitted", "processing", "publish_pending":
	default:
		return nil, &PublishConflictError{Code: "poll_requires_submitted", Status: job.Status}
	}
	// 内置注册表没有网络客户端：适配器恒为 None。
	return nil, &PublishAdapterUnavailableError{Code: "platform_adapter_not_connected"}
}

// RetractPublishJob 回撤已发布内容；未接入适配器时 fail closed。
func (s *PublishingService) RetractPublishJob(ctx context.Context, jobID, userID string) (map[string]any, error) {
	job, err := s.ownedJob(ctx, jobID, userID)
	if err != nil {
		return nil, err
	}
	if job.Status != "published" {
		return nil, &PublishConflictError{Code: "retract_requires_published", Status: job.Status}
	}
	if capability, ok := GetPlatformCapabilities(job.Platform); ok && !capability.SupportsRetract {
		return nil, &PublishConflictError{Code: "platform_retract_not_supported", Status: job.Status}
	}
	return nil, &PublishAdapterUnavailableError{Code: "platform_adapter_not_connected"}
}

// --- 内部工具 ---

func (s *PublishingService) ownedJob(ctx context.Context, jobID, userID string) (*repository.PublishJob, error) {
	job, err := s.repo.GetOwnedJob(ctx, jobID, userID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, &PublishJobNotFoundError{Ref: jobID}
	}
	return job, nil
}

// validateBinding 校验 Final 产物 / 复核快照 / 当前 revision 三者的一致性。
func (s *PublishingService) validateBinding(
	ctx context.Context, artifact *repository.RenderArtifact, reviewSnapshotID, userID string,
) (*repository.AssemblyPlan, *repository.AssemblyPlanRevision, *repository.RenderReviewSnapshot, error) {
	plan, err := s.repo.GetOwnedPlan(ctx, artifact.PlanID, userID)
	if err != nil {
		return nil, nil, nil, err
	}
	if plan == nil {
		return nil, nil, nil, &PublishJobNotFoundError{Ref: artifact.PlanID}
	}
	if artifact.Kind != "final" {
		return nil, nil, nil, &PublishConflictError{Code: "final_artifact_required", Status: plan.Status}
	}
	revision, err := s.plans.GetCurrentRevision(ctx, plan)
	if err != nil {
		return nil, nil, nil, err
	}
	if revision == nil {
		return nil, nil, nil, &PublishConflictError{Code: "current_revision_missing", Status: plan.Status}
	}
	if artifact.RevisionNumber != revision.VersionNumber {
		return nil, nil, nil, &PublishConflictError{Code: "revision_conflict", Status: plan.Status}
	}
	snapshot, err := s.repo.GetOwnedSnapshot(ctx, reviewSnapshotID, userID)
	if err != nil {
		return nil, nil, nil, err
	}
	if snapshot == nil {
		return nil, nil, nil, &PublishJobNotFoundError{Ref: reviewSnapshotID}
	}
	if snapshot.ArtifactID != artifact.ID || snapshot.PlanID != plan.ID ||
		snapshot.RevisionNumber != revision.VersionNumber {
		return nil, nil, nil, &PublishConflictError{Code: "review_binding_mismatch", Status: plan.Status}
	}
	if snapshot.Status != "ready" {
		return nil, nil, nil, &PublishConflictError{Code: "review_blocked", Status: snapshot.Status}
	}
	if snapshot.ConfirmedBy == nil || *snapshot.ConfirmedBy != userID || !snapshot.ConfirmedAt.Valid {
		return nil, nil, nil, &PublishConflictError{Code: "review_confirmation_required", Status: snapshot.Status}
	}
	if snapshot.SourceFingerprint != revision.SourceFingerprint {
		return nil, nil, nil, &PublishConflictError{Code: "source_fingerprint_mismatch", Status: plan.Status}
	}
	if snapshot.ArtifactFingerprint != artifact.Fingerprint {
		return nil, nil, nil, &PublishConflictError{Code: "artifact_fingerprint_mismatch", Status: plan.Status}
	}
	return plan, revision, snapshot, nil
}

func normalizePublishPlatform(platform string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(platform))
	if normalized == "" || len(normalized) > 64 {
		return "", &PublishValidationError{Code: "platform_invalid"}
	}
	return normalized, nil
}

func publishRequestFingerprint(
	artifactID, reviewSnapshotID, platform, destinationJSON string, maxAttempts int,
) string {
	payload := artifactID + "\n" + reviewSnapshotID + "\n" + platform + "\n" + destinationJSON + "\n" +
		strconv.Itoa(maxAttempts)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func dedupSortedStrings(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		set[value] = true
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
