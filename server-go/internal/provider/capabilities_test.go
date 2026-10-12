package provider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultVideoCapabilities(t *testing.T) {
	caps := DefaultVideoCapabilities()
	if !caps.FirstFrame {
		t.Fatal("默认 FirstFrame 应为 true（Go 零值 false 不是 Python 默认值）")
	}
	if caps.ReferenceAudioMode != ReferenceAudioNone {
		t.Fatalf("默认 ReferenceAudioMode = %q，期望 %q", caps.ReferenceAudioMode, ReferenceAudioNone)
	}
	if caps.LastFrame || caps.MaxReferenceImages != 0 || caps.MaxReferenceAudioCount != 0 ||
		caps.MaxReferenceAudioTotalSeconds != nil || caps.ReferenceAudioPerImage ||
		caps.MaxPromptChars != nil || caps.FirstFrameRatioAdaptiveOnly {
		t.Fatalf("默认能力声明的其余字段应全为零值：%+v", caps)
	}
}

func TestVideoCapabilitiesNormalized(t *testing.T) {
	if got := (VideoCapabilities{}).Normalized().ReferenceAudioMode; got != ReferenceAudioNone {
		t.Fatalf("空串模式补齐后 = %q，期望 %q", got, ReferenceAudioNone)
	}
	caps := VideoCapabilities{ReferenceAudioMode: ReferenceAudioDirect, FirstFrame: false}
	if got := caps.Normalized(); got.ReferenceAudioMode != ReferenceAudioDirect || got.FirstFrame {
		t.Fatalf("已声明字段不应被改写：%+v", got)
	}
}

func TestVideoCapabilitiesJSONRoundTrip(t *testing.T) {
	limit := 800
	total := 30.0
	caps := VideoCapabilities{
		FirstFrame:                    true,
		LastFrame:                     true,
		MaxReferenceImages:            4,
		ReferenceAudioMode:            ReferenceAudioDirect,
		MaxReferenceAudioCount:        2,
		MaxReferenceAudioTotalSeconds: &total,
		ReferenceAudioPerImage:        true,
		MaxPromptChars:                &limit,
		FirstFrameRatioAdaptiveOnly:   true,
	}
	data, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	for _, field := range []string{
		"first_frame", "last_frame", "max_reference_images", "reference_audio_mode",
		"max_reference_audio_count", "max_reference_audio_total_seconds", "reference_audio_per_image",
		"max_prompt_chars", "first_frame_ratio_adaptive_only",
	} {
		if !strings.Contains(string(data), `"`+field+`"`) {
			t.Fatalf("序列化结果缺少字段 %q：%s", field, data)
		}
	}
	var back VideoCapabilities
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if !reflect.DeepEqual(caps, back) {
		t.Fatalf("往返后不一致：\n原始 %+v\n回读 %+v", caps, back)
	}
}

func TestImageCapabilityNames(t *testing.T) {
	cases := []struct {
		capability ImageCapability
		want       string
	}{
		{CapTextToImage, "text_to_image"},
		{CapImageToImage, "image_to_image"},
		{ImageCapability(0), ""},
	}
	for _, c := range cases {
		if got := c.capability.Name(); got != c.want {
			t.Errorf("ImageCapability(%d).Name() = %q，期望 %q", c.capability, got, c.want)
		}
	}
	if got := CapTextToSpeech.Name(); got != "text_to_speech" {
		t.Errorf("CapTextToSpeech.Name() = %q", got)
	}
	if got := AudioCapability(0).Name(); got != "" {
		t.Errorf("零值音频能力名应为空串，实得 %q", got)
	}
}

func TestImageCapabilitySetJSON(t *testing.T) {
	set := NewImageCapabilitySet(CapTextToImage, CapImageToImage)
	if !set.Has(CapTextToImage) || !set.Has(CapImageToImage) {
		t.Fatalf("聚合后的集合缺少能力位：%v", set)
	}
	if got := set.Names(); !reflect.DeepEqual(got, []string{"image_to_image", "text_to_image"}) {
		t.Fatalf("Names() = %v，期望 Python sorted 字典序", got)
	}
	if got := set.String(); got != "image_to_image|text_to_image" {
		t.Fatalf("String() = %q", got)
	}
	data, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if string(data) != `["image_to_image","text_to_image"]` {
		t.Fatalf("序列化结果 = %s", data)
	}
	var back ImageCapabilitySet
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if back != set {
		t.Fatalf("往返后 = %d，期望 %d", back, set)
	}
	if err := json.Unmarshal([]byte(`["text_to_image"]`), &back); err != nil {
		t.Fatalf("反序列化单元素失败：%v", err)
	}
	if !back.Has(CapTextToImage) || back.Has(CapImageToImage) {
		t.Fatalf("单元素反序列化结果错误：%v", back)
	}
	if err := json.Unmarshal([]byte("null"), &back); err != nil {
		t.Fatalf("null 应反序列化为空集：%v", err)
	}
	if back != 0 {
		t.Fatalf("null 反序列化结果应为空集，实得 %d", back)
	}
	if err := json.Unmarshal([]byte(`["nope"]`), &back); err == nil {
		t.Fatal("未知能力名应报错，不静默丢弃")
	}
	if err := json.Unmarshal([]byte(`"text_to_image"`), &back); err == nil {
		t.Fatal("非数组输入应报错")
	}
	if got := NewImageCapabilitySet().Names(); len(got) != 0 {
		t.Fatalf("空集 Names() 应为空，实得 %v", got)
	}
}

func TestAudioCapabilitySetJSON(t *testing.T) {
	set := NewAudioCapabilitySet(CapTextToSpeech)
	if got := set.Names(); !reflect.DeepEqual(got, []string{"text_to_speech"}) {
		t.Fatalf("Names() = %v", got)
	}
	data, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if string(data) != `["text_to_speech"]` {
		t.Fatalf("序列化结果 = %s", data)
	}
	var back AudioCapabilitySet
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if back != set || !back.Has(CapTextToSpeech) {
		t.Fatalf("往返后 = %d，期望 %d", back, set)
	}
	if err := json.Unmarshal([]byte(`["image_to_image"]`), &back); err == nil {
		t.Fatal("未知音频能力名应报错")
	}
}

func TestVoiceOptionFields(t *testing.T) {
	voice := VoiceOption{ID: "Cherry", Label: "芊悦"}
	data, err := json.Marshal(voice)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if string(data) != `{"id":"Cherry","label":"芊悦"}` {
		t.Fatalf("序列化结果 = %s", data)
	}
}
