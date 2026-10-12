package pricing

import (
	"fmt"
	"log/slog"
	"math"
	"strings"
)

// (resolution, duration) 离散档计费里 duration 缺省时的兜底秒数。海螺最短档为 6s，
// 缺省按最短档计避免高估；真实请求恒带 duration，仅防御性兜底。
const defaultBucketDuration = 6

// per_second_tiered 档位派生常量：service_tier "default" 归一到 "std"，4K 分辨率独立成 "4k" 档。
const (
	tieredDefaultTier   = "std"
	tiered4KResolution  = "4k"
	matrixDefaultRes    = "1080p"
	bucketDefaultRes    = "768p"
	imageDefaultRes     = "1K"
	openAIDefaultSize   = "1024x1024"
	openAIDefaultQual   = "medium"
	openAIFallbackPrice = 0.034
	tokenVideoFallback  = 16.00
)

// Calculate 按定价声明的 Kind 派发到对应策略，返回 (金额, 币种)。
//
// 未知 Kind（含零值声明）返回错误而非静默计零：计费静默归零比失败更危险。
func Calculate(p Pricing, params Params) (float64, string, error) {
	switch p.Kind {
	case KindPerToken:
		return perToken(p, params), p.Currency, nil
	case KindPerImageFlat:
		return perImageFlat(p, params), p.Currency, nil
	case KindPerImageByResolution:
		return perImageByResolution(p, params), p.Currency, nil
	case KindPerImageOpenAIToken:
		return perImageOpenAIToken(p, params), p.Currency, nil
	case KindPerSecondMatrix:
		return perSecondMatrix(p, params), p.Currency, nil
	case KindPerSecondTiered:
		return perSecondTiered(p, params), p.Currency, nil
	case KindPerVideoBucket:
		return perVideoBucket(p, params), p.Currency, nil
	case KindPerTokenVideo:
		return perTokenVideo(p, params), p.Currency, nil
	case KindPerCharacter:
		return perCharacter(p, params), p.Currency, nil
	case KindViduDelegate:
		amount, currency := calculateViduCost(params)
		return amount, currency, nil
	}
	return 0, p.Currency, fmt.Errorf("pricing: unknown pricing kind %q", p.Kind)
}

// ratesFor 取 model 的费率表，未命中回落 DefaultModel 的表（可能为 nil，读零值）。
func ratesFor[K comparable](tables map[string]map[K]float64, model, defaultModel string) map[K]float64 {
	if rate, ok := tables[model]; ok {
		return rate
	}
	return tables[defaultModel]
}

// effectiveModel 求生效模型名：请求 model 为空按声明的默认模型。
func effectiveModel(requested, defaultModel string) string {
	if requested != "" {
		return requested
	}
	return defaultModel
}

func perToken(p Pricing, params Params) float64 {
	rates := ratesFor(p.TokenRates, effectiveModel(params.Model, p.DefaultModel), p.DefaultModel)
	return (float64(IntValue(params.InputTokens))*rates["input"] + float64(IntValue(params.OutputTokens))*rates["output"]) / 1_000_000
}

func perImageFlat(p Pricing, params Params) float64 {
	model := effectiveModel(params.Model, p.DefaultModel)
	perImage, ok := p.FlatRates[model]
	if !ok {
		perImage = p.FlatRates[p.DefaultModel]
	}
	return perImage * float64(params.imageCount())
}

func perImageByResolution(p Pricing, params Params) float64 {
	model := effectiveModel(params.Model, p.DefaultModel)
	modelCosts := ratesFor(p.ResolutionRates, model, p.DefaultModel)
	// 用 ok 而非零值判断：模型可显式声明 0.0 免费档，0.0 不应被当作缺失而回落默认费率。
	defaultCost, ok := modelCosts[imageDefaultRes]
	if !ok {
		defaultCost = p.ResolutionRates[p.DefaultModel][imageDefaultRes]
	}
	resolution := params.Resolution
	if resolution == "" {
		resolution = imageDefaultRes
	}
	perImage, ok := modelCosts[strings.ToUpper(resolution)]
	if !ok {
		perImage = defaultCost
	}
	return perImage * float64(params.imageCount())
}

