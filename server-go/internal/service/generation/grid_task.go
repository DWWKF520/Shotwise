// 宫格图生成与收编（对应 server/routers/grids.py 的生成 / 能力 / 列表 / 详情 / 重生成 /
// 切分六个入口；联合图上传见文件末尾说明）。
//
// 三条不可回退的语义：
//   - 动作端点（重生成 / 切分 / 上传）都要过 ensure_grid_writable 闸门：写入边界已拒绝
//     ad / 关闭宫格的项目，但 HTTP 直调能绕过写入边界，所以动作端点必须再设一道；
//   - 资产 preflight 早于任何「删旧记录 / 写新记录 / 入队计费任务」：入队即计费，先校验；
//   - 重生成把同一次产出重跑一遍：rows/cols、prompt、单格比例全部沿用记录上冻结的值。
package generation

import (
	"github.com/shotwise/shotwise/internal/service"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/repository"
)

// service.GridQueue 是宫格入队需要的队列能力。
type GridQueue interface {
	EnqueueTask(ctx context.Context, p repository.EnqueueParams) (repository.EnqueueResult, error)
}

// GridCapabilityResolver 是「4×4 / 5×5 是否放行」的图像档位解析能力。
type GridCapabilityResolver interface {
	service.ImageCapabilityResolver
}

// GridTaskService 承载宫格的生成、查询、重生成与切分。
type GridTaskService struct {
	store  *service.ProjectStore
	queue  service.GridQueue
	images GridCapabilityResolver
	split  *service.GridService
}

// NewGridTaskService 构造；split 复用已有的分镜落格服务。
func NewGridTaskService(
	store *service.ProjectStore, queue service.GridQueue, images GridCapabilityResolver, split *service.GridService,
) *GridTaskService {
	return &GridTaskService{store: store, queue: queue, images: images, split: split}
}

// gridWriteBlockedError 是宫格写入口的项目级闸门错误（对应 ensure_grid_writable）。
//
// 两个码都是 400，但语义不同：广告项目根本不该有宫格，开关关闭则是「本项目此刻不产宫格」。
func ensureGridWritable(project *domain.ProjectFile) error {
	if project.ContentMode == string(domain.ContentAd) {
		return service.GenerateBadRequest("ad_grid_not_supported", nil)
	}
	if !domain.GridStoryboardEnabled(project) {
		return service.GenerateBadRequest("grid_storyboard_not_enabled", nil)
	}
	return nil
}

