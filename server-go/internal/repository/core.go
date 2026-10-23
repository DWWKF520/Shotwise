package repository

// 本文件的结构体由既有库的实测 DDL 生成，列名与可空性与 migrations/0001_init.sql 一致。
// schema 的真相源是那份 SQL，这里不打索引/约束 tag，也不参与 AutoMigrate。
// 时间列一律用 db.FlexibleTime：既有库里同一语义的列混用 TEXT / DATETIME，
// 指针式可空由 FlexibleTime.Valid 表达，不用 *FlexibleTime。

import (
	"github.com/shotwise/shotwise/internal/infra/db"
)

// User 对应 users 表。
type User struct {
	ID        string          `gorm:"column:id;primaryKey"`
	Username  string          `gorm:"column:username"`
	Role      string          `gorm:"column:role"`
	IsActive  bool            `gorm:"column:is_active"`
	CreatedAt db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt db.FlexibleTime `gorm:"column:updated_at"`
	// Python 侧 GitHub OAuth 的登录标识（users.github_id 列，共享 schema）；
	// Go 侧不实现 OAuth，字段仅维持 schema_test 的模型 ↔ DDL 双向对齐。
	GithubID *string `gorm:"column:github_id"`
}

func (User) TableName() string { return "users" }

// APIKey 对应 api_keys 表。
type APIKey struct {
	ID         int64           `gorm:"column:id;primaryKey"`
	Name       string          `gorm:"column:name"`
	KeyHash    string          `gorm:"column:key_hash"`
	KeyPrefix  string          `gorm:"column:key_prefix"`
	CreatedAt  db.FlexibleTime `gorm:"column:created_at"`
	ExpiresAt  db.FlexibleTime `gorm:"column:expires_at"`
	LastUsedAt db.FlexibleTime `gorm:"column:last_used_at"`
	UpdatedAt  db.FlexibleTime `gorm:"column:updated_at"`
	UserID     string          `gorm:"column:user_id"`
}

func (APIKey) TableName() string { return "api_keys" }

// SystemSetting 对应 system_setting 表。
type SystemSetting struct {
	ID        int64           `gorm:"column:id;primaryKey"`
	Key       string          `gorm:"column:key"`
	Value     string          `gorm:"column:value"`
	UpdatedAt db.FlexibleTime `gorm:"column:updated_at"`
}

func (SystemSetting) TableName() string { return "system_setting" }

// ProviderConfig 对应 provider_config 表。
type ProviderConfig struct {
	ID        int64           `gorm:"column:id;primaryKey"`
	Provider  string          `gorm:"column:provider"`
	Key       string          `gorm:"column:key"`
	Value     string          `gorm:"column:value"`
	IsSecret  bool            `gorm:"column:is_secret"`
	UpdatedAt db.FlexibleTime `gorm:"column:updated_at"`
}

func (ProviderConfig) TableName() string { return "provider_config" }

// ProviderCredential 对应 provider_credential 表。
type ProviderCredential struct {
	ID              int64           `gorm:"column:id;primaryKey"`
	Provider        string          `gorm:"column:provider"`
	Name            string          `gorm:"column:name"`
	APIKey          *string         `gorm:"column:api_key"`
	CredentialsPath *string         `gorm:"column:credentials_path"`
	BaseURL         *string         `gorm:"column:base_url"`
	IsActive        bool            `gorm:"column:is_active"`
	CreatedAt       db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt       db.FlexibleTime `gorm:"column:updated_at"`
	AccessKey       *string         `gorm:"column:access_key"`
	SecretKey       *string         `gorm:"column:secret_key"`
}

func (ProviderCredential) TableName() string { return "provider_credential" }

// AgentAnthropicCredential 对应 agent_anthropic_credentials 表。
type AgentAnthropicCredential struct {
	ID            int64           `gorm:"column:id;primaryKey"`
	UserID        string          `gorm:"column:user_id"`
	PresetID      string          `gorm:"column:preset_id"`
	DisplayName   string          `gorm:"column:display_name"`
	BaseURL       string          `gorm:"column:base_url"`
	APIKey        string          `gorm:"column:api_key"`
	Model         *string         `gorm:"column:model"`
	HaikuModel    *string         `gorm:"column:haiku_model"`
	SonnetModel   *string         `gorm:"column:sonnet_model"`
	OpusModel     *string         `gorm:"column:opus_model"`
	SubagentModel *string         `gorm:"column:subagent_model"`
	IsActive      bool            `gorm:"column:is_active"`
	CreatedAt     db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt     db.FlexibleTime `gorm:"column:updated_at"`
	SDKType       string          `gorm:"column:sdk_type"`
	ModelMap      *string         `gorm:"column:model_map"`
	Protocol      string          `gorm:"column:protocol"`
}

func (AgentAnthropicCredential) TableName() string { return "agent_anthropic_credentials" }

// CustomProvider 对应 custom_provider 表。
type CustomProvider struct {
	ID                         int64           `gorm:"column:id;primaryKey"`
	DisplayName                string          `gorm:"column:display_name"`
	BaseURL                    string          `gorm:"column:base_url"`
	APIKey                     string          `gorm:"column:api_key"`
	CreatedAt                  db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt                  db.FlexibleTime `gorm:"column:updated_at"`
	DiscoveryFormat            string          `gorm:"column:discovery_format"`
	ImageMaxWorkers            *int64          `gorm:"column:image_max_workers"`
	VideoMaxWorkers            *int64          `gorm:"column:video_max_workers"`
	AudioMaxWorkers            *int64          `gorm:"column:audio_max_workers"`
	IsEnabled                  bool            `gorm:"column:is_enabled"`
	ImageRequestTimeoutSeconds *float64        `gorm:"column:image_request_timeout_seconds"`
}

func (CustomProvider) TableName() string { return "custom_provider" }

// CustomProviderModel 对应 custom_provider_model 表。
type CustomProviderModel struct {
	ID                  int64           `gorm:"column:id;primaryKey"`
	ProviderID          int64           `gorm:"column:provider_id"`
	ModelID             string          `gorm:"column:model_id"`
	DisplayName         string          `gorm:"column:display_name"`
	IsDefault           bool            `gorm:"column:is_default"`
	IsEnabled           bool            `gorm:"column:is_enabled"`
	PriceUnit           *string         `gorm:"column:price_unit"`
	PriceInput          *float64        `gorm:"column:price_input"`
	PriceOutput         *float64        `gorm:"column:price_output"`
	Currency            *string         `gorm:"column:currency"`
	CreatedAt           db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt           db.FlexibleTime `gorm:"column:updated_at"`
	SupportedDurations  *string         `gorm:"column:supported_durations"`
	Resolution          *string         `gorm:"column:resolution"`
	Endpoint            string          `gorm:"column:endpoint"`
	CapabilityOverrides *string         `gorm:"column:capability_overrides"`
	EndpointDeclaration *string         `gorm:"column:endpoint_declaration"`
}

func (CustomProviderModel) TableName() string { return "custom_provider_model" }
