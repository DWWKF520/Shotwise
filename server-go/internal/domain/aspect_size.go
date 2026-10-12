// 统一「比例优先、清晰度其次」的尺寸计算（对应 Python lib/aspect_size.py）。
//
// 媒体生成的输出比例只有一个来源——项目的 aspect_ratio，永远优先；分辨率（预设档位或自定义值）
// 只决定清晰度规模，不决定比例。本文件把这条原则收口成两个纯函数 + 一组档位短边表，供各
// 「用像素尺寸」的后端复用：
//
//   - AspectSize —— 给定比例 + 短边目标，算出精确遵循比例、且被 RoundTo 整除的 (宽, 高)。
//   - ResolutionToShortEdge —— 把分辨率（空 / 档位词 / 宽*高 自定义值 / 纯数字）规范化成短边像素，
//     自定义值剥离其自带比例（取 min）。
package domain

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// DefaultShortEdge 缺分辨率但必需尺寸来控制比例时的兜底短边（720P）。
const DefaultShortEdge = 720

// 跨后端统一的档位 → 短边像素。各后端按自身约束（MaxLongEdge / MaxTotalPixels）夹取。
var (
	// ImageTierShortEdge 图片档位：512px / 1K / 2K / 4K。
	ImageTierShortEdge = map[string]int{"512px": 512, "1K": 1024, "2K": 1440, "4K": 2160}
	// VideoTierShortEdge 视频档位：480p / 720p / 1080p / 4K。
	VideoTierShortEdge = map[string]int{"480p": 480, "720p": 720, "1080p": 1080, "4K": 2160}
)

// 默认比例：来源受控（项目固定竖屏短剧居多），无法解析时回退竖屏 9:16，最小惊讶。
const (
	defaultAspectW = 9
	defaultAspectH = 16
)

// whRe 匹配自定义 "宽*高" 值：分隔符为英文 x/X、星号、全角叉号。
var whRe = regexp.MustCompile(`^\s*(\d+)\s*[xX×*]\s*(\d+)\s*$`)

// ParseAspectRatio 把 "9:16" 解析成约简互质的 (9, 16)；非法值回退 (9, 16)。
func ParseAspectRatio(aspectRatio string) (int, int) {
	parts := strings.FieldsFunc(aspectRatio, func(r rune) bool { return r == ':' || r == '：' })
	if len(parts) != 2 {
		return defaultAspectW, defaultAspectH
	}
	aw, errW := strconv.Atoi(strings.TrimSpace(parts[0]))
	ah, errH := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errW != nil || errH != nil || aw <= 0 || ah <= 0 {
		return defaultAspectW, defaultAspectH
	}
	g := gcd(aw, ah)
	return aw / g, ah / g
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// AspectSizeOptions 是后端各自的像素约束。零值表示该项不约束。
type AspectSizeOptions struct {
	// RoundTo 宽高必须被该值整除（gpt-image-2 要求 16）。0 表示不取整（退化为按 1 计算）。
	RoundTo int
	// MaxLongEdge 长边像素上限（如 gpt-image-2 的 4K=3840）。
	MaxLongEdge *int
	// MaxTotalPixels 总像素预算上限（如 DashScope 标准 2048²）。
	MaxTotalPixels *int
	// MaxRatio 后端支持的最极端比例（长短边比）；超出不报错，留 API 判定。
	MaxRatio *float64
}

// AspectRatioExceedsMax 报告该比例是否比后端支持的最极端比例（长短边比）更极端。
//
// Python 侧在 aspect_size 内 warn 后照算；Go 侧把判定单独暴露，由调用方决定告警方式
// （domain 不持有 logger）——超出不抛错、尺寸照算，是否被拒由 API 判定。
func AspectRatioExceedsMax(aspectRatio string, maxRatio float64) bool {
	aw, ah := ParseAspectRatio(aspectRatio)
	return float64(max(aw, ah))/float64(min(aw, ah)) > maxRatio+1e-9
}

// AspectSize 按比例 + 短边目标算出精确遵循比例、且被 RoundTo 整除的 (宽, 高)。
//
// 合法尺寸 = (aw·RoundTo·t, ah·RoundTo·t)（aw:ah 为约简比例，t 正整数）→ 比例零偏差、长宽
// 均被 RoundTo 整除。t 取使短边最接近 shortEdge 的值，再受 MaxLongEdge / MaxTotalPixels 夹取
// （取更严者），下限 t≥1——即便最小合法尺寸超出约束也不取 0，宁可略超上限交 API 判定，
// 也不产出非法的 0 尺寸。
func AspectSize(aspectRatio string, shortEdge int, opt AspectSizeOptions) (int, int) {
	aw, ah := ParseAspectRatio(aspectRatio)

	roundTo := opt.RoundTo
	if roundTo <= 0 {
		roundTo = 1
	}
	shortComp, longComp := min(aw, ah), max(aw, ah)
	shortUnit := roundTo * shortComp // 短边每单位 t 的像素增量

	t := int(math.Round(float64(shortEdge) / float64(shortUnit)))
	if t < 1 {
		t = 1
	}
	if opt.MaxLongEdge != nil {
		maxTLong := *opt.MaxLongEdge / (roundTo * longComp)
		t = min(t, max(1, maxTLong))
	}
	if opt.MaxTotalPixels != nil {
		denom := aw * ah * roundTo * roundTo
		maxTPixels := 0
		if denom > 0 {
			maxTPixels = int(math.Sqrt(float64(*opt.MaxTotalPixels / denom)))
		}
		t = min(t, max(1, maxTPixels))
	}
	t = max(1, t)
	return aw * roundTo * t, ah * roundTo * t
}

// ResolutionToShortEdge 把分辨率规范化成「短边像素」。
//
//   - 空串 → defaultShort
//   - 档位词（大小写不敏感，如 "2K" / "720p"）→ 查 tierMap
//   - 自定义 "宽*高" → 取 min(宽, 高)，剥离其自带比例（比例只由 aspect_ratio 决定）
//   - 纯数字 → 直接当短边
//   - 无法解析 → defaultShort
func ResolutionToShortEdge(resolution string, tierMap map[string]int, defaultShort int) int {
	s := strings.TrimSpace(resolution)
	if s == "" {
		return defaultShort
	}
	if v, ok := tierMap[s]; ok {
		return v
	}
	// 档位词大小写不敏感（用户自定义档位可能写成 "2k" / "4K "）。
	lower := strings.ToLower(s)
	for key, v := range tierMap {
		if strings.ToLower(key) == lower {
			return v
		}
	}
	if m := whRe.FindStringSubmatch(s); m != nil {
		w, _ := strconv.Atoi(m[1])
		h, _ := strconv.Atoi(m[2])
		return min(w, h)
	}
	if v, err := strconv.Atoi(s); err == nil && v > 0 {
		return v
	}
	return defaultShort
}
