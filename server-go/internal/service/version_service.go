// 版本查询 / 还原的应用服务（对应 server/routers/versions.py）。
//
// 还原是一条联合图 / 分镜 / 视频的写入路径：除了把文件换回历史版本，还要把引用它的
// 元数据（project.json 资产表、剧本 generated_assets、宫格记录）同步到同一路径，否则
// 前端拿到的仍是旧指针。同步是 best-effort：主文件已还原成功，兄弟集的临时 IO 失败
// 不该把整个还原操作打成 5xx。
package service

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/shotwise/shotwise/internal/domain/project"
	domainscript "github.com/shotwise/shotwise/internal/domain/script"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// 还原准入与参数错误码。
var (
	// ErrAdGridNotSupported 表示广告 / 短片项目不开放宫格分镜，残留的 grid 记录不可再改写。
	ErrAdGridNotSupported = errors.New("ad_grid_not_supported")
	// ErrGridStoryboardNotEnabled 表示宫格开关关闭（含 reference_video 路线），历史 grid 不可还原。
	ErrGridStoryboardNotEnabled = errors.New("grid_storyboard_not_enabled")
	// ErrInvalidResourceID 表示 resource_id 拼出的路径逃出了项目目录。
	ErrInvalidResourceID = errors.New("invalid_resource_id")

	// ErrResourceMissingInScript 是「该集剧本不引用此资源」的哨兵：跨集同步属正常跳过。
	ErrResourceMissingInScript = errors.New("resource_not_in_script")
)

// restorableResourceTypes 是经 API 面可还原的资源类型。路径形状委托 resourcePatterns，
// 本服务仅放行有还原后元数据同步分支的这几类。grids 的还原只换回联合图文件并复位宫格
// 记录的切分态，不触发切分、不碰任何分镜图——落格由宫格切分端点显式执行。
var restorableResourceTypes = map[string]bool{
	"storyboards": true, "videos": true, "characters": true, "scenes": true,
	"props": true, "products": true, "reference_videos": true, "grids": true,
}

// resourceToAssetKind 是 resource_type（复数）→ 资产类型（单数）的映射。
var resourceToAssetKind = map[string]project.AssetKind{
	"characters": project.AssetCharacter,
	"scenes":     project.AssetScene,
	"props":      project.AssetProp,
	"products":   project.AssetProduct,
}

// VersionService 是版本查询 / 还原的应用服务。
type VersionService struct {
	store *ProjectStore
}

// NewVersionService 构造。
func NewVersionService(store *ProjectStore) *VersionService { return &VersionService{store: store} }

// List 返回资源的所有版本列表（含 resource_type / resource_id 回显）。
func (s *VersionService) List(projectName, resourceType, resourceID string) (map[string]any, error) {
	dir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	info, err := NewVersions(dir).GetVersions(resourceType, resourceID)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"resource_type": resourceType, "resource_id": resourceID}
	for k, v := range info {
		out[k] = v
	}
	return out, nil
}

