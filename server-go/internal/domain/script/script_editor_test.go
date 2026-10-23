package script

import (
	"strings"
	"testing"
)

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

func TestResolveScriptItemsFailsLoudOnCorruptedKey(t *testing.T) {
	items, idField, kind, err := ResolveScriptItems(narrationScript(segment("E1S1", nil)))
	if err != nil || len(items) != 1 || idField != "segment_id" || kind != "segments" {
		t.Fatalf("items=%v idField=%q kind=%q err=%v", items, idField, kind, err)
	}

	// 键缺失视为空数组。
	items, _, _, err = ResolveScriptItems(map[string]any{"content_mode": "narration"})
	if err != nil || len(items) != 0 {
		t.Fatalf("缺键应视为空数组: %v %v", items, err)
	}

	// 键存在但类型非数组（含 null）fail loud，不静默降级。
	for _, bad := range []any{nil, map[string]any{}, "x", float64(3)} {
		_, _, kind, err := ResolveScriptItems(map[string]any{"content_mode": "narration", "segments": bad})
		if err == nil {
			t.Errorf("segments=%v 应报错", bad)
			continue
		}
		var editErr *ScriptEditError
		if !asScriptEditError(err, &editErr) || editErr.Key != ScriptEditItemsNotList {
			t.Errorf("错误应带 %s key: %v", ScriptEditItemsNotList, err)
		}
		if kind != "segments" {
			t.Errorf("kind = %q", kind)
		}
	}
}

func TestPatchScriptFieldSetsNestedLeafAndCreatesMissingLeaf(t *testing.T) {
	script := narrationScript(segment("E1S1", map[string]any{"video_prompt": map[string]any{"action": "跑"}}))

	if err := PatchScriptField(script, "E1S1", "video_prompt.action", "走"); err != nil {
		t.Fatal(err)
	}
	if err := PatchScriptField(script, "E1S1", "video_prompt.dialogue", []any{map[string]any{"speaker": "阿明"}}); err != nil {
		t.Fatalf("叶子不存在应允许创建: %v", err)
	}
	if err := PatchScriptField(script, "E1S1", "duration_seconds", float64(5)); err != nil {
		t.Fatal(err)
	}

	item := script["segments"].([]any)[0].(map[string]any)
	if item["video_prompt"].(map[string]any)["action"] != "走" {
		t.Fatalf("嵌套叶子未写入: %+v", item)
	}
	if _, ok := item["video_prompt"].(map[string]any)["dialogue"]; !ok {
		t.Fatalf("可选叶子应被创建: %+v", item["video_prompt"])
	}
	if item["duration_seconds"] != float64(5) {
		t.Fatalf("顶层字段未写入: %+v", item)
	}
}

func TestPatchScriptFieldRejectsIllegalTargets(t *testing.T) {
	cases := map[string]string{
		"generated_assets.storyboard_image": "不可改 generated_assets",
		"end_frame_image":                   "不可改 end_frame_image",
		"segment_id":                        "不可改分镜 id 字段",
		"video_prompt..action":              "非法字段路径",
		"video_prompt.nonexistent.action":   "字段路径不存在",
		"image_prompt.action":               "父节点非对象",
	}
	for fieldPath, want := range cases {
		script := narrationScript(segment("E1S1", map[string]any{
			"video_prompt": map[string]any{"action": "跑"},
			"image_prompt": "纯字符串",
		}))
		err := PatchScriptField(script, "E1S1", fieldPath, "x")
		if err == nil {
			t.Errorf("%s 应被拒绝", fieldPath)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: 错误应含 %q，实际 %v", fieldPath, want, err)
		}
	}
	// id 未命中。
	if err := PatchScriptField(narrationScript(segment("E1S1", nil)), "E9S9", "x", "y"); err == nil {
		t.Error("id 未命中应报错")
	}
}

