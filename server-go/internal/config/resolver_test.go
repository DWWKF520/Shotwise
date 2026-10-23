package config

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/migrations"
)

func newTestResolver(t *testing.T) (*Resolver, *gorm.DB) {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "cfg.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open temp sqlite: %v", err)
	}
	if _, err := db.Migrate(gdb, dialect, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if pool, err := gdb.DB(); err == nil {
			_ = pool.Close()
		}
	})
	return NewResolver(gdb), gdb
}

func setSetting(t *testing.T, gdb *gorm.DB, key, value string) {
	t.Helper()
	if err := gdb.Exec(
		"INSERT INTO system_setting (key, value, updated_at) VALUES (?, ?, ?) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value, "2026-01-01T00:00:00+00:00",
	).Error; err != nil {
		t.Fatalf("set setting %s: %v", key, err)
	}
}

// makeReady 写入一条活跃凭证：ProviderReady 以「有活跃凭证」为准。
func makeReady(t *testing.T, gdb *gorm.DB, providerID string) {
	t.Helper()
	key := "sk-test"
	if err := gdb.Exec(
		"INSERT INTO provider_credential (provider, name, api_key, is_active, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?)",
		providerID, "default", key, true, "2026-01-01T00:00:00+00:00", "2026-01-01T00:00:00+00:00",
	).Error; err != nil {
		t.Fatalf("seed credential %s: %v", providerID, err)
	}
}

func defaultModel(t *testing.T, providerID string, media provider.MediaType) string {
	t.Helper()
	model, ok := provider.DefaultModelForProvider(providerID, media)
	if !ok {
		t.Fatalf("provider %s has no default %s model", providerID, media)
	}
	return model
}

// addCustomProvider 写入一个自定义供应商行，返回其 DB 主键。
func addCustomProvider(t *testing.T, gdb *gorm.DB, enabled bool) int64 {
	t.Helper()
	prov := repository.CustomProvider{
		DisplayName: "custom",
		BaseURL:     "https://example.test",
		APIKey:      "sk",
		IsEnabled:   enabled,
		CreatedAt:   mustTime(t),
		UpdatedAt:   mustTime(t),
	}
	if err := gdb.Create(&prov).Error; err != nil {
		t.Fatalf("create custom provider: %v", err)
	}
	return prov.ID
}

// addCustomModel 在指定自定义供应商下追加一个模型行。
func addCustomModel(t *testing.T, gdb *gorm.DB, providerID int64, modelID, endpoint string, enabled, isDefault bool) {
	t.Helper()
	model := repository.CustomProviderModel{
		ProviderID:  providerID,
		ModelID:     modelID,
		DisplayName: modelID,
		IsDefault:   isDefault,
		IsEnabled:   enabled,
		Endpoint:    endpoint,
		CreatedAt:   mustTime(t),
		UpdatedAt:   mustTime(t),
	}
	if err := gdb.Create(&model).Error; err != nil {
		t.Fatalf("create custom model: %v", err)
	}
}

func mustTime(t *testing.T) db.FlexibleTime {
	t.Helper()
	var ts db.FlexibleTime
	if err := ts.Scan("2026-01-01T00:00:00+00:00"); err != nil {
		t.Fatalf("parse time: %v", err)
	}
	return ts
}

