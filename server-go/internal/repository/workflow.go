package repository

// 本文件的结构体由既有库的实测 DDL 生成，列名与可空性与 migrations/0001_init.sql 一致。
// schema 的真相源是那份 SQL，这里不打索引/约束 tag，也不参与 AutoMigrate。
// 时间列一律用 db.FlexibleTime：既有库里同一语义的列混用 TEXT / DATETIME，
// 指针式可空由 FlexibleTime.Valid 表达，不用 *FlexibleTime。

import (
	"github.com/shotwise/shotwise/internal/infra/db"
)

// WorkflowDefinition 对应 workflow_definitions 表。
type WorkflowDefinition struct {
	ID               string          `gorm:"column:id;primaryKey"`
	UserID           string          `gorm:"column:user_id"`
	WorkspaceID      string          `gorm:"column:workspace_id"`
	ProjectID        string          `gorm:"column:project_id"`
	Name             string          `gorm:"column:name"`
	Scope            string          `gorm:"column:scope"`
	ActiveRevisionID *string         `gorm:"column:active_revision_id"`
	CreatedAt        db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt        db.FlexibleTime `gorm:"column:updated_at"`
}

func (WorkflowDefinition) TableName() string { return "workflow_definitions" }

// WorkflowRevision 对应 workflow_revisions 表。
type WorkflowRevision struct {
	ID               string          `gorm:"column:id;primaryKey"`
	DefinitionID     string          `gorm:"column:definition_id"`
	RevisionNo       int64           `gorm:"column:revision_no"`
	Status           string          `gorm:"column:status"`
	GraphHash        string          `gorm:"column:graph_hash"`
	ExecutionHash    string          `gorm:"column:execution_hash"`
	TemplateLockJSON *string         `gorm:"column:template_lock_json"`
	CreatedBy        string          `gorm:"column:created_by"`
	CreatedAt        db.FlexibleTime `gorm:"column:created_at"`
	ContentMode      string          `gorm:"column:content_mode"`
	GenerationMode   string          `gorm:"column:generation_mode"`
	InputSchemaJSON  string          `gorm:"column:input_schema_json"`
}

func (WorkflowRevision) TableName() string { return "workflow_revisions" }

// WorkflowNode 对应 workflow_nodes 表。
type WorkflowNode struct {
	ID                       string  `gorm:"column:id;primaryKey"`
	RevisionID               string  `gorm:"column:revision_id"`
	NodeKey                  string  `gorm:"column:node_key"`
	NodeType                 string  `gorm:"column:node_type"`
	NodeTypeVersion          string  `gorm:"column:node_type_version"`
	ConfigSchemaVersion      string  `gorm:"column:config_schema_version"`
	ConfigJSON               string  `gorm:"column:config_json"`
	UiPositionJSON           *string `gorm:"column:ui_position_json"`
	Weight                   float64 `gorm:"column:weight"`
	RetryPolicyJSON          *string `gorm:"column:retry_policy_json"`
	ApprovalPolicyJSON       *string `gorm:"column:approval_policy_json"`
	InputSchemaJSON          string  `gorm:"column:input_schema_json"`
	OutputSchemaJSON         string  `gorm:"column:output_schema_json"`
	ExecutorID               string  `gorm:"column:executor_id"`
	RequiredCapabilitiesJSON string  `gorm:"column:required_capabilities_json"`
	EstimatedCost            float64 `gorm:"column:estimated_cost"`
	CachePolicy              string  `gorm:"column:cache_policy"`
}

func (WorkflowNode) TableName() string { return "workflow_nodes" }

// WorkflowEdge 对应 workflow_edges 表。
type WorkflowEdge struct {
	ID            string  `gorm:"column:id;primaryKey"`
	RevisionID    string  `gorm:"column:revision_id"`
	EdgeKey       string  `gorm:"column:edge_key"`
	SourceNodeKey string  `gorm:"column:source_node_key"`
	TargetNodeKey string  `gorm:"column:target_node_key"`
	ConditionJSON *string `gorm:"column:condition_json"`
	OnFailure     string  `gorm:"column:on_failure"`
	Priority      int64   `gorm:"column:priority"`
}

func (WorkflowEdge) TableName() string { return "workflow_edges" }

