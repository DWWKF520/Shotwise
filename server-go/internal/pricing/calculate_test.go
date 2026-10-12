package pricing

import (
	"testing"
	"time"
)

// TestCalculateCostReferenceVectors 是统一入口的费用对拍：金额与币种逐项锁迁移前行为。
// 向量来自 tests/test_cost_calculator.py、tests/test_cost_calculator_text.py 与 tests/test_vidu_cost.py。
func TestCalculateCostReferenceVectors(t *testing.T) {
	calc := NewCalculator(testCatalog())

	type vector struct {
		name     string
		provider string
		params   Params
		opts     CostOptions
		want     float64
		currency string
	}
	vectors := []vector{
		// --- Anthropic 文本（tests/test_cost_calculator.py::TestAnthropicTextCost）---
		{"anthropic sonnet-4", "anthropic", Params{CallType: "text", Model: "claude-sonnet-4",
			InputTokens: IntPtr(100_000), OutputTokens: IntPtr(50_000)}, CostOptions{}, 1.05, "USD"},
		{"anthropic 未知模型回落默认", "anthropic", Params{CallType: "text", Model: "unknown-claude",
			InputTokens: IntPtr(100_000), OutputTokens: IntPtr(50_000)}, CostOptions{}, 1.05, "USD"},
		{"anthropic haiku", "anthropic", Params{CallType: "text", Model: "claude-haiku-4-5",
			InputTokens: IntPtr(100_000), OutputTokens: IntPtr(50_000)}, CostOptions{}, 0.35, "USD"},

		// --- Gemini 图片（TestImageCost）---
		{"gemini 图片 1k", "gemini-aistudio", Params{CallType: "image", Resolution: "1k"}, CostOptions{}, 0.067, "USD"},
		{"gemini 图片 2K", "gemini-aistudio", Params{CallType: "image", Resolution: "2K"}, CostOptions{}, 0.101, "USD"},
		{"gemini 图片 4K", "gemini-aistudio", Params{CallType: "image", Resolution: "4K"}, CostOptions{}, 0.151, "USD"},
		{"gemini 图片未知分辨率落 1K", "gemini-aistudio", Params{CallType: "image", Resolution: "unknown"}, CostOptions{}, 0.067, "USD"},
		{"gemini pro 图片 1k", "gemini-aistudio", Params{CallType: "image", Model: "gemini-3-pro-image-preview", Resolution: "1k"}, CostOptions{}, 0.134, "USD"},
		{"gemini pro 图片 2K", "gemini-aistudio", Params{CallType: "image", Model: "gemini-3-pro-image-preview", Resolution: "2K"}, CostOptions{}, 0.134, "USD"},

		// --- Gemini 视频（TestVideoCost）---
		{"gemini lite 1080p 有声", "gemini-aistudio", Params{CallType: "video", DurationSeconds: IntPtr(8),
			Resolution: "1080p", GenerateAudio: BoolPtr(true)}, CostOptions{}, 0.64, "USD"},
		{"gemini lite 1080p 无声", "gemini-aistudio", Params{CallType: "video", DurationSeconds: IntPtr(8),
			Resolution: "1080p", GenerateAudio: BoolPtr(false)}, CostOptions{}, 0.40, "USD"},
		{"gemini lite 720p 有声", "gemini-aistudio", Params{CallType: "video", DurationSeconds: IntPtr(8),
			Resolution: "720p", GenerateAudio: BoolPtr(true)}, CostOptions{}, 0.40, "USD"},
		{"gemini lite 720p 无声", "gemini-aistudio", Params{CallType: "video", DurationSeconds: IntPtr(8),
			Resolution: "720p", GenerateAudio: BoolPtr(false)}, CostOptions{}, 0.24, "USD"},
		{"gemini lite 未知分辨率回落 1080p 有声", "gemini-aistudio", Params{CallType: "video", DurationSeconds: IntPtr(5),
			Resolution: "unknown", GenerateAudio: BoolPtr(true)}, CostOptions{}, 0.40, "USD"},
		{"vertex fast 1080p 有声", "gemini-vertex", Params{CallType: "video", Model: "veo-3.1-fast-generate-001",
			DurationSeconds: IntPtr(8), Resolution: "1080p", GenerateAudio: BoolPtr(true)}, CostOptions{}, 0.96, "USD"},
		{"vertex fast 1080p 无声", "gemini-vertex", Params{CallType: "video", Model: "veo-3.1-fast-generate-001",
			DurationSeconds: IntPtr(8), Resolution: "1080p", GenerateAudio: BoolPtr(false)}, CostOptions{}, 0.80, "USD"},
		{"vertex fast 720p 有声", "gemini-vertex", Params{CallType: "video", Model: "veo-3.1-fast-generate-001",
			DurationSeconds: IntPtr(8), Resolution: "720p", GenerateAudio: BoolPtr(true)}, CostOptions{}, 0.80, "USD"},
		{"vertex fast 720p 无声", "gemini-vertex", Params{CallType: "video", Model: "veo-3.1-fast-generate-001",
			DurationSeconds: IntPtr(8), Resolution: "720p", GenerateAudio: BoolPtr(false)}, CostOptions{}, 0.64, "USD"},
		{"vertex fast 未知分辨率回落自身费率", "gemini-vertex", Params{CallType: "video", Model: "veo-3.1-fast-generate-001",
			DurationSeconds: IntPtr(5), Resolution: "unknown", GenerateAudio: BoolPtr(true)}, CostOptions{}, 0.60, "USD"},
		{"standard 4k 有声", "gemini-aistudio", Params{CallType: "video", Model: "veo-3.1-generate-preview",
			DurationSeconds: IntPtr(8), Resolution: "4k", GenerateAudio: BoolPtr(true)}, CostOptions{}, 4.80, "USD"},
		{"standard 4k 无声", "gemini-aistudio", Params{CallType: "video", Model: "veo-3.1-generate-preview",
			DurationSeconds: IntPtr(8), Resolution: "4k", GenerateAudio: BoolPtr(false)}, CostOptions{}, 3.20, "USD"},
		{"fast preview 4k 有声", "gemini-aistudio", Params{CallType: "video", Model: "veo-3.1-fast-generate-preview",
			DurationSeconds: IntPtr(8), Resolution: "4k", GenerateAudio: BoolPtr(true)}, CostOptions{}, 2.40, "USD"},
		{"fast preview 4k 无声", "gemini-aistudio", Params{CallType: "video", Model: "veo-3.1-fast-generate-preview",
			DurationSeconds: IntPtr(8), Resolution: "4k", GenerateAudio: BoolPtr(false)}, CostOptions{}, 2.00, "USD"},
		{"standard preview 1080p 有声", "gemini-aistudio", Params{CallType: "video", Model: "veo-3.1-generate-preview",
			DurationSeconds: IntPtr(8), Resolution: "1080p", GenerateAudio: BoolPtr(true)}, CostOptions{}, 3.20, "USD"},
		{"standard preview 1080p 无声", "gemini-aistudio", Params{CallType: "video", Model: "veo-3.1-generate-preview",
			DurationSeconds: IntPtr(8), Resolution: "1080p", GenerateAudio: BoolPtr(false)}, CostOptions{}, 1.60, "USD"},
		{"fast preview 1080p 有声", "gemini-aistudio", Params{CallType: "video", Model: "veo-3.1-fast-generate-preview",
			DurationSeconds: IntPtr(8), Resolution: "1080p", GenerateAudio: BoolPtr(true)}, CostOptions{}, 0.96, "USD"},

		// --- MiniMax H3（TestVideoCost::test_minimax_h3_resolution_omitted_settles_at_768p）---
		{"minimax H3 空分辨率落 768p", "minimax", Params{CallType: "video", Model: "MiniMax-H3",
			DurationSeconds: IntPtr(8)}, CostOptions{}, 4.0, "CNY"},
		{"minimax H3 2k", "minimax", Params{CallType: "video", Model: "MiniMax-H3",
			DurationSeconds: IntPtr(8), Resolution: "2k"}, CostOptions{}, 6.4, "CNY"},

		// --- Kling（TestKlingVideoCost）---
		{"kling std 无声", "kling", Params{CallType: "video", Model: "kling-v2-5-turbo",
			ServiceTier: "std", GenerateAudio: BoolPtr(false), DurationSeconds: IntPtr(5)}, CostOptions{}, 3.0, "CNY"},
		{"kling pro 有声", "kling", Params{CallType: "video", Model: "kling-v2-5-turbo",
			ServiceTier: "pro", GenerateAudio: BoolPtr(true), DurationSeconds: IntPtr(5)}, CostOptions{}, 5.0, "CNY"},
		{"kling default 归一到 std", "kling", Params{CallType: "video", Model: "kling-v2-5-turbo",
			GenerateAudio: BoolPtr(false), DurationSeconds: IntPtr(10)}, CostOptions{}, 6.0, "CNY"},
		{"kling 未知模型回落 turbo", "kling", Params{CallType: "video", Model: "kling-mystery",
			ServiceTier: "pro", GenerateAudio: BoolPtr(false), DurationSeconds: IntPtr(5)}, CostOptions{}, 4.0, "CNY"},

		// --- Ark 视频（TestArkVideoCost）---
		{"ark 1.5-pro 在线有声", "ark", Params{CallType: "video", Model: "doubao-seedance-1-5-pro-251215",
			UsageTokens: IntPtr(246_840), ServiceTier: "default", GenerateAudio: BoolPtr(true)}, CostOptions{}, 3.94944, "CNY"},
		{"ark 1.5-pro 在线无声", "ark", Params{CallType: "video", Model: "doubao-seedance-1-5-pro-251215",
			UsageTokens: IntPtr(246_840), ServiceTier: "default", GenerateAudio: BoolPtr(false)}, CostOptions{}, 1.97472, "CNY"},
		{"ark 1.5-pro flex 有声", "ark", Params{CallType: "video", Model: "doubao-seedance-1-5-pro-251215",
			UsageTokens: IntPtr(246_840), ServiceTier: "flex", GenerateAudio: BoolPtr(true)}, CostOptions{}, 1.97472, "CNY"},
		{"ark 1.5-pro flex 无声", "ark", Params{CallType: "video", Model: "doubao-seedance-1-5-pro-251215",
			UsageTokens: IntPtr(246_840), ServiceTier: "flex", GenerateAudio: BoolPtr(false)}, CostOptions{}, 0.98736, "CNY"},
		{"ark 零 token", "ark", Params{CallType: "video", UsageTokens: IntPtr(0),
			ServiceTier: "default", GenerateAudio: BoolPtr(true)}, CostOptions{}, 0.0, "CNY"},
		{"ark 未知模型回落 mini", "ark", Params{CallType: "video", Model: "unknown-model",
			UsageTokens: IntPtr(1_000_000), ServiceTier: "default", GenerateAudio: BoolPtr(true)}, CostOptions{}, 23.0, "CNY"},
		{"ark seedance-2.0", "ark", Params{CallType: "video", Model: "doubao-seedance-2-0-260128",
			UsageTokens: IntPtr(1_000_000), ServiceTier: "default", GenerateAudio: BoolPtr(true)}, CostOptions{}, 46.0, "CNY"},
		{"ark seedance-2.0 无声同价", "ark", Params{CallType: "video", Model: "doubao-seedance-2-0-260128",
			UsageTokens: IntPtr(1_000_000), ServiceTier: "default", GenerateAudio: BoolPtr(false)}, CostOptions{}, 46.0, "CNY"},
		{"ark seedance-2.0-fast", "ark", Params{CallType: "video", Model: "doubao-seedance-2-0-fast-260128",
			UsageTokens: IntPtr(1_000_000), ServiceTier: "default", GenerateAudio: BoolPtr(true)}, CostOptions{}, 37.0, "CNY"},
		{"ark seedance-2.0-mini", "ark", Params{CallType: "video", Model: "doubao-seedance-2-0-mini-260615",
			UsageTokens: IntPtr(1_000_000), ServiceTier: "default", GenerateAudio: BoolPtr(true)}, CostOptions{}, 23.0, "CNY"},

		// --- Grok 视频（TestGrokVideoCost）---
		{"grok 10s", "grok", Params{CallType: "video", Model: "grok-imagine-video", DurationSeconds: IntPtr(10)}, CostOptions{}, 0.50, "USD"},
		{"grok 1s", "grok", Params{CallType: "video", Model: "grok-imagine-video", DurationSeconds: IntPtr(1)}, CostOptions{}, 0.050, "USD"},
		{"grok 15s", "grok", Params{CallType: "video", Model: "grok-imagine-video", DurationSeconds: IntPtr(15)}, CostOptions{}, 0.75, "USD"},
		{"grok 0s 按默认 8s", "grok", Params{CallType: "video", Model: "grok-imagine-video", DurationSeconds: IntPtr(0)}, CostOptions{}, 0.40, "USD"},
		{"grok 未知模型回落默认", "grok", Params{CallType: "video", Model: "unknown-grok-model", DurationSeconds: IntPtr(10)}, CostOptions{}, 0.50, "USD"},

		// --- Ark / Grok 图片（TestArkImageCost / TestGrokImageCost）---
		{"ark 图片默认", "ark", Params{CallType: "image"}, CostOptions{}, 0.22, "CNY"},
		{"ark 图片 4.5", "ark", Params{CallType: "image", Model: "doubao-seedream-4-5-251128"}, CostOptions{}, 0.25, "CNY"},
		{"ark 图片未知模型回落默认", "ark", Params{CallType: "image", Model: "unknown-model"}, CostOptions{}, 0.22, "CNY"},
		{"grok 图片默认", "grok", Params{CallType: "image"}, CostOptions{}, 0.02, "USD"},
		{"grok 图片 pro", "grok", Params{CallType: "image", Model: "grok-imagine-image-pro"}, CostOptions{}, 0.07, "USD"},
		{"grok 图片未知模型回落默认", "grok", Params{CallType: "image", Model: "unknown-model"}, CostOptions{}, 0.02, "USD"},

		// --- OpenAI（TestOpenAICost / TestOpenAIImageTokenCost）---
		{"openai 文本 gpt-5.4-mini", "openai", Params{CallType: "text", Model: "gpt-5.4-mini",
			InputTokens: IntPtr(1_000_000), OutputTokens: IntPtr(1_000_000)}, CostOptions{}, 5.25, "USD"},
		{"openai 文本默认模型", "openai", Params{CallType: "text",
			InputTokens: IntPtr(1_000_000)}, CostOptions{}, 0.75, "USD"},
		{"openai 文本 gpt-5.5", "openai", Params{CallType: "text", Model: "gpt-5.5",
			InputTokens: IntPtr(1_000_000), OutputTokens: IntPtr(1_000_000)}, CostOptions{}, 35.0, "USD"},
		{"openai 文本低价量", "openai", Params{CallType: "text",
			InputTokens: IntPtr(500_000), OutputTokens: IntPtr(100_000)}, CostOptions{}, 0.825, "USD"},
		{"openai 图片 low", "openai", Params{CallType: "image", Model: "gpt-image-2", Quality: "low"}, CostOptions{}, 0.006, "USD"},
		{"openai 图片 high 默认方图", "openai", Params{CallType: "image", Model: "gpt-image-2", Quality: "high"}, CostOptions{}, 0.211, "USD"},
		{"openai 图片 high 竖图", "openai", Params{CallType: "image", Model: "gpt-image-2", Quality: "high", Size: "1024x1792"}, CostOptions{}, 0.317, "USD"},
		{"openai 图片 high 横图", "openai", Params{CallType: "image", Model: "gpt-image-2", Quality: "high", Size: "1792x1024"}, CostOptions{}, 0.317, "USD"},
		{"openai 图片默认模型 medium", "openai", Params{CallType: "image", Quality: "medium"}, CostOptions{}, 0.053, "USD"},
		{"openai 图片兜底与 aspect 解耦", "openai", Params{CallType: "image", Model: "gpt-image-2", Quality: "high",
			Resolution: "1K", AspectRatio: "9:16"}, CostOptions{}, 0.211, "USD"},
		{"openai 图片 token 主路径", "openai", Params{CallType: "image", Model: "gpt-image-2",
			ImageInputTokens: IntPtr(10_000), ImageOutputTokens: IntPtr(2_000),
			TextInputTokens: IntPtr(500), TextOutputTokens: IntPtr(100)}, CostOptions{}, 0.1425, "USD"},
		{"openai 图片全零 token 仍走 token 路径", "openai", Params{CallType: "image", Model: "gpt-image-2",
			ImageInputTokens: IntPtr(0), ImageOutputTokens: IntPtr(0),
			TextInputTokens: IntPtr(0), TextOutputTokens: IntPtr(0)}, CostOptions{}, 0.0, "USD"},
		{"openai 图片 token 部分缺省", "openai", Params{CallType: "image", Model: "gpt-image-2",
			ImageOutputTokens: IntPtr(2_200), TextInputTokens: IntPtr(350)}, CostOptions{}, 0.06775, "USD"},
		{"openai 视频 sora-2 8s", "openai", Params{CallType: "video", Model: "sora-2", DurationSeconds: IntPtr(8)}, CostOptions{}, 0.80, "USD"},
		{"openai 视频 sora-2 12s", "openai", Params{CallType: "video", Model: "sora-2", DurationSeconds: IntPtr(12)}, CostOptions{}, 1.20, "USD"},
		{"openai 视频 sora-2-pro 1080p", "openai", Params{CallType: "video", Model: "sora-2-pro",
			DurationSeconds: IntPtr(4), Resolution: "1080p"}, CostOptions{}, 2.80, "USD"},

		// --- Gemini / Ark / Grok 文本（tests/test_cost_calculator_text.py）---
		{"gemini 文本", "gemini", Params{CallType: "text", InputTokens: IntPtr(1000), OutputTokens: IntPtr(500)}, CostOptions{}, 0.002, "USD"},
		{"ark 文本", "ark", Params{CallType: "text", InputTokens: IntPtr(1000), OutputTokens: IntPtr(500)}, CostOptions{}, 0.0024, "CNY"},
		{"grok 文本", "grok", Params{CallType: "text", InputTokens: IntPtr(1000), OutputTokens: IntPtr(500)}, CostOptions{}, 0.00045, "USD"},
		{"未知 provider 文本回落 gemini", "unknown", Params{CallType: "text", InputTokens: IntPtr(1000), OutputTokens: IntPtr(500)}, CostOptions{}, 0.002, "USD"},

		// --- EstimateOnly 的 Ark token 近似换算 ---
		{"ark 仅预估按 60 tokens/s 换算", "ark", Params{CallType: "video", DurationSeconds: IntPtr(5),
			GenerateAudio: BoolPtr(true)}, CostOptions{EstimateOnly: true}, 6.9, "CNY"},
		{"ark 真实结算不换算", "ark", Params{CallType: "video", DurationSeconds: IntPtr(5),
			GenerateAudio: BoolPtr(true)}, CostOptions{}, 0.0, "CNY"},
	}

	for _, tc := range vectors {
		t.Run(tc.name, func(t *testing.T) {
			amount, currency, err := calc.CalculateCost(tc.provider, tc.params, tc.opts)
			if err != nil {
				t.Fatalf("CalculateCost 报错: %v", err)
			}
			checkAmount(t, amount, tc.want, tc.name)
			if currency != tc.currency {
				t.Errorf("币种 = %q, 期望 %q", currency, tc.currency)
			}
		})
	}
}

