// 创意画布图片任务（对应 server/services/canvas_image_tasks.py 的入队侧口径）。
//
// 画布操作刻意不触碰项目资产与剧本宫格记录：每次操作都保留源图，产出独立的
// MediaAsset。本文件只负责「校验 + 入队」，真正的执行在 worker 侧
// （tasks_canvas.go），两者共用同一份操作白名单与源解析口径。
package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/repository"
)

// CanvasImageOperationSplit 是宫格切分操作标识（提交路径的默认操作）。
const CanvasImageOperationSplit = "canvas_image_split"

// CanvasImageOperations 是创意画布图片任务的合法操作集合（对应 CANVAS_IMAGE_OPERATIONS）。
var CanvasImageOperations = map[string]struct{}{
	"canvas_image_split":    {},
	"canvas_image_panorama": {},
	"canvas_image_angles":   {},
	"canvas_image_layers":   {},
	"canvas_image_hd":       {},
	"canvas_image_outpaint": {},
	"canvas_image_redraw":   {},
	"canvas_image_erase":    {},
	"canvas_image_cutout":   {},
	"canvas_image_crop":     {},
}

// CanvasImageRequest 是画布任务的提交体（两个端点共用一个结构，字段按操作取舍）。
type CanvasImageRequest struct {
	SourceKind        string
	ResourceType      *string
	ResourceID        *string
	MediaAssetID      *string
	ScriptFile        *string
	Rows              *int
	Cols              *int
	IncludeSplitLines *bool
	Operation         *string
	Instruction       *string
	Count             *int
	Region            map[string]any
	AspectRatio       *string
	Multiplier        *int
	Quality           *string
}

// Payload 序列化为入队载荷，语义等同 Python 的 model_dump(exclude_none=True)：
// 未提供的字段不进载荷，执行侧才能区分「没给」与「给了零值」。
func (r CanvasImageRequest) Payload() map[string]any {
	out := map[string]any{"source_kind": r.SourceKind}
	putString := func(key string, v *string) {
		if v != nil {
			out[key] = *v
		}
	}
	putInt := func(key string, v *int) {
		if v != nil {
			out[key] = *v
		}
	}
	putString("resource_type", r.ResourceType)
	putString("resource_id", r.ResourceID)
	putString("media_asset_id", r.MediaAssetID)
	putString("script_file", r.ScriptFile)
	putInt("rows", r.Rows)
	putInt("cols", r.Cols)
	if r.IncludeSplitLines != nil {
		out["include_split_lines"] = *r.IncludeSplitLines
	}
	putString("operation", r.Operation)
	putString("instruction", r.Instruction)
	putInt("count", r.Count)
	if r.Region != nil {
		out["region"] = r.Region
	}
	putString("aspect_ratio", r.AspectRatio)
	putInt("multiplier", r.Multiplier)
	putString("quality", r.Quality)
	return out
}

// CanvasResourceID 派生去重用的资源标识（对应 _canvas_resource_id）。
//
// 项目内来源用 "<resource_type>:<resource_id>"，媒体库来源用 "media:<id>"；
// 来源不成立时返回 ok=false——调用方统一翻成 canvas_image_source_invalid。
func CanvasResourceID(r CanvasImageRequest) (string, bool) {
	switch r.SourceKind {
	case "project":
		if r.ResourceType != nil && *r.ResourceType != "" && r.ResourceID != nil && *r.ResourceID != "" {
			return *r.ResourceType + ":" + *r.ResourceID, true
		}
	case "media":
		if r.MediaAssetID != nil && strings.TrimSpace(*r.MediaAssetID) != "" {
			return "media:" + strings.TrimSpace(*r.MediaAssetID), true
		}
	}
	return "", false
}

// ValidateCanvasGrid 校验宫格行列数（对应 _validate_grid）：2..8 的整数。
func ValidateCanvasGrid(rows, cols int) bool {
	return rows >= 2 && rows <= 8 && cols >= 2 && cols <= 8
}

// SplitCanvasImage 提交宫格切分任务（不触碰分镜 Grid 记录）。
//
// 校验顺序与 Python 一致：先宫格参数、再资源标识，任一不成立都是 400
// canvas_image_source_invalid（不是 422）；源图解析失败同码。
func (s *GenerateService) SplitCanvasImage(
	ctx context.Context, projectName string, req CanvasImageRequest, userID string,
) (map[string]any, error) {
	if req.Rows == nil || req.Cols == nil || !ValidateCanvasGrid(*req.Rows, *req.Cols) {
		return nil, generateBadRequest("canvas_image_source_invalid", nil)
	}
	resourceID, ok := CanvasResourceID(req)
	if !ok {
		return nil, generateBadRequest("canvas_image_source_invalid", nil)
	}
	return s.enqueueCanvasImage(ctx, projectName, CanvasImageOperationSplit, req.Payload(), resourceID, userID)
}

