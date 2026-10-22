package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/repository"
)

// 生成类工具的共用件（对应 Python lib/generation_queue_client.py 的 batch_enqueue_and_wait）。
//
// 这些工具的统一形状：读剧本 → 选出目标分镜 → 组装入队载荷 → 逐个入队 → 等待终态 → 汇总。
// 队列与等待语义与 generate_assets 完全一致，共用同一份轮询与离线宽限逻辑。

// GenerationTask 是一个待入队的生成任务。
type GenerationTask struct {
	ResourceID string
	Prompt     string
	ScriptFile string
	// ExtraPayload 追加进入队载荷的字段（如 image_edit 的 resource_type）。
	ExtraPayload map[string]any
	// DedupResourceType 写入任务行 resource_type 列（入队去重键维度之一）；仅 image_edit 需要
	// ——四类资产与 storyboard 共用 task_type，resource_id 命名空间不互斥（角色和道具可同名）。
	DedupResourceType string
	// 依赖字段用于「同组参考图先生成」的串行约束，无依赖时全部为空。
	DependencyResourceID string
	DependencyGroup      string
	DependencyIndex      int
}

// EnqueueGenerationTasks 按计划顺序逐个入队，并把「依赖资源」解析为已入队任务的 task_id
// （对应 Python batch_enqueue_and_wait 的第一阶段：依赖解析要求顺序）。
//
// 依赖资源不在本批次内（例如其实已生成、未参与本次入队）时不留依赖记录，
// 与 Python 的 task_ids.get(...) 返回 None 同语义。
func EnqueueGenerationTasks(
	ctx context.Context, q AssetQueue, project, taskType string, tasks []GenerationTask,
) ([]string, error) {
	ids := make([]string, 0, len(tasks))
	taskIDsByResource := map[string]string{}
	for _, t := range tasks {
		payload := map[string]any{"prompt": t.Prompt}
		if t.ScriptFile != "" {
			payload["script_file"] = t.ScriptFile
		}
		for k, v := range t.ExtraPayload {
			payload[k] = v
		}
		params := repository.EnqueueParams{
			ProjectName: project,
			TaskType:    taskType,
			MediaType:   generationMediaType(taskType),
			ResourceID:  t.ResourceID,
			Payload:     payload,
			Source:      "agent",
		}
		if t.ScriptFile != "" {
			scriptFile := t.ScriptFile
			params.ScriptFile = &scriptFile
		}
		if t.DedupResourceType != "" {
			resourceType := t.DedupResourceType
			params.ResourceType = &resourceType
		}
		if t.DependencyResourceID != "" {
			if depTaskID, ok := taskIDsByResource[t.DependencyResourceID]; ok {
				params.DependencyTaskID = &depTaskID
				if t.DependencyGroup != "" {
					group := t.DependencyGroup
					params.DependencyGroup = &group
				}
				index := int64(t.DependencyIndex)
				params.DependencyIndex = &index
			}
		}
		res, err := q.EnqueueTask(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("enqueue %s %q: %w", taskType, t.ResourceID, err)
		}
		ids = append(ids, res.TaskID)
		taskIDsByResource[t.ResourceID] = res.TaskID
	}
	return ids, nil
}

// generationMediaType 映射任务类型的媒体通道（video / reference_video → video；tts → audio；其余 → image）。
func generationMediaType(taskType string) string {
	switch taskType {
	case "tts":
		return "audio"
	case "video", "reference_video":
		return "video"
	default:
		return "image"
	}
}

// AwaitGenerationTasks 逐个等待任务终态，返回成功与失败明细（保序）。
func AwaitGenerationTasks(
	ctx context.Context, q AssetQueue, tasks []GenerationTask, taskIDs []string,
) (successes []GenerationOutcome, failures []GenerationOutcome, err error) {
	for i, taskID := range taskIDs {
		outcome, err := AwaitAssetTask(ctx, q, taskID)
		if err != nil {
			return nil, nil, err
		}
		result := GenerationOutcome{
			ResourceID: tasks[i].ResourceID,
			Status:     outcome.Status,
			Details:    outcome.Result,
		}
		if outcome.ErrorMessage != nil {
			result.Message = *outcome.ErrorMessage
		}
		if outcome.Status == repository.TaskStatusSucceeded {
			successes = append(successes, result)
			continue
		}
		if result.Message == "" {
			result.Message = outcome.Status
		}
		failures = append(failures, result)
	}
	return successes, failures, nil
}

