// 参考生视频路径的三段论渲染（对应 lib/reference_video/prompt_render.py）：unit 内容 +
// 资产表 + 能力档 → 发给视频模型的 prompt。
//
// 第一段（主体绑定 + 声音声明）与第三段（风格锚定 + 画质/稳定/字幕/水印约束包）由本模块在
// 渲染期机械生成，不依赖 LLM 自觉。渲染是纯函数、结果不落盘。
//
// 所有偏移与切片按 rune（Python 是码点坐标系；Go 的 len(string) 是字节）。
package reference

import (
	"github.com/shotwise/shotwise/internal/service"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// AssetAudioSubdir 是角色参考音频的项目内固定目录（与上传 / TTS 样本落盘口径一致）。
const AssetAudioSubdir = "characters/refs_audio"

// 第三段约束包。面向视频模型的提示词文本（非用户可见文案），按仓库口径豁免 i18n。
const (
	QualityPack   = "高清，细节丰富，电影质感，色彩自然，光影柔和。"
	StabilityPack = "人物面部稳定不变形、五官清晰、动作连贯自然，不僵硬，无穿模无卡顿。"
	subtitlePack  = "保持无字幕，避免生成任何文字或字幕。"
	watermarkPack = "不要生成水印；不要生成 Logo。"
	noBGMPack     = "禁止出现背景音乐。"
	twinPack      = "视频全程禁止出现外形、着装、配饰完全一致的人物，禁止生成同款分身、" +
		"双胞胎效果，同一画面中仅保留单个对应人物，不出现人物重复复刻。"
)

// RenderedUnitPrompt 是一个 unit 的渲染产物。
//
// AudioSpeakers 的顺序即「@音频N」编号，调用方须按同一顺序组装 reference_audio_files——这是
// prompt 文本与请求字段之间唯一的绑定契约。AudioSpeakerReferenceIndex 与之等长同序：第 i 项
// 是该 speaker 对应的 reference_images 下标（0-based），没有参考图（纯画外角色）时为 nil。
type RenderedUnitPrompt struct {
	Prompt                     string
	AudioSpeakers              []string
	AudioSpeakerReferenceIndex []*int
	Warnings                   []map[string]any
}

// voiceWarningsAsMaps 把声音 warning 转成 locale-neutral 的 {key, params} 条目（供 result.warnings）。
func voiceWarningsAsMaps(warnings []voiceWarning) []map[string]any {
	out := make([]map[string]any, 0, len(warnings))
	for _, warning := range warnings {
		params := warning.params
		if params == nil {
			params = map[string]any{}
		}
		out = append(out, map[string]any{"key": warning.key, "params": params})
	}
	return out
}

// RenderUnitPrompt 把一个 unit 的书写文稿渲染成三段论 backend prompt（对应 render_unit_prompt）。
//
// references 是本次实际随请求发出的参考图列表（已按能力上限裁剪），其顺序即「图片N」编号——与
// reference_images 严格等长同序，被裁掉的名字退化为原文不产生悬空绑定。
func RenderUnitPrompt(
	text string, project *domain.ProjectFile, references []map[string]any,
	settings VoiceRenderSettings, style string,
) RenderedUnitPrompt {
	shots, mentions := parsePrompt(text)
	utterances, warnings := deriveUtterances(shots)

	// references 是入参（上游持久化的派生结果），其名字以哪种编码形式落盘不可控；正文一侧出自
	// 解析器、已归一。两侧同形，主体记号与图号才对得上。
	normalizedRefs := make([]map[string]any, 0, len(references))
	for _, ref := range references {
		name, _ := ref["name"].(string)
		entry := map[string]any{
			"type": ref["type"],
			"name": domain.NormalizeAssetName(name),
		}
		if id, ok := ref["variant_id"].(string); ok && id != "" {
			entry["variant_id"] = id
		}
		if slug, ok := ref["variant_slug"].(string); ok && slug != "" {
			entry["variant_slug"] = slug
		}
		normalizedRefs = append(normalizedRefs, entry)
	}

	registered, missing := resolveReferences(mentions, project)
	for _, name := range missing {
		warnings = append([]voiceWarning{{key: WarnUnregisteredMention, params: map[string]any{"name": name}}}, warnings...)
	}

	// 主体记号按资产表登记判定，与参考图编号解耦：被能力上限裁掉的名字仍是画面主体。
	var subjects []string
	for _, ref := range registered {
		subjects = append(subjects, referenceSubjectLabel(ref))
	}

	// 音频只能对齐到「同名且类型也是 character」的图：resolve_references 按 character → scene → prop
	// 的优先级分派，场景/道具可能与某个角色同名，名字键的字典若不先按类型过滤再建会互相覆盖。
	characterImageNo := map[string]int{}
	for i, ref := range normalizedRefs {
		if refType, _ := ref["type"].(string); refType != "character" {
			continue
		}
		name, _ := ref["name"].(string)
		if _, exists := characterImageNo[name]; !exists {
			characterImageNo[name] = i + 1
		}
	}

	characters := characterBucket(project)
	bindings := deriveVoiceBindings(utterances, characters, settings, keySet(characterImageNo))
	warnings = append(warnings, bindings.warnings...)

	audioNo, referenceIndex := numberAudioSpeakers(bindings.audioSpeakers, characterImageNo)

	labels := make([]string, 0, len(normalizedRefs))
	characterRefCount := 0
	for _, ref := range normalizedRefs {
		labels = append(labels, referenceSubjectLabel(ref))
		if refType, _ := ref["type"].(string); refType == "character" {
			characterRefCount++
		}
	}

	segments := []string{
		renderSegmentOne(labels, bindings.speakers, audioNo, characters, settings),
		renderSegmentTwo(shots, subjects, characters),
		renderSegmentThree(characterRefCount, style),
	}
	return RenderedUnitPrompt{
		Prompt:                     joinNonEmpty(segments, "\n\n"),
		AudioSpeakers:              append([]string(nil), bindings.audioSpeakers...),
		AudioSpeakerReferenceIndex: referenceIndex,
		Warnings:                   voiceWarningsAsMaps(warnings),
	}
}

// RenderAdBackendPrompt 把 ad 派生 unit 的结构化镜头渲染成三段论 backend prompt
// （对应 render_ad_backend_prompt），与剧集路径共用第一、三段与音频接线。
func RenderAdBackendPrompt(
	shots []map[string]any, entries []map[string]any, project *domain.ProjectFile,
	settings VoiceRenderSettings, style string,
) (RenderedUnitPrompt, error) {
	body := domain.RenderAdUnitPrompt(shots, nil)
	if strings.TrimSpace(body) == "" {
		return RenderedUnitPrompt{}, fmt.Errorf("reference video unit prompt is empty: all member shots have no visual content")
	}

	characters := characterBucket(project)
	utterances := deriveAdUtterances(shots)

	// 音频只能对齐到「同名且类型也是 character」的图：entries 里 character/scene/prop 的设计图
	// 共用 kind == "asset"，三类资产允许重名，只有 asset_type 能把角色的图与同名场景/道具的图分开。
	characterImageNo := map[string]int{}
	for i, entry := range entries {
		assetType, _ := entry["asset_type"].(string)
		name, isString := entry["name"].(string)
		if assetType == "character" && isString {
			characterImageNo[domain.NormalizeAssetName(name)] = i + 1
		}
	}

	bindings := deriveVoiceBindings(utterances, characters, settings, keySet(characterImageNo))
	audioNo, referenceIndex := numberAudioSpeakers(bindings.audioSpeakers, characterImageNo)

	labels := make([]string, 0, len(entries))
	for _, entry := range entries {
		label, _ := entry["label"].(string)
		labels = append(labels, label)
	}
	segments := []string{
		renderSegmentOne(labels, bindings.speakers, audioNo, characters, settings),
		body,
		renderSegmentThree(len(characterImageNo), style),
	}
	return RenderedUnitPrompt{
		Prompt:                     joinNonEmpty(segments, "\n\n"),
		AudioSpeakers:              append([]string(nil), bindings.audioSpeakers...),
		AudioSpeakerReferenceIndex: referenceIndex,
		Warnings:                   voiceWarningsAsMaps(bindings.warnings),
	}, nil
}

// referenceSubjectLabel 渲染主体记号文本：角色衍生渲染为 `角色/slug`。
func referenceSubjectLabel(ref map[string]any) string {
	name, _ := ref["name"].(string)
	refType, _ := ref["type"].(string)
	slug, _ := ref["variant_slug"].(string)
	if refType == "character" && slug != "" {
		return name + "/" + slug
	}
	return name
}

// characterBucket 返回项目角色表（按归一名索引）；project 为 nil 时按空表处理。
func characterBucket(project *domain.ProjectFile) domain.AssetBucket {
	if project == nil {
		return domain.AssetBucket{}
	}
	return project.Characters
}

// keySet 把映射的键收成集合。
func keySet(m map[string]int) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// joinNonEmpty 用 sep 连接非空段。
func joinNonEmpty(parts []string, sep string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, sep)
}

