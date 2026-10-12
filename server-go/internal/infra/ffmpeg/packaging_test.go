package ffmpeg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildPackagingArgsCoverScalesAndDropsAudio(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "cover.png")
	writeFile(t, source, "image")
	output := filepath.Join(dir, "cover.mp4")

	args, err := BuildPackagingArgs("ffmpeg", PackagingClip{
		Kind:            PackagingCover,
		SourcePath:      source,
		DurationSeconds: 2,
		OutputPath:      output,
	}, 480, 854, 30)
	if err != nil {
		t.Fatalf("BuildPackagingArgs: %v", err)
	}
	for _, pair := range [][2]string{
		{"-protocol_whitelist", "file"},
		{"-loop", "1"},
		{"-i", source},
		{"-t", "2"},
		{"-r", "30"},
		{"-c:v", "libx264"},
		{"-pix_fmt", "yuv420p"},
	} {
		if !containsPair(args, pair[0], pair[1]) {
			t.Errorf("args = %v, want %s %s", args, pair[0], pair[1])
		}
	}
	if !containsArg(args, "-an") {
		t.Errorf("args = %v, want -an so the cover clip stays silent", args)
	}
	scale := argValue(args, "-vf")
	if !strings.Contains(scale, "scale=480:854:force_original_aspect_ratio=increase,crop=480:854") {
		t.Errorf("scale = %q, want the cover fit to crop the image to the canvas", scale)
	}
	if last := args[len(args)-1]; last != output {
		t.Errorf("args last = %q, want output path", last)
	}
}

func TestBuildPackagingArgsCoverContainFitKeepsBars(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "cover.png")
	writeFile(t, source, "image")

	args, err := BuildPackagingArgs("ffmpeg", PackagingClip{
		Kind:            PackagingCover,
		SourcePath:      source,
		DurationSeconds: 1.5,
		Fit:             "contain",
		OutputPath:      filepath.Join(dir, "cover.mp4"),
	}, 480, 854, 24)
	if err != nil {
		t.Fatalf("BuildPackagingArgs: %v", err)
	}
	scale := argValue(args, "-vf")
	if !strings.Contains(scale, "force_original_aspect_ratio=decrease,pad=480x854:(ow-iw)/2:(oh-ih)/2") {
		t.Errorf("scale = %q, want the contain fit to keep letterbox bars", scale)
	}
}

func TestBuildPackagingArgsTextUsesDrawtextFile(t *testing.T) {
	dir := t.TempDir()
	textFile := filepath.Join(dir, "intro.txt")
	writeFile(t, textFile, "标题")

	args, err := BuildPackagingArgs("ffmpeg", PackagingClip{
		Kind:            PackagingIntro,
		Text:            "标题",
		TextFile:        textFile,
		DurationSeconds: 3,
		OutputPath:      filepath.Join(dir, "intro.mp4"),
	}, 720, 1280, 30)
	if err != nil {
		t.Fatalf("BuildPackagingArgs: %v", err)
	}
	if !containsPair(args, "-f", "lavfi") {
		t.Errorf("args = %v, want a lavfi color source", args)
	}
	if source := argValue(args, "-i"); source != "color=c=black:s=720x1280:r=30:d=3" {
		t.Errorf("lavfi source = %q, want a black canvas sized to the render", source)
	}
	filter := argValue(args, "-vf")
	if !strings.Contains(filter, "drawtext=textfile='"+FilterPathEscape(textFile)+"'") {
		t.Errorf("drawtext = %q, want the escaped text file", filter)
	}
	if !strings.Contains(filter, "fontsize=40") {
		t.Errorf("drawtext = %q, want fontsize scaled to the canvas width", filter)
	}
	if !containsArg(args, "-an") {
		t.Errorf("args = %v, want -an", args)
	}
}

