// 剧集（drama）step1 规范化剧本的 schema / 解析 / 校验（对应 lib/script_models.py 的
// DramaNormalizedScript / DramaSceneContent / Utterance 与 build_drama_normalized_script_model）。
//
// step1 产物落 drafts/episode_N/step1_normalized_script.json，供 generate_episode_script
// （drama）透传消费：场景边界、出场资产、逐字口播 utterances、原文锚 source_text、视觉改编
// 描述 scene_description 一次定稿；step2 只补视觉层。
package script

import (
	"fmt"

	"github.com/shotwise/shotwise/internal/service"
	"strings"
)

// DramaNormalizedScriptSchema 构造 duration_seconds 被 supported_durations 枚举硬约束的
// step1 规范化剧本 JSON Schema（单值渲染为 const、多值渲染为 enum）。
func DramaNormalizedScriptSchema(supportedDurations []int) (map[string]any, error) {
	durations := service.NormalizeDurations(supportedDurations)
	if len(durations) == 0 {
		return nil, fmt.Errorf("supported_durations 为空，无法构造 duration 枚举约束")
	}
	constraint := service.DurationConstraint(durations)

	utterance := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind":    map[string]any{"type": "string", "enum": []any{"dialogue", "voiceover"}},
			"speaker": map[string]any{"type": []any{"string", "null"}},
			"text":    map[string]any{"type": "string"},
		},
		"required":             []any{"kind", "text"},
		"additionalProperties": false,
	}
	scene := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"scene_id":            map[string]any{"type": "string", "minLength": 1},
			"duration_seconds":    constraint,
			"segment_break":       map[string]any{"type": "boolean"},
			"characters_in_scene": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"scenes":              map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"props":               map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"scene_description":   map[string]any{"type": "string"},
			"utterances":          map[string]any{"type": "array", "items": utterance},
			"source_text":         map[string]any{"type": "string"},
		},
		// duration_seconds 在动态模型里被收紧为无默认字段 → 必填。
		"required":             []any{"scene_id", "duration_seconds", "characters_in_scene", "scene_description"},
		"additionalProperties": false,
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":  map[string]any{"type": "string"},
			"scenes": map[string]any{"type": "array", "items": scene},
		},
		"required": []any{"title", "scenes"},
	}, nil
}


// ParseDramaNormalizedScript 解析并校验 step1 规范化响应，返回归一化后的 {title, scenes}。
//
// 校验与 response_schema 同口径（extra=forbid、字段非空、时长硬约束、utterances 的 kind ⇄
// speaker 联合约束）：即使 backend 未严格执行 schema，越界内容也在此被拦截，不写盘。
func ParseDramaNormalizedScript(responseText string, supportedDurations []int) (map[string]any, error) {
	root, err := service.ParseJSONObject(responseText)
	if err != nil {
		return nil, fmt.Errorf("step1 规范化内容%s", strings.TrimPrefix(err.Error(), "JSON 解析失败: "))
	}
	if err := service.RejectUnknownKeys(root, []string{"title", "scenes"}, "根对象"); err != nil {
		return nil, fmt.Errorf("step1 结构校验失败：%w", err)
	}
	title, ok := root["title"].(string)
	if !ok {
		return nil, fmt.Errorf("step1 规范化内容结构异常：title 必须为字符串")
	}
	rawScenes, ok := root["scenes"].([]any)
	if !ok || len(rawScenes) == 0 {
		return nil, fmt.Errorf("step1 规范化内容结构异常：scenes 必须是非空的场景对象数组")
	}
	allowedDurations := map[int]bool{}
	for _, d := range service.NormalizeDurations(supportedDurations) {
		allowedDurations[d] = true
	}
	scenes := make([]any, 0, len(rawScenes))
	for index, item := range rawScenes {
		scene, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("scenes.%d: 必须是对象", index)
		}
		normalized, err := normalizeDramaSceneContent(scene, index, allowedDurations)
		if err != nil {
			return nil, err
		}
		scenes = append(scenes, normalized)
	}
	return map[string]any{"title": title, "scenes": scenes}, nil
}

