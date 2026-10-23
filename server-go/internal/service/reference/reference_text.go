// 参考生视频「书写层」的文本解析与机械校验（对应 lib/reference_video/shot_parser.py 与
// lib/reference_video/draft_validation.py）。
//
// 书写层是 LLM 产出与人在编辑器书写的同一种格式：一段扁平文本，按行分为镜头行 / 台词行 /
// 画外音行。本模块把 prompt ↔ Shot[]/references 做双向转换，并对机器产物逐 unit 判定违约
// （正文语法误用、引用未登记、台词量念不完等），违约走隔离草稿的修复闭环。
//
// 所有偏移与切片按 rune（Python 是码点坐标系；Go 的 len(string) 是字节）。
package reference

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	domainproject "github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/service"
)

// MaxShotsPerUnit 是单 unit 允许的镜头行（`镜头N：`）上限（对应 MAX_SHOTS_PER_UNIT）。
// unit 是一次生成调用，镜头只是同一段 clip 内的画面切换。
const MaxShotsPerUnit = 4

// SpeechOverflowTolerance 是台词口播时长相对 unit 时长的宽容系数（对应 SPEECH_OVERFLOW_TOLERANCE）。
const SpeechOverflowTolerance = 0.20

// WritingSyntaxSpec 是书写层语法规范（对应 WRITING_SYNTAX_SPEC），由两级 prompt builder 共同注入。
const WritingSyntaxSpec = "每个 unit 的正文是一段**扁平文本**，按行书写，只有三种行：\n\n" +
	"1. **镜头行**——以 `镜头N：` 开头（N 从 1 起递增，中英冒号均可），其后是该镜头的画面描述。\n" +
	"   一个 unit 最多 " + "4" + " 个镜头行；镜头行不带秒数，时长是 unit 级字段。\n" +
	"2. **台词行**——`@[角色名]：{台词内容}`，独立成行、整行只有这个结构。\n" +
	"   花括号 `{}` 里是逐字台词；角色名必须是已登记的角色资产名。\n" +
	"3. **画外音行**——`{台词内容}`，独立成行、整行只被一对花括号包裹，表示画外音（不指定说话人）。\n\n" +
	"资产引用：角色 / 场景 / 道具一律写成 `@[名称]`，名称逐字取自候选表，不要发明表外的名称。\n\n" +
	"结构标记不随输出语言变化：`镜头N：` 前缀、`@[]` 与花括号 `{}` 是解析器认的语法记号，\n" +
	"任何语言的项目都逐字保持这几个记号（写成 `Shot 1:` / `Cảnh 1:` 会解析失败），只有记号之间的\n" +
	"画面描述与台词内容用项目语言书写。\n\n" +
	"硬性约束（违反即整份产出被拒）：\n\n" +
	"- 花括号只用于台词行与画外音行，且必须整行包裹、成对闭合、不嵌套；\n" +
	"  **镜头行的画面描述里不得出现 `{` 或 `}`**——台词写进画面描述不会被识别为台词。\n" +
	"- 台词单独成行，不与画面描述混写在同一行；台词行不承担画面描述职能。\n" +
	"- 每个 `镜头N：` 后都要有画面描述，不留空镜头行。\n" +
	"- 语法记号一律半角：`@[名称]` 的方括号成对闭合且名称非空，台词的花括号写 `{}` 不写 `｛｝`。\n" +
	"- 每个 `@[名称]` 都必须是候选表中已登记的资产名，台词行的角色名同样如此。\n" +
	"- 画面描述不写外貌 / 服装 / 场景陈设 / 色调光影——静态外观由参考图承担；\n" +
	"  泛指群演（老人甲 / 村民若干）直接写进描述即可，不用 `@` 引用。\n\n" +
	"示例（一个 unit 的正文）：\n\n" +
	"```\n" +
	"镜头1：@[李明] 推开 @[酒馆] 的木门，侧身跨过门槛，目光扫过厅内。\n" +
	"@[李明]：{这地方比我想的还热闹。}\n" +
	"镜头2：@[李明] 走向柜台，把 @[长剑] 横放在台面上，指节在剑鞘上叩了两下。\n" +
	"{他知道，今晚不会太平。}\n" +
	"```"

