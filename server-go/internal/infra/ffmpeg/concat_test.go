package ffmpeg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConcatSegmentsNormalizesAndConcatenates(t *testing.T) {
	segments := []Segment{
		{Path: "a.mp4", DurationSeconds: 3},
		{Path: "b.mp4", DurationSeconds: 4},
	}
	probes := []SegmentProbe{{AudioPresent: true}, {AudioPresent: true}}

	graph, err := ConcatSegments(segments, probes, 480, 854, 30)
	if err != nil {
		t.Fatalf("ConcatSegments: %v", err)
	}
	want := "[0:v]scale=480:854:force_original_aspect_ratio=decrease," +
		"pad=480:854:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=30,format=yuv420p,setpts=PTS-STARTPTS[v0];" +
		"[0:a]aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo,asetpts=PTS-STARTPTS[a0];" +
		"[1:v]scale=480:854:force_original_aspect_ratio=decrease," +
		"pad=480:854:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=30,format=yuv420p,setpts=PTS-STARTPTS[v1];" +
		"[1:a]aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo,asetpts=PTS-STARTPTS[a1];" +
		"[v0][a0][v1][a1]concat=n=2:v=1:a=1[outv][outa]"
	if graph != want {
		t.Errorf("graph =\n%s\nwant\n%s", graph, want)
	}
}

func TestConcatSegmentsPadsSilentInputsWithAnullsrc(t *testing.T) {
	segments := []Segment{{Path: "silent.mp4", DurationSeconds: 1.5}}
	probes := []SegmentProbe{{AudioPresent: false, DurationSeconds: 4}}

	graph, err := ConcatSegments(segments, probes, 320, 240, 24)
	if err != nil {
		t.Fatalf("ConcatSegments: %v", err)
	}
	if !strings.Contains(graph, "anullsrc=channel_layout=stereo:sample_rate=48000:d=1.500000[a0]") {
		t.Errorf("graph = %s, want anullsrc silence for the silent segment", graph)
	}
}

func TestConcatSegmentsFallsBackToProbedLengthForSilentInputs(t *testing.T) {
	segments := []Segment{{Path: "silent.mp4", StartSeconds: 0.5}}
	probes := []SegmentProbe{{AudioPresent: false, DurationSeconds: 3.5}}

	graph, err := ConcatSegments(segments, probes, 320, 240, 24)
	if err != nil {
		t.Fatalf("ConcatSegments: %v", err)
	}
	if !strings.Contains(graph, "anullsrc=channel_layout=stereo:sample_rate=48000:d=3.000000[a0]") {
		t.Errorf("graph = %s, want silence as probed duration minus start", graph)
	}
}

func TestConcatSegmentsRejectsSilentInputWithUnknownLength(t *testing.T) {
	segments := []Segment{{Path: "silent.mp4"}}
	probes := []SegmentProbe{{AudioPresent: false}}

	_, err := ConcatSegments(segments, probes, 320, 240, 24)
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeSourceDurationInvalid {
		t.Fatalf("err = %v, want RenderToolError with code %s", err, CodeSourceDurationInvalid)
	}
	if !strings.Contains(toolErr.Message, "silent.mp4") {
		t.Errorf("message = %q, want clip name", toolErr.Message)
	}
}

