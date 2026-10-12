package pricing

import (
	"errors"
	"log/slog"
)

// 供应商 id 常量：定价回落链按名字分支的只有 anthropic / vidu 两家。
const (
	ProviderAnthropic = "anthropic"
	ProviderVidu      = "vidu"
	// gemini 家族 provider：兜底费率按 aistudio + vertex 两处命中，再回落 aistudio 默认模型。
	providerGeminiAistudio = "gemini-aistudio"
	providerGeminiVertex   = "gemini-vertex"
)

// ownTableProviders 是各有独立费率表的 provider：未知 model 回落到「该 provider 的默认模型」。
// 其余 provider（gemini-aistudio/vertex、裸 gemini、ark-agent-plan、未知）一律回落 Gemini
// 家族通用默认费率，复刻历史「仅 ark/grok/openai/dashscope/minimax/kling/agnes 走专属表」的路由。
var ownTableProviders = map[string]bool{
	"ark": true, "grok": true, "openai": true, "dashscope": true,
	"minimax": true, "kling": true, "agnes": true,
}

// ModelInfo 是 registry 中一个模型的定价相关视图。
type ModelInfo struct {
	// MediaType 是模型所属媒体类型（text / image / video / audio）。
	MediaType string
	// Pricing 为 nil 表示已登记但未声明定价（如 Agent Plan 套餐），查找时继续回落。
	Pricing *Pricing
}

// ProviderMeta 是一个供应商的模型登记表。
type ProviderMeta struct {
	Models map[string]ModelInfo
	// DefaultModel 按媒体类型记录该供应商的默认模型；无该媒体类型即无此键。
	DefaultModel map[string]string
}

// Catalog 是定价查找依赖的供应商目录只读视图，生产由 registry 实现。
type Catalog interface {
	// Provider 返回供应商的模型登记表；未登记返回 ok=false。
	Provider(providerID string) (ProviderMeta, bool)
}

// MapCatalog 是按 provider id 组织的静态目录，registry 与测试都可直接用它。
type MapCatalog map[string]ProviderMeta

// Provider 实现 Catalog。
func (c MapCatalog) Provider(providerID string) (ProviderMeta, bool) {
	meta, ok := c[providerID]
	return meta, ok
}

// anthropicPricing 是 Anthropic 的文本定价：该 provider 不在 registry（无 ModelInfo 落点），
// 作为 registry-external 例外内置。助手主链路优先使用 SDK 回报的实际费用，此表仅在只拿到
// token 数时兜底。费率为美元/百万 token。
var anthropicPricing = Pricing{
	Kind: KindPerToken,
	TokenRates: map[string]map[string]float64{
		"claude-sonnet-4":           {"input": 3.00, "output": 15.00},
		"claude-sonnet-4-20250514":  {"input": 3.00, "output": 15.00},
		"claude-sonnet-4.5":         {"input": 3.00, "output": 15.00},
		"claude-sonnet-4-5":         {"input": 3.00, "output": 15.00},
		"claude-sonnet-4-6":         {"input": 3.00, "output": 15.00},
		"claude-haiku-4-5":          {"input": 1.00, "output": 5.00},
		"claude-haiku-4-5-20251001": {"input": 1.00, "output": 5.00},
		"claude-haiku-4-20250514":   {"input": 1.00, "output": 5.00},
		"claude-opus-4":             {"input": 15.00, "output": 75.00},
		"claude-opus-4-6":           {"input": 15.00, "output": 75.00},
		"claude-opus-4-7":           {"input": 15.00, "output": 75.00},
	},
	DefaultModel: "claude-sonnet-4",
	Currency:     "USD",
}

