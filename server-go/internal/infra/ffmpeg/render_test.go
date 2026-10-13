package ffmpeg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func renderSegments(t *testing.T, dir string) []Segment {
	t.Helper()
	first := filepath.Join(dir, "a.mp4")
	second := filepath.Join(dir, "b.mp4")
	writeFile(t, first, "clip-a")
	writeFile(t, second, "clip-b")
	return []Segment{
		{Path: first, StartSeconds: 0.5, DurationSeconds: 3, TransitionType: "fade", TransitionDurationSeconds: 0.5},
		{Path: second},
	}
}

func TestRenderVideoValidatesOptionsBeforeTouchingTools(t *testing.T) {
	removeTools(t)
	ctx := context.Background()

	_, err := RenderVideo(ctx, nil, "out.mp4", RenderOptions{Width: 0, Height: 854})
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeRenderDimensionsInvalid {
		t.Fatalf("err = %v, want code %s", err, CodeRenderDimensionsInvalid)
	}

	_, err = RenderVideo(ctx, nil, "out.mp4", RenderOptions{Width: 480, Height: 854, CRF: 99})
	if !errors.As(err, &toolErr) || toolErr.Code != CodeRenderQualityInvalid {
		t.Fatalf("err = %v, want code %s", err, CodeRenderQualityInvalid)
	}
}

func TestRenderVideoRequiresFFmpeg(t *testing.T) {
	removeTools(t)
	_, err := RenderVideo(context.Background(), nil, "out.mp4", RenderOptions{Width: 480, Height: 854})
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeFFmpegUnavailable {
		t.Fatalf("err = %v, want code %s", err, CodeFFmpegUnavailable)
	}
}

func TestRenderVideoRequiresFFprobe(t *testing.T) {
	// 只把 ffmpeg 桩放进 PATH：素材探测缺 ffprobe 时必须报错而不是降级跳过。
	shared := stubBinDir(t)
	onlyFFmpeg := t.TempDir()
	if err := copyExecutable(filepath.Join(shared, stubBinaryName("ffmpeg")),
		filepath.Join(onlyFFmpeg, stubBinaryName("ffmpeg"))); err != nil {
		t.Fatalf("copy ffmpeg stub: %v", err)
	}
	t.Cleanup(resetAvailabilityForTests)
	t.Setenv("PATH", onlyFFmpeg)
	resetAvailabilityForTests()

	segments := []Segment{{Path: "a.mp4", DurationSeconds: 1}}
	_, err := RenderVideo(context.Background(), segments, filepath.Join(t.TempDir(), "out.mp4"),
		RenderOptions{Width: 480, Height: 854})
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeFFprobeUnavailable {
		t.Fatalf("err = %v, want code %s", err, CodeFFprobeUnavailable)
	}
}

func TestRenderVideoRendersAndValidatesOutput(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	segments := renderSegments(t, dir)
	outputPath := filepath.Join(dir, "nested", "out.mp4")
	argvPath := filepath.Join(dir, "argv.txt")
	stubEnv(t, map[string]string{
		stubModeEnv:  stubModeMedia,
		stubWriteEnv: stubWriteLatest,
		stubArgvEnv:  argvPath,
	})

	info, err := RenderVideo(context.Background(), segments, outputPath, RenderOptions{
		Width: 480, Height: 854, FPS: 30, CRF: 30, Preset: "veryfast", AudioBitrate: "96k",
	})
	if err != nil {
		t.Fatalf("RenderVideo: %v", err)
	}
	want := MediaInfo{DurationSeconds: 3, Width: 480, Height: 854, AudioPresent: true}
	if info != want {
		t.Errorf("info = %+v, want %+v", info, want)
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("output missing: %v", err)
	}

	argv := readStubArgv(t, argvPath)
	if last := argv[len(argv)-1]; last != outputPath {
		t.Errorf("argv last = %q, want output path", last)
	}
	// 输入窗口选项须紧邻对应 -i 之前，白名单插在输入选项与 -i 之间。
	wantPrefix := []string{
		"-y",
		"-ss", "0.500000", "-t", "3.000000", "-protocol_whitelist", "file", "-i", segments[0].Path,
		"-protocol_whitelist", "file", "-i", segments[1].Path,
	}
	if strings.Join(argv[:len(wantPrefix)], "\x00") != strings.Join(wantPrefix, "\x00") {
		t.Errorf("argv prefix = %v, want %v", argv[:len(wantPrefix)], wantPrefix)
	}
	for _, pair := range [][2]string{
		{"-map", "[outv]"},
		{"-map", "[outa]"},
		{"-c:v", "libx264"},
		{"-preset", "veryfast"},
		{"-crf", "30"},
		{"-pix_fmt", "yuv420p"},
		{"-c:a", "aac"},
		{"-b:a", "96k"},
		{"-movflags", "+faststart"},
	} {
		if !containsPair(argv, pair[0], pair[1]) {
			t.Errorf("argv = %v, want %s %s", argv, pair[0], pair[1])
		}
	}
	graph := argValue(argv, "-filter_complex")
	if !strings.Contains(graph, "xfade=transition=fade:duration=0.500000:offset=2.500000") {
		t.Errorf("filter graph = %s, want fade merging the two clips", graph)
	}
	// 整条时间线只有一组可渲染转场：该组直接改名到 [outv]/[outa]，不走组间 concat。
	if !strings.Contains(graph, "[gv0_1]null[outv];[ga0_1]anull[outa]") {
		t.Errorf("filter graph = %s, want the merged group mapped to [outv]/[outa]", graph)
	}
}

