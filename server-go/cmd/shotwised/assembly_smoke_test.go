package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/config"
	domainproject "github.com/shotwise/shotwise/internal/domain/project"
	infradb "github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/pricing"
	"github.com/shotwise/shotwise/internal/queue"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/generation"
	"github.com/shotwise/shotwise/migrations"
)

// P4 端到端冒烟：按 cmd/shotwised 的装配方式把 queue + worker + 全部钩子接起来，
// 验证「批量生成 / 取消 / 断点续传」三条链路在真实 DB 与真实钩子实现下走得通。
//
// 执行层用替身而非 TaskDispatcher：真实 dispatcher 需要供应商凭据与网络。
// 各 task_type 的真执行路径另有 tasks_*_test.go 覆盖，本测试只证明装配与编排闭环。

// smokeExecutor 是可编排的替身：按 resource_id 决定阻塞或失败。
type smokeExecutor struct {
	mu      sync.Mutex
	block   map[string]chan struct{}
	failOn  map[string]error
	callsMu sync.Mutex
	calls   []string
}

func (e *smokeExecutor) Execute(ctx context.Context, task *repository.TaskData) (map[string]any, error) {
	e.callsMu.Lock()
	e.calls = append(e.calls, task.ResourceID)
	e.callsMu.Unlock()

	e.mu.Lock()
	release := e.block[task.ResourceID]
	err := e.failOn[task.ResourceID]
	e.mu.Unlock()

	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "resource_id": task.ResourceID}, nil
}

func (e *smokeExecutor) called() []string {
	e.callsMu.Lock()
	defer e.callsMu.Unlock()
	return append([]string(nil), e.calls...)
}

func openSmokeDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, dialect, err := infradb.Open("sqlite://"+filepath.Join(t.TempDir(), "smoke.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := infradb.Migrate(gdb, dialect, migrations.FS()); err != nil {
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

// waitForTaskStatus 轮询到任务落到期望状态。
func waitForTaskStatus(t *testing.T, gdb *gorm.DB, taskID, want string) *repository.TaskData {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		task, err := repository.NewTaskRepo(gdb).Get(context.Background(), taskID)
		if err == nil && task != nil && task.Status == want {
			return task
		}
		time.Sleep(20 * time.Millisecond)
	}
	task, _ := repository.NewTaskRepo(gdb).Get(context.Background(), taskID)
	got := "<nil>"
	if task != nil {
		got = task.Status
	}
	t.Fatalf("task %s 未在限时内变为 %s（当前 %s）", taskID, want, got)
	return nil
}

// waitForCallStatus 轮询到记账行落到期望状态。
func waitForCallStatus(t *testing.T, gdb *gorm.DB, callID int64, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var row repository.APICall
		if err := gdb.Where("id = ?", callID).First(&row).Error; err == nil && row.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	var row repository.APICall
	_ = gdb.Where("id = ?", callID).First(&row).Error
	t.Fatalf("api_call %d 未在限时内变为 %s（当前 %s）", callID, want, row.Status)
}

// seedPendingCall 插入一条指定发起时刻的 pending 记账行，返回其 id。
func seedPendingCall(t *testing.T, gdb *gorm.DB, project string, startedAt time.Time) int64 {
	t.Helper()
	row := repository.APICall{
		ProjectName: project,
		CallType:    "audio",
		Model:       "smoke-model",
		Status:      "pending",
		Provider:    "p1",
		UserID:      repository.DefaultUserID,
		StartedAt:   infradb.NewFlexibleTime(startedAt),
		CreatedAt:   infradb.NewFlexibleTime(startedAt),
		UpdatedAt:   infradb.NewFlexibleTime(startedAt),
	}
	if err := gdb.Create(&row).Error; err != nil {
		t.Fatalf("seed pending api_call: %v", err)
	}
	return row.ID
}

func TestP4AssemblySmoke(t *testing.T) {
	gdb := openSmokeDB(t)
	store := service.NewProjectStore(t.TempDir())
	project := "smoke"
	if _, err := store.Create(project, "冒烟项目", domainproject.ContentNarration, domainproject.GenerationStoryboard, "写实"); err != nil {
		t.Fatalf("create project: %v", err)
	}

	ledger := service.NewLedger(gdb, pricing.NewCalculator(pricing.BuiltinCatalog()))
	images := generation.NewImageGenerator(gdb, ledger)
	dispatcher := generation.NewTaskDispatcher(store, images)
	dispatcher.SetVideoGenerator(generation.NewVideoGenerator(gdb, ledger))

	hub := service.NewEventHub()
	q := queue.NewQueue(gdb,
		queue.WithProjectLoader(store),
		queue.WithTerminalEmitter(generation.NewTerminalEmitter(hub, store, gdb)))

	exec := &smokeExecutor{
		block:  map[string]chan struct{}{},
		failOn: map[string]error{},
	}
	// 装配与 cmd/shotwised/main.go 同形：全部钩子都注入真实实现。
	worker := queue.NewWorker(q, exec,
		queue.WithCapacityTable(queue.NewCapacityTable(
			map[string]map[string]int{"p1": {"image": 1, "video": 1, "audio": 1}},
			map[string]int{"image": 1, "video": 1, "audio": 1},
		)),
		queue.WithInterruptedSettler(generation.NewLedgerSettler(ledger)),
		queue.WithFailureEncoder(generation.EncodeTaskFailure),
		queue.WithTaskErrorEncoder(generation.EncodeTaskError),
		queue.WithTransientClassifier(generation.IsTransientTaskError),
		queue.WithProviderExtractor(generation.NewProviderExtractor(store, config.NewResolver(gdb))),
		queue.WithPreflight(generation.NewWorkerPreflight(store)),
		queue.WithResumeExecutor(generation.NewResumeExecutor(dispatcher)))
	q.SetCancelCallback(worker.RequestCancel)

	// 启动收口的对象：本进程构造之前发起的 pending 行，必须在 worker 起来后被翻失败。
	staleCall := seedPendingCall(t, gdb, project, time.Now().Add(-1*time.Hour))

	// 断点续传对象：一条 running 的视频孤儿，带 provider_job_id。启动期孤儿扫描会把它交给
	// 真实 resume 执行器（无凭据 → 必然失败），但不得走到「resume executor not configured」分支。
	orphanID := enqueueSmokeTask(t, q, project, "video", "video", "orphan-unit", "p1", nil)
	setSmokeJobID(t, gdb, orphanID, "job-smoke-1")
	setSmokeStatus(t, gdb, orphanID, "running")

	// 终态事件订阅：成功任务的批次经它广播。
	frames, unsubscribe := hub.Subscribe(project)
	defer unsubscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go worker.Run(ctx)

	// ---- 1) 启动收口：残留 pending 记账行被翻失败 ----
	waitForCallStatus(t, gdb, staleCall, "failed")

	// ---- 2) 批量生成：3 个任务全部成功 ----
	wantBatch := []string{"s1", "s2", "s3"}
	batchIDs := make([]string, 0, len(wantBatch))
	for _, res := range wantBatch {
		batchIDs = append(batchIDs, enqueueSmokeTask(t, q, project, "tts", "audio", res, "p1", nil))
	}
	for _, id := range batchIDs {
		waitForTaskStatus(t, gdb, id, "succeeded")
	}

	// 成功终态必须广播项目变更事件（前端 cache-bust 依赖它）。
	deadline := time.Now().Add(10 * time.Second)
	gotFrames := 0
	for gotFrames < len(wantBatch) && time.Now().Before(deadline) {
		select {
		case _, ok := <-frames:
			if !ok {
				t.Fatal("事件订阅通道被意外关闭")
			}
			gotFrames++
		case <-time.After(50 * time.Millisecond):
		}
	}
	if gotFrames < len(wantBatch) {
		t.Fatalf("成功任务共 %d 个，只广播了 %d 个变更事件", len(wantBatch), gotFrames)
	}

	// ---- 3) 取消：阻塞中的任务被取消，且其 pending 记账行被收口 ----
	cancelCall := seedPendingCall(t, gdb, project, time.Now())
	blockCh := make(chan struct{})
	exec.mu.Lock()
	exec.block["blk"] = blockCh
	exec.mu.Unlock()
	blockID := enqueueSmokeTask(t, q, project, "tts", "audio", "blk", "p1",
		map[string]any{"api_call_id": cancelCall})

	// 等到执行层真的开始跑，取消才落在在途执行上。
	waitForTaskStatus(t, gdb, blockID, "running")
	if _, err := q.CancelTask(context.Background(), blockID); err != nil {
		t.Fatalf("cancel task: %v", err)
	}
	waitForTaskStatus(t, gdb, blockID, "cancelled")
	close(blockCh) // 让执行协程退出，避免泄漏
	waitForCallStatus(t, gdb, cancelCall, "failed")

	// ---- 4) 断点续传：孤儿走真实 resume 执行器，不得落到「未注入」分支 ----
	orphan := waitForTaskStatus(t, gdb, orphanID, "failed")
	if orphan.ErrorMessage == nil {
		t.Fatal("resume 孤儿应有失败原因")
	}
	if strings.Contains(*orphan.ErrorMessage, "resume executor not configured") {
		t.Fatalf("resume 执行器未被注入：%s", *orphan.ErrorMessage)
	}

	// 批量任务确实各跑了一次，取消任务与孤儿不被常规执行路径重复处理。
	seen := map[string]int{}
	for _, id := range exec.called() {
		seen[id]++
	}
	for _, res := range wantBatch {
		if seen[res] != 1 {
			t.Fatalf("批量任务 %s 执行次数 = %d，期望 1", res, seen[res])
		}
	}
}

func enqueueSmokeTask(
	t *testing.T, q *queue.Queue, project, taskType, mediaType, resourceID, providerID string, payload map[string]any,
) string {
	t.Helper()
	pid := providerID
	res, err := q.EnqueueTask(context.Background(), repository.EnqueueParams{
		ProjectName: project,
		TaskType:    taskType,
		MediaType:   mediaType,
		ResourceID:  resourceID,
		ProviderID:  &pid,
		Payload:     payload,
	})
	if err != nil {
		t.Fatalf("enqueue %s/%s: %v", taskType, resourceID, err)
	}
	if res.Deduped {
		t.Fatalf("unexpected dedupe for %s/%s", taskType, resourceID)
	}
	return res.TaskID
}

func setSmokeJobID(t *testing.T, gdb *gorm.DB, taskID, jobID string) {
	t.Helper()
	if err := gdb.Model(&repository.Task{}).Where("task_id = ?", taskID).
		Update("provider_job_id", jobID).Error; err != nil {
		t.Fatalf("set job id: %v", err)
	}
}

func setSmokeStatus(t *testing.T, gdb *gorm.DB, taskID, status string) {
	t.Helper()
	if err := gdb.Model(&repository.Task{}).Where("task_id = ?", taskID).
		Update("status", status).Error; err != nil {
		t.Fatalf("set status %s: %v", status, err)
	}
}
