// 剧本生成 Prompt 构建器（drama / narration 两种 content_mode，对应 lib/prompt_builders_script.py）。
//
// 设计原则：
//   - 不重复 schema 已声明的枚举（shot_type / camera_motion 等）；让 response_schema 直接约束。
//   - 多选枚举字段不在 prompt 里写「如何选」判据，让模型按画面内容自行决定。
//   - 不写无法被 LLM 自检的字数硬限制；用示例隐性表达节奏。
//   - 节奏建议由 RenderPacingSection 注入，跨 subagent 与 builder 共享。
package script

import (
	"fmt"
	"github.com/shotwise/shotwise/internal/service"
	"sort"
	"strconv"
	"strings"

	"github.com/shotwise/shotwise/internal/domain/project"
)

// UserInstructionsHeader 是用户意见（instructions）注入分节的统一标题：五个分集生成入口
// （plan / step1 三工具 / step2）共用，措辞保持中性——遵循强度由意见正文自行表达。
const UserInstructionsHeader = "# 用户意见"

// AppendUserInstructions 把用户意见以中性分节追加到 prompt 末尾；空时原样返回。
func AppendUserInstructions(prompt, instructions string) string {
	if instructions == "" {
		return prompt
	}
	return prompt + "\n\n" + UserInstructionsHeader + "\n" + instructions
}

// assetNames 返回资产桶内的登记名（排序，保证 prompt 稳定可复现）。
func assetNames(bucket *project.AssetBucket) []string {
	if bucket == nil || len(*bucket) == 0 {
		return nil
	}
	names := make([]string, 0, len(*bucket))
	for name := range *bucket {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// formatNames 渲染资产名清单（仅名字）。
func formatNames(bucket *project.AssetBucket) string {
	names := assetNames(bucket)
	if len(names) == 0 {
		return "（暂无）"
	}
	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, "- "+name)
	}
	return strings.Join(lines, "\n")
}

// formatAssetsWithDesc 渲染资产块：名称 + 外观描述，供 step2 视觉层写细节时取材。
//
// 缺描述 / 条目缺失时退化为纯名字。名称与描述都是 project.json 动态文本，过 neutralizeTags
// 中和尖括号；多行描述续行缩进，避免 flush-left 溢出标签块。
func formatAssetsWithDesc(bucket *project.AssetBucket) string {
	names := assetNames(bucket)
	if len(names) == 0 {
		return "（暂无）"
	}
	lines := make([]string, 0, len(names))
	for _, name := range names {
		safeName := neutralizeTags(name)
		desc := ""
		if bucket != nil {
			if entry, ok := (*bucket)[name]; ok && entry != nil {
				desc = strings.TrimSpace(entry.Description())
			}
		}
		if desc != "" {
			descBlock := strings.ReplaceAll(neutralizeTags(desc), "\n", "\n  ")
			lines = append(lines, "- "+safeName+"："+descBlock)
		} else {
			lines = append(lines, "- "+safeName)
		}
	}
	return strings.Join(lines, "\n")
}

// assetAppearanceNote 是 step2 资产块的取材注记（narration / drama 共用）。
const assetAppearanceNote = "资产外观以上述描述为准：视觉字段写到出场资产的服装 / 材质 / 陈设细节时从中取材，不自行发明。"

// formatAspectRatioDesc 渲染画面比例的描述性文案。
func formatAspectRatioDesc(aspectRatio string) string {
	switch aspectRatio {
	case "9:16":
		return "竖屏构图"
	case "16:9":
		return "横屏构图"
	}
	return aspectRatio + " 构图"
}

// formatOutlineLines 渲染分集大纲条目：故事节点 / 集尾钩子 / 下集预告语，缺失的行省略。
func formatOutlineLines(outline map[string]any) string {
	var lines []string
	if beats := listStrings(outline["story_beats"]); len(beats) > 0 {
		lines = append(lines, "故事节点：")
		for _, beat := range beats {
			lines = append(lines, "- "+beat)
		}
	}
	if hook, ok := outline["hook"].(string); ok && hook != "" {
		lines = append(lines, "集尾钩子："+hook)
	}
	if teaser, ok := outline["next_episode_teaser"].(string); ok && teaser != "" {
		lines = append(lines, "下集预告语："+teaser)
	}
	return strings.Join(lines, "\n")
}

// hookLandingGuide 是钩子落地要求：集尾钩子与下集预告是分集规划的核心设计，必须体现在成片末场，
// 而不是只停留在规划文档里。仅在账本提供了钩子 / 预告时渲染。
const hookLandingGuide = "末场（最后一个或几个分镜）的画面与对白须实际呈现集尾钩子的戏剧内容，让悬念定格在画面上；" +
	"有下集预告语时，用结尾画面或对白自然引出，不要生硬插入「下集预告」字样的旁白。"

