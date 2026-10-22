// 镜头级分镜图 / 视频自主上传（对应 server/routers/shot_uploads.py）。
//
// 与通用资产上传分离：本管线按 script_file + shot_id 定位剧本条目、纳入版本管理、
// 回写剧本 generated_assets，并返回 asset_fingerprints 供上传方即时 cache-bust。
//
// 宫格切分后的单元格图 canonical 路径与图生视频模式相同（storyboards/scene_{id}.png），
// 按镜头上传即覆盖该单元格，宫格记录不动；联合图整图上传走宫格路由。
package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/ffmpeg"
	"github.com/shotwise/shotwise/internal/infra/img"
)

// 上传档位常量（与 Python upload_finalize 一致；视频上限复用 reference_video_upload 的同一值）。
const uploadImageMaxBytes = 30 * 1024 * 1024

// ShotUploadKind 是镜头上传的两条通道。
type ShotUploadKind string

const (
	ShotUploadStoryboard ShotUploadKind = "storyboard"
	ShotUploadVideo      ShotUploadKind = "video"
)

// uploadImageExtensions 是图片上传允许的扩展名（与 Python UPLOAD_IMAGE_EXTENSIONS 一致）。
var uploadImageExtensions = []string{".png", ".jpg", ".jpeg", ".webp"}

// ValidateUpload 按上传策略校验扩展名与申报大小，返回该类型的字节上限。
//
// declaredSize 只做 Content-Length 预拒；实际写入字节数由落盘环节兜底。
func ValidateUpload(filename string, declaredSize *int64, kind ShotUploadKind) (int64, error) {
	extensions := uploadImageExtensions
	maxBytes := int64(uploadImageMaxBytes)
	typeErrorKey := "unsupported_image_type"
	if kind == ShotUploadVideo {
		extensions = []string{".mp4", ".mov", ".m4v"}
		maxBytes = uploadVideoMaxBytes
		typeErrorKey = "unsupported_video_type"
	}
	if strings.TrimSpace(filename) == "" {
		return 0, &UploadValidationError{Status: 400, Key: "missing_filename"}
	}
	ext := strings.ToLower(filepath.Ext(filename))
	allowed := false
	for _, candidate := range extensions {
		if ext == candidate {
			allowed = true
			break
		}
	}
	if !allowed {
		return 0, &UploadValidationError{Status: 400, Key: typeErrorKey, Params: map[string]any{
			"ext": ext, "allowed": strings.Join(extensions, ", "),
		}}
	}
	if declaredSize != nil && *declaredSize > maxBytes {
		return 0, &UploadValidationError{Status: 413, Key: "upload_too_large", Params: map[string]any{
			"max_mb": maxBytes / (1024 * 1024),
		}}
	}
	return maxBytes, nil
}

// ShotUploadService 承载镜头上传的落盘、版本与剧本回写。
type ShotUploadService struct{ store *ProjectStore }

// NewShotUploadService 构造。
func NewShotUploadService(store *ProjectStore) *ShotUploadService {
	return &ShotUploadService{store: store}
}