func TestResolveImageLayeredPriority(t *testing.T) {
	r, gdb := newTestResolver(t)
	ctx := context.Background()
	openaiImage := defaultModel(t, "openai", provider.MediaImage)
	geminiImage := defaultModel(t, "gemini-aistudio", provider.MediaImage)

	// 层 1：项目桶。
	project := &project.ProjectFile{
		ImageProviderT2I:    "openai/" + openaiImage,
		DefaultImageBackend: "gemini-aistudio/" + geminiImage,
	}
	got, err := r.ResolveImageBackend(ctx, project, nil, ImageCapT2I)
	if err != nil {
		t.Fatalf("resolve image: %v", err)
	}
	if got.PairKey() != "openai/"+openaiImage {
		t.Fatalf("project bucket should win, got %s", got.PairKey())
	}

	// 层 2：项目桶清空 → 项目默认层。
	project.ImageProviderT2I = ""
	if got, err = r.ResolveImageBackend(ctx, project, nil, ImageCapT2I); err != nil {
		t.Fatalf("resolve image: %v", err)
	}
	if got.PairKey() != "gemini-aistudio/"+geminiImage {
		t.Fatalf("project default should win, got %s", got.PairKey())
	}

	// 层 3：全局桶（无项目）。
	setSetting(t, gdb, "default_image_backend_t2i", "openai/"+openaiImage)
	setSetting(t, gdb, "default_image_backend", "gemini-aistudio/"+geminiImage)
	if got, err = r.DefaultImageBackend(ctx, ImageCapT2I); err != nil {
		t.Fatalf("resolve global image: %v", err)
	}
	if got.PairKey() != "openai/"+openaiImage {
		t.Fatalf("global bucket should win, got %s", got.PairKey())
	}

	// 层 4：全局桶清空 → 全局默认层。
	setSetting(t, gdb, "default_image_backend_t2i", "")
	if got, err = r.DefaultImageBackend(ctx, ImageCapT2I); err != nil {
		t.Fatalf("resolve global image: %v", err)
	}
	if got.PairKey() != "gemini-aistudio/"+geminiImage {
		t.Fatalf("global default should win, got %s", got.PairKey())
	}

	// i2i 桶不被 t2i 配置影响：桶层为空 → 落到默认层。
	if got, err = r.DefaultImageBackend(ctx, ImageCapI2I); err != nil {
		t.Fatalf("resolve global i2i: %v", err)
	}
	if got.PairKey() != "gemini-aistudio/"+geminiImage {
		t.Fatalf("i2i should fall back to default layer, got %s", got.PairKey())
	}
}

func TestResolveImageBareProviderFillsDefaultModel(t *testing.T) {
	r, _ := newTestResolver(t)
	openaiImage := defaultModel(t, "openai", provider.MediaImage)

	// project.json 里只写了裸 provider：pin 住该供应商并补全默认 model，
	// 而不是回退到全局默认的另一供应商。
	project := &project.ProjectFile{DefaultImageBackend: "openai"}
	got, err := r.ResolveImageBackend(context.Background(), project, nil, ImageCapT2I)
	if err != nil {
		t.Fatalf("resolve image: %v", err)
	}
	if got.PairKey() != "openai/"+openaiImage {
		t.Fatalf("bare provider should pin provider, got %s", got.PairKey())
	}
}

func TestResolveImagePayloadWins(t *testing.T) {
	r, gdb := newTestResolver(t)
	openaiImage := defaultModel(t, "openai", provider.MediaImage)
	project := &project.ProjectFile{ImageProviderT2I: "gemini-aistudio/" + defaultModel(t, "gemini-aistudio", provider.MediaImage)}

	// payload 缺 model 时补该 provider 的默认 model。
	got, err := r.ResolveImageBackend(context.Background(), project, map[string]any{
		"image_provider": "openai",
	}, ImageCapT2I)
	if err != nil {
		t.Fatalf("resolve image: %v", err)
	}
	if got.PairKey() != "openai/"+openaiImage {
		t.Fatalf("payload should win, got %s", got.PairKey())
	}

	// 不可信 provider（registry 外）不予信任 → 回退骨架。
	if got, err = r.ResolveImageBackend(context.Background(), project, map[string]any{
		"image_provider": "legacy-seedance",
		"image_model":    "whatever",
	}, ImageCapT2I); err != nil {
		t.Fatalf("resolve image: %v", err)
	}
	if got.ProviderID != "gemini-aistudio" {
		t.Fatalf("untrusted payload provider should fall back, got %s", got.PairKey())
	}

	// 禁用的供应商被 payload 引用同样拦截。
	setSetting(t, gdb, "provider_enabled:openai", "0")
	if _, err = r.ResolveImageBackend(context.Background(), project, map[string]any{
		"image_provider": "openai",
	}, ImageCapT2I); !errors.Is(err, ErrProviderDisabled) {
		t.Fatalf("disabled provider via payload should error, got %v", err)
	}
}

func TestResolveDisabledProviderFailsLoud(t *testing.T) {
	r, gdb := newTestResolver(t)
	ctx := context.Background()
	setSetting(t, gdb, "provider_enabled:openai", "0")

	project := &project.ProjectFile{DefaultImageBackend: "openai/" + defaultModel(t, "openai", provider.MediaImage)}
	if _, err := r.ResolveImageBackend(ctx, project, nil, ImageCapT2I); !errors.Is(err, ErrProviderDisabled) {
		t.Fatalf("disabled provider should not be silently replaced, got %v", err)
	}
}

