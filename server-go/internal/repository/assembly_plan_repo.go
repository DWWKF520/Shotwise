package repository

// 用户级媒体装配计划与不可变 revision 的仓储（对应
// lib/db/repositories/assembly_plan_repository.py）。

import (
	"context"
	"errors"
	"time"

	"github.com/shotwise/shotwise/internal/infra/db"
	"gorm.io/gorm"
)

// AssemblyPlanRepo 读写 assembly_plans / assembly_plan_revisions。
type AssemblyPlanRepo struct{ db *gorm.DB }

// NewAssemblyPlanRepo 构造仓储。
func NewAssemblyPlanRepo(gdb *gorm.DB) *AssemblyPlanRepo { return &AssemblyPlanRepo{db: gdb} }

// WithTx 返回绑定到给定事务的仓储副本，供服务层在同一事务内串起多次写入。
func (r *AssemblyPlanRepo) WithTx(tx *gorm.DB) *AssemblyPlanRepo { return &AssemblyPlanRepo{db: tx} }

// GetOwned 仅在计划属于请求用户时返回；不存在或非本人所有都返回 (nil, nil)。
func (r *AssemblyPlanRepo) GetOwned(ctx context.Context, planID, userID string) (*AssemblyPlan, error) {
	var plan AssemblyPlan
	err := r.db.WithContext(ctx).Where("id = ? AND user_id = ?", planID, userID).Take(&plan).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &plan, nil
}

// GetCurrentRevision 按计划的 current_revision_number 指针加载对应 revision。
func (r *AssemblyPlanRepo) GetCurrentRevision(ctx context.Context, plan *AssemblyPlan) (*AssemblyPlanRevision, error) {
	var revision AssemblyPlanRevision
	err := r.db.WithContext(ctx).
		Where("plan_id = ? AND version_number = ?", plan.ID, plan.CurrentRevisionNumber).
		Take(&revision).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &revision, nil
}

// AddPlan 写入新计划与其首个 revision。
func (r *AssemblyPlanRepo) AddPlan(ctx context.Context, plan *AssemblyPlan, revision *AssemblyPlanRevision) error {
	if err := r.db.WithContext(ctx).Create(plan).Error; err != nil {
		return err
	}
	return r.db.WithContext(ctx).Create(revision).Error
}

// AddRevision 写入一个不可变 revision。
func (r *AssemblyPlanRepo) AddRevision(ctx context.Context, revision *AssemblyPlanRevision) error {
	return r.db.WithContext(ctx).Create(revision).Error
}

// ListByProject 按 updated_at 倒序（再按 id 倒序）列出项目下的计划。
func (r *AssemblyPlanRepo) ListByProject(ctx context.Context, userID, projectName string) ([]AssemblyPlan, error) {
	var plans []AssemblyPlan
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND project_name = ?", userID, projectName).
		Order("updated_at DESC, id DESC").Find(&plans).Error
	return plans, err
}

// UpdateStatus 直接落库计划状态（转移合法性已由调用方校验）。
func (r *AssemblyPlanRepo) UpdateStatus(ctx context.Context, planID, userID, status string, now time.Time) error {
	return r.db.WithContext(ctx).Model(&AssemblyPlan{}).
		Where("id = ? AND user_id = ?", planID, userID).
		Updates(map[string]any{"status": status, "updated_at": db.NewFlexibleTime(now)}).Error
}

// SetPreviewReady 记录某一 revision 的预览已就绪并把状态推进为 preview_ready。
func (r *AssemblyPlanRepo) SetPreviewReady(
	ctx context.Context, planID, userID string, revisionNumber int64, now time.Time,
) error {
	return r.db.WithContext(ctx).Model(&AssemblyPlan{}).
		Where("id = ? AND user_id = ?", planID, userID).
		Updates(map[string]any{
			"preview_revision_number": revisionNumber,
			"preview_ready_at":        db.NewFlexibleTime(now),
			"status":                  "preview_ready",
			"updated_at":              db.NewFlexibleTime(now),
		}).Error
}

// ConfirmPreview 条件更新预览确认字段；命中一行返回 true，否则 false（转成冲突）。
func (r *AssemblyPlanRepo) ConfirmPreview(
	ctx context.Context, planID, userID string, revisionNumber int64, now time.Time,
) (bool, error) {
	result := r.db.WithContext(ctx).Model(&AssemblyPlan{}).
		Where(
			"id = ? AND user_id = ? AND status = ? AND current_revision_number = ? AND preview_revision_number = ?",
			planID, userID, "preview_ready", revisionNumber, revisionNumber,
		).
		Updates(map[string]any{
			"preview_confirmed_by": userID,
			"preview_confirmed_at": db.NewFlexibleTime(now),
			"updated_at":           db.NewFlexibleTime(now),
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// ConfirmRender 条件更新渲染确认字段并把状态推进为 render_pending。
func (r *AssemblyPlanRepo) ConfirmRender(
	ctx context.Context, planID, userID string, revisionNumber int64, now time.Time,
) (bool, error) {
	result := r.db.WithContext(ctx).Model(&AssemblyPlan{}).
		Where(
			"id = ? AND user_id = ? AND status = ? AND current_revision_number = ? AND preview_revision_number = ?"+
				" AND preview_confirmed_by IS NOT NULL AND preview_confirmed_at IS NOT NULL",
			planID, userID, "preview_ready", revisionNumber, revisionNumber,
		).
		Updates(map[string]any{
			"render_confirmed_by": userID,
			"render_confirmed_at": db.NewFlexibleTime(now),
			"status":              "render_pending",
			"updated_at":          db.NewFlexibleTime(now),
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// MarkStale 把计划状态翻成 stale。
func (r *AssemblyPlanRepo) MarkStale(ctx context.Context, planID string, now time.Time) error {
	return r.db.WithContext(ctx).Model(&AssemblyPlan{}).
		Where("id = ?", planID).
		Updates(map[string]any{"status": "stale", "updated_at": db.NewFlexibleTime(now)}).Error
}

// AdvanceRevision 用乐观比较交换推进 current revision 指针；
// 期望值不匹配时返回 false（调用方转成 revision_conflict）。
// 同时按 Python 口径把状态复位为 draft 并清空 preview / render 确认字段。
func (r *AssemblyPlanRepo) AdvanceRevision(
	ctx context.Context, planID string, expected int64, newFingerprint string, updatedAt time.Time,
) (bool, error) {
	result := r.db.WithContext(ctx).
		Model(&AssemblyPlan{}).
		Where("id = ? AND current_revision_number = ?", planID, expected).
		Updates(map[string]any{
			"current_revision_number":    expected + 1,
			"current_source_fingerprint": newFingerprint,
			"status":                     "draft",
			"preview_revision_number":    nil,
			"preview_ready_at":           nil,
			"render_confirmed_by":        nil,
			"render_confirmed_at":        nil,
			"updated_at":                 db.NewFlexibleTime(updatedAt),
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// ClearPreviewConfirmation 清空与预览复核绑定的确认字段：
// 新的不可变 revision 让绑在旧预览上的批准失效。
func (r *AssemblyPlanRepo) ClearPreviewConfirmation(ctx context.Context, planID string) error {
	return r.db.WithContext(ctx).
		Model(&AssemblyPlan{}).
		Where("id = ?", planID).
		Updates(map[string]any{
			"preview_confirmed_by": nil,
			"preview_confirmed_at": nil,
		}).Error
}
