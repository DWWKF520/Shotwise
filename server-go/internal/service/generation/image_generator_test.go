package generation

import "github.com/shotwise/shotwise/internal/service"

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/pricing"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/migrations"
)

// openAIImageStub 起一个假的 OpenAI 图片接口：回一张最小 PNG + token 用量。
func openAIImageStub(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("upstream exploded"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"b64_json": minimalPNGBase64}},
			"usage": map[string]any{
				"input_tokens":          120,
				"output_tokens":         800,
				"input_tokens_details":  map[string]any{"text_tokens": 120},
				"output_tokens_details": map[string]any{"image_tokens": 800},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// minimalPNGBase64 是 1x1 PNG 的 base64（够写入与 size>0 校验）。
const minimalPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// newImageGeneratorFixture 装配一次「解析 → 生成 → 记账」所需的全部依赖。
func newImageGeneratorFixture(t *testing.T, status int) (*ImageGenerator, *gorm.DB, string) {
	return newImageGeneratorFixtureWithStub(t, openAIImageStub(t, status))
}

// newImageGeneratorFixtureWithStub 用调用方提供的假图片端点装配，供需要定制响应行为
// （如在响应前并发改写项目状态模拟竞态）的测试复用。
func newImageGeneratorFixtureWithStub(t *testing.T, srv *httptest.Server) (*ImageGenerator, *gorm.DB, string) {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "gen.db"), logger.Silent)
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

	// api_calls.user_id 有到 users 的外键；运行时保证 default 用户存在。
	if err := gdb.Exec(
		"INSERT INTO users (id, username, role, is_active, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		repository.DefaultUserID, "default", "user", true, "2026-01-01T00:00:00+00:00", "2026-01-01T00:00:00+00:00",
	).Error; err != nil {
		t.Fatalf("seed default user: %v", err)
	}

	srvURL := srv.URL
	// 全局默认图片供应商 + 该供应商的凭证与端点。
	for key, value := range map[string]string{
		"default_image_backend": "openai/gpt-image-2",
	} {
		if err := gdb.Exec(
			"INSERT INTO system_setting (key, value, updated_at) VALUES (?, ?, ?)",
			key, value, "2026-01-01T00:00:00+00:00",
		).Error; err != nil {
			t.Fatalf("seed setting: %v", err)
		}
	}
	// provider_config 的 is_secret 标记决定该键是否回显给前端，api_key 属秘密项。
	for key, secret := range map[string]bool{"api_key": true, "base_url": false} {
		value := srvURL
		if key == "api_key" {
			value = "sk-test"
		}
		if err := gdb.Exec(
			"INSERT INTO provider_config (provider, key, value, is_secret, updated_at) VALUES (?, ?, ?, ?, ?)",
			"openai", key, value, secret, "2026-01-01T00:00:00+00:00",
		).Error; err != nil {
			t.Fatalf("seed provider config: %v", err)
		}
	}

	catalog := pricing.MapCatalog{
		"openai": {
			Models: map[string]pricing.ModelInfo{
				"gpt-image-2": {
					MediaType: "image",
					// 按 input/output token 计费：120 in + 800 out，每百万 token 10 / 40 美元。
					Pricing: ptrPricing(pricing.NewPerToken(
						map[string]map[string]float64{"gpt-image-2": {"input": 10, "output": 40}},
						"gpt-image-2", "USD",
					)),
				},
			},
			DefaultModel: map[string]string{"image": "gpt-image-2"},
		},
	}
	ledger := service.NewLedger(gdb, pricing.NewCalculator(catalog))
	return NewImageGenerator(gdb, ledger), gdb, srv.URL
}

func ptrPricing(p pricing.Pricing) *pricing.Pricing { return &p }

