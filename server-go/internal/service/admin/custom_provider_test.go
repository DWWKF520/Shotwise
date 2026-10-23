package admin

import "github.com/shotwise/shotwise/internal/service"

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/providers"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/migrations"
)

type customProviderFixture struct {
	svc   *CustomProviderService
	store *service.ProjectStore
	db    *gorm.DB
}

func newCustomProviderFixture(t *testing.T) customProviderFixture {
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
	return customProviderFixture{
		svc:   NewCustomProviderService(gdb, store, nil),
		store: store,
		db:    gdb,
	}
}

func (f customProviderFixture) setSetting(t *testing.T, key, value string) {
	t.Helper()
	if err := config.NewConfigService(f.db).SetSetting(context.Background(), key, value); err != nil {
		t.Fatalf("set setting %s: %v", key, err)
	}
}

func (f customProviderFixture) setting(t *testing.T, key string) string {
	t.Helper()
	value, err := config.NewConfigService(f.db).GetSetting(context.Background(), key, "")
	if err != nil {
		t.Fatalf("get setting %s: %v", key, err)
	}
	return value
}

func customErrCode(t *testing.T, err error) string {
	t.Helper()
	var appErr *apperr.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("expected apperr.Error, got %T: %v", err, err)
	}
	return appErr.Code
}

// customVideoModel 是一个合法的视频模型入参。
func customVideoModel(modelID, endpoint string) CustomModelInput {
	return CustomModelInput{
		ModelID:            modelID,
		DisplayName:        modelID,
		Endpoint:           endpoint,
		IsEnabled:          true,
		SupportedDurations: []int{5, 10},
	}
}

func customCreateInput(name string, models ...CustomModelInput) CustomProviderCreateInput {
	return CustomProviderCreateInput{
		DisplayName:     name,
		DiscoveryFormat: "openai",
		BaseURL:         "https://example.com/v1",
		APIKey:          "sk-secret-value-1234",
		Models:          models,
	}
}

// ── CRUD ─────────────────────────────────────────────────────────────────

func TestCustomProviderCreateRoundTrip(t *testing.T) {
	f := newCustomProviderFixture(t)
	created, err := f.svc.CreateProvider(context.Background(), customCreateInput("中转站",
		customVideoModel("sora-2", "openai-video"),
		CustomModelInput{
			ModelID: "gpt-image-1", DisplayName: "绘图", Endpoint: "openai-images", IsEnabled: true,
		},
	))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("expected assigned id")
	}
	if created.APIKeyMasked == "sk-secret-value-1234" {
		t.Fatal("api key leaked in response")
	}
	if created.APIKeyMasked == "" {
		t.Fatal("expected masked api key")
	}
	if len(created.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(created.Models))
	}
	if !created.IsEnabled {
		t.Fatal("expected default enabled")
	}
	if created.CreatedAt == nil {
		t.Fatal("expected created_at")
	}

	byID, err := f.svc.GetProvider(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(byID.Models) != 2 {
		t.Fatalf("expected 2 models on read, got %d", len(byID.Models))
	}

	list, err := f.svc.ListProviders(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(list))
	}
}

