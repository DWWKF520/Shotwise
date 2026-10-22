// 剧本生成器（对应 lib/script_generator.py 的 narration / drama 两段式 step2 路径）。
//
// 读 step1 结构化中间文件，调用文本生成 Backend 只产视觉层，按 id 对齐合并回 step1 已确认的
// 内容层，补元数据后经写盘统一入口保存。
//
// 迁移边界（fail loud，不静默降级）：ad（brief + 产品信息 + 审定配比表）与 reference_video
// （video_unit 拆分 + 保结构合并）两条路径尚未迁移，Generate / BuildPrompt 显式报错。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/domain"
)

// ScriptGenStore 是剧本生成器对项目与剧本的读写依赖（*ProjectStore 满足）。
type ScriptGenStore interface {
	Get(name string) (*domain.ProjectFile, error)
	Dir(name string) (string, error)
	SaveScript(project, scriptFilename string, script map[string]any) error
	// Update 供 step1 存量迁移在项目锁内平移 / 失效审阅确认记录（*ProjectStore 的同名方法）。
	Update(name string, mutate func(*domain.ProjectFile) error) error
}

// ScriptGenerator 是剧本生成器。
type ScriptGenerator struct {
	store         ScriptGenStore
	projectName   string
	dir           string
	pf            *domain.ProjectFile
	gen           TextCompletion
	supportedDurs []int
	// refCtx 是参考路径 step2 所需的运行时能力上下文（WithReferenceContext 注入）；非参考路径为 nil。
	refCtx *ScriptReferenceContext
}

// ScriptGenOption 是 ScriptGenerator 的可选配置项。
type ScriptGenOption func(*ScriptGenerator)

// NewScriptGenerator 构造剧本生成器；supportedDurations 是本项目当前生效的视频时长档位。
func NewScriptGenerator(
	store ScriptGenStore, projectName string, gen TextCompletion, supportedDurations []int, opts ...ScriptGenOption,
) (*ScriptGenerator, error) {
	dir, err := store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	pf, err := store.Get(projectName)
	if err != nil {
		return nil, err
	}
	g := &ScriptGenerator{
		store: store, projectName: projectName, dir: dir, pf: pf,
		gen: gen, supportedDurs: normalizeDurations(supportedDurations),
	}
	for _, opt := range opts {
		opt(g)
	}
	return g, nil
}

// requireReferenceContext 返回参考路径的能力上下文；缺失时 fail loud。
func (g *ScriptGenerator) requireReferenceContext() (*ScriptReferenceContext, error) {
	if g.refCtx == nil {
		return nil, fmt.Errorf("参考生视频路径缺少视频能力上下文（参考拆分档位不可解析）")
	}
	return g.refCtx, nil
}

// contentMode 返回项目内容模式（缺省 narration）。
func (g *ScriptGenerator) contentMode() string {
	if g.pf == nil || g.pf.ContentMode == "" {
		return "narration"
	}
	return g.pf.ContentMode
}

// generationMode 返回项目生成路线。
func (g *ScriptGenerator) generationMode() string { return g.pf.GenerationMode }

// aspectRatio 解析项目画面比例：narration / ad 默认竖屏，其余默认横屏。
func (g *ScriptGenerator) aspectRatio() string {
	if g.pf != nil && strings.TrimSpace(g.pf.AspectRatio) != "" {
		return g.pf.AspectRatio
	}
	if g.contentMode() == "narration" || g.contentMode() == "ad" {
		return "9:16"
	}
	return "16:9"
}

// ErrScriptStep1Missing 表示该集缺少 step1 结构化中间文件。
type ErrScriptStep1Missing struct{ Message string }

func (e *ErrScriptStep1Missing) Error() string { return e.Message }

