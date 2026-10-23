// 广告/短片模式（content_mode=ad）剧本生成 Prompt 构建器（对应 lib/prompt_builders_ad.py）。
//
// 产出平铺 shots[] 的带货镜头脚本 prompt：按目标总时长选择带货八段框架的时长配比档位
// （15/30/60/90 秒，经维护者审定的配比表）。products 为空时自动分流为通用短片 prompt。
package script

import (
	"fmt"
	"github.com/shotwise/shotwise/internal/service"
	"strconv"
	"strings"

	"github.com/shotwise/shotwise/internal/domain/project"
)

// ReferenceShotDurationRange 是 ad + 参考生视频路径下单镜头时长的合法区间（秒）。
const (
	ReferenceShotDurationRangeLow  = 1
	ReferenceShotDurationRangeHigh = 15
)

// adDurationTiers 是四个审定档位（秒）。
var adDurationTiers = []int{15, 30, 60, 90}

// adDefaultTier 是默认推荐档（秒），档位等距 tie-break 的锚点。
const adDefaultTier = 30

// adSectionValues 是 section 八值引导。
var adSectionValues = []string{
	"hook", "pain_point", "product_reveal", "selling_point", "demo", "trust", "price_promo", "cta",
}

const adGeneralRules = "通用规则（适用于全部档位）：\n\n" +
	"- hook 与 cta 是绝对时长段（hook 2-4s、cta 3-6s），不随档位等比放大；加长的秒数优先给 selling_point/demo，其次 trust\n" +
	"- price_promo 永远紧贴 cta 构成「促单收尾块」\n" +
	"- 即使 hook 不是产品画面，产品也应在前 3 秒内入画（文字/局部/手持均可）\n" +
	"- 单 section 超过 6 秒必须拆成多个镜头；全片平均 3-5 秒/镜，开头允许 2-3 秒快切；镜头数宁多勿少（多场景多角度有平台官方数据背书）\n" +
	"- 30 秒档为默认推荐档；90 秒档用「小故事」组织而非平铺卖点，仅适合高客单/需教育的产品"

var adTierTables = map[int]string{
	15: "15 秒档（冲动型/投流款，5-6 镜头；只保五段：pain_point 并入 hook、trust 砍掉、price_promo 并入 cta）：\n\n" +
		"| section | 秒数 | 累计 | 镜头数 |\n|---|---|---|---|\n" +
		"| hook（兼任 pain_point） | 3 | 0-3 | 1 |\n" +
		"| product_reveal | 2 | 3-5 | 1 |\n" +
		"| selling_point（1 个核心卖点） | 3 | 5-8 | 1 |\n" +
		"| demo | 4 | 8-12 | 1-2 |\n" +
		"| cta（可带一句促销词兼任 price_promo） | 3 | 12-15 | 1 |",
	30: "30 秒档（标准带货位，默认推荐，8-10 镜头；八段全保，trust 与 price_promo 压成一句话镜头；产品极低客单且无可信背书时首砍 trust、秒数还给 demo）：\n\n" +
		"| section | 秒数 | 累计 | 镜头数 |\n|---|---|---|---|\n" +
		"| hook | 3 | 0-3 | 1 |\n" +
		"| pain_point | 4 | 3-7 | 1-2 |\n" +
		"| product_reveal | 3 | 7-10 | 1 |\n" +
		"| selling_point（1-2 个） | 6 | 10-16 | 2 |\n" +
		"| demo | 6 | 16-22 | 2 |\n" +
		"| trust（一句话社证） | 3 | 22-25 | 1 |\n" +
		"| price_promo | 2 | 25-27 | 1 |\n" +
		"| cta | 3 | 27-30 | 1 |",
	60: "60 秒档（完整说服链，13-16 镜头；八段全保各自成块，增量给 selling_point+demo）：\n\n" +
		"| section | 秒数 | 累计 | 镜头数 |\n|---|---|---|---|\n" +
		"| hook | 3 | 0-3 | 1 |\n" +
		"| pain_point（1-2 个具体情境） | 7 | 3-10 | 2 |\n" +
		"| product_reveal | 5 | 10-15 | 1-2 |\n" +
		"| selling_point（2-3 个，每个 4-6s 一镜） | 12 | 15-27 | 3 |\n" +
		"| demo（多角度/前后对比） | 15 | 27-42 | 3-4 |\n" +
		"| trust（评价+销量/资质） | 8 | 42-50 | 2 |\n" +
		"| price_promo（原价锚定→到手价→限时） | 5 | 50-55 | 1-2 |\n" +
		"| cta（行动指令+重申核心利益） | 5 | 55-60 | 1 |",
	90: "90 秒档（叙事型/高客单，18-22 镜头；八段全保，增量给 demo/selling_point/trust/pain_point）：\n\n" +
		"| section | 秒数 | 累计 | 镜头数 |\n|---|---|---|---|\n" +
		"| hook（可用悬念/故事钩） | 4 | 0-4 | 1 |\n" +
		"| pain_point（人物+冲突小叙事） | 10 | 4-14 | 2-3 |\n" +
		"| product_reveal（转折点：遇见产品） | 6 | 14-20 | 1-2 |\n" +
		"| selling_point（3 个，逐个展开） | 20 | 20-40 | 3-4 |\n" +
		"| demo（多场景使用过程，核心块） | 24 | 40-64 | 4-6 |\n" +
		"| trust（证言/检测报告/销量） | 12 | 64-76 | 2-3 |\n" +
		"| price_promo（价格锚定+优惠拆解） | 8 | 76-84 | 2 |\n" +
		"| cta（紧迫感收口） | 6 | 84-90 | 1 |",
}

