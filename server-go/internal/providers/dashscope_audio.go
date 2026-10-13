package providers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// DashScopeAudioBackend 是阿里百炼 Qwen3-TTS 语音合成后端（同步）。
//
// 走原生 multimodal-generation/generation 同步端点：请求体 input 直接携带 text / voice /
// language_type（区别于图像的 messages 结构），响应在 output.audio.url 给出 wav 文件 URL
// （24kHz，24h 有效），再 HTTP GET 下载字节落盘。TTS 同步调用不带 X-DashScope-Async 头。

// DashScope 语音合成常量。
const (
	dashscopeAudioDefaultModel = "qwen3-tts-flash"

	dashscopeAudioEndpoint = "/services/aigc/multimodal-generation/generation"

	// dashscopeAudioRequestTimeout 是音频后端单次请求的墙钟预算（Python http_timeout 默认 60s）。
	dashscopeAudioRequestTimeout = 60 * time.Second
)

// dashscopeVoiceCatalog 是 Qwen3-TTS 系统预设音色（voice 参数取值）子集。
//
// label 存的是 lib/i18n 翻译 key（voice_label_dashscope_*），不是直出文案——端点固定硬编码中文
// 会让 en/vi 用户在下拉里看到中文描述。真正的本地化文案由路由层按请求 locale 渲染。
var dashscopeVoiceCatalog = []provider.VoiceOption{
	{ID: "Cherry", Label: "voice_label_dashscope_cherry"},
	{ID: "Serena", Label: "voice_label_dashscope_serena"},
	{ID: "Ethan", Label: "voice_label_dashscope_ethan"},
	{ID: "Chelsie", Label: "voice_label_dashscope_chelsie"},
	{ID: "Nofish", Label: "voice_label_dashscope_nofish"},
	{ID: "Jennifer", Label: "voice_label_dashscope_jennifer"},
	{ID: "Ryan", Label: "voice_label_dashscope_ryan"},
	{ID: "Bellona", Label: "voice_label_dashscope_bellona"},
	{ID: "Neil", Label: "voice_label_dashscope_neil"},
	{ID: "Elias", Label: "voice_label_dashscope_elias"},
	{ID: "Momo", Label: "voice_label_dashscope_momo"},
	{ID: "Vivian", Label: "voice_label_dashscope_vivian"},
	{ID: "Moon", Label: "voice_label_dashscope_moon"},
	{ID: "Maia", Label: "voice_label_dashscope_maia"},
	{ID: "Kai", Label: "voice_label_dashscope_kai"},
	{ID: "Katerina", Label: "voice_label_dashscope_katerina"},
	{ID: "Bella", Label: "voice_label_dashscope_bella"},
	{ID: "Eldric Sage", Label: "voice_label_dashscope_eldric_sage"},
	{ID: "Vincent", Label: "voice_label_dashscope_vincent"},
}

// dashscopeEmptyDownloadError 表示 200 但空响应体（瞬时代理 / CDN 异常），视为瞬态触发下载重试。
type dashscopeEmptyDownloadError struct{ url string }

func (e *dashscopeEmptyDownloadError) Error() string {
	return "DashScope 音频下载返回空内容: " + e.url
}

// dashscopeAudioBackend 是阿里百炼语音合成后端（同步 multimodal 端点）。
type dashscopeAudioBackend struct {
	apiKey  string
	baseURL string
	model   string
	client  *httpc.Client
}

// newDashscopeAudioBackend 按通用信封构造；model / base_url 留空时由本后端兜底。
func newDashscopeAudioBackend(opts provider.BackendOptions) (*dashscopeAudioBackend, error) {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = dashscopeAudioDefaultModel
	}
	return &dashscopeAudioBackend{
		apiKey:  opts.APIKey,
		baseURL: dashscopeNativeBaseURL(opts.BaseURL),
		model:   model,
		client:  httpc.NewClient(dashscopeAudioRequestTimeout),
	}, nil
}

// Name 返回供应商标识。
func (b *dashscopeAudioBackend) Name() string { return dashscopeProviderName }

// Model 返回实际下发的模型名。
func (b *dashscopeAudioBackend) Model() string { return b.model }

// Capabilities 返回本实例的能力位：仅文本转语音。
func (b *dashscopeAudioBackend) Capabilities() provider.AudioCapabilitySet {
	return provider.NewAudioCapabilitySet(provider.CapTextToSpeech)
}

// ListVoices 返回音色目录（不做网络请求）。
func (b *dashscopeAudioBackend) ListVoices() []provider.VoiceOption {
	voices := make([]provider.VoiceOption, len(dashscopeVoiceCatalog))
	copy(voices, dashscopeVoiceCatalog)
	return voices
}

