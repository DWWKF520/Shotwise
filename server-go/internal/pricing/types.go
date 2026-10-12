// Package pricing 是计费定价的计算侧：定价声明（Pricing）按形状 kind 派发计算，
// 查找（LookupPricing）复刻 provider 回落链，费用入口（Calculator）不含 provider 分支。
//
// 与 Python 侧 lib/pricing + lib/cost_calculator.py 对应。本包不读 DB、不发 HTTP：
// 供应商目录由 caller 注入（Catalog），定价数据本身仍是 registry 的单一真相源。
package pricing

// Kind 是定价形状的判别标签（与 Python 侧每种 frozen dataclass 的 kind 字段一一对应）。
type Kind string

const (
	KindPerToken             Kind = "per_token"
	KindPerImageFlat         Kind = "per_image_flat"
	KindPerImageByResolution Kind = "per_image_by_resolution"
	KindPerImageOpenAIToken  Kind = "per_image_openai_token"
	KindPerSecondMatrix      Kind = "per_second_matrix"
	KindPerSecondTiered      Kind = "per_second_tiered"
	KindPerVideoBucket       Kind = "per_video_bucket"
	KindPerTokenVideo        Kind = "per_token_video"
	KindPerCharacter         Kind = "per_character"
	KindViduDelegate         Kind = "vidu_delegate"
)

// CharacterUnit 是按字符计费（TTS）的计价单位：费率一律以「每万字符」声明。
const CharacterUnit = 10_000

// AudioDimension 是 per_second_matrix 费率键上的音频维度。
// Python 侧用 bool|None 表达三态；Go 的 map 键不能用指针（同值不同指针不相等），故显式枚举。
type AudioDimension uint8

const (
	// AudioUnspecified 对应 Python 的 None：该档不区分是否生成音频。
	AudioUnspecified AudioDimension = iota
	AudioOn
	AudioOff
)

// AudioDim 把「是否生成音频」转成费率键维度。
func AudioDim(generateAudio bool) AudioDimension {
	if generateAudio {
		return AudioOn
	}
	return AudioOff
}

// MatrixKey 是 per_second_matrix 的费率键：分辨率小写 + 音频维度。
type MatrixKey struct {
	Resolution string
	Audio      AudioDimension
}

// TierKey 是 per_second_tiered 与 per_token_video 的费率键：质量档（service_tier）+ 是否有声。
type TierKey struct {
	Tier  string
	Audio bool
}

// BucketKey 是 per_video_bucket 的费率键：分辨率小写 + 时长秒。
type BucketKey struct {
	Resolution      string
	DurationSeconds int
}

// QualitySizeKey 是 per_image_openai_token 静态兜底表的费率键。
type QualitySizeKey struct {
	Quality string
	Size    string
}

// Dimensions 是 per_second_matrix 的维度组合选择。
type Dimensions string

const (
	DimensionsResolutionAudio Dimensions = "resolution_audio"
	DimensionsResolutionOnly  Dimensions = "resolution_only"
	DimensionsFlat            Dimensions = "flat"
)

// Pricing 是一条定价声明：Kind 决定哪张费率表生效，其余表忽略。
//
// 费率表的键口径与 Python 侧 types.py 同名 dataclass 的 docstring 一致：
//   - 所有表的键先按请求 model 查，未命中回落 DefaultModel；
//   - ResolutionRates / BucketKey 的分辨率键在查表前统一 .upper() / .lower() 归一。
type Pricing struct {
	Kind     Kind
	Currency string

	// TokenRates：per_token 用 {input, output} 每百万费率；per_image_openai_token 的 token 主路径
	// 用 {image_in, image_out, text_in, text_out} 每百万费率。
	TokenRates map[string]map[string]float64
	// FlatRates：per_image_flat（每张价）与 per_character（每万字符价）共用。
	FlatRates map[string]float64
	// ResolutionRates：per_image_by_resolution，分辨率键以大写形态存储。
	ResolutionRates map[string]map[string]float64
	// FallbackRates：per_image_openai_token 在 SDK 不返回 usage 时的 (quality, size) 静态兜底表。
	FallbackRates map[string]map[QualitySizeKey]float64
	// MatrixRates：per_second_matrix。
	MatrixRates map[string]map[MatrixKey]float64
	// TieredRates：per_second_tiered（每秒价）与 per_token_video（每百万 token 价）。
	TieredRates map[string]map[TierKey]float64
	// BucketRates：per_video_bucket，金额 = 查表 flat_price，与秒数不成比例。
	BucketRates map[string]map[BucketKey]float64

	// DefaultModel 是各费率表未命中请求 model 时的回落模型。
	DefaultModel string
	// Dimensions 与 DefaultResolution 仅 per_second_matrix 使用；
	// DefaultResolution 为空视为 "720p"，须与该模型在分辨率未显式指定时实际下发的档位一致。
	Dimensions        Dimensions
	DefaultResolution string
}

// NewPerToken 构造按 token 计费的声明。
func NewPerToken(rates map[string]map[string]float64, defaultModel, currency string) Pricing {
	return Pricing{Kind: KindPerToken, TokenRates: rates, DefaultModel: defaultModel, Currency: currency}
}

