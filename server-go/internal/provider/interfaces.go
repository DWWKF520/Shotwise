package provider

import (
	"context"
	"errors"
	"fmt"
)

// MediaType 是生成媒体的通道类型，与队列的 image / video / audio 三条 lane 对齐；
// text 不是媒体通道，仅为注册表数据（构造规格表）保留该取值。
type MediaType string

const (
	MediaImage MediaType = "image"
	MediaVideo MediaType = "video"
	MediaAudio MediaType = "audio"
	MediaText  MediaType = "text"
)

// VideoBackend 是视频生成后端协议。
type VideoBackend interface {
	Name() string
	Model() string
	VideoCapabilities() VideoCapabilities
	Generate(ctx context.Context, req VideoGenerationRequest) (VideoGenerationResult, error)

	// ResumeVideo 接续 provider 上已发起的 job：轮询 + 下载，绝不重新 submit（重复 submit 等于
	// 重复计费）。未实现的 backend 返回 ErrResumeUnsupported，orphan 处理据此走
	// [resume_unsupported]；provider 端 job 过期/未找到返回 ResumeExpiredError，走
	// [resume_expired]。
	ResumeVideo(ctx context.Context, jobID string, req VideoGenerationRequest) (VideoGenerationResult, error)
}

// ImageBackend 是图片生成后端协议。
type ImageBackend interface {
	Name() string
	Model() string
	Capabilities() ImageCapabilitySet
	Generate(ctx context.Context, req ImageGenerationRequest) (ImageGenerationResult, error)
}

// AudioBackend 是语音合成后端协议。
type AudioBackend interface {
	Name() string
	Model() string
	Capabilities() AudioCapabilitySet
	// ListVoices 同步返回音色目录，不做网络请求。
	ListVoices() []VoiceOption
	Synthesize(ctx context.Context, req AudioSynthesisRequest) (AudioSynthesisResult, error)
}

// VideoGenerationRequest 是通用视频生成请求。各 backend 忽略不支持的字段。
//
// 路径字段用 string 承载本地文件路径（Python 侧为 pathlib.Path）。零值不等于 Python dataclass
// 默认值：AspectRatio / DurationSeconds / GenerateAudio / ServiceTier 的非零默认值由
// NewVideoGenerationRequest 填充，手工构造结构体时须自行补齐。
type VideoGenerationRequest struct {
	Prompt          string `json:"prompt"`
	OutputPath      string `json:"output_path"`
	AspectRatio     string `json:"aspect_ratio"`     // 默认 "9:16"
	DurationSeconds int    `json:"duration_seconds"` // 默认 5
	Resolution      string `json:"resolution,omitempty"`

	StartImage string `json:"start_image,omitempty"`
	// EndImage 供 first_last 模式（尾帧槽位，能力由 LastFrame 声明）。
	EndImage string `json:"end_image,omitempty"`
	// ReferenceImages 供多参考图模式，上限由 MaxReferenceImages 声明。
	ReferenceImages []string `json:"reference_images,omitempty"`

	// ReferenceAudioFiles 是参考音频（音色复刻）。列表顺序即 prompt 中「音频N」的指认契约：
	// 编排层按该顺序拼指认文本，后端按同一顺序下发，故任何一侧都不得重排或跳过。哪个角色对应
	// 哪段音频不进请求——绑定由 prompt 文本表达，供应商 API 均无结构化的「角色-音频」字段。
	ReferenceAudioFiles []string `json:"reference_audio_files,omitempty"`
	// ReferenceAudioTargets 仅被声明了 ReferenceAudioPerImage 的 backend 读取：与
	// ReferenceAudioFiles 等长同序，第 i 项是该段音频对应的 ReferenceImages 下标（0-based）。
	// 为空时这类 backend 按位置回退对齐，仅用于未经编排层填充的调用方（如手写测试）——参考音频
	// 与参考图各自独立派生顺序，位置对齐不构成契约，编排层必须显式提供。
	ReferenceAudioTargets []int `json:"reference_audio_targets,omitempty"`
	GenerateAudio         bool  `json:"generate_audio"` // 默认 true

	// ProjectName 是项目上下文（用于构建文件服务 URL 等）。
	ProjectName string `json:"project_name,omitempty"`

	// TaskID 在 worker 路径下从 task 记录传入，让 backend submit 后立即持久化 job_id；
	// 非 worker 路径（grid / 直生 / 测试）留空，持久化统一点据此跳过。
	TaskID string `json:"task_id,omitempty"`

	// ExecutionEndpoint 是自定义供应商包装层在转发给协议 backend 前注入的 endpoint 标识，
	// 与 job_id 一并持久化供续跑比对。内置供应商无 endpoint 维度，保持为空。
	ExecutionEndpoint string `json:"execution_endpoint,omitempty"`

	// SubmittedBaseURL 是续跑路径专用的、提交本 job 时实际使用的请求域名，由 resume 执行器
	// 从持久化列回放。backend 轮询时优先用它而非当下配置解析出的域名——域名是连接维度而非协议
	// 维度，用户在途改配置后按新域名轮旧 job 会查无（404）而被误判成过期。提交路径恒为空。
	SubmittedBaseURL string `json:"submitted_base_url,omitempty"`

	// 以下为 Seedance 特有字段。
	ServiceTier string `json:"service_tier"` // 默认 "default"
	Seed        *int64 `json:"seed,omitempty"`
}