// BuildPrompt 构建 step2 prompt（dry-run）。
func (g *ScriptGenerator) BuildPrompt(episode int, instructions string) (string, error) {
	contentMode := g.contentMode()
	genMode := g.generationMode()
	if contentMode == "ad" {
		prompt, _, err := g.composeAD(episode, genMode)
		if err != nil {
			return "", err
		}
		return AppendUserInstructions(prompt, instructions), nil
	}
	if genMode != string(domain.GenerationReferenceVideo) && contentMode != "narration" {
		content, err := g.loadDramaStep1(episode)
		if err != nil {
			return "", err
		}
		scenes, _ := content["scenes"].([]any)
		prompt, err := g.buildDramaPrompt(scenes, episode)
		if err != nil {
			return "", err
		}
		return AppendUserInstructions(prompt, instructions), nil
	}
	if genMode == string(domain.GenerationReferenceVideo) {
		rc, err := g.requireReferenceContext()
		if err != nil {
			return "", err
		}
		units, err := g.loadReferenceStep1(episode, rc.Caps.RawDurations)
		if err != nil {
			return "", err
		}
		prompt, err := g.buildReferencePrompt(units, rc, episode)
		if err != nil {
			return "", err
		}
		return AppendUserInstructions(prompt, instructions), nil
	}
	segments, err := g.loadNarrationStep1(episode)
	if err != nil {
		return "", err
	}
	prompt, err := g.buildNarrationPrompt(segments, episode)
	if err != nil {
		return "", err
	}
	return AppendUserInstructions(prompt, instructions), nil
}

// Generate 生成并落盘该集剧本，返回剧本路径。
func (g *ScriptGenerator) Generate(ctx context.Context, episode int, instructions string) (string, error) {
	contentMode := g.contentMode()
	genMode := g.generationMode()
	if contentMode == "ad" {
		return g.generateAD(ctx, episode, instructions)
	}
	if genMode != string(domain.GenerationReferenceVideo) && contentMode != "narration" {
		return g.generateDramaStep2(ctx, episode, instructions)
	}
	if genMode == string(domain.GenerationReferenceVideo) {
		return g.generateReferenceStep2(ctx, episode, instructions)
	}
	return g.generateNarrationStep2(ctx, episode, instructions)
}

// buildDramaPrompt 构建 drama step2（视觉层）prompt。
func (g *ScriptGenerator) buildDramaPrompt(contentScenes []any, episode int) (string, error) {
	return BuildDramaPrompt(DramaPromptOptions{
		ProjectOverview:  g.pf.Overview,
		Style:            g.pf.Style,
		StyleDescription: "",
		ScenesContent:    RenderDramaContentForStep2(contentScenes),
		Episode:          episode,
		AspectRatio:      g.aspectRatio(),
		TargetLanguage:   targetLanguageOf(g.pf),
		Characters:       &g.pf.Characters,
		Scenes:           &g.pf.Scenes,
		Props:            &g.pf.Props,
	})
}

// buildNarrationPrompt 构建 narration step2（视觉层）prompt。
func (g *ScriptGenerator) buildNarrationPrompt(segments []map[string]any, episode int) (string, error) {
	return BuildNarrationPrompt(NarrationPromptOptions{
		ProjectOverview:  g.pf.Overview,
		Style:            g.pf.Style,
		StyleDescription: "",
		Characters:       &g.pf.Characters,
		Scenes:           &g.pf.Scenes,
		Props:            &g.pf.Props,
		Step1Segments:    segments,
		Episode:          episode,
		AspectRatio:      g.aspectRatio(),
		TargetLanguage:   targetLanguageOf(g.pf),
	})
}

// generateDramaStep2 是 drama 两段式 step2。
func (g *ScriptGenerator) generateDramaStep2(ctx context.Context, episode int, instructions string) (string, error) {
	if g.gen == nil {
		return "", fmt.Errorf("文本生成器未初始化")
	}
	content, err := g.loadDramaStep1(episode)
	if err != nil {
		return "", err
	}
	contentScenesRaw, _ := content["scenes"].([]any)
	if err := g.assertDramaStep1Durations(contentScenesRaw); err != nil {
		return "", err
	}
	prompt, err := g.buildDramaPrompt(contentScenesRaw, episode)
	if err != nil {
		return "", err
	}
	prompt = AppendUserInstructions(prompt, instructions)

	response, err := g.gen.Generate(ctx, prompt, DramaVisualScriptSchema(), defaultMaxOutputTokens)
	if err != nil {
		return "", err
	}
	visualScenes, err := ParseDramaVisual(response)
	if err != nil {
		return "", err
	}
	contentScenes := mapsOnly(contentScenesRaw)
	merged, err := MergeDramaVisualIntoScenes(contentScenes, visualScenes)
	if err != nil {
		return "", err
	}
	title, _ := content["title"].(string)
	if title == "" {
		title = fmt.Sprintf("第%d集", episode)
	}
	script := map[string]any{"title": title, "scenes": merged}
	return g.finish(script, episode, nil, nil)
}

