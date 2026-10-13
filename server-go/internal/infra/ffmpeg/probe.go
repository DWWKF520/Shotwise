package ffmpeg

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
)

// MediaInfo 是一次产物校验（ffprobe）的结果。
type MediaInfo struct {
	DurationSeconds float64
	Width           int
	Height          int
	AudioPresent    bool
}

// DurationSeconds 读取媒体容器时长（秒）；ffprobe 不可用或输出不可解析时 ok=false——
// 时长探测失败按跳过处理，不阻断调用方。
func DurationSeconds(ctx context.Context, path string) (float64, bool) {
	if !ProbeAvailable() {
		return 0, false
	}
	var stdout []byte
	if err := Run(ctx, MetadataTimeout, durationProbeArgs(path), WithStdout(&stdout)); err != nil {
		return 0, false
	}
	return parseSeconds(stdout)
}

// AudioDuration 读取单个参考音频文件的时长（秒）：先确认容器里确有音频流，再取容器时长。
// 两次探测都限定 -select_streams a，全程失败（含 ffprobe 缺失）都返回 ok=false，
// 调用方跳过总时长校验而不是当作超限。
func AudioDuration(ctx context.Context, path string) (float64, bool) {
	if !ProbeAvailable() {
		return 0, false
	}
	var stdout []byte
	err := Run(ctx, MetadataTimeout, []string{
		"ffprobe", "-v", "error", "-protocol_whitelist", "file",
		"-select_streams", "a",
		"-show_entries", "stream=codec_type",
		"-of", "csv=p=0",
		path,
	}, WithStdout(&stdout))
	if err != nil || !strings.Contains(string(stdout), "audio") {
		return 0, false
	}
	var durationBytes []byte
	if err := Run(ctx, MetadataTimeout, audioDurationProbeArgs(path), WithStdout(&durationBytes)); err != nil {
		return 0, false
	}
	return parseSeconds(durationBytes)
}

// MultiTotalDuration 累加多段参考音频总时长（秒）。任一文件探测失败即整体 ok=false
// 而非部分求和：半截总时长会让调用方误判「未超限」放行本该拦截的请求。
func MultiTotalDuration(ctx context.Context, paths []string) (float64, bool) {
	total := 0.0
	for _, path := range paths {
		duration, ok := AudioDuration(ctx, path)
		if !ok {
			return 0, false
		}
		total += duration
	}
	return total, true
}

// CountFrames 统计视频总帧数：走 -count_frames 全量解码读 nb_read_frames（精确但慢，
// 120s 档）。ffprobe 不可用或结果不可解析时 ok=false，调用方按降级处理。
func CountFrames(ctx context.Context, path string) (int, bool) {
	if !ProbeAvailable() {
		return 0, false
	}
	args := append(
		[]string{"ffprobe", "-v", "error", "-count_frames"},
		frameCountProbeArgs(path, "stream=nb_read_frames")...,
	)
	var stdout []byte
	if err := Run(ctx, CountFramesTimeout, args, WithStdout(&stdout)); err != nil {
		return 0, false
	}
	return parseFrameCount(stdout)
}

// ContainerFrameCount 从容器元数据读 nb_frames（瞬时，部分容器为 N/A，10s 档），
// 供尾帧提取先走快路径。
func ContainerFrameCount(ctx context.Context, path string) (int, bool) {
	if !ProbeAvailable() {
		return 0, false
	}
	args := append(
		[]string{"ffprobe", "-v", "error"},
		frameCountProbeArgs(path, "stream=nb_frames")...,
	)
	var stdout []byte
	if err := Run(ctx, MetadataTimeout, args, WithStdout(&stdout)); err != nil {
		return 0, false
	}
	return parseFrameCount(stdout)
}

// ProbeSize 读取首个视频流的宽高，供产物后置校验使用（probe + 文件非空）。
// ffprobe 不可用、文件缺失/为空或描述非法时 ok=false，不返回错误。
func ProbeSize(ctx context.Context, path string) (int, int, bool) {
	info, err := probeMedia(ctx, path, CodeRenderProbeInvalid)
	if err != nil {
		return 0, 0, false
	}
	return info.Width, info.Height, true
}

// durationProbeArgs 是只取时长的 ffprobe 参数；-protocol_whitelist file 限制 ffprobe
// 只读本地文件，避免把探测调用变成跟随 HLS/RTMP 播放列表发起网络请求的 SSRF 跳板。
func durationProbeArgs(path string) []string {
	return []string{
		"ffprobe", "-v", "error", "-protocol_whitelist", "file",
		"-show_entries", "format=duration",
		"-of", "csv=p=0",
		path,
	}
}

// audioDurationProbeArgs 是音频时长探测参数：-select_streams a 让这次调用的作用域
// 从 argv 即可读出；format=duration 是容器级字段，不受流选择影响。
func audioDurationProbeArgs(path string) []string {
	return []string{
		"ffprobe", "-v", "error", "-protocol_whitelist", "file",
		"-select_streams", "a",
		"-show_entries", "format=duration",
		"-of", "csv=p=0",
		path,
	}
}