// NewVideoGenerationRequest 按 Python dataclass 默认值构造请求（prompt / output_path 必填）。
func NewVideoGenerationRequest(prompt, outputPath string) VideoGenerationRequest {
	return VideoGenerationRequest{
		Prompt:          prompt,
		OutputPath:      outputPath,
		AspectRatio:     "9:16",
		DurationSeconds: 5,
		GenerateAudio:   true,
		ServiceTier:     "default",
	}
}

// VideoGenerationResult 是通用视频生成结果。
type VideoGenerationResult struct {
	VideoPath       string `json:"video_path"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	DurationSeconds int    `json:"duration_seconds"`

	VideoURI      string `json:"video_uri,omitempty"`
	Seed          *int64 `json:"seed,omitempty"`
	UsageTokens   *int64 `json:"usage_tokens,omitempty"`
	TaskID        string `json:"task_id,omitempty"`
	GenerateAudio *bool  `json:"generate_audio,omitempty"`
}

// ReferenceImage 是图片生成的参考图片。
type ReferenceImage struct {
	Path  string `json:"path"`
	Label string `json:"label,omitempty"`
}

// ImageGenerationRequest 是通用图片生成请求。各 backend 忽略不支持的字段。
type ImageGenerationRequest struct {
	Prompt          string           `json:"prompt"`
	OutputPath      string           `json:"output_path"`
	ReferenceImages []ReferenceImage `json:"reference_images,omitempty"`
	AspectRatio     string           `json:"aspect_ratio"` // 默认 "9:16"
	ImageSize       string           `json:"image_size,omitempty"`
	ProjectName     string           `json:"project_name,omitempty"`
	Seed            *int64           `json:"seed,omitempty"`
}

// NewImageGenerationRequest 按 Python dataclass 默认值构造请求。
func NewImageGenerationRequest(prompt, outputPath string) ImageGenerationRequest {
	return ImageGenerationRequest{Prompt: prompt, OutputPath: outputPath, AspectRatio: "9:16"}
}

// ImageGenerationResult 是通用图片生成结果。
type ImageGenerationResult struct {
	ImagePath string `json:"image_path"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`

	ImageURI    string `json:"image_uri,omitempty"`
	Seed        *int64 `json:"seed,omitempty"`
	UsageTokens *int64 `json:"usage_tokens,omitempty"`
	Quality     string `json:"quality,omitempty"`
	// 以下为 OpenAI GPT Image 系列的 token 用量拆分（其它 backend 默认 nil）。
	ImageInputTokens  *int64 `json:"image_input_tokens,omitempty"`
	ImageOutputTokens *int64 `json:"image_output_tokens,omitempty"`
	TextInputTokens   *int64 `json:"text_input_tokens,omitempty"`
	TextOutputTokens  *int64 `json:"text_output_tokens,omitempty"`
}

// AudioSynthesisRequest 是通用语音合成请求。各 backend 忽略不支持的字段。
type AudioSynthesisRequest struct {
	Text         string `json:"text"`
	OutputPath   string `json:"output_path"`
	Voice        string `json:"voice"`
	LanguageType string `json:"language_type"` // 默认 "Chinese"
	// Speed 为语速预留：同步的 qwen3-tts-flash 不支持（speech_rate 仅 realtime WebSocket 版
	// 可用），后端记 debug log 忽略。保留字段以便将来接入实时/可调速后端。
	Speed *float64 `json:"speed,omitempty"`
}

// NewAudioSynthesisRequest 按 Python dataclass 默认值构造请求。
func NewAudioSynthesisRequest(text, outputPath, voice string) AudioSynthesisRequest {
	return AudioSynthesisRequest{Text: text, OutputPath: outputPath, Voice: voice, LanguageType: "Chinese"}
}

// AudioSynthesisResult 是通用语音合成结果。Characters 驱动按字符计费。
type AudioSynthesisResult struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Characters int    `json:"characters"`
	OutputPath string `json:"output_path"`
}

// ErrResumeUnsupported 表示该 backend 未实现续跑（Python 侧为 NotImplementedError）。
// orphan 处理据此把持有 job_id 的 running 视频孤儿标成 [resume_unsupported]，而非重新 submit
// 造成二次扣费。
var ErrResumeUnsupported = errors.New("resume unsupported by this video backend")

// AmbiguousSubmitError 是 create/submit（非幂等的「创建 + 计费」）阶段的歧义态失败。
//
// 请求可能已抵达服务端并已落库 + 已计费，但响应在途丢失（读超时、写超时、连接中途断开等）。
// 此时自动重试会重复建任务 + 重复计费，故不重试、直接终态失败；error_message 带
// [create_ambiguous] 前缀提示运维到供应商侧确认任务状态后再手动重试（与 [restart_lost] /
// [resume_unsupported] 的「宁可手动重试、不可重复计费」先例一致，agent-facing 豁免 i18n）。
type AmbiguousSubmitError struct {
	Provider string
	Err      error
}

func (e *AmbiguousSubmitError) Error() string {
	return fmt.Sprintf(
		"[create_ambiguous] %s 创建请求可能已送达服务端但响应在途丢失（读超时/连接中途断开），"+
			"为避免重复建任务与重复计费不自动重试；请到供应商侧确认任务状态后再手动重试",
		e.Provider,
	)
}

// Unwrap 保留底层传输错误，供上层诊断（SubmitRetryable 等谓词按类型判定，不看消息文本）。
func (e *AmbiguousSubmitError) Unwrap() error { return e.Err }

// ResumeExpiredError 表示 provider 端 job 已过期或未找到——重启自愈无法接续，须走 mark_failed。
// 失败原因带 [resume_expired] 前缀（agent-facing，i18n 豁免），运维分析可见。
type ResumeExpiredError struct {
	JobID    string
	Provider string
	// Message 为空时用默认文案。
	Message string
}

func (e *ResumeExpiredError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("resume job %s expired or not found on provider %s", e.JobID, e.Provider)
}

// ResumeEndpointChangedError 表示提交本 job 时的 endpoint 与模型行当下的 endpoint 不同——
// 续跑必须显式失败。
//
// endpoint 决定协议，换 endpoint 等于换 backend；拿新协议 backend 轮旧协议下创建的 job 会
// 误读响应，把仍在跑仍在计费的远端 job 标成失败。「换身份续跑必须显式报错」在 endpoint 维度的
// 落点：只拦已提交、持有 job_id 的续跑，排队未提交的任务照常按新 endpoint 提交。仅自定义
// 供应商有该维度。
type ResumeEndpointChangedError struct {
	JobID             string
	Provider          string
	SubmittedEndpoint string
	CurrentEndpoint   string
}

func (e *ResumeEndpointChangedError) Error() string {
	return fmt.Sprintf(
		"resume job %s was submitted via endpoint %s on provider %s, but the model row now points to %s",
		e.JobID, e.SubmittedEndpoint, e.Provider, e.CurrentEndpoint,
	)
}

// CapabilityError 是能力不匹配错误（视频 / 图片侧共用同一结构，与 Python 的两个对称异常类
// 同形）。
//
// 不携带本地化字符串，只带稳定失败码 + 上下文 params：路由层按码渲染本地化文案，Worker 则按
// code + params 落 task.error_message，文案留到读侧按 Accept-Language 渲染。Error() 只返回码
// 本身（与 Python str(exc) 一致）——worker 的兜底编码路径会取 str(exc)，带参数渲染会把机器码
// 混成人类文本。
type CapabilityError struct {
	Code   FailureCode    `json:"code"`
	Params map[string]any `json:"params,omitempty"`
	Media  MediaType      `json:"media"`
}

func (e *CapabilityError) Error() string { return string(e.Code) }

// Detail 返回 [code] {json} 形态的诊断串，供日志使用；错误值本身仍是纯机器码。
func (e *CapabilityError) Detail() string {
	return encodeFailureRaw(e.Code, e.Params)
}

// NewVideoCapabilityError 构造视频侧能力错误。
func NewVideoCapabilityError(code FailureCode, params map[string]any) *CapabilityError {
	return &CapabilityError{Code: code, Params: params, Media: MediaVideo}
}

// NewImageCapabilityError 构造图片侧能力错误。
func NewImageCapabilityError(code FailureCode, params map[string]any) *CapabilityError {
	return &CapabilityError{Code: code, Params: params, Media: MediaImage}
}
