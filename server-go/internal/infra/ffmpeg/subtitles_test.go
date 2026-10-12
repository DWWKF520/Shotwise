package ffmpeg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSubtitleBurnInArgsShape(t *testing.T) {
	args := SubtitleBurnInArgs("ffmpeg", "in.mp4", "subs/it's.srt", "out.mp4")
	for _, pair := range [][2]string{
		{"-protocol_whitelist", "file"},
		{"-i", "in.mp4"},
		{"-map", "0:v:0"},
		{"-map", "0:a:0?"},
		{"-c:v", "libx264"},
		{"-preset", "veryfast"},
		{"-crf", "30"},
		{"-pix_fmt", "yuv420p"},
		{"-c:a", "copy"},
		{"-movflags", "+faststart"},
	} {
		if !containsPair(args, pair[0], pair[1]) {
			t.Errorf("args = %v, want %s %s", args, pair[0], pair[1])
		}
	}
	// 字幕路径按滤镜图口径转义：单引号不能让 filter 参数提前闭合。
	filter := argValue(args, "-vf")
	want := `subtitles=filename='subs/it\'s.srt'`
	if filter != want {
		t.Errorf("filter = %q, want %q", filter, want)
	}
	if last := args[len(args)-1]; last != "out.mp4" {
		t.Errorf("args last = %q, want output path", last)
	}
}

func TestBurnInSubtitlesRunsAndValidatesOutput(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	video := filepath.Join(dir, "in.mp4")
	subtitle := filepath.Join(dir, "subs.srt")
	writeFile(t, video, "video")
	writeFile(t, subtitle, "1\n00:00:00,000 --> 00:00:01,000\nhi\n")
	output := filepath.Join(dir, "out.mp4")
	argvPath := filepath.Join(dir, "argv.txt")
	stubEnv(t, map[string]string{
		stubModeEnv:  stubModeMedia,
		stubWriteEnv: stubWriteLatest,
		stubArgvEnv:  argvPath,
	})

	if err := BurnInSubtitles(context.Background(), video, subtitle, output); err != nil {
		t.Fatalf("BurnInSubtitles: %v", err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("output missing: %v", err)
	}
	argv := readStubArgv(t, argvPath)
	if !containsPair(argv, "-protocol_whitelist", "file") {
		t.Errorf("argv = %v, want -protocol_whitelist file", argv)
	}
	if filter := argValue(argv, "-vf"); filter != "subtitles=filename='"+FilterPathEscape(subtitle)+"'" {
		t.Errorf("filter = %q, want the escaped subtitle path", filter)
	}
}

func TestBurnInSubtitlesReportsMissingOutput(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	// 退出码为 0 但不写产物：产物校验必须发现并报错。
	stubEnv(t, map[string]string{stubModeEnv: stubModeMedia})

	err := BurnInSubtitles(context.Background(),
		filepath.Join(dir, "in.mp4"), filepath.Join(dir, "subs.srt"), filepath.Join(dir, "out.mp4"))
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeSubtitleOutputMissing {
		t.Fatalf("err = %v, want code %s", err, CodeSubtitleOutputMissing)
	}
}

func TestBurnInSubtitlesRequiresFFmpeg(t *testing.T) {
	removeTools(t)
	err := BurnInSubtitles(context.Background(), "in.mp4", "subs.srt", "out.mp4")
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeFFmpegUnavailable {
		t.Fatalf("err = %v, want code %s", err, CodeFFmpegUnavailable)
	}
}

func TestBurnInSubtitlesPassesRunOptions(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	stubEnv(t, map[string]string{
		stubModeEnv:    stubModeMedia,
		stubWriteEnv:   stubWriteLatest,
		stubSleepMsEnv: "700",
	})

	cancels := 0
	err := BurnInSubtitles(context.Background(), "in.mp4", "subs.srt", filepath.Join(dir, "out.mp4"),
		WithCancelCheck(func() bool { cancels++; return false }))
	if err != nil {
		t.Fatalf("BurnInSubtitles: %v", err)
	}
	if cancels != 1 {
		t.Errorf("cancel checks = %d, want 1", cancels)
	}
}
