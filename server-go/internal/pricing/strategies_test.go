package pricing

import (
	"math"
	"testing"
)

// approxEqual 比较计费金额：链路是浮点累乘，须容差比较；对拍向量来自 Python 侧
// tests/test_pricing_strategies.py 与 tests/test_tts_pricing.py。
func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func mustCalculate(t *testing.T, p Pricing, params Params) (float64, string) {
	t.Helper()
	amount, currency, err := Calculate(p, params)
	if err != nil {
		t.Fatalf("Calculate(%s) 报错: %v", p.Kind, err)
	}
	return amount, currency
}

func checkAmount(t *testing.T, got, want float64, context string) {
	t.Helper()
	if !approxEqual(got, want) {
		t.Errorf("%s: 金额 = %v, 期望 %v", context, got, want)
	}
}

func TestCalculatePerToken(t *testing.T) {
	p := NewPerToken(map[string]map[string]float64{
		"m1": {"input": 0.50, "output": 3.00},
	}, "m1", "USD")

	cases := []struct {
		name       string
		model      string
		input, out int
		want       float64
	}{
		{"已知模型", "m1", 1000, 500, (1000*0.50 + 500*3.00) / 1_000_000},
		{"未知模型回落默认", "unknown", 1000, 500, (1000*0.50 + 500*3.00) / 1_000_000},
		{"空模型用默认", "", 1000, 0, 1000 * 0.50 / 1_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amount, currency := mustCalculate(t, p, Params{
				CallType: "text", Model: tc.model, InputTokens: IntPtr(tc.input), OutputTokens: IntPtr(tc.out),
			})
			if currency != "USD" {
				t.Errorf("币种 = %q, 期望 USD", currency)
			}
			checkAmount(t, amount, tc.want, tc.name)
		})
	}
}

func TestCalculatePerTokenMissingRatesIsZero(t *testing.T) {
	// 表里连默认模型都没有：读零值而非报错（Python 侧 rates.get 链的等价兜底）。
	p := NewPerToken(nil, "m1", "USD")
	amount, _ := mustCalculate(t, p, Params{CallType: "text", Model: "m1", InputTokens: IntPtr(1000)})
	checkAmount(t, amount, 0, "空费率表")
}

func TestCalculatePerImageFlat(t *testing.T) {
	p := NewPerImageFlat(map[string]float64{"m1": 0.22}, "m1", "CNY")

	cases := []struct {
		name       string
		model      string
		n          int
		wantAmount float64
	}{
		{"单张", "m1", 0, 0.22},
		{"n 张", "m1", 3, 0.22 * 3},
		{"未知模型回落默认", "unknown", 4, 0.22 * 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amount, currency := mustCalculate(t, p, Params{CallType: "image", Model: tc.model, N: tc.n})
			if currency != "CNY" {
				t.Errorf("币种 = %q, 期望 CNY", currency)
			}
			checkAmount(t, amount, tc.wantAmount, tc.name)
		})
	}
}

func TestCalculatePerImageByResolution(t *testing.T) {
	p := NewPerImageByResolution(map[string]map[string]float64{
		"m1": {"512PX": 0.045, "1K": 0.067, "2K": 0.101, "4K": 0.151},
	}, "m1", "USD")

	cases := []struct {
		name       string
		resolution string
		n          int
		wantAmount float64
	}{
		{"小写分辨率归一", "1k", 0, 0.067},
		{"4K", "4K", 0, 0.151},
		{"未知分辨率落 1K", "unknown", 0, 0.067},
		{"空分辨率落 1K", "", 0, 0.067},
		{"n 张", "1K", 3, 0.067 * 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amount, _ := mustCalculate(t, p, Params{CallType: "image", Model: "m1", Resolution: tc.resolution, N: tc.n})
			checkAmount(t, amount, tc.wantAmount, tc.name)
		})
	}
}

func TestCalculatePerImageByResolutionZeroTier(t *testing.T) {
	// free 模型 1K 显式 0.0：未知分辨率回落 default_cost 时应保留自身 0.0，不误用 paid 费率。
	p := NewPerImageByResolution(map[string]map[string]float64{
		"free": {"1K": 0.0},
		"paid": {"1K": 0.067},
	}, "paid", "USD")
	amount, _ := mustCalculate(t, p, Params{CallType: "image", Model: "free", Resolution: "4K"})
	checkAmount(t, amount, 0.0, "免费档 0.0 不被当作缺失")
}

