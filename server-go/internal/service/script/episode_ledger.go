// 分集账本与源文解析（对应 lib/episode_ledger.py / lib/episode_paths.py / lib/episode_source.py）。
//
// project.json 的 episodes[] 是分集单一真相源，条目在 episode/title/script_file 之外扩展账本
// 字段（source_range / hook / outline / ledger_status），顶层 planning_cursor 标记下一批规划
// 起点、source_fingerprints 记录已参与规划的源文内容指纹。物理 source/episode_N.txt 是派生物。
//
// source_range 是账本里唯一的位置真相：有它才能从源文重造派生文件、才能续接规划。
package script

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// --- step1 / 剧本路径（episode_paths.py） ---

// step1Filenames 是结构化 step1 中间态文件名（按 content_mode）；审核 gate 仅认这两类。
var step1Filenames = map[string]string{
	"drama":     "step1_normalized_script.json",
	"narration": "step1_segments.json",
}

// step1LegacyFilenames 是旧版非结构化 step1 别名，仅供读取 / 浏览层兼认存量在制品。
var step1LegacyFilenames = map[string][]string{
	"drama":     {"step1_normalized_script.md"},
	"narration": {"step1_segments.md"},
}

// step1ProductGlobPrefix 是派生集产物文件名的公共前缀（仅账本扫描用）。
const step1ProductGlobPrefix = "step1_"

// Step1Filename 返回该内容模式的结构化 step1 文件名；不走结构化 step1（如 ad）时 ok=false。
func Step1Filename(contentMode string) (string, bool) {
	name, ok := step1Filenames[contentMode]
	return name, ok
}

// Step1ReadCandidates 返回结构化 step1 文件名 + 旧版别名（读取侧候选，主文件缺失时回落探测）。
func Step1ReadCandidates(contentMode string) []string {
	primary, ok := step1Filenames[contentMode]
	if !ok {
		return nil
	}
	out := []string{primary}
	out = append(out, step1LegacyFilenames[contentMode]...)
	return out
}


// EpisodeScriptFilename 返回该集剧本文件名 episode_N.json（不含 scripts/ 目录前缀）。
func EpisodeScriptFilename(episode int) string { return fmt.Sprintf("episode_%d.json", episode) }

// EpisodeScriptRelpath 返回该集剧本相对项目根的默认路径 scripts/episode_N.json。
func EpisodeScriptRelpath(episode int) string { return "scripts/" + EpisodeScriptFilename(episode) }

// --- 账本解析 ---

// 仅 ASCII 数字：\d 会放行全角等 Unicode 数字，把非流水线产物误判为派生集文件。
var (
	episodeFileRe  = regexp.MustCompile(`^episode_([0-9]+)\.txt$`)
	scriptFileRe   = regexp.MustCompile(`^episode_([0-9]+)\.json$`)
	draftDirRe     = regexp.MustCompile(`^episode_([0-9]+)$`)
	sourceSuffixes = map[string]bool{".txt": true, ".md": true}
)

// NormalizeSourceText 是账本坐标系的唯一归一化函数：Unicode NFC + 换行统一为 \n。
//
// source_range / planning_cursor 的偏移量全部落在本函数输出的坐标系内，任何按偏移切片源文
// 的消费方必须先对源文执行本函数。
func NormalizeSourceText(text string) string {
	normalized := norm.NFC.String(text)
	normalized = strings.ReplaceAll(normalized, "\r\n", "\n")
	return strings.ReplaceAll(normalized, "\r", "\n")
}

// ParseEpisodeNum 宽松解析条目集号：int（排除 bool）或纯 ASCII 数字字符串，其余 ok=false。
func ParseEpisodeNum(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if v != float64(int(v)) {
			return 0, false
		}
		return int(v), true
	case string:
		if v == "" || !isASCIIDigits(v) {
			return 0, false
		}
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return 0, false
		}
		return parsed, true
	}
	return 0, false
}

