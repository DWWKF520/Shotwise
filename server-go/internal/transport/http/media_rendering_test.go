package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"gorm.io/gorm"
)

func TestRegisterMediaRenderingRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterMediaRendering(r.Group("/api/v1"), MediaRenderingDeps{})

	registered := map[string]bool{}
	for _, route := range r.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	want := []string{
		http.MethodPost + " /api/v1/assembly-plans/:plan_id/preview-renders",
		http.MethodPost + " /api/v1/assembly-plans/:plan_id/final-renders",
		http.MethodGet + " /api/v1/assembly-plans/:plan_id/preview-renders",
		http.MethodGet + " /api/v1/assembly-plans/:plan_id/final-renders",
		http.MethodGet + " /api/v1/assembly-plans/:plan_id/final-review",
		http.MethodGet + " /api/v1/assembly-plans/:plan_id/subtitles",
		http.MethodGet + " /api/v1/render-jobs/:job_id",
		http.MethodPost + " /api/v1/render-jobs/:job_id/retry",
		http.MethodPost + " /api/v1/render-jobs/:job_id/final-retry",
		http.MethodPost + " /api/v1/render-jobs/:job_id/cancel",
		http.MethodGet + " /api/v1/render-jobs/:job_id/artifact",
		http.MethodGet + " /api/v1/render-artifacts/:artifact_id",
		http.MethodPost + " /api/v1/render-artifacts/:artifact_id/review-confirm",
		http.MethodGet + " /api/v1/render-artifacts/:artifact_id/review-frames/:position",
	}
	for _, route := range want {
		if !registered[route] {
			t.Errorf("route not registered: %s", route)
		}
	}
}

// renderHandlerEnv 是渲染 handler 测试环境：真实服务 + 迁移库 + draft 计划。
type renderHandlerEnv struct {
	r        *gin.Engine
	gdb      *gorm.DB
	store    *service.ProjectStore
	assembly *service.MediaAssemblyService
	renders  *service.MediaRenderingService
	planID   string
}

