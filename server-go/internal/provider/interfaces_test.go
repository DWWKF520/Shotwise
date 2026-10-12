package provider

import (
	"errors"
	"strings"
	"testing"
)

func TestMediaTypeConstants(t *testing.T) {
	if MediaImage != "image" || MediaVideo != "video" || MediaAudio != "audio" || MediaText != "text" {
		t.Fatalf("媒体通道取值与队列 lane 约定不符：%q %q %q %q", MediaImage, MediaVideo, MediaAudio, MediaText)
	}
}

func TestNewGenerationRequestDefaults(t *testing.T) {
	video := NewVideoGenerationRequest("看镜头", "out.mp4")
	if video.Prompt != "看镜头" || video.OutputPath != "out.mp4" {
		t.Fatalf("必填字段未落位：%+v", video)
	}
	if video.AspectRatio != "9:16" || video.DurationSeconds != 5 || !video.GenerateAudio || video.ServiceTier != "default" {
		t.Fatalf("默认值与 Python dataclass 不一致：%+v", video)
	}
	if video.Seed != nil || video.StartImage != "" || len(video.ReferenceAudioFiles) != 0 {
		t.Fatalf("可选字段应保持零值：%+v", video)
	}

	image := NewImageGenerationRequest("一匹马", "out.png")
	if image.AspectRatio != "9:16" || image.Prompt != "一匹马" || image.OutputPath != "out.png" {
		t.Fatalf("图片请求默认值错误：%+v", image)
	}

	audio := NewAudioSynthesisRequest("你好", "out.mp3", "Cherry")
	if audio.LanguageType != "Chinese" || audio.Voice != "Cherry" || audio.Speed != nil {
		t.Fatalf("音频请求默认值错误：%+v", audio)
	}
}

func TestAmbiguousSubmitError(t *testing.T) {
	cause := errors.New("read timeout")
	err := &AmbiguousSubmitError{Provider: "ark", Err: cause}
	if !strings.Contains(err.Error(), "[create_ambiguous]") || !strings.Contains(err.Error(), "ark") {
		t.Fatalf("错误文案应带 [create_ambiguous] 与 provider 名：%s", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Fatal("Unwrap 应保留底层传输错误")
	}
}

func TestResumeErrors(t *testing.T) {
	expired := &ResumeExpiredError{JobID: "job-1", Provider: "vidu"}
	if !strings.Contains(expired.Error(), "job-1") || !strings.Contains(expired.Error(), "vidu") {
		t.Fatalf("默认文案应含 job 与 provider：%s", expired.Error())
	}
	custom := &ResumeExpiredError{JobID: "job-2", Provider: "vidu", Message: "自定义文案"}
	if custom.Error() != "自定义文案" {
		t.Fatalf("Message 非空时应原样返回：%s", custom.Error())
	}

	changed := &ResumeEndpointChangedError{
		JobID: "job-3", Provider: "custom-x", SubmittedEndpoint: "ep_a", CurrentEndpoint: "ep_b",
	}
	for _, frag := range []string{"job-3", "custom-x", "ep_a", "ep_b"} {
		if !strings.Contains(changed.Error(), frag) {
			t.Fatalf("错误文案缺片段 %q：%s", frag, changed.Error())
		}
	}
}

func TestErrResumeUnsupported(t *testing.T) {
	if ErrResumeUnsupported == nil || !strings.Contains(ErrResumeUnsupported.Error(), "resume") {
		t.Fatalf("ErrResumeUnsupported 文案异常：%v", ErrResumeUnsupported)
	}
}

func TestCapabilityErrorForms(t *testing.T) {
	err := NewVideoCapabilityError(FailureVideoPromptTooLong, map[string]any{"limit": 10, "count": 12})
	if err.Media != MediaVideo {
		t.Fatalf("Media = %q，期望 video", err.Media)
	}
	if err.Error() != "video_prompt_too_long" {
		t.Fatalf("Error() 应只返回机器码，实得 %q", err.Error())
	}
	if err.Detail() != `[video_prompt_too_long] {"count": 12, "limit": 10}` {
		t.Fatalf("Detail() = %q", err.Detail())
	}
	imageErr := NewImageCapabilityError(FailureImageCapabilityMissingT2I, nil)
	if imageErr.Media != MediaImage || imageErr.Error() != "image_capability_missing_t2i" {
		t.Fatalf("图片侧能力错误异常：%+v", imageErr)
	}
}
