package ffmpeg

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFrameHelpersSkipWhenToolsMissing(t *testing.T) {
	removeTools(t)
	ctx := context.Background()
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	writeFile(t, video, "video")

	if MakeThumbnail(ctx, video, filepath.Join(dir, "thumb.jpg")) {
		t.Error("MakeThumbnail = true, want false without ffmpeg")
	}
	if ExtractFrame(ctx, video, 0, filepath.Join(dir, "frame.png")) {
		t.Error("ExtractFrame = true, want false without ffmpeg")
	}
	if ExtractLastFrame(ctx, video, filepath.Join(dir, "last.png")) {
		t.Error("ExtractLastFrame = true, want false without ffmpeg/ffprobe")
	}
}

func TestMakeThumbnailSkipsMissingVideoWithoutRunningTool(t *testing.T) {
	installStub(t)
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	stubEnv(t, map[string]string{stubArgvEnv: argvPath})

	out := filepath.Join(t.TempDir(), "thumb.jpg")
	if MakeThumbnail(context.Background(), filepath.Join(t.TempDir(), "missing.mp4"), out) {
		t.Error("MakeThumbnail = true, want false for missing video")
	}
	if _, err := os.Stat(argvPath); err == nil {
		t.Error("ffmpeg was invoked for a missing video")
	}
}

func TestMakeThumbnailWritesFirstFrame(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	writeFile(t, video, "video")
	out := filepath.Join(dir, "nested", "thumb.jpg")
	argvPath := filepath.Join(dir, "argv.txt")
	stubEnv(t, map[string]string{stubWriteEnv: stubWriteLatest, stubArgvEnv: argvPath})

	if !MakeThumbnail(context.Background(), video, out) {
		t.Fatal("MakeThumbnail = false, want true")
	}
	content, err := os.ReadFile(out)
	if err != nil || string(content) != stubBytes {
		t.Fatalf("thumbnail = %q, %v; want stub output", content, err)
	}
	argv := readStubArgv(t, argvPath)
	if argv[0] != "-protocol_whitelist" || argv[1] != "file" || argv[2] != "-i" || argv[3] != video {
		t.Errorf("argv = %v, want -protocol_whitelist file before -i", argv)
	}
	if !containsPair(argv, "-vframes", "1") || !containsPair(argv, "-q:v", "2") {
		t.Errorf("argv = %v, want first-frame JPEG settings", argv)
	}
}

func TestMakeThumbnailHonoursQualityOption(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	writeFile(t, video, "video")
	argvPath := filepath.Join(dir, "argv.txt")
	stubEnv(t, map[string]string{stubWriteEnv: stubWriteLatest, stubArgvEnv: argvPath})

	if !MakeThumbnail(context.Background(), video, filepath.Join(dir, "thumb.jpg"), WithThumbnailQuality(9)) {
		t.Fatal("MakeThumbnail = false, want true")
	}
	argv := readStubArgv(t, argvPath)
	if !containsPair(argv, "-q:v", "9") {
		t.Errorf("argv = %v, want -q:v 9", argv)
	}

	// 非正质量回落到默认 2。
	if !MakeThumbnail(context.Background(), video, filepath.Join(dir, "thumb2.jpg"), WithThumbnailQuality(0)) {
		t.Fatal("MakeThumbnail = false, want true")
	}
	argv = readStubArgv(t, argvPath)
	if !containsPair(argv, "-q:v", "2") {
		t.Errorf("argv = %v, want -q:v 2 for a non-positive quality", argv)
	}
}

func TestMakeThumbnailFailsOnNonZeroExit(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	writeFile(t, video, "video")
	out := filepath.Join(dir, "thumb.jpg")
	stubEnv(t, map[string]string{stubWriteEnv: stubWriteLatest, stubExitEnv: "1"})

	if MakeThumbnail(context.Background(), video, out) {
		t.Error("MakeThumbnail = true, want false on non-zero exit")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("thumbnail exists although ffmpeg failed")
	}
}

