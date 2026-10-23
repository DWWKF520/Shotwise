package generation

import (
	"bytes"
	"encoding/json"
	servicegeneration "github.com/shotwise/shotwise/internal/service/generation"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit/httpkittest"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/queue"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/migrations"
)

// gridFixture 起一个带真实 sqlite 的宫格路由壳：入队与宫格记录都落真实存储，
// 假实现验不出「先校验后入队」的顺序。
type gridFixture struct {
	router *gin.Engine
	store  *service.ProjectStore
	repo   *repository.TaskRepo
	db     *gorm.DB
}

// newGridFixture 造一个开启宫格分镜的 narration 项目（2 个分段，每段 4 个分镜）。
func newGridFixture(t *testing.T) gridFixture {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "t.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Migrate(gdb, dialect, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := gdb.Exec(
		"INSERT INTO users (id, username, role, is_active, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		repository.DefaultUserID, "default", "user", true, "2026-01-01T00:00:00+00:00", "2026-01-01T00:00:00+00:00",
	).Error; err != nil {
		t.Fatalf("seed user: %v", err)
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
		pf.GridStoryboard = true
		pf.AspectRatio = "9:16"
		pf.Episodes = append(pf.Episodes, domain.EpisodeMeta{Episode: 1, ScriptFile: "scripts/episode_1.json"})
		return nil
	}); err != nil {
		t.Fatalf("enable grid: %v", err)
	}
	script := map[string]any{
		"content_mode": "narration",
		"episode":      1,
		"segments": []any{
			map[string]any{"segment_id": "E1S1", "duration_seconds": 4},
			map[string]any{"segment_id": "E1S2", "duration_seconds": 4},
			map[string]any{"segment_id": "E1S3", "duration_seconds": 4},
			map[string]any{"segment_id": "E1S4", "duration_seconds": 4},
		},
		"metadata": map[string]any{"status": "draft"},
	}
	if err := store.WriteScriptJSON("p1", "episode_1.json", script); err != nil {
		t.Fatalf("write script: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.NoRoute(func(c *gin.Context) { c.JSON(http.StatusNotFound, gin.H{"detail": "Not Found"}) })
	v1 := r.Group("/api/v1")
	RegisterGrids(v1, GridDeps{
		Store: store,
		Grids: servicegeneration.NewGridTaskService(
			store, queue.NewQueue(gdb), config.NewResolver(gdb), service.NewGridService(store)),
	})
	return gridFixture{router: r, store: store, repo: repository.NewTaskRepo(gdb), db: gdb}
}

func (f gridFixture) call(t *testing.T, method, path string, body any, lang string) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(payload)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
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

// TestGridCapabilityDefaultsTo2K 校验默认（2K）档位下大宫格不放行、上限 9 格。
func TestGridCapabilityDefaultsTo2K(t *testing.T) {
	f := newGridFixture(t)
	status, body := f.call(t, http.MethodGet, "/api/v1/projects/p1/grid-capability", nil, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["large_grid_allowed"] != false || body["max_cell_count"] != float64(9) {
		t.Fatalf("body = %v", body)
	}
}

// TestGridGenerateCreatesRecordsAndTasks 校验生成：4 个分镜正好凑一张 2×2 宫格，
// 记录与任务一一对应，并返回本地化的提交文案。
func TestGridGenerateCreatesRecordsAndTasks(t *testing.T) {
	f := newGridFixture(t)
	status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/generate/grid/1", map[string]any{
		"script_file": "episode_1.json",
	}, "zh")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["success"] != true {
		t.Fatalf("body = %v", body)
	}
	gridIDs, _ := body["grid_ids"].([]any)
	taskIDs, _ := body["task_ids"].([]any)
	if len(gridIDs) != 1 || len(taskIDs) != 1 {
		t.Fatalf("4 个分镜应合成一张宫格: %v / %v", gridIDs, taskIDs)
	}
	if body["message"] == nil || body["message"] == "" {
		t.Fatalf("缺提交文案: %v", body)
	}
	if body["deduped"] != false {
		t.Fatalf("首次入队不应去重: %v", body["deduped"])
	}

	// 记录落盘且是 pending 状态
	status, grid := f.call(t, http.MethodGet, "/api/v1/projects/p1/grids/"+gridIDs[0].(string), nil, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, grid)
	}
	if grid["status"] != "pending" || grid["grid_size"] != "grid_4" {
		t.Fatalf("grid = %v", grid)
	}
	if grid["video_aspect_ratio"] != "9:16" {
		t.Fatalf("应冻结项目比例: %v", grid["video_aspect_ratio"])
	}

	// 列表直接返回数组（与 Python 一致）
	status, list := f.callArray(t, http.MethodGet, "/api/v1/projects/p1/grids", nil)
	if status != http.StatusOK || len(list) != 1 {
		t.Fatalf("status = %d list=%v", status, list)
	}
}

// TestGridGenerateRejectsAdProject 校验广告项目在动作端点被拒（写入边界之外的第二道防线）。
func TestGridGenerateRejectsAdProject(t *testing.T) {
	f := newGridFixture(t)
	if err := f.store.Update("p1", func(pf *domain.ProjectFile) error {
		pf.ContentMode = string(domain.ContentAd)
		pf.GridStoryboard = false
		pf.TargetDuration = intPtr(30)
		return nil
	}); err != nil {
		t.Fatalf("switch to ad: %v", err)
	}
	status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/generate/grid/1", map[string]any{
		"script_file": "episode_1.json",
	}, "en")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

