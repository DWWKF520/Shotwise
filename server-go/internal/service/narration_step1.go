// 说书（narration）step1 片段拆分的 schema / 解析 / 后校验（对应 lib/script_models.py 的
// NarrationStep1Draft 与 server/agent_runtime/sdk_tools/text_generation.py 的校验函数）。
//
// step1 产物落 drafts/episode_N/step1_segments.json，供 generate_episode_script（narration）
// 透传消费：novel_text 逐字保留原文、由 step2 透传，不经 step2 的 LLM 重出。
package service

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// NarrationStep1DraftSchema 是 NarrationStep1Draft 的 JSON Schema（顶层容忍附加字段）。
//
// 片段时长的成员约束（∈ supported_durations）由 ValidateNarrationSegments 兜底——静态
// NarrationStep1Segment.duration_seconds 是 ge=1, le=60 开区间，不在 schema 层枚举硬约束。
func NarrationStep1DraftSchema() map[string]any {
	segment := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"segment_id":            map[string]any{"type": "string", "minLength": 1},
			"novel_text":            map[string]any{"type": "string", "minLength": 1},
			"duration_seconds":      map[string]any{"type": "integer", "minimum": 1, "maximum": 60},
			"segment_break":         map[string]any{"type": "boolean"},
			"characters_in_segment": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"scenes":                map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"props":                 map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required":             []any{"segment_id", "novel_text", "duration_seconds", "characters_in_segment", "scenes", "props"},
		"additionalProperties": false,
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"segments": map[string]any{"type": "array", "minItems": 1, "items": segment},
		},
		"required": []any{"segments"},
	}
}

// ParseNarrationStep1Draft 解析 step1 拆分响应并校验顶层形状为 {segments: [...]}（非空数组）。
func ParseNarrationStep1Draft(responseText string) ([]map[string]any, error) {
	root, err := parseJSONObject(responseText)
	if err != nil {
		return nil, err
	}
	rawSegments, ok := root["segments"].([]any)
	if !ok || len(rawSegments) == 0 {
		return nil, fmt.Errorf("step1 拆分内容结构异常：segments 必须是非空的片段对象数组")
	}
	segments := make([]map[string]any, 0, len(rawSegments))
	for _, item := range rawSegments {
		segment, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("step1 拆分内容结构异常：segments 内必须都是对象")
		}
		segments = append(segments, segment)
	}
	return segments, nil
}

// ValidateNarrationSegments 按 narration step1 读取契约做后校验：segment_id 唯一 +
// novel_text 非空白 + duration ∈ supported_durations。
//
// 纯空白 novel_text（如单个空格）能满足 schema 的 minLength 却不携带旁白内容，会产生「有时长但
// 无旁白」的哑片段；超出 supported_durations 的时长能过 schema 开区间。此处 fail loud 补上，
// 与 ScriptGenerator 的 step1 读取同口径——只有经此校验的内容才写盘成为 step1 真值源。
func ValidateNarrationSegments(segments []map[string]any, supportedDurations []int) error {
	ids := make([]string, 0, len(segments))
	for _, segment := range segments {
		ids = append(ids, fmt.Sprint(segment["segment_id"]))
	}
	if dupes := duplicateStrings(ids); len(dupes) > 0 {
		return fmt.Errorf("step1 拆分内容 segment_id 重复: %v", dupes)
	}
	var blank []string
	for _, segment := range segments {
		if strings.TrimSpace(fmt.Sprint(segment["novel_text"])) == "" {
			blank = append(blank, fmt.Sprint(segment["segment_id"]))
		}
	}
	if len(blank) > 0 {
		sort.Strings(blank)
		return fmt.Errorf("step1 拆分内容 novel_text 为空白: %v", blank)
	}
	allowed := map[int]bool{}
	for _, d := range supportedDurations {
		allowed[d] = true
	}
	var bad []int
	for _, segment := range segments {
		duration := intValueOrZero(segment["duration_seconds"])
		if !allowed[duration] {
			bad = append(bad, duration)
		}
	}
	if len(bad) > 0 {
		bad = uniqueSortedInts(bad)
		allowedList := normalizeDurations(supportedDurations)
		return fmt.Errorf("step1 拆分内容 duration_seconds 非法（不在 %v 内）: %v", allowedList, bad)
	}
	return nil
}

