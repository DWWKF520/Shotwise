// 分集规划服务（对应 lib/episode_planner.py）：读源文窗口 → 调项目配置的文本模型 → 写账本
// 并派生集文件。
//
// plan() 从 planning_cursor 起取一个源文窗口，一次规划出窗口内所有剧情弧完整的集；schema 强
// 约束 + 锚点存在性/唯一性/连续性的机械校验，失败自动重试并附上一轮失败原因。
//
// 写入阶段在同一把项目锁内完成：写账本 + 按账本重写派生集文件 + 清理账本之外的残留派生文件，
// 下游读到的 source/episode_N.txt 永远与账本一致。
//
// 坐标口径：source_range / planning_cursor 的偏移是 Python 的字符（码点）偏移，故本文件所有
// 偏移计算与切片都走 rune（ledgerSlice / ledgerRuneLen），不得直接用字节下标。
package script

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/textgen"
)

// 窗口 / 批量内部默认值；project.json 顶层同名字段可覆盖。
const (
	DEFAULTPlanningWindowChars = 30000
	DEFAULTPlanningMaxEpisodes = 20
	maxPlanAttempts            = 3
	contextEpisodesLimit       = 5
	missingRangeListedLimit    = 10
	defaultMaxOutputTokens     = 64000
)

// EpisodePlanningError 是分集规划失败（源文缺失、校验重试耗尽等）。
type EpisodePlanningError struct{ Message string }

func (e *EpisodePlanningError) Error() string { return e.Message }

// PlanningConflictError 表示规划期间账本被并发修改，提交被拒绝；重新调用即可基于新状态规划。
type PlanningConflictError struct{ Message string }

func (e *PlanningConflictError) Error() string { return e.Message }

// EpisodePlanSummary 是单集摘要：标题 + 钩子 + 体量（按 source_language 计的阅读单位）。
type EpisodePlanSummary struct {
	Episode      int
	Title        string
	Hook         string
	ReadingUnits int
	LedgerStatus string
}

// LedgerStats 是全账本体量分布快照（机械现算，不做「多小算畸小」之类的阈值判断）。
type LedgerStats struct {
	TotalEpisodes int
	Smallest      [][2]int // (集号, 体量)，体量最小的最多 5 集，按体量升序
	MedianUnits   *int
	TargetUnits   *int
}

// PlanResult 是一次 plan 调用的结果。
type PlanResult struct {
	Episodes        []EpisodePlanSummary
	Cursor          *domain.PlanningCursor
	SourceExhausted bool
	StaleEpisodes   []int
	TotalPlanned    int
	LedgerStats     *LedgerStats
}

// TextCompletion 是文本生成的最小能力（textgen.Generator 满足）。
type TextCompletion interface {
	Generate(ctx context.Context, prompt string, schema map[string]any, maxOutputTokens int) (string, error)
}

// TextCompletionFactory 按项目创建文本生成器（走与剧本生成同一条 SCRIPT 任务配置链）。
type TextCompletionFactory func(ctx context.Context, pf *domain.ProjectFile) (TextCompletion, error)

// planningProgress 是本批规划时的全局进度快照，仅在 instructions 非空时注入 prompt。
type planningProgress struct {
	plannedCount   int
	remainingUnits int
	windowUnits    int
}

// planningDraftEpisode 是单集草稿（narration / drama 两形态共用）。
type planningDraftEpisode struct {
	Title             string
	Hook              string
	EndAnchor         string
	StoryBeats        []string
	NextEpisodeTeaser *string
	hasOutline        bool // drama 形态（story_beats 必填）
}

// draftRejected 表示单轮 LLM 输出被 schema / 机械校验拒绝；reasons 注入下一轮重试 prompt。
type draftRejected struct{ Reasons []string }

func (e *draftRejected) Error() string { return strings.Join(e.Reasons, "; ") }

// 全/半角标点折叠表：仅供匹配时构造折叠副本用，绝不落库。逐字符 1:1 映射（长度不变），
// 折叠坐标即 NFC 精确坐标。
var punctFold = map[rune]rune{
	'。': '.', '．': '.', '，': ',', '！': '!', '？': '?', '：': ':',
	'；': ';', '（': '(', '）': ')', '～': '~', '〜': '~',
	'“': '"', '”': '"', '‘': '\'', '’': '\'', '＂': '"', '＇': '\'',
}

// foldForMatch 构造匹配用折叠副本：折叠全/半角标点 + 把每个空白字符折成一个半角空格。
//
// 严格逐字符 1:1 映射（不合并连续空白），长度与原文一致（按 rune 计），因而折叠串上的偏移即
// 原文 NFC 坐标系内的精确偏移。
func foldForMatch(text string) []rune {
	runes := []rune(text)
	out := make([]rune, 0, len(runes))
	for _, r := range runes {
		if folded, ok := punctFold[r]; ok {
			out = append(out, folded)
			continue
		}
		if isSpaceRune(r) {
			out = append(out, ' ')
			continue
		}
		out = append(out, r)
	}
	return out
}

// isSpaceRune 复刻 Python str.isspace 的常见口径（含全角空格 U+3000）。
func isSpaceRune(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x85, 0xA0, 0x1680, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200A || r == 0x2028 || r == 0x2029 || r == 0x202F || r == 0x205F
}

// findAllOverlapping 收集 needle 在 haystack 内的全部起点（允许重叠），偏移按 rune 计。
func findAllOverlapping(haystack, needle []rune) []int {
	if len(needle) == 0 {
		return nil
	}
	var starts []int
	for i := 0; i+len(needle) <= len(haystack); i++ {
		matched := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				matched = false
				break
			}
		}
		if matched {
			starts = append(starts, i)
		}
	}
	return starts
}

