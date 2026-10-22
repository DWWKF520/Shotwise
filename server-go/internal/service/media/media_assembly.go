// 媒体装配计划的持久化服务（对应 server/services/media_assembly.py）：
// 计划/不可变 revision 的创建与读取，以及在服务边界归一 source snapshot。
package media

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
	"gorm.io/gorm"
)

// AssemblyPlanNotFoundError 表示计划不存在或不属于请求用户；Error 文本即 plan_id
// （与 Python AssemblyPlanNotFoundError(LookupError) 的 str(exc) 口径一致）。
type AssemblyPlanNotFoundError struct{ PlanID string }

func (e *AssemblyPlanNotFoundError) Error() string { return e.PlanID }

// AssemblyPlanConflictError 表示操作与计划当前状态冲突，Code 是稳定的冲突码。
type AssemblyPlanConflictError struct {
	Code   string
	Status string
}

func (e *AssemblyPlanConflictError) Error() string { return e.Code }

// MediaAssemblyService 承载装配计划与 revision 的读写。
type MediaAssemblyService struct {
	db   *gorm.DB
	repo *repository.AssemblyPlanRepo
}

// NewMediaAssemblyService 构造服务。
func NewMediaAssemblyService(gdb *gorm.DB) *MediaAssemblyService {
	return &MediaAssemblyService{db: gdb, repo: repository.NewAssemblyPlanRepo(gdb)}
}

// CreatePlanParams 是 create_plan 的入参（**document 摊平为显式字段）。
type CreatePlanParams struct {
	UserID         string
	ProjectName    string
	Name           string
	Scope          string
	EpisodeNumber  *int64
	SourceManifest map[string]any
	Timeline       []map[string]any
	Audio          map[string]any
	Subtitle       map[string]any
	Packaging      map[string]any
	OutputProfile  map[string]any
}

// CreateRevisionParams 是 create_revision 的入参。
type CreateRevisionParams struct {
	UserID             string
	SourceManifest     map[string]any
	Timeline           []map[string]any
	Audio              map[string]any
	Subtitle           map[string]any
	Packaging          map[string]any
	OutputProfile      map[string]any
	ValidationMetadata map[string]any
	// ExpectedRevision 非 nil 时做乐观并发校验。
	ExpectedRevision *int64
}

// CreatePlan 校验并入队一份新计划及其首个 revision；校验失败返回 *domain.AssemblyPlanValidationError。
func (s *MediaAssemblyService) CreatePlan(ctx context.Context, p CreatePlanParams) (map[string]any, error) {
	snapshot := domain.BuildSourceSnapshot(p.ProjectName, p.SourceManifest)
	audio, subtitle, packaging := orEmptyMap(p.Audio), orEmptyMap(p.Subtitle), orEmptyMap(p.Packaging)
	outputProfile := orDefaultProfile(p.OutputProfile)
	validation, err := domain.ValidatePlanDocument(snapshot, p.Timeline, audio, subtitle, packaging, outputProfile)
	if err != nil {
		return nil, err
	}
	fingerprint, err := domain.SourceFingerprint(snapshot)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	plan := &repository.AssemblyPlan{
		ID:                       uuid.NewString(),
		UserID:                   p.UserID,
		ProjectName:              p.ProjectName,
		Scope:                    p.Scope,
		EpisodeNumber:            p.EpisodeNumber,
		Name:                     p.Name,
		Status:                   "draft",
		CurrentRevisionNumber:    1,
		CurrentSourceFingerprint: fingerprint,
		CreatedAt:                db.NewFlexibleTime(now),
		UpdatedAt:                db.NewFlexibleTime(now),
	}
	revision, err := newAssemblyRevision(
		plan.ID, 1, p.UserID, now, snapshot, p.Timeline, audio, subtitle, packaging, outputProfile, fingerprint, validation,
	)
	if err != nil {
		return nil, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.repo.WithTx(tx).AddPlan(ctx, plan, revision)
	})
	if err != nil {
		return nil, err
	}
	return assemblyPlanPayload(plan, revision), nil
}

// GetPlan 读取当前用户拥有的计划及其 current revision。
func (s *MediaAssemblyService) GetPlan(ctx context.Context, planID, userID string) (map[string]any, error) {
	plan, err := s.repo.GetOwned(ctx, planID, userID)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, &AssemblyPlanNotFoundError{PlanID: planID}
	}
	revision, err := s.repo.GetCurrentRevision(ctx, plan)
	if err != nil {
		return nil, err
	}
	if revision == nil {
		return nil, &AssemblyPlanConflictError{Code: "current_revision_missing", Status: plan.Status}
	}
	return assemblyPlanPayload(plan, revision), nil
}

