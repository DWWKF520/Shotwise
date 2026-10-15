package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/repository"
)

// 生成类工具的共用件（对应 Python lib/generation_queue_client.py 的 batch_enqueue_and_wait）。
//
// 这些工具的统一形状：读剧本 → 选出目标分镜 → 组装入队载荷 → 逐个入队 → 等待终态 → 汇总。
// 队列与等待语义与 generate_assets 完全一致，共用同一份轮询与离线宽限逻辑。

// GenerationQueue 是生成类工具的队列能力（*queue.Queue 满足 AssetQueue）。
// 与 generate_assets 共用同一接口，避免每个工具各声明一份。

// GenerationProjects 是生成类工具的项目读取能力（*service.ProjectStore 满足）。
type GenerationProjects interface {
	Get(name string) (*domain.ProjectFile, error)
	Dir(name string) (string, error)
	ReadScriptJSON(name, filename string) (map[string]any, error)
}

// generationTask 是一个待入队的生成任务。
type generationTask struct {
	resourceID string
	prompt     string
	scriptFile string
	// extraPayload 追加进入队载荷的字段（如 image_edit 的 resource_type）。
	extraPayload map[string]any
	// dedupResourceType 写入任务行 resource_type 列（入队去重键维度之一）；仅 image_edit 需要
	// ——四类资产与 storyboard 共用 task_type，resource_id 命名空间不互斥（角色和道具可同名）。
	dedupResourceType string
	// 依赖字段用于「同组参考图先生成」的串行约束，无依赖时全部为空。
	dependencyResourceID string
	dependencyGroup      string
	dependencyIndex      int
}

// enqueueGenerationTasks 按计划顺序逐个入队，并把「依赖资源」解析为已入队任务的 task_id
// （对应 Python batch_enqueue_and_wait 的第一阶段：依赖解析要求顺序）。
//
// 依赖资源不在本批次内（例如其实已生成、未参与本次入队）时不留依赖记录，
// 与 Python 的 task_ids.get(...) 返回 None 同语义。
func enqueueGenerationTasks(
	ctx context.Context, q AssetQueue, project, taskType string, tasks []generationTask,
) ([]string, error) {
	ids := make([]string, 0, len(tasks))
	taskIDsByResource := map[string]string{}
	for _, t := range tasks {
		payload := map[string]any{"prompt": t.prompt}
		if t.scriptFile != "" {
			payload["script_file"] = t.scriptFile
		}
		for k, v := range t.extraPayload {
			payload[k] = v
		}
		params := repository.EnqueueParams{
			ProjectName: project,
			TaskType:    taskType,
			MediaType:   generationMediaType(taskType),
			ResourceID:  t.resourceID,
			Payload:     payload,
			Source:      "agent",
		}
		if t.scriptFile != "" {
			scriptFile := t.scriptFile
			params.ScriptFile = &scriptFile
		}
		if t.dedupResourceType != "" {
			resourceType := t.dedupResourceType
			params.ResourceType = &resourceType
		}
		if t.dependencyResourceID != "" {
			if depTaskID, ok := taskIDsByResource[t.dependencyResourceID]; ok {
				params.DependencyTaskID = &depTaskID
				if t.dependencyGroup != "" {
					group := t.dependencyGroup
					params.DependencyGroup = &group
				}
				index := int64(t.dependencyIndex)
				params.DependencyIndex = &index
			}
		}
		res, err := q.EnqueueTask(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("enqueue %s %q: %w", taskType, t.resourceID, err)
		}
		ids = append(ids, res.TaskID)
		taskIDsByResource[t.resourceID] = res.TaskID
	}
	return ids, nil
}

// generationMediaType 映射任务类型的媒体通道。
func generationMediaType(taskType string) string {
	switch taskType {
	case "tts":
		return "audio"
	default:
		return "image"
	}
}

// awaitGenerationTasks 逐个等待任务终态，返回成功与失败明细（保序）。
func awaitGenerationTasks(
	ctx context.Context, q AssetQueue, tasks []generationTask, taskIDs []string,
) (successes []generationOutcome, failures []generationOutcome, err error) {
	for i, taskID := range taskIDs {
		outcome, err := awaitAssetTask(ctx, q, taskID)
		if err != nil {
			return nil, nil, err
		}
		result := generationOutcome{
			resourceID: tasks[i].resourceID,
			status:     outcome.Status,
			details:    outcome.Result,
		}
		if outcome.ErrorMessage != nil {
			result.message = *outcome.ErrorMessage
		}
		if outcome.Status == repository.TaskStatusSucceeded {
			successes = append(successes, result)
			continue
		}
		if result.message == "" {
			result.message = outcome.Status
		}
		failures = append(failures, result)
	}
	return successes, failures, nil
}

// generationOutcome 是一个任务的终态摘要。
type generationOutcome struct {
	resourceID string
	status     string
	message    string
	details    map[string]any
}

// filePath 取结果里的产物路径，缺失时回落到按资源类型推导的默认路径。
func (o generationOutcome) filePath(resourceType string) string {
	if path, ok := o.details["file_path"].(string); ok && path != "" {
		return path
	}
	if path, ok := resourceRelativePathForTool(resourceType, o.resourceID); ok {
		return path
	}
	return fmt.Sprintf("%s/%s", resourceType, o.resourceID)
}

// versionSuffix 渲染版本号后缀（无版本信息时为空串）。
func (o generationOutcome) versionSuffix() string {
	if version, ok := o.details["version"]; ok && version != nil {
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

// selectScriptItemsByID 按显式 id 列表挑选条目；返回选中项与未命中的 id（保序去重）。
//
// ids 为 nil 表示「不传过滤」（由调用方决定默认集），空切片表示「显式空选择」。
func selectScriptItemsByID(items []any, idField string, ids []string) ([]map[string]any, []string) {
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

// generatedAssetPath 读条目的 generated_assets 里某资产路径（脏数据按未生成处理）。
func generatedAssetPath(item map[string]any, assetType string) string {
	assets, ok := item["generated_assets"].(map[string]any)
	if !ok {
		return ""
	}
	path, _ := assets[assetType].(string)
	return strings.TrimSpace(path)
}

// missingGeneratedAsset 返回尚无该产物的条目 id（保序）。
func missingGeneratedAsset(items []any, idField, assetType string) []string {
	var ids []string
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if generatedAssetPath(entry, assetType) != "" {
			continue
		}
		ids = append(ids, fmt.Sprint(entry[idField]))
	}
	return ids
}

// generationSummary 渲染「N succeeded, M failed」头部与明细行。
func generationSummary(name string, successes, failures []generationOutcome, resourceType string) string {
	lines := []string{fmt.Sprintf("%s summary: %d succeeded, %d failed", name, len(successes), len(failures))}
	for _, s := range successes {
		lines = append(lines, fmt.Sprintf("  ✓ %s → %s%s", s.resourceID, s.filePath(resourceType), s.versionSuffix()))
	}
	for _, f := range failures {
		lines = append(lines, fmt.Sprintf("  ✗ %s: %s", f.resourceID, f.message))
	}
	return strings.Join(lines, "\n")
}

// toolWaitTimeout 单任务的等待上限，避免工具调用无限挂起（队列自身有 worker 离线宽限）。
const toolWaitTimeout = 30 * time.Minute
