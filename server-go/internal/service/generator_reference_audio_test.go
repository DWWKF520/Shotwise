// 参考音频多段总时长闸门的生成侧接入测试（对应 lib/media_generator.py 中
// probe_reference_audio_total_seconds 的条件探测 + gate_video_request 传递）。
//
// 探测本身（求和 / 任一失败整体失败）由 infra/ffmpeg 的 MultiTotalDuration 测试覆盖，
// gate 的拒绝语义由 provider 包测试覆盖；本文件钉死三件事：
//   - caps 声明总时长约束且参考音频超限时，生成在记账括号前被拦下（backend 不被调用）；
//   - 探测失败（文件缺失 / ffprobe 不可用）按 nil 传入，跳过该单项校验不阻断请求；
//   - caps 未声明总时长约束时不探测，超限音频照常放行（省 ffprobe 开销的语义边界）。
package service

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/infra/ffmpeg"
	"github.com/shotwise/shotwise/internal/provider"
)

// gateAudioFakeBackend 声明总时长上限的假视频后端：Generate 只记调用、返回最小结果。
type gateAudioFakeBackend struct {
	called bool
	limit  *float64
}

func (b *gateAudioFakeBackend) Name() string  { return "fake-audio" }
func (b *gateAudioFakeBackend) Model() string { return "fake-model" }

func (b *gateAudioFakeBackend) VideoCapabilities() provider.VideoCapabilities {
	count := 5
	return provider.VideoCapabilities{
		ReferenceAudioMode:            provider.ReferenceAudioDirect,
		MaxReferenceAudioCount:        count,
		MaxReferenceAudioTotalSeconds: b.limit,
	}
}

func (b *gateAudioFakeBackend) Generate(
	ctx context.Context, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	b.called = true
	return provider.VideoGenerationResult{VideoPath: req.OutputPath}, nil
}

func (b *gateAudioFakeBackend) ResumeVideo(
	ctx context.Context, jobID string, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	return provider.VideoGenerationResult{}, provider.ErrResumeUnsupported
}

// writeSineWav 生成一段指定秒数的正弦波 wav，供 ffprobe 探测真实时长。
func writeSineWav(t *testing.T, path string, seconds int) {
	t.Helper()
	tool, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available on PATH")
	}
	cmd := exec.Command(tool,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration="+strconv.Itoa(seconds),
		path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate sine wav: %v: %s", err, out)
	}
}

// newGateAudioGenerator 构造不记账、不落库的最小生成器：GenerateAudio 显式提供以绕开
// 配置读取，TaskID 留空以绕开 payload 写回，ledger 为 nil 以绕开记账括号。
func newGateAudioGenerator() *VideoGenerator {
	return NewVideoGenerator(nil, nil)
}

func gateAudioParams(dir string, audioFiles []string) VideoGenerateParams {
	generateAudio := true
	return VideoGenerateParams{
		ProjectName:         "p1",
		ProjectDir:          dir,
		ResourceType:        "videos",
		ResourceID:          "scene_1",
		Prompt:              "p",
		AspectRatio:         "16:9",
		DurationSeconds:     5,
		Resolution:          "720p",
		ReferenceAudioFiles: audioFiles,
		GenerateAudio:       &generateAudio,
	}
}

func TestGenerateGatesReferenceAudioTotalDuration(t *testing.T) {
	if !ffmpeg.ProbeAvailable() {
		t.Skip("ffprobe not available on PATH")
	}
	dir := t.TempDir()
	wav1 := filepath.Join(dir, "a1.wav")
	wav2 := filepath.Join(dir, "a2.wav")
	writeSineWav(t, wav1, 2)
	writeSineWav(t, wav2, 2)

	limit := 3.0
	backend := &gateAudioFakeBackend{limit: &limit}
	target := &VideoTarget{
		ProviderModel: configProviderModelForGateTest(),
		Backend:       backend,
		durations:     []int{5},
		caps:          backend.VideoCapabilities(),
	}
	gen := newGateAudioGenerator()

	// 两段 2s 共 4s > 3s 上限：记账括号前拦下，backend 不被调用。
	_, err := gen.Generate(context.Background(), target, gateAudioParams(dir, []string{wav1, wav2}))
	if err == nil {
		t.Fatalf("expected duration_exceeded error, got nil")
	}
	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) || capErr.Code != provider.FailureVideoReferenceAudioDurationExceeded {
		t.Fatalf("expected video_reference_audio_duration_exceeded, got %v", err)
	}
	if backend.called {
		t.Fatalf("backend must not be called when gate rejects")
	}

	// 单段 2s 在上限内：照常生成。
	backend.called = false
	if _, err := gen.Generate(context.Background(), target, gateAudioParams(dir, []string{wav1})); err != nil {
		t.Fatalf("in-limit audio should generate: %v", err)
	}
	if !backend.called {
		t.Fatalf("backend should be called for in-limit audio")
	}
}

func TestGenerateSkipsTotalDurationCheckWhenProbeFails(t *testing.T) {
	if !ffmpeg.ProbeAvailable() {
		t.Skip("ffprobe not available on PATH")
	}
	dir := t.TempDir()
	// 指向不存在文件：探测失败 → nil → 按降级口径跳过总时长校验，不阻断请求。
	missing := filepath.Join(dir, "missing.wav")
	limit := 1.0
	backend := &gateAudioFakeBackend{limit: &limit}
	target := &VideoTarget{
		ProviderModel: configProviderModelForGateTest(),
		Backend:       backend,
		durations:     []int{5},
		caps:          backend.VideoCapabilities(),
	}
	gen := newGateAudioGenerator()
	if _, err := gen.Generate(context.Background(), target, gateAudioParams(dir, []string{missing})); err != nil {
		t.Fatalf("probe failure should degrade to skip, got %v", err)
	}
	if !backend.called {
		t.Fatalf("backend should be called when probe degrades")
	}
}

func TestGenerateSkipsProbeWhenCapabilityUndeclared(t *testing.T) {
	dir := t.TempDir()
	wav1 := filepath.Join(dir, "a1.wav")
	wav2 := filepath.Join(dir, "a2.wav")
	writeSineWav(t, wav1, 2)
	writeSineWav(t, wav2, 2)

	// 未声明总时长约束（limit nil）：不探测、不拦截，即便真实总时长超限。
	backend := &gateAudioFakeBackend{}
	target := &VideoTarget{
		ProviderModel: configProviderModelForGateTest(),
		Backend:       backend,
		durations:     []int{5},
		caps:          backend.VideoCapabilities(),
	}
	gen := newGateAudioGenerator()
	if _, err := gen.Generate(context.Background(), target, gateAudioParams(dir, []string{wav1, wav2})); err != nil {
		t.Fatalf("undeclared limit should not gate, got %v", err)
	}
	if !backend.called {
		t.Fatalf("backend should be called when limit undeclared")
	}
}

// configProviderModelForGateTest 返回占位身份：Generate 只在结算（ledger 非 nil）时读它，
// 不记账路径下字段值不影响行为。
func configProviderModelForGateTest() config.ProviderModel {
	return config.ProviderModel{}
}
