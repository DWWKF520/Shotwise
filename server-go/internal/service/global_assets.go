// 全局资产库（跨项目复用的 character / scene / prop）服务。
//
// DB 与磁盘的一致性约定与 Python 侧一致:先落盘再写 DB;任何失败路径清理已落盘
// 文件不留孤儿;覆盖写时旧文件在 DB 写入成功后才删除。
package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/repository"
)

// GlobalAssetService 承载 assets 表 + _global_assets/ 目录的读写。
type GlobalAssetService struct {
	repo  *repository.GlobalAssetRepository
	store *ProjectStore
}

// NewGlobalAssetService 构造全局资产库服务。
func NewGlobalAssetService(repo *repository.GlobalAssetRepository, store *ProjectStore) *GlobalAssetService {
	return &GlobalAssetService{repo: repo, store: store}
}

var (
	// ErrGlobalAssetNotFound 库里没有该 id 的资产。
	ErrGlobalAssetNotFound = errors.New("global asset not found")
	// ErrGlobalInvalidType 类型不在全局库登记表（product 不入库）。
	ErrGlobalInvalidType = errors.New("asset type not allowed in global library")
)

// GlobalAssetExistsError 同类型同名资产已存在。
type GlobalAssetExistsError struct{ Name string }

func (e *GlobalAssetExistsError) Error() string { return "asset already exists: " + e.Name }

// 全局库图片上传限制,与 Python 侧一致。
const (
	globalAssetMaxUploadBytes = 5 * 1024 * 1024
)

var globalAssetImageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true}

// SerializedGlobalAsset 是对外序列化形状,字段与 Python 侧 _serialize 一致。
type SerializedGlobalAsset struct {
	ID            string  `json:"id"`
	Type          string  `json:"type"`
	Name          string  `json:"name"`
	Description   string  `json:"description"`
	VoiceStyle    string  `json:"voice_style"`
	ImagePath     *string `json:"image_path"`
	AudioPath     *string `json:"audio_path"`
	SourceProject *string `json:"source_project"`
	UpdatedAt     string  `json:"updated_at"`
}

