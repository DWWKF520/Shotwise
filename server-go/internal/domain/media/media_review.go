package media

// 渲染产物质量审查的纯函数集（对应 lib/media_assembly/review.py，
// 含 plan.py 的 resolve_timeline_transitions）。

import (
	"fmt"
	"math"
	"regexp"
	"sort"
)

// reviewNumberPattern 匹配 FFmpeg 日志里的带符号十进制数（对应 _NUMBER）。
const reviewNumberPattern = `[-+]?(?:\d+(?:\.\d*)?|\.\d+)`

var (
	reviewSilenceStart = regexp.MustCompile(`(?i)silence_start:\s*(?P<start>` + reviewNumberPattern + `)`)
	reviewSilenceEnd   = regexp.MustCompile(
		`(?i)silence_end:\s*(?P<end>` + reviewNumberPattern + `).*?silence_duration:\s*(?P<duration>` + reviewNumberPattern + `)`)
	reviewMeanVolume = regexp.MustCompile(`(?i)mean_volume:\s*(?P<value>` + reviewNumberPattern + `)\s*dB`)
	reviewMaxVolume  = regexp.MustCompile(`(?i)max_volume:\s*(?P<value>` + reviewNumberPattern + `)\s*dB`)
	// Python 侧该模式带 re.DOTALL（integrated loudness 换行后才出现 I:）。
	reviewIntegratedLUFS = regexp.MustCompile(
		`(?is)(?:integrated\s+loudness:.*?\bI:|\bI:)\s*(?P<value>` + reviewNumberPattern + `)\s*LUFS`)
	reviewClippedSamples = regexp.MustCompile(
		`(?i)(?:number\s+of\s+clipped\s+samples|clipping_count|histogram_0db)\s*[:=]\s*(?P<value>` + reviewNumberPattern + `)`)
	reviewBlackFrame = regexp.MustCompile(
		`black_start:(?P<start>[0-9.]+).*?black_end:(?P<end>[0-9.]+).*?black_duration:(?P<duration>[0-9.]+)`)
)

// reviewNumber 对应 Python _number：bool / 非数值 / 非有限数一律视为缺失。
func reviewNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case bool:
		return 0, false
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

// reviewDuration 对应 Python _duration：必须是非负有限数，否则报错。
func reviewDuration(value any, name string) (float64, error) {
	result, ok := reviewNumber(value)
	if !ok || result < 0 {
		return 0, fmt.Errorf("%s must be a finite non-negative number", name)
	}
	return result, nil
}

