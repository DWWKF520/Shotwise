package pricing

import (
	"testing"

	"github.com/shotwise/shotwise/internal/provider"
)

// TestBuiltinCatalogMatchesProviderRegistry 锁定定价目录与 internal/provider 注册表的模型集合：
// 目录漏登记会让该模型按兜底费率计价，多登记或媒体类型/默认模型漂移会让回落链走错，故逐项相等。
func TestBuiltinCatalogMatchesProviderRegistry(t *testing.T) {
	catalog := BuiltinCatalog()
	providerIDs := provider.ProviderIDs()

	if len(catalog) != len(providerIDs) {
		t.Errorf("定价目录 provider 数 = %d, 期望 %d", len(catalog), len(providerIDs))
	}
	for providerID := range catalog {
		if _, ok := provider.ProviderMetaFor(providerID); !ok {
			t.Errorf("定价目录多登记了 provider %q", providerID)
		}
	}

	for _, providerID := range providerIDs {
		meta, ok := provider.ProviderMetaFor(providerID)
		if !ok {
			t.Fatalf("provider %q 未登记", providerID)
		}
		catalogMeta, ok := catalog.Provider(providerID)
		if !ok {
			t.Errorf("定价目录缺少 provider %q", providerID)
			continue
		}

		for _, entry := range meta.Models {
			info, ok := catalogMeta.Models[entry.ID]
			if !ok {
				t.Errorf("定价目录缺少 %s/%s", providerID, entry.ID)
				continue
			}
			if info.MediaType != string(entry.Info.MediaType) {
				t.Errorf("%s/%s 媒体类型 = %q, 期望 %q",
					providerID, entry.ID, info.MediaType, entry.Info.MediaType)
			}
		}
		for modelID := range catalogMeta.Models {
			if _, ok := meta.Model(modelID); !ok {
				t.Errorf("定价目录多登记了 %s/%s", providerID, modelID)
			}
		}

		// 默认模型逐媒体类型对齐：目录缺默认会让未知模型落到 Gemini 兜底而非该供应商默认费率。
		for _, media := range meta.MediaTypes() {
			wantModel, wantOK := provider.DefaultModelForProvider(providerID, media)
			gotModel, gotOK := catalogMeta.DefaultModel[string(media)]
			if gotOK != wantOK || gotModel != wantModel {
				t.Errorf("%s 默认模型(%s) = (%q, %v), 期望 (%q, %v)",
					providerID, media, gotModel, gotOK, wantModel, wantOK)
			}
		}
	}
}

// verifier 断言一条命中定价的某个费率面向；内置声明都是单模型表，故键统一取 p.DefaultModel。
type verifier func(t *testing.T, p Pricing)

func wantToken(input, output float64) verifier {
	return func(t *testing.T, p Pricing) {
		t.Helper()
		rate, ok := p.TokenRates[p.DefaultModel]
		if !ok {
			t.Fatalf("token 费率表缺少模型 %q", p.DefaultModel)
		}
		if rate["input"] != input || rate["output"] != output {
			t.Errorf("token 费率 = %v, 期望 input %v / output %v", rate, input, output)
		}
	}
}

func wantFlat(perUnit float64) verifier {
	return func(t *testing.T, p Pricing) {
		t.Helper()
		if got := p.FlatRates[p.DefaultModel]; got != perUnit {
			t.Errorf("单费率 = %v, 期望 %v", got, perUnit)
		}
	}
}

func wantResolutionRate(resolution string, want float64) verifier {
	return func(t *testing.T, p Pricing) {
		t.Helper()
		rate, ok := p.ResolutionRates[p.DefaultModel]
		if !ok {
			t.Fatalf("分辨率费率表缺少模型 %q", p.DefaultModel)
		}
		if got := rate[resolution]; got != want {
			t.Errorf("分辨率 %s 单价 = %v, 期望 %v", resolution, got, want)
		}
	}
}

func wantMatrixRate(key MatrixKey, want float64) verifier {
	return func(t *testing.T, p Pricing) {
		t.Helper()
		rate, ok := p.MatrixRates[p.DefaultModel]
		if !ok {
			t.Fatalf("秒费率矩阵缺少模型 %q", p.DefaultModel)
		}
		if got := rate[key]; got != want {
			t.Errorf("秒费率 %+v = %v, 期望 %v", key, got, want)
		}
	}
}

