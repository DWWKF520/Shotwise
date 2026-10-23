// 读时状态计算（对应 lib/status_calculator.py）。
//
// 项目详情与列表里的 status / 每集统计都是读时派生，不写回 project.json：剧本、资产
// 与 step1 草稿随时可能被生成链路改动，存冗余必然与磁盘真相漂移。
//
// 全部入参/出参用 map[string]any 直通磁盘 JSON：Python 侧就是在原始 dict 上就地注入
// 计算字段，Go 侧保持同一形态才能保证「响应里多出来的键」与 Python 逐字段一致。
package script

import (
	"github.com/shotwise/shotwise/internal/service"
	"os"
	"path/filepath"
	"sort"

	"github.com/shotwise/shotwise/internal/domain/media"
	domainproject "github.com/shotwise/shotwise/internal/domain/project"
	domainscript "github.com/shotwise/shotwise/internal/domain/script"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// CountPair 是 {total, completed} 形式的计数对。
type CountPair struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
}

// EpisodeStatus 是单集统计（读时注入 episode 对象）。
//
// ScriptStatus: generated | segmented | none
// Status:       draft | scripted | in_production | completed
type EpisodeStatus struct {
	ScriptStatus    string    `json:"script_status"`
	Status          string    `json:"status"`
	Storyboards     CountPair `json:"storyboards"`
	Videos          CountPair `json:"videos"`
	ScenesCount     int       `json:"scenes_count"`
	DurationSeconds int       `json:"duration_seconds"`
	UnitsCount      *int      `json:"units_count,omitempty"`
}

// EpisodeSummary 是项目级分集汇总。
type EpisodeSummary struct {
	Total        int `json:"total"`
	Scripted     int `json:"scripted"`
	InProduction int `json:"in_production"`
	Completed    int `json:"completed"`
}

// ProjectStatus 是项目级状态（列表与详情共用）。
type ProjectStatus struct {
	CurrentPhase    string         `json:"current_phase"`
	PhaseProgress   float64        `json:"phase_progress"`
	Characters      CountPair      `json:"characters"`
	Scenes          CountPair      `json:"scenes"`
	Props           CountPair      `json:"props"`
	EpisodesSummary EpisodeSummary `json:"episodes_summary"`
}

// StatusCalculator 读取项目磁盘真相并派生统计字段。
type StatusCalculator struct {
	store *service.ProjectStore
}

// NewStatusCalculator 构造计算器。
func NewStatusCalculator(store *service.ProjectStore) *StatusCalculator {
	return &StatusCalculator{store: store}
}

// legacyDuckTypeKinds 是缺 content_mode 声明时按主结构探测的兜底顺序（video_units 不参与）。
var legacyDuckTypeKinds = []string{"segments", "scenes", "shots"}

// SelectKindAndItems 返回 (骨架种类, 条目数组)。
//
// 主路径按剧本级 content_mode 与项目级 generation_mode 走 domainscript.ResolveDeclaredKind；
// 缺失/未知 content_mode 时保留历史容忍：参考路线按 video_units 短路，否则按主结构探测。
func SelectKindAndItems(script map[string]any, generationMode string) (string, []any) {
	contentMode, _ := script["content_mode"].(string)
	kind, err := domainscript.ResolveDeclaredKind(contentMode, generationMode)
	if err != nil {
		kind = ""
	}

	if kind == "video_units" {
		return "video_units", unitItems(script)
	}
	if kind != "" {
		if items, ok := mapItems(script[kind]); ok {
			return kind, items
		}
	} else if generationMode == string(domainproject.GenerationReferenceVideo) {
		// 以 video_units 键在场为前提（与 ensure_route_skeleton 同判据）：ad 剧本恒为 shots
		// 骨架却可能落在参考路线项目下，无条件短路会把它的计分抢成空 video_units。
		if _, present := script["video_units"]; present {
			return "video_units", unitItems(script)
		}
	}

	for _, legacyKind := range legacyDuckTypeKinds {
		if items, ok := mapItems(script[legacyKind]); ok {
			return legacyKind, items
		}
	}
	if kind == "" {
		kind = "segments"
	}
	return kind, nil
}