// Generate 按分镜分组提交宫格生成任务。
//
// 分组 → （可选）按 scene_ids 过滤 → preflight → 清旧记录 → 分块建记录并入队。
// 每一步的先后顺序都是计费安全的：任何校验失败都在入队之前返回。
func (s *GridTaskService) Generate(
	ctx context.Context, projectName string, episode int, scriptFile string, sceneIDs []string, userID string,
) (map[string]any, error) {
	project, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	if err := ensureGridWritable(project); err != nil {
		return nil, err
	}
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	script, err := s.store.ReadScriptJSON(projectName, scriptFile)
	if err != nil {
		return nil, service.GenerateBadRequest("invalid_script_file", map[string]any{"name": scriptFile})
	}
	items, idField, fields := domain.StoryboardItems(script)
	aspectRatio := domain.VideoAspectRatioOf(project)
	style := project.Style
	allowLargeGrid := service.ResolveLargeGridAllowed(ctx, s.images, project)

	groups := domain.GroupScenesBySegmentBreak(items, idField)
	if len(sceneIDs) > 0 {
		wanted := map[string]bool{}
		for _, id := range sceneIDs {
			wanted[id] = true
		}
		filtered := make([][]map[string]any, 0, len(groups))
		for _, group := range groups {
			for _, item := range group {
				if wanted[storyboardIDOf(item, idField)] {
					filtered = append(filtered, group)
					break
				}
			}
		}
		groups = filtered
	}

	// preflight 只覆盖「真的会产出一张宫格」的分组：布局为空的分组不会入队，
	// 让它们的缺失资产挡住整批请求是无意义的拒绝。
	toValidate := make([]map[string]any, 0, len(items))
	for _, group := range groups {
		if _, ok := domain.CalculateGridLayout(len(group), aspectRatio, allowLargeGrid); !ok {
			continue
		}
		toValidate = append(toValidate, group...)
	}
	if preflight := validateGridItems(project, projectDir, toValidate, fields); len(preflight.Issues) > 0 {
		return nil, service.GenerateBadRequest("generation_asset_preflight_failed", preflight.I18NParams())
	}

	manager, err := service.NewGridManager(projectDir)
	if err != nil {
		return nil, err
	}
	existing, err := manager.ListAll()
	if err != nil {
		return nil, err
	}

	gridIDs := make([]any, 0, len(groups))
	taskIDs := make([]any, 0, len(groups))
	allDeduped := true
	for _, group := range groups {
		groupIDs := make([]string, 0, len(group))
		for _, item := range group {
			groupIDs = append(groupIDs, storyboardIDOf(item, idField))
		}
		layout, ok := domain.CalculateGridLayout(len(groupIDs), aspectRatio, allowLargeGrid)
		if !ok {
			continue
		}
		s.cleanupCheckpointGrids(manager, existing, scriptFile, episode, groupIDs)

		// 超出单宫格容量的分组按档位切块，余下不足一档的块用小一档 + 占位符。
		chunks := chunkGroup(group, layout.CellCount)
		for _, chunk := range chunks {
			chunkIDs := make([]string, 0, len(chunk))
			for _, item := range chunk {
				chunkIDs = append(chunkIDs, storyboardIDOf(item, idField))
			}
			chunkLayout, ok := domain.CalculateGridLayout(len(chunkIDs), aspectRatio, allowLargeGrid)
			if !ok {
				continue
			}
			prompt := domain.BuildGridPrompt(chunk, idField, chunkLayout.Rows, chunkLayout.Cols,
				style, aspectRatio, chunkLayout.GridAspectRatio, nil)
			grid := domain.NewGridGeneration(
				episode, scriptFile, chunkIDs, chunkLayout.Rows, chunkLayout.Cols,
				chunkLayout.GridSize, "", "", aspectRatio, &prompt)
			if err := manager.Save(grid); err != nil {
				return nil, err
			}
			result, err := s.queue.EnqueueTask(ctx, repository.EnqueueParams{
				ProjectName: projectName,
				TaskType:    "grid",
				MediaType:   "image",
				ResourceID:  grid.ID,
				Payload:     gridTaskPayload(prompt, scriptFile, chunkIDs, chunkLayout, aspectRatio),
				ScriptFile:  &scriptFile,
				Source:      "webui",
				UserID:      userID,
			})
			if err != nil {
				return nil, err
			}
			gridIDs = append(gridIDs, grid.ID)
			taskIDs = append(taskIDs, result.TaskID)
			if !result.Deduped {
				allDeduped = false
			}
		}
	}
	return map[string]any{
		"success":  true,
		"grid_ids": gridIDs,
		"task_ids": taskIDs,
		// 批量语义：全部命中既有任务（本次一个新任务都没建）才算去重。
		"deduped":     len(taskIDs) > 0 && allDeduped,
		"message_key": "grid_task_submitted",
		"message_params": map[string]any{
			"count": len(gridIDs),
		},
	}, nil
}

// Capability 返回当前项目的宫格档位上限。
func (s *GridTaskService) Capability(ctx context.Context, projectName string) (map[string]any, error) {
	project, err := s.store.Get(projectName)
	if err != nil {
		return nil, service.GenerateBadRequest("invalid_project_name", map[string]any{"name": projectName})
	}
	allowed := service.ResolveLargeGridAllowed(ctx, s.images, project)
	return map[string]any{
		"large_grid_allowed": allowed,
		"max_cell_count":     domain.MaxCellCount(allowed),
	}, nil
}

// List 列出项目下全部宫格记录。
func (s *GridTaskService) List(projectName string) ([]any, error) {
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, service.GenerateBadRequest("invalid_project_name", map[string]any{"name": projectName})
	}
	manager, err := service.NewGridManager(projectDir)
	if err != nil {
		return nil, err
	}
	grids, err := manager.ListAll()
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(grids))
	for _, grid := range grids {
		out = append(out, grid)
	}
	return out, nil
}

