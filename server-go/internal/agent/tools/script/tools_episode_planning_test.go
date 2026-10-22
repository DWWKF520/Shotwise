package script

import "github.com/shotwise/shotwise/internal/agent/tools"

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
)

// fakeTextCompletion 按序返回预置输出。
type fakeTextCompletion struct {
	outputs []string
	calls   int
}

func (f *fakeTextCompletion) Generate(_ context.Context, _ string, _ map[string]any, _ int) (string, error) {
	f.calls++
	if f.calls <= len(f.outputs) {
		return f.outputs[f.calls-1], nil
	}
	return "", errors.New("no more outputs")
}

// newEpisodePlanningFixture 造一个含一集已消费集的账本项目。
func newEpisodePlanningFixture(t *testing.T) (*fakeGenerationProjects, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source", "novel.txt"), []byte("甲乙丙丁戊己庚辛"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 已有剧本产物 → 该集被视为已消费（重置需确认）。
	if err := os.WriteFile(filepath.Join(dir, "scripts", "episode_1.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	pf := &domain.ProjectFile{
		Title: "测试", ContentMode: "narration", SourceLanguage: "zh",
		Episodes: []domain.EpisodeMeta{{
			Episode: 1, Title: "一", ScriptFile: "scripts/episode_1.json",
			SourceRange:  &domain.SourceRange{SourceFile: "source/novel.txt", Start: 0, End: 4},
			LedgerStatus: domain.LedgerStatusConsumed,
		}},
		PlanningCursor: &domain.PlanningCursor{SourceFile: "source/novel.txt", Offset: 4},
	}
	return &fakeGenerationProjects{pf: pf, dir: dir}, dir
}

func TestResetEpisodePlanningToolTwoStageConfirmation(t *testing.T) {
	projects, _ := newEpisodePlanningFixture(t)
	tl, err := NewResetEpisodePlanningTool("p1", projects)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"from_episode":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "会波及已消费集") || !strings.Contains(out, "第 1 集") ||
		!strings.Contains(out, "尚未执行任何改动") {
		t.Fatalf("应要求确认: %s", out)
	}
	if len(projects.pf.Episodes) != 1 {
		t.Fatal("未确认时不得改动账本")
	}

	out, err = tl.InvokableRun(context.Background(), `{"from_episode":1,"confirm_consumed":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "已全量重置分集规划") || !strings.Contains(out, "planning_cursor 已置空") {
		t.Fatalf("确认后应执行: %s", out)
	}
	if len(projects.pf.Episodes) != 0 {
		t.Fatalf("应清空账本: %+v", projects.pf.Episodes)
	}

	// 非正整数入参被拒。
	out, err = tl.InvokableRun(context.Background(), `{"from_episode":0}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "必须是正整数") || !strings.Contains(out, `"is_error":true`) {
		t.Fatalf("非法入参应被拒: %s", out)
	}
}

func TestPlanEpisodesToolReportsSummaryAndLedgerStats(t *testing.T) {
	projects, _ := newEpisodePlanningFixture(t)
	// 清空账本与上游产物，走首次规划路径。
	projects.pf.Episodes = []domain.EpisodeMeta{}
	projects.pf.PlanningCursor = nil
	if err := os.Remove(filepath.Join(projects.dir, "scripts", "episode_1.json")); err != nil {
		t.Fatal(err)
	}
	completion := &fakeTextCompletion{outputs: []string{
		`{"episodes":[{"title":"第一集","hook":"留个悬念","end_anchor":"丙丁"}]}`,
	}}
	factory := func(context.Context, *domain.ProjectFile) (service.TextCompletion, error) {
		return completion, nil
	}
	tl, err := NewPlanEpisodesTool("p1", projects, factory)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"instructions":"按章节对齐"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "✅ 已规划 1 集：") || !strings.Contains(out, "《第一集》") ||
		!strings.Contains(out, "累计已规划 1 集。") {
		t.Fatalf("摘要渲染有误: %s", out)
	}
	if len(projects.pf.Episodes) != 1 {
		t.Fatalf("账本应写入 1 条: %+v", projects.pf.Episodes)
	}

	// instructions 超长 → 参数错误。
	out, err = tl.InvokableRun(context.Background(),
		`{"instructions":"`+strings.Repeat("字", tools.MaxInstructionsLen+1)+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "instructions 过长") || !strings.Contains(out, `"is_error":true`) {
		t.Fatalf("超长入参应被拒: %s", out)
	}

	// 未注入依赖 → deny。
	bare, _ := NewPlanEpisodesTool("p1", projects, nil)
	out, err = bare.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "episode planning not configured") {
		t.Fatalf("未注入文本生成应 deny: %s", out)
	}
}

func TestEpisodePlanningToolsDenyWithoutStore(t *testing.T) {
	tl, err := NewResetEpisodePlanningTool("p1", nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"from_episode":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "episode planning not configured") {
		t.Fatalf("未注入项目依赖应 deny: %s", out)
	}
}
