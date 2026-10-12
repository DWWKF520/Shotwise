package repository

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/migrations"
)

func newTestRepo(t *testing.T) (*TaskRepo, *gorm.DB) {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "t.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open temp sqlite: %v", err)
	}
	if _, err := db.Migrate(gdb, dialect, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// tasks.user_id 有到 users 的外键；Python 侧运行时保证 default 用户存在。
	if err := gdb.Exec(
		"INSERT INTO users (id, username, role, is_active, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		DefaultUserID, "default", "user", true, "2026-01-01T00:00:00+00:00", "2026-01-01T00:00:00+00:00",
	).Error; err != nil {
		t.Fatalf("seed default user: %v", err)
	}
	t.Cleanup(func() {
		if pool, err := gdb.DB(); err == nil {
			_ = pool.Close()
		}
	})
	return NewTaskRepo(gdb), gdb
}

func ptr[T any](v T) *T { return &v }

func enqueueForTest(t *testing.T, repo *TaskRepo, project, taskType, media, resourceID string, opts ...func(*EnqueueParams)) EnqueueResult {
	t.Helper()
	p := EnqueueParams{
		ProjectName: project,
		TaskType:    taskType,
		MediaType:   media,
		ResourceID:  resourceID,
		Payload:     map[string]any{"prompt": "a cat"},
	}
	for _, opt := range opts {
		opt(&p)
	}
	res, err := repo.Enqueue(context.Background(), p)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return res
}

func withProvider(providerID string) func(*EnqueueParams) {
	return func(p *EnqueueParams) { p.ProviderID = &providerID }
}

func withDependency(taskID string) func(*EnqueueParams) {
	return func(p *EnqueueParams) { p.DependencyTaskID = &taskID }
}

func withScriptFile(name string) func(*EnqueueParams) {
	return func(p *EnqueueParams) { p.ScriptFile = &name }
}

func statusOf(t *testing.T, repo *TaskRepo, taskID string) string {
	t.Helper()
	data, err := repo.Get(context.Background(), taskID)
	if err != nil {
		t.Fatalf("get %s: %v", taskID, err)
	}
	if data == nil {
		t.Fatalf("task %s not found", taskID)
	}
	return data.Status
}