func perImageOpenAIToken(p Pricing, params Params) float64 {
	model := effectiveModel(params.Model, p.DefaultModel)
	hasUsage := params.ImageInputTokens != nil || params.ImageOutputTokens != nil ||
		params.TextInputTokens != nil || params.TextOutputTokens != nil
	if hasUsage {
		rates := ratesFor(p.TokenRates, model, p.DefaultModel)
		return (float64(IntValue(params.ImageInputTokens))*rates["image_in"] +
			float64(IntValue(params.ImageOutputTokens))*rates["image_out"] +
			float64(IntValue(params.TextInputTokens))*rates["text_in"] +
			float64(IntValue(params.TextOutputTokens))*rates["text_out"]) / 1_000_000
	}

	// 主路径按 token 计费已覆盖绝大多数；此处为 SDK 不返回 usage 的兜底。不按 (resolution,
	// aspect_ratio) 反查 size（已废弃）：size 缺失即落默认档，计费与输出尺寸解耦。
	quality := params.Quality
	if quality == "" {
		quality = openAIDefaultQual
	}
	size := params.Size
	if size == "" {
		size = openAIDefaultSize
	}
	modelCosts := ratesFor(p.FallbackRates, model, p.DefaultModel)
	perImage, ok := modelCosts[QualitySizeKey{Quality: quality, Size: size}]
	if !ok {
		if v, found := modelCosts[QualitySizeKey{Quality: quality, Size: openAIDefaultSize}]; found {
			perImage = v
		} else if v, found := modelCosts[QualitySizeKey{Quality: openAIDefaultQual, Size: openAIDefaultSize}]; found {
			perImage = v
		} else {
			perImage = openAIFallbackPrice
		}
	}
	return perImage * float64(params.imageCount())
}

func perSecondMatrix(p Pricing, params Params) float64 {
	model := effectiveModel(params.Model, p.DefaultModel)
	modelCosts := ratesFor(p.MatrixRates, model, p.DefaultModel)
	// 真实 0 秒（如参考模式全零时长聚合）保持 0；缺省（nil）才按 8 秒兜底。
	duration := 8
	if params.DurationSeconds != nil {
		duration = *params.DurationSeconds
	}

	switch p.Dimensions {
	case DimensionsResolutionAudio:
		resolution := strings.ToLower(firstNonEmpty(params.Resolution, matrixDefaultRes))
		// 同上：0.0 免费档不应被 ok 之外的判断当作缺失而回落默认模型费率。
		fallback, ok := modelCosts[MatrixKey{Resolution: matrixDefaultRes, Audio: AudioOn}]
		if !ok {
			fallback = p.MatrixRates[p.DefaultModel][MatrixKey{Resolution: matrixDefaultRes, Audio: AudioOn}]
		}
		perSecond, ok := modelCosts[MatrixKey{Resolution: resolution, Audio: AudioDim(params.audio())}]
		if !ok {
			perSecond = fallback
		}
		return float64(duration) * perSecond
	case DimensionsResolutionOnly:
		defaultResolution := strings.ToLower(p.effectiveDefaultResolution())
		resolution := strings.ToLower(firstNonEmpty(params.Resolution, defaultResolution))
		perSecond, ok := modelCosts[MatrixKey{Resolution: resolution, Audio: AudioUnspecified}]
		if !ok {
			perSecond = modelCosts[MatrixKey{Resolution: defaultResolution, Audio: AudioUnspecified}]
		}
		return float64(duration) * perSecond
	default: // flat
		return float64(duration) * modelCosts[MatrixKey{Audio: AudioUnspecified}]
	}
}