// SubtitleBoundsCheck 找出超出渲染时长或相互重叠的字幕 cue
// （对应 subtitle_bounds_check）。
func SubtitleBoundsCheck(subtitle any, durationSeconds float64) (map[string]any, error) {
	duration, err := reviewDuration(durationSeconds, "duration_seconds")
	if err != nil {
		return nil, err
	}
	config, _ := subtitle.(map[string]any)
	if config == nil {
		config = map[string]any{}
	}
	var rawCues []any
	if rawValue, present := config["cues"]; present {
		list, isList := rawValue.([]any)
		if !isList {
			return map[string]any{
				"detected":  true,
				"valid":     false,
				"cue_count": 0,
				"severity":  "blocking",
				"items":     []any{map[string]any{"code": "cues_not_a_list", "severity": "blocking"}},
			}, nil
		}
		rawCues = list
	} else {
		rawCues = []any{}
	}
	issues := make([]map[string]any, 0)

	type normalizedCue struct {
		index int
		start float64
		end   float64
	}
	normalized := make([]normalizedCue, 0, len(rawCues))
	for index, rawCue := range rawCues {
		cue, _ := rawCue.(map[string]any)
		if cue == nil {
			cue = map[string]any{}
		}
		start, startOK := reviewNumber(cue["start_seconds"])
		end, endOK := reviewNumber(cue["end_seconds"])
		base := map[string]any{"index": index}
		cueID, idPresent := cue["id"]
		if !idPresent {
			cueID = cue["identifier"]
		}
		if s, ok := cueID.(string); ok && s != "" {
			base["id"] = s
		}
		if !startOK || !endOK {
			issue := map[string]any{"code": "invalid_interval"}
			for k, v := range base {
				issue[k] = v
			}
			issues = append(issues, issue)
			continue
		}
		normalized = append(normalized, normalizedCue{index: index, start: start, end: end})
		if start < 0 || end > duration+0.05 {
			issues = append(issues, map[string]any{
				"code": "out_of_bounds", "start_seconds": start, "end_seconds": end, "duration_seconds": duration,
				"index": index,
			})
		}
		if end <= start {
			issues = append(issues, map[string]any{
				"code": "invalid_interval", "start_seconds": start, "end_seconds": end, "index": index,
			})
		}
	}

	ordered := make([]normalizedCue, len(normalized))
	copy(ordered, normalized)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].start != ordered[j].start {
			return ordered[i].start < ordered[j].start
		}
		if ordered[i].end != ordered[j].end {
			return ordered[i].end < ordered[j].end
		}
		return ordered[i].index < ordered[j].index
	})
	for i := 1; i < len(ordered); i++ {
		previous, current := ordered[i-1], ordered[i]
		if current.start < previous.end {
			overlapEnd := previous.end
			if current.end < overlapEnd {
				overlapEnd = current.end
			}
			issues = append(issues, map[string]any{
				"code": "overlap", "previous_index": previous.index, "index": current.index,
				"overlap_start_seconds": current.start, "overlap_end_seconds": overlapEnd,
			})
		}
	}

	items := make([]any, 0, len(issues))
	for _, issue := range issues {
		if _, present := issue["severity"]; !present {
			issue["severity"] = "blocking"
		}
		items = append(items, issue)
	}
	severity := any(nil)
	if len(issues) > 0 {
		severity = "blocking"
	}
	return map[string]any{
		"detected":  len(issues) > 0,
		"valid":     len(issues) == 0,
		"cue_count": len(rawCues),
		"severity":  severity,
		"items":     items,
	}, nil
}

// ParseSilencedetectLog 把 FFmpeg silencedetect 输出解析成确定的静音区间
// （对应 parse_silencedetect_log）：start 与 end 按出现顺序配对，
// 配不上 end 的 start 以 nil 收尾。
func ParseSilencedetectLog(log string) []map[string]any {
	starts := reviewSilenceStart.FindAllStringSubmatch(log, -1)
	ends := reviewSilenceEnd.FindAllStringSubmatch(log, -1)
	segments := make([]map[string]any, 0)
	endIndex := 0
	for _, startMatch := range starts {
		start := parseFloatCompat(startMatch[1])
		for endIndex < len(ends) && parseFloatCompat(ends[endIndex][1]) < start {
			endIndex++
		}
		if endIndex < len(ends) {
			end := parseFloatCompat(ends[endIndex][1])
			duration := parseFloatCompat(ends[endIndex][2])
			if end >= start {
				segments = append(segments, map[string]any{
					"start_seconds": start, "end_seconds": end, "duration_seconds": duration,
				})
				endIndex++
				continue
			}
		}
		segments = append(segments, map[string]any{
			"start_seconds": start, "end_seconds": nil, "duration_seconds": nil,
		})
	}
	return segments
}

// ParseAudioMetricsLog 把 FFmpeg 音频分析输出解析成稳定的响度指标
// （对应 parse_audio_metrics_log）。
func ParseAudioMetricsLog(log string) map[string]any {
	var meanVolume, maxVolume, integratedLUFS any
	if m := reviewMeanVolume.FindStringSubmatch(log); m != nil {
		meanVolume = parseFloatCompat(m[1])
	}
	if m := reviewMaxVolume.FindStringSubmatch(log); m != nil {
		maxVolume = parseFloatCompat(m[1])
	}
	if m := reviewIntegratedLUFS.FindStringSubmatch(log); m != nil {
		integratedLUFS = parseFloatCompat(m[1])
	}
	var clippedSamples any
	if m := reviewClippedSamples.FindStringSubmatch(log); m != nil {
		if value, ok := reviewNumber(parseFloatCompat(m[1])); ok && value >= 0 {
			clippedSamples = int64(value)
		}
	}
	return map[string]any{
		"mean_volume_db":  meanVolume,
		"max_volume_db":   maxVolume,
		"integrated_lufs": integratedLUFS,
		"clipped_samples": clippedSamples,
	}
}

