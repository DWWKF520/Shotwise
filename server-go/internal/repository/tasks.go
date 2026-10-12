package repository

// 本文件的结构体由既有库的实测 DDL 生成，列名与可空性与 migrations/0001_init.sql 一致。
// schema 的真相源是那份 SQL，这里不打索引/约束 tag，也不参与 AutoMigrate。
// 时间列一律用 db.FlexibleTime：既有库里同一语义的列混用 TEXT / DATETIME，
// 指针式可空由 FlexibleTime.Valid 表达，不用 *FlexibleTime。

import (
	"github.com/shotwise/shotwise/internal/infra/db"
)

// Task 对应 tasks 表。
type Task struct {
	TaskID                  string          `gorm:"column:task_id;primaryKey"`
	ProjectName             string          `gorm:"column:project_name"`
	TaskType                string          `gorm:"column:task_type"`
	MediaType               string          `gorm:"column:media_type"`
	ResourceID              string          `gorm:"column:resource_id"`
	ScriptFile              *string         `gorm:"column:script_file"`
	PayloadJSON             *string         `gorm:"column:payload_json"`
	Status                  string          `gorm:"column:status"`
	ResultJSON              *string         `gorm:"column:result_json"`
	ErrorMessage            *string         `gorm:"column:error_message"`
	Source                  string          `gorm:"column:source"`
	DependencyTaskID        *string         `gorm:"column:dependency_task_id"`
	DependencyGroup         *string         `gorm:"column:dependency_group"`
	DependencyIndex         *int64          `gorm:"column:dependency_index"`
	QueuedAt                db.FlexibleTime `gorm:"column:queued_at"`
	StartedAt               db.FlexibleTime `gorm:"column:started_at"`
	FinishedAt              db.FlexibleTime `gorm:"column:finished_at"`
	UpdatedAt               db.FlexibleTime `gorm:"column:updated_at"`
	UserID                  string          `gorm:"column:user_id"`
	CancelledBy             *string         `gorm:"column:cancelled_by"`
	ProviderID              *string         `gorm:"column:provider_id"`
	ProviderJobID           *string         `gorm:"column:provider_job_id"`
	ResourceType            *string         `gorm:"column:resource_type"`
	ProviderEndpoint        *string         `gorm:"column:provider_endpoint"`
	WorkflowRunID           *string         `gorm:"column:workflow_run_id"`
	WorkflowNodeRunID       *string         `gorm:"column:workflow_node_run_id"`
	WorkflowNodeRunItemID   *string         `gorm:"column:workflow_node_run_item_id"`
	InputFingerprint        *string         `gorm:"column:input_fingerprint"`
	RequestIdempotencyKey   *string         `gorm:"column:request_idempotency_key"`
	Progress                *float64        `gorm:"column:progress"`
	ProgressSource          *string         `gorm:"column:progress_source"`
	PhaseCode               *string         `gorm:"column:phase_code"`
	LeaseOwner              *string         `gorm:"column:lease_owner"`
	LeaseUntil              db.FlexibleTime `gorm:"column:lease_until"`
	FencingToken            int64           `gorm:"column:fencing_token"`
	SubmittedBaseURL        *string         `gorm:"column:submitted_base_url"`
	ExecutionCheckpointJSON *string         `gorm:"column:execution_checkpoint_json"`
}

func (Task) TableName() string { return "tasks" }

// WorkerLease 对应 worker_lease 表。
// lease_until 是 epoch 秒（FLOAT 列），不是时间戳文本：租约比较在 DB 内以数值进行。
type WorkerLease struct {
	Name       string          `gorm:"column:name;primaryKey"`
	OwnerID    string          `gorm:"column:owner_id"`
	LeaseUntil float64         `gorm:"column:lease_until"`
	UpdatedAt  db.FlexibleTime `gorm:"column:updated_at"`
}

func (WorkerLease) TableName() string { return "worker_lease" }

// GenerationBatch 对应 generation_batches 表。
type GenerationBatch struct {
	BatchID         string          `gorm:"column:batch_id;primaryKey"`
	ProjectName     string          `gorm:"column:project_name"`
	AdmissionState  string          `gorm:"column:admission_state"`
	ItemsJSON       string          `gorm:"column:items_json"`
	TaskIdsJSON     string          `gorm:"column:task_ids_json"`
	CancelRequested bool            `gorm:"column:cancel_requested"`
	ErrorMessage    *string         `gorm:"column:error_message"`
	CreatedAt       db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt       db.FlexibleTime `gorm:"column:updated_at"`
	UserID          string          `gorm:"column:user_id"`
}