func TestBuildPackagingArgsRejectsInvalidInput(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		clip PackagingClip
		code string
	}{
		{
			"时长非正",
			PackagingClip{Kind: PackagingIntro, Text: "a", TextFile: "t.txt", OutputPath: filepath.Join(dir, "o.mp4")},
			CodePackagingDurationInvalid,
		},
		{
			"封面源缺失",
			PackagingClip{Kind: PackagingCover, SourcePath: filepath.Join(dir, "missing.png"),
				DurationSeconds: 1, OutputPath: filepath.Join(dir, "o.mp4")},
			CodePackagingSourceInvalid,
		},
		{
			"缺文字文件",
			PackagingClip{Kind: PackagingOutro, Text: "a", DurationSeconds: 1, OutputPath: filepath.Join(dir, "o.mp4")},
			CodePackagingTextInvalid,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := BuildPackagingArgs("ffmpeg", testCase.clip, 480, 854, 30)
			var toolErr *RenderToolError
			if !errors.As(err, &toolErr) || toolErr.Code != testCase.code {
				t.Fatalf("err = %v, want RenderToolError with code %s", err, testCase.code)
			}
		})
	}
}

func TestCreatePackagingClipWritesAndCleansTextFile(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	output := filepath.Join(dir, "intro.mp4")
	argvPath := filepath.Join(dir, "argv.txt")
	stubEnv(t, map[string]string{
		stubModeEnv:  stubModeMedia,
		stubWriteEnv: stubWriteLatest,
		stubArgvEnv:  argvPath,
	})

	err := CreatePackagingClip(context.Background(), PackagingClip{
		Kind: PackagingIntro, Text: "片头\n文字", DurationSeconds: 2, OutputPath: output,
	}, 480, 854, 30)
	if err != nil {
		t.Fatalf("CreatePackagingClip: %v", err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("output missing: %v", err)
	}
	filter := argValue(readStubArgv(t, argvPath), "-vf")
	if !strings.Contains(filter, "drawtext=textfile='") {
		t.Fatalf("drawtext = %q, want a textfile reference", filter)
	}
	// 文字文件就近生成、返回前清理，不留在产物目录里。
	textFile := filepath.Join(dir, ".intro-text.txt")
	if _, err := os.Stat(textFile); err == nil {
		t.Errorf("packaging text file %s survived the call", textFile)
	}
}

func TestCreatePackagingClipRejectsEmptyText(t *testing.T) {
	installStub(t)
	err := CreatePackagingClip(context.Background(), PackagingClip{
		Kind: PackagingOutro, Text: "  ", DurationSeconds: 2, OutputPath: filepath.Join(t.TempDir(), "outro.mp4"),
	}, 480, 854, 30)
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodePackagingTextInvalid {
		t.Fatalf("err = %v, want code %s", err, CodePackagingTextInvalid)
	}
}

func TestCreatePackagingClipReportsMissingOutput(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "cover.png")
	writeFile(t, source, "image")
	// 退出码为 0 但不写产物。
	stubEnv(t, map[string]string{stubModeEnv: stubModeMedia})

	err := CreatePackagingClip(context.Background(), PackagingClip{
		Kind: PackagingCover, SourcePath: source, DurationSeconds: 2, OutputPath: filepath.Join(dir, "cover.mp4"),
	}, 480, 854, 30)
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodePackagingOutputMissing {
		t.Fatalf("err = %v, want code %s", err, CodePackagingOutputMissing)
	}
}

func TestCreatePackagingClipRequiresFFmpeg(t *testing.T) {
	removeTools(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "cover.png")
	writeFile(t, source, "image")

	err := CreatePackagingClip(context.Background(), PackagingClip{
		Kind: PackagingCover, SourcePath: source, DurationSeconds: 2, OutputPath: filepath.Join(dir, "cover.mp4"),
	}, 480, 854, 30)
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeFFmpegUnavailable {
		t.Fatalf("err = %v, want code %s", err, CodeFFmpegUnavailable)
	}
}
