package generation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/domain/script"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/migrations"
)

// newServiceTestDB 起一个内存 SQLite 库并跑全量迁移，顺带种下 default 用户（记账行有外键）。
func newServiceTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "service.db"), logger.Silent)
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
		t.Fatalf("seed default user: %v", err)
	}
	return gdb
}

// seedSetting 写一条 system_setting。
func seedSetting(t *testing.T, gdb *gorm.DB, key, value string) {
	t.Helper()
	if err := gdb.Exec(
		"INSERT INTO system_setting (key, value, updated_at) VALUES (?, ?, ?)",
		key, value, "2026-01-01T00:00:00+00:00",
	).Error; err != nil {
		t.Fatalf("seed setting %s: %v", key, err)
	}
}

func TestEncodeTaskFailureStructuredAndDegraded(t *testing.T) {
	got := EncodeTaskFailure("video_resolution_not_supported", map[string]any{"resolution": "4k"})
	want := `[video_resolution_not_supported] {"resolution": "4k"}`
	if got != want {
		t.Fatalf("structured encode = %q, want %q", got, want)
	}
	if got := EncodeTaskFailure("video_resolution_not_supported", nil); got != "[video_resolution_not_supported]" {
		t.Fatalf("empty params encode = %q", got)
	}
	if got := EncodeTaskFailure("not_a_registered_code", map[string]any{"x": 1}); got != "[not_a_registered_code]" {
		t.Fatalf("unknown code must degrade to [code], got %q", got)
	}
}

func TestEncodeTaskErrorEncodesCapabilityAndScriptEdit(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "video capability",
			err:  provider.NewVideoCapabilityError(provider.FailureVideoDurationNotSupported, map[string]any{"duration": 7}),
			want: `[video_duration_not_supported] {"duration": 7}`,
		},
		{
			name: "image capability",
			err:  provider.NewImageCapabilityError(provider.FailureImageCapabilityMissingI2I, nil),
			want: `[image_capability_missing_i2i]`,
		},
		{
			name: "video bucket gate",
			err: &config.VideoBucketCapabilityError{
				Code: provider.FailureVideoCapabilityMissingI2V, ProviderID: "p", ModelID: "m", Message: "cap",
			},
			want: `[video_capability_missing_i2v] {"model": "m", "provider": "p"}`,
		},
		{
			name: "script edit",
			err: &script.ScriptEditError{
				Message: "items must be list",
				Key:     script.ScriptEditItemsNotList,
				Params:  map[string]any{"kind": "segments", "type_name": "dict"},
			},
			want: `[script_edit_items_not_list] {"kind": "segments", "type_name": "dict"}`,
		},
		{
			name: "plain error is bounded text",
			err:  errors.New("供应商返回 503 Service Unavailable"),
			want: "供应商返回 503 Service Unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EncodeTaskError(tc.err); got != tc.want {
				t.Fatalf("EncodeTaskError = %q, want %q", got, tc.want)
			}
		})
	}
	if got := EncodeTaskError(nil); got != "" {
		t.Fatalf("nil error must encode empty, got %q", got)
	}
}

func TestIsTransientTaskError(t *testing.T) {
	if IsTransientTaskError(context.Canceled) {
		t.Fatal("user cancel is not an upstream health signal")
	}
	if IsTransientTaskError(nil) {
		t.Fatal("nil must not be transient")
	}
	if !IsTransientTaskError(errors.New("upstream 503 Service Unavailable")) {
		t.Fatal("retryable status pattern must be transient")
	}
}

