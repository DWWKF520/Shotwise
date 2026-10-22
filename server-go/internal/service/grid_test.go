package service

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// newGridProject 造一个 narration 项目，含两名分镜的可用剧本。
func newGridProject(t *testing.T) (*ProjectStore, string) {
	t.Helper()
	store := NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "测试项目", domain.ContentNarration, domain.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	dir, err := store.Dir("p1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := map[string]any{
		"title": "第1集", "content_mode": "narration", "episode": float64(1),
		"segments": []any{
			map[string]any{
				"segment_id": "E1S1", "duration_seconds": float64(4), "novel_text": "第一段",
				"characters_in_segment": []any{}, "image_prompt": "甲",
			},
			map[string]any{
				"segment_id": "E1S2", "duration_seconds": float64(4), "novel_text": "第二段",
				"characters_in_segment": []any{}, "image_prompt": "乙",
			},
		},
	}
	if err := fs.WriteJSONAtomic(filepath.Join(dir, "scripts", "episode_1.json"), script); err != nil {
		t.Fatal(err)
	}
	return store, dir
}

// writeSolidPNG 生成一张纯色 PNG（切分测试只需可解码的真实图像）。
func writeSolidPNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 128, A: 255})
		}
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, img); err != nil {
		t.Fatal(err)
	}
}

func TestGridManagerRejectsBadIDAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	manager, err := NewGridManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ImagePath("grid_ZZZZZZZZZZZZ"); err == nil {
		t.Fatal("非法 id 应被拒")
	}

	grid := domain.NewGridGeneration(1, "episode_1.json", []string{"E1S1"}, 2, 2, "grid_4", "p", "m", "16:9", nil)
	if err := manager.Save(grid); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := manager.Get(grid.ID)
	if err != nil || !ok {
		t.Fatalf("读取记录失败: %v %v", ok, err)
	}
	if loaded.ID != grid.ID || loaded.GridSize != "grid_4" || len(loaded.FrameChain) != 4 {
		t.Fatalf("记录内容有误: %+v", loaded)
	}
	all, err := manager.ListAll()
	if err != nil || len(all) != 1 {
		t.Fatalf("列记录有误: %v %v", all, err)
	}
	deleted, err := manager.Delete(grid.ID)
	if err != nil || !deleted {
		t.Fatalf("删除失败: %v %v", deleted, err)
	}
	if _, ok, _ := manager.Get(grid.ID); ok {
		t.Fatal("删除后不应还能读到")
	}
}

func TestApplyGridSplitWritesCellsAndScript(t *testing.T) {
	store, dir := newGridProject(t)
	manager, err := NewGridManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	grid := domain.NewGridGeneration(1, "episode_1.json", []string{"E1S1", "E1S2"}, 2, 2, "grid_4", "p", "m", "16:9", nil)
	if err := manager.Save(grid); err != nil {
		t.Fatal(err)
	}
	imagePath, err := manager.ImagePath(grid.ID)
	if err != nil {
		t.Fatal(err)
	}
	writeSolidPNG(t, imagePath, 400, 400)
	rel := "grids/" + grid.ID + ".png"
	grid.GridImagePath = &rel

	result, err := NewGridService(store).ApplySplit(context.Background(), "p1", grid)
	if err != nil {
		t.Fatalf("切分失败: %v", err)
	}
	if len(result.UpdatedSceneIDs) != 2 || result.UpdatedSceneIDs[0] != "E1S1" || result.UpdatedSceneIDs[1] != "E1S2" {
		t.Fatalf("落格分镜有误: %+v", result)
	}
	if len(result.MissingSceneIDs) != 0 {
		t.Fatalf("不应有缺失分镜: %+v", result.MissingSceneIDs)
	}
	for _, id := range result.UpdatedSceneIDs {
		cell := filepath.Join(dir, "storyboards", "scene_"+id+".png")
		if info, err := os.Stat(cell); err != nil || info.Size() == 0 {
			t.Fatalf("单元格未落盘: %s %v", cell, err)
		}
	}

	// 剧本回写：三件套（storyboard_image / grid_id / grid_cell_index）。
	script, err := store.ReadScriptJSON("p1", "episode_1.json")
	if err != nil {
		t.Fatal(err)
	}
	items, idField, _, err := domain.ResolveScriptItems(script)
	if err != nil {
		t.Fatal(err)
	}
	for index, item := range items {
		entry := item.(map[string]any)
		assets, ok := entry["generated_assets"].(map[string]any)
		if !ok {
			t.Fatalf("generated_assets 缺失: %+v", entry)
		}
		sceneID := entry[idField].(string)
		if assets["storyboard_image"] != "storyboards/scene_"+sceneID+".png" {
			t.Fatalf("storyboard_image 回写有误: %+v", assets)
		}
		if assets["grid_id"] != grid.ID {
			t.Fatalf("grid_id 回写有误: %+v", assets)
		}
		if got, ok := assets["grid_cell_index"].(float64); !ok || int(got) != index {
			t.Fatalf("grid_cell_index 应随帧链下标写入: %+v", assets)
		}
	}

	// split_at 写入并落盘。
	if grid.SplitAt == nil {
		t.Fatal("切分后应写入 split_at")
	}
	reloaded, ok, err := manager.Get(grid.ID)
	if err != nil || !ok || reloaded.SplitAt == nil {
		t.Fatalf("切分状态未落盘: %+v %v %v", reloaded, ok, err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "versions", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"source": "grid_split"`) || !strings.Contains(string(raw), grid.ID) {
		t.Fatalf("版本记录缺切分标记: %s", raw)
	}
}

func TestApplyGridSplitSkipsMissingScenes(t *testing.T) {
	store, dir := newGridProject(t)
	manager, err := NewGridManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 帧链里放进一个剧本已不存在的分镜（规划后 agent 改过剧本的情形）。
	grid := domain.NewGridGeneration(1, "episode_1.json", []string{"E1S1", "E9S9"}, 2, 2, "grid_4", "p", "m", "16:9", nil)
	if err := manager.Save(grid); err != nil {
		t.Fatal(err)
	}
	imagePath, _ := manager.ImagePath(grid.ID)
	writeSolidPNG(t, imagePath, 400, 400)
	rel := "grids/" + grid.ID + ".png"
	grid.GridImagePath = &rel

	result, err := NewGridService(store).ApplySplit(context.Background(), "p1", grid)
	if err != nil {
		t.Fatalf("切分失败: %v", err)
	}
	if len(result.UpdatedSceneIDs) != 1 || result.UpdatedSceneIDs[0] != "E1S1" {
		t.Fatalf("只应落格存在的分镜: %+v", result)
	}
	if len(result.MissingSceneIDs) != 1 || result.MissingSceneIDs[0] != "E9S9" {
		t.Fatalf("缺失分镜应被收集: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "storyboards", "scene_E9S9.png")); err == nil {
		t.Fatal("缺失分镜不应落盘")
	}
}

func TestApplyGridSplitRequiresComposite(t *testing.T) {
	store, dir := newGridProject(t)
	manager, err := NewGridManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	grid := domain.NewGridGeneration(1, "episode_1.json", []string{"E1S1"}, 2, 2, "grid_4", "p", "m", "16:9", nil)
	if err := manager.Save(grid); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGridService(store).ApplySplit(context.Background(), "p1", grid); err == nil {
		t.Fatal("尚无联合图应报错")
	}
}

