package media

import "testing"

// narrationScript / segment 是剧本夹具（与 script 包测试同构的本地副本）：
// 测试辅助不跨包共享，media 测试按自身需要就地构造。
func narrationScript(segments ...map[string]any) map[string]any {
	items := make([]any, 0, len(segments))
	for _, segment := range segments {
		items = append(items, segment)
	}
	return map[string]any{"content_mode": "narration", "segments": items}
}

func segment(id string, extra map[string]any) map[string]any {
	item := map[string]any{"segment_id": id, "image_prompt": "画面"}
	for k, v := range extra {
		item[k] = v
	}
	return item
}

func TestStoryboardItemsPerSkeleton(t *testing.T) {
	cases := []struct {
		name       string
		script     map[string]any
		wantItems  int
		wantID     string
		wantFields StoryboardItemFields
	}{
		{
			name:      "narration segments",
			script:    narrationScript(segment("E1S1", nil)),
			wantItems: 1, wantID: "segment_id",
			wantFields: StoryboardItemFields{CharField: "characters_in_segment", SceneField: "scenes", PropField: "props"},
		},
		{
			name: "drama scenes",
			script: map[string]any{"content_mode": "drama", "scenes": []any{
				map[string]any{"scene_id": "E1S1", "characters_in_scene": []any{"甲"}},
			}},
			wantItems: 1, wantID: "scene_id",
			wantFields: StoryboardItemFields{CharField: "characters_in_scene", SceneField: "scenes", PropField: "props"},
		},
		{
			name: "ad shots",
			script: map[string]any{"content_mode": "ad", "shots": []any{
				map[string]any{"shot_id": "E1S1", "characters_in_shot": []any{"甲"}},
			}},
			wantItems: 1, wantID: "shot_id",
			wantFields: StoryboardItemFields{CharField: "characters_in_shot", SceneField: "scenes", PropField: "props"},
		},
		{
			name: "reference video_units",
			script: map[string]any{"video_units": []any{
				map[string]any{"unit_id": "E1U1"},
			}},
			wantItems: 0, wantID: "unit_id",
			wantFields: StoryboardItemFields{CharField: "", SceneField: "scenes", PropField: "props"},
		},
	}
	for _, c := range cases {
		items, idField, fields := StoryboardItems(c.script)
		if len(items) != c.wantItems || idField != c.wantID || fields != c.wantFields {
			t.Errorf("%s: items=%d idField=%q fields=%+v；期望 %d/%q/%+v",
				c.name, len(items), idField, fields, c.wantItems, c.wantID, c.wantFields)
		}
	}
}

func TestStoryboardItemsMissingSkeletonIsEmpty(t *testing.T) {
	cases := []struct {
		name   string
		script map[string]any
		wantID string
	}{
		{"narration 缺 segments", map[string]any{"content_mode": "narration"}, "segment_id"},
		{"空剧本兜底 segments", map[string]any{}, "segment_id"},
		{"drama 缺 scenes", map[string]any{"content_mode": "drama"}, "scene_id"},
		{"ad 缺 shots", map[string]any{"content_mode": "ad"}, "shot_id"},
	}
	for _, c := range cases {
		items, idField, _ := StoryboardItems(c.script)
		if len(items) != 0 || idField != c.wantID {
			t.Errorf("%s: items=%d idField=%q；期望 0/%q", c.name, len(items), idField, c.wantID)
		}
	}

	// video_units 骨架下即便数组在场也强制返回空条目。
	items, idField, fields := StoryboardItems(map[string]any{"video_units": []any{
		map[string]any{"unit_id": "E1U1"},
	}})
	if len(items) != 0 || idField != "unit_id" || fields.CharField != "" {
		t.Errorf("video_units 应返回空条目与空角色字段: items=%d idField=%q fields=%+v", len(items), idField, fields)
	}
}

func storyboardPlanItems() []any {
	return []any{
		segment("E1S1", nil),
		segment("E1S2", nil),
		segment("E1S3", map[string]any{"segment_break": true}),
		segment("E1S4", nil),
	}
}

func TestBuildStoryboardDependencyPlanGroups(t *testing.T) {
	plans := BuildStoryboardDependencyPlan(storyboardPlanItems(), "segment_id",
		[]string{"E1S1", "E1S2", "E1S3", "E1S4"}, "episode_1.json")

	want := []StoryboardTaskPlan{
		{ResourceID: "E1S1", DependencyResourceID: "", DependencyGroup: "episode_1.json:group:1", DependencyIndex: 0},
		{ResourceID: "E1S2", DependencyResourceID: "E1S1", DependencyGroup: "episode_1.json:group:1", DependencyIndex: 1},
		{ResourceID: "E1S3", DependencyResourceID: "", DependencyGroup: "episode_1.json:group:2", DependencyIndex: 0},
		{ResourceID: "E1S4", DependencyResourceID: "E1S3", DependencyGroup: "episode_1.json:group:2", DependencyIndex: 1},
	}
	if len(plans) != len(want) {
		t.Fatalf("plan 数量=%d；期望 %d: %+v", len(plans), len(want), plans)
	}
	for i := range want {
		if plans[i] != want[i] {
			t.Errorf("plan[%d]=%+v；期望 %+v", i, plans[i], want[i])
		}
	}
}