// LookupPricing 返回该调用应使用的定价声明；mediaType 即 callType。
//
// 回落次序复刻历史计费行为，保证迁移前后金额一致：
// anthropic → 内置表；vidu → 委托标记（provider 级，不经 model 回落）；
// registry 命中且媒体类型匹配且有定价 → 该声明；否则自有费率表的 provider 回落其默认模型；
// 其余一律回落 Gemini 家族通用默认费率。
func LookupPricing(catalog Catalog, provider, model, mediaType string) (Pricing, error) {
	if provider == ProviderAnthropic {
		return anthropicPricing, nil
	}
	if provider == ProviderVidu {
		// provider 级判定：不经 model→pricing 回落，确保策略拿到原始 model 透传给 vidu 计费。
		return NewViduDelegate("CNY"), nil
	}

	var meta ProviderMeta
	known := false
	if catalog != nil {
		meta, known = catalog.Provider(provider)
	}
	if !known {
		return geminiDefaultPricing(catalog, mediaType, model)
	}

	info, found := meta.Models[model]
	if model == "" {
		found = false
	}
	// 必须媒体类型匹配才命中：模型名只在本模态费率表内查。跨模态的 (model, call_type)
	// 组合（如 video 调用带着图像模型名）应回落当前媒体类型的默认模型，而非按该模型自身的
	// 异模态费率计价。
	if found && info.MediaType == mediaType && info.Pricing != nil {
		return *info.Pricing, nil
	}
	warnFallback(provider, model, mediaType, info, found)

	// ark/grok/openai/dashscope/minimax/kling/agnes 各有专属费率表 → 未命中回落到该 provider
	// 的默认模型；其余（gemini 家族等）回落 Gemini 通用默认费率，与历史路由一致。
	if ownTableProviders[provider] {
		if defaultModel, ok := meta.DefaultModel[mediaType]; ok {
			if defaultInfo, ok := meta.Models[defaultModel]; ok && defaultInfo.Pricing != nil {
				return *defaultInfo.Pricing, nil
			}
		}
	}
	return geminiDefaultPricing(catalog, mediaType, model)
}

// warnFallback 区分三类回落原因：未知 model / 媒体类型与调用不符 / 已登记但未声明定价。
// 前两类是配置或调用错误，第三类（如 Agent Plan 套餐）是预期兜底，仅 debug。
func warnFallback(provider, model, mediaType string, info ModelInfo, found bool) {
	switch {
	case model != "" && !found:
		slog.Warn("pricing lookup: 模型未在 registry，回落默认模型费率", "provider", provider, "model", model)
	case found && info.MediaType != mediaType:
		slog.Warn("pricing lookup: 媒体类型与调用不符，回落默认模型费率",
			"provider", provider, "model", model, "model_media_type", info.MediaType, "call_type", mediaType)
	case found && info.Pricing == nil:
		slog.Debug("pricing lookup: 模型未声明定价，回落默认模型费率", "provider", provider, "model", model)
	}
}

// geminiDefaultPricing 是非专属费率表 provider（含裸 gemini / 未知 provider / Agent Plan /
// gemini-aistudio / gemini-vertex）的统一兜底：先按 model 在 aistudio + vertex 命中（覆盖
// 裸 gemini + veo-3.1-generate-001 这类历史调用），未命中再回落 aistudio 该媒体类型的默认
// 模型。历史上所有 Gemini 家族 provider 共用同一通用默认（如视频 veo-3.1-lite），故按
// aistudio 的默认取，避免按各 provider 自身默认（如 vertex 视频默认是 fast-001）算错价。
func geminiDefaultPricing(catalog Catalog, mediaType, model string) (Pricing, error) {
	if catalog == nil {
		return Pricing{}, errors.New("pricing: gemini fallback needs a provider catalog")
	}
	if model != "" {
		for _, providerID := range []string{providerGeminiAistudio, providerGeminiVertex} {
			meta, ok := catalog.Provider(providerID)
			if !ok {
				continue
			}
			if info, ok := meta.Models[model]; ok && info.Pricing != nil && info.MediaType == mediaType {
				return *info.Pricing, nil
			}
		}
	}

	meta, ok := catalog.Provider(providerGeminiAistudio)
	if ok {
		for _, fallbackMedia := range []string{mediaType, "text"} {
			modelID, ok := meta.DefaultModel[fallbackMedia]
			if !ok {
				continue
			}
			if info, ok := meta.Models[modelID]; ok && info.Pricing != nil {
				return *info.Pricing, nil
			}
		}
	}
	return Pricing{}, errors.New("pricing: catalog has no gemini-aistudio default model pricing for fallback")
}
