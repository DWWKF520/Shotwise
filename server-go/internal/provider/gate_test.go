package provider

import (
	"reflect"
	"strings"
	"testing"
)

func newPromptLimit(limit int) *int { return &limit }

func newSeconds(value float64) *float64 { return &value }

// assertGateRejects 断言闸门以指定失败码拒绝，并返回能力错误供参数核对。
func assertGateRejects(t *testing.T, err error, want FailureCode) *CapabilityError {
	t.Helper()
	if err == nil {
		t.Fatalf("应拒绝（码 %s），实得 nil", want)
	}
	capErr, ok := AssertCapabilityError(err)
	if !ok {
		t.Fatalf("错误应为 *CapabilityError，实得 %T：%v", err, err)
	}
	if capErr.Code != want {
		t.Fatalf("失败码 = %s，期望 %s", capErr.Code, want)
	}
	if capErr.Media != MediaVideo {
		t.Fatalf("能力错误媒体类型 = %q，期望 video", capErr.Media)
	}
	if capErr.Error() != string(want) {
		t.Fatalf("Error() = %q，应只返回机器码", capErr.Error())
	}
	return capErr
}

func TestGateVideoRequestPasses(t *testing.T) {
	direct := DefaultVideoCapabilities()
	direct.LastFrame = true
	direct.MaxReferenceImages = 3
	direct.ReferenceAudioMode = ReferenceAudioDirect
	direct.MaxReferenceAudioCount = 2
	direct.MaxReferenceAudioTotalSeconds = newSeconds(30)
	direct.MaxPromptChars = newPromptLimit(10)

	prompt := strings.Repeat("字", 10)
	cases := []struct {
		name string
		caps *VideoCapabilities
		in   VideoGateInput
	}{
		{"空输入空能力声明", nil, VideoGateInput{}},
		{"提示词恰在上限", &direct, VideoGateInput{Prompt: &prompt}},
		{"尾帧受支持", &direct, VideoGateInput{EndImage: true}},
		{"参考图恰在上限", &direct, VideoGateInput{ReferenceImages: []string{"a", "b", "c"}}},
		{"参考音频恰在段数上限", &direct, VideoGateInput{ReferenceAudioFiles: []string{"a", "b"}}},
		{"参考音频恰在总时长上限", &direct, VideoGateInput{
			ReferenceAudioFiles: []string{"a"}, ReferenceAudioTotalSeconds: newSeconds(30),
		}},
		{"总时长未知时跳过该项校验", &direct, VideoGateInput{
			ReferenceAudioFiles: []string{"a", "b"}, ReferenceAudioTotalSeconds: nil,
		}},
		{"未声明提示词上限时跳过（不视为 0）", &VideoCapabilities{FirstFrame: true},
			VideoGateInput{Prompt: &prompt}},
		{"空参考图列表不触发校验", &direct, VideoGateInput{ReferenceImages: []string{}}},
		{"空参考音频列表不触发校验", &VideoCapabilities{}, VideoGateInput{ReferenceAudioFiles: nil}},
	}
	for _, c := range cases {
		if err := GateVideoRequest(c.caps, "ark", "m", c.in); err != nil {
			t.Errorf("%s: 应通过，实得 %v", c.name, err)
		}
	}
}

func TestGateVideoRequestPromptTooLong(t *testing.T) {
	caps := DefaultVideoCapabilities()
	caps.MaxPromptChars = newPromptLimit(10)

	// 计量口径为字符数（中英文同权）：10 个汉字 = 30 字节，按字节算会误拒。
	ok := strings.Repeat("字", 10)
	if err := GateVideoRequest(&caps, "ark", "m", VideoGateInput{Prompt: &ok}); err != nil {
		t.Fatalf("10 个汉字不应超限：%v", err)
	}
	bad := strings.Repeat("字", 11)
	capErr := assertGateRejects(t, GateVideoRequest(&caps, "ark", "m", VideoGateInput{Prompt: &bad}),
		FailureVideoPromptTooLong)
	if capErr.Params["limit"] != 10 || capErr.Params["count"] != 11 {
		t.Fatalf("超限参数错误：%v", capErr.Params)
	}
	if capErr.Params["provider"] != "ark" || capErr.Params["model"] != "m" {
		t.Fatalf("上下文参数错误：%v", capErr.Params)
	}

	// Prompt 为 nil 表示调用方未提供文本，不计长度。
	if err := GateVideoRequest(&caps, "ark", "m", VideoGateInput{Prompt: nil}); err != nil {
		t.Fatalf("nil 提示词不应校验：%v", err)
	}
	// caps 为 nil 时未声明上限，跳过该项。
	if err := GateVideoRequest(nil, "ark", "m", VideoGateInput{Prompt: &bad}); err != nil {
		t.Fatalf("caps 为 nil 时不应校验提示词长度：%v", err)
	}
}