func newRenderHandlerEnv(t *testing.T) *renderHandlerEnv {
	t.Helper()
	gdb := newJianyingHTTPTestDB(t)
	store := service.NewProjectStore(t.TempDir())
	assembly := service.NewMediaAssemblyService(gdb)
	manifest := map[string]any{
		"contract_version": "episode-media-manifest/v1",
		"persistence":      "read_only",
		"script":           "episode_1.json",
		"episode":          1,
		"content_mode":     "drama",
		"generation_mode":  "storyboard",
		"collection":       "scenes",
		"fingerprint":      "manifest-fingerprint",
		"items": []any{map[string]any{
			"order": 1, "unit_id": "E1U1", "planned_duration_seconds": 5.0,
			"source":      map[string]any{"path": "videos/E1U1.mp4", "kind": "generated_video"},
			"media_probe": map[string]any{"exists": true, "probe_available": false, "has_video_stream": nil},
			"file_fingerprint": map[string]any{
				"exists": true, "sha256": "0000000000000000000000000000000000000000000000000000000000000000",
			},
		}},
	}
	timeline := []map[string]any{{
		"id": "E1U1:0", "source_unit_id": "E1U1", "order": 0, "kind": "video_unit",
		"source_ref": "videos/E1U1.mp4", "duration_seconds": 5.0,
		"trim_start_seconds": 0.0, "trim_end_seconds": 0.0,
		"audio_policy": "duck", "transition": nil,
	}}
	payload, err := assembly.CreatePlan(context.Background(), service.CreatePlanParams{
		UserID: repository.DefaultUserID, ProjectName: "p1", Name: "plan", Scope: "episode",
		SourceManifest: manifest, Timeline: timeline,
		Audio:     map[string]any{"tracks": []any{}},
		Subtitle:  map[string]any{"cues": []any{}},
		Packaging: map[string]any{"cover": map[string]any{"enabled": false}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	planID, _ := payload["id"].(string)

	renders := service.NewMediaRenderingService(gdb, store)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterMediaRendering(r.Group("/api/v1"), MediaRenderingDeps{Renders: renders})
	return &renderHandlerEnv{r: r, gdb: gdb, store: store, assembly: assembly, renders: renders, planID: planID}
}

// confirmPlan 把计划推进到 confirmed（预览任务门槛）。
func (e *renderHandlerEnv) confirmPlan(t *testing.T) {
	t.Helper()
	if _, err := e.assembly.TransitionPlan(context.Background(), e.planID, repository.DefaultUserID, "confirmed"); err != nil {
		t.Fatalf("confirm plan: %v", err)
	}
}

// craftFinalReady 直接把计划写成 render_pending 并补齐确认字段（成片任务门槛）。
func (e *renderHandlerEnv) craftFinalReady(t *testing.T) {
	t.Helper()
	now := "2026-01-02T00:00:00+00:00"
	err := e.gdb.Exec(
		`UPDATE assembly_plans SET status = 'render_pending', preview_revision_number = 1,
		 preview_confirmed_by = ?, preview_confirmed_at = ?, render_confirmed_by = ?, render_confirmed_at = ?,
		 updated_at = ? WHERE id = ?`,
		repository.DefaultUserID, now, repository.DefaultUserID, now, now, e.planID,
	).Error
	if err != nil {
		t.Fatalf("craft render_pending: %v", err)
	}
}

// insertHTTPRenderJob 插入任务父行，满足 render_artifacts 外键；
// revisionNumber 用于绕开活跃任务的部分唯一索引 (plan_id, revision_number, kind)。
func (e *renderHandlerEnv) insertHTTPRenderJob(t *testing.T, jobID, kind, status string, revisionNumber int64) {
	t.Helper()
	now := db.NewFlexibleTime(time.Now().UTC())
	job := &repository.RenderJob{
		ID: jobID, PlanID: e.planID, ProjectName: "p1", UserID: repository.DefaultUserID,
		RevisionNumber: revisionNumber, Kind: kind, Status: status, Attempt: 1, MaxAttempts: 3,
		InputFingerprint: "fp-job", ProgressStage: "queued",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := e.gdb.Create(job).Error; err != nil {
		t.Fatalf("insert render job: %v", err)
	}
}

func renderRequest(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = strings.NewReader(string(raw))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func decodeRenderBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body.String(), err)
	}
	return out
}

// renderDetailCode 取错误信封里 detail 对象的 code 字段。
func renderDetailCode(t *testing.T, rec *httptest.ResponseRecorder) (map[string]any, string) {
	t.Helper()
	out := decodeRenderBody(t, rec)
	detail, _ := out["detail"].(map[string]any)
	if detail == nil {
		t.Fatalf("detail object missing: %s", rec.Body.String())
	}
	code, _ := detail["code"].(string)
	return detail, code
}

func TestPreviewRenderJobEndpoints(t *testing.T) {
	env := newRenderHandlerEnv(t)
	base := "/api/v1/assembly-plans/" + env.planID + "/preview-renders"

	// 计划未确认 → 409 preview_requires_confirmed_plan。
	rec := renderRequest(t, env.r, http.MethodPost, base, map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("draft status = %d body=%s", rec.Code, rec.Body.String())
	}
	detail, code := renderDetailCode(t, rec)
	if code != "preview_requires_confirmed_plan" || detail["status"] != "draft" {
		t.Fatalf("draft detail = %v", detail)
	}

	env.confirmPlan(t)
	rec = renderRequest(t, env.r, http.MethodPost, base, map[string]any{})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("accepted status = %d body=%s", rec.Code, rec.Body.String())
	}
	job := decodeRenderBody(t, rec)
	if job["kind"] != "preview" || job["status"] != "queued" || job["max_attempts"] != float64(3) {
		t.Fatalf("job = %v", job)
	}

	// 重复入队：计划转回 confirmed 后命中活动任务去重。
	if err := env.gdb.Exec(
		"UPDATE assembly_plans SET status = 'confirmed' WHERE id = ?", env.planID,
	).Error; err != nil {
		t.Fatalf("re-confirm plan: %v", err)
	}
	rec = renderRequest(t, env.r, http.MethodPost, base, map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, code = renderDetailCode(t, rec); code != "preview_job_already_running" {
		t.Fatalf("duplicate code = %s", code)
	}

	// revision_number < 1 → 422。
	rec = renderRequest(t, env.r, http.MethodPost, base, map[string]any{"revision_number": 0})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "revision_number") {
		t.Fatalf("bad revision = %d %s", rec.Code, rec.Body.String())
	}
	// max_attempts 越界 → 422。
	rec = renderRequest(t, env.r, http.MethodPost, base, map[string]any{"max_attempts": 11})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "max_attempts") {
		t.Fatalf("bad attempts = %d %s", rec.Code, rec.Body.String())
	}
	// 计划不存在 → 404 字面量。
	rec = renderRequest(t, env.r, http.MethodPost, "/api/v1/assembly-plans/none/preview-renders", map[string]any{})
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "render_resource_not_found") {
		t.Fatalf("missing plan = %d %s", rec.Code, rec.Body.String())
	}
}