// resolveBoundaries 把每集 end_anchor 解析为窗口内相对结束偏移，校验存在/唯一/连续。
//
// 范围由锚点构造性保证连续不重叠：第 i 集 = [第 i-1 集末尾, 第 i 集锚点末尾)。末尾只剩空白时
// （snapWhitespaceTail）把最后一集贴齐到窗口末尾。
func resolveBoundaries(window string, drafts []planningDraftEpisode, snapWhitespaceTail bool) ([]int, error) {
	windowRunes := []rune(window)
	windowLen := len(windowRunes)
	var (
		reasons []string
		ends    []int
		pos     = 0
	)
	orderingValid := true
	var foldedWindow []rune

	for idx, ep := range drafts {
		anchor := ep.EndAnchor
		anchorRunes := []rune(anchor)
		matchLen := len(anchorRunes)
		starts := findAllOverlapping(windowRunes, anchorRunes)
		matchedByFold := false
		if len(starts) == 0 {
			if foldedWindow == nil {
				foldedWindow = foldForMatch(window)
			}
			normalizedAnchor := []rune(NormalizeSourceText(anchor))
			starts = findAllOverlapping(foldedWindow, foldForMatch(string(normalizedAnchor)))
			if len(starts) > 0 {
				matchedByFold = true
				matchLen = len(normalizedAnchor)
			}
		}
		if len(starts) == 0 {
			reasons = append(reasons, fmt.Sprintf(
				"第 %d 条的 end_anchor 在原文窗口中不存在（必须逐字摘抄，含标点）: %q", idx+1, anchor))
			orderingValid = false
			continue
		}
		if len(starts) > 1 {
			if matchedByFold {
				reasons = append(reasons, fmt.Sprintf(
					"第 %d 条的 end_anchor 在原文窗口中按归一（NFC 与换行统一）及标点全/半角、空白折叠对齐后出现 %d 次"+
						"（逐字精确匹配可能为 0 次），无法唯一定位，请改用更长或更独特、与原文逐字一致的片段: %q",
					idx+1, len(starts), anchor))
			} else {
				reasons = append(reasons, fmt.Sprintf(
					"第 %d 条的 end_anchor 在原文窗口中出现 %d 次，无法唯一定位，请改用更长或更独特的片段: %q",
					idx+1, len(starts), anchor))
			}
			orderingValid = false
			continue
		}
		end := starts[0] + matchLen
		if orderingValid && end <= pos {
			reasons = append(reasons, fmt.Sprintf(
				"第 %d 条的 end_anchor 位置不在上一集结尾之后（各集范围必须连续推进、不重叠）: %q", idx+1, anchor))
			orderingValid = false
			continue
		}
		ends = append(ends, end)
		pos = end
	}
	if len(reasons) > 0 {
		return nil, &draftRejected{Reasons: reasons}
	}
	if snapWhitespaceTail && ends[len(ends)-1] < windowLen {
		if tail, ok := ledgerSlice(window, ends[len(ends)-1], windowLen); ok && strings.TrimSpace(tail) == "" {
			ends[len(ends)-1] = windowLen
		}
	}
	return ends, nil
}

// ledgerEntryFromDraft 把单集草稿物化为账本条目。
func ledgerEntryFromDraft(draft planningDraftEpisode, num int, sourceRel string, start, end int, status string) domain.EpisodeMeta {
	entry := domain.EpisodeMeta{
		Episode:      num,
		Title:        draft.Title,
		ScriptFile:   EpisodeScriptRelpath(num),
		SourceRange:  &domain.SourceRange{SourceFile: sourceRel, Start: start, End: end},
		Hook:         draft.Hook,
		LedgerStatus: status,
	}
	if draft.hasOutline {
		teaser := ""
		if draft.NextEpisodeTeaser != nil {
			teaser = *draft.NextEpisodeTeaser
		}
		entry.Outline = &domain.Outline{StoryBeats: append([]string(nil), draft.StoryBeats...), NextEpisodeTeaser: teaser}
	}
	return entry
}

// --- prompt 定位文案 ---

var planIntroNovel = []string{
	"你是短视频分集规划师。请把下面的小说原文片段切分为若干集，每一集都必须是一个完整的剧情弧，",
	"并在集尾留下让观众想看下一集的钩子。",
}

var planIntroScreenplay = []string{
	"你是短视频分集规划师。下面是作者已写好的成品剧本片段，请尊重作者自带的分集、提取而非重切：",
	"- 若剧本自带分集（任意形态——分集标记、结构表、标题体系、分隔等，不要依赖任何固定标记或正则识别），",
	"  照用作者划定的每一集边界，title、hook 与分集大纲都取自剧本原文。",
	"- 若剧本没有任何分集线索，再按完整剧情弧语义切分，每一集都是一个完整的故事段落，绝不按字数机械切碎。",
}

const planRuleScreenplay = "- 优先照用作者的分集：剧本已划定每集边界时，end_anchor 取作者每集结尾处的原文片段，" +
	"title / hook 也取自剧本（作者写明的集标题、集尾钩子）；剧本未分集时才按剧情弧自行切，绝不按字数硬凑集数。"

const planDramaOutlineNovel = "- 每一集另给出 story_beats（本集故事节点列表，按顺序）" +
	"与 next_episode_teaser（下集预告语；最后一集若后续未知可为 null）。"

const planDramaOutlineScreenplay = "- 每一集另给出 story_beats（本集故事节点列表，按顺序）" +
	"与 next_episode_teaser（下集预告语；最后一集若后续未知可为 null）；" +
	"剧本已写明本集节点 / 下集预告时照搬其原文，未写明再自行提炼。"

// userInstructionsHeader 是「用户意见」分节标题（对应 lib/prompt_builders_script.USER_INSTRUCTIONS_HEADER）。
const userInstructionsHeader = "# 用户意见"

