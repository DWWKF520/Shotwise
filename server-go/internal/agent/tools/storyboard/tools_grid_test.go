package storyboard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain/grid"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/service"
)

// fakeGridSplitter 记录切分调用并按需报错。
type fakeGridSplitter struct {
	err   error
	calls []string
}

func (f *fakeGridSplitter) ApplySplit(
	_ context.Context, _ string, grid *grid.GridGeneration,
) (service.GridSplitResult, error) {
	f.calls = append(f.calls, grid.ID)
	if f.err != nil {
		return service.GridSplitResult{}, f.err
	}
	return service.GridSplitResult{UpdatedSceneIDs: grid.SceneIDs}, nil
}

// newGridFixture 造一个开启宫格的分镜路线项目，剧本含 4 个同组场景。
func newGridFixture(t *testing.T, sceneCount int) (*fakeAssetQueue, *fakeGenerationProjects) {
	t.Helper()
	dir := t.TempDir()
	if err := fs.WriteJSONAtomic(filepath.Join(dir, "project.json"), map[string]any{
		"title": "测试", "content_mode": "narration", "generation_mode": "storyboard",
		"grid_storyboard": true, "style": "水墨", "aspect_ratio": "16:9",
	}); err != nil {
		t.Fatal(err)
	}
	segments := make([]any, 0, sceneCount)
	for i := 1; i <= sceneCount; i++ {
		segments = append(segments, map[string]any{
			"segment_id":            "E1S" + string(rune('0'+i)),
			"duration_seconds":      float64(4),
			"novel_text":            "第" + string(rune('0'+i)) + "段",
			"characters_in_segment": []any{},
			"image_prompt":          "画面" + string(rune('0'+i)),
		})
	}
	script := map[string]any{
		"title": "第1集", "content_mode": "narration", "episode": float64(1),
		"segments": segments,
	}
	return &fakeAssetQueue{}, &fakeGenerationProjects{
		pf: &project.ProjectFile{
			ContentMode: "narration", GenerationMode: "storyboard", Style: "水墨",
			AspectRatio: "16:9", GridStoryboard: true,
		},
		dir:     dir,
		scripts: map[string]map[string]any{"episode_1.json": script},
	}
}

func TestGenerateGridListOnlyDoesNotEnqueue(t *testing.T) {
	q, projects := newGridFixture(t, 4)
	tl, err := NewGenerateGridTool("proj", q, projects, fakeImageCaps{}, &fakeGridSplitter{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json","list_only":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "共 1 个分组：") || !strings.Contains(out, "4 场景") {
		t.Fatalf("分组清单不符: %s", out)
	}
	if !strings.Contains(out, "grid_4 (2×2)") {
		t.Fatalf("档位展示不符: %s", out)
	}
	if len(q.enqueued) != 0 {
		t.Fatalf("预览模式不应入队: %+v", q.enqueued)
	}
	if _, err := os.Stat(filepath.Join(projects.dir, "grids")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(projects.dir, "grids"))
		if len(entries) != 0 {
			t.Fatalf("预览模式不应创建记录: %v", entries)
		}
	}
}

func TestGenerateGridHappyPathCreatesRecordAndSplits(t *testing.T) {
	q, projects := newGridFixture(t, 4)
	splitter := &fakeGridSplitter{}
	tl, err := NewGenerateGridTool("proj", q, projects, fakeImageCaps{}, splitter)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "generate_grid summary: 1 succeeded, 0 failed") {
		t.Fatalf("汇总文案不符: %s", out)
	}
	if !strings.Contains(out, "已切分 4 格") {
		t.Fatalf("应报告切分格数: %s", out)
	}
	if len(q.enqueued) != 1 {
		t.Fatalf("应入队一条宫格任务: %+v", q.enqueued)
	}
	enqueued := q.enqueued[0]
	if enqueued.TaskType != "grid" || enqueued.MediaType != "image" {
		t.Fatalf("任务身份有误: %+v", enqueued)
	}
	for _, key := range []string{"prompt", "script_file", "scene_ids", "grid_size", "rows", "cols",
		"grid_aspect_ratio", "video_aspect_ratio"} {
		if _, ok := enqueued.Payload[key]; !ok {
			t.Fatalf("载荷缺 %s: %+v", key, enqueued.Payload)
		}
	}
	if enqueued.ScriptFile == nil || *enqueued.ScriptFile != "episode_1.json" {
		t.Fatalf("剧本文件名应透传: %+v", enqueued.ScriptFile)
	}
	// 记录已落盘且切分被调用。
	manager, err := service.NewGridManager(projects.dir)
	if err != nil {
		t.Fatal(err)
	}
	grids, err := manager.ListAll()
	if err != nil || len(grids) != 1 {
		t.Fatalf("记录未落盘: %v %v", grids, err)
	}
	if len(splitter.calls) != 1 || splitter.calls[0] != grids[0].ID {
		t.Fatalf("切分未按记录 id 调用: %+v", splitter.calls)
	}
	if grids[0].GridSize != "grid_4" || len(grids[0].SceneIDs) != 4 {
		t.Fatalf("记录内容有误: %+v", grids[0])
	}
}

func TestGenerateGridGatesAndFilters(t *testing.T) {
	// 未开启宫格 → 直接拒绝，不入队也不建记录。
	q, projects := newGridFixture(t, 4)
	projects.pf.GridStoryboard = false
	tl, _ := NewGenerateGridTool("proj", q, projects, fakeImageCaps{}, &fakeGridSplitter{})
	out, _ := tl.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if !strings.Contains(out, "未启用宫格分镜") || len(q.enqueued) != 0 {
		t.Fatalf("未开宫格应被拒: %s", out)
	}

	// 显式 scene_ids 全部不命中 → 报错。
	q2, projects2 := newGridFixture(t, 4)
	tl2, _ := NewGenerateGridTool("proj", q2, projects2, fakeImageCaps{}, &fakeGridSplitter{})
	out, _ = tl2.InvokableRun(context.Background(), `{"script":"episode_1.json","scene_ids":["E9S9"]}`)
	if !strings.Contains(out, "没有匹配的场景组") || !strings.Contains(out, `"is_error":true`) {
		t.Fatalf("显式不命中应报错: %s", out)
	}
	if len(q2.enqueued) != 0 {
		t.Fatal("不命中时不得入队")
	}
}

func TestGenerateGridReportsSplitFailureSeparately(t *testing.T) {
	q, projects := newGridFixture(t, 4)
	splitter := &fakeGridSplitter{err: os.ErrPermission}
	tl, _ := NewGenerateGridTool("proj", q, projects, fakeImageCaps{}, splitter)
	out, _ := tl.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if !strings.Contains(out, "联合图已生成，但切分落格失败") {
		t.Fatalf("切分失败应单独提示: %s", out)
	}
	if !strings.Contains(out, "0 succeeded, 1 failed") {
		t.Fatalf("切分失败应计入失败: %s", out)
	}
}