func openAIPricing() Pricing {
	return NewPerImageOpenAIToken(
		map[string]map[string]float64{
			"gpt-image-2": {
				"image_in": 8.0, "image_cached_in": 2.0, "image_out": 30.0,
				"text_in": 5.0, "text_cached_in": 1.25, "text_out": 0.0,
			},
		},
		map[string]map[QualitySizeKey]float64{
			"gpt-image-2": {
				{Quality: "low", Size: "1024x1024"}:    0.006,
				{Quality: "medium", Size: "1024x1024"}: 0.053,
				{Quality: "medium", Size: "1024x1792"}: 0.106,
				{Quality: "high", Size: "1024x1024"}:   0.211,
				{Quality: "high", Size: "1024x1792"}:   0.317,
				{Quality: "high", Size: "1792x1024"}:   0.317,
			},
		},
		"gpt-image-2", "USD",
	)
}

func TestCalculatePerImageOpenAITokenUsagePath(t *testing.T) {
	p := openAIPricing()
	amount, currency := mustCalculate(t, p, Params{
		CallType: "image", Model: "gpt-image-2",
		ImageInputTokens: IntPtr(10_000), ImageOutputTokens: IntPtr(2_000),
		TextInputTokens: IntPtr(500), TextOutputTokens: IntPtr(100),
	})
	if currency != "USD" {
		t.Errorf("币种 = %q, 期望 USD", currency)
	}
	checkAmount(t, amount, (10_000*8.0+2_000*30.0+500*5.0+100*0.0)/1_000_000, "token 主路径")

	// 四个 token 全为 0 仍走 token 主路径（非 None 即算有 usage）。
	amount, _ = mustCalculate(t, p, Params{
		CallType: "image", Model: "gpt-image-2",
		ImageInputTokens: IntPtr(0), ImageOutputTokens: IntPtr(0),
		TextInputTokens: IntPtr(0), TextOutputTokens: IntPtr(0),
	})
	checkAmount(t, amount, 0.0, "全零 token 仍走 token 路径")
}

func TestCalculatePerImageOpenAITokenFallbackPath(t *testing.T) {
	p := openAIPricing()

	cases := []struct {
		name       string
		quality    string
		size       string
		n          int
		wantAmount float64
	}{
		{"无 size 落 1024x1024", "medium", "", 0, 0.053},
		{"high 无 size 落默认方图", "high", "", 0, 0.211},
		{"显式 size", "medium", "1024x1792", 0, 0.106},
		{"未知 quality+size 三级回落", "ultra", "9999x9999", 0, 0.053},
		{"兜底按 n 缩放", "medium", "", 2, 0.053 * 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amount, _ := mustCalculate(t, p, Params{
				CallType: "image", Model: "gpt-image-2",
				Quality: tc.quality, Size: tc.size, N: tc.n,
				// 带 resolution/aspect 也不参与反查（计费与输出尺寸解耦）。
				Resolution: "1K", AspectRatio: "9:16",
			})
			checkAmount(t, amount, tc.wantAmount, tc.name)
		})
	}
}

func TestCalculatePerImageOpenAITokenFallbackLevels(t *testing.T) {
	// 三级回落：第二级 (quality, 1024x1024)、第三级 (medium, 1024x1024)、最末 0.034。
	second := NewPerImageOpenAIToken(nil, map[string]map[QualitySizeKey]float64{
		"m": {{Quality: "high", Size: "1024x1024"}: 0.211},
	}, "m", "USD")
	amount, _ := mustCalculate(t, second, Params{CallType: "image", Model: "m", Quality: "high", Size: "1x1"})
	checkAmount(t, amount, 0.211, "第二级同 quality 方图")

	last := NewPerImageOpenAIToken(nil, map[string]map[QualitySizeKey]float64{"m": {}}, "m", "USD")
	amount, _ = mustCalculate(t, last, Params{CallType: "image", Model: "m", Quality: "high", Size: "1x1"})
	checkAmount(t, amount, 0.034, "最末静态兜底 0.034")
}