// buildPlanningPrompt 组装规划 prompt。仅面向文本模型，不做 i18n。
func buildPlanningPrompt(
	project *domain.ProjectFile, window string, windowIsFinal bool, maxEpisodes int,
	contentMode string, contextEntries []domain.EpisodeMeta, instructions string,
	failure []string, progress *planningProgress,
) string {
	isScreenplay := project.SourceKind == "screenplay"
	intro := planIntroNovel
	if isScreenplay {
		intro = planIntroScreenplay
	}
	unitName := service.ReadingUnitNoun(project.SourceLanguage)

	lines := append([]string{}, intro...)
	lines = append(lines, "", "# 项目信息")
	modeText := "说书旁白（narration）"
	if contentMode == "drama" {
		modeText = "剧集动画（drama）"
	}
	lines = append(lines, "- 内容模式："+modeText)
	if project.Overview != nil {
		if project.Overview.Synopsis != "" {
			lines = append(lines, "- 故事概述："+project.Overview.Synopsis)
		}
		if project.Overview.Genre != "" {
			lines = append(lines, "- 题材："+project.Overview.Genre)
		}
	}
	if project.EpisodeTargetUnits != nil && *project.EpisodeTargetUnits >= 1 {
		lines = append(lines, fmt.Sprintf("- 每集目标体量：约 %d %s（允许为剧情完整性上下浮动）",
			*project.EpisodeTargetUnits, unitName))
	} else {
		lines = append(lines, "- 每集目标体量：未设置，请按短视频节奏自行把握（以剧情弧完整优先）")
	}
	lines = append(lines, fmt.Sprintf("- 本批最多规划 %d 集", maxEpisodes))

	if len(contextEntries) > 0 {
		lines = append(lines, "", "# 已规划的前情（已定上下文，不可改动，续着它往下规划）")
		for _, entry := range contextEntries {
			title := entry.Title
			if title == "" {
				title = "（无标题）"
			}
			lines = append(lines, fmt.Sprintf("- 第 %d 集《%s》 钩子：%s", entry.Episode, title, entry.Hook))
		}
	}

	if instructions != "" {
		lines = append(lines, "", userInstructionsHeader, instructions)
	}
	if progress != nil {
		lines = append(lines, "",
			"# 全局进度",
			fmt.Sprintf("- 已规划 %d 集", progress.plannedCount),
			fmt.Sprintf("- 未规划余量约 %d %s（含本窗口，含后续源文件）", progress.remainingUnits, unitName),
			fmt.Sprintf("- 本窗口为其中前 %d %s", progress.windowUnits, unitName),
			"- 若用户意见含总集数、按章节对齐等全局约束，请结合以上进度与余量换算本批的切分节奏与集数。")
	}

	lines = append(lines, "",
		"# 切分规则",
		"- 每一集给出 title（吸引人的短标题）、hook（集尾钩子说明：这一刀为什么切在这、给观众留了什么悬念）、",
		"  end_anchor（本集结尾处的原文片段，10~30 个字符，必须从下方原文中逐字摘抄、含标点，且在整段原文中唯一出现；",
		"  本集内容 = 上一集结尾之后到该片段末尾为止的全部原文）。")
	if isScreenplay {
		lines = append(lines, planRuleScreenplay)
	}
	if contentMode == "drama" {
		if isScreenplay {
			lines = append(lines, planDramaOutlineScreenplay)
		} else {
			lines = append(lines, planDramaOutlineNovel)
		}
	}
	lines = append(lines, "- 各集按顺序排列，end_anchor 位置必须严格递增（范围连续、不重叠、不留空洞）。")
	if windowIsFinal {
		lines = append(lines, "- 这段原文已包含全文结尾：请规划到结尾，最后一集的 end_anchor 取全文结尾处的片段，不要留尾巴。")
	} else {
		lines = append(lines, "- 这段原文只是全文的一个窗口：窗口尾部剧情弧不完整的内容不要硬凑成集，留给下一批规划即可。")
	}
	lines = append(lines, "- 只输出符合 schema 的 JSON，不要输出其他内容。")

	if len(failure) > 0 {
		lines = append(lines, "", "# 上一轮输出未通过校验，请针对性修正后重新输出")
		for _, reason := range failure {
			lines = append(lines, "- "+reason)
		}
	}

	bodyHeader := "# 小说原文片段"
	if isScreenplay {
		bodyHeader = "# 剧本原文片段"
	}
	lines = append(lines, "", bodyHeader, "---", window, "---")
	return strings.Join(lines, "\n")
}

// --- 结构化输出 schema ---

// narrationPlanSchema 是 narration 规划的 JSON Schema（对应 Pydantic NarrationPlanDraft，extra=forbid）。
func narrationPlanSchema() map[string]any {
	return planSchema(false)
}

func dramaPlanSchema() map[string]any {
	return planSchema(true)
}

func planSchema(drama bool) map[string]any {
	properties := map[string]any{
		"title":      map[string]any{"type": "string", "minLength": 1},
		"hook":       map[string]any{"type": "string", "minLength": 1},
		"end_anchor": map[string]any{"type": "string", "minLength": 2},
	}
	required := []any{"title", "hook", "end_anchor"}
	if drama {
		properties["story_beats"] = map[string]any{
			"type": "array", "minItems": 1, "items": map[string]any{"type": "string"},
		}
		properties["next_episode_teaser"] = map[string]any{"type": []any{"string", "null"}}
		required = append(required, "story_beats")
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"episodes": map[string]any{
				"type":     "array",
				"minItems": 1,
				"items": map[string]any{
					"type":                 "object",
					"properties":           properties,
					"required":             required,
					"additionalProperties": false,
				},
			},
		},
		"required":             []any{"episodes"},
		"additionalProperties": false,
	}
}