func TestFinalRenderJobEndpoints(t *testing.T) {
	env := newRenderHandlerEnv(t)
	base := "/api/v1/assembly-plans/" + env.planID + "/final-renders"

	// confirmed 但缺预览 revision → 409。
	env.confirmPlan(t)
	rec := renderRequest(t, env.r, http.MethodPost, base, map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("confirmed status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, code := renderDetailCode(t, rec); code != "preview_revision_required" {
		t.Fatalf("confirmed code = %s", code)
	}

	env.craftFinalReady(t)
	rec = renderRequest(t, env.r, http.MethodPost, base, map[string]any{})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("accepted status = %d body=%s", rec.Code, rec.Body.String())
	}
	job := decodeRenderBody(t, rec)
	if job["kind"] != "final" || job["status"] != "queued" {
		t.Fatalf("job = %v", job)
	}
	jobID, _ := job["id"].(string)

	// final-retry 对排队中的任务 → 409 retry_requires_terminal_job。
	rec = renderRequest(t, env.r, http.MethodPost, "/api/v1/render-jobs/"+jobID+"/final-retry", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("queued final-retry = %d %s", rec.Code, rec.Body.String())
	}
	if _, code := renderDetailCode(t, rec); code != "retry_requires_terminal_job" {
		t.Fatalf("queued final-retry code = %s", code)
	}

	// 失败后 final-retry → 202。
	if err := env.gdb.Exec(
		"UPDATE render_jobs SET status = 'failed', attempt = 1 WHERE id = ?", jobID,
	).Error; err != nil {
		t.Fatalf("fail job: %v", err)
	}
	rec = renderRequest(t, env.r, http.MethodPost, "/api/v1/render-jobs/"+jobID+"/final-retry", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("final-retry status = %d body=%s", rec.Code, rec.Body.String())
	}
	retried := decodeRenderBody(t, rec)
	if retried["status"] != "queued" {
		t.Fatalf("retried = %v", retried)
	}

	// 计划不存在 → 404。
	rec = renderRequest(t, env.r, http.MethodPost, "/api/v1/assembly-plans/none/final-renders", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing plan = %d %s", rec.Code, rec.Body.String())
	}
}

func TestListRenderJobsEndpoints(t *testing.T) {
	env := newRenderHandlerEnv(t)
	env.confirmPlan(t)
	job, err := env.renders.CreatePreviewJob(
		context.Background(), env.planID, repository.DefaultUserID, nil, 3,
	)
	if err != nil {
		t.Fatalf("create preview job: %v", err)
	}
	jobID, _ := job["id"].(string)

	rec := renderRequest(t, env.r, http.MethodGet, "/api/v1/assembly-plans/"+env.planID+"/preview-renders", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview list = %d %s", rec.Code, rec.Body.String())
	}
	out := decodeRenderBody(t, rec)
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("preview items = %v", items)
	}
	first, _ := items[0].(map[string]any)
	if first["id"] != jobID {
		t.Fatalf("item = %v", first)
	}

	rec = renderRequest(t, env.r, http.MethodGet, "/api/v1/assembly-plans/"+env.planID+"/final-renders", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("final list = %d %s", rec.Code, rec.Body.String())
	}
	out = decodeRenderBody(t, rec)
	if items, _ = out["items"].([]any); len(items) != 0 {
		t.Fatalf("final items = %v", items)
	}

	rec = renderRequest(t, env.r, http.MethodGet, "/api/v1/assembly-plans/none/preview-renders", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing plan = %d %s", rec.Code, rec.Body.String())
	}
}