// unitItems 取 video_units 数组，容器非数组、成员非对象一律剔除（读时不抛错）。
func unitItems(script map[string]any) []any {
	raw, ok := script["video_units"].([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(raw))
	for _, item := range raw {
		if _, ok := item.(map[string]any); ok {
			out = append(out, item)
		}
	}
	return out
}

// mapItems 把 JSON 数组归一为 []any；非数组返回 ok=false。
func mapItems(raw any) ([]any, bool) {
	items, ok := raw.([]any)
	return items, ok
}

// CalculateEpisodeStats 按骨架种类计算单集统计。
func (c *StatusCalculator) CalculateEpisodeStats(script map[string]any, generationMode string) EpisodeStatus {
	kind, items := SelectKindAndItems(script, generationMode)

	if kind == "video_units" {
		return referenceVideoStats(items)
	}
	if kind == "shots" && generationMode == string(domainproject.GenerationReferenceVideo) {
		return adReferenceStats(script, items)
	}

	storyboardDone, videoDone := 0, 0
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		assets := media.GeneratedAssets(m)
		if service.PyTruthy(assets["storyboard_image"]) {
			storyboardDone++
		}
		if service.PyTruthy(assets["video_clip"]) {
			videoDone++
		}
	}
	total := len(items)
	status := "draft"
	switch {
	case total > 0 && videoDone == total:
		status = "completed"
	case storyboardDone > 0 || videoDone > 0:
		status = "in_production"
	}
	return EpisodeStatus{
		Status:          status,
		Storyboards:     CountPair{Total: total, Completed: storyboardDone},
		Videos:          CountPair{Total: total, Completed: videoDone},
		ScenesCount:     total,
		DurationSeconds: domainscript.ScriptDurationTotal(kind, items),
	}
}

// adReferenceStats 是 ad + 参考路线：视频进度按派生 unit 计，其余口径仍以 shots 为真相。
func adReferenceStats(script map[string]any, shots []any) EpisodeStatus {
	rawUnits, present := script["reference_units"]
	units := []any{}
	if present && rawUnits != nil {
		arr, ok := rawUnits.([]any)
		wellformed := ok
		if ok {
			for _, item := range arr {
				m, isMap := item.(map[string]any)
				if !isMap {
					wellformed = false
					break
				}
				if ga, hasGA := m["generated_assets"]; hasGA && ga != nil {
					if _, gaIsMap := ga.(map[string]any); !gaIsMap {
						wellformed = false
						break
					}
				}
			}
		}
		if wellformed {
			units = arr
		}
	}

	videoDone := 0
	for _, item := range units {
		if m, ok := item.(map[string]any); ok && service.PyTruthy(media.GeneratedAssets(m)["video_clip"]) {
			videoDone++
		}
	}
	totalUnits := len(units)
	status := "draft"
	switch {
	case totalUnits == 0:
		status = "draft"
	case videoDone == totalUnits:
		status = "completed"
	case videoDone > 0:
		status = "in_production"
	}

	totalShots := len(shots)
	unitCount := totalUnits
	return EpisodeStatus{
		Status:          status,
		Storyboards:     CountPair{Total: totalShots, Completed: 0},
		Videos:          CountPair{Total: totalUnits, Completed: videoDone},
		ScenesCount:     totalShots,
		DurationSeconds: domainscript.ScriptDurationTotal("shots", shots),
		UnitsCount:      &unitCount,
	}
}

// referenceVideoStats 按 video_units[].generated_assets.video_clip 计分。
func referenceVideoStats(units []any) EpisodeStatus {
	videoDone := 0
	for _, item := range units {
		if m, ok := item.(map[string]any); ok && service.PyTruthy(media.GeneratedAssets(m)["video_clip"]) {
			videoDone++
		}
	}
	total := len(units)
	status := "draft"
	switch {
	case total == 0:
		status = "draft"
	case videoDone == total:
		status = "completed"
	case videoDone > 0:
		status = "in_production"
	}
	unitCount := total
	return EpisodeStatus{
		Status:          status,
		Storyboards:     CountPair{Total: total, Completed: 0},
		Videos:          CountPair{Total: total, Completed: videoDone},
		ScenesCount:     total,
		DurationSeconds: domainscript.ScriptDurationTotal("video_units", units),
		UnitsCount:      &unitCount,
	}
}

