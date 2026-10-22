package service

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// 剧本写盘统一入口与锁内读-改-写（对应 ProjectManager 的 locked_script / _write_script_unlocked）。
//
// 所有剧本编辑都必须经这里落盘：结构「不更坏」守卫、filename↔episode 一致性、metadata 重算、
// 原子写与 project.json 集元数据同步集中在一处，工具层只负责改 dict。

// ScriptStructureValidationError 表示本次写入把一份原本合法的剧本改成了非法。
type ScriptStructureValidationError struct{ Errors []string }

func (e *ScriptStructureValidationError) Error() string {
	if len(e.Errors) == 0 {
		return "script structure invalid"
	}
	return "剧本结构校验失败: " + joinErrors(e.Errors)
}

// scriptEpisodePattern 是文件名里隐含集号的正则（支持下划线/空格/连字符分隔）。
var scriptEpisodePattern = regexp.MustCompile(`(?i)episode[-_\s]*(\d+)`)

// NormalizeScriptFilename 剥离 scripts/ 前缀并折叠路径片段，归一到剧本锁使用的身份。
//
// episode_1.json、scripts/episode_1.json、./episode_1.json 都指向同一剧本。须先折叠再剥前缀，
// 否则 ./scripts/x.json 剥前缀不命中，会被当成裸文件名再次拼接 scripts/ 目录。
// 保留 .. 片段原样交给路径解析拒绝，这里不做越界判断。
func NormalizeScriptFilename(name string) string {
	normalized := filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
	normalized = trimScriptsPrefix(normalized)
	if normalized == "." || normalized == "/" {
		return ""
	}
	return normalized
}

func trimScriptsPrefix(name string) string {
	const prefix = "scripts/"
	if len(name) > len(prefix) && name[:len(prefix)] == prefix {
		return name[len(prefix):]
	}
	return name
}

// UpdateScript 在剧本锁内完成 load → mutate → 写盘，消除读改写之间的丢失更新。
//
// mutate 内就地修改剧本 dict；返回错误时跳过写盘（整批零落盘），锁照常释放。
// 写盘走 writeScript：结构不更坏 + 集号一致性 + metadata 重算 + 集元数据同步。
func (s *ProjectStore) UpdateScript(project, scriptFilename string, mutate func(script map[string]any) error) error {
	return s.WithScriptLock(project, scriptFilename, func(dir, filename string) error {
		script, err := s.LoadScriptOrNew(dir, filename)
		if err != nil {
			return err
		}
		before := CloneScript(script)
		if err := mutate(script); err != nil {
			return err
		}
		return s.WriteScript(project, dir, filename, script, before, true)
	})
}

// SaveScript 直连写盘（无 mutate）：改前版本按需读盘取快照。
func (s *ProjectStore) SaveScript(project, scriptFilename string, script map[string]any) error {
	return s.WithScriptLock(project, scriptFilename, func(dir, filename string) error {
		before, err := readScriptOrNone(filepath.Join(dir, "scripts", filename))
		if err != nil {
			return err
		}
		return s.WriteScript(project, dir, filename, script, before, true)
	})
}

// WithScriptLock 在按剧本身份取的锁内执行 fn；fn 拿到项目目录与归一后的文件名。
func (s *ProjectStore) WithScriptLock(project, scriptFilename string, fn func(dir, filename string) error) error {
	filename := NormalizeScriptFilename(scriptFilename)
	if err := ValidateScriptBase(filename); err != nil {
		return err
	}
	dir, err := s.Dir(project)
	if err != nil {
		return err
	}
	var runErr error
	s.locks.WithLock(project+"/scripts/"+filename, func() { runErr = fn(dir, filename) })
	return runErr
}

