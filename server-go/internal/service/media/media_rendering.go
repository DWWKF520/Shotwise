// 渲染任务服务（对应 server/services/media_rendering.py 的 HTTP 侧函数）：
// 预览/成片的任务入队与生命周期、产物读取、成片审查流水线与字幕导出。
// worker 侧的 run_preview_job / run_final_job / render_*_pipeline 由渲染
// worker 调用，不在本文件范围。
package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shotwise/shotwise/internal/domain/media"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/infra/ffmpeg"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"gorm.io/gorm"
)

// RenderJobNotFoundError 表示任务不存在或不属于请求用户；Error 文本即 job_id
// （与 Python RenderJobNotFoundError(LookupError) 的 str(exc) 口径一致）。
type RenderJobNotFoundError struct{ JobID string }

func (e *RenderJobNotFoundError) Error() string { return e.JobID }

// RenderArtifactNotFoundError 表示产物不存在或不属于请求用户。
type RenderArtifactNotFoundError struct{ ArtifactID string }

func (e *RenderArtifactNotFoundError) Error() string { return e.ArtifactID }

// FinalRenderNotReadyError 表示当前 revision 还没有成片产物；Error 文本即 plan_id
// （作为 409 not_ready 的 resource 字段）。
type FinalRenderNotReadyError struct{ PlanID string }

func (e *FinalRenderNotReadyError) Error() string { return e.PlanID }

// FinalRenderReviewError 表示成片审查的确定性检查无法完成。
type FinalRenderReviewError struct {
	Code    string
	Message string
}

func (e *FinalRenderReviewError) Error() string { return e.Message }

// RenderReviewConflictError 表示当前产物的终审无法确认。
type RenderReviewConflictError struct {
	Code   string
	Status string
}

func (e *RenderReviewConflictError) Error() string { return e.Code }

// SubtitleExportError 表示 revision 里的字幕数据非法；Error 文本即用户可见消息。
type SubtitleExportError struct{ Message string }

func (e *SubtitleExportError) Error() string { return e.Message }

// RenderRevisionConflictError 表示任务/产物不再指向计划当前 revision；
// Code 默认 revision_conflict。
type RenderRevisionConflictError struct {
	Code   string
	Status string
}

func (e *RenderRevisionConflictError) Error() string { return e.Code }

// RenderJobConflictError 表示任务生命周期或前置门槛拒绝操作。
type RenderJobConflictError struct {
	Code   string
	Status string
}

func (e *RenderJobConflictError) Error() string { return e.Code }

// MediaRenderingService 承载渲染任务的持久化与审查。
type MediaRenderingService struct {
	db    *gorm.DB
	store *service.ProjectStore
}

// NewMediaRenderingService 构造服务。
func NewMediaRenderingService(gdb *gorm.DB, store *service.ProjectStore) *MediaRenderingService {
	return &MediaRenderingService{db: gdb, store: store}
}

// --- payload 构造 ---

func renderJobPayload(job *repository.RenderJob) map[string]any {
	return map[string]any{
		"id":                  job.ID,
		"plan_id":             job.PlanID,
		"project_name":        job.ProjectName,
		"user_id":             job.UserID,
		"revision_number":     job.RevisionNumber,
		"kind":                job.Kind,
		"status":              job.Status,
		"attempt":             job.Attempt,
		"max_attempts":        job.MaxAttempts,
		"input_fingerprint":   job.InputFingerprint,
		"progress":            job.Progress,
		"progress_stage":      job.ProgressStage,
		"heartbeat_at":        service.NullableISO(job.HeartbeatAt),
		"cancel_requested_at": service.NullableISO(job.CancelRequestedAt),
		"error_code":          job.ErrorCode,
		"error_message":       job.ErrorMessage,
		"created_at":          service.IsoformatTime(job.CreatedAt.Time),
		"started_at":          service.NullableISO(job.StartedAt),
		"completed_at":        service.NullableISO(job.CompletedAt),
		"updated_at":          service.IsoformatTime(job.UpdatedAt.Time),
	}
}

func renderArtifactPayload(artifact *repository.RenderArtifact) map[string]any {
	return map[string]any{
		"id":               artifact.ID,
		"render_job_id":    artifact.RenderJobID,
		"plan_id":          artifact.PlanID,
		"project_name":     artifact.ProjectName,
		"user_id":          artifact.UserID,
		"revision_number":  artifact.RevisionNumber,
		"kind":             artifact.Kind,
		"relative_path":    artifact.RelativePath,
		"mime_type":        artifact.MimeType,
		"size_bytes":       artifact.SizeBytes,
		"duration_seconds": artifact.DurationSeconds,
		"width":            artifact.Width,
		"height":           artifact.Height,
		"fingerprint":      artifact.Fingerprint,
		"created_at":       service.IsoformatTime(artifact.CreatedAt.Time),
	}
}

func reviewSnapshotPayload(snapshot *repository.RenderReviewSnapshot) map[string]any {
	return map[string]any{
		"id":                   snapshot.ID,
		"artifact_id":          snapshot.ArtifactID,
		"plan_id":              snapshot.PlanID,
		"project_name":         snapshot.ProjectName,
		"user_id":              snapshot.UserID,
		"revision_number":      snapshot.RevisionNumber,
		"source_fingerprint":   snapshot.SourceFingerprint,
		"artifact_fingerprint": snapshot.ArtifactFingerprint,
		"status":               snapshot.Status,
		"checks":               service.ParseJSONValue(snapshot.ChecksJSON),
		"created_at":           service.IsoformatTime(snapshot.CreatedAt.Time),
		"confirmed_by":         snapshot.ConfirmedBy,
		"confirmed_at":         service.NullableISO(snapshot.ConfirmedAt),
	}
}

// --- 取数辅助 ---

func ownedRenderPlan(
	ctx context.Context, repo *repository.AssemblyPlanRepo, planID, userID string,
) (*repository.AssemblyPlan, error) {
	plan, err := repo.GetOwned(ctx, planID, userID)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, &AssemblyPlanNotFoundError{PlanID: planID}
	}
	return plan, nil
}

func currentRenderRevision(
	ctx context.Context, repo *repository.AssemblyPlanRepo, plan *repository.AssemblyPlan,
) (*repository.AssemblyPlanRevision, error) {
	revision, err := repo.GetCurrentRevision(ctx, plan)
	if err != nil {
		return nil, err
	}
	if revision == nil {
		return nil, &RenderRevisionConflictError{Code: "current_revision_missing", Status: plan.Status}
	}
	return revision, nil
}

