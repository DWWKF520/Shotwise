// 剧集剧本结构校验（纯逻辑）。对应 lib/script_structure_validator.py 与其依据的 lib/script_models.py。
//
// 把「一份剧本 dict 是否结构良构」收敛到唯一一处：喂入 dict、返回错误消息列表，不读磁盘、
// 不依赖项目状态。剧本写盘统一入口、测试与后续其它写入面都复用它。
//
// 校验对象是结构层模型（Python 侧为 lib/script_models.py 的 pydantic 模型），而非 FS 感知的
// 归档层校验器——后者会读磁盘并拒绝合法的半成品草稿（分镜图尚未生成）。
//
// 与 Python 的已知差异（都有意为之，逐项说明）：
//   - 不做读时迁移：Python 的模型在校验前会剥离遗留字段（clues_in_segment / scene_type 等）
//     并把旧口播双字段合成 utterances。Go 侧对遗留字段直接放行（视为可忽略），
//     但「有 utterances 时 video_prompt 不得带 dialogue」这条仍按迁移后的形态校验。
//   - shot_type / camera_motion 的枚举漂移在 Python 里被归一为默认值而不报错，故这里不报错。
//   - JSON 解出的数字统一是 float64，无法区分 4 与 4.0；整数值一律按整数处理。
package domain

import (
	"fmt"
	"strings"
)

// --- 枚举与取值表（与 lib/script_models.py 同源）---

var scriptShotTypes = []string{
	"Extreme Close-up", "Close-up", "Medium Close-up", "Medium Shot", "Medium Long Shot",
	"Long Shot", "Extreme Long Shot", "Over-the-shoulder", "Point-of-view",
}

var scriptCameraMotions = []string{
	"Static", "Pan Left", "Pan Right", "Tilt Up", "Tilt Down", "Zoom In", "Zoom Out",
	"Push In", "Pull Out", "Truck Left", "Truck Right", "Pedestal Up", "Pedestal Down",
	"Orbit", "Tracking Shot", "Shake",
}

var scriptTransitionTypes = []string{"cut", "fade", "dissolve"}

// ShotTypes 暴露分镜景别枚举（供上层构造 response_schema）。
func ShotTypes() []string { return append([]string(nil), scriptShotTypes...) }

// CameraMotions 暴露运镜枚举（供上层构造 response_schema）。
func CameraMotions() []string { return append([]string(nil), scriptCameraMotions...) }

// TransitionTypes 暴露转场枚举。
func TransitionTypes() []string { return append([]string(nil), scriptTransitionTypes...) }

var scriptGeneratedStatuses = []string{"pending", "storyboard_ready", "completed"}

// scriptLegacyDroppedFields 是存量 JSON 可能残留、Python 在校验前剥离的字段。
var scriptLegacyDroppedFields = map[string]bool{
	"clues_in_segment": true, "scene_type": true, "clues_in_scene": true,
}

// scriptItemFallbackDurations 是缺 duration_seconds 时按骨架种类取的兜底时长（秒）。
var scriptItemFallbackDurations = map[string]int{"segments": 4, "scenes": 8, "shots": 0, "video_units": 0}

// referenceUnitDurationRange 是参考生视频 unit 时长的结构合理性区间（秒）。
var referenceUnitDurationRange = [2]int{1, 300}

// ValidateScriptStructure 校验剧本 dict 的结构是否良构，返回错误消息列表（空列表 = 合法）。
//
// 错误消息格式与 Python 侧对齐：字段路径用 "." 连接（含数组下标），形如
// "segments.4.video_prompt.action: 字段必填"。
func ValidateScriptStructure(script map[string]any) []string {
	v := &scriptValidator{}
	switch ResolveScriptKind(script) {
	case "video_units":
		v.validateReferenceVideoScript("", script)
	case "scenes":
		v.validateDramaEpisodeScript("", script)
	case "shots":
		v.validateAdEpisodeScript("", script)
	default:
		v.validateNarrationEpisodeScript("", script)
	}
	return v.errors
}