// BOM / ZWNBSP。
const shotSourceBOM = "\uFEFF"

// normalizeShotSource 是书写层文本的入口归一：去掉全部 U+FEFF，并把编码形式收敛到 Unicode NFC。
//
// 两者同一性质——屏幕上看不见的字节差异，却让按字节走的判定分叉。BOM 不止出现在文档开头，
// 粘贴拼接会把它带到任意行首；NFC 则是资产名比对的坐标系，说话人位与 `@[名称]` 引用都要与
// 资产表的 key 判等。
func normalizeShotSource(text string) string {
	stripped := text
	if strings.Contains(text, shotSourceBOM) {
		stripped = strings.ReplaceAll(text, shotSourceBOM, "")
	}
	return norm.NFC.String(stripped)
}

// isASCIIWordChar 复刻 Python 的 ASCII 词字符判定（字母 / 数字 / 下划线）。
func isASCIIWordChar(r rune) bool {
	return r == '_' || domainproject.IsASCIIWordChar(r)
}

// isLegacyMentionChar 判定旧式裸 `@名称` 的延续字符。
func isLegacyMentionChar(r rune) bool {
	if r == '_' || domainproject.IsASCIIWordChar(r) {
		return true
	}
	return r >= '一' && r <= '鿿'
}

// splitShotSourceLines 近似 Python str.splitlines：兼容 \r\n 与 \r，末尾换行不产生空行。
func splitShotSourceLines(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	var current strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\n' || r == '\r' || r == '\v' || r == '\f' || r == '\u0085' || r == '\u2028' || r == '\u2029' {
			out = append(out, current.String())
			current.Reset()
			if r == '\r' && i+1 < len(runes) && runes[i+1] == '\n' {
				i++
			}
			continue
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}

// mentionSpan 是文本里一个 mention 的区间（rune 下标）。
type mentionSpan struct {
	start, end int
	name       string
}

// iterMentions 定位文本里的 @名称 / @[名称] mention。
//
// @ 左侧是 ASCII 词字符时视为邮箱 / id 片段；包裹形态不跨行；花括号包裹刻意不认。
func iterMentions(runes []rune) []mentionSpan {
	var out []mentionSpan
	for i := 0; i < len(runes); {
		if runes[i] != '@' {
			i++
			continue
		}
		if i > 0 && isASCIIWordChar(runes[i-1]) {
			i++
			continue
		}
		if i+1 >= len(runes) {
			i++
			continue
		}
		if runes[i+1] == '[' {
			close := -1
			for j := i + 2; j < len(runes); j++ {
				if runes[j] == '\n' || runes[j] == '\r' {
					break
				}
				if runes[j] == ']' {
					close = j
					break
				}
			}
			if close < 0 || !(i+2 < close) {
				i++
				continue
			}
			out = append(out, mentionSpan{start: i, end: close + 1, name: string(runes[i+2 : close])})
			i = close + 1
			continue
		}
		j := i + 1
		for j < len(runes) && isLegacyMentionChar(runes[j]) {
			j++
		}
		if j > i+1 {
			out = append(out, mentionSpan{start: i, end: j, name: string(runes[i+1 : j])})
			i = j
			continue
		}
		i++
	}
	return out
}

// shotHeaderRest 匹配行首 `镜头N：` header 并返回其后的正文；无 header 时 ok=false。
func shotHeaderRest(line string) (string, bool) {
	s := strings.TrimSpace(line)
	runes := []rune(s)
	if len(runes) < 3 || runes[0] != '镜' || runes[1] != '头' {
		return "", false
	}
	i := 2
	for i < len(runes) && isShotSpaceRune(runes[i]) {
		i++
	}
	start := i
	for i < len(runes) && runes[i] >= '0' && runes[i] <= '9' {
		i++
	}
	if i == start {
		return "", false
	}
	for i < len(runes) && isShotSpaceRune(runes[i]) {
		i++
	}
	if i >= len(runes) || (runes[i] != ':' && runes[i] != '：') {
		return "", false
	}
	return strings.TrimLeftFunc(string(runes[i+1:]), isShotSpaceRune), true
}

