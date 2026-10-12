package provider

import (
	"reflect"
	"testing"
)

func TestVideoBucketOrderAndDefault(t *testing.T) {
	if !reflect.DeepEqual(VideoBucketOrder, []VideoBucket{BucketI2V, BucketR2V}) {
		t.Fatalf("桶遍历顺序 = %v", VideoBucketOrder)
	}
	if BucketI2V != "i2v" || BucketR2V != "r2v" || DefaultVideoBucket != BucketI2V {
		t.Fatalf("桶取值异常：%q %q %q", BucketI2V, BucketR2V, DefaultVideoBucket)
	}
}

func TestVideoCapabilitySatisfied(t *testing.T) {
	cases := []struct {
		name         string
		bucket       VideoBucket
		firstFrame   bool
		maxRefImages int
		want         bool
	}{
		{"i2v 看首帧", BucketI2V, true, 0, true},
		{"i2v 无首帧不满足", BucketI2V, false, 5, false},
		{"i2v 首帧与参考图并存", BucketI2V, true, 5, true},
		{"r2v 看参考图上限", BucketR2V, false, 1, true},
		{"r2v 上限为 0 不满足", BucketR2V, false, 0, false},
		{"r2v 上限为 0 即便有首帧也不满足", BucketR2V, true, 0, false},
		{"r2v 上限与首帧并用", BucketR2V, false, 4, true},
	}
	for _, c := range cases {
		if got := VideoCapabilitySatisfied(c.bucket, c.firstFrame, c.maxRefImages); got != c.want {
			t.Errorf("%s: VideoCapabilitySatisfied(%q, %v, %d) = %v，期望 %v",
				c.name, c.bucket, c.firstFrame, c.maxRefImages, got, c.want)
		}
	}
}

func TestSatisfiedVideoBuckets(t *testing.T) {
	cases := []struct {
		name string
		caps VideoCapabilities
		want []VideoBucket
	}{
		{"仅首帧", VideoCapabilities{FirstFrame: true}, []VideoBucket{BucketI2V}},
		{"仅参考图", VideoCapabilities{MaxReferenceImages: 2}, []VideoBucket{BucketR2V}},
		{"两者兼备", VideoCapabilities{FirstFrame: true, MaxReferenceImages: 3}, []VideoBucket{BucketI2V, BucketR2V}},
		{"都不支持（纯文生）", VideoCapabilities{}, nil},
	}
	for _, c := range cases {
		got := SatisfiedVideoBuckets(c.caps)
		if len(got) != len(c.want) {
			t.Errorf("%s: SatisfiedVideoBuckets = %v，期望 %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: 桶[%d] = %q，期望 %q", c.name, i, got[i], c.want[i])
			}
		}
	}
}

func TestVideoBucketForTaskType(t *testing.T) {
	cases := []struct {
		taskType string
		want     VideoBucket
		wantOK   bool
	}{
		{"video", BucketI2V, true},
		{"reference_video", BucketR2V, true},
		{"image", "", false},
		{"audio", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := VideoBucketForTaskType(c.taskType)
		if ok != c.wantOK || got != c.want {
			t.Errorf("VideoBucketForTaskType(%q) = (%q, %v)，期望 (%q, %v)", c.taskType, got, ok, c.want, c.wantOK)
		}
	}
}

func TestVideoBucketForGenerationMode(t *testing.T) {
	cases := []struct {
		mode string
		want VideoBucket
	}{
		{"storyboard", BucketI2V},
		{"reference_video", BucketR2V},
		{"", BucketI2V},           // 脏值（空串）落默认桶
		{"dirty-mode", BucketI2V}, // 脏值落默认桶
	}
	for _, c := range cases {
		if got := VideoBucketForGenerationMode(c.mode); got != c.want {
			t.Errorf("VideoBucketForGenerationMode(%q) = %q，期望 %q", c.mode, got, c.want)
		}
	}
}

func TestReferenceVideoBucket(t *testing.T) {
	if got := ReferenceVideoBucket(true); got != BucketR2V {
		t.Fatalf("有参考图应落 r2v，实得 %q", got)
	}
	if got := ReferenceVideoBucket(false); got != BucketI2V {
		t.Fatalf("无参考图的退化镜头应落 i2v，实得 %q", got)
	}
}

func TestResolveVideoBucket(t *testing.T) {
	cases := []struct {
		name           string
		taskType       string
		generationMode string
		withReferences bool
		want           VideoBucket
	}{
		{"video 任务恒 i2v", "video", "reference_video", true, BucketI2V},
		{"reference_video 有参考图落 r2v", "reference_video", "storyboard", true, BucketR2V},
		{"reference_video 无参考图退化 i2v", "reference_video", "storyboard", false, BucketI2V},
		{"表外任务类型按生成模式定桶", "grid", "storyboard", true, BucketI2V},
		{"表外任务类型参考路线落 r2v", "image", "reference_video", false, BucketR2V},
		{"无任务类型与项目上下文落默认桶", "", "", true, BucketI2V},
		{"空生成模式落默认桶", "", "", false, BucketI2V},
		{"脏生成模式落默认桶", "unknown-type", "dirty-mode", true, BucketI2V},
	}
	for _, c := range cases {
		if got := ResolveVideoBucket(c.taskType, c.generationMode, c.withReferences); got != c.want {
			t.Errorf("%s: ResolveVideoBucket(%q, %q, %v) = %q，期望 %q",
				c.name, c.taskType, c.generationMode, c.withReferences, got, c.want)
		}
	}
}
