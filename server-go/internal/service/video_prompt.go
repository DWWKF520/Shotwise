// 视频 prompt 的书写层组装（对应 lib/prompt_utils.py 的视频面）。供视频生成工具与执行层共用：
// 结构化 video_prompt → YAML 文本，以及 drama 口型台词与 Voice_Profiles 声明的机械派生出口。
package service

import (
	"fmt"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
)

// videoAvoidItems 是视频统一反向提示词清单（与分镜图侧同源）。
const videoAvoidItems = "BGM、文字字幕、水印"

// IsStructuredVideoPrompt 对应 is_structured_video_prompt：对象且带 action 键。
func IsStructuredVideoPrompt(videoPrompt any) bool {
	m, ok := videoPrompt.(map[string]any)
	if !ok {
		return false
	}
	_, has := m["action"]
	return has
}

// StripVoiceProfiles 剥离入参自带的 voice_profiles 键。
//
// Voice_Profiles 声明段由编排层从角色资产机械派生，剧本 JSON / 调用方请求体均不承载它——
// drama 之外的 content_mode 或缺 utterances 的条目才不会让残留值绕过真无声门控进入 YAML。
func StripVoiceProfiles(videoPrompt map[string]any) map[string]any {
	out := make(map[string]any, len(videoPrompt))
	for k, v := range videoPrompt {
		if k == "voice_profiles" {
			continue
		}
		out[k] = v
	}
	return out
}

// BuildDramaVideoPrompt 是 drama video_prompt 的 dialogue 与 Voice_Profiles 唯一注入出口。
//
// characters 为 nil 表示不注入 Voice_Profiles（真无声模型 / 本集关闭音频）。
func BuildDramaVideoPrompt(videoPrompt map[string]any, utterances any, characters *domain.AssetBucket) map[string]any {
	prompt := StripVoiceProfiles(videoPrompt)
	dialogue := utterancesToDialogue(utterances)
	return attachVoiceProfiles(prompt, dialogue, characters)
}

// BuildDramaVideoPromptFromLegacyDialogue 是 legacy drama 场景（台词仍留在 video_prompt.dialogue、
// 无场景级 utterances 字段）的 Voice_Profiles 注入出口。
func BuildDramaVideoPromptFromLegacyDialogue(videoPrompt map[string]any, characters *domain.AssetBucket) map[string]any {
	prompt := StripVoiceProfiles(videoPrompt)
	dialogue, ok := prompt["dialogue"].([]any)
	if !ok {
		dialogue = nil
	}
	return attachVoiceProfiles(prompt, dialogue, characters)
}

// attachVoiceProfiles 写回 dialogue 并按角色资产派生 Voice_Profiles（characters 为 nil 时不注入）。
func attachVoiceProfiles(prompt map[string]any, dialogue []any, characters *domain.AssetBucket) map[string]any {
	prompt["dialogue"] = dialogue
	if characters != nil {
		if profiles := buildVoiceProfiles(dialogue, characters); len(profiles) > 0 {
			prompt["voice_profiles"] = profiles
		}
	}
	return prompt
}

// buildVoiceProfiles 从 dialogue speakers 与角色资产派生 Voice_Profiles 声明段。
//
// 仅收录角色资产存在且 voice_style 非空者，一个 speaker 一条（按首现顺序去重）；speaker 与角色
// 表 key 可能是 NFC/NFD 中的任一形态，按归一名索引与去重，Speaker 展示值保留 dialogue 原文。
func buildVoiceProfiles(dialogue []any, characters *domain.AssetBucket) []any {
	seen := map[string]bool{}
	var profiles []any
	for _, raw := range dialogue {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		speaker, _ := entry["speaker"].(string)
		if speaker == "" {
			continue
		}
		speakerKey := domain.NormalizeAssetName(speaker)
		if seen[speakerKey] {
			continue
		}
		seen[speakerKey] = true
		if characters == nil {
			continue
		}
		key, found := characters.ResolveKey(speakerKey)
		if !found {
			continue
		}
		character := (*characters)[key]
		if character == nil {
			continue
		}
		voiceStyle := character.MustString("voice_style")
		if strings.TrimSpace(voiceStyle) != "" {
			profiles = append(profiles, map[string]any{
				"Speaker":     speaker,
				"Voice_Style": strings.TrimSpace(voiceStyle),
			})
		}
	}
	return profiles
}

