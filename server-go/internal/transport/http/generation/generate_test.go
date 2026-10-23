package generation

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/queue"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/generation"
	"github.com/shotwise/shotwise/migrations"
)

// generateFixture 起一个带真实 sqlite 库的生成路由壳：preflight / 能力解析 / 入队都读
// 真实存储，假实现验不出这些分支。
type generateFixture struct {
	router *gin.Engine
	store  *service.ProjectStore
	repo   *repository.TaskRepo
}

func newGenerateFixture(t *testing.T) generateFixture {
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
	if _, err := store.Create("p1", "测试项目", project.ContentNarration, project.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.NoRoute(func(c *gin.Context) { c.JSON(http.StatusNotFound, gin.H{"detail": "Not Found"}) })
	v1 := r.Group("/api/v1")
	q := queue.NewQueue(gdb)
	RegisterGenerate(v1, GenerateDeps{
		Store: store,
		Generate: generation.NewGenerateService(
			store, q, config.NewResolver(gdb), generation.NewAudioGenerator(gdb, nil)),
	})
	return generateFixture{router: r, store: store, repo: repository.NewTaskRepo(gdb)}
}

func (f generateFixture) seedScript(t *testing.T, segments []any) {
	t.Helper()
	script := map[string]any{
		"content_mode": "narration",
		"episode":      1,
		"segments":     segments,
	}
	if err := f.store.WriteScriptJSON("p1", "episode_1.json", script); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if err := f.store.Update("p1", func(pf *project.ProjectFile) error {
		pf.Episodes = append(pf.Episodes, project.EpisodeMeta{Episode: 1, ScriptFile: "scripts/episode_1.json"})
		return nil
	}); err != nil {
		t.Fatalf("register episode: %v", err)
	}
}

func (f generateFixture) post(t *testing.T, path string, body any, lang string) (int, map[string]any) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
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
			t.Fatalf("decode body %q: %v", w.Body.String(), err)
		}
	}
	return w.Code, out
}

// TestGenerateStoryboardSubmitsAndLocalizesMessage 校验分镜入队的响应形状与提交文案的
// 本地化：message 由服务层 message_key 在传输层按 locale 渲染，两个语言各出一条。
func TestGenerateStoryboardSubmitsAndLocalizesMessage(t *testing.T) {
	f := newGenerateFixture(t)
	f.seedScript(t, []any{
		map[string]any{"segment_id": "E1S1", "duration_seconds": 4, "novel_text": "第一段"},
	})

	path := "/api/v1/projects/p1/generate/storyboard/E1S1"
	status, body := f.post(t, path, map[string]any{
		"prompt":      "雨夜街角",
		"script_file": "episode_1.json",
	}, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["success"] != true || body["task_id"] == nil || body["task_id"] == "" {
		t.Fatalf("body = %v", body)
	}
	if body["message"] != "分镜「E1S1」生成任务已提交" {
		t.Fatalf("message = %v", body["message"])
	}

	status, body = f.post(t, path, map[string]any{
		"prompt":      "雨夜街角",
		"script_file": "episode_1.json",
	}, "en")
	// 同 resource_id 重复入队：去重返回既有任务，文案仍是本语言的。
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["deduped"] != true {
		t.Fatalf("重复入队应去重: %v", body)
	}
	if body["message"] != "Storyboard generation task for 'E1S1' submitted" {
		t.Fatalf("message = %v", body["message"])
	}
}

// TestGenerateStoryboardSegmentNotFound 校验未知分镜是 404 而不是提交成功。
func TestGenerateStoryboardSegmentNotFound(t *testing.T) {
	f := newGenerateFixture(t)
	f.seedScript(t, []any{map[string]any{"segment_id": "E1S1"}})

	status, body := f.post(t, "/api/v1/projects/p1/generate/storyboard/E9S9", map[string]any{
		"prompt":      "雨夜街角",
		"script_file": "episode_1.json",
	}, "en")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "E9S9") {
		t.Fatalf("detail = %v", body["detail"])
	}
}

