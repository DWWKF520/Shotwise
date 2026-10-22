// 结构化 step1 中间态的 web 保存路径校验（对应 lib/script_models.py 的
// DramaNormalizedScript / NarrationStep1Draft / ReferenceStep1Draft 三个内容模型，及
// server/services/script_review.py::_STEP1_CONTENT_MODEL 的 model_validate().model_dump() 语义）。
//
// 与 LLM 产出侧校验器（narration_step1.go / drama_step1.go / reference_units.go 的扁平形状）
// 的分工：那些校验的是「两级 LLM 产出的书写层形状」，这里校验的是「web 编辑回写的 Pydantic
// 结构化形状」——顶层 extra=ignore（丢弃附带的运行时字段，如 narration 的 episode 头）、
// 嵌套模型 extra=forbid（挡 typo / 漂移）、缺省字段按模型默认值补齐后再落盘。
//
// 校验通过即返回规范化后的完整内容（等价 Python 的 model_dump()）：未知顶层键被剥掉、
// 默认值填充、utterance.speaker 空串归一为 null。落盘与指纹都取这份规范化结果，与
// Python 侧写盘内容语义一致（键序不影响指纹，指纹按键排序的规范化 dump 计算）。
package script

import (
	"github.com/shotwise/shotwise/internal/service/reference"
	"fmt"

	"github.com/shotwise/shotwise/internal/service"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
)

// ValidateStep1Content 按变体校验结构化 step1 内容，返回规范化后的完整内容。
// kind 取 Step1Kind 的返回值；不认识的变体名报错（调用方已排除 not_applicable）。
func ValidateStep1Content(kind string, content any) (map[string]any, error) {
	switch kind {
	case "drama":
		return validateDramaStep1Content(content)
	case "narration":
		return validateNarrationStep1Content(content)
	case step1KindReferenceVideo:
		return validateReferenceStep1Content(content)
	}
	return nil, fmt.Errorf("未知 step1 变体: %s", kind)
}

// --- drama：DramaNormalizedScript（title + scenes[DramaSceneContent]） ---
// 嵌套场景字段白名单复用 drama_step1.go 的 dramaSceneContentAllowed（两处模型同源）。

func validateDramaStep1Content(content any) (map[string]any, error) {
	obj, ok := content.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("drama step1 内容必须是 JSON 对象")
	}
	title, err := step1RequireString(obj, "title", "")
	if err != nil {
		return nil, err
	}
	rawScenes, err := step1RequireList(obj, "scenes")
	if err != nil {
		return nil, err
	}
	scenes := make([]any, 0, len(rawScenes))
	for index, raw := range rawScenes {
		scene, err := validateDramaSceneContent(raw, fmt.Sprintf("scenes.%d", index))
		if err != nil {
			return nil, err
		}
		scenes = append(scenes, scene)
	}
	return map[string]any{"title": title, "scenes": scenes}, nil
}

// validateDramaSceneContent 校验单个场景内容层并返回补齐默认值后的规范化 dict。
func validateDramaSceneContent(raw any, where string) (map[string]any, error) {
	scene, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s 必须是对象", where)
	}
	if err := service.RejectUnknownKeys(scene, dramaSceneContentAllowed, where); err != nil {
		return nil, err
	}
	sceneID, err := step1RequireNonEmptyString(scene, "scene_id", where)
	if err != nil {
		return nil, err
	}
	duration, err := step1IntWithDefault(scene, "duration_seconds", 8, 1, 60, where)
	if err != nil {
		return nil, err
	}
	segmentBreak, err := step1BoolWithDefault(scene, "segment_break", false, where)
	if err != nil {
		return nil, err
	}
	characters, err := step1RequireStringList(scene, "characters_in_scene", where)
	if err != nil {
		return nil, err
	}
	scenes, err := step1StringListWithDefault(scene, "scenes", where)
	if err != nil {
		return nil, err
	}
	props, err := step1StringListWithDefault(scene, "props", where)
	if err != nil {
		return nil, err
	}
	description, err := step1RequireString(scene, "scene_description", where)
	if err != nil {
		return nil, err
	}
	rawUtterances, err := step1ListWithDefault(scene, "utterances", where)
	if err != nil {
		return nil, err
	}
	utterances := make([]any, 0, len(rawUtterances))
	for index, rawUtterance := range rawUtterances {
		utterance, err := validateUtterance(rawUtterance, fmt.Sprintf("%s.utterances.%d", where, index))
		if err != nil {
			return nil, err
		}
		utterances = append(utterances, utterance)
	}
	sourceText, err := step1StringWithDefault(scene, "source_text", "", where)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"scene_id":            sceneID,
		"duration_seconds":    duration,
		"segment_break":       segmentBreak,
		"characters_in_scene": characters,
		"scenes":              scenes,
		"props":               props,
		"scene_description":   description,
		"utterances":          utterances,
		"source_text":         sourceText,
	}, nil
}

