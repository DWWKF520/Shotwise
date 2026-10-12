package pricing

import (
	"errors"
	"strings"
	"time"
)

// 默认模型常量：registry 之外的地方（gemini 共享模块、任务兜底）直接读取。
const (
	DefaultImageModel = "gemini-3.1-flash-image-preview"
	DefaultVideoModel = "veo-3.1-lite-generate-preview"
	// ArkTokensPerSecondEstimate 是 Ark 生成视频的 token/s 近似常量，仅用于费用预估口径。
	ArkTokensPerSecondEstimate = 60_000
	// customProviderPrefix 是自定义供应商 provider_id 的前缀（custom-<db id>）。
	customProviderPrefix = "custom-"
	// MaxBilledDurationSeconds 是计费时长合理上限（24 小时）：写入层是全部 backend 落账的最后
	// 防线，超限的计费时长视同未提供、回落请求时长，防超大数值写入 DB Integer 列溢出。
	MaxBilledDurationSeconds = 86400
	// ProviderGemini 是记账行缺失 provider 时的历史兜底名；裸 "gemini" 走 Gemini 家族默认费率。
	ProviderGemini = "gemini"
)

// Calculator 是费用计算入口：按定价声明的 kind 派发，不含 provider 分支。
type Calculator struct{ catalog Catalog }

// NewCalculator 构造费用计算器；catalog 为供应商目录（registry 的只读视图）。
func NewCalculator(catalog Catalog) *Calculator { return &Calculator{catalog: catalog} }

// CustomPrice 是调用方预查的自定义供应商价格；PriceInput 为 nil 表示无价可依，按零费用结算。
type CustomPrice struct {
	PriceInput  *float64
	PriceOutput *float64
	Currency    string
}

// CostOptions 是「申报时刻」的价格来源与口径开关，与 Params（生效定价输入）刻意分层。
type CostOptions struct {
	// CustomPrice 仅自定义供应商使用，由调用方预先查库传入。
	CustomPrice *CustomPrice
	// EstimateOnly 置 true 表示调用方明确只是预估（非真实调用结算），允许对缺失的 usage_tokens
	// 做近似换算兜底。真实调用的费用结算必须保持 false——provider 成功响应但漏报 usage 是真实的
	// 数据缺陷，结算侧应如实按 0 处理，不能用预估口径的近似值掩盖。
	EstimateOnly bool
}

// CalculateCost 是按 (provider, call_type, params) 计算一次调用费用的统一入口。
func (c *Calculator) CalculateCost(provider string, params Params, opts CostOptions) (float64, string, error) {
	if IsCustomProvider(provider) {
		amount, currency := customCost(params, opts)
		return amount, currency, nil
	}

	// 文本无 token 数据时无从计费，保留早返回。
	if params.CallType == "text" && params.InputTokens == nil {
		return 0.0, "USD", nil
	}

	declaration, err := LookupPricing(c.catalog, provider, params.Model, params.CallType)
	if err != nil {
		return 0, "", err
	}
	// 按秒计费的视频：单次实时调用无/0 时长时按默认 8 秒计（历史行为）。参考模式聚合走
	// EstimateReferenceVideoCost，传真实累计时长（可为 0），不经此默认。
	switch declaration.Kind {
	case KindPerSecondMatrix, KindPerSecondTiered:
		if params.DurationSeconds == nil || *params.DurationSeconds == 0 {
			params.DurationSeconds = IntPtr(8)
		}
	}
	// 按 token 计费的视频（Ark/Seedance）：仅预估场景下，调用方只传了时长、未预先换算 token 时
	// 按估算近似值换算，否则该模型的预估恒为 0。
	if opts.EstimateOnly && declaration.Kind == KindPerTokenVideo && params.UsageTokens == nil &&
		params.DurationSeconds != nil && *params.DurationSeconds != 0 {
		params.UsageTokens = IntPtr(*params.DurationSeconds * ArkTokensPerSecondEstimate)
	}
	return Calculate(declaration, params)
}

// ReferenceVideoEstimate 是参考模式视频费用聚合的入参。
type ReferenceVideoEstimate struct {
	UnitDurationsSeconds []int
	Provider             string
	Model                string
	Resolution           string
	// GenerateAudio 为 nil 按有声计。
	GenerateAudio *bool
	ServiceTier   string
}

