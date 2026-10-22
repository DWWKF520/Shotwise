package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/infra/ffmpeg"
	"github.com/shotwise/shotwise/internal/repository"
	"gorm.io/gorm"
)

// newRenderingTestEnv 构造渲染服务测试环境：迁移好的库 + 项目存储，并返回底层句柄供测试直接改行。
func newRenderingTestEnv(t *testing.T) (*MediaRenderingService, *MediaAssemblyService, *ProjectStore, *gorm.DB) {
	t.Helper()
	gdb := newAssemblyPlanTestDB(t)
	store := NewProjectStore(t.TempDir())
	return NewMediaRenderingService(gdb, store), NewMediaAssemblyService(gdb), store, gdb
}

// confirmPlanForPreview 把计划推进到 confirmed（预览任务的门槛状态）。
func confirmPlanForPreview(t *testing.T, svc *MediaAssemblyService, planID string) {
	t.Helper()
	if _, err := svc.TransitionPlan(context.Background(), planID, repository.DefaultUserID, "confirmed"); err != nil {
		t.Fatalf("transition plan to confirmed: %v", err)
	}
}

// craftRenderPendingPlan 直接把计划写成目标状态并补齐预览/渲染确认字段，
// 模拟用户走完预览确认与渲染确认后的落库形态（绕过生命周期校验，仅用于门槛测试）。
func craftRenderPendingPlan(t *testing.T, gdb *gorm.DB, planID, status string) {
	t.Helper()
	now := "2026-01-02T00:00:00+00:00"
	err := gdb.Exec(
		`UPDATE assembly_plans SET status = ?, preview_revision_number = 1, preview_confirmed_by = ?, preview_confirmed_at = ?,
		 render_confirmed_by = ?, render_confirmed_at = ?, updated_at = ? WHERE id = ?`,
		status, repository.DefaultUserID, now, repository.DefaultUserID, now, now, planID,
	).Error
	if err != nil {
		t.Fatalf("craft render_pending plan: %v", err)
	}
}

// insertRenderJob 插入任务父行，满足 render_artifacts 的外键。
func insertRenderJob(t *testing.T, gdb *gorm.DB, jobID, planID, userID, kind string) {
	t.Helper()
	now := db.NewFlexibleTime(time.Now().UTC())
	job := &repository.RenderJob{
		ID: jobID, PlanID: planID, ProjectName: "p1", UserID: userID,
		RevisionNumber: 1, Kind: kind, Status: "succeeded", Attempt: 1, MaxAttempts: 3,
		InputFingerprint: "fp-job", ProgressStage: "completed", CreatedAt: now, UpdatedAt: now,
	}
	if err := gdb.Create(job).Error; err != nil {
		t.Fatalf("insert render job %s: %v", jobID, err)
	}
}

// setJobState 直接改任务行，模拟 worker 侧的执行结果。
func setJobState(t *testing.T, gdb *gorm.DB, jobID string, updates map[string]any) {
	t.Helper()
	if err := gdb.Model(&repository.RenderJob{}).Where("id = ?", jobID).Updates(updates).Error; err != nil {
		t.Fatalf("set job state: %v", err)
	}
}

func isRenderConflictCode(err error, code string) bool {
	var (
		jobConflict *RenderJobConflictError
		revConflict *RenderRevisionConflictError
	)
	if errors.As(err, &jobConflict) {
		return jobConflict.Code == code
	}
	if errors.As(err, &revConflict) {
		return revConflict.Code == code
	}
	return false
}