// GridData 返回宫格记录本体（供传输层直接序列化，不带包装）。
func (s *GridTaskService) GridData(projectName, gridID string) (*domain.GridGeneration, error) {
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, service.GenerateBadRequest("invalid_project_name", map[string]any{"name": projectName})
	}
	return loadGridOrNotFound(projectDir, gridID)
}

// Regenerate 重置宫格状态并重新入队（不隐含落格）。
func (s *GridTaskService) Regenerate(
	ctx context.Context, projectName, gridID, userID string,
) (map[string]any, error) {
	project, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	if err := ensureGridWritable(project); err != nil {
		return nil, err
	}
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	manager, err := service.NewGridManager(projectDir)
	if err != nil {
		return nil, err
	}
	grid, err := loadGridOrNotFound(projectDir, gridID)
	if err != nil {
		return nil, err
	}
	// 重生成也是准入边界：改状态、入队之前先校验冻结记录里的源分镜。
	script, err := s.store.ReadScriptJSON(projectName, grid.ScriptFile)
	if err != nil {
		return nil, service.GenerateBadRequest("invalid_script_file", map[string]any{"name": grid.ScriptFile})
	}
	items, idField, fields := domain.StoryboardItems(script)
	wanted := map[string]bool{}
	for _, id := range grid.SceneIDs {
		wanted[id] = true
	}
	sourceItems := make([]map[string]any, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if wanted[storyboardIDOf(item, idField)] {
			sourceItems = append(sourceItems, item)
		}
	}
	if preflight := validateGridItems(project, projectDir, sourceItems, fields); len(preflight.Issues) > 0 {
		return nil, service.GenerateBadRequest("generation_asset_preflight_failed", preflight.I18NParams())
	}

	// prompt 里写死了画布比例：换用项目当前比例会让画布描述与下发参数自相矛盾。
	// 存量记录没有冻结值，回落到项目当前比例并就地补齐。
	aspectRatio := domain.VideoAspectRatioOf(project)
	if grid.VideoAspectRatio != nil && *grid.VideoAspectRatio != "" {
		aspectRatio = *grid.VideoAspectRatio
	}
	gridAspectRatio := domain.GridAspectRatioFor(grid.Rows, grid.Cols, aspectRatio)

	grid.Status = domain.GridStatusPending
	grid.ErrorMessage = nil
	// provider/model 留给执行侧按 needs_i2i 回填。
	grid.Provider = ""
	grid.Model = ""
	grid.VideoAspectRatio = &aspectRatio
	if err := manager.Save(grid); err != nil {
		return nil, err
	}

	prompt := ""
	if grid.Prompt != nil {
		prompt = *grid.Prompt
	}
	layout := domain.GridLayout{
		GridSize:        grid.GridSize,
		Rows:            grid.Rows,
		Cols:            grid.Cols,
		GridAspectRatio: gridAspectRatio,
		CellCount:       grid.CellCount,
	}
	result, err := s.queue.EnqueueTask(ctx, repository.EnqueueParams{
		ProjectName: projectName,
		TaskType:    "grid",
		MediaType:   "image",
		ResourceID:  grid.ID,
		Payload:     gridTaskPayload(prompt, grid.ScriptFile, grid.SceneIDs, layout, aspectRatio),
		ScriptFile:  &grid.ScriptFile,
		Source:      "webui",
		UserID:      userID,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"success": true, "task_id": result.TaskID, "deduped": result.Deduped}, nil
}

// Split 按当前联合图切分并覆写各分镜格。
func (s *GridTaskService) Split(ctx context.Context, projectName, gridID string) (map[string]any, error) {
	project, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	if err := ensureGridWritable(project); err != nil {
		return nil, err
	}
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	manager, err := service.NewGridManager(projectDir)
	if err != nil {
		return nil, err
	}
	grid, err := loadGridOrNotFound(projectDir, gridID)
	if err != nil {
		return nil, err
	}
	if err := ensureGridIdle(grid); err != nil {
		return nil, err
	}
	imagePath, err := manager.ImagePath(gridID)
	if err != nil {
		return nil, err
	}
	if grid.GridImagePath == nil || *grid.GridImagePath == "" || !fileExists(imagePath) {
		return nil, service.GenerateBadRequest("grid_image_not_ready", map[string]any{"grid_id": gridID})
	}
	if s.split == nil {
		return nil, errors.New("grid split service is not configured")
	}
	result, err := s.split.ApplySplit(ctx, projectName, grid)
	if err != nil {
		if errors.Is(err, service.GridImageNotReadyError) {
			// 与上方预检之间存在「文件被并发删除」的窗口。
			return nil, service.GenerateBadRequest("grid_image_not_ready", map[string]any{"grid_id": gridID})
		}
		return nil, err
	}
	return map[string]any{
		"success":            true,
		"split_at":           grid.SplitAt,
		"updated_scene_ids":  result.UpdatedSceneIDs,
		"missing_scene_ids":  result.MissingSceneIDs,
		"asset_fingerprints": ComputeAffectedFingerprints(s.store, projectName, "grid_split", gridID),
	}, nil
}

// UploadGridImage 用上传的字节替换联合图（格式归一化 → 补登旧版本 → 落盘 → 登记新版本）。
//
// 参数 pngBytes 必须是已归一化的 PNG：归一化（格式转换 / EXIF 定向 / 像素上限）由
// infra/img 承担，本方法只负责版本与记录副作用，便于两者各自单测。
func (s *GridTaskService) UploadGridImage(
	ctx context.Context, projectName, gridID, originalFilename string, pngBytes []byte,
) (map[string]any, error) {
	project, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	if err := ensureGridWritable(project); err != nil {
		return nil, err
	}
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	manager, err := service.NewGridManager(projectDir)
	if err != nil {
		return nil, err
	}
	grid, err := loadGridOrNotFound(projectDir, gridID)
	if err != nil {
		return nil, err
	}
	if err := ensureGridIdle(grid); err != nil {
		return nil, err
	}
	target, err := manager.ImagePath(gridID)
	if err != nil {
		return nil, err
	}
	versions := service.NewVersions(projectDir)
	// 旧联合图若从未入版本库（历史迁移等），先补登，避免被覆盖后字节丢失。
	if _, err := versions.EnsureCurrentTracked("grids", gridID, "", target, nil); err != nil {
		return nil, err
	}
	if err := service.WriteFileAtomic(target, pngBytes); err != nil {
		return nil, err
	}
	metadata := map[string]any{"source": service.UploadVersionSource}
	if originalFilename != "" {
		metadata["original_filename"] = originalFilename
	}
	version, _, err := versions.AddVersion("grids", gridID, "", target, metadata)
	if err != nil {
		return nil, err
	}

	// 手动补图等价于一次成功的联合图产出：failed 记录回到就绪态，split_at 清空表示待切分；
	// 补的图按项目当前比例排布，冻结值随之改写（沿用旧值会在项目改过比例后把新图按旧比例裁切）。
	grid.MarkCompositeReplaced()
	aspectRatio := domain.VideoAspectRatioOf(project)
	grid.VideoAspectRatio = &aspectRatio
	if err := manager.Save(grid); err != nil {
		return nil, err
	}
	return map[string]any{
		"success":            true,
		"path":               "grids/" + gridID + ".png",
		"version":            version,
		"asset_fingerprints": ComputeAffectedFingerprints(s.store, projectName, "grid", gridID),
	}, nil
}

// ---- 内部 ----

// cleanupCheckpointGrids 清理同脚本同集、且 scene_ids 是当前组子集的旧记录。
//
// 跳过 pending/generating：worker 还在用那条记录，删掉会让它执行时找不到资源。
func (s *GridTaskService) cleanupCheckpointGrids(
	manager *service.GridManager, existing []*domain.GridGeneration, scriptFile string, episode int, groupIDs []string,
) {
	groupSet := map[string]bool{}
	for _, id := range groupIDs {
		groupSet[id] = true
	}
	for _, old := range existing {
		if old.ScriptFile != scriptFile || old.Episode != episode {
			continue
		}
		if old.Status == domain.GridStatusPending || old.Status == domain.GridStatusGenerating {
			continue
		}
		if len(old.SceneIDs) == 0 {
			continue
		}
		subset := true
		for _, id := range old.SceneIDs {
			if !groupSet[id] {
				subset = false
				break
			}
		}
		if subset {
			_, _ = manager.Delete(old.ID)
		}
	}
}

// validateGridItems 合并多条分镜的资产引用校验结果。
func validateGridItems(
	project *domain.ProjectFile, projectDir string, items []map[string]any, fields domain.StoryboardItemFields,
) service.PreflightResult {
	results := make([]service.PreflightResult, 0, len(items))
	for _, item := range items {
		results = append(results, service.ValidateItemAssetReferences(project, projectDir, item, fields))
	}
	return service.MergePreflightResults(results...)
}

// loadGridOrNotFound 按 ID 取宫格记录；格式非法与不存在都是 404。
func loadGridOrNotFound(projectDir, gridID string) (*domain.GridGeneration, error) {
	manager, err := service.NewGridManager(projectDir)
	if err != nil {
		return nil, err
	}
	grid, found, err := manager.Get(gridID)
	if err != nil {
		return nil, generateNotFound("grid_not_found", map[string]any{"grid_id": gridID})
	}
	if !found || grid == nil {
		return nil, generateNotFound("grid_not_found", map[string]any{"grid_id": gridID})
	}
	return grid, nil
}

// ensureGridIdle 拒绝在途（pending/generating）的宫格被切分或上传：
// worker 完成时会覆写联合图，与刚上传的图或按旧图的切分互相踩踏。
func ensureGridIdle(grid *domain.GridGeneration) error {
	if grid.Status == domain.GridStatusPending || grid.Status == domain.GridStatusGenerating {
		return generateConflict("grid_generation_in_progress")
	}
	return nil
}

// gridTaskPayload 构造一致的宫格入队载荷。
//
// 入队不携带 provider：provider 由执行侧按当前项目配置解析（ADR 0001），
// 因为只有 task 层能按 reference_images 判断走 t2i 还是 i2i 槽。
func gridTaskPayload(
	prompt, scriptFile string, sceneIDs []string, layout domain.GridLayout, videoAspectRatio string,
) map[string]any {
	return map[string]any{
		"prompt":             prompt,
		"script_file":        scriptFile,
		"scene_ids":          sceneIDs,
		"grid_size":          layout.GridSize,
		"rows":               layout.Rows,
		"cols":               layout.Cols,
		"grid_aspect_ratio":  layout.GridAspectRatio,
		"video_aspect_ratio": videoAspectRatio,
	}
}

// chunkGroup 按单宫格容量把分组切块（余下不足一档的块单独成块，由档位阶梯回落）。
func chunkGroup(group []map[string]any, cellCount int) [][]map[string]any {
	if cellCount <= 0 || len(group) <= cellCount {
		return [][]map[string]any{group}
	}
	chunks := make([][]map[string]any, 0, (len(group)+cellCount-1)/cellCount)
	for i := 0; i < len(group); i += cellCount {
		end := i + cellCount
		if end > len(group) {
			end = len(group)
		}
		chunks = append(chunks, group[i:end])
	}
	return chunks
}

// storyboardIDOf 读分镜条目的标识。
func storyboardIDOf(item map[string]any, idField string) string {
	return service.PythonStrLike(item[idField])
}

// fileExists 判断路径是否为已存在的普通文件。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// sortedUnique 排序去重字符串（响应里的 missing_scene_ids 需要稳定序）。
func sortedUnique(values []string) []any {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	items := make([]any, 0, len(out))
	for _, value := range out {
		items = append(items, value)
	}
	return items
}

// gridImageRelativePath 是联合图在项目内的相对路径（版本与指纹共用）。
func gridImageRelativePath(gridID string) string {
	return filepath.ToSlash(filepath.Join("grids", gridID+".png"))
}

// describeGridRoute 仅供日志与调试：把关键参数压成一行。
func describeGridRoute(projectName, gridID string) string {
	return fmt.Sprintf("project=%s grid=%s", projectName, gridID)
}


// sortedSceneIDsCSV 是错误信息与日志用的稳定拼接。
func sortedSceneIDsCSV(ids []string) string {
	copied := append([]string(nil), ids...)
	sort.Strings(copied)
	return strings.Join(copied, ",")
}