func TestConcatSegmentsChainsXfadeAndAcrossfadeWithOffsets(t *testing.T) {
	segments := []Segment{
		{Path: "a.mp4", DurationSeconds: 4, TransitionType: "fade", TransitionDurationSeconds: 1},
		{Path: "b.mp4", DurationSeconds: 5, TransitionType: "wipe", TransitionDurationSeconds: 0.5},
		{Path: "c.mp4", DurationSeconds: 3},
	}
	probes := []SegmentProbe{{AudioPresent: true}, {AudioPresent: true}, {AudioPresent: true}}

	graph, err := ConcatSegments(segments, probes, 480, 854, 30)
	if err != nil {
		t.Fatalf("ConcatSegments: %v", err)
	}
	if !strings.Contains(graph, "xfade=transition=fade:duration=1.000000:offset=3.000000") {
		t.Errorf("graph = %s, want fade at offset 3", graph)
	}
	if !strings.Contains(graph, "xfade=transition=wipeleft:duration=0.500000:offset=7.500000") {
		t.Errorf("graph = %s, want wipe mapped to wipeleft at offset 7.5", graph)
	}
	if count := strings.Count(graph, "acrossfade=d="); count != 2 {
		t.Errorf("acrossfade count = %d, want 2", count)
	}
	if strings.Contains(graph, "concat=n=3") {
		t.Errorf("graph = %s, want a single xfade chain instead of concat", graph)
	}
	if !strings.Contains(graph, "[gv0_2]null[outv];[ga0_2]anull[outa]") {
		t.Errorf("graph = %s, want the merged group renamed to [outv]/[outa]", graph)
	}
}

func TestConcatSegmentsKeepsCutBoundariesBetweenXfadeGroups(t *testing.T) {
	segments := []Segment{
		{Path: "a.mp4", DurationSeconds: 3},
		{Path: "b.mp4", DurationSeconds: 2, TransitionType: "fade", TransitionDurationSeconds: 0.5},
		{Path: "c.mp4", DurationSeconds: 4},
	}
	probes := []SegmentProbe{{AudioPresent: true}, {AudioPresent: true}, {AudioPresent: true}}

	graph, err := ConcatSegments(segments, probes, 480, 854, 30)
	if err != nil {
		t.Fatalf("ConcatSegments: %v", err)
	}
	if !strings.Contains(graph, "xfade=transition=fade:duration=0.500000:offset=1.500000") {
		t.Errorf("graph = %s, want xfade offset 1.5 inside the second group", graph)
	}
	// 组标签取分组序号（第二组即 gv1_1），与 Python 参考实现的 build_concat_filter 一致。
	if !strings.Contains(graph, "[v0][a0][gv1_1][ga1_1]concat=n=2:v=1:a=1[outv][outa]") {
		t.Errorf("graph = %s, want concat of the cut group and the merged group", graph)
	}
}

func TestConcatSegmentsDegradesTransitionOnTooShortSegment(t *testing.T) {
	segments := []Segment{
		{Path: "a.mp4", DurationSeconds: 0.25, TransitionType: "fade", TransitionDurationSeconds: 0.5},
		{Path: "b.mp4", DurationSeconds: 3},
	}
	probes := []SegmentProbe{{AudioPresent: true}, {AudioPresent: true}}

	graph, err := ConcatSegments(segments, probes, 480, 854, 30)
	if err != nil {
		t.Fatalf("ConcatSegments: %v", err)
	}
	if strings.Contains(graph, "xfade") {
		t.Errorf("graph = %s, want the transition degraded to a hard cut", graph)
	}
	if !strings.Contains(graph, "[v0][a0][v1][a1]concat=n=2:v=1:a=1[outv][outa]") {
		t.Errorf("graph = %s, want a plain concat", graph)
	}
}

func TestConcatSegmentsDegradesUnknownTransitionTypes(t *testing.T) {
	segments := []Segment{
		{Path: "a.mp4", DurationSeconds: 4, TransitionType: "star-wipe", TransitionDurationSeconds: 1},
		{Path: "b.mp4", DurationSeconds: 3},
	}
	probes := []SegmentProbe{{AudioPresent: true}, {AudioPresent: true}}

	graph, err := ConcatSegments(segments, probes, 480, 854, 30)
	if err != nil {
		t.Fatalf("ConcatSegments: %v", err)
	}
	if strings.Contains(graph, "xfade") {
		t.Errorf("graph = %s, want unsupported transition type treated as a cut", graph)
	}
}