// validateUtterance 校验单条发声并返回规范化 dict（speaker 空串/纯空白归一为 null）。
// 字段白名单复用 drama_step1.go 的 utteranceAllowed（两处模型同源）。
func validateUtterance(raw any, where string) (map[string]any, error) {
	item, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s 必须是对象", where)
	}
	if err := service.RejectUnknownKeys(item, utteranceAllowed, where); err != nil {
		return nil, err
	}
	kind, err := step1RequireString(item, "kind", where)
	if err != nil {
		return nil, err
	}
	if kind != "dialogue" && kind != "voiceover" {
		return nil, fmt.Errorf("%s.kind 必须是 dialogue 或 voiceover，收到 %q", where, kind)
	}
	var speaker any
	if rawSpeaker, present := item["speaker"]; present && rawSpeaker != nil {
		text, ok := rawSpeaker.(string)
		if !ok {
			return nil, fmt.Errorf("%s.speaker 必须是字符串或 null", where)
		}
		// 空串 / 纯空白归一为 null，交由 kind⇄speaker 约束裁决。
		if strings.TrimSpace(text) != "" {
			speaker = text
		}
	}
	text, err := step1RequireString(item, "text", where)
	if err != nil {
		return nil, err
	}
	if kind == "dialogue" {
		if speaker == nil {
			return nil, fmt.Errorf("%s: dialogue utterance 必须带非空 speaker", where)
		}
	} else if speaker != nil {
		return nil, fmt.Errorf("%s: voiceover utterance 不得带 speaker", where)
	}
	return map[string]any{"kind": kind, "speaker": speaker, "text": text}, nil
}

// --- narration：NarrationStep1Draft（segments[NarrationStep1Segment]） ---

// narrationSegmentAllowed 是 NarrationStep1Segment 声明过的键（extra=forbid）。
var narrationSegmentAllowed = []string{
	"segment_id", "novel_text", "duration_seconds", "segment_break",
	"characters_in_segment", "scenes", "props",
}

func validateNarrationStep1Content(content any) (map[string]any, error) {
	obj, ok := content.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("narration step1 内容必须是 JSON 对象")
	}
	rawSegments, err := step1RequireList(obj, "segments")
	if err != nil {
		return nil, err
	}
	segments := make([]any, 0, len(rawSegments))
	for index, raw := range rawSegments {
		segment, err := validateNarrationStep1Segment(raw, fmt.Sprintf("segments.%d", index))
		if err != nil {
			return nil, err
		}
		segments = append(segments, segment)
	}
	// 顶层 extra=ignore：episode 头等附带字段不进落盘内容。
	return map[string]any{"segments": segments}, nil
}

