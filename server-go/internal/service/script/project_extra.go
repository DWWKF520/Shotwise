// 项目管理扩展端点的服务层（对应 server/routers/projects.py 的 17 个未迁端点中
// 与剧本/分集/概述相关的 10 个）。
//
// 收敛口径：**导入 / 导出 / generate-overview 三块不在本批**（import 是 zip 全项目搬运、
// export 是打包下载 + 短时效 token、generate-overview 需要 LLM 调用，各自单独成批）。
//
// 两条不变式：
//   - 剧本编辑（scenes / shots / segments）整段 RMW 在单一剧本锁内完成，未命中在锁内
//     报错跳过写回——「锁内 raise」是消除「先写一半再回滚」的根；
//   - 分集 title 以剧本顶层 title 为唯一真相源：走「脚本锁 → 项目锁」改剧本并内联同步
//     project.json 镜像，不产生第二真相源。
package script

import (
	"github.com/shotwise/shotwise/internal/service"
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service/reference"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

// SegmentPatchInput 是 narration 分镜段的字段级补丁（nil 表示未提供）。
type SegmentPatchInput struct {
	DurationSeconds *int
	SegmentBreak    *bool
	ImagePrompt     *any
	VideoPrompt     *any
	TransitionToNext *string
	Note            *string
	NoteSet         bool
	CharactersInSegment []string
	Scenes          []string
	Props           []string
}

// OverviewInput 是项目概述的字段级补丁（nil 表示未提供）。
type OverviewInput struct {
	Synopsis     *string
	Genre        *string
	Theme        *string
	WorldSetting *string
}

// ProjectExtraService 承载项目扩展端点。
type ProjectExtraService struct {
	store    *service.ProjectStore
	queue    service.GridQueue
	resolver *config.Resolver
}

// NewProjectExtraService 构造。
func NewProjectExtraService(store *service.ProjectStore, queue service.GridQueue, resolver *config.Resolver) *ProjectExtraService {
	return &ProjectExtraService{store: store, queue: queue, resolver: resolver}
}

// GetScript 读剧本内容。
func (s *ProjectExtraService) GetScript(projectName, scriptFile string) (map[string]any, error) {
	script, err := s.store.ReadScriptJSON(projectName, scriptFile)
	if err != nil {
		return nil, ErrScriptNotFound
	}
	return script, nil
}

// ErrScriptNotFound 表示剧本不存在。
var ErrScriptNotFound = fmt.Errorf("script not found")

// UpdateScriptScene 按 scene_id 更新 drama 剧本的单个场景（字段白名单）。
func (s *ProjectExtraService) UpdateScriptScene(
	projectName, scriptFile, sceneID string, updates map[string]any,
) (map[string]any, error) {
	var matched map[string]any
	err := s.store.UpdateScript(projectName, scriptFile, func(script map[string]any) error {
		rawScenes, _ := script["scenes"].([]any)
		for _, raw := range rawScenes {
			scene, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if service.PythonStrLike(scene["scene_id"]) == sceneID {
				matched = scene
				break
			}
		}
		if matched == nil {
			return ErrSceneNotFound
		}
		applySceneUpdates(matched, updates, sceneUpdatableFields)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return matched, nil
}

// ErrSceneNotFound 表示场景未命中。
var ErrSceneNotFound = fmt.Errorf("scene not found")

// ErrShotNotFound 表示镜头未命中。
var ErrShotNotFound = fmt.Errorf("shot not found")

// ErrSegmentNotFound 表示分镜段未命中。
var ErrSegmentNotFound = fmt.Errorf("segment not found")

// sceneUpdatableFields 是场景 PATCH 白名单。
var sceneUpdatableFields = map[string]bool{
	"duration_seconds": true, "image_prompt": true, "video_prompt": true,
	"characters_in_scene": true, "scenes": true, "props": true,
	"segment_break": true, "utterances": true, "note": true,
}

// applySceneUpdates 把白名单内的字段写回条目（note 允许显式置空，其余 None 视为未提供）。
func applySceneUpdates(item map[string]any, updates map[string]any, whitelist map[string]bool) {
	for key, value := range updates {
		if !whitelist[key] {
			continue
		}
		if value == nil && key != "note" {
			continue
		}
		item[key] = value
	}
}

// shotUpdatableFields 是 ad 镜头 PATCH 白名单（shot_id 与 generated_assets 不可改写）。
var shotUpdatableFields = map[string]bool{
	"section": true, "voiceover_text": true, "duration_seconds": true,
	"image_prompt": true, "video_prompt": true, "characters_in_shot": true,
	"scenes": true, "props": true, "products_in_shot": true,
	"transition_to_next": true, "note": true,
}

// UpdateScriptShot 按 shot_id 更新 ad 剧本的单个镜头。
func (s *ProjectExtraService) UpdateScriptShot(
	projectName, scriptFile, shotID string, updates map[string]any,
) (map[string]any, error) {
	var matched map[string]any
	err := s.store.UpdateScript(projectName, scriptFile, func(script map[string]any) error {
		shots, err := requireADScript(script)
		if err != nil {
			return err
		}
		for _, shot := range shots {
			if service.PythonStrLike(shot["shot_id"]) == shotID {
				matched = shot
				break
			}
		}
		if matched == nil {
			return ErrShotNotFound
		}
		applySceneUpdates(matched, updates, shotUpdatableFields)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return matched, nil
}

// ReorderScriptShots 按给定全排列重排 ad 剧本的 shots。
func (s *ProjectExtraService) ReorderScriptShots(
	projectName, scriptFile string, shotIDs []string,
) ([]map[string]any, error) {
	reordered := []map[string]any{}
	err := s.store.UpdateScript(projectName, scriptFile, func(script map[string]any) error {
		shots, err := requireADScript(script)
		if err != nil {
			return err
		}
		existing := make([]string, 0, len(shots))
		for _, shot := range shots {
			existing = append(existing, service.PythonStrLike(shot["shot_id"]))
		}
		// 全排列校验失败在锁内报错跳过写回，损坏的 shots 不会被空列表覆盖。
		switch reference.FullPermutationError(existing, shotIDs) {
		case "length":
			return &service.ValueError{Msg: "shot_ids_length_mismatch"}
		case "duplicate":
			return &service.ValueError{Msg: "duplicate_shot_ids"}
		case "mismatch":
			return &service.ValueError{Msg: "shot_ids_mismatch"}
		}
		byID := map[string]map[string]any{}
		for _, shot := range shots {
			byID[service.PythonStrLike(shot["shot_id"])] = shot
		}
		for _, id := range shotIDs {
			reordered = append(reordered, byID[id])
		}
		script["shots"] = reordered
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reordered, nil
}

// requireADScript 断言剧本是 ad 形状并返回 shots 列表。
//
// 非法形状 fail loud：静默降级为空列表会让 reorder 在空 shot_ids 时把损坏的 shots
// 覆盖成 []，直接丢数据。shot_id 是单镜头身份键，重复值会让 PATCH 静默更新首个命中项。
func requireADScript(script map[string]any) ([]map[string]any, error) {
	if mode, _ := script["content_mode"].(string); mode != string(domain.ContentAd) {
		return nil, &service.ValueError{Msg: "ad_mode_required"}
	}
	if _, present := script["shots"]; !present {
		return nil, &service.ValueError{Msg: "ad_mode_required"}
	}
	rawShots, ok := script["shots"].([]any)
	if !ok {
		return nil, &service.ValueError{Msg: "ad script field 'shots' must be a list"}
	}
	shots := make([]map[string]any, 0, len(rawShots))
	for _, raw := range rawShots {
		shot, ok := raw.(map[string]any)
		if !ok {
			return nil, &service.ValueError{Msg: "ad script field 'shots' contains non-object elements"}
		}
		if id, _ := shot["shot_id"].(string); id == "" {
			return nil, &service.ValueError{Msg: "ad script field 'shots' contains elements missing valid 'shot_id'"}
		}
		shots = append(shots, shot)
	}
	seen := map[string]bool{}
	for _, shot := range shots {
		id := service.PythonStrLike(shot["shot_id"])
		if seen[id] {
			return nil, &service.ValueError{Msg: "ad script field 'shots' contains duplicate 'shot_id' values"}
		}
		seen[id] = true
	}
	return shots, nil
}

// UpdateScriptSegment 按 segment_id 更新 narration 剧本的单个分镜段。
func (s *ProjectExtraService) UpdateScriptSegment(
	projectName, scriptFile, segmentID string, in SegmentPatchInput,
) (map[string]any, error) {
	var matched map[string]any
	err := s.store.UpdateScript(projectName, scriptFile, func(script map[string]any) error {
		// 模式守卫：仅 narration 且含 segments 才放行；drama 即使残留 segments 也拒绝。
		if mode, _ := script["content_mode"].(string); mode != string(domain.ContentNarration) {
			return &service.ValueError{Msg: "narration_mode_required"}
		}
		if _, present := script["segments"]; !present {
			return &service.ValueError{Msg: "narration_mode_required"}
		}
		rawSegments, _ := script["segments"].([]any)
		for _, raw := range rawSegments {
			segment, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if service.PythonStrLike(segment["segment_id"]) == segmentID {
				matched = segment
				break
			}
		}
		if matched == nil {
			return ErrSegmentNotFound
		}
		if in.DurationSeconds != nil {
			matched["duration_seconds"] = *in.DurationSeconds
		}
		if in.SegmentBreak != nil {
			matched["segment_break"] = *in.SegmentBreak
		}
		if in.ImagePrompt != nil {
			matched["image_prompt"] = *in.ImagePrompt
		}
		if in.VideoPrompt != nil {
			matched["video_prompt"] = *in.VideoPrompt
		}
		if in.TransitionToNext != nil {
			matched["transition_to_next"] = *in.TransitionToNext
		}
		if in.NoteSet {
			matched["note"] = in.Note
		}
		for field, list := range map[string][]string{
			"characters_in_segment": in.CharactersInSegment,
			"scenes":                in.Scenes,
			"props":                 in.Props,
		} {
			if list != nil {
				matched[field] = toAnySlice(list)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return matched, nil
}

// UpdateEpisodeTitle 更新分集标题（剧本顶层 title 为唯一真相源，镜像随锁内同步）。
func (s *ProjectExtraService) UpdateEpisodeTitle(
	ctx context.Context, projectName string, episode int, title string,
) error {
	return s.store.UpdateEpisodeScript(projectName, episodeResolver(episode), func(script map[string]any) error {
		script["title"] = title
		return nil
	})
}

// DeleteEpisode 删除分集元数据（不删剧本与产物），并取消该集在途任务。
func (s *ProjectExtraService) DeleteEpisode(
	ctx context.Context, projectName string, episode int,
) (map[string]any, error) {
	pf, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	scriptFile := ""
	for _, meta := range pf.Episodes {
		if meta.Episode == episode {
			scriptFile = meta.ScriptFile
			break
		}
	}
	cancelled, cancelling := 0, 0
	if scriptFile != "" {
		// 取消该集在途任务（cancel 已含级联下游），错误不阻断删除。
		if canceller, ok := s.queue.(interface {
			CancelEpisodeTasks(ctx context.Context, projectName, scriptFile string) (repository.CancelResult, error)
		}); ok {
			if result, err := canceller.CancelEpisodeTasks(ctx, projectName, scriptFile); err == nil {
				cancelled = len(result.Cancelled)
				cancelling = len(result.Cancelling)
			}
		}
	}
	err = s.store.Update(projectName, func(project *domain.ProjectFile) error {
		remaining := make([]domain.EpisodeMeta, 0, len(project.Episodes))
		found := false
		for _, meta := range project.Episodes {
			if meta.Episode == episode {
				found = true
				continue
			}
			remaining = append(remaining, meta)
		}
		if !found {
			return ErrEpisodeNotFound
		}
		sort.Slice(remaining, func(i, j int) bool {
			return episodeDisplayKey(remaining[i]) < episodeDisplayKey(remaining[j])
		})
		for index := range remaining {
			value := index + 1
			remaining[index].DisplayEpisode = &value
		}
		project.Episodes = remaining
		// 记入已删除清单：项目导入 / fingerprint 比对据此忽略旧剧本文件。
		deleted := scriptFile
		if deleted == "" {
			deleted = EpisodeScriptRelpath(episode)
		}
		present := false
		for _, name := range project.DeletedEpisodeScriptFiles {
			if name == deleted {
				present = true
				break
			}
		}
		if !present {
			project.DeletedEpisodeScriptFiles = append(project.DeletedEpisodeScriptFiles, deleted)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"success":          true,
		"deleted_episode":  episode,
		"script_file":      scriptFile,
		"cancelled_tasks":  cancelled,
		"cancelling_tasks": cancelling,
	}, nil
}

// ErrEpisodeNotFound 表示分集不存在。
var ErrEpisodeNotFound = fmt.Errorf("episode not found")

// episodeDisplayKey 是分集排序键（display_episode 优先，缺失回退 episode）。
func episodeDisplayKey(meta domain.EpisodeMeta) int {
	if meta.DisplayEpisode != nil {
		return *meta.DisplayEpisode
	}
	return meta.Episode
}

// UpdateEpisodeDisplayNumber 调整分集显示编号（不改脚本身份与任何产物）。
func (s *ProjectExtraService) UpdateEpisodeDisplayNumber(
	projectName string, episode, displayEpisode int,
) error {
	return s.store.Update(projectName, func(project *domain.ProjectFile) error {
		ordered := append([]domain.EpisodeMeta(nil), project.Episodes...)
		sort.Slice(ordered, func(i, j int) bool {
			return episodeDisplayKey(ordered[i]) < episodeDisplayKey(ordered[j])
		})
		if displayEpisode > len(ordered) {
			return &service.ValueError{Msg: fmt.Sprintf("episode_display_number_invalid: max %d", len(ordered))}
		}
		index := -1
		for i, meta := range ordered {
			if meta.Episode == episode {
				index = i
				break
			}
		}
		if index < 0 {
			return ErrEpisodeNotFound
		}
		target := ordered[index]
		ordered = append(ordered[:index], ordered[index+1:]...)
		// display_episode 是 1 基插入位。
		insertAt := displayEpisode - 1
		if insertAt > len(ordered) {
			insertAt = len(ordered)
		}
		ordered = append(ordered[:insertAt], append([]domain.EpisodeMeta{target}, ordered[insertAt:]...)...)
		for i := range ordered {
			value := i + 1
			ordered[i].DisplayEpisode = &value
		}
		project.Episodes = ordered
		return nil
	})
}

// UpdateOverview 手动编辑项目概述（字段级覆盖，未提供的字段不动）。
func (s *ProjectExtraService) UpdateOverview(projectName string, in OverviewInput) (map[string]any, error) {
	var overview map[string]any
	err := s.store.Update(projectName, func(project *domain.ProjectFile) error {
		current := map[string]any{}
		if project.Overview != nil {
			if raw, err := json.Marshal(project.Overview); err == nil {
				_ = json.Unmarshal(raw, &current)
			}
		}
		if in.Synopsis != nil {
			current["synopsis"] = *in.Synopsis
		}
		if in.Genre != nil {
			current["genre"] = *in.Genre
		}
		if in.Theme != nil {
			current["theme"] = *in.Theme
		}
		if in.WorldSetting != nil {
			current["world_setting"] = *in.WorldSetting
		}
		overview = current
		project.Overview = &domain.Overview{}
		if raw, err := json.Marshal(current); err == nil {
			_ = json.Unmarshal(raw, project.Overview)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return overview, nil
}

// VideoCapabilitySummary 是项目视频能力端点的响应体（对齐 Python 字段）。
type VideoCapabilitySummary struct {
	ProviderID              string   `json:"provider_id"`
	Model                   string   `json:"model"`
	SupportedDurations      []int    `json:"supported_durations"`
	MaxDuration             int      `json:"max_duration"`
	MaxReferenceImages      int      `json:"max_reference_images"`
	FirstFrame              bool     `json:"first_frame"`
	LastFrame               bool     `json:"last_frame"`
	GenerateAudio           bool     `json:"generate_audio"`
	RequestedGenerateAudio  bool     `json:"requested_generate_audio"`
	MaxReferenceAudioCount  int      `json:"max_reference_audio_count"`
	ReferenceAudioPerImage  bool     `json:"reference_audio_per_image"`
	Source                  string   `json:"source"`
	DefaultDuration         any      `json:"default_duration"`
	ContentMode             any      `json:"content_mode"`
	GenerationMode          any      `json:"generation_mode"`
	VoiceConsistency        string   `json:"voice_consistency"`
}

// VideoCapabilities 解析当前项目视频模型能力 + 用户偏好。
//
// 「当前配置真正会执行的那个模型」按 generation_mode 定桶，与执行链同一解析入口；
// default_duration 是用户偏好，与能力档位解耦。
func (s *ProjectExtraService) VideoCapabilities(
	ctx context.Context, projectName string,
) (VideoCapabilitySummary, error) {
	pf, err := s.store.Get(projectName)
	if err != nil {
		return VideoCapabilitySummary{}, err
	}
	view, err := s.resolver.VideoCapabilitiesForProject(ctx, pf)
	if err != nil {
		return VideoCapabilitySummary{}, err
	}
	requested, _ := s.resolver.VideoGenerateAudioForProject(ctx, pf)
	summary := composeCapabilitySummary(view, requested)
	summary.DefaultDuration = nil
	if pf.DefaultDuration != nil {
		summary.DefaultDuration = *pf.DefaultDuration
	}
	summary.ContentMode = pf.ContentMode
	summary.GenerationMode = pf.GenerationMode
	return summary, nil
}

// composeCapabilitySummary 把能力视图折叠成对外字段（voice_consistency 二维派生同源）。
func composeCapabilitySummary(view config.VideoCapabilityView, requestedGenerateAudio bool) VideoCapabilitySummary {
	caps := view.Caps
	maxDuration := 0
	for _, seconds := range view.SupportedDurations {
		if seconds > maxDuration {
			maxDuration = seconds
		}
	}
	if maxDuration == 0 {
		maxDuration = 1
	}
	summary := VideoCapabilitySummary{
		ProviderID:             view.ProviderID,
		Model:                  view.ModelID,
		SupportedDurations:     view.SupportedDurations,
		MaxDuration:            maxDuration,
		MaxReferenceImages:     caps.MaxReferenceImages,
		FirstFrame:             caps.FirstFrame,
		LastFrame:              caps.LastFrame,
		RequestedGenerateAudio: requestedGenerateAudio,
		MaxReferenceAudioCount: caps.MaxReferenceAudioCount,
		ReferenceAudioPerImage: caps.ReferenceAudioPerImage,
		Source:                 view.Source,
	}
	if info, ok := provider.ModelInfoFor(view.ProviderID, view.ModelID); ok {
		summary.GenerateAudio = provider.ModelHasAudioTrack(view.ProviderID, info)
		summary.VoiceConsistency = service.VoiceConsistencyForCaps(caps, config.ProviderModel{
			ProviderID: view.ProviderID, ModelID: view.ModelID,
		}, "")
	}
	return summary
}

// ---- 内部 ----

// toAnySlice 把字符串列表转成 JSON 数组形态。
func toAnySlice(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}