// formatEpisodeOutlineBlock 渲染本集大纲 + 下集大纲两个上下文块；无规划数据时返回空串。
func formatEpisodeOutlineBlock(episodeOutline, nextEpisodeOutline map[string]any) string {
	var parts []string
	if episodeOutline != nil {
		title, _ := episodeOutline["title"].(string)
		titleLine := ""
		if title != "" {
			titleLine = "本集标题：" + title + "\n"
		}
		parts = append(parts, "<episode_outline>\n本集大纲（分集规划设计，剧本内容应覆盖全部故事节点）：\n"+
			titleLine+formatOutlineLines(episodeOutline)+"\n</episode_outline>")
		if hook, _ := episodeOutline["hook"].(string); hook != "" {
			parts = append(parts, hookLandingGuide)
		} else if teaser, _ := episodeOutline["next_episode_teaser"].(string); teaser != "" {
			parts = append(parts, hookLandingGuide)
		}
	}
	if nextEpisodeOutline != nil {
		title, _ := nextEpisodeOutline["title"].(string)
		titleLine := ""
		if title != "" {
			titleLine = "下集标题：" + title + "\n"
		}
		parts = append(parts, "<next_episode_outline>\n下集大纲（仅用于设计本集结尾的衔接，不要把下集情节提前写进本集）：\n"+
			titleLine+formatOutlineLines(nextEpisodeOutline)+"\n</next_episode_outline>")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n") + "\n\n"
}

// --- 字段写作指导（drama / narration 共用） ---

// sceneWritingGuide 是 image_prompt.scene 写作指导：单帧契约 + 分层要素 + 正反例。
const sceneWritingGuide = "这段文字将直接生成一张静态图：只描述此刻单帧画面上可见的内容，按主体（姿态 / 表情 / 服装）、" +
	"环境（陈设 / 物件）、光线、氛围（雾、雨等可见信号）分层给出具体细节，写成连贯的叙述句而非关键词堆叠。" +
	"光线与氛围在叙述中一笔带过即可，细节分别由 composition.lighting / composition.ambiance 承载。" +
	"原文若是回忆 / 闪回 / 心理活动，将其改编为此刻可见的载体——人物神态、手中物件、环境痕迹，画面里只保留一个时空。" +
	"画面元素（材质、装束、道具质感、环境年代特征）须贴合上方 `<style>` 块定义的风格基调，避免与风格相冲的元素混入" +
	"（例如赛博朋克风下不出现榻榻米，国风水墨下不出现霓虹屏）。\n" +
	"   正例：「林清坐在窗边木桌前，左手撑着下巴，目光落在桌上一封拆开的信纸上；桌面摊着信封与一只褪色的怀表。" +
	"半边脸笼在右侧落地窗逆光的蓝灰色阴影里，雨丝拍在木格窗棂，玻璃凝着细小水珠。」——主体、环境、光线、氛围各有一笔，全程叙述句。\n" +
	"   正例（原文是回忆）：原文「她想起亡母临终的嘱托」→「沈茹跪坐在灵位前，指尖抵着一串磨得发亮的佛珠，眼睑低垂。" +
	"案上白烛烧过半，烛泪层层堆叠，烛焰在她侧脸投下摇晃的暖橙光斑；一缕香灰无声落在青砖上。」——回忆的分量由此刻的神态与旧物承载，画面只有一个时空。\n" +
	"   反例（跑偏）：「林清陷入了多年前那个绝望的雨夜，画面基调：忧郁。光影设定：冷调。」——「多年前的雨夜」不在此刻画面上；「忧郁 / 冷调」是抽象标签，不是可见细节。\n" +
	"   反例（过短）：「林清坐在窗边发呆。」——缺少环境元素、光线方向、氛围细节，至少应覆盖主体 / 环境 / 光线 / 氛围中三层。"

// actionWritingGuide 是 video_prompt.action 写作指导：i2v 契约（首帧已定、只写运动）+ 分层要素 + 正反例。
const actionWritingGuide = "首帧画面已定格主体、场景与风格，这段文字驱动它动起来：只描述该时长内发生的运动与变化，不复述画面中的静态内容；" +
	"镜头运动专由 camera_motion 字段承载，action 只写主体与环境的运动。按主体动作（肢体 / 手势 / 表情过渡）、" +
	"物件互动（摩挲信纸、推门带起的气流等）、环境动态（衣摆、尘埃、雨势、光影移动）分层写成连贯叙述句；" +
	"动词应描述物理可观察动作（伸手 / 转身 / 摩挲 / 投向 / 收紧），避免内心动词。优先低缓、连贯的细微动作，" +
	"动作量与该镜头时长匹配：5 秒级镜头通常完成一个连贯动作 + 一个细节互动；8 秒级可承载一次动作过渡（如「抬头—对视—开口」）；" +
	"更长的镜头保持单一低缓的动作主线，随时长递增动作段数（如「起身—走到窗前—驻足」），而非叠加多条并行动作。\n" +
	"   正例：「林清缓缓抬起头，眼角微微收紧，手指无意识地摩挲信纸边缘；窗外雨势渐大，桌面投下的雨痕影子在缓慢移动。」——主体动作、物件互动、环境动态各有一笔。\n" +
	"   反例：「林清像蝴蝶般飞舞，思绪在过去与现在之间快速切换。」——「思绪切换」不是可拍摄的运动；「像蝴蝶般」是修辞，不是动作描述。\n" +
	"   避开任务类型触发词：不要用「增加 / 删除 / 去掉 / 修改 / 替换 / 改成 / 延长 / 续写」这类祈使动词。" +
	"部分模型按 prompt 措辞判定任务类型，带这些词会把参考生视频误判成视频编辑或视频延长，而误判在异步生成阶段才报错——任务已排队、已计费。" +
	"直接描述目标画面本身即可：不写「把外套改成红色」，写「她穿着红色外套」。"

const lightingWritingGuide = "描述具体的光源、方向、色温（如「左侧窗户透入的暖黄色晨光（约 3500K）」「头顶单点冷白色的吊灯」）。" +
	"可附加摄影质感术语（如「浅景深」「逆光剪影」「丁达尔光柱」「轮廓光勾边」「35mm 胶片颗粒感」），" +
	"让画面具备可观察的镜头语言而非抽象修辞；避免「光影神秘」「氛围唯美」这类抽象词。"

const ambianceWritingGuide = "描述可观察的环境效果（如「薄雾弥漫」「尘埃在光柱里翻飞」），避免抽象情绪词。"

const ambianceAudioWritingGuide = "只描写画内音（diegetic sound）：环境声、脚步、物体声响。不要写 BGM、配乐、画外音、旁白。"

// --- 两段式分层文案（step1 = 内容、step2 = 视觉） ---

const normalizeTaskNovel = "你的任务是将小说原文**改编**为结构化的分镜场景内容（含视觉改编描述、逐字口播 utterances " +
	"与原文锚 source_text），用于后续 AI 视频生成。"

const normalizeTaskScreenplay = "你的任务是从作者已写好的剧本中**提取**结构化的分镜场景内容：" +
	"逐字保留台词与画外音（落在 utterances）、摘录原文锚 source_text、把视觉层转写为场景描述，" +
	"用于后续 AI 视频生成。这是成品剧本、不是待加工的素材——只做提取、不做再创作。"

const normalizeSceneRuleNovel = "改编后的视觉化描述：角色动作、神态、环境、光影氛围，适合画面呈现。" +
	"以本场景当下的单一时空落笔——原文的回忆、闪回、心理活动，改编为此刻可见的载体" +
	"（人物神态、手中物件、环境痕迹）；这段描述是后续单帧分镜画面的内容来源。" +
	"**台词 / 画外音不要写进这里**——口播统一落在 utterances。"

const normalizeSceneRuleScreenplay = "把作者写下的运镜、景别、舞台提示、视觉场景转写为画面视觉描述。" +
	"**台词 / 画外音不要写进这里**——逐字落在 utterances；" +
	"排版符号（markdown、△、各类标签、表格、emoji）一律剥离，只留干净文本。"

const normalizeUtterancesNovel = "按口播出现顺序产出发声序列，台词（dialogue）的 speaker 必须出现在 characters_in_scene。" +
	"叙述、心理独白等不靠画面演出的内容，可按剧情语境判断写为画外音（voiceover）——" +
	"是否产出由你依语境创作判断，自然需要则产出。场景无口播则留空。"

const normalizeUtterancesScreenplay = "把作者写下的台词与画外音**逐字照搬**为有序发声序列，按它们在场景中的先后排列：" +
	"台词（dialogue）的 speaker 填原文说话人——命名角色应来自 characters_in_scene，" +
	"路人群演如「老人甲」「村民若干」照填原文称呼即可、可不在 characters_in_scene；" +
	"画外音 / 旁白写为 voiceover。不改写、不润色、不删减、不补写。场景无口播则留空。"

const normalizeSourceTextGuide = "逐字摘录本场景对应的原文片段，尽量与原文一致、宁缺毋造（无把握可留空）。"

const normalizeBreakRuleScreenplay = "\n- **segment_break**：沿用剧本自带的场次/场景切换——场次变更（地点 / 时间 / 场景切换）标「是」，" +
	"同一场次内标「否」；不要重新切碎作者的场次"

const dramaVisualRole = "你是一位资深的短剧分镜摄影 / 动作设计师。下方分镜内容（场景边界、出场资产、逐字口播、" +
	"原文锚、视觉改编描述）均已定稿，你的唯一职责是为每个分镜补全视觉生产层：" +
	"image_prompt（画面）与 video_prompt（动作 / 运镜 / 环境音）。" +
	"**不要改写或重述口播、不要新增 / 删除 / 重排分镜、不要改动场景内容**——只按 scene_id 逐条产出视觉字段。"

const dramaVisualGoal = "输出可直接驱动 AI 图像 / 视频生成的、视觉一致、节奏紧凑的视觉层。忠于已定稿的分镜内容与戏剧张力。"

// neutralizeTags 中和动态文本里的尖括号：novel_text / 资产名出现 </segments> 等标签序列时，
// 避免打散标签化 prompt 的块结构。属 prompt 鲁棒性——step2 输出仍由 response_schema 强制。
func neutralizeTags(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "<", "＜"), ">", "＞")
}

