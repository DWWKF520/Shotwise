package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
)

// fakeEpisodeStore 是分集账本工具的替身：Get 返回同一指针，Update 就地 mutate。
type fakeEpisodeStore struct {
	pf  *domain.ProjectFile
	dir string
}

func (f *fakeEpisodeStore) Get(string) (*domain.ProjectFile, error) { return f.pf, nil }
func (f *fakeEpisodeStore) Dir(string) (string, error)              { return f.dir, nil }
func (f *fakeEpisodeStore) Update(_ string, mutate func(*domain.ProjectFile) error) error {
	return mutate(f.pf)
}

// fakeCompletion 按序返回预置输出并记录最后一次 prompt / schema。
type fakeCompletion struct {
	outputs    []string
	calls      int
	lastPrompt string
	lastSchema map[string]any
}

func (f *fakeCompletion) Generate(_ context.Context, prompt string, schema map[string]any, _ int) (string, error) {
	f.calls++
	f.lastPrompt = prompt
	f.lastSchema = schema
	if f.calls <= len(f.outputs) {
		return f.outputs[f.calls-1], nil
	}
	return "", errors.New("no more outputs")
}

func TestReadingUnitsByLanguage(t *testing.T) {
	zh := "你好，世界！abc"
	// 你好(2) + 全角逗号(1) + 世界(2) + 全角叹号(1) = 6；abc 不属 CJK 区。
	if got := CountReadingUnits(zh, "zh"); got != 6 {
		t.Fatalf("zh 读数有误: %d", got)
	}
	// en 按词：Python 的 \w 含 CJK 字母，故「你好」「世界」「abc」各算一个词。
	if got := CountReadingUnits(zh, "en"); got != 3 {
		t.Fatalf("en 读数有误: %d", got)
	}
	if got := CountReadingUnits("Hôm nay trời đẹp", "vi"); got != 4 {
		t.Fatalf("vi 读数有误: %d", got)
	}
	if ReadingUnitNoun("en") != "词" || ReadingUnitNoun("zh") != "字" || ReadingUnitNoun("") != "字" {
		t.Fatal("量词映射有误")
	}
	// 未知语言按 zh 路径。
	if got := CountReadingUnits("你好", "de"); got != 2 {
		t.Fatalf("未知语言应走 zh 路径: %d", got)
	}
}

func TestFindReadingUnitOffset(t *testing.T) {
	text := "你好世界啊" // 5 个汉字
	if got := FindReadingUnitOffset(text, 3, "zh"); got != 3 {
		t.Fatalf("zh 偏移有误: %d", got)
	}
	if got := FindReadingUnitOffset(text, 99, "zh"); got != 5 {
		t.Fatalf("单位不足应返回总长: %d", got)
	}
	if got := FindReadingUnitOffset(text, 0, "zh"); got != 0 {
		t.Fatalf("target<=0 应返回 0: %d", got)
	}
	if got := FindReadingUnitOffset("don't stop", 1, "en"); got != 3 {
		t.Fatalf("en 词边界偏移有误: %d", got)
	}
}

func TestFoldForMatchPreservesLength(t *testing.T) {
	text := "A，B。\tC   D"
	folded := string(foldForMatch(text))
	if len([]rune(folded)) != len([]rune(text)) {
		t.Fatalf("折叠必须逐字符等长: %q", folded)
	}
	if !strings.Contains(folded, ",") || !strings.Contains(folded, ".") {
		t.Fatalf("全角标点未折叠: %q", folded)
	}
	// 连续空白不压缩。
	if strings.Count(folded, " ") != 4 {
		t.Fatalf("空白折叠应保留个数: %q", folded)
	}
}