// parsePlanDraft 解析并校验模型输出（对应 Pydantic model_validate + extra=forbid）。
func parsePlanDraft(responseText string, drama bool) ([]planningDraftEpisode, error) {
	text := service.StripJSONCodeFences(responseText)
	var raw any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, &draftRejected{Reasons: []string{"输出不是合法 JSON：" + err.Error()}}
	}
	root, ok := raw.(map[string]any)
	if !ok {
		return nil, &draftRejected{Reasons: []string{"输出不符合 schema：根节点必须是对象"}}
	}
	episodesRaw, ok := root["episodes"].([]any)
	if !ok || len(episodesRaw) == 0 {
		return nil, &draftRejected{Reasons: []string{"输出不符合 schema：episodes 必须是非空数组"}}
	}
	allowed := map[string]bool{"title": true, "hook": true, "end_anchor": true}
	if drama {
		allowed["story_beats"] = true
		allowed["next_episode_teaser"] = true
	}
	out := make([]planningDraftEpisode, 0, len(episodesRaw))
	for index, item := range episodesRaw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, &draftRejected{Reasons: []string{fmt.Sprintf("episodes.%d: 必须是对象", index)}}
		}
		for key := range m {
			if !allowed[key] {
				return nil, &draftRejected{Reasons: []string{fmt.Sprintf("episodes.%d.%s: 不允许的字段", index, key)}}
			}
		}
		draft := planningDraftEpisode{hasOutline: drama}
		title, ok := m["title"].(string)
		if !ok || len([]rune(title)) < 1 {
			return nil, &draftRejected{Reasons: []string{fmt.Sprintf("episodes.%d.title: 必填且非空", index)}}
		}
		draft.Title = title
		hook, ok := m["hook"].(string)
		if !ok || len([]rune(hook)) < 1 {
			return nil, &draftRejected{Reasons: []string{fmt.Sprintf("episodes.%d.hook: 必填且非空", index)}}
		}
		draft.Hook = hook
		anchor, ok := m["end_anchor"].(string)
		if !ok || len([]rune(anchor)) < 2 {
			return nil, &draftRejected{Reasons: []string{fmt.Sprintf("episodes.%d.end_anchor: 至少 2 个字符", index)}}
		}
		draft.EndAnchor = anchor
		if drama {
			beatsRaw, ok := m["story_beats"].([]any)
			if !ok || len(beatsRaw) == 0 {
				return nil, &draftRejected{Reasons: []string{fmt.Sprintf("episodes.%d.story_beats: 必填且为非空数组", index)}}
			}
			beats := make([]string, 0, len(beatsRaw))
			for beatIdx, beat := range beatsRaw {
				s, ok := beat.(string)
				if !ok {
					return nil, &draftRejected{Reasons: []string{
						fmt.Sprintf("episodes.%d.story_beats.%d: 必须是字符串", index, beatIdx)}}
				}
				beats = append(beats, s)
			}
			draft.StoryBeats = beats
			if teaser, present := m["next_episode_teaser"]; present && teaser != nil {
				s, ok := teaser.(string)
				if !ok {
					return nil, &draftRejected{Reasons: []string{
						fmt.Sprintf("episodes.%d.next_episode_teaser: 必须是字符串或 null", index)}}
				}
				draft.NextEpisodeTeaser = &s
			}
		}
		out = append(out, draft)
	}
	return out, nil
}


// --- 规划器 ---

// EpisodePlanner 是分集规划器。
type EpisodePlanner struct {
	store       EpisodeStore
	projectName string
	projectDir  string
	gen         TextCompletion
}

// NewEpisodePlanner 构造（gen 为 nil 时调用 Plan 会报错）。
func NewEpisodePlanner(store EpisodeStore, projectName string, gen TextCompletion) (*EpisodePlanner, error) {
	dir, err := store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	return &EpisodePlanner{store: store, projectName: projectName, projectDir: dir, gen: gen}, nil
}

func (p *EpisodePlanner) loadProject() (*domain.ProjectFile, error) {
	return p.store.Get(p.projectName)
}

