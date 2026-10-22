package media

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/migrations"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newAssemblyPlanTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "assembly.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open temp sqlite: %v", err)
	}
	if _, err := db.Migrate(gdb, dialect, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := gdb.Exec(
		"INSERT INTO users (id, username, role, is_active, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		repository.DefaultUserID, "default", "user", true, "2026-01-01T00:00:00+00:00", "2026-01-01T00:00:00+00:00",
	).Error; err != nil {
		t.Fatalf("seed default user: %v", err)
	}
	t.Cleanup(func() {
		if pool, err := gdb.DB(); err == nil {
			_ = pool.Close()
		}
	})
	return gdb
}

func assemblyTestManifest() map[string]any {
	return map[string]any{
		"contract_version": "episode-media-manifest/v1",
		"persistence":      "read_only",
		"script":           "episode_1.json",
		"episode":          1,
		"content_mode":     "drama",
		"generation_mode":  "storyboard",
		"collection":       "scenes",
		"fingerprint":      "manifest-fingerprint",
		"items": []any{
			map[string]any{
				"order": 1, "unit_id": "E1U1", "planned_duration_seconds": 5.0,
				"source":      map[string]any{"path": "videos/E1U1.mp4", "kind": "generated_video"},
				"media_probe": map[string]any{"exists": true, "probe_available": false, "has_video_stream": nil},
			},
		},
	}
}

func assemblyTestTimeline() []map[string]any {
	return []map[string]any{{
		"id": "E1U1:0", "source_unit_id": "E1U1", "order": 0, "kind": "video_unit",
		"source_ref": "videos/E1U1.mp4", "duration_seconds": 5.0,
		"trim_start_seconds": 0.0, "trim_end_seconds": 0.0,
		"audio_policy": "duck", "transition": nil,
	}}
}

