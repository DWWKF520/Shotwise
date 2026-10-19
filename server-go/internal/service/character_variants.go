// 角色变体 CRUD 与头像上传。对应 project_manager.py 的 create/update/delete_character_variant
// 与 replace_project_character_avatar，加上 characters.py 的 upload_character_avatar。
package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/infra/img"
)

// 变体域错误。传输层按类型映射状态码：
//   - *CharacterVariantSlugExistsError → 409（Python ValueError）
//   - ErrVariantInvalidSlug / ErrVariantInvalidStatus / ErrVariantInvalidPath → 422
//   - ErrAssetNotFound → 角色 404
//   - ErrVariantNotFound → 变体 404
var (
	ErrVariantNotFound      = errors.New("character variant not found")
	ErrVariantInvalidSlug   = errors.New("character_variant_invalid_slug")
	ErrVariantInvalidStatus = errors.New("character_variant_invalid_status")
	ErrVariantInvalidPath   = errors.New("character_variant_invalid_image_path")
	// ErrAvatarInvalidFile 表示上传字节不是可解析的图片（400）。
	ErrAvatarInvalidFile = errors.New("character_avatar_invalid_file")
)

// CharacterVariantSlugExistsError 是 slug 已占用的冲突错误。
type CharacterVariantSlugExistsError struct{ Slug string }

func (e *CharacterVariantSlugExistsError) Error() string { return "slug exists: " + e.Slug }

// CharacterAvatarTooLargeError 是头像像素超限错误。
type CharacterAvatarTooLargeError struct{ MaxMegapixels int }

func (e *CharacterAvatarTooLargeError) Error() string { return "avatar too large" }

// AvatarMaxBytes 是头像字节上限（10MB，与 Python 一致）。
const AvatarMaxBytes = 10 << 20

// CharacterVariants 承载角色变体与头像操作。
type CharacterVariants struct {
	store *ProjectStore
}

// NewCharacterVariants 构造服务。
func NewCharacterVariants(store *ProjectStore) *CharacterVariants {
	return &CharacterVariants{store: store}
}

// ListVariants 列出角色的全部变体（容忍脏数据，非法条目跳过）。
func (s *CharacterVariants) ListVariants(project, name string) ([]map[string]any, error) {
	character, _, err := s.characterEntry(project, name)
	if err != nil {
		return nil, err
	}
	return domain.IterCharacterVariants(character, name), nil
}

// GetVariant 按 id 或 slug 定位变体。
func (s *CharacterVariants) GetVariant(project, name, identifier string) (map[string]any, error) {
	character, _, err := s.characterEntry(project, name)
	if err != nil {
		return nil, err
	}
	variant := domain.FindCharacterVariant(character, identifier)
	if variant == nil {
		return nil, ErrVariantNotFound
	}
	return variant, nil
}

// VariantCreateInput 是创建入参（对应 CharacterVariantCreateRequest，extra="forbid"）。
type VariantCreateInput struct {
	Slug         string
	DisplayName  *string
	Description  *string
	ImagePath    *string
	Status       *string
	Metadata     map[string]any
	ImageAssetID *string
}

