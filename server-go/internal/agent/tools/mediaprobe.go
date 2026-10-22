package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/shotwise/shotwise/internal/infra/ffmpeg"
)

// 导演审阅的媒体探测与抽帧（对应 director_review.py 的 _probe_media / _extract_frame）。
// 只读：抽帧输出写入调用方提供的临时目录，不落盘到项目。

// reviewMaxFrameBytes 是单帧图片的体积上限，超出则该帧视为未抽取（与 Python _MAX_FRAME_BYTES 一致）。
const reviewMaxFrameBytes = 768_000

// ffprobeDoc 保留 format / streams 的原始形态，宽松解析与 Python 口径一致：
// 非对象的条目按缺省处理，而不是整体判定为无效结果。
type ffprobeDoc struct {
	Format  any `json:"format"`
	Streams any `json:"streams"`
}

// LookPathOK 报告可执行文件是否在 PATH 中（每次调用实时探测，不做进程级缓存）。
func LookPathOK(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// isRegularFile 对应 Python Path.is_file()：跟随符号链接，且必须是普通文件。
func isRegularFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// ProbeMedia 探测视频元数据。ffprobe 缺失时不报错，而是在结果中标记 probe_available=false，
// 由调用方转为 probe_unavailable 发现项。
func ProbeMedia(ctx context.Context, path string) map[string]any {
	available := LookPathOK("ffprobe")
	exists := isRegularFile(path)
	result := map[string]any{
		"exists":           exists,
		"readable":         exists,
		"probe_available":  available,
		"has_video_stream": nil,
		"has_audio_stream": nil,
		"duration_seconds": nil,
		"width":            nil,
		"height":           nil,
	}
	if !exists {
		return result
	}
	if !available {
		result["readable"] = nil
		result["error"] = "ffprobe 不可用，已跳过媒体元数据探测"
		return result
	}

	var stdout []byte
	err := ffmpeg.Run(ctx, ffmpeg.MetadataTimeout, []string{
		"ffprobe", "-v", "error", "-protocol_whitelist", "file",
		"-show_entries", "format=duration:stream=codec_type,width,height",
		"-of", "json",
		path,
	}, ffmpeg.WithStdout(&stdout))
	if err != nil {
		result["readable"] = false
		result["error"] = probeFailureMessage(err)
		return result
	}

	var doc ffprobeDoc
	if err := json.Unmarshal(stdout, &doc); err != nil {
		result["readable"] = false
		result["error"] = "ffprobe 返回了无效结果"
		return result
	}
	streams, _ := AsList(doc.Streams)
	video := firstStreamOfKind(streams, "video")
	audio := firstStreamOfKind(streams, "audio")
	format, _ := AsMap(doc.Format)
	result["has_video_stream"] = video != nil
	result["has_audio_stream"] = audio != nil
	if duration, ok := AsNumber(format["duration"]); ok {
		result["duration_seconds"] = duration
	}
	if video != nil {
		result["width"] = video["width"]
		result["height"] = video["height"]
	}
	return result
}

// probeFailureMessage 把子进程失败归类为与 Python 一致的可读原因：非零退出即「视频文件无法读取」。
func probeFailureMessage(err error) string {
	var rerr *ffmpeg.RenderToolError
	if errors.As(err, &rerr) && rerr.Code == ffmpeg.CodeProcessFailed {
		return "视频文件无法读取"
	}
	code := "unknown"
	if errors.As(err, &rerr) {
		code = rerr.Code
	}
	return fmt.Sprintf("ffprobe 执行失败: %s", code)
}

// firstStreamOfKind 返回第一个 codec_type 匹配的流对象，没有则返回 nil。
func firstStreamOfKind(streams []any, kind string) map[string]any {
	for _, raw := range streams {
		stream, ok := AsMap(raw)
		if !ok {
			continue
		}
		if codec, _ := AsString(stream["codec_type"]); codec == kind {
			return stream
		}
	}
	return nil
}

// ExtractFrame 在 timestamp 秒处抽取一帧 JPEG（与 Python 一致：输入端 -ss 定位、-q:v 4）。
// ffmpeg 缺失、执行失败、产物为空或超过体积上限时返回 false，由调用方记为未抽取。
// 输出文件名由帧序号生成，不拼接剧本中的 unit_id，避免不可信字段进入文件路径。
func ExtractFrame(ctx context.Context, videoPath string, timestamp float64, dir string, index int) ([]byte, bool) {
	if !LookPathOK("ffmpeg") {
		return nil, false
	}
	if timestamp < 0 {
		timestamp = 0
	}
	output := filepath.Join(dir, fmt.Sprintf("frame-%d.jpg", index))
	err := ffmpeg.Run(ctx, ffmpeg.FrameExtractTimeout, []string{
		"ffmpeg", "-y",
		"-protocol_whitelist", "file",
		"-ss", fmt.Sprintf("%.3f", timestamp),
		"-i", videoPath,
		"-frames:v", "1",
		"-f", "image2",
		"-q:v", "4",
		output,
	})
	if err != nil {
		return nil, false
	}
	data, err := os.ReadFile(output)
	if err != nil || len(data) == 0 || len(data) > reviewMaxFrameBytes {
		return nil, false
	}
	return data, true
}
