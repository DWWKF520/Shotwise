package publishing

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
)

// testPublishingKey 是测试用的 32 字节密钥（urlsafe base64，无填充）。
const testPublishingKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func seedPublishableArtifact(t *testing.T, gdb *gorm.DB, userID string) (string, string) {
	t.Helper()
	now := db.NewFlexibleTime(time.Now().UTC())
	planID := "plan-1"
	revisionID := "rev-1"
	artifactID := "artifact-1"
	snapshotID := "snapshot-1"

	if err := gdb.Create(&repository.AssemblyPlan{
		ID: planID, UserID: userID, ProjectName: "p1", Scope: "episode", Name: "plan",
		Status: "preview_ready", CurrentRevisionNumber: 1, CurrentSourceFingerprint: "sfp",
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if err := gdb.Create(&repository.AssemblyPlanRevision{
		ID: revisionID, PlanID: planID, VersionNumber: 1, SourceFingerprint: "sfp",
		SourceSnapshotJSON: "{}", TimelineJSON: "[]", AudioJSON: "{}", SubtitleJSON: "{}",
		PackagingJSON: "{}", OutputProfileJSON: "{}", ValidationJSON: "{}",
		CreatedBy: userID, CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed revision: %v", err)
	}
	if err := gdb.Create(&repository.RenderJob{
		ID: "job-1", PlanID: planID, ProjectName: "p1", UserID: userID, RevisionNumber: 1,
		Kind: "final", Status: "succeeded", Attempt: 1, MaxAttempts: 3, InputFingerprint: "ifp",
		CreatedAt: now, UpdatedAt: now, Progress: 1.0, ProgressStage: "done",
	}).Error; err != nil {
		t.Fatalf("seed render job: %v", err)
	}
	if err := gdb.Create(&repository.RenderArtifact{
		ID: artifactID, RenderJobID: "job-1", PlanID: planID, ProjectName: "p1", UserID: userID,
		RevisionNumber: 1, Kind: "final", RelativePath: "out.mp4", MimeType: "video/mp4",
		Fingerprint: "afp", CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	confirmedBy := userID
	if err := gdb.Create(&repository.RenderReviewSnapshot{
		ID: snapshotID, ArtifactID: artifactID, PlanID: planID, ProjectName: "p1", UserID: userID,
		RevisionNumber: 1, SourceFingerprint: "sfp", ArtifactFingerprint: "afp", Status: "ready",
		ChecksJSON: "{}", CreatedAt: now, ConfirmedBy: &confirmedBy, ConfirmedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	return artifactID, snapshotID
}

func TestPublishingPlatformCapabilities(t *testing.T) {
	capabilities := ListPlatformCapabilities()
	if len(capabilities) != 2 {
		t.Fatalf("expected 2 platforms, got %d", len(capabilities))
	}
	if capabilities[0].Platform != "douyin" || capabilities[1].Platform != "hongguo" {
		t.Fatalf("platform order = %v %v", capabilities[0].Platform, capabilities[1].Platform)
	}
	for _, capability := range capabilities {
		if capability.AdapterConnected || capability.SupportsPublish || capability.SupportsRetract {
			t.Fatalf("built-in registry must not claim a connected adapter: %+v", capability)
		}
		if capability.UnavailableReason == nil || *capability.UnavailableReason != "official_adapter_not_connected" {
			t.Fatalf("unavailable reason = %v", capability.UnavailableReason)
		}
	}
	if _, ok := GetPlatformCapabilities("抖音"); ok {
		t.Fatal("unknown platform must not resolve capabilities")
	}
}

func TestPublishingEncryptionRoundTrip(t *testing.T) {
	t.Setenv(PublishingKeyEnv, testPublishingKey)
	encrypted, err := EncryptPublishingSecret("oauth-token-123")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	decrypted, err := DecryptPublishingSecret(encrypted)
	if err != nil || decrypted != "oauth-token-123" {
		t.Fatalf("decrypt: %v %q", err, decrypted)
	}
	if encrypted == "oauth-token-123" {
		t.Fatal("ciphertext must differ from plaintext")
	}

	t.Setenv(PublishingKeyEnv, "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")
	if _, err := DecryptPublishingSecret(encrypted); err == nil {
		t.Fatal("decrypt with a different key must fail")
	}
}

func TestPublishingEncryptionMissingKey(t *testing.T) {
	t.Setenv(PublishingKeyEnv, "")
	if _, err := EncryptPublishingSecret("x"); err == nil {
		t.Fatal("missing key must fail closed")
	}
}

func TestPublishingAccountLifecycle(t *testing.T) {
	t.Setenv(PublishingKeyEnv, testPublishingKey)
	svc := NewPublishingService(newServiceTestDB(t))
	ctx := context.Background()
	user := repository.DefaultUserID

	account, err := svc.CreatePublishingAccount(ctx, CreatePublishingAccountParams{
		UserID: user, Platform: "Douyin", PlatformAccountID: "  acc-1  ", AccountName: " 主号 ",
		AccessToken: "token", RefreshToken: publishReasonPtr("refresh"), Scopes: []string{"b", "a", "a"},
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if account["platform"] != "douyin" || account["platform_account_id"] != "acc-1" || account["account_name"] != "主号" {
		t.Fatalf("account payload = %v", account)
	}
	scopes, _ := account["scopes"].([]string)
	if len(scopes) != 2 || scopes[0] != "a" || scopes[1] != "b" {
		t.Fatalf("scopes = %v", scopes)
	}

	accounts, err := svc.ListPublishingAccounts(ctx, user)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("list accounts: %v len=%d", err, len(accounts))
	}

	revoked, err := svc.RevokePublishingAccount(ctx, account["id"].(string), user)
	if err != nil || revoked["status"] != "revoked" {
		t.Fatalf("revoke: %v payload=%v", err, revoked)
	}
}

func TestPublishingAccountUnsupportedPlatform(t *testing.T) {
	svc := NewPublishingService(newServiceTestDB(t))
	_, err := svc.CreatePublishingAccount(context.Background(), CreatePublishingAccountParams{
		UserID: repository.DefaultUserID, Platform: "unknown", PlatformAccountID: "a", AccountName: "b", AccessToken: "c",
	})
	var validation *PublishValidationError
	if !errors.As(err, &validation) || validation.Code != "platform_not_supported" {
		t.Fatalf("want platform_not_supported, got %v", err)
	}
}

func TestPublishingJobLifecycle(t *testing.T) {
	gdb := newServiceTestDB(t)
	svc := NewPublishingService(gdb)
	ctx := context.Background()
	user := repository.DefaultUserID
	artifactID, snapshotID := seedPublishableArtifact(t, gdb, user)

	job, err := svc.CreatePublishJob(ctx, artifactID, CreatePublishJobParams{
		UserID: user, ReviewSnapshotID: snapshotID, Platform: "Douyin",
		IdempotencyKey: "idem-1", MaxAttempts: 3, Destination: map[string]any{"title": "t"},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if job["status"] != "queued" || job["platform"] != "douyin" {
		t.Fatalf("job payload = %v", job)
	}
	jobID := job["id"].(string)

	// 同一幂等键 + 同一指纹 → 复用同一条任务。
	again, err := svc.CreatePublishJob(ctx, artifactID, CreatePublishJobParams{
		UserID: user, ReviewSnapshotID: snapshotID, Platform: "douyin",
		IdempotencyKey: "idem-1", MaxAttempts: 3, Destination: map[string]any{"title": "t"},
	})
	if err != nil || again["id"] != jobID {
		t.Fatalf("idempotent reuse: %v %v", err, again["id"])
	}

	// 同一幂等键 + 不同指纹 → 冲突。
	var conflict *PublishConflictError
	if _, err := svc.CreatePublishJob(ctx, artifactID, CreatePublishJobParams{
		UserID: user, ReviewSnapshotID: snapshotID, Platform: "douyin",
		IdempotencyKey: "idem-1", MaxAttempts: 5,
	}); !errors.As(err, &conflict) || conflict.Code != "idempotency_key_reused" {
		t.Fatalf("want idempotency_key_reused, got %v", err)
	}

	// queued 状态轮询 / 回撤都因状态不符被拒。
	if _, err := svc.PollPublishJob(ctx, jobID, user); !errors.As(err, &conflict) || conflict.Code != "poll_requires_submitted" {
		t.Fatalf("want poll_requires_submitted, got %v", err)
	}
	if _, err := svc.RetractPublishJob(ctx, jobID, user); !errors.As(err, &conflict) || conflict.Code != "retract_requires_published" {
		t.Fatalf("want retract_requires_published, got %v", err)
	}
	// queued 状态下不允许重试。
	if _, err := svc.RetryPublishJob(ctx, jobID, user); !errors.As(err, &conflict) || conflict.Code != "retry_requires_failed" {
		t.Fatalf("want retry_requires_failed, got %v", err)
	}

	// 取消后状态为 canceled。
	canceled, err := svc.CancelPublishJob(ctx, jobID, user)
	if err != nil || canceled["status"] != "canceled" {
		t.Fatalf("cancel: %v payload=%v", err, canceled)
	}
	if _, err := svc.CancelPublishJob(ctx, jobID, user); !errors.As(err, &conflict) {
		t.Fatalf("second cancel must conflict, got %v", err)
	}
}

func TestPublishingJobBindingRejectsNonFinalArtifact(t *testing.T) {
	gdb := newServiceTestDB(t)
	svc := NewPublishingService(gdb)
	ctx := context.Background()
	user := repository.DefaultUserID
	artifactID, snapshotID := seedPublishableArtifact(t, gdb, user)

	if err := gdb.Exec("UPDATE render_artifacts SET kind = 'preview' WHERE id = ?", artifactID).Error; err != nil {
		t.Fatal(err)
	}
	var conflict *PublishConflictError
	_, err := svc.CreatePublishJob(ctx, artifactID, CreatePublishJobParams{
		UserID: user, ReviewSnapshotID: snapshotID, Platform: "douyin", IdempotencyKey: "idem-2", MaxAttempts: 3,
	})
	if !errors.As(err, &conflict) || conflict.Code != "final_artifact_required" {
		t.Fatalf("want final_artifact_required, got %v", err)
	}
}

func TestPublishingListJobsPagination(t *testing.T) {
	gdb := newServiceTestDB(t)
	svc := NewPublishingService(gdb)
	ctx := context.Background()
	user := repository.DefaultUserID
	artifactID, snapshotID := seedPublishableArtifact(t, gdb, user)

	for _, key := range []string{"k1", "k2", "k3"} {
		if _, err := svc.CreatePublishJob(ctx, artifactID, CreatePublishJobParams{
			UserID: user, ReviewSnapshotID: snapshotID, Platform: "douyin", IdempotencyKey: key, MaxAttempts: 3,
		}); err != nil {
			t.Fatalf("create job %s: %v", key, err)
		}
	}
	result, err := svc.ListPublishJobs(ctx, user, 2, 0)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if result["total"] != int64(3) || result["limit"] != 2 {
		t.Fatalf("list payload = %v", result)
	}
	if items, _ := result["items"].([]map[string]any); len(items) != 2 {
		t.Fatalf("page size = %v", result["items"])
	}
}