func TestConcatSegmentsClearsOverlapThatTheMiddleSegmentCannotCarry(t *testing.T) {
	segments := []Segment{
		{Path: "a.mp4", DurationSeconds: 2, TransitionType: "fade", TransitionDurationSeconds: 1},
		{Path: "b.mp4", DurationSeconds: 1.2, TransitionType: "fade", TransitionDurationSeconds: 1},
		{Path: "c.mp4", DurationSeconds: 2},
	}
	probes := []SegmentProbe{{AudioPresent: true}, {AudioPresent: true}, {AudioPresent: true}}

	graph, err := ConcatSegments(segments, probes, 480, 854, 30)
	if err != nil {
		t.Fatalf("ConcatSegments: %v", err)
	}
	// 中间段装不下两侧各 1s 重叠：靠左的边界退化为硬切，只剩一条 xfade。
	if count := strings.Count(graph, "xfade="); count != 1 {
		t.Errorf("xfade count = %d, want 1; graph = %s", count, graph)
	}
	if strings.Contains(graph, "offset=1.000000") {
		t.Errorf("graph = %s, want the left boundary cleared", graph)
	}
}

func TestConcatSegmentsRejectsEmptyAndMismatchedInputs(t *testing.T) {
	if _, err := ConcatSegments(nil, nil, 480, 854, 30); err == nil {
		t.Error("ConcatSegments(nil, nil) error = nil, want error for an empty timeline")
	}
	segments := []Segment{{Path: "a.mp4", DurationSeconds: 1}}
	if _, err := ConcatSegments(segments, nil, 480, 854, 30); err == nil {
		t.Error("ConcatSegments with missing probes error = nil, want error")
	}
}

func TestConcatSegmentsFormatsFractionalFrameRates(t *testing.T) {
	segments := []Segment{{Path: "a.mp4", DurationSeconds: 1}}
	probes := []SegmentProbe{{AudioPresent: true}}

	graph, err := ConcatSegments(segments, probes, 480, 854, 29.97)
	if err != nil {
		t.Fatalf("ConcatSegments: %v", err)
	}
	if !strings.Contains(graph, ",fps=29.97,") {
		t.Errorf("graph = %s, want fps=29.97", graph)
	}
}

