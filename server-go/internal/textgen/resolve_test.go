package textgen

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/migrations"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "textgen.db"), logger.Silent)
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
	return gdb
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

// seedGeminiCredential 写入 gemini-aistudio 的活跃凭证（api_key + 可选 base_url）。
func seedGeminiCredential(t *testing.T, gdb *gorm.DB, baseURL string) {
	t.Helper()
	if err := gdb.Exec(
		"INSERT INTO provider_credential (provider, name, api_key, base_url, is_active, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?)",
		providerGeminiAistudio, "default", "k-test", baseURL, true,
		"2026-01-01T00:00:00+00:00", "2026-01-01T00:00:00+00:00",
	).Error; err != nil {
		t.Fatalf("seed credential: %v", err)
	}
}

func TestResolveTarget(t *testing.T) {
	cases := []struct {
		name         string
		settings     map[string]string
		project      *domain.ProjectFile
		task         TaskType
		wantProvider string
		wantModel    string
		wantErr      error
	}{
		{
			name: "无任何配置时回落兜底常量", task: TaskScript,
			wantProvider: "gemini-aistudio", wantModel: "gemini-3-flash-preview",
		},
		{
			name:     "全局复杂档生效",
			settings: map[string]string{"text_backend_complex": "gemini-aistudio/g-complex"},
			task:     TaskScript, wantProvider: "gemini-aistudio", wantModel: "g-complex",
		},
		{
			name: "简单任务读简单档而非复杂档",
			settings: map[string]string{
				"text_backend_complex": "gemini-aistudio/g-complex",
				"text_backend_simple":  "gemini-aistudio/g-simple",
			},
			task: TaskOverview, wantProvider: "gemini-aistudio", wantModel: "g-simple",
		},
		{
			name:     "档位缺省时回落全局默认",
			settings: map[string]string{"default_text_backend": "gemini-aistudio/g-def"},
			task:     TaskScript, wantProvider: "gemini-aistudio", wantModel: "g-def",
		},
		{
			name:     "项目档位优先于全局档位",
			settings: map[string]string{"text_backend_complex": "gemini-aistudio/g-complex"},
			project:  &domain.ProjectFile{TextBackendComplex: "gemini-aistudio/p-tier"},
			task:     TaskScript, wantProvider: "gemini-aistudio", wantModel: "p-tier",
		},
		{
			name:     "项目默认优先于全局档位",
			settings: map[string]string{"text_backend_complex": "gemini-aistudio/g-complex"},
			project:  &domain.ProjectFile{DefaultTextBackend: "gemini-aistudio/p-def"},
			task:     TaskScript, wantProvider: "gemini-aistudio", wantModel: "p-def",
		},
		{
			name:     "项目裸 provider 取文本默认模型",
			settings: map[string]string{"default_text_backend": "gemini-aistudio/g-def"},
			project:  &domain.ProjectFile{TextBackendComplex: "gemini-aistudio"},
			task:     TaskScript, wantProvider: "gemini-aistudio", wantModel: "gemini-3-flash-preview",
		},
		{
			name:     "全局层非完整形态不作数",
			settings: map[string]string{"text_backend_complex": "gemini-aistudio"},
			task:     TaskScript, wantProvider: "gemini-aistudio", wantModel: "gemini-3-flash-preview",
		},
		{
			name:     "全局解析到非 gemini 供应商报错而非换供应商",
			settings: map[string]string{"text_backend_complex": "openai/gpt-5.4-mini"},
			task:     TaskScript, wantErr: ErrUnsupportedProvider,
		},
		{
			name:     "项目层解析到非 gemini 供应商报错而非回落全局",
			settings: map[string]string{"text_backend_complex": "gemini-aistudio/g-complex"},
			project:  &domain.ProjectFile{TextBackendComplex: "dashscope/qwen-plus"},
			task:     TaskScript, wantErr: ErrUnsupportedProvider,
		},
		{
			name:     "供应商被禁用直接报错",
			settings: map[string]string{"provider_enabled:gemini-aistudio": "0"},
			task:     TaskScript, wantErr: config.ErrProviderDisabled,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gdb := newTestDB(t)
			for k, v := range c.settings {
				setSetting(t, gdb, k, v)
			}
			target, err := resolveTarget(context.Background(), gdb, c.project, c.task)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v，期望 %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if target.ProviderID != c.wantProvider || target.ModelID != c.wantModel {
				t.Fatalf("target = %s/%s，期望 %s/%s",
					target.ProviderID, target.ModelID, c.wantProvider, c.wantModel)
			}
		})
	}
}

func TestNewGeneratorEndToEnd(t *testing.T) {
	srv, captured := newGeminiTestServer(t, 200, geminiTextBody("剧本正文", "STOP"))
	gdb := newTestDB(t)
	seedGeminiCredential(t, gdb, srv.URL)

	gen, err := NewGenerator(context.Background(), gdb, nil, TaskScript)
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	got, err := gen.Generate(context.Background(), "写个开头", nil, 0)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got != "剧本正文" {
		t.Fatalf("text = %q", got)
	}
	if captured.path != "/v1beta/models/gemini-3-flash-preview:generateContent" || captured.apiKey != "k-test" {
		t.Fatalf("path=%s key=%q", captured.path, captured.apiKey)
	}
}

func TestNewGeneratorMissingAPIKeyFailsLoud(t *testing.T) {
	gdb := newTestDB(t)
	if _, err := NewGenerator(context.Background(), gdb, nil, TaskScript); err == nil {
		t.Fatal("未配置 api_key 应报错")
	}
}