// DraftCandidates 返回该 (content_mode, generation_mode) 下「已分段」可探测的 step1 草稿文件名。
func DraftCandidates(contentMode, generationMode string) []string {
	if contentMode == string(domainproject.ContentAd) {
		return nil
	}
	if generationMode == string(domainproject.GenerationReferenceVideo) {
		return []string{service.ReferenceVideoStep1Filename, service.ReferenceVideoStep1QuarantineName}
	}
	primary, ok := Step1Filename(contentMode)
	if !ok {
		primary = step1Filenames["drama"]
	}
	out := []string{primary}
	// 旧版 .md 仅 narration 视为有效 step1（drama 的 .md 早于内容抽取前移）。
	if contentMode == string(domainproject.ContentNarration) {
		out = append(out, step1LegacyFilenames[string(domainproject.ContentNarration)]...)
	}
	return out
}

// buildEpisodesStats 遍历分集，加载剧本并计算每集统计。
//
// preloadedScripts 以 episode.script_file 原值为键，命中即跳过磁盘读取。
func (c *StatusCalculator) buildEpisodesStats(
	projectName string, project map[string]any, preloadedScripts map[string]map[string]any,
) []EpisodeStatus {
	contentMode, _ := project["content_mode"].(string)
	if contentMode == "" {
		contentMode = string(domainproject.ContentNarration)
	}
	generationMode, _ := project["generation_mode"].(string)
	episodes, _ := project["episodes"].([]any)

	stats := make([]EpisodeStatus, 0, len(episodes))
	for _, raw := range episodes {
		ep, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// 账本标 stale 的集：读时状态回退为待预处理，驱动重做流程。
		if ledgerStatus, _ := ep["ledger_status"].(string); ledgerStatus == domainproject.LedgerStatusStale {
			stats = append(stats, fallbackEpisodeStats("none"))
			continue
		}

		scriptFile, _ := ep["script_file"].(string)
		if scriptFile == "" {
			stats = append(stats, fallbackEpisodeStats("none"))
			continue
		}

		scriptStatus, script := c.loadEpisodeScript(projectName, ep, scriptFile, contentMode, generationMode, preloadedScripts)
		if scriptStatus == "generated" && script != nil {
			epStats := c.CalculateEpisodeStats(script, generationMode)
			if epStats.Status == "draft" {
				epStats.Status = "scripted"
			}
			epStats.ScriptStatus = "generated"
			stats = append(stats, epStats)
			continue
		}
		stats = append(stats, fallbackEpisodeStats(scriptStatus))
	}
	return stats
}

// loadEpisodeScript 返回 (script_status, script)。
//
// script_status: generated（剧本存在/损坏）| segmented（仅有 step1 草稿）| none。
func (c *StatusCalculator) loadEpisodeScript(
	projectName string, ep map[string]any, scriptFile, contentMode, generationMode string,
	preloadedScripts map[string]map[string]any,
) (string, map[string]any) {
	if preloadedScripts != nil {
		if script, ok := preloadedScripts[scriptFile]; ok {
			return "generated", script
		}
	}
	script, err := c.store.ReadScriptJSON(projectName, scriptFile)
	if err == nil {
		return "generated", script
	}
	if !os.IsNotExist(unwrapPathError(err)) {
		// 剧本 JSON 损坏/路径非法：按已生成但无内容处理（不阻断项目详情）。
		return "generated", nil
	}

	episodeNum, ok := intValue(ep["episode"])
	if !ok {
		return "none", nil
	}
	candidates := DraftCandidates(contentMode, generationMode)
	if len(candidates) == 0 {
		return "none", nil
	}
	projectDir, dirErr := c.store.Dir(projectName)
	if dirErr != nil {
		return "none", nil
	}
	draftsDir := service.EpisodeDraftsDir(projectDir, episodeNum)
	for _, name := range candidates {
		if _, statErr := os.Stat(filepath.Join(draftsDir, name)); statErr == nil {
			return "segmented", nil
		}
	}
	return "none", nil
}