func perSecondTiered(p Pricing, params Params) float64 {
	model := effectiveModel(params.Model, p.DefaultModel)
	modelRates := ratesFor(p.TieredRates, model, p.DefaultModel)
	// 真实 0 秒保持 0；缺省（nil）按 8 秒兜底（与 per_second_matrix 对齐）。
	duration := 8
	if params.DurationSeconds != nil {
		duration = *params.DurationSeconds
	}

	// 档位派生：4K 分辨率独立成档（忽略 std/pro），否则取 service_tier（"default"→"std"）。
	tier := tieredDefaultTier
	if resolution := strings.ToLower(params.Resolution); resolution == tiered4KResolution {
		tier = tiered4KResolution
	} else if serviceTier := strings.ToLower(params.serviceTier()); serviceTier != "default" {
		tier = serviceTier
	}

	audio := params.audio()
	perSecond, ok := modelRates[TierKey{Tier: tier, Audio: audio}]
	if !ok {
		// 未命中档：回落该 model 的 std 档（取相同音频维度，其次无声），并 WARN——档表与请求
		// 漂移（如未知 service_tier）的可观测信号。
		if v, found := modelRates[TierKey{Tier: tieredDefaultTier, Audio: audio}]; found {
			perSecond = v
		} else {
			perSecond = modelRates[TierKey{Tier: tieredDefaultTier}]
		}
		slog.Warn("per_second_tiered 未命中档，回落 std 档", "model", model, "tier", tier, "audio", audio)
	}
	return float64(duration) * perSecond
}

func perVideoBucket(p Pricing, params Params) float64 {
	model := effectiveModel(params.Model, p.DefaultModel)
	buckets := ratesFor(p.BucketRates, model, p.DefaultModel)
	resolution := strings.ToLower(firstNonEmpty(params.Resolution, bucketDefaultRes))
	duration := defaultBucketDuration
	if params.DurationSeconds != nil {
		duration = *params.DurationSeconds
	}

	if exact, ok := buckets[BucketKey{Resolution: resolution, DurationSeconds: duration}]; ok {
		return exact
	}

	// 未命中档：先在同分辨率档内取 |时长差| 最小者，无同分辨率档再在全部档内取最近。
	// tie-break 链保证完全确定性（不依赖 map 迭代序）：|时长差| → 更小时长 → 更低价
	// （跨分辨率回落时同距离取便宜档，避免高估）→ 分辨率名（同价时兜底，保证全序）。
	candidates := make([]BucketKey, 0, len(buckets))
	for key := range buckets {
		if key.Resolution == resolution {
			candidates = append(candidates, key)
		}
	}
	if len(candidates) == 0 {
		for key := range buckets {
			candidates = append(candidates, key)
		}
	}
	nearest := bucketNearest(candidates, duration, buckets)
	slog.Warn("per_video_bucket 未命中档，回落最近档",
		"model", model, "resolution", resolution, "duration_seconds", duration, "nearest", nearest)
	return buckets[nearest]
}

func perTokenVideo(p Pricing, params Params) float64 {
	model := effectiveModel(params.Model, p.DefaultModel)
	modelCosts := ratesFor(p.TieredRates, model, p.DefaultModel)
	price, ok := modelCosts[TierKey{Tier: params.serviceTier(), Audio: params.audio()}]
	if !ok {
		if v, found := modelCosts[TierKey{Tier: "default", Audio: true}]; found {
			price = v
		} else {
			price = tokenVideoFallback
		}
	}
	return float64(IntValue(params.UsageTokens)) / 1_000_000 * price
}

func perCharacter(p Pricing, params Params) float64 {
	// 字符数复用通用计数字段 usage_tokens 承载（与 per_token_video 同模式，免新增 DB 列）。
	model := effectiveModel(params.Model, p.DefaultModel)
	rate, ok := p.FlatRates[model]
	if !ok {
		rate = p.FlatRates[p.DefaultModel]
	}
	return float64(IntValue(params.UsageTokens)) / CharacterUnit * rate
}

// effectiveDefaultResolution 返回 per_second_matrix 的分辨率回落档，未声明按 720p。
func (p Pricing) effectiveDefaultResolution() string {
	if p.DefaultResolution != "" {
		return p.DefaultResolution
	}
	return "720p"
}

// bucketNearest 按 |时长差| → 更小时长 → 更低价 → 分辨率名的字典序取最近档。
func bucketNearest(candidates []BucketKey, duration int, prices map[BucketKey]float64) BucketKey {
	nearest := candidates[0]
	for _, key := range candidates[1:] {
		if bucketLess(key, nearest, duration, prices) {
			nearest = key
		}
	}
	return nearest
}