// ListPlans 列出项目下的装配计划（不含 current_revision，与 Python 侧一致）。
func (s *MediaAssemblyService) ListPlans(ctx context.Context, userID, projectName string) ([]map[string]any, error) {
	plans, err := s.repo.ListByProject(ctx, userID, projectName)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(plans))
	for i := range plans {
		items = append(items, assemblyPlanPayload(&plans[i], nil))
	}
	return items, nil
}

// TransitionPlan 校验状态机后推进计划状态；preview_pending / render_pending / rendering
// 不能由本入口直接落成（前者要靠预览任务、后者要靠渲染确认）。
func (s *MediaAssemblyService) TransitionPlan(
	ctx context.Context, planID, userID, targetStatus string,
) (map[string]any, error) {
	plan, err := s.repo.GetOwned(ctx, planID, userID)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, &AssemblyPlanNotFoundError{PlanID: planID}
	}
	if err := domain.AssertTransition(plan.Status, targetStatus); err != nil {
		return nil, &AssemblyPlanConflictError{Code: "invalid_status_transition", Status: plan.Status}
	}
	if targetStatus == "preview_ready" {
		return nil, &AssemblyPlanConflictError{Code: "preview_task_required", Status: plan.Status}
	}
	if targetStatus == "render_pending" || targetStatus == "rendering" {
		return nil, &AssemblyPlanConflictError{Code: "render_confirmation_required", Status: plan.Status}
	}
	if err := s.repo.UpdateStatus(ctx, planID, userID, targetStatus, time.Now().UTC()); err != nil {
		return nil, err
	}
	return s.GetPlan(ctx, planID, userID)
}

// MarkPreviewReady 把某个 revision 的预览就绪状态落库（供渲染流水线回调）。
func (s *MediaAssemblyService) MarkPreviewReady(
	ctx context.Context, planID, userID string, revisionNumber int64,
) (map[string]any, error) {
	plan, err := s.repo.GetOwned(ctx, planID, userID)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, &AssemblyPlanNotFoundError{PlanID: planID}
	}
	if plan.CurrentRevisionNumber != revisionNumber {
		return nil, &AssemblyPlanConflictError{Code: "revision_conflict", Status: plan.Status}
	}
	if plan.Status != "preview_pending" {
		return nil, &AssemblyPlanConflictError{Code: "preview_pending_required", Status: plan.Status}
	}
	if err := s.repo.SetPreviewReady(ctx, planID, userID, revisionNumber, time.Now().UTC()); err != nil {
		return nil, err
	}
	return s.GetPlan(ctx, planID, userID)
}

// ConfirmPreview 记录当前用户对某就绪预览 revision 的批准。
func (s *MediaAssemblyService) ConfirmPreview(
	ctx context.Context, planID, userID string, revisionNumber int64,
) (map[string]any, error) {
	plan, err := s.repo.GetOwned(ctx, planID, userID)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, &AssemblyPlanNotFoundError{PlanID: planID}
	}
	if plan.CurrentRevisionNumber != revisionNumber ||
		plan.PreviewRevisionNumber == nil || *plan.PreviewRevisionNumber != revisionNumber {
		return nil, &AssemblyPlanConflictError{Code: "revision_conflict", Status: plan.Status}
	}
	if plan.Status != "preview_ready" {
		return nil, &AssemblyPlanConflictError{Code: "preview_ready_required", Status: plan.Status}
	}
	ok, err := s.repo.ConfirmPreview(ctx, planID, userID, revisionNumber, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &AssemblyPlanConflictError{Code: "preview_confirmation_conflict", Status: plan.Status}
	}
	return s.GetPlan(ctx, planID, userID)
}