// utterancesToDialogue 是 drama 口型音轨出口：从有序 utterances 取 dialogue-kind 条目，转成
// video YAML 的 {speaker, line} 列表（保留时序）。
//
// 对脏数据稳健：非 list 整体、非对象元素、非 dialogue 条目一律跳过；dialogue 须 speaker 与
// text 同时非空才进口型音轨。
func utterancesToDialogue(utterances any) []any {
	list, ok := utterances.([]any)
	if !ok {
		return nil
	}
	var dialogue []any
	for _, raw := range list {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := entry["kind"].(string); kind != "dialogue" {
			continue
		}
		speaker, _ := entry["speaker"].(string)
		line, _ := entry["text"].(string)
		speaker = strings.TrimSpace(speaker)
		line = strings.TrimSpace(line)
		if speaker != "" && line != "" {
			dialogue = append(dialogue, map[string]any{"speaker": speaker, "line": line})
		}
	}
	return dialogue
}

// VideoPromptToYAML 把结构化 video_prompt 结构转换为 YAML 文本，键序
// Voice_Profiles / Action / Camera_Motion / Ambiance_Audio / Dialogue / Avoid
// （对应 lib/prompt_utils.py::video_prompt_to_yaml）。
func VideoPromptToYAML(videoPrompt map[string]any) (string, error) {
	action, _ := videoPrompt["action"].(string)
	if strings.TrimSpace(action) == "" {
		return "", fmt.Errorf("prompt.action must not be empty")
	}
	cameraMotion, _ := videoPrompt["camera_motion"].(string)
	ambiance, _ := videoPrompt["ambiance_audio"].(string)

	var b strings.Builder
	if profiles, ok := videoPrompt["voice_profiles"].([]any); ok && len(profiles) > 0 {
		b.WriteString("Voice_Profiles:")
		writeVoiceProfileItems(&b, profiles)
	}
	b.WriteString("Action: " + PromptYAMLScalar(action) + "\n")
	b.WriteString("Camera_Motion: " + PromptYAMLScalar(cameraMotion) + "\n")
	b.WriteString("Ambiance_Audio: " + PromptYAMLScalar(ambiance) + "\n")

	if dialogue, ok := videoPrompt["dialogue"].([]any); ok && len(dialogue) > 0 {
		b.WriteString("Dialogue:\n")
		for _, raw := range dialogue {
			entry, _ := raw.(map[string]any)
			speaker, _ := entry["speaker"].(string)
			line, _ := entry["line"].(string)
			b.WriteString("- Speaker: " + PromptYAMLScalar(speaker) + "\n")
			b.WriteString("  Line: " + PromptYAMLScalar(line) + "\n")
		}
	}
	b.WriteString("Avoid: " + PromptYAMLScalar(videoAvoidItems) + "\n")
	return b.String(), nil
}

// writeVoiceProfileItems 渲染 Voice_Profiles 声明段（列表紧跟顶层键名）。
func writeVoiceProfileItems(b *strings.Builder, profiles []any) {
	for _, raw := range profiles {
		entry, _ := raw.(map[string]any)
		speaker, _ := entry["Speaker"].(string)
		style, _ := entry["Voice_Style"].(string)
		b.WriteString("\n- Speaker: " + PromptYAMLScalar(speaker) + "\n")
		b.WriteString("  Voice_Style: " + PromptYAMLScalar(style) + "\n")
	}
}

// NormalizeStylePrefix 去掉风格值开头的「画风：」前缀并 strip 两端空白（幂等）。
// 存量项目的 style 取自旧版风格模版，不清理会渲染成 Style: 画风：... 的中英混叠。
func NormalizeStylePrefix(style string) string {
	s := strings.TrimSpace(style)
	for _, prefix := range []string{"画风：", "画风:"} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
			break
		}
	}
	return s
}
