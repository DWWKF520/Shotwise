package repository

import (
	"time"

	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/infra/db"
	"gorm.io/gorm"
)

// UserRepository 提供用户与 API Key 的查询。
type UserRepository struct{ db *gorm.DB }

// NewUserRepository 构造用户仓储。
func NewUserRepository(gdb *gorm.DB) *UserRepository { return &UserRepository{db: gdb} }

// EnsureDefaultUser 保证默认用户存在。
func (r *UserRepository) EnsureDefaultUser(username string) error {
	u := User{ID: "default", Username: username, Role: "admin", IsActive: true}
	return r.db.Where(User{ID: "default"}).
		Assign(User{Username: username, Role: "admin"}).
		FirstOrCreate(&u).Error
}

// RoleOf 解析角色；无记录返回空串（调用方降级为 user，fail-closed）。
func (r *UserRepository) RoleOf(sub string) string {
	var u User
	if err := r.db.Where("username = ?", sub).First(&u).Error; err != nil {
		return ""
	}
	return u.Role
}

// APIKeyUser 按 key 摘要解析用户；顺带更新 last_used_at（失败不影响鉴权结果）。
func (r *UserRepository) APIKeyUser(keyHash string) (*auth.User, bool) {
	var k APIKey
	if err := r.db.Where("key_hash = ?", keyHash).First(&k).Error; err != nil {
		return nil, false
	}
	if !k.ExpiresAt.IsZero() && k.ExpiresAt.Before(time.Now()) {
		return nil, false
	}
	_ = r.db.Model(&APIKey{}).Where("id = ?", k.ID).
		Update("last_used_at", db.NewFlexibleTime(time.Now())).Error

	role := "user"
	if k.UserID != "" {
		var u User
		if err := r.db.Where("id = ?", k.UserID).First(&u).Error; err == nil {
			role = u.Role
		}
	}
	return &auth.User{ID: k.UserID, Sub: "apikey:" + k.Name, Role: role, AuthVia: "apikey"}, true
}
