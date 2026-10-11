package repository

// 本文件的结构体由既有库的实测 DDL 生成，列名与可空性与 migrations/0001_init.sql 一致。
// schema 的真相源是那份 SQL，这里不打索引/约束 tag，也不参与 AutoMigrate。
// 时间列一律用 db.FlexibleTime：既有库里同一语义的列混用 TEXT / DATETIME，
// 指针式可空由 FlexibleTime.Valid 表达，不用 *FlexibleTime。

import (
	"github.com/shotwise/shotwise/internal/infra/db"
)

// CreativeBoard 对应 creative_boards 表。
type CreativeBoard struct {
	ID                  string          `gorm:"column:id;primaryKey"`
	UserID              string          `gorm:"column:user_id"`
	ProjectID           string          `gorm:"column:project_id"`
	Name                string          `gorm:"column:name"`
	ViewportJSON        string          `gorm:"column:viewport_json"`
	DisplaySettingsJSON string          `gorm:"column:display_settings_json"`
	CreatedAt           db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt           db.FlexibleTime `gorm:"column:updated_at"`
	Revision            int64           `gorm:"column:revision"`
}

func (CreativeBoard) TableName() string { return "creative_boards" }

// CreativeBoardItem 对应 creative_board_items 表。
type CreativeBoardItem struct {
	ID                  string          `gorm:"column:id;primaryKey"`
	BoardID             string          `gorm:"column:board_id"`
	ItemType            string          `gorm:"column:item_type"`
	ResourceType        string          `gorm:"column:resource_type"`
	ResourceID          string          `gorm:"column:resource_id"`
	PositionX           int64           `gorm:"column:position_x"`
	PositionY           int64           `gorm:"column:position_y"`
	Width               int64           `gorm:"column:width"`
	Height              int64           `gorm:"column:height"`
	GroupID             *string         `gorm:"column:group_id"`
	DisplaySettingsJSON string          `gorm:"column:display_settings_json"`
	CreatedAt           db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt           db.FlexibleTime `gorm:"column:updated_at"`
}

func (CreativeBoardItem) TableName() string { return "creative_board_items" }

// CreativeBoardEdge 对应 creative_board_edges 表。
type CreativeBoardEdge struct {
	ID           string          `gorm:"column:id;primaryKey"`
	BoardID      string          `gorm:"column:board_id"`
	SourceItemID string          `gorm:"column:source_item_id"`
	TargetItemID string          `gorm:"column:target_item_id"`
	Relation     string          `gorm:"column:relation"`
	Ordinal      *int64          `gorm:"column:ordinal"`
	MetadataJSON string          `gorm:"column:metadata_json"`
	CreatedAt    db.FlexibleTime `gorm:"column:created_at"`
}

func (CreativeBoardEdge) TableName() string { return "creative_board_edges" }

// CreativeBoardVersion 对应 creative_board_versions 表。
type CreativeBoardVersion struct {
	ID            string          `gorm:"column:id;primaryKey"`
	BoardID       string          `gorm:"column:board_id"`
	VersionNumber int64           `gorm:"column:version_number"`
	VersionName   string          `gorm:"column:version_name"`
	SnapshotJSON  string          `gorm:"column:snapshot_json"`
	CreatedAt     db.FlexibleTime `gorm:"column:created_at"`
}

func (CreativeBoardVersion) TableName() string { return "creative_board_versions" }