func TestAutoResolveBackend(t *testing.T) {
	r, gdb := newTestResolver(t)
	ctx := context.Background()

	// 无凭证 = 无 ready 供应商。
	if _, err := r.DefaultImageBackend(ctx, ImageCapT2I); !errors.Is(err, ErrNoUsableProvider) {
		t.Fatalf("expected no usable provider, got %v", err)
	}

	// 只有配了凭证的供应商参与自动推断（registry 注册顺序：gemini-aistudio 先于 openai）。
	makeReady(t, gdb, "openai")
	got, err := r.DefaultImageBackend(ctx, ImageCapT2I)
	if err != nil {
		t.Fatalf("auto resolve: %v", err)
	}
	if got.ProviderID != "openai" {
		t.Fatalf("auto resolve should pick the ready provider, got %s", got.PairKey())
	}

	// 自定义供应商在内置全不可用（禁用）时才轮到。
	setSetting(t, gdb, "provider_enabled:openai", "0")
	dbID := addCustomProvider(t, gdb, true)
	addCustomModel(t, gdb, dbID, "my-image", "openai-images", true, true)
	if got, err = r.DefaultImageBackend(ctx, ImageCapT2I); err != nil {
		t.Fatalf("auto resolve custom: %v", err)
	}
	if got.PairKey() != MakeProviderID(dbID)+"/my-image" {
		t.Fatalf("auto resolve should fall back to custom default, got %s", got.PairKey())
	}
}

func TestResolveVideoPayloadPinnedWins(t *testing.T) {
	r, _ := newTestResolver(t)
	openaiVideo := defaultModel(t, "openai", provider.MediaVideo)
	bucket := provider.BucketI2V

	project := &project.ProjectFile{VideoBackend: "gemini-aistudio/" + defaultModel(t, "gemini-aistudio", provider.MediaVideo)}
	got, err := r.ResolveVideoBackend(context.Background(), project, map[string]any{
		"video_provider_i2v": "openai/" + openaiVideo,
	}, &bucket)
	if err != nil {
		t.Fatalf("resolve video: %v", err)
	}
	if got.PairKey() != "openai/"+openaiVideo {
		t.Fatalf("pinned payload should win, got %s", got.PairKey())
	}
}

func TestResolveVideoIdentityGate(t *testing.T) {
	r, _ := newTestResolver(t)
	ctx := context.Background()
	bucket := provider.BucketI2V

	// 引用一个不是视频模型的 id：身份闸先于能力闸报错。
	project := &project.ProjectFile{VideoBackend: "openai/" + defaultModel(t, "openai", provider.MediaImage)}
	_, err := r.ResolveVideoBackend(ctx, project, nil, &bucket)
	if _, ok := IsVideoBucketCapabilityError(err); !ok {
		t.Fatalf("expected bucket capability error, got %v", err)
	}
	target, _ := IsVideoBucketCapabilityError(err)
	if target.Code != provider.FailureVideoCapabilityReferenceUnavailable {
		t.Fatalf("expected reference unavailable, got %s", target.Code)
	}
}

func TestResolveVideoBucketCapabilityGate(t *testing.T) {
	// 能力闸读 backend 声明：注册一个只支持首帧、不支持参考图的 openai 视频 backend。
	provider.RegisterVideoBackend("openai", provider.VideoBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.VideoBackend, error) {
			return nil, errors.New("not implemented in test")
		},
		CapabilitiesForModel: func(modelID string) (provider.VideoCapabilities, error) {
			caps := provider.DefaultVideoCapabilities()
			caps.MaxReferenceImages = 0
			return caps, nil
		},
	})
	t.Cleanup(func() {
		provider.RegisterVideoBackend("openai", provider.VideoBackendRegistration{})
	})

	r, _ := newTestResolver(t)
	ctx := context.Background()
	model := defaultModel(t, "openai", provider.MediaVideo)
	project := &project.ProjectFile{VideoBackend: "openai/" + model}

	i2v := provider.BucketI2V
	if _, err := r.ResolveVideoBackend(ctx, project, nil, &i2v); err != nil {
		t.Fatalf("i2v should pass the gate: %v", err)
	}

	r2v := provider.BucketR2V
	_, err := r.ResolveVideoBackend(ctx, project, nil, &r2v)
	target, ok := IsVideoBucketCapabilityError(err)
	if !ok {
		t.Fatalf("r2v should be rejected, got %v", err)
	}
	if target.Code != provider.FailureVideoCapabilityMissingR2V {
		t.Fatalf("expected r2v missing code, got %s", target.Code)
	}
}