func TestGateVideoRequestEndImage(t *testing.T) {
	caps := DefaultVideoCapabilities()
	capErr := assertGateRejects(t, GateVideoRequest(&caps, "vidu", "m", VideoGateInput{EndImage: true}),
		FailureVideoLastFrameUnsupported)
	if capErr.Params["provider"] != "vidu" {
		t.Fatalf("尾帧拒绝参数错误：%v", capErr.Params)
	}
	capErr = assertGateRejects(t, GateVideoRequest(nil, "vidu", "m", VideoGateInput{EndImage: true}),
		FailureVideoLastFrameUnsupported)
	_ = capErr

	caps.LastFrame = true
	if err := GateVideoRequest(&caps, "vidu", "m", VideoGateInput{EndImage: true}); err != nil {
		t.Fatalf("声明 LastFrame 后应放行：%v", err)
	}
}

func TestGateVideoRequestReferenceImages(t *testing.T) {
	caps := DefaultVideoCapabilities()
	caps.MaxReferenceImages = 3

	capErr := assertGateRejects(t, GateVideoRequest(&caps, "ark", "m",
		VideoGateInput{ReferenceImages: []string{"a", "b", "c", "d"}}), FailureVideoReferenceImagesExceeded)
	if capErr.Params["limit"] != 3 || capErr.Params["count"] != 4 {
		t.Fatalf("超量参数错误：%v", capErr.Params)
	}
	if err := GateVideoRequest(&caps, "ark", "m",
		VideoGateInput{ReferenceImages: []string{"a", "b", "c"}}); err != nil {
		t.Fatalf("恰在上限应放行：%v", err)
	}

	// 上限 <= 0 视为路径不可用（不设布尔位，避免「称支持但上限为 0」的自相矛盾声明）。
	noRefs := DefaultVideoCapabilities()
	assertGateRejects(t, GateVideoRequest(&noRefs, "grok", "m",
		VideoGateInput{ReferenceImages: []string{"a"}}), FailureVideoReferenceImagesUnsupported)
	assertGateRejects(t, GateVideoRequest(nil, "grok", "m",
		VideoGateInput{ReferenceImages: []string{"a"}}), FailureVideoReferenceImagesUnsupported)
}

func TestGateVideoRequestReferenceAudio(t *testing.T) {
	// 模式 none（含零值空串）收到音频硬失败，不静默丢弃。
	none := DefaultVideoCapabilities()
	capErr := assertGateRejects(t, GateVideoRequest(&none, "kling", "m",
		VideoGateInput{ReferenceAudioFiles: []string{"a"}}), FailureVideoReferenceAudioUnsupported)
	_ = capErr
	zeroMode := VideoCapabilities{FirstFrame: true, ReferenceAudioMode: ""}
	assertGateRejects(t, GateVideoRequest(&zeroMode, "kling", "m",
		VideoGateInput{ReferenceAudioFiles: []string{"a"}}), FailureVideoReferenceAudioUnsupported)
	assertGateRejects(t, GateVideoRequest(nil, "kling", "m",
		VideoGateInput{ReferenceAudioFiles: []string{"a"}}), FailureVideoReferenceAudioUnsupported)

	// 直传模式：段数上限。
	direct := DefaultVideoCapabilities()
	direct.ReferenceAudioMode = ReferenceAudioDirect
	direct.MaxReferenceAudioCount = 2
	capErr = assertGateRejects(t, GateVideoRequest(&direct, "ark", "m",
		VideoGateInput{ReferenceAudioFiles: []string{"a", "b", "c"}}), FailureVideoReferenceAudioExceeded)
	if capErr.Params["limit"] != 2 || capErr.Params["count"] != 3 {
		t.Fatalf("段数超限参数错误：%v", capErr.Params)
	}
	if err := GateVideoRequest(&direct, "ark", "m",
		VideoGateInput{ReferenceAudioFiles: []string{"a", "b"}}); err != nil {
		t.Fatalf("段数恰在上限应放行：%v", err)
	}

	// 直传模式：总时长上限（仅当能力声明与调用方探测值都非 nil 时判定）。
	direct.MaxReferenceAudioTotalSeconds = newSeconds(30)
	capErr = assertGateRejects(t, GateVideoRequest(&direct, "ark", "m", VideoGateInput{
		ReferenceAudioFiles: []string{"a"}, ReferenceAudioTotalSeconds: newSeconds(30.5),
	}), FailureVideoReferenceAudioDurationExceeded)
	if capErr.Params["limit"] != 30.0 || capErr.Params["total"] != 30.5 {
		t.Fatalf("总时长超限参数错误：%v", capErr.Params)
	}
	if err := GateVideoRequest(&direct, "ark", "m", VideoGateInput{
		ReferenceAudioFiles: []string{"a"}, ReferenceAudioTotalSeconds: newSeconds(10),
	}); err != nil {
		t.Fatalf("总时长在限内应放行：%v", err)
	}
	direct.MaxReferenceAudioTotalSeconds = nil
	if err := GateVideoRequest(&direct, "ark", "m", VideoGateInput{
		ReferenceAudioFiles: []string{"a"}, ReferenceAudioTotalSeconds: newSeconds(1000),
	}); err != nil {
		t.Fatalf("能力未声明总时长约束时应放行：%v", err)
	}
}

