// 参考生视频模式 Prompt 构建器（对应 lib/prompt_builders_reference.py）。
//
// 两级 prompt 注入的书写层语法规范取自同一份常量 reference.WritingSyntaxSpec：LLM 产出与人在编辑器书写的
// 是同一种格式，语法只能有一份措辞。
package script

import (
	"fmt"
	"github.com/shotwise/shotwise/internal/service"
	"strconv"
	"strings"

	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/service/reference"
)

// formatReferenceAssetNames 渲染资产块：`- 名称: 描述`（对应参考 builder 的 _format_asset_names）。
func formatReferenceAssetNames(bucket *project.AssetBucket) string {
	names := assetNames(bucket)
	if len(names) == 0 {
		return "（暂无）"
	}
	lines := make([]string, 0, len(names))
	for _, name := range names {
		desc := ""
		if bucket != nil {
			if entry, ok := (*bucket)[name]; ok && entry != nil {
				desc = entry.Description()
			}
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", name, desc))
	}
	return strings.Join(lines, "\n")
}

// referenceCandidateBlock 渲染候选名块（对应 _candidate_block，用 ", " 连接、空集回退「（暂无）」）。
func referenceCandidateBlock(characters, scenes, props *project.AssetBucket) string {
	join := func(names []string) string {
		if len(names) == 0 {
			return "（暂无）"
		}
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("  - character: %s\n  - scene: %s\n  - prop: %s",
		join(assetNames(characters)), join(assetNames(scenes)), join(assetNames(props)))
}

// formatReferenceOutlineBlock 把分集大纲渲染为 XML 块；两者皆空时返回空串。
func formatReferenceOutlineBlock(episodeOutline, nextEpisodeOutline map[string]any) string {
	var blocks []string
	for _, entry := range []struct {
		tag     string
		outline map[string]any
	}{{"episode_outline", episodeOutline}, {"next_episode_outline", nextEpisodeOutline}} {
		if len(entry.outline) == 0 {
			continue
		}
		var lines []string
		if title, ok := entry.outline["title"].(string); ok && strings.TrimSpace(title) != "" {
			lines = append(lines, "标题："+strings.TrimSpace(title))
		}
		if hook, ok := entry.outline["hook"].(string); ok && strings.TrimSpace(hook) != "" {
			lines = append(lines, "钩子："+strings.TrimSpace(hook))
		}
		if beats, ok := entry.outline["story_beats"].([]any); ok {
			for _, raw := range beats {
				if beat, ok := raw.(string); ok && strings.TrimSpace(beat) != "" {
					lines = append(lines, "- "+beat)
				}
			}
		}
		if teaser, ok := entry.outline["next_episode_teaser"].(string); ok && strings.TrimSpace(teaser) != "" {
			lines = append(lines, "下集预告："+strings.TrimSpace(teaser))
		}
		if len(lines) > 0 {
			blocks = append(blocks, "<"+entry.tag+">\n"+strings.Join(lines, "\n")+"\n</"+entry.tag+">")
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	return strings.Join(blocks, "\n\n") + "\n\n"
}

// ReferenceUnitsSplitPromptOptions 是 BuildReferenceUnitsSplitPrompt 的入参。
type ReferenceUnitsSplitPromptOptions struct {
	NovelText          string
	ProjectOverview    *project.Overview
	Characters         *project.AssetBucket
	Scenes             *project.AssetBucket
	Props              *project.AssetBucket
	SupportedDurations []int
	ReferenceDurations []int
	TextDurations      []int
	MaxDuration        int
	MaxReferenceImages *int
	DefaultDuration    *int
	Episode            int
	TargetLanguage     string
	SourceLanguage     string
	SpeechRateOverride *float64
	EpisodeOutline     map[string]any
	NextEpisodeOutline map[string]any
}

// BuildReferenceUnitsSplitPrompt 构建 step-1 video_unit 拆分 prompt：源文 → 扁平 unit 表。
func BuildReferenceUnitsSplitPrompt(opts ReferenceUnitsSplitPromptOptions) (string, error) {
	normalized := service.NormalizeDurations(opts.SupportedDurations)
	if len(normalized) == 0 {
		return "", fmt.Errorf("supported_durations 不能为空：必须提供模型支持的秒数集合")
	}
	if opts.DefaultDuration != nil && !service.ContainsInt(normalized, *opts.DefaultDuration) {
		return "", fmt.Errorf("default_duration=%d 不在 supported_durations=%v 内", *opts.DefaultDuration, normalized)
	}
	refDurations := service.NormalizeDurations(opts.ReferenceDurations)
	textDurations := service.NormalizeDurations(opts.TextDurations)
	for _, tier := range []struct {
		name   string
		values []int
	}{{"reference_supported_durations", refDurations}, {"text_supported_durations", textDurations}} {
		for _, d := range tier.values {
			if !service.ContainsInt(normalized, d) {
				return "", fmt.Errorf("%s=%v 不是 supported_durations=%v 的子集", tier.name, tier.values, normalized)
			}
		}
	}

	durationsStr := joinInts(normalized, ", ")
	tiersDiffer := len(refDurations) > 0 && len(textDurations) > 0 && !intSlicesEqual(refDurations, textDurations)
	referenceRule := ""
	if tiersDiffer {
		referenceRule = "\n     本型号下该档位还随「有无参考图」分两套，按该 unit **镜头描述行里有没有 `@` 资产引用**" +
			"取用（台词行 `@[角色]：{台词}` 的说话人不计入——它不生成参考图，只驱动音色声明）：" +
			"带 `@` 引用取（" + joinInts(refDurations, ", ") + "），" +
			"不带取（" + joinInts(textDurations, ", ") + "）。" +
			"两者取其一：要么改取该 unit 引用状态对应档位内的值，要么调整引用——" +
			"把次要资产融入描述文字、不用 `@` 引用，从而适用不带引用的那套档位。"
	}
	defaultScope := ""
	if opts.DefaultDuration != nil && tiersDiffer {
		inReference := service.ContainsInt(refDurations, *opts.DefaultDuration)
		inText := service.ContainsInt(textDurations, *opts.DefaultDuration)
		if inReference != inText {
			applies := "不带 `@` 引用的"
			if inReference {
				applies = "带 `@` 引用的"
			}
			defaultScope = fmt.Sprintf("（该默认值只落在%s unit 的档位内，另一种状态的 unit 按上面的硬约束取值）", applies)
		}
	}
	defaultRule := "按叙事需要从档位中取值，不强制默认值"
	if opts.DefaultDuration != nil {
		defaultRule = fmt.Sprintf("unit 默认取 %d 秒%s，叙事需要更长时可取更长档（偏好可被内容需要覆盖，硬约束不可）",
			*opts.DefaultDuration, defaultScope)
	}
	maxRefsRule := ""
	if opts.MaxReferenceImages != nil {
		maxRefsRule = fmt.Sprintf("\n- **references 上限**：一个 unit 内 `@` 引用的资产名（去重后）不超过 %d 个；"+
			"超出时把次要角色融入背景描述（不用 `@` 引用），不要压缩主体资产。", *opts.MaxReferenceImages)
	}
	sourceLanguage := opts.SourceLanguage
	speechRate := service.SpeechRateUnitsPerSecond(sourceLanguage, opts.SpeechRateOverride)
	unitLabel := service.ReadingUnitNoun(sourceLanguage)
	targetLanguage := opts.TargetLanguage
	if targetLanguage == "" {
		targetLanguage = "中文"
	}

	return "# 角色与任务\n\n" +
		"你是一位参考生视频单元架构师，本任务是把源文拆分为适配多模态参考视频模型的 video_unit 表（step1 内容拆分）。\n" +
		"每个 video_unit 对应**一次视频生成调用**，含 1-" + strconv.Itoa(reference.MaxShotsPerUnit) + " 个镜头；镜头表示画面切换，但共享同一次生成。\n" +
		"本阶段定的是**结构与内容契约**：unit 边界、时长（时长即计费单位）、台词落位、核心资产指认——用户会逐 unit 审阅确认这份契约。\n" +
		"视觉编排（景别 / 构图 / 运镜扩写）由后续 step2 以你的拆分为基底生成，本阶段不写。\n\n" +
		"**输出语言**：所有字符串值必须使用 " + targetLanguage + "；JSON 键名保持英文。\n" +
		"例外（逐字保留、不翻译）：`@[名称]` 中的资产名须逐字等于下方候选表中的登记名；`source_text` 须逐字复制小说原文。\n" +
		"**结构约束**：字段 / 枚举 / 必填项由 response_schema 强制；本提示只解释**如何写好每个字段的内容**。\n\n" +
		"# 上下文\n\n" +
		"<overview>\n" + overviewValue(opts.ProjectOverview, "synopsis") + "\n\n" +
		"题材：" + overviewValue(opts.ProjectOverview, "genre") + "\n" +
		"主题：" + overviewValue(opts.ProjectOverview, "theme") + "\n" +
		"世界观：" + overviewValue(opts.ProjectOverview, "world_setting") + "\n" +
		"</overview>\n\n" +
		"<characters>\n" + formatReferenceAssetNames(opts.Characters) + "\n</characters>\n\n" +
		"<scenes>\n" + formatReferenceAssetNames(opts.Scenes) + "\n</scenes>\n\n" +
		"<props>\n" + formatReferenceAssetNames(opts.Props) + "\n</props>\n\n" +
		"## 小说原文\n\n" +
		"<novel>\n" + opts.NovelText + "\n</novel>\n\n" +
		formatReferenceOutlineBlock(opts.EpisodeOutline, opts.NextEpisodeOutline) + "# 拆分规则\n\n" +
		"当前正在生成第 " + strconv.Itoa(opts.Episode) + " 集。请覆盖全部源文情节，按叙事顺序逐 unit 产出。\n\n" +
		"- **unit 边界**：每个 unit 对应一个连贯的视频生成片段——同一时间、同一地点、主体动作连续；\n" +
		"  时间 / 空间 / 情节重大切换点开新 unit。\n" +
		"- **source_text**：该 unit 所依据的小说原文片段，**逐字复制**（可截断首尾，但中间不得删字、改写、翻译或概括）。\n" +
		"  它是追溯锚，用于把生成结果对回原文；不逐字复制会被机械校验拒绝。\n" +
		"- **时长决策序**（自上而下，高优先级是硬边界，低优先级在其内做优化）：\n" +
		"  1. 硬约束：`duration_seconds` 是 unit 时长（一次生成调用一个时长），必须取支持档位（" + durationsStr + "）中的值。\n" +
		"     叙事需要的时长放不下时，把该 unit 按叙事顺序重拆为多个 unit，**不得违约时长**。" + referenceRule + "\n" +
		"  2. 台词下界：先估算该 unit 全部台词与画外音念完约需的秒数（口播语速约 " + formatFloat(speechRate) + " " + unitLabel + "/秒），\n" +
		"     取**不低于**这个秒数的档位。这是单向下界——台词永不压进念不完的短档；无台词的 unit 没有此下界。\n" +
		"     台词量超过最长档（" + strconv.Itoa(opts.MaxDuration) + " 秒）时把该 unit 拆开，不要把台词硬塞进一个 unit。\n" +
		"  3. 默认偏好：" + defaultRule + "。\n" +
		"  4. 打包效率：在 1-3 之内组织镜头，使 unit 时长贴近 " + strconv.Itoa(opts.MaxDuration) + " 秒；不要默认选最短 / 保守值。" + maxRefsRule + "\n\n" +
		"# 正文书写语法\n\n" +
		reference.WritingSyntaxSpec + "\n\n" +
		"# 本阶段的正文写作指引\n\n" +
		"- 镜头行的描述聚焦当下瞬间的**可见动作**：谁做了什么、物件互动、环境动态；动词描述物理可观察动作\n" +
		"  （伸手 / 转身 / 推门 / 投向），避免「陷入 / 回忆 / 意识到 / 决定」等内心动词。\n" +
		"- 资产名必须逐字取自下列候选，不要发明候选之外的名称：\n" +
		referenceCandidateBlock(opts.Characters, opts.Scenes, opts.Props) + "\n" +
		"- 原文里的人物对白写成台词行（`@[角色]：{台词}`），旁白 / 心声写成画外音行（`{台词}`），逐字保留原文措辞；\n" +
		"  台词是内容契约的一部分，step2 不会再改动它。\n" +
		"- 本阶段不写景别 / 构图 / 运镜（step2 补），把叙事内容与动作过程写清楚即可。\n", nil
}

// RenderReferenceUnitsForStep2 把 step1 units 渲染为 step2 prompt 的输入文本。
func RenderReferenceUnitsForStep2(units []map[string]any) string {
	blocks := make([]string, 0, len(units))
	for index, unit := range units {
		duration := service.IntValueOrZero(unit["duration_seconds"])
		shots, _ := unit["shots"].([]any)
		body := reference.RenderShotsTextList(shots)
		blocks = append(blocks, fmt.Sprintf("#### unit %d（时长 %ds）\n%s", index+1, duration, body))
	}
	return strings.Join(blocks, "\n\n")
}

// ReferenceVideoPromptOptions 是 BuildReferenceVideoPrompt 的入参。
type ReferenceVideoPromptOptions struct {
	ProjectOverview  *project.Overview
	Style            string
	StyleDescription string
	Characters       *project.AssetBucket
	Scenes           *project.AssetBucket
	Props            *project.AssetBucket
	Step1Units       []map[string]any
	MaxRefs          *int
	Episode          int
	AspectRatio      string
	TargetLanguage   string
}

// BuildReferenceVideoPrompt 构建参考生视频模式 step2（视觉展开）的 LLM Prompt。
func BuildReferenceVideoPrompt(opts ReferenceVideoPromptOptions) (string, error) {
	maxRefsLine := ""
	if opts.MaxRefs != nil {
		maxRefsLine = fmt.Sprintf("\n- 单个 unit 内 `@` 引用的资产名（去重后）不超过 %d 个（模型上限）；"+
			"超出时把次要角色合并到背景描述，不用 `@` 引用。", *opts.MaxRefs)
	}
	targetLanguage := opts.TargetLanguage
	if targetLanguage == "" {
		targetLanguage = "中文"
	}
	aspectRatio := opts.AspectRatio
	if aspectRatio == "" {
		aspectRatio = "9:16"
	}

	return "# 角色与任务\n\n" +
		"你是一位资深的短视频分镜编剧，本任务是为「参考生视频」模式的第 " + strconv.Itoa(opts.Episode) + " 集做**视觉展开**。\n" +
		"下方 step1_units 表给出的是已经用户确认的内容契约；你的任务是逐 unit 把正文扩写出景别 / 构图 / 运镜与画面细节。\n\n" +
		"**输出语言**：所有字符串值必须使用 " + targetLanguage + "；JSON 键名保持英文。\n" +
		"**结构约束**：字段 / 必填项由 response_schema 强制；本提示只解释**如何写好正文**。\n\n" +
		"# 保结构要求（违反即整份产出被拒）\n\n" +
		"- `units` 数组与 step1_units **等长、同序**：不合并、不拆分、不增删 unit。\n" +
		"- 每个 unit 的**台词行与画外音行逐字保留**：不改词、不增删、不重排、不换说话人。\n" +
		"  台词配不上你想要的画面时，请按台词写画面——**不要**改台词。\n" +
		"- 镜头行数不增减：step1 写了几个 `镜头N：`，展开后仍是几个。\n" +
		"- 正文里新出现的 `@[名称]` 必须是候选表中的登记名（step1 没引用过的资产也可以引用，但必须已登记）。" + maxRefsLine + "\n\n" +
		"# 上下文\n\n" +
		"<overview>\n" + overviewValue(opts.ProjectOverview, "synopsis") + "\n\n" +
		"题材：" + overviewValue(opts.ProjectOverview, "genre") + "\n" +
		"主题：" + overviewValue(opts.ProjectOverview, "theme") + "\n" +
		"世界观：" + overviewValue(opts.ProjectOverview, "world_setting") + "\n" +
		"</overview>\n\n" +
		"<style>\n风格：" + opts.Style + "\n描述：" + opts.StyleDescription + "\n画面比例：" + aspectRatio + "\n</style>\n\n" +
		"<characters>\n" + formatReferenceAssetNames(opts.Characters) + "\n</characters>\n\n" +
		"<scenes>\n" + formatReferenceAssetNames(opts.Scenes) + "\n</scenes>\n\n" +
		"<props>\n" + formatReferenceAssetNames(opts.Props) + "\n</props>\n\n" +
		"<step1_units>\n" + RenderReferenceUnitsForStep2(opts.Step1Units) + "\n</step1_units>\n\n" +
		"# 正文书写语法\n\n" +
		reference.WritingSyntaxSpec + "\n\n" +
		"# 视觉展开写作指引\n\n" +
		"镜头行的描述将直接驱动该镜头的视频生成，按「景别 → 构图 → 运镜 → 画面内容」四要素依次组织，写足画面信息、宁详勿略：\n\n" +
		"- 景别：大全景 / 全景 / 中景 / 近景 / 特写，及拍摄角度（俯拍 / 仰拍 / 平视）。\n" +
		"- 构图：主体在画面中的位置、前景与背景的关系（如中心构图、对角线构图、以公路 / 廊柱作引导线）。\n" +
		"- 运镜：机位与镜头运动（固定机位 / 跟随 / 推近 / 拉远 / 摇移），含镜头内焦点主体的变更。\n" +
		"- 画面内容：占篇幅大头——镜头时长内发生的全部可见运动：每个出场主体各自的动作链（肢体 / 手势 / 神态过渡）、\n" +
		"  物件互动、背景与环境动态（人群、天气、衣摆、光影移动），可带运动质感（如动态模糊），末尾用一句点明氛围基调。\n" +
		"  动作量与 unit 时长匹配：镜头多则每镜完成一个连贯动作 + 一个细节互动，镜头少则随时长递增动作段数。\n" +
		"- 角色 / 场景 / 道具仅用 `@[名称]` 引用，候选：\n" +
		referenceCandidateBlock(opts.Characters, opts.Scenes, opts.Props) + "\n" +
		"  外貌、服装、场景陈设等静态外观由参考图承担，**不要**在文本里描写；动作、姿态、互动与环境动态则写得越具体越好。\n" +
		"  动词应描述物理可观察动作（伸手 / 转身 / 摩挲 / 投向 / 收紧），避免「陷入 / 回忆 / 意识到 / 决定」等内心动词。\n" +
		"- 正例：「镜头1：景别：中景，轻微仰拍。构图：@[角色A] 居画面中心，@[场景A] 的窗棂与案几为前景。运镜：固定机位，缓慢推近。\n" +
		"  画面内容：@[角色A] 在 @[场景A] 中缓步走向窗前，抬手推开木窗，衣摆随穿堂风轻扬；随后低头凝视手中的 @[道具A]，\n" +
		"  指尖缓缓收紧，呼吸放缓，目光从 @[道具A] 缓慢抬起投向窗外；烛焰随风明灭，光影在面部缓慢移动，渲染压抑而克制的氛围。」\n" +
		"- 反例（过短）：「镜头1：@[角色A] 站在 @[场景A] 里。」——没有景别 / 构图 / 运镜，也没有动作过程与环境动态，生成的视频会近乎静止。\n" +
		"- 反例（写外貌）：「身穿某色服装的角色A 站在某色场景A 前」——外貌 / 服装 / 颜色应由参考图承担，且未用 `@[名称]` 引用。\n\n" +
		"`title` 给本集拟一个简短标题。请按 step1_units 顺序逐 unit 产出。\n", nil
}

// intSlicesEqual 判定两个已排序的时长切片相等。
func intSlicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
