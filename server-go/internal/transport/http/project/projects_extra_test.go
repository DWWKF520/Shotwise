package project

import (
	"bytes"
	"encoding/json"
	servicescript "github.com/shotwise/shotwise/internal/service/script"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/queue"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/migrations"
)

// projectExtraFixture 起一个项目扩展端点壳。
type projectExtraFixture struct {
	router *gin.Engine
	store  *service.ProjectStore
}

func newProjectExtraFixture(t *testing.T, contentMode project.ContentMode) projectExtraFixture {
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
	if _, err := store.Create("p1", "测试项目", contentMode, project.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := store.Update("p1", func(pf *project.ProjectFile) error {
		// ad 项目创建即带第 1 集单条，不再追加集；其余模式登记第 1、2 两集。
		if contentMode == project.ContentAd {
			return nil
		}
		pf.Episodes = append(pf.Episodes,
			project.EpisodeMeta{Episode: 1, ScriptFile: "scripts/episode_1.json"},
			project.EpisodeMeta{Episode: 2, ScriptFile: "scripts/episode_2.json"},
		)
		return nil
	}); err != nil {
		t.Fatalf("register episodes: %v", err)
	}
	writeExtraScript := func(episode int, extra map[string]any) {
		script := map[string]any{
			"content_mode": string(contentMode),
			"episode":      episode,
			"title":        "第" + httpkit.ItoaInt(episode) + "集",
			"metadata":     map[string]any{"status": "draft"},
		}
		for key, value := range extra {
			script[key] = value
		}
		if err := store.WriteScriptJSON("p1", "episode_"+itoInt(episode)+".json", script); err != nil {
			t.Fatalf("write script: %v", err)
		}
	}
	writeExtraScript(1, nil)
	writeExtraScript(2, nil)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterProjectExtras(r.Group("/api/v1"), ProjectExtraDeps{
		Store: store,
		Extra: servicescript.NewProjectExtraService(store, queue.NewQueue(gdb), config.NewResolver(gdb)),
	})
	return projectExtraFixture{router: r, store: store}
}

// itoInt 与 itoaInt 等价（文件内已有 itoaInt，这里避免重名冲突不需要）。
func itoInt(v int) string { return httpkit.ItoaInt(v) }

func (f projectExtraFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
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

// TestProjectScriptRoundtrip 校验剧本读取与 404。
func TestProjectScriptRoundtrip(t *testing.T) {
	f := newProjectExtraFixture(t, project.ContentDrama)
	status, body := f.do(t, http.MethodGet, "/api/v1/projects/p1/scripts/episode_1.json", nil)
	if status != http.StatusOK || body["script"] == nil {
		t.Fatalf("status = %d body=%v", status, body)
	}
	status, body = f.do(t, http.MethodGet, "/api/v1/projects/p1/scripts/missing.json", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

// TestScriptScenePatchWhitelist 校验 drama 场景 PATCH：白名单内字段生效、未命中 404。
func TestScriptScenePatchWhitelist(t *testing.T) {
	f := newProjectExtraFixture(t, project.ContentDrama)
	script := map[string]any{
		"content_mode": "drama",
		"episode":      1,
		"scenes": []any{
			map[string]any{"scene_id": "S1", "duration_seconds": 4, "note": "原备注"},
		},
	}
	if err := f.store.WriteScriptJSON("p1", "episode_1.json", script); err != nil {
		t.Fatalf("write: %v", err)
	}
	status, body := f.do(t, http.MethodPatch, "/api/v1/projects/p1/script-scenes/S1", map[string]any{
		"script_file": "episode_1.json",
		"updates": map[string]any{
			"duration_seconds": 8,
			"note":             nil, // note 允许显式置空
			"shot_id":          "hack",
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	scene := body["scene"].(map[string]any)
	if scene["duration_seconds"] != float64(8) {
		t.Fatalf("scene = %v", scene)
	}
	if value, present := scene["note"]; present && value != nil {
		t.Fatalf("note 应被置空: %v", scene)
	}
	if _, hacked := scene["shot_id"]; hacked {
		t.Fatalf("白名单外字段不应写入: %v", scene)
	}

	// 未命中 → 404
	status, _ = f.do(t, http.MethodPatch, "/api/v1/projects/p1/script-scenes/S9", map[string]any{
		"script_file": "episode_1.json", "updates": map[string]any{"duration_seconds": 8},
	})
	if status != http.StatusNotFound {
		t.Fatalf("status = %d", status)
	}
}

// TestScriptShotPatchAndReorder 校验 ad 镜头 PATCH 与全排列重排。
func TestScriptShotPatchAndReorder(t *testing.T) {
	f := newProjectExtraFixture(t, project.ContentAd)
	script := map[string]any{
		"content_mode": "ad",
		"shots": []any{
			map[string]any{"shot_id": "A", "voiceover_text": "第一镜"},
			map[string]any{"shot_id": "B", "voiceover_text": "第二镜"},
		},
	}
	if err := f.store.WriteScriptJSON("p1", "episode_1.json", script); err != nil {
		t.Fatalf("write: %v", err)
	}

	status, body := f.do(t, http.MethodPatch, "/api/v1/projects/p1/script-shots/B", map[string]any{
		"script_file": "episode_1.json",
		"updates":     map[string]any{"voiceover_text": "改写", "shot_id": "hack"},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["shot"].(map[string]any)["voiceover_text"] != "改写" {
		t.Fatalf("shot = %v", body["shot"])
	}

	// reorder：非全排列 400
	status, _ = f.do(t, http.MethodPost, "/api/v1/projects/p1/script-shots/reorder", map[string]any{
		"script_file": "episode_1.json", "shot_ids": []string{"B"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("非全排列应 400: status = %d", status)
	}
	// 合法全排列
	status, body = f.do(t, http.MethodPost, "/api/v1/projects/p1/script-shots/reorder", map[string]any{
		"script_file": "episode_1.json", "shot_ids": []string{"B", "A"},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	shots, _ := body["shots"].([]any)
	if shots[0].(map[string]any)["shot_id"] != "B" {
		t.Fatalf("shots = %v", shots)
	}
}

// TestEpisodeDeleteRenumbersDisplay 校验删集：取消计数、剩余集重排显示编号、剧本入已删清单。
func TestEpisodeDeleteRenumbersDisplay(t *testing.T) {
	f := newProjectExtraFixture(t, project.ContentNarration)
	status, body := f.do(t, http.MethodDelete, "/api/v1/projects/p1/episodes/1", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["deleted_episode"] != float64(1) || body["success"] != true {
		t.Fatalf("body = %v", body)
	}
	pf, err := f.store.Get("p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(pf.Episodes) != 1 || pf.Episodes[0].Episode != 2 {
		t.Fatalf("episodes = %+v", pf.Episodes)
	}
	if pf.Episodes[0].DisplayEpisode == nil || *pf.Episodes[0].DisplayEpisode != 1 {
		t.Fatalf("剩余集显示编号应重排为 1: %+v", pf.Episodes[0])
	}
	deletedKnown := false
	for _, name := range pf.DeletedEpisodeScriptFiles {
		if name == "scripts/episode_1.json" {
			deletedKnown = true
		}
	}
	if !deletedKnown {
		t.Fatalf("已删剧本应入清单: %v", pf.DeletedEpisodeScriptFiles)
	}
	// 再删一次：404
	status, _ = f.do(t, http.MethodDelete, "/api/v1/projects/p1/episodes/1", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d", status)
	}
}

// TestEpisodeDisplayNumberMove 校验显示编号调整。
func TestEpisodeDisplayNumberMove(t *testing.T) {
	f := newProjectExtraFixture(t, project.ContentNarration)
	status, _ := f.do(t, http.MethodPatch, "/api/v1/projects/p1/episodes/2/display-number",
		map[string]any{"display_episode": 1})
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	pf, err := f.store.Get("p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	byEpisode := map[int]*int{}
	for i := range pf.Episodes {
		byEpisode[pf.Episodes[i].Episode] = pf.Episodes[i].DisplayEpisode
	}
	if byEpisode[2] == nil || *byEpisode[2] != 1 || byEpisode[1] == nil || *byEpisode[1] != 2 {
		t.Fatalf("编号应交换: %+v", pf.Episodes)
	}
}

// TestEpisodeTitleSyncsMirror 校验标题改的是剧本顶层并镜像回 project.json。
func TestEpisodeTitleSyncsMirror(t *testing.T) {
	f := newProjectExtraFixture(t, project.ContentNarration)
	status, body := f.do(t, http.MethodPatch, "/api/v1/projects/p1/episodes/1", map[string]any{
		"title": "新标题",
	})
	if status != http.StatusOK || body["success"] != true {
		t.Fatalf("status = %d body=%v", status, body)
	}
	script, err := f.store.ReadScriptJSON("p1", "episode_1.json")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if script["title"] != "新标题" {
		t.Fatalf("剧本 title = %v", script["title"])
	}
	pf, err := f.store.Get("p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if pf.Episodes[0].Title != "新标题" {
		t.Fatalf("镜像未同步: %+v", pf.Episodes[0])
	}
	// 空标题 422
	status, _ = f.do(t, http.MethodPatch, "/api/v1/projects/p1/episodes/1", map[string]any{"title": "  "})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", status)
	}
}

// TestOverviewPatch 校验概述字段级覆盖。
func TestOverviewPatch(t *testing.T) {
	f := newProjectExtraFixture(t, project.ContentNarration)
	status, body := f.do(t, http.MethodPatch, "/api/v1/projects/p1/overview", map[string]any{
		"synopsis": "新梗概",
	})
	if status != http.StatusOK || body["success"] != true {
		t.Fatalf("status = %d body=%v", status, body)
	}
	overview := body["overview"].(map[string]any)
	if overview["synopsis"] != "新梗概" {
		t.Fatalf("overview = %v", overview)
	}
	pf, err := f.store.Get("p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if pf.Overview == nil || pf.Overview.Synopsis != "新梗概" {
		t.Fatalf("overview 未落盘: %+v", pf.Overview)
	}
}

// TestSegmentPatchRequiresNarration 校验 segments PATCH 的模式守卫。
func TestSegmentPatchRequiresNarration(t *testing.T) {
	f := newProjectExtraFixture(t, project.ContentDrama)
	status, _ := f.do(t, http.MethodPatch, "/api/v1/projects/p1/segments/E1S1", map[string]any{
		"script_file": "episode_1.json", "duration_seconds": 9,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("drama 应被拒: status = %d", status)
	}
}
