package generation

import "github.com/shotwise/shotwise/internal/service"

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

// executeDesign 执行 scene / prop / product 设计图（对应 Python execute_design_task）：
// 构造 prompt → （product 带原图参考）→ 生成 → 写回 sheet 字段 → 登记版本。
//
// character 不在此路径：其后还需头像裁剪（lib/character_avatar），依赖未迁移的 infra/img，
// 由调用方 fail loud。
func (d *TaskDispatcher) executeDesign(ctx context.Context, task *repository.TaskData, kind domain.AssetKind) (map[string]any, error) {
	spec, ok := domain.Spec(kind)
	if !ok {
		return nil, fmt.Errorf("unknown asset kind: %s", kind)
	}
	raw := payloadString(task.Payload, "prompt")
	if raw == "" {
		return nil, fmt.Errorf("prompt is required for %s task", kind)
	}
	pf, err := d.store.Get(task.ProjectName)
	if err != nil {
		return nil, err
	}
	projectDir, err := d.store.Dir(task.ProjectName)
	if err != nil {
		return nil, err
	}
	bucket, ok := pf.BucketRef(kind)
	if !ok {
		return nil, fmt.Errorf("unknown asset kind: %s", kind)
	}
	key, ok := bucket.ResolveKey(task.ResourceID)
	if !ok {
		return nil, fmt.Errorf("%s not found: %s", kind, task.ResourceID)
	}
	entry := (*bucket)[key]

	var prompt string
	var refs []provider.ReferenceImage
	switch kind {
	case domain.AssetScene:
		prompt = domain.BuildScenePrompt(key, raw, pf.Style, "")
	case domain.AssetProp:
		prompt = domain.BuildPropPrompt(key, raw, pf.Style, "")
	case domain.AssetProduct:
		// 产品 sheet 由实拍原图整理而来：原图全量作为 i2i 参考，缺失文件跳过。
		prompt = domain.BuildProductPrompt(key, raw)
		refs = existingReferenceImages(projectDir, entry)
	default:
		return nil, fmt.Errorf("design task not supported for %s", kind)
	}

	outRel, err := service.ResourceRelativePath(spec.BucketKey, key)
	if err != nil {
		return nil, err
	}
	outPath, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(outRel))
	if err != nil {
		return nil, fmt.Errorf("非法资源 ID: %s", task.ResourceID)
	}

	request := provider.NewImageGenerationRequest(prompt, outPath)
	request.ReferenceImages = refs
	request.AspectRatio = service.AspectRatioFor(pf, spec.BucketKey)
	if size := payloadString(task.Payload, "image_size"); size != "" {
		request.ImageSize = size
	}
	request.ProjectName = task.ProjectName

	capability := config.ImageCapT2I
	if len(refs) > 0 {
		capability = config.ImageCapI2I
	}
	if _, err := d.images.Generate(ctx, ImageGenerateParams{
		ProjectName: task.ProjectName,
		Project:     pf,
		Capability:  capability,
		Request:     request,
		UserID:      task.UserID,
		SegmentID:   segmentIDFor("image", spec.BucketKey, key),
	}); err != nil {
		return nil, err
	}

	// 写回 sheet 字段（对应 _update_asset_sheet）：项目锁内读改写，经「不更坏」校验落盘。
	if err := d.store.Update(task.ProjectName, func(p *domain.ProjectFile) error {
		b, ok := p.BucketRef(kind)
		if !ok {
			return fmt.Errorf("unknown asset kind: %s", kind)
		}
		k, ok := b.ResolveKey(key)
		if !ok {
			return fmt.Errorf("%s not found: %s", kind, key)
		}
		(*b)[k].SetString(spec.SheetKey, outRel)
		return nil
	}); err != nil {
		return nil, err
	}

	return finishImageResult(projectDir, spec.BucketKey, key, raw, request.AspectRatio)
}

// existingReferenceImages 解析产品原图列表：逐项 SafeJoin，越界、缺失或指向目录的条目跳过；
// 全部缺失时返回 nil（退化为纯文生图，与 Python _collect_product_reference_images 一致）。
func existingReferenceImages(projectDir string, entry *domain.AssetEntry) []provider.ReferenceImage {
	if entry == nil {
		return nil
	}
	refs, ok := entry.List("reference_images")
	if !ok {
		return nil
	}
	var out []provider.ReferenceImage
	for _, rel := range refs {
		full := fs.TrySafeJoin(projectDir, false, filepath.FromSlash(rel))
		if full == "" {
			continue
		}
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			continue
		}
		out = append(out, provider.ReferenceImage{Path: full})
	}
	return out
}