// NewPerImageFlat 构造按张计费的声明，单价与分辨率无关。
func NewPerImageFlat(rates map[string]float64, defaultModel, currency string) Pricing {
	return Pricing{Kind: KindPerImageFlat, FlatRates: rates, DefaultModel: defaultModel, Currency: currency}
}

// NewPerImageByResolution 构造按张计费、单价随分辨率档位变化的声明。
func NewPerImageByResolution(rates map[string]map[string]float64, defaultModel, currency string) Pricing {
	return Pricing{Kind: KindPerImageByResolution, ResolutionRates: rates, DefaultModel: defaultModel, Currency: currency}
}

// NewPerImageOpenAIToken 构造 OpenAI 图片声明：有 usage 走 TokenRates，否则走 FallbackRates。
func NewPerImageOpenAIToken(
	tokenRates map[string]map[string]float64,
	fallbackRates map[string]map[QualitySizeKey]float64,
	defaultModel, currency string,
) Pricing {
	return Pricing{
		Kind:          KindPerImageOpenAIToken,
		TokenRates:    tokenRates,
		FallbackRates: fallbackRates,
		DefaultModel:  defaultModel,
		Currency:      currency,
	}
}

// NewPerSecondMatrix 构造按秒计费、单价由维度组合查表的声明。
func NewPerSecondMatrix(
	rates map[string]map[MatrixKey]float64,
	defaultModel string,
	dimensions Dimensions,
	currency, defaultResolution string,
) Pricing {
	return Pricing{
		Kind:              KindPerSecondMatrix,
		MatrixRates:       rates,
		DefaultModel:      defaultModel,
		Dimensions:        dimensions,
		Currency:          currency,
		DefaultResolution: defaultResolution,
	}
}

// NewPerSecondTiered 构造按「质量档 × 是否有声」定每秒价的声明。
func NewPerSecondTiered(rates map[string]map[TierKey]float64, defaultModel, currency string) Pricing {
	return Pricing{Kind: KindPerSecondTiered, TieredRates: rates, DefaultModel: defaultModel, Currency: currency}
}

// NewPerVideoBucket 构造按 (分辨率, 时长) 离散档计费的声明。
func NewPerVideoBucket(rates map[string]map[BucketKey]float64, defaultModel, currency string) Pricing {
	return Pricing{Kind: KindPerVideoBucket, BucketRates: rates, DefaultModel: defaultModel, Currency: currency}
}

// NewPerTokenVideo 构造按 token 计费的视频声明（service_tier × 是否生成音频查每百万 token 价）。
func NewPerTokenVideo(rates map[string]map[TierKey]float64, defaultModel, currency string) Pricing {
	return Pricing{Kind: KindPerTokenVideo, TieredRates: rates, DefaultModel: defaultModel, Currency: currency}
}

// NewPerCharacter 构造按字符计费（TTS）的声明，费率单位为每万字符。
func NewPerCharacter(rates map[string]float64, defaultModel, currency string) Pricing {
	return Pricing{Kind: KindPerCharacter, FlatRates: rates, DefaultModel: defaultModel, Currency: currency}
}

// NewViduDelegate 构造委托标记：实际费率在 vidu 的 credits 计费里，本声明不携带费率。
func NewViduDelegate(currency string) Pricing {
	return Pricing{Kind: KindViduDelegate, Currency: currency}
}

// Params 承载一次计费所需的全部维度；各 kind 策略按需取用。
//
// 字段与 Python 侧 PricingParams 逐项对应，指针字段区分「未提供」（nil）与显式 0：
// duration 的 nil/0 在 per_second_* 与 per_video_bucket 上语义不同，不可合并。
type Params struct {
	CallType    string
	Model       string
	Resolution  string
	AspectRatio string

	// DurationSeconds 为 nil 时按 8 秒（per_second_*）或 6 秒（per_video_bucket）兜底；
	// 显式 0 是「真实零时长」，不被兜底覆盖。
	DurationSeconds *int
	// GenerateAudio 为 nil 时按有声（true）计。
	GenerateAudio *bool

	UsageTokens *int
	// ServiceTier 为空串等价 "default"。
	ServiceTier string

	InputTokens  *int
	OutputTokens *int

	Quality string
	Size    string

	ImageInputTokens  *int
	ImageOutputTokens *int
	TextInputTokens   *int
	TextOutputTokens  *int

	// N 是图片张数；未设置（≤0）按 1 计——零值漏填不该把金额静默乘成 0。
	N int
}

// audio 返回生效的有声标志：未提供按 Python 侧 PricingParams 的默认 True。
func (p Params) audio() bool { return p.GenerateAudio == nil || *p.GenerateAudio }

// serviceTier 返回生效的服务档位：未提供按 "default"。
func (p Params) serviceTier() string {
	if p.ServiceTier == "" {
		return "default"
	}
	return p.ServiceTier
}

// imageCount 返回生效的图片张数：未设置按 1。
func (p Params) imageCount() int {
	if p.N < 1 {
		return 1
	}
	return p.N
}

// IntPtr 便于构造可选维度（测试与调用点共用）。
func IntPtr(v int) *int { return &v }

// Float64Ptr 便于构造可选金额。
func Float64Ptr(v float64) *float64 { return &v }

// BoolPtr 便于构造可选布尔。
func BoolPtr(v bool) *bool { return &v }

// IntValue 读可选整数，nil 按 0。
func IntValue(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
