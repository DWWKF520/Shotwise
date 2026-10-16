package pricing

// 内置供应商的定价目录数据（Catalog 的生产实现）：把 Python 侧 lib/config/registry.py 里每个
// 模型的 pricing 声明逐项搬到 Go，作为 LookupPricing 的查表真相源。
//
// 划分：internal/provider 的注册表持有编排与展示口径的模型元数据（媒体类型、能力、时长、
// 分辨率），本文件持有同名模型的计费声明；两者模型 ID 集合与媒体类型逐一对应
// （catalog_registry_test.go 校验），但互不 import，避免 provider → pricing 的耦合。
//
// pricing 为 nil 表示该模型已登记但未声明定价（如 Agent Plan 套餐），查找时按 LookupPricing
// 的回落链继续回落，不做「缺席即零价」的隐式兜底。

// 定价目录里模型条目的媒体类型取值，与查找入参 call_type 同口径。
const (
	catalogMediaText  = "text"
	catalogMediaImage = "image"
	catalogMediaVideo = "video"
	catalogMediaAudio = "audio"
)

// catalogModel 是定价目录里的一条模型登记。
type catalogModel struct {
	id        string
	mediaType string
	// pricing 为 nil 表示已登记但未声明定价；isDefault 标记该供应商在该媒体类型下的默认模型。
	pricing   *Pricing
	isDefault bool
}

// catalogProvider 是一个供应商的模型登记表（登记顺序即注册表声明顺序）。
type catalogProvider struct {
	id     string
	models []catalogModel
}

// --- 构造 helper：内置供应商的每个模型条目各自声明费率，费率表只含该模型自身、
// default_model 即该模型名（与 Python 侧 registry 的 _*_pricing 同形）。 -----------------

// tokenPricing 构造按 token（每百万）双费率的文本声明；对应 Python 的 _*_text_pricing。
func tokenPricing(model string, inputRate, outputRate float64, currency string) *Pricing {
	p := NewPerToken(map[string]map[string]float64{
		model: {"input": inputRate, "output": outputRate},
	}, model, currency)
	return &p
}

// imageFlatPricing 构造按张计费、单价与分辨率无关的图片声明；对应 Python 的 _*_image_pricing
// （ark / grok / dashscope / minimax / agnes）与 _kling_image_flat_pricing。
func imageFlatPricing(model string, perImage float64, currency string) *Pricing {
	p := NewPerImageFlat(map[string]float64{model: perImage}, model, currency)
	return &p
}

// imageByResolutionPricing 构造按分辨率分档计费的图片声明；对应 Python 的 _gemini_image_pricing
// 与 _kling_image_by_resolution_pricing。
func imageByResolutionPricing(model string, rates map[string]float64, currency string) *Pricing {
	p := NewPerImageByResolution(map[string]map[string]float64{model: rates}, model, currency)
	return &p
}

// openAIImagePricing 构造 OpenAI 图片声明：SDK 返回 usage 时按 token 主路径计，否则按
// (quality, size) 静态表兜底；对应 Python 的 _openai_image_pricing。
func openAIImagePricing(
	model string,
	tokenRates map[string]float64,
	fallbackRates map[QualitySizeKey]float64,
	currency string,
) *Pricing {
	p := NewPerImageOpenAIToken(
		map[string]map[string]float64{model: tokenRates},
		map[string]map[QualitySizeKey]float64{model: fallbackRates},
		model, currency,
	)
	return &p
}

// secondMatrixPricing 构造按秒、由维度组合查表的视频声明；对应 Python 的 _veo_video_pricing
// （resolution_audio）、_sora_video_pricing / _dashscope_video_pricing /
// _minimax_video_per_second_pricing（resolution_only）与 Grok / Agnes 的内联 flat 声明。
func secondMatrixPricing(
	model string,
	rates map[MatrixKey]float64,
	dimensions Dimensions,
	currency, defaultResolution string,
) *Pricing {
	p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{model: rates}, model, dimensions, currency, defaultResolution)
	return &p
}

// secondTieredPricing 构造视频「质量档 × 是否有声」每秒计费声明；对应 Python 的 _kling_video_pricing。
func secondTieredPricing(model string, rates map[TierKey]float64, currency string) *Pricing {
	p := NewPerSecondTiered(map[string]map[TierKey]float64{model: rates}, model, currency)
	return &p
}

