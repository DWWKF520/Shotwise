package media

// 确定性字幕 cue 校验与 SRT/VTT 序列化（对应 lib/media_assembly/subtitles.py）。
//
// 导出链路（剪映草稿包）在创建装配计划的结构校验之外，用本模块对 cue 做第二次、
// 更严格的校验：有限数值、严格递增区间、非空文本、无 NUL，随后才序列化。

import (
	"fmt"
	"math"
	"strings"
)

// SubtitleValidationError 表示 cue 非法或不安全（对应 SubtitleValidationError(ValueError)）。
type SubtitleValidationError struct{ Msg string }

func (e *SubtitleValidationError) Error() string { return e.Msg }

// SubtitleCue 是以秒计时的字幕 cue；Identifier 为 nil 表示未提供。
type SubtitleCue struct {
	StartSeconds float64
	EndSeconds   float64
	Text         string
	Identifier   *string
}

// subtitleTime 校验时间字段：必须是有限非负数值（bool 不算数值）。
func subtitleTime(value any, name string) (float64, error) {
	var f float64
	switch v := value.(type) {
	case float64:
		f = v
	case int:
		f = float64(v)
	case int64:
		f = float64(v)
	default:
		return 0, &SubtitleValidationError{Msg: name + " must be a finite number"}
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, &SubtitleValidationError{Msg: name + " must be a finite number"}
	}
	if f < 0 {
		return 0, &SubtitleValidationError{Msg: name + " must be non-negative"}
	}
	return f, nil
}

// subtitleCueFromValue 从 JSON 反序列化出的映射校验并归一一条 cue。
func subtitleCueFromValue(value any, index int) (SubtitleCue, error) {
	mapping, ok := value.(map[string]any)
	if !ok {
		return SubtitleCue{}, &SubtitleValidationError{Msg: fmt.Sprintf("cues[%d] must be a SubtitleCue or object", index)}
	}
	rawStart, ok := mapping["start_seconds"]
	if !ok {
		return SubtitleCue{}, &SubtitleValidationError{Msg: fmt.Sprintf("cues[%d] is missing start_seconds", index)}
	}
	rawEnd, ok := mapping["end_seconds"]
	if !ok {
		return SubtitleCue{}, &SubtitleValidationError{Msg: fmt.Sprintf("cues[%d] is missing end_seconds", index)}
	}
	rawText, ok := mapping["text"]
	if !ok {
		return SubtitleCue{}, &SubtitleValidationError{Msg: fmt.Sprintf("cues[%d] is missing text", index)}
	}
	start, err := subtitleTime(rawStart, fmt.Sprintf("cues[%d].start_seconds", index))
	if err != nil {
		return SubtitleCue{}, err
	}
	end, err := subtitleTime(rawEnd, fmt.Sprintf("cues[%d].end_seconds", index))
	if err != nil {
		return SubtitleCue{}, err
	}
	if end <= start {
		return SubtitleCue{}, &SubtitleValidationError{
			Msg: fmt.Sprintf("cues[%d].end_seconds must be greater than start_seconds", index),
		}
	}
	text, ok := rawText.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return SubtitleCue{}, &SubtitleValidationError{Msg: fmt.Sprintf("cues[%d].text must be a non-empty string", index)}
	}
	if strings.Contains(text, "\x00") {
		return SubtitleCue{}, &SubtitleValidationError{Msg: fmt.Sprintf("cues[%d].text must not contain NUL", index)}
	}
	var identifier *string
	if raw, present := mapping["identifier"]; present && raw != nil {
		s, ok := raw.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return SubtitleCue{}, &SubtitleValidationError{
				Msg: fmt.Sprintf("cues[%d].identifier must be a non-empty string when provided", index),
			}
		}
		identifier = &s
	}
	normalized := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	return SubtitleCue{StartSeconds: start, EndSeconds: end, Text: normalized, Identifier: identifier}, nil
}

// NormalizeSubtitleCues 校验 cue 并要求时序递增、互不重叠（对应 normalize_cues）。
func NormalizeSubtitleCues(raw []any) ([]SubtitleCue, error) {
	normalized := make([]SubtitleCue, 0, len(raw))
	for index, value := range raw {
		cue, err := subtitleCueFromValue(value, index)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, cue)
	}
	previousEnd := 0.0
	for index, cue := range normalized {
		if cue.StartSeconds < previousEnd {
			return nil, &SubtitleValidationError{Msg: fmt.Sprintf("cues[%d] overlaps the previous cue", index)}
		}
		previousEnd = cue.EndSeconds
	}
	return normalized, nil
}

// formatSubtitleTimestamp 输出 HH:MM:SS{sep}mmm；取整采用 banker's rounding，
// 与 Python round() 的 half-even 语义一致。
func formatSubtitleTimestamp(seconds float64, separator string) string {
	totalMilliseconds := int(math.RoundToEven(seconds * 1000))
	hours := totalMilliseconds / 3_600_000
	remainder := totalMilliseconds % 3_600_000
	minutes := remainder / 60_000
	remainder %= 60_000
	wholeSeconds := remainder / 1000
	milliseconds := remainder % 1000
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", hours, minutes, wholeSeconds, separator, milliseconds)
}

// GenerateSRT 把 cue 序列化为带稳定编号的 SRT 文本（对应 generate_srt）。
func GenerateSRT(rawCues []any) (string, error) {
	cues, err := NormalizeSubtitleCues(rawCues)
	if err != nil {
		return "", err
	}
	if len(cues) == 0 {
		return "", nil
	}
	blocks := make([]string, 0, len(cues))
	for index, cue := range cues {
		blocks = append(blocks, fmt.Sprintf(
			"%d\n%s --> %s\n%s",
			index+1,
			formatSubtitleTimestamp(cue.StartSeconds, ","),
			formatSubtitleTimestamp(cue.EndSeconds, ","),
			cue.Text,
		))
	}
	return strings.Join(blocks, "\n\n") + "\n", nil
}

// GenerateVTT 把 cue 序列化为带可选标识行的 WebVTT 文本（对应 generate_vtt）。
func GenerateVTT(rawCues []any) (string, error) {
	cues, err := NormalizeSubtitleCues(rawCues)
	if err != nil {
		return "", err
	}
	blocks := []string{"WEBVTT", ""}
	for _, cue := range cues {
		lines := make([]string, 0, 3)
		if cue.Identifier != nil {
			lines = append(lines, *cue.Identifier)
		}
		lines = append(lines,
			fmt.Sprintf(
				"%s --> %s",
				formatSubtitleTimestamp(cue.StartSeconds, "."),
				formatSubtitleTimestamp(cue.EndSeconds, "."),
			),
			cue.Text,
		)
		blocks = append(blocks, strings.Join(lines, "\n"))
		blocks = append(blocks, "")
	}
	return strings.Join(blocks, "\n"), nil
}