func TestEnqueueDedupeOnActiveStatusOnly(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	first := enqueueForTest(t, repo, "p1", "image", "image", "s1")
	if first.Deduped {
		t.Fatal("first enqueue should not dedupe")
	}
	second := enqueueForTest(t, repo, "p1", "image", "image", "s1")
	if !second.Deduped || second.TaskID != first.TaskID || second.ExistingTaskID == nil || *second.ExistingTaskID != first.TaskID {
		t.Fatalf("second enqueue = %+v, want deduped onto %s", second, first.TaskID)
	}
	// 不同资源不互相去重。
	other := enqueueForTest(t, repo, "p1", "image", "image", "s2")
	if other.Deduped {
		t.Fatal("different resource should not dedupe")
	}

	// 终态释放去重键：worker 认领后成功即解锁，同键可再次入队。
	if claimed, err := repo.ClaimNext(ctx, "image", nil); err != nil || claimed == nil || claimed.TaskID != first.TaskID {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if _, _, err := repo.MarkSucceeded(ctx, first.TaskID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("mark succeeded: %v", err)
	}
	third := enqueueForTest(t, repo, "p1", "image", "image", "s1")
	if third.Deduped {
		t.Fatal("terminal task must not occupy the dedupe key")
	}
}

func TestEnqueueDedupeSeparatesScriptFileAndResourceType(t *testing.T) {
	repo, _ := newTestRepo(t)
	a := enqueueForTest(t, repo, "p1", "image", "image", "s1", withScriptFile("ep1.json"))
	b := enqueueForTest(t, repo, "p1", "image", "image", "s1", withScriptFile("ep2.json"))
	if b.Deduped || b.TaskID == a.TaskID {
		t.Fatal("different script_file must not share the dedupe key")
	}
}

func TestClaimNextFIFOWithDependencyGuard(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	root := enqueueForTest(t, repo, "p1", "storyboard", "image", "seg1")
	child := enqueueForTest(t, repo, "p1", "video", "video", "seg1", withDependency(root.TaskID))

	claimed, err := repo.ClaimNext(ctx, "image", nil)
	if err != nil || claimed == nil {
		t.Fatalf("claim image = %v, %v", claimed, err)
	}
	if claimed.TaskID != root.TaskID {
		t.Fatalf("claimed %s, want root %s", claimed.TaskID, root.TaskID)
	}
	// 根任务在跑、尚未 succeeded：依赖它的视频不能被认领。
	if got, err := repo.ClaimNext(ctx, "video", nil); err != nil || got != nil {
		t.Fatalf("claim blocked dependency = %v, %v; want nil", got, err)
	}
	// 终态 succeeded 后下游解除阻塞。
	if _, _, err := repo.MarkSucceeded(ctx, root.TaskID, nil); err != nil {
		t.Fatalf("mark succeeded: %v", err)
	}
	got, err := repo.ClaimNext(ctx, "video", nil)
	if err != nil || got == nil || got.TaskID != child.TaskID {
		t.Fatalf("claim after dependency = %v, %v", got, err)
	}
}

func TestClaimNextPoolFullBlacklistKeepsNullProvider(t *testing.T) {
	repo, gdb := newTestRepo(t)
	ctx := context.Background()

	pooled := enqueueForTest(t, repo, "p1", "image", "image", "s1", withProvider("p-full"))
	legacy := enqueueForTest(t, repo, "p1", "image", "image", "s2")
	// 让池满任务排在最前，验证黑名单语义：NULL provider 的老数据不被过滤。
	if err := gdb.Exec("UPDATE tasks SET queued_at = ? WHERE task_id = ?", "2000-01-01T00:00:00+00:00", pooled.TaskID).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err := repo.ClaimNext(ctx, "image", []string{"p-full"})
	if err != nil || claimed == nil {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if claimed.TaskID != legacy.TaskID {
		t.Fatalf("claimed %s, want legacy NULL-provider %s", claimed.TaskID, legacy.TaskID)
	}
	// 未知 provider（不在池满集合里）照常可认领。
	unknown := enqueueForTest(t, repo, "p1", "image", "image", "s3", withProvider("p-unknown-elsewhere"))
	claimed, err = repo.ClaimNext(ctx, "image", []string{"p-full"})
	if err != nil || claimed == nil || claimed.TaskID != unknown.TaskID {
		t.Fatalf("claim unknown provider = %v, %v", claimed, err)
	}
}

func TestMarkZeroRowsMeansExternalFlip(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()
	task := enqueueForTest(t, repo, "p1", "image", "image", "s1")
	if _, err := repo.ClaimNext(ctx, "image", nil); err != nil {
		t.Fatal(err)
	}
	// 外部（取消路径）先把它翻成 cancelling。
	if _, err := repo.CancelTask(ctx, task.TaskID); err != nil {
		t.Fatal(err)
	}
	rows, event, err := repo.MarkSucceeded(ctx, task.TaskID, nil)
	if err != nil || rows != 0 || event != nil {
		t.Fatalf("mark succeeded after external flip = %d, %v, %v; want 0 rows, no event", rows, event, err)
	}
	rows, events, err := repo.MarkFailed(ctx, task.TaskID, "boom")
	if err != nil || rows != 0 || len(events) != 0 {
		t.Fatalf("mark failed after external flip = %d, %v, %v; want 0 rows, no events", rows, events, err)
	}
	if got := statusOf(t, repo, task.TaskID); got != TaskStatusCancelling {
		t.Fatalf("status = %q, want cancelling untouched", got)
	}
}

func TestMarkFailedCascadesWithCollapsedReason(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()
	root := enqueueForTest(t, repo, "p1", "image", "image", "s1")
	mid := enqueueForTest(t, repo, "p1", "image", "image", "s2", withDependency(root.TaskID))
	leaf := enqueueForTest(t, repo, "p1", "image", "image", "s3", withDependency(mid.TaskID))
	if _, err := repo.ClaimNext(ctx, "image", nil); err != nil {
		t.Fatal(err)
	}

	rows, events, err := repo.MarkFailed(ctx, root.TaskID, "provider exploded: Authorization: Bearer sk-secret123")
	if err != nil || rows != 1 {
		t.Fatalf("mark failed = %d, %v", rows, err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3 (root + 2 cascaded)", len(events))
	}
	for _, id := range []string{mid.TaskID, leaf.TaskID} {
		if got := statusOf(t, repo, id); got != TaskStatusFailed {
			t.Fatalf("cascaded %s status = %q", id, got)
		}
	}
	// 凭证清洗：原始串不进任何落库行。
	rootData, _ := repo.Get(ctx, root.TaskID)
	if rootData.ErrorMessage == nil || strings.Contains(*rootData.ErrorMessage, "sk-secret123") {
		t.Fatalf("root error_message leaked credential: %v", rootData.ErrorMessage)
	}
	// mid 的信封：code 正确、dependency 指向 root、内层 reason 是根因（不是再包一层）。
	midData, _ := repo.Get(ctx, mid.TaskID)
	code, params, ok := domain.ParseStructuredFailure(*midData.ErrorMessage)
	if !ok || code != domain.CascadeFailureCode {
		t.Fatalf("mid error = %q, want cascade envelope", *midData.ErrorMessage)
	}
	if params["dependency_task_id"] != root.TaskID {
		t.Fatalf("mid dependency = %v, want %s", params["dependency_task_id"], root.TaskID)
	}
	nested, _ := params["reason"].(string)
	if strings.Contains(nested, domain.CascadeFailureCode) {
		t.Fatalf("cascade reason should be collapsed to root cause, got %q", nested)
	}
	// leaf 的信封指向 mid，但内层同样是根因。
	leafData, _ := repo.Get(ctx, leaf.TaskID)
	_, leafParams, ok := domain.ParseStructuredFailure(*leafData.ErrorMessage)
	if !ok || leafParams["dependency_task_id"] != mid.TaskID {
		t.Fatalf("leaf envelope = %q", *leafData.ErrorMessage)
	}
}

func TestCancelQueuedCascadesAndMarksCancelledBy(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()
	root := enqueueForTest(t, repo, "p1", "image", "image", "s1")
	child := enqueueForTest(t, repo, "p1", "image", "image", "s2", withDependency(root.TaskID))

	res, err := repo.CancelTask(ctx, root.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cancelled) != 2 || len(res.Cancelling) != 0 || len(res.SkippedTerminal) != 0 {
		t.Fatalf("cancel result = %+v", res)
	}
	if res.Cancelled[0].TaskID != root.TaskID {
		t.Fatal("parent must precede children in cancelled list")
	}
	childData, _ := repo.Get(ctx, child.TaskID)
	if *childData.CancelledBy != "cascade" {
		t.Fatalf("child cancelled_by = %v, want cascade", childData.CancelledBy)
	}
	// 幂等：终态再取消进 skipped_terminal。
	res, err = repo.CancelTask(ctx, root.TaskID)
	if err != nil || len(res.SkippedTerminal) != 1 {
		t.Fatalf("re-cancel = %+v, %v", res, err)
	}
}

func TestCancelRunningGoesCancellingThenFinalize(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()
	task := enqueueForTest(t, repo, "p1", "video", "video", "s1")
	child := enqueueForTest(t, repo, "p1", "image", "image", "s2", withDependency(task.TaskID))
	if _, err := repo.ClaimNext(ctx, "video", nil); err != nil {
		t.Fatal(err)
	}

	res, err := repo.CancelTask(ctx, task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cancelling) != 1 || len(res.Cancelled) != 0 {
		t.Fatalf("running cancel = %+v, want only cancelling", res)
	}
	// running 父未终止前，下游 queued 不被立即级联。
	if got := statusOf(t, repo, child.TaskID); got != TaskStatusQueued {
		t.Fatalf("child status = %q, want queued until parent finalizes", got)
	}
	fin, err := repo.FinalizeCancelled(ctx, task.TaskID, "user")
	if err != nil || fin.Rows != 1 {
		t.Fatalf("finalize = %+v, %v", fin, err)
	}
	parent, _ := repo.Get(ctx, task.TaskID)
	if parent.Status != TaskStatusCancelled || *parent.CancelledBy != "user" {
		t.Fatalf("parent after finalize = %+v", parent)
	}
	if got := statusOf(t, repo, child.TaskID); got != TaskStatusCancelled {
		t.Fatalf("child after parent finalize = %q, want cancelled", got)
	}
}

func TestFinalizeCancellingBatchFallsBackWithoutWorker(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()
	a := enqueueForTest(t, repo, "p1", "image", "image", "s1")
	b := enqueueForTest(t, repo, "p1", "image", "image", "s2")
	if _, err := repo.ClaimNext(ctx, "image", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ClaimNext(ctx, "image", nil); err != nil {
		t.Fatal(err)
	}
	res, err := repo.CancelAllActive(ctx, "p1")
	if err != nil || len(res.Cancelling) != 2 {
		t.Fatalf("cancel all = %+v, %v", res, err)
	}
	done, err := repo.FinalizeCancelling(ctx, []string{a.TaskID, b.TaskID}, "user")
	if err != nil || len(done) != 2 {
		t.Fatalf("finalize batch = %d, %v", len(done), err)
	}
	if got := statusOf(t, repo, a.TaskID); got != TaskStatusCancelled {
		t.Fatalf("status = %q", got)
	}
}

func TestOrphanScanAndRequeueRunning(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()
	running := enqueueForTest(t, repo, "p1", "image", "image", "s1")
	other := enqueueForTest(t, repo, "p1", "video", "video", "s2")
	if _, err := repo.ClaimNext(ctx, "image", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ClaimNext(ctx, "video", nil); err != nil {
		t.Fatal(err)
	}
	orphans, err := repo.ListOrphanTasksOnStart(ctx)
	if err != nil || len(orphans) != 2 {
		t.Fatalf("orphans = %d, %v", len(orphans), err)
	}
	n, err := repo.RequeueRunning(ctx, 10)
	if err != nil || n != 2 {
		t.Fatalf("requeue = %d, %v", n, err)
	}
	for _, id := range []string{running.TaskID, other.TaskID} {
		if got := statusOf(t, repo, id); got != TaskStatusQueued {
			t.Fatalf("requeued %s = %q", id, got)
		}
	}
}

func TestPersistProviderJobIDAndExecutionIdentity(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()
	task := enqueueForTest(t, repo, "p1", "video", "video", "s1")
	if _, err := repo.ClaimNext(ctx, "video", nil); err != nil {
		t.Fatal(err)
	}
	endpoint := "https://api.example.com"
	if err := repo.PersistProviderJobID(ctx, task.TaskID, "job-1", &endpoint); err != nil {
		t.Fatal(err)
	}
	if err := repo.PersistAPICallID(ctx, task.TaskID, 42); err != nil {
		t.Fatal(err)
	}
	if err := repo.PersistExecutionIdentity(ctx, task.TaskID, "p-real", map[string]any{
		"video_provider_i2v": "p-real/model-x",
		"warnings":           nil,
	}); err != nil {
		t.Fatal(err)
	}
	data, _ := repo.Get(ctx, task.TaskID)
	if data.ProviderJobID == nil || *data.ProviderJobID != "job-1" {
		t.Fatalf("provider_job_id = %v", data.ProviderJobID)
	}
	if data.ProviderEndpoint == nil || *data.ProviderEndpoint != endpoint {
		t.Fatalf("provider_endpoint = %v", data.ProviderEndpoint)
	}
	if data.ProviderID == nil || *data.ProviderID != "p-real" {
		t.Fatalf("provider_id = %v", data.ProviderID)
	}
	if data.Payload["api_call_id"] != float64(42) {
		t.Fatalf("payload api_call_id = %v", data.Payload["api_call_id"])
	}
	if data.Payload["video_provider_i2v"] != "p-real/model-x" {
		t.Fatalf("payload patch lost: %v", data.Payload)
	}
	// PersistAPICallID 对不存在的任务必须报错（fail-fast，防静默以为已持久化）。
	if err := repo.PersistAPICallID(ctx, "missing", 1); err == nil {
		t.Fatal("persist api_call_id on missing task must fail")
	}
}

func TestEnqueueBatchRollsBackWholeAdmission(t *testing.T) {
	repo, gdb := newTestRepo(t)
	ctx := context.Background()
	dup := enqueueForTest(t, repo, "p1", "image", "image", "s1")

	_, err := repo.EnqueueBatch(ctx, []EnqueueParams{
		{ProjectName: "p1", TaskType: "video", MediaType: "video", ResourceID: "v1", Payload: map[string]any{}},
		{ProjectName: "p1", TaskType: "image", MediaType: "image", ResourceID: "s1", Payload: map[string]any{}},
	})
	if err == nil {
		t.Fatal("batch with conflicting dedupe key must fail")
	}
	var count int64
	if err := gdb.Model(&Task{}).Where("task_type = ?", "video").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("batch rollback left %d rows", count)
	}
	var total int64
	if err := gdb.Model(&Task{}).Count(&total).Error; err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("total rows = %d, want only the pre-existing task", total)
	}
	_ = dup
}

func TestLeaseLifecycleAndContention(t *testing.T) {
	repo, gdb := newTestRepo(t)
	ctx := context.Background()

	ok, err := repo.AcquireOrRenewLease(ctx, "default", "worker-a", 10)
	if err != nil || !ok {
		t.Fatalf("acquire = %v, %v", ok, err)
	}
	// 续约（fast path）。
	ok, err = repo.AcquireOrRenewLease(ctx, "default", "worker-a", 10)
	if err != nil || !ok {
		t.Fatalf("renew = %v, %v", ok, err)
	}
	// 竞争：B 不能抢未过期的租约。
	ok, err = repo.AcquireOrRenewLease(ctx, "default", "worker-b", 10)
	if err != nil || ok {
		t.Fatalf("contention = %v, %v; want false", ok, err)
	}
	online, err := repo.IsWorkerOnline(ctx, "default")
	if err != nil || !online {
		t.Fatalf("online = %v, %v", online, err)
	}
	info, err := repo.GetWorkerLease(ctx, "default")
	if err != nil || info == nil || info.OwnerID != "worker-a" || !info.IsOnline {
		t.Fatalf("lease info = %+v, %v", info, err)
	}
	// 过期后 B 可接管。
	if err := gdb.Exec("UPDATE worker_lease SET lease_until = 0 WHERE name = ?", "default").Error; err != nil {
		t.Fatal(err)
	}
	ok, err = repo.AcquireOrRenewLease(ctx, "default", "worker-b", 10)
	if err != nil || !ok {
		t.Fatalf("takeover = %v, %v", ok, err)
	}
	// 释放是 owner-scoped：A 释放不了 B 的租约。
	if err := repo.ReleaseLease(ctx, "default", "worker-a"); err != nil {
		t.Fatal(err)
	}
	online, _ = repo.IsWorkerOnline(ctx, "default")
	if !online {
		t.Fatal("owner-scoped release must not drop another owner's lease")
	}
	if err := repo.ReleaseLease(ctx, "default", "worker-b"); err != nil {
		t.Fatal(err)
	}
	online, _ = repo.IsWorkerOnline(ctx, "default")
	if online {
		t.Fatal("lease should be gone")
	}
}

func TestCancelEpisodeTasksScopesByScriptFile(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()
	enqueueForTest(t, repo, "p1", "image", "image", "s1", withScriptFile("ep1.json"))
	enqueueForTest(t, repo, "p1", "image", "image", "s2", withScriptFile("ep2.json"))
	res, err := repo.CancelEpisodeTasks(ctx, "p1", "ep1.json")
	if err != nil || len(res.Cancelled) != 1 {
		t.Fatalf("episode cancel = %+v, %v", res, err)
	}
	if res.Cancelled[0].ResourceID != "s1" {
		t.Fatalf("cancelled wrong episode task: %+v", res.Cancelled[0])
	}
	if *res.Cancelled[0].CancelledBy != "episode_deleted" {
		t.Fatalf("cancelled_by = %v", res.Cancelled[0].CancelledBy)
	}
}
