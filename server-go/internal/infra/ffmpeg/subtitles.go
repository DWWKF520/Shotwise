package ffmpeg

import (
	"context"
)

// SubtitleBurnInArgs 构造把 SRT 字幕烧进视频的 argv：视频重编码为 H.264（veryfast/CRF 30），
// 音轨直通，可选音轨用 0:a:0? 映射，源视频无音轨时不报错。字幕路径按滤镜图口径转义。
func SubtitleBurnInArgs(ffmpegPath, videoPath, subtitlePath, outputPath string) []string {
	return []string{
		ffmpegPath, "-y",
		"-protocol_whitelist", "file",
		"-i", videoPath,
		"-map", "0:v:0",
		"-map", "0:a:0?",
		"-vf", "subtitles=filename='" + FilterPathEscape(subtitlePath) + "'",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "30",
		"-pix_fmt", "yuv420p",
		"-c:a", "copy",
		"-movflags", "+faststart",
		outputPath,
	}
}

// BurnInSubtitles 烧录字幕并校验产物非空（空产物报 subtitle_output_missing）。
// 路径归属校验（须落在项目内）由调用方完成，本函数只负责构造 argv、执行与产物校验。
func BurnInSubtitles(ctx context.Context, videoPath, subtitlePath, outputPath string, opts ...RunOption) error {
	ffmpegPath, err := resolveTool("ffmpeg")
	if err != nil {
		return err
	}
	if err := Run(ctx, RenderTimeout, SubtitleBurnInArgs(ffmpegPath, videoPath, subtitlePath, outputPath), opts...); err != nil {
		return err
	}
	if !nonEmptyFile(outputPath) {
		return &RenderToolError{Code: CodeSubtitleOutputMissing, Message: "ffmpeg did not produce a subtitled video"}
	}
	return nil
}