// tokenVideoPricing 构造视频按 (service_tier, 是否有声) 每百万 token 计费的声明；
// 对应 Python 的 _ark_video_pricing。
func tokenVideoPricing(model string, rates map[TierKey]float64, currency string) *Pricing {
	p := NewPerTokenVideo(map[string]map[TierKey]float64{model: rates}, model, currency)
	return &p
}

// videoBucketPricing 构造按 (分辨率, 时长) 离散档计费的视频声明；对应 Python 的 _minimax_video_pricing。
func videoBucketPricing(model string, buckets map[BucketKey]float64, currency string) *Pricing {
	p := NewPerVideoBucket(map[string]map[BucketKey]float64{model: buckets}, model, currency)
	return &p
}

// characterPricing 构造按每万字符计费的语音合成声明；对应 Python 的 _dashscope_audio_pricing。
func characterPricing(model string, per10kChars float64, currency string) *Pricing {
	p := NewPerCharacter(map[string]float64{model: per10kChars}, model, currency)
	return &p
}

// viduDelegatePricing 构造 Vidu 委托标记（币种 CNY）：实际费率在 calculateViduCost 里按响应
// credits 结算，本声明不携带费率。
func viduDelegatePricing() *Pricing {
	p := NewViduDelegate("CNY")
	return &p
}

// resOnlyRates 把「分辨率 → 每秒价」展开成 resolution_only 维度的矩阵费率键（音频维度恒为未指定）；
// 对应 Python 里 {(res, None): rate} 的构造。
func resOnlyRates(rates map[string]float64) map[MatrixKey]float64 {
	out := make(map[MatrixKey]float64, len(rates))
	for resolution, rate := range rates {
		out[MatrixKey{Resolution: resolution}] = rate
	}
	return out
}

// Veo 三档费率（美元/秒），按 (分辨率小写, 是否生成音频)。含音价取自 AI Studio 定价页（Veo 3.1
// 段），与 Vertex 定价页的「Video + Audio generation」档逐项一致；无音价只见于 Vertex 定价页的
// 「Video generation」档。Lite 两页均无 4k 档，故不设。
var (
	veoStandardRates = map[MatrixKey]float64{
		{Resolution: "720p", Audio: AudioOn}:   0.40,
		{Resolution: "720p", Audio: AudioOff}:  0.20,
		{Resolution: "1080p", Audio: AudioOn}:  0.40,
		{Resolution: "1080p", Audio: AudioOff}: 0.20,
		{Resolution: "4k", Audio: AudioOn}:     0.60,
		{Resolution: "4k", Audio: AudioOff}:    0.40,
	}
	veoFastRates = map[MatrixKey]float64{
		{Resolution: "720p", Audio: AudioOn}:   0.10,
		{Resolution: "720p", Audio: AudioOff}:  0.08,
		{Resolution: "1080p", Audio: AudioOn}:  0.12,
		{Resolution: "1080p", Audio: AudioOff}: 0.10,
		{Resolution: "4k", Audio: AudioOn}:     0.30,
		{Resolution: "4k", Audio: AudioOff}:    0.25,
	}
	veoLiteRates = map[MatrixKey]float64{
		{Resolution: "720p", Audio: AudioOn}:   0.05,
		{Resolution: "720p", Audio: AudioOff}:  0.03,
		{Resolution: "1080p", Audio: AudioOn}:  0.08,
		{Resolution: "1080p", Audio: AudioOff}: 0.05,
	}
	// klingVideoTierRates 是可灵视频「质量档 × 是否有声」的 ¥/s 矩阵（1 积分 = ¥1）。官方按维度
	// 组合定价、不分模型，故全部 video 模型共享同一档位矩阵：4K 档仅 v3 / v3-omni 可达、有声档仅
	// v2-6 / v3 / v3-omni 可达；turbo 与 video-o1 仅触达 std / pro 无声档。
	klingVideoTierRates = map[TierKey]float64{
		{Tier: "std", Audio: false}: 0.6,
		{Tier: "std", Audio: true}:  0.8,
		{Tier: "pro", Audio: false}: 0.8,
		{Tier: "pro", Audio: true}:  1.0,
		{Tier: "4k", Audio: false}:  3.0,
		{Tier: "4k", Audio: true}:   3.0,
	}
)