// CalculateProjectStatus 计算项目整体状态。preloadedStats 非 nil 时直接复用，避免重复加载剧本。
func (c *StatusCalculator) CalculateProjectStatus(
	projectName string, project map[string]any,
	preloadedStats []EpisodeStatus, preloadedScripts map[string]map[string]any,
) ProjectStatus {
	projectDir, _ := c.store.Dir(projectName)

	charsTotal, charsDone := bucketCounts(project["characters"], projectDir, "character_sheet")
	scenesTotal, scenesDone := bucketCounts(project["scenes"], projectDir, "scene_sheet")
	propsTotal, propsDone := bucketCounts(project["props"], projectDir, "prop_sheet")

	stats := preloadedStats
	if stats == nil {
		stats = c.buildEpisodesStats(projectName, project, preloadedScripts)
	}

	phase := calculateCurrentPhase(project, stats, charsDone+scenesDone+propsDone)
	progress := calculatePhaseProgress(phase, stats)
	if phase == "worldbuilding" {
		totalAssets := charsTotal + scenesTotal + propsTotal
		completedAssets := charsDone + scenesDone + propsDone
		if totalAssets > 0 {
			progress = float64(completedAssets) / float64(totalAssets)
		}
	}

	summary := EpisodeSummary{Total: len(stats)}
	for _, s := range stats {
		if s.ScriptStatus == "generated" {
			summary.Scripted++
		}
		if s.Status == "in_production" {
			summary.InProduction++
		}
		if s.Status == "completed" {
			summary.Completed++
		}
	}

	return ProjectStatus{
		CurrentPhase:    phase,
		PhaseProgress:   progress,
		Characters:      CountPair{Total: charsTotal, Completed: charsDone},
		Scenes:          CountPair{Total: scenesTotal, Completed: scenesDone},
		Props:           CountPair{Total: propsTotal, Completed: propsDone},
		EpisodesSummary: summary,
	}
}

// EnrichProject 把每集统计注入 episode 对象，并写入 project["status"]（就地修改）。
func (c *StatusCalculator) EnrichProject(projectName string, project map[string]any) map[string]any {
	stats := c.buildEpisodesStats(projectName, project, nil)

	if episodes, ok := project["episodes"].([]any); ok {
		for i, raw := range episodes {
			if i >= len(stats) {
				break
			}
			ep, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			ep["script_status"] = stats[i].ScriptStatus
			ep["status"] = stats[i].Status
			ep["storyboards"] = stats[i].Storyboards
			ep["videos"] = stats[i].Videos
			ep["scenes_count"] = stats[i].ScenesCount
			ep["duration_seconds"] = stats[i].DurationSeconds
			if stats[i].UnitsCount != nil {
				ep["units_count"] = *stats[i].UnitsCount
			} else {
				delete(ep, "units_count")
			}
		}
	}

	status := c.CalculateProjectStatus(projectName, project, stats, nil)
	project["status"] = status
	return project
}

