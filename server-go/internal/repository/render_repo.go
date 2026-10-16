package repository

// 渲染任务仓储（对应 lib/db/repositories/render_repository.py 的 HTTP 侧查询；
// worker 侧的租约回收不在本仓范围）。

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// RenderRepo 封装 render_jobs / render_artifacts / render_review_snapshots 的查询与写入。
type RenderRepo struct{ db *gorm.DB }

// NewRenderRepo 构造渲染仓储。
func NewRenderRepo(gdb *gorm.DB) *RenderRepo { return &RenderRepo{db: gdb} }

// WithTx 返回绑定到事务的仓储。
func (r *RenderRepo) WithTx(tx *gorm.DB) *RenderRepo { return &RenderRepo{db: tx} }

// GetOwnedJob 按 id + user 取任务；不存在返回 nil。
func (r *RenderRepo) GetOwnedJob(ctx context.Context, jobID, userID string) (*RenderJob, error) {
	var job RenderJob
	err := r.db.WithContext(ctx).Where("id = ? AND user_id = ?", jobID, userID).First(&job).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &job, nil
}

// FindActiveJob 查同 plan + revision + kind 下仍处于排队/运行/取消中的任务
// （对应 create_preview_job / create_final_job 的 active 预检）。
func (r *RenderRepo) FindActiveJob(ctx context.Context, planID, userID string, revisionNumber int64, kind string) (*RenderJob, error) {
	var job RenderJob
	err := r.db.WithContext(ctx).
		Where("plan_id = ? AND user_id = ? AND revision_number = ? AND kind = ? AND status IN ?", planID, userID, revisionNumber, kind, []string{"queued", "running", "cancelling"}).
		First(&job).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &job, nil
}

// ListJobs 列出 plan 的任务；kind 非 nil 时过滤；created_at 倒序、id 倒序。
func (r *RenderRepo) ListJobs(ctx context.Context, planID, userID string, kind *string) ([]RenderJob, error) {
	query := r.db.WithContext(ctx).Where("plan_id = ? AND user_id = ?", planID, userID)
	if kind != nil {
		query = query.Where("kind = ?", *kind)
	}
	var jobs []RenderJob
	if err := query.Order("created_at DESC, id DESC").Find(&jobs).Error; err != nil {
		return nil, err
	}
	return jobs, nil
}

// LatestArtifactForJob 取任务最新产物（created_at 倒序）。
func (r *RenderRepo) LatestArtifactForJob(ctx context.Context, jobID, userID string) (*RenderArtifact, error) {
	var artifact RenderArtifact
	err := r.db.WithContext(ctx).
		Where("render_job_id = ? AND user_id = ?", jobID, userID).
		Order("created_at DESC").First(&artifact).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &artifact, nil
}

// LatestFinalArtifactForRevision 取 plan 当前 revision 的最新 final 产物。
func (r *RenderRepo) LatestFinalArtifactForRevision(ctx context.Context, planID, userID string, revisionNumber int64) (*RenderArtifact, error) {
	var artifact RenderArtifact
	err := r.db.WithContext(ctx).
		Where("plan_id = ? AND user_id = ? AND kind = ? AND revision_number = ?", planID, userID, "final", revisionNumber).
		Order("created_at DESC").First(&artifact).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &artifact, nil
}

// GetOwnedArtifact 按 id + user 取产物；不存在返回 nil。
func (r *RenderRepo) GetOwnedArtifact(ctx context.Context, artifactID, userID string) (*RenderArtifact, error) {
	var artifact RenderArtifact
	err := r.db.WithContext(ctx).Where("id = ? AND user_id = ?", artifactID, userID).First(&artifact).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &artifact, nil
}

// FindReviewSnapshot 按 artifact + revision + 双指纹取最新审查快照。
func (r *RenderRepo) FindReviewSnapshot(
	ctx context.Context, artifactID, userID string, revisionNumber int64, sourceFingerprint, artifactFingerprint string,
) (*RenderReviewSnapshot, error) {
	var snapshot RenderReviewSnapshot
	err := r.db.WithContext(ctx).
		Where(
			"artifact_id = ? AND user_id = ? AND revision_number = ? AND source_fingerprint = ? AND artifact_fingerprint = ?",
			artifactID, userID, revisionNumber, sourceFingerprint, artifactFingerprint,
		).
		Order("created_at DESC").First(&snapshot).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &snapshot, nil
}

// CreateJob 写入一条渲染任务。
func (r *RenderRepo) CreateJob(ctx context.Context, job *RenderJob) error {
	return r.db.WithContext(ctx).Create(job).Error
}

// CreateArtifact 写入一条渲染产物。
func (r *RenderRepo) CreateArtifact(ctx context.Context, artifact *RenderArtifact) error {
	return r.db.WithContext(ctx).Create(artifact).Error
}

// CreateSnapshot 写入一条审查快照。
func (r *RenderRepo) CreateSnapshot(ctx context.Context, snapshot *RenderReviewSnapshot) error {
	return r.db.WithContext(ctx).Create(snapshot).Error
}

// RequestCancel 把排队/运行中的任务标记为取消中（对应 request_cancel）；
// 未命中返回 false。
func (r *RenderRepo) RequestCancel(ctx context.Context, jobID, userID string, now time.Time) (bool, error) {
	result := r.db.WithContext(ctx).Model(&RenderJob{}).
		Where("id = ? AND user_id = ? AND status IN ?", jobID, userID, []string{"queued", "running"}).
		Updates(map[string]any{"status": "cancelling", "cancel_requested_at": now, "updated_at": now})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// FinalizeUnclaimedCancellation 把无 worker 认领的取消中任务直接落成 cancelled
// （对应 finalize_unclaimed_cancellation）。
func (r *RenderRepo) FinalizeUnclaimedCancellation(ctx context.Context, jobID string, now time.Time) (bool, error) {
	result := r.db.WithContext(ctx).Model(&RenderJob{}).
		Where("id = ? AND status = ? AND worker_id IS NULL", jobID, "cancelling").
		Updates(map[string]any{"status": "cancelled", "progress_stage": "cancelled", "completed_at": now, "updated_at": now})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}