// Restore 把资源切回指定版本，并同步引用它的元数据。
func (s *VersionService) Restore(
	projectName, resourceType, resourceID string, version int,
) (map[string]any, error) {
	if !restorableResourceTypes[resourceType] {
		return nil, ErrUnsupportedResourceType
	}
	if resourceType == "grids" {
		// 还原同样是一条联合图写入路径（换回历史联合图 + 复位宫格记录），与重生成 /
		// 切分 / 上传共用准入判定；漏掉这里，被封禁项目就能从还原绕过。
		pf, err := s.store.Get(projectName)
		if err != nil {
			return nil, err
		}
		if pf.ContentMode == string(project.ContentAd) {
			return nil, ErrAdGridNotSupported
		}
		if pf.GenerationMode != string(project.GenerationStoryboard) || !pf.GridStoryboard {
			return nil, ErrGridStoryboardNotEnabled
		}
	}
	dir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	relative, err := ResourceRelativePath(resourceType, resourceID)
	if err != nil {
		return nil, ErrUnsupportedResourceType
	}
	// 路径遍历防护：resource_id 拼出的绝对路径不得逃出项目目录。
	currentFile, err := fs.SafeJoin(dir, false, filepath.FromSlash(relative))
	if err != nil {
		return nil, ErrInvalidResourceID
	}
	versions := NewVersions(dir)
	result, err := versions.RestoreVersion(resourceType, resourceID, version, currentFile)
	if err != nil {
		return nil, err
	}

	// 还原参考视频时把该版本的原始入库时间一并写回，避免存量声音判定被还原动作洗新；
	// 来源签名同理取该版档案，读时 stale 判定按还原回来的旧产物基准成立。
	var restoredCreatedAt, restoredSignature string
	if resourceType == "reference_videos" {
		if restoredCreatedAt, err = versions.GetVersionCreatedAt(resourceType, resourceID, version); err != nil {
			return nil, err
		}
		raw, err := versions.GetVersionMetadata(resourceType, resourceID, version, "source_signature")
		if err != nil {
			return nil, err
		}
		if text, ok := raw.(string); ok {
			restoredSignature = text
		}
	}
	s.syncMetadata(resourceType, projectName, resourceID, relative, dir, restoredCreatedAt, restoredSignature)

	// 计算还原后文件的 fingerprint；视频 / 参考视频还原时同步删除缩略图（内容已失效）。
	fingerprints := map[string]int64{}
	if info, err := os.Stat(currentFile); err == nil {
		fingerprints[relative] = info.ModTime().UnixNano()
	}
	switch resourceType {
	case "videos":
		key := "thumbnails/scene_" + resourceID + ".jpg"
		_ = os.Remove(filepath.Join(dir, filepath.FromSlash(key)))
		// fingerprint=0 通知前端该文件已失效（poster 消失直到重新生成）。
		fingerprints[key] = 0
	case "reference_videos":
		key := "reference_videos/thumbnails/" + resourceID + ".jpg"
		_ = os.Remove(filepath.Join(dir, filepath.FromSlash(key)))
		fingerprints[key] = 0
	}

	return map[string]any{
		"success":            true,
		"restored_version":   result.RestoredVersion,
		"current_version":    result.CurrentVersion,
		"prompt":             result.Prompt,
		"file_path":          relative,
		"asset_fingerprints": fingerprints,
	}, nil
}

// syncMetadata 让引用该资源的元数据指向还原后的统一文件路径。
func (s *VersionService) syncMetadata(
	resourceType, projectName, resourceID, relative, projectDir, restoredCreatedAt, restoredSignature string,
) {
	if kind, ok := resourceToAssetKind[resourceType]; ok {
		s.updateAssetSheet(kind, projectName, resourceID, relative)
		return
	}
	switch resourceType {
	case "storyboards":
		s.syncStoryboardMetadata(projectName, resourceID, relative)
	case "videos":
		s.syncVideoMetadata(projectName, resourceID, relative)
	case "reference_videos":
		s.syncReferenceVideoMetadata(projectName, resourceID, restoredCreatedAt, restoredSignature)
	case "grids":
		manager, err := NewGridManager(projectDir)
		if err != nil {
			return
		}
		grid, found, err := manager.Get(resourceID)
		if err != nil || !found || grid == nil {
			return
		}
		// 内容已换回历史版本，旧的落格结果不再对应当前图；记录缺失 / 损坏时跳过，
		// 联合图文件已还原成功，记录属 best-effort。
		grid.MarkCompositeReplaced()
		if err := manager.Save(grid); err != nil {
			slog.Warn("还原后复位宫格记录失败", "project", projectName, "grid_id", resourceID, "err", err)
		}
	}
}