// renderSegmentOne 渲染第一段：主体绑定 + 声音声明。
//
// labels 是主体记号文本，逐项对应本次随请求发出的参考图（narration/drama 传 mention 派生的
// 资产名，ad 传参考条目的展示 label）。图号按位置直接编号（非名字查表）：不同类型的资产允许
// 同名，位置编号天然不受影响；空 label 占位不产出绑定行，编号照样前进。
func renderSegmentOne(
	labels, speakers []string, audioNo map[string]int, characters domain.AssetBucket, settings VoiceRenderSettings,
) string {
	var lines []string
	parts := make([]string, 0, len(labels))
	for i, label := range labels {
		if label == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("<%s>@图片%d", label, i+1))
	}
	if len(parts) > 0 {
		lines = append(lines, strings.Join(parts, "、")+"。")
	}
	lines = append(lines, renderVoiceDeclarations(speakers, audioNo, characters, settings)...)
	return strings.Join(lines, "\n")
}

// renderVoiceDeclarations 渲染声音声明行：`<X>的台词音色参考 @音频N，声音特征：…`。
//
// 两条无声路径（模型不产音的 C 类、本集关闭音频）都不注入声音声明；台词行不受影响、照常进第二段。
func renderVoiceDeclarations(
	speakers []string, audioNo map[string]int, characters domain.AssetBucket, settings VoiceRenderSettings,
) []string {
	if settings.isSilent() {
		return nil
	}
	var lines []string
	for _, name := range speakers {
		var parts []string
		if no, ok := audioNo[name]; ok {
			parts = append(parts, fmt.Sprintf("台词音色参考 @音频%d", no))
		}
		voiceStyle := ""
		if character := resolveEntry(characters, name); character != nil {
			voiceStyle = strings.TrimSpace(character.MustString("voice_style"))
		}
		if voiceStyle != "" {
			parts = append(parts, "声音特征："+voiceStyle)
		}
		if len(parts) > 0 {
			lines = append(lines, fmt.Sprintf("<%s>的%s。", name, strings.Join(parts, "，")))
		}
	}
	return lines
}

