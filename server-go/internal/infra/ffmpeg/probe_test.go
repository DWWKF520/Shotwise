package ffmpeg

import (
	"context"
	"path/filepath"
	"testing"
)

func TestProbesSkipWhenToolsMissing(t *testing.T) {
	removeTools(t)
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "clip.mp4")

	if _, ok := ContainerFrameCount(ctx, missing); ok {
		t.Error("ContainerFrameCount ok = true, want false without ffprobe")
	}
	if _, ok := CountFrames(ctx, missing); ok {
		t.Error("CountFrames ok = true, want false without ffprobe")
	}
	if _, ok := DurationSeconds(ctx, missing); ok {
		t.Error("DurationSeconds ok = true, want false without ffprobe")
	}
	if _, ok := AudioDuration(ctx, missing); ok {
		t.Error("AudioDuration ok = true, want false without ffprobe")
	}
	if _, ok := MultiTotalDuration(ctx, []string{missing, missing}); ok {
		t.Error("MultiTotalDuration ok = true, want false without ffprobe")
	}
	if _, _, ok := ProbeSize(ctx, missing); ok {
		t.Error("ProbeSize ok = true, want false without ffprobe")
	}
}

func TestFrameCountProbesUseDistinctTiers(t *testing.T) {
	cases := []struct {
		name       string
		probe      func(context.Context, string) (int, bool)
		wantEntry  string
		wantArgues bool
	}{
		{"元数据快路径", ContainerFrameCount, "stream=nb_frames", false},
		{"全量解码", CountFrames, "stream=nb_read_frames", true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			installStub(t)
			argvPath := filepath.Join(t.TempDir(), "argv.txt")
			stubEnv(t, map[string]string{stubStdoutEnv: "42", stubArgvEnv: argvPath})

			frames, ok := testCase.probe(context.Background(), "clip.mp4")
			if !ok || frames != 42 {
				t.Fatalf("frame count = %d, %v; want 42, true", frames, ok)
			}
			argv := readStubArgv(t, argvPath)
			if containsArg(argv, "-count_frames") != testCase.wantArgues {
				t.Errorf("argv = %v, want -count_frames = %v", argv, testCase.wantArgues)
			}
			if !containsArg(argv, testCase.wantEntry) {
				t.Errorf("argv = %v, want %s entry", argv, testCase.wantEntry)
			}
			if !containsPair(argv, "-protocol_whitelist", "file") {
				t.Errorf("argv = %v, want -protocol_whitelist file", argv)
			}
			if last := argv[len(argv)-1]; last != "clip.mp4" {
				t.Errorf("last argv = %q, want input path", last)
			}
		})
	}
}

func TestFrameCountRejectsUnparsableOutput(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{stubStdoutEnv: "N/A\n"})

	if frames, ok := ContainerFrameCount(context.Background(), "clip.mp4"); ok {
		t.Errorf("ContainerFrameCount = %d, true; want ok = false for N/A", frames)
	}
	if frames, ok := CountFrames(context.Background(), "clip.mp4"); ok {
		t.Errorf("CountFrames = %d, true; want ok = false for N/A", frames)
	}
}

func TestDurationSecondsReadsContainerDuration(t *testing.T) {
	installStub(t)
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	stubEnv(t, map[string]string{stubStdoutEnv: "12.500000\n", stubArgvEnv: argvPath})

	duration, ok := DurationSeconds(context.Background(), "audio.mp3")
	if !ok || duration != 12.5 {
		t.Fatalf("DurationSeconds = %v, %v; want 12.5, true", duration, ok)
	}
	argv := readStubArgv(t, argvPath)
	if !containsPair(argv, "-protocol_whitelist", "file") {
		t.Errorf("argv = %v, want -protocol_whitelist file", argv)
	}
	if !containsArg(argv, "format=duration") {
		t.Errorf("argv = %v, want format=duration entry", argv)
	}
}

func TestDurationSecondsRejectsUnparsableOutput(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{stubStdoutEnv: "not-a-number"})

	if _, ok := DurationSeconds(context.Background(), "audio.mp3"); ok {
		t.Error("DurationSeconds ok = true, want false for unparsable output")
	}
}

func TestAudioDurationConfirmsAudioStreamThenReadsDuration(t *testing.T) {
	installStub(t)
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	stubEnv(t, map[string]string{stubModeEnv: stubModeProbe, stubArgvEnv: argvPath})

	duration, ok := AudioDuration(context.Background(), "ref.mp3")
	if !ok || duration != 3.5 {
		t.Fatalf("AudioDuration = %v, %v; want 3.5, true", duration, ok)
	}
	if argv := readStubArgv(t, argvPath); !containsPair(argv, "-select_streams", "a") {
		t.Errorf("last probe argv = %v, want -select_streams a", argv)
	}
}