func wantTierRate(key TierKey, want float64) verifier {
	return func(t *testing.T, p Pricing) {
		t.Helper()
		rate, ok := p.TieredRates[p.DefaultModel]
		if !ok {
			t.Fatalf("档位费率表缺少模型 %q", p.DefaultModel)
		}
		if got := rate[key]; got != want {
			t.Errorf("档位费率 %+v = %v, 期望 %v", key, got, want)
		}
	}
}

func wantBucketRate(key BucketKey, want float64) verifier {
	return func(t *testing.T, p Pricing) {
		t.Helper()
		rate, ok := p.BucketRates[p.DefaultModel]
		if !ok {
			t.Fatalf("离散档费率表缺少模型 %q", p.DefaultModel)
		}
		if got := rate[key]; got != want {
			t.Errorf("离散档费率 %+v = %v, 期望 %v", key, got, want)
		}
	}
}

func wantDefaultResolution(resolution string) verifier {
	return func(t *testing.T, p Pricing) {
		t.Helper()
		if p.DefaultResolution != resolution {
			t.Errorf("默认分辨率 = %q, 期望 %q", p.DefaultResolution, resolution)
		}
	}
}

func wantOpenAITokenRate(key string, want float64) verifier {
	return func(t *testing.T, p Pricing) {
		t.Helper()
		rate, ok := p.TokenRates[p.DefaultModel]
		if !ok {
			t.Fatalf("token 费率表缺少模型 %q", p.DefaultModel)
		}
		if got := rate[key]; got != want {
			t.Errorf("openai token 费率 %s = %v, 期望 %v", key, got, want)
		}
	}
}

func wantOpenAIFallbackRate(key QualitySizeKey, want float64) verifier {
	return func(t *testing.T, p Pricing) {
		t.Helper()
		rate, ok := p.FallbackRates[p.DefaultModel]
		if !ok {
			t.Fatalf("openai 兜底费率表缺少模型 %q", p.DefaultModel)
		}
		if got := rate[key]; got != want {
			t.Errorf("openai 兜底费率 %+v = %v, 期望 %v", key, got, want)
		}
	}
}

