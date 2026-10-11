package repository

// 本文件的结构体由既有库的实测 DDL 生成，列名与可空性与 migrations/0001_init.sql 一致。
// schema 的真相源是那份 SQL，这里不打索引/约束 tag，也不参与 AutoMigrate。
// 时间列一律用 db.FlexibleTime：既有库里同一语义的列混用 TEXT / DATETIME，
// 指针式可空由 FlexibleTime.Valid 表达，不用 *FlexibleTime。

import (
	"github.com/shotwise/shotwise/internal/infra/db"
)

// AssemblyPlan 对应 assembly_plans 表。
type AssemblyPlan struct {
	ID                       string          `gorm:"column:id;primaryKey"`
	UserID                   string          `gorm:"column:user_id"`
	ProjectName              string          `gorm:"column:project_name"`
	Scope                    string          `gorm:"column:scope"`
	EpisodeNumber            *int64          `gorm:"column:episode_number"`
	Name                     string          `gorm:"column:name"`
	Status                   string          `gorm:"column:status"`
	CurrentRevisionNumber    int64           `gorm:"column:current_revision_number"`
	CurrentSourceFingerprint string          `gorm:"column:current_source_fingerprint"`
	CreatedAt                db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt                db.FlexibleTime `gorm:"column:updated_at"`
	PreviewRevisionNumber    *int64          `gorm:"column:preview_revision_number"`
	PreviewReadyAt           db.FlexibleTime `gorm:"column:preview_ready_at"`
	RenderConfirmedBy        *string         `gorm:"column:render_confirmed_by"`
	RenderConfirmedAt        db.FlexibleTime `gorm:"column:render_confirmed_at"`
	PreviewConfirmedBy       *string         `gorm:"column:preview_confirmed_by"`
	PreviewConfirmedAt       db.FlexibleTime `gorm:"column:preview_confirmed_at"`
}

func (AssemblyPlan) TableName() string { return "assembly_plans" }

// AssemblyPlanRevision 对应 assembly_plan_revisions 表。
type AssemblyPlanRevision struct {
	ID                 string          `gorm:"column:id;primaryKey"`
	PlanID             string          `gorm:"column:plan_id"`
	VersionNumber      int64           `gorm:"column:version_number"`
	SourceFingerprint  string          `gorm:"column:source_fingerprint"`
	SourceSnapshotJSON string          `gorm:"column:source_snapshot_json"`
	TimelineJSON       string          `gorm:"column:timeline_json"`
	AudioJSON          string          `gorm:"column:audio_json"`
	SubtitleJSON       string          `gorm:"column:subtitle_json"`
	PackagingJSON      string          `gorm:"column:packaging_json"`
	OutputProfileJSON  string          `gorm:"column:output_profile_json"`
	ValidationJSON     string          `gorm:"column:validation_json"`
	CreatedBy          string          `gorm:"column:created_by"`
	CreatedAt          db.FlexibleTime `gorm:"column:created_at"`
}

func (AssemblyPlanRevision) TableName() string { return "assembly_plan_revisions" }

// RenderJob 对应 render_jobs 表。
type RenderJob struct {
	ID                string          `gorm:"column:id;primaryKey"`
	PlanID            string          `gorm:"column:plan_id"`
	ProjectName       string          `gorm:"column:project_name"`
	UserID            string          `gorm:"column:user_id"`
	RevisionNumber    int64           `gorm:"column:revision_number"`
	Kind              string          `gorm:"column:kind"`
	Status            string          `gorm:"column:status"`
	Attempt           int64           `gorm:"column:attempt"`
	MaxAttempts       int64           `gorm:"column:max_attempts"`
	InputFingerprint  string          `gorm:"column:input_fingerprint"`
	ErrorCode         *string         `gorm:"column:error_code"`
	ErrorMessage      *string         `gorm:"column:error_message"`
	CreatedAt         db.FlexibleTime `gorm:"column:created_at"`
	StartedAt         db.FlexibleTime `gorm:"column:started_at"`
	CompletedAt       db.FlexibleTime `gorm:"column:completed_at"`
	UpdatedAt         db.FlexibleTime `gorm:"column:updated_at"`
	Progress          float64         `gorm:"column:progress"`
	ProgressStage     string          `gorm:"column:progress_stage"`
	WorkerID          *string         `gorm:"column:worker_id"`
	HeartbeatAt       db.FlexibleTime `gorm:"column:heartbeat_at"`
	CancelRequestedAt db.FlexibleTime `gorm:"column:cancel_requested_at"`
}

func (RenderJob) TableName() string { return "render_jobs" }