// EstimateReferenceVideoCost 聚合参考模式一集的视频费用：sum over units of (duration × 单价)。
//
// token 计费的视频（Ark）按 duration × ArkTokensPerSecondEstimate 近似换算 token；其余按秒
// 计费的模型直接用累计时长。空列表返回该定价声明自带的币种；累计时长为 0（非空全零）按真实 0
// 计，不落 8 秒兜底。
func (c *Calculator) EstimateReferenceVideoCost(in ReferenceVideoEstimate) (float64, string, error) {
	declaration, err := LookupPricing(c.catalog, in.Provider, in.Model, "video")
	if err != nil {
		return 0, "", err
	}
	if len(in.UnitDurationsSeconds) == 0 {
		return 0.0, declaration.Currency, nil
	}

	total := 0
	for _, duration := range in.UnitDurationsSeconds {
		if duration > 0 {
			total += duration
		}
	}
	params := Params{
		CallType:        "video",
		Model:           in.Model,
		Resolution:      in.Resolution,
		DurationSeconds: IntPtr(total),
		GenerateAudio:   in.GenerateAudio,
		ServiceTier:     in.ServiceTier,
	}
	if declaration.Kind == KindPerTokenVideo {
		params.UsageTokens = IntPtr(total * ArkTokensPerSecondEstimate)
	}
	return Calculate(declaration, params)
}

// IsCustomProvider 判断是否为自定义供应商的 provider_id（custom-<db id>）。
func IsCustomProvider(providerID string) bool {
	return strings.HasPrefix(providerID, customProviderPrefix)
}

// customCost 按调用方预查的单价计算自定义供应商费用。
//
// 单价口径与内置 kind 对齐：text 按每百万 token，image 按张，video 按每秒（缺省 8 秒），
// audio 按每万字符（usage_tokens 承载合成字符数）。
func customCost(params Params, opts CostOptions) (float64, string) {
	if opts.CustomPrice == nil || opts.CustomPrice.PriceInput == nil {
		return 0.0, "USD"
	}
	currency := opts.CustomPrice.Currency
	if currency == "" {
		currency = "USD"
	}

	switch params.CallType {
	case "text":
		inputPrice := *opts.CustomPrice.PriceInput
		outputPrice := 0.0
		if opts.CustomPrice.PriceOutput != nil {
			outputPrice = *opts.CustomPrice.PriceOutput
		}
		return (float64(IntValue(params.InputTokens))*inputPrice +
			float64(IntValue(params.OutputTokens))*outputPrice) / 1_000_000, currency
	case "image":
		return *opts.CustomPrice.PriceInput, currency
	case "video":
		duration := 8
		if params.DurationSeconds != nil && *params.DurationSeconds != 0 {
			duration = *params.DurationSeconds
		}
		return float64(duration) * *opts.CustomPrice.PriceInput, currency
	case "audio":
		return float64(IntValue(params.UsageTokens)) / CharacterUnit * *opts.CustomPrice.PriceInput, currency
	}
	return 0.0, currency
}

// Settlement 是写侧的申报值：承载 caller 在快照时刻提交的原始计费维度。
//
// 这些是「申报时刻」的原始值——BilledDurationSeconds 可能非法/超限、显式 CostAmount 绕过自动
// 计算——由 Settle 归一为生效定价（Params）。新增计费字段自此只扩本对象。
type Settlement struct {
	CostAmount            *float64
	Currency              string
	ServiceTier           string
	GenerateAudio         *bool
	BilledDurationSeconds *int
	UsageTokens           *int
	InputTokens           *int
	OutputTokens          *int
	Quality               string
	ImageInputTokens      *int
	ImageOutputTokens     *int
	TextInputTokens       *int
	TextOutputTokens      *int
}

// SettleRow 是结算所需的 api_calls 行快照：起算时刻与 start 时记录的请求口径。
type SettleRow struct {
	CallType        string
	Model           string
	Resolution      string
	AspectRatio     string
	DurationSeconds *int
	GenerateAudio   *bool
	Provider        string
	// StartedAt 缺失（零值）时 duration_ms 按 0 兜底。
	StartedAt time.Time
}