// isShotSpaceRune 复刻 Python 的空白判定（含全角空格与常见 Unicode 空白）。
func isShotSpaceRune(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', '\u0085', '\u00a0':
		return true
	}
	return r == '\u3000' || (r >= 0x2000 && r <= 0x200a) || r == 0x2028 || r == 0x2029 || r == 0x202f || r == 0x205f
}

// stripShotHeader 去掉行首的 `镜头N：` header，返回 header 之后的正文；无 header 时返回归一后的整行。
func stripShotHeader(line string) string {
	normalized := normalizeShotSource(line)
	if rest, ok := shotHeaderRest(normalized); ok {
		return rest
	}
	return normalized
}

// unwrapBraces 判定 `{…}` 整体包裹：去空白后须以 `{` 开头、`}` 结尾且内部无花括号；空台词不算。
func unwrapBraces(text string) (string, bool) {
	body := []rune(strings.TrimSpace(text))
	if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return "", false
	}
	inner := string(body[1 : len(body)-1])
	if strings.ContainsAny(inner, "{}") || strings.TrimSpace(inner) == "" {
		return "", false
	}
	return inner, true
}

// matchDialogueLine 规范台词行 `@[角色]：{台词}`（中英冒号均可）→ (speaker, text)。
func matchDialogueLine(line string) (string, string, bool) {
	stripped := strings.TrimSpace(normalizeShotSource(line))
	if !strings.HasPrefix(stripped, "@") {
		return "", "", false
	}
	runes := []rune(stripped)
	first := iterMentions(runes)
	if len(first) == 0 || first[0].start != 0 {
		return "", "", false
	}
	rest := strings.TrimLeftFunc(string(runes[first[0].end:]), isShotSpaceRune)
	restRunes := []rune(rest)
	if len(rest) == 0 || (restRunes[0] != '：' && restRunes[0] != ':') {
		return "", "", false
	}
	spoken, ok := unwrapBraces(string(restRunes[1:]))
	speaker := first[0].name
	if !ok || strings.TrimSpace(speaker) == "" {
		return "", "", false
	}
	return speaker, spoken, true
}

// leadingMentionBeforeColon 行首为 `@[名称]：` 形态时返回该名称（只看名称与冒号，不判花括号）。
func leadingMentionBeforeColon(line string) (string, bool) {
	stripped := strings.TrimSpace(normalizeShotSource(line))
	if !strings.HasPrefix(stripped, "@") {
		return "", false
	}
	runes := []rune(stripped)
	first := iterMentions(runes)
	if len(first) == 0 || first[0].start != 0 {
		return "", false
	}
	rest := strings.TrimLeftFunc(string(runes[first[0].end:]), isShotSpaceRune)
	restRunes := []rune(rest)
	if len(rest) == 0 || (restRunes[0] != '：' && restRunes[0] != ':') {
		return "", false
	}
	return first[0].name, true
}

// findMalformedMention 返回行内首个写坏的 `@[` 引用片段（如 `@[李明` / `@[]`）；没有则 ok=false。
//
// 全角形（＠[李明] / @［李明］）一并算坏 token：中文输入法下模型很容易写出，而语法只认半角。
func findMalformedMention(line string) (string, bool) {
	text := normalizeShotSource(line)
	runes := []rune(text)
	for index := 0; index < len(runes); index++ {
		if runes[index] == '＠' || (runes[index] == '@' && index+1 < len(runes) && runes[index+1] == '［') {
			return sliceRunes(text, index, index+20), true
		}
	}
	valid := map[int]bool{}
	for _, span := range iterMentions(runes) {
		valid[span.start] = true
	}
	for index := 0; index+1 < len(runes); index++ {
		if runes[index] != '@' || runes[index+1] != '[' || valid[index] {
			continue
		}
		if index > 0 && isASCIIWordChar(runes[index-1]) {
			continue
		}
		return sliceRunes(text, index, index+20), true
	}
	return "", false
}