// builtinCatalogProviders 是内置供应商的定价目录数据（登记顺序与 provider 注册表一致）。
var builtinCatalogProviders = []catalogProvider{
	{
		id: "gemini-aistudio",
		models: []catalogModel{
			{id: "gemini-3.1-pro-preview", mediaType: catalogMediaText, pricing: tokenPricing("gemini-3.1-pro-preview", 2.00, 12.00, "USD")},
			{id: "gemini-3-flash-preview", mediaType: catalogMediaText, pricing: tokenPricing("gemini-3-flash-preview", 0.50, 3.00, "USD"), isDefault: true},
			{id: "gemini-3.1-flash-lite-preview", mediaType: catalogMediaText, pricing: tokenPricing("gemini-3.1-flash-lite-preview", 0.25, 1.50, "USD")},
			{id: "gemini-3-pro-image-preview", mediaType: catalogMediaImage, pricing: imageByResolutionPricing(
				"gemini-3-pro-image-preview", map[string]float64{"1K": 0.134, "2K": 0.134, "4K": 0.24}, "USD",
			)},
			{id: "gemini-3.1-flash-image-preview", mediaType: catalogMediaImage, pricing: imageByResolutionPricing(
				"gemini-3.1-flash-image-preview",
				map[string]float64{"512PX": 0.045, "1K": 0.067, "2K": 0.101, "4K": 0.151}, "USD",
			), isDefault: true},
			{id: "veo-3.1-generate-preview", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"veo-3.1-generate-preview", veoStandardRates, DimensionsResolutionAudio, "USD", "720p",
			)},
			{id: "veo-3.1-fast-generate-preview", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"veo-3.1-fast-generate-preview", veoFastRates, DimensionsResolutionAudio, "USD", "720p",
			)},
			{id: "veo-3.1-lite-generate-preview", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"veo-3.1-lite-generate-preview", veoLiteRates, DimensionsResolutionAudio, "USD", "720p",
			), isDefault: true},
		},
	},
	{
		id: "gemini-vertex",
		models: []catalogModel{
			{id: "gemini-3.1-pro-preview", mediaType: catalogMediaText, pricing: tokenPricing("gemini-3.1-pro-preview", 2.00, 12.00, "USD")},
			{id: "gemini-3-flash-preview", mediaType: catalogMediaText, pricing: tokenPricing("gemini-3-flash-preview", 0.50, 3.00, "USD"), isDefault: true},
			{id: "gemini-3.1-flash-lite-preview", mediaType: catalogMediaText, pricing: tokenPricing("gemini-3.1-flash-lite-preview", 0.25, 1.50, "USD")},
			{id: "gemini-3-pro-image-preview", mediaType: catalogMediaImage, pricing: imageByResolutionPricing(
				"gemini-3-pro-image-preview", map[string]float64{"1K": 0.134, "2K": 0.134, "4K": 0.24}, "USD",
			)},
			{id: "gemini-3.1-flash-image-preview", mediaType: catalogMediaImage, pricing: imageByResolutionPricing(
				"gemini-3.1-flash-image-preview",
				map[string]float64{"512PX": 0.045, "1K": 0.067, "2K": 0.101, "4K": 0.151}, "USD",
			), isDefault: true},
			{id: "veo-3.1-generate-001", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"veo-3.1-generate-001", veoStandardRates, DimensionsResolutionAudio, "USD", "720p",
			)},
			{id: "veo-3.1-fast-generate-001", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"veo-3.1-fast-generate-001", veoFastRates, DimensionsResolutionAudio, "USD", "720p",
			), isDefault: true},
		},
	},
	{
		id: "ark",
		models: []catalogModel{
			{id: "doubao-seed-2-0-pro-260215", mediaType: catalogMediaText, pricing: tokenPricing("doubao-seed-2-0-pro-260215", 3.20, 16.00, "CNY")},
			{id: "doubao-seed-2-0-lite-260215", mediaType: catalogMediaText, pricing: tokenPricing("doubao-seed-2-0-lite-260215", 0.60, 3.60, "CNY"), isDefault: true},
			{id: "doubao-seed-2-0-mini-260215", mediaType: catalogMediaText, pricing: tokenPricing("doubao-seed-2-0-mini-260215", 0.20, 2.00, "CNY")},
			{id: "doubao-seed-1-8-251228", mediaType: catalogMediaText, pricing: tokenPricing("doubao-seed-1-8-251228", 0.80, 2.00, "CNY")},
			{id: "doubao-seedream-5-0-lite-260128", mediaType: catalogMediaImage, pricing: imageFlatPricing("doubao-seedream-5-0-lite-260128", 0.22, "CNY"), isDefault: true},
			{id: "doubao-seedream-5-0-260128", mediaType: catalogMediaImage, pricing: imageFlatPricing("doubao-seedream-5-0-260128", 0.22, "CNY")},
			{id: "doubao-seedream-4-5-251128", mediaType: catalogMediaImage, pricing: imageFlatPricing("doubao-seedream-4-5-251128", 0.25, "CNY")},
			{id: "doubao-seedream-4-0-250828", mediaType: catalogMediaImage, pricing: imageFlatPricing("doubao-seedream-4-0-250828", 0.20, "CNY")},
			{id: "doubao-seedance-1-5-pro-251215", mediaType: catalogMediaVideo, pricing: tokenVideoPricing(
				"doubao-seedance-1-5-pro-251215", map[TierKey]float64{
					{Tier: "default", Audio: true}:  16.00,
					{Tier: "default", Audio: false}: 8.00,
					{Tier: "flex", Audio: true}:     8.00,
					{Tier: "flex", Audio: false}:    4.00,
				}, "CNY",
			)},
			{id: "doubao-seedance-2-0-260128", mediaType: catalogMediaVideo, pricing: tokenVideoPricing(
				"doubao-seedance-2-0-260128", map[TierKey]float64{
					{Tier: "default", Audio: true}:  46.00,
					{Tier: "default", Audio: false}: 46.00,
				}, "CNY",
			)},
			{id: "doubao-seedance-2-0-fast-260128", mediaType: catalogMediaVideo, pricing: tokenVideoPricing(
				"doubao-seedance-2-0-fast-260128", map[TierKey]float64{
					{Tier: "default", Audio: true}:  37.00,
					{Tier: "default", Audio: false}: 37.00,
				}, "CNY",
			)},
			{id: "doubao-seedance-2-0-mini-260615", mediaType: catalogMediaVideo, pricing: tokenVideoPricing(
				"doubao-seedance-2-0-mini-260615", map[TierKey]float64{
					{Tier: "default", Audio: true}:  23.00,
					{Tier: "default", Audio: false}: 23.00,
				}, "CNY",
			), isDefault: true},
			// 本表只覆盖 PerTokenVideo 消费的输出 usage；视频输入（参考视频转 token）另有单价，不计入。
			{id: "doubao-seedance-2-5-260628", mediaType: catalogMediaVideo, pricing: tokenVideoPricing(
				"doubao-seedance-2-5-260628", map[TierKey]float64{
					{Tier: "default", Audio: true}:  70.00,
					{Tier: "default", Audio: false}: 70.00,
				}, "CNY",
			)},
		},
	},
	{
		// Agent Plan 套餐价当前无独立费率表：全部条目沿用历史行为，按 Gemini 默认费率兜底。
		id: "ark-agent-plan",
		models: []catalogModel{
			{id: "doubao-seed-2.0-mini", mediaType: catalogMediaText},
			{id: "doubao-seed-2.0-lite", mediaType: catalogMediaText, isDefault: true},
			{id: "doubao-seed-2.0-pro", mediaType: catalogMediaText},
			{id: "doubao-seed-2.0-code", mediaType: catalogMediaText},
			{id: "deepseek-v4-flash", mediaType: catalogMediaText},
			{id: "deepseek-v4-pro", mediaType: catalogMediaText},
			{id: "glm-5.1", mediaType: catalogMediaText},
			{id: "kimi-k2.6", mediaType: catalogMediaText},
			{id: "minimax-m2.7", mediaType: catalogMediaText},
			{id: "doubao-seedream-5.0-lite", mediaType: catalogMediaImage, isDefault: true},
			{id: "doubao-seedance-1.5-pro", mediaType: catalogMediaVideo},
			{id: "doubao-seedance-2.0", mediaType: catalogMediaVideo},
			{id: "doubao-seedance-2.0-fast", mediaType: catalogMediaVideo},
			{id: "doubao-seedance-2.0-mini", mediaType: catalogMediaVideo, isDefault: true},
		},
	},
	{
		id: "grok",
		models: []catalogModel{
			{id: "grok-4.20-0309-reasoning", mediaType: catalogMediaText, pricing: tokenPricing("grok-4.20-0309-reasoning", 2.00, 6.00, "USD")},
			{id: "grok-4.20-0309-non-reasoning", mediaType: catalogMediaText, pricing: tokenPricing("grok-4.20-0309-non-reasoning", 2.00, 6.00, "USD")},
			{id: "grok-4-1-fast-reasoning", mediaType: catalogMediaText, pricing: tokenPricing("grok-4-1-fast-reasoning", 0.20, 0.50, "USD"), isDefault: true},
			{id: "grok-4-1-fast-non-reasoning", mediaType: catalogMediaText, pricing: tokenPricing("grok-4-1-fast-non-reasoning", 0.20, 0.50, "USD")},
			{id: "grok-imagine-image-pro", mediaType: catalogMediaImage, pricing: imageFlatPricing("grok-imagine-image-pro", 0.07, "USD")},
			{id: "grok-imagine-image", mediaType: catalogMediaImage, pricing: imageFlatPricing("grok-imagine-image", 0.02, "USD"), isDefault: true},
			// 不区分分辨率 / 音频的单一秒费率。
			{id: "grok-imagine-video", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"grok-imagine-video", map[MatrixKey]float64{{}: 0.050}, DimensionsFlat, "USD", "720p",
			), isDefault: true},
		},
	},
	{
		id: "openai",
		models: []catalogModel{
			{id: "gpt-5.5", mediaType: catalogMediaText, pricing: tokenPricing("gpt-5.5", 5.00, 30.00, "USD")},
			{id: "gpt-5.4", mediaType: catalogMediaText, pricing: tokenPricing("gpt-5.4", 2.50, 15.00, "USD")},
			{id: "gpt-5.4-mini", mediaType: catalogMediaText, pricing: tokenPricing("gpt-5.4-mini", 0.75, 4.50, "USD"), isDefault: true},
			{id: "gpt-5.4-nano", mediaType: catalogMediaText, pricing: tokenPricing("gpt-5.4-nano", 0.20, 1.25, "USD")},
			{id: "gpt-image-2", mediaType: catalogMediaImage, pricing: openAIImagePricing(
				"gpt-image-2",
				map[string]float64{
					"image_in": 8.0, "image_cached_in": 2.0, "image_out": 30.0,
					"text_in": 5.0, "text_cached_in": 1.25, "text_out": 0.0,
				},
				map[QualitySizeKey]float64{
					{Quality: "low", Size: "1024x1024"}:    0.006,
					{Quality: "low", Size: "1024x1792"}:    0.012,
					{Quality: "low", Size: "1792x1024"}:    0.012,
					{Quality: "medium", Size: "1024x1024"}: 0.053,
					{Quality: "medium", Size: "1024x1792"}: 0.106,
					{Quality: "medium", Size: "1792x1024"}: 0.106,
					{Quality: "high", Size: "1024x1024"}:   0.211,
					{Quality: "high", Size: "1024x1792"}:   0.317,
					{Quality: "high", Size: "1792x1024"}:   0.317,
				}, "USD",
			), isDefault: true},
			{id: "sora-2", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"sora-2", resOnlyRates(map[string]float64{"720p": 0.10}), DimensionsResolutionOnly, "USD", "720p",
			), isDefault: true},
			{id: "sora-2-pro", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"sora-2-pro", resOnlyRates(map[string]float64{"720p": 0.30, "1024p": 0.50, "1080p": 0.70}),
				DimensionsResolutionOnly, "USD", "720p",
			)},
		},
	},
	{
		// Vidu 计费以响应 credits 为准，费率逻辑在 calculateViduCost；此处统一委托标记。
		id: "vidu",
		models: []catalogModel{
			{id: "viduq2", mediaType: catalogMediaImage, pricing: viduDelegatePricing(), isDefault: true},
			{id: "viduq1", mediaType: catalogMediaImage, pricing: viduDelegatePricing()},
			{id: "viduq3-turbo", mediaType: catalogMediaVideo, pricing: viduDelegatePricing(), isDefault: true},
			{id: "viduq3-pro", mediaType: catalogMediaVideo, pricing: viduDelegatePricing()},
			{id: "viduq3", mediaType: catalogMediaVideo, pricing: viduDelegatePricing()},
			{id: "vidu2.0", mediaType: catalogMediaVideo, pricing: viduDelegatePricing()},
		},
	},
	{
		id: "dashscope",
		models: []catalogModel{
			{id: "qwen-plus", mediaType: catalogMediaText, pricing: tokenPricing("qwen-plus", 0.8, 2.0, "CNY"), isDefault: true},
			{id: "qwen3.6-plus", mediaType: catalogMediaText, pricing: tokenPricing("qwen3.6-plus", 2.0, 12.0, "CNY")},
			{id: "qwen3-max", mediaType: catalogMediaText, pricing: tokenPricing("qwen3-max", 2.5, 10.0, "CNY")},
			{id: "qwen3.7-max", mediaType: catalogMediaText, pricing: tokenPricing("qwen3.7-max", 12.0, 36.0, "CNY")},
			{id: "qwen3.6-flash", mediaType: catalogMediaText, pricing: tokenPricing("qwen3.6-flash", 1.2, 7.2, "CNY")},
			{id: "qwen-long", mediaType: catalogMediaText, pricing: tokenPricing("qwen-long", 0.5, 2.0, "CNY")},
			// qwen-image 融合系列：T2I + I2I 同价；qwen-image-edit 系列仅图生图。
			{id: "qwen-image-2.0", mediaType: catalogMediaImage, pricing: imageFlatPricing("qwen-image-2.0", 0.2, "CNY"), isDefault: true},
			{id: "qwen-image-2.0-pro", mediaType: catalogMediaImage, pricing: imageFlatPricing("qwen-image-2.0-pro", 0.5, "CNY")},
			{id: "qwen-image-edit-plus", mediaType: catalogMediaImage, pricing: imageFlatPricing("qwen-image-edit-plus", 0.2, "CNY")},
			{id: "qwen-image-edit-max", mediaType: catalogMediaImage, pricing: imageFlatPricing("qwen-image-edit-max", 0.5, "CNY")},
			{id: "wan2.7-image", mediaType: catalogMediaImage, pricing: imageFlatPricing("wan2.7-image", 0.2, "CNY")},
			{id: "wan2.7-image-pro", mediaType: catalogMediaImage, pricing: imageFlatPricing("wan2.7-image-pro", 0.5, "CNY")},
			// 视频秒价不随音轨变化，故音频不入计费维度。
			{id: "happyhorse-1.1-i2v", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"happyhorse-1.1-i2v", resOnlyRates(map[string]float64{"480p": 0.45, "720p": 0.9, "1080p": 1.2}),
				DimensionsResolutionOnly, "CNY", "720p",
			), isDefault: true},
			{id: "happyhorse-1.1-t2v", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"happyhorse-1.1-t2v", resOnlyRates(map[string]float64{"480p": 0.45, "720p": 0.9, "1080p": 1.2}),
				DimensionsResolutionOnly, "CNY", "720p",
			)},
			{id: "happyhorse-1.1-r2v", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"happyhorse-1.1-r2v", resOnlyRates(map[string]float64{"480p": 0.45, "720p": 0.9, "1080p": 1.2}),
				DimensionsResolutionOnly, "CNY", "720p",
			)},
			{id: "happyhorse-1.0-i2v", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"happyhorse-1.0-i2v", resOnlyRates(map[string]float64{"720p": 0.9, "1080p": 1.6}),
				DimensionsResolutionOnly, "CNY", "720p",
			)},
			{id: "happyhorse-1.0-t2v", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"happyhorse-1.0-t2v", resOnlyRates(map[string]float64{"720p": 0.9, "1080p": 1.6}),
				DimensionsResolutionOnly, "CNY", "720p",
			)},
			{id: "happyhorse-1.0-r2v", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"happyhorse-1.0-r2v", resOnlyRates(map[string]float64{"720p": 0.9, "1080p": 1.6}),
				DimensionsResolutionOnly, "CNY", "720p",
			)},
			{id: "wan2.7-i2v", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"wan2.7-i2v", resOnlyRates(map[string]float64{"720p": 0.6, "1080p": 1.0}),
				DimensionsResolutionOnly, "CNY", "720p",
			)},
			{id: "wan2.7-t2v", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"wan2.7-t2v", resOnlyRates(map[string]float64{"720p": 0.6, "1080p": 1.0}),
				DimensionsResolutionOnly, "CNY", "720p",
			)},
			{id: "wan2.7-r2v", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"wan2.7-r2v", resOnlyRates(map[string]float64{"720p": 0.6, "1080p": 1.0}),
				DimensionsResolutionOnly, "CNY", "720p",
			)},
			{id: "wan3.0-video", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"wan3.0-video", resOnlyRates(map[string]float64{"480p": 0.3, "720p": 0.6, "1080p": 1.2}),
				DimensionsResolutionOnly, "CNY", "720p",
			)},
			{id: "qwen3-tts-flash", mediaType: catalogMediaAudio, pricing: characterPricing("qwen3-tts-flash", 0.8, "CNY"), isDefault: true},
		},
	},
	{
		id: "minimax",
		models: []catalogModel{
			{id: "MiniMax-M3", mediaType: catalogMediaText, pricing: tokenPricing("MiniMax-M3", 2.1, 8.4, "CNY"), isDefault: true},
			{id: "MiniMax-M2.7", mediaType: catalogMediaText, pricing: tokenPricing("MiniMax-M2.7", 2.1, 8.4, "CNY")},
			{id: "image-01", mediaType: catalogMediaImage, pricing: imageFlatPricing("image-01", 0.025, "CNY"), isDefault: true},
			// H3 未显式指定分辨率（Auto）时实际下发 768P（无 720P 档位），结算须跟随同一默认，
			// 否则回落 720P 会因该档不存在而落空至 0。
			{id: "MiniMax-H3", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"MiniMax-H3", resOnlyRates(map[string]float64{"768p": 0.50, "2k": 0.80}),
				DimensionsResolutionOnly, "CNY", "768p",
			), isDefault: true},
			{id: "MiniMax-Hailuo-2.3", mediaType: catalogMediaVideo, pricing: videoBucketPricing(
				"MiniMax-Hailuo-2.3", map[BucketKey]float64{
					{Resolution: "768p", DurationSeconds: 6}:  2.0,
					{Resolution: "768p", DurationSeconds: 10}: 4.0,
					{Resolution: "1080p", DurationSeconds: 6}: 3.5,
				}, "CNY",
			)},
			{id: "MiniMax-Hailuo-2.3-Fast", mediaType: catalogMediaVideo, pricing: videoBucketPricing(
				"MiniMax-Hailuo-2.3-Fast", map[BucketKey]float64{
					{Resolution: "768p", DurationSeconds: 6}:  1.35,
					{Resolution: "768p", DurationSeconds: 10}: 2.25,
					{Resolution: "1080p", DurationSeconds: 6}: 2.31,
				}, "CNY",
			)},
			// S2V-01 固定输出 720P / 6s，请求不接受 resolution / duration；键到缺省档 768P / 6s
			// 求精确命中，任意分辨率漂移由最近档回落到唯一档。
			{id: "S2V-01", mediaType: catalogMediaVideo, pricing: videoBucketPricing(
				"S2V-01", map[BucketKey]float64{
					{Resolution: "768p", DurationSeconds: 6}: 3.0,
				}, "CNY",
			)},
		},
	},
	{
		id: "kling",
		models: []catalogModel{
			{id: "kling-v2-5-turbo", mediaType: catalogMediaVideo, pricing: secondTieredPricing("kling-v2-5-turbo", klingVideoTierRates, "CNY"), isDefault: true},
			{id: "kling-image-o1", mediaType: catalogMediaImage, pricing: imageFlatPricing("kling-image-o1", 0.2, "CNY"), isDefault: true},
			// v3-omni 各分辨率分档：1K / 2K 同价、4K 翻倍（图像 1 积分 = ¥0.025）。
			{id: "kling-v3-omni-image", mediaType: catalogMediaImage, pricing: imageByResolutionPricing(
				"kling-v3-omni-image", map[string]float64{"1K": 0.2, "2K": 0.2, "4K": 0.4}, "CNY",
			)},
			{id: "kling-v3", mediaType: catalogMediaVideo, pricing: secondTieredPricing("kling-v3", klingVideoTierRates, "CNY")},
			{id: "kling-v3-turbo", mediaType: catalogMediaVideo, pricing: secondTieredPricing("kling-v3-turbo", klingVideoTierRates, "CNY")},
			{id: "kling-v3-omni", mediaType: catalogMediaVideo, pricing: secondTieredPricing("kling-v3-omni", klingVideoTierRates, "CNY")},
			{id: "kling-v2-6", mediaType: catalogMediaVideo, pricing: secondTieredPricing("kling-v2-6", klingVideoTierRates, "CNY")},
			{id: "kling-video-o1", mediaType: catalogMediaVideo, pricing: secondTieredPricing("kling-video-o1", klingVideoTierRates, "CNY")},
		},
	},
	{
		id: "agnes",
		models: []catalogModel{
			{id: "agnes-2.0-flash", mediaType: catalogMediaText, pricing: tokenPricing("agnes-2.0-flash", 0.03, 0.15, "USD")},
			{id: "agnes-2.5-flash", mediaType: catalogMediaText, pricing: tokenPricing("agnes-2.5-flash", 0.03, 0.15, "USD"), isDefault: true},
			// 官方原价；当前促销 $0 不建模。
			{id: "agnes-image-2.1-flash", mediaType: catalogMediaImage, pricing: imageFlatPricing("agnes-image-2.1-flash", 0.003, "USD")},
			{id: "agnes-image-2.5-flash", mediaType: catalogMediaImage, pricing: imageFlatPricing("agnes-image-2.5-flash", 0.003, "USD"), isDefault: true},
			// 视频为 flat 按秒、与分辨率 / 音频无关。
			{id: "agnes-video-v2.0", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"agnes-video-v2.0", map[MatrixKey]float64{{}: 0.005}, DimensionsFlat, "USD", "720p",
			)},
			{id: "agnes-video-2.5", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"agnes-video-2.5", map[MatrixKey]float64{{}: 0.005}, DimensionsFlat, "USD", "720p",
			), isDefault: true},
			{id: "agnes-video-2.5-flash", mediaType: catalogMediaVideo, pricing: secondMatrixPricing(
				"agnes-video-2.5-flash", map[MatrixKey]float64{{}: 0.005}, DimensionsFlat, "USD", "720p",
			)},
		},
	},
}

