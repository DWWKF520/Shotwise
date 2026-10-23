// 语速估算单一真相源（对应 lib/speech_rate.py）。
//
// 把「一段口播文本朗读需多少秒」收敛到一处，供 drama 成片字幕定时与说话量对场景时长的上界
// 校验共用。语速以「阅读单位 / 秒」表示，阅读单位口径复用 CountReadingUnits（zh 计汉字 /
// CJK 标点，en / vi 计词）；语速值可调、按 source_language 覆盖、缺省回退默认。
//
// 取值优先级：项目级覆盖（project.json 顶层 speech_rate_units_per_second）> 语言默认 > 全局默认。
package service

import (
	"math"
	"strings"

	"github.com/shotwise/shotwise/internal/domain/project"
)

// DefaultSpeechRateUPS 是默认语速（阅读单位 / 秒）。中文可懂配音常见约 4–6 字 / 秒，取 5 为中位；
// 未登记 / 缺失语言回退此值（与未知语言按 zh 计字的口径对齐）。
const DefaultSpeechRateUPS = 5.0

// speechRateUPSByLanguage 按语言代码覆盖语速（阅读单位 / 秒），键用项目 source_language（zh / en / vi）。
// en / vi 的阅读单位是「词」，正常口语约 2–3 词 / 秒，取 2.5。
var speechRateUPSByLanguage = map[string]float64{
	"zh": 5.0,
	"en": 2.5,
	"vi": 2.5,
}

// SpeechRateField 是项目级语速覆盖在 project.json 的顶层字段名（阅读单位 / 秒）。
const SpeechRateField = "speech_rate_units_per_second"

// 项目级语速覆盖的硬区间（闭区间）。下界不止拦 ≤0，还为下游时间表示留出余量：0.001 意味着
// 每个阅读单位读满 1000 秒，真实口播远在其上。
const (
	MinSpeechRateUPS = 0.001
	MaxSpeechRateUPS = 20.0
)

// IsValidSpeechRate 判定数值是否落在项目级语速覆盖的硬区间内（0.001 <= value <= 20）。
//
// 入参允许是 project.json 解析出的原始值，故在这里收掉两类病理：NaN / Inf（与边界的比较均为假，
// 天然越界），以及区间的下界保证下游时长换算不溢出。区间内的值保证下游乘数不撑破整数类型。
func IsValidSpeechRate(value float64) bool {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return false
	}
	return value >= MinSpeechRateUPS && value <= MaxSpeechRateUPS
}

// ProjectSpeechRateOverride 从 project.json 解析项目级语速覆盖，未填 / 越界一律返回 nil。
//
// 返回 nil 即「无覆盖」，调用方把它原样交给下面两个函数即回退语言默认。这里的守卫是对手改
// project.json 与历史脏数据的读时兜底：估算语速不值得让一次已付费的生成崩在脏字段上。
func ProjectSpeechRateOverride(project *project.ProjectFile) *float64 {
	if project == nil || project.SpeechRate == nil {
		return nil
	}
	if !IsValidSpeechRate(*project.SpeechRate) {
		return nil
	}
	return project.SpeechRate
}

// SpeechRateUnitsPerSecond 返回生效语速（阅读单位 / 秒）：override 优先，否则按语言取默认。
//
// override 是项目级覆盖（由 ProjectSpeechRateOverride 解析），越界数按无覆盖处理。语言代码大小写
// 不敏感；空 / 未登记语言回退 DefaultSpeechRateUPS。
func SpeechRateUnitsPerSecond(language string, override *float64) float64 {
	if override != nil && IsValidSpeechRate(*override) {
		return *override
	}
	if strings.TrimSpace(language) == "" {
		return DefaultSpeechRateUPS
	}
	if rate, ok := speechRateUPSByLanguage[strings.ToLower(strings.TrimSpace(language))]; ok {
		return rate
	}
	return DefaultSpeechRateUPS
}

// EstimateSpokenSeconds 估算 text 以 language 朗读所需秒数。
//
// 口径：阅读单位数 ÷ 语速。空串 / 纯空白 / 纯标点（无阅读单位）一律计 0 秒——既是字幕单条定时的
// 输入，也是说话量求和的单项，两处共用同一换算。
func EstimateSpokenSeconds(text, language string, override *float64) float64 {
	if text == "" {
		return 0
	}
	units := CountReadingUnits(text, language)
	if units <= 0 {
		return 0
	}
	return float64(units) / SpeechRateUnitsPerSecond(language, override)
}