func TestCreatePreviewJobGateAndConflicts(t *testing.T) {
	renderSvc, assemblySvc, _, _ := newRenderingTestEnv(t)
	ctx := context.Background()
	user := repository.DefaultUserID

	plan := createAssemblyPlan(t, assemblySvc)
	planID, _ := plan["id"].(string)
	defaultAttempts := int64(3)

	// 计划未确认 → 拒绝。
	_, err := renderSvc.CreatePreviewJob(ctx, planID, user, nil, defaultAttempts)
	if !isRenderConflictCode(err, "preview_requires_confirmed_plan") {
		t.Fatalf("draft plan err = %v", err)
	}

	confirmPlanForPreview(t, assemblySvc, planID)

	payload, err := renderSvc.CreatePreviewJob(ctx, planID, user, nil, defaultAttempts)
	if err != nil {
		t.Fatalf("create preview job: %v", err)
	}
	if payload["kind"] != "preview" || payload["status"] != "queued" || payload["max_attempts"] != int64(3) {
		t.Fatalf("payload = %v", payload)
	}
	if payload["artifact"] != nil {
		t.Fatalf("fresh job artifact = %v", payload["artifact"])
	}

	// 任务入队后计划推进为 preview_pending。
	got, err := assemblySvc.GetPlan(ctx, planID, user)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if got["status"] != "preview_pending" {
		t.Fatalf("plan status = %v", got["status"])
	}

	// 活动任务去重：先经 preview_pending → failed → confirmed 把计划转回门槛状态。
	if _, err := assemblySvc.TransitionPlan(context.Background(), planID, user, "failed"); err != nil {
		t.Fatalf("transition plan to failed: %v", err)
	}
	if _, err := assemblySvc.TransitionPlan(context.Background(), planID, user, "confirmed"); err != nil {
		t.Fatalf("transition plan back to confirmed: %v", err)
	}
	_, err = renderSvc.CreatePreviewJob(ctx, planID, user, nil, defaultAttempts)
	if !isRenderConflictCode(err, "preview_job_already_running") {
		t.Fatalf("active duplicate err = %v", err)
	}

	// revision 不匹配。
	rev2 := int64(2)
	_, err = renderSvc.CreatePreviewJob(ctx, planID, user, &rev2, defaultAttempts)
	if !isRenderConflictCode(err, "revision_conflict") {
		t.Fatalf("revision mismatch err = %v", err)
	}

	// max_attempts 越界。
	if _, err := renderSvc.CreatePreviewJob(ctx, planID, user, nil, 0); !isRenderConflictCode(err, "invalid_max_attempts") {
		t.Fatalf("max attempts err = %v", err)
	}

	// 计划不存在 → 404 口径。
	var notFound *AssemblyPlanNotFoundError
	if _, err := renderSvc.CreatePreviewJob(ctx, "none", user, nil, defaultAttempts); !errors.As(err, &notFound) {
		t.Fatalf("missing plan err = %v", err)
	}
}

func TestCreateFinalJobGate(t *testing.T) {
	renderSvc, assemblySvc, _, gdb := newRenderingTestEnv(t)
	ctx := context.Background()
	user := repository.DefaultUserID

	plan := createAssemblyPlan(t, assemblySvc)
	planID, _ := plan["id"].(string)
	defaultAttempts := int64(3)

	// 计划停在 confirmed：预览 revision 缺失。
	confirmPlanForPreview(t, assemblySvc, planID)
	_, err := renderSvc.CreateFinalJob(ctx, planID, user, nil, defaultAttempts)
	if !isRenderConflictCode(err, "preview_revision_required") {
		t.Fatalf("confirmed plan err = %v", err)
	}

	// 状态不是 render_pending。
	craftRenderPendingPlan(t, gdb, planID, "preview_ready")
	_, err = renderSvc.CreateFinalJob(ctx, planID, user, nil, defaultAttempts)
	if !isRenderConflictCode(err, "final_render_pending_required") {
		t.Fatalf("preview_ready err = %v", err)
	}

	craftRenderPendingPlan(t, gdb, planID, "render_pending")
	payload, err := renderSvc.CreateFinalJob(ctx, planID, user, nil, defaultAttempts)
	if err != nil {
		t.Fatalf("create final job: %v", err)
	}
	if payload["kind"] != "final" || payload["status"] != "queued" {
		t.Fatalf("payload = %v", payload)
	}

	// 活动任务去重。
	_, err = renderSvc.CreateFinalJob(ctx, planID, user, nil, defaultAttempts)
	if !isRenderConflictCode(err, "final_job_already_running") {
		t.Fatalf("active duplicate err = %v", err)
	}

	// 渲染确认被清掉 → 拒绝。
	confirmPlanForPreview(t, assemblySvc, planID)
	if err := gdb.Exec("UPDATE assembly_plans SET status = 'render_pending', render_confirmed_by = NULL WHERE id = ?", planID).Error; err != nil {
		t.Fatalf("clear render confirmation: %v", err)
	}
	_, err = renderSvc.CreateFinalJob(ctx, planID, user, nil, defaultAttempts)
	if !isRenderConflictCode(err, "render_confirmation_required") {
		t.Fatalf("missing render confirmation err = %v", err)
	}
}

