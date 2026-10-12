package ffmpeg

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Segment 是拼接时间线上的一段素材：本地文件 + 播放窗口 + 段后转场。
type Segment struct {
	Path string
	// StartSeconds 起播点（> 0 时转成 -ss）。
	StartSeconds float64
	// DurationSeconds 播放窗口（> 0 时转成 -t）；<= 0 表示播到素材结尾，
	// 实际长度改由探测到的容器时长扣除起播点推算。
	DurationSeconds float64
	// TransitionType 是本段与下一段之间的转场：空串与 "cut" 为硬切，
	// fade/dissolve/wipe 走 xfade；末段的声明没有后继边界，被忽略。
	TransitionType string
	// TransitionDurationSeconds 转场时长（秒）：非正数、类型不可渲染或相邻段不长于
	// 该时长时都退化为硬切。
	TransitionDurationSeconds float64
}

// SegmentProbe 是拼接前对一段素材的 ffprobe 结果（时长 + 是否含音轨）。
type SegmentProbe struct {
	AudioPresent    bool
	DurationSeconds float64
}

// PlayLength 返回该段在时间线上的播放长度：显式窗口优先，否则用探测时长扣除起播点；
// 两者都未知时返回 0。
func (s Segment) PlayLength(probe SegmentProbe) float64 {
	if s.DurationSeconds > 0 {
		return s.DurationSeconds
	}
	if probe.DurationSeconds > 0 {
		return math.Max(probe.DurationSeconds-s.StartSeconds, 0)
	}
	return 0
}

// xfadeTransitions 是可渲染转场到 xfade transition 名的映射；表外类型按硬切处理。
var xfadeTransitions = map[string]string{
	"fade":     "fade",
	"dissolve": "dissolve",
	"wipe":     "wipeleft",
}

// FilterPathEscape 把文件路径转成滤镜图里的路径字面量：统一正斜杠分隔，
// 再转义反斜杠、冒号（Windows 盘符）与单引号，供 drawtext/subtitles 的
// textfile/filename 参数使用。
func FilterPathEscape(path string) string {
	escaped := filepath.ToSlash(path)
	escaped = strings.ReplaceAll(escaped, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, ":", `\:`)
	escaped = strings.ReplaceAll(escaped, "'", `\'`)
	return escaped
}

// transitionBoundary 是一个边界上解析出的转场；Overlap <= 0 表示退化为硬切。
type transitionBoundary struct {
	Type    string
	Overlap float64
}

// resolveTransitions 把每段声明的转场解析成该边界真正渲染的重叠时长，语义与
// Python 侧 resolve_timeline_transitions 一致：类型不可渲染、时长非正、相邻段不长于
// 重叠时退化为硬切；中间段装不下两侧重叠时清掉靠左的那一侧重叠。
func resolveTransitions(segments []Segment, lengths []float64) []transitionBoundary {
	if len(lengths) < 2 {
		return nil
	}
	boundaries := make([]transitionBoundary, len(lengths)-1)
	for index := range boundaries {
		declared := segments[index]
		if _, ok := xfadeTransitions[declared.TransitionType]; !ok {
			continue
		}
		overlap := declared.TransitionDurationSeconds
		if math.IsNaN(overlap) || math.IsInf(overlap, 0) || overlap <= 0 {
			continue
		}
		if lengths[index] > overlap && lengths[index+1] > overlap {
			boundaries[index] = transitionBoundary{Type: declared.TransitionType, Overlap: overlap}
		}
	}
	for index := 1; index < len(boundaries); index++ {
		if boundaries[index-1].Overlap <= 0 || boundaries[index].Overlap <= 0 {
			continue
		}
		if lengths[index] < boundaries[index-1].Overlap+boundaries[index].Overlap {
			boundaries[index-1].Overlap = 0
		}
	}
	return boundaries
}

