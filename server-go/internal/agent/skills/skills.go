// Package skills 实现智能体 Skill 的文件系统加载（设计文档 03-agent-runtime-eino.md §17.1–§17.2）。
//
// 目录契约与 Python 侧 service.py::_resolve_skill_entry_file 一致：
//   - 通用 skill：<dir>/SKILL.md
//   - 变体 skill：<dir>/SKILL.{drama,narration,ad}.md，三 mode 必须齐全且 user-invocable 一致
//   - 两者同时存在为配置冲突
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	commonSkillFile = "SKILL.md"
	// SkillToolName 是 skill 中间件注册给模型的工具名（Eino 默认值）。
	SkillToolName = "skill"
)

// ContentModes 是合法的 content_mode 取值（已排序，与 Python sorted(VALID_CONTENT_MODES) 对齐）。
var ContentModes = []string{"ad", "drama", "narration"}

// Meta 是从 SKILL.md frontmatter + 正文解析出的 skill 元数据。
type Meta struct {
	Name          string
	Description   string
	UserInvocable bool
	Body          string
	Path          string
}

// IsValidMode 报告 mode 是否为合法 content_mode。
func IsValidMode(mode string) bool {
	for _, m := range ContentModes {
		if m == mode {
			return true
		}
	}
	return false
}

// resolveEntry 解析一个 skill 目录应使用的入口文件。
// 返回 ("", false, nil) 表示该目录不是 skill 或当前 mode 下不可见。
func resolveEntry(dir, mode string) (string, bool, error) {
	common := filepath.Join(dir, commonSkillFile)
	commonExists := isFile(common)
	variants := variantPaths(dir)
	existing := existingVariants(variants)

	switch {
	case commonExists && len(existing) > 0:
		return "", false, fmt.Errorf("skill %s: SKILL.md 与 SKILL.<mode>.md 变体互斥", filepath.Base(dir))
	case commonExists:
		return common, true, nil
	case len(existing) == 0:
		return "", false, nil
	}
	if mode == "" {
		return "", false, nil
	}
	if p, ok := variants[mode]; ok && isFile(p) {
		return p, true, nil
	}
	return "", false, nil
}

// ValidateDir 装配期校验：变体 skill 三 mode 必须齐全，且 user-invocable 在各变体间一致。
// 违反即报错（fail loud），与 profile manifest 决策表同一约束。
func ValidateDir(root string) error {
	dirs, err := skillDirs(root)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if isFile(filepath.Join(dir, commonSkillFile)) {
			if len(existingVariants(variantPaths(dir))) > 0 {
				return fmt.Errorf("skill %s: SKILL.md 与 SKILL.<mode>.md 变体互斥", filepath.Base(dir))
			}
			continue
		}
		variants := variantPaths(dir)
		existing := existingVariants(variants)
		if len(existing) == 0 {
			continue
		}
		var missing []string
		for _, m := range ContentModes {
			if !isFile(variants[m]) {
				missing = append(missing, m)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("skill %s: 变体缺少 mode %v", filepath.Base(dir), missing)
		}
		if _, err := consistentInvocable(dir); err != nil {
			return err
		}
	}
	return nil
}

// consistentInvocable 读取变体的 user-invocable，要求各变体一致。
func consistentInvocable(dir string) (bool, error) {
	var states []bool
	for _, m := range ContentModes {
		meta, err := ParseFile(variantPaths(dir)[m], filepath.Base(dir))
		if err != nil {
			return false, err
		}
		states = append(states, meta.UserInvocable)
	}
	for _, s := range states[1:] {
		if s != states[0] {
			return false, fmt.Errorf("skill %s: 各 content_mode 变体的 user-invocable 不一致", filepath.Base(dir))
		}
	}
	return states[0], nil
}

// ParseFile 解析 SKILL.md：frontmatter 宽松解析（与 Python _load_skill_metadata 同语义）。
func ParseFile(path, fallbackName string) (Meta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Meta{}, err
	}
	content := string(raw)
	meta := Meta{Name: fallbackName, UserInvocable: true, Path: path}

	if strings.HasPrefix(content, "---") {
		parts := strings.SplitN(content, "---", 3)
		if len(parts) >= 3 {
			for _, line := range strings.Split(parts[1], "\n") {
				key, value, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				key = strings.TrimSpace(key)
				value = strings.Trim(strings.TrimSpace(value), `"'`)
				switch {
				case key == "name" && value != "":
					meta.Name = value
				case key == "description" && value != "":
					meta.Description = value
				case key == "user-invocable":
					meta.UserInvocable = !isFalsy(value)
				}
			}
			meta.Body = parts[2]
		}
	} else {
		meta.Body = content
	}
	if meta.Description == "" {
		meta.Description = firstTextLine(meta.Body)
	}
	return meta, nil
}

func isFalsy(v string) bool {
	switch strings.ToLower(v) {
	case "false", "no", "0":
		return true
	}
	return false
}

func firstTextLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		text := strings.TrimSpace(line)
		if text != "" && !strings.HasPrefix(text, "#") {
			return text
		}
	}
	return ""
}

func variantPaths(dir string) map[string]string {
	out := make(map[string]string, len(ContentModes))
	for _, m := range ContentModes {
		out[m] = filepath.Join(dir, fmt.Sprintf("SKILL.%s.md", m))
	}
	return out
}

func existingVariants(variants map[string]string) []string {
	var out []string
	for _, m := range ContentModes {
		if isFile(variants[m]) {
			out = append(out, variants[m])
		}
	}
	return out
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// skillDirs 返回 root 下一级子目录（已排序）。root 不存在时返回空。
func skillDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}
