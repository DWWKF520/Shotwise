package generation

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/queue"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/generation"
	"github.com/shotwise/shotwise/migrations"
)

// batchFixture 起一个带真实 sqlite 的批次路由壳：原子准入依赖真事务与部分唯一索引，
// 假仓储验不出「整批回滚」。
type batchFixture struct {
	router *gin.Engine
	store  *service.ProjectStore
	repo   *repository.TaskRepo
	db     *gorm.DB
}

func newBatchFixture(t *testing.T) batchFixture {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "t.db"), logger.Silent)
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

	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "测试项目", domain.ContentNarration, domain.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := store.Update("p1", func(pf *domain.ProjectFile) error {
		pf.Episodes = append(pf.Episodes, domain.EpisodeMeta{Episode: 1, ScriptFile: "scripts/episode_1.json"})
		return nil
	}); err != nil {
		t.Fatalf("register episode: %v", err)
	}
	if err := store.WriteScriptJSON("p1", "episode_1.json", map[string]any{
		"content_mode": "narration", "episode": 1,
		"segments": []any{
			map[string]any{"segment_id": "E1S1", "duration_seconds": 4, "novel_text": "第一段"},
			map[string]any{"segment_id": "E1S2", "duration_seconds": 4, "novel_text": "第二段"},
		},
	}); err != nil {
		t.Fatalf("write script: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.NoRoute(func(c *gin.Context) { c.JSON(http.StatusNotFound, gin.H{"detail": "Not Found"}) })
	v1 := r.Group("/api/v1")
	q := queue.NewQueue(gdb)
	taskRepo := repository.NewTaskRepo(gdb)
	RegisterBatches(v1, BatchDeps{
		Store: store,
		NewOrchestrator: func(projectName, userID string) *generation.BatchOrchestrator {
			return generation.NewBatchOrchestrator(
				generation.NewGormBatchRepository(gdb, userID, projectName),
				generation.NewBatchTaskAdapter(taskRepo, q, projectName))
		},
	})
	_ = config.NewResolver(gdb)
	return batchFixture{router: r, store: store, repo: taskRepo, db: gdb}
}

func (f batchFixture) post(t *testing.T, path string, body any, lang string) (int, map[string]any) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
	}
	return w.Code, out
}

func (f batchFixture) get(t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
	}
	return w.Code, out
}

// storyboardTask 造一条合法的分镜批次任务。
func storyboardTask(segmentID string) map[string]any {
	return map[string]any{
		"item_id": "item-" + segmentID,
		"task": map[string]any{
			"task_type":   "storyboard",
			"media_type":  "image",
			"resource_id": segmentID,
			"script_file": "episode_1.json",
			"payload":     map[string]any{"prompt": "雨夜街角"},
		},
	}
}

// TestBatchAdmitReturnsProjectedStatus 校验创建批次是 201，响应带派生状态与逐条任务。
func TestBatchAdmitReturnsProjectedStatus(t *testing.T) {
	f := newBatchFixture(t)
	status, body := f.post(t, "/api/v1/projects/p1/batches", map[string]any{
		"items": []any{storyboardTask("E1S1"), storyboardTask("E1S2")},
	}, "zh")
	if status != http.StatusCreated {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["batch_id"] == nil || body["batch_id"] == "" {
		t.Fatalf("缺 batch_id: %v", body)
	}
	// 刚入队的任务都是 queued → 批次处于 running。
	if body["status"] != "running" {
		t.Fatalf("status = %v", body["status"])
	}
	tasks, _ := body["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %v", tasks)
	}
	first, _ := tasks[0].(map[string]any)
	if first["item_id"] != "item-E1S1" || first["occupied"] != true || first["resource_id"] != "E1S1" {
		t.Fatalf("task = %v", first)
	}

	// 再读一次：同一份投影。
	code, again := f.get(t, "/api/v1/projects/p1/batches/"+body["batch_id"].(string))
	if code != http.StatusOK || again["batch_id"] != body["batch_id"] {
		t.Fatalf("code=%d body=%v", code, again)
	}
}

// TestBatchAdmissionIsAtomic 校验任一条目非法时整批不入队。
func TestBatchAdmissionIsAtomic(t *testing.T) {
	f := newBatchFixture(t)
	status, body := f.post(t, "/api/v1/projects/p1/batches", map[string]any{
		"items": []any{
			storyboardTask("E1S1"),
			map[string]any{"item_id": "bad", "task": map[string]any{
				"task_type": "storyboard", "media_type": "image", "resource_id": "E1S2",
			}},
		},
	}, "en")
	if status != http.StatusConflict {
		t.Fatalf("status = %d body=%v", status, body)
	}
	// 整批回滚：合法的那条也不能留库。
	var count int64
	if err := f.db.Model(&repository.Task{}).Where("project_name = ?", "p1").Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("整批应回滚，实际留下 %d 条", count)
	}
}

