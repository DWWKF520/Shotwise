package queue

import (
	"context"
	"testing"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

func enqueueTestTask(t *testing.T, gdb *gorm.DB, projectName, taskType, mediaType, resourceID string) string {
	t.Helper()
	result, err := repository.NewTaskRepo(gdb).Enqueue(context.Background(), repository.EnqueueParams{
		ProjectName: projectName,
		TaskType:    taskType,
		MediaType:   mediaType,
		ResourceID:  resourceID,
	})
	if err != nil {
		t.Fatalf("enqueue test task: %v", err)
	}
	if result.Deduped {
		t.Fatalf("unexpected dedupe for %s/%s", taskType, resourceID)
	}
	return result.TaskID
}

func claimRunning(t *testing.T, gdb *gorm.DB, mediaType string) {
	t.Helper()
	task, err := repository.NewTaskRepo(gdb).ClaimNext(context.Background(), mediaType, nil)
	if err != nil {
		t.Fatalf("claim next: %v", err)
	}
	if task == nil {
		t.Fatal("expected a claimable task")
	}
}

func captureEmitter(events *[]repository.TerminalEvent) TerminalEmitter {
	return func(batch []repository.TerminalEvent) { *events = append(*events, batch...) }
}

func TestPinVideoExecutionModel(t *testing.T) {
	bucket := provider.BucketI2V
	original := map[string]any{"prompt": "x"}
	pinned := pinVideoExecutionModel(original, &bucket, config.ProviderModel{ProviderID: "p", ModelID: "m"})
	if pinned["video_provider_i2v"] != "p/m" {
		t.Errorf("pinned key = %v, want p/m", pinned["video_provider_i2v"])
	}
	if _, ok := original["video_provider_i2v"]; ok {
		t.Error("pin must not mutate the caller's payload")
	}
	// 不定桶 / 解析不出 model → 原样返回
	if got := pinVideoExecutionModel(original, nil, config.ProviderModel{ProviderID: "p", ModelID: "m"}); len(got) != 1 {
		t.Errorf("nil bucket must return the payload untouched, got %v", got)
	}
	if got := pinVideoExecutionModel(original, &bucket, config.ProviderModel{ProviderID: "p"}); len(got) != 1 {
		t.Errorf("empty model must return the payload untouched, got %v", got)
	}
}

func TestEnqueueTaskKeepsExplicitProviderAndSkipsPinning(t *testing.T) {
	gdb := newQueueTestDB(t)
	q := NewQueue(gdb)
	providerID := "pinned"
	result, err := q.EnqueueTask(context.Background(), repository.EnqueueParams{
		ProjectName: "proj",
		TaskType:    "video",
		MediaType:   "video",
		ResourceID:  "r1",
		ProviderID:  &providerID,
		Payload:     map[string]any{"prompt": "x"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	task, err := q.GetTask(context.Background(), result.TaskID)
	if err != nil || task == nil {
		t.Fatalf("get task: %v (%v)", err, task)
	}
	if task.ProviderID == nil || *task.ProviderID != "pinned" {
		t.Errorf("provider_id = %v, want the explicit pinned value", task.ProviderID)
	}
	if _, ok := task.Payload["video_provider_i2v"]; ok {
		t.Error("the explicit-provider path must not pin an execution model")
	}
}

func TestEnqueueTaskLeavesProviderNullWhenDerivationFails(t *testing.T) {
	gdb := newQueueTestDB(t)
	// 无项目读取器 → 派生返回 nil，provider_id 留 NULL 交 worker 兜底。
	q := NewQueue(gdb)
	result, err := q.EnqueueTask(context.Background(), repository.EnqueueParams{
		ProjectName: "proj",
		TaskType:    "video",
		MediaType:   "video",
		ResourceID:  "r1",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	task, err := q.GetTask(context.Background(), result.TaskID)
	if err != nil || task == nil {
		t.Fatalf("get task: %v", err)
	}
	if task.ProviderID != nil {
		t.Errorf("provider_id = %v, want NULL on a failed derivation", *task.ProviderID)
	}
}

func TestMarkTaskSucceededEmitsTerminalEvent(t *testing.T) {
	gdb := newQueueTestDB(t)
	var events []repository.TerminalEvent
	q := NewQueue(gdb, WithTerminalEmitter(captureEmitter(&events)))
	id := enqueueTestTask(t, gdb, "proj", "storyboard", "image", "r1")
	claimRunning(t, gdb, "image")

	affected, err := q.MarkTaskSucceeded(context.Background(), id, map[string]any{"ok": true})
	if err != nil {
		t.Fatalf("mark succeeded: %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected = %d, want 1", affected)
	}
	if len(events) != 1 || events[0].TaskID != id || events[0].Status != "succeeded" {
		t.Errorf("events = %+v, want one succeeded event for %s", events, id)
	}
	if events[0].ProjectName != "proj" {
		t.Errorf("event project = %q, want proj", events[0].ProjectName)
	}
}

func TestMarkTaskFailedEmitsTerminalEvent(t *testing.T) {
	gdb := newQueueTestDB(t)
	var events []repository.TerminalEvent
	q := NewQueue(gdb, WithTerminalEmitter(captureEmitter(&events)))
	id := enqueueTestTask(t, gdb, "proj", "storyboard", "image", "r1")
	claimRunning(t, gdb, "image")

	affected, err := q.MarkTaskFailed(context.Background(), id, "upstream boom")
	if err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected = %d, want 1", affected)
	}
	if len(events) != 1 || events[0].TaskID != id || events[0].Status != "failed" {
		t.Errorf("events = %+v, want one failed event for %s", events, id)
	}
}

func TestCancelQueuedTaskEmitsEventWithoutSignal(t *testing.T) {
	gdb := newQueueTestDB(t)
	var events []repository.TerminalEvent
	var signalled []string
	q := NewQueue(gdb,
		WithTerminalEmitter(captureEmitter(&events)),
		WithCancelCallback(func(taskID string) bool { signalled = append(signalled, taskID); return true }),
	)
	id := enqueueTestTask(t, gdb, "proj", "storyboard", "image", "r1")

	result, err := q.CancelTask(context.Background(), id)
	if err != nil {
		t.Fatalf("cancel task: %v", err)
	}
	if len(result.Cancelled) != 1 || result.Cancelled[0].Status != "cancelled" {
		t.Errorf("cancelled = %+v, want the queued task cancelled", result.Cancelled)
	}
	if len(signalled) != 0 {
		t.Errorf("signalled = %v, want no in-process signal for a queued task", signalled)
	}
	if len(events) != 1 || events[0].Status != "cancelled" {
		t.Errorf("events = %+v, want one cancelled event", events)
	}
}

func TestCancelRunningTaskSignalsWithoutTerminalEventYet(t *testing.T) {
	gdb := newQueueTestDB(t)
	var events []repository.TerminalEvent
	var signalled []string
	q := NewQueue(gdb,
		WithTerminalEmitter(captureEmitter(&events)),
		WithCancelCallback(func(taskID string) bool { signalled = append(signalled, taskID); return true }),
	)
	id := enqueueTestTask(t, gdb, "proj", "storyboard", "image", "r1")
	claimRunning(t, gdb, "image")

	result, err := q.CancelTask(context.Background(), id)
	if err != nil {
		t.Fatalf("cancel task: %v", err)
	}
	if len(result.Cancelling) != 1 || result.Cancelling[0] != id {
		t.Fatalf("cancelling = %v, want the running task id %s", result.Cancelling, id)
	}
	if len(signalled) != 1 || signalled[0] != id {
		t.Errorf("signalled = %v, want the running task signalled once", signalled)
	}
	// running → cancelling 是中间态，尚无终态事件（等 worker finally 收口）。
	if len(events) != 0 {
		t.Errorf("events = %+v, want none while the task is still cancelling", events)
	}
}

func TestCancelAllActiveFinalizesWhenWorkerOffline(t *testing.T) {
	gdb := newQueueTestDB(t)
	var events []repository.TerminalEvent
	q := NewQueue(gdb,
		WithTerminalEmitter(captureEmitter(&events)),
		WithCancelCallback(func(string) bool { return true }),
	)
	id := enqueueTestTask(t, gdb, "proj", "storyboard", "image", "r1")
	claimRunning(t, gdb, "image")

	result, err := q.CancelAllActive(context.Background(), "proj")
	if err != nil {
		t.Fatalf("cancel all: %v", err)
	}
	// 无 lease 行 → worker 离线 → running 任务直接落 cancelled，不留 cancelling。
	if len(result.Cancelling) != 0 {
		t.Errorf("cancelling = %v, want none when the worker is offline", result.Cancelling)
	}
	if len(result.Cancelled) != 1 || result.Cancelled[0].TaskID != id || result.Cancelled[0].Status != "cancelled" {
		t.Errorf("cancelled = %+v, want the running task finalized to cancelled", result.Cancelled)
	}
	if len(events) != 1 || events[0].Status != "cancelled" {
		t.Errorf("events = %+v, want one cancelled event", events)
	}
}

func TestMarkTaskCancelledFinalizesRunningTask(t *testing.T) {
	gdb := newQueueTestDB(t)
	var events []repository.TerminalEvent
	q := NewQueue(gdb, WithTerminalEmitter(captureEmitter(&events)))
	id := enqueueTestTask(t, gdb, "proj", "storyboard", "image", "r1")
	claimRunning(t, gdb, "image")

	rows, err := q.MarkTaskCancelled(context.Background(), id, "user")
	if err != nil {
		t.Fatalf("mark cancelled: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want 1", rows)
	}
	if len(events) != 1 || events[0].TaskID != id || events[0].Status != "cancelled" {
		t.Errorf("events = %+v, want one cancelled event for %s", events, id)
	}
}