// numberAudioSpeakers 派生「@音频N」编号与「每段音频对应哪张参考图」的下标（两条路径共用）。
func numberAudioSpeakers(audioSpeakers []string, characterImageNo map[string]int) (map[string]int, []*int) {
	audioNo := make(map[string]int, len(audioSpeakers))
	for i, name := range audioSpeakers {
		audioNo[name] = i + 1
	}
	index := make([]*int, 0, len(audioSpeakers))
	for _, name := range audioSpeakers {
		if no, ok := characterImageNo[name]; ok {
			value := no - 1
			index = append(index, &value)
			continue
		}
		index = append(index, nil)
	}
	return audioNo, index
}

// renderSegmentTwo 渲染第二段：镜头分镜段，描述行做 mention 替换，规范台词行重组为官方句式。
//
// 台词行的说话人按资产表判定而非参考图列表：纯画外角色无参考图，台词行照常重组。裸画外音行
// 属于字幕 / TTS 的后续输入，整行丢弃；未闭合花括号行原样发送。
func renderSegmentTwo(shots []map[string]any, subjects []string, characters domain.AssetBucket) string {
	var blocks []string
	for index, shot := range shots {
		bodyText, _ := shot["text"].(string)
		var kept []string
		for _, line := range splitShotSourceLines(bodyText) {
			if speaker, spoken, ok := matchDialogueLine(line); ok {
				if _, registered := characters.ResolveKey(speaker); registered {
					kept = append(kept, fmt.Sprintf("<%s>说 {%s}", speaker, spoken))
					continue
				}
			}
			// 裸旁白留给字幕 / TTS 管线；仅丢弃规范的整行旁白，混写行原样保留。
			if _, isVoiceover := matchVoiceoverLine(line); isVoiceover {
				continue
			}
			kept = append(kept, RenderMentionsAsSubjects(line, subjects))
		}
		nonBlank := make([]string, 0, len(kept))
		for _, line := range kept {
			if strings.TrimSpace(line) != "" {
				nonBlank = append(nonBlank, line)
			}
		}
		text := strings.Join(nonBlank, "\n")
		if text != "" {
			blocks = append(blocks, fmt.Sprintf("镜头%d：\n%s", index+1, text))
		} else {
			blocks = append(blocks, fmt.Sprintf("镜头%d：", index+1))
		}
	}
	return strings.Join(blocks, "\n\n")
}

