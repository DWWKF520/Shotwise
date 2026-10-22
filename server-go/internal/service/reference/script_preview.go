package reference

// 分镜文稿的读时派生预览（对应 lib/reference_video/script_preview.py 的 build_script_preview）。
//
// 只读、不落盘——文稿是唯一真相，utterances 与 references 都是机械派生物。声音相关的
// warning 依赖该集视频后端的能力与本集的无声开关，settings 由调用方从同一份能力解析
// 出口派生（VoiceRenderSettingsFromCaps）；能力解析失败时按 soft 降级，只是少发提示。

import "github.com/shotwise/shotwise/internal/domain"

// ScriptPreviewUtterance 是镜头级发声条目：ShotIndex 是 1-based 镜头序号。
type ScriptPreviewUtterance struct {
	ShotIndex int
	Kind      string
	Speaker   string
	Text      string
}

// ScriptPreview 是一份分镜文稿的读时派生结果，即编辑器「解析预览面板」的内容源。
//
// Warnings 是 locale-neutral 的 {key, params} 条目，由 transport 按请求语言渲染；
// References 已按落盘形态去掉空的 variant_* 键（与 Python model_dump(exclude_none=True) 同形）。
type ScriptPreview struct {
	Shots      []map[string]any
	References []any
	Utterances []ScriptPreviewUtterance
	Warnings   []map[string]any
}

// BuildScriptPreview 把书写文稿派生成 shots / references / utterances + 降级可见性 warning。
//
// settings 必填无兜底（同 RenderUnitPrompt），须与执行层拿到的同一份声音输入档同步，
// 否则预览会显示音频已绑定、实际请求却不带音频段；其中 AudioReady 在预览侧留 nil：
// 预览不解析文件，按角色资产的 reference_audio 字段非空判定。
// maxReferenceImages 非 nil 时与执行层「先裁 references 再渲染」同口径先裁再判定纯画外
// 降级；nil 表示不裁（能力不可解析时的降级口径）。
func BuildScriptPreview(
	text string, project *domain.ProjectFile, settings VoiceRenderSettings, maxReferenceImages *int,
) ScriptPreview {
	shots, mentions := parsePrompt(text)
	refs, missing := resolveReferences(mentions, project)

	warnings := make([]map[string]any, 0, len(missing))
	for _, name := range missing {
		warnings = append(warnings, previewWarning(WarnUnregisteredMention, map[string]any{"name": name}))
	}
	utterances, syntaxWarnings := deriveUtterances(shots)
	warnings = append(warnings, previewWarnings(syntaxWarnings)...)

	// 音频只能对齐到「同名且类型也是 character」的图：scene/prop 可能与角色同名，若按名字
	// 判定会把「这个名字有图」误判成「这个角色有图」。同时按能力上限裁剪后再判定。
	clipped := refs
	if maxReferenceImages != nil && len(refs) > *maxReferenceImages {
		clipped = refs[:*maxReferenceImages]
	}
	characterImageNames := map[string]bool{}
	for _, ref := range clipped {
		if refType, _ := ref["type"].(string); refType == "character" {
			if name, _ := ref["name"].(string); name != "" {
				characterImageNames[name] = true
			}
		}
	}

	var characters domain.AssetBucket
	if project != nil {
		characters = project.Characters
	}
	bindings := deriveVoiceBindings(utterances, characters, settings, characterImageNames)
	warnings = append(warnings, previewWarnings(bindings.warnings)...)

	out := ScriptPreview{
		Shots:      shots,
		References: RenderUnitReferences(refs),
		Utterances: make([]ScriptPreviewUtterance, 0, len(utterances)),
		Warnings:   warnings,
	}
	for _, u := range utterances {
		out.Utterances = append(out.Utterances, ScriptPreviewUtterance{
			ShotIndex: u.shotIndex, Kind: u.kind, Speaker: u.speaker, Text: u.text,
		})
	}
	return out
}

// previewWarning 构造一条 locale-neutral warning。
func previewWarning(key string, params map[string]any) map[string]any {
	return map[string]any{"key": key, "params": params}
}

// previewWarnings 批量转换。
func previewWarnings(in []voiceWarning) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, w := range in {
		out = append(out, previewWarning(w.key, w.params))
	}
	return out
}