func TestGetAndListRenderJobs(t *testing.T) {
	renderSvc, assemblySvc, _, gdb := newRenderingTestEnv(t)
	ctx := context.Background()
	user := repository.DefaultUserID

	plan := createAssemblyPlan(t, assemblySvc)
	planID, _ := plan["id"].(string)
	confirmPlanForPreview(t, assemblySvc, planID)

	job, err := renderSvc.CreatePreviewJob(ctx, planID, user, nil, 3)
	if err != nil {
		t.Fatalf("create preview job: %v", err)
	}
	jobID, _ := job["id"].(string)

	got, err := renderSvc.GetRenderJob(ctx, jobID, user)
	if err != nil {
		t.Fatalf("get render job: %v", err)
	}
	if got["artifact"] != nil {
		t.Fatalf("artifact = %v", got["artifact"])
	}

	// 挂一个产物后 payload 带上 artifact。
	if err := gdb.Create(&repository.RenderArtifact{
		ID: "artifact-1", RenderJobID: jobID, PlanID: planID, ProjectName: "p1", UserID: user,
		RevisionNumber: 1, Kind: "preview", RelativePath: "render_artifacts/preview/revision-1/preview-x.mp4",
		MimeType: "video/mp4", SizeBytes: 10, DurationSeconds: 2, Fingerprint: "fp-1",
	}).Error; err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
	got, err = renderSvc.GetRenderJob(ctx, jobID, user)
	if err != nil {
		t.Fatalf("get render job: %v", err)
	}
	artifact, _ := got["artifact"].(map[string]any)
	if artifact == nil || artifact["id"] != "artifact-1" {
		t.Fatalf("artifact = %v", got["artifact"])
	}

	preview := "preview"
	final := "final"
	items, err := renderSvc.ListRenderJobs(ctx, planID, user, &preview)
	if err != nil || len(items) != 1 {
		t.Fatalf("list preview jobs = %v err = %v", items, err)
	}
	items, err = renderSvc.ListRenderJobs(ctx, planID, user, &final)
	if err != nil || len(items) != 0 {
		t.Fatalf("list final jobs = %v err = %v", items, err)
	}
	items, err = renderSvc.ListRenderJobs(ctx, planID, user, nil)
	if err != nil || len(items) != 1 {
		t.Fatalf("list all jobs = %v err = %v", items, err)
	}

	var jobNotFound *RenderJobNotFoundError
	if _, err := renderSvc.GetRenderJob(ctx, "none", user); !errors.As(err, &jobNotFound) {
		t.Fatalf("missing job err = %v", err)
	}
	var planNotFound *AssemblyPlanNotFoundError
	if _, err := renderSvc.ListRenderJobs(ctx, "none", user, nil); !errors.As(err, &planNotFound) {
		t.Fatalf("missing plan err = %v", err)
	}
}

func TestRetryPreviewJob(t *testing.T) {
	renderSvc, assemblySvc, _, gdb := newRenderingTestEnv(t)
	ctx := context.Background()
	user := repository.DefaultUserID

	plan := createAssemblyPlan(t, assemblySvc)
	planID, _ := plan["id"].(string)
	confirmPlanForPreview(t, assemblySvc, planID)

	job, err := renderSvc.CreatePreviewJob(ctx, planID, user, nil, 3)
	if err != nil {
		t.Fatalf("create preview job: %v", err)
	}
	jobID, _ := job["id"].(string)

	// 非终态不可重试。
	if _, err := renderSvc.RetryPreviewJob(ctx, jobID, user); !isRenderConflictCode(err, "retry_requires_terminal_job") {
		t.Fatalf("queued retry err = %v", err)
	}

	// 计划停在 preview_pending：失败任务直接重排。
	setJobState(t, gdb, jobID, map[string]any{"status": "failed", "attempt": int64(1), "error_code": "render_failed"})
	payload, err := renderSvc.RetryPreviewJob(ctx, jobID, user)
	if err != nil {
		t.Fatalf("retry failed job: %v", err)
	}
	retryErrorCode, _ := payload["error_code"].(*string)
	if payload["status"] != "queued" || retryErrorCode != nil {
		t.Fatalf("retried payload = %v", payload)
	}

	// 重试次数用尽。
	setJobState(t, gdb, jobID, map[string]any{"status": "failed", "attempt": int64(3)})
	if _, err := renderSvc.RetryPreviewJob(ctx, jobID, user); !isRenderConflictCode(err, "retry_limit_reached") {
		t.Fatalf("retry limit err = %v", err)
	}

	// 计划状态不允许。
	if err := gdb.Model(&repository.AssemblyPlan{}).Where("id = ?", planID).Update("status", "completed").Error; err != nil {
		t.Fatalf("complete plan: %v", err)
	}
	setJobState(t, gdb, jobID, map[string]any{"status": "failed", "attempt": int64(1)})
	if _, err := renderSvc.RetryPreviewJob(ctx, jobID, user); !isRenderConflictCode(err, "preview_retry_not_allowed") {
		t.Fatalf("completed plan retry err = %v", err)
	}

	// 计划 failed：重试走 failed → confirmed → preview_pending 的双跳。
	if err := gdb.Model(&repository.AssemblyPlan{}).Where("id = ?", planID).Update("status", "failed").Error; err != nil {
		t.Fatalf("fail plan: %v", err)
	}
	if _, err := renderSvc.RetryPreviewJob(ctx, jobID, user); err != nil {
		t.Fatalf("retry on failed plan: %v", err)
	}
	got, err := assemblySvc.GetPlan(ctx, planID, user)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if got["status"] != "preview_pending" {
		t.Fatalf("plan status after retry = %v", got["status"])
	}
}

