// 宫格布局计算（对应 lib/grid/layout.py）。
//
// 宫格把一组连续分镜按 N×N 平方切分渲染成一张联合图：整图比例取项目视频比例的规范朝向，
// 单格比例因而恒等于整图比例，切格后 center-crop 近乎 no-op。4×4 / 5×5 档只在图像分辨率档
// 为 4K 时放行（单格分辨率随格数缩水）。
package domain

import (
	"strconv"
	"strings"
)

// GRIDFallbackResolution 是未配置分辨率档时宫格渲染实际下发的档位（与 worker 执行侧同源：
// 门控与实际下发必须取同一常量，否则会出现「按 2K 判定、按别档下渲染」的漂移）。
const GRIDFallbackResolution = "2K"

const (
	gridBaseWidth     = 1920
	largeGridRes      = "4K"
	gatedMaxCellCount = 9
	hardMaxCellCount  = 25
)

// GridLayout 描述多张分镜合成宫格时的布局。
type GridLayout struct {
	GridSize         string // grid_4 / grid_9 / grid_16 / grid_25
	Rows             int
	Cols             int
	GridAspectRatio  string
	CellCount        int
	PlaceholderCount int
}

// PixelDimensions 按整图比例返回渲染像素尺寸（以较大边对齐基准宽度）。
func (l GridLayout) PixelDimensions() (int, int) {
	parts := strings.Split(l.GridAspectRatio, ":")
	if len(parts) != 2 {
		return gridBaseWidth, gridBaseWidth
	}
	wRatio, errW := strconv.Atoi(parts[0])
	hRatio, errH := strconv.Atoi(parts[1])
	if errW != nil || errH != nil || wRatio <= 0 || hRatio <= 0 {
		return gridBaseWidth, gridBaseWidth
	}
	if wRatio >= hRatio {
		return gridBaseWidth, roundedDiv(gridBaseWidth*hRatio, wRatio)
	}
	return roundedDiv(gridBaseWidth*wRatio, hRatio), gridBaseWidth
}

// roundedDiv 是 Python round(a/b) 的整数除法（四舍五入到最近整数）。
func roundedDiv(a, b int) int {
	if b == 0 {
		return 0
	}
	return int(float64(a)/float64(b) + 0.5)
}

// gridLadder 是档位阶梯：全部为 N×N 平方切分。顺序即选档顺序。
var gridLadder = []struct {
	cellCount int
	gridSize  string
	side      int
}{
	{4, "grid_4", 2},
	{9, "grid_9", 3},
	{16, "grid_16", 4},
	{25, "grid_25", 5},
}

// orientationAspect 是规范朝向对应的整图比例。
var orientationAspect = map[string]string{"horizontal": "16:9", "vertical": "9:16"}

// legacyNonSquareAspect 是存量 grid_6（曾唯一的非方形档）写入时的整图比例，按 (rows, cols) 索引。
var legacyNonSquareAspect = map[[2]int]string{{3, 2}: "4:3", {2, 3}: "3:4"}

// LargeGridAllowed 判定 4×4 / 5×5 是否可用：仅当宫格实际生效的图像分辨率档为 4K。
// 空分辨率表示调用时不传 SDK resolution 参数，按保底档处理。
func LargeGridAllowed(imageResolution string) bool {
	res := strings.TrimSpace(imageResolution)
	if res == "" {
		res = GRIDFallbackResolution
	}
	return strings.EqualFold(res, largeGridRes)
}

// MaxCellCount 返回单张宫格的格数上限；分组超出时由调用方按此切块。
func MaxCellCount(allowLargeGrid bool) int {
	if allowLargeGrid {
		return hardMaxCellCount
	}
	return gatedMaxCellCount
}

// VideoAspectRatio 取项目的视频比例，宫格画布与冻结在记录上的单格比例共用同一取值。
//
// project.json 的 aspect_ratio 允许显式为 null；Go 解码后为 ""，与「未设置」同义，
// 都回退到竖屏 9:16。
func VideoAspectRatio(pf *ProjectFile) string {
	if pf == nil {
		return orientationAspect["vertical"]
	}
	if strings.TrimSpace(pf.AspectRatio) != "" {
		return pf.AspectRatio
	}
	return orientationAspect["vertical"]
}

// orientationOf 按数值比较宽高判定朝向。
func orientationOf(aspectRatio string) string {
	parts := strings.Split(aspectRatio, ":")
	if len(parts) != 2 {
		return "vertical"
	}
	w, errW := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, errH := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errW != nil || errH != nil {
		return "vertical"
	}
	if w > h {
		return "horizontal"
	}
	return "vertical"
}

// GridAspectRatioFor 按记录自身的 rows/cols 取整图比例，供存量记录重生成使用。
//
// 方形档（rows == cols）下结果即视频比例本身；非方形只可能来自存量 grid_6 记录，
// 取其写入时的比例。未登记的非方形几何回落到视频比例。
func GridAspectRatioFor(rows, cols int, aspectRatio string) string {
	videoAspect := orientationAspect[orientationOf(aspectRatio)]
	if rows == cols {
		return videoAspect
	}
	if legacy, ok := legacyNonSquareAspect[[2]int{rows, cols}]; ok {
		return legacy
	}
	return videoAspect
}

// CalculateGridLayout 为给定分镜数量选择宫格布局；numScenes < 1 时 ok=false。
func CalculateGridLayout(numScenes int, aspectRatio string, allowLargeGrid bool) (GridLayout, bool) {
	if numScenes < 1 {
		return GridLayout{}, false
	}
	cap := MaxCellCount(allowLargeGrid)
	effective := numScenes
	if effective > cap {
		effective = cap
	}
	chosen := gridLadder[len(gridLadder)-1]
	for _, cfg := range gridLadder {
		if effective <= cfg.cellCount {
			chosen = cfg
			break
		}
	}
	return GridLayout{
		GridSize:         chosen.gridSize,
		Rows:             chosen.side,
		Cols:             chosen.side,
		GridAspectRatio:  orientationAspect[orientationOf(aspectRatio)],
		CellCount:        chosen.cellCount,
		PlaceholderCount: chosen.cellCount - effective,
	}, true
}
