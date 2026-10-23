// 剧本 step2 视觉层的 schema / 解析 / 合并（对应 lib/script_models.py 的
// NarrationVisualEpisodeScript / DramaVisualScript / merge_drama_visual_into_scenes）。
//
// 两段式职责切分：step1 定内容层（novel_text / utterances / source_text 等），step2 只产视觉层
// （image_prompt / video_prompt），按 id 对齐合并回 step1——内容字段永不经 step2 的 LLM 重出。
package script

import (
	"fmt"

	"github.com/shotwise/shotwise/internal/service"
	"sort"

	"github.com/shotwise/shotwise/internal/domain/script"
)

// imagePromptSchema 构造 image_prompt 的 JSON Schema。
func imagePromptSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"scene": map[string]any{"type": "string"},
			"composition": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"shot_type": map[string]any{"type": "string", "enum": toAnyStrings(script.ShotTypes())},
					"lighting":  map[string]any{"type": "string"},
					"ambiance":  map[string]any{"type": "string"},
				},
				"required":             []any{"shot_type", "lighting", "ambiance"},
				"additionalProperties": false,
			},
		},
		"required":             []any{"scene", "composition"},
		"additionalProperties": false,
	}
}

// videoPromptSchema 构造 video_prompt 的 JSON Schema；withDialogue 为 true 时含 dialogue 字段。
func videoPromptSchema(withDialogue bool) map[string]any {
	properties := map[string]any{
		"action":         map[string]any{"type": "string"},
		"camera_motion":  map[string]any{"type": "string", "enum": toAnyStrings(script.CameraMotions())},
		"ambiance_audio": map[string]any{"type": "string"},
	}
	required := []any{"action", "camera_motion"}
	if withDialogue {
		properties["dialogue"] = map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"speaker": map[string]any{"type": "string"},
					"line":    map[string]any{"type": "string"},
				},
				"required":             []any{"speaker", "line"},
				"additionalProperties": false,
			},
		}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

// NarrationVisualEpisodeScriptSchema 构造说书 step2 视觉层的 response_schema。
func NarrationVisualEpisodeScriptSchema() map[string]any {
	segment := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"segment_id":   map[string]any{"type": "string", "minLength": 1},
			"image_prompt": imagePromptSchema(),
			"video_prompt": videoPromptSchema(true),
		},
		"required":             []any{"segment_id", "image_prompt", "video_prompt"},
		"additionalProperties": false,
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":    map[string]any{"type": "string"},
			"segments": map[string]any{"type": "array", "items": segment},
		},
		"required": []any{"title", "segments"},
	}
}

// DramaVisualScriptSchema 构造剧集动画 step2 视觉层的 response_schema（video_prompt 无 dialogue）。
func DramaVisualScriptSchema() map[string]any {
	scene := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"scene_id":     map[string]any{"type": "string", "minLength": 1},
			"image_prompt": imagePromptSchema(),
			"video_prompt": videoPromptSchema(false),
		},
		"required":             []any{"scene_id", "image_prompt", "video_prompt"},
		"additionalProperties": false,
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":  map[string]any{"type": "string"},
			"scenes": map[string]any{"type": "array", "items": scene},
		},
		"required": []any{"scenes"},
	}
}

// ParseNarrationVisual 解析 step2 视觉层响应（严格校验 + 归一化）。
func ParseNarrationVisual(responseText string) (map[string]any, error) {
	root, err := service.ParseJSONObject(responseText)
	if err != nil {
		return nil, fmt.Errorf("step2 视觉层 JSON 解析失败: %w", err)
	}
	rawSegments, ok := root["segments"].([]any)
	if !ok {
		return nil, fmt.Errorf("step2 视觉层结构校验失败：segments 必须为数组")
	}
	segments := make([]any, 0, len(rawSegments))
	for index, item := range rawSegments {
		segment, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("step2 视觉层结构校验失败：segments.%d 必须是对象", index)
		}
		sid, ok := segment["segment_id"].(string)
		if !ok || sid == "" {
			return nil, fmt.Errorf("step2 视觉层结构校验失败：segments.%d.segment_id 必填且非空", index)
		}
		if _, ok := segment["image_prompt"].(map[string]any); !ok {
			return nil, fmt.Errorf("step2 视觉层结构校验失败：segments.%d.image_prompt 必填", index)
		}
		if _, ok := segment["video_prompt"].(map[string]any); !ok {
			return nil, fmt.Errorf("step2 视觉层结构校验失败：segments.%d.video_prompt 必填", index)
		}
		segments = append(segments, map[string]any{
			"segment_id":   sid,
			"image_prompt": segment["image_prompt"],
			"video_prompt": segment["video_prompt"],
		})
	}
	title, _ := root["title"].(string)
	return map[string]any{"title": title, "segments": segments}, nil
}

