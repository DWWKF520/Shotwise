package project

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/service"
)

// projectFixture 挂载项目路由的测试壳（不带鉴权，与 app 同口径的 404 / 405 信封）。
type projectFixture struct {
	router *gin.Engine
	store  *service.ProjectStore
}

func newProjectFixture(t *testing.T) projectFixture {
	t.Helper()
	root := t.TempDir()
	store := service.NewProjectStore(root)
	if _, err := store.Create("p1", "测试项目", project.ContentNarration, project.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.NoRoute(func(c *gin.Context) { c.JSON(http.StatusNotFound, gin.H{"detail": "Not Found"}) })
	r.NoMethod(func(c *gin.Context) { c.JSON(http.StatusMethodNotAllowed, gin.H{"detail": "Method Not Allowed"}) })
	v1 := r.Group("/api/v1")
	deps := ProjectDeps{Store: store, Hub: service.NewEventHub()}
	v1.GET("/projects", ListProjects(deps))
	v1.POST("/projects", CreateProject(deps))
	v1.GET("/projects/:name", GetProject(deps))
	v1.PATCH("/projects/:name", UpdateProject(deps))
	v1.DELETE("/projects/:name", DeleteProject(deps))
	v1.GET("/projects/:name/events/stream", ProjectEventStream(deps))
	return projectFixture{router: r, store: store}
}

func (f projectFixture) call(t *testing.T, method, path string, body any, lang string) (int, map[string]any) {
	t.Helper()
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		payload = b
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s → %d, unparseable body %q", method, path, w.Code, w.Body.String())
		}
	}
	return w.Code, out
}

// detail 取统一错误信封里的 detail 字符串。
func detail(t *testing.T, body map[string]any) string {
	t.Helper()
	s, ok := body["detail"].(string)
	if !ok {
		t.Fatalf("detail 不是字符串: %#v", body)
	}
	return s
}

// writeProjectScript 把剧本落盘并登记到 project.json.episodes。
func writeProjectScript(t *testing.T, f projectFixture, episode int, script map[string]any) {
	t.Helper()
	filename := "episode_1.json"
	if episode != 1 {
		filename = "episode_" + string(rune('0'+episode)) + ".json"
	}
	if err := f.store.WriteScriptJSON("p1", filename, script); err != nil {
		t.Fatalf("write script: %v", err)
	}
	relpath := "scripts/" + filename
	if err := f.store.Update("p1", func(pf *project.ProjectFile) error {
		pf.Episodes = append(pf.Episodes, project.EpisodeMeta{Episode: episode, ScriptFile: relpath})
		return nil
	}); err != nil {
		t.Fatalf("register episode: %v", err)
	}
}

