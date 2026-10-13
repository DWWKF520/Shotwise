package repository

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shotwise/shotwise/internal/infra/db"
	"gorm.io/gorm"
)

// GlobalAssetQuery 是资产库列表的过滤条件（type / 关键字 q / 分页）。
type GlobalAssetQuery struct {
	Type   string
	Search string
	Limit  int
	Offset int
}

// GlobalAssetRepository 对应 Python 侧 AssetRepository：assets 表的 CRUD。
//
// DB/磁盘一致性由 service 层保证：先落盘再建 DB 记录，任何失败路径清理孤儿文件。
type GlobalAssetRepository struct{ gdb *gorm.DB }

// NewGlobalAssetRepository 构造资产仓储。
func NewGlobalAssetRepository(gdb *gorm.DB) *GlobalAssetRepository {
	return &GlobalAssetRepository{gdb: gdb}
}

// List 按条件列出资产，updated_at 倒序。
func (r *GlobalAssetRepository) List(q GlobalAssetQuery) ([]Asset, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	stmt := r.gdb.Model(&Asset{}).Order("updated_at DESC").Limit(limit).Offset(q.Offset)
	if q.Type != "" {
		stmt = stmt.Where("type = ?", q.Type)
	}
	if q.Search != "" {
		stmt = stmt.Where("name LIKE ?", "%"+q.Search+"%")
	}
	var items []Asset
	if err := stmt.Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// GetByID 按 id 取资产。
func (r *GlobalAssetRepository) GetByID(id string) (*Asset, error) {
	var a Asset
	if err := r.gdb.Where("id = ?", id).First(&a).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// GetByIDs 批量取资产（供 apply-to-project 一次读齐）。
func (r *GlobalAssetRepository) GetByIDs(ids []string) ([]Asset, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var items []Asset
	if err := r.gdb.Where("id IN ?", ids).Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// ErrAssetNotFound 统一的「不存在」哨兵错误（含 record not found 与空 id 两种来源）。
var ErrAssetNotFound = gorm.ErrRecordNotFound

// CreateParams 是建资产的字段集合。
type CreateParams struct {
	Type          string
	Name          string
	Description   string
	VoiceStyle    string
	ImagePath     *string
	AudioPath     *string
	SourceProject *string
}

// Create 插入资产记录；唯一约束冲突（type+name）返回 isUnique=false。
//
// 调用方据此走 409 路径并清理已落盘的孤儿文件。
func (r *GlobalAssetRepository) Create(p CreateParams) (*Asset, bool, error) {
	now := db.NewFlexibleTime(time.Now())
	a := Asset{
		ID:            uuid.NewString(),
		Type:          p.Type,
		Name:          p.Name,
		Description:   p.Description,
		VoiceStyle:    p.VoiceStyle,
		ImagePath:     p.ImagePath,
		AudioPath:     p.AudioPath,
		SourceProject: p.SourceProject,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := r.gdb.Create(&a).Error; err != nil {
		if isUniqueViolation(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &a, true, nil
}

// Update 就地更新给定字段；返回更新后的记录。
func (r *GlobalAssetRepository) Update(id string, fields map[string]any) (*Asset, error) {
	if len(fields) > 0 {
		if _, exists := fields["updated_at"]; !exists {
			fields["updated_at"] = db.NewFlexibleTime(time.Now())
		}
		if err := r.gdb.Model(&Asset{}).Where("id = ?", id).Updates(fields).Error; err != nil {
			return nil, err
		}
	}
	return r.GetByID(id)
}

// Delete 删除资产记录；不存在不报错（幂等）。
func (r *GlobalAssetRepository) Delete(id string) error {
	return r.gdb.Where("id = ?", id).Delete(&Asset{}).Error
}

// GetByTypeName 同 type + 同名查询。
func (r *GlobalAssetRepository) GetByTypeName(assetType, name string) (*Asset, error) {
	var a Asset
	if err := r.gdb.Where("type = ? AND name = ?", assetType, name).First(&a).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// isUniqueViolation 识别 SQLite/PostgreSQL 两方言下的唯一约束冲突。
func isUniqueViolation(err error) bool {
	msg := err.Error()
	return containsAny(msg, "UNIQUE constraint failed", "duplicate key value", "unique constraint")
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
