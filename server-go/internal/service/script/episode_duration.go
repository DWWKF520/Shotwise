// 分集目标时长规划（对应 server/routers/episode_duration.py + lib/project_manager.py 的
// episode duration 方法组）。
//
// 三个不变式：
//   - 「保存规划设置」只写 metadata.episode_duration_plan，不动任何分镜的 duration_seconds；
//   - 「预览」是纯读，永不自施加；「应用」才写回分镜时长；
//   - 所有写路径都在脚本锁内做修订号比对（乐观并发），过期快照一律 409 带 actual_revision，
//     让前端拿到新基线后重算，而不是静默覆盖别人的改动。
package script

import (
	"github.com/shotwise/shotwise/internal/service"
	"context"
	"errors"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/provider"
)

// EpisodeDurationRevisionConflictError 是修订号过期（对应 EpisodeDurationRevisionConflict）。
type EpisodeDurationRevisionConflictError struct {
	Expected string
	Actual   string
}

func (e *EpisodeDurationRevisionConflictError) Error() string { return "episode duration revision is stale" }

// EpisodeDurationInvalidError 对应 Python service.ValueError：422 episode_duration_invalid。
type EpisodeDurationInvalidError struct{ Detail string }

func (e *EpisodeDurationInvalidError) Error() string { return e.Detail }

// EpisodeDurationNotFoundError 对应 Python KeyError(episode)：404 episode_duration_resource_not_found。
type EpisodeDurationNotFoundError struct{}

func (e *EpisodeDurationNotFoundError) Error() string { return "episode_duration_resource_not_found" }

// EpisodeDurationService 承载分集时长规划的读写。
type EpisodeDurationService struct {
	store *service.ProjectStore
	caps  service.VideoCapabilityResolver
}

// NewEpisodeDurationService 构造；caps 用于解析「后端真正支持的时长档位」。
func NewEpisodeDurationService(store *service.ProjectStore, caps service.VideoCapabilityResolver) *EpisodeDurationService {
	return &EpisodeDurationService{store: store, caps: caps}
}

// EpisodeDurationPlanInput 是保存/预览/应用的入参。
type EpisodeDurationPlanInput struct {
	TargetSeconds     int
	Strategy          string
	ManualAllocations map[string]int
}

// config 归一为规划配置；非法组合（如目标时长非正、策略未知）返回 422。
func (p EpisodeDurationPlanInput) config() (domain.EpisodeDurationPlanConfig, error) {
	raw := map[string]any{
		"target_seconds":     p.TargetSeconds,
		"strategy":           p.Strategy,
		"manual_allocations": manualAllocationsAny(p.ManualAllocations),
	}
	config := domain.ReadEpisodeDurationPlan(map[string]any{}, map[string]any{
		"metadata": map[string]any{"episode_duration_plan": raw},
	})
	if config == nil {
		return domain.EpisodeDurationPlanConfig{}, &EpisodeDurationInvalidError{Detail: "invalid episode duration plan"}
	}
	return *config, nil
}

// manualAllocationsAny 把 map[string]int 转成 JSON 形态的 map[string]any。
func manualAllocationsAny(in map[string]int) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// State 返回当前规划设置与逐项状态（只读）。
func (s *EpisodeDurationService) State(ctx context.Context, projectName string, episode int) (map[string]any, error) {
	pf, script, err := s.snapshot(projectName, episode)
	if err != nil {
		return nil, err
	}
	return episodeDurationState(pf, script), nil
}