// formatNarrationStep1Segments 把 step1 结构化片段渲染为 step2 的只读上下文。
func formatNarrationStep1Segments(step1Segments []map[string]any) string {
	if len(step1Segments) == 0 {
		return "（无片段）"
	}
	lines := make([]string, 0, len(step1Segments))
	for _, seg := range step1Segments {
		sid := neutralizeTags(service.StringValue(seg["segment_id"], "?"))
		dur := fmt.Sprint(seg["duration_seconds"])
		brk := ""
		if truthy(seg["segment_break"]) {
			brk = "，场景切换"
		}
		chars := neutralizeTags(joinOr(listStrings(seg["characters_in_segment"]), "无"))
		sceneNames := neutralizeTags(joinOr(listStrings(seg["scenes"]), "无"))
		propNames := neutralizeTags(joinOr(listStrings(seg["props"]), "无"))
		novelBlock := strings.ReplaceAll(neutralizeTags(service.StringValue(seg["novel_text"], "")), "\n", "\n  ")
		lines = append(lines, fmt.Sprintf("- %s（时长 %ss%s）｜出场角色：%s｜场景：%s｜道具：%s\n  原文：%s",
			sid, dur, brk, chars, sceneNames, propNames, novelBlock))
	}
	return strings.Join(lines, "\n")
}

// NarrationPromptOptions 是 BuildNarrationPrompt 的入参。
type NarrationPromptOptions struct {
	ProjectOverview  *project.Overview
	Style            string
	StyleDescription string
	Characters       *project.AssetBucket
	Scenes           *project.AssetBucket
	Props            *project.AssetBucket
	Step1Segments    []map[string]any
	Episode          int
	AspectRatio      string
	TargetLanguage   string
}

