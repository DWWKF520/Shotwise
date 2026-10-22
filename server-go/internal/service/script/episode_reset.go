// 分集规划重置（对应 lib/episode_reset.py）：把账本退回未规划状态的逃生口。
//
// 全量重置（from_episode=1）零前置校验——不读旧坐标、不解析范围，账本处于任何损坏状态都必须
// 执行成功；执行后 episodes 清空、planning_cursor 置空、源文指纹清除。部分重置（>1）保留
// 第 1..from_episode-1 集，走前置校验（指纹一致 + 保留段坐标完整连续），不满足即拒绝并指引
// 改用全量重置。
//
// 本模块刻意不依赖文本生成：重置不调模型，逃生口不能因供应商未配置而失效。
//
// 派生集文件按「是否可从账本重造」分流：带 source_range 的 episode_N.txt 是派生物，直接删除；
// 无 source_range 的集文件可能是老项目原件（含手工内容，无坐标可重造），改名留底而非删除。
// 下游产物（剧本 JSON、step1 中间文件、媒体）一律不删。
package script

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
)

// EpisodeResetError 是分集规划重置失败。
type EpisodeResetError struct{ Message string }

func (e *EpisodeResetError) Error() string { return e.Message }

// EpisodeResetConflictError 表示重置期间账本被并发修改（出现确认清单之外的已消费集）。
type EpisodeResetConflictError struct{ Message string }

func (e *EpisodeResetConflictError) Error() string { return e.Message }

// ResetConfirmationRequired 表示重置波及已消费集，需显式确认后才执行。
//
// 返回本对象时未发生任何写入；ArchivedFiles 供调用方向用户交代留底去向。
type ResetConfirmationRequired struct {
	ConsumedEpisodes []int
	ArchivedFiles    []string
}

// EpisodeResetResult 是重置执行结果：清掉的集号与文件处置去向（相对项目根的 POSIX 路径）。
type EpisodeResetResult struct {
	RemovedEpisodes  []int
	DeletedFiles     []string
	ArchivedFiles    [][2]string // (原路径, 留底路径)
	ConsumedEpisodes []int
}

// resetPlan 是一次扫描得出的处置计划。
type resetPlan struct {
	episodeNums []int
	consumed    []int
	deletes     []string
	archives    []string
}

const fullResetHint = "请改用 from_episode=1 做全量重置"

// archivePath 派生集文件的留底路径：下划线前缀 + .bak 尾缀，同名时追加序号。
//
// 两处改动缺一不可地把文件挡在发现逻辑之外：下划线前缀让 DiscoverSources 跳过（不会被当成
// 新源文），.bak 后缀既不属于源文后缀白名单、也让 episode_N.txt 全匹配落空（不会被当成派生
// 集文件重新补建账本条目）。
func archivePath(path string) string {
	dir := filepath.Dir(path)
	base := "_" + filepath.Base(path)
	candidate := filepath.Join(dir, base+".bak")
	index := 1
	// 悬空符号链接会被 os.Stat 判为不存在：上一次重置把悬空 episode_N.txt 留底后，candidate
	// 本身可能就是这样一个悬空链接，仅查 Stat 会漏判占用、rename 静默覆盖上一次留底。
	for pathOccupied(candidate) {
		candidate = filepath.Join(dir, fmt.Sprintf("%s.%d.bak", base, index))
		index++
	}
	return candidate
}