func TestCalculatePerSecondMatrixResolutionAudio(t *testing.T) {
	p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{
		"veo": {
			{Resolution: "720p", Audio: AudioOn}:   0.05,
			{Resolution: "720p", Audio: AudioOff}:  0.05,
			{Resolution: "1080p", Audio: AudioOn}:  0.08,
			{Resolution: "1080p", Audio: AudioOff}: 0.08,
		},
	}, "veo", DimensionsResolutionAudio, "USD", "")

	cases := []struct {
		name       string
		duration   *int
		resolution string
		audio      *bool
		wantAmount float64
	}{
		{"已知档", IntPtr(8), "1080p", BoolPtr(true), 0.64},
		{"大写分辨率归一", IntPtr(8), "1080P", BoolPtr(true), 0.64},
		{"未知分辨率回落 1080p+有声", IntPtr(5), "UNKNOWN", BoolPtr(true), 0.40},
		{"空分辨率落 1080p", IntPtr(8), "", BoolPtr(false), 0.64},
		{"音频维度区分", IntPtr(8), "720p", BoolPtr(false), 0.40},
		{"真实零秒保持 0", IntPtr(0), "1080p", BoolPtr(true), 0.0},
		{"缺省时长按 8 秒", nil, "1080p", BoolPtr(true), 0.64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amount, currency := mustCalculate(t, p, Params{
				CallType: "video", Model: "veo", Resolution: tc.resolution,
				DurationSeconds: tc.duration, GenerateAudio: tc.audio,
			})
			if currency != "USD" {
				t.Errorf("币种 = %q, 期望 USD", currency)
			}
			checkAmount(t, amount, tc.wantAmount, tc.name)
		})
	}
}

func TestCalculatePerSecondMatrixZeroFallbackRate(t *testing.T) {
	// free 模型 (1080p,True) 显式 0.0：未知分辨率回落 fallback 时应保留自身 0.0，不误用 paid 费率。
	p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{
		"free": {{Resolution: "1080p", Audio: AudioOn}: 0.0},
		"paid": {{Resolution: "1080p", Audio: AudioOn}: 0.08},
	}, "paid", DimensionsResolutionAudio, "USD", "")
	amount, _ := mustCalculate(t, p, Params{
		CallType: "video", Model: "free", DurationSeconds: IntPtr(10),
		Resolution: "UNKNOWN", GenerateAudio: BoolPtr(true),
	})
	checkAmount(t, amount, 0.0, "免费档 0.0 不被当作缺失")
}

func TestCalculatePerSecondMatrixResolutionOnly(t *testing.T) {
	p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{
		"sora": {
			{Resolution: "720p", Audio: AudioUnspecified}:  0.30,
			{Resolution: "1024p", Audio: AudioUnspecified}: 0.50,
			{Resolution: "1080p", Audio: AudioUnspecified}: 0.70,
		},
	}, "sora", DimensionsResolutionOnly, "USD", "")

	cases := []struct {
		name       string
		duration   *int
		resolution string
		wantAmount float64
	}{
		{"已知档", IntPtr(4), "1080p", 2.80},
		{"大写分辨率归一", IntPtr(4), "1080P", 2.80},
		{"空分辨率落 720p", IntPtr(8), "", 2.40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amount, _ := mustCalculate(t, p, Params{
				CallType: "video", Model: "sora", Resolution: tc.resolution, DurationSeconds: tc.duration,
			})
			checkAmount(t, amount, tc.wantAmount, tc.name)
		})
	}
}

func TestCalculatePerSecondMatrixCustomDefaultResolution(t *testing.T) {
	// 该模型未显式指定分辨率时实际下发的档位不是通用 720p（如 MiniMax H3 是 768p），
	// 回落须跟随 default_resolution，否则会落进不存在的 720p 档而结算为 0。
	p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{
		"h3": {
			{Resolution: "768p", Audio: AudioUnspecified}: 0.50,
			{Resolution: "2k", Audio: AudioUnspecified}:   0.80,
		},
	}, "h3", DimensionsResolutionOnly, "CNY", "768p")

	amount, currency := mustCalculate(t, p, Params{CallType: "video", Model: "h3", DurationSeconds: IntPtr(8)})
	if currency != "CNY" {
		t.Errorf("币种 = %q, 期望 CNY", currency)
	}
	checkAmount(t, amount, 4.0, "空分辨率落 default_resolution")
	amount, _ = mustCalculate(t, p, Params{CallType: "video", Model: "h3", Resolution: "2k", DurationSeconds: IntPtr(8)})
	checkAmount(t, amount, 6.4, "2k 档")
}