func TestCalculateCostTextWithoutTokensReturnsZero(t *testing.T) {
	// 文本无 token 数据时无从计费：早返回 (0, USD)，不查定价表。
	calc := NewCalculator(testCatalog())
	amount, currency, err := calc.CalculateCost("gemini", Params{CallType: "text"}, CostOptions{})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	checkAmount(t, amount, 0.0, "无 token 文本")
	if currency != "USD" {
		t.Errorf("币种 = %q, 期望 USD", currency)
	}
}

// TestCalculateCostCustomProviders 向量来自 tests/test_custom_cost.py。
func TestCalculateCostCustomProviders(t *testing.T) {
	calc := NewCalculator(testCatalog())

	usd := func(in, out float64, currency string) *CustomPrice {
		return &CustomPrice{PriceInput: Float64Ptr(in), PriceOutput: Float64Ptr(out), Currency: currency}
	}

	cases := []struct {
		name     string
		provider string
		params   Params
		opts     CostOptions
		want     float64
		currency string
	}{
		{"文本 USD", "custom-3", Params{CallType: "text", Model: "deepseek-v3",
			InputTokens: IntPtr(1000), OutputTokens: IntPtr(500)}, CostOptions{CustomPrice: usd(1.0, 2.0, "USD")}, 0.002, "USD"},
		{"文本 CNY", "custom-7", Params{CallType: "text", Model: "qwen-turbo",
			InputTokens: IntPtr(2000), OutputTokens: IntPtr(1000)}, CostOptions{CustomPrice: usd(0.5, 1.0, "CNY")}, 0.002, "CNY"},
		{"文本缺省 currency 落 USD", "custom-1", Params{CallType: "text", Model: "model",
			InputTokens: IntPtr(1000), OutputTokens: IntPtr(500)}, CostOptions{CustomPrice: usd(1.0, 2.0, "")}, 0.002, "USD"},
		{"图片按张", "custom-5", Params{CallType: "image", Model: "dall-e-3"},
			CostOptions{CustomPrice: usd(0.05, 0, "USD")}, 0.05, "USD"},
		{"图片 CNY", "custom-1", Params{CallType: "image", Model: "seedream"},
			CostOptions{CustomPrice: usd(0.22, 0, "CNY")}, 0.22, "CNY"},
		{"视频按秒", "custom-2", Params{CallType: "video", Model: "sora-2", DurationSeconds: IntPtr(10)},
			CostOptions{CustomPrice: usd(0.10, 0, "USD")}, 1.0, "USD"},
		{"视频缺省时长按 8s", "custom-4", Params{CallType: "video", Model: "some-video-model"},
			CostOptions{CustomPrice: usd(0.05, 0, "USD")}, 0.4, "USD"},
		{"视频零时长按 8s", "custom-4", Params{CallType: "video", Model: "some-video-model", DurationSeconds: IntPtr(0)},
			CostOptions{CustomPrice: usd(0.05, 0, "USD")}, 0.4, "USD"},
		{"音频按万字符", "custom-6", Params{CallType: "audio", Model: "tts-1", UsageTokens: IntPtr(5_000)},
			CostOptions{CustomPrice: usd(0.8, 0, "CNY")}, 0.4, "CNY"},
		{"音频零字符", "custom-6", Params{CallType: "audio", Model: "tts-1"},
			CostOptions{CustomPrice: usd(0.8, 0, "USD")}, 0.0, "USD"},
		{"音频无价归零", "custom-6", Params{CallType: "audio", Model: "tts-1", UsageTokens: IntPtr(5_000)},
			CostOptions{}, 0.0, "USD"},
		{"文本无价归零", "custom-1", Params{CallType: "text", Model: "some-model",
			InputTokens: IntPtr(1000), OutputTokens: IntPtr(500)}, CostOptions{CustomPrice: &CustomPrice{}}, 0.0, "USD"},
		{"完全无价归零", "custom-99", Params{CallType: "text", Model: "nonexistent",
			InputTokens: IntPtr(1000), OutputTokens: IntPtr(500)}, CostOptions{}, 0.0, "USD"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amount, currency, err := calc.CalculateCost(tc.provider, tc.params, tc.opts)
			if err != nil {
				t.Fatalf("CalculateCost 报错: %v", err)
			}
			checkAmount(t, amount, tc.want, tc.name)
			if currency != tc.currency {
				t.Errorf("币种 = %q, 期望 %q", currency, tc.currency)
			}
		})
	}
}