func TestAudioDurationRejectsFileWithoutAudioStream(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{stubModeEnv: stubModeProbe, stubStreamsEnv: "video"})

	if _, ok := AudioDuration(context.Background(), "clip.wav"); ok {
		t.Error("AudioDuration ok = true, want false when the container has no audio stream")
	}
}

func TestMultiTotalDurationSumsSegments(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{stubModeEnv: stubModeProbe})

	total, ok := MultiTotalDuration(context.Background(), []string{"a.mp3", "b.wav", "c.m4a"})
	if !ok || total != 10.5 {
		t.Fatalf("total = %v, %v; want 10.5, true", total, ok)
	}
}

func TestMultiTotalDurationRejectsPartialSum(t *testing.T) {
	installStub(t)
	// 时长不可解析：任一文件失败都不得返回部分和。
	stubEnv(t, map[string]string{stubModeEnv: stubModeProbe, stubDurationEnv: "bogus"})

	if total, ok := MultiTotalDuration(context.Background(), []string{"a.mp3", "b.wav"}); ok {
		t.Errorf("total = %v, true; want ok = false when any segment is unprobeable", total)
	}
}

func TestProbeSizeReadsFirstVideoStream(t *testing.T) {
	installStub(t)
	candidate := filepath.Join(t.TempDir(), "out.mp4")
	writeFile(t, candidate, "rendered")

	stubEnv(t, map[string]string{stubStdoutEnv: stubOutputProbeJSON})
	width, height, ok := ProbeSize(context.Background(), candidate)
	if !ok || width != 480 || height != 854 {
		t.Fatalf("ProbeSize = %d, %d, %v; want 480, 854, true", width, height, ok)
	}
}

func TestProbeSizeRejectsInvalidDescriptions(t *testing.T) {
	installStub(t)
	ctx := context.Background()
	candidate := filepath.Join(t.TempDir(), "out.mp4")
	writeFile(t, candidate, "rendered")

	cases := map[string]string{
		"非法 json": `not json`,
		"缺视频流":    `{"format":{"duration":"3.0"},"streams":[{"codec_type":"audio"}]}`,
		"时长为零":    `{"format":{"duration":"0"},"streams":[{"codec_type":"video","width":480,"height":854}]}`,
		"视频流宽高为零": `{"format":{"duration":"3.0"},"streams":[{"codec_type":"video","width":0,"height":854}]}`,
		"时长字段缺失":  `{"format":{},"streams":[{"codec_type":"video","width":480,"height":854}]}`,
	}
	for name, output := range cases {
		t.Run(name, func(t *testing.T) {
			stubEnv(t, map[string]string{stubStdoutEnv: output})
			if _, _, ok := ProbeSize(ctx, candidate); ok {
				t.Errorf("ProbeSize ok = true for %s", name)
			}
		})
	}
}

func TestProbeSizeRequiresNonEmptyFile(t *testing.T) {
	installStub(t)
	stubEnv(t, map[string]string{stubStdoutEnv: stubOutputProbeJSON})

	missing := filepath.Join(t.TempDir(), "missing.mp4")
	if _, _, ok := ProbeSize(context.Background(), missing); ok {
		t.Error("ProbeSize ok = true, want false for missing file")
	}
	empty := filepath.Join(t.TempDir(), "empty.mp4")
	writeFile(t, empty, "")
	if _, _, ok := ProbeSize(context.Background(), empty); ok {
		t.Error("ProbeSize ok = true, want false for empty file")
	}
}

func TestProbeMediaSendsWhitelistGuard(t *testing.T) {
	installStub(t)
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	candidate := filepath.Join(t.TempDir(), "out.mp4")
	writeFile(t, candidate, "rendered")
	stubEnv(t, map[string]string{stubStdoutEnv: stubOutputProbeJSON, stubArgvEnv: argvPath})

	if _, _, ok := ProbeSize(context.Background(), candidate); !ok {
		t.Fatal("ProbeSize ok = false, want true")
	}
	if argv := readStubArgv(t, argvPath); !containsPair(argv, "-protocol_whitelist", "file") {
		t.Errorf("probe argv = %v, want -protocol_whitelist file", argv)
	}
}
