// 参考生视频渲染层的声音能力档与读时派生（对应 lib/reference_video/voice_settings.py 与
// lib/reference_video/script_preview.py 的 utterances / 声音绑定派生）。
//
// 渲染层（解析预览 / 三段论渲染 / 执行期组装）对声音的判定全部取自同一组入参。本模块把
// 「谁在说话、谁绑到第几段参考音频」收敛到一处，并渲染回给 agent 的容忍类 warning 文案。
package reference

import (
	"fmt"
	"github.com/shotwise/shotwise/internal/service"
	"sort"
	"strings"

	domainproject "github.com/shotwise/shotwise/internal/domain/project"
)

// 参考文稿派生的降级可见性 warning 键（与 script_preview.py 同源）。
const (
	WarnUnregisteredMention     = "ref_warn_unregistered_mention"
	WarnUnclosedBrace           = "ref_warn_unclosed_brace"
	WarnDialogueInline          = "ref_warn_dialogue_inline"
	WarnUnregisteredSpeaker     = "ref_warn_unregistered_speaker"
	WarnSpeakerWithoutAudio     = "ref_warn_speaker_without_audio"
	WarnSpeakerAudioUnavailable = "ref_warn_speaker_audio_unavailable"
	WarnReferenceAudioOverflow  = "ref_warn_reference_audio_overflow"
	WarnSilentModel             = "ref_warn_silent_model"
	WarnSilentEpisode           = "ref_warn_silent_episode"
	WarnSpeakerAudioNeedsImage  = "ref_warn_speaker_audio_needs_image"
)

// referenceToleratedVoiceWarnings 是落盘照常、只随产物呈现的容忍 warning（声音降级）。
// 其余 warning 键（未登记 mention / 说话人、语法误用）在机器产物这条路上是阻断违约。
var referenceToleratedVoiceWarnings = map[string]bool{
	WarnSpeakerWithoutAudio:    true,
	WarnReferenceAudioOverflow: true,
	WarnSilentModel:            true,
	WarnSilentEpisode:          true,
}

// VoiceRenderSettings 是一次渲染的声音输入档（对应 VoiceRenderSettings）。
type VoiceRenderSettings struct {
	// VoiceConsistency 是服务端派生的三级模型档标识（native / soft / none）。
	VoiceConsistency string
	// RequestedGenerateAudio 是本集的无声开关（用户意图，非计价口径）。
	RequestedGenerateAudio bool
	// MaxReferenceAudio 是每请求可携带的参考音频段数上限（backend 能力声明）。
	MaxReferenceAudio int
	// ModelID 只用于降级 warning 的文案回显。
	ModelID string
	// AudioReady 是「音频确实可用」的角色名集合（键已归一）：解析预览留 nil 表示按角色资产的
	// reference_audio 字段非空判定；执行层传入已解析且确实存在的文件对应的角色名，让音频编号
	// 与实际随请求发出的段数严格等长。
	AudioReady map[string]bool
	// RequiresReferenceImage 为 true 时目标 backend 要求音频逐段挂在具体参考素材项上。
	RequiresReferenceImage bool
}

// isSilent 判定这一集是否听不到声音——模型不产音（C 类）或本集关闭了音频，两条路径同口径。
func (s VoiceRenderSettings) isSilent() bool {
	return s.VoiceConsistency == "none" || !s.RequestedGenerateAudio
}

// VoiceRenderSettingsFromCaps 从项目视频能力派生声音输入档，与解析预览 / 执行层同源。
func VoiceRenderSettingsFromCaps(caps service.ProjectVideoCaps, project *domainproject.ProjectFile) VoiceRenderSettings {
	settings := VoiceRenderSettings{
		VoiceConsistency:       "soft",
		RequestedGenerateAudio: caps.RequestedGenerateAudio,
	}
	if caps.Resolved {
		settings.VoiceConsistency = service.DeriveVoiceConsistency(caps.View, project.GenerationMode)
		settings.MaxReferenceAudio = caps.View.Caps.MaxReferenceAudioCount
		settings.ModelID = caps.View.ModelID
		settings.RequiresReferenceImage = caps.View.Caps.ReferenceAudioPerImage
	}
	return settings
}

// voiceWarning 是 locale-neutral 的 warning 条目（key + params），由呈现层渲染。
type voiceWarning struct {
	key    string
	params map[string]any
}