// EnrichScript 注入 metadata.total_scenes / estimated_duration_seconds / duration_seconds
// 与 characters_in_episode / scenes_in_episode / props_in_episode（就地修改）。
func (c *StatusCalculator) EnrichScript(script map[string]any, generationMode string) map[string]any {
	kind, items := SelectKindAndItems(script, generationMode)
	totalDuration := domainscript.ScriptDurationTotal(kind, items)

	metadata, ok := script["metadata"].(map[string]any)
	if !ok {
		metadata = map[string]any{}
		script["metadata"] = metadata
	}
	metadata["total_scenes"] = len(items)
	metadata["estimated_duration_seconds"] = totalDuration
	script["duration_seconds"] = totalDuration

	charsSet := map[string]bool{}
	scenesSet := map[string]bool{}
	propsSet := map[string]bool{}
	addName := func(set map[string]bool, value any) {
		if name, ok := value.(string); ok && name != "" {
			set[name] = true
		}
	}

	if kind == "video_units" {
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			refs, ok := m["references"].([]any)
			if !ok {
				continue
			}
			for _, rawRef := range refs {
				ref, ok := rawRef.(map[string]any)
				if !ok {
					continue
				}
				refType, _ := ref["type"].(string)
				switch refType {
				case "character":
					addName(charsSet, ref["name"])
				case "scene":
					addName(scenesSet, ref["name"])
				case "prop":
					addName(propsSet, ref["name"])
				}
			}
		}
	} else {
		skeleton := domainscript.ScriptSkeletons[kind]
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if skeleton.CharsField != "" {
				for _, value := range anyList(m[skeleton.CharsField]) {
					addName(charsSet, value)
				}
			}
			for _, value := range anyList(m["scenes"]) {
				addName(scenesSet, value)
			}
			for _, value := range anyList(m["props"]) {
				addName(propsSet, value)
			}
		}
	}

	script["characters_in_episode"] = sortedKeyList(charsSet)
	script["scenes_in_episode"] = sortedKeyList(scenesSet)
	script["props_in_episode"] = sortedKeyList(propsSet)
	return script
}

// --- 内部辅助 ---

func fallbackEpisodeStats(scriptStatus string) EpisodeStatus {
	return EpisodeStatus{
		ScriptStatus: scriptStatus,
		Status:       "draft",
		Storyboards:  CountPair{},
		Videos:       CountPair{},
	}
}

func calculateCurrentPhase(project map[string]any, stats []EpisodeStatus, assetsCompleted int) string {
	anyGenerated := false
	allGenerated := len(stats) > 0
	anySegmented := false
	allCompleted := len(stats) > 0
	for _, s := range stats {
		if s.ScriptStatus == "generated" {
			anyGenerated = true
		} else {
			allGenerated = false
			if s.ScriptStatus == "segmented" {
				anySegmented = true
			}
		}
		if s.Status != "completed" {
			allCompleted = false
		}
	}

	if allGenerated {
		if allCompleted {
			return "completed"
		}
		return "production"
	}
	if anyGenerated {
		return "scripting"
	}
	if anySegmented || assetsCompleted > 0 || overviewPresent(project) {
		return "worldbuilding"
	}
	return "setup"
}

func calculatePhaseProgress(phase string, stats []EpisodeStatus) float64 {
	switch phase {
	case "setup", "worldbuilding":
		return 0
	case "scripting":
		if len(stats) == 0 {
			return 0
		}
		done := 0
		for _, s := range stats {
			if s.ScriptStatus == "generated" {
				done++
			}
		}
		return float64(done) / float64(len(stats))
	case "production":
		total, done := 0, 0
		for _, s := range stats {
			total += s.Videos.Total
			done += s.Videos.Completed
		}
		if total == 0 {
			return 0
		}
		return float64(done) / float64(total)
	default: // completed
		return 1
	}
}

func overviewPresent(project map[string]any) bool {
	overview, ok := project["overview"].(map[string]any)
	return ok && len(overview) > 0
}

// bucketCounts 统计资产桶的 total / 已完成（sheet 文件存在）。
func bucketCounts(raw any, projectDir, sheetField string) (total, done int) {
	bucket, ok := raw.(map[string]any)
	if !ok {
		return 0, 0
	}
	for _, entryRaw := range bucket {
		entry, ok := entryRaw.(map[string]any)
		if !ok {
			continue
		}
		total++
		if sheet, _ := entry[sheetField].(string); fs.SafeExists(projectDir, sheet) {
			done++
		}
	}
	return total, done
}

func anyList(raw any) []any {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	return items
}

func intValue(raw any) (int, bool) {
	switch v := raw.(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, false
		}
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

// unwrapPathError 取出 PathError 的底层错误，供 os.IsNotExist 判断。
func unwrapPathError(err error) error {
	for {
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		next := unwrapped.Unwrap()
		if next == nil {
			return err
		}
		err = next
	}
}

// sortedKeyList 对 map[string]bool 的键排序输出（与 creative 侧 sortedKeys 同口径）。
func sortedKeyList(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