// pathOccupied 判定路径是否已被占用（含悬空符号链接）。
func pathOccupied(path string) bool {
	if _, err := os.Stat(path); err == nil {
		return true
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// hasRecreatableSourceRange 判定条目的 source_range 是否携带足以重造派生文件的坐标结构。
//
// 只看坐标结构是否完整、不读源文校验数值越界——重置零前置校验。结构不完整按「不可重造」
// 处理，其集文件走留底而非删除（删除不可逆，证据不足时偏保守）。
func hasRecreatableSourceRange(entry *domain.EpisodeMeta) bool {
	_, ok := ParseSourceRange(entry)
	return ok
}

// scanResetPlan 扫描账本与磁盘得出处置计划。纯读：不改入参、不动文件。
//
// 已消费判定取账本状态与磁盘产物的并集——账本损坏时 ledger_status 未必可信，磁盘上的剧本 /
// step1 产物才是「这一集已经被消费过」的硬证据。fromEpisode 限定处置范围。
func scanResetPlan(projectDir string, pf *domain.ProjectFile, fromEpisode int) resetPlan {
	entriesByNum := map[int][]*domain.EpisodeMeta{}
	consumedByStatus := map[int]bool{}
	if pf != nil {
		for index := range pf.Episodes {
			entry := &pf.Episodes[index]
			num, ok := ParseEpisodeNum(entry.Episode)
			if !ok {
				continue
			}
			if entry.LedgerStatus == domain.LedgerStatusConsumed {
				consumedByStatus[num] = true
			}
			entriesByNum[num] = append(entriesByNum[num], entry)
		}
	}
	fileAliases := DiscoverEpisodeFileAliases(projectDir)
	productNums := DiscoverProductEpisodeNums(projectDir)

	inScope := func(num int) bool {
		// 全量重置无条件放行，不比较集号：损坏账本可能写出 0 或负数集号，拿它们与 1 比较会把
		// 这些条目挡在扫描外、悄悄留在「已清空」的账本里。
		return fromEpisode == 1 || num >= fromEpisode
	}

	union := map[int]bool{}
	for num := range entriesByNum {
		union[num] = true
	}
	for num := range fileAliases {
		union[num] = true
	}
	for num := range productNums {
		union[num] = true
	}
	nums := make([]int, 0, len(union))
	for num := range union {
		nums = append(nums, num)
	}
	sort.Ints(nums)

	var plan resetPlan
	for _, num := range nums {
		if !inScope(num) {
			continue
		}
		dupes := entriesByNum[num]
		isConsumed := consumedByStatus[num] || productNums[num]
		for _, dupe := range dupes {
			if HasDownstreamProducts(projectDir, num, dupe) {
				isConsumed = true
				break
			}
		}
		if isConsumed {
			plan.consumed = append(plan.consumed, num)
		}
		paths := fileAliases[num]
		if len(paths) == 0 {
			continue
		}
		// 是否可删除取该集号全部重复条目的可重造性交集：任一条目无法证明可重造都按不可重造
		// 处理——删除不可逆，证据冲突时偏保守。
		canRecreate := len(dupes) > 0
		for _, dupe := range dupes {
			if !hasRecreatableSourceRange(dupe) {
				canRecreate = false
				break
			}
		}
		sort.Strings(paths)
		if canRecreate {
			plan.deletes = append(plan.deletes, paths...)
		} else {
			plan.archives = append(plan.archives, paths...)
		}
	}
	for num := range entriesByNum {
		if inScope(num) {
			plan.episodeNums = append(plan.episodeNums, num)
		}
	}
	sort.Ints(plan.episodeNums)
	return plan
}

// applyResetFiles 落盘文件处置：删派生集文件、留底非派生集文件、清理余文文件。
//
// 失败一律中止提交（账本写回随之回滚）：残留的集文件会被孤儿条目登记重新认领成账本条目，
// 「重置后账本为空」的承诺会被悄悄推翻。重置本身幂等，重跑即可自愈。
func applyResetFiles(projectDir string, plan resetPlan) ([]string, [][2]string, error) {
	sourceDir := filepath.Join(projectDir, "source")
	// source/ 是符号链接时拒绝处置：这类 reparse point 会让其下路径解析穿透到项目外目录。
	// 边界：Windows 目录联接（junction）不置 ModeSymlink，本检查在 Windows 上只覆盖符号
	// 链接，不覆盖联接。
	if info, err := os.Lstat(sourceDir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, &EpisodeResetError{Message: "source/ 不能是符号链接，拒绝处置派生集文件"}
	}
	var (
		deleted  []string
		archived [][2]string
	)
	for _, path := range plan.deletes {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, nil, &EpisodeResetError{
				Message: fmt.Sprintf("派生集文件删除失败，重置已中止：%s: %v", filepath.Base(path), err)}
		}
		deleted = append(deleted, relPosix(projectDir, path))
	}
	for _, path := range plan.archives {
		target := archivePath(path)
		if err := os.Rename(path, target); err != nil {
			return nil, nil, &EpisodeResetError{
				Message: fmt.Sprintf("集文件留底改名失败，重置已中止：%s: %v", filepath.Base(path), err)}
		}
		archived = append(archived, [2]string{relPosix(projectDir, path), relPosix(projectDir, target)})
	}
	// 余文文件是旧拆分流程的进度指针，账本游标已取代它；随重置一并清理。
	remaining := filepath.Join(sourceDir, "_remaining.txt")
	if info, err := os.Stat(remaining); err == nil && !info.IsDir() {
		if err := os.Remove(remaining); err != nil {
			return nil, nil, &EpisodeResetError{
				Message: fmt.Sprintf("余文文件清理失败，重置已中止：%s: %v", filepath.Base(remaining), err)}
		}
	}
	return deleted, archived, nil
}

// relPosix 取相对项目根的 POSIX 路径。
func relPosix(projectDir, path string) string {
	rel, err := filepath.Rel(projectDir, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

// resolvePartialResetCursor 校验部分重置的前置条件，返回重置后 planning_cursor 的
// (source_file, offset)（第 from_episode-1 集原文范围末尾）。
//
// 任一不满足都会让「保留到哪、退回到哪」无法安全推算，报错并指引改用全量重置；调用方保证
// 校验失败时账本不被改动。
func resolvePartialResetCursor(
	projectDir string, pf *domain.ProjectFile, fromEpisode int,
) (string, int, error) {
	fail := func(format string, args ...any) (string, int, error) {
		return "", 0, &EpisodeResetError{Message: fmt.Sprintf(format, args...) + "，" + fullResetHint}
	}
	if pf == nil {
		return fail("项目未加载，无法安全推算部分重置边界")
	}

	entriesByNum := map[int]*domain.EpisodeMeta{}
	for index := range pf.Episodes {
		entry := &pf.Episodes[index]
		num, ok := ParseEpisodeNum(entry.Episode)
		if !ok {
			return fail("账本存在无法解析集号的条目，无法安全推算部分重置边界")
		}
		if num < 1 {
			return fail("账本存在非法集号 %d（须为正整数），无法安全推算部分重置边界", num)
		}
		if _, dup := entriesByNum[num]; dup {
			return fail("账本存在重复集号 %d，无法安全推算部分重置边界", num)
		}
		entriesByNum[num] = entry
	}

	if _, ok := entriesByNum[fromEpisode]; !ok {
		nums := make([]int, 0, len(entriesByNum))
		for num := range entriesByNum {
			nums = append(nums, num)
		}
		sort.Ints(nums)
		return "", 0, &EpisodeResetError{Message: fmt.Sprintf(
			"第 %d 集不在账本中，无法从此处部分重置（当前已规划集号：%v）", fromEpisode, nums)}
	}
	// 只扫描已存在的条目找缺口，不物化 [1, from_episode)：损坏账本可能把集号写成天文数字，
	// 物化整段区间会让本应快速拒绝的逃生口自己先耗尽内存。
	expected := 1
	less := make([]int, 0, len(entriesByNum))
	for num := range entriesByNum {
		if num < fromEpisode {
			less = append(less, num)
		}
	}
	sort.Ints(less)
	for _, num := range less {
		if num != expected {
			break
		}
		expected++
	}
	if expected != fromEpisode {
		return fail("账本缺少第 %d 集起的条目，保留段不连续，无法安全推算部分重置边界", expected)
	}

	retainNum := fromEpisode - 1

	currentSources := DiscoverSources(projectDir)
	mismatched := MismatchedSourceFingerprints(pf.SourceFingerprints, currentSources)
	if len(mismatched) > 0 {
		return "", 0, &EpisodeResetError{Message: fmt.Sprintf(
			"源文件已被修改或移除：%s；账本坐标绑定的是修改前的原文内容，无法安全部分重置，%s",
			strings.Join(mismatched, "、"), fullResetHint)}
	}

	textByRel := map[string]string{}
	sourceOrder := map[string]int{}
	for index, doc := range currentSources {
		textByRel[doc.RelPath] = doc.Text
		sourceOrder[doc.RelPath] = index
	}

	var prevRel string
	var prevEnd int
	hasPrev := false
	for num := 1; num < fromEpisode; num++ {
		entry := entriesByNum[num]
		coords, ok := ParseSourceRange(entry)
		if !ok {
			// 无可信坐标（缺失或结构不完整），切断连续性比较。
			hasPrev = false
			continue
		}
		rel, start, end := coords.SourceFile, coords.Start, coords.End
		text, has := textByRel[rel]
		if !has || start < 0 || start >= end || end > ledgerRuneLen(text) {
			length := 0
			if has {
				length = ledgerRuneLen(text)
			}
			return fail("第 %d 集原文范围无效（源文件 %s 当前长度 %d，记录范围 [%d, %d)），无法安全部分重置",
				num, rel, length, start, end)
		}
		if !hasPrev {
			if num != 1 {
				return fail("第 %d 集之前存在没有原文范围记录的条目，无法确认原文范围是否与保留段其余部分衔接，无法安全部分重置", num)
			}
			relIdx, hasIdx := sourceOrder[rel]
			headHasContent := false
			for i := 0; hasIdx && i < relIdx; i++ {
				if strings.TrimSpace(currentSources[i].Text) != "" {
					headHasContent = true
					break
				}
			}
			if start != 0 || !hasIdx || headHasContent {
				return fail("第 1 集原文范围未从源文件起点开始（记录范围 [%d, %d) @ %s，其前源文件仍有非空白内容），账本可能已损坏，无法安全部分重置",
					start, end, rel)
			}
		} else {
			expectedStart := 0
			if rel == prevRel {
				expectedStart = prevEnd
			} else {
				prevIdx, hasPrevIdx := sourceOrder[prevRel]
				relIdx, hasRelIdx := sourceOrder[rel]
				prevText, hasPrevText := textByRel[prevRel]
				skippedHasContent := false
				if hasPrevIdx && hasRelIdx && relIdx > prevIdx {
					for i := prevIdx + 1; i < relIdx; i++ {
						if strings.TrimSpace(currentSources[i].Text) != "" {
							skippedHasContent = true
							break
						}
					}
				}
				tail := ""
				if hasPrevText {
					if sliced, ok := ledgerSlice(prevText, prevEnd, ledgerRuneLen(prevText)); ok {
						tail = sliced
					}
				}
				if !hasPrevIdx || !hasRelIdx || relIdx <= prevIdx || !hasPrevText ||
					strings.TrimSpace(tail) != "" || skippedHasContent {
					return fail("第 %d 集切换源文件不合法（应在 %s 及其间源文件耗尽后顺序切到下一有内容的源文件），账本可能已损坏，无法安全部分重置",
						num, prevRel)
				}
			}
			if start != expectedStart {
				return fail("第 %d 集原文范围与前一集不连续（应从 %d 起，实际 %d），账本可能已损坏，无法安全部分重置",
					num, expectedStart, start)
			}
		}
		prevRel, prevEnd, hasPrev = rel, end, true
	}

	if !hasPrev {
		return fail("第 %d 集缺少可信的原文范围记录（source_range），无法确定部分重置后的规划起点", retainNum)
	}
	return prevRel, prevEnd, nil
}

// EpisodeStore 是分集账本工具所需的最小项目能力（*service.ProjectStore 满足）。
type EpisodeStore interface {
	Get(name string) (*domain.ProjectFile, error)
	Dir(name string) (string, error)
	Update(name string, mutate func(*domain.ProjectFile) error) error
}

// ResetEpisodePlanning 重置分集规划账本。
//
// 返回 (结果, 需确认, error)：需确认非 nil 时未发生任何写入。from_episode=1 全量、>1 部分。
//
// 迁移边界：Python 侧在原始 dict 上操作，故账本被写坏成非列表等极端损坏形状时全量重置仍能
// 执行成功；Go 侧经域结构体读项目，这类形状在加载期即被拒，需先手工修复 project.json。
func ResetEpisodePlanning(
	store EpisodeStore, projectName string, fromEpisode int, confirmConsumed bool,
) (*EpisodeResetResult, *ResetConfirmationRequired, error) {
	if fromEpisode < 1 {
		return nil, nil, &EpisodeResetError{Message: fmt.Sprintf("from_episode 必须是正整数，收到 %d", fromEpisode)}
	}
	projectDir, err := store.Dir(projectName)
	if err != nil {
		return nil, nil, err
	}

	// 锁外预扫描只为二段确认与快速失败服务：需要确认或校验不通过时零写入返回，不碰文件。
	project, err := store.Get(projectName)
	if err != nil {
		return nil, nil, err
	}
	if fromEpisode > 1 {
		if _, _, err := resolvePartialResetCursor(projectDir, project, fromEpisode); err != nil {
			return nil, nil, err
		}
	}
	plan := scanResetPlan(projectDir, project, fromEpisode)
	if len(plan.consumed) > 0 && !confirmConsumed {
		archived := make([]string, 0, len(plan.archives))
		for _, path := range plan.archives {
			archived = append(archived, relPosix(projectDir, path))
		}
		sort.Strings(archived)
		return nil, &ResetConfirmationRequired{ConsumedEpisodes: plan.consumed, ArchivedFiles: archived}, nil
	}

	var (
		committed *EpisodeResetResult
		conflict  error
	)
	err = store.Update(projectName, func(pf *domain.ProjectFile) error {
		// 锁内重新校验/重新扫描：确认清单与前置校验都是锁外读取时刻的快照，期间源文件可能被
		// 外部改动、也可能出现清单之外的新消费集。
		cursorRel, cursorOffset := "", 0
		hasCursor := false
		if fromEpisode > 1 {
			rel, offset, err := resolvePartialResetCursor(projectDir, pf, fromEpisode)
			if err != nil {
				return err
			}
			cursorRel, cursorOffset, hasCursor = rel, offset, true
		}
		current := scanResetPlan(projectDir, pf, fromEpisode)
		confirmed := map[int]bool{}
		for _, num := range plan.consumed {
			confirmed[num] = true
		}
		for _, num := range current.consumed {
			if !confirmed[num] {
				conflict = &EpisodeResetConflictError{Message: "重置期间出现新的已消费集，需重新确认后再执行"}
				return conflict
			}
		}

		if hasCursor {
			retained := make([]domain.EpisodeMeta, 0, len(pf.Episodes))
			for _, entry := range pf.Episodes {
				num, ok := ParseEpisodeNum(entry.Episode)
				if ok && num < fromEpisode {
					retained = append(retained, entry)
				}
			}
			sort.SliceStable(retained, func(i, j int) bool {
				left, _ := ParseEpisodeNum(retained[i].Episode)
				right, _ := ParseEpisodeNum(retained[j].Episode)
				return left < right
			})
			pf.Episodes = retained
			pf.PlanningCursor = &domain.PlanningCursor{SourceFile: cursorRel, Offset: cursorOffset}
		} else {
			pf.Episodes = []domain.EpisodeMeta{}
			pf.PlanningCursor = nil
			pf.SourceFingerprints = nil
		}

		deleted, archived, err := applyResetFiles(projectDir, current)
		if err != nil {
			return err
		}
		committed = &EpisodeResetResult{
			RemovedEpisodes:  current.episodeNums,
			DeletedFiles:     deleted,
			ArchivedFiles:    archived,
			ConsumedEpisodes: current.consumed,
		}
		return nil
	})
	if err != nil {
		if conflict != nil && errors.Is(err, conflict) {
			return nil, nil, conflict
		}
		return nil, nil, err
	}
	if committed == nil {
		return nil, nil, &EpisodeResetError{Message: "重置未执行：账本更新回调未被调用"}
	}
	return committed, nil, nil
}
