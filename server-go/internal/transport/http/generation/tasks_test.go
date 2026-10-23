package generation

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/domain/project"
	domaintask "github.com/shotwise/shotwise/internal/domain/task"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/queue"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/generation"
	"github.com/shotwise/shotwise/migrations"
)

// queueFixture 起一个带真实 sqlite 库的路由壳：任务视图与队列诊断都直接读表，
// 用假实现验不出投影口径。
type queueFixture struct {
	router *gin.Engine
	repo   *repository.TaskRepo
	store  *service.ProjectStore
	gdb    *gorm.DB
}

func newQueueFixture(t *testing.T) queueFixture {
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
	if _, err := store.Create("p1", "测试项目", project.ContentNarration, project.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.NoRoute(func(c *gin.Context) { c.JSON(http.StatusNotFound, gin.H{"detail": "Not Found"}) })
	v1 := r.Group("/api/v1")
	q := queue.NewQueue(gdb)
	RegisterTasks(v1, TaskDeps{Queue: q})
	RegisterQueueObservability(v1, QueueObservabilityDeps{
		Obs: generation.NewQueueObservability(gdb, nil),
	})
	RegisterVersions(v1, VersionDeps{Versions: service.NewVersionService(store)})
	return queueFixture{router: r, repo: repository.NewTaskRepo(gdb), store: store, gdb: gdb}
}

func (f queueFixture) enqueue(t *testing.T, project, taskType, media, resourceID string) repository.EnqueueResult {
	t.Helper()
	res, err := f.repo.Enqueue(context.Background(), repository.EnqueueParams{
		ProjectName: project,
		TaskType:    taskType,
		MediaType:   media,
		ResourceID:  resourceID,
		Payload:     map[string]any{"prompt": "a cat"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return res
}

func (f queueFixture) call(t *testing.T, method, path string, lang string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(nil))
	req.Header.Set("Content-Type", "application/json")
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	body := map[string]any{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body %q: %v", w.Body.String(), err)
		}
	}
	return w.Code, body
}

// TestTaskListLocalizesFailureAndWarnings 校验两个读时渲染口径：error_message 的
// [code] {params} 信封与 result.warnings 的 {key, params} 条目都按请求语言出文案。
func TestTaskListLocalizesFailureAndWarnings(t *testing.T) {
	f := newQueueFixture(t)
	task := f.enqueue(t, "p1", "storyboard", "image", "E1S1")

	// 直接落库模拟 worker 收尾：结构化失败码 + 参考路线 warning。
	encoded, err := domaintask.EncodeFailure(domaintask.CascadeFailureCode, map[string]any{
		"dependency_task_id": "upstream",
		"reason":             "boom",
	})
	if err != nil {
		t.Fatalf("encode failure: %v", err)
	}
	result := map[string]any{
		"warnings": []any{
			map[string]any{"key": "ref_ad_reference_skipped", "params": map[string]any{"type": "product"}},
			"malformed",
		},
	}
	resultJSON, _ := json.Marshal(result)
	if err := f.gdb.Exec(
		"UPDATE tasks SET status = ?, error_message = ?, result_json = ? WHERE task_id = ?",
		repository.TaskStatusFailed, encoded, string(resultJSON), task.TaskID,
	).Error; err != nil {
		t.Fatalf("update task: %v", err)
	}

	status, body := f.call(t, http.MethodGet, "/api/v1/tasks", "en")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", body["items"])
	}
	entry, _ := items[0].(map[string]any)
	message, _ := entry["error_message"].(string)
	if message == "" || message == encoded {
		t.Fatalf("error_message 未本地化: %q", message)
	}
	// 级联信封的上游 reason 必须被展开成本地文案，而不是原样嵌套 [code]。
	if bytes.Contains([]byte(message), []byte(domaintask.CascadeFailureCode)) {
		t.Fatalf("级联原因未展开: %q", message)
	}
	taskResult, _ := entry["result"].(map[string]any)
	warnings, _ := taskResult["warnings"].([]any)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v（畸形条目应被跳过）", taskResult["warnings"])
	}
	text, _ := warnings[0].(string)
	if text == "" || text == "ref_ad_reference_skipped" {
		t.Fatalf("warning 未本地化: %q", text)
	}
}

