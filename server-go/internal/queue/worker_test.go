package queue

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

// ----------------------------------------------------------------------
// 测试替身
// ----------------------------------------------------------------------

// fakeExecutor 是可编排的任务执行替身：可阻塞、可报错、可记录被调用的 task。
type fakeExecutor struct {
	mu      sync.Mutex
	calls   []string
	result  map[string]any
	err     error
	started chan struct{} // 非 nil 时首次执行即 close，供测试同步
	release chan struct{} // 非 nil 时阻塞到 close 或 ctx 取消
}

func (f *fakeExecutor) Execute(ctx context.Context, task *repository.TaskData) (map[string]any, error) {
	f.mu.Lock()
	f.calls = append(f.calls, task.TaskID)
	f.mu.Unlock()
	if f.started != nil {
		select {
		case <-f.started:
		default:
			close(f.started)
		}
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.result == nil {
		return map[string]any{"ok": true}, nil
	}
	return f.result, nil
}

func (f *fakeExecutor) calledIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// fakeResumeExecutor 是 resume 执行替身。
type fakeResumeExecutor struct {
	mu     sync.Mutex
	jobIDs []string
	err    error
	result map[string]any
}

func (f *fakeResumeExecutor) ResumeVideo(_ context.Context, task *repository.TaskData, jobID string) (map[string]any, error) {
	f.mu.Lock()
	f.jobIDs = append(f.jobIDs, jobID)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.result == nil {
		return map[string]any{"resumed": true}, nil
	}
	return f.result, nil
}

// ----------------------------------------------------------------------
// 夹具
// ----------------------------------------------------------------------

func fixedCapacity(limits map[string]map[string]int) *CapacityTable {
	return NewCapacityTable(limits, map[string]int{laneImage: 1, laneVideo: 1, laneAudio: 1})
}

func fixedExtractor(providerID string) ProviderExtractor {
	return func(context.Context, *repository.TaskData) string { return providerID }
}

func enqueueWithProvider(t *testing.T, gdb *gorm.DB, taskType, mediaType, resourceID, providerID string) string {
	t.Helper()
	pid := providerID
	result, err := repository.NewTaskRepo(gdb).Enqueue(context.Background(), repository.EnqueueParams{
		ProjectName: "proj",
		TaskType:    taskType,
		MediaType:   mediaType,
		ResourceID:  resourceID,
		ProviderID:  &pid,
	})
	if err != nil {
		t.Fatalf("enqueue %s/%s: %v", taskType, resourceID, err)
	}
	return result.TaskID
}

func setTaskStatus(t *testing.T, gdb *gorm.DB, taskID, status string, jobID *string) {
	t.Helper()
	updates := map[string]any{"status": status}
	if jobID != nil {
		updates["provider_job_id"] = *jobID
	}
	if err := gdb.Model(&repository.Task{}).Where("task_id = ?", taskID).Updates(updates).Error; err != nil {
		t.Fatalf("set status %s: %v", status, err)
	}
}

// waitForStatus 轮询到任务落到期望状态；超时即失败。
func waitForStatus(t *testing.T, gdb *gorm.DB, taskID, want string) *repository.TaskData {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := repository.NewTaskRepo(gdb).Get(context.Background(), taskID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if task != nil && task.Status == want {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := repository.NewTaskRepo(gdb).Get(context.Background(), taskID)
	t.Fatalf("task %s did not reach %q; last = %+v", taskID, want, task)
	return nil
}

// ----------------------------------------------------------------------
// 认领与执行
// ----------------------------------------------------------------------

func TestWorkerClaimsAndSucceedsTask(t *testing.T) {
	gdb := newQueueTestDB(t)
	exec := &fakeExecutor{result: map[string]any{"url": "http://x/1.png"}}
	w := NewWorker(NewQueue(gdb), exec,
		WithCapacityTable(fixedCapacity(map[string]map[string]int{"p1": {laneImage: 1}})),
		WithProviderExtractor(fixedExtractor("p1")),
	)
	id := enqueueWithProvider(t, gdb, "storyboard", "image", "r1", "p1")

	if claimed := w.claimTasks(context.Background()); !claimed {
		t.Fatal("claimTasks should report a claim")
	}
	task := waitForStatus(t, gdb, id, repository.TaskStatusSucceeded)
	if task.Result["url"] != "http://x/1.png" {
		t.Errorf("result = %+v, want the executor result written back", task.Result)
	}
	if ids := exec.calledIDs(); len(ids) != 1 || ids[0] != id {
		t.Errorf("executor calls = %v, want [%s]", ids, id)
	}
}

func TestWorkerPoolFullRequeuesNullProviderTask(t *testing.T) {
	gdb := newQueueTestDB(t)
	w := NewWorker(NewQueue(gdb), &fakeExecutor{},
		WithCapacityTable(fixedCapacity(map[string]map[string]int{"p1": {laneImage: 1}})),
		WithProviderExtractor(fixedExtractor("p1")),
	)
	// provider_id 为 NULL → 绕过 SQL 黑名单被 claim，二次校验发现池满 → 回队。
	id := enqueueTestTask(t, gdb, "proj", "storyboard", "image", "r1")
	// 预先占满 p1/image 的唯一槽位。
	w.slots.Register("p1", provider.MediaImage, "blocker", pendingTask(), false)

	if claimed := w.claimTasks(context.Background()); claimed {
		t.Fatal("a deferred task must not count as claimed")
	}
	task := waitForStatus(t, gdb, id, repository.TaskStatusQueued)
	if task.ProviderID == nil || *task.ProviderID != "p1" {
		t.Errorf("provider_id = %v, want the re-derived p1 written back before requeue", task.ProviderID)
	}
}

func TestWorkerUnsupportedMediaFailsFast(t *testing.T) {
	gdb := newQueueTestDB(t)
	exec := &fakeExecutor{}
	w := NewWorker(NewQueue(gdb), exec,
		WithCapacityTable(fixedCapacity(map[string]map[string]int{"p1": {laneImage: 1, laneVideo: 0}})),
		WithProviderExtractor(fixedExtractor("p1")),
	)
	id := enqueueWithProvider(t, gdb, "video", "video", "r1", "p1")

	if claimed := w.claimTasks(context.Background()); !claimed {
		t.Fatal("fail-fast must count as processed")
	}
	task := waitForStatus(t, gdb, id, repository.TaskStatusFailed)
	if task.ErrorMessage == nil || !strings.Contains(*task.ErrorMessage, "provider_unsupported_media") {
		t.Errorf("error_message = %v, want the unsupported-media code", task.ErrorMessage)
	}
	if len(exec.calledIDs()) != 0 {
		t.Error("the executor must not run for an unsupported lane")
	}
}

func TestWorkerRequestCancelLandsCancelled(t *testing.T) {
	gdb := newQueueTestDB(t)
	started := make(chan struct{})
	exec := &fakeExecutor{started: started, release: make(chan struct{})}
	w := NewWorker(NewQueue(gdb), exec,
		WithCapacityTable(fixedCapacity(map[string]map[string]int{"p1": {laneImage: 1}})),
		WithProviderExtractor(fixedExtractor("p1")),
	)
	id := enqueueWithProvider(t, gdb, "storyboard", "image", "r1", "p1")

	w.claimTasks(context.Background())
	<-started
	if !w.RequestCancel(id) {
		t.Fatal("RequestCancel should hit the inflight task")
	}
	task := waitForStatus(t, gdb, id, repository.TaskStatusCancelled)
	if task.CancelledBy == nil || *task.CancelledBy != "user" {
		t.Errorf("cancelled_by = %v, want user", task.CancelledBy)
	}
}

// ----------------------------------------------------------------------
// 孤儿收口与 resume
// ----------------------------------------------------------------------

func TestWorkerOrphanSweepFinalizesByMedia(t *testing.T) {
	gdb := newQueueTestDB(t)
	resume := &fakeResumeExecutor{}
	w := NewWorker(NewQueue(gdb), &fakeExecutor{}, WithResumeExecutor(resume),
		WithCapacityTable(fixedCapacity(map[string]map[string]int{
			"ark":  {laneVideo: 2},
			"grok": {laneVideo: 1},
		})),
		WithProviderExtractor(fixedExtractor("ark")),
	)

	cancelling := enqueueTestTask(t, gdb, "proj", "video", "video", "c1")
	setTaskStatus(t, gdb, cancelling, repository.TaskStatusCancelling, nil)

	imageOrphan := enqueueTestTask(t, gdb, "proj", "storyboard", "image", "i1")
	setTaskStatus(t, gdb, imageOrphan, repository.TaskStatusRunning, nil)

	grokOrphan := enqueueWithProvider(t, gdb, "video", "video", "g1", "grok")
	job := "job-grok"
	setTaskStatus(t, gdb, grokOrphan, repository.TaskStatusRunning, &job)

	arkOrphan := enqueueWithProvider(t, gdb, "video", "video", "a1", "ark")
	jobArk := "job-ark"
	setTaskStatus(t, gdb, arkOrphan, repository.TaskStatusRunning, &jobArk)

	w.handleOrphanTasksOnStart(context.Background())
	w.orphanWG.Wait()

	if task := waitForStatus(t, gdb, cancelling, repository.TaskStatusCancelled); task.Status != repository.TaskStatusCancelled {
		t.Errorf("cancelling orphan = %s, want cancelled", task.Status)
	}
	img := waitForStatus(t, gdb, imageOrphan, repository.TaskStatusFailed)
	if img.ErrorMessage == nil || !strings.Contains(*img.ErrorMessage, "restart_lost_image") {
		t.Errorf("image orphan error = %v, want restart_lost_image", img.ErrorMessage)
	}
	grok := waitForStatus(t, gdb, grokOrphan, repository.TaskStatusFailed)
	if grok.ErrorMessage == nil || !strings.Contains(*grok.ErrorMessage, "resume_unsupported_provider") {
		t.Errorf("grok orphan error = %v, want resume_unsupported_provider", grok.ErrorMessage)
	}
	ark := waitForStatus(t, gdb, arkOrphan, repository.TaskStatusSucceeded)
	if ark.Status != repository.TaskStatusSucceeded {
		t.Errorf("ark orphan = %s, want succeeded via resume", ark.Status)
	}
	resume.mu.Lock()
	defer resume.mu.Unlock()
	if len(resume.jobIDs) != 1 || resume.jobIDs[0] != "job-ark" {
		t.Errorf("resume job IDs = %v, want [job-ark]", resume.jobIDs)
	}
}

func TestWorkerOrphanSkipsSelfActive(t *testing.T) {
	gdb := newQueueTestDB(t)
	w := NewWorker(NewQueue(gdb), &fakeExecutor{},
		WithCapacityTable(fixedCapacity(map[string]map[string]int{"p1": {laneImage: 1}})),
		WithProviderExtractor(fixedExtractor("p1")),
	)
	id := enqueueTestTask(t, gdb, "proj", "storyboard", "image", "r1")
	setTaskStatus(t, gdb, id, repository.TaskStatusRunning, nil)
	// 该 task 仍在本进程占用台账里 → self-active，不得被当孤儿标失败。
	w.slots.Register("p1", provider.MediaImage, id, pendingTask(), false)

	w.handleOrphanTasksOnStart(context.Background())
	w.orphanWG.Wait()

	task, err := repository.NewTaskRepo(gdb).Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.Status != repository.TaskStatusRunning {
		t.Errorf("self-active task = %s, want it left running", task.Status)
	}
}

func TestWorkerOrphanVideoWithoutJobIsRestartLost(t *testing.T) {
	gdb := newQueueTestDB(t)
	w := NewWorker(NewQueue(gdb), &fakeExecutor{},
		WithCapacityTable(fixedCapacity(map[string]map[string]int{"ark": {laneVideo: 1}})),
		WithProviderExtractor(fixedExtractor("ark")),
	)
	id := enqueueWithProvider(t, gdb, "video", "video", "r1", "ark")
	setTaskStatus(t, gdb, id, repository.TaskStatusRunning, nil)

	w.handleOrphanTasksOnStart(context.Background())
	w.orphanWG.Wait()

	task := waitForStatus(t, gdb, id, repository.TaskStatusFailed)
	if task.ErrorMessage == nil || !strings.Contains(*task.ErrorMessage, "restart_lost_no_job_id") {
		t.Errorf("error = %v, want restart_lost_no_job_id", task.ErrorMessage)
	}
}

// ----------------------------------------------------------------------
// 诊断与主循环
// ----------------------------------------------------------------------

func TestWorkerObservabilitySnapshot(t *testing.T) {
	gdb := newQueueTestDB(t)
	w := NewWorker(NewQueue(gdb), &fakeExecutor{},
		WithCapacityTable(fixedCapacity(map[string]map[string]int{"p1": {laneImage: 2}})),
	)
	w.slots.Register("p1", provider.MediaImage, "t1", pendingTask(), false)

	snapshot := w.ObservabilitySnapshot()
	lane, ok := snapshot.Lanes["p1:image"]
	if !ok {
		t.Fatalf("lane p1:image missing from %+v", snapshot.Lanes)
	}
	if lane.Capacity != 2 || lane.EffectiveCapacity != 2 || lane.Occupied != 1 {
		t.Errorf("lane = %+v, want capacity 2 / effective 2 / occupied 1", lane)
	}
	if lane.Utilization != 0.5 {
		t.Errorf("utilization = %v, want 0.5", lane.Utilization)
	}
}

func TestWorkerRunLoopProcessesThenStops(t *testing.T) {
	gdb := newQueueTestDB(t)
	exec := &fakeExecutor{}
	w := NewWorker(NewQueue(gdb), exec,
		WithCapacityTable(fixedCapacity(map[string]map[string]int{"p1": {laneImage: 1}})),
		WithProviderExtractor(fixedExtractor("p1")),
	)
	w.pollInterval = 0.01
	w.heartbeatInterval = 0.5
	id := enqueueWithProvider(t, gdb, "storyboard", "image", "r1", "p1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	waitForStatus(t, gdb, id, repository.TaskStatusSucceeded)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	w.Stop(stopCtx)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
}