// GenerationOutcome 是一个任务的终态摘要。
type GenerationOutcome struct {
	ResourceID string
	Status     string
	Message    string
	Details    map[string]any
}

// FilePath 取结果里的产物路径，缺失时回落到按资源类型推导的默认路径。
func (o GenerationOutcome) FilePath(resourceType string) string {
	if path, ok := o.Details["file_path"].(string); ok && path != "" {
		return path
	}
	if path, ok := resourceRelativePathForTool(resourceType, o.ResourceID); ok {
		return path
	}
	return fmt.Sprintf("%s/%s", resourceType, o.ResourceID)
}

// VersionSuffix 渲染版本号后缀（无版本信息时为空串）。
func (o GenerationOutcome) VersionSuffix() string {
	if version, ok := o.Details["version"]; ok && version != nil {
		return fmt.Sprintf(" (v%v)", version)
	}
	return ""
}

// resourceRelativePathForTool 是产物相对路径的展示用推导（只影响汇总文案，不参与落盘）。
func resourceRelativePathForTool(resourceType, resourceID string) (string, bool) {
	switch resourceType {
	case "storyboards":
		return "storyboards/scene_" + resourceID + ".png", true
	case "audio":
		return "audio/segment_" + resourceID + ".wav", true
	}
	return "", false
}

// SelectScriptItemsByID 按显式 id 列表挑选条目；返回选中项与未命中的 id（保序去重）。
//
// ids 为 nil 表示「不传过滤」（由调用方决定默认集），空切片表示「显式空选择」。
func SelectScriptItemsByID(items []any, idField string, ids []string) ([]map[string]any, []string) {
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var selected []map[string]any
	found := map[string]bool{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := fmt.Sprint(entry[idField])
		if !wanted[id] {
			continue
		}
		found[id] = true
		selected = append(selected, entry)
	}
	var unmatched []string
	seen := map[string]bool{}
	for _, id := range ids {
		if found[id] || seen[id] {
			continue
		}
		seen[id] = true
		unmatched = append(unmatched, id)
	}
	return selected, unmatched
}

// GeneratedAssetPath 读条目的 generated_assets 里某资产路径（脏数据按未生成处理）。
func GeneratedAssetPath(item map[string]any, assetType string) string {
	assets, ok := item["generated_assets"].(map[string]any)
	if !ok {
		return ""
	}
	path, _ := assets[assetType].(string)
	return strings.TrimSpace(path)
}

// MissingGeneratedAsset 返回尚无该产物的条目 id（保序）。
func MissingGeneratedAsset(items []any, idField, assetType string) []string {
	var ids []string
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if GeneratedAssetPath(entry, assetType) != "" {
			continue
		}
		ids = append(ids, fmt.Sprint(entry[idField]))
	}
	return ids
}

// GenerationSummary 渲染「N succeeded, M failed」头部与明细行。
func GenerationSummary(name string, successes, failures []GenerationOutcome, resourceType string) string {
	lines := []string{fmt.Sprintf("%s summary: %d succeeded, %d failed", name, len(successes), len(failures))}
	for _, s := range successes {
		lines = append(lines, fmt.Sprintf("  ✓ %s → %s%s", s.ResourceID, s.FilePath(resourceType), s.VersionSuffix()))
	}
	for _, f := range failures {
		lines = append(lines, fmt.Sprintf("  ✗ %s: %s", f.ResourceID, f.Message))
	}
	return strings.Join(lines, "\n")
}

// ToolWaitTimeout 单任务的等待上限，避免工具调用无限挂起（队列自身有 worker 离线宽限）。
const ToolWaitTimeout = 30 * time.Minute
