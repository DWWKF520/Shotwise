package providers

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// OpenAIAudioBackend —— OpenAI 兼容语音合成后端（同步 /v1/audio/speech）
// （端口自 lib/audio_backends/openai.py）。
//
// 请求体携带 model / input / voice（必填）与可选 response_format / speed，响应直接返回音频字节
// （无需二段下载）。主要服务自定义供应商通路：任意 OpenAI 兼容 TTS 经 openai-tts endpoint 包装
// 为 CustomAudioBackend 后接入。本后端不注册，仅作 delegate 使用。

const (
	// openAIAudioFallbackFormat 是落盘扩展名不受支持时的兜底输出格式。
	openAIAudioFallbackFormat = "wav"
	// openAIAudioRequestTimeout 单次合成请求的墙钟预算（SDK 默认 600s）。
	openAIAudioRequestTimeout = 600 * time.Second
)

// openAIAudioSupportedFormats 是 /v1/audio/speech 支持的输出格式，用于按落盘扩展名选 response_format。
var openAIAudioSupportedFormats = map[string]struct{}{
	"mp3": {}, "opus": {}, "aac": {}, "flac": {}, "wav": {}, "pcm": {},
}

// openAIAudioVoiceCatalog 是官方内置音色（gpt-4o-mini-tts，含 tts-1/tts-1-hd legacy 子集）。
//
// 官方文档未给出性别/描述信息，故 label 仅取 id 本身——不编造。
var openAIAudioVoiceCatalog = []provider.VoiceOption{
	{ID: "alloy", Label: "alloy"},
	{ID: "ash", Label: "ash"},
	{ID: "ballad", Label: "ballad"},
	{ID: "coral", Label: "coral"},
	{ID: "echo", Label: "echo"},
	{ID: "fable", Label: "fable"},
	{ID: "nova", Label: "nova"},
	{ID: "onyx", Label: "onyx"},
	{ID: "sage", Label: "sage"},
	{ID: "shimmer", Label: "shimmer"},
	{ID: "verse", Label: "verse"},
	{ID: "marin", Label: "marin"},
	{ID: "cedar", Label: "cedar"},
}

// openAIAudioLegacyModels 是 legacy 模型（tts-1 / tts-1-hd）。
var openAIAudioLegacyModels = map[string]struct{}{"tts-1": {}, "tts-1-hd": {}}

// openAIAudioLegacyUnsupportedVoices 是 legacy 模型不支持的音色子集：仅 gpt-4o-mini-tts 支持。
// legacy 模型下若仍暴露它们，用户选中即会在合成阶段确定性失败。
var openAIAudioLegacyUnsupportedVoices = map[string]struct{}{
	"ballad": {}, "verse": {}, "marin": {}, "cedar": {},
}

// openAIAudioResponseFormat 按落盘扩展名选输出格式，保证文件内容与扩展名一致（资源路径约定 .wav）。
func openAIAudioResponseFormat(outputPath string) string {
	suffix := strings.TrimPrefix(strings.ToLower(filepath.Ext(outputPath)), ".")
	if _, ok := openAIAudioSupportedFormats[suffix]; ok {
		return suffix
	}
	return openAIAudioFallbackFormat
}

// openAIAudioBackend 是 OpenAI 兼容语音合成后端实例。
type openAIAudioBackend struct {
	model        string
	providerName string
	baseURL      string
	apiKey       string
	client       *httpc.Client
}

// newOpenAIAudioBackend 按通用信封构造；model 必填，provider_name 缺省为 "openai"。
//
// 不注册，仅作自定义供应商包装层的 delegate 构造闭包使用：OpenAI 兼容协议的 provider（如
// openai-tts）须用真实 provider 记账，故 provider_name 可被覆写。
func newOpenAIAudioBackend(opts provider.BackendOptions) (provider.AudioBackend, error) {
	if strings.TrimSpace(opts.Model) == "" {
		return nil, errors.New("OpenAIAudioBackend 需要 model")
	}
	baseURL := strings.TrimSpace(opts.BaseURL)
	if baseURL == "" {
		baseURL = openAIDefaultBaseURL
	}
	providerName := strings.TrimSpace(opts.Extra["provider_name"])
	if providerName == "" {
		providerName = ProviderOpenAI
	}
	return &openAIAudioBackend{
		model:        strings.TrimSpace(opts.Model),
		providerName: providerName,
		baseURL:      strings.TrimRight(baseURL, "/"),
		apiKey:       opts.APIKey,
		client:       httpc.NewClient(openAIAudioRequestTimeout),
	}, nil
}

