package domain

import (
	"strings"
	"testing"
)

func validComposition() map[string]any {
	return map[string]any{"shot_type": "Medium Shot", "lighting": "暖光", "ambiance": "安静"}
}

func validImagePrompt() map[string]any {
	return map[string]any{"scene": "桥上的两人", "composition": validComposition()}
}

func validVideoPrompt() map[string]any {
	return map[string]any{
		"action": "并肩前行", "camera_motion": "Static", "ambiance_audio": "风声",
		"dialogue": []any{map[string]any{"speaker": "阿明", "line": "你好"}},
	}
}

func validNarrationSegment() map[string]any {
	return map[string]any{
		"segment_id": "E1S1", "duration_seconds": float64(4), "novel_text": "原文",
		"characters_in_segment": []any{"阿明"}, "image_prompt": validImagePrompt(),
		"video_prompt": validVideoPrompt(),
	}
}

func validNarrationScript() map[string]any {
	return map[string]any{
		"title": "第1集", "content_mode": "narration", "duration_seconds": float64(4),
		"novel":   map[string]any{"title": "小说", "chapter": "第1集"},
		"episode": float64(1), "metadata": map[string]any{"status": "draft"},
		"segments": []any{validNarrationSegment()},
	}
}

func validDramaScene() map[string]any {
	return map[string]any{
		"scene_id": "E1S1", "duration_seconds": float64(8), "characters_in_scene": []any{"阿明"},
		"image_prompt": validImagePrompt(),
		"video_prompt": map[string]any{"action": "a", "camera_motion": "Static", "ambiance_audio": "b"},
		"utterances": []any{
			map[string]any{"kind": "dialogue", "speaker": "阿明", "text": "你好"},
			map[string]any{"kind": "voiceover", "speaker": nil, "text": "旁白"},
		},
		"source_text": "原文",
	}
}

func validAdShot() map[string]any {
	return map[string]any{
		"shot_id": "E1S1", "section": "hook", "duration_seconds": float64(5),
		"voiceover_text": "文案", "characters_in_shot": []any{},
		"image_prompt": validImagePrompt(), "video_prompt": validVideoPrompt(),
	}
}

func validReferenceUnit() map[string]any {
	return map[string]any{
		"unit_id": "E1U1", "duration_seconds": float64(8),
		"shots":      []any{map[string]any{"text": "@[阿明] 站在桥上"}},
		"references": []any{map[string]any{"type": "character", "name": "阿明"}},
	}
}

func TestValidateScriptStructureAcceptsAllFourModes(t *testing.T) {
	cases := map[string]map[string]any{
		"narration": validNarrationScript(),
		"drama": {
			"title": "第1集", "content_mode": "drama", "episode": float64(1),
			"scenes": []any{validDramaScene()},
		},
		"ad": {
			"title": "短片", "content_mode": "ad", "episode": float64(1),
			"shots": []any{validAdShot()},
		},
		"reference_video": {
			"title": "第1集", "content_mode": "narration", "episode": float64(1),
			"video_units": []any{validReferenceUnit()},
		},
	}
	for name, script := range cases {
		if errs := ValidateScriptStructure(script); len(errs) != 0 {
			t.Errorf("%s 合法剧本被判非法: %v", name, errs)
		}
	}
}