func validateNarrationStep1Segment(raw any, where string) (map[string]any, error) {
	segment, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s 必须是对象", where)
	}
	if err := service.RejectUnknownKeys(segment, narrationSegmentAllowed, where); err != nil {
		return nil, err
	}
	segmentID, err := step1RequireNonEmptyString(segment, "segment_id", where)
	if err != nil {
		return nil, err
	}
	novelText, err := step1RequireNonEmptyString(segment, "novel_text", where)
	if err != nil {
		return nil, err
	}
	duration, err := step1RequireIntRange(segment, "duration_seconds", 1, 60, where)
	if err != nil {
		return nil, err
	}
	segmentBreak, err := step1BoolWithDefault(segment, "segment_break", false, where)
	if err != nil {
		return nil, err
	}
	characters, err := step1RequireStringList(segment, "characters_in_segment", where)
	if err != nil {
		return nil, err
	}
	scenes, err := step1RequireStringList(segment, "scenes", where)
	if err != nil {
		return nil, err
	}
	props, err := step1RequireStringList(segment, "props", where)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"segment_id":            segmentID,
		"novel_text":            novelText,
		"duration_seconds":      duration,
		"segment_break":         segmentBreak,
		"characters_in_segment": characters,
		"scenes":                scenes,
		"props":                 props,
	}, nil
}

// --- reference_video：ReferenceStep1Draft（units[ReferenceStep1Unit]） ---

// referenceStep1UnitAllowed 是 ReferenceStep1Unit 声明过的键（extra=forbid）。
var referenceStep1UnitAllowed = []string{
	"unit_id", "shots", "duration_seconds", "references", "source_text",
}

// referenceShotAllowed / referenceResourceAllowed 是 Shot / ReferenceResource 声明过的键。
var (
	referenceShotAllowed     = []string{"text"}
	referenceResourceAllowed = []string{"type", "name", "variant_id", "variant_slug"}
)

func validateReferenceStep1Content(content any) (map[string]any, error) {
	obj, ok := content.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("reference_video step1 内容必须是 JSON 对象")
	}
	rawUnits, err := step1RequireList(obj, "units")
	if err != nil {
		return nil, err
	}
	units := make([]any, 0, len(rawUnits))
	for index, raw := range rawUnits {
		unit, err := validateReferenceStep1Unit(raw, fmt.Sprintf("units.%d", index))
		if err != nil {
			return nil, err
		}
		units = append(units, unit)
	}
	return map[string]any{"units": units}, nil
}

func validateReferenceStep1Unit(raw any, where string) (map[string]any, error) {
	unit, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s 必须是对象", where)
	}
	if err := service.RejectUnknownKeys(unit, referenceStep1UnitAllowed, where); err != nil {
		return nil, err
	}
	unitID, err := step1RequireNonEmptyString(unit, "unit_id", where)
	if err != nil {
		return nil, err
	}
	rawShots, err := step1RequireList(unit, "shots")
	if err != nil {
		return nil, err
	}
	if len(rawShots) < 1 || len(rawShots) > 4 {
		return nil, fmt.Errorf("%s.shots 必须是 1-4 个 shot", where)
	}
	shots := make([]any, 0, len(rawShots))
	for index, rawShot := range rawShots {
		shot, err := validateReferenceShot(rawShot, fmt.Sprintf("%s.shots.%d", where, index))
		if err != nil {
			return nil, err
		}
		shots = append(shots, shot)
	}
	low, high := domain.ReferenceUnitDurationRange()
	duration, err := step1RequireIntRange(unit, "duration_seconds", low, high, where)
	if err != nil {
		return nil, err
	}
	rawReferences, err := step1ListWithDefault(unit, "references", where)
	if err != nil {
		return nil, err
	}
	references := make([]any, 0, len(rawReferences))
	for index, rawReference := range rawReferences {
		reference, err := validateReferenceResource(rawReference, fmt.Sprintf("%s.references.%d", where, index))
		if err != nil {
			return nil, err
		}
		references = append(references, reference)
	}
	sourceText, err := step1StringWithDefault(unit, "source_text", "", where)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"unit_id":          unitID,
		"shots":            shots,
		"duration_seconds": duration,
		"references":       references,
		"source_text":      sourceText,
	}, nil
}