// ParsePositiveEpisodeNum 在 ParseEpisodeNum 基础上再要求正整数（0/负数视为损坏账本）。
func ParsePositiveEpisodeNum(value any) (int, bool) {
	num, ok := ParseEpisodeNum(value)
	if !ok || num <= 0 {
		return 0, false
	}
	return num, true
}

// isASCIIDigits 判定是否仅由 ASCII 数字组成（等价 Python str.isdigit 的窄化口径）。
func isASCIIDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ParseSourceRange 解析条目的 source_range 坐标；结构不完整（字段类型不对）时 ok=false。
//
// 只查字段类型，不校验数值是否越界——是否要求坐标落在源文界内由调用方按各自口径决定。
func ParseSourceRange(entry *domain.EpisodeMeta) (domain.SourceRange, bool) {
	if entry == nil || entry.SourceRange == nil {
		return domain.SourceRange{}, false
	}
	return *entry.SourceRange, true
}

// EpisodesWithoutSourceRange 返回账本中没有位置记录的集号（升序去重）。
//
// 这类条目无法重造派生文件、也无法据以续接规划，是规划入口拒绝执行的依据。
func EpisodesWithoutSourceRange(pf *domain.ProjectFile) []int {
	if pf == nil {
		return nil
	}
	seen := map[int]bool{}
	for index := range pf.Episodes {
		entry := &pf.Episodes[index]
		num, ok := ParseEpisodeNum(entry.Episode)
		if !ok {
			continue
		}
		if _, hasRange := ParseSourceRange(entry); !hasRange {
			seen[num] = true
		}
	}
	nums := make([]int, 0, len(seen))
	for num := range seen {
		nums = append(nums, num)
	}
	sort.Ints(nums)
	return nums
}

// --- 目录枚举 ---

// SourceDoc 是候选源文件：项目根相对 POSIX 路径 + 归一化全文。
type SourceDoc struct {
	RelPath string
	Text    string
}

// readTextOrNone 读取文本文件；不可读 / 非 UTF-8 返回 ok=false（容错降级）。
func readTextOrNone(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// DiscoverSources 枚举 source/ 直下一级的候选源文件（.txt/.md），按文件名排序。
//
// 排除派生集文件（episode_N.txt）、下划线/点前缀文件与子目录。
func DiscoverSources(projectDir string) []SourceDoc {
	sourceDir := filepath.Join(projectDir, "source")
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil
	}
	var docs []SourceDoc
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		if !sourceSuffixes[strings.ToLower(filepath.Ext(name))] || episodeFileRe.MatchString(name) {
			continue
		}
		text, ok := readTextOrNone(filepath.Join(sourceDir, name))
		if !ok {
			continue
		}
		docs = append(docs, SourceDoc{RelPath: "source/" + name, Text: NormalizeSourceText(text)})
	}
	return docs
}

// DiscoverEpisodeFileAliases 枚举派生集文件别名 source/episode_N.txt → {集号: [路径...]}。
//
// 悬空符号链接同样纳入：这类文件对被 is_file 过滤的发现逻辑不可见，处置类调用方会漏清它。
func DiscoverEpisodeFileAliases(projectDir string) map[int][]string {
	sourceDir := filepath.Join(projectDir, "source")
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil
	}
	result := map[int][]string{}
	for _, entry := range entries {
		match := episodeFileRe.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		path := filepath.Join(sourceDir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		// 目录不参与；普通文件与符号链接（含悬空链接）纳入。
		if info.IsDir() {
			continue
		}
		num, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		result[num] = append(result[num], path)
	}
	return result
}

// DiscoverEpisodeFiles 枚举派生集文件 source/episode_N.txt → {集号: 可读代表路径}。
//
// 代表路径只从别名里选可读的普通文件；全部别名都是悬空链接时该集号整体不出现。
func DiscoverEpisodeFiles(projectDir string) map[int]string {
	result := map[int]string{}
	for num, paths := range DiscoverEpisodeFileAliases(projectDir) {
		for _, path := range paths {
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				result[num] = path
				break
			}
		}
	}
	return result
}

