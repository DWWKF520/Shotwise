// 全局资产库（跨项目复用的 character / scene / prop）服务。
//
// DB 与磁盘的一致性约定与 Python 侧一致:先落盘再写 DB;任何失败路径清理已落盘
// 文件不留孤儿;覆盖写时旧文件在 DB 写入成功后才删除。
package service

import (
	"context"
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
	"gorm.io/gorm"
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

// FromProjectInput 是「项目资产入库」的入参。
type FromProjectInput struct {
	ProjectName  string
	ResourceType string
	ResourceID   string
	OverrideName *string
	Overwrite    bool
}

// GlobalAssetConflictError 是同类型同名已存在（带既有资产供前端展示）。
type GlobalAssetConflictError struct {
	Name    string
	Existing *SerializedGlobalAsset
}

func (e *GlobalAssetConflictError) Error() string { return "asset already exists: " + e.Name }

// globalAssetBucketKey 把全局库类型映射到 project.json 的桶键。
func globalAssetBucketKey(assetType string) (domain.AssetKind, bool) {
	switch assetType {
	case "character":
		return domain.AssetCharacter, true
	case "scene":
		return domain.AssetScene, true
	case "prop":
		return domain.AssetProp, true
	}
	return "", false
}

// FromProject 把项目内的角色/场景/道具入库到全局库。
//
// 三层安全与降级口径：
//   - sheet 相对路径经 SafeJoin 校验，非法或缺失静默降级为「无源图」；
//   - reference_audio 额外要求父目录命中 characters/refs_audio（字段可被 PATCH 成
//     项目内任意字符串，不加这层会把 project.json 当音频复制进全局库）；
//   - 文件拷贝与 DB 写入共用一个失败边界：任一失败清理已落盘文件，不留孤儿。
func (s *GlobalAssetService) FromProject(ctx context.Context, in FromProjectInput) (*SerializedGlobalAsset, error) {
	kind, ok := globalAssetBucketKey(in.ResourceType)
	if !ok {
		return nil, ErrGlobalInvalidType
	}
	pf, err := s.store.Get(in.ProjectName)
	if err != nil {
		return nil, ErrGlobalTargetProjectNotFound
	}
	projectDir, err := s.store.Dir(in.ProjectName)
	if err != nil {
		return nil, err
	}
	bucket, ok := pf.BucketRef(kind)
	if !ok || bucket == nil {
		return nil, &GlobalSourceMissingError{Project: in.ProjectName, Resource: in.ResourceID}
	}
	key, ok := bucket.ResolveKey(in.ResourceID)
	if !ok || (*bucket)[key] == nil {
		return nil, &GlobalSourceMissingError{Project: in.ProjectName, Resource: in.ResourceID}
	}
	entry := (*bucket)[key]

	assetName := in.ResourceID
	if in.OverrideName != nil && strings.TrimSpace(*in.OverrideName) != "" {
		assetName = strings.TrimSpace(*in.OverrideName)
	}
	description := entry.MustString("description")
	voiceStyle := ""
	if in.ResourceType == "character" {
		voiceStyle = entry.MustString("voice_style")
	}
	spec, _ := domain.Spec(kind)

	// 源图：sheet 相对路径存在才拷贝。
	sourceSheet := ""
	if sheetRel := entry.MustString(spec.SheetKey); sheetRel != "" {
		if resolved, joinErr := fs.SafeJoin(projectDir, false, filepath.FromSlash(sheetRel)); joinErr == nil {
			if info, statErr := os.Stat(resolved); statErr == nil && info.Mode().IsRegular() {
				sourceSheet = resolved
			}
		}
	}
	// 源音频：只有 character 有意义，且必须落在 refs_audio 内。
	sourceAudio := ""
	if in.ResourceType == "character" {
		if audioRel := entry.MustString("reference_audio"); audioRel != "" {
			refsAudioDir := filepath.Join(projectDir, "characters", "refs_audio")
			if resolved, joinErr := fs.SafeJoin(projectDir, false, filepath.FromSlash(audioRel)); joinErr == nil {
				if info, statErr := os.Stat(resolved); statErr == nil && info.Mode().IsRegular() &&
					filepath.Clean(filepath.Dir(resolved)) == filepath.Clean(refsAudioDir) {
					sourceAudio = resolved
				}
			}
		}
	}

	existing, existingErr := s.repo.GetByTypeName(in.ResourceType, assetName)
	if existingErr != nil && !errors.Is(existingErr, gorm.ErrRecordNotFound) {
		return nil, existingErr
	}
	if existing != nil && !in.Overwrite {
		view := serializeGlobalAsset(existing)
		return nil, &GlobalAssetConflictError{Name: assetName, Existing: &view}
	}

	root, err := s.globalAssetsDir(in.ResourceType)
	if err != nil {
		return nil, err
	}
	copyInto := func(source, fallbackExt string) (string, error) {
		ext := strings.ToLower(filepath.Ext(source))
		if ext == "" {
			ext = fallbackExt
		}
		target := filepath.Join(root, uuid.NewString()+ext)
		if err := copyFileValue(source, target); err != nil {
			return "", err
		}
		rel, relErr := filepath.Rel(s.store.Root(), target)
		if relErr != nil {
			return "", err
		}
		return filepath.ToSlash(rel), nil
	}
	var newImage, newAudio string
	if sourceSheet != "" {
		if newImage, err = copyInto(sourceSheet, ".png"); err != nil {
			return nil, err
		}
	}
	if sourceAudio != "" {
		if newAudio, err = copyInto(sourceAudio, ".wav"); err != nil {
			if newImage != "" {
				s.deleteGlobalAssetFile(newImage)
			}
			return nil, err
		}
	}

	if existing != nil {
		oldImage := ""
		if existing.ImagePath != nil && *existing.ImagePath != newImage {
			oldImage = *existing.ImagePath
		}
		oldAudio := ""
		if existing.AudioPath != nil && *existing.AudioPath != newAudio {
			oldAudio = *existing.AudioPath
		}
		fields := map[string]any{
			"description":    description,
			"voice_style":    voiceStyle,
			"source_project": in.ProjectName,
		}
		if newImage != "" {
			fields["image_path"] = newImage
		}
		if newAudio != "" {
			fields["audio_path"] = newAudio
		}
		updated, updateErr := s.repo.Update(existing.ID, fields)
		if updateErr != nil {
			if newImage != "" {
				s.deleteGlobalAssetFile(newImage)
			}
			if newAudio != "" {
				s.deleteGlobalAssetFile(newAudio)
			}
			return nil, updateErr
		}
		// 覆盖写：旧文件在 DB 写入成功后才删，回滚时旧文件保留。
		if oldImage != "" {
			s.deleteGlobalAssetFile(oldImage)
		}
		if oldAudio != "" {
			s.deleteGlobalAssetFile(oldAudio)
		}
		out := serializeGlobalAsset(updated)
		return &out, nil
	}

	created, createdOK, createErr := s.repo.Create(repository.CreateParams{
		Type:          in.ResourceType,
		Name:          assetName,
		Description:   description,
		VoiceStyle:    voiceStyle,
		ImagePath:     stringPtrOrNil(newImage),
		AudioPath:     stringPtrOrNil(newAudio),
		SourceProject: &in.ProjectName,
	})
	if createErr != nil {
		if newImage != "" {
			s.deleteGlobalAssetFile(newImage)
		}
		if newAudio != "" {
			s.deleteGlobalAssetFile(newAudio)
		}
		return nil, createErr
	}
	if !createdOK {
		if newImage != "" {
			s.deleteGlobalAssetFile(newImage)
		}
		if newAudio != "" {
			s.deleteGlobalAssetFile(newAudio)
		}
		return nil, &GlobalAssetExistsError{Name: assetName}
	}
	out := serializeGlobalAsset(created)
	return &out, nil
}

// GlobalSourceMissingError 表示项目内未找到指定资源。
type GlobalSourceMissingError struct {
	Project  string
	Resource string
}

func (e *GlobalSourceMissingError) Error() string { return "source resource not found" }

// ErrGlobalTargetProjectNotFound 表示目标项目不存在。
var ErrGlobalTargetProjectNotFound = fmt.Errorf("target project not found")

// stringPtrOrNil 空串转 nil（可空列语义）。
func stringPtrOrNil(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

// copyFileValue 复制文件（保证目标目录存在）。
func copyFileValue(source, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, content, 0o644)
}

// ApplyToProjectInput 是「全局资产应用到项目」的入参。
type ApplyToProjectInput struct {
	AssetIDs       []string
	TargetProject  string
	ConflictPolicy string // skip | overwrite | rename
}

// ApplyToProjectResult 是批量应用的三桶结果。
type ApplyToProjectResult struct {
	Succeeded []map[string]any `json:"succeeded"`
	Skipped   []map[string]any `json:"skipped"`
	Failed    []map[string]any `json:"failed"`
}

// ApplyToProject 把全局资产批量写入目标项目的对应桶。
//
// 冲突三策略：skip 跳过 / overwrite 保留原名覆盖存量（key 按 NFC 坐标系解析，可能落在
// NFD 存量 key 上）/ rename 以「名字 (2)」起名。文件拷贝先全部完成后才一次性写回
// project.json，避免半批资产。
func (s *GlobalAssetService) ApplyToProject(ctx context.Context, in ApplyToProjectInput) (ApplyToProjectResult, error) {
	result := ApplyToProjectResult{
		Succeeded: []map[string]any{}, Skipped: []map[string]any{}, Failed: []map[string]any{},
	}
	switch in.ConflictPolicy {
	case "skip", "overwrite", "rename":
	default:
		return result, &ValueError{Msg: "asset_invalid_conflict_policy"}
	}
	pf, err := s.store.Get(in.TargetProject)
	if err != nil {
		return result, ErrGlobalTargetProjectNotFound
	}
	projectDir, err := s.store.Dir(in.TargetProject)
	if err != nil {
		return result, err
	}
	assets, err := s.repo.GetByIDs(in.AssetIDs)
	if err != nil {
		return result, err
	}
	byID := map[string]*repository.Asset{}
	for i := range assets {
		byID[assets[i].ID] = &assets[i]
	}
	for _, id := range in.AssetIDs {
		if _, ok := byID[id]; !ok {
			result.Failed = append(result.Failed, map[string]any{"id": id, "reason": "not_found"})
		}
	}

	// 按桶维护已占用名字集合（NFC 坐标系），供冲突判定与 rename 累积检查。
	occupied := map[string]map[string]bool{}
	for kind := range map[domain.AssetKind]bool{domain.AssetCharacter: true, domain.AssetScene: true, domain.AssetProp: true} {
		spec, ok := domain.Spec(kind)
		if !ok {
			continue
		}
		names := map[string]bool{}
		if bucket, ok := pf.BucketRef(kind); ok && bucket != nil {
			for key := range *bucket {
				names[domain.NormalizeAssetName(key)] = true
			}
		}
		occupied[spec.BucketKey] = names
	}

	type applyPlan struct {
		asset      *repository.Asset
		kind       domain.AssetKind
		bucketKey  string
		sheetKey   string
		name       string
		targetSheet string
		srcSheet   string
		targetAudio string
		srcAudio   string
	}
	var plans []applyPlan
	for _, assetID := range in.AssetIDs {
		asset := byID[assetID]
		if asset == nil {
			continue
		}
		kind, ok := globalAssetBucketKey(asset.Type)
		if !ok {
			result.Failed = append(result.Failed, map[string]any{"id": asset.ID, "reason": "invalid_type"})
			continue
		}
		spec, _ := domain.Spec(kind)
		names := occupied[spec.BucketKey]
		desired := domain.NormalizeAssetName(asset.Name)
		if desired == "" {
			result.Failed = append(result.Failed, map[string]any{"id": asset.ID, "reason": "invalid_name"})
			continue
		}
		if names[desired] {
			switch in.ConflictPolicy {
			case "skip":
				result.Skipped = append(result.Skipped, map[string]any{"id": asset.ID, "name": asset.Name})
				continue
			case "rename":
				desired = fmt.Sprintf("%s (%d)", desired, nextRenameSuffix(names, desired))
			}
		}

		plan := applyPlan{asset: asset, kind: kind, bucketKey: spec.BucketKey, sheetKey: spec.SheetKey, name: desired}
		if asset.ImagePath != nil && *asset.ImagePath != "" {
			src := filepath.Join(s.store.Root(), filepath.FromSlash(*asset.ImagePath))
			if info, statErr := os.Stat(src); statErr != nil || !info.Mode().IsRegular() {
				result.Failed = append(result.Failed, map[string]any{"id": asset.ID, "reason": "image_missing"})
				continue
			}
			ext := strings.ToLower(filepath.Ext(src))
			if ext == "" {
				ext = ".png"
			}
			rel := spec.BucketKey + "/" + desired + ext
			if _, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(rel)); err != nil {
				result.Failed = append(result.Failed, map[string]any{"id": asset.ID, "reason": "invalid_name"})
				continue
			}
			plan.targetSheet = rel
			plan.srcSheet = src
		}
		if asset.Type == "character" && asset.AudioPath != nil && *asset.AudioPath != "" {
			src := filepath.Join(s.store.Root(), filepath.FromSlash(*asset.AudioPath))
			if info, statErr := os.Stat(src); statErr != nil || !info.Mode().IsRegular() {
				result.Failed = append(result.Failed, map[string]any{"id": asset.ID, "reason": "audio_missing"})
				continue
			}
			ext := strings.ToLower(filepath.Ext(src))
			if ext == "" {
				ext = ".wav"
			}
			rel := "characters/refs_audio/" + desired + ext
			if _, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(rel)); err != nil {
				result.Failed = append(result.Failed, map[string]any{"id": asset.ID, "reason": "invalid_name"})
				continue
			}
			plan.targetAudio = rel
			plan.srcAudio = src
		}
		names[desired] = true
		plans = append(plans, plan)
	}

	// 先全部拷贝文件，再一次性写回 project.json。
	for _, plan := range plans {
		for src, dst := range map[string]string{plan.srcSheet: plan.targetSheet, plan.srcAudio: plan.targetAudio} {
			if src == "" || dst == "" {
				continue
			}
			if err := copyFileValue(src, filepath.Join(projectDir, filepath.FromSlash(dst))); err != nil {
				return result, err
			}
		}
	}
	if len(plans) > 0 {
		if err := s.store.Update(in.TargetProject, func(project *domain.ProjectFile) error {
			for _, plan := range plans {
				bucket, ok := project.BucketRef(plan.kind)
				if !ok {
					continue
				}
				// 桶可能尚未初始化（目标项目没有该类型条目）：先初始化再写，
				// 否则向 nil map 赋值 panic。
				if *bucket == nil {
					*bucket = domain.AssetBucket{}
				}
				payload := map[string]any{"description": plan.asset.Description}
				if plan.asset.Type == "character" {
					payload["voice_style"] = plan.asset.VoiceStyle
					if plan.targetAudio != "" {
						payload["reference_audio"] = plan.targetAudio
						// 资产即开关：导入即等效「设置了这个声音」，过渡横幅计数须能感知。
						payload["voice_updated_at"] = nowISO()
					}
				}
				if plan.targetSheet != "" {
					payload[plan.sheetKey] = plan.targetSheet
				}
				// overwrite 策略要落在存量真实 key 上（可能是 NFD），否则会并存两条同名条目。
				key, ok := bucket.ResolveKey(plan.name)
				if !ok {
					key = plan.name
				}
				(*bucket)[key] = domain.WrapAssetEntry(payload)
			}
			return nil
		}); err != nil {
			return result, err
		}
	}
	for _, plan := range plans {
		result.Succeeded = append(result.Succeeded, map[string]any{"id": plan.asset.ID, "name": plan.name})
	}
	return result, nil
}

// nextRenameSuffix 找 rename 策略可用的最小后缀。
func nextRenameSuffix(names map[string]bool, base string) int {
	for i := 2; ; i++ {
		if !names[fmt.Sprintf("%s (%d)", base, i)] {
			return i
		}
	}
}