// renderSegmentThree 渲染第三段：风格锚定 + 画质/稳定/字幕/水印约束包；≥2 角色参考图时补双胞胎兜底。
func renderSegmentThree(characterReferenceCount int, style string) string {
	var lines []string
	if normalized := service.NormalizeStylePrefix(style); normalized != "" {
		lines = append(lines, "整体视觉风格："+normalized+"。")
	}
	lines = append(lines, QualityPack+StabilityPack)
	lines = append(lines, subtitlePack+watermarkPack+noBGMPack)
	if characterReferenceCount >= 2 {
		lines = append(lines, twinPack)
	}
	return strings.Join(lines, "\n")
}

// deriveAdUtterances 从 ad 结构化镜头派生台词 utterances（video_prompt.dialogue 已是判别式结构）。
//
// 有 speaker 派生 dialogue utterance，无 speaker 的裸台词派生 voiceover utterance；脏 video_prompt /
// dialogue 条目（非对象、非字符串 speaker 或 line）按空处理。
func deriveAdUtterances(shots []map[string]any) []shotUtterance {
	var utterances []shotUtterance
	for index, shot := range shots {
		videoPrompt, _ := shot["video_prompt"].(map[string]any)
		dialogue, ok := videoPrompt["dialogue"].([]any)
		if !ok {
			continue
		}
		for _, raw := range dialogue {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			speaker, _ := entry["speaker"].(string)
			text, _ := entry["line"].(string)
			speaker = strings.TrimSpace(speaker)
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			if speaker != "" {
				utterances = append(utterances, shotUtterance{shotIndex: index + 1, kind: "dialogue", speaker: speaker, text: text})
			} else {
				utterances = append(utterances, shotUtterance{shotIndex: index + 1, kind: "voiceover", text: text})
			}
		}
	}
	return utterances
}

// RenderMentionsAsSubjects 把文本里的 `@[X]` / `@X` 替换为三段论的主体记号 `<X>`
// （对应 render_mentions_as_subjects）。
//
// names 是资产表里已登记的名字：不在其中的 mention 原样保留、从不删字。正文与 names 都归一到
// 比对坐标系后再判成员。
func RenderMentionsAsSubjects(text string, names []string) string {
	normalized := make(map[string]bool, len(names))
	for _, name := range names {
		normalized[domain.NormalizeAssetName(name)] = true
	}
	text = normalizeShotSource(text)
	runes := []rune(text)
	var b strings.Builder
	last := 0
	for _, span := range iterMentions(runes) {
		b.WriteString(string(runes[last:span.start]))
		if normalized[domain.NormalizeAssetName(span.name)] {
			b.WriteString("<" + span.name + ">")
		} else {
			b.WriteString(string(runes[span.start:span.end]))
		}
		last = span.end
	}
	b.WriteString(string(runes[last:]))
	return b.String()
}

// ResolveReferenceAudioPaths 返回项目内「参考音频确实可用」的角色名 → 绝对路径映射
// （对应 resolve_reference_audio_paths）。
//
// 只收录字段指向 characters/refs_audio 内且文件确实存在的条目；key 是归一后的角色名。
func ResolveReferenceAudioPaths(project *domain.ProjectFile, projectDir string) map[string]string {
	resolved := map[string]string{}
	for name, entry := range characterBucket(project) {
		if entry == nil {
			continue
		}
		rel := entry.MustString("reference_audio")
		path := ResolveAudioRefPath(projectDir, rel)
		if path == "" {
			continue
		}
		if !PathIsFile(path) {
			continue
		}
		resolved[name] = path
	}
	return resolved
}

// AudioPathSet 把「参考音频确实可用」的角色名映射收成集合（键已归一）。
func AudioPathSet(audioPaths map[string]string) map[string]bool {
	out := make(map[string]bool, len(audioPaths))
	for name := range audioPaths {
		out[domain.NormalizeAssetName(name)] = true
	}
	return out
}

