package reference

import "github.com/shotwise/shotwise/internal/service"

// 参考生视频 unit 成片的用户上传链路（对应 server/services/upload_finalize.py 的视频面与
// server/routers/reference_videos.py upload_unit_video 的服务段）。
//
// 复用生成链路的 finalize 原语（抽缩略图、清旧 video_uri、status=completed、来源签名补写
// 版本档案），让上传产物的成片在状态推导、SSE 刷新、版本回滚上与 AI 生成的一致。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/infra/ffmpeg"
)

// 上传策略：只看扩展名与大小上限，宽高比/时长不阻塞——用户自主上传即自己负责。
// 不收 webm：字节原样存为 canonical .mp4，VP8/VP9 在 Safari/剪映一侧解码不可用；mp4/mov/m4v
// 同属 ISO BMFF 容器家族。
const (
	uploadChunkSize     = 1024 * 1024
)

var uploadVideoExtensions = map[string]bool{".mp4": true, ".mov": true, ".m4v": true}


// ValidateUploadVideo 按上传策略校验文件名扩展名与申报大小，返回该类型的字节上限。
// declaredSize 只做头部预拒；实际写入字节数由落盘函数兜底。
func ValidateUploadVideo(filename string, declaredSize *int64) (int64, error) {
	if filename == "" {
		return 0, &service.UploadValidationError{Status: 400, Key: "missing_filename"}
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if !uploadVideoExtensions[ext] {
		return 0, &service.UploadValidationError{Status: 400, Key: "unsupported_video_type", Params: map[string]any{
			"ext": ext, "allowed": ".mp4, .mov, .m4v",
		}}
	}
	if declaredSize != nil && *declaredSize > service.UploadVideoMaxBytes {
		return 0, uploadTooLarge()
	}
	return service.UploadVideoMaxBytes, nil
}

func uploadTooLarge() *service.UploadValidationError {
	return &service.UploadValidationError{Status: 413, Key: "upload_too_large", Params: map[string]any{
		"max_mb": service.UploadVideoMaxBytes / (1024 * 1024),
	}}
}

// IsUploadTooLarge 判定错误链上是否为上传超限（transport 与内部调用共用）。
func IsUploadTooLarge(err error) bool {
	var target *service.UploadValidationError
	if !errors.As(err, &target) {
		return false
	}
	return target.Key == "upload_too_large"
}

// ReferenceVideoRelativePath 返回 unit 成片在项目内的规范相对路径。
func ReferenceVideoRelativePath(unitID string) (string, error) {
	return service.ResourceRelativePath("reference_videos", unitID)
}

// SaveUploadedVideoStream 把上传流分块写入 target（不整体读入内存），超限报 service.UploadValidationError。
//
// 先写同目录 dot-tmp 再 rename：跨卷 rename 在 Windows 会失败，且避免半截文件被 canonical
// 路径的读取方看到。tmp 名带每次调用唯一的后缀，避免并发上传交错写同一 tmp 文件。
func SaveUploadedVideoStream(src io.Reader, target string, maxBytes int64) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	tmpPath := filepath.Join(filepath.Dir(target),
		fmt.Sprintf(".%s.%s.tmp%s", strings.TrimSuffix(filepath.Base(target), filepath.Ext(target)), suffix, filepath.Ext(target)))
	fail := func(cause error) error {
		_ = os.Remove(tmpPath)
		return cause
	}
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	var total int64
	buf := make([]byte, uploadChunkSize)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > maxBytes {
				out.Close()
				return fail(uploadTooLarge())
			}
			if _, writeErr := out.Write(buf[:n]); writeErr != nil {
				out.Close()
				return fail(writeErr)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			out.Close()
			return fail(readErr)
		}
	}
	if err := out.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return fail(err)
	}
	return nil
}

// randomSuffix 生成 8 位十六进制的 tmp 文件后缀。
func randomSuffix() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// RecordUploadVersion 把刚写入 canonical 路径的上传文件登记为新版本，返回版本号。
//
// 调用方须在覆写 canonical 文件之前先调 service.Versions.EnsureCurrentTracked 补登旧文件
// （镜像生成链路的版本顺序），否则旧版本字节会丢失。
func RecordUploadVersion(versions *service.Versions, resourceID, currentFile, originalFilename string) (int, error) {
	metadata := map[string]any{"source": service.UploadVersionSource}
	if originalFilename != "" {
		metadata["original_filename"] = originalFilename
	}
	version, _, err := versions.AddVersion("reference_videos", resourceID, "", currentFile, metadata)
	return version, err
}

// FinalizeUploadedReferenceVideoUnit 是上传成片的 finalize：抽缩略图 + 写回
// unit.generated_assets（清旧 video_uri、status=completed），来源签名按写回时刻的编排现算
// 并补进版本档案（上传拿不到生成时的编排快照）。
//
// unit 已被并发删除时返回 ReferenceUnitNotFoundError（落盘后绑定重查到锁内写回之间的窄竞态），
// 调用方转 404。
func FinalizeUploadedReferenceVideoUnit(
	store *service.ProjectStore, projectName, scriptFile, projectDir, resourceID, outputPath string,
	version int, versions *service.Versions,
) error {
	thumbDir := filepath.Join(projectDir, "reference_videos", "thumbnails")
	if err := os.MkdirAll(thumbDir, 0o755); err != nil {
		return err
	}
	thumbPath := filepath.Join(thumbDir, resourceID+".jpg")
	thumbRel := ""
	if ffmpeg.MakeThumbnail(context.Background(), outputPath, thumbPath) {
		thumbRel = "reference_videos/thumbnails/" + resourceID + ".jpg"
	} else {
		_ = os.Remove(thumbPath)
	}

	var writtenSignature *string
	err := store.UpdateScript(projectName, scriptFile, func(script map[string]any) error {
		written, err := service.ApplyUnitVideoAssetsHook(script, resourceID, "", thumbRel, service.NowISO(), service.SignatureCompute, "")
		if err != nil {
			return err
		}
		writtenSignature = written
		return nil
	})
	if err != nil {
		return err
	}
	if writtenSignature != nil && *writtenSignature != "" {
		// 版本记录是签名的还原档案：补写失败只降级还原时的基准精度，不足以推翻已成功的 finalize。
		if _, err := versions.UpdateVersionMetadata("reference_videos", resourceID, version, map[string]any{
			"source_signature": *writtenSignature,
		}); err != nil {
			slog.Warn("来源签名补写版本记录失败", "resource_id", resourceID, "version", version, "error", err)
		}
	}
	return nil
}