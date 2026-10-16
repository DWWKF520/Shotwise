package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"

	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/migrations"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 时间线内部工具（get_episode_media_manifest / create_timeline_plan / get_timeline_plan /
// update_timeline_plan / validate_timeline_plan）的端到端测试：真实 sqlite + 真实项目目录。

func newTimelineTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "timeline.db"), logger.Silent)
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
	return gdb
}

func writeTimelineFixture(t *testing.T, dir string) {
	t.Helper()
	writeReviewJSON(t, filepath.Join(dir, "project.json"), map[string]any{
		"content_mode":    "drama",
		"generation_mode": "storyboard",
		"current_episode": float64(1),
		"episodes": []any{
			map[string]any{"episode": float64(1), "script_file": "scripts/episode_1.json"},
		},
	})
	writeReviewJSON(t, filepath.Join(dir, "scripts", "episode_1.json"), map[string]any{
		"episode":      1,
		"content_mode": "drama",
		"scenes": []any{
			map[string]any{
				"scene_id":         "E1S01",
				"duration_seconds": 5,
				"video_prompt":     "夜色中的天台",
				"generated_assets": map[string]any{"video_clip": "videos/E1S01.mp4", "status": "completed"},
			},
		},
	})
	writeReviewVideo(t, dir, "videos/E1S01.mp4")
}

// timelineToolsetForTest 构造带真实计划服务的工具集。
func timelineToolsetForTest(t *testing.T) (*Runtime, map[string]tool.InvokableTool) {
	t.Helper()
	base := t.TempDir()
	projectDir := filepath.Join(base, "demo")
	writeTimelineFixture(t, projectDir)

	svc := service.NewMediaAssemblyService(newTimelineTestDB(t))
	r, err := NewRuntime(context.Background(), ModelConfig{}, base, nil, nil, WithTimelineTools(svc))
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	tools, err := r.timelineTools("demo")
	if err != nil {
		t.Fatalf("timeline tools: %v", err)
	}
	if len(tools) != 5 {
		t.Fatalf("want 5 timeline tools, got %d", len(tools))
	}
	byName := map[string]tool.InvokableTool{}
	for _, bt := range tools {
		inv, ok := bt.(tool.InvokableTool)
		if !ok {
			t.Fatalf("tool is not invokable: %T", bt)
		}
		info, err := inv.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		byName[info.Name] = inv
	}
	return r, byName
}

func invokeTimelineTool(t *testing.T, byName map[string]tool.InvokableTool, name, args string) (string, map[string]any, bool) {
	t.Helper()
	inv, ok := byName[name]
	if !ok {
		t.Fatalf("tool %s not registered", name)
	}
	raw, err := inv.InvokableRun(context.Background(), args)
	if err != nil {
		t.Fatalf("invoke %s: %v", name, err)
	}
	return decodeTimelineEnvelope(t, raw)
}

func decodeTimelineEnvelope(t *testing.T, raw string) (string, map[string]any, bool) {
	t.Helper()
	var envelope struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"is_error"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("envelope is not JSON: %v\n%s", err, raw)
	}
	if len(envelope.Content) == 0 {
		t.Fatalf("envelope has no content: %s", raw)
	}
	text := envelope.Content[0].Text
	var payload map[string]any
	if idx := strings.Index(text, "\n"); idx >= 0 {
		_ = json.Unmarshal([]byte(text[idx+1:]), &payload)
	} else {
		_ = json.Unmarshal([]byte(text), &payload)
	}
	return text, payload, envelope.IsError
}