// shotUtterance 是镜头级发声条目：shotIndex 是 1-based 镜头序号。
type shotUtterance struct {
	shotIndex int
	kind      string
	speaker   string
	text      string
}

// deriveUtterances 逐镜逐行派生 utterances，并收集语法层 warning。
func deriveUtterances(shots []map[string]any) ([]shotUtterance, []voiceWarning) {
	var utterances []shotUtterance
	var warnings []voiceWarning
	for index, shot := range shots {
		body, _ := shot["text"].(string)
		for _, line := range splitShotSourceLines(body) {
			if strings.Count(line, "{") != strings.Count(line, "}") {
				warnings = append(warnings, voiceWarning{WarnUnclosedBrace, map[string]any{
					"shot": index + 1, "excerpt": truncateRunes(normalizeShotSource(line), 30),
				}})
				continue
			}
			if speaker, spoken, ok := matchDialogueLine(line); ok {
				utterances = append(utterances, shotUtterance{shotIndex: index + 1, kind: "dialogue", speaker: speaker, text: spoken})
				continue
			}
			if voiceover, ok := matchVoiceoverLine(line); ok {
				utterances = append(utterances, shotUtterance{shotIndex: index + 1, kind: "voiceover", text: voiceover})
				continue
			}
			if strings.Contains(line, "{") {
				warnings = append(warnings, voiceWarning{WarnDialogueInline, map[string]any{"shot": index + 1}})
			}
		}
	}
	return utterances, warnings
}

// voiceBindings 是一份文稿的声音派生结果。
type voiceBindings struct {
	// speakers 是已登记的 dialogue speaker（首现顺序）。
	speakers []string
	// audioSpeakers 是其中真正绑上参考音频的子集，顺序即 @音频N 编号。
	audioSpeakers []string
	warnings      []voiceWarning
}

// deriveVoiceBindings 从 utterances 机械派生声音绑定：说话人顺序、参考音频编号与降级 warning。
func deriveVoiceBindings(
	utterances []shotUtterance,
	characters domainproject.AssetBucket,
	settings VoiceRenderSettings,
	speakersWithReferenceImage map[string]bool,
) voiceBindings {
	var warnings []voiceWarning
	if characters == nil {
		characters = domainproject.AssetBucket{}
	}

	var seen []string
	for _, entry := range utterances {
		speaker := domainproject.NormalizeAssetName(entry.speaker)
		if speaker != "" && !service.ContainsString(seen, speaker) {
			seen = append(seen, speaker)
		}
	}

	var registered []string
	for _, speaker := range seen {
		if _, ok := characters.ResolveKey(speaker); ok {
			registered = append(registered, speaker)
		} else {
			warnings = append(warnings, voiceWarning{WarnUnregisteredSpeaker, map[string]any{"name": speaker}})
		}
	}

	var audioSpeakers []string
	if settings.isSilent() {
		hasDialogue := false
		for _, entry := range utterances {
			if entry.kind == "dialogue" {
				hasDialogue = true
				break
			}
		}
		if hasDialogue {
			if !settings.RequestedGenerateAudio {
				warnings = append(warnings, voiceWarning{WarnSilentEpisode, map[string]any{}})
			} else {
				warnings = append(warnings, voiceWarning{WarnSilentModel, map[string]any{"model": settings.ModelID}})
			}
		}
	} else if settings.VoiceConsistency == "native" {
		for _, speaker := range registered {
			charData := resolveEntry(characters, speaker)
			audioFieldSet := false
			if charData != nil {
				if value, ok := charData.Fields()["reference_audio"].(string); ok && value != "" {
					audioFieldSet = true
				}
			}
			// AudioReady 为 nil 时按字段非空判定（预览口径）；非 nil 时按「确实可用」判定，
			// 并把「字段已设置但音频不可用」与「字段未设置」分成两条不同措辞的 warning
			// （排查方向不同）。
			hasAudio := audioFieldSet
			if settings.AudioReady != nil {
				hasAudio = settings.AudioReady[speaker]
			}
			if !hasAudio {
				if settings.AudioReady != nil && audioFieldSet {
					warnings = append(warnings, voiceWarning{WarnSpeakerAudioUnavailable, map[string]any{"name": speaker}})
				} else {
					warnings = append(warnings, voiceWarning{WarnSpeakerWithoutAudio, map[string]any{"name": speaker}})
				}
			} else if settings.RequiresReferenceImage && !speakersWithReferenceImage[speaker] {
				warnings = append(warnings, voiceWarning{WarnSpeakerAudioNeedsImage, map[string]any{"name": speaker}})
			} else if len(audioSpeakers) >= settings.MaxReferenceAudio {
				warnings = append(warnings, voiceWarning{WarnReferenceAudioOverflow, map[string]any{
					"limit": settings.MaxReferenceAudio, "name": speaker,
				}})
			} else {
				audioSpeakers = append(audioSpeakers, speaker)
			}
		}
	}

	return voiceBindings{speakers: registered, audioSpeakers: audioSpeakers, warnings: warnings}
}