func TestRetryFinalJob(t *testing.T) {
	renderSvc, assemblySvc, _, gdb := newRenderingTestEnv(t)
	ctx := context.Background()
	user := repository.DefaultUserID

	plan := createAssemblyPlan(t, assemblySvc)
	planID, _ := plan["id"].(string)
	confirmPlanForPreview(t, assemblySvc, planID)

	// 预览任务不能走成片重试。
	previewJob, err := renderSvc.CreatePreviewJob(ctx, planID, user, nil, 3)
	if err != nil {
		t.Fatalf("create preview job: %v", err)
	}
	previewJobID, _ := previewJob["id"].(string)
	if _, err := renderSvc.RetryFinalJob(ctx, previewJobID, user); !isRenderConflictCode(err, "final_retry_requires_final_job") {
		t.Fatalf("preview final retry err = %v", err)
	}

	craftRenderPendingPlan(t, gdb, planID, "render_pending")
	finalJob, err := renderSvc.CreateFinalJob(ctx, planID, user, nil, 3)
	if err != nil {
		t.Fatalf("create final job: %v", err)
	}
	finalJobID, _ := finalJob["id"].(string)

	setJobState(t, gdb, finalJobID, map[string]any{"status": "failed", "attempt": int64(1)})
	payload, err := renderSvc.RetryFinalJob(ctx, finalJobID, user)
	if err != nil {
		t.Fatalf("retry final job: %v", err)
	}
	if payload["status"] != "queued" || payload["kind"] != "final" {
		t.Fatalf("retried payload = %v", payload)
	}
	got, err := assemblySvc.GetPlan(ctx, planID, user)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if got["status"] != "render_pending" {
		t.Fatalf("plan status after final retry = %v", got["status"])
	}

	// 计划状态不在允许集合 → 拒绝。

	if err := gdb.Model(&repository.AssemblyPlan{}).Where("id = ?", planID).Update("status", "completed").Error; err != nil {
		t.Fatalf("complete plan: %v", err)
	}
	setJobState(t, gdb, finalJobID, map[string]any{"status": "failed", "attempt": int64(1)})
	if _, err := renderSvc.RetryFinalJob(ctx, finalJobID, user); !isRenderConflictCode(err, "final_render_status_conflict") {
		t.Fatalf("completed plan final retry err = %v", err)
	}
}

func TestCancelRenderJob(t *testing.T) {
	renderSvc, assemblySvc, _, gdb := newRenderingTestEnv(t)
	ctx := context.Background()
	user := repository.DefaultUserID

	plan := createAssemblyPlan(t, assemblySvc)
	planID, _ := plan["id"].(string)
	confirmPlanForPreview(t, assemblySvc, planID)

	job, err := renderSvc.CreatePreviewJob(ctx, planID, user, nil, 3)
	if err != nil {
		t.Fatalf("create preview job: %v", err)
	}
	jobID, _ := job["id"].(string)

	// 排队中的任务没有 worker 认领，取消就地落成 cancelled。
	payload, err := renderSvc.CancelRenderJob(ctx, jobID, user)
	if err != nil {
		t.Fatalf("cancel queued job: %v", err)
	}
	if payload["status"] != "cancelled" || payload["progress_stage"] != "cancelled" {
		t.Fatalf("cancelled payload = %v", payload)
	}

	// 有 worker 认领的运行中任务停在 cancelling。
	setJobState(t, gdb, jobID, map[string]any{
		"status": "running", "worker_id": "worker-1", "progress_stage": "rendering_video",
	})
	payload, err = renderSvc.CancelRenderJob(ctx, jobID, user)
	if err != nil {
		t.Fatalf("cancel running job: %v", err)
	}
	if payload["status"] != "cancelling" {
		t.Fatalf("cancelling payload = %v", payload)
	}

	// 终态任务不可取消。
	setJobState(t, gdb, jobID, map[string]any{"status": "succeeded"})
	if _, err := renderSvc.CancelRenderJob(ctx, jobID, user); !isRenderConflictCode(err, "cancel_requires_active_job") {
		t.Fatalf("succeeded cancel err = %v", err)
	}

	var jobNotFound *RenderJobNotFoundError
	if _, err := renderSvc.CancelRenderJob(ctx, "none", user); !errors.As(err, &jobNotFound) {
		t.Fatalf("missing job err = %v", err)
	}
}