// CreationPlan 对应 creation_plans 表。
type CreationPlan struct {
	ID                     string          `gorm:"column:id;primaryKey"`
	UserID                 string          `gorm:"column:user_id"`
	WorkspaceID            string          `gorm:"column:workspace_id"`
	ProjectID              string          `gorm:"column:project_id"`
	CreationSkillVersionID string          `gorm:"column:creation_skill_version_id"`
	SkillID                string          `gorm:"column:skill_id"`
	WorkflowRevision       string          `gorm:"column:workflow_revision"`
	Fingerprint            string          `gorm:"column:fingerprint"`
	IdempotencyKey         string          `gorm:"column:idempotency_key"`
	ProjectSnapshotJSON    string          `gorm:"column:project_snapshot_json"`
	ResourceIdsJSON        string          `gorm:"column:resource_ids_json"`
	ParametersJSON         string          `gorm:"column:parameters_json"`
	PreviewJSON            string          `gorm:"column:preview_json"`
	EstimatedCost          float64         `gorm:"column:estimated_cost"`
	Status                 string          `gorm:"column:status"`
	WorkflowRunID          *string         `gorm:"column:workflow_run_id"`
	CreatedBy              string          `gorm:"column:created_by"`
	CreatedAt              db.FlexibleTime `gorm:"column:created_at"`
	StartedAt              db.FlexibleTime `gorm:"column:started_at"`
	CancelledAt            db.FlexibleTime `gorm:"column:cancelled_at"`
}

func (CreationPlan) TableName() string { return "creation_plans" }

// CreationCompatibilityEvent 对应 creation_compatibility_events 表。
type CreationCompatibilityEvent struct {
	ID                     string          `gorm:"column:id;primaryKey"`
	CreationSkillVersionID string          `gorm:"column:creation_skill_version_id"`
	ProjectContentMode     string          `gorm:"column:project_content_mode"`
	ProjectGenerationMode  string          `gorm:"column:project_generation_mode"`
	ResourceTypesJSON      string          `gorm:"column:resource_types_json"`
	Reason                 string          `gorm:"column:reason"`
	Outcome                string          `gorm:"column:outcome"`
	CreatedAt              db.FlexibleTime `gorm:"column:created_at"`
}

func (CreationCompatibilityEvent) TableName() string { return "creation_compatibility_events" }

// CreationSkillDefinition 对应 creation_skill_definitions 表。
type CreationSkillDefinition struct {
	ID        string          `gorm:"column:id;primaryKey"`
	Slug      string          `gorm:"column:slug"`
	Official  bool            `gorm:"column:official"`
	Active    bool            `gorm:"column:active"`
	CreatedAt db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt db.FlexibleTime `gorm:"column:updated_at"`
}

func (CreationSkillDefinition) TableName() string { return "creation_skill_definitions" }

// CreationSkillVersion 对应 creation_skill_versions 表。
type CreationSkillVersion struct {
	ID                            string          `gorm:"column:id;primaryKey"`
	SkillID                       string          `gorm:"column:skill_id"`
	Version                       int64           `gorm:"column:version"`
	Title                         string          `gorm:"column:title"`
	Summary                       string          `gorm:"column:summary"`
	Category                      string          `gorm:"column:category"`
	WorkflowTemplateRevisionAlias string          `gorm:"column:workflow_template_revision_alias"`
	ExpectedOutputsJSON           string          `gorm:"column:expected_outputs_json"`
	ReviewRequired                bool            `gorm:"column:review_required"`
	EstimatedCostHint             *string         `gorm:"column:estimated_cost_hint"`
	Status                        string          `gorm:"column:status"`
	FrozenAt                      db.FlexibleTime `gorm:"column:frozen_at"`
	WorkflowRevisionID            *string         `gorm:"column:workflow_revision_id"`
}

func (CreationSkillVersion) TableName() string { return "creation_skill_versions" }

// CreationSkillCompatibility 对应 creation_skill_compatibilities 表。
type CreationSkillCompatibility struct {
	SkillVersionID      string  `gorm:"column:skill_version_id;primaryKey"`
	ContentModesJSON    string  `gorm:"column:content_modes_json"`
	GenerationModesJSON string  `gorm:"column:generation_modes_json"`
	RequiredInputsJSON  string  `gorm:"column:required_inputs_json"`
	ScopesJSON          string  `gorm:"column:scopes_json"`
	GridStoryboardsJSON *string `gorm:"column:grid_storyboards_json"`
}

func (CreationSkillCompatibility) TableName() string { return "creation_skill_compatibilities" }