// TestGenerateStoryboardPreflightFailure 校验资产预检失败在入队前返回 400，
// 而不是让任务进去异步失败。
func TestGenerateStoryboardPreflightFailure(t *testing.T) {
	f := newGenerateFixture(t)
	f.seedScript(t, []any{
		map[string]any{
			"segment_id":            "E1S1",
			"characters_in_segment": []any{"未登记角色"},
		},
	})

	status, body := f.post(t, "/api/v1/projects/p1/generate/storyboard/E1S1", map[string]any{
		"prompt":      "雨夜街角",
		"script_file": "episode_1.json",
	}, "zh")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v", status, body)
	}
	detail, _ := body["detail"].(string)
	if !strings.Contains(detail, "未登记角色") {
		t.Fatalf("detail = %v", body["detail"])
	}
}

// TestGenerateVideoRejectsReferenceRoute 校验参考生视频路线在分镜图检查之前就被拒
// （409），避免拿到「请先生成分镜图」的误导指引。
func TestGenerateVideoRejectsReferenceRoute(t *testing.T) {
	f := newGenerateFixture(t)
	if err := f.store.Update("p1", func(pf *project.ProjectFile) error {
		pf.GenerationMode = string(project.GenerationReferenceVideo)
		return nil
	}); err != nil {
		t.Fatalf("switch generation mode: %v", err)
	}
	f.seedScript(t, []any{map[string]any{"segment_id": "E1S1"}})

	status, body := f.post(t, "/api/v1/projects/p1/generate/video/E1S1", map[string]any{
		"prompt":      "雨夜街角",
		"script_file": "episode_1.json",
	}, "en")
	if status != http.StatusConflict {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

// TestGenerateVideoRequiresStoryboard 校验没有分镜图时是 400 generate_storyboard_first。
func TestGenerateVideoRequiresStoryboard(t *testing.T) {
	f := newGenerateFixture(t)
	f.seedScript(t, []any{map[string]any{"segment_id": "E1S1"}})

	status, body := f.post(t, "/api/v1/projects/p1/generate/video/E1S1", map[string]any{
		"prompt":      "雨夜街角",
		"script_file": "episode_1.json",
	}, "zh")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v", status, body)
	}
	detail, _ := body["detail"].(string)
	if !strings.Contains(detail, "分镜") {
		t.Fatalf("detail = %v", body["detail"])
	}
}

// TestGenerateTTSRequiresNovelText 校验没有小说原文的分段拒绝入队。
func TestGenerateTTSRequiresNovelText(t *testing.T) {
	f := newGenerateFixture(t)
	f.seedScript(t, []any{map[string]any{"segment_id": "E1S1", "novel_text": "   "}})

	status, body := f.post(t, "/api/v1/projects/p1/generate/tts/E1S1", map[string]any{
		"script_file": "episode_1.json",
	}, "en")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v", status, body)
	}
	detail, _ := body["detail"].(string)
	if !strings.Contains(detail, "novel text") {
		t.Fatalf("detail = %v", body["detail"])
	}
}

// TestGenerateAssetReusesExistingSheet 校验已有设计图且未要求重生成时直接复用，
// 不产生新任务。
func TestGenerateAssetReusesExistingSheet(t *testing.T) {
	f := newGenerateFixture(t)
	dir, err := f.store.Dir("p1")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	writeVersionFile(t, dir, "characters/甲.png", "png")
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

	status, body := f.post(t, "/api/v1/projects/p1/generate/character/甲", map[string]any{
		"prompt": "水墨角色",
	}, "zh")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["reused"] != true || body["task_id"] != nil {
		t.Fatalf("应复用既有设计图: %v", body)
	}
	if body["message"] != "资产“甲”已有可用设计图，已跳过重复生成" {
		t.Fatalf("message = %v", body["message"])
	}

	// regenerate=true 时应真正入队。
	status, body = f.post(t, "/api/v1/projects/p1/generate/character/甲", map[string]any{
		"prompt": "水墨角色", "regenerate": true,
	}, "zh")
	if status != http.StatusOK || body["reused"] != false || body["task_id"] == nil {
		t.Fatalf("regenerate 应入队: status=%d body=%v", status, body)
	}
}