// Upload 处理一次镜头上传：旧文件补登版本 → 写 canonical 路径 → 登记新版本 →
// 剧本元数据回写 → 返回相对路径、版本号与指纹。
//
// content 为 nil 表示视频通道以流式方式由调用方写入磁盘（大文件不进内存）。
func (s *ShotUploadService) Upload(
	ctx context.Context, projectName string, kind ShotUploadKind, scriptFile, shotID, originalFilename string,
	content []byte, stream TargetWriter,
) (map[string]any, error) {
	if kind != ShotUploadStoryboard && kind != ShotUploadVideo {
		return nil, &UploadValidationError{Status: 422, Key: "request_invalid"}
	}
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, &UploadValidationError{Status: 400, Key: "invalid_project_name",
			Params: map[string]any{"name": projectName}}
	}
	if !s.store.Exists(projectName) {
		return nil, &UploadValidationError{Status: 404, Key: "project_not_found",
			Params: map[string]any{"name": projectName}}
	}
	script, err := s.store.ReadScriptJSON(projectName, scriptFile)
	if err != nil {
		return nil, &UploadValidationError{Status: 404, Key: "script_not_found",
			Params: map[string]any{"name": scriptFile}}
	}
	if _, err := findShot(script, shotID); err != nil {
		return nil, &UploadValidationError{Status: 404, Key: "segment_not_found",
			Params: map[string]any{"id": shotID}}
	}

	resourceType := "storyboards"
	if kind == ShotUploadVideo {
		resourceType = "videos"
	}
	relativePath, err := resourceRelativePath(resourceType, shotID)
	if err != nil {
		return nil, &UploadValidationError{Status: 400, Key: "invalid_resource_id",
			Params: map[string]any{"resource_id": shotID}}
	}
	target := filepath.Join(projectDir, filepath.FromSlash(relativePath))

	versions := NewVersions(projectDir)
	// 旧文件若从未入版本库（如历史迁移），先补登，避免被覆盖后字节丢失。
	if _, err := versions.EnsureCurrentTracked(resourceType, shotID, "", target, nil); err != nil {
		return nil, err
	}

	if kind == ShotUploadStoryboard {
		pngBytes, normErr := img.NormalizeToPNG(content, nil)
		if normErr != nil {
			if errors.Is(normErr, img.ErrPixelLimit) {
				return nil, &UploadValidationError{Status: 400, Key: "image_pixels_too_large",
					Params: map[string]any{"max_megapixels": img.MaxUploadPixels / 1_000_000}}
			}
			return nil, &UploadValidationError{Status: 400, Key: "invalid_image_file"}
		}
		if err := writeFileAtomic(target, pngBytes); err != nil {
			return nil, err
		}
	} else {
		if stream == nil {
			return nil, &UploadValidationError{Status: 400, Key: "request_invalid"}
		}
		if err := stream(target); err != nil {
			return nil, err
		}
	}

	metadata := map[string]any{"source": uploadVersionSource}
	if originalFilename != "" {
		metadata["original_filename"] = originalFilename
	}
	version, _, err := versions.AddVersion(resourceType, shotID, "", target, metadata)
	if err != nil {
		return nil, err
	}

	if kind == ShotUploadStoryboard {
		if err := s.updateSceneAssets(projectName, scriptFile, shotID, map[string]any{
			"storyboard_image": relativePath,
		}); err != nil {
			return nil, err
		}
	} else {
		thumbnailRel := s.extractThumbnail(ctx, projectDir, shotID, target)
		// 旧 video_uri 指向上一次生成的 provider 端产物，与本地新文件不再对应，必须清空。
		if err := s.updateSceneAssets(projectName, scriptFile, shotID, map[string]any{
			"video_clip":      relativePath,
			"video_uri":       nil,
			"video_thumbnail": thumbnailRel,
		}); err != nil {
			return nil, err
		}
	}

	fingerprints := ComputeAffectedFingerprints(s.store, projectName, string(kind), shotID)
	return map[string]any{
		"success":            true,
		"path":               relativePath,
		"version":            version,
		"asset_fingerprints": fingerprints,
	}, nil
}

// TargetWriter 是「把上传字节写到目标路径」的回调（视频走流式，避免整段进内存）。
type TargetWriter func(target string) error

// extractThumbnail 抽首帧缩略图；抽取失败时删掉残留文件并返回 nil。
func (s *ShotUploadService) extractThumbnail(ctx context.Context, projectDir, shotID, videoPath string) any {
	thumbnailRel := "thumbnails/scene_" + shotID + ".jpg"
	thumbnailPath := filepath.Join(projectDir, filepath.FromSlash(thumbnailRel))
	if ffmpeg.MakeThumbnail(ctx, videoPath, thumbnailPath) {
		return thumbnailRel
	}
	_ = os.Remove(thumbnailPath)
	return nil
}

// updateSceneAssets 在剧本锁内补齐资产模板字段并回写指定键，随后重算条目状态。
//
// 资产回写热路径：只动 generated_assets，结构不可能因此变坏，故免结构校验。
func (s *ShotUploadService) updateSceneAssets(
	projectName, scriptFile, shotID string, updates map[string]any,
) error {
	return s.store.UpdateScript(projectName, scriptFile, func(script map[string]any) error {
		item, err := findShot(script, shotID)
		if err != nil {
			return err
		}
		assets, ok := item["generated_assets"].(map[string]any)
		if !ok {
			assets = map[string]any{}
			item["generated_assets"] = assets
		}
		for key, value := range domain.NewStoryboardGeneratedAssets() {
			if _, present := assets[key]; !present {
				assets[key] = value
			}
		}
		for key, value := range updates {
			assets[key] = value
		}
		domain.UpdateStoryboardItemStatus(item)
		return nil
	})
}