// ConcatSegments 构造把异构素材归一到统一规格再拼接的 filter_complex 图，
// 以 [outv]/[outa] 结尾。
//
// 每段先 scale+pad+setsar+fps+format+setpts 归一到画布，音频 aresample+aformat+asetpts
// 归一到 48kHz 立体声；缺音轨的段用 anullsrc 补等长静音。相邻段声明了转场时按组
// xfade+acrossfade 合并、组间 concat，没有任何可渲染转场时整体一次 concat。
//
// 输入编号取自 -i 在 argv 中的出现顺序，调用方须按同一顺序传参。
func ConcatSegments(segments []Segment, probes []SegmentProbe, width, height int, fps float64) (string, error) {
	if len(segments) == 0 {
		return "", &RenderToolError{Code: CodeSourceRefInvalid, Message: "timeline must contain at least one clip"}
	}
	if len(segments) != len(probes) {
		return "", fmt.Errorf("ffmpeg: %d segments but %d probes", len(segments), len(probes))
	}
	chains := make([]string, 0, len(segments)*3)
	lengths := make([]float64, len(segments))
	for index, segment := range segments {
		probe := probes[index]
		lengths[index] = segment.PlayLength(probe)
		chains = append(chains, fmt.Sprintf(
			"[%d:v]scale=%d:%d:force_original_aspect_ratio=decrease,"+
				"pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=%s,format=yuv420p,"+
				"setpts=PTS-STARTPTS[v%d]",
			index, width, height, width, height, formatFloat(fps), index,
		))
		if probe.AudioPresent {
			chains = append(chains, fmt.Sprintf(
				"[%d:a]aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo,"+
					"asetpts=PTS-STARTPTS[a%d]",
				index, index,
			))
			continue
		}
		if lengths[index] <= 0 {
			return "", &RenderToolError{
				Code:    CodeSourceDurationInvalid,
				Message: fmt.Sprintf("clip duration is unknown for silent input: %s", filepath.Base(segment.Path)),
			}
		}
		chains = append(chains, fmt.Sprintf(
			"anullsrc=channel_layout=stereo:sample_rate=48000:d=%.6f[a%d]", lengths[index], index,
		))
	}

	boundaries := resolveTransitions(segments, lengths)
	renderable := false
	for _, boundary := range boundaries {
		if boundary.Overlap > 0 {
			renderable = true
			break
		}
	}
	if !renderable {
		inputs := make([]string, 0, len(segments))
		for index := range segments {
			inputs = append(inputs, fmt.Sprintf("[v%d][a%d]", index, index))
		}
		chains = append(chains, fmt.Sprintf(
			"%sconcat=n=%d:v=1:a=1[outv][outa]", strings.Join(inputs, ""), len(segments),
		))
		return strings.Join(chains, ";"), nil
	}

	// 按可渲染边界切组：组内链式 xfade+acrossfade，组间 concat 保硬切。
	groups := make([][]int, 0, len(segments))
	current := []int{0}
	for index, boundary := range boundaries {
		if boundary.Overlap <= 0 {
			groups = append(groups, current)
			current = []int{index + 1}
			continue
		}
		current = append(current, index+1)
	}
	groups = append(groups, current)

	outputs := make([][2]string, 0, len(groups))
	for groupIndex, group := range groups {
		if len(group) == 1 {
			clip := group[0]
			outputs = append(outputs, [2]string{
				fmt.Sprintf("[v%d]", clip), fmt.Sprintf("[a%d]", clip),
			})
			continue
		}
		previousVideo := fmt.Sprintf("[v%d]", group[0])
		previousAudio := fmt.Sprintf("[a%d]", group[0])
		merged := lengths[group[0]]
		for offsetIndex := 1; offsetIndex < len(group); offsetIndex++ {
			boundary := boundaries[group[offsetIndex]-1]
			videoOut := fmt.Sprintf("[gv%d_%d]", groupIndex, offsetIndex)
			audioOut := fmt.Sprintf("[ga%d_%d]", groupIndex, offsetIndex)
			chains = append(chains,
				fmt.Sprintf("%s[v%d]xfade=transition=%s:duration=%.6f:offset=%.6f%s",
					previousVideo, group[offsetIndex], xfadeTransitions[boundary.Type],
					boundary.Overlap, math.Max(merged-boundary.Overlap, 0), videoOut),
				fmt.Sprintf("%s[a%d]acrossfade=d=%.6f:c1=tri:c2=tri%s",
					previousAudio, group[offsetIndex], boundary.Overlap, audioOut),
			)
			previousVideo, previousAudio = videoOut, audioOut
			merged += lengths[group[offsetIndex]] - boundary.Overlap
		}
		outputs = append(outputs, [2]string{previousVideo, previousAudio})
	}

	if len(outputs) == 1 {
		chains = append(chains,
			outputs[0][0]+"null[outv]",
			outputs[0][1]+"anull[outa]",
		)
		return strings.Join(chains, ";"), nil
	}
	concatInputs := make([]string, 0, len(outputs))
	for _, output := range outputs {
		concatInputs = append(concatInputs, output[0], output[1])
	}
	chains = append(chains, fmt.Sprintf(
		"%sconcat=n=%d:v=1:a=1[outv][outa]", strings.Join(concatInputs, ""), len(outputs),
	))
	return strings.Join(chains, ";"), nil
}

