package domain

import (
	"math"
	"testing"
)

func TestSubtitleBoundsCheckValidAndIssues(t *testing.T) {
	subtitle := map[string]any{"cues": []any{
		map[string]any{"id": "a", "start_seconds": 0.0, "end_seconds": 1.0},
		map[string]any{"start_seconds": 1.0, "end_seconds": 2.0},
	}}
	result, err := SubtitleBoundsCheck(subtitle, 5.0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result["valid"] != true || result["detected"] != false || result["cue_count"] != 2 {
		t.Fatalf("result = %v", result)
	}

	// 越界 + 倒置 + 重叠。
	bad := map[string]any{"cues": []any{
		map[string]any{"start_seconds": 0.0, "end_seconds": 6.0}, // out_of_bounds
		map[string]any{"start_seconds": 5.5, "end_seconds": 5.5}, // out_of_bounds + invalid_interval
		map[string]any{"start_seconds": 0.5, "end_seconds": 1.5}, // 与第一条重叠
	}}
	result, err = SubtitleBoundsCheck(bad, 5.0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result["valid"] != false || result["severity"] != "blocking" {
		t.Fatalf("result = %v", result)
	}
	items := result["items"].([]any)
	codes := map[string]int{}
	for _, raw := range items {
		issue := raw.(map[string]any)
		codes[issue["code"].(string)]++
		if issue["severity"] != "blocking" {
			t.Fatalf("issue severity = %v", issue)
		}
	}
	if codes["out_of_bounds"] != 2 || codes["invalid_interval"] != 1 || codes["overlap"] != 1 {
		t.Fatalf("codes = %v", codes)
	}

	// cues 不是列表 → cues_not_a_list。
	result, err = SubtitleBoundsCheck(map[string]any{"cues": "nope"}, 5.0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result["valid"] != false {
		t.Fatalf("result = %v", result)
	}
	items = result["items"].([]any)
	if items[0].(map[string]any)["code"] != "cues_not_a_list" {
		t.Fatalf("items = %v", items)
	}

	// 非对象 subtitle 视为空配置；缺 start/end 记 invalid_interval。
	result, err = SubtitleBoundsCheck("not-a-map", 5.0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result["cue_count"] != 0 || result["valid"] != true {
		t.Fatalf("result = %v", result)
	}
	result, err = SubtitleBoundsCheck(map[string]any{"cues": []any{
		map[string]any{"start_seconds": 0.0},
	}}, 5.0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	items = result["items"].([]any)
	if items[0].(map[string]any)["code"] != "invalid_interval" {
		t.Fatalf("items = %v", items)
	}
}

func TestParseSilencedetectLog(t *testing.T) {
	log := "[silencedetect @ 0x1] silence_start: 1.5\n" +
		"[silencedetect @ 0x1] silence_end: 3.0 | silence_duration: 1.5\n" +
		"[silencedetect @ 0x1] silence_start: 9.2\n"
	segments := ParseSilencedetectLog(log)
	if len(segments) != 2 {
		t.Fatalf("segments = %v", segments)
	}
	first := segments[0]
	if first["start_seconds"] != 1.5 || first["end_seconds"] != 3.0 || first["duration_seconds"] != 1.5 {
		t.Fatalf("first = %v", first)
	}
	second := segments[1]
	if second["start_seconds"] != 9.2 || second["end_seconds"] != nil || second["duration_seconds"] != nil {
		t.Fatalf("second = %v", second)
	}

	// end 早于后续 start 的孤儿 end 被跳过。
	log = "silence_start: 2\nsilence_end: 2.5 | silence_duration: 0.5\nsilence_start: 4\nsilence_end: 4.2 | silence_duration: 0.2"
	segments = ParseSilencedetectLog(log)
	if len(segments) != 2 {
		t.Fatalf("segments = %v", segments)
	}
	if segments[1]["end_seconds"] != 4.2 {
		t.Fatalf("second = %v", segments[1])
	}
}

func TestParseAudioMetricsLog(t *testing.T) {
	log := "[Parsed_audio_0 @ 0x1] mean_volume: -18.2 dB\n" +
		"[Parsed_audio_0 @ 0x1] max_volume: -1.4 dB\n" +
		"\n  Integrated loudness:\n    I:  -15.3 LUFS\n" +
		"Number of clipped samples: 12\n"
	metrics := ParseAudioMetricsLog(log)
	if metrics["mean_volume_db"] != -18.2 || metrics["max_volume_db"] != -1.4 {
		t.Fatalf("metrics = %v", metrics)
	}
	if metrics["integrated_lufs"] != -15.3 {
		t.Fatalf("integrated = %v", metrics["integrated_lufs"])
	}
	if metrics["clipped_samples"] != int64(12) {
		t.Fatalf("clipped = %v", metrics["clipped_samples"])
	}

	empty := ParseAudioMetricsLog("nothing here")
	if empty["mean_volume_db"] != nil || empty["clipped_samples"] != nil {
		t.Fatalf("empty = %v", empty)
	}
}

func TestAudioLevelCheck(t *testing.T) {
	// 空指标：不可用但无 issue。
	result := AudioLevelCheck(map[string]any{})
	if result["available"] != false || result["valid"] != true || result["detected"] != false {
		t.Fatalf("result = %v", result)
	}

	// 响度越界 → warning。
	result = AudioLevelCheck(map[string]any{"integrated_lufs": -30.0})
	if result["available"] != true || result["severity"] != "warning" {
		t.Fatalf("result = %v", result)
	}
	issues := result["issues"].([]any)
	if issues[0].(map[string]any)["code"] != "loudness_out_of_range" {
		t.Fatalf("issues = %v", issues)
	}

	// 峰值 ≥ 0dB → 削波 blocking。
	result = AudioLevelCheck(map[string]any{"max_volume_db": 0.2})
	if result["severity"] != "blocking" || result["valid"] != false {
		t.Fatalf("result = %v", result)
	}
	issues = result["issues"].([]any)
	if issues[0].(map[string]any)["code"] != "audio_clipping_detected" {
		t.Fatalf("issues = %v", issues)
	}

	// 接近削波 → warning。
	result = AudioLevelCheck(map[string]any{"max_volume_db": -0.05})
	if result["severity"] != "warning" {
		t.Fatalf("result = %v", result)
	}

	// 削波样本计数 > 0 也判削波。
	result = AudioLevelCheck(map[string]any{"clipped_samples": int64(3), "max_volume_db": -6.0})
	if result["severity"] != "blocking" {
		t.Fatalf("result = %v", result)
	}
}

func TestAudioQualityCheck(t *testing.T) {
	// 无音轨 → info。
	result, err := AudioQualityCheck(false, nil, 10.0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result["stream_present"] != false || result["severity"] != "info" || result["abnormal"] != false {
		t.Fatalf("result = %v", result)
	}

	// 全程静音 → blocking。
	result, err = AudioQualityCheck(true, []map[string]any{
		{"start_seconds": 0.0, "end_seconds": 10.0, "duration_seconds": 10.0},
	}, 10.0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result["severity"] != "blocking" || result["abnormal"] != true {
		t.Fatalf("result = %v", result)
	}

	// 长静音（无 duration，用 end-start 推导）→ warning。
	result, err = AudioQualityCheck(true, []map[string]any{
		{"start_seconds": 2.0, "end_seconds": 8.0},
	}, 10.0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result["severity"] != "warning" || result["abnormal"] != true {
		t.Fatalf("result = %v", result)
	}

	// 短静音 → 无 issue。
	result, err = AudioQualityCheck(true, []map[string]any{
		{"start_seconds": 2.0, "end_seconds": 3.0, "duration_seconds": 1.0},
	}, 10.0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result["severity"] != nil || result["abnormal"] != false {
		t.Fatalf("result = %v", result)
	}
}

func TestExpectedTimelineDuration(t *testing.T) {
	timeline := []any{
		map[string]any{"duration_seconds": 5.0},
		map[string]any{"duration_seconds": 4.0, "trim_start_seconds": 1.0},
		map[string]any{"duration_seconds": 3.0, "transition": map[string]any{"type": "fade", "duration_seconds": 0.5}},
	}
	total, err := ExpectedTimelineDuration(timeline, nil)
	if err != nil {
		t.Fatalf("duration: %v", err)
	}
	// 5 + 3 + 3，无边界转场（转场挂在第 3 条目上、其右侧无相邻段）。
	if math.Abs(total-11.0) > 1e-9 {
		t.Fatalf("total = %v", total)
	}

	// 相邻段之间的 fade 重叠扣减时长。
	timeline = []any{
		map[string]any{"duration_seconds": 5.0, "transition": map[string]any{"type": "fade", "duration_seconds": 0.5}},
		map[string]any{"duration_seconds": 5.0},
	}
	total, err = ExpectedTimelineDuration(timeline, nil)
	if err != nil {
		t.Fatalf("duration: %v", err)
	}
	if math.Abs(total-9.5) > 1e-9 {
		t.Fatalf("total = %v", total)
	}

	// cut 不可渲染、时长超相邻段长 → 退化为硬切。
	timeline = []any{
		map[string]any{"duration_seconds": 5.0, "transition": map[string]any{"type": "cut", "duration_seconds": 2.0}},
		map[string]any{"duration_seconds": 5.0},
	}
	total, err = ExpectedTimelineDuration(timeline, nil)
	if err != nil {
		t.Fatalf("duration: %v", err)
	}
	if math.Abs(total-10.0) > 1e-9 {
		t.Fatalf("total = %v", total)
	}

	// packaging 启用分节时长累加。
	timeline = []any{map[string]any{"duration_seconds": 5.0}}
	total, err = ExpectedTimelineDuration(timeline, map[string]any{
		"cover": map[string]any{"duration_seconds": 1.5},
		"intro": map[string]any{"enabled": false, "duration_seconds": 9.0},
		"outro": nil,
	})
	if err != nil {
		t.Fatalf("duration: %v", err)
	}
	if math.Abs(total-6.5) > 1e-9 {
		t.Fatalf("total = %v", total)
	}

	// 非法时长 → 错误。
	_, err = ExpectedTimelineDuration([]any{map[string]any{"duration_seconds": -1.0}}, nil)
	if err == nil {
		t.Fatal("want error for negative duration")
	}
}

func TestClassifyBlackFrameEdges(t *testing.T) {
	// 中段黑场 → blocking。
	result, err := ClassifyBlackFrameEdges([]map[string]any{
		{"start_seconds": 1.0, "end_seconds": 2.0, "duration_seconds": 1.0},
	}, 10.0)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if result["severity"] != "blocking" || result["full_duration"] != false {
		t.Fatalf("result = %v", result)
	}
	if len(result["middle_segments"].([]any)) != 1 {
		t.Fatalf("middle = %v", result["middle_segments"])
	}

	// 片头黑场 → warning。
	result, err = ClassifyBlackFrameEdges([]map[string]any{
		{"start_seconds": 0.0, "end_seconds": 0.5, "duration_seconds": 0.5},
	}, 10.0)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if result["severity"] != "warning" || len(result["opening_segments"].([]any)) != 1 {
		t.Fatalf("result = %v", result)
	}

	// 全程黑场 → blocking + full_duration。
	result, err = ClassifyBlackFrameEdges([]map[string]any{
		{"start_seconds": 0.0, "end_seconds": 10.0, "duration_seconds": 10.0},
	}, 10.0)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if result["severity"] != "blocking" || result["full_duration"] != true {
		t.Fatalf("result = %v", result)
	}

	// 缺 start/end 的段被跳过。
	result, err = ClassifyBlackFrameEdges([]map[string]any{{"duration_seconds": 1.0}}, 10.0)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if result["detected"] != false || result["severity"] != nil {
		t.Fatalf("result = %v", result)
	}
}

func TestParseBlackFrameLog(t *testing.T) {
	log := "[blackdetect @ 0x1] black_start:0.5 black_end:1.25 black_duration:0.75\n" +
		"[blackdetect @ 0x1] black_start:8 black_end:9.5 black_duration:1.5\n"
	segments := ParseBlackFrameLog(log)
	if len(segments) != 2 {
		t.Fatalf("segments = %v", segments)
	}
	if segments[0]["start_seconds"] != 0.5 || segments[0]["end_seconds"] != 1.25 || segments[0]["duration_seconds"] != 0.75 {
		t.Fatalf("first = %v", segments[0])
	}
	if segments[1]["start_seconds"] != 8.0 || segments[1]["duration_seconds"] != 1.5 {
		t.Fatalf("second = %v", segments[1])
	}
}