func TestCalculatePerSecondMatrixFlat(t *testing.T) {
	p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{
		"grok": {{Audio: AudioUnspecified}: 0.050},
	}, "grok", DimensionsFlat, "USD", "")

	amount, _ := mustCalculate(t, p, Params{
		CallType: "video", Model: "grok", DurationSeconds: IntPtr(10), Resolution: "480p",
	})
	checkAmount(t, amount, 0.50, "flat 忽略分辨率")
	amount, _ = mustCalculate(t, p, Params{CallType: "video", Model: "grok"})
	checkAmount(t, amount, 0.40, "缺省时长按 8 秒")
}

func TestCalculatePerSecondTiered(t *testing.T) {
	p := NewPerSecondTiered(map[string]map[TierKey]float64{
		"kling-v2-5-turbo": {
			{Tier: "std", Audio: false}: 0.6,
			{Tier: "std", Audio: true}:  0.8,
			{Tier: "pro", Audio: false}: 0.8,
			{Tier: "pro", Audio: true}:  1.0,
			{Tier: "4k", Audio: false}:  3.0,
			{Tier: "4k", Audio: true}:   3.0,
		},
	}, "kling-v2-5-turbo", "CNY")

	cases := []struct {
		name       string
		model      string
		tier       string
		audio      *bool
		resolution string
		duration   *int
		wantAmount float64
	}{
		{"std 无声", "kling-v2-5-turbo", "std", BoolPtr(false), "", IntPtr(5), 3.0},
		{"std 有声", "kling-v2-5-turbo", "std", BoolPtr(true), "", IntPtr(5), 4.0},
		{"pro 无声", "kling-v2-5-turbo", "pro", BoolPtr(false), "", IntPtr(5), 4.0},
		{"pro 有声", "kling-v2-5-turbo", "pro", BoolPtr(true), "", IntPtr(5), 5.0},
		{"default 归一到 std", "kling-v2-5-turbo", "default", BoolPtr(false), "", IntPtr(10), 6.0},
		{"4K 覆盖 service_tier 且忽略音频", "kling-v2-5-turbo", "pro", BoolPtr(true), "4K", IntPtr(5), 15.0},
		{"4K 小写", "kling-v2-5-turbo", "std", BoolPtr(false), "4k", IntPtr(5), 15.0},
		{"未知档回落 std 同音频", "kling-v2-5-turbo", "ultra", BoolPtr(false), "", IntPtr(5), 3.0},
		{"未知模型回落默认模型", "unknown-model", "pro", BoolPtr(true), "", IntPtr(5), 5.0},
		{"缺省时长按 8 秒", "kling-v2-5-turbo", "std", BoolPtr(false), "", nil, 4.8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amount, currency := mustCalculate(t, p, Params{
				CallType: "video", Model: tc.model, ServiceTier: tc.tier,
				GenerateAudio: tc.audio, Resolution: tc.resolution, DurationSeconds: tc.duration,
			})
			if currency != "CNY" {
				t.Errorf("币种 = %q, 期望 CNY", currency)
			}
			checkAmount(t, amount, tc.wantAmount, tc.name)
		})
	}
}