func TestInsertScriptSegmentDerivesStableIDAndClearsAssets(t *testing.T) {
	script := narrationScript(
		segment("E1S1", map[string]any{"generated_assets": map[string]any{"storyboard_image": "a.png"}}),
		segment("E1S2", nil),
	)
	err := InsertScriptSegment(script, "E1S1", map[string]any{
		"segment_id":       "agent-supplied",
		"generated_assets": map[string]any{"storyboard_image": "someone-elses.png"},
		"end_frame_image":  "E1S1_end.png",
		"image_prompt":     "新画面",
	})
	if err != nil {
		t.Fatal(err)
	}
	items := script["segments"].([]any)
	if len(items) != 3 {
		t.Fatalf("应插在锚点之后: %+v", items)
	}
	inserted := items[1].(map[string]any)
	if inserted["segment_id"] != "E1S1_1" {
		t.Fatalf("新 id 应由锚点派生: %+v", inserted)
	}
	if len(inserted["generated_assets"].(map[string]any)) != 0 {
		t.Fatalf("新分镜资产应清空: %+v", inserted)
	}
	if _, ok := inserted["end_frame_image"]; ok {
		t.Fatalf("新分镜不应带尾帧快照: %+v", inserted)
	}
	if items[0].(map[string]any)["segment_id"] != "E1S1" || items[2].(map[string]any)["segment_id"] != "E1S2" {
		t.Fatalf("其余分镜 id 不应改动: %+v", items)
	}

	// 锚点本身已带后缀时收敛到 stem，不产生多层嵌套。
	script = narrationScript(segment("E1S01_1", nil), segment("E1S01_2", nil))
	if err := InsertScriptSegment(script, "E1S01_1", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	items = script["segments"].([]any)
	if items[1].(map[string]any)["segment_id"] != "E1S01_3" {
		t.Fatalf("应向下顺延而非嵌套: %+v", items)
	}
}

func TestRemoveScriptSegmentKeepsOtherIDs(t *testing.T) {
	script := narrationScript(segment("E1S1", nil), segment("E1S2", nil), segment("E1S3", nil))
	if err := RemoveScriptSegment(script, "E1S2"); err != nil {
		t.Fatal(err)
	}
	ids, err := ScriptItemIDs(script)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "E1S1,E1S3" {
		t.Fatalf("删除不应重排其余 id: %v", ids)
	}
	if err := RemoveScriptSegment(script, "E1S9"); err == nil {
		t.Error("id 未命中应报错")
	}
}

func TestSplitScriptSegmentKeepsAnchorAssets(t *testing.T) {
	script := narrationScript(segment("E1S1", map[string]any{
		"generated_assets": map[string]any{"storyboard_image": "keep.png"},
		"end_frame_image":  "E1S1_end.png",
	}))
	err := SplitScriptSegment(script, "E1S1", []map[string]any{
		{"image_prompt": "第一份", "generated_assets": map[string]any{"storyboard_image": "agent.png"}},
		{"image_prompt": "第二份", "generated_assets": map[string]any{"storyboard_image": "agent.png"},
			"end_frame_image": "agent_end.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items := script["segments"].([]any)
	if len(items) != 2 {
		t.Fatalf("应拆成 2 份: %+v", items)
	}
	anchor := items[0].(map[string]any)
	if anchor["segment_id"] != "E1S1" {
		t.Fatalf("首份保留原 id: %+v", anchor)
	}
	// 锚点延续：以原分镜实际值为准，agent 自带值被覆盖。
	if anchor["generated_assets"].(map[string]any)["storyboard_image"] != "keep.png" {
		t.Fatalf("首份资产应以锚点为准: %+v", anchor)
	}
	if anchor["end_frame_image"] != "E1S1_end.png" {
		t.Fatalf("首份尾帧应以锚点为准: %+v", anchor)
	}
	second := items[1].(map[string]any)
	if second["segment_id"] != "E1S1_1" {
		t.Fatalf("其余份取派生 id: %+v", second)
	}
	if len(second["generated_assets"].(map[string]any)) != 0 {
		t.Fatalf("其余份资产应清空: %+v", second)
	}
	if _, ok := second["end_frame_image"]; ok {
		t.Fatalf("其余份不应带尾帧: %+v", second)
	}

	// 份数不足与元素非法。
	if err := SplitScriptSegment(narrationScript(segment("E1S1", nil)), "E1S1", []map[string]any{{"a": 1}}); err == nil {
		t.Error("少于 2 份应报错")
	}
	if err := SplitScriptSegment(narrationScript(segment("E1S1", nil)), "E1S1",
		[]map[string]any{{"a": 1}, nil}); err == nil {
		t.Error("非对象部分应报错")
	}
}

func TestScriptEditorWorksOnReferenceRouteSkeleton(t *testing.T) {
	script := map[string]any{
		"video_units": []any{map[string]any{"unit_id": "E1U1", "duration_seconds": float64(8)}},
	}
	if err := PatchScriptField(script, "E1U1", "duration_seconds", float64(5)); err != nil {
		t.Fatal(err)
	}
	if err := InsertScriptSegment(script, "E1U1", map[string]any{"shots": []any{}}); err != nil {
		t.Fatal(err)
	}
	ids, _ := ScriptItemIDs(script)
	if strings.Join(ids, ",") != "E1U1,E1U1_1" {
		t.Fatalf("reference 模式应编辑 video_units: %v", ids)
	}
}

// asScriptEditError 是 errors.As 的薄封装，避免测试文件重复 import errors。
func asScriptEditError(err error, target **ScriptEditError) bool {
	e, ok := err.(*ScriptEditError)
	if ok {
		*target = e
	}
	return ok
}