func parseFloatCompat(raw string) float64 {
	var value float64
	_, _ = fmt.Sscanf(raw, "%g", &value)
	return value
}

// AudioLevelCheck 分类偏离短视频目标响度与削波峰值（对应 audio_level_check），
// 阈值取 Python 侧默认值。
func AudioLevelCheck(metrics map[string]any) map[string]any {
	const (
		targetLufs        = -16.0
		loudnessTolerance = 6.0
		clippingWarningDb = -0.1
	)
	integratedLufs, integratedOK := reviewNumber(metrics["integrated_lufs"])
	meanVolumeDb, meanOK := reviewNumber(metrics["mean_volume_db"])
	maxVolumeDb, maxOK := reviewNumber(metrics["max_volume_db"])
	clippedValue, clippedOK := reviewNumber(metrics["clipped_samples"])
	var clippedSamples any
	if clippedOK {
		clippedSamples = int64(clippedValue)
	}
	issues := make([]any, 0)

	if integratedOK && !(targetLufs-loudnessTolerance <= integratedLufs && integratedLufs <= targetLufs+loudnessTolerance) {
		issues = append(issues, map[string]any{
			"code": "loudness_out_of_range", "severity": "warning",
			"integrated_lufs": integratedLufs, "target_lufs": targetLufs, "tolerance_lufs": loudnessTolerance,
		})
	}

	clippingDetected := clippedOK && clippedValue > 0
	if maxOK && maxVolumeDb >= 0 {
		clippingDetected = true
	}
	if clippingDetected {
		issues = append(issues, map[string]any{
			"code": "audio_clipping_detected", "severity": "blocking",
			"max_volume_db": nilOrValue(maxOK, maxVolumeDb), "clipped_samples": clippedSamples,
		})
	} else if maxOK && maxVolumeDb >= clippingWarningDb {
		issues = append(issues, map[string]any{
			"code": "audio_peak_near_clipping", "severity": "warning",
			"max_volume_db": maxVolumeDb, "threshold_db": clippingWarningDb,
		})
	}

	severity := any(nil)
	for _, candidate := range []string{"blocking", "warning"} {
		found := false
		for _, issue := range issues {
			if m, ok := issue.(map[string]any); ok && m["severity"] == candidate {
				found = true
				break
			}
		}
		if found {
			severity = candidate
			break
		}
	}
	valid := true
	for _, issue := range issues {
		if m, ok := issue.(map[string]any); ok && m["severity"] == "blocking" {
			valid = false
			break
		}
	}
	return map[string]any{
		"available":       integratedOK || meanOK || maxOK || clippedOK,
		"mean_volume_db":  nilOrValue(meanOK, meanVolumeDb),
		"max_volume_db":   nilOrValue(maxOK, maxVolumeDb),
		"integrated_lufs": nilOrValue(integratedOK, integratedLufs),
		"clipped_samples": clippedSamples,
		"detected":        len(issues) > 0,
		"valid":           valid,
		"severity":        severity,
		"issues":          issues,
	}
}

func nilOrValue(ok bool, value float64) any {
	if !ok {
		return nil
	}
	return value
}