// Synthesize 合成语音并落盘到 req.OutputPath。
//
// 合成（计费）与下载分两段独立重试：下载瞬时失败只重试 GET，绝不回头重跑会再次计费的合成 POST。
func (b *dashscopeAudioBackend) Synthesize(
	ctx context.Context, req provider.AudioSynthesisRequest,
) (provider.AudioSynthesisResult, error) {
	if req.Speed != nil {
		// 同步 qwen3-tts-flash 不支持 speech_rate（仅 realtime WebSocket 版可用），忽略。
		slog.Debug("DashScope 同步 TTS 不支持语速参数，已忽略", "speed", *req.Speed)
	}
	url, err := b.requestSynthesis(ctx, req)
	if err != nil {
		return provider.AudioSynthesisResult{}, err
	}
	if err := b.downloadAudio(ctx, url, req.OutputPath); err != nil {
		return provider.AudioSynthesisResult{}, err
	}
	slog.Info("DashScope 语音合成完成", "output", req.OutputPath)

	return provider.AudioSynthesisResult{
		Provider:   dashscopeProviderName,
		Model:      b.model,
		Characters: utf8.RuneCountInString(req.Text),
		OutputPath: req.OutputPath,
	}, nil
}

// requestSynthesis 提交合成请求（计费段），返回 output.audio.url。
func (b *dashscopeAudioBackend) requestSynthesis(
	ctx context.Context, req provider.AudioSynthesisRequest,
) (string, error) {
	payload := map[string]any{
		"model": b.model,
		"input": map[string]any{
			"text":          req.Text,
			"voice":         req.Voice,
			"language_type": req.LanguageType,
		},
	}
	slog.Info("调用 DashScope 语音合成 API", "model", b.model, "voice", req.Voice, "language", req.LanguageType)

	// 合成是非幂等的「计费」POST：SubmitPost 把歧义传输错误转 AmbiguousSubmitError 终态失败避免重复
	// 计费；HTTP 状态错误交 SubmitRetryable 按状态码分流——4xx fail-fast、5xx/429 重试。
	var url string
	err := provider.RunWithRetry(ctx, provider.DefaultRetryPolicy, provider.SubmitRetryable,
		func(ctx context.Context) error {
			resp, err := SubmitPost(ctx, dashscopeProviderName, func(ctx context.Context) (*httpc.Response, error) {
				return b.client.PostJSON(ctx, b.baseURL+dashscopeAudioEndpoint, dashscopeHeaders(b.apiKey, false), payload)
			})
			if err != nil {
				return err
			}
			u, err := dashscopeExtractAudioURL(resp.Body)
			if err != nil {
				return err
			}
			url = u
			return nil
		})
	if err != nil {
		return "", err
	}
	return url, nil
}

// downloadAudio 下载合成音频（非计费段，可独立多次重试）。
//
// 下载是幂等 GET：HTTP 状态错误按状态码闸门（DownloadRetryable，4xx 含 404 一律 fail-fast），
// 5xx / 传输 / 网络错误重试；200-空体（dashscopeEmptyDownloadError）属瞬态另行重试。
func (b *dashscopeAudioBackend) downloadAudio(ctx context.Context, rawURL, outputPath string) error {
	// 日志与异常只带去掉 query 的 URL：预签名参数在有效期内等同下载凭证。
	safeURL := strings.SplitN(rawURL, "?", 2)[0]
	return provider.RunWithRetry(ctx, provider.DownloadRetryPolicy, func(err error) bool {
		var empty *dashscopeEmptyDownloadError
		if errors.As(err, &empty) {
			return true
		}
		return provider.DownloadRetryable(err)
	}, func(ctx context.Context) error {
		resp, err := b.client.Get(ctx, rawURL, nil)
		if err != nil {
			return err
		}
		if len(resp.Body) == 0 {
			// 200 但空体：不写 0 字节 wav。
			return &dashscopeEmptyDownloadError{url: safeURL}
		}
		return writeFileAtomic(outputPath, resp.Body)
	})
}

// dashscopeExtractAudioURL 从同步语音合成响应 output.audio.url 提取音频文件 URL（wav，24h 有效）。
func dashscopeExtractAudioURL(body []byte) (string, error) {
	var payload map[string]any
	if err := decodeJSON(body, &payload); err != nil {
		return "", err
	}
	output, _ := payload["output"].(map[string]any)
	audio, _ := output["audio"].(map[string]any)
	url, _ := audio["url"].(string)
	if url == "" {
		if reason := dashscopeFailureReason(payload); reason != "" {
			return "", errors.New(reason)
		}
		return "", fmt.Errorf("DashScope 语音合成响应缺少 audio.url: %v", payload)
	}
	return url, nil
}

func init() {
	provider.RegisterAudioBackend(dashscopeProviderName, provider.AudioBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.AudioBackend, error) {
			return newDashscopeAudioBackend(opts)
		},
	})
}