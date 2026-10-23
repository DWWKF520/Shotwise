// 宫格联合图生成任务（对应 server/services/generation_tasks.py::execute_grid_task）。
//
// resource_id 即 grid_id。步骤：加载记录 → 置 generating → 收集参考图 → 生成联合图
// （按资源类型 "grids" 记账与登记版本）→ 置 completed。只产出联合图，不触碰任何分镜格；
// 落格由独立的切分操作（service.GridService.ApplySplit）显式执行。
package generation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shotwise/shotwise/internal/config"
	domaingrid "github.com/shotwise/shotwise/internal/domain/grid"
	"github.com/shotwise/shotwise/internal/domain/media"
	"github.com/shotwise/shotwise/internal/domain/project"
	domaintask "github.com/shotwise/shotwise/internal/domain/task"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
)

// decodeScriptJSON 把剧本 JSON 文本解成通用对象（宫格参考图收集只做只读遍历）。
func decodeScriptJSON(raw []byte) (map[string]any, error) {
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("剧本 JSON 解析失败: %w", err)
	}
	return out, nil
}

// gridMaxReferenceImages 是参考图上限（与 Python 侧 max_count 一致）。
const gridMaxReferenceImages = 6

// executeGrid 生成宫格联合图并写回记录状态。
func (d *TaskDispatcher) executeGrid(ctx context.Context, task *repository.TaskData) (map[string]any, error) {
	if d.images == nil {
		return nil, fmt.Errorf("grid task requires an image generator")
	}
	projectDir, err := d.store.Dir(task.ProjectName)
	if err != nil {
		return nil, err
	}
	pf, err := d.store.Get(task.ProjectName)
	if err != nil {
		return nil, err
	}
	manager, err := service.NewGridManager(projectDir)
	if err != nil {
		return nil, err
	}
	grid, ok, err := manager.Get(task.ResourceID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("grid not found: %s", task.ResourceID)
	}

	// 失败一律把记录翻 failed 并留脱敏后的错误原因，再向上抛（与 Python 侧同语义）。
	fail := func(cause error) (map[string]any, error) {
		grid.Status = domaingrid.GridStatusFailed
		message := domaintask.BoundReason(domaintask.SanitizeFailureReason(cause.Error()), 2000)
		grid.ErrorMessage = &message
		_ = manager.Save(grid)
		return nil, cause
	}

	grid.Status = domaingrid.GridStatusGenerating
	grid.ErrorMessage = nil
	if err := manager.Save(grid); err != nil {
		return nil, err
	}

	refs, refMetadata, err := collectGridReferenceImages(pf, projectDir, task.Payload, grid.SceneIDs)
	if err != nil {
		return fail(err)
	}
	grid.ReferenceImages = refMetadata
	if err := manager.Save(grid); err != nil {
		return nil, err
	}

	promptText := payloadString(task.Payload, "prompt")
	if promptText == "" && grid.Prompt != nil {
		promptText = *grid.Prompt
	}
	if promptText == "" {
		return fail(fmt.Errorf("prompt is required for grid task"))
	}

	capability := config.ImageCapT2I
	if len(refs) > 0 {
		capability = config.ImageCapI2I
	}
	aspectRatio := payloadString(task.Payload, "grid_aspect_ratio")
	if aspectRatio == "" {
		aspectRatio = service.AspectRatioFor(pf, "storyboards")
	}

	// 回填 grid metadata：创建/重建时无法预知 needs_i2i，由此处补齐。
	selected, err := d.images.resolver.ResolveImageBackend(ctx, pf, task.Payload, capability)
	if err != nil {
		return fail(err)
	}
	grid.Provider = selected.ProviderID
	grid.Model = selected.ModelID
	if err := manager.Save(grid); err != nil {
		return nil, err
	}

	// 保底档与档位门控取自同一常量，避免门控按 2K 判定、渲染却按别的档位下发。
	imageSize := service.ResolveImageResolutionFor(ctx, d.images.resolver, pf, task.Payload, capability)
	if imageSize == "" {
		imageSize = domaingrid.GRIDFallbackResolution
	}

	outRel, err := service.ResourceRelativePath("grids", task.ResourceID)
	if err != nil {
		return nil, err
	}
	outPath, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(outRel))
	if err != nil {
		return nil, fmt.Errorf("非法资源 ID: %s", task.ResourceID)
	}
	// 旧联合图若已存在（重生成路径）先补登版本，否则重生成会原地覆盖、历史内容不可回溯。
	versions := service.NewVersions(projectDir)
	if _, err := versions.EnsureCurrentTracked("grids", task.ResourceID, promptText, outPath, map[string]any{
		"aspect_ratio": aspectRatio,
	}); err != nil {
		return nil, err
	}
	if _, err := d.images.Generate(ctx, ImageGenerateParams{
		ProjectName: task.ProjectName,
		Project:     pf,
		Capability:  capability,
		Request:     gridImageRequest(promptText, outPath, task.ProjectName, aspectRatio, imageSize, refs),
		UserID:      task.UserID,
		SegmentID:   segmentIDFor("image", "grids", task.ResourceID),
	}); err != nil {
		return fail(err)
	}

	// 联合图内容已更新，旧的落格结果不再对应当前图：置 completed 并清空 split_at（待显式切分）。
	imagePath := outRel
	grid.GridImagePath = &imagePath
	grid.Status = domaingrid.GridStatusCompleted
	grid.SplitAt = nil
	if err := manager.Save(grid); err != nil {
		return nil, err
	}

	version, _, err := versions.AddVersion("grids", task.ResourceID, promptText, outPath, map[string]any{
		"aspect_ratio": aspectRatio,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"version":       version,
		"file_path":     outRel,
		"created_at":    grid.CreatedAt,
		"resource_type": "grids",
		"resource_id":   task.ResourceID,
	}, nil
}

