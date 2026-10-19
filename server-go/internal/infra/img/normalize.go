// 图像归一化（对应 lib/image_utils.py 的上传路径）。
//
// 只做上传面需要的那一小块：格式识别、像素预算、转 PNG、按长边缩放。工作用标准库
// image/png、image/jpeg 完成——宫格联合图与分镜图的上传都是「已有编码的位图 → PNG」，
// 不需要 EXIF 之外的元数据处理。
//
// 已知留白：**EXIF 方向矫正未实现**（Python 侧用 Pillow 的 ImageOps.exif_transpose）。
// 手机直出照片的 Orientation 标记会被忽略，横竖可能反。生成器产出的图不带该标记，
// 因此当前受影响面只有「用户用手机拍的分镜图/联合图」。补的时候只需在解码后按
// APP1 的 0x0112 值做转置，其余接口不用改。
package img

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
)

// MaxUploadPixels 是单张上传图允许解码的最大像素数（对应 Python MAX_UPLOAD_PIXELS）。
const MaxUploadPixels = 30_000_000

// ErrPixelLimit 表示图片可解析但像素数超限（调用方给出可操作提示，而非「格式非法」）。
var ErrPixelLimit = errors.New("image pixel limit exceeded")

// Format 是识别出的容器格式。
type Format string

const (
	FormatPNG     Format = "PNG"
	FormatJPEG    Format = "JPEG"
	FormatWebP    Format = "WEBP"
	FormatGIF     Format = "GIF"
	FormatUnknown Format = "UNKNOWN"
	pngSignature         = "\x89PNG\r\n\x1a\n"
)

// Sniff 按文件头识别格式（不解码像素）。
func Sniff(content []byte) Format {
	switch {
	case bytes.HasPrefix(content, []byte(pngSignature)):
		return FormatPNG
	case bytes.HasPrefix(content, []byte{0xFF, 0xD8, 0xFF}):
		return FormatJPEG
	case len(content) >= 12 && bytes.Equal(content[0:4], []byte("RIFF")) &&
		bytes.Equal(content[8:12], []byte("WEBP")):
		return FormatWebP
	case bytes.HasPrefix(content, []byte("GIF8")):
		return FormatGIF
	default:
		return FormatUnknown
	}
}

// NormalizeToPNG 把上传字节归一为 PNG。
//
// maxLongEdge 为 nil 表示不缩放，只做格式归一化——宫格联合图等「分辨率即信息量」的
// 上传必须走这一档，否则 4K 图会被静默降采样后切格失真。
//
// 返回 ErrPixelLimit 表示图可解析但过大；其它错误表示字节不是有效图片。
func NormalizeToPNG(content []byte, maxLongEdge *int) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil {
		return nil, fmt.Errorf("invalid image: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, errors.New("invalid image: empty dimensions")
	}
	if cfg.Width*cfg.Height > MaxUploadPixels {
		return nil, ErrPixelLimit
	}
	// 已合规的 PNG 原样返回，不做无谓的解码重编码。
	if format == "png" && (maxLongEdge == nil || maxInt(cfg.Width, cfg.Height) <= *maxLongEdge) {
		return content, nil
	}

	src, _, err := image.Decode(bytes.NewReader(content))
	if err != nil {
		return nil, fmt.Errorf("invalid image: %w", err)
	}
	out := src
	if maxLongEdge != nil {
		out = FitLongEdge(src, *maxLongEdge)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ReadAllBounded 按上限读取 reader；超限返回字节数与 ErrTooLarge。
//
// Content-Length 缺失或不可信时用它兜住内存：上传面不应按客户端声明决定分配多少。
func ReadAllBounded(r io.Reader, maxBytes int64) ([]byte, error) {
	limited := io.LimitReader(r, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

// ErrTooLarge 表示上传体超过允许的字节数。
var ErrTooLarge = errors.New("upload too large")

// FitLongEdge 按长边等比缩放到 limit（不放大）；长边已在限内时原样返回。
//
// 采样用最近邻：分镜图与联合图的缩放只影响版面预览，不追求重采样质量；
// 需要高质量缩放时换成双线性即可，调用方接口不变。
func FitLongEdge(src image.Image, limit int) image.Image {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	longEdge := maxInt(width, height)
	if limit <= 0 || longEdge <= limit {
		return src
	}
	scale := float64(limit) / float64(longEdge)
	newWidth := int(float64(width)*scale + 0.5)
	newHeight := int(float64(height)*scale + 0.5)
	if newWidth < 1 {
		newWidth = 1
	}
	if newHeight < 1 {
		newHeight = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))
	for y := 0; y < newHeight; y++ {
		srcY := bounds.Min.Y + y*height/newHeight
		for x := 0; x < newWidth; x++ {
			srcX := bounds.Min.X + x*width/newWidth
			dst.Set(x, y, src.At(srcX, srcY))
		}
	}
	return dst
}

// DecodeConfig 只读文件头解析尺寸（不分配像素缓冲）。
func DecodeConfig(content []byte) (width, height int, err error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

// Decode 完整解码为 image.Image（编码格式由内容决定）。
func Decode(content []byte) (image.Image, error) {
	src, _, err := image.Decode(bytes.NewReader(content))
	return src, err
}

// EncodePNG 把图片编码为 PNG 字节。
func EncodePNG(src image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeJPEG 显式按 JPEG 解码（供需要 JPEG 元数据的分支使用）。
func DecodeJPEG(content []byte) (image.Image, error) {
	return jpeg.Decode(bytes.NewReader(content))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
