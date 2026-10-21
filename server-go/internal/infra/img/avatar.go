package img

// 角色头像裁剪（对应 lib/character_avatar.py 的收敛移植）。
//
// 收敛差异：Python 的人脸检测走 OpenCV Haar 级联，Go 侧不引入该依赖，只保留两条
// 无检测路径——宽版设计图按固定比例框取左侧胸像面板（与 Python fallback 一致），
// 窄图整幅取景；随后统一适配为 256×256 方形 PNG。带可检脸的设计图上头像取景可能
// 比检测路径偏远，属已知收敛差异。

import (
	"image"
	"math"
	"os"
)

// AvatarSize 是角色头像的目标边长（像素）。
const AvatarSize = 256

// avatarWideFaceBox 是宽版设计图左侧胸像面板的裁剪框（比例坐标 left/top/right/bottom），
// 与 Python WIDE_SHEET_FACE_BOX 一致。
var avatarWideFaceBox = [4]float64{0.06, 0.10, 0.36, 0.72}

// CreateAvatar 从角色设计图裁出方形头像 PNG，经临时文件原子落盘。
func CreateAvatar(sourcePath, destinationPath string) error {
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	src, err := Decode(raw)
	if err != nil {
		return err
	}
	avatar := FitSquare(CropAvatarPanel(src), AvatarSize, 0.42)
	encoded, err := EncodePNG(avatar)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dirOf(destinationPath), 0o755); err != nil {
		return err
	}
	return writeAtomic(destinationPath, encoded)
}

// CropAvatarPanel 按角色设计图形状取头像源：宽版（宽 > 高×1.25，右侧是 turnaround
// 与参考图）只取左侧胸像面板，其余整幅。
func CropAvatarPanel(src image.Image) image.Image {
	b := src.Bounds()
	if b.Dx() <= int(math.Round(float64(b.Dy())*1.25)) {
		return src
	}
	left := int(math.Round(float64(b.Dx()) * avatarWideFaceBox[0]))
	top := int(math.Round(float64(b.Dy()) * avatarWideFaceBox[1]))
	right := int(math.Round(float64(b.Dx()) * avatarWideFaceBox[2]))
	bottom := int(math.Round(float64(b.Dy()) * avatarWideFaceBox[3]))
	return Crop(src, image.Rect(left, top, right, bottom))
}

// Crop 返回 src 裁剪 rect 后的子图，结果坐标锚定在原点；rect 越出图像边界时按交集钳制。
func Crop(src image.Image, rect image.Rectangle) image.Image {
	b := src.Bounds()
	rect = rect.Intersect(b)
	if rect.Empty() {
		return src
	}
	type subImg interface {
		SubImage(r image.Rectangle) image.Image
	}
	if sub, ok := src.(subImg); ok {
		result := sub.SubImage(rect)
		// SubImage 保留父图坐标系（Min 非 0），At 的调用方须自行叠加偏移；此处统一
		// 平移到原点，让 At(x, y) 与 PNG 编码都以裁剪结果自身为参照。
		switch v := result.(type) {
		case *image.RGBA:
			v.Rect = image.Rect(0, 0, rect.Dx(), rect.Dy())
			return v
		case *image.NRGBA:
			v.Rect = image.Rect(0, 0, rect.Dx(), rect.Dy())
			return v
		case *image.Gray:
			v.Rect = image.Rect(0, 0, rect.Dx(), rect.Dy())
			return v
		case *image.YCbCr:
			v.Rect = image.Rect(0, 0, rect.Dx(), rect.Dy())
			return v
		default:
			return result
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	for y := 0; y < rect.Dy(); y++ {
		for x := 0; x < rect.Dx(); x++ {
			dst.Set(x, y, src.At(rect.Min.X+x, rect.Min.Y+y))
		}
	}
	return dst
}

// FitSquare 等比缩放到恰好覆盖 size×size 的正方形，再按水平居中、垂直取景位置
// vCenter（0 = 顶对齐，0.5 = 居中）裁出正方形。对应 Python ImageOps.fit(centering)。
func FitSquare(src image.Image, size int, vCenter float64) image.Image {
	b := src.Bounds()
	width, height := b.Dx(), b.Dy()
	if width <= 0 || height <= 0 || size <= 0 {
		return src
	}
	scale := math.Max(float64(size)/float64(width), float64(size)/float64(height))
	newWidth := maxInt(int(math.Round(float64(width)*scale)), size)
	newHeight := maxInt(int(math.Round(float64(height)*scale)), size)
	scaled := resize(src, newWidth, newHeight)

	left := (newWidth - size) / 2
	top := int(math.Round(float64(newHeight-size) * vCenter))
	top = clampInt(top, 0, newHeight-size)
	return Crop(scaled, image.Rect(left, top, left+size, top+size))
}

// resize 最近邻缩放（与 FitLongEdge 同采样口径：头像缩放为一次性预览产物，
// Python 用 LANCZOS 的质量差异在此不构成功能差异）。
func resize(src image.Image, newWidth, newHeight int) image.Image {
	b := src.Bounds()
	width, height := b.Dx(), b.Dy()
	if width == newWidth && height == newHeight {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))
	for y := 0; y < newHeight; y++ {
		srcY := b.Min.Y + y*height/newHeight
		for x := 0; x < newWidth; x++ {
			dst.Set(x, y, src.At(b.Min.X+x*width/newWidth, srcY))
		}
	}
	return dst
}

func clampInt(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if os.IsPathSeparator(path[i]) {
			return path[:i]
		}
	}
	return "."
}

// writeAtomic 同目录临时文件 + os.Replace 原子落盘（目标可能被并发读者读到）。
func writeAtomic(destinationPath string, content []byte) error {
	tmp, err := os.CreateTemp(dirOf(destinationPath), ".avatar-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, destinationPath)
}