// DiscoverProductEpisodeNums 枚举磁盘上有下游产物（剧本 JSON / step1 草稿目录）的集号。
func DiscoverProductEpisodeNums(projectDir string) map[int]bool {
	nums := map[int]bool{}
	if entries, err := os.ReadDir(filepath.Join(projectDir, "scripts")); err == nil {
		for _, entry := range entries {
			match := scriptFileRe.FindStringSubmatch(entry.Name())
			if match == nil || entry.IsDir() {
				continue
			}
			if num, err := strconv.Atoi(match[1]); err == nil {
				nums[num] = true
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(projectDir, "drafts")); err == nil {
		for _, entry := range entries {
			match := draftDirRe.FindStringSubmatch(entry.Name())
			if match == nil || !entry.IsDir() {
				continue
			}
			// 目录存在不等于有产物：只有真正落了 step1_* 才算下游产物。
			if !dirHasStep1Product(filepath.Join(projectDir, "drafts", entry.Name())) {
				continue
			}
			if num, err := strconv.Atoi(match[1]); err == nil {
				nums[num] = true
			}
		}
	}
	return nums
}

// dirHasStep1Product 判定目录下是否有 step1_* 产物（format-agnostic）。
func dirHasStep1Product(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), step1ProductGlobPrefix) {
			return true
		}
	}
	return false
}

// HasDownstreamProducts 判定该集是否已有下游产物（剧本 JSON / step1 中间文件）。
//
// 磁盘证据优先于账本状态：账本损坏时仍能判定该集是否已被消费。
func HasDownstreamProducts(projectDir string, episodeNum int, entry *domain.EpisodeMeta) bool {
	if entry != nil && entry.ScriptFile != "" && fs.SafeExists(projectDir, entry.ScriptFile) {
		return true
	}
	if fs.SafeExists(projectDir, EpisodeScriptRelpath(episodeNum)) {
		return true
	}
	return dirHasStep1Product(service.EpisodeDraftsDir(projectDir, episodeNum))
}

// RegisterOrphanEpisodeEntries 为磁盘上有派生集文件、账本却无条目的集号补建条目。
//
// 只做登记（episode / title / script_file / ledger_status），不写 source_range——补建出的条目
// 因此没有位置记录，规划入口会据此拒绝并指引全量重置，消费链路照常。已登记的集号不重复补建，
// 可安全重跑。
func RegisterOrphanEpisodeEntries(projectDir string, pf *domain.ProjectFile) {
	if pf == nil {
		return
	}
	known := map[int]bool{}
	for index := range pf.Episodes {
		if num, ok := ParseEpisodeNum(pf.Episodes[index].Episode); ok {
			known[num] = true
		}
	}
	nums := make([]int, 0)
	for num := range DiscoverEpisodeFiles(projectDir) {
		nums = append(nums, num)
	}
	sort.Ints(nums)
	changed := false
	for _, num := range nums {
		if known[num] {
			continue
		}
		entry := domain.EpisodeMeta{Episode: num, Title: "", ScriptFile: EpisodeScriptRelpath(num)}
		status := domain.LedgerStatusPlanned
		if HasDownstreamProducts(projectDir, num, &entry) {
			status = domain.LedgerStatusConsumed
		}
		entry.LedgerStatus = status
		pf.Episodes = append(pf.Episodes, entry)
		known[num] = true
		changed = true
	}
	if !changed {
		return
	}
	// 全部条目集号可解析时才排序（与 Python 侧同口径），否则保持原序避免打乱损坏账本。
	for index := range pf.Episodes {
		if _, ok := ParseEpisodeNum(pf.Episodes[index].Episode); !ok {
			return
		}
	}
	sort.SliceStable(pf.Episodes, func(i, j int) bool {
		left, _ := ParseEpisodeNum(pf.Episodes[i].Episode)
		right, _ := ParseEpisodeNum(pf.Episodes[j].Episode)
		return left < right
	})
}