func TestResolveVideoCustomProviderConvergesToDefaultModel(t *testing.T) {
	r, gdb := newTestResolver(t)
	ctx := context.Background()
	dbID := addCustomProvider(t, gdb, true)
	addCustomModel(t, gdb, dbID, "stale-model", "openai-video", false, false)
	addCustomModel(t, gdb, dbID, "fallback-model", "openai-video", true, true)
	providerID := MakeProviderID(dbID)

	// 不定桶路径：model 已禁用 → 收敛到该供应商默认启用的 video model。
	project := &project.ProjectFile{VideoBackend: providerID + "/stale-model"}
	got, err := r.ResolveVideoBackend(ctx, project, nil, nil)
	if err != nil {
		t.Fatalf("resolve video custom: %v", err)
	}
	if got.PairKey() != providerID+"/fallback-model" {
		t.Fatalf("expected convergence to default model, got %s", got.PairKey())
	}

	// 供应商级禁用 → 报错，不换供应商。
	if err = gdb.Exec("UPDATE custom_provider SET is_enabled = ? WHERE id = ?", false, dbID).Error; err != nil {
		t.Fatalf("disable custom provider: %v", err)
	}
	if _, err = r.ResolveVideoBackend(ctx, project, nil, nil); !errors.Is(err, ErrProviderDisabled) {
		t.Fatalf("disabled custom provider should error, got %v", err)
	}
}

func TestResolveAudioBackend(t *testing.T) {
	r, gdb := newTestResolver(t)
	ctx := context.Background()
	audioModel := defaultModel(t, "dashscope", provider.MediaAudio)

	setSetting(t, gdb, "default_audio_backend", "dashscope/"+audioModel)
	got, err := r.ResolveAudioBackend(ctx, nil, nil)
	if err != nil {
		t.Fatalf("resolve audio: %v", err)
	}
	if got.PairKey() != "dashscope/"+audioModel {
		t.Fatalf("global audio default expected, got %s", got.PairKey())
	}

	// 项目层覆盖优先（自定义供应商走同一骨架）。
	dbID := addCustomProvider(t, gdb, true)
	addCustomModel(t, gdb, dbID, "my-tts", "openai-tts", true, true)
	project := &project.ProjectFile{AudioBackend: MakeProviderID(dbID) + "/my-tts"}
	if got, err = r.ResolveAudioBackend(ctx, project, nil); err != nil {
		t.Fatalf("resolve audio: %v", err)
	}
	if got.PairKey() != MakeProviderID(dbID)+"/my-tts" {
		t.Fatalf("project audio backend expected, got %s", got.PairKey())
	}
}

func TestSynthesizeVideoCapabilities(t *testing.T) {
	caps, err := synthesizeVideoCapabilities("openai-video", "any", nil)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if !caps.FirstFrame || caps.MaxReferenceImages != 1 {
		t.Fatalf("expected endpoint default caps, got %+v", caps)
	}

	// 覆盖只收类型匹配的键；脏值忽略并回退系统判定。
	overrides := `{"first_frame": false, "max_reference_images": 3, "last_frame": "yes"}`
	caps, err = synthesizeVideoCapabilities("openai-video", "any", &overrides)
	if err != nil {
		t.Fatalf("synthesize with overrides: %v", err)
	}
	if caps.FirstFrame || caps.MaxReferenceImages != 3 || caps.LastFrame {
		t.Fatalf("overrides not applied as expected: %+v", caps)
	}

	// 非 video endpoint 与未声明能力来源的 endpoint 一律 fail loud。
	if _, err = synthesizeVideoCapabilities("openai-tts", "any", nil); err == nil {
		t.Fatal("non-video endpoint should fail")
	}
	if _, err = synthesizeVideoCapabilities("ark-seedance", "any", nil); err == nil {
		t.Fatal("endpoint without system capability source should fail loud")
	}
}