// ValidateNarrationAssetReferences 校验片段登记的角色 / 场景 / 道具已在 project.json 对应表注册。
//
// 与参考路径侧对资产引用的校验同口径：只信登记过的资产名，不允许模型发明或拼错的名称被当真值写盘。
func ValidateNarrationAssetReferences(
	segments []map[string]any, characters, scenes, props []string,
) error {
	known := map[string]map[string]bool{
		"characters_in_segment": stringSet(characters),
		"scenes":                stringSet(scenes),
		"props":                 stringSet(props),
	}
	missing := map[string][]string{}
	for _, segment := range segments {
		sid := fmt.Sprint(segment["segment_id"])
		for _, field := range []string{"characters_in_segment", "scenes", "props"} {
			var bad []string
			for _, name := range listStrings(segment[field]) {
				if !known[field][name] {
					bad = append(bad, name)
				}
			}
			if len(bad) > 0 {
				bad = uniqueSortedStrings(bad)
				missing[sid+"."+field] = bad
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("step1 拆分内容引用了未登记的资产名: %v；资产名必须逐字取自 project.json 三张表", missing)
	}
	return nil
}

var narrationCoverageWhitespaceRe = regexp.MustCompile(`\s+`)

// normalizeForCoverage 把连续空白折叠为单个空格，不删除空白本身，仅消除空白的类型 / 数量差异。
func normalizeForCoverage(text string) string {
	return strings.TrimSpace(narrationCoverageWhitespaceRe.ReplaceAllString(text, " "))
}

// ValidateNarrationNovelTextCoverage 机械校验片段 novel_text 按序、完整覆盖源文。
//
// 杜绝模型删减 / 改写 / 重排后仍被当真值落盘。片段边界处的空白存在与否天然歧义，故仅在交界处
// 允许可选的单个空格；片段自身文本内部与源文其余部分一律要求折叠后逐字相等。
func ValidateNarrationNovelTextCoverage(segments []map[string]any, novelText string) error {
	parts := make([]string, 0, len(segments))
	for _, segment := range segments {
		parts = append(parts, regexp.QuoteMeta(normalizeForCoverage(fmt.Sprint(segment["novel_text"]))))
	}
	pattern := "^" + strings.Join(parts, " ?") + "$"
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("step1 拆分内容覆盖校验正则构造失败: %w", err)
	}
	if !re.MatchString(normalizeForCoverage(novelText)) {
		return fmt.Errorf("step1 拆分内容 novel_text 未逐字、完整覆盖小说原文（存在删减、改写或重排）")
	}
	return nil
}

// parseJSONObject 剥离 markdown 代码栅栏后解析为 JSON 对象（顶层须为对象）。
func parseJSONObject(responseText string) (map[string]any, error) {
	text := stripJSONCodeFences(responseText)
	var root map[string]any
	if err := json.Unmarshal([]byte(text), &root); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	if root == nil {
		return nil, fmt.Errorf("结构异常：顶层应为对象")
	}
	return root, nil
}

// --- 小工具 ---

func duplicateStrings(values []string) []string {
	counts := map[string]int{}
	for _, v := range values {
		counts[v]++
	}
	var out []string
	for v, n := range counts {
		if n > 1 {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func uniqueSortedStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func uniqueSortedInts(values []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(values))
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

// intValueOrZero 取整数语义的值（JSON 数字的整数值 / 纯数字字符串），否则 0。
func intValueOrZero(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		if v == float64(int(v)) {
			return int(v)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return 0
}
