package ffmpeg

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// RenderOptions 控制一次拼接渲染的输出规格与执行参数。
type RenderOptions struct {
	Width  int
	Height int
	// FPS <= 0 时按 24 处理。
	FPS float64
	// Preset 为空时取 "medium"。
	Preset string
	// CRF 为 0 时取 18，其余取值须落在 1..51。
	CRF int
	// AudioBitrate 为空时取 "192k"。
	AudioBitrate string
	// Timeout <= 0 时取 RenderTimeout。
	Timeout time.Duration
	// OnProgress 收到 0..1 的渲染进度，分母为各段播放长度合计。
	OnProgress func(fraction float64)
	// CancelCheck 每 0.5s 轮询一次，返回 true 即 kill 渲染进程。
	CancelCheck func() bool
}

// PreviewOptions 控制低清预览渲染；零值等价于 Python 口径（480x854、veryfast、CRF 30、96k）。
type PreviewOptions struct {
	Width  int
	Height int
	// FPS <= 0 时按 24 处理。
	FPS float64
	// Timeout <= 0 时取 RenderTimeout。
	Timeout time.Duration
	// OnProgress 收到 0..1 的渲染进度。
	OnProgress func(fraction float64)
	// CancelCheck 每 0.5s 轮询一次，返回 true 即 kill 渲染进程。
	CancelCheck func() bool
}

// RenderVideo 把多段素材按统一规格拼接渲染成 H.264/AAC 的 MP4（-movflags +faststart），
// 返回前校验产物（文件非空 + ffprobe 描述出可用视频流）。工具缺失、超时、非零退出与
// 产物校验失败都返回 RenderToolError——渲染路径不做降级跳过。
func RenderVideo(ctx context.Context, segments []Segment, outputPath string, opts RenderOptions) (MediaInfo, error) {
	return renderVideo(ctx, segments, outputPath, opts, CodeRenderProbeInvalid)
}

// RenderPreview 渲染低清预览：宽高缺省 480x854，产物校验失败以 preview_probe_invalid 报告，
// 与主渲染的失败码区分开，便于上层把预览失败与成片失败分开处理。
func RenderPreview(ctx context.Context, segments []Segment, outputPath string, opts PreviewOptions) (MediaInfo, error) {
	width, height := opts.Width, opts.Height
	if width <= 0 {
		width = 480
	}
	if height <= 0 {
		height = 854
	}
	return renderVideo(ctx, segments, outputPath, RenderOptions{
		Width:        width,
		Height:       height,
		FPS:          opts.FPS,
		Preset:       "veryfast",
		CRF:          30,
		AudioBitrate: "96k",
		Timeout:      opts.Timeout,
		OnProgress:   opts.OnProgress,
		CancelCheck:  opts.CancelCheck,
	}, CodePreviewProbeInvalid)
}

// renderVideo 是两条渲染路径的公共实现，probeCode 区分产物校验失败的错误码。
func renderVideo(ctx context.Context, segments []Segment, outputPath string, opts RenderOptions, probeCode string) (MediaInfo, error) {
	if opts.Width <= 0 || opts.Height <= 0 {
		return MediaInfo{}, &RenderToolError{
			Code:    CodeRenderDimensionsInvalid,
			Message: "render dimensions must be positive",
		}
	}
	crf := opts.CRF
	if crf == 0 {
		crf = 18
	}
	if crf < 1 || crf > 51 {
		return MediaInfo{}, &RenderToolError{
			Code:    CodeRenderQualityInvalid,
			Message: "CRF must be between 1 and 51",
		}
	}
	ffmpegPath, err := resolveTool("ffmpeg")
	if err != nil {
		return MediaInfo{}, err
	}
	frameRate := opts.FPS
	if frameRate <= 0 {
		frameRate = 24
	}
	preset := opts.Preset
	if preset == "" {
		preset = "medium"
	}
	audioBitrate := opts.AudioBitrate
	if audioBitrate == "" {
		audioBitrate = "192k"
	}

	probes := make([]SegmentProbe, len(segments))
	for index, segment := range segments {
		probe, err := probeClipInput(ctx, segment.Path)
		if err != nil {
			return MediaInfo{}, err
		}
		probes[index] = probe
	}
	graph, err := ConcatSegments(segments, probes, opts.Width, opts.Height, frameRate)
	if err != nil {
		return MediaInfo{}, err
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return MediaInfo{}, fmt.Errorf("ffmpeg: mkdir output dir: %w", err)
	}

	args := []string{ffmpegPath, "-y"}
	for _, segment := range segments {
		// -ss/-t 是输入选项，须紧邻对应 -i 之前。
		if segment.StartSeconds > 0 {
			args = append(args, "-ss", fmt.Sprintf("%.6f", segment.StartSeconds))
		}
		if segment.DurationSeconds > 0 {
			args = append(args, "-t", fmt.Sprintf("%.6f", segment.DurationSeconds))
		}
		args = append(args, "-protocol_whitelist", "file", "-i", segment.Path)
	}
	args = append(args,
		"-filter_complex", graph,
		"-map", "[outv]",
		"-map", "[outa]",
		"-c:v", "libx264",
		"-preset", preset,
		"-crf", strconv.Itoa(crf),
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-b:a", audioBitrate,
		"-movflags", "+faststart",
		outputPath,
	)

	runOpts := []RunOption{}
	total := 0.0
	for index, segment := range segments {
		total += segment.PlayLength(probes[index])
	}
	if total > 0 && opts.OnProgress != nil {
		runOpts = append(runOpts, WithProgress(total, opts.OnProgress))
	}
	if opts.CancelCheck != nil {
		runOpts = append(runOpts, WithCancelCheck(opts.CancelCheck))
	}
	if err := Run(ctx, opts.Timeout, args, runOpts...); err != nil {
		return MediaInfo{}, err
	}
	return probeMedia(ctx, outputPath, probeCode)
}