// Save 只持久化规划设置，不改分镜时长。
func (s *EpisodeDurationService) Save(
	ctx context.Context, projectName string, episode int, expectedRevision string, in EpisodeDurationPlanInput,
) (map[string]any, error) {
	config, err := in.config()
	if err != nil {
		return nil, err
	}
	var revision string
	err = s.store.UpdateEpisodeScriptNoValidate(projectName, episodeResolver(episode), func(script map[string]any) error {
		actual := domain.EpisodeDurationRevision(script)
		if actual != expectedRevision {
			return &EpisodeDurationRevisionConflictError{Expected: expectedRevision, Actual: actual}
		}
		metadata, ok := script["metadata"].(map[string]any)
		if !ok {
			metadata = map[string]any{}
			script["metadata"] = metadata
		}
		metadata["episode_duration_plan"] = config.AsDict()
		revision = domain.EpisodeDurationRevision(script)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"revision": revision, "plan": config.AsDict()}, nil
}

// Preview 生成只读 diff，不落盘。
func (s *EpisodeDurationService) Preview(
	ctx context.Context, projectName string, episode int, in EpisodeDurationPlanInput,
) (map[string]any, error) {
	config, err := in.config()
	if err != nil {
		return nil, err
	}
	_, script, err := s.snapshot(projectName, episode)
	if err != nil {
		return nil, err
	}
	supported, err := s.supportedDurations(ctx, projectName)
	if err != nil {
		return nil, err
	}
	revision := domain.EpisodeDurationRevision(script)
	shots := durationShotInputs(script, supported)
	planner := domain.EpisodeDurationPlanner{}
	plan, err := planner.BuildPlan(config.TargetSeconds, shots, config.Strategy, config.ManualAllocations, &revision)
	if err != nil {
		return nil, &EpisodeDurationInvalidError{Detail: err.Error()}
	}
	preview := planner.PreviewReplan(shots, plan)
	changes := make([]any, 0, len(preview.Changes))
	for _, change := range preview.Changes {
		changes = append(changes, map[string]any{
			"resource_id":  change.ShotID,
			"from_seconds": change.FromSeconds,
			"to_seconds":   change.ToSeconds,
			"clamp_reason": pointerOrNil(change.ClampReason),
		})
	}
	return map[string]any{
		"revision":       revision,
		"plan":           config.AsDict(),
		"target_seconds": preview.TargetSeconds,
		"changes":        changes,
	}, nil
}

// Apply 在锁内复核修订号与逐项状态后写回分镜时长。
func (s *EpisodeDurationService) Apply(
	ctx context.Context, projectName string, episode int, expectedRevision string, in EpisodeDurationPlanInput,
) (map[string]any, error) {
	config, err := in.config()
	if err != nil {
		return nil, err
	}
	supported, err := s.supportedDurations(ctx, projectName)
	if err != nil {
		return nil, err
	}
	planner := domain.EpisodeDurationPlanner{}
	applied := map[string]any{}
	skipped := []any{}
	var revision string
	err = s.store.UpdateEpisodeScriptNoValidate(projectName, episodeResolver(episode), func(script map[string]any) error {
		actual := domain.EpisodeDurationRevision(script)
		if actual != expectedRevision {
			return &EpisodeDurationRevisionConflictError{Expected: expectedRevision, Actual: actual}
		}
		shots := durationShotInputs(script, supported)
		plan, buildErr := planner.BuildPlan(
			config.TargetSeconds, shots, config.Strategy, config.ManualAllocations, &actual)
		if buildErr != nil {
			return &EpisodeDurationInvalidError{Detail: buildErr.Error()}
		}
		changes := planner.ApplyConfirmedPlan(shots, plan)
		items, idField, _, resolveErr := domain.ResolveScriptItems(script)
		if resolveErr != nil {
			return &EpisodeDurationInvalidError{Detail: resolveErr.Error()}
		}
		byID := map[string]map[string]any{}
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			byID[pythonStrOf(item[idField])] = item
		}
		for resourceID, seconds := range changes {
			item := byID[resourceID]
			// 逐项状态可能在本请求期间变化（别人锁了 / 视频已生成）：跳过而不是覆盖。
			if item == nil || domain.IsItemDurationLocked(item) || domain.IsItemVideoGenerated(item) {
				skipped = append(skipped, resourceID)
				continue
			}
			item["duration_seconds"] = seconds
			applied[resourceID] = seconds
		}
		metadata, ok := script["metadata"].(map[string]any)
		if !ok {
			metadata = map[string]any{}
			script["metadata"] = metadata
		}
		metadata["episode_duration_plan"] = config.AsDict()
		revision = domain.EpisodeDurationRevision(script)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"revision": revision,
		"plan":     config.AsDict(),
		"applied":  applied,
		"skipped":  skipped,
	}, nil
}

// SetLock 切换单个条目的时长锁。
func (s *EpisodeDurationService) SetLock(
	ctx context.Context, projectName string, episode int, resourceID string, locked bool, expectedRevision string,
) (map[string]any, error) {
	var (
		revision string
		result   bool
	)
	err := s.store.UpdateEpisodeScriptNoValidate(projectName, episodeResolver(episode), func(script map[string]any) error {
		actual := domain.EpisodeDurationRevision(script)
		if actual != expectedRevision {
			return &EpisodeDurationRevisionConflictError{Expected: expectedRevision, Actual: actual}
		}
		items, idField, _, resolveErr := domain.ResolveScriptItems(script)
		if resolveErr != nil {
			return &EpisodeDurationInvalidError{Detail: resolveErr.Error()}
		}
		var target map[string]any
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if pythonStrOf(item[idField]) == resourceID {
				target = item
				break
			}
		}
		if target == nil {
			return &EpisodeDurationNotFoundError{}
		}
		// 视频已生成的分镜时长由成片决定，锁状态不允许再改（否则「锁」与「不可改」冲突）。
		if domain.IsItemVideoGenerated(target) && domain.IsItemDurationLocked(target) != locked {
			return &EpisodeDurationInvalidError{Detail: "generated item duration lock cannot be changed"}
		}
		target["duration_locked"] = locked
		result = domain.IsItemDurationLocked(target)
		revision = domain.EpisodeDurationRevision(script)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"revision": revision, "resource_id": resourceID, "locked": result}, nil
}

// ---- 内部 ----

// episodeResolver 构造「按集号找 script_file」的解析器；缺集/缺 script_file 都是 404。
func episodeResolver(episode int) service.EpisodeScriptResolver {
	return func(project *domain.ProjectFile) (string, error) {
		scriptFile, err := service.EpisodeScriptFile(project, episode)
		if err != nil {
			return "", &EpisodeDurationNotFoundError{}
		}
		return scriptFile, nil
	}
}

// snapshot 只读加载 (project, script)：不加锁、不迁移、不写盘。
func (s *EpisodeDurationService) snapshot(projectName string, episode int) (*domain.ProjectFile, map[string]any, error) {
	pf, err := s.store.Get(projectName)
	if err != nil {
		return nil, nil, err
	}
	scriptFile, err := service.EpisodeScriptFile(pf, episode)
	if err != nil {
		return nil, nil, &EpisodeDurationNotFoundError{}
	}
	script, err := s.store.ReadScriptJSON(projectName, scriptFile)
	if err != nil {
		return nil, nil, &EpisodeDurationNotFoundError{}
	}
	return pf, script, nil
}

// supportedDurations 解析项目视频后端支持的时长档位；不可用按 422 处理（对应 Python service.ValueError）。
func (s *EpisodeDurationService) supportedDurations(ctx context.Context, projectName string) ([]int, error) {
	pf, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	if s.caps == nil {
		return nil, &EpisodeDurationInvalidError{Detail: "supported durations are unavailable"}
	}
	dctx := service.ResolveProjectDurationContext(
		ctx, s.caps, pf, provider.VideoBucketForGenerationMode(pf.GenerationMode))
	durations := dedupePositiveInts(dctx.SupportedDurations)
	if len(durations) == 0 {
		return nil, &EpisodeDurationInvalidError{Detail: "supported durations are unavailable"}
	}
	return durations, nil
}

// durationShotInputs 把剧本条目转成规划输入（对应 _duration_shot_inputs）。
func durationShotInputs(script map[string]any, supported []int) []domain.ShotDurationInput {
	items, idField, _, err := domain.ResolveScriptItems(script)
	if err != nil {
		return nil
	}
	shots := make([]domain.ShotDurationInput, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		resourceID := strings.TrimSpace(pythonStrOf(item[idField]))
		var current *int
		if seconds, ok := scriptInt(item["duration_seconds"]); ok {
			value := seconds
			current = &value
		}
		weight := 1.0
		if current != nil && *current > 0 {
			weight = float64(*current)
		}
		shots = append(shots, domain.ShotDurationInput{
			ShotID:             resourceID,
			CurrentSeconds:     current,
			SupportedDurations: supported,
			Locked:             domain.IsItemDurationLocked(item),
			Generated:          domain.IsItemVideoGenerated(item),
			Weight:             weight,
		})
	}
	return shots
}

// episodeDurationState 组装 GET 响应（对应 _episode_duration_state）。
func episodeDurationState(pf *domain.ProjectFile, script map[string]any) map[string]any {
	var plan any
	if config := domain.ReadEpisodeDurationPlan(projectAsMap(pf), script); config != nil {
		plan = config.AsDict()
	}
	items, idField, _, err := domain.ResolveScriptItems(script)
	list := make([]any, 0, len(items))
	if err == nil {
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			list = append(list, map[string]any{
				"resource_id":      pythonStrOf(item[idField]),
				"duration_seconds": item["duration_seconds"],
				"locked":           domain.IsItemDurationLocked(item),
				"generated":        domain.IsItemVideoGenerated(item),
			})
		}
	}
	return map[string]any{
		"revision": domain.EpisodeDurationRevision(script),
		"plan":     plan,
		"items":    list,
	}
}