func TestExtractFrameWritesTempThenReplacesTarget(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	writeFile(t, video, "video")
	out := filepath.Join(dir, "frame.png")
	argvPath := filepath.Join(dir, "argv.txt")
	stubEnv(t, map[string]string{stubWriteEnv: stubWriteLatest, stubArgvEnv: argvPath})

	if !ExtractFrame(context.Background(), video, 7, out) {
		t.Fatal("ExtractFrame = false, want true")
	}
	if content, err := os.ReadFile(out); err != nil || string(content) != stubBytes {
		t.Fatalf("frame = %q, %v; want stub output", content, err)
	}
	tempPath := filepath.Join(dir, ".frame.tmp.png")
	if _, err := os.Stat(tempPath); err == nil {
		t.Error("temporary frame file was not replaced by the target")
	}
	argv := readStubArgv(t, argvPath)
	if last := argv[len(argv)-1]; last != tempPath {
		t.Errorf("argv last = %q, want temp path %q", last, tempPath)
	}
	if !containsPair(argv, "-vf", `select='eq(n\,7)'`) {
		t.Errorf("argv = %v, want frame selection filter", argv)
	}
	if !containsPair(argv, "-fps_mode", "vfr") || !containsPair(argv, "-frames:v", "1") {
		t.Errorf("argv = %v, want vfr single-frame extraction", argv)
	}
	if !containsPair(argv, "-protocol_whitelist", "file") {
		t.Errorf("argv = %v, want -protocol_whitelist file", argv)
	}
}

func TestExtractFrameCleansTempWithoutOutput(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	writeFile(t, video, "video")
	out := filepath.Join(dir, "frame.png")
	tempPath := filepath.Join(dir, ".frame.tmp.png")

	// 退出码为 0 但没写出任何产物，同样按失败处理。
	if ExtractFrame(context.Background(), video, 1, out) {
		t.Error("ExtractFrame = true, want false when ffmpeg produced nothing")
	}
	for _, path := range []string{out, tempPath} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s exists, want cleanup", path)
		}
	}
}

func TestExtractFrameRemovesStaleTempBeforeRunning(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	writeFile(t, video, "video")
	out := filepath.Join(dir, "frame.png")
	writeFile(t, filepath.Join(dir, ".frame.tmp.png"), "stale")
	stubEnv(t, map[string]string{stubExitEnv: "1"})

	if ExtractFrame(context.Background(), video, 3, out) {
		t.Fatal("ExtractFrame = true, want false on non-zero exit")
	}
	if _, err := os.Stat(filepath.Join(dir, ".frame.tmp.png")); err == nil {
		t.Error("stale temp file survived the failed extraction")
	}
}

func TestExtractLastFrameUsesMetadataFastPath(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	writeFile(t, video, "video")
	out := filepath.Join(dir, "last.png")
	argvPath := filepath.Join(dir, "argv.txt")
	stubEnv(t, map[string]string{
		stubModeEnv:   stubModeProbe,
		stubFramesEnv: "2",
		stubWriteEnv:  stubWriteLatest,
		stubArgvEnv:   argvPath,
	})

	if !ExtractLastFrame(context.Background(), video, out) {
		t.Fatal("ExtractLastFrame = false, want true")
	}
	if content, err := os.ReadFile(out); err != nil || string(content) != stubBytes {
		t.Fatalf("last frame = %q, %v; want stub output", content, err)
	}
	argv := readStubArgv(t, argvPath)
	if !containsPair(argv, "-vf", `select='eq(n\,1)'`) {
		t.Errorf("argv = %v, want the last frame index of a 2-frame video", argv)
	}
}

func TestExtractLastFrameFallsBackToFullDecode(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	writeFile(t, video, "video")
	out := filepath.Join(dir, "last.png")
	argvPath := filepath.Join(dir, "argv.txt")
	stubEnv(t, map[string]string{
		stubModeEnv:        stubModeProbe,
		stubFramesEnv:      "N/A",
		stubExactFramesEnv: "5",
		stubWriteEnv:       stubWriteLatest,
		stubArgvEnv:        argvPath,
	})

	if !ExtractLastFrame(context.Background(), video, out) {
		t.Fatal("ExtractLastFrame = false, want true")
	}
	argv := readStubArgv(t, argvPath)
	if !containsPair(argv, "-vf", `select='eq(n\,4)'`) {
		t.Errorf("argv = %v, want the last frame index of the counted video", argv)
	}
}

func TestExtractLastFrameFailsWhenFrameCountUnavailable(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	writeFile(t, video, "video")
	stubEnv(t, map[string]string{
		stubModeEnv:        stubModeProbe,
		stubFramesEnv:      "N/A",
		stubExactFramesEnv: "N/A",
	})

	if ExtractLastFrame(context.Background(), video, filepath.Join(dir, "last.png")) {
		t.Error("ExtractLastFrame = true, want false when the frame count is unknown")
	}
}