func TestGetRetryCancelRenderJobEndpoints(t *testing.T) {
	env := newRenderHandlerEnv(t)
	env.confirmPlan(t)
	job, err := env.renders.CreatePreviewJob(
		context.Background(), env.planID, repository.DefaultUserID, nil, 3,
	)
	if err != nil {
		t.Fatalf("create preview job: %v", err)
	}
	jobID, _ := job["id"].(string)

	rec := renderRequest(t, env.r, http.MethodGet, "/api/v1/render-jobs/"+jobID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get job = %d %s", rec.Code, rec.Body.String())
	}
	got := decodeRenderBody(t, rec)
	if got["id"] != jobID {
		t.Fatalf("job = %v", got)
	}

	rec = renderRequest(t, env.r, http.MethodGet, "/api/v1/render-jobs/none", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing job = %d %s", rec.Code, rec.Body.String())
	}

	// 排队中不可重试。
	rec = renderRequest(t, env.r, http.MethodPost, "/api/v1/render-jobs/"+jobID+"/retry", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("queued retry = %d %s", rec.Code, rec.Body.String())
	}

	// 失败后重试 → 202。
	if err := env.gdb.Exec(
		"UPDATE render_jobs SET status = 'failed', attempt = 1 WHERE id = ?", jobID,
	).Error; err != nil {
		t.Fatalf("fail job: %v", err)
	}
	rec = renderRequest(t, env.r, http.MethodPost, "/api/v1/render-jobs/"+jobID+"/retry", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body.String())
	}

	// 排队任务取消 → 202 cancelled。
	rec = renderRequest(t, env.r, http.MethodPost, "/api/v1/render-jobs/"+jobID+"/cancel", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body.String())
	}
	cancelled := decodeRenderBody(t, rec)
	if cancelled["status"] != "cancelled" {
		t.Fatalf("cancelled = %v", cancelled)
	}

	// preview 任务不能走成片重试。
	rec = renderRequest(t, env.r, http.MethodPost, "/api/v1/render-jobs/"+jobID+"/final-retry", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("preview final-retry = %d %s", rec.Code, rec.Body.String())
	}
	if _, code := renderDetailCode(t, rec); code != "final_retry_requires_final_job" {
		t.Fatalf("preview final-retry code = %s", code)
	}

	rec = renderRequest(t, env.r, http.MethodPost, "/api/v1/render-jobs/none/cancel", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing cancel = %d %s", rec.Code, rec.Body.String())
	}
}

func TestFinalReviewEndpoint(t *testing.T) {
	env := newRenderHandlerEnv(t)

	// 无成片 → 409 not_ready，resource 即 plan_id。
	rec := renderRequest(t, env.r, http.MethodGet, "/api/v1/assembly-plans/"+env.planID+"/final-review", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("not ready = %d %s", rec.Code, rec.Body.String())
	}
	detail, code := renderDetailCode(t, rec)
	if code != "not_ready" || detail["resource"] != env.planID {
		t.Fatalf("not ready detail = %v", detail)
	}

	// 计划不存在 → 404。
	rec = renderRequest(t, env.r, http.MethodGet, "/api/v1/assembly-plans/none/final-review", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing plan = %d %s", rec.Code, rec.Body.String())
	}
}