func TestLedgerSettlerSettlesAndResumes(t *testing.T) {
	gdb := newServiceTestDB(t)
	ledger := service.NewLedger(gdb, nil)
	settler := NewLedgerSettler(ledger)
	ctx := context.Background()

	call1, err := ledger.Start(ctx, service.StartParams{ProjectName: "p1", CallType: "image", Model: "m", Provider: "openai"})
	if err != nil {
		t.Fatalf("start call1: %v", err)
	}
	call2, err := ledger.Start(ctx, service.StartParams{ProjectName: "p1", CallType: "image", Model: "m", Provider: "openai"})
	if err != nil {
		t.Fatalf("start call2: %v", err)
	}

	settled, err := settler.SettleInterruptedCalls(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("settle interrupted: %v", err)
	}
	if settled != 2 {
		t.Fatalf("settled = %d, want 2", settled)
	}
	assertCallStatus(t, gdb, call1.ID, service.StatusFailed)

	if err := settler.ResumeFailed(ctx, call2.ID); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	assertCallStatus(t, gdb, call2.ID, service.StatusFailed)

	// 幂等：已终态的行不再被重复收口。
	settled, err = settler.SettleInterruptedCalls(ctx, time.Time{})
	if err != nil || settled != 0 {
		t.Fatalf("second settle = (%d, %v), want (0, nil)", settled, err)
	}
}

func assertCallStatus(t *testing.T, gdb *gorm.DB, callID int64, want string) {
	t.Helper()
	var row repository.APICall
	if err := gdb.Where("id = ?", callID).First(&row).Error; err != nil {
		t.Fatalf("load api_call %d: %v", callID, err)
	}
	if row.Status != want {
		t.Fatalf("call %d status = %q, want %q", callID, row.Status, want)
	}
}

func strPtr(s string) *string { return &s }

func TestProviderExtractorPrefersPersistedThenPinned(t *testing.T) {
	ctx := context.Background()
	extractor := NewProviderExtractor(nil, nil)

	if got := extractor(ctx, &repository.TaskData{ProviderID: strPtr("openai")}); got != "openai" {
		t.Fatalf("persisted provider_id should win, got %q", got)
	}

	videoTask := &repository.TaskData{
		TaskType:  "video",
		MediaType: "video",
		Payload:   map[string]any{"video_provider_i2v": "kling/kling-v2-5-turbo"},
	}
	if got := extractor(ctx, videoTask); got != "kling" {
		t.Fatalf("pinned bucket identity should win, got %q", got)
	}

	// 无持久化、无锁定键、无 resolver → 空串（worker 会回退 DefaultProvider）。
	if got := extractor(ctx, &repository.TaskData{TaskType: "video", MediaType: "video"}); got != "" {
		t.Fatalf("unresolvable must return empty, got %q", got)
	}
}

func TestProviderExtractorFallsBackToResolver(t *testing.T) {
	gdb := newServiceTestDB(t)
	// 全局默认图片供应商 + 凭证，让 resolver 的自动/默认层可解析出 provider。
	seedSetting(t, gdb, "default_image_backend", "openai/gpt-image-2")
	for _, row := range []struct {
		key    string
		value  string
		secret bool
	}{
		{"api_key", "sk-test", true},
		{"base_url", "https://api.example.invalid/v1", false},
	} {
		if err := gdb.Exec(
			"INSERT INTO provider_config (provider, key, value, is_secret, updated_at) VALUES (?, ?, ?, ?, ?)",
			"openai", row.key, row.value, row.secret, "2026-01-01T00:00:00+00:00",
		).Error; err != nil {
			t.Fatalf("seed provider config: %v", err)
		}
	}

	extractor := NewProviderExtractor(nil, config.NewResolver(gdb))
	got := extractor(context.Background(), &repository.TaskData{
		TaskType:  "storyboard",
		MediaType: "image",
		Payload:   map[string]any{},
	})
	if got != "openai" {
		t.Fatalf("resolver fallback = %q, want openai", got)
	}
}