func TestRenderVideoDefaultsToMediumPresetAndStandardCRF(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	segments := []Segment{{Path: filepath.Join(dir, "a.mp4"), DurationSeconds: 2}}
	writeFile(t, segments[0].Path, "clip")
	argvPath := filepath.Join(dir, "argv.txt")
	stubEnv(t, map[string]string{
		stubModeEnv:  stubModeMedia,
		stubWriteEnv: stubWriteLatest,
		stubArgvEnv:  argvPath,
	})

	if _, err := RenderVideo(context.Background(), segments, filepath.Join(dir, "out.mp4"),
		RenderOptions{Width: 480, Height: 854}); err != nil {
		t.Fatalf("RenderVideo: %v", err)
	}
	argv := readStubArgv(t, argvPath)
	if !containsPair(argv, "-preset", "medium") || !containsPair(argv, "-crf", "18") {
		t.Errorf("argv = %v, want medium preset with CRF 18", argv)
	}
	if !containsPair(argv, "-b:a", "192k") {
		t.Errorf("argv = %v, want 192k audio bitrate", argv)
	}
}

func TestRenderVideoReportsProgressAndCancelOptions(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	segments := []Segment{{Path: filepath.Join(dir, "a.mp4"), DurationSeconds: 2}}
	writeFile(t, segments[0].Path, "clip")
	stubEnv(t, map[string]string{
		stubModeEnv:    stubModeMedia,
		stubWriteEnv:   stubWriteLatest,
		stubStderrEnv:  "time=00:00:01.00\n",
		stubSleepMsEnv: "700",
	})

	fractions := 0
	cancels := 0
	if _, err := RenderVideo(context.Background(), segments, filepath.Join(dir, "out.mp4"), RenderOptions{
		Width: 480, Height: 854,
		OnProgress:  func(float64) { fractions++ },
		CancelCheck: func() bool { cancels++; return false },
	}); err != nil {
		t.Fatalf("RenderVideo: %v", err)
	}
	// 进度分母是各段播放长度合计（2s），1s 处应上报一次 0.5。
	if fractions != 1 {
		t.Errorf("progress callbacks = %d, want 1", fractions)
	}
	if cancels != 1 {
		t.Errorf("cancel checks = %d, want 1", cancels)
	}
}

func TestRenderVideoReportsMissingOutput(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	segments := []Segment{{Path: filepath.Join(dir, "a.mp4"), DurationSeconds: 2}}
	writeFile(t, segments[0].Path, "clip")
	// 渲染调用退出码为 0 但不写产物：后置校验必须发现并报错。
	stubEnv(t, map[string]string{stubModeEnv: stubModeMedia})

	_, err := RenderVideo(context.Background(), segments, filepath.Join(dir, "out.mp4"),
		RenderOptions{Width: 480, Height: 854})
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeRenderOutputMissing {
		t.Fatalf("err = %v, want code %s", err, CodeRenderOutputMissing)
	}
}

