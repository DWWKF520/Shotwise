package project

import "testing"

func TestParseAspectRatio(t *testing.T) {
	cases := []struct {
		in   string
		w, h int
	}{
		{"9:16", 9, 16},
		{"16:9", 16, 9},
		{"1:1", 1, 1},
		{"1920：1080", 16, 9}, // 全角冒号 + 非互质输入 → 约简
		{"garbage", 9, 16},
		{"0:16", 9, 16},
	}
	for _, c := range cases {
		w, h := ParseAspectRatio(c.in)
		if w != c.w || h != c.h {
			t.Errorf("ParseAspectRatio(%q) = (%d, %d), want (%d, %d)", c.in, w, h, c.w, c.h)
		}
	}
}

func TestAspectSize(t *testing.T) {
	// 短边取最接近 720 且被 16 整除的合法尺寸：t = round(720 / (16*9)) = 5 → 720x1280。
	w, h := AspectSize("9:16", DefaultShortEdge, AspectSizeOptions{RoundTo: 16})
	if w != 720 || h != 1280 {
		t.Fatalf("AspectSize(9:16, 720, 16) = %dx%d, want 720x1280", w, h)
	}

	// 长边上限夹取：4K=3840 长边 → t 从 round(2160/(16*9))=15 降到 3840/(16*16)=15（恰好）。
	maxLong := 3840
	w, h = AspectSize("9:16", 2160, AspectSizeOptions{RoundTo: 16, MaxLongEdge: &maxLong})
	if h > maxLong {
		t.Fatalf("long edge %d exceeds max %d", h, maxLong)
	}

	// 极端比例下长边上限仍生效，且尺寸不为 0。
	maxLong = 2560
	w, h = AspectSize("1:3", 2160, AspectSizeOptions{RoundTo: 16, MaxLongEdge: &maxLong})
	if w <= 0 || h <= 0 {
		t.Fatalf("size must stay positive, got %dx%d", w, h)
	}
	if h%16 != 0 || w%16 != 0 {
		t.Fatalf("size must align to 16, got %dx%d", w, h)
	}

	// 总像素预算夹取。
	maxPixels := 2048 * 2048
	w, h = AspectSize("1:1", 4096, AspectSizeOptions{RoundTo: 16, MaxTotalPixels: &maxPixels})
	if w*h > maxPixels {
		t.Fatalf("total pixels %d exceed budget %d", w*h, maxPixels)
	}

	if !AspectRatioExceedsMax("1:4", 3.0) {
		t.Fatal("1:4 should exceed max ratio 3")
	}
	if AspectRatioExceedsMax("3:1", 3.0) {
		t.Fatal("3:1 should not exceed max ratio 3")
	}
}

func TestResolutionToShortEdge(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", DefaultShortEdge},
		{"2K", 1440},
		{"2k", 1440},
		{"4K", 2160},
		{"1920x1080", 1080},
		{"1080*1920", 1080},
		{"960", 960},
		{"nonsense", DefaultShortEdge},
	}
	for _, c := range cases {
		if got := ResolutionToShortEdge(c.in, ImageTierShortEdge, DefaultShortEdge); got != c.want {
			t.Errorf("ResolutionToShortEdge(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	if got := ResolutionToShortEdge("720p", VideoTierShortEdge, DefaultShortEdge); got != 720 {
		t.Errorf("video tier lookup failed: %d", got)
	}
}