// MergeNarrationVisual 把 step2 视觉层按 segment_id 合并回 step1 已确认的结构。
func MergeNarrationVisual(step1Segments []map[string]any, visualData map[string]any, episode int) (map[string]any, error) {
	visualSegments, _ := visualData["segments"].([]any)
	visualByID := map[string]map[string]any{}
	for _, item := range visualSegments {
		segment, ok := item.(map[string]any)
		if !ok {
			continue
		}
		sid, _ := segment["segment_id"].(string)
		if _, exists := visualByID[sid]; exists {
			return nil, fmt.Errorf("episode %d 视觉层 segment_id 重复: %s", episode, sid)
		}
		visualByID[sid] = segment
	}

	step1IDs := make([]string, 0, len(step1Segments))
	step1IDSet := map[string]bool{}
	for _, s := range step1Segments {
		sid, _ := s["segment_id"].(string)
		step1IDs = append(step1IDs, sid)
		step1IDSet[sid] = true
	}
	var missing []string
	for _, sid := range step1IDs {
		if _, ok := visualByID[sid]; !ok {
			missing = append(missing, sid)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("episode %d 视觉层缺少 step1 片段: %v", episode, missing)
	}
	var extra []string
	for sid := range visualByID {
		if !step1IDSet[sid] {
			extra = append(extra, sid)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return nil, fmt.Errorf("episode %d 视觉层含 step1 未定义的 segment_id: %v", episode, extra)
	}

	merged := make([]any, 0, len(step1Segments))
	for _, s1 := range step1Segments {
		sid, _ := s1["segment_id"].(string)
		combined := make(map[string]any, len(s1)+2)
		for k, v := range s1 {
			combined[k] = v
		}
		for k, v := range visualByID[sid] {
			combined[k] = v
		}
		merged = append(merged, combined)
	}
	title, _ := visualData["title"].(string)
	if title == "" {
		title = fmt.Sprintf("第%d集", episode)
	}
	return map[string]any{"title": title, "segments": merged}, nil
}

// ParseDramaVisual 解析 step2 视觉层响应为场景视觉 dict 列表（校验失败降级取原始 scenes）。
func ParseDramaVisual(responseText string) ([]map[string]any, error) {
	root, err := service.ParseJSONObject(responseText)
	if err != nil {
		return nil, fmt.Errorf("step2 视觉层 JSON 解析失败: %w", err)
	}
	rawScenes, ok := root["scenes"].([]any)
	if !ok || len(rawScenes) == 0 {
		return nil, fmt.Errorf("step2 视觉层结构校验失败：scenes 必须是非空数组")
	}
	out := make([]map[string]any, 0, len(rawScenes))
	for index, item := range rawScenes {
		scene, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("step2 视觉层结构校验失败：scenes.%d 必须是对象", index)
		}
		out = append(out, scene)
	}
	return out, nil
}

// DramaVisualMergeError 表示 step2 视觉层与 step1 内容层按 scene_id 合并失败。
type DramaVisualMergeError struct{ Message string }

func (e *DramaVisualMergeError) Error() string { return e.Message }

// dramaContentOnlyFields 是合并后从内容层剔除的、不属于最终场景的 step1-only 字段。
var dramaContentOnlyFields = map[string]bool{"scene_description": true}

// MergeDramaVisualIntoScenes 把 step2 视觉层按 scene_id 合并回 step1 内容层。
func MergeDramaVisualIntoScenes(contentScenes, visualScenes []map[string]any) ([]any, error) {
	visualByID := map[string]map[string]any{}
	for _, visual := range visualScenes {
		sid, _ := visual["scene_id"].(string)
		if sid == "" {
			return nil, &DramaVisualMergeError{Message: fmt.Sprintf("step2 视觉层条目缺少 scene_id: %v", visual)}
		}
		if _, exists := visualByID[sid]; exists {
			return nil, &DramaVisualMergeError{Message: "step2 视觉层 scene_id 重复: " + sid}
		}
		visualByID[sid] = visual
	}
	merged := make([]any, 0, len(contentScenes))
	contentIDs := map[string]bool{}
	for _, content := range contentScenes {
		sid, _ := content["scene_id"].(string)
		if sid == "" {
			return nil, &DramaVisualMergeError{Message: fmt.Sprintf("step1 内容层条目缺少 scene_id: %v", content)}
		}
		if contentIDs[sid] {
			return nil, &DramaVisualMergeError{Message: "step1 内容层 scene_id 重复: " + sid}
		}
		contentIDs[sid] = true
		visual, ok := visualByID[sid]
		if !ok {
			return nil, &DramaVisualMergeError{Message: "step1 场景 " + sid + " 缺少对应的 step2 视觉层"}
		}
		if _, ok := visual["image_prompt"]; !ok {
			return nil, &DramaVisualMergeError{Message: "step2 视觉层场景 " + sid + " 缺少必要的视觉字段"}
		}
		if _, ok := visual["video_prompt"]; !ok {
			return nil, &DramaVisualMergeError{Message: "step2 视觉层场景 " + sid + " 缺少必要的视觉字段"}
		}
		scene := map[string]any{}
		for k, v := range content {
			if dramaContentOnlyFields[k] {
				continue
			}
			scene[k] = v
		}
		scene["image_prompt"] = visual["image_prompt"]
		scene["video_prompt"] = visual["video_prompt"]
		merged = append(merged, scene)
	}
	var orphans []string
	for sid := range visualByID {
		if !contentIDs[sid] {
			orphans = append(orphans, sid)
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		return nil, &DramaVisualMergeError{Message: fmt.Sprintf("step2 视觉层存在 step1 内容中不存在的 scene_id: %v", orphans)}
	}
	return merged, nil
}

// toAnyStrings 把字符串切片包成 []any（JSON Schema 的 enum 值）。
func toAnyStrings(values []string) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}