// Plan 规划下一批集：从 planning_cursor 起的窗口产出剧情弧完整的集并提交账本。
func (p *EpisodePlanner) Plan(ctx context.Context, instructions string) (*PlanResult, error) {
	if p.gen == nil {
		return nil, &EpisodePlanningError{Message: "文本生成器未初始化"}
	}
	planningInstructions := strings.TrimSpace(instructions)

	project, err := p.loadProject()
	if err != nil {
		return nil, err
	}
	RegisterOrphanEpisodeEntries(p.projectDir, project)
	if err := checkSourceRanges(project); err != nil {
		return nil, err
	}
	preCallSources := DiscoverSources(p.projectDir)
	if err := p.checkSourceFingerprints(project, preCallSources); err != nil {
		return nil, err
	}
	usedFingerprints := ComputeSourceFingerprints(preCallSources)

	preCallText := map[string]string{}
	for _, doc := range preCallSources {
		preCallText[doc.RelPath] = doc.Text
	}

	sourceRel, start, err := p.effectiveStart(project)
	if err != nil {
		return nil, err
	}
	text, ok := preCallText[sourceRel]
	if !ok {
		text, err = p.loadNormalizedSource(sourceRel)
		if err != nil {
			return nil, err
		}
		usedFingerprints[sourceRel] = ComputeSourceFingerprints([]SourceDoc{{RelPath: sourceRel, Text: text}})[sourceRel]
	}
	if start > ledgerRuneLen(text) {
		return nil, &EpisodePlanningError{Message: fmt.Sprintf(
			"规划起点越界：%s 长度 %d，起点 %d；请检查账本", sourceRel, ledgerRuneLen(text), start)}
	}
	for {
		rest, _ := ledgerSlice(text, start, ledgerRuneLen(text))
		if strings.TrimSpace(rest) != "" {
			break
		}
		nextRel, ok := p.nextSourceRel(sourceRel)
		if !ok {
			if err := p.backfillSourceFingerprintsIfMissing(usedFingerprints); err != nil {
				return nil, err
			}
			final, err := p.loadProject()
			if err != nil {
				return nil, err
			}
			return &PlanResult{
				Episodes:        []EpisodePlanSummary{},
				Cursor:          final.PlanningCursor,
				SourceExhausted: true,
				TotalPlanned:    countPlannedEpisodes(final),
				LedgerStats:     p.computeLedgerStats(final),
			}, nil
		}
		sourceRel, start = nextRel, 0
		text, ok = preCallText[sourceRel]
		if !ok {
			text, err = p.loadNormalizedSource(sourceRel)
			if err != nil {
				return nil, err
			}
			usedFingerprints[sourceRel] = ComputeSourceFingerprints([]SourceDoc{{RelPath: sourceRel, Text: text}})[sourceRel]
		}
	}
	preCallSources = nil

	windowChars := p.settingInt(project, "planning_window_chars", DEFAULTPlanningWindowChars)
	maxEpisodes := p.settingInt(project, "planning_max_episodes", DEFAULTPlanningMaxEpisodes)
	textLen := ledgerRuneLen(text)
	remainingChars := textLen - start
	// 窗口弹性：剩余全文不足 1.2 倍窗口时直接吃到底，避免下一批只剩孤儿残余被迫单独成集。
	windowEnd := start + windowChars
	if remainingChars <= int(float64(windowChars)*1.2) {
		windowEnd = textLen
	}
	if windowEnd > textLen {
		windowEnd = textLen
	}
	window, _ := ledgerSlice(text, start, windowEnd)
	windowIsFinal := windowEnd >= textLen
	contentMode := project.ContentMode
	if contentMode == "" {
		contentMode = "narration"
	}
	isDrama := contentMode == "drama"
	language := project.SourceLanguage
	contextEntries := p.contextEntries(project)

	// 全局进度仅在有 instructions 时算、仅在有 instructions 时注入 prompt：无指令路径的 prompt
	// 必须逐字保持不变。
	var progress *planningProgress
	if planningInstructions != "" {
		progress = &planningProgress{
			plannedCount:   countPlannedEpisodes(project),
			remainingUnits: p.remainingUnitsFrom(sourceRel, start, text, language),
			windowUnits:    service.CountReadingUnits(window, language),
		}
	}

	promptBuilder := func(failure []string) string {
		return buildPlanningPrompt(project, window, windowIsFinal, maxEpisodes, contentMode,
			contextEntries, planningInstructions, failure, progress)
	}

	drafts, ends, err := p.requestValidatedDrafts(ctx, isDrama, promptBuilder, window, windowIsFinal, maxEpisodes)
	if err != nil {
		return nil, err
	}

	var (
		summaries  []EpisodePlanSummary
		staleNums  []int
		cursor     *domain.PlanningCursor
		exhausted  bool
		totalCount int
		stats      *LedgerStats
	)
	err = p.store.Update(p.projectName, func(pf *domain.ProjectFile) error {
		// 锁内复核缺位置记录的条目，并补齐同一并发窗口内新出现的孤儿派生文件。
		RegisterOrphanEpisodeEntries(p.projectDir, pf)
		if err := checkSourceRanges(pf); err != nil {
			return err
		}
		effectiveRel, effectiveStart, err := p.effectiveStart(pf)
		if err != nil {
			return err
		}
		if effectiveRel != sourceRel || effectiveStart != start {
			return &PlanningConflictError{Message: "规划期间账本进度被并发修改，本次结果作废；请重新调用规划"}
		}
		currentSources := DiscoverSources(p.projectDir)
		if err := p.checkSourceFingerprints(pf, currentSources); err != nil {
			return err
		}
		currentFingerprints := ComputeSourceFingerprints(currentSources)
		var changed []string
		for rel, fingerprint := range usedFingerprints {
			if currentFingerprints[rel] != fingerprint {
				changed = append(changed, rel)
			}
		}
		if len(changed) > 0 {
			sort.Strings(changed)
			return sourceChangedError(changed)
		}

		nextNum := 1
		for index := range pf.Episodes {
			num, ok := ParseEpisodeNum(pf.Episodes[index].Episode)
			if ok && num >= nextNum {
				nextNum = num + 1
			}
		}
		prev := start
		for offset, draft := range drafts {
			num := nextNum + offset
			relEnd := ends[offset]
			absEnd := start + relEnd
			entry := ledgerEntryFromDraft(draft, num, sourceRel, prev, absEnd, domain.LedgerStatusPlanned)
			// 新集号若在磁盘上已有剧本/step1/媒体产物（重置到更早集号后重新规划、新布局与原消费
			// 范围重叠），说明该集实际已被消费过；标 stale 提示需重做下游产物，产物本身不删除。
			if HasDownstreamProducts(p.projectDir, num, &entry) {
				entry.LedgerStatus = domain.LedgerStatusStale
				staleNums = append(staleNums, num)
			}
			pf.Episodes = append(pf.Episodes, entry)
			segment, _ := ledgerSlice(text, prev, absEnd)
			summaries = append(summaries, EpisodePlanSummary{
				Episode:      num,
				Title:        draft.Title,
				Hook:         draft.Hook,
				ReadingUnits: service.CountReadingUnits(segment, language),
				LedgerStatus: entry.LedgerStatus,
			})
			prev = absEnd
		}
		sortEpisodesIfPossible(pf.Episodes)
		cursor = &domain.PlanningCursor{SourceFile: sourceRel, Offset: start + ends[len(ends)-1]}
		pf.PlanningCursor = cursor
		pf.SourceFingerprints = currentFingerprints
		if err := p.reconcileDerivedFiles(pf, map[string]string{sourceRel: text}); err != nil {
			return err
		}
		rest, _ := ledgerSlice(text, cursor.Offset, ledgerRuneLen(text))
		_, hasNext := p.nextSourceRel(sourceRel)
		exhausted = windowIsFinal && strings.TrimSpace(rest) == "" && !hasNext
		totalCount = countPlannedEpisodes(pf)
		if exhausted {
			stats = p.computeLedgerStats(pf)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &PlanResult{
		Episodes:        summaries,
		Cursor:          cursor,
		SourceExhausted: exhausted,
		StaleEpisodes:   staleNums,
		TotalPlanned:    totalCount,
		LedgerStats:     stats,
	}, nil
}

// requestValidatedDrafts 是 LLM 调用 + schema/机械校验循环；重试 prompt 附上一轮失败原因。
func (p *EpisodePlanner) requestValidatedDrafts(
	ctx context.Context, drama bool, promptBuilder func([]string) string,
	window string, snapWhitespaceTail bool, maxEpisodes int,
) ([]planningDraftEpisode, []int, error) {
	schema := narrationPlanSchema()
	if drama {
		schema = dramaPlanSchema()
	}
	var failure []string
	for attempt := 1; attempt <= maxPlanAttempts; attempt++ {
		output, err := p.gen.Generate(ctx, promptBuilder(failure), schema, defaultMaxOutputTokens)
		if err != nil {
			if errors.Is(err, textgen.ErrOutputTruncated) {
				return nil, nil, &EpisodePlanningError{Message: fmt.Sprintf(
					"%v也可调小项目设置 planning_window_chars（单批窗口字数）或 "+
						"planning_max_episodes（单批集数上限）以缩小本批输出体量后重试。", err)}
			}
			return nil, nil, &EpisodePlanningError{Message: err.Error()}
		}
		drafts, err := parsePlanDraft(output, drama)
		if err != nil {
			var rejected *draftRejected
			if errors.As(err, &rejected) {
				failure = rejected.Reasons
				slog.Warn("分集规划尝试未通过校验", "attempt", attempt, "max", maxPlanAttempts, "reasons", failure)
				continue
			}
			return nil, nil, err
		}
		if len(drafts) > maxEpisodes {
			slog.Warn("规划输出超过每批上限，截断保留前若干集（其余留给下一批）",
				"episodes", len(drafts), "max", maxEpisodes)
			drafts = drafts[:maxEpisodes]
		}
		ends, err := resolveBoundaries(window, drafts, snapWhitespaceTail)
		if err != nil {
			var rejected *draftRejected
			if errors.As(err, &rejected) {
				failure = rejected.Reasons
				slog.Warn("分集规划尝试未通过校验", "attempt", attempt, "max", maxPlanAttempts, "reasons", failure)
				continue
			}
			return nil, nil, err
		}
		return drafts, ends, nil
	}
	return nil, nil, &EpisodePlanningError{Message: fmt.Sprintf(
		"分集规划连续 %d 次未通过校验，最后一轮原因：%s", maxPlanAttempts, strings.Join(failure, "; "))}
}

// --- helpers ---

// effectiveStart 返回下一批规划起点：以账本中最后一个锚定集的范围末尾为准，游标更靠后时取游标。
func (p *EpisodePlanner) effectiveStart(project *domain.ProjectFile) (string, int, error) {
	var lastRel string
	var lastEnd int
	bestNum := 0
	hasLast := false
	for index := range project.Episodes {
		entry := &project.Episodes[index]
		num, ok := ParseEpisodeNum(entry.Episode)
		sourceRange, hasRange := ParseSourceRange(entry)
		if !ok || !hasRange {
			continue
		}
		if !hasLast || num > bestNum {
			bestNum = num
			lastRel, lastEnd, hasLast = sourceRange.SourceFile, sourceRange.End, true
		}
	}
	var curRel string
	var curOffset int
	hasCur := false
	if cursor := project.PlanningCursor; cursor != nil && cursor.Offset >= 0 && cursor.SourceFile != "" {
		curRel, curOffset, hasCur = cursor.SourceFile, cursor.Offset, true
	}
	if hasLast && hasCur {
		if curRel == lastRel {
			return lastRel, maxInt(lastEnd, curOffset), nil
		}
		rels := relPaths(DiscoverSources(p.projectDir))
		lastIdx, hasLastIdx := indexOf(rels, lastRel)
		curIdx, hasCurIdx := indexOf(rels, curRel)
		if !hasLastIdx {
			if hasCurIdx {
				return curRel, curOffset, nil
			}
			return lastRel, lastEnd, nil
		}
		if !hasCurIdx || curIdx < lastIdx {
			return lastRel, lastEnd, nil
		}
		return curRel, curOffset, nil
	}
	if hasLast {
		return lastRel, lastEnd, nil
	}
	if hasCur {
		return curRel, curOffset, nil
	}
	sources := DiscoverSources(p.projectDir)
	if len(sources) == 0 {
		return "", 0, &EpisodePlanningError{Message: "source/ 下没有可规划的源文件（.txt/.md），请先上传小说原文"}
	}
	return sources[0].RelPath, 0, nil
}

// remainingUnitsFrom 计当前窗口起点之后全部 + 后续源文件的阅读单位总量（全局进度提示用）。
func (p *EpisodePlanner) remainingUnitsFrom(sourceRel string, start int, text, language string) int {
	rest, _ := ledgerSlice(text, start, ledgerRuneLen(text))
	total := service.CountReadingUnits(rest, language)
	sources := DiscoverSources(p.projectDir)
	idx, ok := indexOf(relPaths(sources), sourceRel)
	if !ok {
		return total
	}
	for _, doc := range sources[idx+1:] {
		total += service.CountReadingUnits(doc.Text, language)
	}
	return total
}

// nextSourceRel 按文件名序返回 rel 之后的下一个候选源文件。
func (p *EpisodePlanner) nextSourceRel(rel string) (string, bool) {
	rels := relPaths(DiscoverSources(p.projectDir))
	idx, ok := indexOf(rels, rel)
	if !ok || idx+1 >= len(rels) {
		return "", false
	}
	return rels[idx+1], true
}

// loadNormalizedSource 读入并归一化源文。
func (p *EpisodePlanner) loadNormalizedSource(rel string) (string, error) {
	path, err := safeJoinAllowBase(p.projectDir, rel)
	if err != nil {
		return "", &EpisodePlanningError{Message: "源文件路径越出项目目录：" + rel}
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", &EpisodePlanningError{Message: "源文件不存在：" + rel}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", &EpisodePlanningError{Message: fmt.Sprintf("源文件读取失败：%s: %v", rel, err)}
	}
	return NormalizeSourceText(string(raw)), nil
}

// checkSourceFingerprints 比对账本记录的源文指纹与当前源文，不一致即拒绝规划。
func (p *EpisodePlanner) checkSourceFingerprints(project *domain.ProjectFile, sources []SourceDoc) error {
	docs := sources
	if docs == nil {
		docs = DiscoverSources(p.projectDir)
	}
	if mismatched := MismatchedSourceFingerprints(project.SourceFingerprints, docs); len(mismatched) > 0 {
		return sourceChangedError(mismatched)
	}
	return nil
}

// checkSourceRanges 账本存在无位置记录的条目即拒绝规划（老项目遗留，指路全量重置）。
func checkSourceRanges(project *domain.ProjectFile) error {
	if missing := EpisodesWithoutSourceRange(project); len(missing) > 0 {
		listed := make([]string, 0, missingRangeListedLimit)
		for _, num := range missing[:minInt(len(missing), missingRangeListedLimit)] {
			listed = append(listed, fmt.Sprint(num))
		}
		suffix := ""
		if len(missing) > missingRangeListedLimit {
			suffix = fmt.Sprintf(" 等 %d 集", len(missing))
		}
		return &EpisodePlanningError{Message: fmt.Sprintf(
			"账本中第 %s%s 没有原文范围记录（source_range），无法据此续接规划——这类条目的物理集文件"+
				"就是它们的最终记录，既无法重造也无法确定下一批的起点；请先调用 reset_episode_planning 做"+
				"全量重置（这些集的集文件会改名留底、下游产物不删），再重新规划。",
			strings.Join(listed, "、"), suffix)}
	}
	return nil
}

// backfillSourceFingerprintsIfMissing 存量项目在 source_exhausted 早退路径上补记指纹。
func (p *EpisodePlanner) backfillSourceFingerprintsIfMissing(usedFingerprints map[string]string) error {
	project, err := p.loadProject()
	if err != nil {
		return err
	}
	if project.SourceFingerprints != nil {
		return nil
	}
	return p.store.Update(p.projectName, func(pf *domain.ProjectFile) error {
		currentSources := DiscoverSources(p.projectDir)
		if err := p.checkSourceFingerprints(pf, currentSources); err != nil {
			return err
		}
		currentFingerprints := ComputeSourceFingerprints(currentSources)
		var changed []string
		for rel, fingerprint := range usedFingerprints {
			if currentFingerprints[rel] != fingerprint {
				changed = append(changed, rel)
			}
		}
		if len(changed) > 0 {
			sort.Strings(changed)
			return sourceChangedError(changed)
		}
		pf.SourceFingerprints = currentFingerprints
		return nil
	})
}

// settingInt 读项目设置里的正整数；缺失或非法回落内部默认。
func (p *EpisodePlanner) settingInt(project *domain.ProjectFile, key string, fallback int) int {
	var value *int
	switch key {
	case "planning_window_chars":
		value = project.PlanningWindowChars
	case "planning_max_episodes":
		value = project.PlanningMaxEpisodes
	}
	if value != nil && *value >= 1 {
		return *value
	}
	if value != nil {
		slog.Warn("项目设置非法，回退内部默认", "key", key, "value", *value, "default", fallback)
	}
	return fallback
}

// reconcileDerivedFiles 按账本全量对账派生集文件：重写每一集、删除账本之外的残留。
//
// 两阶段执行：先全量校验并构建写入计划，全部通过后再统一落盘——锚定集原文范围非法或源文
// 不可读时在校验阶段抛错中止提交（账本写回随之回滚，派生文件未动）。残留文件删除失败仅告警。
func (p *EpisodePlanner) reconcileDerivedFiles(project *domain.ProjectFile, textCache map[string]string) error {
	sourceDir := filepath.Join(p.projectDir, "source")
	// source/ 是符号链接时拒绝写入：派生文件会落到链接目标（可能在项目外）。
	// 边界：Windows 目录联接不置 ModeSymlink，本检查在 Windows 上只覆盖符号链接。
	if info, err := os.Lstat(sourceDir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return &EpisodePlanningError{Message: "source/ 不能是符号链接，拒绝派生集文件"}
	}
	keep := map[int]bool{}
	type pendingWrite struct {
		path    string
		content string
	}
	var writes []pendingWrite
	for index := range project.Episodes {
		entry := &project.Episodes[index]
		num, ok := ParseEpisodeNum(entry.Episode)
		if !ok {
			continue
		}
		keep[num] = true
		sourceRange, hasRange := ParseSourceRange(entry)
		if !hasRange {
			return &EpisodePlanningError{Message: fmt.Sprintf(
				"第 %d 集缺少原文范围记录，无法完成派生文件对账，提交已中止", num)}
		}
		text, cached := textCache[sourceRange.SourceFile]
		if !cached {
			loaded, err := p.loadNormalizedSource(sourceRange.SourceFile)
			if err != nil {
				return &EpisodePlanningError{Message: fmt.Sprintf(
					"第 %d 集派生文件重写失败，提交已中止：%v", num, err)}
			}
			text = loaded
			textCache[sourceRange.SourceFile] = text
		}
		segment, ok := ledgerSlice(text, sourceRange.Start, sourceRange.End)
		if !ok {
			return &EpisodePlanningError{Message: fmt.Sprintf(
				"第 %d 集原文范围越界（start=%d，end=%d，源文长度 %d），无法完成派生文件对账，提交已中止",
				num, sourceRange.Start, sourceRange.End, ledgerRuneLen(text))}
		}
		episodePath := filepath.Join(sourceDir, fmt.Sprintf("episode_%d.txt", num))
		if info, err := os.Lstat(episodePath); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return &EpisodePlanningError{Message: fmt.Sprintf(
				"第 %d 集派生文件是符号链接，拒绝写入，提交已中止", num)}
		}
		writes = append(writes, pendingWrite{path: episodePath, content: segment})
	}
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		return err
	}
	for _, write := range writes {
		if err := os.WriteFile(write.path, []byte(write.content), 0o644); err != nil {
			return err
		}
	}
	for num, path := range DiscoverEpisodeFiles(p.projectDir) {
		if keep[num] {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("残留派生文件清理失败（不阻断提交）", "path", path, "err", err)
		}
	}
	remaining := filepath.Join(sourceDir, "_remaining.txt")
	if info, err := os.Stat(remaining); err == nil && !info.IsDir() {
		if err := os.Remove(remaining); err != nil {
			slog.Warn("余文文件清理失败（不阻断提交）", "path", remaining, "err", err)
		}
	}
	return nil
}

// computeLedgerStats 账本现算全局体量分布：累计集数、最小 5 集、体量中位数。
func (p *EpisodePlanner) computeLedgerStats(project *domain.ProjectFile) *LedgerStats {
	language := project.SourceLanguage
	textCache := map[string]string{}
	unitsByEpisode := map[int]int{}
	for index := range project.Episodes {
		entry := &project.Episodes[index]
		num, ok := ParseEpisodeNum(entry.Episode)
		sourceRange, hasRange := ParseSourceRange(entry)
		if !ok || !hasRange {
			continue
		}
		text, cached := textCache[sourceRange.SourceFile]
		if !cached {
			loaded, err := p.loadNormalizedSource(sourceRange.SourceFile)
			if err != nil {
				continue
			}
			text = loaded
			textCache[sourceRange.SourceFile] = text
		}
		segment, ok := ledgerSlice(text, sourceRange.Start, sourceRange.End)
		if !ok {
			continue
		}
		unitsByEpisode[num] = service.CountReadingUnits(segment, language)
	}

	type pair struct{ num, units int }
	ordered := make([]pair, 0, len(unitsByEpisode))
	for num, units := range unitsByEpisode {
		ordered = append(ordered, pair{num, units})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].units != ordered[j].units {
			return ordered[i].units < ordered[j].units
		}
		return ordered[i].num < ordered[j].num
	})
	stats := &LedgerStats{TotalEpisodes: countPlannedEpisodes(project)}
	for idx, item := range ordered {
		if idx >= contextEpisodesLimit {
			break
		}
		stats.Smallest = append(stats.Smallest, [2]int{item.num, item.units})
	}
	if len(ordered) > 0 {
		values := make([]int, 0, len(ordered))
		for _, item := range ordered {
			values = append(values, item.units)
		}
		sort.Ints(values)
		median := medianRounded(values)
		stats.MedianUnits = &median
	}
	if project.EpisodeTargetUnits != nil && *project.EpisodeTargetUnits >= 1 {
		target := *project.EpisodeTargetUnits
		stats.TargetUnits = &target
	}
	return stats
}

