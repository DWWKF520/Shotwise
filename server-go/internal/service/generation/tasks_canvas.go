// 创意画布图片任务执行（对应 server/services/canvas_image_tasks.py::execute_canvas_image_task）。
//
// 画布操作刻意不触碰项目资产与剧本宫格记录：每次操作保留源图，产出独立的 MediaAsset。
//   - split / crop 是确定性几何操作（无 AI 语义）；
//   - 其余八个操作走项目「图生图」i2i 管线：redraw / erase 以选区裁剪参考聚焦模型、
//     生成贴片合成回全图（选区外像素保持原样）；outpaint 的选区描述源图在更大画布
//     中的位置，生成后把源图回贴到画布对应位置。
//
// 与 Python 的收敛差异：画质档位只认显式 2K / 4K，未指定时留空走 backend 默认
// （Python 回落项目配置的 i2i 分辨率，该配置读取在 Go 侧未接线）。
package service

import (
	"context"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/infra/img"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

// canvasAIPrompts 各 AI 操作的语义指令（与 Python 逐字一致：进入模型 prompt，不参与 i18n）。
var canvasAIPrompts = map[string]string{
	"canvas_image_panorama": "将画面向左、右延伸为全景横幅，保持主体与整体风格一致，自然补全新区域。",
	"canvas_image_angles":   "保持主体与风格一致，生成新的拍摄角度/视角变体。",
	"canvas_image_layers":   "将主体与背景分离，输出仅含主体的前景图。",
	"canvas_image_hd":       "高清晰放大图片，提升细节与锐度。",
	"canvas_image_outpaint": "向外扩图画布并自然补全四周新区域，保持主体与风格一致。",
	"canvas_image_redraw":   "在指定区域重新绘制，按指令修改内容。",
	"canvas_image_erase":    "移除指定区域内的物体并自然补全背景。",
	"canvas_image_cutout":   "抠图：移除背景，仅保留主体，输出透明背景。",
}

// canvasRenderedItem 是确定性渲染路径的一个待落盘产物。
type canvasRenderedItem struct {
	label string
	image image.Image
}

// canvasOutpaintGeo 是 outpaint 的画布几何：画布尺寸与源图在画布中的左上角位置。
type canvasOutpaintGeo struct {
	canvasWidth  int
	canvasHeight int
	sourceLeft   int
	sourceTop    int
}

// executeCanvasImageTask 执行一个创意画布图片操作（对应 execute_canvas_image_task）。
// operation 读 payload（缺省 split）；源图来自 media 资产或项目资源当前图。
func (d *TaskDispatcher) executeCanvasImageTask(ctx context.Context, task *repository.TaskData) (map[string]any, error) {
	operation := payloadString(task.Payload, "operation")
	if operation == "" {
		operation = CanvasImageOperationSplit
	}
	if _, ok := CanvasImageOperations[operation]; !ok {
		return nil, fmt.Errorf("unsupported canvas image operation: %s", operation)
	}
	sourceAbs, projectDir, sourceID, pf, err := d.resolveCanvasImageSource(task)
	if err != nil {
		return nil, err
	}

	metadata := map[string]any{
		"operation":   operation,
		"source_kind": task.Payload["source_kind"],
		"source_id":   sourceID,
	}

	switch operation {
	case "canvas_image_split":
		rows, cols, ok := canvasGridDims(task.Payload)
		if !ok || !ValidateCanvasGrid(rows, cols) {
			return nil, fmt.Errorf("rows and cols must be integers between 2 and 8")
		}
		source, err := canvasLoadImage(sourceAbs)
		if err != nil {
			return nil, err
		}
		outputs, err := d.writeCanvasRenderedOutputs(task, operation, projectDir, sourceID, canvasSplitCells(source, rows, cols))
		if err != nil {
			return nil, err
		}
		for index, output := range outputs {
			output["row"] = index / cols
			output["col"] = index % cols
		}
		includeLines := true
		if v, ok := task.Payload["include_split_lines"].(bool); ok {
			includeLines = v
		}
		metadata["rows"] = rows
		metadata["cols"] = cols
		metadata["include_split_lines"] = includeLines
		metadata["cells"] = outputs
	case "canvas_image_crop":
		source, err := canvasLoadImage(sourceAbs)
		if err != nil {
			return nil, err
		}
		item := canvasRenderedItem{label: "crop", image: source}
		if region, ok := task.Payload["region"].(map[string]any); ok {
			bounds := source.Bounds()
			box, berr := canvasRegionBox(bounds.Dx(), bounds.Dy(), region)
			if berr != nil {
				return nil, berr
			}
			item.image = img.Crop(source, box)
		}
		outputs, err := d.writeCanvasRenderedOutputs(task, operation, projectDir, sourceID, []canvasRenderedItem{item})
		if err != nil {
			return nil, err
		}
		metadata["region"] = task.Payload["region"]
		metadata["outputs"] = outputs
	default:
		// 其余操作（全景 / 多角度 / 图层分离 / 高清 / 扩图 / 重绘 / 擦除 / 抠图）走 i2i。
		if pf == nil {
			if pf, err = d.store.Get(task.ProjectName); err != nil {
				return nil, err
			}
		}
		outputs, err := d.generateCanvasAIOutputs(ctx, task, operation, sourceAbs, projectDir, sourceID, pf)
		if err != nil {
			return nil, err
		}
		metadata["region"] = task.Payload["region"]
		metadata["outputs"] = outputs
	}
	return metadata, nil
}

// resolveCanvasImageSource 解析画布任务源图（对应 resolve_canvas_image_source）：
// media 走媒体索引（须为图片且文件在项目内），project 走资源当前图（四类资产 + 分镜）。
// project 来源时顺带返回已加载的 project.json（AI 路径要用）；media 来源返回 nil。
func (d *TaskDispatcher) resolveCanvasImageSource(
	task *repository.TaskData,
) (sourceAbs, projectDir, sourceID string, pf *domain.ProjectFile, err error) {
	projectDir, err = d.store.Dir(task.ProjectName)
	if err != nil {
		return "", "", "", nil, err
	}
	sourceKind := payloadString(task.Payload, "source_kind")
	if sourceKind == "media" {
		mediaID := strings.TrimSpace(payloadString(task.Payload, "media_asset_id"))
		if mediaID == "" {
			return "", "", "", nil, fmt.Errorf("media_asset_id is required for media source")
		}
		full, merr := resolveCanvasMediaPath(projectDir, mediaID)
		if merr != nil {
			return "", "", "", nil, merr
		}
		return full, projectDir, mediaID, nil, nil
	}
	if sourceKind != "project" {
		return "", "", "", nil, fmt.Errorf("source_kind must be project or media")
	}
	resourceType := payloadString(task.Payload, "resource_type")
	if !isCanvasProjectResourceType(resourceType) {
		return "", "", "", nil, fmt.Errorf("unsupported project image resource_type")
	}
	resourceID := strings.TrimSpace(payloadString(task.Payload, "resource_id"))
	if resourceID == "" {
		return "", "", "", nil, fmt.Errorf("resource_id is required for project source")
	}
	pf, err = d.store.Get(task.ProjectName)
	if err != nil {
		return "", "", "", nil, err
	}
	var script map[string]any
	if resourceType == StoryboardResourceType {
		scriptFile := payloadString(task.Payload, "script_file")
		if script, err = d.store.ReadScriptJSON(task.ProjectName, scriptFile); err != nil {
			return "", "", "", nil, err
		}
	}
	currentRel, found := ResolveCurrentImageRel(pf, resourceType, resourceID, script)
	if !found || currentRel == "" || !fs.SafeExists(projectDir, filepath.FromSlash(currentRel)) {
		return "", "", "", nil, fmt.Errorf("project image does not exist")
	}
	full := fs.TrySafeJoin(projectDir, false, filepath.FromSlash(currentRel))
	if full == "" {
		return "", "", "", nil, fmt.Errorf("project image does not exist")
	}
	return full, projectDir, resourceID, pf, nil
}

// resolveCanvasMediaPath 解析媒体库来源的图片资产绝对路径（须为图片且文件在项目内存在）。
func resolveCanvasMediaPath(projectDir, mediaAssetID string) (string, error) {
	asset, err := GetProjectMediaAsset(projectDir, mediaAssetID)
	if err != nil || asset == nil {
		return "", fmt.Errorf("reference media asset not found")
	}
	if kind, _ := asset["kind"].(string); kind != "image" {
		return "", fmt.Errorf("reference media asset must be an image")
	}
	physical, _ := asset["physical_path"].(string)
	if physical == "" {
		return "", fmt.Errorf("reference media asset has no file path")
	}
	if !fs.SafeExists(projectDir, filepath.FromSlash(physical)) {
		return "", fmt.Errorf("reference media asset file is missing")
	}
	full := fs.TrySafeJoin(projectDir, false, filepath.FromSlash(physical))
	if full == "" {
		return "", fmt.Errorf("reference media asset file is missing")
	}
	return full, nil
}

// generateCanvasAIOutputs 走项目 i2i 管线生成独立输出并登记 media asset
// （对应 _generate_ai_outputs，不触碰源资产版本）。
func (d *TaskDispatcher) generateCanvasAIOutputs(
	ctx context.Context,
	task *repository.TaskData,
	operation, sourceAbs, projectDir, sourceID string,
	pf *domain.ProjectFile,
) ([]map[string]any, error) {
	if d.images == nil {
		return nil, fmt.Errorf("canvas AI operation requires an image generator")
	}
	prompt := canvasBuildAIPrompt(operation, task.Payload)
	count, ok := canvasValidateCount(task.Payload["count"], canvasDefaultCount(operation))
	if !ok {
		return nil, fmt.Errorf("count must be an integer between 1 and 8")
	}

	sourceBytes, err := os.ReadFile(sourceAbs)
	if err != nil {
		return nil, err
	}
	srcWidth, srcHeight, err := img.DecodeConfig(sourceBytes)
	if err != nil {
		return nil, fmt.Errorf("decode canvas source image: %w", err)
	}
	region, _ := task.Payload["region"].(map[string]any)

	// outpaint 几何：选区描述源图在更大画布中的位置。
	var outpaint canvasOutpaintGeo
	hasOutpaint := false
	if operation == "canvas_image_outpaint" && region != nil {
		geo, gerr := canvasOutpaintGeometry(srcWidth, srcHeight, region)
		if gerr != nil {
			return nil, gerr
		}
		outpaint = geo
		hasOutpaint = true
	}

	outputDir := canvasOutputDir(projectDir, operation, task.TaskID)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, err
	}

	// 区域编辑（redraw / erase）：裁剪选区作为参考聚焦模型，生成后把贴片合成回全图。
	var regionBox image.Rectangle
	hasRegionBox := false
	refPath := sourceAbs
	if operation != "canvas_image_outpaint" && region != nil {
		box, berr := canvasRegionBox(srcWidth, srcHeight, region)
		if berr != nil {
			return nil, berr
		}
		source, derr := img.Decode(sourceBytes)
		if derr != nil {
			return nil, fmt.Errorf("decode canvas source image: %w", derr)
		}
		refPath = filepath.Join(outputDir, "region-ref.png")
		encoded, eerr := img.EncodePNG(img.Crop(source, box))
		if eerr != nil {
			return nil, eerr
		}
		if werr := os.WriteFile(refPath, encoded, 0o644); werr != nil {
			return nil, werr
		}
		regionBox = box
		hasRegionBox = true
	}

	aspectRatio := strings.TrimSpace(payloadString(task.Payload, "aspect_ratio"))
	if aspectRatio == "" {
		if hasOutpaint {
			aspectRatio = fmt.Sprintf("%d:%d", outpaint.canvasWidth, outpaint.canvasHeight)
		} else {
			aspectRatio = fmt.Sprintf("%d:%d", srcWidth, srcHeight)
		}
	}
	imageSize := strings.ToUpper(strings.TrimSpace(payloadString(task.Payload, "quality")))
	if imageSize != "2K" && imageSize != "4K" {
		imageSize = ""
	}

	label := strings.TrimPrefix(operation, "canvas_image_")
	outputs := make([]map[string]any, 0, count)
	for index := 0; index < count; index++ {
		path := filepath.Join(outputDir, fmt.Sprintf("%02d-%s.png", index+1, label))
		request := provider.NewImageGenerationRequest(prompt, path)
		request.ReferenceImages = []provider.ReferenceImage{{Path: refPath}}
		request.AspectRatio = aspectRatio
		request.ImageSize = imageSize
		request.ProjectName = task.ProjectName
		if _, err := d.images.Generate(ctx, ImageGenerateParams{
			ProjectName: task.ProjectName,
			Project:     pf,
			Capability:  config.ImageCapI2I,
			Request:     request,
			UserID:      task.UserID,
		}); err != nil {
			return nil, err
		}
		if hasRegionBox {
			if err := compositeCanvasRegionPatch(sourceAbs, path, regionBox); err != nil {
				return nil, err
			}
		} else if hasOutpaint {
			if err := compositeCanvasOutpaint(sourceAbs, path, outpaint); err != nil {
				return nil, err
			}
		}
		finalBytes, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, rerr
		}
		width, height, derr := img.DecodeConfig(finalBytes)
		if derr != nil {
			return nil, fmt.Errorf("decode canvas output: %w", derr)
		}
		rel, relErr := filepath.Rel(projectDir, path)
		if relErr != nil {
			return nil, relErr
		}
		output, regErr := canvasRegisterOutput(task.ProjectName, projectDir, sourceID, filepath.ToSlash(rel), label, canvasRegisterOrigin(operation))
		if regErr != nil {
			return nil, regErr
		}
		output["index"] = index
		output["width"] = width
		output["height"] = height
		outputs = append(outputs, output)
	}
	return outputs, nil
}