// TestGenerateAssetUnknownName 校验未登记资产是 404（而非 400/500）。
func TestGenerateAssetUnknownName(t *testing.T) {
	f := newGenerateFixture(t)
	status, body := f.post(t, "/api/v1/projects/p1/generate/scene/不存在", map[string]any{
		"prompt": "水墨场景",
	}, "zh")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

// TestEditImageValidatesInput 校验图片编辑的三条入参守卫：资源类型、编辑指令、当前图。
func TestEditImageValidatesInput(t *testing.T) {
	f := newGenerateFixture(t)

	cases := []struct {
		body   map[string]any
		status int
	}{
		{map[string]any{"resource_type": "unknown", "resource_id": "甲", "instruction": "改成夜景"}, http.StatusBadRequest},
		{map[string]any{"resource_type": "character", "resource_id": "甲", "instruction": "  "}, http.StatusBadRequest},
		{map[string]any{"resource_type": "storyboard", "resource_id": "E1S1", "instruction": "改成夜景"}, http.StatusBadRequest},
	}
	for _, entry := range cases {
		status, body := f.post(t, "/api/v1/projects/p1/edit/image", entry.body, "en")
		if status != entry.status {
			t.Fatalf("%v: status = %d body=%v", entry.body, status, body)
		}
	}
}

// TestVoiceSampleRejectsMalformedInput 校验试听样本的空文本与超长文本守卫。
func TestVoiceSampleRejectsMalformedInput(t *testing.T) {
	f := newGenerateFixture(t)
	path := "/api/v1/projects/p1/characters/%E7%94%B2/voice-sample"

	status, body := f.post(t, path, map[string]any{"text": "  ", "voice": "v1"}, "en")
	if status != http.StatusBadRequest {
		t.Fatalf("空文本: status = %d body=%v", status, body)
	}
	long := strings.Repeat("字", generation.VoiceSampleTextMaxLength+1)
	status, body = f.post(t, path, map[string]any{"text": long, "voice": "v1"}, "en")
	if status != http.StatusBadRequest {
		t.Fatalf("超长文本: status = %d body=%v", status, body)
	}
	status, body = f.post(t, path, map[string]any{"text": "你好", "voice": "  "}, "en")
	if status != http.StatusBadRequest {
		t.Fatalf("缺音色: status = %d body=%v", status, body)
	}
}

// TestConfirmVoiceSampleRejectsForeignTask 校验归属校验：task_id 指向别的任务时 404，
// 不落任何资产。
func TestConfirmVoiceSampleRejectsForeignTask(t *testing.T) {
	f := newGenerateFixture(t)
	task, err := f.repo.Enqueue(t.Context(), repository.EnqueueParams{
		ProjectName: "p1",
		TaskType:    "voice_sample",
		MediaType:   "audio",
		ResourceID:  "乙",
		Payload:     map[string]any{"prompt": "你好"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	status, body := f.post(t, "/api/v1/projects/p1/characters/%E7%94%B2/voice-sample/confirm",
		map[string]any{"task_id": task.TaskID}, "en")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

// seedSceneImage 给场景资产落一张当前图，供画布端点解析源图。
func (f generateFixture) seedSceneImage(t *testing.T, name string) {
	t.Helper()
	dir, err := f.store.Dir("p1")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	writeVersionFile(t, dir, "scenes/"+name+".png", "png")
	if err := f.store.Update("p1", func(pf *project.ProjectFile) error {
		if pf.Scenes == nil {
			pf.Scenes = project.AssetBucket{}
		}
		pf.Scenes[name] = project.WrapAssetEntry(map[string]any{
			"scene_sheet": "scenes/" + name + ".png",
			"description": "测试场景",
		})
		return nil
	}); err != nil {
		t.Fatalf("seed scene: %v", err)
	}
}

// TestCanvasSplitSubmitsTask 校验宫格切分入队：资源标识由「类型:名字」派生，
// 载荷带 operation 语义的 prompt。
func TestCanvasSplitSubmitsTask(t *testing.T) {
	f := newGenerateFixture(t)
	f.seedSceneImage(t, "客厅")

	status, body := f.post(t, "/api/v1/projects/p1/canvas-images/split", map[string]any{
		"source_kind":   "project",
		"resource_type": "scene",
		"resource_id":   "客厅",
		"rows":          3,
		"cols":          3,
	}, "zh")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["success"] != true || body["task_id"] == nil {
		t.Fatalf("body = %v", body)
	}

	task, err := f.repo.Get(t.Context(), body["task_id"].(string))
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.TaskType != "canvas_image_split" || task.ResourceID != "scene:客厅" || task.MediaType != "image" {
		t.Fatalf("task = %+v", task)
	}
	if task.Payload["prompt"] != "canvas_image_split" {
		t.Fatalf("payload = %v", task.Payload)
	}
	// include_split_lines 有默认值，未提供时不落载荷（exclude_none 语义）。
	if _, ok := task.Payload["include_split_lines"]; ok {
		t.Fatalf("不应写入未提供的字段: %v", task.Payload)
	}
}

// TestCanvasSplitRejectsBadGrid 校验 1..8 之外的宫格参数是 400 而不是 422。
func TestCanvasSplitRejectsBadGrid(t *testing.T) {
	f := newGenerateFixture(t)
	f.seedSceneImage(t, "客厅")

	for _, dims := range [][2]int{{1, 3}, {3, 9}, {0, 0}} {
		status, body := f.post(t, "/api/v1/projects/p1/canvas-images/split", map[string]any{
			"source_kind":   "project",
			"resource_type": "scene",
			"resource_id":   "客厅",
			"rows":          dims[0],
			"cols":          dims[1],
		}, "en")
		if status != http.StatusBadRequest {
			t.Fatalf("%v: status = %d body=%v", dims, status, body)
		}
	}
}

// TestCanvasSplitRequiresFields 校验缺字段是 FastAPI 风格 422，loc 指到具体字段。
func TestCanvasSplitRequiresFields(t *testing.T) {
	f := newGenerateFixture(t)
	status, body := f.post(t, "/api/v1/projects/p1/canvas-images/split", map[string]any{
		"source_kind": "project",
	}, "en")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%v", status, body)
	}
	items, _ := body["detail"].([]any)
	if len(items) != 2 {
		t.Fatalf("detail = %v", body["detail"])
	}
}

// TestCanvasSplitRejectsUnknownSource 校验未登记的源图不产生任务。
func TestCanvasSplitRejectsUnknownSource(t *testing.T) {
	f := newGenerateFixture(t)
	status, body := f.post(t, "/api/v1/projects/p1/canvas-images/split", map[string]any{
		"source_kind":   "project",
		"resource_type": "scene",
		"resource_id":   "不存在",
		"rows":          2,
		"cols":          2,
	}, "en")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v", status, body)
	}
	// 来源非法（source_kind 不认识）同样 400。
	status, body = f.post(t, "/api/v1/projects/p1/canvas-images/split", map[string]any{
		"source_kind": "upload",
		"rows":        2,
		"cols":        2,
	}, "en")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

// TestCanvasAdvancedRejectsUnknownOperation 校验操作白名单外的 operation 是 400。
func TestCanvasAdvancedRejectsUnknownOperation(t *testing.T) {
	f := newGenerateFixture(t)
	f.seedSceneImage(t, "客厅")

	status, body := f.post(t, "/api/v1/projects/p1/canvas-images/advanced", map[string]any{
		"source_kind":   "project",
		"resource_type": "scene",
		"resource_id":   "客厅",
		"operation":     "canvas_image_magic",
	}, "en")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v", status, body)
	}

	status, body = f.post(t, "/api/v1/projects/p1/canvas-images/advanced", map[string]any{
		"source_kind":   "project",
		"resource_type": "scene",
		"resource_id":   "客厅",
		"operation":     "canvas_image_hd",
		"quality":       "4K",
	}, "zh")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	task, err := f.repo.Get(t.Context(), body["task_id"].(string))
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.TaskType != "canvas_image_hd" || task.Payload["quality"] != "4K" {
		t.Fatalf("task = %+v payload=%v", task, task.Payload)
	}
}