func TestCalculatePerVideoBucket(t *testing.T) {
	rates := map[string]map[BucketKey]float64{
		"hailuo": {
			{Resolution: "768p", DurationSeconds: 6}:  2.0,
			{Resolution: "768p", DurationSeconds: 10}: 4.0,
			{Resolution: "1080p", DurationSeconds: 6}: 3.5,
		},
		"hailuo-fast": {
			{Resolution: "768p", DurationSeconds: 6}:  1.35,
			{Resolution: "768p", DurationSeconds: 10}: 2.25,
			{Resolution: "1080p", DurationSeconds: 6}: 2.31,
		},
	}
	p := NewPerVideoBucket(rates, "hailuo", "CNY")

	cases := []struct {
		name       string
		model      string
		resolution string
		duration   *int
		wantAmount float64
	}{
		{"精确档 768p 6s", "hailuo", "768p", IntPtr(6), 2.0},
		{"精确档 768p 10s", "hailuo", "768p", IntPtr(10), 4.0},
		{"精确档 1080p 6s", "hailuo", "1080p", IntPtr(6), 3.5},
		{"另一模型档表", "hailuo-fast", "1080p", IntPtr(6), 2.31},
		{"大写分辨率归一", "hailuo", "768P", IntPtr(10), 4.0},
		{"未知模型回落默认", "unknown", "768p", IntPtr(6), 2.0},
		{"同分辨率内取最近档", "hailuo", "1080p", IntPtr(10), 3.5},
		{"无同分辨率档取全档最近", "hailuo", "540p", IntPtr(10), 4.0},
		{"跨分辨率打平取更低价", "hailuo", "540p", IntPtr(6), 2.0},
		{"空分辨率落 768p", "hailuo", "", IntPtr(6), 2.0},
		{"缺省时长落最短档", "hailuo", "768p", nil, 2.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amount, currency := mustCalculate(t, p, Params{
				CallType: "video", Model: tc.model, Resolution: tc.resolution, DurationSeconds: tc.duration,
			})
			if currency != "CNY" {
				t.Errorf("币种 = %q, 期望 CNY", currency)
			}
			checkAmount(t, amount, tc.wantAmount, tc.name)
		})
	}
}

func TestCalculatePerVideoBucketTieBreakInsertionOrderIndependent(t *testing.T) {
	// 540p + duration=6 → ("768p",6)=2.0 与 ("1080p",6)=3.5 时长差同为 0。tie-break 取更低价
	// 且与档表迭代序无关：反向声明的同一档表必须得同一结果（Go map 迭代序本身随机，此断言兜底）。
	reversed := NewPerVideoBucket(map[string]map[BucketKey]float64{
		"hailuo": {
			{Resolution: "1080p", DurationSeconds: 6}: 3.5,
			{Resolution: "768p", DurationSeconds: 10}: 4.0,
			{Resolution: "768p", DurationSeconds: 6}:  2.0,
		},
	}, "hailuo", "CNY")
	amount, _ := mustCalculate(t, reversed, Params{
		CallType: "video", Model: "hailuo", Resolution: "540p", DurationSeconds: IntPtr(6),
	})
	checkAmount(t, amount, 2.0, "反向插入序仍取更低价档")
}

func TestCalculatePerTokenVideo(t *testing.T) {
	p := NewPerTokenVideo(map[string]map[TierKey]float64{
		"seedance": {
			{Tier: "default", Audio: true}:  16.00,
			{Tier: "default", Audio: false}: 8.00,
			{Tier: "flex", Audio: true}:     8.00,
			{Tier: "flex", Audio: false}:    4.00,
		},
	}, "seedance", "CNY")

	cases := []struct {
		name   string
		model  string
		tier   string
		audio  *bool
		tokens *int
		want   float64
	}{
		{"flex 无声", "seedance", "flex", BoolPtr(false), IntPtr(1_000_000), 4.00},
		{"flex 有声", "seedance", "flex", BoolPtr(true), IntPtr(1_000_000), 8.00},
		{"default 有声", "seedance", "default", BoolPtr(true), IntPtr(1_000_000), 16.00},
		{"未知档回落 default 有声", "seedance", "ultra", nil, IntPtr(1_000_000), 16.00},
		{"未知模型回落默认模型", "unknown", "", BoolPtr(true), IntPtr(1_000_000), 16.00},
		{"零 token", "seedance", "default", BoolPtr(true), IntPtr(0), 0.0},
		{"缺 usage_tokens 按 0", "seedance", "default", BoolPtr(true), nil, 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amount, currency := mustCalculate(t, p, Params{
				CallType: "video", Model: tc.model, ServiceTier: tc.tier,
				GenerateAudio: tc.audio, UsageTokens: tc.tokens,
			})
			if currency != "CNY" {
				t.Errorf("币种 = %q, 期望 CNY", currency)
			}
			checkAmount(t, amount, tc.want, tc.name)
		})
	}
}

func TestCalculatePerTokenVideoFallbackPrice(t *testing.T) {
	// 档表缺 ("default", True) 时的静态兜底 16.00。
	p := NewPerTokenVideo(map[string]map[TierKey]float64{
		"m": {{Tier: "flex", Audio: true}: 8.0},
	}, "m", "CNY")
	amount, _ := mustCalculate(t, p, Params{
		CallType: "video", Model: "m", UsageTokens: IntPtr(1_000_000), GenerateAudio: BoolPtr(true),
	})
	checkAmount(t, amount, 16.00, "静态兜底 16.00")
}