// contextEntries 取已规划末尾若干集的标题+钩子，作为续写连贯性上下文（只取有位置记录的条目）。
func (p *EpisodePlanner) contextEntries(project *domain.ProjectFile) []domain.EpisodeMeta {
	type anchoredEntry struct {
		num   int
		entry domain.EpisodeMeta
	}
	var anchored []anchoredEntry
	for index := range project.Episodes {
		entry := project.Episodes[index]
		num, ok := ParseEpisodeNum(entry.Episode)
		if !ok {
			continue
		}
		if _, hasRange := ParseSourceRange(&entry); !hasRange {
			continue
		}
		anchored = append(anchored, anchoredEntry{num, entry})
	}
	sort.SliceStable(anchored, func(i, j int) bool { return anchored[i].num < anchored[j].num })
	start := 0
	if len(anchored) > contextEpisodesLimit {
		start = len(anchored) - contextEpisodesLimit
	}
	out := make([]domain.EpisodeMeta, 0, len(anchored)-start)
	for _, item := range anchored[start:] {
		out = append(out, item.entry)
	}
	return out
}

// --- 包级工具 ---

// sourceChangedError 构造源文已变动的拒绝错误：指名变动文件并指路全量重置。
func sourceChangedError(paths []string) error {
	return &EpisodePlanningError{Message: fmt.Sprintf(
		"源文件已被修改或移除：%s。账本坐标绑定的是修改前的原文内容，继续规划会静默切出"+
			"错误内容；请先调用 reset_episode_planning 做全量重置，再重新规划。",
		strings.Join(paths, "、"))}
}