func bucketLess(a, b BucketKey, duration int, prices map[BucketKey]float64) bool {
	diffA, diffB := math.Abs(float64(a.DurationSeconds-duration)), math.Abs(float64(b.DurationSeconds-duration))
	if diffA != diffB {
		return diffA < diffB
	}
	if a.DurationSeconds != b.DurationSeconds {
		return a.DurationSeconds < b.DurationSeconds
	}
	if prices[a] != prices[b] {
		return prices[a] < prices[b]
	}
	return a.Resolution < b.Resolution
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Vidu（生数科技）计费：优先用响应 credits（=usage_tokens，0 是合法值），否则按参考表估算。
// 数值为参考估算，并未逐项对账官方 pricing 页。
const (
	// ViduCreditToCNY 是积分折算费率：Vidu 标准积分包 500 元 / 16000 积分。
	ViduCreditToCNY = 0.03125
	// DefaultViduVideoModel / DefaultViduImageModel 是参考表未命中模型名时的回落。
	DefaultViduVideoModel = "viduq3-turbo"
	DefaultViduImageModel = "viduq2"
)

var viduVideoCreditsPerSecond = map[string]map[string]float64{
	// q3 系列（估算）
	"viduq3-pro":      {"540p": 10, "720p": 25, "1080p": 30},
	"viduq3-turbo":    {"540p": 8, "720p": 12, "1080p": 14},
	"viduq3-pro-fast": {"540p": 8, "720p": 12, "1080p": 14},
	"viduq3":          {"540p": 10, "720p": 25, "1080p": 30},
	"viduq3-mix":      {"540p": 10, "720p": 25, "1080p": 30},
	// q2 / q1 / 2.0
	"vidu2.0": {"360p": 5, "720p": 10, "1080p": 25},
}

// viduImageTier 是图片按张计费的档位。用有序切片而非 map：未命中档回落「表内第一个档」，
// map 迭代序不可依赖。
type viduImageTier struct {
	Resolution string
	Credits    float64
}

// viduImageCredits 图片按张计费（积分/张），仅作 fallback；实际从响应 credits 取。
var viduImageCredits = map[string][]viduImageTier{
	"viduq2": {{Resolution: "1080p", Credits: 8}, {Resolution: "2k", Credits: 12}, {Resolution: "4k", Credits: 20}},
	"viduq1": {{Resolution: "1080p", Credits: 20}},
}

// calculateViduCost 是 vidu_delegate 形状的实际费率入口。
func calculateViduCost(params Params) (float64, string) {
	// credits=0 是合法值（如 off_peak / 平台促销），不能被当成「无 credits」。
	if params.UsageTokens != nil && *params.UsageTokens >= 0 {
		return float64(*params.UsageTokens) * ViduCreditToCNY, "CNY"
	}

	resKey := strings.ToLower(params.Resolution)

	switch params.CallType {
	case "image":
		model := firstNonEmpty(params.Model, DefaultViduImageModel)
		tiers, ok := viduImageCredits[model]
		if !ok {
			tiers = viduImageCredits[DefaultViduImageModel]
		}
		credits := tiers[0].Credits
		for _, tier := range tiers {
			if tier.Resolution == firstNonEmpty(resKey, "1080p") {
				credits = tier.Credits
				break
			}
		}
		return credits * ViduCreditToCNY, "CNY"
	case "video":
		model := firstNonEmpty(params.Model, DefaultViduVideoModel)
		perSecondTable, ok := viduVideoCreditsPerSecond[model]
		if !ok {
			perSecondTable = viduVideoCreditsPerSecond[DefaultViduVideoModel]
		}
		perSecond := float64(25)
		if v, ok := perSecondTable["720p"]; ok {
			perSecond = v
		}
		if v, ok := perSecondTable[firstNonEmpty(resKey, "720p")]; ok {
			perSecond = v
		}
		duration := 5
		if params.DurationSeconds != nil && *params.DurationSeconds != 0 {
			duration = *params.DurationSeconds
		}
		return float64(duration) * perSecond * ViduCreditToCNY, "CNY"
	}
	return 0.0, "CNY"
}