// SettledCall 是 Settle 的输出：终态 UPDATE 直接取用的生效结算值。
type SettledCall struct {
	DurationMs               int64
	EffectiveDurationSeconds *int
	EffectiveGenerateAudio   *bool
	InputTokens              *int
	OutputTokens             *int
	CostAmount               float64
	Currency                 string
}

// ErrSettleNeedsCalculator：settle 需要自动计费但结算器未注入目录。
var ErrSettleNeedsCalculator = errors.New("pricing: settle with auto calc requires a non-nil calculator")

// Settle 是 pending → 终态两条快照路径共享的结算函数：输入行快照与申报值，输出 duration_ms、
// 生效计费时长、生效有声标志与费用币种。
//
//   - duration_ms 按 finished - started 回写；started 缺失或时间相减异常时兜底为 0（最终防线）。
//   - 计费时长/有声标志覆盖：backend 回报值覆盖 start 时的请求值，非正或超出
//     MaxBilledDurationSeconds 的计费时长视同未提供、回落请求时长。
//   - OpenAI 图片 token 聚合：input/output_tokens 列的「总和」语义 = image_*_tokens + text_*_tokens
//     （用于跨 call_type 聚合查询保持兼容）；无图片 token 时保持申报值原样。
//   - 费用：显式 CostAmount 视作 provider 直报计费数据、优先于自动计算；否则在 autoCalc 为真时
//     按行字段 + 申报值调 CalculateCost。
func (c *Calculator) Settle(
	row SettleRow,
	finishedAt time.Time,
	s Settlement,
	autoCalc bool,
	baseCurrency string,
	custom *CustomPrice,
) (SettledCall, error) {
	out := SettledCall{}
	if !row.StartedAt.IsZero() && !finishedAt.IsZero() {
		out.DurationMs = finishedAt.Sub(row.StartedAt).Milliseconds()
	}

	out.EffectiveGenerateAudio = row.GenerateAudio
	if s.GenerateAudio != nil {
		out.EffectiveGenerateAudio = s.GenerateAudio
	}

	out.EffectiveDurationSeconds = row.DurationSeconds
	if billed := s.BilledDurationSeconds; billed != nil && *billed > 0 && *billed <= MaxBilledDurationSeconds {
		out.EffectiveDurationSeconds = billed
	}

	out.InputTokens = s.InputTokens
	out.OutputTokens = s.OutputTokens
	if s.ImageInputTokens != nil || s.ImageOutputTokens != nil || s.TextInputTokens != nil || s.TextOutputTokens != nil {
		out.InputTokens = IntPtr(IntValue(s.ImageInputTokens) + IntValue(s.TextInputTokens))
		out.OutputTokens = IntPtr(IntValue(s.ImageOutputTokens) + IntValue(s.TextOutputTokens))
	}

	switch {
	case s.CostAmount != nil:
		out.CostAmount = *s.CostAmount
		out.Currency = baseCurrency
		if s.Currency != "" {
			out.Currency = s.Currency
		}
	case autoCalc:
		if c == nil {
			return SettledCall{}, ErrSettleNeedsCalculator
		}
		provider := row.Provider
		if provider == "" {
			provider = ProviderGemini
		}
		params := Params{
			CallType:          row.CallType,
			Model:             row.Model,
			Resolution:        row.Resolution,
			AspectRatio:       row.AspectRatio,
			DurationSeconds:   out.EffectiveDurationSeconds,
			GenerateAudio:     BoolPtr(out.EffectiveGenerateAudio != nil && *out.EffectiveGenerateAudio),
			UsageTokens:       s.UsageTokens,
			ServiceTier:       s.ServiceTier,
			InputTokens:       out.InputTokens,
			OutputTokens:      out.OutputTokens,
			Quality:           s.Quality,
			ImageInputTokens:  s.ImageInputTokens,
			ImageOutputTokens: s.ImageOutputTokens,
			TextInputTokens:   s.TextInputTokens,
			TextOutputTokens:  s.TextOutputTokens,
		}
		amount, currency, err := c.CalculateCost(provider, params, CostOptions{CustomPrice: custom})
		if err != nil {
			return SettledCall{}, err
		}
		out.CostAmount = amount
		out.Currency = currency
	default:
		out.Currency = baseCurrency
	}
	return out, nil
}