// ConfirmRender 记录渲染前的用户批准并把状态推进为 render_pending。
func (s *MediaAssemblyService) ConfirmRender(
	ctx context.Context, planID, userID string, revisionNumber int64,
) (map[string]any, error) {
	plan, err := s.repo.GetOwned(ctx, planID, userID)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, &AssemblyPlanNotFoundError{PlanID: planID}
	}
	if plan.Status != "preview_ready" ||
		plan.PreviewRevisionNumber == nil || *plan.PreviewRevisionNumber != revisionNumber {
		return nil, &AssemblyPlanConflictError{Code: "preview_revision_required", Status: plan.Status}
	}
	if plan.CurrentRevisionNumber != revisionNumber {
		return nil, &AssemblyPlanConflictError{Code: "revision_conflict", Status: plan.Status}
	}
	if plan.PreviewConfirmedBy == nil || !plan.PreviewConfirmedAt.Valid {
		return nil, &AssemblyPlanConflictError{Code: "preview_confirmation_required", Status: plan.Status}
	}
	ok, err := s.repo.ConfirmRender(ctx, planID, userID, revisionNumber, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &AssemblyPlanConflictError{Code: "render_confirmation_conflict", Status: plan.Status}
	}
	return s.GetPlan(ctx, planID, userID)
}

// CheckStale 用当前源快照指纹与计划记录比对；不一致则把计划翻成 stale。
func (s *MediaAssemblyService) CheckStale(
	ctx context.Context, planID, userID string, currentSourceSnapshot map[string]any,
) (map[string]any, error) {
	plan, err := s.repo.GetOwned(ctx, planID, userID)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, &AssemblyPlanNotFoundError{PlanID: planID}
	}
	snapshot := domain.BuildSourceSnapshot(plan.ProjectName, currentSourceSnapshot)
	fingerprint, err := domain.SourceFingerprint(snapshot)
	if err != nil {
		return nil, err
	}
	stale := fingerprint != plan.CurrentSourceFingerprint
	if stale && plan.Status != "stale" {
		if err := s.repo.MarkStale(ctx, planID, time.Now().UTC()); err != nil {
			return nil, err
		}
	}
	payload, err := s.GetPlan(ctx, planID, userID)
	if err != nil {
		return nil, err
	}
	payload["stale"] = stale
	payload["current_source_fingerprint"] = fingerprint
	return payload, nil
}

// CreateRevision 在乐观并发校验下写入一个不可变 revision，并推进计划指针。
func (s *MediaAssemblyService) CreateRevision(ctx context.Context, planID string, p CreateRevisionParams) (map[string]any, error) {
	plan, err := s.repo.GetOwned(ctx, planID, p.UserID)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, &AssemblyPlanNotFoundError{PlanID: planID}
	}
	snapshot := domain.BuildSourceSnapshot(plan.ProjectName, p.SourceManifest)
	audio, subtitle, packaging := orEmptyMap(p.Audio), orEmptyMap(p.Subtitle), orEmptyMap(p.Packaging)
	outputProfile := orDefaultProfile(p.OutputProfile)
	if p.ExpectedRevision != nil && plan.CurrentRevisionNumber != *p.ExpectedRevision {
		return nil, &AssemblyPlanConflictError{Code: "revision_conflict", Status: plan.Status}
	}
	if domain.IsRunningStatus(plan.Status) {
		return nil, &AssemblyPlanConflictError{Code: "plan_busy", Status: plan.Status}
	}
	validation, err := domain.ValidatePlanDocument(snapshot, p.Timeline, audio, subtitle, packaging, outputProfile)
	if err != nil {
		return nil, err
	}
	if p.ValidationMetadata != nil {
		validation = p.ValidationMetadata
	}
	fingerprint, err := domain.SourceFingerprint(snapshot)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	expected := plan.CurrentRevisionNumber
	revision, err := newAssemblyRevision(
		plan.ID, expected+1, p.UserID, now, snapshot, p.Timeline, audio, subtitle, packaging, outputProfile, fingerprint, validation,
	)
	if err != nil {
		return nil, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := s.repo.WithTx(tx)
		if err := repo.AddRevision(ctx, revision); err != nil {
			return err
		}
		ok, err := repo.AdvanceRevision(ctx, plan.ID, expected, fingerprint, now)
		if err != nil {
			return err
		}
		if !ok {
			return &AssemblyPlanConflictError{Code: "revision_conflict", Status: plan.Status}
		}
		return repo.ClearPreviewConfirmation(ctx, plan.ID)
	})
	if err != nil {
		return nil, err
	}
	// 推进内存中的计划视图，供 payload 渲染。
	plan.CurrentRevisionNumber = expected + 1
	plan.Status = "draft"
	plan.CurrentSourceFingerprint = fingerprint
	plan.PreviewRevisionNumber = nil
	plan.PreviewReadyAt = db.FlexibleTime{}
	plan.RenderConfirmedBy = nil
	plan.RenderConfirmedAt = db.FlexibleTime{}
	plan.PreviewConfirmedBy = nil
	plan.PreviewConfirmedAt = db.FlexibleTime{}
	plan.UpdatedAt = db.NewFlexibleTime(now)
	return assemblyPlanPayload(plan, revision), nil
}