func TestGateVideoRequestCheckOrder(t *testing.T) {
	// 校验顺序固定：提示词 → 尾帧 → 参考图 → 参考音频，先命中先返回。
	caps := DefaultVideoCapabilities()
	caps.MaxPromptChars = newPromptLimit(5)
	long := strings.Repeat("x", 6)
	capErr := assertGateRejects(t, GateVideoRequest(&caps, "ark", "m", VideoGateInput{
		Prompt:              &long,
		EndImage:            true,
		ReferenceImages:     []string{"a"},
		ReferenceAudioFiles: []string{"b"},
	}), FailureVideoPromptTooLong)
	_ = capErr

	capErr = assertGateRejects(t, GateVideoRequest(&caps, "ark", "m", VideoGateInput{
		EndImage:            true,
		ReferenceImages:     []string{"a"},
		ReferenceAudioFiles: []string{"b"},
	}), FailureVideoLastFrameUnsupported)
	_ = capErr

	capErr = assertGateRejects(t, GateVideoRequest(&caps, "ark", "m", VideoGateInput{
		ReferenceImages:     []string{"a"},
		ReferenceAudioFiles: []string{"b"},
	}), FailureVideoReferenceImagesUnsupported)
	_ = capErr

	capErr = assertGateRejects(t, GateVideoRequest(&caps, "ark", "m", VideoGateInput{
		ReferenceAudioFiles: []string{"b"},
	}), FailureVideoReferenceAudioUnsupported)
	_ = capErr
}

func TestAssertCapabilityError(t *testing.T) {
	if _, ok := AssertCapabilityError(nil); ok {
		t.Fatal("nil 不应判定为能力错误")
	}
	if _, ok := AssertCapabilityError(errUnableForTest{}); ok {
		t.Fatal("普通错误不应判定为能力错误")
	}
	wrapped := wrapError{Err: NewVideoCapabilityError(FailureVideoLastFrameUnsupported, nil)}
	capErr, ok := AssertCapabilityError(wrapped)
	if !ok || capErr.Code != FailureVideoLastFrameUnsupported {
		t.Fatalf("错误链上的能力错误应被识别：%v %v", capErr, ok)
	}
}

type errUnableForTest struct{}

func (errUnableForTest) Error() string { return "普通错误" }

type wrapError struct{ Err error }

func (e wrapError) Error() string { return "外层：" + e.Err.Error() }
func (e wrapError) Unwrap() error { return e.Err }

// ---- 能力查询与首帧比例改写 ----

type staticProbe struct{ caps VideoCapabilities }

func (p staticProbe) VideoCapabilities() VideoCapabilities { return p.caps }

type tierProbe struct {
	static VideoCapabilities
	tier   VideoCapabilities
}

func (p tierProbe) VideoCapabilities() VideoCapabilities { return p.static }

func (p tierProbe) VideoCapabilitiesForTier(serviceTier, resolution string) VideoCapabilities {
	if serviceTier == "pro" && resolution == "1080p" {
		return p.tier
	}
	return p.static
}

func TestResolveVideoCapabilities(t *testing.T) {
	static := DefaultVideoCapabilities()
	tier := DefaultVideoCapabilities()
	tier.LastFrame = true

	if got := ResolveVideoCapabilities(staticProbe{caps: static}, "default", "720p"); !reflect.DeepEqual(got, static) {
		t.Fatalf("静态 backend 应返回静态声明：%+v", got)
	}
	if got := ResolveVideoCapabilities(tierProbe{static: static, tier: tier}, "pro", "1080p"); !got.LastFrame {
		t.Fatalf("档位感知 backend 在 pro/1080p 应返回精确声明：%+v", got)
	}
	if got := ResolveVideoCapabilities(tierProbe{static: static, tier: tier}, "default", "720p"); !reflect.DeepEqual(got, static) {
		t.Fatalf("非目标档位应回落静态声明：%+v", got)
	}
}

func TestResolveFirstFrameAspectRatio(t *testing.T) {
	adaptive := DefaultVideoCapabilities()
	adaptive.FirstFrameRatioAdaptiveOnly = true

	cases := []struct {
		name          string
		caps          *VideoCapabilities
		aspectRatio   string
		hasFirstFrame bool
		want          string
	}{
		{"未查询能力时原样返回", nil, "16:9", true, "16:9"},
		{"未声明约束时原样返回", &VideoCapabilities{}, "16:9", true, "16:9"},
		{"声明约束且带首帧时改写为 adaptive", &adaptive, "16:9", true, "adaptive"},
		{"声明约束但不带首帧时不受影响", &adaptive, "16:9", false, "16:9"},
		{"已是 adaptive 时保持不变", &adaptive, "adaptive", true, "adaptive"},
	}
	for _, c := range cases {
		if got := ResolveFirstFrameAspectRatio(c.caps, c.aspectRatio, c.hasFirstFrame); got != c.want {
			t.Errorf("%s: ResolveFirstFrameAspectRatio = %q，期望 %q", c.name, got, c.want)
		}
	}
	if FirstFrameAdaptiveRatio != "adaptive" {
		t.Fatalf("供应商侧字面量 = %q", FirstFrameAdaptiveRatio)
	}
}
