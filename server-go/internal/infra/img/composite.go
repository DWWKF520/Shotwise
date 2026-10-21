package img

// 画布合成原语（对应 PIL alpha_composite / resize 在画布任务里的用法）。

import (
	"image"
	"image/color"
)

// Resize 把图像缩放到精确目标尺寸（最近邻，与头像缩放同采样口径）。
func Resize(src image.Image, newWidth, newHeight int) image.Image {
	return resize(src, newWidth, newHeight)
}

// CompositeOver 把 over 以 (dx, dy) 为左上角叠加到 base 上，返回合成结果。
//
// 对应 PIL alpha_composite 的「贴片盖在底图」方向（redraw / erase 的贴片合成）；
// 反方向（源图盖在生成画布上，outpaint 用）只是交换两个实参。over 越出 base
// 边界的部分被裁掉——贴片落位由调用方按几何保证。
func CompositeOver(base, over image.Image, dx, dy int) *image.RGBA {
	bb := base.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bb.Dx(), bb.Dy()))
	for y := 0; y < bb.Dy(); y++ {
		for x := 0; x < bb.Dx(); x++ {
			dst.Set(x, y, base.At(bb.Min.X+x, bb.Min.Y+y))
		}
	}
	ob := over.Bounds()
	for y := ob.Min.Y; y < ob.Max.Y; y++ {
		ty := y - ob.Min.Y + dy
		if ty < 0 || ty >= dst.Rect.Dy() {
			continue
		}
		for x := ob.Min.X; x < ob.Max.X; x++ {
			tx := x - ob.Min.X + dx
			if tx < 0 || tx >= dst.Rect.Dx() {
				continue
			}
			dst.SetRGBA(tx, ty, blendOverPixel(dst.At(tx, ty), over.At(x, y)))
		}
	}
	return dst
}

// blendOverPixel 以 over 为上层做标准 over 合成，返回非预乘 8 位色。
// 输入经 color.Color.RGBA() 归一到 16 位预乘空间，公式在预乘域闭合后再反预乘。
func blendOverPixel(base, over color.Color) color.RGBA {
	or, og, ob, oa := over.RGBA()
	br, bg, bl, ba := base.RGBA()
	inv := 65535 - oa
	outA := oa + (ba*inv)/65535
	if outA == 0 {
		return color.RGBA{}
	}
	un := func(c uint32) uint8 { return uint8((c * 65535 / outA) >> 8) }
	return color.RGBA{
		un(or + (br*inv)/65535),
		un(og + (bg*inv)/65535),
		un(ob + (bl*inv)/65535),
		uint8(outA >> 8),
	}
}
