package ffmpeg

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PackagingKind 是包装片段的种类。
type PackagingKind string

const (
	// PackagingCover 是静帧封面：单张图片循环成 dur 长度的无声片段。
	PackagingCover PackagingKind = "cover"
	// PackagingIntro 是片头：黑底居中文字。
	PackagingIntro PackagingKind = "intro"
	// PackagingOutro 是片尾：黑底居中文字。
	PackagingOutro PackagingKind = "outro"
)

// PackagingClip 描述一个片头/片尾/封面片段的生成参数。
type PackagingClip struct {
	Kind PackagingKind
	// SourcePath 是封面用的静帧图片路径，cover 必填。
	SourcePath string
	// Text 是 intro/outro 的文字，strip 后须非空。
	Text string
	// TextFile 是 drawtext 读取的文字文件路径；CreatePackagingClip 在为空时
	// 就近生成并在返回前清理，手动调用 BuildPackagingArgs 时须自行准备。
	TextFile string
	// DurationSeconds 须为正。
	DurationSeconds float64
	// Fit 是封面缩放方式："cover"（默认，裁满画布）或 "contain"（留黑边）。
	Fit string
	// OutputPath 是片段产物路径。
	OutputPath string
}

// BuildPackagingArgs 构造生成包装片段的 argv：封面走 -loop 1 静图 + scale/crop，
// 片头片尾走 lavfi 黑底 + drawtext；两者都不带音轨，由主渲染统一归一化。
func BuildPackagingArgs(ffmpegPath string, clip PackagingClip, width, height int, fps float64) ([]string, error) {
	if clip.DurationSeconds <= 0 {
		return nil, &RenderToolError{
			Code:    CodePackagingDurationInvalid,
			Message: "packaging " + string(clip.Kind) + " duration must be positive",
		}
	}
	size := strconv.Itoa(width) + "x" + strconv.Itoa(height)
	duration := formatFloat(clip.DurationSeconds)
	frameRate := formatFloat(fps)
	if clip.Kind == PackagingCover {
		if !fileExists(clip.SourcePath) {
			return nil, &RenderToolError{
				Code:    CodePackagingSourceInvalid,
				Message: "packaging cover source is unavailable: " + clip.SourcePath,
			}
		}
		scale := "scale=" + strconv.Itoa(width) + ":" + strconv.Itoa(height) + ":force_original_aspect_ratio=decrease," +
			"pad=" + size + ":(ow-iw)/2:(oh-ih)/2"
		if clip.Fit != "contain" {
			scale = "scale=" + strconv.Itoa(width) + ":" + strconv.Itoa(height) + ":force_original_aspect_ratio=increase," +
				"crop=" + size
		}
		return []string{
			ffmpegPath, "-y",
			"-protocol_whitelist", "file",
			"-loop", "1",
			"-i", clip.SourcePath,
			"-t", duration,
			"-vf", scale,
			"-r", frameRate,
			"-an",
			"-c:v", "libx264",
			"-pix_fmt", "yuv420p",
			clip.OutputPath,
		}, nil
	}
	if clip.TextFile == "" {
		return nil, &RenderToolError{
			Code:    CodePackagingTextInvalid,
			Message: "packaging " + string(clip.Kind) + " needs a text file",
		}
	}
	fontSize := width / 18
	if fontSize < 24 {
		fontSize = 24
	}
	drawtext := "drawtext=textfile='" + FilterPathEscape(clip.TextFile) + "':fontcolor=white:fontsize=" +
		strconv.Itoa(fontSize) + ":x=(w-text_w)/2:y=(h-text_h)/2"
	return []string{
		ffmpegPath, "-y",
		"-protocol_whitelist", "file",
		"-f", "lavfi",
		"-i", "color=c=black:s=" + size + ":r=" + frameRate + ":d=" + duration,
		"-vf", drawtext,
		"-an",
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		clip.OutputPath,
	}, nil
}

// CreatePackagingClip 生成包装片段并校验产物非空。intro/outro 的文字先写进就近的
// 文字文件再交给 drawtext（按 UTF-8 落盘，避免换行与过滤图元字符被内联解释），
// 返回前删除该文件；产物为空报 packaging_output_missing。
func CreatePackagingClip(ctx context.Context, clip PackagingClip, width, height int, fps float64, opts ...RunOption) error {
	if clip.Kind != PackagingCover {
		if strings.TrimSpace(clip.Text) == "" {
			return &RenderToolError{
				Code:    CodePackagingTextInvalid,
				Message: "packaging " + string(clip.Kind) + " text must be non-empty",
			}
		}
		if clip.TextFile == "" {
			clip.TextFile = filepath.Join(filepath.Dir(clip.OutputPath), "."+string(clip.Kind)+"-text.txt")
		}
		if err := os.WriteFile(clip.TextFile, []byte(clip.Text), 0o644); err != nil {
			return &RenderToolError{
				Code:    CodePackagingTextInvalid,
				Message: "write packaging text file: " + err.Error(),
				Err:     err,
			}
		}
		defer func() {
			if err := os.Remove(clip.TextFile); err != nil {
				slog.Warn("清理包装文字文件失败", "path", clip.TextFile, "err", err)
			}
		}()
	}
	ffmpegPath, err := resolveTool("ffmpeg")
	if err != nil {
		return err
	}
	args, err := BuildPackagingArgs(ffmpegPath, clip, width, height, fps)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(clip.OutputPath), 0o755); err != nil {
		return err
	}
	if err := Run(ctx, RenderTimeout, args, opts...); err != nil {
		return err
	}
	if !nonEmptyFile(clip.OutputPath) {
		return &RenderToolError{
			Code:    CodePackagingOutputMissing,
			Message: "ffmpeg did not produce the " + string(clip.Kind) + " clip",
		}
	}
	return nil
}