func TestResolveBoundariesChecks(t *testing.T) {
	window := "甲乙丙丁戊己庚辛壬癸"
	drafts := []planningDraftEpisode{
		{Title: "一", Hook: "h", EndAnchor: "丁戊"},
		{Title: "二", Hook: "h", EndAnchor: "庚辛"},
	}
	ends, err := resolveBoundaries(window, drafts, false)
	if err != nil {
		t.Fatalf("应通过: %v", err)
	}
	if len(ends) != 2 || ends[0] != 5 || ends[1] != 8 {
		t.Fatalf("边界解析有误: %v", ends)
	}

	// 锚点不存在。
	_, err = resolveBoundaries(window, []planningDraftEpisode{{Title: "一", Hook: "h", EndAnchor: "子丑"}}, false)
	var rejected *draftRejected
	if !errors.As(err, &rejected) || !strings.Contains(rejected.Reasons[0], "不存在") {
		t.Fatalf("缺失锚点应被拒: %v", err)
	}

	// 锚点多次出现（窗口里重复 "甲乙"）。
	dupWindow := "甲乙甲乙丙"
	_, err = resolveBoundaries(dupWindow, []planningDraftEpisode{{Title: "一", Hook: "h", EndAnchor: "甲乙"}}, false)
	if !errors.As(err, &rejected) || !strings.Contains(rejected.Reasons[0], "无法唯一定位") {
		t.Fatalf("重复锚点应被拒: %v", err)
	}

	// 顺序倒置：第二条锚点在第一条之前。
	_, err = resolveBoundaries(window, []planningDraftEpisode{
		{Title: "一", Hook: "h", EndAnchor: "庚辛"},
		{Title: "二", Hook: "h", EndAnchor: "丁戊"},
	}, false)
	if !errors.As(err, &rejected) || !strings.Contains(rejected.Reasons[0], "不在上一集结尾之后") {
		t.Fatalf("倒序应被拒: %v", err)
	}

	// 命中全文结尾且尾部只剩空白 → 贴齐到窗口末尾。
	ends, err = resolveBoundaries("甲乙丙丁  ", []planningDraftEpisode{{Title: "一", Hook: "h", EndAnchor: "丙丁"}}, true)
	if err != nil {
		t.Fatalf("应通过: %v", err)
	}
	if ends[0] != 6 {
		t.Fatalf("尾部空白应贴齐窗口末尾: %v", ends)
	}
}

func TestResolveBoundariesFoldsFullWidthPunctuation(t *testing.T) {
	// 窗口用半角，锚点用全角 → 精确匹配落空，折叠匹配命中。
	window := "他说:hello world"
	ends, err := resolveBoundaries(window, []planningDraftEpisode{
		{Title: "一", Hook: "h", EndAnchor: "他说：hello"},
	}, false)
	if err != nil {
		t.Fatalf("折叠匹配应命中: %v", err)
	}
	if ends[0] != 8 {
		t.Fatalf("折叠匹配偏移有误: %v", ends)
	}
}

func TestParsePlanDraftRejectsBadShapes(t *testing.T) {
	if _, err := parsePlanDraft("not json", false); err == nil {
		t.Fatal("非 JSON 应被拒")
	}
	if _, err := parsePlanDraft(`{"episodes":[]}`, false); err == nil {
		t.Fatal("空 episodes 应被拒")
	}
	if _, err := parsePlanDraft(`{"episodes":[{"title":"t","hook":"h","end_anchor":"ab","extra":1}]}`, false); err == nil {
		t.Fatal("多余字段应被拒")
	}
	if _, err := parsePlanDraft(`{"episodes":[{"title":"t","hook":"h","end_anchor":"a"}]}`, false); err == nil {
		t.Fatal("end_anchor 过短应被拒")
	}
	// drama 形态缺 story_beats。
	if _, err := parsePlanDraft(`{"episodes":[{"title":"t","hook":"h","end_anchor":"ab"}]}`, true); err == nil {
		t.Fatal("drama 缺 story_beats 应被拒")
	}
	// 代码栅栏包裹的合法输出应放行。
	drafts, err := parsePlanDraft("```json\n{\"episodes\":[{\"title\":\"t\",\"hook\":\"h\",\"end_anchor\":\"ab\"}]}\n```", false)
	if err != nil || len(drafts) != 1 {
		t.Fatalf("带栅栏输出应放行: %v %v", drafts, err)
	}
}