// --- 源文指纹 ---

// ComputeSourceFingerprints 按源文件记录归一化文本的 sha256 指纹（相对路径 → hexdigest）。
func ComputeSourceFingerprints(sources []SourceDoc) map[string]string {
	out := make(map[string]string, len(sources))
	for _, doc := range sources {
		sum := sha256.Sum256([]byte(doc.Text))
		out[doc.RelPath] = hex.EncodeToString(sum[:])
	}
	return out
}

// MismatchedSourceFingerprints 比对记录指纹与当前源文，返回不一致的源文相对路径（排序）。
//
// 只比对「已记录」的文件：未记录视为存量项目 / 新源文件尚未补记，不参与比对。已记录文件若
// 当前指纹不同、或该文件已从候选源文件中消失，均判为不一致。
func MismatchedSourceFingerprints(recorded map[string]string, sources []SourceDoc) []string {
	if len(recorded) == 0 {
		return nil
	}
	current := ComputeSourceFingerprints(sources)
	var mismatched []string
	for rel, fingerprint := range recorded {
		if current[rel] != fingerprint {
			mismatched = append(mismatched, rel)
		}
	}
	sort.Strings(mismatched)
	return mismatched
}

// --- 大纲上下文 ---

// EpisodeOutlineContext 从分集账本提取 (本集大纲, 下集大纲) 作为剧本内容生成的规划输入。
//
// 大纲 dict 含 title / hook / story_beats / next_episode_teaser。条目无任何规划数据时对应项
// 为 nil；末集无下集，第二项为 nil。story_beats 内非字符串项一并过滤。
func EpisodeOutlineContext(pf *domain.ProjectFile, episode int) (map[string]any, map[string]any) {
	entry := func(num int) *domain.EpisodeMeta {
		if pf == nil {
			return nil
		}
		for index := range pf.Episodes {
			if pf.Episodes[index].Episode == num {
				return &pf.Episodes[index]
			}
		}
		return nil
	}
	build := func(item *domain.EpisodeMeta) map[string]any {
		if item == nil {
			return nil
		}
		var beats []string
		var storyBeats []any
		teaser := ""
		if item.Outline != nil {
			for _, beat := range item.Outline.StoryBeats {
				beats = append(beats, beat)
			}
			teaser = item.Outline.NextEpisodeTeaser
		}
		for _, beat := range beats {
			storyBeats = append(storyBeats, beat)
		}
		hooks := item.Hook
		if hooks == "" && len(beats) == 0 && teaser == "" {
			return nil
		}
		return map[string]any{
			"title":               item.Title,
			"hook":                hooks,
			"story_beats":         storyBeats,
			"next_episode_teaser": teaser,
		}
	}
	return build(entry(episode)), build(entry(episode + 1))
}

// --- 源文解析（episode_source.py） ---

// readSourceFile 按项目相对路径读源文；越界 / 缺失 / 不可读返回 ok=false。
func readSourceFile(projectDir, relativePath string) (string, string, bool) {
	path, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(relativePath))
	if err != nil {
		return "", "", false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", "", false
	}
	text, ok := readTextOrNone(path)
	if !ok {
		return "", "", false
	}
	return path, NormalizeSourceText(text), true
}

// ledgerRuneLen 返回账本坐标系下的文本长度。
//
// 账本坐标（source_range / planning_cursor 的偏移）是 Python 的字符偏移，即码点计数；Go 的
// len(string) 是字节数，中文经 UTF-8 编码后一个字符占 3 字节。两侧共写同一份 project.json，
// 故 Go 侧一律按 rune 计数与切片，绝不能直接用字节下标。
func ledgerRuneLen(text string) int { return utf8.RuneCountInString(text) }