// writeScript 是写盘主体：调用方必须已持剧本锁。
//
// 顺序与 Python 一致：结构守卫 → 集号一致性 → metadata 重算 → 原子写 → 集元数据同步。
// validate 为 false 时整体豁免结构校验与 metadata 重算（资产回写热路径专用）。
func (s *ProjectStore) WriteScript(project, dir, filename string, script map[string]any, before map[string]any, validate bool) error {
	if err := s.writeScriptFile(dir, filename, script, before, validate); err != nil {
		return err
	}
	if s.Exists(project) {
		if _, isInt := scriptEpisodeNumber(script); isInt {
			return s.syncEpisodeFromScript(project, filename)
		}
	}
	return nil
}

// writeScriptFile 只做结构守卫 + 集号一致性 + metadata 重算 + 原子写，不触碰 project.json。
//
// 供已持项目锁的调用方（UpdateEpisodeScript）内联完成集元数据同步——若在此调用
// syncEpisodeFromScript 会二次取项目锁，与调用方已持的锁自死锁。
func (s *ProjectStore) writeScriptFile(dir, filename string, script map[string]any, before map[string]any, validate bool) error {
	if validate {
		if err := guardNoWorse(before, script); err != nil {
			return err
		}
	}
	// 集号一致性先于落盘：否则会留下「剧本已落盘、project.json 未同步」的部分提交。
	if err := requireFilenameEpisodeConsistency(script, filename); err != nil {
		return err
	}
	if validate {
		recomputeScriptMetadata(script)
	}

	target, err := fs.SafeJoin(filepath.Join(dir, "scripts"), false, filename)
	if err != nil {
		return err
	}
	return fs.WriteJSONAtomic(target, script)
}

// guardNoWorse 「不更坏」守卫：仅当本次写入引入新结构错误时拒绝。
//
// 改后合法放行；改后非法时，改前已非法或没有改前快照则放行（不为历史遗留背锅），
// 否则拒绝。改前快照读不到（文件不存在或损坏）时按严格校验处理。
func guardNoWorse(before, after map[string]any) error {
	afterErrors := domain.ValidateScriptStructure(after)
	if len(afterErrors) == 0 {
		return nil
	}
	if before != nil && len(domain.ValidateScriptStructure(before)) > 0 {
		return nil
	}
	return &ScriptStructureValidationError{Errors: afterErrors}
}

// ResolveEpisodeFromScript 从剧本解析集号（对应 Python ProjectManager.resolve_episode_from_script）。
//
// 优先取剧本顶层 episode 字段（真相源），回退到文件名隐含集号；两者都无时报错。
func ResolveEpisodeFromScript(script map[string]any, scriptFilename string) (int, error) {
	if episode, ok := scriptEpisodeNumber(script); ok {
		return episode, nil
	}
	if match := scriptEpisodePattern.FindStringSubmatch(scriptFilename); match != nil {
		if episode, err := strconv.Atoi(match[1]); err == nil {
			return episode, nil
		}
	}
	return 0, fmt.Errorf("无法确定集号：剧本缺少 episode 字段且文件名 %s 不含 episodeN 模式", scriptFilename)
}

// requireFilenameEpisodeConsistency 校验剧本内 episode 字段与文件名隐含集号一致。
// 文件名不含集号模式或剧本内无 episode 整数时静默放行（兼容旧数据）。
func requireFilenameEpisodeConsistency(script map[string]any, scriptFilename string) error {
	match := scriptEpisodePattern.FindStringSubmatch(NormalizeScriptFilename(scriptFilename))
	if match == nil {
		return nil
	}
	scriptEpisode, ok := scriptEpisodeNumber(script)
	if !ok {
		return nil
	}
	filenameEpisode, err := strconv.Atoi(match[1])
	if err != nil {
		return nil
	}
	if scriptEpisode != filenameEpisode {
		return fmt.Errorf(
			"脚本 %s 内部 episode=%d 与文件名隐含的 episode=%d 不一致，拒绝操作以避免污染 project.json",
			NormalizeScriptFilename(scriptFilename), scriptEpisode, filenameEpisode)
	}
	return nil
}