// Name 返回供应商（记账）标识。
func (b *openAIAudioBackend) Name() string { return b.providerName }

// Model 返回实际下发的模型名。
func (b *openAIAudioBackend) Model() string { return b.model }

// Capabilities 返回本后端能力位。
func (b *openAIAudioBackend) Capabilities() provider.AudioCapabilitySet {
	return provider.NewAudioCapabilitySet(provider.CapTextToSpeech)
}

// ListVoices 返回音色目录（同步，不做网络请求）。
//
// legacy 收窄只对官方 OpenAI 生效：自定义 openai-tts 供应商（provider_name 被覆写）即使模型名
// 恰好也叫 tts-1/tts-1-hd，也无法确定其是否真的继承官方同名模型的音色限制——维持全量目录。
func (b *openAIAudioBackend) ListVoices() []provider.VoiceOption {
	if b.providerName == ProviderOpenAI {
		if _, legacy := openAIAudioLegacyModels[b.model]; legacy {
			filtered := make([]provider.VoiceOption, 0, len(openAIAudioVoiceCatalog))
			for _, voice := range openAIAudioVoiceCatalog {
				if _, unsupported := openAIAudioLegacyUnsupportedVoices[voice.ID]; !unsupported {
					filtered = append(filtered, voice)
				}
			}
			return filtered
		}
	}
	return append([]provider.VoiceOption(nil), openAIAudioVoiceCatalog...)
}

// Synthesize 合成语音并落盘。
func (b *openAIAudioBackend) Synthesize(
	ctx context.Context, req provider.AudioSynthesisRequest,
) (provider.AudioSynthesisResult, error) {
	audio, err := b.requestSpeech(ctx, req)
	if err != nil {
		return provider.AudioSynthesisResult{}, err
	}
	if err := writeFileAtomic(req.OutputPath, audio); err != nil {
		return provider.AudioSynthesisResult{}, err
	}
	return provider.AudioSynthesisResult{
		Provider:   b.providerName,
		Model:      b.model,
		Characters: utf8.RuneCountInString(req.Text),
		OutputPath: req.OutputPath,
	}, nil
}

// requestSpeech 提交合成请求（计费段），返回音频字节。
//
// language_type 是 DashScope 特有字段，/v1/audio/speech 无对应参数（语种随输入文本），不发送。
func (b *openAIAudioBackend) requestSpeech(
	ctx context.Context, req provider.AudioSynthesisRequest,
) ([]byte, error) {
	payload := map[string]any{
		"model":           b.model,
		"input":           req.Text,
		"voice":           req.Voice,
		"response_format": openAIAudioResponseFormat(req.OutputPath),
	}
	if req.Speed != nil {
		payload["speed"] = *req.Speed
	}

	var audio []byte
	err := provider.RunWithRetry(
		ctx, provider.DefaultRetryPolicy, provider.IsTransientUpstreamError,
		func(ctx context.Context) error {
			resp, err := b.client.PostJSON(
				ctx, httpc.JoinURL(b.baseURL, "/audio/speech"), b.authHeaders(), payload,
			)
			if err != nil {
				return err
			}
			if len(resp.Body) == 0 {
				// 宽松 shim 可能 200 + 空体；不落 0 字节文件、不计成功。该次合成已在供应商侧发生，
				// 重试等于再次计费，故直接抛错交由任务层失败（重生成廉价）。
				return errors.New("OpenAI 兼容语音合成返回空响应体")
			}
			audio = resp.Body
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return audio, nil
}

// authHeaders 构造鉴权头。
func (b *openAIAudioBackend) authHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + b.apiKey,
		"Content-Type":  "application/json",
	}
}