func TestBuildStoryboardDependencyPlanNonContiguousSelection(t *testing.T) {
	// E1S3 带 segment_break，E1S2 未选中：两项各自成组、组首无依赖。
	plans := BuildStoryboardDependencyPlan(storyboardPlanItems(), "segment_id",
		[]string{"E1S1", "E1S3"}, "episode_1.json")

	want := []StoryboardTaskPlan{
		{ResourceID: "E1S1", DependencyGroup: "episode_1.json:group:1", DependencyIndex: 0},
		{ResourceID: "E1S3", DependencyGroup: "episode_1.json:group:2", DependencyIndex: 0},
	}
	if len(plans) != len(want) {
		t.Fatalf("plan 数量=%d；期望 %d: %+v", len(plans), len(want), plans)
	}
	for i := range want {
		if plans[i] != want[i] {
			t.Errorf("plan[%d]=%+v；期望 %+v", i, plans[i], want[i])
		}
	}
}

func TestBuildStoryboardDependencyPlanBreaksOnUnselectedPrevious(t *testing.T) {
	// 无 segment_break，但 E1S2 未选中 → E1S3 的前驱不在选择集中，另起一组且无依赖。
	items := []any{segment("E1S1", nil), segment("E1S2", nil), segment("E1S3", nil)}
	plans := BuildStoryboardDependencyPlan(items, "segment_id", []string{"E1S1", "E1S3"}, "ep.json")

	want := []StoryboardTaskPlan{
		{ResourceID: "E1S1", DependencyGroup: "ep.json:group:1", DependencyIndex: 0},
		{ResourceID: "E1S3", DependencyGroup: "ep.json:group:2", DependencyIndex: 0},
	}
	if len(plans) != len(want) {
		t.Fatalf("plan 数量=%d；期望 %d: %+v", len(plans), len(want), plans)
	}
	for i := range want {
		if plans[i] != want[i] {
			t.Errorf("plan[%d]=%+v；期望 %+v", i, plans[i], want[i])
		}
	}
}

func TestBuildStoryboardDependencyPlanEmptyTargets(t *testing.T) {
	if plans := BuildStoryboardDependencyPlan(storyboardPlanItems(), "segment_id", nil, "ep.json"); len(plans) != 0 {
		t.Errorf("空 targetIDs 应返回空计划: %+v", plans)
	}
	if plans := BuildStoryboardDependencyPlan(storyboardPlanItems(), "segment_id", []string{}, "ep.json"); len(plans) != 0 {
		t.Errorf("空 targetIDs 应返回空计划: %+v", plans)
	}
}

func TestBuildStoryboardDependencyPlanDefaultGroupName(t *testing.T) {
	plans := BuildStoryboardDependencyPlan([]any{segment("E1S1", nil)}, "segment_id", []string{"E1S1"}, "")
	if len(plans) != 1 || plans[0].DependencyGroup != "storyboard:group:1" {
		t.Errorf("scriptFilename 为空应回退 storyboard 前缀: %+v", plans)
	}
}

func TestStoryboardItemsMissingImageSkipsGenerated(t *testing.T) {
	items := []any{
		map[string]any{"segment_id": "E1S1", "generated_assets": map[string]any{"storyboard_image": "storyboards/scene_E1S1.png"}},
		map[string]any{"segment_id": "E1S2", "generated_assets": map[string]any{}},
		map[string]any{"segment_id": "E1S3", "generated_assets": []any{"dirty"}},
		map[string]any{"segment_id": "E1S4"},
		map[string]any{"segment_id": "E1S5", "generated_assets": map[string]any{"storyboard_image": ""}},
	}
	got := StoryboardItemsMissingImage(items, "segment_id")
	want := []string{"E1S2", "E1S3", "E1S4", "E1S5"}
	if len(got) != len(want) {
		t.Fatalf("missing=%v；期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("missing[%d]=%q；期望 %q", i, got[i], want[i])
		}
	}
}

func TestGroupScenesBySegmentBreak(t *testing.T) {
	items := []any{
		map[string]any{"segment_id": "A"},
		map[string]any{"segment_id": "B"},
		map[string]any{"segment_id": "C", "segment_break": true},
		map[string]any{"segment_id": "D"},
	}
	groups := GroupScenesBySegmentBreak(items, "segment_id")
	if len(groups) != 2 {
		t.Fatalf("分组数有误: %d", len(groups))
	}
	if len(groups[0]) != 2 || len(groups[1]) != 2 {
		t.Fatalf("分组内容有误: %+v", groups)
	}
	if groups[1][0]["segment_id"] != "C" || groups[1][1]["segment_id"] != "D" {
		t.Fatalf("break 应以当前项开启新组: %+v", groups[1])
	}
}