// RenderArtifact 对应 render_artifacts 表。
type RenderArtifact struct {
	ID              string          `gorm:"column:id;primaryKey"`
	RenderJobID     string          `gorm:"column:render_job_id"`
	PlanID          string          `gorm:"column:plan_id"`
	ProjectName     string          `gorm:"column:project_name"`
	UserID          string          `gorm:"column:user_id"`
	RevisionNumber  int64           `gorm:"column:revision_number"`
	Kind            string          `gorm:"column:kind"`
	RelativePath    string          `gorm:"column:relative_path"`
	MimeType        string          `gorm:"column:mime_type"`
	SizeBytes       int64           `gorm:"column:size_bytes"`
	DurationSeconds float64         `gorm:"column:duration_seconds"`
	Width           *int64          `gorm:"column:width"`
	Height          *int64          `gorm:"column:height"`
	Fingerprint     string          `gorm:"column:fingerprint"`
	CreatedAt       db.FlexibleTime `gorm:"column:created_at"`
}

func (RenderArtifact) TableName() string { return "render_artifacts" }

// RenderReviewSnapshot 对应 render_review_snapshots 表。
type RenderReviewSnapshot struct {
	ID                  string          `gorm:"column:id;primaryKey"`
	ArtifactID          string          `gorm:"column:artifact_id"`
	PlanID              string          `gorm:"column:plan_id"`
	ProjectName         string          `gorm:"column:project_name"`
	UserID              string          `gorm:"column:user_id"`
	RevisionNumber      int64           `gorm:"column:revision_number"`
	SourceFingerprint   string          `gorm:"column:source_fingerprint"`
	ArtifactFingerprint string          `gorm:"column:artifact_fingerprint"`
	Status              string          `gorm:"column:status"`
	ChecksJSON          string          `gorm:"column:checks_json"`
	CreatedAt           db.FlexibleTime `gorm:"column:created_at"`
	ConfirmedBy         *string         `gorm:"column:confirmed_by"`
	ConfirmedAt         db.FlexibleTime `gorm:"column:confirmed_at"`
}

func (RenderReviewSnapshot) TableName() string { return "render_review_snapshots" }

// PublishJob 对应 publish_jobs 表。
type PublishJob struct {
	ID                  string          `gorm:"column:id;primaryKey"`
	UserID              string          `gorm:"column:user_id"`
	PlanID              string          `gorm:"column:plan_id"`
	ProjectName         string          `gorm:"column:project_name"`
	ArtifactID          string          `gorm:"column:artifact_id"`
	ReviewSnapshotID    string          `gorm:"column:review_snapshot_id"`
	RevisionNumber      int64           `gorm:"column:revision_number"`
	SourceFingerprint   string          `gorm:"column:source_fingerprint"`
	ArtifactFingerprint string          `gorm:"column:artifact_fingerprint"`
	Platform            string          `gorm:"column:platform"`
	DestinationJSON     string          `gorm:"column:destination_json"`
	IdempotencyKey      string          `gorm:"column:idempotency_key"`
	RequestFingerprint  string          `gorm:"column:request_fingerprint"`
	Status              string          `gorm:"column:status"`
	Attempt             int64           `gorm:"column:attempt"`
	MaxAttempts         int64           `gorm:"column:max_attempts"`
	ErrorCode           *string         `gorm:"column:error_code"`
	ErrorMessage        *string         `gorm:"column:error_message"`
	CreatedAt           db.FlexibleTime `gorm:"column:created_at"`
	StartedAt           db.FlexibleTime `gorm:"column:started_at"`
	CompletedAt         db.FlexibleTime `gorm:"column:completed_at"`
	UpdatedAt           db.FlexibleTime `gorm:"column:updated_at"`
	AccountID           *string         `gorm:"column:account_id"`
	ExternalContentID   *string         `gorm:"column:external_content_id"`
	ExternalStatus      *string         `gorm:"column:external_status"`
	NextPollAt          db.FlexibleTime `gorm:"column:next_poll_at"`
	LastPolledAt        db.FlexibleTime `gorm:"column:last_polled_at"`
	WorkerID            *string         `gorm:"column:worker_id"`
	LeaseUntil          db.FlexibleTime `gorm:"column:lease_until"`
}

func (PublishJob) TableName() string { return "publish_jobs" }

// PublishingAccount 对应 publishing_accounts 表。
type PublishingAccount struct {
	ID                    string          `gorm:"column:id;primaryKey"`
	UserID                string          `gorm:"column:user_id"`
	Platform              string          `gorm:"column:platform"`
	PlatformAccountID     string          `gorm:"column:platform_account_id"`
	AccountName           string          `gorm:"column:account_name"`
	AvatarURL             *string         `gorm:"column:avatar_url"`
	EncryptedAccessToken  *string         `gorm:"column:encrypted_access_token"`
	EncryptedRefreshToken *string         `gorm:"column:encrypted_refresh_token"`
	TokenExpiresAt        db.FlexibleTime `gorm:"column:token_expires_at"`
	ScopesJSON            string          `gorm:"column:scopes_json"`
	EncryptionKeyVersion  string          `gorm:"column:encryption_key_version"`
	Status                string          `gorm:"column:status"`
	LastRefreshAt         db.FlexibleTime `gorm:"column:last_refresh_at"`
	LastErrorCode         *string         `gorm:"column:last_error_code"`
	CreatedAt             db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt             db.FlexibleTime `gorm:"column:updated_at"`
}

func (PublishingAccount) TableName() string { return "publishing_accounts" }