// ResolveAudioRefPath 解析 reference_audio 字段值，仅当其确实落在 characters/refs_audio 内才返回
// （对应 lib/audio_utils.py::resolve_audio_ref_path）：该字段可经资产 PATCH 被写成项目内任意字符串。
func ResolveAudioRefPath(projectDir, rel string) string {
	if strings.TrimSpace(rel) == "" {
		return ""
	}
	resolved, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(rel))
	if err != nil {
		return ""
	}
	refsDir, err := fs.SafeJoin(projectDir, false, "characters", "refs_audio")
	if err != nil {
		return ""
	}
	if filepath.Dir(resolved) != refsDir {
		return ""
	}
	return resolved
}

// ---------------------------------------------------------------------------
// 产品参考装配与高保真尾注
// ---------------------------------------------------------------------------

// 产品高保真还原指令的核心措辞（与分镜图路径共用同一份文本）。
const productFidelityCore = "画面中的产品必须与产品参考图完全一致——" +
	"包装、配色、字体、材质与结构细节均不得改动，不得重新设计或美化产品本身"

// AppendProductFidelityTail 给产品镜头的生成 prompt 追加高保真还原指令
// （对应 append_product_fidelity_tail）。productNames 为空返回原 prompt；重复调用幂等。
func AppendProductFidelityTail(prompt string, productNames []string) string {
	var names strings.Builder
	for _, name := range productNames {
		if name != "" {
			names.WriteString("「" + name + "」")
		}
	}
	if names.Len() == 0 {
		return prompt
	}
	tail := fmt.Sprintf(
		"产品高保真还原（最高优先级，优先于前述文字/Logo 禁止项）：画面中的产品%s"+
			"%s，不得重新设计或美化产品本身；"+
			"前述文字/Logo 禁止项仅指画面中不得凭空新增文字或 Logo，产品参考图自带的文字与 Logo"+
			"须原样保留；项目画风只作用于产品以外的画面元素。",
		names.String(), productFidelityCore,
	)
	if strings.TrimSpace(prompt) == "" {
		return tail
	}
	if strings.Contains(prompt, tail) {
		return prompt
	}
	return strings.TrimRight(prompt, " \t\n\r") + "\n\n" + tail
}

// collectProductReferencesForNames 按产品名列表收集产品参考集（条目语义见
// _collect_shot_product_references）。
//
// 每个产品：有 product sheet 时注入集为「sheet 多角度 + 原图压阵」，无 sheet 时原图直注。
// 返回 “{"image", "label", "name", "kind"}“ 列表；kind 供截断时让 sheet 优先存活。
// 脏数据（产品名非字符串、引用不存在的产品）按既有装配口径跳过不抛。
func collectProductReferencesForNames(
	project *domain.ProjectFile, projectDir string, names []string,
) []map[string]any {
	spec, ok := domain.Spec(domain.AssetProduct)
	if !ok {
		return nil
	}
	var references []map[string]any
	seen := map[string]bool{}
	for _, name := range names {
		if name == "" {
			continue
		}
		canonical := domain.NormalizeAssetName(name)
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		entry := lookupAssetEntry(project, domain.AssetProduct, canonical)
		if entry == nil {
			continue
		}
		if sheet := entry.MustString(spec.SheetKey); sheet != "" && fs.SafeExists(projectDir, sheet) {
			references = append(references, map[string]any{
				"image": filepath.Join(projectDir, filepath.FromSlash(sheet)),
				"label": fmt.Sprintf("产品「%s」标准多角度参考图", canonical),
				"name":  canonical,
				"kind":  "sheet",
			})
		}
		for _, original := range collectProductReferenceImages(project, projectDir, canonical) {
			references = append(references, map[string]any{
				"image": original,
				"label": fmt.Sprintf("产品「%s」实拍原图（保真锚点）", canonical),
				"name":  canonical,
				"kind":  "original",
			})
		}
	}
	return references
}