// NearestAdTier 取最近的审定档位；等距时取更接近默认推荐档（30 秒）的一侧。
func NearestAdTier(target int) int {
	best := adDurationTiers[0]
	bestDiff := absInt(best - target)
	bestTie := absInt(best - adDefaultTier)
	for _, tier := range adDurationTiers[1:] {
		diff := absInt(tier - target)
		tie := absInt(tier - adDefaultTier)
		if diff < bestDiff || (diff == bestDiff && tie < bestTie) {
			best, bestDiff, bestTie = tier, diff, tie
		}
	}
	return best
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// formatDurationConstraint 生成时长约束描述：连续整数集 ≥5 用区间表达，否则枚举。
func formatDurationConstraint(supportedDurations []int, defaultDuration *int) (string, error) {
	normalized := service.NormalizeDurations(supportedDurations)
	if len(normalized) == 0 {
		return "", fmt.Errorf("supported_durations 不能为空：调用方必须提供 model 的合法时长列表")
	}
	isContinuous := len(normalized) >= 5
	for i := 1; i < len(normalized); i++ {
		if normalized[i] != normalized[i-1]+1 {
			isContinuous = false
			break
		}
	}
	var body string
	if isContinuous {
		body = fmt.Sprintf("%d 到 %d 秒间整数任选", normalized[0], normalized[len(normalized)-1])
	} else {
		body = "从 [" + joinInts(normalized, ", ") + "] 秒中选择"
	}
	if defaultDuration != nil {
		if !service.ContainsInt(normalized, *defaultDuration) {
			return "", fmt.Errorf("default_duration=%d 不在 supported_durations=%v 内，调用方必须保证默认值合法（否则 prompt 会自相矛盾）",
				*defaultDuration, normalized)
		}
		return fmt.Sprintf("时长：%s，默认 %d 秒", body, *defaultDuration), nil
	}
	return fmt.Sprintf("时长：%s，按内容节奏自行决定", body), nil
}

// adShotDurationConstraint 按 generation_mode 渲染单镜头时长约束文本。
func adShotDurationConstraint(generationMode string, supportedDurations []int) (string, error) {
	if generationMode == string(project.GenerationReferenceVideo) {
		full := make([]int, 0, ReferenceShotDurationRangeHigh-ReferenceShotDurationRangeLow+1)
		for d := ReferenceShotDurationRangeLow; d <= ReferenceShotDurationRangeHigh; d++ {
			full = append(full, d)
		}
		return formatDurationConstraint(full, nil)
	}
	if len(supportedDurations) == 0 {
		return "", fmt.Errorf("storyboard 路径必须提供 supported_durations（视频模型的合法时长集合）")
	}
	return formatDurationConstraint(supportedDurations, nil)
}

// formatPacingBlock 渲染配比段：通用规则 + 命中档位的审定表；非四档整数附按比例适配说明。
func formatPacingBlock(targetDuration int) string {
	tier := NearestAdTier(targetDuration)
	var parts []string
	parts = append(parts, adGeneralRules, adTierTables[tier])
	if tier != targetDuration {
		parts = append(parts, fmt.Sprintf("目标总时长 %d 秒不在审定档位内，按最近档位 %d 秒的配比模板"+
			"按比例适配到 %d 秒：hook 与 cta 是绝对时长段维持原秒数，"+
			"伸缩量按通用规则优先给 selling_point/demo，其次 trust。", targetDuration, tier, targetDuration))
	}
	return strings.Join(parts, "\n\n")
}

// formatProducts 渲染产品信息块：名称 / 品牌 / 描述 / 卖点（selling_points）。
func formatProducts(products *project.AssetBucket) string {
	names := assetNames(products)
	if len(names) == 0 {
		return ""
	}
	var lines []string
	for _, name := range names {
		entry := (*products)[name]
		lines = append(lines, "### "+name)
		if brand := entry.MustString("brand"); brand != "" {
			lines = append(lines, "品牌："+brand)
		}
		if desc := entry.MustString("description"); desc != "" {
			lines = append(lines, "描述："+desc)
		}
		var points []string
		if raw, ok := entry.Fields()["selling_points"].([]any); ok {
			for _, item := range raw {
				if point, ok := item.(string); ok && strings.TrimSpace(point) != "" {
					points = append(points, point)
				}
			}
		}
		if len(points) > 0 {
			lines = append(lines, "卖点：")
			for _, point := range points {
				lines = append(lines, "- "+point)
			}
		}
		lines = append(lines, "")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// AdPromptOptions 是 BuildAdPrompt 的入参。
type AdPromptOptions struct {
	ProjectOverview    *project.Overview
	Style              string
	StyleDescription   string
	Characters         *project.AssetBucket
	Scenes             *project.AssetBucket
	Props              *project.AssetBucket
	Products           *project.AssetBucket
	Brief              string
	TargetDuration     int
	GenerationMode     string
	SupportedDurations []int
	Episode            int
	AspectRatio        string
	TargetLanguage     string
	SpeechRateOverride *float64
}

// BuildAdPrompt 构建广告/短片模式的剧本生成 prompt。
func BuildAdPrompt(opts AdPromptOptions) (string, error) {
	if opts.TargetDuration <= 0 {
		return "", fmt.Errorf("target_duration 必须为正整数秒，当前为 %d", opts.TargetDuration)
	}
	durationConstraint, err := adShotDurationConstraint(opts.GenerationMode, opts.SupportedDurations)
	if err != nil {
		return "", err
	}
	targetLanguage := opts.TargetLanguage
	if targetLanguage == "" {
		targetLanguage = "中文"
	}
	aspectRatio := opts.AspectRatio
	if aspectRatio == "" {
		aspectRatio = "9:16"
	}
	speechRate := service.SpeechRateUnitsPerSecond(targetLanguage, opts.SpeechRateOverride)
	unitLabel := service.ReadingUnitNoun(targetLanguage)
	voiceoverRateNote := fmt.Sprintf("口播长度按约 %s %s/秒折算", formatFloat(speechRate), unitLabel)

	episode := opts.Episode
	characterNames := assetNames(opts.Characters)
	sceneNames := assetNames(opts.Scenes)
	propNames := assetNames(opts.Props)
	productNames := assetNames(opts.Products)

	commonHeader := "**输出语言**：所有字符串值必须使用 " + targetLanguage + "；JSON 键名 / 枚举值保持英文。\n" +
		"**结构约束**：字段 / 枚举 / 必填项由 response_schema 强制；本提示只解释**如何写好每个字段的内容**。\n\n" +
		"# 上下文\n\n" +
		"<overview>\n" + overviewValue(opts.ProjectOverview, "synopsis") + "\n\n" +
		"题材：" + overviewValue(opts.ProjectOverview, "genre") + "\n" +
		"主题：" + overviewValue(opts.ProjectOverview, "theme") + "\n" +
		"世界观：" + overviewValue(opts.ProjectOverview, "world_setting") + "\n" +
		"</overview>\n\n" +
		"<style>\n风格：" + opts.Style + "\n描述：" + opts.StyleDescription + "\n" +
		"画面比例：" + aspectRatio + "（" + formatAspectRatioDesc(aspectRatio) + "）\n" +
		"</style>\n\n" +
		"<brief>\n" + briefOrDefault(opts.Brief) + "\n</brief>\n\n" +
		"<characters>\n" + formatNames(opts.Characters) + "\n</characters>\n\n" +
		"<scenes>\n" + formatNames(opts.Scenes) + "\n</scenes>\n\n" +
		"<props>\n" + formatNames(opts.Props) + "\n</props>"

	commonConstraints := fmt.Sprintf("<episode_constraints>\n"+
		"本片为单视频（恒第 %d 集）。所有 shot_id 必须严格使用 `E%dS{两位序号}` 格式（如 E%dS01、E%dS02），"+
		"按播放顺序连续编号，不得使用其他集号前缀。\n</episode_constraints>", episode, episode, episode, episode)

	commonFieldGuides := "## 图片提示词（image_prompt）——切换到「摄影师」视角\n\n" +
		"- **image_prompt.scene**：" + sceneWritingGuide + "\n" +
		"- **image_prompt.composition.shot_type**：从枚举中按画面内容选择，不强加倾向。\n" +
		"- **image_prompt.composition.lighting**：" + lightingWritingGuide + "\n" +
		"- **image_prompt.composition.ambiance**：" + ambianceWritingGuide + "\n\n" +
		"## 视频提示词（video_prompt）——切换到「动作设计师」视角\n\n" +
		"- **video_prompt.action**：" + actionWritingGuide + "\n" +
		"- **video_prompt.camera_motion**：按画面内容自行选择。\n" +
		"- **video_prompt.ambiance_audio**：" + ambianceAudioWritingGuide + "\n" +
		"- **video_prompt.dialogue**：仅当镜头内有出镜人物开口说话时填写（口播旁白写在 voiceover_text，不要重复进 dialogue）；" +
		"speaker 必须出现在 characters_in_shot。"

	if len(productNames) == 0 {
		return "# 角色与任务\n\n" +
			"你是一位资深的短视频编导，精通把一段创作诉求转写为可直接驱动 AI 图像 / 视频生成的结构化镜头脚本。\n" +
			"你的任务：基于下方创作 brief，产出一支约 " + strconv.Itoa(opts.TargetDuration) + " 秒的通用短片镜头脚本（平铺 shots[]），符合 schema 的 JSON。\n\n" +
			commonHeader + "\n\n" +
			"# 时长与节奏\n\n" +
			"- 全片目标总时长 " + strconv.Itoa(opts.TargetDuration) + " 秒，各镜头 duration_seconds 之和应贴近该值。\n" +
			"- 单镜头" + durationConstraint + "；全片平均 3-5 秒/镜，按内容节奏自行切分。\n\n" +
			commonConstraints + "\n\n" +
			"# 字段写作指引\n\n" +
			"## 基础字段\n\n" +
			"- **section**：本片无带货框架，按内容自拟简短英文段落标签（如 opening/development/climax/ending），用于标记镜头在叙事中的位置。\n" +
			"- **voiceover_text**：每镜头的口播文案，必须完整可照稿配音；无口播的纯画面镜头填空字符串。" + voiceoverRateNote + "。\n" +
			"- **characters_in_shot** / **scenes** / **props**：仅列出此镜头画面中实际出现的资产。\n" +
			"  - 候选 characters：[" + joinComma(characterNames, "（无）") + "]\n" +
			"  - 候选 scenes：[" + joinComma(sceneNames, "（无）") + "]\n" +
			"  - 候选 props：[" + joinComma(propNames, "（无）") + "]\n" +
			"  - 不要发明候选之外的名称。\n" +
			"- **products_in_shot**：本项目无产品资产，所有镜头一律填空数组。\n\n" +
			commonFieldGuides + "\n\n" +
			"# 创作目标\n\n" +
			"输出可直接驱动 AI 生成的、视觉一致、节奏紧凑的短片脚本。忠于创作 brief、保留情绪张力。\n", nil
	}

	return "# 角色与任务\n\n" +
		"你是一位资深的带货短视频编导，精通把产品卖点与创作诉求转写为可直接驱动 AI 图像 / 视频生成的结构化镜头脚本。\n" +
		"你的任务：基于下方产品信息与创作 brief，按带货八段框架与时长配比，产出一支约 " + strconv.Itoa(opts.TargetDuration) + " 秒的带货短视频镜头脚本（平铺 shots[]），符合 schema 的 JSON。\n\n" +
		commonHeader + "\n\n" +
		"<products>\n" + formatProducts(opts.Products) + "\n</products>\n\n" +
		"# 带货八段框架与时长配比\n\n" +
		"带货短视频按八个段落组织：" + strings.Join(adSectionValues, " → ") + "。\n" +
		"下方配比表经审定，是各段时长与镜头数的执行标准：\n\n" +
		formatPacingBlock(opts.TargetDuration) + "\n\n" +
		commonConstraints + "\n\n" +
		"# 字段写作指引\n\n" +
		"对每个镜头，按下列章节填写字段。\n\n" +
		"## 基础字段\n\n" +
		"- **section**：该镜头所属的带货框架段落标签，使用上方八值（如 hook、pain_point）；同段多镜头重复同一标签。\n" +
		"- **voiceover_text**：每镜头的口播文案，必须完整可照稿配音、与画面同步；无口播的纯画面镜头填空字符串。全片口播连起来应是一篇完整流畅的带货话术，" + voiceoverRateNote + "。\n" +
		"- **duration_seconds**：单镜头" + durationConstraint + "；各段合计遵循配比表，全片总和贴近 " + strconv.Itoa(opts.TargetDuration) + " 秒。\n" +
		"- **products_in_shot**：该镜头画面中实际出现的产品名称列表（产品入画即列出，含局部/手持/包装）；氛围镜头填空数组。\n" +
		"  - 候选 products：[" + joinComma(productNames, "（暂无）") + "]\n" +
		"  - 不要发明候选之外的名称。\n" +
		"- **characters_in_shot** / **scenes** / **props**：仅列出此镜头画面中实际出现的资产。\n" +
		"  - 候选 characters：[" + joinComma(characterNames, "（无）") + "]\n" +
		"  - 候选 scenes：[" + joinComma(sceneNames, "（无）") + "]\n" +
		"  - 候选 props：[" + joinComma(propNames, "（无）") + "]\n" +
		"  - 不要发明候选之外的名称。\n\n" +
		commonFieldGuides + "\n\n" +
		"# 创作目标\n\n" +
		"输出可直接驱动 AI 生成的、产品忠实、节奏紧凑的带货镜头脚本。卖点表达贴合 <products> 的 selling_points，不夸大、不虚构功效。\n", nil
}

// joinComma 用 ", " 连接名字清单（空集回退 fallback，与 Python `", ".join(...)` 同口径）。
func joinComma(names []string, fallback string) string {
	if len(names) == 0 {
		return fallback
	}
	return strings.Join(names, ", ")
}

func briefOrDefault(brief string) string {
	if brief == "" {
		return "（未提供，按产品信息与常识自行设计）"
	}
	return brief
}

// adReferenceEpisodeScriptSchema 是 ad + reference_video 路径的剧集脚本 schema：镜头时长 1-15 自由整数。
func adReferenceEpisodeScriptSchema() map[string]any {
	return adEpisodeScriptSchemaWithDuration(map[string]any{
		"type": "integer", "minimum": ReferenceShotDurationRangeLow, "maximum": ReferenceShotDurationRangeHigh,
	})
}

// adEpisodeScriptSchema 是 ad storyboard 路径的剧集脚本 schema：镜头时长被枚举硬约束。
func adEpisodeScriptSchema(supportedDurations []int) (map[string]any, error) {
	normalized := service.NormalizeDurations(supportedDurations)
	if len(normalized) == 0 {
		return nil, fmt.Errorf("supported_durations 为空，无法构造 duration 枚举约束")
	}
	return adEpisodeScriptSchemaWithDuration(service.DurationConstraint(normalized)), nil
}

// adEpisodeScriptSchemaWithDuration 构造 shots[] 骨架的 ad 剧本 schema（仅 durations 约束不同）。
func adEpisodeScriptSchemaWithDuration(duration map[string]any) map[string]any {
	shot := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"shot_id":            map[string]any{"type": "string", "minLength": 1},
			"section":            map[string]any{"type": "string"},
			"duration_seconds":   duration,
			"voiceover_text":     map[string]any{"type": "string"},
			"products_in_shot":   stringArraySchema(),
			"characters_in_shot": stringArraySchema(),
			"scenes":             stringArraySchema(),
			"props":              stringArraySchema(),
			"image_prompt":       imagePromptSchema(),
			"video_prompt":       adVideoPromptSchema(),
		},
		"required":             []any{"shot_id", "section", "duration_seconds", "voiceover_text", "products_in_shot", "characters_in_shot", "scenes", "props", "image_prompt", "video_prompt"},
		"additionalProperties": false,
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{"type": "string"},
			"shots": map[string]any{"type": "array", "minItems": 1, "items": shot},
		},
		"required": []any{"title", "shots"},
	}
}

func stringArraySchema() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
}

// adVideoPromptSchema 是 ad 的 video_prompt schema（含 dialogue）。
func adVideoPromptSchema() map[string]any {
	return videoPromptSchema(true)
}
