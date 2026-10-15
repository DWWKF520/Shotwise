package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
)

// generate_grid（对应 Python sdk_tools/enqueue_grid.py）：为已开启宫格装配的 storyboard 项目
// 生成宫格联合图（按 segment_break 分组），每张生成完成后显式调用切分落格，端到端产出各场景
// 起始分镜图。
//
// 两阶段：先构造全部生成数据并完成资产预检（此阶段绝不保存记录或入队），全部通过后才创建持久化
// 记录并入队——避免前几张已入队、后续才发现引用缺失的半批状态。

// GridSplitter 执行宫格切分落格（*service.GridService 满足）。
type GridSplitter interface {
	ApplySplit(ctx context.Context, projectName string, grid *domain.GridGeneration) (service.GridSplitResult, error)
}

// WithGridTools 注入 generate_grid 的队列、项目、能力解析与切分依赖；任一缺失则不注册。
func WithGridTools(
	q AssetQueue, projects GenerationProjects, caps service.ImageCapabilityResolver, splitter GridSplitter,
) RuntimeOption {
	return func(r *Runtime) {
		r.gridQueue = q
		r.gridProjects = projects
		r.gridCaps = caps
		r.gridSplitter = splitter
	}
}

type generateGridIn struct {
	Script   string   `json:"script" jsonschema:"description=剧本文件名（如 episode_1.json），必须是纯文件名"`
	SceneIDs []string `json:"scene_ids,omitempty" jsonschema:"description=只生成包含这些场景的分组"`
	ListOnly bool     `json:"list_only,omitempty" jsonschema:"description=仅列出分组信息，不入队"`
}

// newGenerateGridTool 构造 generate_grid。
func newGenerateGridTool(
	project string, q AssetQueue, projects GenerationProjects,
	caps service.ImageCapabilityResolver, splitter GridSplitter,
) (tool.InvokableTool, error) {
	return toolutils.InferTool("generate_grid",
		"为已开启宫格装配的 storyboard 项目（generation_mode=storyboard 且 grid_storyboard=true）"+
			"生成宫格联合图（按 segment_break 分组），并在每张生成完成后自动执行切分落格，"+
			"端到端产出各场景起始分镜图。"+
			"list_only=true 时只列出分组不执行生成。scene_ids 过滤包含这些场景的分组。",
		func(ctx context.Context, in generateGridIn) (string, error) {
			if err := validateScriptFilename(in.Script); err != nil {
				return denyEnvelope(err.Error()), nil
			}
			if projects == nil || caps == nil {
				return denyEnvelope("grid generation not configured"), nil
			}
			text, failed, err := runGenerateGrid(ctx, project, in, q, projects, caps, splitter)
			if err != nil {
				return denyEnvelope(err.Error()), nil
			}
			if failed {
				return denyEnvelope(text), nil
			}
			return okEnvelope(text), nil
		})
}