// scriptEpisodeNumber 取剧本顶层 episode 字段的整数形态；bool 不算（与 Python 排除 bool 同口径）。
func scriptEpisodeNumber(script map[string]any) (int, bool) {
	switch v := script["episode"].(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, false
		}
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

// recomputeScriptMetadata 重算剧本 metadata：创建/更新时间与分镜统计。
//
// 剧本数据损坏（骨架数组类型非数组）时跳过统计重算以保留旧值——旧的统计即便陈旧，
// 也好过把损坏脚本改写成 0 分镜的假状态。
func recomputeScriptMetadata(script map[string]any) {
	now := time.Now().UTC().Format(time.RFC3339)
	metadata, ok := script["metadata"].(map[string]any)
	if !ok {
		metadata = map[string]any{}
		script["metadata"] = metadata
	}
	if _, exists := metadata["created_at"]; !exists {
		metadata["created_at"] = now
	}
	if _, exists := metadata["status"]; !exists {
		metadata["status"] = "draft"
	}
	metadata["updated_at"] = now

	items, _, kind, err := domain.ResolveScriptItems(script)
	if err != nil {
		return
	}
	// 损坏脚本可能混入非对象元素：各读取路径都把它们当作不存在过滤掉，统计一并排除。
	clean := make([]any, 0, len(items))
	for _, item := range items {
		if _, ok := item.(map[string]any); ok {
			clean = append(clean, item)
		}
	}
	metadata["total_scenes"] = len(clean)
	metadata["estimated_duration_seconds"] = domain.ScriptDurationTotal(kind, clean)
}

// syncEpisodeFromScript 把剧本的集号/标题/script_file 同步进 project.json 的 episodes。
//
// 文件名隐含集号与剧本内 episode 不一致时拒绝，避免错写覆盖真实集号条目。
func (s *ProjectStore) syncEpisodeFromScript(project, scriptFilename string) error {
	filename := NormalizeScriptFilename(scriptFilename)
	script, err := s.readScriptByFilename(project, filename)
	if err != nil {
		return err
	}
	return s.Update(project, func(pf *domain.ProjectFile) error {
		return applyEpisodeSync(pf, script, filename)
	})
}

// applyEpisodeSync 就地同步一集的元数据（供已持项目锁的调用方复用）。
func applyEpisodeSync(pf *domain.ProjectFile, script map[string]any, scriptFilename string) error {
	filename := NormalizeScriptFilename(scriptFilename)
	if err := requireFilenameEpisodeConsistency(script, filename); err != nil {
		return err
	}

	episodeNum, ok := scriptEpisodeNumber(script)
	if !ok {
		if match := scriptEpisodePattern.FindStringSubmatch(filename); match != nil {
			if parsed, err := strconv.Atoi(match[1]); err == nil {
				episodeNum = parsed
			} else {
				episodeNum = 1
			}
		} else {
			episodeNum = 1
		}
	}
	title, _ := script["title"].(string)
	scriptFile := "scripts/" + filename

	// 受支持的剧本保存代表显式重新创建该集：解除删除抑制后才允许写回元数据。
	if len(pf.DeletedEpisodeScriptFiles) > 0 {
		remaining := make([]string, 0, len(pf.DeletedEpisodeScriptFiles))
		for _, item := range pf.DeletedEpisodeScriptFiles {
			if item != scriptFile {
				remaining = append(remaining, item)
			}
		}
		sort.Strings(remaining)
		pf.DeletedEpisodeScriptFiles = remaining
	}

	index := -1
	for i, ep := range pf.Episodes {
		if ep.Episode == episodeNum {
			index = i
			break
		}
	}
	if index < 0 {
		pf.Episodes = append(pf.Episodes, domain.EpisodeMeta{Episode: episodeNum})
		index = len(pf.Episodes) - 1
	}
	// 只同步核心元数据；统计字段由读时计算注入，不落盘。
	pf.Episodes[index].Title = title
	pf.Episodes[index].ScriptFile = scriptFile
	sort.SliceStable(pf.Episodes, func(i, j int) bool { return pf.Episodes[i].Episode < pf.Episodes[j].Episode })
	return nil
}

// readScriptByFilename 读剧本；文件不存在或损坏时返回错误。
func (s *ProjectStore) readScriptByFilename(project, filename string) (map[string]any, error) {
	dir, err := s.Dir(project)
	if err != nil {
		return nil, err
	}
	target, err := fs.SafeResolve(filepath.Join(dir, "scripts"), filename)
	if err != nil {
		return nil, err
	}
	var script map[string]any
	if err := fs.ReadJSON(target, &script); err != nil {
		return nil, err
	}
	if script == nil {
		script = map[string]any{}
	}
	return script, nil
}

// loadScriptOrNew 读剧本供锁内改；不存在时按空对象起步（新剧本创建走同一条写盘路径）。
//
// 读后就地跑时长收编迁移（对应 Python _read_script_unlocked 被锁定链复用）：迁移结果随
// 调用方的正常写盘落盘，与外部读路径（ReadScriptJSON）「谁先跑谁定终局」。
func (s *ProjectStore) LoadScriptOrNew(dir, filename string) (map[string]any, error) {
	target, err := fs.SafeJoin(filepath.Join(dir, "scripts"), false, filename)
	if err != nil {
		return nil, err
	}
	script, err := readScriptOrNone(target)
	if err != nil {
		return nil, err
	}
	if script == nil {
		script = map[string]any{}
	}
	migrateScriptInPlace(script, target)
	return script, nil
}

// readScriptMigrated 裸读剧本并就地跑存量迁移，返回 (剧本, 是否发生迁移)。
func ReadScriptMigrated(path string) (map[string]any, bool, error) {
	var script map[string]any
	if err := fs.ReadJSON(path, &script); err != nil {
		return nil, false, err
	}
	if script == nil {
		script = map[string]any{}
	}
	migrated := migrateScriptInPlace(script, path)
	return script, migrated, nil
}

// migrateScriptInPlace 就地跑剧集脚本的时长收编迁移并记日志；返回是否发生迁移。
func migrateScriptInPlace(script map[string]any, path string) bool {
	if MigrateScriptUnitDurationsHook == nil {
		return false
	}
	migrated, warnings := MigrateScriptUnitDurationsHook(script)
	for _, message := range warnings {
		slog.Warn("剧本时长收编迁移",
			"file", filepath.Base(path), "key", message.Key, "params", message.Params)
	}
	return migrated
}

// readScriptOrNone 裸读剧本 JSON 取「改前」快照；文件不存在或损坏时返回 nil（按严格校验处理）。
func readScriptOrNone(path string) (map[string]any, error) {
	var script map[string]any
	if err := fs.ReadJSON(path, &script); err != nil {
		return nil, nil
	}
	if script == nil {
		return nil, nil
	}
	return script, nil
}

func CloneScript(script map[string]any) map[string]any {
	out := make(map[string]any, len(script))
	for k, v := range script {
		out[k] = cloneScriptValue(v)
	}
	return out
}

func cloneScriptValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return CloneScript(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cloneScriptValue(item)
		}
		return out
	default:
		return v
	}
}

// validateScriptBase 拒绝任何非纯文件名的剧本参数。
func ValidateScriptBase(filename string) error {
	if filename == "" {
		return errors.New("script 文件名不能为空")
	}
	if filepath.Base(filename) != filename || filename == "." || filename == ".." {
		return fmt.Errorf("script 必须是纯文件名，禁止路径分隔符: %q", filename)
	}
	return nil
}

func joinErrors(list []string) string {
	out := ""
	for i, item := range list {
		if i > 0 {
			out += "; "
		}
		out += item
	}
	return out
}

// MigrationWarning 是剧本时长收编迁移的告警（reference 域产出，根包仅承载类型）。
type MigrationWarning struct {
	Key    string
	Params map[string]any
}

// MigrateScriptUnitDurationsHook 由 reference 包 init 注册（依赖倒置：写盘守卫
// 触发存量迁移，根包不反向依赖 reference 域）。
var MigrateScriptUnitDurationsHook func(script any) (bool, []MigrationWarning)