func TestImageGeneratorEndToEndSuccess(t *testing.T) {
	generator, gdb, _ := newImageGeneratorFixture(t, http.StatusOK)
	out := filepath.Join(t.TempDir(), "shot.png")

	res, err := generator.Generate(context.Background(), ImageGenerateParams{
		ProjectName: "demo",
		Capability:  config.ImageCapT2I,
		Request:     provider.NewImageGenerationRequest("a cat on a roof", out),
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.ImagePath != out {
		t.Fatalf("unexpected path: %s", res.ImagePath)
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() == 0 {
		t.Fatalf("output file missing or empty: %v", err)
	}

	var row repository.APICall
	if err := gdb.Where("project_name = ?", "demo").First(&row).Error; err != nil {
		t.Fatalf("api_call row missing: %v", err)
	}
	if row.Status != service.StatusSuccess {
		t.Fatalf("expected success ledger row, got %s", row.Status)
	}
	if row.Provider != "openai" || row.Model != "gpt-image-2" || row.CallType != "image" {
		t.Fatalf("unexpected ledger identity: %s/%s/%s", row.Provider, row.Model, row.CallType)
	}
	if row.OutputPath == nil || *row.OutputPath != out {
		t.Fatalf("ledger output_path not recorded: %v", row.OutputPath)
	}
	// 120 input + 800 output token @ 10/40 per million = 0.0012 + 0.032 = 0.0332。
	if row.CostAmount < 0.0331 || row.CostAmount > 0.0333 {
		t.Fatalf("unexpected cost: %f", row.CostAmount)
	}
	if row.InputTokens == nil || *row.InputTokens != 120 {
		t.Fatalf("input tokens not settled: %v", row.InputTokens)
	}
	if row.OutputTokens == nil || *row.OutputTokens != 800 {
		t.Fatalf("output tokens not settled: %v", row.OutputTokens)
	}
	if row.DurationMs == nil || *row.DurationMs < 0 {
		t.Fatalf("duration not settled: %v", row.DurationMs)
	}
	if row.FinishedAt.IsZero() {
		t.Fatal("finished_at not stamped")
	}
}

func TestImageGeneratorFailureSettlesZeroCost(t *testing.T) {
	generator, gdb, _ := newImageGeneratorFixture(t, http.StatusInternalServerError)
	out := filepath.Join(t.TempDir(), "shot.png")

	_, err := generator.Generate(context.Background(), ImageGenerateParams{
		ProjectName: "demo",
		Capability:  config.ImageCapT2I,
		Request:     provider.NewImageGenerationRequest("a cat on a roof", out),
	})
	if err == nil {
		t.Fatal("upstream 5xx should surface as error")
	}

	var row repository.APICall
	if err := gdb.Where("project_name = ?", "demo").First(&row).Error; err != nil {
		t.Fatalf("api_call row missing: %v", err)
	}
	if row.Status != service.StatusFailed {
		t.Fatalf("expected failed ledger row, got %s", row.Status)
	}
	if row.CostAmount != 0 {
		t.Fatalf("failed call must settle zero cost, got %f", row.CostAmount)
	}
	if row.ErrorMessage == nil || *row.ErrorMessage == "" {
		t.Fatal("error_message should be recorded")
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("no output file should be produced on failure")
	}
}

func TestImageGeneratorDisabledProviderLeavesNoLedgerRow(t *testing.T) {
	generator, gdb, _ := newImageGeneratorFixture(t, http.StatusOK)
	if err := gdb.Exec(
		"INSERT INTO system_setting (key, value, updated_at) VALUES (?, ?, ?) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		"provider_enabled:openai", "0", "2026-01-01T00:00:00+00:00",
	).Error; err != nil {
		t.Fatalf("disable provider: %v", err)
	}

	_, err := generator.Generate(context.Background(), ImageGenerateParams{
		ProjectName: "demo",
		Capability:  config.ImageCapT2I,
		Request:     provider.NewImageGenerationRequest("a cat", filepath.Join(t.TempDir(), "shot.png")),
	})
	if err == nil {
		t.Fatal("disabled provider should fail before the ledger bracket opens")
	}
	var count int64
	if err := gdb.Model(&repository.APICall{}).Where("project_name = ?", "demo").Count(&count).Error; err != nil {
		t.Fatalf("count api_calls: %v", err)
	}
	if count != 0 {
		t.Fatalf("no api_call row should be written, got %d", count)
	}
}

func TestMinimalPNGBase64IsDecodable(t *testing.T) {
	// 兜住测试夹具本身：base64 若损坏，上面所有落盘断言都会以「文件为空」的形式误导排查。
	raw, err := base64.StdEncoding.DecodeString(minimalPNGBase64)
	if err != nil || len(raw) == 0 || raw[0] != 0x89 {
		t.Fatalf("fixture png is not a valid PNG: %v", err)
	}
}