// compositeCanvasRegionPatch 把生成贴片缩放到选区尺寸后合成回源图（redraw / erase）。
func compositeCanvasRegionPatch(sourcePath, outputPath string, box image.Rectangle) error {
	source, err := canvasLoadImage(sourcePath)
	if err != nil {
		return err
	}
	patch, err := canvasLoadImage(outputPath)
	if err != nil {
		return err
	}
	patch = img.Resize(patch, box.Dx(), box.Dy())
	composited := img.CompositeOver(source, patch, box.Min.X, box.Min.Y)
	return writeCanvasPNG(outputPath, composited)
}

// compositeCanvasOutpaint 把生成图缩放到画布尺寸，再把源图回贴到几何给出的位置。
func compositeCanvasOutpaint(sourcePath, outputPath string, geo canvasOutpaintGeo) error {
	source, err := canvasLoadImage(sourcePath)
	if err != nil {
		return err
	}
	generated, err := canvasLoadImage(outputPath)
	if err != nil {
		return err
	}
	generated = img.Resize(generated, geo.canvasWidth, geo.canvasHeight)
	composited := img.CompositeOver(generated, source, geo.sourceLeft, geo.sourceTop)
	return writeCanvasPNG(outputPath, composited)
}

// writeCanvasRenderedOutputs 落盘确定性渲染产物并逐个登记 MediaAsset
// （对应 _write_outputs：PNG 命名 {序号:02d}-{label}.png，目录 canvas_outputs/{op}/{task_id}）。
func (d *TaskDispatcher) writeCanvasRenderedOutputs(
	task *repository.TaskData, operation, projectDir, sourceID string,
	items []canvasRenderedItem,
) ([]map[string]any, error) {
	outputDir := canvasOutputDir(projectDir, operation, task.TaskID)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, err
	}
	outputs := make([]map[string]any, 0, len(items))
	for index, item := range items {
		encoded, err := img.EncodePNG(item.image)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(outputDir, fmt.Sprintf("%02d-%s.png", index+1, item.label))
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			return nil, err
		}
		width, height, err := img.DecodeConfig(encoded)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(projectDir, path)
		if err != nil {
			return nil, err
		}
		output, err := canvasRegisterOutput(task.ProjectName, projectDir, sourceID, filepath.ToSlash(rel), item.label, canvasRegisterOrigin(operation))
		if err != nil {
			return nil, err
		}
		output["index"] = index
		output["width"] = width
		output["height"] = height
		outputs = append(outputs, output)
	}
	return outputs, nil
}