func TestPlayLengthPrefersExplicitWindow(t *testing.T) {
	cases := []struct {
		name    string
		segment Segment
		probe   SegmentProbe
		want    float64
	}{
		{"显式窗口", Segment{DurationSeconds: 2}, SegmentProbe{DurationSeconds: 9}, 2},
		{"探测时长扣起播点", Segment{StartSeconds: 1}, SegmentProbe{DurationSeconds: 4}, 3},
		{"起播点超出探测时长", Segment{StartSeconds: 5}, SegmentProbe{DurationSeconds: 4}, 0},
		{"两者都未知", Segment{}, SegmentProbe{}, 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.segment.PlayLength(testCase.probe); got != testCase.want {
				t.Errorf("PlayLength = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestFilterPathEscapeEscapesFilterGraphSpecials(t *testing.T) {
	cases := map[string]string{
		"work/intro.txt":         "work/intro.txt",
		"work/intro's.txt":       `work/intro\'s.txt`,
		"work/intro-1:2.txt":     `work/intro-1\:2.txt`,
		"work/a b/intro's:1.txt": `work/a b/intro\'s\:1.txt`,
	}
	for input, want := range cases {
		if got := FilterPathEscape(input); got != want {
			t.Errorf("FilterPathEscape(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestFilterPathEscapeConvertsWindowsSeparatorsAndDriveColon(t *testing.T) {
	if filepath.Separator != '\\' {
		t.Skip("仅 Windows 上路径分隔符是反斜杠")
	}
	got := FilterPathEscape(`C:\work\intro's.txt`)
	want := `C\:/work/intro\'s.txt`
	if got != want {
		t.Errorf("FilterPathEscape = %q, want %q", got, want)
	}
}

func TestConcatListContentQuotesAbsolutePaths(t *testing.T) {
	list := ConcatListContent([]string{"a.mp4", "dir/it's clip.mp4"})
	lines := strings.Split(strings.TrimSuffix(list, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("list = %q, want one line per clip", list)
	}
	for index, line := range lines {
		if !strings.HasPrefix(line, "file '") || !strings.HasSuffix(line, "'") {
			t.Errorf("line %d = %q, want a quoted file directive", index, line)
		}
	}
	// 绝对路径：清单可在任意工作目录下被 ffmpeg 读取。
	first := strings.TrimSuffix(strings.TrimPrefix(lines[0], "file '"), "'")
	if !filepath.IsAbs(first) {
		t.Errorf("line 0 = %q, want an absolute path", lines[0])
	}
	if !strings.Contains(lines[1], `it'\''s clip.mp4`) {
		t.Errorf("line 1 = %q, want the single quote escaped as '\\''", lines[1])
	}
}

func TestConcatListContentKeepsBackslashesOffWindowsPaths(t *testing.T) {
	list := ConcatListContent([]string{`C:\work\clip.mp4`})
	if strings.Contains(list, `\work`) {
		t.Errorf("list = %q, want forward slashes so ffmpeg never sees a raw backslash path", list)
	}
}

func TestConcatDemuxerArgsGuardProtocols(t *testing.T) {
	args := ConcatDemuxerArgs("ffmpeg", "list.txt", "out.mp4")
	for _, pair := range [][2]string{
		{"-f", "concat"},
		{"-safe", "0"},
		{"-protocol_whitelist", "file"},
		{"-i", "list.txt"},
		{"-c", "copy"},
		{"-movflags", "+faststart"},
	} {
		if !containsPair(args, pair[0], pair[1]) {
			t.Errorf("args = %v, want %s %s", args, pair[0], pair[1])
		}
	}
	if last := args[len(args)-1]; last != "out.mp4" {
		t.Errorf("args last = %q, want output path", last)
	}
}

func TestConcatDemuxerRejectsEmptyList(t *testing.T) {
	err := ConcatDemuxer(context.Background(), nil, "out.mp4", 0)
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeSourceRefInvalid {
		t.Fatalf("err = %v, want RenderToolError with code %s", err, CodeSourceRefInvalid)
	}
}

func TestConcatDemuxerWritesListAndValidatesOutput(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	clips := []string{filepath.Join(dir, "a.mp4"), filepath.Join(dir, "b.mp4")}
	argvPath := filepath.Join(dir, "argv.txt")
	stubEnv(t, map[string]string{
		stubModeEnv:  stubModeMedia,
		stubWriteEnv: stubWriteLatest,
		stubArgvEnv:  argvPath,
	})

	output := filepath.Join(dir, "out.mp4")
	if err := ConcatDemuxer(context.Background(), clips, output, 0); err != nil {
		t.Fatalf("ConcatDemuxer: %v", err)
	}
	argv := readStubArgv(t, argvPath)
	listPath := argValue(argv, "-i")
	if !strings.Contains(listPath, "concat-list.txt") {
		t.Errorf("argv = %v, want a concat list input", argv)
	}
	// 清单落在临时目录，调用结束即清理。
	if _, err := os.Stat(listPath); err == nil {
		t.Errorf("concat list %s survived the call", listPath)
	}
	if _, err := os.Stat(output); err != nil {
		t.Errorf("output missing: %v", err)
	}
}

func TestConcatDemuxerReportsMissingOutput(t *testing.T) {
	installStub(t)
	dir := t.TempDir()
	stubEnv(t, map[string]string{stubModeEnv: stubModeMedia})

	err := ConcatDemuxer(context.Background(), []string{filepath.Join(dir, "a.mp4")}, filepath.Join(dir, "out.mp4"), 0)
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeRenderOutputMissing {
		t.Fatalf("err = %v, want RenderToolError with code %s", err, CodeRenderOutputMissing)
	}
}

func TestConcatDemuxerRequiresFFmpeg(t *testing.T) {
	removeTools(t)
	err := ConcatDemuxer(context.Background(), []string{"a.mp4"}, "out.mp4", 0)
	var toolErr *RenderToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeFFmpegUnavailable {
		t.Fatalf("err = %v, want RenderToolError with code %s", err, CodeFFmpegUnavailable)
	}
}
