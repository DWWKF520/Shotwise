package repository

// 本文件的结构体由既有库的实测 DDL 生成，列名与可空性与 migrations/0001_init.sql 一致。
// schema 的真相源是那份 SQL，这里不打索引/约束 tag，也不参与 AutoMigrate。
// 时间列一律用 db.FlexibleTime：既有库里同一语义的列混用 TEXT / DATETIME，
// 指针式可空由 FlexibleTime.Valid 表达，不用 *FlexibleTime。

import (
	"github.com/shotwise/shotwise/internal/infra/db"
)

// Asset 对应 assets 表。
type Asset struct {
	ID            string          `gorm:"column:id;primaryKey"`
	Type          string          `gorm:"column:type"`
	Name          string          `gorm:"column:name"`
	Description   string          `gorm:"column:description"`
	VoiceStyle    string          `gorm:"column:voice_style"`
	ImagePath     *string         `gorm:"column:image_path"`
	SourceProject *string         `gorm:"column:source_project"`
	CreatedAt     db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt     db.FlexibleTime `gorm:"column:updated_at"`
	AudioPath     *string         `gorm:"column:audio_path"`
}

func (Asset) TableName() string { return "assets" }

// MediaAsset 对应 media_assets 表。
type MediaAsset struct {
	ID              string          `gorm:"column:id;primaryKey"`
	ProjectID       string          `gorm:"column:project_id"`
	Kind            string          `gorm:"column:kind"`
	OriginalName    string          `gorm:"column:original_name"`
	MimeType        *string         `gorm:"column:mime_type"`
	Extension       string          `gorm:"column:extension"`
	PhysicalPath    string          `gorm:"column:physical_path"`
	SizeBytes       int64           `gorm:"column:size_bytes"`
	Fingerprint     string          `gorm:"column:fingerprint"`
	Origin          string          `gorm:"column:origin"`
	CreatedAt       db.FlexibleTime `gorm:"column:created_at"`
	Width           *int64          `gorm:"column:width"`
	Height          *int64          `gorm:"column:height"`
	DurationSeconds *float64        `gorm:"column:duration_seconds"`
	WorkflowRunID   *string         `gorm:"column:workflow_run_id"`
	WorkflowNodeKey *string         `gorm:"column:workflow_node_key"`
	ProviderID      *string         `gorm:"column:provider_id"`
	ModelID         *string         `gorm:"column:model_id"`
	PromptSnapshot  *string         `gorm:"column:prompt_snapshot"`
	Archived        bool            `gorm:"column:archived"`
}

func (MediaAsset) TableName() string { return "media_assets" }

// MediaBinding 对应 media_bindings 表。
type MediaBinding struct {
	ID           string          `gorm:"column:id;primaryKey"`
	MediaAssetID string          `gorm:"column:media_asset_id"`
	ProjectID    string          `gorm:"column:project_id"`
	BindingKind  string          `gorm:"column:binding_kind"`
	TargetID     *string         `gorm:"column:target_id"`
	Purpose      string          `gorm:"column:purpose"`
	CreatedAt    db.FlexibleTime `gorm:"column:created_at"`
}

func (MediaBinding) TableName() string { return "media_bindings" }

// MediaDerivation 对应 media_derivations 表。
type MediaDerivation struct {
	ID                 string          `gorm:"column:id;primaryKey"`
	ParentMediaAssetID string          `gorm:"column:parent_media_asset_id"`
	ChildMediaAssetID  string          `gorm:"column:child_media_asset_id"`
	Operation          string          `gorm:"column:operation"`
	CreatedAt          db.FlexibleTime `gorm:"column:created_at"`
}

func (MediaDerivation) TableName() string { return "media_derivations" }