// projectAsMap 把 project.json 投影成读取规划配置所需的通用形态。
//
// 规划配置可能落在 episode 级 metadata（旧格式）或 project.json，读侧只需这两处，
// 因此只投影必要字段，避免为一次读配置做全量序列化。
func projectAsMap(pf *domain.ProjectFile) map[string]any {
	if pf == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if pf.ContentMode != "" {
		out["content_mode"] = pf.ContentMode
	}
	if pf.GenerationMode != "" {
		out["generation_mode"] = pf.GenerationMode
	}
	if pf.TargetDuration != nil {
		out["target_duration"] = *pf.TargetDuration
	}
	if len(pf.Episodes) > 0 {
		episodes := make([]any, 0, len(pf.Episodes))
		for _, meta := range pf.Episodes {
			episodes = append(episodes, map[string]any{
				"episode":     meta.Episode,
				"script_file": meta.ScriptFile,
			})
		}
		out["episodes"] = episodes
	}
	return out
}

// pythonStrOf 把条目里的标识取成字符串（缺省空串），与 Python str(item.get(id_field, "")) 同口径。
func pythonStrOf(value any) string { return service.PythonStrLike(value) }

// scriptInt 取 JSON 里的整数；布尔与浮点非整数值一律视为「没有」。
func scriptInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if float64(int(v)) == v {
			return int(v), true
		}
	}
	return 0, false
}

// pointerOrNil 把可选字符串指针转成 any（nil 指针 → JSON null）。
func pointerOrNil(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// dedupePositiveInts 排序去重正整数（对应 Python sorted({...})）。
func dedupePositiveInts(values []int) []int {
	seen := map[int]struct{}{}
	out := make([]int, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ErrEpisodeDurationUnsupported 保留给装配层判定能力缺失（当前统一走 422）。
var ErrEpisodeDurationUnsupported = errors.New("supported durations are unavailable")