// updateAssetSheet 更新资产表里的定妆图 / 设计稿字段路径。资产条目已被删除时静默跳过。
func (s *VersionService) updateAssetSheet(kind project.AssetKind, projectName, name, sheetPath string) {
	spec, ok := project.Spec(kind)
	if !ok {
		return
	}
	err := s.store.Update(projectName, func(pf *project.ProjectFile) error {
		bucket, ok := pf.BucketRef(kind)
		if !ok || bucket == nil {
			return ErrResourceMissingInScript
		}
		key, ok := bucket.ResolveKey(name)
		if !ok {
			return ErrResourceMissingInScript
		}
		entry := (*bucket)[key]
		if entry == nil {
			return ErrResourceMissingInScript
		}
		entry.SetString(spec.SheetKey, sheetPath)
		return nil
	})
	if err != nil && !errors.Is(err, ErrResourceMissingInScript) {
		slog.Warn("还原后同步资产表字段失败", "project", projectName, "asset", name, "err", err)
	}
}

// syncStoryboardMetadata 把每集剧本里该镜头的 storyboard_image 指回还原后的文件。
func (s *VersionService) syncStoryboardMetadata(projectName, sceneID, relative string) {
	s.syncScriptsBestEffort(projectName, func(script map[string]any) error {
		items, idField, _, err := domainscript.ResolveScriptItems(script)
		if err != nil {
			return err
		}
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if fmt.Sprint(entry[idField]) != sceneID {
				continue
			}
			s.setGeneratedAssets(entry, map[string]any{"storyboard_image": relative}, nil)
			return nil
		}
		return ErrResourceMissingInScript
	})
}

// syncVideoMetadata 还原镜头视频后同步 generated_assets：旧 provider URI / 缩略图与
// 历史本地文件不再对应，一并清空（缩略图文件本身由还原端点删除）。
func (s *VersionService) syncVideoMetadata(projectName, sceneID, relative string) {
	s.syncScriptsBestEffort(projectName, func(script map[string]any) error {
		items, idField, _, err := domainscript.ResolveScriptItems(script)
		if err != nil {
			return err
		}
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if fmt.Sprint(entry[idField]) != sceneID {
				continue
			}
			s.setGeneratedAssets(entry,
				map[string]any{"video_clip": relative},
				[]string{"video_uri", "video_thumbnail"})
			return nil
		}
		return ErrResourceMissingInScript
	})
}

// syncReferenceVideoMetadata 还原参考视频单元后同步 unit.generated_assets（写回口径与
// 生成 finalize 共用）。
func (s *VersionService) syncReferenceVideoMetadata(
	projectName, unitID, generatedAt, signature string,
) {
	s.syncScriptsBestEffort(projectName, func(script map[string]any) error {
		// 该集脚本不含此 unit 时 applyUnitVideoAssets 返回错误，按正常跳过处理。
		if _, err := ApplyUnitVideoAssetsHook(script, unitID, "", "", generatedAt, SignatureValue, signature); err != nil {
			return ErrResourceMissingInScript
		}
		return nil
	})
}

// setGeneratedAssets 写入 / 删除 generated_assets 上的键。
func (s *VersionService) setGeneratedAssets(entry map[string]any, values map[string]any, remove []string) {
	assets, ok := entry["generated_assets"].(map[string]any)
	if !ok {
		assets = map[string]any{}
		entry["generated_assets"] = assets
	}
	for k, v := range values {
		assets[k] = v
	}
	for _, key := range remove {
		delete(assets, key)
	}
}

// syncScriptsBestEffort 对项目内每集剧本执行 apply，逐集降级而非整体失败。
//
// 该集不引用此资源时跳过同步是正常情况；脏脚本 / 单集临时 IO 失败降级为 warning——
// 跨集同步是 best-effort housekeeping，主集恢复在调用本函数前已成功。
func (s *VersionService) syncScriptsBestEffort(projectName string, apply func(script map[string]any) error) {
	scriptsDir, err := s.store.ScriptsDir(projectName)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(scriptsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		err := s.store.UpdateScript(projectName, entry.Name(), apply)
		if err == nil || errors.Is(err, ErrResourceMissingInScript) {
			continue
		}
		slog.Warn("还原后跨集同步元数据跳过", "project", projectName, "script", entry.Name(), "err", err)
	}
}