// sliceRunes 按 rune 下标截取（越界截断到末尾）。
func sliceRunes(text string, start, end int) string {
	runes := []rune(text)
	if start < 0 {
		start = 0
	}
	if end > len(runes) {
		end = len(runes)
	}
	if start > end {
		start = end
	}
	return string(runes[start:end])
}

// matchVoiceoverLine 裸 `{台词}` 行 = 画外音 → 台词正文；不匹配返回 ok=false。
func matchVoiceoverLine(line string) (string, bool) {
	return unwrapBraces(normalizeShotSource(line))
}

// ParsePromptShots 导出 parsePrompt 的公开包装：抛掉 mention 名，只返回 shots 列表。
//
// 供 reference_video_api 的新建 unit（shots 各取 {"text": ...}）与 PATCH 重解析正文复用，
// 不复制切分口径。
func ParsePromptShots(text string) ([]map[string]any, []string) {
	return parsePrompt(text)
}

// parsePrompt 把书写层 prompt 文本拆为 (shots, mention_names)。
//
// 有 `镜头N：` header 时按 header 切分；无 header 时整段视为单镜头。shots 以 {"text": ...} 呈现
// （与落盘形状同形）。时长不从文本解析：它是 unit 级字段。
func parsePrompt(text string) ([]map[string]any, []string) {
	text = normalizeShotSource(text)
	var segments []string
	started := false
	var currentBuf []string
	for _, line := range splitShotSourceLines(text) {
		rest, ok := shotHeaderRest(line)
		if ok {
			if started {
				segments = append(segments, strings.TrimSpace(strings.Join(currentBuf, "\n")))
				currentBuf = []string{rest}
			} else {
				preHeader := strings.TrimSpace(strings.Join(currentBuf, "\n"))
				if preHeader != "" {
					currentBuf = []string{preHeader, rest}
				} else {
					currentBuf = []string{rest}
				}
				started = true
			}
			continue
		}
		currentBuf = append(currentBuf, line)
	}
	if started {
		segments = append(segments, strings.TrimSpace(strings.Join(currentBuf, "\n")))
	}
	mentions := extractMentions(text)
	if len(segments) == 0 {
		return []map[string]any{{"text": strings.TrimSpace(text)}}, mentions
	}
	shots := make([]map[string]any, 0, len(segments))
	for _, segment := range segments {
		shots = append(shots, map[string]any{"text": segment})
	}
	return shots, mentions
}

// renderShotsText 是 parsePrompt 的逆向：把 shots 还原为带 `镜头N：` header 的书写层正文。
//
// 镜头正文可跨多行（台词行在描述行之下），故 header 只加在首行。
func renderShotsText(shots []map[string]any) string {
	blocks := make([]string, 0, len(shots))
	for index, shot := range shots {
		body, _ := shot["text"].(string)
		head, rest := body, ""
		if i := strings.Index(body, "\n"); i >= 0 {
			head, rest = body[:i], body[i+1:]
		}
		block := fmt.Sprintf("镜头%d：%s", index+1, head)
		if rest != "" {
			block += "\n" + rest
		}
		blocks = append(blocks, block)
	}
	return strings.Join(blocks, "\n")
}

// renderShotsTextAny 接受 []any 形态的 shots（磁盘 JSON 解出的形状）。
func renderShotsTextAny(shots []any) string {
	return renderShotsText(shotsAsMaps(shots))
}

