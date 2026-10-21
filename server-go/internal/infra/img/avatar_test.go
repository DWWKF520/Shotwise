package img

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func solidPNG(t *testing.T, width, height int, leftHalf color.RGBA) []byte {
	t.Helper()
	src := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if x < width/2 {
				src.SetRGBA(x, y, leftHalf)
			} else {
				src.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
			}
		}
	}
	buf, err := encodePNGBytes(src)
	if err != nil {
		t.Fatal(err)
	}
	return buf
}

func encodePNGBytes(src image.Image) ([]byte, error) {
	var buf growableBytes
	if err := png.Encode(&buf, src); err != nil {
		return nil, err
	}
	return buf.bytes(), nil
}

type growableBytes struct{ b []byte }

func (g *growableBytes) Write(p []byte) (int, error) {
	g.b = append(g.b, p...)
	return len(p), nil
}

func (g *growableBytes) bytes() []byte { return g.b }

func TestCreateAvatarWideSheetCropsLeftPanel(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "sheet.png")
	// 800×400 宽版：左半涂红、右半涂白——宽图走左侧面板兜底裁剪。
	if err := os.WriteFile(source, solidPNG(t, 800, 400, color.RGBA{R: 255, A: 255}), 0o644); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "avatar.png")
	if err := CreateAvatar(source, destination); err != nil {
		t.Fatalf("create avatar: %v", err)
	}
	raw, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("avatar missing: %v", err)
	}
	width, height, err := DecodeConfig(raw)
	if err != nil || width != AvatarSize || height != AvatarSize {
		t.Fatalf("avatar size = %dx%d err=%v", width, height, err)
	}
	// 裁剪框取自左半（红色区），适配后的像素应以红色为主。
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode avatar: %v", err)
	}
	r, g, b, _ := decoded.At(AvatarSize/2, AvatarSize/2).RGBA()
	if r < g || r < b {
		t.Fatalf("中心像素应为红色主导: r=%d g=%d b=%d", r>>8, g>>8, b>>8)
	}
}

func TestCreateAvatarNarrowImageFitsWhole(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "sheet.png")
	if err := os.WriteFile(source, solidPNG(t, 300, 300, color.RGBA{B: 255, A: 255}), 0o644); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "avatar.png")
	if err := CreateAvatar(source, destination); err != nil {
		t.Fatalf("create avatar: %v", err)
	}
	raw, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	width, height, err := DecodeConfig(raw)
	if err != nil || width != AvatarSize || height != AvatarSize {
		t.Fatalf("avatar size = %dx%d err=%v", width, height, err)
	}
	decoded, _ := Decode(raw)
	r, g, b, _ := decoded.At(AvatarSize/2, AvatarSize/2).RGBA()
	if b < r || b < g {
		t.Fatalf("中心像素应为蓝色主导: r=%d g=%d b=%d", r>>8, g>>8, b>>8)
	}
}

func TestCreateAvatarRejectsNonImage(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "junk.png")
	if err := os.WriteFile(source, []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CreateAvatar(source, filepath.Join(dir, "avatar.png")); err == nil {
		t.Fatal("非图片输入应报错")
	}
}

func TestFitSquareVerticalCentering(t *testing.T) {
	// 100×400 竖长图 → 100×100 方形：vCenter=0.42 时取景窗口偏向顶部。
	src := image.NewRGBA(image.Rect(0, 0, 100, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 100; x++ {
			src.SetRGBA(x, y, color.RGBA{R: uint8(y % 256), A: 255})
		}
	}
	out := FitSquare(src, 100, 0.42)
	if got := out.Bounds().Dx(); got != 100 || out.Bounds().Dy() != 100 {
		t.Fatalf("fit size = %dx%d", got, out.Bounds().Dy())
	}
	// 缩放后 top = round((400-100)*0.42) = 126：第 0 行应来自源图 y≈126 处。
	r, _, _, _ := out.At(50, 0).RGBA()
	if int(r>>8) < 120 || int(r>>8) > 132 {
		t.Fatalf("vCenter 取景偏移不符: 顶行亮度 %d，期望 ≈126", r>>8)
	}
}
