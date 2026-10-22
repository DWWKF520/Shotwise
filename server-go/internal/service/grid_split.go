// 宫格切分：把宫格当前联合图切割落格到各分镜（对应 server/services/grid_split.py）。
//
// 切分是覆写分镜格的唯一步骤，与联合图的产生（生成任务 / 手动上传 / 版本还原）解耦：
// 联合图内容变更只刷新联合图自身，落格必须经本服务显式执行。
package service

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// gridEdgeMargin 是切格时从每边裁掉的边长比例（对应 Python split_grid_image 的默认值）。
const gridEdgeMargin = 0.02

// GridImageNotReadyError：宫格尚无联合图（未生成完成且未上传），无法切分。
var GridImageNotReadyError = errors.New("grid has no grid image to split")

// GridSplitResult 是一次切分的结果。
//
// Python 侧还带 AssetFingerprints（供前端 cache-bust）；Go 的项目变更事件层尚未迁移，
// 该字段随之待补——切分本身与指纹无关。
type GridSplitResult struct {
	UpdatedSceneIDs []string
	MissingSceneIDs []string
}

// GridService 承载宫格切分落格（依赖项目读写与剧本写盘统一入口）。
type GridService struct{ store *ProjectStore }

// NewGridService 构造。
func NewGridService(store *ProjectStore) *GridService { return &GridService{store: store} }

// ApplySplit 按 grid 当前联合图切割并覆写各分镜格。
//
//   - 每格覆写前旧文件先补登版本、覆写后登记新版本（source="grid_split"）；
//   - frame_chain 中已不在剧本内的 scene id 跳过并收集到 MissingSceneIDs；
//   - 完成后写 grid.split_at。
func (s *GridService) ApplySplit(ctx context.Context, projectName string, grid *domain.GridGeneration) (GridSplitResult, error) {
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return GridSplitResult{}, err
	}
	pf, err := s.store.Get(projectName)
	if err != nil {
		return GridSplitResult{}, err
	}

	manager, err := NewGridManager(projectDir)
	if err != nil {
		return GridSplitResult{}, err
	}
	gridImageFile, err := manager.ImagePath(grid.ID)
	if err != nil {
		return GridSplitResult{}, err
	}
	if grid.GridImagePath == nil || *grid.GridImagePath == "" {
		return GridSplitResult{}, GridImageNotReadyError
	}
	if _, err := os.Stat(gridImageFile); err != nil {
		return GridSplitResult{}, GridImageNotReadyError
	}

	script, err := s.store.ReadScriptJSON(projectName, grid.ScriptFile)
	if err != nil {
		return GridSplitResult{}, err
	}
	items, idField, _, err := domain.ResolveScriptItems(script)
	if err != nil {
		return GridSplitResult{}, err
	}
	validIDs := map[string]bool{}
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			validIDs[fmt.Sprint(m[idField])] = true
		}
	}

	src, err := decodePNG(gridImageFile)
	if err != nil {
		return GridSplitResult{}, err
	}
	// 比例取记录冻结值：项目 aspect_ratio 改过之后再切历史联合图，按新比例中心裁切会把每格
	// 削掉大半（横版图按竖版切）。存量记录无该字段，回退到项目当前设置。
	videoAspectRatio := AspectRatioFor(pf, "videos")
	if grid.VideoAspectRatio != nil && *grid.VideoAspectRatio != "" {
		videoAspectRatio = *grid.VideoAspectRatio
	}
	cells := splitGridImage(src, grid.Rows, grid.Cols, videoAspectRatio, gridEdgeMargin)

	storyboardsDir := filepath.Join(projectDir, "storyboards")
	if err := os.MkdirAll(storyboardsDir, 0o755); err != nil {
		return GridSplitResult{}, err
	}
	versions := NewVersions(projectDir)

	type assetUpdate struct {
		sceneID   string
		assetType string
		value     any
	}
	var (
		assetUpdates []assetUpdate
		updatedIDs   []string
		missingIDs   []string
	)
	for index, frame := range grid.FrameChain {
		if index >= len(cells) {
			break
		}
		if frame.FrameType != domain.GridFrameFirst && frame.FrameType != domain.GridFrameTransition {
			continue
		}
		if frame.NextSceneID == nil || *frame.NextSceneID == "" {
			continue
		}
		sceneID := *frame.NextSceneID
		if !validIDs[sceneID] {
			missingIDs = append(missingIDs, sceneID)
			continue
		}
		cellRel := fmt.Sprintf("storyboards/scene_%s.png", sceneID)
		cellPath, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(cellRel))
		if err != nil {
			return GridSplitResult{}, fmt.Errorf("非法分镜 ID: %s", sceneID)
		}
		// 与生成路径版本顺序一致：旧文件先补登再覆写、覆写后登记新版本。否则宫格重切的单元格
		// 不进版本史，版本面板的「当前版本」与磁盘内容脱节。
		if _, err := versions.EnsureCurrentTracked("storyboards", sceneID, "", cellPath, nil); err != nil {
			return GridSplitResult{}, err
		}
		if err := writePNG(cellPath, cells[index]); err != nil {
			return GridSplitResult{}, err
		}
		if _, _, err := versions.AddVersion("storyboards", sceneID, "", cellPath, map[string]any{
			"source":  "grid_split",
			"grid_id": grid.ID,
		}); err != nil {
			return GridSplitResult{}, err
		}
		imageRel := cellRel
		frame.ImagePath = &imageRel
		updatedIDs = append(updatedIDs, sceneID)
		assetUpdates = append(assetUpdates,
			assetUpdate{sceneID, "storyboard_image", cellRel},
			assetUpdate{sceneID, "grid_id", grid.ID},
			assetUpdate{sceneID, "grid_cell_index", frame.Index},
		)
	}

	// 整批资产更新一次读改写落盘；任一 scene id 未命中即整批拒绝（与 Python 侧 fail-loud 回滚
	// 同语义），避免 PNG 已落盘却因 KeyError 留下孤儿文件。
	if len(assetUpdates) > 0 {
		err := s.store.UpdateScript(projectName, grid.ScriptFile, func(script map[string]any) error {
			items, idField, _, err := domain.ResolveScriptItems(script)
			if err != nil {
				return err
			}
			byID := map[string]map[string]any{}
			for _, item := range items {
				if m, ok := item.(map[string]any); ok {
					byID[fmt.Sprint(m[idField])] = m
				}
			}
			for _, update := range assetUpdates {
				item, ok := byID[update.sceneID]
				if !ok {
					return fmt.Errorf("未找到 id=%q 的分镜（%s）", update.sceneID, idField)
				}
				assets, ok := item["generated_assets"].(map[string]any)
				if !ok {
					assets = map[string]any{}
					item["generated_assets"] = assets
				}
				assets[update.assetType] = update.value
			}
			return nil
		})
		if err != nil {
			return GridSplitResult{}, err
		}
	}

	splitAt := domain.GridTimestampISO()
	grid.SplitAt = &splitAt
	if err := manager.Save(grid); err != nil {
		return GridSplitResult{}, err
	}

	sort.Strings(missingIDs)
	return GridSplitResult{UpdatedSceneIDs: updatedIDs, MissingSceneIDs: dedupeStrings(missingIDs)}, nil
}