// TestEstimateReferenceVideoCost 向量来自 tests/lib/test_cost_calculator_reference_video.py。
func TestEstimateReferenceVideoCost(t *testing.T) {
	calc := NewCalculator(testCatalog())

	t.Run("grok 两段 8s", func(t *testing.T) {
		amount, currency, err := calc.EstimateReferenceVideoCost(ReferenceVideoEstimate{
			UnitDurationsSeconds: []int{8, 8}, Provider: "grok", Model: "grok-imagine-video",
		})
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		checkAmount(t, amount, 0.8, "2 × 8s × 0.050")
		if currency != "USD" {
			t.Errorf("币种 = %q, 期望 USD", currency)
		}
	})

	t.Run("sora-2-pro 1080p", func(t *testing.T) {
		amount, _, err := calc.EstimateReferenceVideoCost(ReferenceVideoEstimate{
			UnitDurationsSeconds: []int{12}, Provider: "openai", Model: "sora-2-pro", Resolution: "1080p",
		})
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		checkAmount(t, amount, 8.4, "12s × 0.70")
	})

	t.Run("ark token 计费换算", func(t *testing.T) {
		amount, currency, err := calc.EstimateReferenceVideoCost(ReferenceVideoEstimate{
			UnitDurationsSeconds: []int{5, 10}, Provider: "ark", Model: "doubao-seedance-2-0-260128",
			GenerateAudio: BoolPtr(true),
		})
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		// 15s × 60_000 tokens/s = 900_000 tokens × 46 元/百万 = 41.4
		checkAmount(t, amount, 41.4, "合计时长近似换算 token")
		if currency != "CNY" {
			t.Errorf("币种 = %q, 期望 CNY", currency)
		}
	})

	t.Run("空列表归零且带声明币种", func(t *testing.T) {
		amount, currency, err := calc.EstimateReferenceVideoCost(ReferenceVideoEstimate{
			UnitDurationsSeconds: []int{}, Provider: "grok", Model: "grok-imagine-video",
		})
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		checkAmount(t, amount, 0.0, "空列表")
		if currency != "USD" {
			t.Errorf("币种 = %q, 期望 USD", currency)
		}
	})

	t.Run("全零时长保持零", func(t *testing.T) {
		amount, currency, err := calc.EstimateReferenceVideoCost(ReferenceVideoEstimate{
			UnitDurationsSeconds: []int{0, 0}, Provider: "grok", Model: "grok-imagine-video",
		})
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		checkAmount(t, amount, 0.0, "非空全零时长不落 8 秒兜底")
		if currency != "USD" {
			t.Errorf("币种 = %q, 期望 USD", currency)
		}
	})
}