// WorkflowRun 对应 workflow_runs 表。
type WorkflowRun struct {
	ID                 string          `gorm:"column:id;primaryKey"`
	UserID             string          `gorm:"column:user_id"`
	WorkflowRevisionID string          `gorm:"column:workflow_revision_id"`
	WorkspaceID        string          `gorm:"column:workspace_id"`
	ProjectID          string          `gorm:"column:project_id"`
	ScriptRevisionID   *string         `gorm:"column:script_revision_id"`
	Status             string          `gorm:"column:status"`
	Mode               string          `gorm:"column:mode"`
	ExecutionHash      string          `gorm:"column:execution_hash"`
	InputFingerprint   string          `gorm:"column:input_fingerprint"`
	GraphSnapshotRef   *string         `gorm:"column:graph_snapshot_ref"`
	InputSnapshotJSON  string          `gorm:"column:input_snapshot_json"`
	Progress           *float64        `gorm:"column:progress"`
	Version            int64           `gorm:"column:version"`
	ControlGeneration  int64           `gorm:"column:control_generation"`
	TraceID            string          `gorm:"column:trace_id"`
	CreatedBy          string          `gorm:"column:created_by"`
	CreatedAt          db.FlexibleTime `gorm:"column:created_at"`
	StartedAt          db.FlexibleTime `gorm:"column:started_at"`
	FinishedAt         db.FlexibleTime `gorm:"column:finished_at"`
	EpisodeID          *string         `gorm:"column:episode_id"`
	BudgetLimit        *float64        `gorm:"column:budget_limit"`
	SpentAmount        float64         `gorm:"column:spent_amount"`
	ReservedAmount     float64         `gorm:"column:reserved_amount"`
}

func (WorkflowRun) TableName() string { return "workflow_runs" }

// WorkflowNodeRun 对应 workflow_node_runs 表。
type WorkflowNodeRun struct {
	ID                string          `gorm:"column:id;primaryKey"`
	WorkflowRunID     string          `gorm:"column:workflow_run_id"`
	NodeKey           string          `gorm:"column:node_key"`
	AttemptNo         int64           `gorm:"column:attempt_no"`
	Status            string          `gorm:"column:status"`
	InputHash         *string         `gorm:"column:input_hash"`
	InputSnapshotJSON *string         `gorm:"column:input_snapshot_json"`
	Progress          *float64        `gorm:"column:progress"`
	ProgressSource    *string         `gorm:"column:progress_source"`
	PhaseCode         *string         `gorm:"column:phase_code"`
	PhaseParamsJSON   *string         `gorm:"column:phase_params_json"`
	ErrorCode         *string         `gorm:"column:error_code"`
	ErrorParamsJSON   *string         `gorm:"column:error_params_json"`
	OutputRefsJSON    *string         `gorm:"column:output_refs_json"`
	LeaseOwner        *string         `gorm:"column:lease_owner"`
	LeaseUntil        db.FlexibleTime `gorm:"column:lease_until"`
	FencingToken      int64           `gorm:"column:fencing_token"`
	CreatedAt         db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt         db.FlexibleTime `gorm:"column:updated_at"`
}

func (WorkflowNodeRun) TableName() string { return "workflow_node_runs" }

// WorkflowNodeRunItem 对应 workflow_node_run_items 表。
type WorkflowNodeRunItem struct {
	ID                string  `gorm:"column:id;primaryKey"`
	NodeRunID         string  `gorm:"column:node_run_id"`
	ItemKey           string  `gorm:"column:item_key"`
	Ordinal           int64   `gorm:"column:ordinal"`
	InputHash         *string `gorm:"column:input_hash"`
	InputSnapshotJSON *string `gorm:"column:input_snapshot_json"`
	Weight            float64 `gorm:"column:weight"`
	Status            string  `gorm:"column:status"`
	CurrentAttemptNo  int64   `gorm:"column:current_attempt_no"`
	LineageHash       *string `gorm:"column:lineage_hash"`
	OutputRefsJSON    *string `gorm:"column:output_refs_json"`
	ErrorCode         *string `gorm:"column:error_code"`
	ErrorParamsJSON   *string `gorm:"column:error_params_json"`
}

func (WorkflowNodeRunItem) TableName() string { return "workflow_node_run_items" }

// WorkflowTemplate 对应 workflow_templates 表。
type WorkflowTemplate struct {
	ID                  string          `gorm:"column:id;primaryKey"`
	UserID              string          `gorm:"column:user_id"`
	Name                string          `gorm:"column:name"`
	Description         string          `gorm:"column:description"`
	CoverRef            *string         `gorm:"column:cover_ref"`
	TemplateType        string          `gorm:"column:template_type"`
	Status              string          `gorm:"column:status"`
	DraftRevisionID     *string         `gorm:"column:draft_revision_id"`
	PublishedRevisionID *string         `gorm:"column:published_revision_id"`
	ContractJSON        string          `gorm:"column:contract_json"`
	SubmittedAt         db.FlexibleTime `gorm:"column:submitted_at"`
	PublishedAt         db.FlexibleTime `gorm:"column:published_at"`
	CreatedAt           db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt           db.FlexibleTime `gorm:"column:updated_at"`
}

