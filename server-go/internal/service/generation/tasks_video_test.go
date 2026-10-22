package generation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/pricing"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/reference"
	"github.com/shotwise/shotwise/migrations"
)

// stubVideoBackend 是注册进 openai 键的假视频后端：落一个最小文件即成功，不做真实提交。
type stubVideoBackend struct {
	model string
	caps  provider.VideoCapabilities
}

func (b *stubVideoBackend) Name() string                                  { return "openai" }
func (b *stubVideoBackend) Model() string                                 { return b.model }
func (b *stubVideoBackend) VideoCapabilities() provider.VideoCapabilities { return b.caps }

func (b *stubVideoBackend) Generate(_ context.Context, req provider.VideoGenerationRequest) (provider.VideoGenerationResult, error) {
	if err := os.WriteFile(req.OutputPath, []byte("mp4"), 0o644); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	return provider.VideoGenerationResult{
		VideoPath: req.OutputPath, Provider: "openai", Model: b.model, DurationSeconds: req.DurationSeconds,
	}, nil
}

func (b *stubVideoBackend) ResumeVideo(_ context.Context, _ string, _ provider.VideoGenerationRequest) (provider.VideoGenerationResult, error) {
	return provider.VideoGenerationResult{}, provider.ErrResumeUnsupported
}

func stubVideoCaps() provider.VideoCapabilities {
	caps := provider.DefaultVideoCapabilities()
	caps.FirstFrame = true
	caps.MaxReferenceImages = 2
	return caps
}

// newVideoFixture 装配视频执行所需依赖：假 backend + 视频生成器 + 落盘项目。
func newVideoFixture(t *testing.T, contentMode domain.ContentMode, genMode domain.GenerationMode) (*TaskDispatcher, *service.ProjectStore, *gorm.DB, string) {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "video.db"), logger.Silent)
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
	if err := gdb.Exec(
		"INSERT INTO users (id, username, role, is_active, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		repository.DefaultUserID, "default", "user", true, "2026-01-01T00:00:00+00:00", "2026-01-01T00:00:00+00:00",
	).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	for key, secret := range map[string]bool{"api_key": true, "base_url": false} {
		value := "https://example.invalid"
		if key == "api_key" {
			value = "sk-stub"
		}
		if err := gdb.Exec(
			"INSERT INTO provider_config (provider, key, value, is_secret, updated_at) VALUES (?, ?, ?, ?, ?)",
			"openai", key, value, secret, "2026-01-01T00:00:00+00:00",
		).Error; err != nil {
			t.Fatalf("seed provider config: %v", err)
		}
	}

	caps := stubVideoCaps()
	provider.RegisterVideoBackend("openai", provider.VideoBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.VideoBackend, error) {
			return &stubVideoBackend{model: opts.Model, caps: caps}, nil
		},
		CapabilitiesForModel: func(string) (provider.VideoCapabilities, error) { return caps, nil },
	})
	t.Cleanup(func() { provider.RegisterVideoBackend("openai", provider.VideoBackendRegistration{}) })

	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "测试项目", contentMode, genMode, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := store.Update("p1", func(p *domain.ProjectFile) error {
		p.VideoBackend = "openai/sora-2"
		return nil
	}); err != nil {
		t.Fatalf("set video backend: %v", err)
	}

	catalog := pricing.MapCatalog{
		"openai": {
			Models: map[string]pricing.ModelInfo{
				"sora-2": {
					MediaType: "video",
					Pricing: ptrPricing(pricing.NewPerToken(
						map[string]map[string]float64{"sora-2": {"input": 0, "output": 0}}, "sora-2", "USD",
					)),
				},
			},
			DefaultModel: map[string]string{"video": "sora-2"},
		},
	}
	ledger := service.NewLedger(gdb, pricing.NewCalculator(catalog))
	dispatcher := NewTaskDispatcher(store, nil)
	dispatcher.SetVideoGenerator(NewVideoGenerator(gdb, ledger))
	return dispatcher, store, gdb, "p1"
}

func videoTask(resourceID string, payload map[string]any) *repository.TaskData {
	return &repository.TaskData{
		TaskID:      "",
		ProjectName: "p1",
		TaskType:    "video",
		MediaType:   "video",
		ResourceID:  resourceID,
		Payload:     payload,
		UserID:      repository.DefaultUserID,
	}
}