// canvasRegisterOutput 登记一个画布产物并组装输出片段（对应 _register_output）。
// 索引关闭时 RegisterMediaAsset 为空操作，输出片段不含 media_asset 字段（与 Python 一致）。
func canvasRegisterOutput(projectName, projectDir, sourceID, relPath, label, origin string) (map[string]any, error) {
	originalName := fmt.Sprintf("%s-%s.png", sourceID, label)
	asset, err := RegisterMediaAsset(RegisterMediaAssetParams{
		ProjectID:    projectName,
		ProjectRoot:  projectDir,
		RelativePath: relPath,
		Origin:       origin,
		OriginalName: &originalName,
	})
	if err != nil {
		return nil, err
	}
	output := map[string]any{"file_path": relPath, "label": label}
	if asset != nil {
		output["media_asset_id"] = asset.ID
		output["media_asset"] = map[string]any{
			"id":            asset.ID,
			"kind":          asset.Kind,
			"original_name": asset.OriginalName,
			"physical_path": asset.PhysicalPath,
			"mime_type":     asset.MimeType,
			"width":         asset.Width,
			"height":        asset.Height,
		}
	}
	return output, nil
}

// canvasRegisterOrigin 产物登记来源：split / layers 是从源图提取（extracted），
// 其余（含确定性的 crop）按 Python 口径登记为 generated。
func canvasRegisterOrigin(operation string) string {
	if operation == CanvasImageOperationSplit || operation == "canvas_image_layers" {
		return MediaOriginExtracted
	}
	return MediaOriginGenerated
}