func TestGetRenderArtifactFileResolution(t *testing.T) {
	renderSvc, assemblySvc, store, gdb := newRenderingTestEnv(t)
	ctx := context.Background()
	user := repository.DefaultUserID

	plan := createAssemblyPlan(t, assemblySvc)
	planID, _ := plan["id"].(string)
	confirmPlanForPreview(t, assemblySvc, planID)
	job, err := renderSvc.CreatePreviewJob(ctx, planID, user, nil, 3)
	if err != nil {
		t.Fatalf("create preview job: %v", err)
	}
	jobID, _ := job["id"].(string)

	projectDir, err := store.Dir("p1")
	if err != nil {
		t.Fatalf("project dir: %v", err)
	}
	relative := "render_artifacts/preview/revision-1/preview-x.mp4"
	full := filepath.Join(projectDir, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte("mp4-bytes"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	artifact := &repository.RenderArtifact{
		ID: "artifact-1", RenderJobID: jobID, PlanID: planID, ProjectName: "p1", UserID: user,
		RevisionNumber: 1, Kind: "preview", RelativePath: relative,
		MimeType: "video/mp4", SizeBytes: 9, DurationSeconds: 2, Fingerprint: "fp-1",
	}
	if err := gdb.Create(artifact).Error; err != nil {
		t.Fatalf("insert artifact: %v", err)
	}

	payload, path, err := renderSvc.GetRenderArtifactFile(ctx, "artifact-1", user)
	if err != nil {
		t.Fatalf("get artifact file: %v", err)
	}
	if payload["mime_type"] != "video/mp4" || path != full {
		t.Fatalf("payload = %v path = %s", payload, path)
	}

	// 文件消失 → 产物不存在口径。
	if err := os.Remove(full); err != nil {
		t.Fatalf("remove artifact: %v", err)
	}
	var artifactNotFound *RenderArtifactNotFoundError
	if _, _, err := renderSvc.GetRenderArtifactFile(ctx, "artifact-1", user); !errors.As(err, &artifactNotFound) {
		t.Fatalf("missing file err = %v", err)
	}
	if _, _, err := renderSvc.GetRenderArtifactFile(ctx, "none", user); !errors.As(err, &artifactNotFound) {
		t.Fatalf("missing artifact err = %v", err)
	}
}

// generateReviewSampleVideo 用 lavfi 合成一段红底正弦音的 2 秒样本；
// frameRate 决定视频帧率——低帧率时末帧 pts 为 duration-1/fps，last 位置
// （duration-0.001）抽不到帧；取 "1000" 可让三帧全部抽到。
func generateReviewSampleVideo(t *testing.T, path, frameRate string) {
	t.Helper()
	tool, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatalf("lookup ffmpeg: %v", err)
	}
	cmd := exec.Command(tool,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=red:s=320x240:d=2:r="+frameRate,
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-shortest", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate sample video: %v: %s", err, out)
	}
}

