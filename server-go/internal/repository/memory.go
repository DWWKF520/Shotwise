package repository

// 用户 / 项目级长期记忆的仓储（对应 lib/db/repositories/memory_repository.py）。
// 所有查询显式按 user_id 收敛，避免跨用户可见。

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// MemoryRepo 提供记忆条目与候选的持久化原语。
type MemoryRepo struct{ db *gorm.DB }

// NewMemoryRepo 构造仓储。
func NewMemoryRepo(gdb *gorm.DB) *MemoryRepo { return &MemoryRepo{db: gdb} }

// WithTx 返回绑定到给定事务的仓储副本。
func (r *MemoryRepo) WithTx(tx *gorm.DB) *MemoryRepo { return &MemoryRepo{db: tx} }

// ListEntries 列出已确认的记忆条目，按 updated_at 倒序。
func (r *MemoryRepo) ListEntries(ctx context.Context, userID, scope string, projectName *string) ([]MemoryEntry, error) {
	q := r.db.WithContext(ctx).Model(&MemoryEntry{}).Where("user_id = ?", userID)
	if scope != "" {
		q = q.Where("scope = ?", scope)
	}
	if projectName != nil {
		q = q.Where("project_name = ?", *projectName)
	}
	var rows []MemoryEntry
	err := q.Where("confirmed = ?", true).Order("updated_at DESC, id DESC").Find(&rows).Error
	return rows, err
}

// GetEntry 取单条记忆条目；不存在返回 (nil, nil)。
func (r *MemoryRepo) GetEntry(ctx context.Context, memoryID, userID string) (*MemoryEntry, error) {
	var row MemoryEntry
	err := r.db.WithContext(ctx).Where("id = ? AND user_id = ?", memoryID, userID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// CreateEntry 写入一条记忆条目。
func (r *MemoryRepo) CreateEntry(ctx context.Context, row *MemoryEntry) error {
	return r.db.WithContext(ctx).Create(row).Error
}

// UpdateEntry 按 memory_id + user_id 更新指定字段；受影响行数为 0 时返回 (nil, nil)。
func (r *MemoryRepo) UpdateEntry(
	ctx context.Context, memoryID, userID string, values map[string]any,
) (*MemoryEntry, error) {
	values["updated_at"] = time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&MemoryEntry{}).
		Where("id = ? AND user_id = ?", memoryID, userID).Updates(values)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return r.GetEntry(ctx, memoryID, userID)
}

// DeleteEntry 删除指定记忆条目，返回是否真的删掉了。
func (r *MemoryRepo) DeleteEntry(ctx context.Context, memoryID, userID string) (bool, error) {
	result := r.db.WithContext(ctx).Where("id = ? AND user_id = ?", memoryID, userID).Delete(&MemoryEntry{})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// ClearEntries 清空给定作用域的记忆条目，返回删除条数。
func (r *MemoryRepo) ClearEntries(ctx context.Context, userID, scope string, projectName *string) (int64, error) {
	q := r.db.WithContext(ctx).Where("user_id = ? AND scope = ?", userID, scope)
	if projectName != nil {
		q = q.Where("project_name = ?", *projectName)
	}
	result := q.Delete(&MemoryEntry{})
	return result.RowsAffected, result.Error
}

// FindDuplicate 按 (scope, project, category, content) 查重；无重复返回 (nil, nil)。
func (r *MemoryRepo) FindDuplicate(
	ctx context.Context, userID, scope string, projectName *string, category, content string,
) (*MemoryEntry, error) {
	q := r.db.WithContext(ctx).Model(&MemoryEntry{}).
		Where("user_id = ? AND scope = ? AND category = ? AND content = ?", userID, scope, category, content)
	if projectName != nil {
		q = q.Where("project_name = ?", *projectName)
	}
	var row MemoryEntry
	err := q.Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// ListCandidates 列出待决的记忆候选，按 updated_at 倒序。
func (r *MemoryRepo) ListCandidates(
	ctx context.Context, userID string, scope, projectName *string,
) ([]MemoryCandidate, error) {
	q := r.db.WithContext(ctx).Model(&MemoryCandidate{}).Where("user_id = ?", userID)
	if scope != nil {
		q = q.Where("scope = ?", *scope)
	}
	if projectName != nil {
		q = q.Where("project_name = ?", *projectName)
	}
	var rows []MemoryCandidate
	err := q.Where("status = ?", "pending").Order("updated_at DESC, id DESC").Find(&rows).Error
	return rows, err
}

// GetCandidate 取单个候选；不存在返回 (nil, nil)。
func (r *MemoryRepo) GetCandidate(ctx context.Context, candidateID, userID string) (*MemoryCandidate, error) {
	var row MemoryCandidate
	err := r.db.WithContext(ctx).Where("id = ? AND user_id = ?", candidateID, userID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// CreateCandidate 写入一条候选记忆。
func (r *MemoryRepo) CreateCandidate(ctx context.Context, row *MemoryCandidate) error {
	return r.db.WithContext(ctx).Create(row).Error
}

// SetCandidateStatus 翻转候选状态；受影响行数为 0 时返回 (nil, nil)。
func (r *MemoryRepo) SetCandidateStatus(
	ctx context.Context, candidateID, userID, status string, confirmed bool,
) (*MemoryCandidate, error) {
	result := r.db.WithContext(ctx).Model(&MemoryCandidate{}).
		Where("id = ? AND user_id = ?", candidateID, userID).
		Updates(map[string]any{"status": status, "confirmed": confirmed, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return r.GetCandidate(ctx, candidateID, userID)
}

// ClearCandidates 清空候选，返回删除条数。
func (r *MemoryRepo) ClearCandidates(ctx context.Context, userID string, scope, projectName *string) (int64, error) {
	q := r.db.WithContext(ctx).Where("user_id = ?", userID)
	if scope != nil {
		q = q.Where("scope = ?", *scope)
	}
	if projectName != nil {
		q = q.Where("project_name = ?", *projectName)
	}
	result := q.Delete(&MemoryCandidate{})
	return result.RowsAffected, result.Error
}