// writeUnderProject 在项目目录下写文件。
func writeUnderProject(t *testing.T, dir, rel string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// registerCharacter 把角色登记进 project.json 并落盘其设计图。
func registerCharacter(t *testing.T, store *service.ProjectStore, projectName, projectDir, name string) {
	t.Helper()
	writeUnderProject(t, projectDir, "characters/"+name+".png")
	if err := store.Update(projectName, func(p *project.ProjectFile) error {
		if p.Characters == nil {
			p.Characters = project.AssetBucket{}
		}
		p.Characters[name] = project.WrapAssetEntry(map[string]any{
			"description":     name,
			"character_sheet": "characters/" + name + ".png",
		})
		return nil
	}); err != nil {
		t.Fatalf("register character %s: %v", name, err)
	}
}

func TestWorkerPreflightStoryboardItemReferences(t *testing.T) {
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "t", project.ContentNarration, project.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	projectDir, err := store.Dir("p1")
	if err != nil {
		t.Fatal(err)
	}
	registerCharacter(t, store, "p1", projectDir, "阿明")
	if err := store.WriteScriptJSON("p1", "episode_1.json", map[string]any{
		"episode":      1,
		"content_mode": "narration",
		"segments": []any{
			map[string]any{"segment_id": "E1S01", "characters_in_segment": []any{"阿明"}, "scenes": []any{}, "props": []any{}},
			map[string]any{"segment_id": "E1S02", "characters_in_segment": []any{"幽灵"}, "scenes": []any{}, "props": []any{}},
		},
	}); err != nil {
		t.Fatalf("write script: %v", err)
	}

	preflight := NewWorkerPreflight(store)
	ctx := context.Background()

	taskFor := func(resourceID string) *repository.TaskData {
		return &repository.TaskData{
			TaskID: "task-1", ProjectName: "p1", TaskType: "storyboard", MediaType: "image",
			ResourceID: resourceID, Payload: map[string]any{"script_file": "episode_1.json"},
		}
	}
	if err := preflight(ctx, taskFor("E1S01")); err != nil {
		t.Fatalf("registered asset must pass, got %v", err)
	}
	if err := preflight(ctx, taskFor("E1S02")); err == nil {
		t.Fatal("unregistered asset reference must fail preflight")
	}
	if err := preflight(ctx, taskFor("E1S99")); err == nil {
		t.Fatal("missing storyboard item must fail preflight")
	}

	// 未支持的 task_type 放行。
	if err := preflight(ctx, &repository.TaskData{ProjectName: "p1", TaskType: "tts", Payload: map[string]any{"script_file": "episode_1.json"}}); err != nil {
		t.Fatalf("unsupported task type must pass, got %v", err)
	}
}

func TestWorkerPreflightReferenceUnitReferences(t *testing.T) {
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("p2", "t", project.ContentNarration, project.GenerationReferenceVideo, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	projectDir, err := store.Dir("p2")
	if err != nil {
		t.Fatal(err)
	}
	registerCharacter(t, store, "p2", projectDir, "阿明")
	if err := store.WriteScriptJSON("p2", "episode_1.json", map[string]any{
		"episode":      1,
		"content_mode": "narration",
		"video_units": []any{
			map[string]any{"unit_id": "E1U01", "references": []any{map[string]any{"type": "character", "name": "阿明"}}},
			map[string]any{"unit_id": "E1U02", "references": []any{map[string]any{"type": "character", "name": "幽灵"}}},
		},
	}); err != nil {
		t.Fatalf("write script: %v", err)
	}

	preflight := NewWorkerPreflight(store)
	ctx := context.Background()
	taskFor := func(resourceID string) *repository.TaskData {
		return &repository.TaskData{
			TaskID: "task-1", ProjectName: "p2", TaskType: "reference_video", MediaType: "video",
			ResourceID: resourceID, Payload: map[string]any{"script_file": "episode_1.json"},
		}
	}
	if err := preflight(ctx, taskFor("E1U01")); err != nil {
		t.Fatalf("registered reference must pass, got %v", err)
	}
	if err := preflight(ctx, taskFor("E1U02")); err == nil {
		t.Fatal("unregistered reference must fail preflight")
	}
	if err := preflight(ctx, taskFor("E1U99")); err == nil {
		t.Fatal("missing reference unit must fail preflight")
	}
}