func TestCustomProviderCredentialsReturnPlaintext(t *testing.T) {
	f := newCustomProviderFixture(t)
	created, err := f.svc.CreateProvider(context.Background(), customCreateInput("中转站"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	creds, err := f.svc.Credentials(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if creds.APIKey != "sk-secret-value-1234" {
		t.Fatalf("expected plaintext key, got %q", creds.APIKey)
	}
	if creds.BaseURL != "https://example.com/v1" {
		t.Fatalf("unexpected base url %q", creds.BaseURL)
	}
}

func TestCustomProviderPatchRejectsEmptyBody(t *testing.T) {
	f := newCustomProviderFixture(t)
	created, err := f.svc.CreateProvider(context.Background(), customCreateInput("中转站"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = f.svc.PatchProvider(context.Background(), created.ID, CustomProviderPatchInput{})
	if code := customErrCode(t, err); code != "at_least_one_field_required" {
		t.Fatalf("unexpected code %q", code)
	}
}

func TestCustomProviderPatchOnlyTouchesProvidedFields(t *testing.T) {
	f := newCustomProviderFixture(t)
	created, err := f.svc.CreateProvider(context.Background(), customCreateInput("中转站"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	name := "改名后的"
	patched, err := f.svc.PatchProvider(context.Background(), created.ID,
		CustomProviderPatchInput{DisplayName: &name})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if patched.DisplayName != name {
		t.Fatalf("expected renamed, got %q", patched.DisplayName)
	}
	creds, err := f.svc.Credentials(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if creds.APIKey != "sk-secret-value-1234" {
		t.Fatal("patch without api_key must not clear it")
	}
}

func TestCustomProviderReplaceIsAuthoritativeForScalars(t *testing.T) {
	f := newCustomProviderFixture(t)
	workers := int64(3)
	created, err := f.svc.CreateProvider(context.Background(), CustomProviderCreateInput{
		DisplayName: "中转站", DiscoveryFormat: "openai",
		BaseURL: "https://example.com/v1", APIKey: "sk-secret-value-1234",
		VideoMaxWorkers: &workers,
		Models:          []CustomModelInput{customVideoModel("sora-2", "openai-video")},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// api_key 传 nil = 不改；并发上限不传 = 清空回全局默认。
	replaced, err := f.svc.ReplaceProvider(context.Background(), created.ID, CustomProviderReplaceInput{
		DisplayName: "整表提交", BaseURL: "https://example.com/v2",
		Models: []CustomModelInput{customVideoModel("sora-2", "openai-video")},
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if replaced.DisplayName != "整表提交" {
		t.Fatalf("unexpected display name %q", replaced.DisplayName)
	}
	if replaced.VideoMaxWorkers != nil {
		t.Fatal("expected video_max_workers cleared")
	}
	creds, err := f.svc.Credentials(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if creds.APIKey != "sk-secret-value-1234" {
		t.Fatal("replace without api_key must not clear it")
	}
}

func TestCustomProviderMissingIs404(t *testing.T) {
	f := newCustomProviderFixture(t)
	if _, err := f.svc.GetProvider(context.Background(), 404); customErrCode(t, err) != "provider_not_found" {
		t.Fatalf("unexpected code %q", err)
	}
	if err := f.svc.DeleteProvider(context.Background(), 404); customErrCode(t, err) != "provider_not_found" {
		t.Fatalf("unexpected code %q", err)
	}
}

// ── 校验 ─────────────────────────────────────────────────────────────────

func TestCustomProviderRejectsInvalidModels(t *testing.T) {
	cases := []struct {
		name  string
		model CustomModelInput
		code  string
	}{
		{
			name:  "unknown endpoint",
			model: CustomModelInput{ModelID: "m", Endpoint: "not-an-endpoint", IsEnabled: true},
			code:  "unknown_endpoint",
		},
		{
			name:  "video without durations",
			model: CustomModelInput{ModelID: "m", Endpoint: "openai-video", IsEnabled: true},
			code:  "literal",
		},
		{
			name: "last frame on endpoint without end image",
			model: CustomModelInput{
				ModelID: "m", Endpoint: "openai-video", IsEnabled: true,
				SupportedDurations: []int{5}, CapabilityOverrides: map[string]any{"last_frame": true},
			},
			code: "capability_override_last_frame_unsupported",
		},
		{
			name: "reference audio on endpoint without audio channel",
			model: CustomModelInput{
				ModelID: "m", Endpoint: "openai-video", IsEnabled: true,
				SupportedDurations:  []int{5},
				CapabilityOverrides: map[string]any{"reference_audio_mode": "direct"},
			},
			code: "capability_override_reference_audio_unsupported",
		},
		{
			name: "overrides on image endpoint",
			model: CustomModelInput{
				ModelID: "m", Endpoint: "openai-images", IsEnabled: true,
				CapabilityOverrides: map[string]any{"last_frame": true},
			},
			code: "capability_overrides_video_only",
		},
		{
			name: "poll on video endpoint",
			model: CustomModelInput{
				ModelID: "m", Endpoint: "openai-video", IsEnabled: true,
				SupportedDurations:  []int{5},
				EndpointDeclaration: map[string]any{"poll": map[string]any{"done_value": true}},
			},
			code: "literal",
		},
		{
			name: "price output without input",
			model: CustomModelInput{
				ModelID: "m", Endpoint: "openai-chat", IsEnabled: true,
				PriceOutput: float64Ptr(1),
			},
			code: "price_input_required",
		},
		{
			name:  "enabled without model id",
			model: CustomModelInput{Endpoint: "openai-chat", IsEnabled: true},
			code:  "model_id_required",
		},
		{
			name:  "enabled without endpoint",
			model: CustomModelInput{ModelID: "m", IsEnabled: true},
			code:  "endpoint_required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCustomProviderFixture(t)
			_, err := f.svc.CreateProvider(context.Background(), customCreateInput("x", tc.model))
			if err == nil {
				t.Fatal("expected rejection")
			}
			if tc.code == "literal" {
				return // 字面量错误不带 i18n key，只断言被拒
			}
			if code := customErrCode(t, err); code != tc.code {
				t.Fatalf("expected %q, got %q", tc.code, code)
			}
		})
	}
}

func TestCustomProviderAcceptsSupportedOverrides(t *testing.T) {
	f := newCustomProviderFixture(t)
	created, err := f.svc.CreateProvider(context.Background(), customCreateInput("x",
		CustomModelInput{
			ModelID: "seedance-2", DisplayName: "seedance", Endpoint: "ark-seedance", IsEnabled: true,
			SupportedDurations: []int{5, 10},
			CapabilityOverrides: map[string]any{
				"last_frame":                true,
				"reference_audio_mode":      "direct",
				"max_reference_audio_count": 2,
			},
		},
	))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	overrides := created.Models[0].CapabilityOverrides
	if overrides["last_frame"] != true {
		t.Fatalf("last_frame override missing: %#v", overrides)
	}
	if overrides["reference_audio_mode"] != "direct" {
		t.Fatalf("reference_audio_mode override missing: %#v", overrides)
	}
	if overrides["max_reference_audio_count"] == nil {
		t.Fatalf("max_reference_audio_count override missing: %#v", overrides)
	}
}

func TestCustomProviderNarrowsOverridesToAllowlistOnWrite(t *testing.T) {
	f := newCustomProviderFixture(t)
	created, err := f.svc.CreateProvider(context.Background(), customCreateInput("x",
		CustomModelInput{
			ModelID: "m", DisplayName: "m", Endpoint: "v2-video-generations", IsEnabled: true,
			SupportedDurations: []int{5},
			CapabilityOverrides: map[string]any{
				"last_frame":       true,
				"max_prompt_chars": 1234, // 白名单外，只能来自手工改库
			},
		},
	))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	overrides := created.Models[0].CapabilityOverrides
	if _, leaked := overrides["max_prompt_chars"]; leaked {
		t.Fatalf("non-allowlisted key persisted: %#v", overrides)
	}
	var row repository.CustomProviderModel
	if err := f.db.First(&row, created.Models[0].ID).Error; err != nil {
		t.Fatalf("reload row: %v", err)
	}
	if row.CapabilityOverrides == nil {
		t.Fatal("expected persisted overrides")
	}
	if got := *row.CapabilityOverrides; got == "" {
		t.Fatal("expected non-empty persisted overrides")
	}
}

func TestCustomProviderFiltersDirtyOverridesOnRead(t *testing.T) {
	f := newCustomProviderFixture(t)
	created, err := f.svc.CreateProvider(context.Background(), customCreateInput("x"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 绕过写入侧直接落一行「endpoint 已不再 end_image_capable 却留着 last_frame=true」的存量脏数据。
	dirty := `{"last_frame":true,"max_prompt_chars":999}`
	row := repository.CustomProviderModel{
		ProviderID: created.ID, ModelID: "sora-2", DisplayName: "sora-2",
		Endpoint: "openai-video", IsEnabled: true,
		CapabilityOverrides: &dirty,
	}
	if err := f.db.Create(&row).Error; err != nil {
		t.Fatalf("insert dirty row: %v", err)
	}
	view, err := f.svc.GetProvider(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(view.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(view.Models))
	}
	if view.Models[0].CapabilityOverrides != nil {
		t.Fatalf("expected dirty overrides filtered out, got %#v", view.Models[0].CapabilityOverrides)
	}
}

func TestCustomProviderUniqueDefaults(t *testing.T) {
	image := func(modelID, endpoint string) CustomModelInput {
		return CustomModelInput{ModelID: modelID, DisplayName: modelID, Endpoint: endpoint, IsEnabled: true, IsDefault: true}
	}
	t.Run("disjoint image capabilities do not conflict", func(t *testing.T) {
		f := newCustomProviderFixture(t)
		_, err := f.svc.CreateProvider(context.Background(), customCreateInput("x",
			image("a", "openai-images-generations"), image("b", "openai-images-edits")))
		if err != nil {
			t.Fatalf("expected no conflict, got %v", err)
		}
	})
	t.Run("overlapping image capabilities conflict", func(t *testing.T) {
		f := newCustomProviderFixture(t)
		_, err := f.svc.CreateProvider(context.Background(), customCreateInput("x",
			image("a", "openai-images"), image("b", "openai-images-generations")))
		if code := customErrCode(t, err); code != "default_model_conflict" {
			t.Fatalf("expected conflict, got %q", code)
		}
	})
	t.Run("two video defaults conflict", func(t *testing.T) {
		f := newCustomProviderFixture(t)
		a := customVideoModel("a", "openai-video")
		a.IsDefault = true
		b := customVideoModel("b", "openai-video")
		b.IsDefault = true
		_, err := f.svc.CreateProvider(context.Background(), customCreateInput("x", a, b))
		if code := customErrCode(t, err); code != "default_model_conflict" {
			t.Fatalf("expected conflict, got %q", code)
		}
	})
}

func TestCustomProviderRejectsDuplicateModelIDs(t *testing.T) {
	f := newCustomProviderFixture(t)
	_, err := f.svc.CreateProvider(context.Background(), customCreateInput("x",
		customVideoModel("dup", "openai-video"), customVideoModel("dup", "openai-video")))
	if code := customErrCode(t, err); code != "duplicate_model_id" {
		t.Fatalf("expected duplicate rejection, got %q", code)
	}
}

func TestCustomProviderRejectsUnknownDiscoveryFormat(t *testing.T) {
	f := newCustomProviderFixture(t)
	in := customCreateInput("x")
	in.DiscoveryFormat = "anthropic" // 只能用于无状态探测入口，不能落库
	if _, err := f.svc.CreateProvider(context.Background(), in); customErrCode(t, err) != "invalid_discovery_format" {
		t.Fatalf("unexpected code %q", err)
	}
}

// ── 序列化 ───────────────────────────────────────────────────────────────

func TestCustomProviderSystemCapabilitiesOnlyForVideo(t *testing.T) {
	f := newCustomProviderFixture(t)
	created, err := f.svc.CreateProvider(context.Background(), customCreateInput("x",
		customVideoModel("sora-2", "openai-video"),
		CustomModelInput{ModelID: "gpt-image-1", DisplayName: "img", Endpoint: "openai-images", IsEnabled: true},
	))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	byID := map[string]CustomModelView{}
	for _, model := range created.Models {
		byID[model.ModelID] = model
	}
	if byID["sora-2"].SystemCapabilities == nil {
		t.Fatal("expected system capabilities for video model")
	}
	if byID["gpt-image-1"].SystemCapabilities != nil {
		t.Fatal("expected nil system capabilities for image model")
	}
}

func TestParseCustomDurationsAcceptsBothEncodings(t *testing.T) {
	cases := map[string][]int{
		"5,10":       {5, 10},
		"[5, 10]":    {5, 10},
		"5, 10 , 15": {5, 10, 15},
		"":           nil,
		"garbage":    nil,
	}
	for raw, want := range cases {
		got := parseCustomDurations(raw)
		if len(got) != len(want) {
			t.Fatalf("%q: expected %v, got %v", raw, want, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%q: expected %v, got %v", raw, want, got)
			}
		}
	}
}

func TestCustomProviderEndpointCatalogIsStable(t *testing.T) {
	f := newCustomProviderFixture(t)
	catalog := f.svc.EndpointCatalog()
	if len(catalog) == 0 {
		t.Fatal("expected non-empty catalog")
	}
	for i := 1; i < len(catalog); i++ {
		if catalog[i-1].Key >= catalog[i].Key {
			t.Fatalf("catalog not sorted at %d: %q >= %q", i, catalog[i-1].Key, catalog[i].Key)
		}
	}
}

// ── 引用提取 ─────────────────────────────────────────────────────────────

func TestCustomProviderBucketRefs(t *testing.T) {
	f := newCustomProviderFixture(t)
	created, err := f.svc.CreateProvider(context.Background(), customCreateInput("x",
		customVideoModel("sora-2", "openai-video")))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ref := config.MakeProviderID(created.ID) + "/sora-2"
	f.setSetting(t, "default_video_backend", ref)
	f.setSetting(t, "default_video_backend_i2v", ref)
	f.setSetting(t, "default_image_backend", "openai/dall-e-3") // 无关配置

	view, err := f.svc.GetProvider(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	refs := view.Models[0].GlobalBucketRefs
	if len(refs) != 2 {
		t.Fatalf("expected 2 bucket refs, got %v", refs)
	}
	found := false
	for _, key := range refs {
		if key == "default_video_backend_i2v" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected video bucket ref reported, got %v", refs)
	}
}

// ── 级联清理 ─────────────────────────────────────────────────────────────

func TestCustomProviderDeleteCascades(t *testing.T) {
	f := newCustomProviderFixture(t)
	created, err := f.svc.CreateProvider(context.Background(), customCreateInput("x",
		customVideoModel("sora-2", "openai-video")))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ref := config.MakeProviderID(created.ID) + "/sora-2"
	f.setSetting(t, "default_video_backend", ref)
	f.setSetting(t, "default_image_backend", ref)
	f.setSetting(t, "default_video_backend_i2v", ref) // 桶键刻意不级联清理

	if _, err := f.store.Create("p1", "测试项目",
		project.ContentNarration, project.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := f.store.Update("p1", func(pf *project.ProjectFile) error {
		pf.VideoBackend = ref
		pf.AudioBackend = "openai/gpt-4o-mini-tts"
		return nil
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	if err := f.svc.DeleteProvider(context.Background(), created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var models int64
	if err := f.db.Model(&repository.CustomProviderModel{}).
		Where("provider_id = ?", created.ID).Count(&models).Error; err != nil {
		t.Fatalf("count models: %v", err)
	}
	if models != 0 {
		t.Fatalf("expected models cascaded, %d remain", models)
	}
	if got := f.setting(t, "default_video_backend"); got != "" {
		t.Fatalf("expected global default cleared, got %q", got)
	}
	if got := f.setting(t, "default_image_backend"); got != "" {
		t.Fatalf("expected global default cleared, got %q", got)
	}
	if got := f.setting(t, "default_video_backend_i2v"); got != ref {
		t.Fatalf("video bucket key must not be cascade cleared, got %q", got)
	}
	pf, err := f.store.Get("p1")
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if pf.VideoBackend != "" {
		t.Fatalf("expected project video backend cleared, got %q", pf.VideoBackend)
	}
	if pf.AudioBackend != "openai/gpt-4o-mini-tts" {
		t.Fatalf("unrelated project backend must survive, got %q", pf.AudioBackend)
	}
}

func TestCustomProviderReplaceModelsClearsStaleRefs(t *testing.T) {
	f := newCustomProviderFixture(t)
	created, err := f.svc.CreateProvider(context.Background(), customCreateInput("x",
		customVideoModel("old-model", "openai-video"), customVideoModel("kept", "openai-video")))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	prefix := config.MakeProviderID(created.ID) + "/"
	f.setSetting(t, "default_video_backend", prefix+"old-model")
	f.setSetting(t, "default_audio_backend", prefix+"kept")

	if _, err := f.svc.ReplaceModels(context.Background(), created.ID,
		[]CustomModelInput{customVideoModel("kept", "openai-video")}); err != nil {
		t.Fatalf("replace models: %v", err)
	}
	if got := f.setting(t, "default_video_backend"); got != "" {
		t.Fatalf("expected stale ref cleared, got %q", got)
	}
	if got := f.setting(t, "default_audio_backend"); got != prefix+"kept" {
		t.Fatalf("surviving model ref must be kept, got %q", got)
	}

	var rows []repository.CustomProviderModel
	if err := f.db.Where("provider_id = ?", created.ID).Find(&rows).Error; err != nil {
		t.Fatalf("list models: %v", err)
	}
	if len(rows) != 1 || rows[0].ModelID != "kept" {
		t.Fatalf("expected replace semantics, got %#v", rows)
	}
}

func TestCustomProviderReloadLimitsIsInvoked(t *testing.T) {
	f := newCustomProviderFixture(t)
	counter := &countingRefresher{}
	svc := NewCustomProviderService(f.db, f.store, counter)
	if _, err := svc.CreateProvider(context.Background(), customCreateInput("x")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if counter.calls != 1 {
		t.Fatalf("expected one reload after write, got %d", counter.calls)
	}
	if err := svc.DeleteProvider(context.Background(), 1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if counter.calls != 2 {
		t.Fatalf("expected reload after delete, got %d", counter.calls)
	}
}

type countingRefresher struct{ calls int }

func (c *countingRefresher) ReloadLimits(context.Context) { c.calls++ }

func float64Ptr(v float64) *float64 { return &v }

// 确认 catalog 导出的能力判定在本层用到的 endpoint 上确实生效。
func TestCustomProviderEndpointCapabilitiesAreWired(t *testing.T) {
	if !providers.CustomEndpointEndImageCapable("v2-video-generations") {
		t.Fatal("expected end image capability")
	}
	if providers.CustomEndpointEndImageCapable("openai-video") {
		t.Fatal("unexpected end image capability")
	}
	if len(providers.CustomEndpointImageCapabilities("openai-images-generations")) != 1 {
		t.Fatal("expected a single image capability slot")
	}
}