// resolveEntry 按比对坐标系取角色条目。
func resolveEntry(bucket domainproject.AssetBucket, name string) *domainproject.AssetEntry {
	if bucket == nil {
		return nil
	}
	if key, ok := bucket.ResolveKey(name); ok {
		return bucket[key]
	}
	return nil
}

// referenceVoiceWarningLines 逐 unit 派生声音绑定，取容忍类 warning 的渲染文本（去重、保持首现顺序）。
//
// 逐 unit 而非把全集正文拼起来判：unit 就是一次生成调用，参考音频段数上限按调用计。音频编号
// 在本处一律按角色资产的 reference_audio 字段非空判定（预览口径不碰文件系统），与编辑器预览
// 同源。
func ReferenceVoiceWarningLines(unitTexts []string, project *domainproject.ProjectFile, voice VoiceRenderSettings) []string {
	var characters domainproject.AssetBucket
	if project != nil {
		characters = project.Characters
	}
	settings := voice
	settings.RequiresReferenceImage = false
	seen := map[string]bool{}
	var lines []string
	for _, text := range unitTexts {
		shots, _ := parsePrompt(text)
		utterances, _ := deriveUtterances(shots)
		bindings := deriveVoiceBindings(utterances, characters, settings, nil)
		for _, warning := range bindings.warnings {
			if !referenceToleratedVoiceWarnings[warning.key] {
				continue
			}
			rendered := renderVoiceWarning(warning)
			dedupeKey := warning.key + "\x00" + rendered
			if seen[dedupeKey] {
				continue
			}
			seen[dedupeKey] = true
			lines = append(lines, rendered)
		}
	}
	return lines
}

// renderVoiceWarning 把容忍类 warning 渲染成面向 agent 的中文文案（参数用 %v 插值）。
func renderVoiceWarning(warning voiceWarning) string {
	param := func(key string) string {
		if warning.params == nil {
			return ""
		}
		if value, ok := warning.params[key]; ok {
			return fmt.Sprint(value)
		}
		return ""
	}
	switch warning.key {
	case WarnSpeakerWithoutAudio:
		return fmt.Sprintf("角色「%s」未设置参考音频：台词声音将由模型自行决定", param("name"))
	case WarnReferenceAudioOverflow:
		return fmt.Sprintf("参考音频最多 %s 段：角色「%s」的台词声音将由模型自行决定", param("limit"), param("name"))
	case WarnSilentModel:
		return fmt.Sprintf("当前视频模型「%s」不生成音频，台词仅用于提示词参考", param("model"))
	case WarnSilentEpisode:
		return "本集已关闭参考音频：不会上传参考音频，声音一致性不生效，台词仅用于提示词参考；成片是否有声音由所选模型决定"
	case WarnSpeakerAudioNeedsImage:
		return fmt.Sprintf("角色「%s」没有参考图（纯画外）：当前视频模型要求参考音频逐段挂在参考图上，该角色的台词声音将由模型自行决定", param("name"))
	case WarnUnregisteredMention:
		return fmt.Sprintf("@[%s] 未在角色/场景/道具中登记：不会附带参考图，请检查名称或先创建资产", param("name"))
	default:
		return warning.key
	}
}

// sortedSpeakers 返回去重并排序的说话人（供报告稳定输出）。
func sortedSpeakers(speakers []string) []string {
	out := append([]string(nil), speakers...)
	sort.Strings(out)
	return out
}