// generateNarrationStep2 是 narration 两段式 step2。
func (g *ScriptGenerator) generateNarrationStep2(ctx context.Context, episode int, instructions string) (string, error) {
	if g.gen == nil {
		return "", fmt.Errorf("文本生成器未初始化")
	}
	segments, err := g.loadNarrationStep1(episode)
	if err != nil {
		return "", err
	}
	prompt, err := g.buildNarrationPrompt(segments, episode)
	if err != nil {
		return "", err
	}
	prompt = AppendUserInstructions(prompt, instructions)

	response, err := g.gen.Generate(ctx, prompt, NarrationVisualEpisodeScriptSchema(), defaultMaxOutputTokens)
	if err != nil {
		return "", err
	}
	visual, err := ParseNarrationVisual(response)
	if err != nil {
		return "", err
	}
	script, err := MergeNarrationVisual(segments, visual, episode)
	if err != nil {
		return "", err
	}
	return g.finish(script, episode, nil, nil)
}

// finish 补元数据并经写盘统一入口保存，返回剧本路径。
func (g *ScriptGenerator) finish(
	script map[string]any, episode int, referenceUnitDurations map[string]int, rc *ScriptReferenceContext,
) (string, error) {
	if err := g.addMetadata(script, episode, referenceUnitDurations, rc); err != nil {
		return "", err
	}
	filename := EpisodeScriptFilename(episode)
	if err := g.store.SaveScript(g.projectName, filename, script); err != nil {
		return "", err
	}
	return filepath.Join(g.dir, "scripts", filename), nil
}

// addMetadata 补充剧本元数据（集号、id 前缀改写、钩子/预告、novel、时间戳、统计）。
//
// referenceUnitDurations 非 nil 时（参考路径）按 unit_id 机械覆盖 LLM 输出的 unit 时长——
// step1 确认的原始值，取档按落地后的最终 references 状态逐 unit 重算，越档抛 DraftViolation。
func (g *ScriptGenerator) addMetadata(
	script map[string]any, episode int, referenceUnitDurations map[string]int, rc *ScriptReferenceContext,
) error {
	script["episode"] = episode
	kind, err := domain.ResolveDeclaredKind(g.contentMode(), g.generationMode())
	if err != nil {
		return err
	}
	skeleton := domain.ScriptSkeletons[kind]
	idField := skeleton.IDField

	items, _ := script[kind].([]any)
	var rewrittenOutputIDs []string
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := entry[idField]; ok {
			rewritten := rewriteEpisodePrefix(id, episode)
			entry[idField] = rewritten
			if referenceUnitDurations != nil {
				rewrittenOutputIDs = append(rewrittenOutputIDs, fmt.Sprint(rewritten))
			}
		}
	}

	if referenceUnitDurations != nil {
		if err := g.overrideReferenceUnitDurations(script, kind, idField, referenceUnitDurations, rewrittenOutputIDs, rc); err != nil {
			return err
		}
	}

	if g.contentMode() != "ad" {
		entry := FindEpisodeEntry(g.pf, episode)
		if entry != nil {
			script["hook"] = nilIfEmptyString(entry.Hook)
			teaser := ""
			if entry.Outline != nil {
				teaser = entry.Outline.NextEpisodeTeaser
			}
			script["next_episode_teaser"] = nilIfEmptyString(teaser)
		} else {
			script["hook"] = nil
			script["next_episode_teaser"] = nil
		}
	}

	if _, ok := script["content_mode"].(string); !ok {
		script["content_mode"] = g.contentMode()
	}

	novel, ok := script["novel"].(map[string]any)
	if !ok || stringValue(novel["title"], "") == "" || stringValue(novel["chapter"], "") == "" {
		script["novel"] = map[string]any{"title": g.pf.Title, "chapter": fmt.Sprintf("第%d集", episode)}
	} else {
		delete(novel, "source_file")
	}
	delete(script, "generation_mode")

	metadata, _ := script["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
		script["metadata"] = metadata
	}
	now := time.Now().UTC().Format(time.RFC3339)
	metadata["created_at"] = now
	metadata["updated_at"] = now
	metadata["generator"] = "shotwise-go"
	items, _ = script[kind].([]any)
	metadata[metadataCountKey(kind)] = len(items)
	script["duration_seconds"] = domain.ScriptDurationTotal(kind, items)
	delete(script, "characters_in_episode")
	delete(script, "clues_in_episode")
	return nil
}