// CreateVariant 创建稳定 id 的变体；slug 冲突报 409。
func (s *CharacterVariants) CreateVariant(project, name string, in VariantCreateInput) (map[string]any, error) {
	cleanSlug, err := domain.ValidateAssetName(in.Slug)
	if err != nil {
		return nil, ErrVariantInvalidSlug
	}
	cleanImage, err := s.validateVariantImagePath(project, in.ImagePath)
	if err != nil {
		return nil, err
	}
	cleanStatus, err := validateVariantStatus(in.Status)
	if err != nil {
		return nil, err
	}
	var created map[string]any
	err = s.store.Update(project, func(pf *domain.ProjectFile) error {
		character, _, err := locateCharacter(pf, name)
		if err != nil {
			return err
		}
		rawVariants, _ := character.Fields()["variants"].(map[string]any)
		if rawVariants == nil {
			rawVariants = map[string]any{}
			character.Fields()["variants"] = rawVariants
		}
		// slug 冲突按 NFC 坐标系判定（variants key 可能是 NFD 存量形态）。
		if _, conflict := rawVariantsResolveKey(rawVariants, cleanSlug); conflict {
			return &CharacterVariantSlugExistsError{Slug: cleanSlug}
		}
		item := map[string]any{
			"id":          domain.NewVariantID(),
			"slug":        cleanSlug,
			"status":      variantStatusValue(cleanStatus),
			"image_path":  variantImagePathValue(cleanImage),
			"metadata":    map[string]any{},
			"description": "",
		}
		if in.DisplayName != nil {
			item["display_name"] = *in.DisplayName
		}
		if in.Description != nil {
			item["description"] = *in.Description
		}
		if in.Metadata != nil {
			item["metadata"] = in.Metadata
		}
		if in.ImageAssetID != nil && *in.ImageAssetID != "" {
			item["image_asset_id"] = *in.ImageAssetID
		}
		rawVariants[cleanSlug] = item
		created = domain.NormalizeVariant(item, name, cleanSlug)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// VariantUpdateInput 是更新入参；nil 字段保留原值（Python kwargs=None 语义）。
type VariantUpdateInput struct {
	Slug         *string
	DisplayName  *string
	Description  *string
	ImagePath    *string
	Status       *string
	Metadata     map[string]any
	MetadataSet  bool
	ImageAssetID *string
}

// UpdateVariant 更新变体，保留调用方未提供的字段；slug 变更时迁移存储 key。
func (s *CharacterVariants) UpdateVariant(project, name, identifier string, in VariantUpdateInput) (map[string]any, error) {
	var cleanSlug *string
	if in.Slug != nil {
		resolved, err := domain.ValidateAssetName(*in.Slug)
		if err != nil {
			return nil, ErrVariantInvalidSlug
		}
		cleanSlug = &resolved
	}
	cleanImage, err := s.validateVariantImagePath(project, in.ImagePath)
	if err != nil {
		return nil, err
	}
	cleanStatus, err := validateVariantStatus(in.Status)
	if err != nil {
		return nil, err
	}
	var updated map[string]any
	err = s.store.Update(project, func(pf *domain.ProjectFile) error {
		character, _, err := locateCharacter(pf, name)
		if err != nil {
			return err
		}
		rawVariants, _ := character.Fields()["variants"].(map[string]any)
		if rawVariants == nil {
			return ErrVariantNotFound
		}
		// 直接在底层 map 上定位：NormalizeVariant 会生成副本，改副本不落库。
		raw, rawKey, found := findRawVariant(rawVariants, identifier)
		if !found {
			return ErrVariantNotFound
		}
		targetSlug, _ := raw["slug"].(string)
		if cleanSlug != nil {
			targetSlug = *cleanSlug
		}
		// 目标 slug 被另一条变体占用 → 409；自己占用的 key 允许原地写。
		if existingKey, conflict := rawVariantsResolveKey(rawVariants, targetSlug); conflict && existingKey != rawKey {
			return &CharacterVariantSlugExistsError{Slug: targetSlug}
		}
		if in.DisplayName != nil {
			raw["display_name"] = *in.DisplayName
		}
		if in.Description != nil {
			raw["description"] = *in.Description
		}
		if cleanImage != nil {
			raw["image_path"] = *cleanImage
		}
		if cleanStatus != nil {
			raw["status"] = *cleanStatus
		}
		if in.MetadataSet {
			if in.Metadata == nil {
				raw["metadata"] = map[string]any{}
			} else {
				raw["metadata"] = in.Metadata
			}
		}
		if in.ImageAssetID != nil {
			if *in.ImageAssetID == "" {
				delete(raw, "image_asset_id")
			} else {
				raw["image_asset_id"] = *in.ImageAssetID
			}
		}
		raw["character_id"] = name
		raw["slug"] = targetSlug
		// 先写新 key 再删旧 key（key 可能是 NFD 形态）：删除必须后置，
		// 否则 slug 未变时会把条目从 variants 里整个抹掉。
		rawVariants[targetSlug] = raw
		if rawKey != targetSlug {
			delete(rawVariants, rawKey)
		}
		updated = domain.NormalizeVariant(raw, name, targetSlug)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// DeleteVariant 按 id 或 slug 删除，返回被删条目（供响应回显）。
func (s *CharacterVariants) DeleteVariant(project, name, identifier string) (map[string]any, error) {
	var deleted map[string]any
	err := s.store.Update(project, func(pf *domain.ProjectFile) error {
		character, _, err := locateCharacter(pf, name)
		if err != nil {
			return err
		}
		rawVariants, _ := character.Fields()["variants"].(map[string]any)
		if rawVariants == nil {
			return ErrVariantNotFound
		}
		raw, rawKey, found := findRawVariant(rawVariants, identifier)
		if !found {
			return ErrVariantNotFound
		}
		deleted = domain.NormalizeVariant(raw, name, rawKey)
		if deleted == nil {
			deleted = map[string]any{}
		}
		delete(rawVariants, rawKey)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return deleted, nil
}

// --- 头像上传 ---

// UploadCharacterAvatar 归一 PNG 并在项目锁内替换头像文件与 project.json 指针。
//
// 语义对齐 Python replace_project_character_avatar：旧文件先备份；临时文件落在头像
// 目标目录（os.replace 同盘原子性）；闭包内「指针写入 + 文件替换」一体完成，
// 文件替换失败则指针不落库；json 写回失败时恢复备份文件。
// 注：Python 侧此前用 OpenCV 做人脸裁切（character_avatar.py），现行为已是
// 「整图归一直用」；Go 侧同样落整图，人脸检测属遗留留白。
func (s *CharacterVariants) UploadCharacterAvatar(project, name string, content []byte) (string, error) {
	safeName, err := domain.ValidateAssetName(name)
	if err != nil {
		return "", domain.ErrInvalidAssetName
	}
	normalized, err := img.NormalizeToPNG(content, nil)
	if err != nil {
		if errors.Is(err, img.ErrPixelLimit) {
			return "", &CharacterAvatarTooLargeError{MaxMegapixels: 30}
		}
		// NormalizeToPNG 除像素超限外的失败都是「不是有效图片」。
		return "", ErrAvatarInvalidFile
	}
	relativePath := "characters/" + safeName + "_avatar_manual.png"
	projectDir, err := s.store.Dir(project)
	if err != nil {
		return "", ErrAssetNotFound
	}
	target, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(relativePath))
	if err != nil {
		return "", ErrVariantInvalidPath
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}

	// 临时文件必须与目标同目录：跨盘 rename 会退化为拷贝，失去原子性。
	tmp, err := os.CreateTemp(filepath.Dir(target), ".avatar-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(normalized); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	// 旧文件备份：替换成功后删除；json 写回失败时恢复。
	backup := ""
	if info, statErr := os.Stat(target); statErr == nil && info.Mode().IsRegular() {
		backup = target + ".bak-prev"
		if copyErr := copyAvatarBytes(target, backup); copyErr != nil {
			backup = ""
		}
	}

	var oldPointer string
	hadPointer := false
	err = s.store.Update(project, func(pf *domain.ProjectFile) error {
		character, _, err := locateCharacter(pf, name)
		if err != nil {
			return err
		}
		if previous, ok := character.String("character_avatar"); ok {
			oldPointer, hadPointer = previous, true
		}
		character.SetString("character_avatar", relativePath)
		if err := os.Rename(tmpName, target); err != nil {
			// 指针回滚：文件没换成功，指针也不能留。
			if hadPointer {
				character.SetString("character_avatar", oldPointer)
			} else {
				delete(character.Fields(), "character_avatar")
			}
			return err
		}
		return nil
	})
	if err != nil {
		// 闭包已成功但 json 写回失败：文件已替换而指针未持久化——恢复备份保持一致。
		if backup != "" {
			_ = os.Rename(backup, target)
		} else if _, statErr := os.Stat(target); statErr == nil {
			// 无备份说明此前无旧文件；删除新文件。
			_ = os.Remove(target)
		}
		return "", err
	}
	if backup != "" {
		_ = os.Remove(backup)
	}
	return relativePath, nil
}

// copyAvatarBytes 复制文件内容。
func copyAvatarBytes(source, target string) error {
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, content, 0o644)
}

// --- 内部助手 ---

// locateCharacter 在项目文件里定位角色条目；桶未初始化时先初始化（写路径前置条件）。
func locateCharacter(pf *domain.ProjectFile, name string) (*domain.AssetEntry, string, error) {
	bucket, ok := pf.BucketRef(domain.AssetCharacter)
	if !ok {
		return nil, "", ErrAssetNotFound
	}
	if *bucket == nil {
		*bucket = domain.AssetBucket{}
	}
	key, ok := (*bucket).ResolveKey(name)
	if !ok {
		return nil, "", ErrAssetNotFound
	}
	if (*bucket)[key] == nil {
		return nil, "", ErrAssetNotFound
	}
	return (*bucket)[key], key, nil
}

// findRawVariant 在 variants 底层字典里按 id 或 slug 找原始条目（返回引用，可直接改）。
func findRawVariant(rawVariants map[string]any, identifier string) (map[string]any, string, bool) {
	target := domain.NormalizeAssetName(identifier)
	for key, raw := range rawVariants {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		id, _ := item["id"].(string)
		if id == identifier {
			return item, key, true
		}
		slug, _ := item["slug"].(string)
		if slug != "" && domain.NormalizeAssetName(slug) == target {
			return item, key, true
		}
		// key 本身就是 slug（写入侧恒落 slug key）：按 key 兜底匹配 NFD 存量。
		if slug == "" && domain.NormalizeAssetName(key) == target {
			return item, key, true
		}
	}
	return nil, "", false
}

// rawVariantsResolveKey 是 variants 子字典的 ResolveKey（NFC 坐标系）。
func rawVariantsResolveKey(variants map[string]any, slug string) (string, bool) {
	target := domain.NormalizeAssetName(slug)
	for key := range variants {
		if domain.NormalizeAssetName(key) == target {
			return key, true
		}
	}
	return "", false
}

// characterEntry 读取角色条目（只读路径）。
func (s *CharacterVariants) characterEntry(project, name string) (*domain.AssetEntry, string, error) {
	pf, err := s.store.Get(project)
	if err != nil {
		return nil, "", ErrAssetNotFound
	}
	bucket, ok := pf.BucketRef(domain.AssetCharacter)
	if !ok {
		return nil, "", ErrAssetNotFound
	}
	key, ok := (*bucket).ResolveKey(name)
	if !ok || (*bucket)[key] == nil {
		return nil, "", ErrAssetNotFound
	}
	return (*bucket)[key], key, nil
}

// validateVariantImagePath 校验项目相对路径（不要求文件已存在），非法返回 422。
// 与 Python _validate_variant_image_path 一致：空串合法（无图）。
func (s *CharacterVariants) validateVariantImagePath(project string, image *string) (*string, error) {
	if image == nil {
		return nil, nil
	}
	if strings.TrimSpace(*image) == "" {
		clean := ""
		return &clean, nil
	}
	dir, err := s.store.Dir(project)
	if err != nil {
		return nil, ErrAssetNotFound
	}
	if _, err := fs.SafeJoin(dir, false, filepath.FromSlash(*image)); err != nil {
		return nil, ErrVariantInvalidPath
	}
	clean := strings.ReplaceAll(*image, "\\", "/")
	return &clean, nil
}

// variantStatusValue / variantImagePathValue 把可选校验结果落到 map（存值不存指针——
// 指针塞进 map 会在后续 NormalizeVariant 的类型断言处静默回落默认值）。
func variantStatusValue(value *string) string {
	if value == nil {
		return "draft"
	}
	return *value
}

func variantImagePathValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// validateVariantStatus 校验状态枚举；nil 表示未提供。
func validateVariantStatus(status *string) (*string, error) {
	if status == nil {
		return nil, nil
	}
	if !domain.VariantStatuses[*status] {
		return nil, ErrVariantInvalidStatus
	}
	return status, nil
}