// AudioQualityCheck 分类缺音轨、长静音与全程静音（对应 audio_quality_check）。
func AudioQualityCheck(audioStreamPresent bool, silenceSegments []map[string]any, durationSeconds float64) (map[string]any, error) {
	duration, err := reviewDuration(durationSeconds, "duration_seconds")
	if err != nil {
		return nil, err
	}
	segments := make([]any, 0, len(silenceSegments))
	for _, segment := range silenceSegments {
		copied := map[string]any{}
		for k, v := range segment {
			copied[k] = v
		}
		segments = append(segments, copied)
	}
	if !audioStreamPresent {
		return map[string]any{
			"stream_present":   false,
			"silence_segments": segments,
			"abnormal":         false,
			"severity":         "info",
			"issues":           []any{map[string]any{"code": "audio_stream_missing", "severity": "info"}},
		}, nil
	}

	fullSilence := false
	longSilence := false
	for _, segment := range silenceSegments {
		start, startOK := reviewNumber(segment["start_seconds"])
		end, endOK := reviewNumber(segment["end_seconds"])
		segmentDuration, durationOK := reviewNumber(segment["duration_seconds"])
		if !durationOK && startOK {
			endValue := duration
			if endOK {
				endValue = end
			}
			segmentDuration = endValue - start
			if segmentDuration < 0 {
				segmentDuration = 0
			}
			durationOK = true
		}
		if durationOK && segmentDuration >= 5.0 {
			longSilence = true
		}
		if startOK && start <= 0.05 && (!endOK || end >= duration-0.05) {
			fullSilence = true
		}
	}

	severity := any(nil)
	issues := make([]any, 0)
	switch {
	case fullSilence:
		severity = "blocking"
		issues = append(issues, map[string]any{"code": "audio_silence_detected", "severity": "blocking"})
	case longSilence:
		severity = "warning"
		issues = append(issues, map[string]any{"code": "long_silence_detected", "severity": "warning"})
	}
	return map[string]any{
		"stream_present":   true,
		"silence_segments": segments,
		"abnormal":         fullSilence || longSilence,
		"severity":         severity,
		"issues":           issues,
	}, nil
}

// declaredTransition 是时间线一条目声明的转场。
type declaredTransition struct {
	typ      any
	duration any
}