func TestConfirmReviewEndpoint(t *testing.T) {
	env := newRenderHandlerEnv(t)

	rec := renderRequest(t, env.r, http.MethodPost, "/api/v1/render-artifacts/none/review-confirm", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing artifact = %d %s", rec.Code, rec.Body.String())
	}

	env.insertHTTPRenderJob(t, "job-preview", "preview", "succeeded", 1)
	previewArtifact := &repository.RenderArtifact{
		ID: "artifact-preview", RenderJobID: "job-preview", PlanID: env.planID,
		ProjectName: "p1", UserID: repository.DefaultUserID,
		RevisionNumber: 1, Kind: "preview", RelativePath: "render_artifacts/preview/revision-1/p.mp4",
		MimeType: "video/mp4", Fingerprint: "fp-preview",
	}
	if err := env.gdb.Create(previewArtifact).Error; err != nil {
		t.Fatalf("insert preview artifact: %v", err)
	}
	rec = renderRequest(t, env.r, http.MethodPost, "/api/v1/render-artifacts/artifact-preview/review-confirm", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("preview artifact = %d %s", rec.Code, rec.Body.String())
	}
	if _, code := renderDetailCode(t, rec); code != "final_artifact_required" {
		t.Fatalf("preview artifact code = %s", code)
	}

	// 成片缺快照 → review_required。
	env.insertHTTPRenderJob(t, "job-final", "final", "succeeded", 1)
	finalArtifact := &repository.RenderArtifact{
		ID: "artifact-final", RenderJobID: "job-final", PlanID: env.planID,
		ProjectName: "p1", UserID: repository.DefaultUserID,
		RevisionNumber: 1, Kind: "final", RelativePath: "render_artifacts/final/revision-1/f.mp4",
		MimeType: "video/mp4", Fingerprint: "fp-final",
	}
	if err := env.gdb.Create(finalArtifact).Error; err != nil {
		t.Fatalf("insert final artifact: %v", err)
	}
	rec = renderRequest(t, env.r, http.MethodPost, "/api/v1/render-artifacts/artifact-final/review-confirm", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("missing snapshot = %d %s", rec.Code, rec.Body.String())
	}
	if _, code := renderDetailCode(t, rec); code != "review_required" {
		t.Fatalf("missing snapshot code = %s", code)
	}

	// blocked 快照 → review_blocked（快照键需带 revision 真实指纹）。
	var revision repository.AssemblyPlanRevision
	if err := env.gdb.Where("plan_id = ? AND version_number = ?", env.planID, 1).
		First(&revision).Error; err != nil {
		t.Fatalf("load revision: %v", err)
	}
	snapshot := &repository.RenderReviewSnapshot{
		ID: "snapshot-1", ArtifactID: "artifact-final", PlanID: env.planID,
		ProjectName: "p1", UserID: repository.DefaultUserID,
		RevisionNumber: 1, SourceFingerprint: revision.SourceFingerprint, ArtifactFingerprint: "fp-final",
		Status: "blocked", ChecksJSON: "{}",
	}
	if err := env.gdb.Create(snapshot).Error; err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
	rec = renderRequest(t, env.r, http.MethodPost, "/api/v1/render-artifacts/artifact-final/review-confirm", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("blocked snapshot = %d %s", rec.Code, rec.Body.String())
	}
	if _, code := renderDetailCode(t, rec); code != "review_blocked" {
		t.Fatalf("blocked snapshot code = %s", code)
	}
}

