// Package provider 是生成供应商层的核心契约与纯逻辑：能力声明、内置注册表数据、
// 任务失败码、重试谓词、视频能力桶判定与请求期能力闸门。
//
// 本包不含 HTTP 客户端、DB 访问与具体 backend 实现；具体实现随各 backend 波次落地并注册
// 进本包的 registry。对应 Python 侧 lib/video_backends、lib/image_backends、lib/audio_backends
// 的 base/registry 模块与 lib/task_failure、lib/retry、lib/capability_buckets、lib/video_frame_slots
// 的纯逻辑部分。
package provider

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// FirstFrameAdaptiveRatio 是供应商侧字面量，表示「跟随首帧图片自身比例」而非用户指定的固定
// 比例。仅对声明 first_frame_ratio_adaptive_only 的 backend 有意义，通用比例解析器认不得它。
const FirstFrameAdaptiveRatio = "adaptive"

// ReferenceAudioMode 是后端接受参考音频的运输形态。
//
// DIRECT 表示随生成请求直传音频文件，模型据其复刻音色（如 Seedance 2.0 的
// role: reference_audio 内容条目、Wan2.7 r2v 挂在参考素材项上的 reference_voice）。
// NONE 表示该后端没有音色输入通道——带音频的请求在 GateVideoRequest 处硬失败，不静默丢弃。
type ReferenceAudioMode string

const (
	// ReferenceAudioNone 无音色输入通道。
	ReferenceAudioNone ReferenceAudioMode = "none"
	// ReferenceAudioDirect 音频随生成请求直传。
	ReferenceAudioDirect ReferenceAudioMode = "direct"
)

// VideoCapabilities 声明视频后端支持什么。字段语义是后端能力契约的唯一真相源，
// 与 Python 侧 lib/video_backends/base.py 的 dataclass 一一对应，注释含义原样保留。
//
// FirstFrame / LastFrame 描述图生视频路径的首帧与尾帧槽位。
//
// MaxReferenceImages 描述参考生视频路径：后端接受 reference_images 请求字段的数量上限，
// > 0 即该路径可用。不另设布尔位——两份声明会漂移出「称支持但上限为 0」这类自相矛盾的状态。
// 两条路径是否可叠加（同一请求同时带首帧与参考图）因后端而异，不是统一契约：部分后端拒绝
// 叠加（如 Agnes 抛能力错误），部分静默叠加（如 v2 中转、Grok、Sora 首帧与参考共享单槽）。
// 调用方不应假设某种统一行为，需按具体后端核实。
//
// ReferenceAudioMode / MaxReferenceAudioCount 描述参考音频路径，与参考图同构：模式非 NONE
// 时后端接受 reference_audio_files 请求字段，段数受上限约束。上限按 backend 各自的供应商
// 约束声明，不取各家交集。
//
// ReferenceAudioPerImage 表示音频是否必须逐段挂在某个具体的参考素材项上（如 wan2.7-r2v 的
// reference_voice 字段），而非作为独立的音色输入通道（如 Seedance 2.0 的 role: reference_audio
// 内容条目）。为 true 时调用方须随 ReferenceAudioFiles 一并提供
// VideoGenerationRequest.ReferenceAudioTargets，显式声明每段音频对应哪个参考素材项，不能假设
// 两个列表天然同序——参考音频的编排顺序是台词 speaker 首现顺序，参考图的编排顺序是 mention
// 首现顺序，两者独立派生，位置对齐纯属巧合。
//
// MaxReferenceAudioTotalSeconds 是多段参考音频叠加的总时长上限（nil = 该后端未声明聚合约束，
// 仅按 MaxReferenceAudioCount 卡段数）。段数上限推不出总时长——两段各处于单段合法区间的音频，
// 合计仍可能超出供应商总时长上限，故需独立声明。判定需要读音频元数据，调用方在
// GateVideoRequest 前置探测好总时长再传入。
//
// MaxPromptChars 是提示词字符数上限（nil = 该后端未声明约束）。声明的是该 model 无论走哪条
// 端点都成立的上限——部分供应商按端点各设更窄的值（如 Vidu 的参考生视频端点），那层收窄留在
// backend 组装期按实际端点 fail-loud，不塞进这个无端点上下文的静态声明里。计量口径为字符数
// （中英文同权），与各家文档一致。超限的典型失败模式是静默截断而非报错：供应商照常扣费、成片
// 与意图不符、用户无从知情，正是 GateVideoRequest 要在付费前堵住的降级。
//
// FirstFrameRatioAdaptiveOnly 表示该模型的首帧（image-to-video）任务是否只接受 "adaptive"
// 比例。声明为 true 时，ResolveFirstFrameAspectRatio 把带首帧的生成请求的
// VideoGenerationRequest.AspectRatio 改写为字面量 "adaptive"；不带首帧的请求（纯文生 /
// 仅参考图）与续接已发起 job 的 resume 路径不受影响。该字面量是供应商侧的取值，只对认得它的
// backend 有意义，故本位只由这类 backend 声明——别处（如比例解析）解析不了它，会按非法值
// 回退默认比例。
//
// 「首帧在场时用户比例不适用」这一情形另有 backend 各自的表达方式：dashscope 与 vidu 在
// payload 组装期直接不下发 ratio（上游忽略或拒收）。三者形状相近而取值策略不同（省略 vs 改写
// 为 adaptive），未收敛到同一开关；本位表达的是「改写为 adaptive」这一支。用户的比例意图仍
// 完整作用于分镜图生成——首帧图本就按该比例生成，「跟随首帧」与用户所选比例等价；改写只影响
// 视频请求实际下发的值，不改调用方持有的原始 AspectRatio（记账、版本元数据沿用后者）。
type VideoCapabilities struct {
	FirstFrame                    bool               `json:"first_frame"`
	LastFrame                     bool               `json:"last_frame"`
	MaxReferenceImages            int                `json:"max_reference_images"`
	ReferenceAudioMode            ReferenceAudioMode `json:"reference_audio_mode"`
	MaxReferenceAudioCount        int                `json:"max_reference_audio_count"`
	MaxReferenceAudioTotalSeconds *float64           `json:"max_reference_audio_total_seconds"`
	ReferenceAudioPerImage        bool               `json:"reference_audio_per_image"`
	MaxPromptChars                *int               `json:"max_prompt_chars"`
	FirstFrameRatioAdaptiveOnly   bool               `json:"first_frame_ratio_adaptive_only"`
}

