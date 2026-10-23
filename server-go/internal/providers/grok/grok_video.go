package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/pricing"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/providers/provkit"
)

// Grok（xAI）视频生成后端。对齐 Python lib/video_backends/grok.py：先 POST {base}/videos/generations
// 拿到 request_id，再轮询 GET {base}/videos/{request_id} 直到 done/expired/failed，最后下载成片。
// 这是一条「提交-轮询-下载」流程，但供应商侧不可续跑（同步型 API），故 ResumeVideo 直接返回不支持。

const (
	grokDefaultVideoModel = "grok-imagine-video"

	// grokVideoRequestTimeout 提交与轮询单次请求的墙钟预算（产物下载另走 provkit.DownloadVideo 自带重试）。
	grokVideoRequestTimeout = 120 * time.Second

	// grokVideoPollInterval / grokVideoMaxWait 与 Python 传给 SDK 的 interval / timeout 一致。
	grokVideoPollInterval = 5 * time.Second
	grokVideoMaxWait      = 15 * time.Minute
)

// grokVideoBackend 是 xAI 视频生成后端。
type grokVideoBackend struct {
	model   string
	baseURL string
	apiKey  string
	client  *httpc.Client
}

// newGrokVideoBackend 按通用信封构造；model / base_url 留空时由本后端兜底（默认 model、官方端点）。
func newGrokVideoBackend(opts provider.BackendOptions) (*grokVideoBackend, error) {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = grokDefaultVideoModel
	}
	baseURL := strings.TrimSpace(opts.BaseURL)
	if baseURL == "" {
		baseURL = grokDefaultBaseURL
	}
	return &grokVideoBackend{
		model:   model,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  opts.APIKey,
		client:  httpc.NewClient(grokVideoRequestTimeout),
	}, nil
}

// Name 返回供应商标识。
func (b *grokVideoBackend) Name() string { return grokProviderID }

// Model 返回实际下发的模型名。
func (b *grokVideoBackend) Model() string { return b.model }

// VideoCapabilities 返回本实例在自身 model 下的能力声明。
func (b *grokVideoBackend) VideoCapabilities() provider.VideoCapabilities {
	caps, _ := grokVideoCapabilitiesForModel(b.model)
	return caps
}

// grokVideoCapabilitiesForModel 按 model_id 纯计算能力声明：不构造客户端、无需 api_key。
// 当前全系模型能力一致，不按 model_id 分支；参考图上限取自第三方来源，官方文档未明确列出。
func grokVideoCapabilitiesForModel(modelID string) (provider.VideoCapabilities, error) {
	caps := provider.DefaultVideoCapabilities()
	caps.MaxReferenceImages = 7
	return caps, nil
}

// ResumeVideo 恒不支持：Grok 为同步型 API，无 job_id 可接续，orphan 处理据此走 [resume_unsupported]。
func (b *grokVideoBackend) ResumeVideo(
	ctx context.Context, jobID string, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	return provider.VideoGenerationResult{}, provider.ErrResumeUnsupported
}

// grokVideoStartResponse 是提交生成请求的响应：仅需 request_id。
type grokVideoStartResponse struct {
	RequestID string `json:"request_id"`
}