// gridImageRequest 组装宫格联合图的生成请求。
func gridImageRequest(
	prompt, outPath, projectName, aspectRatio, imageSize string, refs []provider.ReferenceImage,
) provider.ImageGenerationRequest {
	request := provider.NewImageGenerationRequest(prompt, outPath)
	request.ReferenceImages = refs
	request.AspectRatio = aspectRatio
	request.ImageSize = imageSize
	request.ProjectName = projectName
	return request
}

// collectGridReferenceImages 收集宫格分组内分镜引用的角色/场景/道具设计图（对应 Python
// _collect_grid_reference_images）：最多 6 张，按分镜顺序、条目内角色→场景→道具顺序去重。
func collectGridReferenceImages(
	pf *project.ProjectFile, projectDir string, payload map[string]any, sceneIDs []string,
) ([]provider.ReferenceImage, []domaingrid.GridReferenceImage, error) {
	scriptFile := payloadString(payload, "script_file")
	if scriptFile == "" {
		return nil, nil, nil
	}
	// 读原始 project.json 只是为了拿到剧本；剧本从 scripts/ 目录读，缺失即按无参考处理。
	scriptPath, err := fs.SafeJoin(projectDir, false, "scripts", filepath.FromSlash(scriptFile))
	if err != nil {
		return nil, nil, nil
	}
	raw, err := os.ReadFile(scriptPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	script, err := decodeScriptJSON(raw)
	if err != nil {
		return nil, nil, err
	}

	items, idField, fields := media.StoryboardItems(script)
	wanted := map[string]bool{}
	for _, id := range sceneIDs {
		wanted[id] = true
	}

	var (
		refs     []provider.ReferenceImage
		metadata []domaingrid.GridReferenceImage
	)
	seen := map[string]bool{}
	for _, item := range items {
		if len(refs) >= gridMaxReferenceImages {
			break
		}
		entry, ok := item.(map[string]any)
		if !ok || !wanted[fmt.Sprint(entry[idField])] {
			continue
		}
		for _, group := range []struct {
			kind  project.AssetKind
			field string
		}{
			{project.AssetCharacter, fields.CharField},
			{project.AssetScene, fields.SceneField},
			{project.AssetProp, fields.PropField},
		} {
			if group.field == "" {
				continue
			}
			names := service.StringList(entry[group.field])
			for _, name := range names {
				if len(refs) >= gridMaxReferenceImages {
					break
				}
				sheet := assetSheetPath(pf, group.kind, name)
				if sheet == "" || seen[sheet] {
					continue
				}
				full := fs.TrySafeJoin(projectDir, false, filepath.FromSlash(sheet))
				if full == "" {
					continue
				}
				if info, err := os.Stat(full); err != nil || info.IsDir() {
					continue
				}
				seen[sheet] = true
				refs = append(refs, provider.ReferenceImage{Path: full})
				metadata = append(metadata, domaingrid.GridReferenceImage{
					Path: sheet, Name: name, RefType: string(group.kind),
				})
			}
		}
	}
	return refs, metadata, nil
}

// assetSheetPath 取资产桶里该资产的设计图相对路径（桶/资产缺失或未出图时为空串）。
func assetSheetPath(pf *project.ProjectFile, kind project.AssetKind, name string) string {
	spec, ok := project.Spec(kind)
	if !ok {
		return ""
	}
	bucket, ok := pf.BucketRef(kind)
	if !ok || bucket == nil {
		return ""
	}
	key, ok := bucket.ResolveKey(name)
	if !ok {
		return ""
	}
	entry := (*bucket)[key]
	if entry == nil {
		return ""
	}
	return entry.MustString(spec.SheetKey)
}