func validateReferenceShot(raw any, where string) (map[string]any, error) {
	shot, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s 必须是对象", where)
	}
	if err := service.RejectUnknownKeys(shot, referenceShotAllowed, where); err != nil {
		return nil, err
	}
	text, err := step1RequireString(shot, "text", where)
	if err != nil {
		return nil, err
	}
	return map[string]any{"text": text}, nil
}

func validateReferenceResource(raw any, where string) (map[string]any, error) {
	resource, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s 必须是对象", where)
	}
	if err := service.RejectUnknownKeys(resource, referenceResourceAllowed, where); err != nil {
		return nil, err
	}
	refType, err := step1RequireString(resource, "type", where)
	if err != nil {
		return nil, err
	}
	if refType != "character" && refType != "scene" && refType != "prop" {
		return nil, fmt.Errorf("%s.type 必须是 character / scene / prop，收到 %q", where, refType)
	}
	name, err := step1RequireString(resource, "name", where)
	if err != nil {
		return nil, err
	}
	variantID, err := step1NullableStringWithDefault(resource, "variant_id", where)
	if err != nil {
		return nil, err
	}
	variantSlug, err := step1NullableStringWithDefault(resource, "variant_slug", where)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"type": refType, "name": name, "variant_id": variantID, "variant_slug": variantSlug,
	}, nil
}

// --- references 机械重派生（对应 lib/reference_video/shot_parser.py::rederive_unit_references） ---

// rederiveUnitReferences 就地按各 unit 的 shot 文本 @[名称] 引用机械重派生 references
// （并集、首现顺序，顺序即参考图编号）。
//
// web 审阅编辑 shot 文本后回写时调用：references 是从正文机械派生的字段，若不随编辑重派生，
// 正文改了引用而 references 停留旧值，step2 会以陈旧的参考图映射生成。只做机械派生，
// 不校验能力上限 / 引用完整性（未登记的名称静默落入 missing、不进 references）——与 web
// 审阅对 drama / narration 只做结构校验同口径。
func rederiveUnitReferences(units any, project *domain.ProjectFile) {
	list, ok := units.([]any)
	if !ok {
		return
	}
	for _, raw := range list {
		unit, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		shots, _ := unit["shots"].([]any)
		parts := make([]string, 0, len(shots))
		for _, rawShot := range shots {
			if shot, ok := rawShot.(map[string]any); ok {
				text, _ := shot["text"].(string)
				parts = append(parts, text)
			}
		}
		refs, _ := reference.DeriveReferencesFromText(strings.Join(parts, "\n"), project)
		unit["references"] = reference.RenderUnitReferences(refs)
	}
}

// --- Pydantic 语义的取值辅助（lax 强制转换口径，仅覆盖 JSON 里实际会出现的类型） ---

func step1RequireString(obj map[string]any, key, where string) (string, error) {
	value, present := obj[key]
	if !present {
		return "", fmt.Errorf("%s%s 字段必填", step1WherePrefix(where), key)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s%s 必须是字符串", step1WherePrefix(where), key)
	}
	return text, nil
}

func step1RequireNonEmptyString(obj map[string]any, key, where string) (string, error) {
	text, err := step1RequireString(obj, key, where)
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", fmt.Errorf("%s%s 不得为空", step1WherePrefix(where), key)
	}
	return text, nil
}

func step1StringWithDefault(obj map[string]any, key, fallback, where string) (string, error) {
	if value, present := obj[key]; present {
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("%s%s 必须是字符串", step1WherePrefix(where), key)
		}
		return text, nil
	}
	return fallback, nil
}

// step1NullableStringWithDefault 取 str | null 字段（缺省 / null 均返回 nil）。
func step1NullableStringWithDefault(obj map[string]any, key, where string) (any, error) {
	value, present := obj[key]
	if !present || value == nil {
		return nil, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("%s%s 必须是字符串或 null", step1WherePrefix(where), key)
	}
	return text, nil
}