func serializeGlobalAsset(a *repository.Asset) SerializedGlobalAsset {
	out := SerializedGlobalAsset{
		ID:            a.ID,
		Type:          a.Type,
		Name:          a.Name,
		Description:   a.Description,
		VoiceStyle:    a.VoiceStyle,
		ImagePath:     a.ImagePath,
		AudioPath:     a.AudioPath,
		SourceProject: a.SourceProject,
	}
	if a.UpdatedAt.Valid {
		out.UpdatedAt = a.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// List 列出资产。
func (s *GlobalAssetService) List(assetType, search string, limit, offset int) ([]SerializedGlobalAsset, error) {
	items, err := s.repo.List(repository.GlobalAssetQuery{
		Type:   assetType,
		Search: search,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, err
	}
	out := make([]SerializedGlobalAsset, 0, len(items))
	for i := range items {
		out = append(out, serializeGlobalAsset(&items[i]))
	}
	return out, nil
}

// Get 按 id 取资产。
func (s *GlobalAssetService) Get(id string) (*SerializedGlobalAsset, error) {
	a, err := s.repo.GetByID(id)
	if err != nil {
		return nil, ErrGlobalAssetNotFound
	}
	out := serializeGlobalAsset(a)
	return &out, nil
}

// saveUploadImage 把上传图片落盘到 _global_assets/{type}/{uuid}{ext},返回相对路径。
func (s *GlobalAssetService) saveUploadImage(reader io.Reader, filename, assetType string) (string, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if !globalAssetImageExts[ext] {
		return "", errUnsupportedImageFormat{}
	}
	root, err := s.globalAssetsDir(assetType)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	uid := uuid.NewString()
	rel := fmt.Sprintf("_global_assets/%s/%s%s", assetType, uid, ext)
	target := filepath.Join(root, uid+ext)
	if err := writeCapped(target, reader, globalAssetMaxUploadBytes); err != nil {
		return "", err
	}
	return rel, nil
}

// errUnsupportedImageFormat 扩展名不在白名单（415）。
type errUnsupportedImageFormat struct{}

func (errUnsupportedImageFormat) Error() string { return "unsupported image format" }

// ErrImageTooLarge 超出 5MB 上限（413）。
var ErrImageTooLarge = errors.New("image too large")

// writeCapped 落盘并强制大小上限;超限时删除半成品。
func writeCapped(target string, reader io.Reader, max int64) error {
	f, err := os.Create(target)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(reader, max+1))
	closeErr := f.Close()
	if err == nil && n > max {
		_ = os.Remove(target)
		return ErrImageTooLarge
	}
	if err != nil {
		_ = os.Remove(target)
		return err
	}
	return closeErr
}

// deleteGlobalAssetFile 删除相对 projects_root 的文件;不存在视为成功（并发删除容忍）。
func (s *GlobalAssetService) deleteGlobalAssetFile(rel string) {
	p, err := fs.SafeJoin(s.store.Root(), true, filepath.FromSlash(rel))
	if err != nil {
		return
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		// 删除失败仅记录:文件残留不影响 DB 一致性,与 Python 侧同策略
		_ = err
	}
}

func (s *GlobalAssetService) globalAssetsDir(assetType string) (string, error) {
	return fs.SafeJoin(s.store.Root(), true, "_global_assets", assetType)
}

// Create 入库新资产;image 为空表示无图。冲突时清理已落盘文件并返回 409 语义错误。
func (s *GlobalAssetService) Create(assetType, rawName, description, voiceStyle string, image io.Reader, imageFilename string) (*SerializedGlobalAsset, error) {
	if !domain.IsGlobalLibraryKind(assetType) {
		return nil, ErrGlobalInvalidType
	}
	name, err := domain.ValidateAssetName(rawName)
	if err != nil {
		return nil, &AssetNameError{Raw: rawName}
	}
	var imagePath *string
	if image != nil {
		rel, err := s.saveUploadImage(image, imageFilename, assetType)
		if err != nil {
			return nil, err
		}
		imagePath = &rel
	}
	a, ok, err := s.repo.Create(repository.CreateParams{
		Type:        assetType,
		Name:        name,
		Description: description,
		VoiceStyle:  voiceStyle,
		ImagePath:   imagePath,
	})
	if err != nil {
		if imagePath != nil {
			s.deleteGlobalAssetFile(*imagePath)
		}
		return nil, err
	}
	if !ok {
		if imagePath != nil {
			s.deleteGlobalAssetFile(*imagePath)
		}
		return nil, &GlobalAssetExistsError{Name: name}
	}
	out := serializeGlobalAsset(a)
	return &out, nil
}

// Patch 更新 name / description / voice_style（nil 表示不更新）。
func (s *GlobalAssetService) Patch(id string, name, description, voiceStyle *string) (*SerializedGlobalAsset, error) {
	a, err := s.repo.GetByID(id)
	if err != nil {
		return nil, ErrGlobalAssetNotFound
	}
	fields := map[string]any{}
	if name != nil {
		n, err := domain.ValidateAssetName(*name)
		if err != nil {
			return nil, &AssetNameError{Raw: *name}
		}
		if n != a.Name {
			existing, err := s.repo.GetByTypeName(a.Type, n)
			if err == nil && existing.ID != a.ID {
				return nil, &GlobalAssetExistsError{Name: n}
			}
		}
		fields["name"] = n
	}
	if description != nil {
		fields["description"] = *description
	}
	if voiceStyle != nil {
		fields["voice_style"] = *voiceStyle
	}
	updated, err := s.repo.Update(id, fields)
	if err != nil {
		// 唯一约束兜底（并发 PATCH 同名）
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, &GlobalAssetExistsError{Name: ""}
		}
		return nil, err
	}
	out := serializeGlobalAsset(updated)
	return &out, nil
}

// Delete 删除资产与磁盘文件;不存在时幂等成功（Python 侧同语义,204）。
func (s *GlobalAssetService) Delete(id string) error {
	a, err := s.repo.GetByID(id)
	if err != nil {
		return nil
	}
	if a.ImagePath != nil {
		s.deleteGlobalAssetFile(*a.ImagePath)
	}
	if a.AudioPath != nil {
		s.deleteGlobalAssetFile(*a.AudioPath)
	}
	return s.repo.Delete(id)
}

// ReplaceImage 换图:新图先落盘,DB 更新失败时清新图保旧图,成功后删旧图。
func (s *GlobalAssetService) ReplaceImage(id string, image io.Reader, imageFilename string) (*SerializedGlobalAsset, error) {
	a, err := s.repo.GetByID(id)
	if err != nil {
		return nil, ErrGlobalAssetNotFound
	}
	oldPath := a.ImagePath
	newRel, err := s.saveUploadImage(image, imageFilename, a.Type)
	if err != nil {
		return nil, err
	}
	updated, err := s.repo.Update(id, map[string]any{"image_path": newRel})
	if err != nil {
		s.deleteGlobalAssetFile(newRel)
		return nil, err
	}
	if oldPath != nil && *oldPath != newRel {
		s.deleteGlobalAssetFile(*oldPath)
	}
	out := serializeGlobalAsset(updated)
	return &out, nil
}