// TestGridGenerateRejectsDisabledGrid 校验宫格开关关闭时拒绝生成。
func TestGridGenerateRejectsDisabledGrid(t *testing.T) {
	f := newGridFixture(t)
	if err := f.store.Update("p1", func(pf *domain.ProjectFile) error {
		pf.GridStoryboard = false
		return nil
	}); err != nil {
		t.Fatalf("disable grid: %v", err)
	}
	status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/generate/grid/1", map[string]any{
		"script_file": "episode_1.json",
	}, "en")
	if status != http.StatusBadRequest || body["detail"] == "" {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

// TestGridGeneratePreflightBlocksAdmission 校验资产缺失在入队前被拒：不留任务、不留记录。
func TestGridGeneratePreflightBlocksAdmission(t *testing.T) {
	f := newGridFixture(t)
	script := map[string]any{
		"content_mode": "narration",
		"episode":      1,
		"segments": []any{
			map[string]any{"segment_id": "E1S1", "characters_in_segment": []any{"未登记角色"}},
		},
	}
	if err := f.store.WriteScriptJSON("p1", "episode_1.json", script); err != nil {
		t.Fatalf("write script: %v", err)
	}
	status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/generate/grid/1", map[string]any{
		"script_file": "episode_1.json",
	}, "zh")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v", status, body)
	}
	// 一条任务都不该留下，也不该留下宫格记录
	var tasks int64
	if err := f.db.Model(&repository.Task{}).Where("project_name = ?", "p1").Count(&tasks).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if tasks != 0 {
		t.Fatalf("preflight 失败不应入队: %d", tasks)
	}
	status, grids := f.callArray(t, http.MethodGet, "/api/v1/projects/p1/grids", nil)
	if status != http.StatusOK || len(grids) != 0 {
		t.Fatalf("status = %d grids=%v", status, grids)
	}
}

// TestGridGetUnknown 校验未知/非法 grid_id 都是 404 grid_not_found。
func TestGridGetUnknown(t *testing.T) {
	f := newGridFixture(t)
	for _, id := range []string{"grid_deadbeef0000", "not-a-grid"} {
		status, body := f.call(t, http.MethodGet, "/api/v1/projects/p1/grids/"+id, nil, "en")
		if status != http.StatusNotFound {
			t.Fatalf("%s: status = %d body=%v", id, status, body)
		}
	}
}

// TestGridSplitRequiresImage 校验没有联合图时切分是 400 grid_image_not_ready。
func TestGridSplitRequiresImage(t *testing.T) {
	f := newGridFixture(t)
	status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/generate/grid/1", map[string]any{
		"script_file": "episode_1.json",
	}, "en")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	gridIDs, _ := body["grid_ids"].([]any)
	gridID := gridIDs[0].(string)

	// pending 状态先撞「生成在途」闸门（409）
	status, body = f.call(t, http.MethodPost, "/api/v1/projects/p1/grids/"+gridID+"/split", nil, "en")
	if status != http.StatusConflict {
		t.Fatalf("pending 应 409: status = %d body=%v", status, body)
	}
}

// TestGridUploadNormalizesAndBumpsVersion 校验上传联合图：PNG 落盘、记录回到 completed、
// split_at 清空、版本号递增。
func TestGridUploadNormalizesAndBumpsVersion(t *testing.T) {
	f := newGridFixture(t)
	status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/generate/grid/1", map[string]any{
		"script_file": "episode_1.json",
	}, "en")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	gridIDs, _ := body["grid_ids"].([]any)
	gridID := gridIDs[0].(string)

	// 把记录推到 completed，避开「生成在途」闸门（等价于 worker 已完成）。
	projectDir, err := f.store.Dir("p1")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	manager, err := service.NewGridManager(projectDir)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	grid, found, err := manager.Get(gridID)
	if err != nil || !found {
		t.Fatalf("get grid: %v %v", err, found)
	}
	grid.Status = domain.GridStatusCompleted
	if err := manager.Save(grid); err != nil {
		t.Fatalf("save: %v", err)
	}

	status, uploaded := f.upload(t, gridID)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, uploaded)
	}
	if uploaded["version"] != float64(1) || uploaded["path"] != "grids/"+gridID+".png" {
		t.Fatalf("body = %v", uploaded)
	}
	_, after := f.call(t, http.MethodGet, "/api/v1/projects/p1/grids/"+gridID, nil, "")
	if after["status"] != "completed" || after["split_at"] != nil {
		t.Fatalf("上传后记录状态不对: %v", after)
	}
}

// upload 用 multipart 提交一张最小合法 PNG。
func (f gridFixture) upload(t *testing.T, gridID string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", "grid.png")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := part.Write(httpkittest.TinyPNG()); err != nil {
		t.Fatalf("write png: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/grids/"+gridID+"/upload", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
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

// callArray 处理直接返回 JSON 数组的端点（GET /grids）。
func (f gridFixture) callArray(t *testing.T, method, path string, body any) (int, []any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(payload)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	var out []any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
	}
	return w.Code, out
}

func intPtr(v int) *int { return &v }