// TestTaskListPaginationValidation 校验 page / page_size 的 FastAPI 口径：
// page ge=1、page_size le=500，越界即 422 而不是静默夹取。
func TestTaskListPaginationValidation(t *testing.T) {
	f := newQueueFixture(t)
	cases := []string{
		"/api/v1/tasks?page=0",
		"/api/v1/tasks?page_size=501",
		"/api/v1/tasks?page=abc",
	}
	for _, path := range cases {
		status, body := f.call(t, http.MethodGet, path, "")
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status = %d body=%v", path, status, body)
		}
	}
	status, body := f.call(t, http.MethodGet, "/api/v1/tasks?page_size=500", "")
	if status != http.StatusOK {
		t.Fatalf("边界值应通过: status = %d body=%v", status, body)
	}
	if body["page_size"] != float64(500) {
		t.Fatalf("page_size = %v", body["page_size"])
	}
}

// TestTaskNotFoundStatusDifference 校验「取消对不存在任务」是 400，而详情是 404——
// 两个端点在 Python 侧用了不同的错误类，前端据此区分提示文案。
func TestTaskNotFoundStatusDifference(t *testing.T) {
	f := newQueueFixture(t)
	if status, _ := f.call(t, http.MethodPost, "/api/v1/tasks/missing/cancel", ""); status != http.StatusBadRequest {
		t.Fatalf("cancel missing: status = %d", status)
	}
	if status, _ := f.call(t, http.MethodGet, "/api/v1/tasks/missing/cancel-preview", ""); status != http.StatusBadRequest {
		t.Fatalf("cancel-preview missing: status = %d", status)
	}
	if status, _ := f.call(t, http.MethodGet, "/api/v1/tasks/missing", ""); status != http.StatusNotFound {
		t.Fatalf("detail missing: status = %d", status)
	}
	if status, _ := f.call(t, http.MethodGet, "/api/v1/tasks/missing/queue-position", ""); status != http.StatusNotFound {
		t.Fatalf("queue-position missing: status = %d", status)
	}
}

// TestCancelAllCounts 校验 cancel-all 的三个桶：queued 直接进 cancelled 且级联下游，
// 计数与 active_count 因级联而可能不等。
func TestCancelAllCounts(t *testing.T) {
	f := newQueueFixture(t)
	parent := f.enqueue(t, "p1", "storyboard", "image", "E1S1")
	child, err := f.repo.Enqueue(context.Background(), repository.EnqueueParams{
		ProjectName:      "p1",
		TaskType:         "video",
		MediaType:        "video",
		ResourceID:       "E1S1",
		Payload:          map[string]any{"prompt": "a cat"},
		DependencyTaskID: &parent.TaskID,
	})
	if err != nil {
		t.Fatalf("enqueue child: %v", err)
	}

	status, body := f.call(t, http.MethodGet, "/api/v1/projects/p1/tasks/cancel-all-preview", "")
	if status != http.StatusOK || body["active_count"] != float64(2) {
		t.Fatalf("preview: status=%d body=%v", status, body)
	}

	status, body = f.call(t, http.MethodPost, "/api/v1/projects/p1/tasks/cancel-all", "")
	if status != http.StatusOK {
		t.Fatalf("cancel-all: status=%d body=%v", status, body)
	}
	if body["cancelled_count"] != float64(2) {
		t.Fatalf("cancel-all 应级联取消下游: body=%v", body)
	}
	data, err := f.repo.Get(context.Background(), child.TaskID)
	if err != nil || data == nil {
		t.Fatalf("get child: %v", err)
	}
	if data.Status != repository.TaskStatusCancelled {
		t.Fatalf("child status = %s", data.Status)
	}
	if data.CancelledBy == nil || *data.CancelledBy != "cascade" {
		t.Fatalf("child cancelled_by = %v", data.CancelledBy)
	}
}