// TestSettleReferenceVectors 锁 Settle 的迁移口径：duration_ms、计费时长/有声覆盖、
// OpenAI token 聚合与显式费用优先。向量来自 tests/test_accounting_characterization.py。
func TestSettleReferenceVectors(t *testing.T) {
	calc := NewCalculator(testCatalog())
	started := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	finished := started.Add(5 * time.Second)

	t.Run("duration_ms 按 finished-started 回写", func(t *testing.T) {
		settled, err := calc.Settle(SettleRow{
			CallType: "text", Model: "claude-sonnet-4", Provider: "anthropic", StartedAt: started,
		}, finished, Settlement{InputTokens: IntPtr(100), OutputTokens: IntPtr(50)}, true, "USD", nil)
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		if settled.DurationMs != 5000 {
			t.Errorf("duration_ms = %d, 期望 5000", settled.DurationMs)
		}
		checkAmount(t, settled.CostAmount, (100*3.0+50*15.0)/1_000_000, "anthropic 结算")
	})

	t.Run("OpenAI 图片 token 聚合", func(t *testing.T) {
		settled, err := calc.Settle(SettleRow{
			CallType: "image", Model: "gpt-image-2", Provider: "openai", StartedAt: started,
		}, finished, Settlement{
			ImageInputTokens: IntPtr(100_000), ImageOutputTokens: IntPtr(200_000),
			TextInputTokens: IntPtr(50_000), TextOutputTokens: IntPtr(10_000),
		}, true, "USD", nil)
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		if got := IntValue(settled.InputTokens); got != 150_000 {
			t.Errorf("input_tokens = %d, 期望 150_000（image_in + text_in）", got)
		}
		if got := IntValue(settled.OutputTokens); got != 210_000 {
			t.Errorf("output_tokens = %d, 期望 210_000（image_out + text_out）", got)
		}
		checkAmount(t, settled.CostAmount, 7.05, "OpenAI 图片 token 费用")
	})

	t.Run("计费时长覆盖与超限回落", func(t *testing.T) {
		row := SettleRow{
			CallType: "video", Model: "veo-3.1-generate-preview", Resolution: "720p",
			DurationSeconds: IntPtr(8), GenerateAudio: BoolPtr(true), Provider: "gemini", StartedAt: started,
		}
		cases := []struct {
			name   string
			billed *int
			want   int
			cost   float64
		}{
			{"6s 覆盖请求 8s", IntPtr(6), 6, 2.4},
			{"超 24h 上限视同未提供", IntPtr(86_401), 8, 3.2},
			{"0 秒视同未提供", IntPtr(0), 8, 3.2},
			{"上限值本身合法", IntPtr(86_400), 86_400, 86_400 * 0.40},
			{"未提供回落请求时长", nil, 8, 3.2},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				settled, err := calc.Settle(row, finished, Settlement{BilledDurationSeconds: tc.billed}, true, "USD", nil)
				if err != nil {
					t.Fatalf("报错: %v", err)
				}
				if got := IntValue(settled.EffectiveDurationSeconds); got != tc.want {
					t.Errorf("生效计费时长 = %d, 期望 %d", got, tc.want)
				}
				checkAmount(t, settled.CostAmount, tc.cost, tc.name)
			})
		}
	})

	t.Run("resume 覆盖音频并按新时长计费", func(t *testing.T) {
		row := SettleRow{
			CallType: "video", Model: "veo-3.1-generate-preview", Resolution: "720p",
			DurationSeconds: IntPtr(8), GenerateAudio: BoolPtr(true), Provider: "gemini", StartedAt: started,
		}
		settled, err := calc.Settle(row, finished, Settlement{
			BilledDurationSeconds: IntPtr(6), GenerateAudio: BoolPtr(false),
		}, true, "USD", nil)
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		if got := IntValue(settled.EffectiveDurationSeconds); got != 6 {
			t.Errorf("生效计费时长 = %d, 期望 6", got)
		}
		if settled.EffectiveGenerateAudio == nil || *settled.EffectiveGenerateAudio {
			t.Error("生效有声标志应为 false（backend 回报覆盖请求）")
		}
		checkAmount(t, settled.CostAmount, 1.2, "720p 无声 6s")

		settled, err = calc.Settle(row, finished, Settlement{BilledDurationSeconds: IntPtr(86_401)}, true, "USD", nil)
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		if settled.EffectiveGenerateAudio == nil || !*settled.EffectiveGenerateAudio {
			t.Error("backend 未回报音频标志时应保留行内请求值 true")
		}
		checkAmount(t, settled.CostAmount, 3.2, "超限回落请求 8s 且有声")
	})

	t.Run("显式费用优先于自动计算", func(t *testing.T) {
		settled, err := calc.Settle(SettleRow{
			CallType: "text", Model: "claude-sonnet-4", Provider: "anthropic", StartedAt: started,
		}, finished, Settlement{CostAmount: Float64Ptr(0.123), InputTokens: IntPtr(1_000_000)}, true, "USD", nil)
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		checkAmount(t, settled.CostAmount, 0.123, "SDK 直报费用")
		if settled.Currency != "USD" {
			t.Errorf("币种 = %q, 期望 baseCurrency USD", settled.Currency)
		}

		settled, err = calc.Settle(SettleRow{CallType: "text", StartedAt: started}, finished,
			Settlement{CostAmount: Float64Ptr(0.2), Currency: "CNY"}, true, "USD", nil)
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		if settled.Currency != "CNY" {
			t.Errorf("币种 = %q, 期望申报币种 CNY", settled.Currency)
		}
	})

	t.Run("resume failed 显式零费用", func(t *testing.T) {
		settled, err := calc.Settle(SettleRow{
			CallType: "video", Model: "veo-3.1-generate-preview", Resolution: "720p",
			DurationSeconds: IntPtr(8), GenerateAudio: BoolPtr(true), Provider: "gemini", StartedAt: started,
		}, finished, Settlement{CostAmount: Float64Ptr(0)}, false, "USD", nil)
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		checkAmount(t, settled.CostAmount, 0.0, "失败补账零费用")
		if settled.Currency != "USD" {
			t.Errorf("币种 = %q, 期望 USD", settled.Currency)
		}
	})

	t.Run("不自动计费且无显式费用时归零", func(t *testing.T) {
		settled, err := calc.Settle(SettleRow{
			CallType: "image", Model: "gpt-image-2", Provider: "openai", StartedAt: started,
		}, finished, Settlement{}, false, "CNY", nil)
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		checkAmount(t, settled.CostAmount, 0.0, "不自动计费")
		if settled.Currency != "CNY" {
			t.Errorf("币种 = %q, 期望 baseCurrency CNY", settled.Currency)
		}
	})

	t.Run("provider 缺失回落 gemini", func(t *testing.T) {
		settled, err := calc.Settle(SettleRow{
			CallType: "video", Resolution: "1080p", DurationSeconds: IntPtr(8),
			GenerateAudio: BoolPtr(true), StartedAt: started,
		}, finished, Settlement{}, true, "USD", nil)
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		checkAmount(t, settled.CostAmount, 0.64, "行无 provider 时按 gemini 默认模型计费")
	})

	t.Run("started 缺失 duration_ms 归零", func(t *testing.T) {
		settled, err := calc.Settle(SettleRow{
			CallType: "text", Model: "claude-sonnet-4", Provider: "anthropic",
		}, finished, Settlement{}, false, "USD", nil)
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		if settled.DurationMs != 0 {
			t.Errorf("duration_ms = %d, 期望 0", settled.DurationMs)
		}
	})
}

func TestSettleNilCalculatorErrors(t *testing.T) {
	// autoCalc 为真但计算器未注入：报错而非静默计零。
	var calc *Calculator
	if _, err := calc.Settle(SettleRow{CallType: "text"}, time.Now(), Settlement{}, true, "USD", nil); err == nil {
		t.Fatal("未注入目录的自动计费必须报错")
	}
}

func TestNewCalculatorWithNilCatalog(t *testing.T) {
	// 目录为 nil 时未知 provider 的兜底查询报错而非静默计零；显式声明仍可查。
	calc := NewCalculator(nil)
	if _, _, err := calc.CalculateCost("unknown", Params{CallType: "video"}, CostOptions{}); err == nil {
		t.Fatal("nil 目录且需 Gemini 兜底时必须报错")
	}
	amount, currency, err := calc.CalculateCost("anthropic", Params{
		CallType: "text", Model: "claude-sonnet-4", InputTokens: IntPtr(1_000_000),
	}, CostOptions{})
	if err != nil {
		t.Fatalf("anthropic 内置表不依赖目录: %v", err)
	}
	checkAmount(t, amount, 3.0, "内置表计费")
	if currency != "USD" {
		t.Errorf("币种 = %q, 期望 USD", currency)
	}
}