// shotsAsMaps 过滤出 map 元素（非对象条目跳过）。
func shotsAsMaps(shots []any) []map[string]any {
	out := make([]map[string]any, 0, len(shots))
	for _, raw := range shots {
		if m, ok := raw.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// extractMentions 提取文本中的 @ 引用名（保持首次出现顺序、去重）。
//
// 规范台词行整行不计入：给画外说话的角色附参考图会诱导模型把他画进画面。
func extractMentions(text string) []string {
	seen := map[string]bool{}
	var result []string
	for _, rawLine := range splitShotSourceLines(text) {
		line := normalizeShotSource(rawLine)
		if _, _, ok := matchDialogueLine(stripShotHeader(line)); ok {
			continue
		}
		for _, span := range iterMentions([]rune(line)) {
			if !seen[span.name] {
				seen[span.name] = true
				result = append(result, span.name)
			}
		}
	}
	return result
}

// splitCharacterVariantMention 拆 `角色/衍生slug`：mention 语法里只有一层斜杠有意义。
func splitCharacterVariantMention(name string) (string, string) {
	i := strings.Index(name, "/")
	if i < 0 {
		return name, ""
	}
	characterName, slug := name[:i], name[i+1:]
	if characterName == "" || slug == "" || strings.Contains(slug, "/") {
		return name, ""
	}
	return characterName, slug
}

// findCharacterVariant 在角色条目的 variants 集合中按稳定 id 或 slug 查衍生形态。
//
// 键为 slug（与落盘形状一致），返回到该 slug 的 variant dict（含 id / slug / image_path；
// image_path 缺失或非字符串时为空串，与 Python normalize_variant 的缺省口径一致）。
func findCharacterVariant(character *domainproject.AssetEntry, characterID, identifier string) map[string]any {
	if character == nil {
		return nil
	}
	rawVariants, _ := character.Fields()["variants"].(map[string]any)
	if len(rawVariants) == 0 {
		return nil
	}
	target := domainproject.NormalizeAssetName(identifier)
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
		if id == identifier || domainproject.NormalizeAssetName(rawSlug) == target {
			imagePath, _ := raw["image_path"].(string)
			return map[string]any{"id": id, "slug": rawSlug, "image_path": imagePath}
		}
	}
	return nil
}

// resolveReferences 按项目三 bucket 把 mention 名字分派成 ReferenceResource（character → scene → prop）。
//
// 名字与三张资产表都先归一到比对坐标系（NFC），产出的 name 与 missing 一律是归一形式。
func resolveReferences(names []string, project *domainproject.ProjectFile) ([]map[string]any, []string) {
	buckets := map[string]domainproject.AssetBucket{}
	if project != nil {
		buckets["character"] = project.Characters
		buckets["scene"] = project.Scenes
		buckets["prop"] = project.Props
	}
	order := []string{"character", "scene", "prop"}
	var refs []map[string]any
	var missing []string
	for _, rawName := range names {
		displayName := domainproject.NormalizeAssetName(rawName)
		characterName, variantSlug := splitCharacterVariantMention(displayName)
		if variantSlug != "" {
			characterName = domainproject.NormalizeAssetName(characterName)
			bucket := buckets["character"]
			var character *domainproject.AssetEntry
			if bucket != nil {
				if key, ok := bucket.ResolveKey(characterName); ok {
					character = bucket[key]
				}
			}
			variant := findCharacterVariant(character, characterName, variantSlug)
			if variant == nil {
				missing = append(missing, displayName)
				continue
			}
			id, _ := variant["id"].(string)
			slug, _ := variant["slug"].(string)
			refs = append(refs, map[string]any{
				"type": "character", "name": characterName, "variant_id": id, "variant_slug": slug,
			})
			continue
		}
		resolved := false
		for _, rtype := range order {
			bucket := buckets[rtype]
			if bucket == nil {
				continue
			}
			if _, ok := bucket.ResolveKey(displayName); ok {
				refs = append(refs, map[string]any{"type": rtype, "name": displayName})
				resolved = true
				break
			}
		}
		if !resolved {
			missing = append(missing, displayName)
		}
	}
	return refs, missing
}

// deriveReferencesFromText 是书写层正文 → (references, missing) 的唯一派生入口。
func DeriveReferencesFromText(text string, project *domainproject.ProjectFile) ([]map[string]any, []string) {
	return resolveReferences(extractMentions(text), project)
}

// renderUnitReferences 把 references 渲染成落盘形态（去 variant_* 空值）。
func RenderUnitReferences(refs []map[string]any) []any {
	out := make([]any, 0, len(refs))
	for _, ref := range refs {
		entry := map[string]any{"type": ref["type"], "name": ref["name"]}
		if id, _ := ref["variant_id"].(string); id != "" {
			entry["variant_id"] = id
		}
		if slug, _ := ref["variant_slug"].(string); slug != "" {
			entry["variant_slug"] = slug
		}
		out = append(out, entry)
	}
	return out
}

// AssembleShotsTextForRender 把 unit.shots[*].text 拼接为供二次解析的书写层文本。
//
// 按数组位置重新注入规范 `镜头N：` header（先剥掉每个 shot 首行可能残留的旧 header）。
// 与 AssembleShotsText 的区别：后者裸拼接、不注入 header（用于空提示词结构校验）。
func AssembleShotsTextForRender(shots []any) string {
	parts := make([]string, 0, len(shots))
	index := 0
	for _, raw := range shots {
		shot, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		index++
		text, _ := shot["text"].(string)
		lines := []string{""}
		if text != "" {
			lines = splitShotSourceLines(text)
			if len(lines) == 0 {
				lines = []string{""}
			}
		}
		lines[0] = stripShotHeader(lines[0])
		parts = append(parts, fmt.Sprintf("镜头%d：%s", index, strings.Join(lines, "\n")))
	}
	return strings.Join(parts, "\n")
}

// contentLines 逐行剥掉 `镜头N：` header 后的正文行。
func contentLines(text string) []string {
	lines := splitShotSourceLines(text)
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, stripShotHeader(line))
	}
	return out
}

