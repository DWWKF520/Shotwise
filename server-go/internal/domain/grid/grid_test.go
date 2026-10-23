package grid

import (
	"encoding/json"
	"github.com/shotwise/shotwise/internal/domain/project"
	"strings"
	"testing"
)

func TestCalculateGridLayoutLadder(t *testing.T) {
	cases := []struct {
		scenes         int
		aspect         string
		allowLarge     bool
		wantGridSize   string
		wantRows       int
		wantCells      int
		wantPlaceolder int
	}{
		{3, "16:9", false, "grid_4", 2, 4, 1},
		{4, "16:9", false, "grid_4", 2, 4, 0},
		{5, "9:16", false, "grid_9", 3, 9, 4},
		{9, "9:16", false, "grid_9", 3, 9, 0},
		// 未放行大宫格时 20 个场景封顶到 3×3。
		{20, "16:9", false, "grid_9", 3, 9, 0},
		{20, "16:9", true, "grid_25", 5, 25, 5},
		{16, "16:9", true, "grid_16", 4, 16, 0},
	}
	for _, tc := range cases {
		layout, ok := CalculateGridLayout(tc.scenes, tc.aspect, tc.allowLarge)
		if !ok {
			t.Fatalf("scenes=%d 应有布局", tc.scenes)
		}
		if layout.GridSize != tc.wantGridSize || layout.Rows != tc.wantRows || layout.Cols != tc.wantRows {
			t.Fatalf("scenes=%d → %+v", tc.scenes, layout)
		}
		if layout.CellCount != tc.wantCells || layout.PlaceholderCount != tc.wantPlaceolder {
			t.Fatalf("scenes=%d 格数有误: %+v", tc.scenes, layout)
		}
	}
	if _, ok := CalculateGridLayout(0, "16:9", false); ok {
		t.Fatal("0 场景不应有布局")
	}
}

func TestLargeGridAllowedOnly4K(t *testing.T) {
	cases := map[string]bool{"4K": true, "4k": true, " 4K ": true, "2K": false, "": false}
	for input, want := range cases {
		if got := LargeGridAllowed(input); got != want {
			t.Fatalf("LargeGridAllowed(%q) = %v; want %v", input, got, want)
		}
	}
}

func TestVideoAspectRatioFallbackAndPixelDimensions(t *testing.T) {
	if got := VideoAspectRatio(&project.ProjectFile{}); got != "9:16" {
		t.Fatalf("缺省应为竖屏: %s", got)
	}
	if got := VideoAspectRatio(&project.ProjectFile{AspectRatio: "16:9"}); got != "16:9" {
		t.Fatalf("显式比例应透传: %s", got)
	}
	layout, _ := CalculateGridLayout(4, "16:9", false)
	w, h := layout.PixelDimensions()
	if w != 1920 || h != 1080 {
		t.Fatalf("横版像素尺寸有误: %dx%d", w, h)
	}
	layout, _ = CalculateGridLayout(4, "9:16", false)
	w, h = layout.PixelDimensions()
	if w != 1080 || h != 1920 {
		t.Fatalf("竖版像素尺寸有误: %dx%d", w, h)
	}
}

func TestGridAspectRatioForLegacyNonSquare(t *testing.T) {
	if got := GridAspectRatioFor(2, 2, "16:9"); got != "16:9" {
		t.Fatalf("方形档应为视频比例: %s", got)
	}
	if got := GridAspectRatioFor(3, 2, "16:9"); got != "4:3" {
		t.Fatalf("存量非方形应按登记比例: %s", got)
	}
	if got := GridAspectRatioFor(3, 4, "16:9"); got != "16:9" {
		t.Fatalf("未登记非方形应回落视频比例: %s", got)
	}
}

func TestBuildFrameChain(t *testing.T) {
	chain := BuildFrameChain([]string{"A", "B"}, 2, 2)
	if len(chain) != 4 {
		t.Fatalf("帧链长度有误: %d", len(chain))
	}
	if chain[0].FrameType != GridFrameFirst || chain[0].NextSceneID == nil || *chain[0].NextSceneID != "A" {
		t.Fatalf("首格有误: %+v", chain[0])
	}
	if chain[1].FrameType != GridFrameTransition || *chain[1].PrevSceneID != "A" || *chain[1].NextSceneID != "B" {
		t.Fatalf("过渡格有误: %+v", chain[1])
	}
	if chain[2].FrameType != GridFramePlaceholder || chain[3].FrameType != GridFramePlaceholder {
		t.Fatalf("占位格有误: %+v / %+v", chain[2], chain[3])
	}
	if chain[1].Row != 0 || chain[1].Col != 1 || chain[2].Row != 1 || chain[2].Col != 0 {
		t.Fatalf("行列有误: %+v", chain)
	}
}