func step1RequireList(obj map[string]any, key string) ([]any, error) {
	value, present := obj[key]
	if !present {
		return nil, fmt.Errorf("%s 字段必填", key)
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s 必须是数组", key)
	}
	return list, nil
}

func step1ListWithDefault(obj map[string]any, key, where string) ([]any, error) {
	if value, present := obj[key]; present {
		list, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("%s%s 必须是数组", step1WherePrefix(where), key)
		}
		return list, nil
	}
	return []any{}, nil
}

// step1IntValue 把 JSON 数字 / 整数字符串归一为 int（Pydantic lax：float 须为整数值；
// bool 不接受，字符串按 int() 语义允许首尾空白）。
func step1IntValue(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if v == float64(int(v)) {
			return int(v), true
		}
	case string:
		parsed, err := parseIntPy(v)
		if err == nil {
			return parsed, true
		}
	}
	return 0, false
}

// parseIntPy 按 Python int() 语义解析整数字符串（允许首尾空白）。
func parseIntPy(text string) (int, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, fmt.Errorf("空字符串不是整数")
	}
	negative := false
	switch trimmed[0] {
	case '-':
		negative = true
		trimmed = trimmed[1:]
	case '+':
		trimmed = trimmed[1:]
	}
	if trimmed == "" {
		return 0, fmt.Errorf("无数字部分")
	}
	value := 0
	for i := 0; i < len(trimmed); i++ {
		digit := trimmed[i]
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("非法整数: %s", text)
		}
		value = value*10 + int(digit-'0')
	}
	if negative {
		return -value, nil
	}
	return value, nil
}

func step1RequireIntRange(obj map[string]any, key string, low, high int, where string) (int, error) {
	value, present := obj[key]
	if !present {
		return 0, fmt.Errorf("%s%s 字段必填", step1WherePrefix(where), key)
	}
	number, ok := step1IntValue(value)
	if !ok {
		return 0, fmt.Errorf("%s%s 必须是整数", step1WherePrefix(where), key)
	}
	if number < low || number > high {
		return 0, fmt.Errorf("%s%s 必须在 %d-%d 之间，收到 %d", step1WherePrefix(where), key, low, high, number)
	}
	return number, nil
}

func step1IntWithDefault(obj map[string]any, key string, fallback, low, high int, where string) (int, error) {
	if value, present := obj[key]; present {
		number, ok := step1IntValue(value)
		if !ok {
			return 0, fmt.Errorf("%s%s 必须是整数", step1WherePrefix(where), key)
		}
		if number < low || number > high {
			return 0, fmt.Errorf("%s%s 必须在 %d-%d 之间，收到 %d", step1WherePrefix(where), key, low, high, number)
		}
		return number, nil
	}
	return fallback, nil
}

func step1BoolWithDefault(obj map[string]any, key string, fallback bool, where string) (bool, error) {
	if value, present := obj[key]; present {
		boolean, ok := value.(bool)
		if !ok {
			return false, fmt.Errorf("%s%s 必须是布尔值", step1WherePrefix(where), key)
		}
		return boolean, nil
	}
	return fallback, nil
}

func step1RequireStringList(obj map[string]any, key, where string) ([]any, error) {
	list, err := step1RequireList(obj, key)
	if err != nil {
		return nil, fmt.Errorf("%s%s", step1WherePrefix(where), err.Error())
	}
	for _, item := range list {
		if _, ok := item.(string); !ok {
			return nil, fmt.Errorf("%s%s 的元素必须是字符串", step1WherePrefix(where), key)
		}
	}
	return list, nil
}

func step1StringListWithDefault(obj map[string]any, key, where string) ([]any, error) {
	if _, present := obj[key]; !present {
		return []any{}, nil
	}
	return step1RequireStringList(obj, key, where)
}

// step1WherePrefix 把字段路径前缀拼进错误消息（顶层字段无前缀）。
func step1WherePrefix(where string) string {
	if where == "" {
		return ""
	}
	return where + "."
}