// collectProductReferenceImages 取产品原图（保真验收锚点）的项目内绝对路径；缺失文件跳过。
func collectProductReferenceImages(project *domain.ProjectFile, projectDir, resourceID string) []string {
	entry := lookupAssetEntry(project, domain.AssetProduct, domain.NormalizeAssetName(resourceID))
	if entry == nil {
		return nil
	}
	refs, ok := entry.List("reference_images")
	if !ok {
		return nil
	}
	var existing []string
	for _, rel := range refs {
		if fs.SafeExists(projectDir, rel) {
			existing = append(existing, filepath.Join(projectDir, filepath.FromSlash(rel)))
		}
	}
	return existing
}

// lookupAssetEntry 按归一名在指定资产桶里取条目。
func lookupAssetEntry(project *domain.ProjectFile, kind domain.AssetKind, name string) *domain.AssetEntry {
	if project == nil {
		return nil
	}
	bucket, ok := project.BucketRef(kind)
	if !ok || bucket == nil {
		return nil
	}
	key, ok := bucket.ResolveKey(name)
	if !ok {
		return nil
	}
	return (*bucket)[key]
}

// ---------------------------------------------------------------------------
// reference 解析与供应商约束
// ---------------------------------------------------------------------------

// missingReference 是一条缺失的 reference（类型 + 名称）。
type missingReference struct {
	AssetType string
	Name      string
}

// missingReferenceError 表示 @ 提及解析到不存在或无 sheet 的资源。
type missingReferenceError struct{ Missing []missingReference }

func (e *missingReferenceError) Error() string {
	parts := make([]string, 0, len(e.Missing))
	for _, item := range e.Missing {
		parts = append(parts, item.AssetType+":"+item.Name)
	}
	return "Missing references: " + strings.Join(parts, ", ")
}

// DedupeTypedReferences 按 (type, 归一名, 衍生键) 去重 reference 字典，保留首次出现顺序。
//
// 同一资产可能以 NFC/NFD 两条等价记录同时留在 unit.references 里；参考图解析与 prompt 渲染前的
// 裁剪必须共用同一份去重结果，否则图片列表与逻辑引用列表长度不一致会造成编号错位。
func DedupeTypedReferences(references []map[string]any) []map[string]any {
	seen := map[string]bool{}
	var deduped []map[string]any
	for _, ref := range references {
		refType, _ := ref["type"].(string)
		name, _ := ref["name"].(string)
		variantKey, _ := ref["variant_id"].(string)
		if variantKey == "" {
			variantKey, _ = ref["variant_slug"].(string)
		}
		key := refType + "\x00" + domain.NormalizeAssetName(name) + "\x00" + variantKey
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, ref)
	}
	return deduped
}

// ResolveUnitReferences 把 unit.references 转成绝对路径列表（按 references 顺序、按类型+归一名去重）。
//
// 任一 reference 在 project.json 对应 bucket 缺失或 sheet 不存在时返回 *missingReferenceError。
func ResolveUnitReferences(project *domain.ProjectFile, projectDir string, references []map[string]any) ([]string, error) {
	var missing []missingReference
	var resolved []string
	for _, ref := range DedupeTypedReferences(references) {
		refType, _ := ref["type"].(string)
		name, _ := ref["name"].(string)
		spec, ok := domain.Spec(domain.AssetKind(refType))
		if !ok {
			missing = append(missing, missingReference{AssetType: refType, Name: name})
			continue
		}
		entry := lookupAssetEntry(project, domain.AssetKind(refType), domain.NormalizeAssetName(name))
		if entry == nil {
			missing = append(missing, missingReference{AssetType: refType, Name: name})
			continue
		}
		sheetRel := ""
		variantID, _ := ref["variant_id"].(string)
		variantSlug, _ := ref["variant_slug"].(string)
		if refType == "character" && (variantID != "" || variantSlug != "") {
			identifier := variantID
			if identifier == "" {
				identifier = variantSlug
			}
			if variant := characterVariantEntry(entry, identifier); variant != nil {
				sheetRel, _ = variant["image_path"].(string)
			}
		} else {
			sheetRel = entry.MustString(spec.SheetKey)
		}
		if strings.TrimSpace(sheetRel) == "" {
			missing = append(missing, missingReference{AssetType: refType, Name: name})
			continue
		}
		path, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(sheetRel))
		if err != nil || !PathIsFile(path) {
			missing = append(missing, missingReference{AssetType: refType, Name: name})
			continue
		}
		resolved = append(resolved, path)
	}
	if len(missing) > 0 {
		return nil, &missingReferenceError{Missing: missing}
	}
	return resolved, nil
}