// TestBatchRejectsUnknownFields 校验 extra="forbid"：未知字段 422 且不产生任务。
func TestBatchRejectsUnknownFields(t *testing.T) {
	f := newBatchFixture(t)
	status, body := f.post(t, "/api/v1/projects/p1/batches", map[string]any{
		"items": []any{map[string]any{
			"item_id": "i1",
			"task": map[string]any{
				"task_type": "storyboard", "media_type": "image", "resource_id": "E1S1",
				"script_file": "episode_1.json", "surprise": 1,
			},
		}},
	}, "en")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

// TestBatchNotFound 校验未知批次是 404 batch_not_found。
func TestBatchNotFound(t *testing.T) {
	f := newBatchFixture(t)
	status, body := f.get(t, "/api/v1/projects/p1/batches/nope")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["detail"] != "batch_not_found" {
		t.Fatalf("detail = %v", body["detail"])
	}
}

// TestBatchRetryFailedReplacesTask 校验重试只重提失败条目并换绑 task_id。
func TestBatchRetryFailedReplacesTask(t *testing.T) {
	f := newBatchFixture(t)
	status, body := f.post(t, "/api/v1/projects/p1/batches", map[string]any{
		"items": []any{storyboardTask("E1S1"), storyboardTask("E1S2")},
	}, "zh")
	if status != http.StatusCreated {
		t.Fatalf("status = %d body=%v", status, body)
	}
	tasks, _ := body["tasks"].([]any)
	first := tasks[0].(map[string]any)
	firstID, _ := first["task_id"].(string)

	// 把第一条任务改成 failed（模拟上游失败）。
	// 直接改状态：MarkFailed 只收 running 任务，queued 行改不动。
	if err := f.db.Model(&repository.Task{}).Where("task_id = ?", firstID).
		Update("status", "failed").Error; err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	code, retried := f.post(t, "/api/v1/projects/p1/batches/"+body["batch_id"].(string)+"/retry-failed",
		map[string]any{}, "zh")
	if code != http.StatusOK {
		t.Fatalf("code = %d body=%v", code, retried)
	}
	newTasks, _ := retried["tasks"].([]any)
	if len(newTasks) != 2 {
		t.Fatalf("tasks = %v", newTasks)
	}
	replaced := newTasks[0].(map[string]any)
	if replaced["task_id"] == firstID {
		t.Fatal("失败条目应换绑到新任务")
	}
	kept := newTasks[1].(map[string]any)
	if kept["task_id"] != tasks[1].(map[string]any)["task_id"] {
		t.Fatal("成功/在跑条目不应换绑")
	}
}

// TestBatchCancelMarksRequested 校验取消把 cancel_requested 写回并可再次读取。
func TestBatchCancelMarksRequested(t *testing.T) {
	f := newBatchFixture(t)
	status, body := f.post(t, "/api/v1/projects/p1/batches", map[string]any{
		"items": []any{storyboardTask("E1S1")},
	}, "zh")
	if status != http.StatusCreated {
		t.Fatalf("status = %d body=%v", status, body)
	}
	code, cancelled := f.post(t, "/api/v1/projects/p1/batches/"+body["batch_id"].(string)+"/cancel",
		map[string]any{}, "zh")
	if code != http.StatusOK {
		t.Fatalf("code = %d body=%v", code, cancelled)
	}
	if cancelled["cancel_requested"] != true {
		t.Fatalf("cancel_requested = %v", cancelled["cancel_requested"])
	}
	if cancelled["status"] != "cancelled" {
		t.Fatalf("status = %v", cancelled["status"])
	}
}

// TestBatchStatusDerivation 直接覆盖派生规则：全成功 / 部分成功 / 全失败。
func TestBatchStatusDerivation(t *testing.T) {
	// 与 Python _derive_status 逐条对齐。
	cases := []struct {
		name      string
		statuses  []string
		cancelled bool
		want      string
	}{
		{"all succeeded", []string{"succeeded", "succeeded"}, false, "succeeded"},
		{"mixed", []string{"succeeded", "failed"}, false, "partially_succeeded"},
		{"all failed", []string{"failed", "failed"}, false, "failed"},
		{"cancelled", []string{"cancelled"}, false, "cancelled"},
		// cancel_requested 只在「没有任何成功也没有失败」时才把批次判成已取消。
		{"cancel requested with failure", []string{"cancelled", "failed"}, true, "failed"},
		{"cancel requested all cancelled", []string{"cancelled", "cancelled"}, true, "cancelled"},
	}
	for _, tc := range cases {
		if got := deriveForTest(tc.statuses, tc.cancelled); got != tc.want {
			t.Fatalf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
}

// deriveForTest 通过公开服务导出批状态派生，避免测试另写一份规则。
func deriveForTest(statuses []string, cancelRequested bool) string {
	return generation.DeriveBatchStatusForTest(statuses, cancelRequested)
}
