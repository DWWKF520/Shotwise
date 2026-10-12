package ffmpeg

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// 真实工具用例：机器上没装 ffmpeg/ffprobe 时整体跳过，其余用例仍靠桩覆盖
// argv 形状与分类逻辑。
func requireRealTools(t *testing.T) {
	t.Helper()
	if !Available() {
		t.Skip("未安装 ffmpeg，跳过真实管线用例")
	}
	if !ProbeAvailable() {
		t.Skip("未安装 ffprobe，跳过真实管线用例")
	}
}

// makeRealClip 生成一段带音轨的测试素材（lavfi 彩条 + 正弦音）。
func makeRealClip(t *testing.T, path string, seconds int) {
	t.Helper()
	length := strconv.Itoa(seconds)
	args := []string{
		"ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=24:duration=" + length,
		"-f", "lavfi", "-i", "sine=frequency=440:duration=" + length,
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest",
		path,
	}
	if err := Run(context.Background(), 60*time.Second, args); err != nil {
		t.Fatalf("generate clip: %v", err)
	}
}

func TestRealProbeFramesAndThumbnail(t *testing.T) {
	requireRealTools(t)
	ctx := context.Background()
	dir := t.TempDir()
	clip := filepath.Join(dir, "clip.mp4")
	makeRealClip(t, clip, 1)

	duration, ok := DurationSeconds(ctx, clip)
	if !ok || duration <= 0 {
		t.Fatalf("DurationSeconds = %v, %v; want a positive duration", duration, ok)
	}
	if frames, ok := CountFrames(ctx, clip); !ok || frames <= 0 {
		t.Errorf("CountFrames = %d, %v; want a positive frame count", frames, ok)
	}
	if frames, ok := ContainerFrameCount(ctx, clip); !ok || frames <= 0 {
		t.Errorf("ContainerFrameCount = %d, %v; want a positive frame count", frames, ok)
	}
	if audio, ok := AudioDuration(ctx, clip); !ok || audio <= 0 {
		t.Errorf("AudioDuration = %v, %v; want a positive duration", audio, ok)
	}
	total, ok := MultiTotalDuration(ctx, []string{clip, clip})
	if !ok || total != 2*duration {
		t.Errorf("MultiTotalDuration = %v, %v; want two probed durations", total, ok)
	}

	thumbnail := filepath.Join(dir, "thumb.jpg")
	if !MakeThumbnail(ctx, clip, thumbnail) {
		t.Error("MakeThumbnail = false, want true")
	}
	if !nonEmptyFile(thumbnail) {
		t.Error("thumbnail is empty")
	}
	if !ExtractFrame(ctx, clip, 0, filepath.Join(dir, "first.png")) {
		t.Error("ExtractFrame = false, want true")
	}
	if !ExtractLastFrame(ctx, clip, filepath.Join(dir, "last.png")) {
		t.Error("ExtractLastFrame = false, want true")
	}
}

func TestRealRenderAndConcatDemuxer(t *testing.T) {
	requireRealTools(t)
	ctx := context.Background()
	dir := t.TempDir()
	first := filepath.Join(dir, "a.mp4")
	second := filepath.Join(dir, "b.mp4")
	makeRealClip(t, first, 1)
	makeRealClip(t, second, 1)

	output := filepath.Join(dir, "out.mp4")
	segments := []Segment{
		{Path: first, DurationSeconds: 1, TransitionType: "fade", TransitionDurationSeconds: 0.25},
		{Path: second},
	}
	info, err := RenderVideo(ctx, segments, output, RenderOptions{Width: 320, Height: 240, FPS: 24, Preset: "ultrafast"})
	if err != nil {
		t.Fatalf("RenderVideo: %v", err)
	}
	if info.Width != 320 || info.Height != 240 {
		t.Errorf("info = %+v, want the 320x240 canvas", info)
	}

	joined := filepath.Join(dir, "joined.mp4")
	if err := ConcatDemuxer(ctx, []string{output, output}, joined, 120*time.Second); err != nil {
		t.Fatalf("ConcatDemuxer: %v", err)
	}
	if !nonEmptyFile(joined) {
		t.Error("concat output is empty")
	}
}