func runGenerateGrid(
	ctx context.Context, project string, in generateGridIn, q AssetQueue,
	projects GenerationProjects, caps service.ImageCapabilityResolver, splitter GridSplitter,
) (string, bool, error) {
	pf, err := projects.Get(project)
	if err != nil {
		return "", false, err
	}
	projectDir, err := projects.Dir(project)
	if err != nil {
		return "", false, err
	}
	script, err := projects.ReadScriptJSON(project, in.Script)
	if err != nil {
		return "", false, fmt.Errorf("剧本 %s 不可读取: %w", in.Script, err)
	}
	// 失配剧本在此被拒：按分镜路线该读的数组不在剧本里，继续走下去只会报「没有匹配的场景组」，
	// 把成因埋掉。
	if _, err := domain.EnsureRouteSkeleton(script, scriptContentMode(script, pf), pf.GenerationMode); err != nil {
		return "", false, err
	}
	// list_only 是预览模式，与生成分支一样必须先过宫格开关校验——否则未开宫格的项目靠
	// list_only=true 就能拿到成功响应，调用方会误以为该工具适用于当前项目。
	if !pf.GridStoryboard {
		return "⚠️  项目未启用宫格分镜（grid_storyboard 未开启）", true, nil
	}

	// 4×4 / 5×5 只在图像分辨率档为 4K 时放行；预览与生成共用同一次判定。
	allowLargeGrid := service.ResolveLargeGridAllowed(ctx, caps, pf)

	items, idField, fields := domain.StoryboardItems(script)
	groups := domain.GroupScenesBySegmentBreak(items, idField)
	// scene_ids 为 nil 表示「不过滤」，空切片表示「显式过滤到 0 个」；用 nil 判定保持区分。
	explicitFilter := in.SceneIDs != nil
	if explicitFilter {
		wanted := map[string]bool{}
		for _, id := range in.SceneIDs {
			wanted[id] = true
		}
		filtered := make([][]map[string]any, 0, len(groups))
		for _, group := range groups {
			for _, item := range group {
				if wanted[fmt.Sprint(item[idField])] {
					filtered = append(filtered, group)
					break
				}
			}
		}
		groups = filtered
	}

	aspectRatio := domain.VideoAspectRatio(pf)
	if in.ListOnly {
		return strings.Join(listGridGroups(groups, idField, aspectRatio, allowLargeGrid), "\n"), false, nil
	}

	if len(groups) == 0 {
		// 显式传了 scene_ids 但全部不命中（或传了 []）→ 错误；不传但脚本本身没有分组也走这条路，
		// 按信息文案不带 is_error。
		return "没有匹配的场景组", explicitFilter, nil
	}

	episode, err := service.ResolveEpisodeFromScript(script, in.Script)
	if err != nil {
		return "", false, err
	}
	style := pf.Style

	// 第一阶段：只构造生成数据并完成全部资产预检；此阶段绝不保存记录或入队。
	type stagedGroup struct {
		groupIDs []string
		layout   domain.GridLayout
		prompt   string
	}
	var (
		staged           []stagedGroup
		skipped          []string
		preflightResults []service.PreflightResult
	)
	for _, group := range groups {
		groupIDs := make([]string, 0, len(group))
		for _, item := range group {
			groupIDs = append(groupIDs, fmt.Sprint(item[idField]))
		}
		layout, ok := domain.CalculateGridLayout(len(groupIDs), aspectRatio, allowLargeGrid)
		if !ok {
			skipped = append(skipped, fmt.Sprintf("⏭️  跳过 %s..%s（%d 场景，不足 4 个）",
				groupIDs[0], groupIDs[len(groupIDs)-1], len(groupIDs)))
			continue
		}
		prompt := domain.BuildGridPrompt(group, idField, layout.Rows, layout.Cols, style,
			aspectRatio, layout.GridAspectRatio, nil)
		for _, item := range group {
			preflightResults = append(preflightResults, service.ValidateItemAssetReferences(pf, projectDir, item, fields))
		}
		staged = append(staged, stagedGroup{groupIDs: groupIDs, layout: layout, prompt: prompt})
	}
	if message := service.FormatPreflightError(service.MergePreflightResults(preflightResults...)); message != "" {
		return "", false, fmt.Errorf("%s", message)
	}

	// 第二阶段：全部组通过预检后才创建持久化记录并入队。
	manager, err := service.NewGridManager(projectDir)
	if err != nil {
		return "", false, err
	}
	type pendingGrid struct {
		grid   *domain.GridGeneration
		taskID string
	}
	var (
		pending         []pendingGrid
		details         = append([]string{}, skipped...)
		enqueueFailures int
	)
	for _, entry := range staged {
		prompt := entry.prompt
		grid := domain.NewGridGeneration(episode, in.Script, entry.groupIDs,
			entry.layout.Rows, entry.layout.Cols, entry.layout.GridSize, "", "", aspectRatio, &prompt)
		if err := manager.Save(grid); err != nil {
			return "", false, err
		}
		scriptFile := in.Script
		res, err := q.EnqueueTask(ctx, repository.EnqueueParams{
			ProjectName: project,
			TaskType:    "grid",
			MediaType:   "image",
			ResourceID:  grid.ID,
			Payload: map[string]any{
				"prompt":             prompt,
				"script_file":        in.Script,
				"scene_ids":          entry.groupIDs,
				"grid_size":          entry.layout.GridSize,
				"rows":               entry.layout.Rows,
				"cols":               entry.layout.Cols,
				"grid_aspect_ratio":  entry.layout.GridAspectRatio,
				"video_aspect_ratio": aspectRatio,
			},
			ScriptFile: &scriptFile,
			Source:     "agent",
		})
		if err != nil {
			// 入队失败即回滚刚创建记录（与 Python 侧 gm.delete 同语义）。
			_, _ = manager.Delete(grid.ID)
			enqueueFailures++
			details = append(details, fmt.Sprintf("  ✗ %s（%s..%s）入队失败: %v",
				grid.ID, entry.groupIDs[0], entry.groupIDs[len(entry.groupIDs)-1], err))
			continue
		}
		pending = append(pending, pendingGrid{grid: grid, taskID: res.TaskID})
	}

	if len(pending) == 0 {
		return strings.Join(append(details, "没有需要生成的宫格组"), "\n"), enqueueFailures > 0, nil
	}

	// 并发等待全部宫格——image worker 通道可并行多张，串行等待会掩盖其吞吐。
	outcomes := make([]gridWaitOutcome, len(pending))
	var wg sync.WaitGroup
	for i, item := range pending {
		wg.Add(1)
		go func(i int, taskID string) {
			defer wg.Done()
			task, err := awaitAssetTask(ctx, q, taskID)
			outcomes[i] = gridWaitOutcome{task: task, err: err}
		}(i, item.taskID)
	}
	wg.Wait()

	var (
		successes []string
		failures  int
	)
	for i, item := range pending {
		outcome := outcomes[i]
		grid := item.grid
		if outcome.err != nil {
			failures++
			details = append(details, fmt.Sprintf("  ✗ %s: %v", grid.ID, outcome.err))
			continue
		}
		if outcome.task.Status != repository.TaskStatusSucceeded {
			message := "unknown"
			if outcome.task.ErrorMessage != nil {
				message = *outcome.task.ErrorMessage
			}
			failures++
			details = append(details, fmt.Sprintf("  ✗ %s: %s", grid.ID, message))
			continue
		}
		// 生成任务只产出联合图；分镜格落盘在此显式调用切分补上。重载记录取 worker 回填的最新状态。
		reloaded, ok, err := manager.Get(grid.ID)
		if err != nil || !ok {
			failures++
			details = append(details, fmt.Sprintf("  ✗ %s: 生成后读不到宫格记录", grid.ID))
			continue
		}
		if splitter == nil {
			failures++
			details = append(details, fmt.Sprintf("  ✗ %s: 宫格切分未配置", grid.ID))
			continue
		}
		splitResult, err := splitter.ApplySplit(ctx, project, reloaded)
		if err != nil {
			// 联合图已生成成功，仅落格失败：提示可在宫格面板重试切分（无需重新生成、不重复计费）。
			failures++
			details = append(details, fmt.Sprintf(
				"  ✗ %s: 联合图已生成，但切分落格失败（可在宫格面板重试切分）: %v", grid.ID, err))
			continue
		}
		successes = append(successes, grid.ID)
		last := ""
		if len(reloaded.SceneIDs) > 0 {
			last = reloaded.SceneIDs[len(reloaded.SceneIDs)-1]
		}
		first := ""
		if len(reloaded.SceneIDs) > 0 {
			first = reloaded.SceneIDs[0]
		}
		line := fmt.Sprintf("  ✓ %s（%s..%s，已切分 %d 格）", grid.ID, first, last, len(splitResult.UpdatedSceneIDs))
		if len(splitResult.MissingSceneIDs) > 0 {
			line += fmt.Sprintf("，跳过已不在剧本的分镜: %v", splitResult.MissingSceneIDs)
		}
		details = append(details, line)
	}

	header := fmt.Sprintf("generate_grid summary: %d succeeded, %d failed", len(successes), failures+enqueueFailures)
	return strings.Join(append([]string{header}, details...), "\n"), failures > 0 || enqueueFailures > 0, nil
}

// gridWaitOutcome 是单个宫格任务的等待结果。
type gridWaitOutcome struct {
	task repository.TaskData
	err  error
}

// listGridGroups 渲染分组清单（对应 Python _list_groups）；空切片与 nil 语义由调用方保持区分。
func listGridGroups(groups [][]map[string]any, idField, aspectRatio string, allowLargeGrid bool) []string {
	lines := []string{fmt.Sprintf("共 %d 个分组：", len(groups))}
	for i, group := range groups {
		ids := make([]string, 0, len(group))
		for _, item := range group {
			ids = append(ids, fmt.Sprint(item[idField]))
		}
		if len(ids) == 0 {
			continue
		}
		status := "single (< 4 场景)"
		if layout, ok := domain.CalculateGridLayout(len(ids), aspectRatio, allowLargeGrid); ok {
			status = fmt.Sprintf("%s (%d×%d)", layout.GridSize, layout.Rows, layout.Cols)
		}
		lines = append(lines, fmt.Sprintf("  组 %d: %s..%s (%d 场景) → %s", i+1, ids[0], ids[len(ids)-1], len(ids), status))
	}
	return lines
}