// decodePNG 读取 PNG 联合图。
func decodePNG(path string) (image.Image, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return png.Decode(file)
}

// writePNG 把切出的单元格写盘。
func writePNG(path string, img image.Image) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return png.Encode(file, img)
}

// cropImage 取子矩形（越界部分裁剪掉；空矩形返回零尺寸图）。
func cropImage(src image.Image, x0, y0, x1, y1 int) *image.RGBA {
	rect := image.Rect(x0, y0, x1, y1).Intersect(src.Bounds())
	if rect.Empty() {
		return image.NewRGBA(image.Rect(0, 0, 0, 0))
	}
	dst := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(dst, dst.Bounds(), src, rect.Min, draw.Src)
	return dst
}

// centerCropToRatio 从中心裁到目标比例（对应 Python center_crop_to_ratio）。
func centerCropToRatio(src *image.RGBA, targetRatio string) *image.RGBA {
	parts := strings.Split(targetRatio, ":")
	if len(parts) != 2 {
		return src
	}
	wRatio, errW := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	hRatio, errH := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if errW != nil || errH != nil || hRatio == 0 {
		return src
	}
	w := src.Bounds().Dx()
	h := src.Bounds().Dy()
	if h == 0 {
		return src
	}
	current := float64(w) / float64(h)
	target := wRatio / hRatio
	if math.Abs(current-target) < 0.01 {
		return src
	}
	if current > target {
		newW := int(float64(h) * target)
		left := (w - newW) / 2
		return cropImage(src, left, 0, left+newW, h)
	}
	newH := int(float64(w) / target)
	top := (h - newH) / 2
	return cropImage(src, 0, top, w, top+newH)
}

// splitGridImage 按 rows×cols 切成单格：逐格裁边后中心裁到目标比例，按行优先顺序返回。
func splitGridImage(src image.Image, rows, cols int, videoAspectRatio string, edgeMargin float64) []*image.RGBA {
	if rows <= 0 || cols <= 0 {
		return nil
	}
	bounds := src.Bounds()
	gridW, gridH := bounds.Dx(), bounds.Dy()
	cellW := gridW / cols
	cellH := gridH / rows
	marginX := int(float64(cellW) * edgeMargin)
	marginY := int(float64(cellH) * edgeMargin)

	cells := make([]*image.RGBA, 0, rows*cols)
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			x0 := col * cellW
			y0 := row * cellH
			cell := cropImage(src, x0, y0, x0+cellW, y0+cellH)
			if marginX > 0 || marginY > 0 {
				cw := cell.Bounds().Dx()
				ch := cell.Bounds().Dy()
				cell = cropImage(cell, marginX, marginY, cw-marginX, ch-marginY)
			}
			cells = append(cells, centerCropToRatio(cell, videoAspectRatio))
		}
	}
	return cells
}

// dedupeStrings 保序去重。
func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