// AdvancedCanvasImage 提交创意画布图片操作（不改写源资产）。
func (s *GenerateService) AdvancedCanvasImage(
	ctx context.Context, projectName string, req CanvasImageRequest, userID string,
) (map[string]any, error) {
	operation := ""
	if req.Operation != nil {
		operation = *req.Operation
	}
	if _, ok := CanvasImageOperations[operation]; !ok {
		return nil, generateBadRequest("canvas_image_operation_invalid", map[string]any{"operation": operation})
	}
	resourceID, ok := CanvasResourceID(req)
	if !ok {
		return nil, generateBadRequest("canvas_image_source_invalid", nil)
	}
	return s.enqueueCanvasImage(ctx, projectName, operation, req.Payload(), resourceID, userID)
}

// enqueueCanvasImage 校验源图后入队。
//
// 源图校验前置到入队前：解析不出的源图不该变成一条异步失败的任务。
func (s *GenerateService) enqueueCanvasImage(
	ctx context.Context, projectName, operation string, payload map[string]any, resourceID, userID string,
) (map[string]any, error) {
	if err := s.validateCanvasImageSource(projectName, payload); err != nil {
		return nil, generateBadRequest("canvas_image_source_invalid", nil)
	}
	taskPayload := map[string]any{"prompt": operation}
	for key, value := range payload {
		taskPayload[key] = value
	}
	result, err := s.queue.EnqueueTask(ctx, repository.EnqueueParams{
		ProjectName: projectName,
		TaskType:    operation,
		MediaType:   "image",
		ResourceID:  resourceID,
		Payload:     taskPayload,
		Source:      "webui",
		UserID:      userID,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"success": true,
		"task_id": result.TaskID,
		"deduped": result.Deduped,
	}, nil
}

// validateCanvasImageSource 解析画布任务的源图，解析不出即拒绝（对应
// resolve_canvas_image_source 的校验部分）。
//
// Python 侧此处返回 (project_root, path, source_id)，只有 path 用于「能不能读」；
// 真正的读图发生在执行侧，因此这里只需保证源图存在且落在项目内。
func (s *GenerateService) validateCanvasImageSource(projectName string, payload map[string]any) error {
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return err
	}
	sourceKind, _ := payload["source_kind"].(string)
	if sourceKind == "media" {
		mediaID, _ := payload["media_asset_id"].(string)
		mediaID = strings.TrimSpace(mediaID)
		if mediaID == "" {
			return errors.New("media_asset_id is required for media source")
		}
		return resolveCanvasMediaSource(projectDir, mediaID)
	}
	if sourceKind != "project" {
		return errors.New("source_kind must be project or media")
	}
	resourceType, _ := payload["resource_type"].(string)
	if !isCanvasProjectResourceType(resourceType) {
		return errors.New("unsupported project image resource_type")
	}
	resourceID, _ := payload["resource_id"].(string)
	resourceID = strings.TrimSpace(resourceID)
	if resourceID == "" {
		return errors.New("resource_id is required for project source")
	}
	project, err := s.store.Get(projectName)
	if err != nil {
		return err
	}
	var script map[string]any
	if resourceType == StoryboardResourceType {
		scriptFile, _ := payload["script_file"].(string)
		if script, err = s.store.ReadScriptJSON(projectName, scriptFile); err != nil {
			return err
		}
	}
	currentRel, found := ResolveCurrentImageRel(project, resourceType, resourceID, script)
	if !found || currentRel == "" {
		return errors.New("project image does not exist")
	}
	if !fs.SafeExists(projectDir, filepath.FromSlash(currentRel)) {
		return errors.New("project image does not exist")
	}
	return nil
}

// resolveCanvasMediaSource 校验媒体库来源的图片资产可用（对应
// resolve_reference_media_asset_path：必须是图片且文件在项目内存在）。
func resolveCanvasMediaSource(projectDir, mediaAssetID string) error {
	asset, err := GetProjectMediaAsset(projectDir, mediaAssetID)
	if err != nil || asset == nil {
		return errors.New("reference media asset not found")
	}
	if kind, _ := asset["kind"].(string); kind != "image" {
		return errors.New("reference media asset must be an image")
	}
	physical, _ := asset["physical_path"].(string)
	if physical == "" {
		return errors.New("reference media asset has no file path")
	}
	if !fs.SafeExists(projectDir, filepath.FromSlash(physical)) {
		return errors.New("reference media asset file is missing")
	}
	return nil
}

// isCanvasProjectResourceType 判定资源类型是否属于画布来源白名单（四类资产 + 分镜图）。
func isCanvasProjectResourceType(resourceType string) bool {
	if resourceType == StoryboardResourceType {
		return true
	}
	_, ok := domain.Spec(domain.AssetKind(resourceType))
	return ok
}

// canvasGridDims 把载荷里的 rows/cols 取成整数（执行侧用，缺省视为非法）。
func canvasGridDims(payload map[string]any) (int, int, bool) {
	rows, okRows := canvasPayloadInt(payload["rows"])
	cols, okCols := canvasPayloadInt(payload["cols"])
	if !okRows || !okCols {
		return 0, 0, false
	}
	return rows, cols, true
}

// canvasPayloadInt 取载荷里的整数；JSON 反序列化默认是 float64，一并兜住。
func canvasPayloadInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if float64(int(v)) != v {
			return 0, false
		}
		return int(v), true
	}
	return 0, false
}
