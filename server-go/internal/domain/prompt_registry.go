package domain

// 提示词模板注册表（对应 lib/prompt_registry.py）。
//
// agent_runtime_profile 的只读元数据投影：把随发行版发布的模板按「流程环节」分四组
// （system / agent / skill / reference），供设置页只读浏览。注册表不改写任何模板，
// 也不参与运行时 prompt 组装。分组、内容模式、标题与描述全部从文件结构与文件头推导，
// 新增模板文件自动进入注册表。

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// 分组顺序；同时是设置页分区浏览的展示顺序。
const (
	PromptGroupSystem    = "system"
	PromptGroupAgent     = "agent"
	PromptGroupSkill     = "skill"
	PromptGroupReference = "reference"
)

// PromptTemplateGroups 分组顺序（与 Python PROMPT_TEMPLATE_GROUPS 一致）。
var PromptTemplateGroups = []string{PromptGroupSystem, PromptGroupAgent, PromptGroupSkill, PromptGroupReference}

// PromptContentModes 模板可声明的内容模式变体，取值与项目 content_mode 一致。
var PromptContentModes = []string{"drama", "narration", "ad"}

const (
	promptMaxTitleChars       = 200
	promptMaxDescriptionChars = 400
	promptProfileDirEnv       = "SHOTWISE_PROFILE_DIR"
	promptProfileDirName      = "agent_runtime_profile"
)

var (
	promptTitlePattern  = regexp.MustCompile(`^#\s+(?P<title>.+?)\s*$`)
	promptSectionRegex  = regexp.MustCompile(`^##\s+(?P<section>.+?)\s*$`)
	promptModeCommentRe = regexp.MustCompile(`<!--\s*mode:\s*(?P<mode>[A-Za-z0-9_-]+)\s*-->`)
)