// ScriptDurationTotal 按骨架种类求剧本条目总时长（秒）——脏数据稳健、不抛。
//
// items 非数组（含 null 这类降级保存的脏值）按空处理返回 0。只认真正的正整数：
// 非对象条目按 0 计，缺失或脏值（null / 布尔 / 非正整数 / 字符串）回退按骨架种类取兜底时长。
func ScriptDurationTotal(kind string, items []any) int {
	total := 0
	for _, item := range items {
		total += scriptItemDuration(kind, item)
	}
	return total
}

func scriptItemDuration(kind string, item any) int {
	m, ok := item.(map[string]any)
	if !ok {
		return 0
	}
	if value, ok := scriptPositiveInt(m["duration_seconds"]); ok {
		return value
	}
	return scriptItemFallbackDurations[kind]
}

// scriptPositiveInt 取真正的正整数（布尔与非整数值不算）。
func scriptPositiveInt(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		if v != float64(int(v)) || v <= 0 {
			return 0, false
		}
		return int(v), true
	case int:
		if v <= 0 {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// --- 校验器 ---

type scriptValidator struct {
	errors []string
}

func (v *scriptValidator) add(loc, message string) {
	v.errors = append(v.errors, fmt.Sprintf("%s: %s", loc, message))
}

func joinScriptLoc(base, field string) string {
	if base == "" {
		return field
	}
	return base + "." + field
}

func joinScriptIndex(base string, index int) string {
	return fmt.Sprintf("%s.%d", base, index)
}

// asMap 断言对象；失败时追加一条错误并返回 ok=false。
func (v *scriptValidator) asMap(loc string, value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		v.add(loc, "Input should be a valid dictionary")
		return nil, false
	}
	return m, true
}

// asList 断言数组；失败时追加一条错误并返回 ok=false。
func (v *scriptValidator) asList(loc string, value any) ([]any, bool) {
	list, ok := value.([]any)
	if !ok {
		v.add(loc, "Input should be a valid list")
		return nil, false
	}
	return list, true
}

// forbid 拒绝白名单外的键（对应 pydantic extra="forbid"）。
func (v *scriptValidator) forbid(loc string, m map[string]any, allowed ...string) {
	for _, key := range sortedKeys(m) {
		if containsString(allowed, key) {
			continue
		}
		v.add(joinScriptLoc(loc, key), "Extra inputs are not permitted")
	}
}

// requiredString 取必填字符串；缺失或类型不符时报错并返回空串。
func (v *scriptValidator) requiredString(loc string, m map[string]any, key string) string {
	value, present := m[key]
	if !present {
		v.add(joinScriptLoc(loc, key), "Field required")
		return ""
	}
	s, ok := value.(string)
	if !ok {
		v.add(joinScriptLoc(loc, key), "Input should be a valid string")
		return ""
	}
	return s
}

// optionalString 取可选字符串（缺失 / null 为合法）。
func (v *scriptValidator) optionalString(loc string, m map[string]any, key string) {
	value, present := m[key]
	if !present || value == nil {
		return
	}
	if _, ok := value.(string); !ok {
		v.add(joinScriptLoc(loc, key), "Input should be a valid string")
	}
}

// optionalBool 取可选布尔。
func (v *scriptValidator) optionalBool(loc string, m map[string]any, key string) {
	value, present := m[key]
	if !present || value == nil {
		return
	}
	if _, ok := value.(bool); !ok {
		v.add(joinScriptLoc(loc, key), "Input should be a valid boolean")
	}
}

// optionalInt 取可选整数（缺失 / null 为合法）。
func (v *scriptValidator) optionalInt(loc string, m map[string]any, key string) {
	value, present := m[key]
	if !present || value == nil {
		return
	}
	if _, ok := scriptIntValue(value); !ok {
		v.add(joinScriptLoc(loc, key), "Input should be a valid integer")
	}
}

// intInRange 校验可选的区间整数；缺失 / null 时按 default 视为合法。
func (v *scriptValidator) intInRange(loc string, m map[string]any, key string, low, high int, hasDefault bool) {
	value, present := m[key]
	if !present || value == nil {
		if !hasDefault {
			v.add(joinScriptLoc(loc, key), "Field required")
		}
		return
	}
	n, ok := scriptIntValue(value)
	if !ok {
		v.add(joinScriptLoc(loc, key), "Input should be a valid integer")
		return
	}
	if n < low {
		v.add(joinScriptLoc(loc, key), fmt.Sprintf("Input should be greater than or equal to %d", low))
		return
	}
	if n > high {
		v.add(joinScriptLoc(loc, key), fmt.Sprintf("Input should be less than or equal to %d", high))
	}
}

// stringList 取字符串数组；required 为 true 时缺失即报错。
func (v *scriptValidator) stringList(loc string, m map[string]any, key string, required bool) {
	value, present := m[key]
	if !present {
		if required {
			v.add(joinScriptLoc(loc, key), "Field required")
		}
		return
	}
	if value == nil {
		if required {
			v.add(joinScriptLoc(loc, key), "Input should be a valid list")
		}
		return
	}
	list, ok := value.([]any)
	if !ok {
		v.add(joinScriptLoc(loc, key), "Input should be a valid list")
		return
	}
	for i, item := range list {
		if _, ok := item.(string); !ok {
			v.add(joinScriptIndex(joinScriptLoc(loc, key), i), "Input should be a valid string")
		}
	}
}

// enum 校验可选的枚举字符串。
func (v *scriptValidator) enum(loc string, m map[string]any, key string, allowed []string) {
	value, present := m[key]
	if !present || value == nil {
		return
	}
	s, ok := value.(string)
	if !ok {
		v.add(joinScriptLoc(loc, key), "Input should be a valid string")
		return
	}
	if containsString(allowed, s) {
		return
	}
	v.add(joinScriptLoc(loc, key), "Input should be "+scriptEnumPhrase(allowed))
}

// listItems 逐项校验一个数组字段。
func (v *scriptValidator) listItems(loc string, m map[string]any, key string, each func(loc string, item any)) {
	value, present := m[key]
	if !present {
		v.add(joinScriptLoc(loc, key), "Field required")
		return
	}
	list, ok := v.asList(joinScriptLoc(loc, key), value)
	if !ok {
		return
	}
	if each == nil {
		return
	}
	for i, item := range list {
		each(joinScriptIndex(joinScriptLoc(loc, key), i), item)
	}
}

func scriptEnumPhrase(allowed []string) string {
	quoted := make([]string, 0, len(allowed))
	for _, item := range allowed {
		quoted = append(quoted, "'"+item+"'")
	}
	switch len(quoted) {
	case 1:
		return quoted[0]
	case 2:
		return quoted[0] + " or " + quoted[1]
	default:
		return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
	}
}

// scriptIntValue 取整数语义的值（JSON 数字的整数值算整数；布尔不算）。
func scriptIntValue(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, false
		}
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

// --- 子模型 ---

// validateGeneratedAssets 校验 generated_assets 容器（extra="forbid"，status 为枚举）。
func (v *scriptValidator) validateGeneratedAssets(loc string, value any) {
	if value == nil {
		return
	}
	m, ok := v.asMap(loc, value)
	if !ok {
		return
	}
	v.forbid(loc, m, "storyboard_image", "storyboard_last_image", "grid_id", "grid_cell_index",
		"video_clip", "video_thumbnail", "video_uri", "narration_audio", "status",
		"video_generated_at", "source_signature")
	for _, field := range []string{
		"storyboard_image", "storyboard_last_image", "grid_id", "video_clip",
		"video_thumbnail", "video_uri", "narration_audio", "video_generated_at", "source_signature",
	} {
		v.optionalString(loc, m, field)
	}
	v.optionalInt(loc, m, "grid_cell_index")
	v.enum(loc, m, "status", scriptGeneratedStatuses)
}

// validateComposition 校构图信息（extra="forbid"）。
func (v *scriptValidator) validateComposition(loc string, value any) {
	m, ok := v.asMap(loc, value)
	if !ok {
		return
	}
	v.forbid(loc, m, "shot_type", "lighting", "ambiance")
	// shot_type 在 Python 侧经 BeforeValidator 归一：漂移值被改写为默认值而不报错，故不校验枚举成员。
	v.requiredString(loc, m, "shot_type")
	v.requiredString(loc, m, "lighting")
	v.requiredString(loc, m, "ambiance")
}

// validateImagePrompt 校验分镜图提示词（extra="forbid"）。
func (v *scriptValidator) validateImagePrompt(loc string, value any) {
	m, ok := v.asMap(loc, value)
	if !ok {
		return
	}
	v.forbid(loc, m, "scene", "composition")
	v.requiredString(loc, m, "scene")
	composition, present := m["composition"]
	if !present {
		v.add(joinScriptLoc(loc, "composition"), "Field required")
		return
	}
	v.validateComposition(joinScriptLoc(loc, "composition"), composition)
}

// validateVideoPromptCore 校验 video_prompt 的画面层公共字段。
func (v *scriptValidator) validateVideoPromptCore(loc string, m map[string]any) {
	v.requiredString(loc, m, "action")
	// camera_motion 同 shot_type：漂移值被归一为默认值，不校验枚举成员。
	v.requiredString(loc, m, "camera_motion")
	v.requiredString(loc, m, "ambiance_audio")
}

// validateVideoPrompt 校验带 dialogue 的 video_prompt（narration / ad）。
func (v *scriptValidator) validateVideoPrompt(loc string, value any) {
	m, ok := v.asMap(loc, value)
	if !ok {
		return
	}
	v.forbid(loc, m, "action", "camera_motion", "ambiance_audio", "dialogue")
	v.validateVideoPromptCore(loc, m)
	// dialogue 的 null 被 Python 的 BeforeValidator 归一为 []，故 null 合法。
	if dialogue, present := m["dialogue"]; present && dialogue != nil {
		list, ok := v.asList(joinScriptLoc(loc, "dialogue"), dialogue)
		if !ok {
			return
		}
		for i, item := range list {
			v.validateDialogue(joinScriptIndex(joinScriptLoc(loc, "dialogue"), i), item)
		}
	}
}

// validateDramaVideoPrompt 校验无 dialogue 的 video_prompt（drama）。
//
// 存量数据可能残留 video_prompt.dialogue，Python 在校验前把它迁移进 utterances 并剥离；
// 仅当 utterances 已在场（迁移不触发）时才按 forbid 拒绝 dialogue。
func (v *scriptValidator) validateDramaVideoPrompt(loc string, value any, utterancesPresent bool) {
	m, ok := v.asMap(loc, value)
	if !ok {
		return
	}
	allowed := []string{"action", "camera_motion", "ambiance_audio"}
	if !utterancesPresent {
		v.forbid(loc, m, append(allowed, "dialogue")...)
	} else {
		v.forbid(loc, m, allowed...)
	}
	v.validateVideoPromptCore(loc, m)
}

func (v *scriptValidator) validateDialogue(loc string, value any) {
	m, ok := v.asMap(loc, value)
	if !ok {
		return
	}
	v.forbid(loc, m, "speaker", "line")
	v.requiredString(loc, m, "speaker")
	v.requiredString(loc, m, "line")
}

// validateUtterance 校验场景级发声条目，含 kind ⇄ speaker 的联合约束。
func (v *scriptValidator) validateUtterance(loc string, value any) {
	m, ok := v.asMap(loc, value)
	if !ok {
		return
	}
	v.forbid(loc, m, "kind", "speaker", "text")
	kind, kindPresent := m["kind"]
	if !kindPresent {
		v.add(joinScriptLoc(loc, "kind"), "Field required")
	}
	v.requiredString(loc, m, "text")
	speaker, _ := m["speaker"].(string)
	if kindPresent {
		kindString, ok := kind.(string)
		if !ok {
			v.add(joinScriptLoc(loc, "kind"), "Input should be a valid string")
			return
		}
		if !containsString([]string{"dialogue", "voiceover"}, kindString) {
			v.add(joinScriptLoc(loc, "kind"), "Input should be 'dialogue' or 'voiceover'")
			return
		}
		// 空串 / 纯空白 speaker 归一为「无说话人」后裁决。
		hasSpeaker := strings.TrimSpace(speaker) != ""
		if kindString == "dialogue" && !hasSpeaker {
			v.add(loc, "Value error, dialogue utterance 必须带非空 speaker")
		}
		if kindString == "voiceover" && m["speaker"] != nil && hasSpeaker {
			v.add(loc, "Value error, voiceover utterance 不得带 speaker")
		}
	}
}

// utteranceSection 校验 utterances 数组及其与 video_prompt.dialogue 的互斥。
func (v *scriptValidator) utterancesPresent(m map[string]any) bool {
	_, present := m["utterances"]
	return present
}

func (v *scriptValidator) validateUtterances(loc string, m map[string]any) {
	value, present := m["utterances"]
	if !present || value == nil {
		return
	}
	list, ok := v.asList(joinScriptLoc(loc, "utterances"), value)
	if !ok {
		return
	}
	for i, item := range list {
		v.validateUtterance(joinScriptIndex(joinScriptLoc(loc, "utterances"), i), item)
	}
}

// --- 四种剧集脚本 ---

// narrationSegmentAllowed / dramaSceneAllowed 等是各子模型的字段白名单。
var narrationSegmentAllowed = []string{
	"segment_id", "duration_seconds", "segment_break", "novel_text", "characters_in_segment",
	"scenes", "props", "image_prompt", "video_prompt", "transition_to_next", "note",
	"end_frame_image", "generated_assets",
}

// validateNarrationSegment 校验说书片段。
func (v *scriptValidator) validateNarrationSegment(loc string, value any) {
	m, ok := v.asMap(loc, value)
	if !ok {
		return
	}
	v.forbid(loc, m, append(append([]string{}, narrationSegmentAllowed...), "clues_in_segment")...)
	v.requiredString(loc, m, "segment_id")
	v.intInRange(loc, m, "duration_seconds", 1, 60, false)
	v.optionalBool(loc, m, "segment_break")
	v.requiredString(loc, m, "novel_text")
	v.stringList(loc, m, "characters_in_segment", true)
	v.stringList(loc, m, "scenes", false)
	v.stringList(loc, m, "props", false)
	v.requireNested(loc, m, "image_prompt", v.validateImagePrompt)
	v.requireNested(loc, m, "video_prompt", v.validateVideoPrompt)
	v.enum(loc, m, "transition_to_next", scriptTransitionTypes)
	v.optionalString(loc, m, "note")
	v.optionalString(loc, m, "end_frame_image")
	v.generatedAssets(loc, m)
}

// dramaSceneAllowed 是 drama 场景的字段白名单。
var dramaSceneAllowed = []string{
	"scene_id", "duration_seconds", "segment_break", "characters_in_scene", "scenes", "props",
	"image_prompt", "video_prompt", "utterances", "source_text", "voiceover", "transition_to_next",
	"note", "end_frame_image", "generated_assets", "scene_description",
}

// validateDramaScene 校验剧集动画场景。
func (v *scriptValidator) validateDramaScene(loc string, value any) {
	m, ok := v.asMap(loc, value)
	if !ok {
		return
	}
	allowed := append([]string{}, dramaSceneAllowed...)
	// 遗留字段在校验前被剥离；scene_description 属 step1 内容层，合并后不应残留。
	for key := range scriptLegacyDroppedFields {
		allowed = append(allowed, key)
	}
	v.forbid(loc, m, allowed...)
	v.requiredString(loc, m, "scene_id")
	v.intInRange(loc, m, "duration_seconds", 1, 60, true)
	v.optionalBool(loc, m, "segment_break")
	v.stringList(loc, m, "characters_in_scene", true)
	v.stringList(loc, m, "scenes", false)
	v.stringList(loc, m, "props", false)
	v.requireNested(loc, m, "image_prompt", v.validateImagePrompt)

	hasUtterances := v.utterancesPresent(m)
	if videoPrompt, present := m["video_prompt"]; present {
		v.validateDramaVideoPrompt(joinScriptLoc(loc, "video_prompt"), videoPrompt, hasUtterances)
	} else {
		v.add(joinScriptLoc(loc, "video_prompt"), "Field required")
	}
	v.validateUtterances(loc, m)
	v.optionalString(loc, m, "source_text")
	v.enum(loc, m, "transition_to_next", scriptTransitionTypes)
	v.optionalString(loc, m, "note")
	v.optionalString(loc, m, "end_frame_image")
	v.generatedAssets(loc, m)
}

// adShotAllowed 是 ad 镜头的字段白名单。
var adShotAllowed = []string{
	"shot_id", "section", "duration_seconds", "voiceover_text", "characters_in_shot", "scenes",
	"props", "products_in_shot", "image_prompt", "video_prompt", "transition_to_next", "note",
	"end_frame_image", "generated_assets",
}

// validateAdShot 校验广告镜头。
func (v *scriptValidator) validateAdShot(loc string, value any) {
	m, ok := v.asMap(loc, value)
	if !ok {
		return
	}
	v.forbid(loc, m, adShotAllowed...)
	v.requiredString(loc, m, "shot_id")
	v.requiredString(loc, m, "section")
	v.intInRange(loc, m, "duration_seconds", 1, 60, false)
	v.requiredString(loc, m, "voiceover_text")
	v.stringList(loc, m, "characters_in_shot", false)
	v.stringList(loc, m, "scenes", false)
	v.stringList(loc, m, "props", false)
	v.stringList(loc, m, "products_in_shot", false)
	v.requireNested(loc, m, "image_prompt", v.validateImagePrompt)
	v.requireNested(loc, m, "video_prompt", v.validateVideoPrompt)
	v.enum(loc, m, "transition_to_next", scriptTransitionTypes)
	v.optionalString(loc, m, "note")
	v.optionalString(loc, m, "end_frame_image")
	v.generatedAssets(loc, m)
}

// validateShot 校验参考单元内的镜头（只承载叙事文本）。
func (v *scriptValidator) validateShot(loc string, value any) {
	m, ok := v.asMap(loc, value)
	if !ok {
		return
	}
	v.forbid(loc, m, "text")
	v.requiredString(loc, m, "text")
}

// validateReferenceResource 校验参考图引用条目。
func (v *scriptValidator) validateReferenceResource(loc string, value any, kinds []string) {
	m, ok := v.asMap(loc, value)
	if !ok {
		return
	}
	allowed := []string{"type", "name"}
	if containsString(kinds, "character") {
		allowed = append(allowed, "variant_id", "variant_slug")
	}
	v.forbid(loc, m, allowed...)
	v.enum(loc, m, "type", kinds)
	v.requiredString(loc, m, "name")
	v.optionalString(loc, m, "variant_id")
	v.optionalString(loc, m, "variant_slug")
}

// validateReferenceVideoUnit 校验参考生视频单元。
func (v *scriptValidator) validateReferenceVideoUnit(loc string, value any) {
	m, ok := v.asMap(loc, value)
	if !ok {
		return
	}
	v.forbid(loc, m, "unit_id", "shots", "references", "duration_seconds",
		"transition_to_next", "note", "generated_assets")
	v.requiredString(loc, m, "unit_id")
	v.listItems(loc, m, "shots", v.validateShot)
	if shots, ok := m["shots"].([]any); ok {
		if len(shots) < 1 {
			v.add(joinScriptLoc(loc, "shots"),
				"List should have at least 1 item after validation, not 0")
		}
		if len(shots) > 4 {
			v.add(joinScriptLoc(loc, "shots"),
				fmt.Sprintf("List should have at most 4 items after validation, not %d", len(shots)))
		}
	}
	if references, present := m["references"]; present && references != nil {
		list, ok := v.asList(joinScriptLoc(loc, "references"), references)
		if ok {
			for i, item := range list {
				v.validateReferenceResource(joinScriptIndex(joinScriptLoc(loc, "references"), i), item,
					[]string{"character", "scene", "prop"})
			}
		}
	}
	v.intInRange(loc, m, "duration_seconds", referenceUnitDurationRange[0], referenceUnitDurationRange[1], false)
	v.enum(loc, m, "transition_to_next", scriptTransitionTypes)
	v.optionalString(loc, m, "note")
	v.generatedAssets(loc, m)
}

// validateAdReferenceUnit 校验 ad 派生分组索引条目。
func (v *scriptValidator) validateAdReferenceUnit(loc string, value any) {
	m, ok := v.asMap(loc, value)
	if !ok {
		return
	}
	v.forbid(loc, m, "unit_id", "shot_ids", "references", "generated_assets", "stale")
	v.requiredString(loc, m, "unit_id")
	v.listItems(loc, m, "shot_ids", func(itemLoc string, item any) {
		if _, ok := item.(string); !ok {
			v.add(itemLoc, "Input should be a valid string")
		}
	})
	if ids, ok := m["shot_ids"].([]any); ok {
		if len(ids) < 1 {
			v.add(joinScriptLoc(loc, "shot_ids"), "List should have at least 1 item after validation, not 0")
		}
		if len(ids) > 4 {
			v.add(joinScriptLoc(loc, "shot_ids"),
				fmt.Sprintf("List should have at most 4 items after validation, not %d", len(ids)))
		}
	}
	if references, present := m["references"]; present && references != nil {
		list, ok := v.asList(joinScriptLoc(loc, "references"), references)
		if ok {
			for i, item := range list {
				v.validateReferenceResource(joinScriptIndex(joinScriptLoc(loc, "references"), i), item,
					[]string{"product", "character", "scene", "prop"})
			}
		}
	}
	v.generatedAssets(loc, m)
	v.optionalBool(loc, m, "stale")
}

// requireNested 校验必填的嵌套对象。
func (v *scriptValidator) requireNested(loc string, m map[string]any, key string, each func(string, any)) {
	value, present := m[key]
	if !present {
		v.add(joinScriptLoc(loc, key), "Field required")
		return
	}
	each(joinScriptLoc(loc, key), value)
}

// generatedAssets 读可选的 generated_assets。
func (v *scriptValidator) generatedAssets(loc string, m map[string]any) {
	if value, present := m["generated_assets"]; present {
		v.validateGeneratedAssets(joinScriptLoc(loc, "generated_assets"), value)
	}
}

// validateNovel 校验 novel 子对象。
func (v *scriptValidator) validateNovel(loc string, m map[string]any) {
	value, present := m["novel"]
	if !present || value == nil {
		return
	}
	nested, ok := v.asMap(joinScriptLoc(loc, "novel"), value)
	if !ok {
		return
	}
	v.forbid(joinScriptLoc(loc, "novel"), nested, "title", "chapter")
	v.optionalString(joinScriptLoc(loc, "novel"), nested, "title")
	v.optionalString(joinScriptLoc(loc, "novel"), nested, "chapter")
}

// 顶层不走 extra="forbid"：episode / metadata 等字段由运行时注入，不是 schema 内字段。
// typo 防护靠子模型的 forbid 在嵌套路径上挡。

func (v *scriptValidator) commonEpisodeTop(loc string, m map[string]any, contentModes []string) {
	v.requiredString(loc, m, "title")
	v.optionalInt(loc, m, "duration_seconds")
	v.enum(loc, m, "content_mode", contentModes)
	v.validateNovel(loc, m)
	v.optionalString(loc, m, "hook")
	v.optionalString(loc, m, "next_episode_teaser")
}

func (v *scriptValidator) validateNarrationEpisodeScript(loc string, script map[string]any) {
	v.commonEpisodeTop(loc, script, []string{"narration"})
	v.listItems(loc, script, "segments", v.validateNarrationSegment)
}

func (v *scriptValidator) validateDramaEpisodeScript(loc string, script map[string]any) {
	v.commonEpisodeTop(loc, script, []string{"drama"})
	v.listItems(loc, script, "scenes", v.validateDramaScene)
}

func (v *scriptValidator) validateAdEpisodeScript(loc string, script map[string]any) {
	v.commonEpisodeTop(loc, script, []string{"ad"})
	v.listItems(loc, script, "shots", v.validateAdShot)
	if units, present := script["reference_units"]; present && units != nil {
		list, ok := v.asList(joinScriptLoc(loc, "reference_units"), units)
		if ok {
			for i, item := range list {
				v.validateAdReferenceUnit(joinScriptIndex(joinScriptLoc(loc, "reference_units"), i), item)
			}
		}
	}
}

func (v *scriptValidator) validateReferenceVideoScript(loc string, script map[string]any) {
	v.commonEpisodeTop(loc, script, []string{"narration", "drama"})
	v.listItems(loc, script, "video_units", v.validateReferenceVideoUnit)
}