func TestSubtitlesEndpoint(t *testing.T) {
	env := newRenderHandlerEnv(t)
	if err := env.gdb.Model(&repository.AssemblyPlanRevision{}).
		Where("plan_id = ?", env.planID).
		Update("subtitle_json", `{"mode":"burn_in","cues":[{"start_seconds":0.2,"end_seconds":0.8,"text":"hello"}]}`).
		Error; err != nil {
		t.Fatalf("update subtitle: %v", err)
	}
	base := "/api/v1/assembly-plans/" + env.planID + "/subtitles"

	rec := renderRequest(t, env.r, http.MethodGet, base+"?format=srt", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("srt = %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/x-subrip") {
		t.Fatalf("srt content-type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "subtitles.srt") {
		t.Fatalf("srt disposition = %q", cd)
	}
	if !strings.Contains(rec.Body.String(), "00:00:00,200 --> 00:00:00,800") {
		t.Fatalf("srt body = %s", rec.Body.String())
	}

	rec = renderRequest(t, env.r, http.MethodGet, base+"?format=vtt", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "WEBVTT") {
		t.Fatalf("vtt = %d %s", rec.Code, rec.Body.String())
	}

	rec = renderRequest(t, env.r, http.MethodGet, base+"?format=ass", nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "string_pattern_mismatch") {
		t.Fatalf("bad format = %d %s", rec.Code, rec.Body.String())
	}
	rec = renderRequest(t, env.r, http.MethodGet, base, nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "format") {
		t.Fatalf("missing format = %d %s", rec.Code, rec.Body.String())
	}

	// 字幕数据非法 → 422 invalid_subtitles。
	if err := env.gdb.Model(&repository.AssemblyPlanRevision{}).
		Where("plan_id = ?", env.planID).
		Update("subtitle_json", `{"cues":"nope"}`).Error; err != nil {
		t.Fatalf("break subtitle: %v", err)
	}
	rec = renderRequest(t, env.r, http.MethodGet, base+"?format=srt", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("broken subtitle = %d %s", rec.Code, rec.Body.String())
	}
	if _, code := renderDetailCode(t, rec); code != "invalid_subtitles" {
		t.Fatalf("broken subtitle code = %s", code)
	}

	rec = renderRequest(t, env.r, http.MethodGet, "/api/v1/assembly-plans/none/subtitles?format=srt", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing plan = %d %s", rec.Code, rec.Body.String())
	}
}

func TestArtifactFileEndpoints(t *testing.T) {
	env := newRenderHandlerEnv(t)
	env.confirmPlan(t)
	job, err := env.renders.CreatePreviewJob(
		context.Background(), env.planID, repository.DefaultUserID, nil, 3,
	)
	if err != nil {
		t.Fatalf("create preview job: %v", err)
	}
	jobID, _ := job["id"].(string)

	projectDir, err := env.store.Dir("p1")
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
		ID: "artifact-1", RenderJobID: jobID, PlanID: env.planID, ProjectName: "p1",
		UserID: repository.DefaultUserID, RevisionNumber: 1, Kind: "preview", RelativePath: relative,
		MimeType: "video/mp4", SizeBytes: 9, DurationSeconds: 2, Fingerprint: "fp-1",
	}
	if err := env.gdb.Create(artifact).Error; err != nil {
		t.Fatalf("insert artifact: %v", err)
	}

	rec := renderRequest(t, env.r, http.MethodGet, "/api/v1/render-artifacts/artifact-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("artifact = %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "video/mp4") {
		t.Fatalf("artifact content-type = %q", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("artifact nosniff missing")
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "preview-x.mp4") {
		t.Fatalf("artifact disposition = %q", cd)
	}
	if rec.Body.String() != "mp4-bytes" {
		t.Fatalf("artifact body = %q", rec.Body.String())
	}

	// 任务最新产物端点。
	rec = renderRequest(t, env.r, http.MethodGet, "/api/v1/render-jobs/"+jobID+"/artifact", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "mp4-bytes" {
		t.Fatalf("job artifact = %d %s", rec.Code, rec.Body.String())
	}

	// 无产物的任务 → 404；revision 2 避开与上方活跃 preview 任务的部分唯一索引。
	env.insertHTTPRenderJob(t, "job-empty", "preview", "queued", 2)
	rec = renderRequest(t, env.r, http.MethodGet, "/api/v1/render-jobs/job-empty/artifact", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty job artifact = %d %s", rec.Code, rec.Body.String())
	}
	rec = renderRequest(t, env.r, http.MethodGet, "/api/v1/render-artifacts/none", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing artifact = %d %s", rec.Code, rec.Body.String())
	}

	// 审查帧：手写帧文件即可覆盖下载路径，无需真实 ffmpeg。
	env.insertHTTPRenderJob(t, "job-frame", "final", "succeeded", 1)
	frameArtifact := &repository.RenderArtifact{
		ID: "artifact-frame", RenderJobID: "job-frame", PlanID: env.planID, ProjectName: "p1",
		UserID: repository.DefaultUserID, RevisionNumber: 1, Kind: "final",
		RelativePath: "render_artifacts/final/revision-1/f.mp4",
		MimeType:     "video/mp4", SizeBytes: 9, DurationSeconds: 2, Fingerprint: "fp-frame",
	}
	if err := env.gdb.Create(frameArtifact).Error; err != nil {
		t.Fatalf("insert frame artifact: %v", err)
	}
	frameDir := filepath.Join(projectDir, "render_artifacts", "reviews", "artifact-frame")
	if err := os.MkdirAll(frameDir, 0o755); err != nil {
		t.Fatalf("mkdir frames: %v", err)
	}
	if err := os.WriteFile(filepath.Join(frameDir, "first.jpg"), []byte("jpeg-bytes"), 0o644); err != nil {
		t.Fatalf("write frame: %v", err)
	}

	rec = renderRequest(t, env.r, http.MethodGet, "/api/v1/render-artifacts/artifact-frame/review-frames/first", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "jpeg-bytes" {
		t.Fatalf("review frame = %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/jpeg") {
		t.Fatalf("frame content-type = %q", ct)
	}

	// 非法 position 与预览产物 → 404。
	rec = renderRequest(t, env.r, http.MethodGet, "/api/v1/render-artifacts/artifact-frame/review-frames/bogus", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bogus position = %d %s", rec.Code, rec.Body.String())
	}
	rec = renderRequest(t, env.r, http.MethodGet, "/api/v1/render-artifacts/artifact-1/review-frames/first", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("preview frame = %d %s", rec.Code, rec.Body.String())
	}
}