func createAssemblyPlan(t *testing.T, svc *MediaAssemblyService) map[string]any {
	t.Helper()
	episode := int64(1)
	payload, err := svc.CreatePlan(context.Background(), CreatePlanParams{
		UserID:         repository.DefaultUserID,
		ProjectName:    "p1",
		Name:           "episode_1.json assembly",
		Scope:          "episode",
		EpisodeNumber:  &episode,
		SourceManifest: assemblyTestManifest(),
		Timeline:       assemblyTestTimeline(),
		Audio:          map[string]any{"unit_audio_policy": "duck", "tracks": []any{}, "ducking_db": -12.0},
		Subtitle:       map[string]any{"mode": "burn_in", "cues": []any{}, "source": "script"},
		Packaging:      map[string]any{"cover": map[string]any{"enabled": false}},
		OutputProfile:  map[string]any{"format": "mp4"},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return payload
}

func TestMediaAssemblyPlanLifecycle(t *testing.T) {
	svc := NewMediaAssemblyService(newAssemblyPlanTestDB(t))
	ctx := context.Background()

	created := createAssemblyPlan(t, svc)
	planID, _ := created["id"].(string)
	if planID == "" {
		t.Fatalf("plan id missing: %v", created)
	}
	if created["status"] != "draft" || created["current_revision_number"] != int64(1) {
		t.Fatalf("created plan = %v", created)
	}

	got, err := svc.GetPlan(ctx, planID, repository.DefaultUserID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	revision, _ := got["current_revision"].(map[string]any)
	if revision["version_number"] != int64(1) {
		t.Fatalf("revision = %v", revision)
	}
	snapshot, _ := revision["source_snapshot"].(map[string]any)
	if snapshot["manifest_fingerprint"] != "manifest-fingerprint" {
		t.Fatalf("source_snapshot = %v", snapshot)
	}
}

func TestMediaAssemblyCreateRevisionAdvanceAndValidationMetadata(t *testing.T) {
	svc := NewMediaAssemblyService(newAssemblyPlanTestDB(t))
	ctx := context.Background()
	created := createAssemblyPlan(t, svc)
	planID, _ := created["id"].(string)
	expected := int64(1)

	validation := map[string]any{"valid": false, "issues": []any{map[string]any{"code": "director_review_required"}}}
	payload, err := svc.CreateRevision(ctx, planID, CreateRevisionParams{
		UserID:             repository.DefaultUserID,
		SourceManifest:     assemblyTestManifest(),
		Timeline:           assemblyTestTimeline(),
		Audio:              map[string]any{"unit_audio_policy": "duck", "tracks": []any{}},
		Subtitle:           map[string]any{"cues": []any{}},
		Packaging:          map[string]any{"cover": map[string]any{"enabled": false}},
		OutputProfile:      map[string]any{"format": "mp4"},
		ValidationMetadata: validation,
		ExpectedRevision:   &expected,
	})
	if err != nil {
		t.Fatalf("create revision: %v", err)
	}
	if payload["current_revision_number"] != int64(2) || payload["status"] != "draft" {
		t.Fatalf("revision payload = %v", payload)
	}
	revision, _ := payload["current_revision"].(map[string]any)
	stored, _ := revision["validation"].(map[string]any)
	if stored["valid"] != false {
		t.Fatalf("validation metadata not persisted: %v", stored)
	}
}

func TestMediaAssemblyCreateRevisionConflicts(t *testing.T) {
	gdb := newAssemblyPlanTestDB(t)
	svc := NewMediaAssemblyService(gdb)
	ctx := context.Background()
	created := createAssemblyPlan(t, svc)
	planID, _ := created["id"].(string)

	stale := int64(9)
	_, err := svc.CreateRevision(ctx, planID, CreateRevisionParams{
		UserID: repository.DefaultUserID, ExpectedRevision: &stale,
	})
	if !isConflictCode(err, "revision_conflict") {
		t.Fatalf("stale expected_revision must conflict, got %v", err)
	}

	if err := gdb.Exec("UPDATE assembly_plans SET status = 'rendering' WHERE id = ?", planID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRevision(ctx, planID, CreateRevisionParams{UserID: repository.DefaultUserID}); !isConflictCode(err, "plan_busy") {
		t.Fatalf("running plan must be busy, got %v", err)
	}
}

func TestMediaAssemblyGetPlanNotFound(t *testing.T) {
	svc := NewMediaAssemblyService(newAssemblyPlanTestDB(t))
	_, err := svc.GetPlan(context.Background(), "missing", repository.DefaultUserID)
	var notFound *AssemblyPlanNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("want AssemblyPlanNotFoundError, got %v", err)
	}
	if notFound.Error() != "missing" {
		t.Fatalf("not-found message = %q", notFound.Error())
	}
}

func TestMediaAssemblyCreatePlanRejectsInvalidDocument(t *testing.T) {
	svc := NewMediaAssemblyService(newAssemblyPlanTestDB(t))
	_, err := svc.CreatePlan(context.Background(), CreatePlanParams{
		UserID:         repository.DefaultUserID,
		ProjectName:    "p1",
		Name:           "bad",
		Scope:          "episode",
		SourceManifest: assemblyTestManifest(),
		Timeline:       nil,
	})
	var validationErr *domain.AssemblyPlanValidationError
	if !errors.As(err, &validationErr) || len(validationErr.Errors) == 0 {
		t.Fatalf("empty timeline must fail validation, got %v", err)
	}
}

func TestMediaAssemblyTransitionAndConfirmation(t *testing.T) {
	svc := NewMediaAssemblyService(newAssemblyPlanTestDB(t))
	ctx := context.Background()
	created := createAssemblyPlan(t, svc)
	planID, _ := created["id"].(string)

	// draft 不能直接跳到 render_pending。
	if _, err := svc.TransitionPlan(ctx, planID, repository.DefaultUserID, "render_pending"); !isConflictCode(err, "invalid_status_transition") {
		t.Fatalf("want invalid_status_transition, got %v", err)
	}
	// draft → confirmed 合法。
	confirmed, err := svc.TransitionPlan(ctx, planID, repository.DefaultUserID, "confirmed")
	if err != nil || confirmed["status"] != "confirmed" {
		t.Fatalf("transition to confirmed: %v payload=%v", err, confirmed)
	}
	// confirmed → preview_pending 合法。
	if _, err := svc.TransitionPlan(ctx, planID, repository.DefaultUserID, "preview_pending"); err != nil {
		t.Fatalf("transition to preview_pending: %v", err)
	}
	// 预览指针尚未落到该 revision 时确认预览 → revision_conflict（Python 口径：先校验 revision）。
	if _, err := svc.ConfirmPreview(ctx, planID, repository.DefaultUserID, 1); !isConflictCode(err, "revision_conflict") {
		t.Fatalf("want revision_conflict, got %v", err)
	}
	if _, err := svc.MarkPreviewReady(ctx, planID, repository.DefaultUserID, 1); err != nil {
		t.Fatalf("mark preview ready: %v", err)
	}
	if _, err := svc.ConfirmPreview(ctx, planID, repository.DefaultUserID, 1); err != nil {
		t.Fatalf("confirm preview: %v", err)
	}
	// 未确认预览前不能确认渲染 —— 这里已确认，故应成功并推进到 render_pending。
	rendered, err := svc.ConfirmRender(ctx, planID, repository.DefaultUserID, 1)
	if err != nil || rendered["status"] != "render_pending" {
		t.Fatalf("confirm render: %v payload=%v", err, rendered)
	}
}

func TestMediaAssemblyConfirmRenderRequiresPreviewConfirmation(t *testing.T) {
	gdb := newAssemblyPlanTestDB(t)
	svc := NewMediaAssemblyService(gdb)
	ctx := context.Background()
	created := createAssemblyPlan(t, svc)
	planID, _ := created["id"].(string)
	// 直接构造 preview_ready 但未确认预览的中间态。
	if err := gdb.Exec(
		"UPDATE assembly_plans SET status = 'preview_ready', preview_revision_number = 1 WHERE id = ?", planID,
	).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmRender(ctx, planID, repository.DefaultUserID, 1); !isConflictCode(err, "preview_confirmation_required") {
		t.Fatalf("want preview_confirmation_required, got %v", err)
	}
}

func TestMediaAssemblyCheckStale(t *testing.T) {
	gdb := newAssemblyPlanTestDB(t)
	svc := NewMediaAssemblyService(gdb)
	ctx := context.Background()
	created := createAssemblyPlan(t, svc)
	planID, _ := created["id"].(string)

	// 同一份源快照应判为不 stale，且状态不变。
	fresh, err := svc.CheckStale(ctx, planID, repository.DefaultUserID, assemblyTestManifest())
	if err != nil || fresh["stale"] != false {
		t.Fatalf("same snapshot should not be stale: %v %v", err, fresh["stale"])
	}
	// 改过的源快照指纹不同 → stale，并把状态翻成 stale。
	changed := assemblyTestManifest()
	changed["fingerprint"] = "changed-fingerprint"
	stalePayload, err := svc.CheckStale(ctx, planID, repository.DefaultUserID, changed)
	if err != nil || stalePayload["stale"] != true {
		t.Fatalf("changed snapshot should be stale: %v %v", err, stalePayload["stale"])
	}
	if stalePayload["status"] != "stale" {
		t.Fatalf("plan status should flip to stale, got %v", stalePayload["status"])
	}
}

func isConflictCode(err error, code string) bool {
	var conflict *AssemblyPlanConflictError
	return errors.As(err, &conflict) && conflict.Code == code
}
