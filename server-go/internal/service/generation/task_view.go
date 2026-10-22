// 任务视图的读时本地化（对应 server/routers/tasks.py 的 _localize_task 一族）。
//
// 存储态里 error_message 是 `[code] {params}` 信封、result.warnings 是 {key, params}
// 条目；两者都要按请求语言渲染成自然语言。渲染只发生在响应副本上——队列层持有的
// dict 保持语言中立，不会被某个请求的 locale 污染。
package generation

import (
	"encoding/json"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/i18n"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
)

// assetTypeParamByWarning 是 warning key → 其 params 中需替换为本地化资产类型名的字段。
var assetTypeParamByWarning = map[string]string{"ref_ad_reference_skipped": "type"}

// LocalizeTask 返回任务的响应副本，失败原因与 warnings 已渲染为 loc 语言。
//
// 已知结构化 code 走 Translator；裸异常文本与历史行先清洗（凭证/绝对路径）再原样返回，
// 避免老库行泄露 provider 密钥。入参不被修改。
func LocalizeTask(task repository.TaskData, loc i18n.Locale) map[string]any {
	translate := func(key string, params map[string]any) string { return i18n.T(loc, key, params) }
	raw, err := json.Marshal(task)
	if err != nil {
		return map[string]any{}
	}
	localized := map[string]any{}
	if err := json.Unmarshal(raw, &localized); err != nil {
		return map[string]any{}
	}
	if message, ok := localized["error_message"].(string); ok && message != "" {
		safe := domain.SanitizeFailureReason(message)
		if rendered := domain.RenderFailure(&safe, translate); rendered != nil {
			localized["error_message"] = *rendered
		}
	}
	if result, ok := localized["result"].(map[string]any); ok {
		if warnings, present := result["warnings"]; present && service.PyTruthy(warnings) {
			rendered := make(map[string]any, len(result))
			for k, v := range result {
				rendered[k] = v
			}
			rendered["warnings"] = RenderWarnings(warnings, translate)
			localized["result"] = rendered
		}
	}
	return localized
}

// RenderWarnings 把 ``result.warnings`` 的 {key, params} 条目渲染成本地语言文本。
//
// 形态不符的条目跳过而非报错：warnings 是纯提示，畸形条目不该让整个任务列表 500。
// 未知 key 由 i18n 兜底回落成 key 本身。
func RenderWarnings(warnings any, translate domain.FailureTranslator) []string {
	items, ok := warnings.([]any)
	if !ok {
		return []string{}
	}
	texts := make([]string, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key, ok := entry["key"].(string)
		if !ok {
			continue
		}
		params, ok := entry["params"].(map[string]any)
		if !ok {
			texts = append(texts, translate(key, nil))
			continue
		}
		texts = append(texts, translate(key, localizeWarningParams(key, params, translate)))
	}
	return texts
}

// localizeWarningParams 把 params 中裸的资产类型标识（如 ``"product"``）换成当前语言显示名。
// 只处理已登记 warning key 的已知字段；非字符串值（畸形持久化值）原样透传。
func localizeWarningParams(key string, params map[string]any, translate domain.FailureTranslator) map[string]any {
	field, ok := assetTypeParamByWarning[key]
	if !ok {
		return params
	}
	value, ok := params[field].(string)
	if !ok {
		return params
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		out[k] = v
	}
	out[field] = localizeAssetType(value, translate)
	return out
}

// localizeAssetType 把资产类型内部标识换成显示名；未登记的类型原样透传。
func localizeAssetType(value string, translate domain.FailureTranslator) string {
	if _, ok := domain.AssetSpecs[domain.AssetKind(value)]; !ok {
		return value
	}
	return translate("asset_type_"+value, nil)
}
