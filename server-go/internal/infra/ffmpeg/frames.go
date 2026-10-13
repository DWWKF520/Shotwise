package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ThumbnailOption 定制缩略图输出。
type ThumbnailOption func(*thumbnailConfig)

type thumbnailConfig struct {
	quality int
}

// WithThumbnailQuality 指定 -q:v 质量（数值越小质量越高）；<= 0 时取 2。
func WithThumbnailQuality(quality int) ThumbnailOption {
	return func(cfg *thumbnailConfig) { cfg.quality = quality }
}

// MakeThumbnail 抽取视频首帧写为 JPEG 缩略图（-vframes 1）。
// ffmpeg 缺失或执行失败返回 false：调用方走「不写缩略图字段」分支，
// 前端 <video poster> 为空时会原生从视频流取首帧，无需服务端 placeholder。
func MakeThumbnail(ctx context.Context, src, out string, opts ...ThumbnailOption) bool {
	if !fileExists(src) {
		return false
	}
	if !Available() {
		logSkip("ffmpeg 不可用，跳过缩略图提取（前端将原生取首帧）", src)
		return false
	}
	cfg := thumbnailConfig{quality: 2}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.quality <= 0 {
		cfg.quality = 2
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		slog.Warn("创建缩略图目录失败", "path", out, "err", err)
		return false
	}
	args := []string{
		"ffmpeg",
		"-protocol_whitelist", "file",
		"-i", src,
		"-vframes", "1",
		"-q:v", strconv.Itoa(cfg.quality),
		"-y", out,
	}
	if err := Run(ctx, ThumbnailTimeout, args); err != nil {
		// ffmpeg 失败前可能已写出半截文件，清掉避免调用方把损坏的缩略图当产物用。
		if removeErr := os.Remove(out); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			slog.Warn("清理缩略图失败", "path", out, "err", removeErr)
		}
		slog.Warn("提取视频缩略图失败", "path", src, "err", err)
		return false
	}
	return fileExists(out)
}

// ExtractFrame 按解码顺序取第 index 帧写到 outPath：先写同目录临时文件，
// 校验非空后原子替换目标文件；失败（含超时）清掉临时文件并返回 false。
// 父目录须已存在，由调用方负责创建。
func ExtractFrame(ctx context.Context, videoPath string, index int, outPath string) bool {
	tempPath := tempSibling(outPath)
	// 先清残留临时文件：否则失败路径上会把上一轮的残留当成本轮产物。
	if err := os.Remove(tempPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("清理抽帧临时文件失败", "path", tempPath, "err", err)
		return false
	}
	args := []string{
		"ffmpeg", "-y",
		"-protocol_whitelist", "file",
		"-i", videoPath,
		"-vf", fmt.Sprintf(`select='eq(n\,%d)'`, index),
		"-fps_mode", "vfr",
		"-frames:v", "1",
		tempPath,
	}
	if err := Run(ctx, FrameExtractTimeout, args); err != nil {
		slog.Warn("抽帧失败", "path", videoPath, "frame", index, "err", err)
		_ = os.Remove(tempPath)
		return false
	}
	info, err := os.Stat(tempPath)
	if err != nil || info.Size() < 1 {
		_ = os.Remove(tempPath)
		return false
	}
	if err := os.Rename(tempPath, outPath); err != nil {
		slog.Warn("抽帧结果替换失败", "path", outPath, "err", err)
		_ = os.Remove(tempPath)
		return false
	}
	return true
}

// ExtractLastFrame 提取视频最后一帧（输出格式由扩展名决定）。需要 ffmpeg 与 ffprobe 都在：
// 先走容器元数据 nb_frames 的瞬时快路径，缺失再回退 -count_frames 全量解码；
// 用 select 精确定位末帧而不是 -sseof 的时间戳近似。任一步失败返回 false。
func ExtractLastFrame(ctx context.Context, videoPath, outPath string) bool {
	if !fileExists(videoPath) {
		return false
	}
	if !Available() || !ProbeAvailable() {
		logSkip("ffmpeg/ffprobe 不可用，跳过尾帧提取", videoPath)
		return false
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		slog.Warn("创建尾帧目录失败", "path", outPath, "err", err)
		return false
	}
	if total, ok := ContainerFrameCount(ctx, videoPath); ok && total > 0 {
		if ExtractFrame(ctx, videoPath, total-1, outPath) {
			return true
		}
	}
	total, ok := CountFrames(ctx, videoPath)
	if !ok || total < 1 {
		return false
	}
	return ExtractFrame(ctx, videoPath, total-1, outPath)
}

// tempSibling 返回与目标同目录的临时文件路径（".<stem>.tmp<ext>"），
// 同目录保证 os.Rename 是原子替换而不是跨卷拷贝。
func tempSibling(outPath string) string {
	base := filepath.Base(outPath)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return filepath.Join(filepath.Dir(outPath), "."+stem+".tmp"+ext)
}