func TestRenderVideoReportsInvalidProbe(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	segments := []Segment{{Path: filepath.Join(dir, "a.mp4"), DurationSeconds: 2}}
	writeFile(t, segments[0].Path, "clip")
	stubEnv(t, map[string]string{
		stubModeEnv:        stubModeMedia,
		stubWriteEnv:       stubWriteLatest,
		stubOutputProbeEnv: `{"format":{"duration":"0"},"streams":[{"codec_type":"video","width":480,"height":854}]}`,
	})

	_, err := RenderVideo(context.Background(), segments, filepath.Join(dir, "out.mp4"),
		RenderOptions{Width: 480, Height: 854})
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeRenderProbeInvalid {
		t.Fatalf("err = %v, want code %s", err, CodeRenderProbeInvalid)
	}
}

func TestRenderVideoReportsClipProbeFailure(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	segments := []Segment{{Path: filepath.Join(dir, "a.mp4"), DurationSeconds: 2}}
	writeFile(t, segments[0].Path, "clip")
	stubEnv(t, map[string]string{stubModeEnv: stubModeMedia, stubClipProbeEnv: "not json"})

	_, err := RenderVideo(context.Background(), segments, filepath.Join(dir, "out.mp4"),
		RenderOptions{Width: 480, Height: 854})
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeSourceProbeInvalid {
		t.Fatalf("err = %v, want code %s", err, CodeSourceProbeInvalid)
	}
}

func TestRenderVideoReportsProcessFailure(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	segments := []Segment{{Path: filepath.Join(dir, "a.mp4"), DurationSeconds: 2}}
	writeFile(t, segments[0].Path, "clip")
	stubEnv(t, map[string]string{stubModeEnv: stubModeMedia, stubExitEnv: "3"})

	_, err := RenderVideo(context.Background(), segments, filepath.Join(dir, "out.mp4"),
		RenderOptions{Width: 480, Height: 854})
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeProcessFailed {
		t.Fatalf("err = %v, want code %s", err, CodeProcessFailed)
	}
}

func TestRenderPreviewUsesPreviewProfile(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	segments := []Segment{{Path: filepath.Join(dir, "a.mp4"), DurationSeconds: 2}}
	writeFile(t, segments[0].Path, "clip")
	argvPath := filepath.Join(dir, "argv.txt")
	stubEnv(t, map[string]string{
		stubModeEnv:  stubModeMedia,
		stubWriteEnv: stubWriteLatest,
		stubArgvEnv:  argvPath,
	})

	info, err := RenderPreview(context.Background(), segments, filepath.Join(dir, "preview.mp4"), PreviewOptions{})
	if err != nil {
		t.Fatalf("RenderPreview: %v", err)
	}
	if info.Width != 480 || info.Height != 854 {
		t.Errorf("info = %+v, want the 480x854 preview canvas", info)
	}
	argv := readStubArgv(t, argvPath)
	graph := argValue(argv, "-filter_complex")
	if !strings.Contains(graph, "scale=480:854:") {
		t.Errorf("filter graph = %s, want the preview canvas", graph)
	}
	for _, pair := range [][2]string{
		{"-preset", "veryfast"},
		{"-crf", "30"},
		{"-b:a", "96k"},
	} {
		if !containsPair(argv, pair[0], pair[1]) {
			t.Errorf("argv = %v, want %s %s", argv, pair[0], pair[1])
		}
	}
}

func TestRenderPreviewReportsPreviewProbeCode(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	segments := []Segment{{Path: filepath.Join(dir, "a.mp4"), DurationSeconds: 2}}
	writeFile(t, segments[0].Path, "clip")
	stubEnv(t, map[string]string{
		stubModeEnv:        stubModeMedia,
		stubWriteEnv:       stubWriteLatest,
		stubOutputProbeEnv: "not json",
	})

	_, err := RenderPreview(context.Background(), segments, filepath.Join(dir, "preview.mp4"), PreviewOptions{})
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodePreviewProbeInvalid {
		t.Fatalf("err = %v, want code %s", err, CodePreviewProbeInvalid)
	}
}