func writeSegmentScript(t *testing.T, store *service.ProjectStore, projectDir, storyboardRel string) {
	t.Helper()
	script := map[string]any{
		"episode":      1,
		"content_mode": "narration",
		"title":        "第一集",
		"segments": []any{
			map[string]any{
				"segment_id": "E1S01",
				"novel_text": "夜色中的天台。",
				"generated_assets": map[string]any{
					"storyboard_image": storyboardRel,
				},
			},
		},
	}
	if err := store.WriteScriptJSON("p1", "episode_1.json", script); err != nil {
		t.Fatalf("write script: %v", err)
	}
}

func TestExecuteVideoTaskEndToEnd(t *testing.T) {
	dispatcher, store, gdb, projectName := newVideoFixture(t, domain.ContentNarration, domain.GenerationStoryboard)
	projectDir, err := store.Dir(projectName)
	if err != nil {
		t.Fatalf("project dir: %v", err)
	}
	storyboard := filepath.Join(projectDir, "storyboards", "scene_E1S01.png")
	if err := os.MkdirAll(filepath.Dir(storyboard), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(storyboard, []byte("png"), 0o644); err != nil {
		t.Fatalf("write storyboard: %v", err)
	}
	writeSegmentScript(t, store, projectDir, "storyboards/scene_E1S01.png")

	result, err := dispatcher.Execute(context.Background(), videoTask("E1S01", map[string]any{
		"script_file": "episode_1.json",
		"prompt":      "夜色中的天台，猫蹲在水塔旁",
	}))
	if err != nil {
		t.Fatalf("execute video task: %v", err)
	}
	if result["file_path"] != "videos/scene_E1S01.mp4" || result["resource_type"] != "videos" {
		t.Fatalf("unexpected result: %v", result)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "videos", "scene_E1S01.mp4")); err != nil {
		t.Fatalf("video output missing: %v", err)
	}

	// 剧本 generated_assets 已回写 video_clip（经剧本写盘统一入口）。
	script, err := store.ReadScriptJSON(projectName, "episode_1.json")
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	segments := script["segments"].([]any)
	assets := segments[0].(map[string]any)["generated_assets"].(map[string]any)
	if assets["video_clip"] != "videos/scene_E1S01.mp4" {
		t.Fatalf("video_clip not written back: %v", assets)
	}

	var row repository.APICall
	if err := gdb.Where("call_type = ?", "video").First(&row).Error; err != nil {
		t.Fatalf("ledger row missing: %v", err)
	}
	if row.Status != service.StatusSuccess {
		t.Fatalf("expected success ledger row, got %s", row.Status)
	}
}

func TestRenderUnitPromptSegments(t *testing.T) {
	character := domain.NewAssetEntry()
	character.SetString("description", "主角")
	character.SetString("character_sheet", "characters/李明.png")
	character.SetString("voice_style", "低沉")
	project := &domain.ProjectFile{
		Characters: domain.AssetBucket{"李明": character},
		Style:      "水墨",
	}
	text := "镜头1：@[李明] 缓步走向窗前。\n@[李明]：{这地方比我想的还热闹。}"
	rendered := reference.RenderUnitPrompt(text, project, []map[string]any{{"type": "character", "name": "李明"}}, reference.VoiceRenderSettings{
		VoiceConsistency:       "native",
		RequestedGenerateAudio: true,
		MaxReferenceAudio:      1,
		ModelID:                "sora-2",
		AudioReady:             map[string]bool{"李明": true},
	}, "水墨")

	for _, want := range []string{
		"<李明>@图片1。",
		"<李明>的台词音色参考 @音频1，声音特征：低沉。",
		"镜头1：\n<李明> 缓步走向窗前。",
		"<李明>说 {这地方比我想的还热闹。}",
		"整体视觉风格：水墨。",
		reference.QualityPack + reference.StabilityPack,
	} {
		if !strings.Contains(rendered.Prompt, want) {
			t.Fatalf("rendered prompt missing %q:\n%s", want, rendered.Prompt)
		}
	}
	if len(rendered.AudioSpeakers) != 1 || rendered.AudioSpeakers[0] != "李明" {
		t.Fatalf("audio speakers = %v", rendered.AudioSpeakers)
	}
	if len(rendered.AudioSpeakerReferenceIndex) != 1 || rendered.AudioSpeakerReferenceIndex[0] == nil ||
		*rendered.AudioSpeakerReferenceIndex[0] != 0 {
		t.Fatalf("audio speaker reference index = %v", rendered.AudioSpeakerReferenceIndex)
	}
}