func newAssemblyRevision(
	planID string, version int64, userID string, createdAt time.Time,
	snapshot map[string]any, timeline []map[string]any,
	audio, subtitle, packaging, outputProfile map[string]any,
	fingerprint string, validation map[string]any,
) (*repository.AssemblyPlanRevision, error) {
	sourceSnapshotJSON, err := service.CanonicalString(snapshot)
	if err != nil {
		return nil, err
	}
	timelineJSON, err := service.CanonicalString(timeline)
	if err != nil {
		return nil, err
	}
	audioJSON, err := service.CanonicalString(audio)
	if err != nil {
		return nil, err
	}
	subtitleJSON, err := service.CanonicalString(subtitle)
	if err != nil {
		return nil, err
	}
	packagingJSON, err := service.CanonicalString(packaging)
	if err != nil {
		return nil, err
	}
	outputProfileJSON, err := service.CanonicalString(outputProfile)
	if err != nil {
		return nil, err
	}
	validationJSON, err := service.CanonicalString(validation)
	if err != nil {
		return nil, err
	}
	return &repository.AssemblyPlanRevision{
		ID:                 uuid.NewString(),
		PlanID:             planID,
		VersionNumber:      version,
		SourceFingerprint:  fingerprint,
		SourceSnapshotJSON: sourceSnapshotJSON,
		TimelineJSON:       timelineJSON,
		AudioJSON:          audioJSON,
		SubtitleJSON:       subtitleJSON,
		PackagingJSON:      packagingJSON,
		OutputProfileJSON:  outputProfileJSON,
		ValidationJSON:     validationJSON,
		CreatedBy:          userID,
		CreatedAt:          db.NewFlexibleTime(createdAt),
	}, nil
}

// revisionPayload 摊平一个 revision 供上层消费（JSON 文本列解析回对象）。
func revisionPayload(revision *repository.AssemblyPlanRevision) map[string]any {
	return map[string]any{
		"id":                 revision.ID,
		"plan_id":            revision.PlanID,
		"version_number":     revision.VersionNumber,
		"source_fingerprint": revision.SourceFingerprint,
		"source_snapshot":    service.ParseJSONValue(revision.SourceSnapshotJSON),
		"timeline":           service.ParseJSONValue(revision.TimelineJSON),
		"audio":              service.ParseJSONValue(revision.AudioJSON),
		"subtitle":           service.ParseJSONValue(revision.SubtitleJSON),
		"packaging":          service.ParseJSONValue(revision.PackagingJSON),
		"output_profile":     service.ParseJSONValue(revision.OutputProfileJSON),
		"validation":         service.ParseJSONValue(revision.ValidationJSON),
		"created_by":         revision.CreatedBy,
		"created_at":         service.IsoformatTime(revision.CreatedAt.Time),
	}
}

// assemblyPlanPayload 渲染计划信封；revision 非 nil 时附上 current_revision。
func assemblyPlanPayload(plan *repository.AssemblyPlan, revision *repository.AssemblyPlanRevision) map[string]any {
	payload := map[string]any{
		"id":                         plan.ID,
		"user_id":                    plan.UserID,
		"project_name":               plan.ProjectName,
		"scope":                      plan.Scope,
		"episode_number":             plan.EpisodeNumber,
		"name":                       plan.Name,
		"status":                     plan.Status,
		"current_revision_number":    plan.CurrentRevisionNumber,
		"current_source_fingerprint": plan.CurrentSourceFingerprint,
		"preview_revision_number":    plan.PreviewRevisionNumber,
		"preview_ready_at":           service.NullableISO(plan.PreviewReadyAt),
		"preview_confirmed_by":       plan.PreviewConfirmedBy,
		"preview_confirmed_at":       service.NullableISO(plan.PreviewConfirmedAt),
		"render_confirmed_by":        plan.RenderConfirmedBy,
		"render_confirmed_at":        service.NullableISO(plan.RenderConfirmedAt),
		"created_at":                 service.IsoformatTime(plan.CreatedAt.Time),
		"updated_at":                 service.IsoformatTime(plan.UpdatedAt.Time),
	}
	if revision != nil {
		payload["current_revision"] = revisionPayload(revision)
	}
	return payload
}





func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func orDefaultProfile(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{"format": "mp4"}
	}
	return m
}