func (WorkflowTemplate) TableName() string { return "workflow_templates" }

// WorkflowMarketplaceReview 对应 workflow_marketplace_reviews 表。
type WorkflowMarketplaceReview struct {
	ID         string          `gorm:"column:id;primaryKey"`
	TemplateID string          `gorm:"column:template_id"`
	RevisionID string          `gorm:"column:revision_id"`
	Status     string          `gorm:"column:status"`
	Decision   string          `gorm:"column:decision"`
	Comment    string          `gorm:"column:comment"`
	ReviewerID string          `gorm:"column:reviewer_id"`
	CreatedAt  db.FlexibleTime `gorm:"column:created_at"`
}

func (WorkflowMarketplaceReview) TableName() string { return "workflow_marketplace_reviews" }

// WorkflowUsageStat 对应 workflow_usage_stats 表。
type WorkflowUsageStat struct {
	TemplateID           string  `gorm:"column:template_id;primaryKey"`
	Views                int64   `gorm:"column:views"`
	Derivations          int64   `gorm:"column:derivations"`
	RunCount             int64   `gorm:"column:run_count"`
	SuccessfulRunCount   int64   `gorm:"column:successful_run_count"`
	TotalCost            float64 `gorm:"column:total_cost"`
	TotalDurationSeconds float64 `gorm:"column:total_duration_seconds"`
	RatingTotal          float64 `gorm:"column:rating_total"`
	RatingCount          int64   `gorm:"column:rating_count"`
}

func (WorkflowUsageStat) TableName() string { return "workflow_usage_stats" }

// WorkflowApproval 对应 workflow_approvals 表。
type WorkflowApproval struct {
	ID            string          `gorm:"column:id;primaryKey"`
	WorkflowRunID string          `gorm:"column:workflow_run_id"`
	NodeRunID     *string         `gorm:"column:node_run_id"`
	Action        string          `gorm:"column:action"`
	ExecutionHash string          `gorm:"column:execution_hash"`
	InputHash     string          `gorm:"column:input_hash"`
	BudgetLimit   *float64        `gorm:"column:budget_limit"`
	Status        string          `gorm:"column:status"`
	RequestedBy   string          `gorm:"column:requested_by"`
	DecidedBy     *string         `gorm:"column:decided_by"`
	ExpiresAt     db.FlexibleTime `gorm:"column:expires_at"`
	DecidedAt     db.FlexibleTime `gorm:"column:decided_at"`
}

func (WorkflowApproval) TableName() string { return "workflow_approvals" }

// BudgetReservation 对应 budget_reservations 表。
type BudgetReservation struct {
	ID                  string          `gorm:"column:id;primaryKey"`
	WorkspaceID         string          `gorm:"column:workspace_id"`
	WorkflowRunID       string          `gorm:"column:workflow_run_id"`
	NodeRunID           *string         `gorm:"column:node_run_id"`
	Currency            string          `gorm:"column:currency"`
	EstimatedAmount     float64         `gorm:"column:estimated_amount"`
	ReservedAmount      float64         `gorm:"column:reserved_amount"`
	SettledAmount       float64         `gorm:"column:settled_amount"`
	PriceCatalogVersion *string         `gorm:"column:price_catalog_version"`
	Status              string          `gorm:"column:status"`
	ExpiresAt           db.FlexibleTime `gorm:"column:expires_at"`
}

func (BudgetReservation) TableName() string { return "budget_reservations" }

// ProjectEventLog 对应 project_event_log 表。
type ProjectEventLog struct {
	Seq              int64           `gorm:"column:seq;primaryKey"`
	EventID          string          `gorm:"column:event_id"`
	WorkspaceID      string          `gorm:"column:workspace_id"`
	ProjectID        string          `gorm:"column:project_id"`
	AggregateType    string          `gorm:"column:aggregate_type"`
	AggregateID      string          `gorm:"column:aggregate_id"`
	AggregateVersion int64           `gorm:"column:aggregate_version"`
	EventType        string          `gorm:"column:event_type"`
	EventVersion     int64           `gorm:"column:event_version"`
	PayloadJSON      string          `gorm:"column:payload_json"`
	CausationID      *string         `gorm:"column:causation_id"`
	CorrelationID    *string         `gorm:"column:correlation_id"`
	ActorType        string          `gorm:"column:actor_type"`
	ActorID          string          `gorm:"column:actor_id"`
	TraceID          *string         `gorm:"column:trace_id"`
	CreatedAt        db.FlexibleTime `gorm:"column:created_at"`
}

func (ProjectEventLog) TableName() string { return "project_event_log" }