// writeProjectFile 在项目目录下写一个文件（用于指纹/封面断言）。
func writeProjectFile(t *testing.T, f projectFixture, rel, content string) {
	t.Helper()
	dir, err := f.store.Dir("p1")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func TestListProjectsSummaryShape(t *testing.T) {
	f := newProjectFixture(t)

	writeProjectFile(t, f, "storyboards/E1S1.png", "png")
	writeProjectScript(t, f, 1, map[string]any{
		"content_mode": "narration",
		"episode":      1,
		"segments": []any{
			map[string]any{
				"segment_id":       "E1S1",
				"duration_seconds": 4,
				"characters_in_segment": []any{
					"乙", "甲",
				},
				"generated_assets": map[string]any{
					"storyboard_image": "storyboards/E1S1.png",
				},
			},
		},
	})

	status, body := f.call(t, http.MethodGet, "/api/v1/projects", nil, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if _, hasTotal := body["total"]; hasTotal {
		t.Errorf("响应不应包含 total 字段（Python list_projects 只回 projects）")
	}
	projects, ok := body["projects"].([]any)
	if !ok || len(projects) != 1 {
		t.Fatalf("projects = %#v", body["projects"])
	}
	item := projects[0].(map[string]any)

	for _, key := range []string{
		"name", "title", "style", "style_template_id", "style_image",
		"thumbnail", "created_at", "updated_at", "status", "media_summary",
	} {
		if _, present := item[key]; !present {
			t.Errorf("缺少字段 %q", key)
		}
	}
	if item["name"] != "p1" {
		t.Errorf("name = %v", item["name"])
	}
	if item["thumbnail"] != "/api/v1/files/p1/storyboards/E1S1.png" {
		t.Errorf("thumbnail = %v", item["thumbnail"])
	}

	statusObj, ok := item["status"].(map[string]any)
	if !ok {
		t.Fatalf("status 不是对象: %#v", item["status"])
	}
	// 全部集都已有剧本 → 进入 production（未全部完成）。
	if statusObj["current_phase"] != "production" {
		t.Errorf("current_phase = %v, want production", statusObj["current_phase"])
	}
	summary, ok := statusObj["episodes_summary"].(map[string]any)
	if !ok || summary["total"] != float64(1) || summary["scripted"] != float64(1) {
		t.Errorf("episodes_summary = %#v", statusObj["episodes_summary"])
	}

	media, ok := item["media_summary"].(map[string]any)
	if !ok {
		t.Fatalf("media_summary 不是对象: %#v", item["media_summary"])
	}
	if media["status"] != "stale" || media["asset_count"] != float64(0) {
		t.Errorf("media_summary = %#v", media)
	}
	if media["summary_version"] != float64(1) {
		t.Errorf("summary_version = %v", media["summary_version"])
	}
	if media["last_indexed_at"] != nil || media["error"] != nil {
		t.Errorf("空摘要的 last_indexed_at / error 应为 null: %#v", media)
	}
}

func TestListProjectsEmptyProjectFallback(t *testing.T) {
	f := newProjectFixture(t)

	// 只建目录、不写 project.json：应返回基本信息行而不是 500。
	dir, err := f.store.Dir("empty")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	status, body := f.call(t, http.MethodGet, "/api/v1/projects", nil, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	projects := body["projects"].([]any)
	found := false
	for _, raw := range projects {
		item := raw.(map[string]any)
		if item["name"] != "empty" {
			continue
		}
		found = true
		if item["title"] != "" || item["style"] != "" {
			t.Errorf("基本信息行 title/style 应为空串: %#v", item)
		}
		if item["thumbnail"] != nil {
			t.Errorf("thumbnail 应为 null: %#v", item["thumbnail"])
		}
		if statusObj, ok := item["status"].(map[string]any); !ok || len(statusObj) != 0 {
			t.Errorf("status 应为空对象: %#v", item["status"])
		}
	}
	if !found {
		t.Fatalf("未找到 empty 项目: %#v", projects)
	}
}

func TestGetProjectDetailShape(t *testing.T) {
	f := newProjectFixture(t)

	writeProjectFile(t, f, "storyboards/E1S1.png", "png")
	writeProjectScript(t, f, 1, map[string]any{
		"content_mode": "narration",
		"episode":      1,
		"segments": []any{
			map[string]any{
				"segment_id":            "E1S1",
				"duration_seconds":      4,
				"characters_in_segment": []any{"乙", "甲"},
				"scenes":                []any{"街"},
				"props":                 []any{"伞"},
				"generated_assets": map[string]any{
					"storyboard_image": "storyboards/E1S1.png",
				},
			},
			map[string]any{
				"segment_id":       "E1S2",
				"duration_seconds": 4,
			},
		},
	})

	status, body := f.call(t, http.MethodGet, "/api/v1/projects/p1", nil, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", status, body)
	}
	for _, key := range []string{"project", "scripts", "asset_fingerprints"} {
		if _, present := body[key]; !present {
			t.Errorf("缺少顶层字段 %q", key)
		}
	}

	project := body["project"].(map[string]any)
	statusObj, ok := project["status"].(map[string]any)
	if !ok {
		t.Fatalf("project.status 不是对象: %#v", project["status"])
	}
	if statusObj["current_phase"] != "production" {
		t.Errorf("current_phase = %v, want production", statusObj["current_phase"])
	}

	episodes := project["episodes"].([]any)
	ep := episodes[0].(map[string]any)
	for _, key := range []string{"script_status", "status", "storyboards", "videos", "scenes_count", "duration_seconds"} {
		if _, present := ep[key]; !present {
			t.Errorf("episode 缺少注入字段 %q", key)
		}
	}
	if ep["script_status"] != "generated" {
		t.Errorf("script_status = %v", ep["script_status"])
	}
	// 有分镜图但无视频 → in_production。
	if ep["status"] != "in_production" {
		t.Errorf("episode.status = %v, want in_production", ep["status"])
	}
	storyboards := ep["storyboards"].(map[string]any)
	if storyboards["total"] != float64(2) || storyboards["completed"] != float64(1) {
		t.Errorf("storyboards = %#v", storyboards)
	}

	// scripts 的键去掉 scripts/ 前缀，且注入计算字段。
	scripts := body["scripts"].(map[string]any)
	script, ok := scripts["episode_1.json"].(map[string]any)
	if !ok {
		t.Fatalf("scripts 缺少 episode_1.json: %#v", scripts)
	}
	if script["duration_seconds"] != float64(8) {
		t.Errorf("duration_seconds = %v, want 8", script["duration_seconds"])
	}
	metadata := script["metadata"].(map[string]any)
	if metadata["total_scenes"] != float64(2) || metadata["estimated_duration_seconds"] != float64(8) {
		t.Errorf("metadata = %#v", metadata)
	}
	chars := script["characters_in_episode"].([]any)
	if len(chars) != 2 || chars[0] != "乙" || chars[1] != "甲" {
		// 期望按 Unicode 码点排序：乙(U+4E59) < 甲(U+7532)。
		t.Errorf("characters_in_episode = %#v", chars)
	}

	fingerprints := body["asset_fingerprints"].(map[string]any)
	if _, ok := fingerprints["storyboards/E1S1.png"]; !ok {
		t.Errorf("asset_fingerprints 缺少 storyboards/E1S1.png: %#v", fingerprints)
	}
	// versions/ 目录不参与指纹。
	if _, ok := fingerprints["storyboards/versions/x.png"]; ok {
		t.Errorf("versions/ 不应进入指纹")
	}
}

func TestGetProjectDetailStaleLedgerFallback(t *testing.T) {
	f := newProjectFixture(t)

	writeProjectScript(t, f, 1, map[string]any{
		"content_mode": "narration",
		"episode":      1,
		"segments":     []any{map[string]any{"segment_id": "E1S1", "duration_seconds": 4}},
	})
	// 账本标 stale：读时回退为待预处理，禁用剧本计分。
	if err := f.store.Update("p1", func(pf *project.ProjectFile) error {
		pf.Episodes[0].LedgerStatus = project.LedgerStatusStale
		return nil
	}); err != nil {
		t.Fatalf("mark stale: %v", err)
	}

	status, body := f.call(t, http.MethodGet, "/api/v1/projects/p1", nil, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	ep := body["project"].(map[string]any)["episodes"].([]any)[0].(map[string]any)
	if ep["script_status"] != "none" {
		t.Errorf("stale 集 script_status = %v, want none", ep["script_status"])
	}
	if ep["status"] != "draft" {
		t.Errorf("stale 集 status = %v, want draft", ep["status"])
	}
}

func TestGetProjectDetailNotFound(t *testing.T) {
	f := newProjectFixture(t)
	status, body := f.call(t, http.MethodGet, "/api/v1/projects/nope", nil, "")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d", status)
	}
	if detail(t, body) == "" {
		t.Errorf("缺少 detail 文案")
	}
}

// 保证 JSON 编码里 scripts 非 null（无剧本时应为 {}）。
func TestGetProjectDetailEmptyScripts(t *testing.T) {
	f := newProjectFixture(t)
	status, body := f.call(t, http.MethodGet, "/api/v1/projects/p1", nil, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	raw, _ := json.Marshal(body["scripts"])
	if string(raw) != "{}" {
		t.Errorf("scripts = %s, want {}", raw)
	}
}

// ad 项目创建：target_duration/brief 落盘，episodes 恒为第 1 集单条；未传时取默认档位。
func TestCreateProjectAd(t *testing.T) {
	f := newProjectFixture(t)

	status, body := f.call(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"title": "广告项目", "content_mode": "ad", "generation_mode": "storyboard",
		"target_duration": 30, "brief": "卖点短片",
	}, "")
	if status != http.StatusCreated {
		t.Fatalf("status = %d, body = %#v", status, body)
	}
	proj := body["project"].(map[string]any)
	if proj["target_duration"] != float64(30) {
		t.Errorf("target_duration = %v, want 30", proj["target_duration"])
	}
	if proj["brief"] != "卖点短片" {
		t.Errorf("brief = %v", proj["brief"])
	}
	episodes := proj["episodes"].([]any)
	if len(episodes) != 1 {
		t.Fatalf("episodes = %#v, want 单集", episodes)
	}
	ep := episodes[0].(map[string]any)
	if ep["episode"] != float64(1) || ep["script_file"] != "scripts/episode_1.json" {
		t.Errorf("episode = %#v", ep)
	}

	status, body = f.call(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"title": "广告项目二", "content_mode": "ad", "generation_mode": "storyboard",
	}, "")
	if status != http.StatusCreated {
		t.Fatalf("status = %d, body = %#v", status, body)
	}
	proj = body["project"].(map[string]any)
	if proj["target_duration"] != float64(project.ADDefaultTargetDuration) {
		t.Errorf("默认 target_duration = %v, want %d", proj["target_duration"], project.ADDefaultTargetDuration)
	}
	if brief, present := proj["brief"]; present && brief != "" {
		t.Errorf("brief = %v, want 空串", brief)
	}
}

// 模式专属字段互斥：非 ad 带 target_duration/brief、ad 带 default_duration/宫格，均 400。
func TestCreateProjectModeFieldMutex(t *testing.T) {
	f := newProjectFixture(t)

	status, body := f.call(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"title": "普通项目", "content_mode": "drama", "generation_mode": "storyboard",
		"target_duration": 30,
	}, "")
	if status != http.StatusBadRequest {
		t.Fatalf("非 ad 带 target_duration: status = %d, body = %#v", status, body)
	}

	status, body = f.call(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"title": "普通项目", "content_mode": "drama", "generation_mode": "storyboard",
		"brief": "诉求",
	}, "")
	if status != http.StatusBadRequest {
		t.Fatalf("非 ad 带 brief: status = %d, body = %#v", status, body)
	}

	status, body = f.call(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"title": "广告项目", "content_mode": "ad", "generation_mode": "storyboard",
		"default_duration": 5,
	}, "")
	if status != http.StatusBadRequest {
		t.Fatalf("ad 带 default_duration: status = %d, body = %#v", status, body)
	}

	status, body = f.call(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"title": "广告项目", "content_mode": "ad", "generation_mode": "storyboard",
		"grid_storyboard": true,
	}, "")
	if status != http.StatusBadRequest {
		t.Fatalf("ad 带宫格: status = %d, body = %#v", status, body)
	}

	status, body = f.call(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"title": "广告项目", "content_mode": "ad", "generation_mode": "storyboard",
		"target_duration": 0,
	}, "")
	if status != http.StatusBadRequest {
		t.Fatalf("ad 带非正 target_duration: status = %d, body = %#v", status, body)
	}
}