// BuildNarrationPrompt 构建说书模式 step2（视觉层）prompt。
func BuildNarrationPrompt(opts NarrationPromptOptions) (string, error) {
	aspectRatio := opts.AspectRatio
	if aspectRatio == "" {
		aspectRatio = "9:16"
	}
	targetLanguage := opts.TargetLanguage
	if targetLanguage == "" {
		targetLanguage = "中文"
	}
	pacing, err := RenderPacingSection("narration")
	if err != nil {
		return "", err
	}
	pacingBlock := pacing + "\n\n"
	segmentsBlock := formatNarrationStep1Segments(opts.Step1Segments)

	return "# 角色与任务\n\n" +
		"你是一位资深的短视频分镜编剧，擅长把已定稿的小说片段转化为可直接驱动 AI 图像 / 视频生成的视觉分镜。\n" +
		"你的任务：基于下方已定稿的「片段表」，为**每个片段**产出视觉层（image_prompt 与 video_prompt），按 segment_id 一一对齐。\n\n" +
		"**只产视觉层**：novel_text、时长、segment_break、出场角色 / 场景 / 道具均已在 step1 定稿、按 segment_id 透传，**不要重复输出、不要改写**；你只产出 image_prompt 与 video_prompt。\n" +
		"**输出语言**：所有字符串值必须使用 " + targetLanguage + "；JSON 键名 / 枚举值保持英文。\n" +
		"**结构约束**：字段 / 枚举 / 必填项由 response_schema 强制；本提示只解释**如何写好每个字段的内容**。\n\n" +
		pacingBlock + "# 上下文\n\n" +
		"<overview>\n" + overviewValue(opts.ProjectOverview, "synopsis") + "\n\n" +
		"题材：" + overviewValue(opts.ProjectOverview, "genre") + "\n" +
		"主题：" + overviewValue(opts.ProjectOverview, "theme") + "\n" +
		"世界观：" + overviewValue(opts.ProjectOverview, "world_setting") + "\n" +
		"</overview>\n\n" +
		"<style>\n风格：" + opts.Style + "\n描述：" + opts.StyleDescription + "\n" +
		"画面比例：" + aspectRatio + "（" + formatAspectRatioDesc(aspectRatio) + "）\n" +
		"</style>\n\n" +
		"<characters>\n" + formatAssetsWithDesc(opts.Characters) + "\n</characters>\n\n" +
		"<scenes>\n" + formatAssetsWithDesc(opts.Scenes) + "\n</scenes>\n\n" +
		"<props>\n" + formatAssetsWithDesc(opts.Props) + "\n</props>\n\n" +
		assetAppearanceNote + "\n\n" +
		"<segments>\n" + segmentsBlock + "\n</segments>\n\n" +
		"segments 表每个片段已定稿（segment_id、逐字原文、时长、出场角色 / 场景 / 道具、是否场景切换），为只读上下文。\n\n" +
		"<episode_constraints>\n当前正在生成第 " + strconv.Itoa(opts.Episode) + " 集。为每个片段输出一条视觉层，其 segment_id 必须与 segments 表逐字一致——逐一对应，不增、不减、不改写。\n" +
		"</episode_constraints>\n\n" +
		"# 字段写作指引\n\n为每个片段产出下列视觉字段。\n\n" +
		"## 图片提示词（image_prompt）——切换到「摄影师」视角\n\n" +
		"- **image_prompt.scene**：" + sceneWritingGuide + "\n" +
		"- **image_prompt.composition.shot_type**：从枚举中按画面内容选择，不强加倾向。\n" +
		"- **image_prompt.composition.lighting**：" + lightingWritingGuide + "\n" +
		"- **image_prompt.composition.ambiance**：" + ambianceWritingGuide + "\n\n" +
		"## 视频提示词（video_prompt）——切换到「动作设计师」视角\n\n" +
		"- **video_prompt.action**：" + actionWritingGuide + "\n" +
		"- **video_prompt.camera_motion**：按画面内容自行选择。\n" +
		"- **video_prompt.ambiance_audio**：" + ambianceAudioWritingGuide + "\n" +
		"- **video_prompt.dialogue**：speaker 必须出现在该片段的出场角色中。\n\n" +
		"# 创作目标\n\n输出可直接驱动 AI 生成的、视觉一致、节奏紧凑的分镜视觉层。忠于原文叙事、保留情绪张力。\n", nil
}