// DefaultVideoCapabilities 返回与 Python dataclass 默认值一致的能力声明：
// FirstFrame 默认 true，ReferenceAudioMode 默认 NONE，其余为零值。
//
// Go 零值不是 Python 默认值（FirstFrame 零值为 false），构造能力声明一律从本函数出发，
// 不能直接声明裸 VideoCapabilities{}。
func DefaultVideoCapabilities() VideoCapabilities {
	return VideoCapabilities{
		FirstFrame:         true,
		ReferenceAudioMode: ReferenceAudioNone,
	}
}

// Normalized 把未声明的参考音频模式（空串）补齐为 NONE。
//
// 空串只可能来自 JSON 反序列化或手写声明遗漏，不是合法的运输形态取值；把「未声明」当 NONE
// 处理与 Python 的默认值语义一致。FirstFrame 等布尔位不在此自动补齐——false 可能是刻意的
// 声明（如纯文生后端），无从区分。
func (c VideoCapabilities) Normalized() VideoCapabilities {
	if c.ReferenceAudioMode == "" {
		c.ReferenceAudioMode = ReferenceAudioNone
	}
	return c
}

// ImageCapability 是图片后端能力位。
type ImageCapability uint8

const (
	// CapTextToImage 文生图。
	CapTextToImage ImageCapability = 1 << iota
	// CapImageToImage 图生图。
	CapImageToImage
)

// Name 返回与 Python 枚举等价的取值字符串。
func (c ImageCapability) Name() string {
	switch c {
	case CapTextToImage:
		return "text_to_image"
	case CapImageToImage:
		return "image_to_image"
	}
	return ""
}

// ImageCapabilitySet 是图片后端能力位集合。
//
// JSON 序列化为字符串数组（与 Python 的 set[ImageCapability] 序列化等价），取值字符串为
// text_to_image / image_to_image。
type ImageCapabilitySet uint8