// dialogueSpeakers 按出现顺序取出规范台词行的说话人（去重）。
func dialogueSpeakers(text string) []string {
	seen := map[string]bool{}
	var speakers []string
	for _, line := range contentLines(text) {
		speaker, _, ok := matchDialogueLine(line)
		if !ok {
			continue
		}
		if !seen[speaker] {
			seen[speaker] = true
			speakers = append(speakers, speaker)
		}
	}
	return speakers
}

// normativeLine 是一条规范发声行：kind（dialogue / voiceover）+ speaker + 台词。
type normativeLine struct {
	kind    string
	speaker string
	text    string
}

// normativeLines 按出现顺序取出全部规范发声行，供 step2 保结构 diff 比对。
func normativeLines(text string) []normativeLine {
	var result []normativeLine
	for _, line := range contentLines(text) {
		if speaker, spoken, ok := matchDialogueLine(line); ok {
			result = append(result, normativeLine{kind: "dialogue", speaker: speaker, text: spoken})
			continue
		}
		if voiceover, ok := matchVoiceoverLine(line); ok {
			result = append(result, normativeLine{kind: "voiceover", text: voiceover})
		}
	}
	return result
}

// hasDescriptionLine 判定该镜头是否有画面描述行（非空、且既不是规范台词行也不是画外音行）。
func hasDescriptionLine(shotText string) bool {
	for _, line := range contentLines(shotText) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if _, _, ok := matchDialogueLine(line); ok {
			continue
		}
		if _, ok := matchVoiceoverLine(line); ok {
			continue
		}
		return true
	}
	return false
}

// fullwidthBraceChars 是全角花括号；语法只认半角。
const fullwidthBraceChars = "｛｝"

// assertLineSyntax 逐行判书写层语法：全角花括号、写坏的引用、缺花括号的台词行、描述行花括号。
func assertLineSyntax(label, text string, characters domainproject.AssetBucket) error {
	for idx, line := range contentLines(text) {
		lineIdx := idx
		if strings.ContainsAny(line, fullwidthBraceChars) {
			return Violation(label, "fullwidth_braces", &lineIdx,
				"%s 使用了全角花括号：%q；台词与画外音的花括号必须是半角 `{}`，全角形不会被识别为台词行",
				label, truncateRunes(strings.TrimSpace(line), 40))
		}
		if malformed, ok := findMalformedMention(line); ok {
			return Violation(label, "malformed_mention", &lineIdx,
				"%s 有写坏的资产引用：%q；引用须写成 `@[资产名]`，方括号要成对闭合、名称非空，"+
					"否则既不进 references，又会原样进入视频请求", label, malformed)
		}
		_, _, isDialogue := matchDialogueLine(line)
		if !isDialogue {
			if name, ok := leadingMentionBeforeColon(line); ok {
				if _, registered := characters.ResolveKey(name); registered {
					return Violation(label, "dialogue_line_syntax", &lineIdx,
						"%s 的台词行写法不合法：%q；台词须写成 `@[角色]：{台词}`——说话人非空、"+
							"台词由半角花括号整体包裹，否则这行会被当成画面描述、台词整句丢失",
						label, truncateRunes(strings.TrimSpace(line), 40))
				}
			}
		}
		if !strings.ContainsAny(line, "{}") {
			continue
		}
		if isDialogue {
			continue
		}
		if _, ok := matchVoiceoverLine(line); ok {
			continue
		}
		excerpt := truncateRunes(strings.TrimSpace(line), 40)
		if strings.Count(line, "{") != strings.Count(line, "}") {
			return Violation(label, "unclosed_brace", &lineIdx, "%s 有未闭合的花括号：%q", label, excerpt)
		}
		return Violation(label, "braces_in_description", &lineIdx,
			"%s 在画面描述行里使用了花括号：%q；花括号是台词保留语法，台词须独立成行写作 "+
				"`@[角色]：{台词}` 或 `{画外音}`", label, excerpt)
	}
	return nil
}