// characterVariantEntry 取角色衍生形态的原始条目（含 image_path）；按 slug 排序稳定遍历。
func characterVariantEntry(character *domain.AssetEntry, identifier string) map[string]any {
	if character == nil {
		return nil
	}
	rawVariants, _ := character.Fields()["variants"].(map[string]any)
	if len(rawVariants) == 0 {
		return nil
	}
	target := domain.NormalizeAssetName(identifier)
	slugs := make([]string, 0, len(rawVariants))
	for slug := range rawVariants {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		raw, ok := rawVariants[slug].(map[string]any)
		if !ok {
			continue
		}
		id, _ := raw["id"].(string)
		rawSlug, _ := raw["slug"].(string)
		if rawSlug == "" {
			rawSlug = slug
		}
		if id == identifier || domain.NormalizeAssetName(rawSlug) == target {
			imagePath, _ := raw["image_path"].(string)
			return map[string]any{"id": id, "slug": rawSlug, "image_path": imagePath}
		}
	}
	return nil
}

// ResolveAdUnitReferenceEntries 解析 ad 派生 unit 的参考：返回 (entries, warnings)。
//
// 与 narration/drama 的 ResolveUnitReferences（缺图硬失败）不同，ad 的参考集由分组器从镜头字段
// 自动继承，缺图按软口径跳过并记 warning。产品沿用注入二元规则且排在所有其它参考之前。
func ResolveAdUnitReferenceEntries(
	project *domain.ProjectFile, projectDir string, references []map[string]any,
) ([]map[string]any, []map[string]any) {
	var warnings []map[string]any
	var productNames []string
	type assetRef struct{ assetType, name, canonical string }
	var assetRefs []assetRef
	seenProducts := map[string]bool{}
	seenAssets := map[string]bool{}
	for _, ref := range references {
		refType, _ := ref["type"].(string)
		name, _ := ref["name"].(string)
		if name == "" {
			continue
		}
		canonical := domain.NormalizeAssetName(name)
		if refType == "product" {
			if !seenProducts[canonical] {
				seenProducts[canonical] = true
				productNames = append(productNames, name)
			}
			continue
		}
		if _, ok := domain.Spec(domain.AssetKind(refType)); ok {
			key := refType + "\x00" + canonical
			if !seenAssets[key] {
				seenAssets[key] = true
				assetRefs = append(assetRefs, assetRef{assetType: refType, name: name, canonical: canonical})
			}
		}
	}

	entries := collectProductReferencesForNames(project, projectDir, productNames)
	injectedProducts := map[string]bool{}
	for _, entry := range entries {
		if name, ok := entry["name"].(string); ok {
			injectedProducts[name] = true
		}
	}
	for _, name := range productNames {
		if !injectedProducts[domain.NormalizeAssetName(name)] {
			warnings = append(warnings, map[string]any{
				"key": "ref_ad_reference_skipped", "params": map[string]any{"type": "product", "name": name},
			})
		}
	}

	for _, ref := range assetRefs {
		spec, _ := domain.Spec(domain.AssetKind(ref.assetType))
		entry := lookupAssetEntry(project, domain.AssetKind(ref.assetType), ref.canonical)
		sheetRel := ""
		if entry != nil {
			sheetRel = entry.MustString(spec.SheetKey)
		}
		if sheetRel != "" && fs.SafeExists(projectDir, sheetRel) {
			entries = append(entries, map[string]any{
				"image":      filepath.Join(projectDir, filepath.FromSlash(sheetRel)),
				"label":      fmt.Sprintf("%s「%s」设计图", spec.LabelZh, ref.canonical),
				"name":       ref.canonical,
				"kind":       "asset",
				"asset_type": ref.assetType,
			})
		} else {
			warnings = append(warnings, map[string]any{
				"key": "ref_ad_reference_skipped", "params": map[string]any{"type": ref.assetType, "name": ref.name},
			})
		}
	}
	return entries, warnings
}