// canvasSplitCells 把源图切成 rows×cols 宫格（对应 _build_split）。
func canvasSplitCells(src image.Image, rows, cols int) []canvasRenderedItem {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	items := make([]canvasRenderedItem, 0, rows*cols)
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			left := col * width / cols
			top := row * height / rows
			right := (col + 1) * width / cols
			bottom := (row + 1) * height / rows
			items = append(items, canvasRenderedItem{
				label: fmt.Sprintf("cell-%d-%d", row+1, col+1),
				image: img.Crop(src, image.Rect(left, top, right, bottom)),
			})
		}
	}
	return items
}

// canvasOutputDir 产物目录：canvas_outputs/{操作去掉前缀}/{task_id}；缺 task_id 用 uuid。
func canvasOutputDir(projectDir, operation, taskID string) string {
	key := strings.TrimSpace(taskID)
	if key == "" {
		key = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	return filepath.Join(projectDir, "canvas_outputs", strings.TrimPrefix(operation, "canvas_image_"), key)
}

// canvasRegionBox 把 payload.region 解析为像素裁剪框（对应 _region_box）。
// 全部坐标绝对值 ≤1 视为归一化比例（按图尺寸放大）；数值非法返回错误；
// 结果钳制在图像范围内且非退化（宽高 ≥1）。
func canvasRegionBox(width, height int, region map[string]any) (image.Rectangle, error) {
	left, errX := regionFloat(region, "x", 0)
	top, errY := regionFloat(region, "y", 0)
	regionWidth, errW := regionFloat(region, "width", float64(width))
	regionHeight, errH := regionFloat(region, "height", float64(height))
	if errX != nil || errY != nil || errW != nil || errH != nil {
		return image.Rectangle{}, fmt.Errorf("region must contain numeric x, y, width, height")
	}
	right, bottom := left+regionWidth, top+regionHeight
	absMax := math.Max(math.Max(math.Abs(left), math.Abs(top)), math.Max(math.Abs(right), math.Abs(bottom)))
	if absMax <= 1.0 {
		left, right = left*float64(width), right*float64(width)
		top, bottom = top*float64(height), bottom*float64(height)
	}
	l := clampRegionInt(int(math.Round(left)), 0, width-1)
	t := clampRegionInt(int(math.Round(top)), 0, height-1)
	r := clampRegionInt(int(math.Round(right)), l+1, width)
	b := clampRegionInt(int(math.Round(bottom)), t+1, height)
	return image.Rect(l, t, r, b), nil
}

// canvasOutpaintGeometry 计算 outpaint 画布几何（对应 _outpaint_geometry）。
// region 语义与裁剪相反：描述源图在更大画布中的归一化位置（值 >1 视为像素，
// 按源图尺寸归一化）；须为画布内的正面积框。
func canvasOutpaintGeometry(width, height int, region map[string]any) (canvasOutpaintGeo, error) {
	x, errX := regionFloat(region, "x", 0)
	y, errY := regionFloat(region, "y", 0)
	regionWidth, errW := regionFloat(region, "width", 1)
	regionHeight, errH := regionFloat(region, "height", 1)
	if errX != nil || errY != nil || errW != nil || errH != nil {
		return canvasOutpaintGeo{}, fmt.Errorf("region must contain numeric x, y, width, height")
	}
	absMax := math.Max(math.Max(math.Abs(x), math.Abs(y)), math.Max(math.Abs(regionWidth), math.Abs(regionHeight)))
	if absMax > 1.0 {
		x, y = x/float64(width), y/float64(height)
		regionWidth, regionHeight = regionWidth/float64(width), regionHeight/float64(height)
	}
	if regionWidth <= 0 || regionHeight <= 0 || x < 0 || y < 0 || x+regionWidth > 1 || y+regionHeight > 1 {
		return canvasOutpaintGeo{}, fmt.Errorf("outpaint region must be a positive normalized box inside the canvas")
	}
	canvasWidth := canvasMaxInt(width, int(math.Round(float64(width)/regionWidth)))
	canvasHeight := canvasMaxInt(height, int(math.Round(float64(height)/regionHeight)))
	sourceLeft := clampRegionInt(int(math.Round(x*float64(canvasWidth))), 0, canvasWidth-width)
	sourceTop := clampRegionInt(int(math.Round(y*float64(canvasHeight))), 0, canvasHeight-height)
	return canvasOutpaintGeo{canvasWidth: canvasWidth, canvasHeight: canvasHeight, sourceLeft: sourceLeft, sourceTop: sourceTop}, nil
}

// canvasBuildAIPrompt 组装 i2i prompt：操作语义 + 区域描述 + 放大倍数 + 用户指令
// （对应 _build_ai_prompt，逐段拼接无分隔符；agent 侧字符串，不参与 i18n）。
func canvasBuildAIPrompt(operation string, payload map[string]any) string {
	opPrompt, ok := canvasAIPrompts[operation]
	if !ok {
		opPrompt = "对图片进行智能编辑。"
	}
	var b strings.Builder
	b.WriteString(opPrompt)
	if region, ok := payload["region"].(map[string]any); ok {
		x, _ := regionFloat(region, "x", 0)
		y, _ := regionFloat(region, "y", 0)
		w, _ := regionFloat(region, "width", 0)
		h, _ := regionFloat(region, "height", 0)
		fmt.Fprintf(&b, " 处理区域：x=%s y=%s 宽=%s 高=%s。",
			canvasFormatFloat(x), canvasFormatFloat(y), canvasFormatFloat(w), canvasFormatFloat(h))
	}
	if multiplier, ok := canvasPayloadInt(payload["multiplier"]); ok && multiplier != 0 {
		fmt.Fprintf(&b, " 放大倍数：%d。", multiplier)
	}
	b.WriteString(strings.TrimSpace(payloadString(payload, "instruction")))
	return strings.TrimSpace(b.String())
}

// canvasValidateCount 校验变体数（对应 _validate_count）：nil 用默认，1..8 整数。
func canvasValidateCount(value any, def int) (int, bool) {
	if value == nil {
		return def, true
	}
	n, ok := canvasPayloadInt(value)
	if !ok || n < 1 || n > 8 {
		return 0, false
	}
	return n, true
}

// canvasDefaultCount 每个操作默认产出的变体数（多角度 4 / 图层分离 3，其余 1）。
func canvasDefaultCount(operation string) int {
	switch operation {
	case "canvas_image_angles":
		return 4
	case "canvas_image_layers":
		return 3
	default:
		return 1
	}
}

// canvasLoadImage 读文件并解码为 image.Image。
func canvasLoadImage(path string) (image.Image, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return img.Decode(raw)
}

// writeCanvasPNG 编码并直接写回（对应 PIL save 直写语义）。
func writeCanvasPNG(path string, src image.Image) error {
	encoded, err := img.EncodePNG(src)
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o644)
}

// regionFloat 读 region 字段的数值（JSON 数字为 float64；直构任务可能为 int）。
func regionFloat(region map[string]any, key string, def float64) (float64, error) {
	raw, ok := region[key]
	if !ok || raw == nil {
		return def, nil
	}
	switch v := raw.(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	default:
		return 0, fmt.Errorf("region %s must be numeric", key)
	}
}

func canvasFormatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func clampRegionInt(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}

func canvasMaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