// frameCountProbeArgs 是帧数探测的公共参数（末尾追加路径）。
func frameCountProbeArgs(path, entry string) []string {
	return []string{
		"-select_streams", "v:0",
		"-show_entries", entry,
		"-of", "csv=p=0",
		"-protocol_whitelist", "file",
		path,
	}
}

func parseSeconds(raw []byte) (float64, bool) {
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	if err != nil {
		return 0, false
	}
	return seconds, true
}

// parseFrameCount 解析帧数输出；容器为 N/A 或输出非整数时按不可用处理。
func parseFrameCount(raw []byte) (int, bool) {
	frames, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, false
	}
	return frames, true
}

// probeDoc 是 ffprobe -of json 产物描述里用到的字段。
type probeDoc struct {
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
	Streams []struct {
		CodecType string `json:"codec_type"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
	} `json:"streams"`
}

// probeMedia 校验一个媒体产物：文件存在且非空 + ffprobe 能描述出带正宽高、正时长的首个视频流。
// 渲染路径不可降级——工具缺失与描述非法都返回 RenderToolError，errorCode 供调用方区分场合。
func probeMedia(ctx context.Context, path, errorCode string) (MediaInfo, error) {
	ffprobe, err := resolveTool("ffprobe")
	if err != nil {
		return MediaInfo{}, err
	}
	fileInfo, err := os.Stat(path)
	if err != nil || fileInfo.IsDir() || fileInfo.Size() <= 0 {
		return MediaInfo{}, &RenderToolError{
			Code:    CodeRenderOutputMissing,
			Message: "rendered media file is missing",
		}
	}
	var stdout []byte
	err = Run(ctx, MetadataTimeout, []string{
		ffprobe, "-v", "error", "-protocol_whitelist", "file",
		"-show_entries", "format=duration:stream=codec_type,width,height",
		"-of", "json",
		path,
	}, WithStdout(&stdout))
	if err != nil {
		return MediaInfo{}, err
	}
	return parseMediaInfo(stdout, errorCode)
}

// parseMediaInfo 解析产物描述：时长必须为正，首个视频流的宽高必须为正
// （json 字段缺省与 0 同样按描述非法处理，Go 侧无法像 KeyError 那样区分两者）。
func parseMediaInfo(raw []byte, errorCode string) (MediaInfo, error) {
	var doc probeDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return MediaInfo{}, invalidDescription(errorCode)
	}
	duration, err := strconv.ParseFloat(doc.Format.Duration, 64)
	if err != nil {
		return MediaInfo{}, invalidDescription(errorCode)
	}
	info := MediaInfo{DurationSeconds: duration}
	foundVideo := false
	for _, stream := range doc.Streams {
		switch stream.CodecType {
		case "audio":
			info.AudioPresent = true
		case "video":
			if foundVideo || stream.Width <= 0 || stream.Height <= 0 {
				continue
			}
			info.Width, info.Height = stream.Width, stream.Height
			foundVideo = true
		}
	}
	if !foundVideo {
		return MediaInfo{}, invalidDescription(errorCode)
	}
	if info.DurationSeconds <= 0 {
		return MediaInfo{}, &RenderToolError{Code: errorCode, Message: "rendered video duration must be positive"}
	}
	return info, nil
}

func invalidDescription(errorCode string) *RenderToolError {
	return &RenderToolError{Code: errorCode, Message: "ffprobe returned an invalid video description"}
}

// probeClipInput 描述一段待拼接素材（容器时长 + 是否含音轨），供滤镜图归一化异构输入。
// 走元数据分级超时，不让损坏素材挂死整条渲染。
func probeClipInput(ctx context.Context, path string) (SegmentProbe, error) {
	ffprobe, err := resolveTool("ffprobe")
	if err != nil {
		return SegmentProbe{}, err
	}
	var stdout []byte
	err = Run(ctx, MetadataTimeout, []string{
		ffprobe, "-v", "error", "-protocol_whitelist", "file",
		"-show_entries", "format=duration:stream=codec_type",
		"-of", "json",
		path,
	}, WithStdout(&stdout))
	if err != nil {
		return SegmentProbe{}, err
	}
	var doc probeDoc
	if err := json.Unmarshal(stdout, &doc); err != nil {
		return SegmentProbe{}, invalidClipDescription(err)
	}
	duration, err := strconv.ParseFloat(doc.Format.Duration, 64)
	if err != nil {
		return SegmentProbe{}, invalidClipDescription(err)
	}
	probe := SegmentProbe{DurationSeconds: duration}
	for _, stream := range doc.Streams {
		if stream.CodecType == "audio" {
			probe.AudioPresent = true
		}
	}
	return probe, nil
}

func invalidClipDescription(cause error) *RenderToolError {
	return &RenderToolError{
		Code:    CodeSourceProbeInvalid,
		Message: "ffprobe returned an invalid clip description",
		Err:     cause,
	}
}