func TestTimelineToolsLifecycle(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // 强制 ffprobe 不可用，媒体探测结果确定
	_, byName := timelineToolsetForTest(t)

	// 1. 媒体清单（只读）
	text, manifest, isError := invokeTimelineTool(t, byName, timelineGetManifestName, `{"script":"episode_1.json"}`)
	if isError {
		t.Fatalf("manifest failed: %s", text)
	}
	items, _ := manifest["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("manifest items = %v", manifest["items"])
	}
	item, _ := items[0].(map[string]any)
	if item["unit_id"] != "E1S01" {
		t.Fatalf("unit_id = %v", item["unit_id"])
	}
	probe, _ := item["media_probe"].(map[string]any)
	if probe["probe_available"] != false {
		t.Fatalf("probe_available = %v", probe)
	}
	if fingerprint, _ := manifest["fingerprint"].(string); len(fingerprint) != 64 {
		t.Fatalf("fingerprint = %v", manifest["fingerprint"])
	}

	// 2. 创建计划
	text, created, isError := invokeTimelineTool(t, byName, timelineCreatePlanName, `{"script":"episode_1.json"}`)
	if isError {
		t.Fatalf("create failed: %s", text)
	}
	planID, _ := created["plan_id"].(string)
	if planID == "" || created["revision"] != float64(1) || created["status"] != "draft" {
		t.Fatalf("created plan = %v", created)
	}
	if created["persistence"] != "database" {
		t.Fatalf("persistence = %v", created["persistence"])
	}
	timelineItems, _ := created["timeline_items"].([]any)
	if len(timelineItems) != 1 {
		t.Fatalf("timeline_items = %v", created["timeline_items"])
	}

	// 3. 读取计划
	_, fetched, isError := invokeTimelineTool(t, byName, timelineGetPlanName, `{"plan_id":"`+planID+`"}`)
	if isError || fetched["plan_id"] != planID {
		t.Fatalf("get plan: err=%v payload=%v", isError, fetched)
	}

	// 4. 更新计划（允许字段）
	text, updated, isError := invokeTimelineTool(t, byName, timelineUpdatePlanName,
		`{"plan_id":"`+planID+`","patch":{"director_review":{"status":"approved"}}}`)
	if isError {
		t.Fatalf("update failed: %s", text)
	}
	if updated["revision"] != float64(2) {
		t.Fatalf("updated revision = %v", updated["revision"])
	}
	review, _ := updated["director_review"].(map[string]any)
	if review["status"] != "approved" {
		t.Fatalf("director_review = %v", updated["director_review"])
	}

	// 5. 校验计划：ffprobe 缺失导致媒体可用性阻塞，其余检查通过
	text, validated, isError := invokeTimelineTool(t, byName, timelineValidatePlanName, `{"plan_id":"`+planID+`"}`)
	if isError {
		t.Fatalf("validate failed: %s", text)
	}
	validation, _ := validated["validation"].(map[string]any)
	if validation["valid"] != false {
		t.Fatalf("validation should be blocked without ffprobe: %v", validation)
	}
	issues, _ := validation["issues"].([]any)
	if len(issues) != 1 {
		t.Fatalf("issues = %v", issues)
	}
	issue, _ := issues[0].(map[string]any)
	if issue["code"] != "media_missing_or_unreadable" || issue["severity"] != "blocker" {
		t.Fatalf("issue = %v", issue)
	}
	if validated["status"] != "draft" {
		t.Fatalf("status = %v", validated["status"])
	}
	if validated["revision"] != float64(3) {
		t.Fatalf("revision = %v", validated["revision"])
	}
}

func TestTimelineToolsRejectsInvalidInput(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, byName := timelineToolsetForTest(t)

	// plan_id 空
	text, _, isError := invokeTimelineTool(t, byName, timelineGetPlanName, `{"plan_id":"  "}`)
	if !isError || !strings.Contains(text, "plan_id 必须是非空字符串") {
		t.Fatalf("empty plan_id not rejected: %s", text)
	}

	// patch 空
	text, _, isError = invokeTimelineTool(t, byName, timelineUpdatePlanName, `{"plan_id":"x","patch":{}}`)
	if !isError || !strings.Contains(text, "patch 必须是非空对象") {
		t.Fatalf("empty patch not rejected: %s", text)
	}

	// 未知 patch 字段：需先有一个真实计划（get_plan 先于 _apply_patch）
	text, created, isError := invokeTimelineTool(t, byName, timelineCreatePlanName, `{"script":"episode_1.json"}`)
	if isError {
		t.Fatalf("create failed: %s", text)
	}
	planID, _ := created["plan_id"].(string)
	text, _, isError = invokeTimelineTool(t, byName, timelineUpdatePlanName,
		`{"plan_id":"`+planID+`","patch":{"bogus":{"a":1}}}`)
	if !isError || !strings.Contains(text, "不允许更新的 plan 字段") {
		t.Fatalf("unknown patch field not rejected: %s", text)
	}

	// 领域校验失败：trim 吃掉整段
	text, _, isError = invokeTimelineTool(t, byName, timelineCreatePlanName,
		`{"script":"episode_1.json","timeline_items":[{"unit_id":"E1S01","duration_seconds":5,"trim":{"start_seconds":5,"end_seconds":null}}]}`)
	if !isError || !strings.Contains(text, "empty_after_trim") {
		t.Fatalf("trim-overrun must surface validation log: %s", text)
	}
}

func TestTimelineToolsUnconfiguredAreNotRegistered(t *testing.T) {
	base := t.TempDir()
	writeTimelineFixture(t, filepath.Join(base, "demo"))
	r, err := NewRuntime(context.Background(), ModelConfig{}, base, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := r.timelineTools("demo")
	if err != nil {
		t.Fatalf("unconfigured runtime must not error: %v", err)
	}
	if tools != nil {
		t.Fatalf("want no timeline tools, got %d", len(tools))
	}
}