func TestValidateScriptStructureRejectsBrokenNarration(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m map[string]any)
		want   string
	}{
		{"缺标题", func(m map[string]any) { delete(m, "title") }, "title: Field required"},
		{"标题类型错", func(m map[string]any) { m["title"] = float64(1) }, "title: Input should be a valid string"},
		{"content_mode 非法取值", func(m map[string]any) { m["content_mode"] = "comic" }, "content_mode: Input should be"},
		{"content_mode 指向另一模式时按该模式校验（与取证解析同口径）", func(m map[string]any) { m["content_mode"] = "drama" }, "scenes: Field required"},
		{"缺 segments", func(m map[string]any) { delete(m, "segments") }, "segments: Field required"},
		{"segments 非数组", func(m map[string]any) { m["segments"] = "x" }, "segments: Input should be a valid list"},
		{"时长越界", func(m map[string]any) {
			segmentOf(m)["duration_seconds"] = float64(61)
		}, "segments.0.duration_seconds: Input should be less than or equal to 60"},
		{"时长缺失", func(m map[string]any) { delete(segmentOf(m), "duration_seconds") },
			"segments.0.duration_seconds: Field required"},
		{"缺 novel_text", func(m map[string]any) { delete(segmentOf(m), "novel_text") },
			"segments.0.novel_text: Field required"},
		{"角色列表缺失", func(m map[string]any) { delete(segmentOf(m), "characters_in_segment") },
			"segments.0.characters_in_segment: Field required"},
		{"嵌套构图缺字段", func(m map[string]any) {
			compositionOf(m)["lighting"] = nil
			delete(compositionOf(m), "lighting")
		}, "segments.0.image_prompt.composition.lighting: Field required"},
		{"video_prompt 未知字段", func(m map[string]any) {
			segmentOf(m)["video_prompt"].(map[string]any)["extra_field"] = "x"
		}, "segments.0.video_prompt.extra_field: Extra inputs are not permitted"},
		{"对白条目类型错", func(m map[string]any) {
			videoOf(m)["dialogue"] = []any{"不是对象"}
		}, "segments.0.video_prompt.dialogue.0: Input should be a valid dictionary"},
		{"转场枚举非法", func(m map[string]any) { segmentOf(m)["transition_to_next"] = "wipe" },
			"segments.0.transition_to_next: Input should be"},
		{"generated_assets 未知字段", func(m map[string]any) {
			segmentOf(m)["generated_assets"] = map[string]any{"bogus": "x"}
		}, "segments.0.generated_assets.bogus: Extra inputs are not permitted"},
		{"generated_assets 状态非法", func(m map[string]any) {
			segmentOf(m)["generated_assets"] = map[string]any{"status": "done"}
		}, "segments.0.generated_assets.status: Input should be"},
		{"novel 未知字段", func(m map[string]any) {
			m["novel"] = map[string]any{"title": "t", "bogus": 1}
		}, "novel.bogus: Extra inputs are not permitted"},
	}
	for _, c := range cases {
		script := validNarrationScript()
		c.mutate(script)
		errs := ValidateScriptStructure(script)
		if !hasScriptError(errs, c.want) {
			t.Errorf("%s: 期望含 %q，实际 %v", c.name, c.want, errs)
		}
	}
}

// 顶层刻意不走 extra="forbid"：episode / metadata 等运行时注入字段必须放行。
func TestValidateScriptStructureToleratesRuntimeTopLevelFields(t *testing.T) {
	script := validNarrationScript()
	script["episode"] = float64(3)
	script["metadata"] = map[string]any{"status": "draft", "total_scenes": float64(1)}
	script["source_fingerprints"] = map[string]any{"x": "y"}
	if errs := ValidateScriptStructure(script); len(errs) != 0 {
		t.Fatalf("运行时字段应放行: %v", errs)
	}
}

func TestValidateScriptStructureChecksDramaUtterances(t *testing.T) {
	// voiceover 带 speaker 非法。
	script := map[string]any{"title": "t", "content_mode": "drama", "scenes": []any{validDramaScene()}}
	scenes := script["scenes"].([]any)
	scene := scenes[0].(map[string]any)
	scene["utterances"] = []any{map[string]any{"kind": "voiceover", "speaker": "阿明", "text": "x"}}
	if errs := ValidateScriptStructure(script); !hasScriptError(errs, "不得带 speaker") {
		t.Fatalf("voiceover 带 speaker 应非法: %v", errs)
	}

	// dialogue 缺 speaker 非法。
	scene["utterances"] = []any{map[string]any{"kind": "dialogue", "speaker": "  ", "text": "x"}}
	if errs := ValidateScriptStructure(script); !hasScriptError(errs, "必须带非空 speaker") {
		t.Fatalf("dialogue 空 speaker 应非法: %v", errs)
	}

	// utterances 在场时 video_prompt 不得再带 dialogue。
	scene["utterances"] = []any{map[string]any{"kind": "dialogue", "speaker": "阿明", "text": "x"}}
	scene["video_prompt"].(map[string]any)["dialogue"] = []any{}
	if errs := ValidateScriptStructure(script); !hasScriptError(errs, "video_prompt.dialogue: Extra inputs") {
		t.Fatalf("utterances 在场时 dialogue 应被拒: %v", errs)
	}

	// utterances 缺席（存量形态）时 video_prompt.dialogue 与 voiceover 照常放行。
	delete(scene, "utterances")
	scene["voiceover"] = []any{"旧旁白"}
	if errs := ValidateScriptStructure(script); len(errs) != 0 {
		t.Fatalf("存量数据形态应放行: %v", errs)
	}
}