func TestCalculatePerCharacter(t *testing.T) {
	p := NewPerCharacter(map[string]float64{"qwen3-tts-flash": 0.8}, "qwen3-tts-flash", "CNY")

	cases := []struct {
		name  string
		model string
		chars *int
		want  float64
	}{
		{"整万字符", "qwen3-tts-flash", IntPtr(10_000), 0.8},
		{"部分字符", "qwen3-tts-flash", IntPtr(1500), 1500.0 / 10_000 * 0.8},
		{"零字符", "qwen3-tts-flash", IntPtr(0), 0.0},
		{"未知模型回落默认", "unknown-tts", IntPtr(10_000), 0.8},
		{"缺 usage_tokens 按 0", "qwen3-tts-flash", nil, 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amount, currency := mustCalculate(t, p, Params{
				CallType: "audio", Model: tc.model, UsageTokens: tc.chars,
			})
			if currency != "CNY" {
				t.Errorf("币种 = %q, 期望 CNY", currency)
			}
			checkAmount(t, amount, tc.want, tc.name)
		})
	}
}

func TestCalculateViduDelegate(t *testing.T) {
	p := NewViduDelegate("CNY")

	t.Run("credits 路径", func(t *testing.T) {
		amount, currency := mustCalculate(t, p, Params{
			CallType: "video", Model: "viduq3-turbo", UsageTokens: IntPtr(100),
		})
		if currency != "CNY" {
			t.Errorf("币种 = %q, 期望 CNY", currency)
		}
		checkAmount(t, amount, 100*ViduCreditToCNY, "credits × 0.03125")
	})

	t.Run("credits=0 是合法值", func(t *testing.T) {
		amount, _ := mustCalculate(t, p, Params{CallType: "video", Model: "viduq3-turbo", UsageTokens: IntPtr(0)})
		checkAmount(t, amount, 0.0, "零 credits")
	})

	t.Run("视频无 credits 按表估算", func(t *testing.T) {
		amount, _ := mustCalculate(t, p, Params{
			CallType: "video", Model: "viduq3-turbo", Resolution: "720p", DurationSeconds: IntPtr(5),
		})
		checkAmount(t, amount, 5*12*ViduCreditToCNY, "720p 12 credits/s")
	})

	t.Run("视频未知模型回落默认模型", func(t *testing.T) {
		amount, _ := mustCalculate(t, p, Params{
			CallType: "video", Model: "totally-unknown", Resolution: "720p", DurationSeconds: IntPtr(5),
		})
		checkAmount(t, amount, 5*12*ViduCreditToCNY, "回落 viduq3-turbo")
	})

	t.Run("视频缺省时长按 5 秒且未知分辨率落 720p", func(t *testing.T) {
		amount, _ := mustCalculate(t, p, Params{CallType: "video", Model: "viduq3-pro", Resolution: "999p"})
		checkAmount(t, amount, 5*25*ViduCreditToCNY, "落 720p 25 credits/s × 5s")
	})

	t.Run("图片按张查表", func(t *testing.T) {
		amount, _ := mustCalculate(t, p, Params{CallType: "image", Model: "viduq2", Resolution: "2k"})
		checkAmount(t, amount, 12*ViduCreditToCNY, "2k 12 credits")
		amount, _ = mustCalculate(t, p, Params{CallType: "image", Model: "viduq2"})
		checkAmount(t, amount, 8*ViduCreditToCNY, "缺省分辨率落 1080p 8 credits")
	})

	t.Run("未知调用类型零费用", func(t *testing.T) {
		amount, currency := mustCalculate(t, p, Params{CallType: "text", Model: "viduq2"})
		if currency != "CNY" {
			t.Errorf("币种 = %q, 期望 CNY", currency)
		}
		checkAmount(t, amount, 0.0, "text 调用不适用 vidu 计费")
	})
}

func TestCalculateUnknownKindFails(t *testing.T) {
	if _, _, err := Calculate(Pricing{}, Params{CallType: "text"}); err == nil {
		t.Fatal("未知 kind 必须报错，不得静默计零")
	}
}
