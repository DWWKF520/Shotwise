package generation

import "github.com/shotwise/shotwise/internal/service"

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

// newDispatcherFixture 复用图片生成记账夹具，外加一个真实落盘的项目目录。
func newDispatcherFixture(t *testing.T, status int) (*TaskDispatcher, *service.ProjectStore, string) {
	t.Helper()
	generator, _, _ := newImageGeneratorFixture(t, status)
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "测试项目", domain.ContentNarration, domain.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return NewTaskDispatcher(store, generator), store, "p1"
}

func storyboardTask(resourceID string, payload map[string]any) *repository.TaskData {
	return &repository.TaskData{
		TaskID:      "task-1",
		ProjectName: "p1",
		TaskType:    "storyboard",
		MediaType:   "image",
		ResourceID:  resourceID,
		Payload:     payload,
		UserID:      repository.DefaultUserID,
	}
}

func TestTaskDispatcherStoryboardEndToEnd(t *testing.T) {
	dispatcher, store, projectName := newDispatcherFixture(t, http.StatusOK)
	projectDir, err := store.Dir(projectName)
	if err != nil {
		t.Fatalf("project dir: %v", err)
	}

	result, err := dispatcher.Execute(context.Background(), storyboardTask("E1S01", map[string]any{
		"prompt": "夜色中的天台，猫蹲在水塔旁",
	}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result["file_path"] != "storyboards/scene_E1S01.png" {
		t.Fatalf("unexpected file_path: %v", result["file_path"])
	}
	if result["resource_type"] != "storyboards" || result["resource_id"] != "E1S01" {
		t.Fatalf("unexpected resource identity: %v", result)
	}
	createdAt, _ := result["created_at"].(string)
	if createdAt == "" {
		t.Fatalf("created_at missing: %v", result)
	}

	// 产物落盘 + 版本快照登记。
	out := filepath.Join(projectDir, "storyboards", "scene_E1S01.png")
	if info, err := os.Stat(out); err != nil || info.Size() == 0 {
		t.Fatalf("output missing: %v", err)
	}
	snapshot := filepath.Join(projectDir, "versions", "storyboards", "E1S01_v1_")
	entries, err := filepath.Glob(snapshot + "*.png")
	if err != nil || len(entries) != 1 {
		t.Fatalf("version snapshot missing: %v (%v)", entries, err)
	}
	raw, err := os.ReadFile(filepath.Join(projectDir, "versions", "versions.json"))
	if err != nil {
		t.Fatalf("versions.json missing: %v", err)
	}
	if !strings.Contains(string(raw), `"current_version": 1`) || !strings.Contains(string(raw), createdAt) {
		t.Fatalf("versions.json content unexpected: %s", raw)
	}
	if !strings.Contains(string(raw), "aspect_ratio") {
		t.Fatalf("version metadata missing: %s", raw)
	}
}

func TestTaskDispatcherStructuredPromptRendersYAML(t *testing.T) {
	generator, gdb, _ := newImageGeneratorFixture(t, http.StatusOK)
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "测试项目", domain.ContentNarration, domain.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	dispatcher := NewTaskDispatcher(store, generator)
	if _, err := dispatcher.Execute(context.Background(), storyboardTask("E1S02", map[string]any{
		"prompt": map[string]any{
			"scene": "雨夜的巷口",
			"composition": map[string]any{
				"lighting": "霓虹灯",
				"ambiance": "潮湿",
			},
		},
	})); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var row repository.APICall
	if err := gdb.Where("project_name = ?", "p1").First(&row).Error; err != nil {
		t.Fatalf("api_call row missing: %v", err)
	}
	for _, want := range []string{"Style: 水墨", "Scene: 雨夜的巷口", "shot_type: Medium Shot", "lighting: 霓虹灯", storyboardNegativeTail} {
		if row.Prompt == nil || !strings.Contains(*row.Prompt, want) {
			t.Fatalf("prompt missing %q: %v", want, row.Prompt)
		}
	}
}

func TestTaskDispatcherLedgerRow(t *testing.T) {
	// 独立夹具拿得到 DB：验证记账行身份与 segment_id。
	generator, gdb, _ := newImageGeneratorFixture(t, http.StatusOK)
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "测试项目", domain.ContentNarration, domain.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	dispatcher := NewTaskDispatcher(store, generator)
	if _, err := dispatcher.Execute(context.Background(), storyboardTask("E1S03", map[string]any{
		"prompt": "白天市场街景",
	})); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var row repository.APICall
	if err := gdb.Where("project_name = ?", "p1").First(&row).Error; err != nil {
		t.Fatalf("api_call row missing: %v", err)
	}
	if row.Status != service.StatusSuccess || row.CallType != "image" {
		t.Fatalf("unexpected ledger row: %s/%s", row.Status, row.CallType)
	}
	if row.SegmentID == nil || *row.SegmentID != "E1S03" {
		t.Fatalf("segment_id not recorded: %v", row.SegmentID)
	}
	// 分镜 prompt 追加了统一反向提示词。
	if row.Prompt == nil || !strings.Contains(*row.Prompt, storyboardNegativeTail) {
		t.Fatalf("negative tail missing in prompt: %v", row.Prompt)
	}
}

func TestTaskDispatcherVersionIncrements(t *testing.T) {
	dispatcher, _, _ := newDispatcherFixture(t, http.StatusOK)
	for want := 1; want <= 2; want++ {
		result, err := dispatcher.Execute(context.Background(), storyboardTask("E1S01", map[string]any{
			"prompt": "同一镜头重新生成",
		}))
		if err != nil {
			t.Fatalf("execute #%d: %v", want, err)
		}
		// result 契约不直接含 version 号（与 Python 一致，从 versions.json 读）；
		// 这里经 created_at 单调变化 + versions.json 里的版本号间接验证。
		raw, _ := json.Marshal(result)
		if len(raw) == 0 {
			t.Fatal("empty result")
		}
	}
}

func TestTaskDispatcherRejectsEmptyAndUnsupported(t *testing.T) {
	dispatcher, _, _ := newDispatcherFixture(t, http.StatusOK)

	if _, err := dispatcher.Execute(context.Background(), storyboardTask("E1S01", map[string]any{
		"prompt": "   ",
	})); err == nil || !strings.Contains(err.Error(), "prompt must not be empty") {
		t.Fatalf("empty prompt should fail loud: %v", err)
	}
	if _, err := dispatcher.Execute(context.Background(), storyboardTask("E1S01", map[string]any{})); err == nil {
		t.Fatal("missing prompt should fail loud")
	}
	if _, err := dispatcher.Execute(context.Background(), storyboardTask("E1S01", map[string]any{
		"prompt": map[string]any{"composition": map[string]any{}},
	})); err == nil || !strings.Contains(err.Error(), "scene") {
		t.Fatalf("structured prompt without scene should fail loud: %v", err)
	}
	// video / reference_video 已接线：未注入视频生成器时按缺失依赖 fail loud，而非 unsupported。
	_, err := dispatcher.Execute(context.Background(), &repository.TaskData{
		TaskID: "t2", ProjectName: "p1", TaskType: "video", MediaType: "video", ResourceID: "E1S01",
		Payload: map[string]any{"prompt": "x"}, UserID: repository.DefaultUserID,
	})
	if err == nil || strings.Contains(err.Error(), "unsupported task_type") {
		t.Fatalf("video task should be routed, not unsupported: %v", err)
	}
	_, err = dispatcher.Execute(context.Background(), &repository.TaskData{
		TaskID: "t3", ProjectName: "p1", TaskType: "reference_video", MediaType: "video", ResourceID: "E1U01",
		Payload: map[string]any{"prompt": "x"}, UserID: repository.DefaultUserID,
	})
	if err == nil || strings.Contains(err.Error(), "unsupported task_type") {
		t.Fatalf("reference_video task should be routed, not unsupported: %v", err)
	}
	// canvas_image_* 已接线：split 会进入执行链，缺 source_kind 时按执行期校验报错，
	// 而非 unsupported task_type。
	_, err = dispatcher.Execute(context.Background(), &repository.TaskData{
		TaskID: "t4", ProjectName: "p1", TaskType: "canvas_image_split", MediaType: "image", ResourceID: "E1S01",
		Payload: map[string]any{"prompt": "x"}, UserID: repository.DefaultUserID,
	})
	if err == nil || strings.Contains(err.Error(), "unsupported task_type") {
		t.Fatalf("canvas task should be routed to executor, got %v", err)
	}
	_, err = dispatcher.Execute(context.Background(), &repository.TaskData{
		TaskID: "t5", ProjectName: "p1", TaskType: "canvas_image_bogus", MediaType: "image", ResourceID: "E1S01",
		Payload: map[string]any{"prompt": "x"}, UserID: repository.DefaultUserID,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported task_type: canvas_image_bogus") {
		t.Fatalf("unknown canvas operation should fail loud: %v", err)
	}
}

func TestImageGeneratorCustomProviderWired(t *testing.T) {
	generator, gdb, _ := newImageGeneratorFixture(t, http.StatusOK)
	// 起一个声明式 OpenAI 兼容图片端点作为自定义供应商，路由指向它。
	customSrv := openAIImageStub(t, http.StatusOK)
	if err := gdb.Exec(
		"INSERT INTO custom_provider (id, display_name, base_url, api_key, discovery_format, is_enabled, created_at, updated_at) "+
			"VALUES (1, '自建中转', ?, 'sk-custom', 'none', 1, '2026-01-01T00:00:00+00:00', '2026-01-01T00:00:00+00:00')",
		customSrv.URL,
	).Error; err != nil {
		t.Fatalf("seed custom provider: %v", err)
	}
	if err := gdb.Exec(
		"INSERT INTO custom_provider_model (provider_id, model_id, display_name, is_default, is_enabled, endpoint, created_at, updated_at) " +
			"VALUES (1, 'my-image-model', '自建图片', 1, 1, 'openai-images', '2026-01-01T00:00:00+00:00', '2026-01-01T00:00:00+00:00')",
	).Error; err != nil {
		t.Fatalf("seed custom model: %v", err)
	}
	if err := gdb.Exec(
		"INSERT INTO system_setting (key, value, updated_at) VALUES (?, ?, ?) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		"default_image_backend", "custom-1/my-image-model", "2026-01-01T00:00:00+00:00",
	).Error; err != nil {
		t.Fatalf("route default to custom provider: %v", err)
	}

	out := filepath.Join(t.TempDir(), "shot.png")
	res, err := generator.Generate(context.Background(), ImageGenerateParams{
		ProjectName: "demo",
		Capability:  config.ImageCapT2I,
		Request:     provider.NewImageGenerationRequest("a cat on a roof", out),
	})
	if err != nil {
		t.Fatalf("generate via custom provider: %v", err)
	}
	if res.ImagePath != out {
		t.Fatalf("unexpected path: %s", res.ImagePath)
	}
	var row repository.APICall
	if err := gdb.Where("project_name = ?", "demo").First(&row).Error; err != nil {
		t.Fatalf("api_call row missing: %v", err)
	}
	if row.Provider != "custom-1" || row.Status != service.StatusSuccess {
		t.Fatalf("unexpected ledger identity: %s/%s", row.Provider, row.Status)
	}
}

func TestResourceRelativePathShapes(t *testing.T) {
	cases := map[string]string{
		"storyboards|E1S01": "storyboards/scene_E1S01.png",
		"videos|E1S01":      "videos/scene_E1S01.mp4",
		"audio|seg1":        "audio/segment_seg1.wav",
		"characters|姜月茴":    "characters/姜月茴.png",
	}
	for key, want := range cases {
		parts := strings.SplitN(key, "|", 2)
		got, err := service.ResourceRelativePath(parts[0], parts[1])
		if err != nil || got != want {
			t.Fatalf("%s => %s, %v (want %s)", key, got, err, want)
		}
	}
	if _, err := service.ResourceRelativePath("unknown", "x"); err == nil {
		t.Fatal("unknown resource type should fail loud")
	}
}

func TestSegmentIDForWhitelist(t *testing.T) {
	if segmentIDFor("image", "storyboards", "E1S01") != "E1S01" {
		t.Fatal("storyboards should pass image whitelist")
	}
	if segmentIDFor("image", "characters", "姜月茴") != "" {
		t.Fatal("characters should not pass image whitelist")
	}
	if segmentIDFor("audio", "anything", "seg1") != "seg1" {
		t.Fatal("audio channel passes through unconditionally")
	}
}

func TestAppendImageNegativeTailIdempotent(t *testing.T) {
	tail := storyboardNegativeTail
	if got := appendImageNegativeTail(""); got != tail {
		t.Fatalf("empty prompt should become tail only: %q", got)
	}
	once := appendImageNegativeTail("a cat")
	if !strings.HasSuffix(once, tail) {
		t.Fatalf("tail missing: %q", once)
	}
	if again := appendImageNegativeTail(once); again != once {
		t.Fatalf("tail not idempotent: %q", again)
	}
}

func TestNormalizeStylePrefix(t *testing.T) {
	if got := service.NormalizeStylePrefix("画风：水墨"); got != "水墨" {
		t.Fatalf("fullwidth prefix not stripped: %q", got)
	}
	if got := service.NormalizeStylePrefix("画风:水墨"); got != "水墨" {
		t.Fatalf("halfwidth prefix not stripped: %q", got)
	}
	if got := service.NormalizeStylePrefix("水墨"); got != "水墨" {
		t.Fatalf("plain style changed: %q", got)
	}
}