// truncateRunes 按 rune 截断并用 %q 渲染，保持与 Python 的 repr 截断近似。
func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit])
}

// Violation 构造一条带定位与行号的违约错误。
func Violation(label, code string, line *int, format string, args ...any) *DraftViolation {
	return &DraftViolation{Code: code, Label: label, Message: fmt.Sprintf(format, args...), Line: line}
}

// ValidateUnitText 校验一个 unit 的正文并机械派生 (shots, references)。
//
// 覆盖正文为空 / 镜头无画面描述 / 镜头行数超上限 / 语法误用 / 引用未登记 / references 超上限。
func ValidateUnitText(label, text string, project *domainproject.ProjectFile, maxRefs *int) ([]map[string]any, []map[string]any, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil, Violation(label, "empty_text", nil, "%s 的正文为空", label)
	}
	var characters domainproject.AssetBucket
	if project != nil {
		characters = project.Characters
	}
	if err := assertLineSyntax(label, text, characters); err != nil {
		return nil, nil, err
	}
	shots, _ := parsePrompt(text)
	var blankShots []int
	for index, shot := range shots {
		if body, _ := shot["text"].(string); !hasDescriptionLine(body) {
			blankShots = append(blankShots, index+1)
		}
	}
	if len(blankShots) > 0 {
		return nil, nil, Violation(label, "blank_shot", nil,
			"%s 的镜头 %v 没有画面描述；每个 `镜头N：` 都要写该镜头拍什么，只有台词行 / 画外音行的镜头没有可生成的画面",
			label, blankShots)
	}
	if len(shots) > MaxShotsPerUnit {
		return nil, nil, Violation(label, "too_many_shots", nil,
			"%s 有 %d 个镜头行，超过单 unit 上限 %d；请把多出的镜头按叙事顺序拆到新的 unit",
			label, len(shots), MaxShotsPerUnit)
	}
	refs, missing := DeriveReferencesFromText(text, project)
	if len(missing) > 0 {
		return nil, nil, Violation(label, "unregistered_asset", nil,
			"%s 引用了未登记的资产名: %v；资产名必须逐字取自 project.json 三张表", label, missing)
	}
	var badSpeakers []string
	for _, speaker := range dialogueSpeakers(text) {
		if _, ok := characters.ResolveKey(speaker); !ok {
			badSpeakers = append(badSpeakers, speaker)
		}
	}
	if len(badSpeakers) > 0 {
		sort.Strings(badSpeakers)
		return nil, nil, Violation(label, "unregistered_speaker", nil,
			"%s 的台词行说话人未登记为角色资产: %v；说话人决定该句台词绑哪段参考音频，必须是登记角色",
			label, badSpeakers)
	}
	if maxRefs != nil && len(refs) > *maxRefs {
		return nil, nil, Violation(label, "refs_over_limit", nil,
			"%s 的 references 数 %d 超过模型上限 %d；请把次要角色融入背景描述（不用 `@` 引用）",
			label, len(refs), *maxRefs)
	}
	return shots, refs, nil
}

// normalizeForAnchor 是源文锚的归一：NFC 后把连续空白折叠为单个空格，不删除空白本身。
func normalizeForAnchor(text string) string {
	collapsed := strings.Join(strings.Fields(norm.NFC.String(text)), " ")
	return collapsed
}

