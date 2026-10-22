// 图片指令式编辑（image_edit）任务执行（对应 server/services/image_edit_tasks.py）。
//
// 编辑语义（见 docs/adr/0050）：以资源的当前图为唯一参考图、用户编辑指令为唯一 prompt 走
// i2i，产出新图覆盖 canonical 路径并登记版本；原 image_prompt 不回写——编辑是对**图**的
// 分叉而非对 prompt 的分叉。因此模板与执行共用 resolve_current_image_rel 的读图口径。
package generation

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
)

// imageEditVersionSource 是版本记录里标记「指令式编辑」的 source 值，前端据此展示编辑标记
// （对应 Python IMAGE_EDIT_VERSION_SOURCE）。
const imageEditVersionSource = "image_edit"

// executeImageEdit 执行图片编辑任务：读当前图 → i2i → 新版本覆盖 canonical → 按资源类型写回。
//
// 编辑必然 i2i（唯一入队即知 capability 的图片任务，见 docs/adr/0001），故恒定走 i2i 槽；
// backend 调用失败时 canonical 图与资源写回都不被触碰（生成失败在 service.Generate 内已记账）。
// 旧图基线登记（service.EnsureCurrentTracked）先于 backend 调用发生，与成败无关、失败不回滚——
// 保证编辑前的旧图始终可回滚。
func (d *TaskDispatcher) executeImageEdit(ctx context.Context, task *repository.TaskData) (map[string]any, error) {
	if d.images == nil {
		return nil, fmt.Errorf("image_edit task requires an image generator")
	}
	resourceType := payloadString(task.Payload, "resource_type")
	if !IsEditableResourceType(resourceType) {
		return nil, fmt.Errorf("unsupported image_edit resource_type: %s", resourceType)
	}
	instruction := strings.TrimSpace(payloadString(task.Payload, "prompt"))
	if instruction == "" {
		return nil, fmt.Errorf("instruction (payload.prompt) is required for image_edit task")
	}
	scriptFile := payloadString(task.Payload, "script_file")
	if resourceType == StoryboardResourceType && scriptFile == "" {
		return nil, fmt.Errorf("script_file is required for storyboard image_edit task")
	}
	if id := strings.TrimSpace(payloadString(task.Payload, "reference_media_asset_id")); id != "" {
		// 「参考图取自项目媒体资产」的分支依赖媒体资产服务，未迁移：fail loud，不静默丢弃该参考图。
		return nil, fmt.Errorf("image_edit reference_media_asset_id is not migrated yet")
	}
	versionResourceType, ok := EditVersionResourceType(resourceType)
	if !ok {
		return nil, fmt.Errorf("unsupported image_edit resource_type: %s", resourceType)
	}

	pf, err := d.store.Get(task.ProjectName)
	if err != nil {
		return nil, err
	}
	projectDir, err := d.store.Dir(task.ProjectName)
	if err != nil {
		return nil, err
	}

	var script map[string]any
	if resourceType == StoryboardResourceType {
		if script, err = d.store.ReadScriptJSON(task.ProjectName, scriptFile); err != nil {
			return nil, err
		}
	}

	// 资产名可能以 NFC/NFD 任一形态传入：先解析真实落盘 key，之后的版本登记与 canonical 图
	// 路径统一按它写，避免同一资产落出两种编码形式的文件与版本记录。
	key := task.ResourceID
	if resourceType != StoryboardResourceType {
		if bucket, ok := pf.BucketRef(domain.AssetKind(resourceType)); ok {
			if resolved, found := bucket.ResolveKey(task.ResourceID); found {
				key = resolved
			}
		}
	}

	currentRel, found := ResolveCurrentImageRel(pf, resourceType, key, script)
	var currentAbs string
	if found && currentRel != "" {
		currentAbs = fs.TrySafeJoin(projectDir, false, filepath.FromSlash(currentRel))
	}
	if currentAbs == "" {
		return nil, fmt.Errorf("no current image to edit: %s/%s", resourceType, task.ResourceID)
	}

	outRel, err := service.ResourceRelativePath(versionResourceType, key)
	if err != nil {
		return nil, err
	}
	outPath, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(outRel))
	if err != nil {
		return nil, fmt.Errorf("非法资源 ID: %s", key)
	}

	versions := service.NewVersions(projectDir)
	if _, err := versions.EnsureCurrentTracked(versionResourceType, key, "", currentAbs, nil); err != nil {
		return nil, err
	}

	aspectRatio := service.AspectRatioFor(pf, versionResourceType)
	// 参考图仅当前图一张、prompt 仅编辑指令（不拼原 image_prompt、不追加生成路径的自动参考图收集）。
	request := provider.NewImageGenerationRequest(instruction, outPath)
	request.ReferenceImages = []provider.ReferenceImage{{Path: currentAbs}}
	request.AspectRatio = aspectRatio
	if size := payloadString(task.Payload, "image_size"); size != "" {
		request.ImageSize = size
	}
	request.ProjectName = task.ProjectName

	if _, err := d.images.Generate(ctx, ImageGenerateParams{
		ProjectName: task.ProjectName,
		Project:     pf,
		Capability:  config.ImageCapI2I,
		Request:     request,
		UserID:      task.UserID,
		SegmentID:   segmentIDFor("image", versionResourceType, key),
	}); err != nil {
		return nil, err
	}

	if resourceType == StoryboardResourceType {
		if err := d.writeScriptGeneratedAsset(task.ProjectName, scriptFile, key, "storyboard_image", outRel); err != nil {
			return nil, err
		}
	} else if err := d.writeAssetSheet(task.ProjectName, resourceType, key, outRel); err != nil {
		return nil, err
	}

	version, createdAt, err := versions.AddVersion(versionResourceType, key, instruction, outPath, map[string]any{
		"aspect_ratio": aspectRatio,
		"source":       imageEditVersionSource,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"version":       version,
		"file_path":     outRel,
		"created_at":    createdAt,
		"resource_type": versionResourceType,
		"resource_id":   key,
	}, nil
}

// writeAssetSheet 把编辑产物路径写回资产 sheet 字段（对应 Python _update_asset_sheet）：
// 项目锁内读改写，经「不更坏」校验落盘。
func (d *TaskDispatcher) writeAssetSheet(project, resourceType, key, assetPath string) error {
	spec, ok := domain.Spec(domain.AssetKind(resourceType))
	if !ok {
		return fmt.Errorf("unknown asset kind: %s", resourceType)
	}
	return d.store.Update(project, func(p *domain.ProjectFile) error {
		b, ok := p.BucketRef(domain.AssetKind(resourceType))
		if !ok {
			return fmt.Errorf("unknown asset kind: %s", resourceType)
		}
		k, ok := b.ResolveKey(key)
		if !ok {
			return fmt.Errorf("%s not found: %s", resourceType, key)
		}
		(*b)[k].SetString(spec.SheetKey, assetPath)
		return nil
	})
}