// ExpectedTimelineDuration 计算时间线内容加片头片尾的期望时长
// （对应 expected_timeline_duration）：可渲染转场重叠会缩短实际时长。
func ExpectedTimelineDuration(timeline []any, packaging any) (float64, error) {
	durations := make([]float64, 0, len(timeline))
	transitions := make([]declaredTransition, 0, len(timeline))
	for index, item := range timeline {
		entry, _ := item.(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		duration, err := reviewDuration(entry["duration_seconds"], fmt.Sprintf("timeline[%d].duration_seconds", index))
		if err != nil {
			return 0, err
		}
		trimStart := 0.0
		if _, present := entry["trim_start_seconds"]; present {
			if trimStart, err = reviewDuration(entry["trim_start_seconds"], fmt.Sprintf("timeline[%d].trim_start_seconds", index)); err != nil {
				return 0, err
			}
		}
		trimEnd := 0.0
		if _, present := entry["trim_end_seconds"]; present {
			if trimEnd, err = reviewDuration(entry["trim_end_seconds"], fmt.Sprintf("timeline[%d].trim_end_seconds", index)); err != nil {
				return 0, err
			}
		}
		effective := duration - trimStart - trimEnd
		if effective < 0 {
			return 0, fmt.Errorf("timeline[%d] has negative effective duration", index)
		}
		durations = append(durations, effective)
		if transition, ok := entry["transition"].(map[string]any); ok {
			declaredDuration := any(0)
			if v, present := transition["duration_seconds"]; present {
				declaredDuration = v
			}
			transitions = append(transitions, declaredTransition{typ: transition["type"], duration: declaredDuration})
		} else {
			transitions = append(transitions, declaredTransition{})
		}
	}

	total := 0.0
	for _, d := range durations {
		total += d
	}
	for _, overlap := range resolveTimelineTransitions(transitions, durations) {
		total -= overlap
	}

	if packagingMap, ok := packaging.(map[string]any); ok {
		for _, name := range []string{"cover", "intro", "outro"} {
			section, _ := packagingMap[name].(map[string]any)
			if section == nil || !PackagingSectionEnabled(section) {
				continue
			}
			rawDuration, present := section["duration_seconds"]
			if !present || rawDuration == nil {
				continue
			}
			sectionDuration, err := reviewDuration(rawDuration, fmt.Sprintf("packaging.%s.duration_seconds", name))
			if err != nil {
				return 0, err
			}
			total += sectionDuration
		}
	}
	return total, nil
}

// resolveTimelineTransitions 把声明的转场解析成各边界真正渲染的重叠时长
// （对应 resolve_timeline_transitions）：类型不可渲染、时长非正、相邻段
// 不长于重叠时退化为硬切；中间段装不下两侧重叠时清掉靠左的那一侧重叠。
func resolveTimelineTransitions(transitions []declaredTransition, durations []float64) []float64 {
	boundaryCount := len(durations)
	if len(transitions)+1 < boundaryCount {
		boundaryCount = len(transitions) + 1
	}
	boundaryCount--
	if boundaryCount < 0 {
		boundaryCount = 0
	}
	boundaries := make([]float64, boundaryCount)
	for index := range boundaries {
		declaredType, _ := transitions[index].typ.(string)
		if !RenderableTimelineTransitions[declaredType] {
			continue
		}
		if _, isBool := transitions[index].duration.(bool); isBool {
			continue
		}
		overlap, ok := reviewNumber(transitions[index].duration)
		if !ok || overlap <= 0 {
			continue
		}
		if durations[index] > overlap && durations[index+1] > overlap {
			boundaries[index] = overlap
		}
	}
	for index := 1; index < len(boundaries); index++ {
		if boundaries[index-1] <= 0 || boundaries[index] <= 0 {
			continue
		}
		if durations[index] < boundaries[index-1]+boundaries[index] {
			boundaries[index-1] = 0
		}
	}
	return boundaries
}

// ClassifyBlackFrameEdges 把黑场区间分类为片头、片尾、全程或中段
// （对应 classify_black_frame_edges）。
func ClassifyBlackFrameEdges(segments []map[string]any, durationSeconds float64) (map[string]any, error) {
	duration, err := reviewDuration(durationSeconds, "duration_seconds")
	if err != nil {
		return nil, err
	}
	classified := make([]any, 0, len(segments))
	opening := make([]any, 0)
	ending := make([]any, 0)
	middle := make([]any, 0)
	fullDuration := false
	for _, rawSegment := range segments {
		start, startOK := reviewNumber(rawSegment["start_seconds"])
		end, endOK := reviewNumber(rawSegment["end_seconds"])
		if !startOK || !endOK {
			continue
		}
		item := map[string]any{}
		for k, v := range rawSegment {
			item[k] = v
		}
		atStart := start <= 0.05
		atEnd := end >= duration-0.05
		edge := "middle"
		switch {
		case atStart && atEnd:
			edge = "full"
		case atStart:
			edge = "opening"
		case atEnd:
			edge = "ending"
		}
		item["edge"] = edge
		classified = append(classified, item)
		switch edge {
		case "full":
			fullDuration = true
		case "opening":
			opening = append(opening, item)
		case "ending":
			ending = append(ending, item)
		case "middle":
			middle = append(middle, item)
		}
	}

	blocking := make([]any, 0)
	warning := make([]any, 0)
	for _, raw := range classified {
		item, _ := raw.(map[string]any)
		switch item["edge"] {
		case "full", "middle":
			blocking = append(blocking, item)
		case "opening", "ending":
			warning = append(warning, item)
		}
	}
	severity := any(nil)
	if len(blocking) > 0 {
		severity = "blocking"
	} else if len(warning) > 0 {
		severity = "warning"
	}
	return map[string]any{
		"detected":          len(classified) > 0,
		"segments":          classified,
		"opening_segments":  opening,
		"ending_segments":   ending,
		"middle_segments":   middle,
		"full_duration":     fullDuration,
		"blocking_segments": blocking,
		"warning_segments":  warning,
		"severity":          severity,
	}, nil
}

// ParseBlackFrameLog 把 FFmpeg blackdetect 的 stderr 解析成黑场区间
// （对应 media_rendering._detect_black_frames 的解析段）。
func ParseBlackFrameLog(log string) []map[string]any {
	segments := make([]map[string]any, 0)
	for _, match := range reviewBlackFrame.FindAllStringSubmatch(log, -1) {
		segments = append(segments, map[string]any{
			"start_seconds":    parseFloatCompat(match[1]),
			"end_seconds":      parseFloatCompat(match[2]),
			"duration_seconds": parseFloatCompat(match[3]),
		})
	}
	return segments
}