// ConcatListContent 生成 concat demuxer 的清单文本：每行一条绝对路径（正斜杠分隔），
// 单引号按 shell 词法转义成 '\”，路径里的空格、引号不会拆散或截断条目。
func ConcatListContent(clips []string) string {
	var builder strings.Builder
	for _, clip := range clips {
		path := clip
		if absolute, err := filepath.Abs(path); err == nil {
			path = absolute
		}
		path = filepath.ToSlash(path)
		builder.WriteString("file '")
		builder.WriteString(strings.ReplaceAll(path, "'", `'\''`))
		builder.WriteString("'\n")
	}
	return builder.String()
}

// ConcatDemuxerArgs 构造 file-list concat 的 argv：-safe 0 放行绝对路径，
// -c copy 不重编码——清单内素材须同编码参数，否则边界处会损坏。
func ConcatDemuxerArgs(ffmpegPath, listPath, outputPath string) []string {
	return []string{
		ffmpegPath, "-y",
		"-f", "concat", "-safe", "0",
		"-protocol_whitelist", "file",
		"-i", listPath,
		"-c", "copy",
		"-movflags", "+faststart",
		outputPath,
	}
}

// ConcatDemuxer 用 file-list concat 把同规格片段串接为一个文件。清单写入临时目录并在
// 返回前清理，产物按「非空 + ffprobe 描述出可用视频流」双检；timeout <= 0 时取 RenderTimeout。
func ConcatDemuxer(ctx context.Context, clips []string, outputPath string, timeout time.Duration, opts ...RunOption) error {
	if len(clips) == 0 {
		return &RenderToolError{Code: CodeSourceRefInvalid, Message: "concat list must contain at least one clip"}
	}
	ffmpegPath, err := resolveTool("ffmpeg")
	if err != nil {
		return err
	}
	workDir, err := os.MkdirTemp("", "shotwise-concat-")
	if err != nil {
		return fmt.Errorf("ffmpeg: create concat work dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	listPath := filepath.Join(workDir, "concat-list.txt")
	if err := os.WriteFile(listPath, []byte(ConcatListContent(clips)), 0o644); err != nil {
		return fmt.Errorf("ffmpeg: write concat list: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("ffmpeg: mkdir output dir: %w", err)
	}
	if err := Run(ctx, timeout, ConcatDemuxerArgs(ffmpegPath, listPath, outputPath), opts...); err != nil {
		return err
	}
	_, err = probeMedia(ctx, outputPath, CodeRenderProbeInvalid)
	return err
}

// formatFloat 按 Python %g 的口径格式化（最多 6 位有效数字，去掉尾随零）。
func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', 6, 64)
}