// validateSourceTextAnchor 校验 source_text 是源文的逐字子串（空白归一后）。
func validateSourceTextAnchor(label, sourceText, novelText string) error {
	anchor := normalizeForAnchor(sourceText)
	if anchor == "" {
		return Violation(label, "source_text_empty", nil,
			"%s 的 source_text 为空：每个 unit 必须摘录其所依据的原文片段作为追溯锚", label)
	}
	if !strings.Contains(normalizeForAnchor(novelText), anchor) {
		return Violation(label, "source_text_not_verbatim", nil,
			"%s 的 source_text 不是小说原文的逐字片段（存在改写、翻译或杜撰）：%q；请原样复制原文，不要转述",
			label, truncateRunes(strings.TrimSpace(sourceText), 40))
	}
	return nil
}

// ValidateDialogueLoad 校验该 unit 的台词量念得完：口播估算超出 unit 时长（含宽容系数）即违约。
func ValidateDialogueLoad(label, text string, durationSeconds int, language string, speechRateOverride *float64) error {
	spoken := 0.0
	for _, line := range normativeLines(text) {
		spoken += service.EstimateSpokenSeconds(line.text, language, speechRateOverride)
	}
	budget := float64(durationSeconds) * (1 + SpeechOverflowTolerance)
	if spoken > budget {
		return Violation(label, "dialogue_overload", nil,
			"%s 的台词念完约需 %.1f 秒，超过该 unit 的 %d 秒（宽容 %.0f%% 后上限 %.1f 秒）；"+
				"请改取更长的时长档、把该 unit 拆开，或精简台词",
			label, spoken, durationSeconds, SpeechOverflowTolerance*100, budget)
	}
	return nil
}

// assertDialoguePreserved 是 step2 保结构 diff：规范发声行序列必须与 step1 逐字一致。
func assertDialoguePreserved(label, step1Text, step2Text string) error {
	before := normativeLines(step1Text)
	after := normativeLines(step2Text)
	if normativeLinesEqual(before, after) {
		return nil
	}
	if len(before) != len(after) {
		return Violation(label, "dialogue_line_count_changed", nil,
			"%s 的台词行数被改动（step1 有 %d 行，step2 产出 %d 行）；step2 只做视觉展开，台词行须逐字保留",
			label, len(before), len(after))
	}
	for index := range before {
		if before[index] != after[index] {
			return Violation(label, "dialogue_rewritten", nil,
				"%s 第 %d 条台词被改写（原：%s「%s」，现：%s「%s」）；step2 只做视觉展开，台词行须逐字保留",
				label, index+1, speakerOrVoiceover(before[index]), before[index].text,
				speakerOrVoiceover(after[index]), after[index].text)
		}
	}
	return nil
}

func speakerOrVoiceover(line normativeLine) string {
	if line.speaker == "" {
		return "画外音"
	}
	return line.speaker
}

func normativeLinesEqual(a, b []normativeLine) bool {
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

// DraftViolations 是一次校验收集到的多条违约（对应 Python 的 DraftViolations）。
type DraftViolations struct {
	Items []DraftViolation
}

func (e *DraftViolations) Error() string { return RenderViolationReport(e.Items) }

// ViolationItems 把单条或聚合的违约一律摊平成条目列表。
func ViolationItems(err error) []DraftViolation {
	if err == nil {
		return nil
	}
	var aggregated *DraftViolations
	if errors.As(err, &aggregated) {
		return aggregated.Items
	}
	var single *DraftViolation
	if errors.As(err, &single) {
		return []DraftViolation{*single}
	}
	var value DraftViolation
	if errors.As(err, &value) {
		return []DraftViolation{value}
	}
	return nil
}

// collectViolations 依次执行各校验，收集 DraftViolation 而不在首个违约处中断。
func collectViolations(checks ...func() error) []DraftViolation {
	var found []DraftViolation
	for _, check := range checks {
		if err := check(); err != nil {
			found = append(found, ViolationItems(err)...)
		}
	}
	return found
}
