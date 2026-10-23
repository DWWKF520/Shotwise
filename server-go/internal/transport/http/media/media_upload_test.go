package media

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit/httpkittest"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/asset"
	"github.com/shotwise/shotwise/internal/service/media"
	"github.com/shotwise/shotwise/migrations"
)

// mediaFixture 造一个 narration 项目，用于尾帧与镜头上传。
type mediaFixture struct {
	router *gin.Engine
	store  *service.ProjectStore
}

func newMediaFixture(t *testing.T) mediaFixture {
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
		pf.Episodes = append(pf.Episodes, domain.EpisodeMeta{Episode: 1, ScriptFile: "scripts/episode_1.json"})
		return nil
	}); err != nil {
		t.Fatalf("register episode: %v", err)
	}
	if err := store.WriteScriptJSON("p1", "episode_1.json", map[string]any{
		"content_mode": "narration",
		"episode":      1,
		"segments": []any{
			map[string]any{"segment_id": "E1S1", "duration_seconds": 4},
		},
		"metadata": map[string]any{"status": "draft"},
	}); err != nil {
		t.Fatalf("write script: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.NoRoute(func(c *gin.Context) { c.JSON(http.StatusNotFound, gin.H{"detail": "Not Found"}) })
	v1 := r.Group("/api/v1")
	RegisterEndFrames(v1, EndFrameDeps{Store: store, EndFrame: asset.NewEndFrameService(store)})
	RegisterShotUploads(v1, ShotUploadDeps{Store: store, Uploads: media.NewShotUploadService(store)})
	return mediaFixture{router: r, store: store}
}

func (f mediaFixture) script(t *testing.T) map[string]any {
	t.Helper()
	script, err := f.store.ReadScriptJSON("p1", "episode_1.json")
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	return script
}

// postFile 提交 multipart 文件。
func (f mediaFixture) postFile(t *testing.T, path, filename string, content []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
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

func (f mediaFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
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

// TestEndFrameUploadWritesSnapshotAndField 校验上传尾帧：快照落到固定路径、字段写回相对路径。
func TestEndFrameUploadWritesSnapshotAndField(t *testing.T) {
	f := newMediaFixture(t)
	status, body := f.postFile(t,
		"/api/v1/projects/p1/shots/E1S1/end-frame/upload?script_file=episode_1.json",
		"frame.png", httpkittest.TinyPNG())
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["end_frame_image"] != "end_frames/scene_E1S1.png" {
		t.Fatalf("body = %v", body)
	}
	projectDir, err := f.store.Dir("p1")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "end_frames", "scene_E1S1.png")); err != nil {
		t.Fatalf("快照未落盘: %v", err)
	}
	script := f.script(t)
	items, _ := script["segments"].([]any)
	item := items[0].(map[string]any)
	if item["end_frame_image"] != "end_frames/scene_E1S1.png" {
		t.Fatalf("字段未写回: %v", item["end_frame_image"])
	}

	// 清除：文件删掉、字段置空
	status, body = f.do(t, http.MethodDelete,
		"/api/v1/projects/p1/shots/E1S1/end-frame?script_file=episode_1.json", nil)
	if status != http.StatusOK {
		t.Fatalf("delete status = %d body=%v", status, body)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "end_frames", "scene_E1S1.png")); !os.IsNotExist(err) {
		t.Fatalf("快照应被删除: %v", err)
	}
	item = f.script(t)["segments"].([]any)[0].(map[string]any)
	if item["end_frame_image"] != nil {
		t.Fatalf("字段应置空: %v", item["end_frame_image"])
	}
}

// TestEndFrameSelectCopiesProjectImage 校验选图通道复制快照，且越界路径被拒。
func TestEndFrameSelectCopiesProjectImage(t *testing.T) {
	f := newMediaFixture(t)
	projectDir, err := f.store.Dir("p1")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, "storyboards"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "storyboards", "scene_E1S1.png"), httpkittest.TinyPNG(), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	status, body := f.do(t, http.MethodPost, "/api/v1/projects/p1/shots/E1S1/end-frame/select", map[string]any{
		"script_file": "episode_1.json",
		"source_path": "storyboards/scene_E1S1.png",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["end_frame_image"] != "end_frames/scene_E1S1.png" {
		t.Fatalf("body = %v", body)
	}

	// 越界路径：400
	status, body = f.do(t, http.MethodPost, "/api/v1/projects/p1/shots/E1S1/end-frame/select", map[string]any{
		"script_file": "episode_1.json",
		"source_path": "../../etc/passwd",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("越界应 400: status = %d body=%v", status, body)
	}
	// 不存在的源图：404
	status, body = f.do(t, http.MethodPost, "/api/v1/projects/p1/shots/E1S1/end-frame/select", map[string]any{
		"script_file": "episode_1.json",
		"source_path": "storyboards/nope.png",
	})
	if status != http.StatusNotFound {
		t.Fatalf("缺文件应 404: status = %d body=%v", status, body)
	}
}

// TestEndFrameRejectsReferenceVideoProject 校验参考生视频路线一律拒绝（按项目路线而非剧本戳）。
func TestEndFrameRejectsReferenceVideoProject(t *testing.T) {
	f := newMediaFixture(t)
	if err := f.store.Update("p1", func(pf *domain.ProjectFile) error {
		pf.GenerationMode = string(domain.GenerationReferenceVideo)
		return nil
	}); err != nil {
		t.Fatalf("switch mode: %v", err)
	}
	status, body := f.postFile(t,
		"/api/v1/projects/p1/shots/E1S1/end-frame/upload?script_file=episode_1.json",
		"frame.png", httpkittest.TinyPNG())
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

// TestShotUploadStoryboardWritesVersionAndFields 校验镜头上传：PNG 落 canonical 路径、
// 版本号递增、generated_assets 与 status 同步。
func TestShotUploadStoryboardWritesVersionAndFields(t *testing.T) {
	f := newMediaFixture(t)
	status, body := f.postFile(t,
		"/api/v1/projects/p1/shots/E1S1/upload/storyboard?script_file=episode_1.json",
		"shot.png", httpkittest.TinyPNG())
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["path"] != "storyboards/scene_E1S1.png" || body["version"] != float64(1) {
		t.Fatalf("body = %v", body)
	}
	projectDir, err := f.store.Dir("p1")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "storyboards", "scene_E1S1.png")); err != nil {
		t.Fatalf("分镜图未落盘: %v", err)
	}
	item := f.script(t)["segments"].([]any)[0].(map[string]any)
	assets, _ := item["generated_assets"].(map[string]any)
	if assets["storyboard_image"] != "storyboards/scene_E1S1.png" || assets["status"] != "storyboard_ready" {
		t.Fatalf("assets = %v", assets)
	}
}

// TestShotUploadRejectsWrongExtension 校验扩展名白名单：非图片后缀 400。
func TestShotUploadRejectsWrongExtension(t *testing.T) {
	f := newMediaFixture(t)
	status, body := f.postFile(t,
		"/api/v1/projects/p1/shots/E1S1/upload/storyboard?script_file=episode_1.json",
		"shot.txt", []byte("not an image"))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

// TestShotUploadRejectsUnknownKind 校验 kind 只接受 storyboard / video（422）。
func TestShotUploadRejectsUnknownKind(t *testing.T) {
	f := newMediaFixture(t)
	status, body := f.postFile(t,
		"/api/v1/projects/p1/shots/E1S1/upload/audio?script_file=episode_1.json",
		"a.png", httpkittest.TinyPNG())
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%v", status, body)
	}
}