func TestResolveAdUnitReferenceEntriesLabelsAndSkips(t *testing.T) {
	projectDir := t.TempDir()
	sheet := filepath.Join(projectDir, "scenes", "酒馆.png")
	if err := os.MkdirAll(filepath.Dir(sheet), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(sheet, []byte("png"), 0o644); err != nil {
		t.Fatalf("write sheet: %v", err)
	}
	scene := domain.NewAssetEntry()
	scene.SetString("description", "酒馆")
	scene.SetString("scene_sheet", "scenes/酒馆.png")
	project := &domain.ProjectFile{Scenes: domain.AssetBucket{"酒馆": scene}}

	entries, warnings := reference.ResolveAdUnitReferenceEntries(project, projectDir, []map[string]any{
		{"type": "scene", "name": "酒馆"},
		{"type": "scene", "name": "缺失场景"},
	})
	if len(entries) != 1 {
		t.Fatalf("expected one asset entry, got %v", entries)
	}
	if entries[0]["label"] != "场景「酒馆」设计图" || entries[0]["kind"] != "asset" || entries[0]["asset_type"] != "scene" {
		t.Fatalf("unexpected entry: %v", entries[0])
	}
	if len(warnings) != 1 || warnings[0]["key"] != "ref_ad_reference_skipped" {
		t.Fatalf("missing-asset skip warning expected: %v", warnings)
	}
}

func TestAppendProductFidelityTailIdempotent(t *testing.T) {
	once := reference.AppendProductFidelityTail("画面内容", []string{"可乐"})
	if !strings.Contains(once, "产品「可乐」") {
		t.Fatalf("fidelity tail missing: %q", once)
	}
	if again := reference.AppendProductFidelityTail(once, []string{"可乐"}); again != once {
		t.Fatalf("fidelity tail not idempotent: %q", again)
	}
	if got := reference.AppendProductFidelityTail("画面内容", nil); got != "画面内容" {
		t.Fatalf("empty product names should return prompt unchanged: %q", got)
	}
}

func TestExecuteVideoTaskMissingScriptFileFailsLoud(t *testing.T) {
	dispatcher, _, _, _ := newVideoFixture(t, domain.ContentNarration, domain.GenerationStoryboard)
	_, err := dispatcher.Execute(context.Background(), videoTask("E1S01", map[string]any{"prompt": "x"}))
	if err == nil || !strings.Contains(err.Error(), "script_file is required") {
		t.Fatalf("missing script_file should fail loud: %v", err)
	}
}

func TestNormalizeVideoPrompt(t *testing.T) {
	text, err := normalizeVideoPrompt("  一个镜头  ")
	if err != nil || !strings.HasSuffix(text, videoNegativeTail) {
		t.Fatalf("string prompt should get negative tail: %q, %v", text, err)
	}
	if _, err := normalizeVideoPrompt("   "); err == nil {
		t.Fatal("blank string prompt should fail loud")
	}
	if _, err := normalizeVideoPrompt(map[string]any{"dialogue": []any{}}); err == nil {
		t.Fatal("structured prompt without action should fail loud")
	}
	if _, err := normalizeVideoPrompt(map[string]any{"action": "走", "dialogue": "x"}); err == nil {
		t.Fatal("non-array dialogue should fail loud")
	}
	text, err = normalizeVideoPrompt(map[string]any{
		"action":         "推开木门",
		"dialogue":       []any{map[string]any{"speaker": " 李明 ", "line": " 我来了 "}, map[string]any{"speaker": "", "line": ""}},
		"voice_profiles": []any{map[string]any{"Speaker": "李明", "Voice_Style": "低沉"}},
	})
	if err != nil {
		t.Fatalf("structured normalize: %v", err)
	}
	for _, want := range []string{"Camera_Motion: Static", "Action: 推开木门", "Speaker: 李明", "Line: 我来了"} {
		if !strings.Contains(text, want) {
			t.Fatalf("normalized prompt missing %q: %s", want, text)
		}
	}
}

func TestResolveEndFrameImage(t *testing.T) {
	projectDir := t.TempDir()
	endFrames := filepath.Join(projectDir, "end_frames")
	if err := os.MkdirAll(endFrames, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	canonical := filepath.Join(endFrames, "scene_E1S01.png")
	if err := os.WriteFile(canonical, []byte("png"), 0o644); err != nil {
		t.Fatalf("write canonical: %v", err)
	}
	if err := os.WriteFile(filepath.Join(endFrames, "scene_E1S02.png"), []byte("png"), 0o644); err != nil {
		t.Fatalf("write other: %v", err)
	}

	cases := []struct {
		name    string
		value   any
		wantErr bool
	}{
		{"未设置为 nil", nil, false},
		{"未设置为空串", "", false},
		{"canonical 相对路径", "end_frames/scene_E1S01.png", false},
		{"裸文件名补前缀", "scene_E1S01.png", false},
		{"指向别的镜头", "end_frames/scene_E1S02.png", true},
		{"越界路径", "../../project.json", true},
		{"非字符串脏数据", 0, true},
		{"文件缺失", "end_frames/scene_E1S09.png", true},
	}
	for _, c := range cases {
		got, err := resolveEndFrameImage(projectDir, map[string]any{"end_frame_image": c.value}, "E1S01")
		if c.wantErr {
			if err == nil {
				t.Fatalf("%s: expected error, got %q", c.name, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error %v", c.name, err)
		}
	}
}

func TestRenderMentionsAsSubjects(t *testing.T) {
	got := reference.RenderMentionsAsSubjects("镜头1：@[李明] 推开 @[酒馆] 的门，未登记 @[幽灵] 原样保留。", []string{"李明", "酒馆"})
	want := "镜头1：<李明> 推开 <酒馆> 的门，未登记 @[幽灵] 原样保留。"
	if got != want {
		t.Fatalf("renderMentionsAsSubjects = %q, want %q", got, want)
	}
}

func TestResolveUnitReferencesMissingFails(t *testing.T) {
	projectDir := t.TempDir()
	project := &domain.ProjectFile{Characters: domain.AssetBucket{}}
	_, err := reference.ResolveUnitReferences(project, projectDir, []map[string]any{{"type": "character", "name": "李明"}})
	if err == nil || !strings.Contains(err.Error(), "Missing references: character:李明") {
		t.Fatalf("missing reference should fail loud: %v", err)
	}
}

func TestApplyUnitVideoAssetsWritesAndClears(t *testing.T) {
	script := map[string]any{
		"video_units": []any{map[string]any{
			"unit_id": "E1U01",
			"stale":   true,
			"generated_assets": map[string]any{
				"video_uri":       "old-uri",
				"video_thumbnail": "old-thumb.jpg",
			},
		}},
	}
	signature, err := applyUnitVideoAssets(script, "E1U01", "", "", "2026-01-01T00:00:00Z", service.SignatureCompute, "")
	if err != nil {
		t.Fatalf("applyUnitVideoAssets: %v", err)
	}
	// narration/drama 的 video_units 条目不承载签名，返回 nil。
	if signature != nil {
		t.Fatalf("video_units should not carry a signature: %v", *signature)
	}
	unit := script["video_units"].([]any)[0].(map[string]any)
	if _, present := unit["stale"]; present {
		t.Fatal("stale key should be stripped")
	}
	assets := unit["generated_assets"].(map[string]any)
	if assets["video_clip"] != "reference_videos/E1U01.mp4" || assets["status"] != "completed" {
		t.Fatalf("unexpected assets: %v", assets)
	}
	if _, present := assets["video_uri"]; present {
		t.Fatal("empty video_uri should clear the old value")
	}
	if _, present := assets["video_thumbnail"]; present {
		t.Fatal("empty thumbnail should clear the old value")
	}
}

func TestApplyProviderConstraintsClampsAndWarns(t *testing.T) {
	refs := []string{"a.png", "b.png", "c.png"}
	maxRefs := 2
	constrained, duration, warnings := reference.ApplyProviderConstraints(
		"openai", "sora-2", &maxRefs, []int{4, 8, 12}, refs, 8, "openai", "720p",
	)
	if len(constrained) != 2 || constrained[0] != "a.png" {
		t.Fatalf("clamp failed: %v", constrained)
	}
	if duration != 8 {
		t.Fatalf("duration = %d, want 8", duration)
	}
	if len(warnings) == 0 {
		t.Fatal("too-many-images clamp should warn")
	}
	// sora 单图专用 warning key。
	_, _, soraWarnings := reference.ApplyProviderConstraints(
		"openai", "sora-2", intPtr(1), []int{4, 8, 12}, []string{"a", "b"}, 8, "openai", "720p",
	)
	if len(soraWarnings) == 0 || soraWarnings[0]["key"] != "ref_sora_single_ref" {
		t.Fatalf("sora single-ref warning missing: %v", soraWarnings)
	}
	// 时长向上取档记 warning。
	_, upDuration, upWarnings := reference.ApplyProviderConstraints(
		"openai", "sora-2", nil, []int{4, 8, 12}, []string{"a"}, 5, "openai", "720p",
	)
	if upDuration != 8 {
		t.Fatalf("expected up-tier 8, got %d", upDuration)
	}
	if len(upWarnings) == 0 || upWarnings[0]["key"] != "ref_duration_rounded_up" {
		t.Fatalf("duration up warning missing: %v", upWarnings)
	}
}

func writeUnitScript(t *testing.T, store *service.ProjectStore, projectDir string) {
	t.Helper()
	sheet := filepath.Join(projectDir, "characters", "李明.png")
	if err := os.MkdirAll(filepath.Dir(sheet), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(sheet, []byte("png"), 0o644); err != nil {
		t.Fatalf("write sheet: %v", err)
	}
	script := map[string]any{
		"episode":      1,
		"content_mode": "narration",
		"title":        "第一集",
		"video_units": []any{
			map[string]any{
				"unit_id":          "E1U01",
				"duration_seconds": 8,
				"shots": []any{
					map[string]any{"text": "镜头1：@[李明] 缓步走向窗前，抬手推开木窗。"},
				},
				"references": []any{
					map[string]any{"type": "character", "name": "李明"},
				},
			},
		},
	}
	if err := store.WriteScriptJSON("p1", "episode_1.json", script); err != nil {
		t.Fatalf("write script: %v", err)
	}
}

func TestExecuteReferenceVideoTaskEndToEnd(t *testing.T) {
	dispatcher, store, _, projectName := newVideoFixture(t, domain.ContentNarration, domain.GenerationReferenceVideo)
	projectDir, err := store.Dir(projectName)
	if err != nil {
		t.Fatalf("project dir: %v", err)
	}
	if err := store.Update(projectName, func(p *domain.ProjectFile) error {
		entry := domain.NewAssetEntry()
		entry.SetString("description", "角色")
		entry.SetString("character_sheet", "characters/李明.png")
		p.Characters = domain.AssetBucket{"李明": entry}
		return nil
	}); err != nil {
		t.Fatalf("seed character: %v", err)
	}
	writeUnitScript(t, store, projectDir)

	task := &repository.TaskData{
		TaskID:      "",
		ProjectName: projectName,
		TaskType:    "reference_video",
		MediaType:   "video",
		ResourceID:  "E1U01",
		Payload:     map[string]any{"script_file": "episode_1.json", "prompt": "占位（执行期不使用）"},
		UserID:      repository.DefaultUserID,
	}
	result, err := dispatcher.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("execute reference_video task: %v", err)
	}
	if result["file_path"] != "reference_videos/E1U01.mp4" || result["resource_type"] != "reference_videos" {
		t.Fatalf("unexpected result: %v", result)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "reference_videos", "E1U01.mp4")); err != nil {
		t.Fatalf("reference video output missing: %v", err)
	}

	script, err := store.ReadScriptJSON(projectName, "episode_1.json")
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	unit := script["video_units"].([]any)[0].(map[string]any)
	assets := unit["generated_assets"].(map[string]any)
	if assets["video_clip"] != "reference_videos/E1U01.mp4" || assets["status"] != "completed" {
		t.Fatalf("unit assets not written back: %v", assets)
	}
}

func TestExecuteReferenceVideoTaskUnitNotFoundFailsLoud(t *testing.T) {
	dispatcher, store, _, projectName := newVideoFixture(t, domain.ContentNarration, domain.GenerationReferenceVideo)
	projectDir, err := store.Dir(projectName)
	if err != nil {
		t.Fatalf("project dir: %v", err)
	}
	writeUnitScript(t, store, projectDir)
	_, err = dispatcher.Execute(context.Background(), &repository.TaskData{
		TaskID: "", ProjectName: projectName, TaskType: "reference_video", MediaType: "video",
		ResourceID: "E1U99",
		Payload:    map[string]any{"script_file": "episode_1.json", "prompt": "x"},
		UserID:     repository.DefaultUserID,
	})
	if err == nil || !strings.Contains(err.Error(), "unit not found") {
		t.Fatalf("missing unit should fail loud: %v", err)
	}
}