// TestQueueSnapshotShape 校验诊断快照的字段集合与无 worker 时的降级口径：
// eta 的容量分母缺失（不是 0 并发），worker.in_process 为 false。
func TestQueueSnapshotShape(t *testing.T) {
	f := newQueueFixture(t)
	f.enqueue(t, "p1", "storyboard", "image", "E1S1")
	f.enqueue(t, "p1", "video", "video", "E1S1")

	status, body := f.call(t, http.MethodGet, "/api/v1/queue/observability", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	for _, key := range []string{
		"generated_at", "project_name", "worker", "counts", "active_by_media",
		"queued_by_provider", "oldest_queued_at", "oldest_queued_age_seconds",
		"eta", "providers", "lanes", "health", "alerts",
	} {
		if _, ok := body[key]; !ok {
			t.Fatalf("快照缺字段 %s: %v", key, body)
		}
	}
	counts, _ := body["counts"].(map[string]any)
	if counts["queued"] != float64(2) || counts["total"] != float64(2) {
		t.Fatalf("counts = %v", counts)
	}
	worker, _ := body["worker"].(map[string]any)
	if worker["in_process"] != false || worker["online"] != false {
		t.Fatalf("worker = %v", worker)
	}
	eta, _ := body["eta"].(map[string]any)
	imageETA, _ := eta["image"].(map[string]any)
	if imageETA["effective_capacity"] != nil || imageETA["eta_seconds"] != nil {
		t.Fatalf("无 worker 时容量分母应缺失: %v", imageETA)
	}
	// 有活没人持有租约 → worker_offline，而不是 queue_stalled。
	alerts, _ := body["alerts"].([]any)
	found := false
	for _, alert := range alerts {
		entry, _ := alert.(map[string]any)
		if entry["code"] == "worker_offline" {
			found = true
		}
		if entry["code"] == "queue_stalled" {
			t.Fatalf("worker 离线不应报 queue_stalled: %v", alerts)
		}
	}
	if !found {
		t.Fatalf("缺 worker_offline 告警: %v", alerts)
	}
}

// TestTaskQueuePositionOrdering 校验 0 基名次按 (queued_at, task_id) 排序。
func TestTaskQueuePositionOrdering(t *testing.T) {
	f := newQueueFixture(t)
	first := f.enqueue(t, "p1", "storyboard", "image", "E1S1")
	second := f.enqueue(t, "p1", "storyboard", "image", "E1S2")
	// 另一条泳道（video）不应影响 image 泳道名次。
	f.enqueue(t, "p1", "video", "video", "E1S1")

	status, body := f.call(t, http.MethodGet, "/api/v1/tasks/"+second.TaskID+"/queue-position", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["position"] != float64(1) {
		t.Fatalf("position = %v", body["position"])
	}
	if body["lane_queued"] != float64(2) {
		t.Fatalf("lane_queued = %v", body["lane_queued"])
	}
	status, body = f.call(t, http.MethodGet, "/api/v1/tasks/"+first.TaskID+"/queue-position", "")
	if status != http.StatusOK || body["position"] != float64(0) {
		t.Fatalf("first position: status=%d body=%v", status, body)
	}
}

// TestVersionsListAndRestore 校验版本列表的 is_current / file_url 口径，
// 以及还原把快照复制回当前路径、推进指针并回写资产表。
func TestVersionsListAndRestore(t *testing.T) {
	f := newQueueFixture(t)
	dir, err := f.store.Dir("p1")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	writeVersionFile(t, dir, "characters/甲.png", "v1")
	writeVersionFile(t, dir, "versions/characters/甲_v1_20260101T000000.png", "v1")
	writeVersionFile(t, dir, "characters/甲.png", "v2")
	writeVersionFile(t, dir, "versions/characters/甲_v2_20260102T000000.png", "v2")
	writeVersionFile(t, dir, "versions/versions.json", `{
  "characters": {
    "甲": {
      "current_version": 2,
      "versions": [
        {"version": 1, "file": "versions/characters/甲_v1_20260101T000000.png", "prompt": "p1", "created_at": "2026-01-01T00:00:00+00:00"},
        {"version": 2, "file": "versions/characters/甲_v2_20260102T000000.png", "prompt": "p2", "created_at": "2026-01-02T00:00:00+00:00"}
      ]
    }
  }
}`)
	if err := f.store.Update("p1", func(pf *project.ProjectFile) error {
		if pf.Characters == nil {
			pf.Characters = project.AssetBucket{}
		}
		pf.Characters["甲"] = project.WrapAssetEntry(map[string]any{
			"character_sheet": "characters/甲.png",
			"description":     "测试角色",
		})
		return nil
	}); err != nil {
		t.Fatalf("seed asset: %v", err)
	}

	status, body := f.call(t, http.MethodGet, "/api/v1/projects/p1/versions/characters/甲", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["current_version"] != float64(2) {
		t.Fatalf("current_version = %v", body["current_version"])
	}
	versions, _ := body["versions"].([]any)
	if len(versions) != 2 {
		t.Fatalf("versions = %v", body["versions"])
	}
	last, _ := versions[1].(map[string]any)
	if last["is_current"] != true {
		t.Fatalf("is_current = %v", last["is_current"])
	}
	if last["file_url"] != "/api/v1/files/p1/versions/characters/甲_v2_20260102T000000.png" {
		t.Fatalf("file_url = %v", last["file_url"])
	}

	status, body = f.call(t, http.MethodPost, "/api/v1/projects/p1/versions/characters/甲/restore/1", "")
	if status != http.StatusOK {
		t.Fatalf("restore: status=%d body=%v", status, body)
	}
	if body["restored_version"] != float64(1) || body["current_version"] != float64(1) {
		t.Fatalf("restore body = %v", body)
	}
	if body["file_path"] != "characters/甲.png" {
		t.Fatalf("file_path = %v", body["file_path"])
	}
	fingerprints, _ := body["asset_fingerprints"].(map[string]any)
	if _, ok := fingerprints["characters/甲.png"]; !ok {
		t.Fatalf("fingerprints = %v", fingerprints)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "characters", "甲.png"))
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if string(raw) != "v1" {
		t.Fatalf("还原内容 = %q", raw)
	}
}

// TestVersionsUnsupportedResourceType 校验白名单外的类型是 400 而不是 500。
func TestVersionsUnsupportedResourceType(t *testing.T) {
	f := newQueueFixture(t)
	status, body := f.call(t, http.MethodGet, "/api/v1/projects/p1/versions/unknown_type/E1S1", "zh")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v", status, body)
	}
	// detail 必须是翻译后的字符串（Python 侧 BadRequestError 走 app 级 handler），
	// 不是 {"code": ...} 对象。
	if detail, ok := body["detail"].(string); !ok || !strings.Contains(detail, "unknown_type") {
		t.Fatalf("detail = %v", body["detail"])
	}
	status, body = f.call(t, http.MethodPost, "/api/v1/projects/p1/versions/unknown_type/E1S1/restore/1", "zh")
	if status != http.StatusBadRequest {
		t.Fatalf("restore status = %d body=%v", status, body)
	}
}

// TestVersionsMissingVersion 校验资源存在但版本号不存在时 404 version_not_found。
func TestVersionsMissingVersion(t *testing.T) {
	f := newQueueFixture(t)
	dir, err := f.store.Dir("p1")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	writeVersionFile(t, dir, "versions/versions.json", `{
  "scenes": {
    "客厅": {"current_version": 1, "versions": [
      {"version": 1, "file": "versions/scenes/客厅_v1.png", "prompt": "", "created_at": "2026-01-01T00:00:00+00:00"}
    ]}
  }
}`)
	writeVersionFile(t, dir, "versions/scenes/客厅_v1.png", "v1")

	status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/versions/scenes/客厅/restore/9", "")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if detail, ok := body["detail"].(string); !ok || !strings.Contains(detail, "9") {
		t.Fatalf("detail = %v", body["detail"])
	}
}

func writeVersionFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