// PromptTemplate 一条模板的只读元数据；Path 是相对 profile 根目录的 POSIX 路径，同时充当标识。
type PromptTemplate struct {
	Path        string   `json:"path"`
	Group       string   `json:"group"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	ContentMode *string  `json:"content_mode"`
	Sections    []string `json:"sections"`
	CharCount   int      `json:"char_count"`
}

// PromptTemplateDetail 模板元数据 + 正文，供详情只读浏览。
type PromptTemplateDetail struct {
	Template PromptTemplate `json:"template"`
	Content  string         `json:"content"`
}

// AgentProfileRoot 解析 agent_runtime_profile 目录。
//
// 对应 Python lib/agent_profile.py：SHOTWISE_PROFILE_DIR 优先，否则回溯查找
// <cwd>/../agent_runtime_profile（源码树布局 lib/.. 的等价物）。
func AgentProfileRoot() string {
	if override := strings.TrimSpace(os.Getenv(promptProfileDirEnv)); override != "" {
		return expandUserPath(override)
	}
	if wd, err := os.Getwd(); err == nil {
		dir := wd
		for i := 0; i < 6; i++ {
			candidate := filepath.Join(dir, promptProfileDirName)
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				return candidate
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return ""
}

// expandUserPath 展开 ~ 前缀并转绝对路径（对应 Python Path.expanduser().resolve()）。
func expandUserPath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[1:])
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// BuildPromptRegistry 枚举 profile 下的提示词模板；目录缺失时返回空表而不是报错。
func BuildPromptRegistry(profileRoot string) []PromptTemplate {
	if profileRoot == "" {
		return nil
	}
	if info, err := os.Stat(profileRoot); err != nil || !info.IsDir() {
		return nil
	}
	order := map[string]int{}
	for index, group := range PromptTemplateGroups {
		order[group] = index
	}
	entries := []PromptTemplate{}
	for _, candidate := range promptTemplateCandidates(profileRoot) {
		if entry, ok := loadPromptTemplate(profileRoot, candidate.path, candidate.group); ok {
			entries = append(entries, entry)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		left, right := entries[i], entries[j]
		if order[left.Group] != order[right.Group] {
			return order[left.Group] < order[right.Group]
		}
		leftMode, rightMode := "", ""
		if left.ContentMode != nil {
			leftMode = *left.ContentMode
		}
		if right.ContentMode != nil {
			rightMode = *right.ContentMode
		}
		if leftMode != rightMode {
			return leftMode < rightMode
		}
		return left.Path < right.Path
	})
	return entries
}

// FindPromptTemplate 按 path 读取单条模板的元数据与正文；未知 id 返回 nil。
//
// 只按注册表索引查表，不把请求值拼进文件系统路径，因此 ../ 之类输入只会命中
// 「未知 id」，不会越出 profile 目录。
func FindPromptTemplate(profileRoot, templatePath string) *PromptTemplateDetail {
	for _, entry := range BuildPromptRegistry(profileRoot) {
		if entry.Path != templatePath {
			continue
		}
		content, ok := readPromptTemplateText(filepath.Join(profileRoot, filepath.FromSlash(entry.Path)))
		if !ok {
			return nil
		}
		return &PromptTemplateDetail{Template: entry, Content: content}
	}
	return nil
}

type promptCandidate struct {
	path  string
	group string
}

func promptTemplateCandidates(root string) []promptCandidate {
	var candidates []promptCandidate
	candidates = append(candidates, promptGlobGroup(filepath.Join(root, "CLAUDE.*.md"), PromptGroupSystem)...)
	candidates = append(candidates, promptGlobGroup(filepath.Join(root, ".claude", "agents", "*.md"), PromptGroupAgent)...)
	candidates = append(candidates, promptGlobGroup(filepath.Join(root, ".claude", "skills", "*", "SKILL*.md"), PromptGroupSkill)...)
	candidates = append(candidates, promptGlobGroup(filepath.Join(root, ".claude", "references", "*.md"), PromptGroupReference)...)
	return candidates
}

// promptGlobGroup 按 glob 取候选（glob 结果天然有序，与 Python sorted(glob) 同口径）。
func promptGlobGroup(pattern, group string) []promptCandidate {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	out := make([]promptCandidate, 0, len(matches))
	for _, match := range matches {
		out = append(out, promptCandidate{path: match, group: group})
	}
	return out
}

// readPromptTemplateText 读取模板正文；模板可能带 UTF-8 BOM，按 utf-8-sig 解码
// 以免标题/围栏误判。同时做通用换行归一（Python read_text 的 universal newlines：
// \r\n 与孤立 \r 都折成 \n），否则 CRLF 模板的码点数与逐行解析都会和 Python 漂移。
func readPromptTemplateText(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return text, true
}

func loadPromptTemplate(root, path, group string) (PromptTemplate, bool) {
	// 只读常规文件：符号链接可能指向 profile 之外，拒绝投影以免元数据越界。
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return PromptTemplate{}, false
	}
	text, ok := readPromptTemplateText(path)
	if !ok {
		return PromptTemplate{}, false
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return PromptTemplate{}, false
	}
	relative = filepath.ToSlash(relative)
	frontmatter := promptFrontmatter(text)
	title := promptFirstHeading(text)
	if title == "" {
		title = frontmatter["name"]
	}
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	description := frontmatter["description"]
	if description == "" {
		description = promptFirstParagraph(text)
	}
	return PromptTemplate{
		Path:        relative,
		Group:       group,
		Title:       promptBounded(title, promptMaxTitleChars),
		Description: promptBounded(description, promptMaxDescriptionChars),
		ContentMode: promptContentMode(path, text),
		Sections:    promptSections(text),
		CharCount:   utf8.RuneCountInString(text),
	}, true
}

// promptBounded 折叠空白并按码点截断（Python str 切片同口径）。
func promptBounded(value string, limit int) string {
	collapsed := strings.Join(strings.Fields(value), " ")
	if utf8.RuneCountInString(collapsed) <= limit {
		return collapsed
	}
	runes := []rune(collapsed)
	return string(runes[:limit-1]) + "…"
}

// promptFrontmatter 读取文件头 --- 围栏里的字符串字段；未闭合一律按空表处理。
func promptFrontmatter(text string) map[string]string {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return map[string]string{}
	}
	values := map[string]string{}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			return values
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(key))
		if name == "name" || name == "description" {
			values[name] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return map[string]string{}
}

func promptFirstHeading(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if match := promptTitlePattern.FindStringSubmatch(line); match != nil {
			return match[1]
		}
	}
	return ""
}

func promptSections(text string) []string {
	sections := []string{}
	for _, line := range strings.Split(text, "\n") {
		if match := promptSectionRegex.FindStringSubmatch(line); match != nil {
			sections = append(sections, promptBounded(match[1], promptMaxTitleChars))
		}
	}
	return sections
}

// promptFirstParagraph 取 frontmatter 之后的第一段正文（跳过标题、注释与空行）。
func promptFirstParagraph(text string) string {
	lines := strings.Split(text, "\n")
	inFrontmatter := false
	for index, line := range lines {
		stripped := strings.TrimSpace(line)
		if index == 0 && stripped == "---" {
			inFrontmatter = true
			continue
		}
		if inFrontmatter {
			if stripped == "---" {
				inFrontmatter = false
			}
			continue
		}
		if stripped == "" || strings.HasPrefix(stripped, "#") || strings.HasPrefix(stripped, "<!--") {
			continue
		}
		return stripped
	}
	return ""
}

// promptContentMode 从文件名后缀（CLAUDE.drama.md）或文件头 <!-- mode: x --> 推导内容模式。
func promptContentMode(path, text string) *string {
	parts := strings.Split(filepath.Base(path), ".")
	for i := len(parts) - 1; i >= 0; i-- {
		for _, mode := range PromptContentModes {
			if parts[i] == mode {
				mode := mode
				return &mode
			}
		}
	}
	if match := promptModeCommentRe.FindStringSubmatch(text); match != nil {
		for _, mode := range PromptContentModes {
			if match[1] == mode {
				mode := mode
				return &mode
			}
		}
	}
	return nil
}