// dramaSceneContentAllowed 是 DramaSceneContent 的字段白名单。
var dramaSceneContentAllowed = []string{
	"scene_id", "duration_seconds", "segment_break", "characters_in_scene",
	"scenes", "props", "scene_description", "utterances", "source_text",
}

// normalizeDramaSceneContent 校验并归一单个场景内容对象。
func normalizeDramaSceneContent(scene map[string]any, index int, allowedDurations map[int]bool) (map[string]any, error) {
	loc := fmt.Sprintf("scenes.%d", index)
	if err := service.RejectUnknownKeys(scene, dramaSceneContentAllowed, loc); err != nil {
		return nil, err
	}
	sceneID, ok := scene["scene_id"].(string)
	if !ok || sceneID == "" {
		return nil, fmt.Errorf("%s.scene_id: 必填且非空", loc)
	}
	duration := service.CoerceDigitInt(scene["duration_seconds"])
	if duration == 0 || !allowedDurations[duration] {
		return nil, fmt.Errorf("%s.duration_seconds: 必须取 supported_durations 中的值", loc)
	}
	if _, ok := scene["characters_in_scene"].([]any); !ok {
		return nil, fmt.Errorf("%s.characters_in_scene: 必填且为数组", loc)
	}
	sceneDesc, ok := scene["scene_description"].(string)
	if !ok {
		return nil, fmt.Errorf("%s.scene_description: 必填且为字符串", loc)
	}
	utterances, err := normalizeUtterances(scene["utterances"], loc)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"scene_id":            sceneID,
		"duration_seconds":    duration,
		"segment_break":       false,
		"characters_in_scene": listStrings(scene["characters_in_scene"]),
		"scenes":              listStrings(scene["scenes"]),
		"props":               listStrings(scene["props"]),
		"scene_description":   sceneDesc,
		"utterances":          utterances,
		"source_text":         service.StringValue(scene["source_text"], ""),
	}
	if b, ok := scene["segment_break"].(bool); ok {
		out["segment_break"] = b
	}
	return out, nil
}

// utteranceAllowed 是 Utterance 的字段白名单。
var utteranceAllowed = []string{"kind", "speaker", "text"}

// normalizeUtterances 校验并归一 utterances 数组，含 kind ⇄ speaker 联合约束。
func normalizeUtterances(raw any, loc string) ([]any, error) {
	if raw == nil {
		return []any{}, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s.utterances: 必须为数组", loc)
	}
	out := make([]any, 0, len(items))
	for i, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s.utterances.%d: 必须是对象", loc, i)
		}
		uloc := fmt.Sprintf("%s.utterances.%d", loc, i)
		if err := service.RejectUnknownKeys(entry, utteranceAllowed, uloc); err != nil {
			return nil, err
		}
		kind, _ := entry["kind"].(string)
		if kind != "dialogue" && kind != "voiceover" {
			return nil, fmt.Errorf("%s.kind: 必须是 dialogue 或 voiceover", uloc)
		}
		text, ok := entry["text"].(string)
		if !ok {
			return nil, fmt.Errorf("%s.text: 必填且为字符串", uloc)
		}
		speaker := ""
		if s, ok := entry["speaker"].(string); ok {
			speaker = strings.TrimSpace(s)
		}
		if kind == "dialogue" && speaker == "" {
			return nil, fmt.Errorf("%s.speaker: dialogue utterance 必须带非空 speaker", uloc)
		}
		if kind == "voiceover" && speaker != "" {
			return nil, fmt.Errorf("%s.speaker: voiceover utterance 不得带 speaker", uloc)
		}
		normalized := map[string]any{"kind": kind, "text": text, "speaker": nil}
		if kind == "dialogue" {
			normalized["speaker"] = speaker
		}
		out = append(out, normalized)
	}
	return out, nil
}