func TestReviewFinalArtifactPipeline(t *testing.T) {
	if !ffmpeg.Available() || !ffmpeg.ProbeAvailable() {
		t.Skip("ffmpeg/ffprobe not available on PATH")
	}
	renderSvc, assemblySvc, store, gdb := newRenderingTestEnv(t)
	ctx := context.Background()
	user := repository.DefaultUserID

	// 无成片 → not_ready。
	plan := createAssemblyPlan(t, assemblySvc)
	planID, _ := plan["id"].(string)
	var notReady *FinalRenderNotReadyError
	if _, err := renderSvc.ReviewFinalArtifact(ctx, planID, user); !errors.As(err, &notReady) {
		t.Fatalf("not ready err = %v", err)
	}

	// 时间线与样本时长对齐（2 秒），字幕 cue 落在区间内。
	if err := gdb.Model(&repository.AssemblyPlanRevision{}).
		Where("plan_id = ?", planID).
		Update("timeline_json", `[{"id":"E1U1:0","source_unit_id":"E1U1","order":0,"kind":"video_unit","source_ref":"videos/E1U1.mp4","duration_seconds":2.0,"trim_start_seconds":0.0,"trim_end_seconds":0.0,"audio_policy":"duck","transition":null}]`).
		Error; err != nil {
		t.Fatalf("update timeline: %v", err)
	}

	projectDir, err := store.Dir("p1")
	if err != nil {
		t.Fatalf("project dir: %v", err)
	}
	relative := "render_artifacts/final/revision-1/final-job1.mp4"
	full := filepath.Join(projectDir, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	generateReviewSampleVideo(t, full, "1000")

	probe, err := renderSvc.probeFinalArtifact(ctx, full)
	if err != nil {
		t.Fatalf("probe sample: %v", err)
	}
	duration, _ := probe["duration_seconds"].(float64)
	info, err := os.Stat(full)
	if err != nil {
		t.Fatalf("stat sample: %v", err)
	}
	insertRenderJob(t, gdb, "job-final-1", planID, user, "final")
	artifact := &repository.RenderArtifact{
		ID: "artifact-final-1", RenderJobID: "job-final-1", PlanID: planID, ProjectName: "p1", UserID: user,
		RevisionNumber: 1, Kind: "final", RelativePath: relative,
		MimeType: "video/mp4", SizeBytes: info.Size(), DurationSeconds: duration,
		Fingerprint: "fp-final-1",
	}
	if err := gdb.Create(artifact).Error; err != nil {
		t.Fatalf("insert final artifact: %v", err)
	}

	review, err := renderSvc.ReviewFinalArtifact(ctx, planID, user)
	if err != nil {
		t.Fatalf("review final artifact: %v", err)
	}
	if review["status"] != "ready" {
		checks, _ := review["checks"].(map[string]any)
		t.Fatalf("review status = %v checks = %v", review["status"], checks)
	}
	blocking, _ := review["blocking_reasons"].([]any)
	if len(blocking) != 0 {
		t.Fatalf("blocking reasons = %v", blocking)
	}
	snapshot, _ := review["review_snapshot"].(map[string]any)
	if snapshot == nil || snapshot["status"] != "ready" {
		t.Fatalf("snapshot = %v", snapshot)
	}
	checks, _ := review["checks"].(map[string]any)
	frames, _ := checks["frames"].(map[string]any)
	if available, _ := frames["all_available"].(bool); !available {
		t.Fatalf("frames check = %v", frames)
	}

	// 幂等：第二次审查命中缓存，快照不重复。
	cached, err := renderSvc.ReviewFinalArtifact(ctx, planID, user)
	if err != nil {
		t.Fatalf("cached review: %v", err)
	}
	cachedSnapshot, _ := cached["review_snapshot"].(map[string]any)
	if cachedSnapshot["id"] != snapshot["id"] {
		t.Fatalf("cached snapshot = %v want %v", cachedSnapshot, snapshot)
	}
	var count int64
	if err := gdb.Model(&repository.RenderReviewSnapshot{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("snapshot count = %d err = %v", count, err)
	}

	// 确认审查后 confirmed_by 落库。
	confirmed, err := renderSvc.ConfirmFinalReview(ctx, "artifact-final-1", user)
	if err != nil {
		t.Fatalf("confirm final review: %v", err)
	}
	confirmedBy, _ := confirmed["confirmed_by"].(*string)
	if confirmedBy == nil || *confirmedBy != user {
		t.Fatalf("confirmed payload = %v", confirmed["confirmed_by"])
	}

	// 审查帧文件存在且只认 first/middle/last。
	_, framePath, err := renderSvc.GetFinalReviewFrameFile(ctx, "artifact-final-1", "first", user)
	if err != nil {
		t.Fatalf("get review frame: %v", err)
	}
	if _, err := os.Stat(framePath); err != nil {
		t.Fatalf("review frame missing: %v", err)
	}
	var artifactNotFound *RenderArtifactNotFoundError
	if _, _, err := renderSvc.GetFinalReviewFrameFile(ctx, "artifact-final-1", "bogus", user); !errors.As(err, &artifactNotFound) {
		t.Fatalf("bad position err = %v", err)
	}

	// 阻断路径：产物元数据时长与实际探针不符 + 字幕 cue 越界，两条 blocking issue。
	blockedPlan := createAssemblyPlan(t, assemblySvc)
	blockedPlanID, _ := blockedPlan["id"].(string)
	if err := gdb.Model(&repository.AssemblyPlanRevision{}).
		Where("plan_id = ?", blockedPlanID).
		Updates(map[string]any{
			"timeline_json": `[{"id":"E1U1:0","source_unit_id":"E1U1","order":0,"kind":"video_unit","source_ref":"videos/E1U1.mp4","duration_seconds":2.0,"trim_start_seconds":0.0,"trim_end_seconds":0.0,"audio_policy":"duck","transition":null}]`,
			"subtitle_json": `{"mode":"burn_in","cues":[{"start_seconds":0.2,"end_seconds":2.5,"text":"overflow"}]}`,
		}).
		Error; err != nil {
		t.Fatalf("update blocked revision: %v", err)
	}
	blockedRelative := "render_artifacts/final/revision-1/final-job2.mp4"
	blockedFull := filepath.Join(projectDir, filepath.FromSlash(blockedRelative))
	if err := os.MkdirAll(filepath.Dir(blockedFull), 0o755); err != nil {
		t.Fatalf("mkdir blocked: %v", err)
	}
	generateReviewSampleVideo(t, blockedFull, "1000")
	blockedProbe, err := renderSvc.probeFinalArtifact(ctx, blockedFull)
	if err != nil {
		t.Fatalf("probe blocked sample: %v", err)
	}
	blockedDuration, _ := blockedProbe["duration_seconds"].(float64)
	blockedInfo, err := os.Stat(blockedFull)
	if err != nil {
		t.Fatalf("stat blocked sample: %v", err)
	}
	insertRenderJob(t, gdb, "job-final-2", blockedPlanID, user, "final")
	blockedArtifact := &repository.RenderArtifact{
		ID: "artifact-final-2", RenderJobID: "job-final-2", PlanID: blockedPlanID, ProjectName: "p1", UserID: user,
		RevisionNumber: 1, Kind: "final", RelativePath: blockedRelative,
		// 元数据时长故意错位 1 秒，触发 duration_mismatch。
		MimeType: "video/mp4", SizeBytes: blockedInfo.Size(), DurationSeconds: blockedDuration + 1,
		Fingerprint: "fp-final-2",
	}
	if err := gdb.Create(blockedArtifact).Error; err != nil {
		t.Fatalf("insert blocked artifact: %v", err)
	}
	blockedReview, err := renderSvc.ReviewFinalArtifact(ctx, blockedPlanID, user)
	if err != nil {
		t.Fatalf("review blocked sample: %v", err)
	}
	if blockedReview["status"] != "blocked" {
		t.Fatalf("blocked review = %v", blockedReview["status"])
	}
	blockedReasons, _ := blockedReview["blocking_reasons"].([]any)
	wantReasons := map[string]bool{"duration_mismatch": false, "subtitle_bounds_invalid": false}
	for _, reason := range blockedReasons {
		if _, ok := wantReasons[reason.(string)]; ok {
			wantReasons[reason.(string)] = true
		}
	}
	for reason, found := range wantReasons {
		if !found {
			t.Fatalf("blocking reasons = %v want %s", blockedReasons, reason)
		}
	}
}

func TestConfirmFinalReviewConflicts(t *testing.T) {
	renderSvc, assemblySvc, _, gdb := newRenderingTestEnv(t)
	ctx := context.Background()
	user := repository.DefaultUserID

	plan := createAssemblyPlan(t, assemblySvc)
	planID, _ := plan["id"].(string)

	insertRenderJob(t, gdb, "job-1", planID, user, "preview")
	previewArtifact := &repository.RenderArtifact{
		ID: "artifact-preview-1", RenderJobID: "job-1", PlanID: planID, ProjectName: "p1", UserID: user,
		RevisionNumber: 1, Kind: "preview", RelativePath: "render_artifacts/preview/revision-1/preview-x.mp4",
		MimeType: "video/mp4", Fingerprint: "fp-preview-1",
	}
	if err := gdb.Create(previewArtifact).Error; err != nil {
		t.Fatalf("insert preview artifact: %v", err)
	}
	var reviewConflict *RenderReviewConflictError
	if _, err := renderSvc.ConfirmFinalReview(ctx, "artifact-preview-1", user); !errors.As(err, &reviewConflict) || reviewConflict.Code != "final_artifact_required" {
		t.Fatalf("preview artifact err = %v", err)
	}

	insertRenderJob(t, gdb, "job-2", planID, user, "final")
	finalArtifact := &repository.RenderArtifact{
		ID: "artifact-final-1", RenderJobID: "job-2", PlanID: planID, ProjectName: "p1", UserID: user,
		RevisionNumber: 1, Kind: "final", RelativePath: "render_artifacts/final/revision-1/final-x.mp4",
		MimeType: "video/mp4", Fingerprint: "fp-final-1",
	}
	if err := gdb.Create(finalArtifact).Error; err != nil {
		t.Fatalf("insert final artifact: %v", err)
	}
	if _, err := renderSvc.ConfirmFinalReview(ctx, "artifact-final-1", user); !errors.As(err, &reviewConflict) || reviewConflict.Code != "review_required" {
		t.Fatalf("missing snapshot err = %v", err)
	}

	// 快照缓存键包含 revision 的 source_fingerprint，取真实值才能命中。
	var revision repository.AssemblyPlanRevision
	if err := gdb.Where("plan_id = ? AND version_number = ?", planID, 1).First(&revision).Error; err != nil {
		t.Fatalf("load revision: %v", err)
	}
	blocked := &repository.RenderReviewSnapshot{
		ID: "snapshot-1", ArtifactID: "artifact-final-1", PlanID: planID, ProjectName: "p1", UserID: user,
		RevisionNumber: 1, SourceFingerprint: revision.SourceFingerprint, ArtifactFingerprint: "fp-final-1",
		Status: "blocked", ChecksJSON: "{}",
	}
	if err := gdb.Create(blocked).Error; err != nil {
		t.Fatalf("insert blocked snapshot: %v", err)
	}
	if _, err := renderSvc.ConfirmFinalReview(ctx, "artifact-final-1", user); !errors.As(err, &reviewConflict) || reviewConflict.Code != "review_blocked" {
		t.Fatalf("blocked snapshot err = %v", err)
	}

	var artifactNotFound *RenderArtifactNotFoundError
	if _, err := renderSvc.ConfirmFinalReview(ctx, "none", user); !errors.As(err, &artifactNotFound) {
		t.Fatalf("missing artifact err = %v", err)
	}
}

func TestExportSubtitles(t *testing.T) {
	renderSvc, assemblySvc, _, gdb := newRenderingTestEnv(t)
	ctx := context.Background()
	user := repository.DefaultUserID

	// 计划带一条合法 cue。
	plan := createAssemblyPlan(t, assemblySvc)
	planID, _ := plan["id"].(string)
	if err := gdb.Model(&repository.AssemblyPlanRevision{}).
		Where("plan_id = ?", planID).
		Update("subtitle_json", `{"mode":"burn_in","cues":[{"start_seconds":0.2,"end_seconds":0.8,"text":"hello"}]}`).
		Error; err != nil {
		t.Fatalf("update subtitle: %v", err)
	}

	srt, err := renderSvc.ExportSubtitles(ctx, planID, "srt", user)
	if err != nil {
		t.Fatalf("export srt: %v", err)
	}
	if content, _ := srt["content"].(string); !strings.Contains(content, "hello") || !strings.Contains(content, "00:00:00,200 --> 00:00:00,800") {
		t.Fatalf("srt content = %q", content)
	}
	if srt["media_type"] != "application/x-subrip; charset=utf-8" || srt["filename"] != "subtitles.srt" {
		t.Fatalf("srt meta = %v", srt)
	}

	vtt, err := renderSvc.ExportSubtitles(ctx, planID, "vtt", user)
	if err != nil {
		t.Fatalf("export vtt: %v", err)
	}
	if content, _ := vtt["content"].(string); !strings.Contains(content, "WEBVTT") || !strings.Contains(content, "00:00:00.200 --> 00:00:00.800") {
		t.Fatalf("vtt content = %q", content)
	}

	var subtitleErr *SubtitleExportError
	if _, err := renderSvc.ExportSubtitles(ctx, planID, "ass", user); !errors.As(err, &subtitleErr) || subtitleErr.Message != "subtitle format must be srt or vtt" {
		t.Fatalf("bad format err = %v", err)
	}

	// cues 形状非法 → 明确消息。
	if err := gdb.Model(&repository.AssemblyPlanRevision{}).
		Where("plan_id = ?", planID).
		Update("subtitle_json", `{"cues":"nope"}`).Error; err != nil {
		t.Fatalf("break subtitle: %v", err)
	}
	if _, err := renderSvc.ExportSubtitles(ctx, planID, "srt", user); !errors.As(err, &subtitleErr) || subtitleErr.Message != "subtitle configuration must contain a cues list" {
		t.Fatalf("bad cues err = %v", err)
	}

	// cue 区间非法 → 归一化校验失败。
	if err := gdb.Model(&repository.AssemblyPlanRevision{}).
		Where("plan_id = ?", planID).
		Update("subtitle_json", `{"cues":[{"start_seconds":2,"end_seconds":1,"text":"x"}]}`).Error; err != nil {
		t.Fatalf("break cue interval: %v", err)
	}
	if _, err := renderSvc.ExportSubtitles(ctx, planID, "srt", user); !errors.As(err, &subtitleErr) || subtitleErr.Message != "subtitle configuration is invalid" {
		t.Fatalf("bad interval err = %v", err)
	}

	var planNotFound *AssemblyPlanNotFoundError
	if _, err := renderSvc.ExportSubtitles(ctx, "none", "srt", user); !errors.As(err, &planNotFound) {
		t.Fatalf("missing plan err = %v", err)
	}
}
