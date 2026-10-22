// 入队请求的结构校验与构造（对应 lib/generation_queue_client.py::TaskSpec.from_request）。
//
// 这是「一条入队请求在结构上是否合法」的唯一守卫点：WebUI 路由与 SDK 工具都经由这里
// 构造，规则不会分叉。校验与 provider 无关——能力类校验（duration ↔ supported_durations）
// 留在执行层，在 provider 解析之后做（见 ADR-0001）。
package generation

import (
	"fmt"
	"sort"
	"strings"
)

// 任务类型分组：决定 prompt 的结构校验规则。
var (
	taskSpecVideoTypes          = map[string]bool{"video": true}
	taskSpecImageStructured     = map[string]bool{"storyboard": true}
	taskSpecTTSTypes            = map[string]bool{"tts": true}
	taskSpecReservedPayloadKeys = []string{"prompt", "script_file"}
)

// TaskSpecValidationError 是结构校验失败：Code 是 i18n errors key，Params 是渲染参数。
type TaskSpecValidationError struct {
	Code   string
	Params map[string]any
}

func (e *TaskSpecValidationError) Error() string { return e.Code }

// TaskSpec 是一条入队请求的规格。
type TaskSpec struct {
	TaskType   string
	MediaType  string
	ResourceID string
	Payload    map[string]any
	ScriptFile *string
	Source     string
}

// BuildTaskSpec 结构校验后构造 TaskSpec。
//
// prompt 为 nil 表示「该任务类型允许缺省」（tts 从剧本 novel_text 读取文本）；
// extraPayload 不得携带守卫点已校验的保留键，否则调用方能绕过单一校验点把未校验的
// prompt / script_file 入队。tts 额外禁止 text（历史排空通道用的键）。
func BuildTaskSpec(
	taskType, mediaType, resourceID string, prompt any, scriptFile *string, extraPayload map[string]any,
) (TaskSpec, error) {
	if resourceID == "" {
		return TaskSpec{}, &TaskSpecValidationError{Code: "resource_id_required"}
	}
	if err := validateTaskPrompt(taskType, prompt); err != nil {
		return TaskSpec{}, err
	}
	reserved := map[string]bool{}
	for _, key := range taskSpecReservedPayloadKeys {
		reserved[key] = true
	}
	if taskSpecTTSTypes[taskType] {
		reserved["text"] = true
	}
	if extraPayload != nil {
		conflict := make([]string, 0)
		for key := range extraPayload {
			if reserved[key] {
				conflict = append(conflict, key)
			}
		}
		if len(conflict) > 0 {
			sort.Strings(conflict)
			return TaskSpec{}, fmt.Errorf("extra_payload contains reserved keys: %s", strings.Join(conflict, ", "))
		}
	}
	payload := map[string]any{}
	for k, v := range extraPayload {
		payload[k] = v
	}
	payload["prompt"] = prompt
	if scriptFile != nil {
		payload["script_file"] = *scriptFile
	}
	return TaskSpec{
		TaskType:   taskType,
		MediaType:  mediaType,
		ResourceID: resourceID,
		Payload:    payload,
		ScriptFile: scriptFile,
		Source:     "webui",
	}, nil
}

// validateTaskPrompt 按任务类型做 prompt 结构校验（provider 无关）。
func validateTaskPrompt(taskType string, prompt any) error {
	if taskSpecVideoTypes[taskType] {
		switch value := prompt.(type) {
		case map[string]any:
			// 结构化视频 prompt 必须带 action 键。
			if _, ok := value["action"]; !ok {
				return &TaskSpecValidationError{Code: "video_prompt_must_be_string_or_action_object"}
			}
			action := ""
			if raw, ok := value["action"].(string); ok {
				action = raw
			}
			if strings.TrimSpace(action) == "" {
				return &TaskSpecValidationError{Code: "video_prompt_action_empty"}
			}
			if dialogue, present := value["dialogue"]; present && dialogue != nil {
				if _, ok := dialogue.([]any); !ok {
					return &TaskSpecValidationError{Code: "video_prompt_dialogue_array"}
				}
			}
			return nil
		case string:
			if strings.TrimSpace(value) == "" {
				return &TaskSpecValidationError{Code: "prompt_text_empty"}
			}
			return nil
		case nil:
			return &TaskSpecValidationError{Code: "prompt_must_be_string_or_object"}
		default:
			return &TaskSpecValidationError{Code: "prompt_must_be_string_or_object"}
		}
	}

	if taskSpecTTSTypes[taskType] {
		if prompt == nil {
			return nil
		}
		if text, ok := prompt.(string); ok {
			if strings.TrimSpace(text) == "" {
				return &TaskSpecValidationError{Code: "prompt_text_empty"}
			}
			return nil
		}
		return &TaskSpecValidationError{Code: "tts_prompt_must_be_string_or_null"}
	}

	if taskSpecImageStructured[taskType] {
		if value, ok := prompt.(map[string]any); ok {
			if _, present := value["scene"]; !present {
				return &TaskSpecValidationError{Code: "prompt_must_be_string_or_scene_object"}
			}
			scene, _ := value["scene"].(string)
			if strings.TrimSpace(scene) == "" {
				return &TaskSpecValidationError{Code: "prompt_scene_empty"}
			}
			return nil
		}
	}

	// 资产（character / scene / prop / product）与字符串形态的分镜 prompt：非空字符串。
	if text, ok := prompt.(string); ok {
		if strings.TrimSpace(text) == "" {
			return &TaskSpecValidationError{Code: "prompt_text_empty"}
		}
		return nil
	}
	return &TaskSpecValidationError{Code: "prompt_must_be_string_or_object"}
}