func (GenerationBatch) TableName() string { return "generation_batches" }

// ExternalExecution 对应 external_executions 表。
type ExternalExecution struct {
	ID                     string          `gorm:"column:id;primaryKey"`
	TaskID                 string          `gorm:"column:task_id"`
	ProviderID             string          `gorm:"column:provider_id"`
	ProviderAccountID      *string         `gorm:"column:provider_account_id"`
	EndpointSnapshot       *string         `gorm:"column:endpoint_snapshot"`
	ModelSnapshot          *string         `gorm:"column:model_snapshot"`
	ProviderJobID          *string         `gorm:"column:provider_job_id"`
	ProviderIdempotencyKey *string         `gorm:"column:provider_idempotency_key"`
	SubmitState            string          `gorm:"column:submit_state"`
	RemoteState            *string         `gorm:"column:remote_state"`
	CancelState            *string         `gorm:"column:cancel_state"`
	RequestHash            *string         `gorm:"column:request_hash"`
	ResponseDigest         *string         `gorm:"column:response_digest"`
	FencingToken           int64           `gorm:"column:fencing_token"`
	SubmittedAt            db.FlexibleTime `gorm:"column:submitted_at"`
	LastPolledAt           db.FlexibleTime `gorm:"column:last_polled_at"`
	FinishedAt             db.FlexibleTime `gorm:"column:finished_at"`
}

func (ExternalExecution) TableName() string { return "external_executions" }

// APICall 对应 api_calls 表。
type APICall struct {
	ID                int64           `gorm:"column:id;primaryKey"`
	ProjectName       string          `gorm:"column:project_name"`
	CallType          string          `gorm:"column:call_type"`
	Model             string          `gorm:"column:model"`
	Prompt            *string         `gorm:"column:prompt"`
	Resolution        *string         `gorm:"column:resolution"`
	DurationSeconds   *int64          `gorm:"column:duration_seconds"`
	AspectRatio       *string         `gorm:"column:aspect_ratio"`
	GenerateAudio     *bool           `gorm:"column:generate_audio"`
	Status            string          `gorm:"column:status"`
	ErrorMessage      *string         `gorm:"column:error_message"`
	OutputPath        *string         `gorm:"column:output_path"`
	StartedAt         db.FlexibleTime `gorm:"column:started_at"`
	FinishedAt        db.FlexibleTime `gorm:"column:finished_at"`
	DurationMs        *int64          `gorm:"column:duration_ms"`
	RetryCount        int64           `gorm:"column:retry_count"`
	CostAmount        float64         `gorm:"column:cost_amount"`
	CreatedAt         db.FlexibleTime `gorm:"column:created_at"`
	Currency          string          `gorm:"column:currency"`
	Provider          string          `gorm:"column:provider"`
	UsageTokens       *int64          `gorm:"column:usage_tokens"`
	UpdatedAt         db.FlexibleTime `gorm:"column:updated_at"`
	UserID            string          `gorm:"column:user_id"`
	InputTokens       *int64          `gorm:"column:input_tokens"`
	OutputTokens      *int64          `gorm:"column:output_tokens"`
	SegmentID         *string         `gorm:"column:segment_id"`
	ImageInputTokens  *int64          `gorm:"column:image_input_tokens"`
	ImageOutputTokens *int64          `gorm:"column:image_output_tokens"`
	TextInputTokens   *int64          `gorm:"column:text_input_tokens"`
	TextOutputTokens  *int64          `gorm:"column:text_output_tokens"`
	TaskID            *string         `gorm:"column:task_id"`
	Purpose           *string         `gorm:"column:purpose"`
	SessionID         *string         `gorm:"column:session_id"`
	Inputs            *string         `gorm:"column:inputs"`
	ErrorCode         *string         `gorm:"column:error_code"`
	ErrorParams       *string         `gorm:"column:error_params"`
}

func (APICall) TableName() string { return "api_calls" }