// NewImageCapabilitySet 按位聚合能力位。
func NewImageCapabilitySet(caps ...ImageCapability) ImageCapabilitySet {
	var s ImageCapabilitySet
	for _, c := range caps {
		s |= ImageCapabilitySet(c)
	}
	return s
}

// Has 判断是否声明了该能力位。
func (s ImageCapabilitySet) Has(c ImageCapability) bool { return s&ImageCapabilitySet(c) != 0 }

// Names 返回已声明能力的取值字符串，按 Python sorted(set[StrEnum]) 的字典序排列。
func (s ImageCapabilitySet) Names() []string {
	names := make([]string, 0, 2)
	for _, c := range []ImageCapability{CapTextToImage, CapImageToImage} {
		if s.Has(c) {
			names = append(names, c.Name())
		}
	}
	sort.Strings(names)
	return names
}

// String 返回以 "|" 连接的能力名，便于日志与错误信息。
func (s ImageCapabilitySet) String() string { return strings.Join(s.Names(), "|") }

// MarshalJSON 序列化为 Python 等价的字符串数组。
func (s ImageCapabilitySet) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Names())
}

// UnmarshalJSON 从字符串数组反序列化；未知取值直接报错（fail loud，不静默丢弃能力位）。
func (s *ImageCapabilitySet) UnmarshalJSON(data []byte) error {
	names, err := decodeCapabilityNames(data)
	if err != nil {
		return err
	}
	var set ImageCapabilitySet
	for _, name := range names {
		switch name {
		case "text_to_image":
			set |= ImageCapabilitySet(CapTextToImage)
		case "image_to_image":
			set |= ImageCapabilitySet(CapImageToImage)
		default:
			return fmt.Errorf("unknown image capability: %q", name)
		}
	}
	*s = set
	return nil
}

// AudioCapability 是音频后端能力位。
type AudioCapability uint8

// CapTextToSpeech 文本转语音。
const CapTextToSpeech AudioCapability = 1 << iota

// Name 返回与 Python 枚举等价的取值字符串。
func (c AudioCapability) Name() string {
	if c == CapTextToSpeech {
		return "text_to_speech"
	}
	return ""
}

// AudioCapabilitySet 是音频后端能力位集合，JSON 序列化与 ImageCapabilitySet 同构。
type AudioCapabilitySet uint8

// NewAudioCapabilitySet 按位聚合能力位。
func NewAudioCapabilitySet(caps ...AudioCapability) AudioCapabilitySet {
	var s AudioCapabilitySet
	for _, c := range caps {
		s |= AudioCapabilitySet(c)
	}
	return s
}

// Has 判断是否声明了该能力位。
func (s AudioCapabilitySet) Has(c AudioCapability) bool { return s&AudioCapabilitySet(c) != 0 }

// Names 返回已声明能力的取值字符串，字典序排列。
func (s AudioCapabilitySet) Names() []string {
	names := make([]string, 0, 1)
	if s.Has(CapTextToSpeech) {
		names = append(names, CapTextToSpeech.Name())
	}
	return names
}

// String 返回以 "|" 连接的能力名。
func (s AudioCapabilitySet) String() string { return strings.Join(s.Names(), "|") }

// MarshalJSON 序列化为 Python 等价的字符串数组。
func (s AudioCapabilitySet) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Names())
}

// UnmarshalJSON 从字符串数组反序列化；未知取值直接报错。
func (s *AudioCapabilitySet) UnmarshalJSON(data []byte) error {
	names, err := decodeCapabilityNames(data)
	if err != nil {
		return err
	}
	var set AudioCapabilitySet
	for _, name := range names {
		if name != "text_to_speech" {
			return fmt.Errorf("unknown audio capability: %q", name)
		}
		set |= AudioCapabilitySet(CapTextToSpeech)
	}
	*s = set
	return nil
}

// VoiceOption 是音色目录条目。ID 是合成请求携带的 voice 参数值，Label 是给人看的展示名。
type VoiceOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func decodeCapabilityNames(data []byte) ([]string, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return nil, fmt.Errorf("capability set must be a JSON string array: %w", err)
	}
	return names, nil
}