func TestEpisodePlannerPlanEndToEnd(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 20 个汉字，锚点唯一定位到第 12 个字符处。
	const text = "甲乙丙丁戊己庚辛壬癸子丑寅卯辰巳午未申酉"
	if err := os.WriteFile(filepath.Join(dir, "source", "novel.txt"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	pf := &domain.ProjectFile{
		Title: "测试", ContentMode: "narration", SourceLanguage: "zh",
		SourceKind: "novel", Episodes: []domain.EpisodeMeta{},
	}
	store := &fakeEpisodeStore{pf: pf, dir: dir}
	completion := &fakeCompletion{outputs: []string{
		`{"episodes":[{"title":"第一集","hook":"留个悬念","end_anchor":"子丑寅卯"}]}`,
	}}
	planner, err := NewEpisodePlanner(store, "p1", completion)
	if err != nil {
		t.Fatal(err)
	}
	result, err := planner.Plan(context.Background(), "每章一集")
	if err != nil {
		t.Fatalf("规划失败: %v", err)
	}
	if len(result.Episodes) != 1 {
		t.Fatalf("应规划 1 集: %+v", result.Episodes)
	}
	summary := result.Episodes[0]
	if summary.Episode != 1 || summary.Title != "第一集" || summary.ReadingUnits != 14 ||
		summary.LedgerStatus != domain.LedgerStatusPlanned {
		t.Fatalf("摘要有误: %+v", summary)
	}
	if len(pf.Episodes) != 1 {
		t.Fatalf("账本应有 1 条: %+v", pf.Episodes)
	}
	entry := pf.Episodes[0]
	if entry.SourceRange == nil || entry.SourceRange.Start != 0 || entry.SourceRange.End != 14 ||
		entry.SourceRange.SourceFile != "source/novel.txt" {
		t.Fatalf("source_range 有误: %+v", entry.SourceRange)
	}
	if pf.PlanningCursor == nil || pf.PlanningCursor.Offset != 14 ||
		pf.PlanningCursor.SourceFile != "source/novel.txt" {
		t.Fatalf("cursor 有误: %+v", pf.PlanningCursor)
	}
	if len(pf.SourceFingerprints) != 1 {
		t.Fatalf("指纹应记录: %+v", pf.SourceFingerprints)
	}
	derived, err := os.ReadFile(filepath.Join(dir, "source", "episode_1.txt"))
	if err != nil {
		t.Fatalf("派生集文件应落盘: %v", err)
	}
	if string(derived) != "甲乙丙丁戊己庚辛壬癸子丑寅卯" {
		t.Fatalf("派生内容应等于原文切片: %q", derived)
	}
	// instructions 非空 → prompt 注入用户意见与全局进度。
	if !strings.Contains(completion.lastPrompt, "# 用户意见") ||
		!strings.Contains(completion.lastPrompt, "每章一集") ||
		!strings.Contains(completion.lastPrompt, "# 全局进度") {
		t.Fatalf("prompt 缺注入分节:\n%s", completion.lastPrompt)
	}
	if completion.lastSchema == nil {
		t.Fatal("应要求结构化输出")
	}
}

func TestEpisodePlannerRejectsMissingSourceRange(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source", "novel.txt"), []byte("甲乙丙丁"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 已有条目缺 source_range（老项目遗留）→ 拒绝规划并指路全量重置。
	pf := &domain.ProjectFile{
		ContentMode: "narration",
		Episodes:    []domain.EpisodeMeta{{Episode: 1, Title: "旧", ScriptFile: "scripts/episode_1.json"}},
	}
	planner, err := NewEpisodePlanner(&fakeEpisodeStore{pf: pf, dir: dir}, "p1", &fakeCompletion{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = planner.Plan(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "reset_episode_planning") {
		t.Fatalf("缺位置记录应被拒并指路重置: %v", err)
	}
}

func TestEpisodePlannerRejectsChangedSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source", "novel.txt"), []byte("甲乙丙丁"), 0o644); err != nil {
		t.Fatal(err)
	}
	pf := &domain.ProjectFile{ContentMode: "narration", Episodes: []domain.EpisodeMeta{}}
	planner, err := NewEpisodePlanner(&fakeEpisodeStore{pf: pf, dir: dir}, "p1", &fakeCompletion{})
	if err != nil {
		t.Fatal(err)
	}
	// 首次规划补记指纹。
	if _, err := planner.Plan(context.Background(), ""); err == nil {
		// 无可用输出 → 报错；但指纹补记发生在成功提交路径，故此处改用显式指纹基线验证：
	}
	pf.SourceFingerprints = map[string]string{"source/novel.txt": "deadbeef"}
	// 源文已变（指纹不符）→ 拒绝。
	err = planner.checkSourceFingerprints(pf, nil)
	if err == nil || !strings.Contains(err.Error(), "reset_episode_planning") {
		t.Fatalf("指纹不符应被拒: %v", err)
	}
}

func TestResetPlanningFullAndPartial(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source", "novel.txt"), []byte("甲乙丙丁戊己庚辛"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 两个派生集文件：episode_1 有坐标（可重造，删除）；episode_2 无坐标（改名留底）。
	if err := os.WriteFile(filepath.Join(dir, "source", "episode_1.txt"), []byte("甲乙丙丁"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source", "episode_2.txt"), []byte("手工内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	pf := &domain.ProjectFile{
		ContentMode: "narration",
		Episodes: []domain.EpisodeMeta{
			{Episode: 1, Title: "一", ScriptFile: "scripts/episode_1.json",
				SourceRange:  &domain.SourceRange{SourceFile: "source/novel.txt", Start: 0, End: 4},
				LedgerStatus: domain.LedgerStatusPlanned},
			{Episode: 2, Title: "二", ScriptFile: "scripts/episode_2.json"},
		},
		PlanningCursor:     &domain.PlanningCursor{SourceFile: "source/novel.txt", Offset: 4},
		SourceFingerprints: ComputeSourceFingerprints(DiscoverSources(dir)),
	}
	store := &fakeEpisodeStore{pf: pf, dir: dir}

	// 部分重置 from_episode=2（ep2 无坐标 → 保留段只有 ep1；起始集存在且连续）。
	result, confirmation, err := ResetEpisodePlanning(store, "p1", 2, false)
	if err != nil || confirmation != nil {
		t.Fatalf("部分重置应成功: %v %v", err, confirmation)
	}
	if len(result.RemovedEpisodes) != 1 || result.RemovedEpisodes[0] != 2 {
		t.Fatalf("应清空第 2 集: %+v", result)
	}
	if len(pf.Episodes) != 1 || pf.Episodes[0].Episode != 1 {
		t.Fatalf("应保留第 1 集: %+v", pf.Episodes)
	}
	if pf.PlanningCursor == nil || pf.PlanningCursor.Offset != 4 {
		t.Fatalf("游标应退到保留集末尾: %+v", pf.PlanningCursor)
	}
	// 部分重置只处置 from_episode 起的范围：第 1 集的派生文件不受影响；第 2 集无坐标 →
	// 改名留底而非删除。
	if len(result.DeletedFiles) != 0 {
		t.Fatalf("保留段的派生文件不应被处置: %+v", result.DeletedFiles)
	}
	if len(result.ArchivedFiles) != 1 ||
		result.ArchivedFiles[0][0] != "source/episode_2.txt" ||
		result.ArchivedFiles[0][1] != "source/_episode_2.txt.bak" {
		t.Fatalf("无坐标的派生文件应改名留底: %+v", result.ArchivedFiles)
	}
	if _, err := os.Stat(filepath.Join(dir, "source", "_episode_2.txt.bak")); err != nil {
		t.Fatalf("留底文件应在: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "source", "episode_1.txt")); err != nil {
		t.Fatalf("保留段的派生文件应仍在: %v", err)
	}

	// 全量重置：清空账本与指纹，第 1 集有坐标 → 派生文件删除。
	result, confirmation, err = ResetEpisodePlanning(store, "p1", 1, false)
	if err != nil || confirmation != nil {
		t.Fatalf("全量重置应成功: %v %v", err, confirmation)
	}
	if len(pf.Episodes) != 0 || pf.PlanningCursor != nil || pf.SourceFingerprints != nil {
		t.Fatalf("全量重置应清空账本/游标/指纹: %+v", pf)
	}
	if len(result.RemovedEpisodes) != 1 || result.RemovedEpisodes[0] != 1 {
		t.Fatalf("应清空残留的第 1 集: %+v", result)
	}
	if len(result.DeletedFiles) != 1 || result.DeletedFiles[0] != "source/episode_1.txt" {
		t.Fatalf("有坐标的派生文件应删除: %+v", result.DeletedFiles)
	}
}

func TestResetPlanningRequiresConfirmationForConsumed(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source", "novel.txt"), []byte("甲乙丙丁"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 已有剧本产物 → 该集被视为已消费。
	if err := os.WriteFile(filepath.Join(dir, "scripts", "episode_1.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	pf := &domain.ProjectFile{
		ContentMode: "narration",
		Episodes: []domain.EpisodeMeta{{Episode: 1, Title: "一", ScriptFile: "scripts/episode_1.json",
			SourceRange: &domain.SourceRange{SourceFile: "source/novel.txt", Start: 0, End: 4}}},
	}
	store := &fakeEpisodeStore{pf: pf, dir: dir}
	result, confirmation, err := ResetEpisodePlanning(store, "p1", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if result != nil || confirmation == nil {
		t.Fatalf("应要求确认且不改动: %+v %+v", result, confirmation)
	}
	if len(confirmation.ConsumedEpisodes) != 1 || confirmation.ConsumedEpisodes[0] != 1 {
		t.Fatalf("应指名已消费集: %+v", confirmation)
	}
	if len(pf.Episodes) != 1 {
		t.Fatal("未确认时不得改动账本")
	}

	// 确认后执行。
	result, confirmation, err = ResetEpisodePlanning(store, "p1", 1, true)
	if err != nil || confirmation != nil || result == nil {
		t.Fatalf("确认后应执行: %v %v %v", err, confirmation, result)
	}
	if len(pf.Episodes) != 0 {
		t.Fatalf("确认后应清空账本: %+v", pf.Episodes)
	}
}