// TestBuiltinCatalogLookupRates 逐供应商对拍代表性模型的声明：kind / 币种 / 费率数值均取自
// Python 侧 lib/config/registry.py 的 pricing 声明，迁移前后不得漂移。
func TestBuiltinCatalogLookupRates(t *testing.T) {
	catalog := BuiltinCatalog()

	vectors := []struct {
		name         string
		provider     string
		model        string
		callType     string
		wantKind     Kind
		wantCurrency string
		verify       []verifier
	}{
		// --- gemini-aistudio（USD）---
		{"aistudio 文本 pro", "gemini-aistudio", "gemini-3.1-pro-preview", "text", KindPerToken, "USD",
			[]verifier{wantToken(2.00, 12.00)}},
		{"aistudio 文本 flash-lite", "gemini-aistudio", "gemini-3.1-flash-lite-preview", "text", KindPerToken, "USD",
			[]verifier{wantToken(0.25, 1.50)}},
		{"aistudio 图片 flash-image", "gemini-aistudio", "gemini-3.1-flash-image-preview", "image", KindPerImageByResolution, "USD",
			[]verifier{wantResolutionRate("512PX", 0.045), wantResolutionRate("1K", 0.067), wantResolutionRate("4K", 0.151)}},
		{"aistudio 图片 pro", "gemini-aistudio", "gemini-3-pro-image-preview", "image", KindPerImageByResolution, "USD",
			[]verifier{wantResolutionRate("1K", 0.134), wantResolutionRate("4K", 0.24)}},
		{"aistudio 视频 standard", "gemini-aistudio", "veo-3.1-generate-preview", "video", KindPerSecondMatrix, "USD",
			[]verifier{
				wantMatrixRate(MatrixKey{Resolution: "1080p", Audio: AudioOn}, 0.40),
				wantMatrixRate(MatrixKey{Resolution: "4k", Audio: AudioOff}, 0.40),
			}},
		{"aistudio 视频 fast", "gemini-aistudio", "veo-3.1-fast-generate-preview", "video", KindPerSecondMatrix, "USD",
			[]verifier{wantMatrixRate(MatrixKey{Resolution: "720p", Audio: AudioOff}, 0.08)}},
		{"aistudio 视频 lite", "gemini-aistudio", "veo-3.1-lite-generate-preview", "video", KindPerSecondMatrix, "USD",
			[]verifier{
				wantMatrixRate(MatrixKey{Resolution: "1080p", Audio: AudioOn}, 0.08),
				wantMatrixRate(MatrixKey{Resolution: "1080p", Audio: AudioOff}, 0.05),
				wantMatrixRate(MatrixKey{Resolution: "720p", Audio: AudioOff}, 0.03),
			}},

		// --- gemini-vertex（USD）---
		{"vertex 文本 flash", "gemini-vertex", "gemini-3-flash-preview", "text", KindPerToken, "USD",
			[]verifier{wantToken(0.50, 3.00)}},
		{"vertex 视频 standard-001", "gemini-vertex", "veo-3.1-generate-001", "video", KindPerSecondMatrix, "USD",
			[]verifier{wantMatrixRate(MatrixKey{Resolution: "4k", Audio: AudioOn}, 0.60)}},
		{"vertex 视频 fast-001", "gemini-vertex", "veo-3.1-fast-generate-001", "video", KindPerSecondMatrix, "USD",
			[]verifier{wantMatrixRate(MatrixKey{Resolution: "4k", Audio: AudioOff}, 0.25)}},

		// --- ark（CNY）---
		{"ark 文本 pro", "ark", "doubao-seed-2-0-pro-260215", "text", KindPerToken, "CNY",
			[]verifier{wantToken(3.20, 16.00)}},
		{"ark 图片 seedream-4.5", "ark", "doubao-seedream-4-5-251128", "image", KindPerImageFlat, "CNY",
			[]verifier{wantFlat(0.25)}},
		{"ark 视频 seedance-1.5-pro", "ark", "doubao-seedance-1-5-pro-251215", "video", KindPerTokenVideo, "CNY",
			[]verifier{
				wantTierRate(TierKey{Tier: "default", Audio: true}, 16.00),
				wantTierRate(TierKey{Tier: "default", Audio: false}, 8.00),
				wantTierRate(TierKey{Tier: "flex", Audio: false}, 4.00),
			}},
		{"ark 视频 seedance-2.5", "ark", "doubao-seedance-2-5-260628", "video", KindPerTokenVideo, "CNY",
			[]verifier{wantTierRate(TierKey{Tier: "default", Audio: true}, 70.00)}},

		// --- ark-agent-plan：无独立费率表，按 Gemini 通用默认兜底 ---
		{"agent-plan 文本回落 gemini 默认", "ark-agent-plan", "doubao-seed-2.0-lite", "text", KindPerToken, "USD",
			[]verifier{wantToken(0.50, 3.00)}},
		{"agent-plan 视频回落 gemini 默认", "ark-agent-plan", "doubao-seedance-2.0", "video", KindPerSecondMatrix, "USD",
			[]verifier{wantMatrixRate(MatrixKey{Resolution: "1080p", Audio: AudioOn}, 0.08)}},

		// --- grok（USD）---
		{"grok 文本 fast-non-reasoning", "grok", "grok-4-1-fast-non-reasoning", "text", KindPerToken, "USD",
			[]verifier{wantToken(0.20, 0.50)}},
		{"grok 图片 pro", "grok", "grok-imagine-image-pro", "image", KindPerImageFlat, "USD",
			[]verifier{wantFlat(0.07)}},
		{"grok 视频 flat", "grok", "grok-imagine-video", "video", KindPerSecondMatrix, "USD",
			[]verifier{wantMatrixRate(MatrixKey{}, 0.050)}},

		// --- openai（USD）---
		{"openai 文本 gpt-5.5", "openai", "gpt-5.5", "text", KindPerToken, "USD",
			[]verifier{wantToken(5.00, 30.00)}},
		{"openai 图片 gpt-image-2", "openai", "gpt-image-2", "image", KindPerImageOpenAIToken, "USD",
			[]verifier{
				wantOpenAITokenRate("image_in", 8.0),
				wantOpenAITokenRate("image_out", 30.0),
				wantOpenAITokenRate("text_out", 0.0),
				wantOpenAIFallbackRate(QualitySizeKey{Quality: "medium", Size: "1024x1792"}, 0.106),
				wantOpenAIFallbackRate(QualitySizeKey{Quality: "high", Size: "1792x1024"}, 0.317),
			}},
		{"openai 视频 sora-2-pro", "openai", "sora-2-pro", "video", KindPerSecondMatrix, "USD",
			[]verifier{
				wantMatrixRate(MatrixKey{Resolution: "720p", Audio: AudioUnspecified}, 0.30),
				wantMatrixRate(MatrixKey{Resolution: "1024p", Audio: AudioUnspecified}, 0.50),
				wantMatrixRate(MatrixKey{Resolution: "1080p", Audio: AudioUnspecified}, 0.70),
			}},

		// --- vidu：provider 级委托标记 ---
		{"vidu 图片委托", "vidu", "viduq2", "image", KindViduDelegate, "CNY", nil},
		{"vidu 视频委托", "vidu", "viduq3-pro", "video", KindViduDelegate, "CNY", nil},

		// --- dashscope（CNY）---
		{"dashscope 文本 qwen3.7-max", "dashscope", "qwen3.7-max", "text", KindPerToken, "CNY",
			[]verifier{wantToken(12.0, 36.0)}},
		{"dashscope 图片 wan2.7-image-pro", "dashscope", "wan2.7-image-pro", "image", KindPerImageFlat, "CNY",
			[]verifier{wantFlat(0.5)}},
		{"dashscope 视频 happyhorse-1.1-i2v", "dashscope", "happyhorse-1.1-i2v", "video", KindPerSecondMatrix, "CNY",
			[]verifier{wantMatrixRate(MatrixKey{Resolution: "1080p", Audio: AudioUnspecified}, 1.2)}},
		{"dashscope 视频 wan3.0", "dashscope", "wan3.0-video", "video", KindPerSecondMatrix, "CNY",
			[]verifier{wantMatrixRate(MatrixKey{Resolution: "480p", Audio: AudioUnspecified}, 0.3)}},
		{"dashscope 语音按字符", "dashscope", "qwen3-tts-flash", "audio", KindPerCharacter, "CNY",
			[]verifier{wantFlat(0.8)}},

		// --- minimax（CNY）---
		{"minimax 文本 M2.7", "minimax", "MiniMax-M2.7", "text", KindPerToken, "CNY",
			[]verifier{wantToken(2.1, 8.4)}},
		{"minimax 图片 image-01", "minimax", "image-01", "image", KindPerImageFlat, "CNY",
			[]verifier{wantFlat(0.025)}},
		{"minimax 视频 H3", "minimax", "MiniMax-H3", "video", KindPerSecondMatrix, "CNY",
			[]verifier{
				wantMatrixRate(MatrixKey{Resolution: "768p", Audio: AudioUnspecified}, 0.50),
				wantMatrixRate(MatrixKey{Resolution: "2k", Audio: AudioUnspecified}, 0.80),
				wantDefaultResolution("768p"),
			}},
		{"minimax 视频海螺 2.3", "minimax", "MiniMax-Hailuo-2.3", "video", KindPerVideoBucket, "CNY",
			[]verifier{
				wantBucketRate(BucketKey{Resolution: "768p", DurationSeconds: 6}, 2.0),
				wantBucketRate(BucketKey{Resolution: "768p", DurationSeconds: 10}, 4.0),
				wantBucketRate(BucketKey{Resolution: "1080p", DurationSeconds: 6}, 3.5),
			}},
		{"minimax 视频 S2V-01", "minimax", "S2V-01", "video", KindPerVideoBucket, "CNY",
			[]verifier{wantBucketRate(BucketKey{Resolution: "768p", DurationSeconds: 6}, 3.0)}},

		// --- kling（CNY）---
		{"kling 视频 v3", "kling", "kling-v3", "video", KindPerSecondTiered, "CNY",
			[]verifier{
				wantTierRate(TierKey{Tier: "std", Audio: false}, 0.6),
				wantTierRate(TierKey{Tier: "pro", Audio: true}, 1.0),
				wantTierRate(TierKey{Tier: "4k", Audio: false}, 3.0),
			}},
		{"kling 视频 video-o1", "kling", "kling-video-o1", "video", KindPerSecondTiered, "CNY",
			[]verifier{wantTierRate(TierKey{Tier: "std", Audio: false}, 0.6)}},
		{"kling 图片 o1", "kling", "kling-image-o1", "image", KindPerImageFlat, "CNY",
			[]verifier{wantFlat(0.2)}},
		{"kling 图片 v3-omni", "kling", "kling-v3-omni-image", "image", KindPerImageByResolution, "CNY",
			[]verifier{wantResolutionRate("1K", 0.2), wantResolutionRate("4K", 0.4)}},

		// --- agnes（USD）---
		{"agnes 文本 2.0", "agnes", "agnes-2.0-flash", "text", KindPerToken, "USD",
			[]verifier{wantToken(0.03, 0.15)}},
		{"agnes 图片 2.1", "agnes", "agnes-image-2.1-flash", "image", KindPerImageFlat, "USD",
			[]verifier{wantFlat(0.003)}},
		{"agnes 视频 flash", "agnes", "agnes-video-2.5-flash", "video", KindPerSecondMatrix, "USD",
			[]verifier{wantMatrixRate(MatrixKey{}, 0.005)}},
	}

	for _, vector := range vectors {
		t.Run(vector.name, func(t *testing.T) {
			p, err := LookupPricing(catalog, vector.provider, vector.model, vector.callType)
			if err != nil {
				t.Fatalf("LookupPricing 报错: %v", err)
			}
			if p.Kind != vector.wantKind {
				t.Fatalf("kind = %q, 期望 %q", p.Kind, vector.wantKind)
			}
			if p.Currency != vector.wantCurrency {
				t.Fatalf("币种 = %q, 期望 %q", p.Currency, vector.wantCurrency)
			}
			for _, check := range vector.verify {
				check(t, p)
			}
		})
	}
}