// ClampAdReferenceEntries 超出后端参考张数上限时裁剪 ad 参考条目；产品参考的 sheet 跨产品稳定前置。
func ClampAdReferenceEntries(
	entries []map[string]any, maxRefs *int, providerName, model string,
) ([]map[string]any, []map[string]any) {
	if maxRefs == nil || len(entries) <= *maxRefs {
		return append([]map[string]any(nil), entries...), nil
	}
	var products, others []map[string]any
	for _, entry := range entries {
		kind, _ := entry["kind"].(string)
		if kind == "sheet" || kind == "original" {
			products = append(products, entry)
		} else {
			others = append(others, entry)
		}
	}
	sort.SliceStable(products, func(i, j int) bool {
		iKind, _ := products[i]["kind"].(string)
		jKind, _ := products[j]["kind"].(string)
		return kindRank(iKind) < kindRank(jKind)
	})
	combined := append(products, others...)
	clamped := combined[:*maxRefs]
	displayModel := model
	if displayModel == "" {
		displayModel = providerName
	}
	return clamped, []map[string]any{{
		"key": "ref_too_many_images",
		"params": map[string]any{
			"count": len(entries), "model": displayModel, "max_count": *maxRefs,
		},
	}}
}

func kindRank(kind string) int {
	if kind == "sheet" {
		return 0
	}
	return 1
}

// ApplyProviderConstraints 按供应商能力取时长档位、裁剪 references；回传 warnings
// （i18n key + 参数）。
//
// maxRefs 为 nil 表示不裁参考图；supportedDurations 为空表示能力不可解析、时长原样透传。
func ApplyProviderConstraints(
	providerName, model string, maxRefs *int, supportedDurations []int,
	references []string, durationSeconds int, registryProviderID, resolution string,
) ([]string, int, []map[string]any) {
	var warnings []map[string]any
	newRefs := append([]string(nil), references...)
	if maxRefs != nil && len(references) > *maxRefs {
		newRefs = references[:*maxRefs]
		if strings.EqualFold(providerName, "openai") && strings.HasPrefix(strings.ToLower(model), "sora") && *maxRefs == 1 {
			warnings = append(warnings, map[string]any{"key": "ref_sora_single_ref", "params": map[string]any{}})
		} else {
			displayModel := model
			if displayModel == "" {
				displayModel = providerName
			}
			warnings = append(warnings, map[string]any{
				"key": "ref_too_many_images",
				"params": map[string]any{
					"count": len(references), "model": displayModel, "max_count": *maxRefs,
				},
			})
		}
	}

	slot := service.ResolveDurationSlot(float64(durationSeconds), service.EffectiveReferenceDurations(
		registryProviderID, model, supportedDurations, resolution, len(newRefs) > 0,
	))
	displayModel := model
	if displayModel == "" {
		displayModel = providerName
	}
	if warning := durationSlotWarning(slot, displayModel); warning != nil {
		warnings = append(warnings, warning)
	}
	return newRefs, slot.Seconds, warnings
}

// durationSlotWarning 是取档偏移了剧本编排时的任务 warning（i18n key + 参数）；未偏移返回 nil。
func durationSlotWarning(slot service.DurationSlot, model string) map[string]any {
	if !slot.NeedsConfirmation() {
		return nil
	}
	key := "ref_duration_exceeded"
	if slot.Adjustment == service.AdjustmentUp {
		key = "ref_duration_rounded_up"
	}
	return map[string]any{
		"key": key,
		"params": map[string]any{
			"total": slot.TotalSeconds, "duration": slot.Seconds, "model": model,
		},
	}
}

// BuildReferenceAudioWiring 把渲染产物的 audio_speakers（+ 图号对齐下标）组装成请求字段
// （对应 _build_reference_audio_wiring）。
//
// referenceAudioPerImage 为 true 时按下标同时过滤两个列表而非直接 zip 全量，保证两列表等长同序。
func BuildReferenceAudioWiring(
	rendered RenderedUnitPrompt, audioPaths map[string]string, referenceAudioPerImage bool,
) ([]string, []int) {
	if referenceAudioPerImage {
		var files []string
		var targets []int
		for i, name := range rendered.AudioSpeakers {
			var idx *int
			if i < len(rendered.AudioSpeakerReferenceIndex) {
				idx = rendered.AudioSpeakerReferenceIndex[i]
			}
			if idx == nil {
				continue
			}
			files = append(files, audioPaths[name])
			targets = append(targets, *idx)
		}
		return files, targets
	}
	files := make([]string, 0, len(rendered.AudioSpeakers))
	for _, name := range rendered.AudioSpeakers {
		files = append(files, audioPaths[name])
	}
	return files, nil
}

// PathIsFile 判定路径指向一个已存在的普通文件（目录或缺失均为 false）。
func PathIsFile(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