// builtinCatalog 是内置供应商定价目录的共享实例（装载期构建一次，只读）。
var builtinCatalog = buildCatalog(builtinCatalogProviders)

// BuiltinCatalog 返回内置供应商的定价目录，可直接传给 NewCalculator / Ledger 的计费装配点。
//
// 返回的 MapCatalog 与包内共享存储同源：调用方只读，不得原地增删（查找只做读操作）。
// 目录只含内置供应商；自定义供应商与未登记的中转站不经此表（其定价由调用方按 DB 声明结算）。
func BuiltinCatalog() MapCatalog { return builtinCatalog }

// buildCatalog 按登记顺序把目录数据整理成查找所需的 provider 视图：模型表按 ID 索引，
// 默认模型按媒体类型取该媒体下标记 default 的条目。
func buildCatalog(providers []catalogProvider) MapCatalog {
	catalog := make(MapCatalog, len(providers))
	for _, providerEntry := range providers {
		meta := ProviderMeta{
			Models:       make(map[string]ModelInfo, len(providerEntry.models)),
			DefaultModel: map[string]string{},
		}
		for _, model := range providerEntry.models {
			meta.Models[model.id] = ModelInfo{MediaType: model.mediaType, Pricing: model.pricing}
			if model.isDefault {
				meta.DefaultModel[model.mediaType] = model.id
			}
		}
		catalog[providerEntry.id] = meta
	}
	return catalog
}