// TestBuiltinCatalogCalculatorProductionWiring 验证生产构造入口可用：Calculator 接 BuiltinCatalog
// 后端到端结算各计费形状（文本 / 图片 / 按 token 视频 / 按档秒价视频 / 按字符语音）。
func TestBuiltinCatalogCalculatorProductionWiring(t *testing.T) {
	calc := NewCalculator(BuiltinCatalog())

	cases := []struct {
		name     string
		provider string
		params   Params
		want     float64
		currency string
	}{
		{"gemini 默认图片", "gemini-aistudio", Params{CallType: "image"}, 0.067, "USD"},
		{"anthropic 文本兜底表", "anthropic", Params{CallType: "text", Model: "claude-sonnet-4",
			InputTokens: IntPtr(1_000_000)}, 3.00, "USD"},
		{"ark 视频按 token", "ark", Params{CallType: "video", Model: "doubao-seedance-2-0-mini-260615",
			UsageTokens: IntPtr(1_000_000)}, 23.00, "CNY"},
		{"kling 视频档位秒价", "kling", Params{CallType: "video", Model: "kling-v3", DurationSeconds: IntPtr(5),
			Resolution: "720p", ServiceTier: "pro", GenerateAudio: BoolPtr(true)}, 5.00, "CNY"},
		{"dashscope 语音按字符", "dashscope", Params{CallType: "audio", Model: "qwen3-tts-flash",
			UsageTokens: IntPtr(10_000)}, 0.80, "CNY"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amount, currency, err := calc.CalculateCost(tc.provider, tc.params, CostOptions{})
			if err != nil {
				t.Fatalf("CalculateCost 报错: %v", err)
			}
			if currency != tc.currency {
				t.Errorf("币种 = %q, 期望 %q", currency, tc.currency)
			}
			checkAmount(t, amount, tc.want, tc.name)
		})
	}
}