// sortEpisodesIfPossible 全部集号可解析时按集号排序，否则保持原序。
func sortEpisodesIfPossible(episodes []domain.EpisodeMeta) {
	for index := range episodes {
		if _, ok := ParseEpisodeNum(episodes[index].Episode); !ok {
			return
		}
	}
	sort.SliceStable(episodes, func(i, j int) bool {
		left, _ := ParseEpisodeNum(episodes[i].Episode)
		right, _ := ParseEpisodeNum(episodes[j].Episode)
		return left < right
	})
}

// countPlannedEpisodes 账本现算已规划集数（含全部 ledger_status）。
func countPlannedEpisodes(project *domain.ProjectFile) int {
	if project == nil {
		return 0
	}
	count := 0
	for index := range project.Episodes {
		if _, ok := ParseEpisodeNum(project.Episodes[index].Episode); ok {
			count++
		}
	}
	return count
}

// medianRounded 复刻 Python statistics.median 后 round 的整数结果（偶数个取中间两者均值再四舍五入）。
func medianRounded(sortedValues []int) int {
	n := len(sortedValues)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sortedValues[n/2]
	}
	mid := float64(sortedValues[n/2-1]+sortedValues[n/2]) / 2
	return int(math.Round(mid))
}

// relPaths 抽取源文快照的相对路径序列。
func relPaths(docs []SourceDoc) []string {
	out := make([]string, 0, len(docs))
	for _, doc := range docs {
		out = append(out, doc.RelPath)
	}
	return out
}

func indexOf(values []string, target string) (int, bool) {
	for index, value := range values {
		if value == target {
			return index, true
		}
	}
	return 0, false
}

// safeJoinAllowBase 拼接项目内相对路径（允许等于项目根）。
func safeJoinAllowBase(projectDir, rel string) (string, error) {
	return fs.SafeJoin(projectDir, true, filepath.FromSlash(rel))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