// loadNarrationStep1 加载并校验 narration step1 结构化片段。
func (g *ScriptGenerator) loadNarrationStep1(episode int) ([]map[string]any, error) {
	drafts := EpisodeDraftsDir(g.dir, episode)
	step1Name, _ := Step1Filename("narration")
	step1Path := filepath.Join(drafts, step1Name)
	if _, err := os.Stat(step1Path); err != nil {
		if legacy, ok := legacyStep1Path(drafts, "narration"); ok {
			return nil, &ErrScriptStep1Missing{Message: fmt.Sprintf(
				"仅找到结构化前的旧拆分表 %s，未找到 %s；请重跑 split_narration_segments 产出结构化 %s",
				legacy, step1Path, step1Name)}
		}
		return nil, &ErrScriptStep1Missing{Message: fmt.Sprintf(
			"未找到 Step 1 中间文件: %s；content_mode=narration 期望该文件，请先完成片段拆分", step1Path)}
	}
	root, err := readJSONObjectFile(step1Path)
	if err != nil {
		return nil, err
	}
	rawSegments, ok := root["segments"].([]any)
	if !ok || len(rawSegments) == 0 {
		return nil, fmt.Errorf("step1_segments.json segments 为空")
	}
	segments := make([]map[string]any, 0, len(rawSegments))
	for index, item := range rawSegments {
		segment, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("step1_segments.json 结构校验失败：segments.%d 必须是对象", index)
		}
		segments = append(segments, normalizeNarrationSegmentForLoad(segment))
	}
	if err := ValidateNarrationSegments(segments, g.supportedDurs); err != nil {
		return nil, err
	}
	// 改写后 id 重复的提前拦截（与 narration 拆分侧同口径）。
	rewritten := make([]string, 0, len(segments))
	for _, segment := range segments {
		rewritten = append(rewritten, fmt.Sprint(rewriteEpisodePrefix(segment["segment_id"], episode)))
	}
	if dupes := duplicateStrings(rewritten); len(dupes) > 0 {
		return nil, fmt.Errorf("step1_segments.json segment_id 改写到 episode=%d 后重复: %v", episode, dupes)
	}
	return segments, nil
}

// loadDramaStep1 加载并校验 drama step1 结构化内容（step1_normalized_script.json）。
func (g *ScriptGenerator) loadDramaStep1(episode int) (map[string]any, error) {
	drafts := EpisodeDraftsDir(g.dir, episode)
	step1Name, _ := Step1Filename("drama")
	step1Path := filepath.Join(drafts, step1Name)
	if _, err := os.Stat(step1Path); err != nil {
		return nil, &ErrScriptStep1Missing{Message: fmt.Sprintf(
			"未找到 Step 1 中间文件: %s；content_mode=drama 期望该文件，请先完成剧本规范化", step1Path)}
	}
	data, err := readJSONObjectFile(step1Path)
	if err != nil {
		return nil, err
	}
	scenes, ok := data["scenes"].([]any)
	if !ok || len(scenes) == 0 {
		return nil, fmt.Errorf("Step 1 内容文件结构异常：scenes 必须是非空的场景对象数组")
	}
	sceneIDs := make([]string, 0, len(scenes))
	for index, scene := range scenes {
		entry, ok := scene.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Step 1 内容文件结构异常：scenes[%d] 必须是场景对象", index)
		}
		sid, _ := entry["scene_id"].(string)
		if sid == "" {
			return nil, fmt.Errorf("Step 1 内容文件结构异常：scenes[%d].scene_id 必须是非空字符串", index)
		}
		sceneIDs = append(sceneIDs, sid)
	}
	rewritten := make([]string, 0, len(sceneIDs))
	for _, sid := range sceneIDs {
		rewritten = append(rewritten, fmt.Sprint(rewriteEpisodePrefix(sid, episode)))
	}
	if dupes := duplicateStrings(rewritten); len(dupes) > 0 {
		return nil, fmt.Errorf("Step 1 内容文件 scene_id 改写到 episode=%d 后重复: %v", episode, dupes)
	}
	return data, nil
}

