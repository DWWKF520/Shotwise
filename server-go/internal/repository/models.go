// Package repository 承载 GORM 模型与数据访问。
//
// 分层契约：domain ← repository ← service ← transport。repository 不得反向依赖上层。
package repository

import (
	"github.com/shotwise/shotwise/internal/infra/db"
)

// User 对应 users 表。
type User struct {
	ID        string `gorm:"primaryKey;size:64;default:default"`
	Username  string `gorm:"uniqueIndex;size:128;not null"`
	Role      string `gorm:"size:32;not null;default:user"`
	IsActive  bool   `gorm:"not null;default:true"`
	GithubID  string `gorm:"column:github_id;uniqueIndex;size:64"`
	CreatedAt db.FlexibleTime
	UpdatedAt db.FlexibleTime
}

func (User) TableName() string { return "users" }

// APIKey 对应 api_keys 表。明文 key 只在创建时返回一次，库里只存 SHA-256。
type APIKey struct {
	ID          uint             `gorm:"primaryKey;autoIncrement"`
	Name        string           `gorm:"uniqueIndex;size:128;not null"`
	KeyHash     string           `gorm:"column:key_hash;uniqueIndex;size:128;not null"`
	KeyPrefix   string           `gorm:"column:key_prefix;size:32"`
	ExpiresAt   *db.FlexibleTime `gorm:"column:expires_at"`
	LastUsedAt  *db.FlexibleTime `gorm:"column:last_used_at"`
	UserID      string           `gorm:"column:user_id;size:64"`
	CreatedAt   db.FlexibleTime
}

func (APIKey) TableName() string { return "api_keys" }

// SystemSetting 对应 system_setting 表：全局配置的唯一真相源。
type SystemSetting struct {
	Key       string `gorm:"primaryKey;size:128"`
	Value     string `gorm:"type:text"`
	UpdatedAt db.FlexibleTime
}

func (SystemSetting) TableName() string { return "system_setting" }

// ProviderConfig 对应 provider_config 表：预置供应商的 per-key 配置。
// is_secret 只是标记（读侧脱敏），值本身明文存储。
type ProviderConfig struct {
	ID        uint   `gorm:"primaryKey;autoIncrement"`
	Provider  string `gorm:"size:64;not null;uniqueIndex:idx_provider_config_pk"`
	Key       string `gorm:"size:128;not null;uniqueIndex:idx_provider_config_pk"`
	Value     string `gorm:"type:text"`
	IsSecret  bool   `gorm:"column:is_secret;not null;default:false"`
	UpdatedAt db.FlexibleTime
}

func (ProviderConfig) TableName() string { return "provider_config" }

// ProjectRecord 是项目的 DB 侧投影。
// 注意：project.json 才是项目元数据的唯一真相源，这张表只做列表缓存与状态索引。
type ProjectRecord struct {
	ID          uint   `gorm:"primaryKey;autoIncrement"`
	Name        string `gorm:"uniqueIndex;size:128;not null"`
	Title       string `gorm:"size:512"`
	ContentMode string `gorm:"column:content_mode;size:32"`
	UserID      string `gorm:"column:user_id;size:64;not null;default:default"`
	CreatedAt   db.FlexibleTime
	UpdatedAt   db.FlexibleTime
}

func (ProjectRecord) TableName() string { return "projects" }

// AutoMigrateTargets 是 P0 阶段纳入自动迁移的模型集合。
//
// P1 会换成从既有库导出的 0001_init.sql 基线（78 个 Alembic 迁移里大量是历史事故修补，
// 对新库无意义，但灰度期必须能读既有库，因此届时改为「按实测 DDL 建 + 补齐缺失表」）。
func AutoMigrateTargets() []any {
	return []any{
		&User{},
		&APIKey{},
		&SystemSetting{},
		&ProviderConfig{},
		&ProjectRecord{},
	}
}
