package files

import (
	"bytes"
	"encoding/json"
	servicescript "github.com/shotwise/shotwise/internal/service/script"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit/httpkittest"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/migrations"
)

// filesFixture 起一个文件管理壳（带鉴权注入）。
type filesFixture struct {
	router *gin.Engine
	store  *service.ProjectStore
}

func newFilesFixture(t *testing.T) filesFixture {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "t.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Migrate(gdb, dialect, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if pool, err := gdb.DB(); err == nil {
			_ = pool.Close()
		}
	})
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "测试项目", domain.ContentDrama, domain.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := store.Update("p1", func(pf *domain.ProjectFile) error {
		pf.Episodes = append(pf.Episodes, domain.EpisodeMeta{Episode: 1, ScriptFile: "scripts/episode_1.json"})
		return nil
	}); err != nil {
		t.Fatalf("register episode: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	deps := FilesDeps{
		Store:  store,
		Files:  servicescript.NewFileService(store),
		Review: servicescript.NewScriptReviewService(store, config.NewResolver(gdb)),
	}
	RegisterPublicFiles(r.Group("/api/v1"), deps)
	// 鉴权关闭时中间件注入 admin 身份。
	authService := auth.New(nil, false, nil)
	RegisterFiles(r.Group("/api/v1", authService.Middleware()), deps)
	return filesFixture{router: r, store: store}
}

func (f filesFixture) do(t *testing.T, method, path string, body []byte) (int, string, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	out := map[string]any{}
	if w.Body.Len() > 0 && strings.Contains(w.Header().Get("Content-Type"), "json") {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, w.Body.String(), out
}

// TestSourceFileRoundtrip 校验 source 文本文件的写读删与 raw 级联。
func TestSourceFileRoundtrip(t *testing.T) {
	f := newFilesFixture(t)
	status, _, body := f.do(t, http.MethodPut, "/api/v1/projects/p1/source/novel.txt", []byte("第一章\n正文"))
	if status != http.StatusOK || body["path"] != "source/novel.txt" {
		t.Fatalf("status = %d body=%v", status, body)
	}
	status, text, _ := f.do(t, http.MethodGet, "/api/v1/projects/p1/source/novel.txt", nil)
	if status != http.StatusOK || !strings.Contains(text, "第一章") {
		t.Fatalf("status = %d text=%q", status, text)
	}

	// 清单里能看到该文件与 URL
	status, _, _ = f.do(t, http.MethodGet, "/api/v1/projects/p1/files", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	// 删除 + raw 级联
	projectDir, err := f.store.Dir("p1")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	rawDir := filepath.Join(projectDir, "source", "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rawDir, "novel.docx"), []byte("raw"), 0o644); err != nil {
		t.Fatalf("write raw: %v", err)
	}
	status, _, _ = f.do(t, http.MethodDelete, "/api/v1/projects/p1/source/novel.txt", nil)
	if status != http.StatusOK {
		t.Fatalf("delete status = %d", status)
	}
	if _, err := os.Stat(filepath.Join(rawDir, "novel.docx")); !os.IsNotExist(err) {
		t.Fatalf("raw 备份应级联删除: %v", err)
	}

	// 再删一次：404
	status, _, _ = f.do(t, http.MethodDelete, "/api/v1/projects/p1/source/novel.txt", nil)
	if status != http.StatusNotFound {
		t.Fatalf("二次删除应 404: status = %d", status)
	}
}

// TestSourceFileRejectsTraversal 校验越界路径 403。
func TestSourceFileRejectsTraversal(t *testing.T) {
	f := newFilesFixture(t)
	status, _, _ := f.do(t, http.MethodGet, "/api/v1/projects/p1/source/..%2F..%2Fproject.json", nil)
	if status != http.StatusForbidden && status != http.StatusNotFound {
		t.Fatalf("越界应 403/404: status = %d", status)
	}
}

// TestStaticFileServing 校验公开静态端点：可读、nosniff、越界 403。
func TestStaticFileServing(t *testing.T) {
	f := newFilesFixture(t)
	projectDir, err := f.store.Dir("p1")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, "scenes"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "scenes", "客厅.png"), httpkittest.TinyPNG(), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/files/p1/scenes/客厅.png", nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("缺 nosniff: %v", w.Header())
	}

	// 越界：403
	req = httptest.NewRequest(http.MethodGet, "/api/v1/files/p1/../../project.json", nil)
	w = httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Fatalf("越界应 403/404: status = %d", w.Code)
	}
}

// TestDraftDramaValidation 校验 drama step1 写入的 JSON 形状校验。
func TestDraftDramaValidation(t *testing.T) {
	f := newFilesFixture(t)
	// 非法：空 scenes
	status, _, _ := f.do(t, http.MethodPut,
		"/api/v1/projects/p1/drafts/1/step1", []byte(`{"scenes": []}`))
	if status != http.StatusBadRequest {
		t.Fatalf("空 scenes 应 400: status = %d", status)
	}
	// 非法：scene_id 缺失
	status, _, _ = f.do(t, http.MethodPut,
		"/api/v1/projects/p1/drafts/1/step1", []byte(`{"scenes": [{"title": "无 id"}]}`))
	if status != http.StatusBadRequest {
		t.Fatalf("缺 scene_id 应 400: status = %d", status)
	}
	// 合法
	valid := `{"scenes": [{"scene_id": "S1", "title": "开场"}]}`
	status, _, body := f.do(t, http.MethodPut, "/api/v1/projects/p1/drafts/1/step1", []byte(valid))
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(body["path"].(string), "drafts/episode_1/") {
		t.Fatalf("path = %v", body["path"])
	}
	// 读回
	status, text, _ := f.do(t, http.MethodGet, "/api/v1/projects/p1/drafts/1/step1", nil)
	if status != http.StatusOK || !strings.Contains(text, "S1") {
		t.Fatalf("status = %d text=%q", status, text)
	}
	// 删除
	status, _, _ = f.do(t, http.MethodDelete, "/api/v1/projects/p1/drafts/1/step1", nil)
	if status != http.StatusOK {
		t.Fatalf("delete status = %d", status)
	}
	// 再删：404
	status, _, _ = f.do(t, http.MethodDelete, "/api/v1/projects/p1/drafts/1/step1", nil)
	if status != http.StatusNotFound {
		t.Fatalf("二次删除应 404: status = %d", status)
	}
}

// TestDraftRejectsStepTwo 校验 step2 不在草稿白名单（只有 step1）。
func TestDraftRejectsStepTwo(t *testing.T) {
	f := newFilesFixture(t)
	status, _, _ := f.do(t, http.MethodGet, "/api/v1/projects/p1/drafts/1/step2", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d", status)
	}
}

// TestDeleteCharacterRefAudioMissing 校验删除不存在角色的参考音频是 404。
func TestDeleteCharacterRefAudioMissing(t *testing.T) {
	f := newFilesFixture(t)
	status, _, _ := f.do(t, http.MethodDelete, "/api/v1/projects/p1/characters/甲/reference-audio", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d", status)
	}
}