// grokVideoStatusResponse 是轮询状态的响应形状。
type grokVideoStatusResponse struct {
	Status string `json:"status"`
	Video  *struct {
		URL      string          `json:"url"`
		Duration json.RawMessage `json:"duration"`
	} `json:"video"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// grokVideoPoll 是轮询结果的内部收敛形态。
type grokVideoPoll struct {
	status      string
	url         string
	duration    json.RawMessage
	failMessage string
}

// Generate 生成视频：提交 → 轮询 → 下载，下载与生成分离，避免下载失败重跑生成浪费额度。
func (b *grokVideoBackend) Generate(
	ctx context.Context, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	payload, err := b.buildPayload(req)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}

	var submitted *httpc.Response
	// 创建即计费，按「请求是否确定送达」分流：连接类失败可重试，歧义态终态失败不重复计费。
	err = provider.RunWithRetry(ctx, provider.DefaultRetryPolicy, provider.SubmitRetryable, func(ctx context.Context) error {
		created, err := provkit.SubmitPost(ctx, grokProviderID, func(ctx context.Context) (*httpc.Response, error) {
			return b.client.PostJSON(ctx, httpc.JoinURL(b.baseURL, "/videos/generations"), b.authHeaders(), payload)
		})
		if err != nil {
			return err
		}
		submitted = created
		return nil
	})
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}

	var start grokVideoStartResponse
	if err := provkit.DecodeJSON(submitted.Body, &start); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	if strings.TrimSpace(start.RequestID) == "" {
		return provider.VideoGenerationResult{}, &provider.NonRetryableError{
			Err: fmt.Errorf("grok video submit returned no request_id (model=%s)", b.model),
		}
	}

	statusURL := httpc.JoinURL(b.baseURL, "/videos/"+start.RequestID)
	final, err := provkit.PollWithRetry(ctx, provkit.PollConfig[grokVideoPoll]{
		PollFn: func(ctx context.Context) (grokVideoPoll, error) {
			return b.pollStatus(ctx, statusURL, start.RequestID)
		},
		IsDone:       func(p grokVideoPoll) bool { return p.status == "done" },
		IsFailed:     func(p grokVideoPoll) string { return p.failMessage },
		PollInterval: grokVideoPollInterval,
		MaxWait:      grokVideoMaxWait,
		Label:        "Grok",
	})
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	if strings.TrimSpace(final.url) == "" {
		return provider.VideoGenerationResult{}, fmt.Errorf("grok video finished without a url (request_id=%s)", start.RequestID)
	}

	if err := provkit.DownloadVideo(ctx, final.url, req.OutputPath, 0); err != nil {
		return provider.VideoGenerationResult{}, err
	}

	generateAudio := true
	return provider.VideoGenerationResult{
		VideoPath:       req.OutputPath,
		Provider:        grokProviderID,
		Model:           b.model,
		DurationSeconds: grokBilledDurationSeconds(final.duration, req.DurationSeconds),
		VideoURI:        final.url,
		GenerateAudio:   &generateAudio,
	}, nil
}

// buildPayload 组装提交请求体；首帧与参考图存在时转 base64 data URI 一并下发。
func (b *grokVideoBackend) buildPayload(req provider.VideoGenerationRequest) (map[string]any, error) {
	payload := map[string]any{
		"prompt":       req.Prompt,
		"model":        b.model,
		"duration":     req.DurationSeconds,
		"aspect_ratio": req.AspectRatio,
	}
	if req.Resolution != "" {
		payload["resolution"] = req.Resolution
	}
	if req.StartImage != "" {
		if _, err := os.Stat(req.StartImage); err == nil {
			uri, err := provkit.ImageToBase64DataURI(req.StartImage)
			if err != nil {
				return nil, err
			}
			payload["image"] = map[string]string{"url": uri}
		}
	}
	if refs := grokVideoReferenceEntries(req.ReferenceImages); len(refs) > 0 {
		payload["reference_images"] = refs
	}
	return payload, nil
}

// grokVideoReferenceEntries 把存在的参考图转成 {url: data URI} 条目；打不开的跳过。
func grokVideoReferenceEntries(paths []string) []map[string]string {
	if len(paths) == 0 {
		return nil
	}
	entries := make([]map[string]string, 0, len(paths))
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		uri, err := provkit.ImageToBase64DataURI(path)
		if err != nil {
			continue
		}
		entries = append(entries, map[string]string{"url": uri})
	}
	return entries
}

// pollStatus 查询一次任务状态；expired / failed 收敛成终态失败信息。
func (b *grokVideoBackend) pollStatus(ctx context.Context, statusURL, requestID string) (grokVideoPoll, error) {
	resp, err := b.client.Get(ctx, statusURL, b.authHeaders())
	if err != nil {
		return grokVideoPoll{}, err
	}
	var parsed grokVideoStatusResponse
	if err := provkit.DecodeJSON(resp.Body, &parsed); err != nil {
		return grokVideoPoll{}, err
	}
	poll := grokVideoPoll{status: parsed.Status}
	if parsed.Video != nil {
		poll.url = parsed.Video.URL
		poll.duration = parsed.Video.Duration
	}
	switch parsed.Status {
	case "expired":
		poll.failMessage = fmt.Sprintf("Grok 视频请求已过期（request_id=%s）", requestID)
	case "failed":
		message := "Grok 视频生成失败"
		if parsed.Error != nil && parsed.Error.Message != "" {
			message = "Grok 视频生成失败: " + parsed.Error.Message
		}
		poll.failMessage = message
	}
	return poll, nil
}

// authHeaders 构造鉴权头。
func (b *grokVideoBackend) authHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + b.apiKey}
}

// grokBilledDurationSeconds 从供应商回报的 duration 推导计费时长，无法采用时回落请求时长。
//
// 接收 "15.0" 这类浮点字符串与数字；缺失 / 不可解析（含 inf/nan）/ 非正 / 超上限一律回落。
// 上下限基于取整前的原始数值判断（86400.9 已超 24h，不得因取整落回上限内）。half-up 取整避免
// 截断少计费秒数；(0, 0.5) 取整到 0 时同样回落，保证结果恒为正。
func grokBilledDurationSeconds(raw json.RawMessage, fallback int) int {
	if len(raw) == 0 || string(raw) == "null" {
		return fallback
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return fallback
		}
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return fallback
		}
		value = parsed
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fallback
	}
	if value <= 0 || value > float64(pricing.MaxBilledDurationSeconds) {
		return fallback
	}
	if rounded := int(value + 0.5); rounded > 0 {
		return rounded
	}
	return fallback
}

func init() {
	provider.RegisterVideoBackend(grokProviderID, provider.VideoBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.VideoBackend, error) {
			return newGrokVideoBackend(opts)
		},
		CapabilitiesForModel: grokVideoCapabilitiesForModel,
	})
}
