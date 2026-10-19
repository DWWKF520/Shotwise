// 版本查询与还原（对应 lib/version_manager.py 的读侧：get_versions /
// restore_version / get_version_created_at / get_version_metadata）。
//
// 与 versions.go 的写侧共用同一份 versions.json 与项目级锁；文件名、记录形状与
// file_url 口径必须与 Python 侧逐字对齐——前端版本面板按同一形状读两个服务共写的目录。
package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shotwise/shotwise/internal/infra/fs"
)

// ErrUnsupportedResourceType 表示资源类型不在可查询 / 可还原的白名单内。
var ErrUnsupportedResourceType = errors.New("unsupported_resource_type")

// VersionResourceNotFoundError 表示该资源在 versions.json 里没有任何版本记录。
type VersionResourceNotFoundError struct {
	ResourceType string
	ResourceID   string
}

func (e *VersionResourceNotFoundError) Error() string {
	return fmt.Sprintf("version resource not found: %s/%s", e.ResourceType, e.ResourceID)
}

// VersionNotFoundError 表示资源存在但指定版本号不存在。
type VersionNotFoundError struct{ Version int }

func (e *VersionNotFoundError) Error() string { return fmt.Sprintf("version not found: %d", e.Version) }

// RestoreResult 是还原操作的返回契约。
type RestoreResult struct {
	RestoredVersion int
	CurrentVersion  int
	Prompt          string
}

// GetVersions 返回资源的所有版本列表，含 is_current 与 file_url。
//
// versions.json 可被篡改：file 越界（逃出项目目录）或非法的记录直接跳过，不为它生成
// 指向项目外的 URL。资源类型不在白名单时返回 ErrUnsupportedResourceType。
func (v *Versions) GetVersions(resourceType, resourceID string) (map[string]any, error) {
	if _, err := resourcePatternFor(resourceType); err != nil {
		return nil, ErrUnsupportedResourceType
	}
	out := map[string]any{"current_version": 0, "versions": []map[string]any{}}
	err := withVersionsLock(v.lockKey(), func() error {
		data, err := v.load()
		if err != nil {
			return err
		}
		rec := data[resourceType][resourceID]
		if rec == nil {
			return nil
		}
		projectName := filepath.Base(filepath.Clean(v.projectDir))
		versions := make([]map[string]any, 0, len(rec.Versions))
		for i := range rec.Versions {
			record := rec.Versions[i]
			if record.File == "" {
				continue
			}
			if fs.TrySafeJoin(v.projectDir, false, filepath.FromSlash(record.File)) == "" {
				continue
			}
			item := map[string]any{
				"version":    record.Version,
				"file":       record.File,
				"prompt":     record.Prompt,
				"created_at": record.CreatedAt,
			}
			for k, value := range record.Extra {
				item[k] = value
			}
			item["is_current"] = record.Version == rec.CurrentVersion
			item["file_url"] = "/api/v1/files/" + projectName + "/" + record.File
			versions = append(versions, item)
		}
		out["current_version"] = rec.CurrentVersion
		out["versions"] = versions
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetVersionMetadata 读取指定版本记录上的元数据键，记录或键不存在时返回 nil。
func (v *Versions) GetVersionMetadata(resourceType, resourceID string, version int, key string) (any, error) {
	info, err := v.GetVersions(resourceType, resourceID)
	if err != nil {
		return nil, err
	}
	items, _ := info["versions"].([]map[string]any)
	for _, item := range items {
		if number, ok := item["version"].(int); ok && number == version {
			return item[key], nil
		}
	}
	return nil, nil
}

// GetVersionCreatedAt 返回指定版本的入库时间（ISO8601），不存在时返回空串。
//
// 还原历史版本时用于把该版本的原始生成时间写回 generated_assets，而不是戳成「现在」——
// 还原回来的是旧内容，声音等派生判定须按旧时间成立。
func (v *Versions) GetVersionCreatedAt(resourceType, resourceID string, version int) (string, error) {
	value, err := v.GetVersionMetadata(resourceType, resourceID, version, "created_at")
	if err != nil {
		return "", err
	}
	text, ok := value.(string)
	if !ok {
		return "", nil
	}
	return text, nil
}

// RestoreVersion 把指定版本复制到当前路径，并把 current_version 指针切到该版本。
//
// versions.json 是持久化元数据、可被篡改：回读的 file 必须落在项目内（越界抛
// PathTraversalError）；目标文件缺失抛 OSError 级错误。
func (v *Versions) RestoreVersion(
	resourceType, resourceID string, version int, currentFile string,
) (RestoreResult, error) {
	if _, err := resourcePatternFor(resourceType); err != nil {
		return RestoreResult{}, ErrUnsupportedResourceType
	}
	var out RestoreResult
	err := withVersionsLock(v.lockKey(), func() error {
		data, err := v.load()
		if err != nil {
			return err
		}
		rec := data[resourceType][resourceID]
		if rec == nil {
			return &VersionResourceNotFoundError{ResourceType: resourceType, ResourceID: resourceID}
		}
		var target *versionRecord
		for i := range rec.Versions {
			if rec.Versions[i].Version == version {
				target = &rec.Versions[i]
				break
			}
		}
		if target == nil {
			return &VersionNotFoundError{Version: version}
		}
		sourcePath, err := fs.SafeJoin(v.projectDir, false, filepath.FromSlash(target.File))
		if err != nil {
			return err
		}
		info, err := os.Stat(sourcePath)
		if err != nil {
			return fmt.Errorf("版本文件不可读: %s: %w", target.File, err)
		}
		if info.IsDir() {
			return fmt.Errorf("版本文件不可读: %s: is a directory", target.File)
		}
		if err := copyFileInto(currentFile, sourcePath); err != nil {
			return err
		}
		rec.CurrentVersion = version
		out = RestoreResult{RestoredVersion: version, CurrentVersion: version, Prompt: target.Prompt}
		return v.save(data)
	})
	if err != nil {
		return RestoreResult{}, err
	}
	return out, nil
}