func TestGridGenerationJSONRoundTripLegacy(t *testing.T) {
	grid := NewGridGeneration(1, "episode_1.json", []string{"E1S1"}, 2, 2, "grid_4", "p", "m", "16:9", nil)
	if !ValidGridID(grid.ID) {
		t.Fatalf("生成的 id 不合法: %s", grid.ID)
	}

	// 显式 null 的 split_at：保持未切分。
	raw := []byte(`{"id":"grid_0123456789ab","episode":1,"script_file":"e.json","scene_ids":["A"],` +
		`"rows":2,"cols":2,"cell_count":4,"frame_chain":[],"status":"completed","provider":"p","model":"m",` +
		`"grid_size":"grid_4","created_at":"2026-01-01T00:00:00+00:00","split_at":null}`)
	parsed, err := GridGenerationFromJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.SplitAt != nil {
		t.Fatalf("显式 null 应保持未切分: %v", *parsed.SplitAt)
	}

	// 旧记录缺 split_at 且 completed：等价于已切分，用 created_at 充当落格时间。
	raw = []byte(`{"id":"grid_0123456789ab","episode":1,"script_file":"e.json","scene_ids":["A"],` +
		`"rows":2,"cols":2,"cell_count":4,"frame_chain":[],"status":"completed","provider":"p","model":"m",` +
		`"grid_size":"grid_4","created_at":"2026-01-01T00:00:00+00:00"}`)
	parsed, err = GridGenerationFromJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.SplitAt == nil || *parsed.SplitAt != parsed.CreatedAt {
		t.Fatalf("旧 completed 记录应按已切分处理: %+v", parsed.SplitAt)
	}

	// splitting 中间态：联合图已就绪、未落格。
	raw = []byte(`{"id":"grid_0123456789ab","episode":1,"script_file":"e.json","scene_ids":["A"],` +
		`"rows":2,"cols":2,"cell_count":4,"frame_chain":[],"status":"splitting","provider":"p","model":"m",` +
		`"grid_size":"grid_4","created_at":"2026-01-01T00:00:00+00:00"}`)
	parsed, err = GridGenerationFromJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Status != GridStatusCompleted || parsed.SplitAt != nil {
		t.Fatalf("splitting 应归一为 completed 且未切分: %+v", parsed)
	}
}

func TestGridGenerationMarshalKeepsPythonShape(t *testing.T) {
	grid := NewGridGeneration(2, "episode_2.json", []string{"A", "B"}, 2, 2, "grid_4", "", "", "9:16", nil)
	raw, err := json.Marshal(grid)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		`"reference_images":null`, `"split_at":null`, `"grid_image_path":null`,
		`"video_aspect_ratio":"9:16"`, `"status":"pending"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("序列化缺 %s: %s", want, text)
		}
	}
}

func TestBuildGridPromptStructure(t *testing.T) {
	scenes := []map[string]any{
		{
			"segment_id": "A",
			"image_prompt": map[string]any{
				"scene":       "桥上的两人",
				"composition": map[string]any{"shot_type": "Wide", "lighting": "暖", "ambiance": ""},
			},
			"video_prompt": map[string]any{"action": "转身"},
		},
		{"segment_id": "B", "image_prompt": "雨夜巷口", "video_prompt": "快步走"},
	}
	prompt := BuildGridPrompt(scenes, "segment_id", 2, 2, "水墨", "16:9", "16:9", nil)
	for _, want := range []string{
		"恰好 2 行 2 列，共 4 个画格",
		"整体图片比例：16:9",
		"每个画格比例：16:9",
		"格0（row1 col1）— A开场：",
		"桥上的两人；shot_type: Wide，lighting: 暖",
		"格1（row1 col2）— A→B过渡：",
		"  转身，过渡到 雨夜巷口",
		"格2（row2 col1）— 空占位：纯灰色背景，无任何内容",
		"【风格要求】\n水墨",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt 缺 %q:\n%s", want, prompt)
		}
	}
	// 空 composition 值不参与渲染。
	if strings.Contains(prompt, "ambiance") {
		t.Fatalf("空值不应渲染: %s", prompt)
	}
}