// ledgerSlice 按账本坐标系切片文本；越界返回 ok=false。
func ledgerSlice(text string, start, end int) (string, bool) {
	runes := []rune(text)
	if start < 0 || start > end || end > len(runes) {
		return "", false
	}
	return string(runes[start:end]), true
}

// ledgerSource 按账本 source_range 切片源文。
func ledgerSource(projectDir string, pf *domain.ProjectFile, episode int) (service.EpisodeSource, bool) {
	if pf == nil {
		return service.EpisodeSource{}, false
	}
	for index := range pf.Episodes {
		entry := &pf.Episodes[index]
		if entry.Episode != episode {
			continue
		}
		sourceRange, ok := ParseSourceRange(entry)
		if !ok {
			return service.EpisodeSource{}, false
		}
		path, text, ok := readSourceFile(projectDir, sourceRange.SourceFile)
		if !ok {
			return service.EpisodeSource{}, false
		}
		episodeText, ok := ledgerSlice(text, sourceRange.Start, sourceRange.End)
		if !ok || strings.TrimSpace(episodeText) == "" {
			return service.EpisodeSource{}, false
		}
		return service.EpisodeSource{Path: path, RelativePath: sourceRange.SourceFile, Text: episodeText}, true
	}
	return service.EpisodeSource{}, false
}

// service.ResolveEpisodeSource 按统一优先级解析一集源文（经根包 Hook 对外暴露）。
//
// 显式 source 始终优先；省略时先使用账本中的原文范围，随后使用本集手动预拆分的
// source/episode_N.txt，最后回退到旧项目的原始源文拼接。
func ResolveEpisodeSource(
	projectDir string, episode int, source string, pf *domain.ProjectFile,
) (service.EpisodeSource, error) {
	if source != "" {
		path, text, ok := readSourceFile(projectDir, source)
		if !ok {
			if _, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(source)); err != nil {
				return service.EpisodeSource{}, fmt.Errorf("路径超出项目目录: %s", source)
			}
			return service.EpisodeSource{}, fmt.Errorf("未找到源文件: %s", filepath.Join(projectDir, source))
		}
		if strings.TrimSpace(text) == "" {
			return service.EpisodeSource{}, errors.New("小说原文为空")
		}
		return service.EpisodeSource{Path: path, RelativePath: source, Text: text}, nil
	}

	if resolved, ok := ledgerSource(projectDir, pf, episode); ok {
		return resolved, nil
	}

	if derived, ok := DiscoverEpisodeFiles(projectDir)[episode]; ok {
		if text, ok := readTextOrNone(derived); ok {
			normalized := NormalizeSourceText(text)
			if strings.TrimSpace(normalized) != "" {
				rel, err := filepath.Rel(projectDir, derived)
				if err != nil {
					rel = filepath.Base(derived)
				}
				return service.EpisodeSource{
					Path: derived, RelativePath: filepath.ToSlash(rel), Text: normalized, IsDerived: true,
				}, nil
			}
		}
	}

	originals := DiscoverSources(projectDir)
	if len(originals) > 0 {
		parts := make([]string, 0, len(originals))
		for _, doc := range originals {
			parts = append(parts, doc.Text)
		}
		text := strings.Join(parts, "\n\n")
		if strings.TrimSpace(text) != "" {
			return service.EpisodeSource{
				Path:         filepath.Join(projectDir, filepath.FromSlash(originals[0].RelPath)),
				RelativePath: originals[0].RelPath,
				Text:         text,
			}, nil
		}
	}

	return service.EpisodeSource{}, fmt.Errorf(
		"找不到第 %d 集源文：请提供 source 文件或准备 source/episode_%d.txt", episode, episode)
}

func init() {
	service.ResolveEpisodeSourceHook = ResolveEpisodeSource
}