// RenderDramaContentForStep2 把 step1 已定稿的场景内容渲染为 step2 视觉生成的输入块（每分镜一段）。
//
// 口播 / 原文锚仅供 LLM 理解戏剧节奏；它们由后端按 scene_id 透传，step2 只产出 image_prompt /
// video_prompt。所有动态文本过 neutralizeTags。本函数 fail-soft：结构性 fail-loud 在上游。
func RenderDramaContentForStep2(contentScenes []any) string {
	if len(contentScenes) == 0 {
		return "（无分镜内容）"
	}
	var blocks []string
	for _, raw := range contentScenes {
		scene, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		sid := neutralizeTags(service.StringValue(scene["scene_id"], "?"))
		header := "### " + sid
		if duration, ok := service.PositiveIntValue(scene["duration_seconds"]); ok {
			header += fmt.Sprintf("（时长 %d 秒）", duration)
		}
		lines := []string{header}
		chars := filterStrings(scene["characters_in_scene"])
		scenesRef := filterStrings(scene["scenes"])
		propsRef := filterStrings(scene["props"])
		lines = append(lines, fmt.Sprintf("出场资产：角色 [%s]、场景 [%s]、道具 [%s]",
			neutralizeTags(joinOr(chars, "无")), neutralizeTags(joinOr(scenesRef, "无")), neutralizeTags(joinOr(propsRef, "无"))))
		sceneDesc := strings.ReplaceAll(neutralizeTags(defaultString(scene["scene_description"], "（无）")), "\n", "\n  ")
		lines = append(lines, "视觉改编："+sceneDesc)
		utterances, _ := scene["utterances"].([]any)
		if len(utterances) > 0 {
			lines = append(lines, "口播：")
			for _, uRaw := range utterances {
				u, ok := uRaw.(map[string]any)
				if !ok {
					continue
				}
				text := strings.ReplaceAll(neutralizeTags(service.StringValue(u["text"], "")), "\n", "\n    ")
				if service.StringValue(u["kind"], "") == "dialogue" {
					speaker := neutralizeTags(service.StringValue(u["speaker"], ""))
					lines = append(lines, "  - [台词] "+speaker+"："+text)
				} else {
					lines = append(lines, "  - [画外音] "+text)
				}
			}
		}
		if sourceText := service.StringValue(scene["source_text"], ""); sourceText != "" {
			lines = append(lines, "原文锚："+strings.ReplaceAll(neutralizeTags(sourceText), "\n", "\n  "))
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	return strings.Join(blocks, "\n\n")
}

// DramaPromptOptions 是 BuildDramaPrompt 的入参。
type DramaPromptOptions struct {
	ProjectOverview  *project.Overview
	Style            string
	StyleDescription string
	ScenesContent    string
	Episode          int
	AspectRatio      string
	TargetLanguage   string
	// Characters / Scenes / Props 有任一非 nil 时渲染资产块（三者都为 nil 时不渲染）。
	Characters *project.AssetBucket
	Scenes     *project.AssetBucket
	Props      *project.AssetBucket
}

// BuildDramaPrompt 构建剧集动画模式 step2（视觉层）prompt。
func BuildDramaPrompt(opts DramaPromptOptions) (string, error) {
	aspectRatio := opts.AspectRatio
	if aspectRatio == "" {
		aspectRatio = "16:9"
	}
	targetLanguage := opts.TargetLanguage
	if targetLanguage == "" {
		targetLanguage = "中文"
	}
	pacing, err := RenderPacingSection("drama")
	if err != nil {
		return "", err
	}
	pacingBlock := pacing + "\n\n"
	assetsBlock := ""
	if opts.Characters != nil || opts.Scenes != nil || opts.Props != nil {
		assetsBlock = "<characters>\n" + formatAssetsWithDesc(opts.Characters) + "\n</characters>\n\n" +
			"<scenes>\n" + formatAssetsWithDesc(opts.Scenes) + "\n</scenes>\n\n" +
			"<props>\n" + formatAssetsWithDesc(opts.Props) + "\n</props>\n\n" +
			assetAppearanceNote + "\n\n"
	}

	return "# 角色与任务\n\n" + dramaVisualRole + "\n" +
		"你的任务：基于下方已定稿的「分镜内容」，为每个 scene_id 逐条产出视觉层 JSON（image_prompt / video_prompt）。\n\n" +
		"**输出语言**：所有字符串值必须使用 " + targetLanguage + "；JSON 键名 / 枚举值保持英文。\n" +
		"**结构约束**：字段 / 枚举 / 必填项由 response_schema 强制；本提示只解释**如何写好每个字段的内容**。\n" +
		"**对齐约束**：每个分镜产出一条视觉层，`scene_id` 必须与下方内容逐字一致、不增不减不改；不要输出口播 / 时长 / 资产等非视觉字段。\n\n" +
		pacingBlock + "# 上下文\n\n" +
		"<overview>\n" + overviewValue(opts.ProjectOverview, "synopsis") + "\n\n" +
		"题材：" + overviewValue(opts.ProjectOverview, "genre") + "\n" +
		"主题：" + overviewValue(opts.ProjectOverview, "theme") + "\n" +
		"世界观：" + overviewValue(opts.ProjectOverview, "world_setting") + "\n" +
		"</overview>\n\n" +
		"<style>\n风格：" + opts.Style + "\n描述：" + opts.StyleDescription + "\n" +
		"画面比例：" + aspectRatio + "（" + formatAspectRatioDesc(aspectRatio) + "）\n" +
		"</style>\n\n" +
		assetsBlock + "分镜内容中的「口播」与「原文锚」仅供理解戏剧节奏与语境，不要复制进视觉字段。\n\n" +
		"<shots>\n" + opts.ScenesContent + "\n</shots>\n\n" +
		"<episode_constraints>\n当前正在生成第 " + strconv.Itoa(opts.Episode) + " 集。每条视觉层的 scene_id 必须逐字等于上方分镜内容里的 scene_id；若该 ID 含拆分/编辑后缀（如 `_1`），也必须原样保留，不得改写、合并或新增。\n" +
		"</episode_constraints>\n\n" +
		"# 字段写作指引\n\n对每个分镜，按下列章节填写视觉字段。\n\n" +
		"## 图片提示词（image_prompt）——切换到「摄影师」视角\n\n" +
		"- **image_prompt.scene**：" + sceneWritingGuide + "\n" +
		"- **image_prompt.composition.shot_type**：从枚举中按画面内容选择，不强加倾向。\n" +
		"- **image_prompt.composition.lighting**：" + lightingWritingGuide + "\n" +
		"- **image_prompt.composition.ambiance**：" + ambianceWritingGuide + "\n\n" +
		"## 视频提示词（video_prompt）——切换到「动作设计师」视角\n\n" +
		"- **video_prompt.action**：" + actionWritingGuide + "\n" +
		"- **video_prompt.camera_motion**：按画面内容自行选择。\n" +
		"- **video_prompt.ambiance_audio**：" + ambianceAudioWritingGuide + "\n\n" +
		"# 创作目标\n\n" + dramaVisualGoal + "\n", nil
}

// NormalizePromptOptions 是 BuildNormalizePrompt 的入参。
type NormalizePromptOptions struct {
	NovelText          string
	ProjectOverview    *project.Overview
	Style              string
	Characters         *project.AssetBucket
	Scenes             *project.AssetBucket
	Props              *project.AssetBucket
	DefaultDuration    *int
	SupportedDurations []int
	Episode            int
	SourceKind         string
	TargetLanguage     string
	SourceLanguage     string
	SpeechRateOverride *float64
	EpisodeOutline     map[string]any
	NextEpisodeOutline map[string]any
}

// BuildNormalizePrompt 构建 step-1 规范化 prompt：源文 → 结构化分镜内容。
func BuildNormalizePrompt(opts NormalizePromptOptions) (string, error) {
	sourceKind := opts.SourceKind
	if sourceKind == "" {
		sourceKind = "novel"
	}
	targetLanguage := opts.TargetLanguage
	if targetLanguage == "" {
		targetLanguage = "中文"
	}
	isScreenplay := sourceKind == "screenplay"

	charList := formatNames(opts.Characters)
	sceneList := formatNames(opts.Scenes)
	propList := formatNames(opts.Props)
	characterNames := assetNames(opts.Characters)
	sceneNames := assetNames(opts.Scenes)
	propNames := assetNames(opts.Props)

	taskLine := normalizeTaskNovel
	sourceHeading := "小说原文"
	sourceTag := "novel"
	sceneRule := normalizeSceneRuleNovel
	utterancesRule := normalizeUtterancesNovel
	breakRule := ""
	if isScreenplay {
		taskLine = normalizeTaskScreenplay
		sourceHeading = "剧本原文"
		sourceTag = "screenplay"
		sceneRule = normalizeSceneRuleScreenplay
		utterancesRule = normalizeUtterancesScreenplay
		breakRule = normalizeBreakRuleScreenplay
	}
	outlineBlock := formatEpisodeOutlineBlock(opts.EpisodeOutline, opts.NextEpisodeOutline)

	languageRule := "自然语言字符串值必须使用 " + targetLanguage + "；JSON 键名 / 枚举值保持英文。" +
		"例外（逐字保留、不翻译）：资产引用字段（`characters_in_scene[]` / `scenes[]` / `props[]`，" +
		"须逐字等于 project.json 登记名）、说话人引用 `utterances[].speaker`" +
		"（须等于 characters_in_scene 中登记的角色名）与逐字原文锚 `source_text`。"
	if isScreenplay {
		languageRule = "自然语言字符串值必须使用 " + targetLanguage + "；JSON 键名 / 枚举值保持英文。" +
			"例外（逐字保留原文、不翻译、不改写）：资产引用字段（`characters_in_scene[]` / `scenes[]` / `props[]`，" +
			"须逐字等于 project.json 登记名）与逐字字段（`utterances[].text` / `utterances[].speaker` / `source_text`）；" +
			"speaker 沿用 characters_in_scene 中登记的角色名原文，群演沿用原文称呼。"
	}

	normalizedDurations := service.NormalizeDurations(opts.SupportedDurations)
	if len(normalizedDurations) == 0 {
		return "", fmt.Errorf("supported_durations 不能为空：必须提供模型支持的秒数集合")
	}
	if opts.DefaultDuration != nil && !service.ContainsInt(normalizedDurations, *opts.DefaultDuration) {
		return "", fmt.Errorf("default_duration=%d 不在 supported_durations=%v 内", *opts.DefaultDuration, normalizedDurations)
	}

	durationsStr := joinInts(normalizedDurations, ", ")
	maxDur := normalizedDurations[len(normalizedDurations)-1]
	var baseDurationRule string
	if opts.DefaultDuration != nil {
		baseDurationRule = fmt.Sprintf("从支持的秒数档位（%s）中按画面内容选择：默认 %d 秒，"+
			"打斗 / 大场面 / 情绪铺陈等画面可取更长档至 %d 秒，不要默认选最短档",
			durationsStr, *opts.DefaultDuration, maxDur)
	} else {
		baseDurationRule = fmt.Sprintf("从支持的秒数档位（%s）中按画面内容复杂度匹配合适时长（最长 %d 秒），不强制默认值", durationsStr, maxDur)
	}

	sourceLanguage := opts.SourceLanguage
	speechRate := service.SpeechRateUnitsPerSecond(sourceLanguage, opts.SpeechRateOverride)
	unitLabel := service.ReadingUnitNoun(sourceLanguage)
	durationLowerBoundRule := "再按台词口播长度设下界：先估算该场 utterances（台词 + 画外音）念完约需的秒数" +
		fmt.Sprintf("（口播语速约 %s %s/秒），在上述档位里取**不低于**这个秒数的最接近档位；", formatFloat(speechRate), unitLabel) +
		"这是单向下界——画面 / 情绪留白可在此之上取更长档位，但台词永不把时长压到念不完的短档，" +
		"utterances 为空（纯画面、无口播）的场景没有此下界、按画面自行取值；" +
		fmt.Sprintf("若口播估算已超过最长 %d 秒，取最长档即可（不删减台词、不强行压进短档），保存时会另有提示", maxDur)
	durationRule := baseDurationRule + "。" + durationLowerBoundRule

	pacing, err := RenderPacingSection("drama")
	if err != nil {
		return "", err
	}
	pacingBlock := pacing + "\n\n"

	return taskLine + "\n\n" +
		"**输出语言**：" + languageRule + "\n" +
		"**结构约束**：字段 / 枚举 / 必填项由 response_schema 强制；本提示只解释**如何写好每个字段的内容**。\n\n" +
		pacingBlock + "## 项目信息\n\n" +
		"<overview>\n" + overviewValue(opts.ProjectOverview, "synopsis") + "\n\n" +
		"题材类型：" + overviewValue(opts.ProjectOverview, "genre") + "\n" +
		"核心主题：" + overviewValue(opts.ProjectOverview, "theme") + "\n" +
		"世界观设定：" + overviewValue(opts.ProjectOverview, "world_setting") + "\n" +
		"</overview>\n\n" +
		"<style>\n" + opts.Style + "\n</style>\n\n" +
		"<characters>\n" + charList + "\n</characters>\n\n" +
		"<scenes>\n" + sceneList + "\n</scenes>\n\n" +
		"<props>\n" + propList + "\n</props>\n\n" +
		"## " + sourceHeading + "\n\n" +
		"<" + sourceTag + ">\n" + opts.NovelText + "\n</" + sourceTag + ">\n\n" +
		outlineBlock + "# 字段写作指引\n\n" +
		"把源文拆为有序分镜，逐条产出结构化场景内容。当前正在生成第 " + strconv.Itoa(opts.Episode) + " 集。\n\n" +
		"## 基础字段\n\n" +
		"- **scene_id**：`E" + strconv.Itoa(opts.Episode) + "S{两位序号}` 格式（如 E" + strconv.Itoa(opts.Episode) + "S01），按分镜顺序递增，不得用其他集号前缀。\n" +
		"- **duration_seconds**：" + durationRule + "。" + breakRule + "\n" +
		"- **characters_in_scene** / **scenes** / **props**：从下列候选中列出此分镜实际出现的资产。\n" +
		"  - 候选 characters：[" + joinOr(characterNames, "（暂无）") + "]\n" +
		"  - 候选 scenes：[" + joinOr(sceneNames, "（暂无）") + "]\n" +
		"  - 候选 props：[" + joinOr(propNames, "（暂无）") + "]\n" +
		"  - 不要发明候选之外的名称；泛指群演（如「老人甲」「村民若干」）不登记为角色资产、不进 characters_in_scene。\n" +
		"- **scene_description**：" + sceneRule + "\n\n" +
		"## 逐字内容（内容真相源，定稿后原样保留、不再改写）\n\n" +
		"- **source_text**：" + normalizeSourceTextGuide + "\n" +
		"- **utterances**：" + utterancesRule + "\n\n" +
		"每个分镜应为一个独立的视觉画面、可在指定时长内完成；避免在一个分镜内安排多个动作或画面切换。\n", nil
}

// NarrationSplitPromptOptions 是 BuildNarrationSplitPrompt 的入参。
type NarrationSplitPromptOptions struct {
	NovelText          string
	ProjectOverview    *project.Overview
	Characters         *project.AssetBucket
	Scenes             *project.AssetBucket
	Props              *project.AssetBucket
	DefaultDuration    *int
	SupportedDurations []int
	Episode            int
	TargetLanguage     string
}

// BuildNarrationSplitPrompt 构建 step-1 说书片段拆分 prompt：源文 → 结构化片段表。
//
// 与 BuildNormalizePrompt 不同，此处对漂移到 supported_durations 之外的 default 按 nil 处理
// （软偏好、可被内容需要覆盖），不 fail loud。
func BuildNarrationSplitPrompt(opts NarrationSplitPromptOptions) (string, error) {
	targetLanguage := opts.TargetLanguage
	if targetLanguage == "" {
		targetLanguage = "中文"
	}
	normalizedDurations := service.NormalizeDurations(opts.SupportedDurations)
	if len(normalizedDurations) == 0 {
		return "", fmt.Errorf("supported_durations 不能为空：必须提供模型支持的秒数集合")
	}
	defaultDuration := opts.DefaultDuration
	if defaultDuration != nil && !service.ContainsInt(normalizedDurations, *defaultDuration) {
		defaultDuration = nil
	}
	durationsStr := joinInts(normalizedDurations, ", ")
	maxDur := normalizedDurations[len(normalizedDurations)-1]
	var durationRule string
	if defaultDuration != nil {
		durationRule = fmt.Sprintf("单片段默认取 %d 秒（按朗读语速估算该秒数内能念完的字数）；长句 / 情绪铺陈 / "+
			"关键对话等可从档位中取更长值（至 %d 秒）——偏好可被内容需要覆盖，硬约束不可", *defaultDuration, maxDur)
	} else {
		durationRule = fmt.Sprintf("按朗读节奏从档位（%s）中取值（最长 %d 秒），不强制默认值", durationsStr, maxDur)
	}
	pacing, err := RenderPacingSection("narration")
	if err != nil {
		return "", err
	}
	pacingBlock := pacing + "\n\n"

	characterNames := assetNames(opts.Characters)
	sceneNames := assetNames(opts.Scenes)
	propNames := assetNames(opts.Props)
	episode := strconv.Itoa(opts.Episode)

	return "# 角色与任务\n\n" +
		"你是一位专业的说书内容架构师，本任务是把源文按朗读节奏拆分为适合短视频配音的片段表（step1 内容拆分）。\n" +
		"说书剧本走两段式：本阶段只定内容层——逐字 `novel_text`、片段边界、时长、场景切换标记与出场资产；\n" +
		"视觉层（image_prompt / video_prompt）由后续 step2 按 `segment_id` 对齐生成，`novel_text` 由本阶段定稿后透传、不再重出。\n\n" +
		"**输出语言**：自然语言字符串值使用 " + targetLanguage + "；JSON 键名保持英文。\n" +
		"例外（逐字保留、不翻译、不改写）：`novel_text` 逐字等于源文原句（含标点）；资产引用字段\n" +
		"（`characters_in_segment` / `scenes` / `props`）逐字等于下方候选表中的登记名。\n" +
		"**结构约束**：字段 / 枚举 / 必填项由 response_schema 强制；本提示只解释**如何写好每个字段的内容**。\n\n" +
		pacingBlock + "# 上下文\n\n" +
		"<overview>\n" + overviewValue(opts.ProjectOverview, "synopsis") + "\n\n" +
		"题材：" + overviewValue(opts.ProjectOverview, "genre") + "\n" +
		"主题：" + overviewValue(opts.ProjectOverview, "theme") + "\n" +
		"世界观：" + overviewValue(opts.ProjectOverview, "world_setting") + "\n" +
		"</overview>\n\n" +
		"<characters>\n" + formatNames(opts.Characters) + "\n</characters>\n\n" +
		"<scenes>\n" + formatNames(opts.Scenes) + "\n</scenes>\n\n" +
		"<props>\n" + formatNames(opts.Props) + "\n</props>\n\n" +
		"## 小说原文\n\n<novel>\n" + opts.NovelText + "\n</novel>\n\n" +
		"# 拆分规则\n\n当前正在生成第 " + episode + " 集。\n\n" +
		"- **novel_text**：逐字保留小说原文，不改编 / 不删减 / 不添加 / 不改标点（后期配音与透传的真相源）；\n" +
		"  对话片段含完整说话内容与引导语（如「他说道」）。在句号 / 问号 / 感叹号 / 省略号等标点或段落结束处拆分，\n" +
		"  保持语义完整，不拆断完整的语义单元。\n" +
		"- **segment_id**：`E" + episode + "S{两位序号}` 格式（如 E" + episode + "S01），按顺序递增，不得用其他集号前缀。\n" +
		"- **duration_seconds**：" + durationRule + "。取值必须落在支持档位（" + durationsStr + "）内。\n" +
		"- **segment_break**：在真正的场景切换点（时间跳跃 / 空间转换 / 情节转折）标 `true`，同一连续场景内标 `false`，不要滥用。\n" +
		"- **characters_in_segment / scenes / props**：列出该片段 `novel_text` 中实际出现（被叙述或对话提及）的已登记资产，\n" +
		"  名称逐字取自下列候选，不要发明候选之外的名称；泛指群演（老人甲 / 村民若干）不登记、不进 characters_in_segment。\n" +
		"  三个数组均必填，无对应资产时显式写空数组 `[]`。\n" +
		"  - character: " + joinOr(characterNames, "（暂无）") + "\n" +
		"  - scene: " + joinOr(sceneNames, "（暂无）") + "\n" +
		"  - prop: " + joinOr(propNames, "（暂无）") + "\n\n" +
		"请覆盖全部源文，按叙事顺序逐片段产出。\n", nil
}

const overviewTaskNovel = "请分析以下小说内容，提取关键信息："

const overviewTaskScreenplay = "请分析以下成品剧本，提炼项目概述（题材 / 主题 / 故事梗概 / 世界观）。\n" +
	"剧本里可能附有作者写下的创作方案——以任意形态（开篇前言、大纲、设定卡等，标题与排版各异）" +
	"写明题材、主题、一句话故事、世界观等核心设定。若能识别出这类创作方案，" +
	"请优先照用作者已写下的设定填充对应字段（忠于原意，可精炼归并、不另起炉灶重新推断）；" +
	"剧本未附创作方案时，再从剧本正文自行归纳。"

// BuildOverviewPrompt 构建项目概述（overview）生成 prompt。
//
// source_kind="screenplay" 时翻为「提取优先」；默认从正文归纳。
func BuildOverviewPrompt(sourceContent, sourceKind, targetLanguage string) string {
	task := overviewTaskNovel
	if sourceKind == "screenplay" {
		task = overviewTaskScreenplay
	}
	if targetLanguage == "" {
		targetLanguage = "中文"
	}
	return task + "\n\n**输出语言**：所有字符串值必须使用 " + targetLanguage +
		"；JSON 键名 / 枚举值保持英文。\n\n" + sourceContent
}

// --- 内部小工具 ---

func overviewValue(ov *project.Overview, field string) string {
	if ov == nil {
		return ""
	}
	switch field {
	case "synopsis":
		return ov.Synopsis
	case "genre":
		return ov.Genre
	case "theme":
		return ov.Theme
	case "world_setting":
		return ov.WorldSetting
	}
	return ""
}

func joinInts(list []int, sep string) string {
	parts := make([]string, 0, len(list))
	for _, item := range list {
		parts = append(parts, strconv.Itoa(item))
	}
	return strings.Join(parts, sep)
}

// formatFloat 按 %g 等价口径渲染：整数比值去掉小数点后的零。
func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func joinOr(items []string, fallback string) string {
	if len(items) == 0 {
		return fallback
	}
	return strings.Join(items, "、")
}

func defaultString(value any, fallback string) string {
	if s, ok := value.(string); ok && s != "" {
		return s
	}
	return fallback
}

func truthy(value any) bool {
	b, ok := value.(bool)
	return ok && b
}

// listStrings 取字符串数组（非数组 / 非字符串项一律忽略）。
func listStrings(value any) []string {
	raw, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// filterStrings 与 listStrings 同义（语义化别名，供 step2 渲染使用）。
func filterStrings(value any) []string { return listStrings(value) }
