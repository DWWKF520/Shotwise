package skills

import (
	"log/slog"
	"path/filepath"
	"sort"
)

// CatalogEntry 是面向用户的 skill 目录项（对应 Python list_available_skills 的返回结构）。
// 显示名由前端 i18n（dashboard:skill_name_<name>）解析，这里不存。
type CatalogEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Icon        string `json:"icon,omitempty"`
	Path        string `json:"path"`
}

// skillIcons 是 user-invocable skill 的 Lucide 图标提示（与 Python _SKILL_ICONS 同步）。
var skillIcons = map[string]string{
	"manga-workflow":           "clapperboard",
	"generate-storyboard":      "images",
	"generate-grid":            "grid-2x2",
	"generate-video":           "film",
	"generate-narration-audio": "audio-lines",
	"generate-assets":          "users",
	"compose-video":            "scissors",
}

// ListUserInvocable 返回 user-invocable 的 skill 列表（目录不存在时返回空）。
// 变体 skill 取任一变体的元数据（装配期已保证各变体一致）；不一致的 skill 跳过并告警。
func ListUserInvocable(root string) ([]CatalogEntry, error) {
	dirs, err := skillDirs(root)
	if err != nil {
		return nil, err
	}
	var out []CatalogEntry
	for _, dir := range dirs {
		path, ok, err := catalogEntryFile(dir)
		if err != nil {
			slog.Warn("skill 目录项跳过", "skill", filepath.Base(dir), "err", err)
			continue
		}
		if !ok {
			continue
		}
		meta, err := ParseFile(path, filepath.Base(dir))
		if err != nil {
			return nil, err
		}
		if !meta.UserInvocable {
			continue
		}
		out = append(out, CatalogEntry{
			Name:        meta.Name,
			Description: meta.Description,
			Icon:        skillIcons[meta.Name],
			Path:        path,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// catalogEntryFile 与 resolveEntry 的区别：目录项不依赖 mode，取第一个可用入口，
// 并要求变体间 user-invocable 一致。
func catalogEntryFile(dir string) (string, bool, error) {
	if isFile(filepath.Join(dir, commonSkillFile)) {
		return filepath.Join(dir, commonSkillFile), true, nil
	}
	existing := existingVariants(variantPaths(dir))
	if len(existing) == 0 {
		return "", false, nil
	}
	if _, err := consistentInvocable(dir); err != nil {
		return "", false, err
	}
	return existing[0], true, nil
}