func TestValidateScriptStructureChecksReferenceUnits(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(unit map[string]any)
		want   string
	}{
		{"shots 为空", func(u map[string]any) { u["shots"] = []any{} }, "List should have at least 1 item"},
		{"shots 超过 4", func(u map[string]any) {
			u["shots"] = []any{map[string]any{"text": "a"}, map[string]any{"text": "b"},
				map[string]any{"text": "c"}, map[string]any{"text": "d"}, map[string]any{"text": "e"}}
		}, "List should have at most 4 items"},
		{"unit 时长越界", func(u map[string]any) { u["duration_seconds"] = float64(301) },
			"Input should be less than or equal to 300"},
		{"引用类型非法", func(u map[string]any) {
			u["references"] = []any{map[string]any{"type": "product", "name": "x"}}
		}, "references.0.type: Input should be"},
		{"shot 未知字段", func(u map[string]any) {
			u["shots"] = []any{map[string]any{"text": "a", "duration_seconds": float64(4)}}
		}, "shots.0.duration_seconds: Extra inputs are not permitted"},
	}
	for _, c := range cases {
		script := map[string]any{"title": "t", "content_mode": "narration", "video_units": []any{validReferenceUnit()}}
		c.mutate(script["video_units"].([]any)[0].(map[string]any))
		if errs := ValidateScriptStructure(script); !hasScriptError(errs, c.want) {
			t.Errorf("%s: 期望含 %q，实际 %v", c.name, c.want, errs)
		}
	}
}

func TestScriptDurationTotalMatchesFallbackTable(t *testing.T) {
	segments := []any{
		map[string]any{"duration_seconds": float64(4)},
		map[string]any{},                         // 缺失 → 兜底 4
		map[string]any{"duration_seconds": "5"},  // 脏值（字符串）→ 兜底 4
		map[string]any{"duration_seconds": true}, // 布尔不算整数 → 兜底 4
	}
	if got := ScriptDurationTotal("segments", segments); got != 16 {
		t.Fatalf("segments 总时长 = %d，期望 16", got)
	}
	scenes := []any{map[string]any{"duration_seconds": float64(4)}, map[string]any{}}
	if got := ScriptDurationTotal("scenes", scenes); got != 12 {
		t.Fatalf("scenes 总时长 = %d，期望 12（兜底 8）", got)
	}
	// ad 镜头与参考 unit 无兜底时长：缺失按 0 计。
	if got := ScriptDurationTotal("shots", []any{map[string]any{}, map[string]any{"duration_seconds": float64(3)}}); got != 3 {
		t.Fatalf("shots 总时长 = %d，期望 3", got)
	}
	if got := ScriptDurationTotal("video_units", []any{map[string]any{}}); got != 0 {
		t.Fatalf("video_units 缺失时长应按 0 计，得到 %d", got)
	}
	// 非数组（null 这类降级保存的脏值）按空处理。
	if got := ScriptDurationTotal("segments", nil); got != 0 {
		t.Fatalf("null 应按空处理，得到 %d", got)
	}
	// 非对象条目按 0 计。
	if got := ScriptDurationTotal("segments", []any{"垃圾", float64(1)}); got != 0 {
		t.Fatalf("非对象条目应按 0 计，得到 %d", got)
	}
}

// --- 夹具辅助 ---

func segmentOf(script map[string]any) map[string]any {
	return script["segments"].([]any)[0].(map[string]any)
}

func videoOf(script map[string]any) map[string]any {
	return segmentOf(script)["video_prompt"].(map[string]any)
}

func compositionOf(script map[string]any) map[string]any {
	return segmentOf(script)["image_prompt"].(map[string]any)["composition"].(map[string]any)
}

func hasScriptError(errs []string, want string) bool {
	for _, err := range errs {
		if strings.Contains(err, want) {
			return true
		}
	}
	return false
}