func (s *MediaRenderingService) ownedJob(
	ctx context.Context, gdb *gorm.DB, jobID, userID string,
) (*repository.RenderJob, error) {
	job, err := repository.NewRenderRepo(gdb).GetOwnedJob(ctx, jobID, userID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, &RenderJobNotFoundError{JobID: jobID}
	}
	return job, nil
}

func (s *MediaRenderingService) ownedArtifact(
	ctx context.Context, gdb *gorm.DB, artifactID, userID string,
) (*repository.RenderArtifact, error) {
	artifact, err := repository.NewRenderRepo(gdb).GetOwnedArtifact(ctx, artifactID, userID)
	if err != nil {
		return nil, err
	}
	if artifact == nil {
		return nil, &RenderArtifactNotFoundError{ArtifactID: artifactID}
	}
	return artifact, nil
}

// --- 任务创建 ---

// CreatePreviewJob 为计划当前不可变 revision 建一个排队中的预览任务
// （对应 create_preview_job）。
func (s *MediaRenderingService) CreatePreviewJob(
	ctx context.Context, planID, userID string, revisionNumber *int64, maxAttempts int64,
) (map[string]any, error) {
	if maxAttempts < 1 || maxAttempts > 10 {
		return nil, &RenderJobConflictError{Code: "invalid_max_attempts"}
	}
	var payload map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result, err := s.createPreviewJobTx(ctx, tx, planID, userID, revisionNumber, maxAttempts)
		if err != nil {
			return err
		}
		payload = result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func (s *MediaRenderingService) createPreviewJobTx(
	ctx context.Context, tx *gorm.DB, planID, userID string, revisionNumber *int64, maxAttempts int64,
) (map[string]any, error) {
	planRepo := repository.NewAssemblyPlanRepo(tx)
	plan, err := ownedRenderPlan(ctx, planRepo, planID, userID)
	if err != nil {
		return nil, err
	}
	revision, err := currentRenderRevision(ctx, planRepo, plan)
	if err != nil {
		return nil, err
	}
	target := plan.CurrentRevisionNumber
	if revisionNumber != nil {
		target = *revisionNumber
	}
	if target != plan.CurrentRevisionNumber || revision.VersionNumber != target {
		return nil, &RenderRevisionConflictError{Code: "revision_conflict", Status: plan.Status}
	}
	if plan.Status != "confirmed" {
		return nil, &RenderJobConflictError{Code: "preview_requires_confirmed_plan", Status: plan.Status}
	}
	renderRepo := repository.NewRenderRepo(tx)
	active, err := renderRepo.FindActiveJob(ctx, plan.ID, userID, target, "preview")
	if err != nil {
		return nil, err
	}
	if active != nil {
		return nil, &RenderJobConflictError{Code: "preview_job_already_running", Status: active.Status}
	}
	now := time.Now().UTC()
	job := &repository.RenderJob{
		ID:               uuid.NewString(),
		PlanID:           plan.ID,
		ProjectName:      plan.ProjectName,
		UserID:           userID,
		RevisionNumber:   target,
		Kind:             "preview",
		Status:           "queued",
		Attempt:          0,
		MaxAttempts:      maxAttempts,
		InputFingerprint: revision.SourceFingerprint,
		Progress:         0,
		ProgressStage:    "queued",
		CreatedAt:        db.NewFlexibleTime(now),
		UpdatedAt:        db.NewFlexibleTime(now),
	}
	if err := renderRepo.CreateJob(ctx, job); err != nil {
		// render_jobs 上的部分唯一索引兜底并发入队（uq_render_jobs_active_plan_revision_kind）。
		if db.IsUniqueViolation(err) {
			return nil, &RenderJobConflictError{Code: "preview_job_already_running"}
		}
		return nil, err
	}
	if _, err := NewMediaAssemblyService(tx).TransitionPlan(ctx, plan.ID, userID, "preview_pending"); err != nil {
		var conflict *AssemblyPlanConflictError
		if errors.As(err, &conflict) {
			return nil, &RenderJobConflictError{Code: conflict.Code, Status: conflict.Status}
		}
		return nil, err
	}
	return renderJobPayload(job), nil
}

// CreateFinalJob 只为用户显式确认过的 revision 排队成片任务
// （对应 create_final_job）。
func (s *MediaRenderingService) CreateFinalJob(
	ctx context.Context, planID, userID string, revisionNumber *int64, maxAttempts int64,
) (map[string]any, error) {
	if maxAttempts < 1 || maxAttempts > 10 {
		return nil, &RenderJobConflictError{Code: "invalid_max_attempts"}
	}
	var payload map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result, err := s.createFinalJobTx(ctx, tx, planID, userID, revisionNumber, maxAttempts)
		if err != nil {
			return err
		}
		payload = result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func (s *MediaRenderingService) createFinalJobTx(
	ctx context.Context, tx *gorm.DB, planID, userID string, revisionNumber *int64, maxAttempts int64,
) (map[string]any, error) {
	planRepo := repository.NewAssemblyPlanRepo(tx)
	plan, err := ownedRenderPlan(ctx, planRepo, planID, userID)
	if err != nil {
		return nil, err
	}
	revision, err := currentRenderRevision(ctx, planRepo, plan)
	if err != nil {
		return nil, err
	}
	target := plan.CurrentRevisionNumber
	if revisionNumber != nil {
		target = *revisionNumber
	}
	if err := validateFinalGate(plan, revision, target); err != nil {
		return nil, err
	}
	renderRepo := repository.NewRenderRepo(tx)
	active, err := renderRepo.FindActiveJob(ctx, plan.ID, userID, target, "final")
	if err != nil {
		return nil, err
	}
	if active != nil {
		return nil, &RenderJobConflictError{Code: "final_job_already_running", Status: active.Status}
	}
	now := time.Now().UTC()
	job := &repository.RenderJob{
		ID:               uuid.NewString(),
		PlanID:           plan.ID,
		ProjectName:      plan.ProjectName,
		UserID:           userID,
		RevisionNumber:   target,
		Kind:             "final",
		Status:           "queued",
		Attempt:          0,
		MaxAttempts:      maxAttempts,
		InputFingerprint: revision.SourceFingerprint,
		Progress:         0,
		ProgressStage:    "queued",
		CreatedAt:        db.NewFlexibleTime(now),
		UpdatedAt:        db.NewFlexibleTime(now),
	}
	if err := renderRepo.CreateJob(ctx, job); err != nil {
		if db.IsUniqueViolation(err) {
			return nil, &RenderJobConflictError{Code: "final_job_already_running"}
		}
		return nil, err
	}
	return renderJobPayload(job), nil
}

// validateFinalGate 校验成片任务的前置门槛（对应 _validate_final_gate）。
func validateFinalGate(plan *repository.AssemblyPlan, revision *repository.AssemblyPlanRevision, target int64) error {
	if target != plan.CurrentRevisionNumber || revision.VersionNumber != target {
		return &RenderRevisionConflictError{Code: "revision_conflict", Status: plan.Status}
	}
	if revision.SourceFingerprint != plan.CurrentSourceFingerprint {
		return &RenderRevisionConflictError{Code: "source_fingerprint_conflict", Status: plan.Status}
	}
	if plan.PreviewRevisionNumber == nil || *plan.PreviewRevisionNumber != target {
		return &RenderJobConflictError{Code: "preview_revision_required", Status: plan.Status}
	}
	if plan.PreviewConfirmedBy == nil || !plan.PreviewConfirmedAt.Valid {
		return &RenderJobConflictError{Code: "preview_confirmation_required", Status: plan.Status}
	}
	if plan.RenderConfirmedBy == nil || !plan.RenderConfirmedAt.Valid {
		return &RenderJobConflictError{Code: "render_confirmation_required", Status: plan.Status}
	}
	if plan.Status != "render_pending" {
		return &RenderJobConflictError{Code: "final_render_pending_required", Status: plan.Status}
	}
	return nil
}

// validateFinalRevision 在任务被 worker 认领后复核不可变输入
// （对应 _validate_final_revision）。
func validateFinalRevision(
	plan *repository.AssemblyPlan, revision *repository.AssemblyPlanRevision,
	job *repository.RenderJob, allowStatuses map[string]bool,
) error {
	if plan.CurrentRevisionNumber != job.RevisionNumber || revision.VersionNumber != job.RevisionNumber {
		return &RenderRevisionConflictError{Code: "revision_conflict", Status: plan.Status}
	}
	if revision.SourceFingerprint != job.InputFingerprint {
		return &RenderRevisionConflictError{Code: "source_fingerprint_conflict", Status: plan.Status}
	}
	if revision.SourceFingerprint != plan.CurrentSourceFingerprint {
		return &RenderRevisionConflictError{Code: "revision_conflict", Status: plan.Status}
	}
	if plan.PreviewRevisionNumber == nil || *plan.PreviewRevisionNumber != job.RevisionNumber {
		return &RenderJobConflictError{Code: "preview_revision_required", Status: plan.Status}
	}
	if plan.PreviewConfirmedBy == nil || !plan.PreviewConfirmedAt.Valid {
		return &RenderJobConflictError{Code: "preview_confirmation_required", Status: plan.Status}
	}
	if plan.RenderConfirmedBy == nil || !plan.RenderConfirmedAt.Valid {
		return &RenderJobConflictError{Code: "render_confirmation_required", Status: plan.Status}
	}
	if !allowStatuses[plan.Status] {
		return &RenderJobConflictError{Code: "final_render_status_conflict", Status: plan.Status}
	}
	return nil
}

// --- 任务读取 ---

// GetRenderJob 读取任务及其最新产物（对应 get_render_job）。
func (s *MediaRenderingService) GetRenderJob(ctx context.Context, jobID, userID string) (map[string]any, error) {
	job, err := s.ownedJob(ctx, s.db, jobID, userID)
	if err != nil {
		return nil, err
	}
	artifact, err := repository.NewRenderRepo(s.db).LatestArtifactForJob(ctx, job.ID, userID)
	if err != nil {
		return nil, err
	}
	payload := renderJobPayload(job)
	if artifact != nil {
		payload["artifact"] = renderArtifactPayload(artifact)
	} else {
		payload["artifact"] = nil
	}
	return payload, nil
}

// ListRenderJobs 列出计划的任务，kind 非 nil 时按类型过滤
// （对应 list_render_jobs）。
func (s *MediaRenderingService) ListRenderJobs(
	ctx context.Context, planID, userID string, kind *string,
) ([]map[string]any, error) {
	if _, err := ownedRenderPlan(ctx, repository.NewAssemblyPlanRepo(s.db), planID, userID); err != nil {
		return nil, err
	}
	jobs, err := repository.NewRenderRepo(s.db).ListJobs(ctx, planID, userID, kind)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(jobs))
	for i := range jobs {
		items = append(items, renderJobPayload(&jobs[i]))
	}
	return items, nil
}

// --- 任务重试与取消 ---

// RetryPreviewJob 把终态的预览任务重新排队（对应 retry_preview_job）。
func (s *MediaRenderingService) RetryPreviewJob(ctx context.Context, jobID, userID string) (map[string]any, error) {
	var payload map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := s.ownedJob(ctx, tx, jobID, userID)
		if err != nil {
			return err
		}
		if job.Status != "failed" && job.Status != "cancelled" {
			return &RenderJobConflictError{Code: "retry_requires_terminal_job", Status: job.Status}
		}
		if job.Attempt >= job.MaxAttempts {
			return &RenderJobConflictError{Code: "retry_limit_reached", Status: job.Status}
		}
		planRepo := repository.NewAssemblyPlanRepo(tx)
		plan, err := ownedRenderPlan(ctx, planRepo, job.PlanID, userID)
		if err != nil {
			return err
		}
		if plan.CurrentRevisionNumber != job.RevisionNumber {
			return &RenderRevisionConflictError{Code: "revision_conflict", Status: plan.Status}
		}
		if plan.Status != "preview_pending" && plan.Status != "confirmed" && plan.Status != "failed" {
			return &RenderJobConflictError{Code: "preview_retry_not_allowed", Status: plan.Status}
		}
		if plan.Status == "failed" || plan.Status == "confirmed" {
			plans := NewMediaAssemblyService(tx)
			if plan.Status == "failed" {
				if _, err := plans.TransitionPlan(ctx, plan.ID, userID, "confirmed"); err != nil {
					return mapTransitionConflict(err)
				}
			}
			if _, err := plans.TransitionPlan(ctx, plan.ID, userID, "preview_pending"); err != nil {
				return mapTransitionConflict(err)
			}
		}
		now := time.Now().UTC()
		if err := resetRenderJobForRetry(tx, job.ID, now); err != nil {
			return err
		}
		applyRetryReset(job, now)
		payload = renderJobPayload(job)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// RetryFinalJob 不改变已确认 revision 地重排失败的成片任务
// （对应 retry_final_job）。
func (s *MediaRenderingService) RetryFinalJob(ctx context.Context, jobID, userID string) (map[string]any, error) {
	var payload map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := s.ownedJob(ctx, tx, jobID, userID)
		if err != nil {
			return err
		}
		if job.Kind != "final" {
			return &RenderJobConflictError{Code: "final_retry_requires_final_job", Status: job.Status}
		}
		if job.Status != "failed" && job.Status != "cancelled" {
			return &RenderJobConflictError{Code: "retry_requires_terminal_job", Status: job.Status}
		}
		if job.Attempt >= job.MaxAttempts {
			return &RenderJobConflictError{Code: "retry_limit_reached", Status: job.Status}
		}
		planRepo := repository.NewAssemblyPlanRepo(tx)
		plan, err := ownedRenderPlan(ctx, planRepo, job.PlanID, userID)
		if err != nil {
			return err
		}
		revision, err := currentRenderRevision(ctx, planRepo, plan)
		if err != nil {
			return err
		}
		if err := validateFinalRevision(plan, revision, job, map[string]bool{"failed": true, "render_pending": true}); err != nil {
			return err
		}
		now := time.Now().UTC()
		// 条件 UPDATE 把确认门槛复核与状态推进合成一次原子写。
		result := tx.Model(&repository.AssemblyPlan{}).
			Where(
				"id = ? AND user_id = ? AND status IN ? AND current_revision_number = ? AND current_source_fingerprint = ? "+
					"AND preview_revision_number = ? AND preview_confirmed_by IS NOT NULL AND preview_confirmed_at IS NOT NULL "+
					"AND render_confirmed_by IS NOT NULL AND render_confirmed_at IS NOT NULL",
				plan.ID, userID, []string{"failed", "render_pending"},
				job.RevisionNumber, job.InputFingerprint, job.RevisionNumber,
			).
			Updates(map[string]any{"status": "render_pending", "updated_at": db.NewFlexibleTime(now)})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return &RenderJobConflictError{Code: "final_retry_confirmation_conflict", Status: plan.Status}
		}
		if err := resetRenderJobForRetry(tx, job.ID, now); err != nil {
			return err
		}
		applyRetryReset(job, now)
		payload = renderJobPayload(job)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// CancelRenderJob 请求取消进行中的任务并落定结果
// （对应 cancel_render_job）。
func (s *MediaRenderingService) CancelRenderJob(ctx context.Context, jobID, userID string) (map[string]any, error) {
	var payload map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := s.ownedJob(ctx, tx, jobID, userID)
		if err != nil {
			return err
		}
		if job.Status == "succeeded" || job.Status == "failed" {
			return &RenderJobConflictError{Code: "cancel_requires_active_job", Status: job.Status}
		}
		renderRepo := repository.NewRenderRepo(tx)
		if job.Status == "queued" || job.Status == "running" {
			ok, err := renderRepo.RequestCancel(ctx, job.ID, userID, time.Now().UTC())
			if err != nil {
				return err
			}
			if !ok {
				return &RenderJobConflictError{Code: "cancel_conflict", Status: job.Status}
			}
			if job, err = s.ownedJob(ctx, tx, jobID, userID); err != nil {
				return err
			}
		}
		if job.Status == "cancelling" {
			// 排队中的任务没有 worker 观察取消标记，就地落成 cancelled。
			if _, err := renderRepo.FinalizeUnclaimedCancellation(ctx, job.ID, time.Now().UTC()); err != nil {
				return err
			}
			if job, err = s.ownedJob(ctx, tx, jobID, userID); err != nil {
				return err
			}
		}
		payload = renderJobPayload(job)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// mapTransitionConflict 把装配计划的状态冲突映射成任务冲突口径。
func mapTransitionConflict(err error) error {
	var conflict *AssemblyPlanConflictError
	if errors.As(err, &conflict) {
		return &RenderJobConflictError{Code: conflict.Code, Status: conflict.Status}
	}
	return err
}

// resetRenderJobForRetry 把任务写回排队态并清空执行痕迹。
func resetRenderJobForRetry(tx *gorm.DB, jobID string, now time.Time) error {
	return tx.Model(&repository.RenderJob{}).Where("id = ?", jobID).Updates(map[string]any{
		"status":              "queued",
		"progress":            0.0,
		"progress_stage":      "queued",
		"error_code":          nil,
		"error_message":       nil,
		"worker_id":           nil,
		"heartbeat_at":        db.FlexibleTime{},
		"cancel_requested_at": db.FlexibleTime{},
		"started_at":          db.FlexibleTime{},
		"completed_at":        db.FlexibleTime{},
		"updated_at":          db.NewFlexibleTime(now),
	}).Error
}

// applyRetryReset 把重试后的内存视图与写库内容对齐，供 payload 渲染。
func applyRetryReset(job *repository.RenderJob, now time.Time) {
	job.Status = "queued"
	job.Progress = 0
	job.ProgressStage = "queued"
	job.ErrorCode = nil
	job.ErrorMessage = nil
	job.WorkerID = nil
	job.HeartbeatAt = db.FlexibleTime{}
	job.CancelRequestedAt = db.FlexibleTime{}
	job.StartedAt = db.FlexibleTime{}
	job.CompletedAt = db.FlexibleTime{}
	job.UpdatedAt = db.NewFlexibleTime(now)
}

// --- 产物读取 ---

// GetRenderArtifactFile 返回产物元数据与限定在其所属项目目录内的文件路径
// （对应 get_render_artifact_file）。
func (s *MediaRenderingService) GetRenderArtifactFile(
	ctx context.Context, artifactID, userID string,
) (map[string]any, string, error) {
	artifact, err := s.ownedArtifact(ctx, s.db, artifactID, userID)
	if err != nil {
		return nil, "", err
	}
	path, err := s.ResolveArtifactFile(artifact)
	if err != nil {
		return nil, "", err
	}
	return renderArtifactPayload(artifact), path, nil
}

// ResolveArtifactFile 只在产物所属项目目录内解析持久化路径
// （对应 resolve_artifact_file）。
func (s *MediaRenderingService) ResolveArtifactFile(artifact *repository.RenderArtifact) (string, error) {
	root, err := s.projectRoot(artifact.ProjectName)
	if err != nil {
		return "", &RenderArtifactNotFoundError{ArtifactID: artifact.ID}
	}
	path, err := fs.SafeResolve(root, artifact.RelativePath)
	if err != nil {
		return "", &RenderArtifactNotFoundError{ArtifactID: artifact.ID}
	}
	return path, nil
}

func (s *MediaRenderingService) projectRoot(projectName string) (string, error) {
	return s.store.Dir(projectName)
}

// --- 成片审查 ---

// ReviewFinalArtifact 检查并持久化当前 revision 成片的审查快照
// （对应 review_final_artifact）。
func (s *MediaRenderingService) ReviewFinalArtifact(ctx context.Context, planID, userID string) (map[string]any, error) {
	planRepo := repository.NewAssemblyPlanRepo(s.db)
	renderRepo := repository.NewRenderRepo(s.db)
	plan, err := ownedRenderPlan(ctx, planRepo, planID, userID)
	if err != nil {
		return nil, err
	}
	artifact, err := renderRepo.LatestFinalArtifactForRevision(ctx, plan.ID, userID, plan.CurrentRevisionNumber)
	if err != nil {
		return nil, err
	}
	if artifact == nil {
		return nil, &FinalRenderNotReadyError{PlanID: plan.ID}
	}
	revision, err := currentRenderRevision(ctx, planRepo, plan)
	if err != nil {
		return nil, err
	}
	existing, err := renderRepo.FindReviewSnapshot(
		ctx, artifact.ID, userID, revision.VersionNumber, revision.SourceFingerprint, artifact.Fingerprint,
	)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		checks, _ := service.ParseJSONValue(existing.ChecksJSON).(map[string]any)
		if checks == nil {
			checks = map[string]any{}
		}
		return map[string]any{
			"status":           existing.Status,
			"plan_id":          plan.ID,
			"revision_number":  artifact.RevisionNumber,
			"artifact":         renderArtifactPayload(artifact),
			"checks":           checks,
			"blocking_reasons": reviewChecksValue(checks, "blocking_reasons"),
			"warning_reasons":  reviewChecksValue(checks, "warning_reasons"),
			"info_reasons":     reviewChecksValue(checks, "info_reasons"),
			"issues":           reviewChecksValue(checks, "issues"),
			"review_snapshot":  reviewSnapshotPayload(existing),
		}, nil
	}
	sourcePath, err := s.ResolveArtifactFile(artifact)
	if err != nil {
		return nil, err
	}
	probe, err := s.probeFinalArtifact(ctx, sourcePath)
	if err != nil {
		return nil, err
	}
	duration, _ := probe["duration_seconds"].(float64)
	blackSegments, err := s.detectBlackFrames(ctx, sourcePath)
	if err != nil {
		return nil, err
	}
	audioPresent, _ := probe["audio_stream_present"].(bool)
	silenceSegments := []map[string]any{}
	if audioPresent {
		if silenceSegments, err = s.detectSilenceSegments(ctx, sourcePath); err != nil {
			return nil, err
		}
	}
	audioMetrics := map[string]any{}
	audioMetricsError := ""
	if audioPresent {
		metrics, metricsErr := s.detectAudioMetrics(ctx, sourcePath)
		if metricsErr != nil {
			var reviewErr *FinalRenderReviewError
			if !errors.As(metricsErr, &reviewErr) {
				return nil, metricsErr
			}
			audioMetricsError = reviewErr.Code
		} else {
			audioMetrics = metrics
		}
	}
	frameSamples := make([]any, 0, 3)
	for _, sample := range []struct {
		position  string
		timestamp float64
	}{
		{"first", 0.0},
		{"middle", duration / 2},
		{"last", math.Max(0.0, duration-0.001)},
	} {
		frame, frameErr := s.extractReviewFrame(ctx, artifact, sourcePath, sample.position, sample.timestamp)
		if frameErr != nil {
			reviewErr, ok := frameErr.(*FinalRenderReviewError)
			if !ok || reviewErr.Code != "review_frame_missing" {
				return nil, frameErr
			}
			frameSamples = append(frameSamples, map[string]any{
				"available":         false,
				"position":          sample.position,
				"timestamp_seconds": sample.timestamp,
				"error_code":        reviewErr.Code,
			})
			continue
		}
		frameSamples = append(frameSamples, frame)
	}

	metadataDuration := artifact.DurationSeconds
	durationDelta := duration - metadataDuration
	durationCheck := map[string]any{
		"seconds":                   duration,
		"metadata_seconds":          metadataDuration,
		"within_metadata_tolerance": math.Abs(durationDelta) <= 0.05,
	}
	var timelineList []any
	if list, ok := service.ParseJSONValue(revision.TimelineJSON).([]any); ok {
		timelineList = list
	}
	packaging := service.ParseJSONValue(revision.PackagingJSON)
	expectedDuration, err := media.ExpectedTimelineDuration(timelineList, packaging)
	if err != nil {
		return nil, err
	}
	timelineDelta := duration - expectedDuration
	timelineDurationCheck := map[string]any{
		"expected_seconds": expectedDuration,
		"actual_seconds":   duration,
		"within_tolerance": math.Abs(timelineDelta) <= 0.05,
	}
	audioCheck := map[string]any{"present": audioPresent, "severity": any(nil)}
	if !audioPresent {
		audioCheck["severity"] = "info"
	}
	audioQuality, err := media.AudioQualityCheck(audioPresent, silenceSegments, duration)
	if err != nil {
		return nil, err
	}
	audioLevels := media.AudioLevelCheck(audioMetrics)
	if audioMetricsError != "" {
		audioLevels["analysis_error"] = audioMetricsError
	}
	blackCheck, err := media.ClassifyBlackFrameEdges(blackSegments, duration)
	if err != nil {
		return nil, err
	}
	subtitleCheck, err := media.SubtitleBoundsCheck(service.ParseJSONValue(revision.SubtitleJSON), duration)
	if err != nil {
		return nil, err
	}
	allFramesAvailable := true
	for _, raw := range frameSamples {
		if frame, ok := raw.(map[string]any); ok {
			if available, isBool := frame["available"].(bool); !isBool || !available {
				allFramesAvailable = false
			}
		}
	}
	framesSeverity := any(nil)
	if !allFramesAvailable {
		framesSeverity = "blocking"
	}
	framesCheck := map[string]any{
		"all_available": allFramesAvailable,
		"items":         frameSamples,
		"severity":      framesSeverity,
	}

	issues := make([]map[string]any, 0)
	addIssue := func(check, code, severity string, details map[string]any) {
		issue := map[string]any{"check": check, "code": code, "severity": severity}
		for key, value := range details {
			issue[key] = value
		}
		issues = append(issues, issue)
	}
	if !durationCheck["within_metadata_tolerance"].(bool) {
		addIssue("duration", "duration_mismatch", "blocking", map[string]any{"delta_seconds": durationDelta})
	}
	if !timelineDurationCheck["within_tolerance"].(bool) {
		addIssue("timeline_duration", "timeline_duration_mismatch", "blocking", map[string]any{"delta_seconds": timelineDelta})
	}
	if blackCheck["severity"] == "blocking" {
		addIssue("black_frames", "black_frames_detected", "blocking", nil)
	} else if blackCheck["severity"] == "warning" {
		addIssue("black_frames", "edge_black_frames_detected", "warning", nil)
	}
	if valid, _ := subtitleCheck["valid"].(bool); !valid {
		addIssue("subtitle_bounds", "subtitle_bounds_invalid", "blocking", nil)
	}
	if qualityIssues, ok := audioQuality["issues"].([]any); ok {
		for _, raw := range qualityIssues {
			if issue, isMap := raw.(map[string]any); isMap {
				code, _ := issue["code"].(string)
				severity, _ := issue["severity"].(string)
				addIssue("audio_quality", code, severity, nil)
			}
		}
	}
	if levelIssues, ok := audioLevels["issues"].([]any); ok {
		for _, raw := range levelIssues {
			if issue, isMap := raw.(map[string]any); isMap {
				code, _ := issue["code"].(string)
				severity, _ := issue["severity"].(string)
				details := map[string]any{}
				for key, value := range issue {
					if key != "code" && key != "severity" {
						details[key] = value
					}
				}
				addIssue("audio_levels", code, severity, details)
			}
		}
	}
	if audioPresent {
		if available, _ := audioLevels["available"].(bool); !available {
			addIssue("audio_levels", "audio_metrics_unavailable", "info", nil)
		}
	}
	if !allFramesAvailable {
		addIssue("frames", "review_frame_missing", "blocking", nil)
	}

	blockingReasons := make([]any, 0)
	warningReasons := make([]any, 0)
	infoReasons := make([]any, 0)
	for _, issue := range issues {
		switch issue["severity"] {
		case "blocking":
			blockingReasons = append(blockingReasons, issue["code"])
		case "warning":
			warningReasons = append(warningReasons, issue["code"])
		case "info":
			infoReasons = append(infoReasons, issue["code"])
		}
	}
	status := "ready"
	if len(blockingReasons) > 0 {
		status = "blocked"
	}
	checksPayload := map[string]any{
		"duration":          durationCheck,
		"timeline_duration": timelineDurationCheck,
		"audio_stream":      audioCheck,
		"audio_quality":     audioQuality,
		"audio_levels":      audioLevels,
		"black_frames":      blackCheck,
		"subtitle_bounds":   subtitleCheck,
		"frames":            framesCheck,
		"issues":            issues,
		"blocking_reasons":  blockingReasons,
		"warning_reasons":   warningReasons,
		"info_reasons":      infoReasons,
	}
	checksJSON, err := service.CanonicalString(checksPayload)
	if err != nil {
		return nil, err
	}
	snapshot := &repository.RenderReviewSnapshot{
		ID:                  uuid.NewString(),
		ArtifactID:          artifact.ID,
		PlanID:              plan.ID,
		ProjectName:         plan.ProjectName,
		UserID:              userID,
		RevisionNumber:      revision.VersionNumber,
		SourceFingerprint:   revision.SourceFingerprint,
		ArtifactFingerprint: artifact.Fingerprint,
		Status:              status,
		ChecksJSON:          checksJSON,
		CreatedAt:           db.NewFlexibleTime(time.Now().UTC()),
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return repository.NewRenderRepo(tx).CreateSnapshot(ctx, snapshot)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"status":           status,
		"plan_id":          plan.ID,
		"revision_number":  artifact.RevisionNumber,
		"artifact":         renderArtifactPayload(artifact),
		"checks":           checksPayload,
		"blocking_reasons": blockingReasons,
		"warning_reasons":  warningReasons,
		"info_reasons":     infoReasons,
		"issues":           issues,
		"review_snapshot":  reviewSnapshotPayload(snapshot),
	}, nil
}

// reviewChecksValue 从缓存快照的 checks 里取列表字段，缺失时回空列表。
func reviewChecksValue(checks map[string]any, key string) any {
	if v, ok := checks[key].([]any); ok {
		return v
	}
	return []any{}
}

// ConfirmFinalReview 记录用户对最新通过审查的成片的批准
// （对应 confirm_final_review）。
func (s *MediaRenderingService) ConfirmFinalReview(ctx context.Context, artifactID, userID string) (map[string]any, error) {
	var payload map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		artifact, err := s.ownedArtifact(ctx, tx, artifactID, userID)
		if err != nil {
			return err
		}
		if artifact.Kind != "final" {
			return &RenderReviewConflictError{Code: "final_artifact_required"}
		}
		planRepo := repository.NewAssemblyPlanRepo(tx)
		plan, err := ownedRenderPlan(ctx, planRepo, artifact.PlanID, userID)
		if err != nil {
			return err
		}
		revision, err := currentRenderRevision(ctx, planRepo, plan)
		if err != nil {
			return err
		}
		if revision.VersionNumber != artifact.RevisionNumber {
			return &RenderReviewConflictError{Code: "review_revision_conflict", Status: plan.Status}
		}
		renderRepo := repository.NewRenderRepo(tx)
		snapshot, err := renderRepo.FindReviewSnapshot(
			ctx, artifact.ID, userID, revision.VersionNumber, revision.SourceFingerprint, artifact.Fingerprint,
		)
		if err != nil {
			return err
		}
		if snapshot == nil {
			return &RenderReviewConflictError{Code: "review_required", Status: plan.Status}
		}
		if snapshot.Status != "ready" {
			return &RenderReviewConflictError{Code: "review_blocked", Status: snapshot.Status}
		}
		now := time.Now().UTC()
		if err := tx.Model(&repository.RenderReviewSnapshot{}).Where("id = ?", snapshot.ID).Updates(map[string]any{
			"confirmed_by": userID,
			"confirmed_at": db.NewFlexibleTime(now),
		}).Error; err != nil {
			return err
		}
		snapshot.ConfirmedBy = &userID
		snapshot.ConfirmedAt = db.NewFlexibleTime(now)
		payload = reviewSnapshotPayload(snapshot)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// GetFinalReviewFrameFile 只向产物所有者返回抽出的审查帧
// （对应 get_final_review_frame_file）。
func (s *MediaRenderingService) GetFinalReviewFrameFile(
	ctx context.Context, artifactID, position, userID string,
) (map[string]any, string, error) {
	artifact, err := s.ownedArtifact(ctx, s.db, artifactID, userID)
	if err != nil {
		return nil, "", err
	}
	if artifact.Kind != "final" {
		return nil, "", &RenderArtifactNotFoundError{ArtifactID: artifactID}
	}
	if position != "first" && position != "middle" && position != "last" {
		return nil, "", &RenderArtifactNotFoundError{ArtifactID: artifactID}
	}
	path, err := s.finalReviewFramePath(artifact, position)
	if err != nil {
		return nil, "", err
	}
	info, statErr := os.Stat(path)
	if statErr != nil || info.IsDir() {
		return nil, "", &RenderArtifactNotFoundError{ArtifactID: artifactID}
	}
	return map[string]any{"mime_type": "image/jpeg", "filename": filepath.Base(path)}, path, nil
}

// finalArtifactRelativePath 计算审查帧的项目内相对路径。
func finalArtifactRelativePath(artifactID, position string) string {
	return fmt.Sprintf("render_artifacts/reviews/%s/%s.jpg", artifactID, position)
}

// finalReviewFramePath 把审查帧相对路径限定在项目目录内
// （对应 _final_review_frame_path）。
func (s *MediaRenderingService) finalReviewFramePath(
	artifact *repository.RenderArtifact, position string,
) (string, error) {
	root, err := s.projectRoot(artifact.ProjectName)
	if err != nil {
		return "", &RenderArtifactNotFoundError{ArtifactID: artifact.ID}
	}
	path, err := fs.SafeJoin(root, false, finalArtifactRelativePath(artifact.ID, position))
	if err != nil {
		return "", &RenderArtifactNotFoundError{ArtifactID: artifact.ID}
	}
	return path, nil
}

// --- 审查子进程 ---

// reviewProcessTimeout 对应 _run_review_process 的 120 秒上限。
const reviewProcessTimeout = 120 * time.Second

// runReviewProcess 执行 ffprobe/ffmpeg 审查命令并把失败收敛成
// FinalRenderReviewError（对应 _run_review_process）。
func runReviewProcess(ctx context.Context, args []string) ([]byte, []byte, error) {
	stdout, stderr, err := ffmpeg.RunCapture(ctx, reviewProcessTimeout, args)
	if err == nil {
		return stdout, stderr, nil
	}
	var toolErr *ffmpeg.RenderToolError
	if errors.As(err, &toolErr) {
		switch toolErr.Code {
		case ffmpeg.CodeProcessUnavailable:
			return nil, stderr, &FinalRenderReviewError{
				Code: "media_tools_unavailable", Message: "media inspection tools are unavailable",
			}
		case ffmpeg.CodeProcessTimeout:
			return nil, stderr, &FinalRenderReviewError{
				Code: "media_inspection_timeout", Message: "media inspection timed out",
			}
		case ffmpeg.CodeProcessFailed:
			detail := reviewStderrTail(stderr)
			if detail == "" {
				detail = "media inspection failed"
			}
			return nil, stderr, &FinalRenderReviewError{Code: "media_inspection_failed", Message: detail}
		}
	}
	return nil, stderr, err
}

// reviewStderrTail 取 stderr 解码后的末尾 2000 个字符。
func reviewStderrTail(stderr []byte) string {
	decoded := strings.ToValidUTF8(string(stderr), "�")
	runes := []rune(decoded)
	if len(runes) > 2000 {
		return string(runes[len(runes)-2000:])
	}
	return decoded
}

// probeFinalArtifact 读取成片的时长/尺寸/音轨信息（对应 _probe_final_artifact）。
func (s *MediaRenderingService) probeFinalArtifact(ctx context.Context, path string) (map[string]any, error) {
	tool, err := ffmpeg.ResolveTool("ffprobe")
	if err != nil {
		return nil, err
	}
	stdout, _, err := runReviewProcess(ctx, []string{
		tool, "-v", "error",
		"-show_entries", "format=duration:stream=codec_type,width,height",
		"-of", "json", path,
	})
	if err != nil {
		return nil, err
	}
	var probe struct {
		Format struct {
			Duration any `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     *int64 `json:"width"`
			Height    *int64 `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(stdout, &probe); err != nil {
		return nil, &FinalRenderReviewError{
			Code: "final_probe_invalid", Message: "ffprobe returned an invalid final video description",
		}
	}
	duration, err := probeFormatDuration(probe.Format.Duration)
	if err != nil {
		return nil, err
	}
	if duration <= 0 {
		return nil, &FinalRenderReviewError{Code: "final_probe_invalid", Message: "final video duration must be positive"}
	}
	var videoWidth, videoHeight any
	audioPresent := false
	videoFound := false
	for _, stream := range probe.Streams {
		switch stream.CodecType {
		case "video":
			if !videoFound {
				videoFound = true
				if stream.Width != nil {
					videoWidth = *stream.Width
				}
				if stream.Height != nil {
					videoHeight = *stream.Height
				}
			}
		case "audio":
			audioPresent = true
		}
	}
	if !videoFound {
		return nil, &FinalRenderReviewError{
			Code: "final_probe_invalid", Message: "ffprobe returned an invalid final video description",
		}
	}
	return map[string]any{
		"duration_seconds":     duration,
		"width":                videoWidth,
		"height":               videoHeight,
		"audio_stream_present": audioPresent,
	}, nil
}

// probeFormatDuration 解析 ffprobe JSON 输出里的 format.duration（字符串或数值）。
func probeFormatDuration(value any) (float64, error) {
	invalid := &FinalRenderReviewError{
		Code: "final_probe_invalid", Message: "ffprobe returned an invalid final video description",
	}
	switch v := value.(type) {
	case string:
		parsed, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, invalid
		}
		return parsed, nil
	case float64:
		return v, nil
	default:
		return 0, invalid
	}
}

// detectBlackFrames 用 blackdetect 滤镜扫黑场区间
// （对应 _detect_black_frames，解析在 media.ParseBlackFrameLog）。
func (s *MediaRenderingService) detectBlackFrames(ctx context.Context, path string) ([]map[string]any, error) {
	tool, err := ffmpeg.ResolveTool("ffmpeg")
	if err != nil {
		return nil, err
	}
	_, stderr, err := runReviewProcess(ctx, []string{
		tool, "-hide_banner", "-nostats", "-i", path,
		"-vf", "blackdetect=d=0.1:pix_th=0.10", "-an", "-f", "null", "-",
	})
	if err != nil {
		return nil, err
	}
	return media.ParseBlackFrameLog(string(stderr)), nil
}

// detectAudioMetrics 跑响度与峰值分析（对应 _detect_audio_metrics，
// 解析在 media.ParseAudioMetricsLog）。
func (s *MediaRenderingService) detectAudioMetrics(ctx context.Context, path string) (map[string]any, error) {
	tool, err := ffmpeg.ResolveTool("ffmpeg")
	if err != nil {
		return nil, err
	}
	_, stderr, err := runReviewProcess(ctx, []string{
		tool, "-hide_banner", "-nostats", "-i", path,
		"-af", "volumedetect,ebur128=framelog=verbose", "-vn", "-f", "null", "-",
	})
	if err != nil {
		return nil, err
	}
	return media.ParseAudioMetricsLog(string(stderr)), nil
}

// detectSilenceSegments 用 silencedetect 滤镜扫静音区间
// （对应 _detect_silence_segments，解析在 media.ParseSilencedetectLog）。
func (s *MediaRenderingService) detectSilenceSegments(ctx context.Context, path string) ([]map[string]any, error) {
	tool, err := ffmpeg.ResolveTool("ffmpeg")
	if err != nil {
		return nil, err
	}
	_, stderr, err := runReviewProcess(ctx, []string{
		tool, "-hide_banner", "-nostats", "-i", path,
		"-af", "silencedetect=noise=-50dB:d=0.5", "-vn", "-f", "null", "-",
	})
	if err != nil {
		return nil, err
	}
	return media.ParseSilencedetectLog(string(stderr)), nil
}

// extractReviewFrame 从成片里抽一帧存成 JPEG（对应 _extract_review_frame）。
func (s *MediaRenderingService) extractReviewFrame(
	ctx context.Context, artifact *repository.RenderArtifact, sourcePath, position string, timestampSeconds float64,
) (map[string]any, error) {
	tool, err := ffmpeg.ResolveTool("ffmpeg")
	if err != nil {
		return nil, err
	}
	outputPath, err := s.finalReviewFramePath(artifact, position)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return nil, err
	}
	if _, _, err := runReviewProcess(ctx, []string{
		tool, "-hide_banner", "-loglevel", "error", "-y",
		"-ss", fmt.Sprintf("%.3f", timestampSeconds), "-i", sourcePath,
		"-frames:v", "1", "-q:v", "2", outputPath,
	}); err != nil {
		return nil, err
	}
	info, statErr := os.Stat(outputPath)
	if statErr != nil || info.Size() <= 0 {
		return nil, &FinalRenderReviewError{
			Code: "review_frame_missing", Message: "could not extract " + position + " frame",
		}
	}
	return map[string]any{
		"available":         true,
		"position":          position,
		"timestamp_seconds": timestampSeconds,
		"relative_path":     finalArtifactRelativePath(artifact.ID, position),
	}, nil
}

// --- 字幕导出 ---

// ExportSubtitles 序列化当前所属 revision 已校验的字幕 cue 供下载
// （对应 export_subtitles）。
func (s *MediaRenderingService) ExportSubtitles(
	ctx context.Context, planID, format, userID string,
) (map[string]any, error) {
	if format != "srt" && format != "vtt" {
		return nil, &SubtitleExportError{Message: "subtitle format must be srt or vtt"}
	}
	planRepo := repository.NewAssemblyPlanRepo(s.db)
	plan, err := ownedRenderPlan(ctx, planRepo, planID, userID)
	if err != nil {
		return nil, err
	}
	revision, err := currentRenderRevision(ctx, planRepo, plan)
	if err != nil {
		return nil, err
	}
	var subtitleDocument any
	if err := json.Unmarshal([]byte(revision.SubtitleJSON), &subtitleDocument); err != nil {
		return nil, &SubtitleExportError{Message: "subtitle configuration is invalid"}
	}
	cues, err := subtitleCuesFromRevision(subtitleDocument)
	if err != nil {
		return nil, err
	}
	var content string
	if format == "srt" {
		content, err = media.GenerateSRT(cues)
	} else {
		content, err = media.GenerateVTT(cues)
	}
	if err != nil {
		var validation *media.SubtitleValidationError
		if errors.As(err, &validation) {
			return nil, &SubtitleExportError{Message: "subtitle configuration is invalid"}
		}
		return nil, err
	}
	mediaType := "application/x-subrip; charset=utf-8"
	if format == "vtt" {
		mediaType = "text/vtt; charset=utf-8"
	}
	return map[string]any{
		"content":    content,
		"media_type": mediaType,
		"filename":   "subtitles." + format,
	}, nil
}

// subtitleCuesFromRevision 从 revision 的字幕文档里取 cue 列表并校验形状
// （对应 _subtitle_cues_from_revision）。
func subtitleCuesFromRevision(value any) ([]any, error) {
	var rawCues any = value
	if document, ok := value.(map[string]any); ok {
		if cues, present := document["cues"]; present {
			rawCues = cues
		} else {
			rawCues = []any{}
		}
	}
	list, ok := rawCues.([]any)
	if !ok {
		return nil, &SubtitleExportError{Message: "subtitle configuration must contain a cues list"}
	}
	for _, cue := range list {
		if _, isMap := cue.(map[string]any); !isMap {
			return nil, &SubtitleExportError{Message: "subtitle cues must be objects"}
		}
	}
	return list, nil
}