// assertDramaStep1Durations 校验 drama step1 已定场景时长在当前能力集合内。
func (g *ScriptGenerator) assertDramaStep1Durations(contentScenes []any) error {
	allowed := map[int]bool{}
	for _, d := range g.supportedDurs {
		allowed[d] = true
	}
	seen := map[int]bool{}
	for _, raw := range contentScenes {
		scene, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		duration, ok := intValueOK(scene["duration_seconds"])
		if !ok {
			duration = 8
		}
		seen[duration] = true
	}
	var bad []int
	for d := range seen {
		if !allowed[d] {
			bad = append(bad, d)
		}
	}
	if len(bad) > 0 {
		bad = uniqueSortedInts(bad)
		return fmt.Errorf("step1 已定场景时长非法（不在 %v 内）: %v；当前分辨率与型号下这些时长不可用，请重跑 normalize_drama_script 按当前能力规范化",
			normalizeDurations(g.supportedDurs), bad)
	}
	return nil
}

// --- 小工具 ---

// episodeIDPrefixRe 匹配 id 起始的 E{数字} 前缀后紧跟 S / U（保留后缀）。
var episodeIDPrefixRe = regexp.MustCompile(`^(E[0-9]+)([SU])`)

// rewriteEpisodePrefix 把 id 中的 E\d+ 前缀强制改写为 E{ep}；非字符串或无前缀原样返回。
func rewriteEpisodePrefix(id any, episode int) any {
	s, ok := id.(string)
	if !ok {
		return id
	}
	return episodeIDPrefixRe.ReplaceAllString(s, "E"+strconv.Itoa(episode)+"$2")
}

// normalizeNarrationSegmentForLoad 补齐 narration 片段默认字段（segment_break / scenes / props）。
func normalizeNarrationSegmentForLoad(segment map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range segment {
		out[k] = v
	}
	if _, ok := out["segment_break"].(bool); !ok {
		out["segment_break"] = false
	}
	if _, ok := out["scenes"].([]any); !ok {
		out["scenes"] = []any{}
	}
	if _, ok := out["props"].([]any); !ok {
		out["props"] = []any{}
	}
	if _, ok := out["characters_in_segment"].([]any); !ok {
		out["characters_in_segment"] = []any{}
	}
	if _, ok := out["duration_seconds"]; !ok {
		out["duration_seconds"] = 0
	}
	return out
}

// legacyStep1Path 返回该内容模式的旧版 step1 文件名（存在时）。
func legacyStep1Path(drafts, contentMode string) (string, bool) {
	candidates := Step1ReadCandidates(contentMode)
	for _, name := range candidates[1:] {
		path := filepath.Join(drafts, name)
		if _, err := os.Stat(path); err == nil {
			return path, true
		}
	}
	return "", false
}

// mapsOnly 过滤出 map 元素（脏数据条目忽略）。
func mapsOnly(items []any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// metadataCountKey 返回骨架种类对应的 metadata 计数键名。
func metadataCountKey(kind string) string {
	switch kind {
	case "segments":
		return "total_segments"
	case "shots":
		return "total_shots"
	case "video_units":
		return "total_units"
	default:
		return "total_scenes"
	}
}

// targetLanguageOf 取项目输出语言：source_language 为唯一真相源，缺省回退中文。
func targetLanguageOf(pf *domain.ProjectFile) string {
	if pf != nil && strings.TrimSpace(pf.SourceLanguage) != "" {
		return pf.SourceLanguage
	}
	return "中文"
}

func nilIfEmptyString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// readJSONObjectFile 读 JSON 文件并要求顶层为对象。
func readJSONObjectFile(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, fmt.Errorf("%s 顶层应为对象", path)
	}
	return data, nil
}
