package providers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// NewAPIVideoBackend —— NewAPI 统一视频生成端点后端（端口自 lib/video_backends/newapi.py）。
//
// 对接 NewAPI 的 /video/generations 接口，支持 Sora / Kling / 即梦 / Wan / Veo 等多家厂商模型，
// 靠请求体的 model 字段分发。submit 走 SubmitPost（歧义态终态失败，避免重复建任务 + 重复计费）。

const (
	// newAPIVideoProvider 与 registry 的 provider_id 同名。
	newAPIVideoProvider = "newapi"
	// newAPIVideoDefaultModel 是 model 缺省时的型号。
	newAPIVideoDefaultModel = "kling-v1"

	newAPIVideoPollInterval         = 5 * time.Second
	newAPIVideoMinPollTimeout       = 600 * time.Second
	newAPIVideoPollTimeoutPerSecond = 30 * time.Second
	// newAPIVideoDefaultHTTPTimeout 是构造时未声明 http_timeout 的兜底单次请求超时。
	newAPIVideoDefaultHTTPTimeout = 60 * time.Second
)

// 超过此阈值的起始图会触发 warning，NewAPI 聚合后端常见 4MB 请求体上限。
const newAPIVideoLargeImageWarnBytes = 4 * 1024 * 1024

// 视频标准尺寸对齐 8 的倍数（1920x1080 / 1080x1920 等；1080 非 16 的倍数），主流模型通用。
const newAPIVideoRoundTo = 8

// newAPIVideoResolveSize：比例优先、清晰度其次。短边来自 resolution（档位 / 自定义 / None 兜底
// 720P），比例精确来自 aspect_ratio、对齐 8 的倍数。
func newAPIVideoResolveSize(resolution, aspectRatio string) (int, int) {
	short := project.ResolutionToShortEdge(resolution, project.VideoTierShortEdge, project.DefaultShortEdge)
	return project.AspectSize(aspectRatio, short, project.AspectSizeOptions{RoundTo: newAPIVideoRoundTo})
}

// newAPIVideoCapabilitiesForModel 按 model_id 纯计算 caps —— 不构造 client（无需 api_key）。
//
// 中转端点不接受参考图；当前全系模型能力一致，不按 model_id 分支。
func newAPIVideoCapabilitiesForModel(model string) provider.VideoCapabilities {
	return provider.DefaultVideoCapabilities()
}

// newAPIVideoFileExists 判断本地路径是否存在（起始图打不开即视为不存在）。
func newAPIVideoFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// newAPIVideoBackend 是 NewAPI 视频生成后端实例。
type newAPIVideoBackend struct {
	model   string
	baseURL string
	apiKey  string
	client  *httpc.Client
	opts    provider.BackendOptions
}

// newNewAPIVideoBackend 按通用信封构造；api_key / base_url 必填。
func newNewAPIVideoBackend(opts provider.BackendOptions) (*newAPIVideoBackend, error) {
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, errors.New("NewAPIVideoBackend 需要 api_key")
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("NewAPIVideoBackend 需要 base_url")
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = newAPIVideoDefaultModel
	}
	timeout := newAPIVideoDefaultHTTPTimeout
	if raw := strings.TrimSpace(opts.Extra["http_timeout"]); raw != "" {
		if seconds, err := strconv.ParseFloat(raw, 64); err == nil && seconds > 0 {
			timeout = time.Duration(seconds * float64(time.Second))
		}
	}
	return &newAPIVideoBackend{
		model:   model,
		baseURL: strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"),
		apiKey:  opts.APIKey,
		client:  httpc.NewClient(timeout),
		opts:    opts,
	}, nil
}

// Name 返回供应商标识。
func (b *newAPIVideoBackend) Name() string { return newAPIVideoProvider }

// Model 返回实际下发的模型名。
func (b *newAPIVideoBackend) Model() string { return b.model }

// VideoCapabilities 返回本 model 的能力声明（委托给纯函数，保持单一真相源）。
func (b *newAPIVideoBackend) VideoCapabilities() provider.VideoCapabilities {
	return newAPIVideoCapabilitiesForModel(b.model)
}

// headers 构造鉴权头。
func (b *newAPIVideoBackend) headers() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + b.apiKey,
		"Content-Type":  "application/json",
	}
}

// Generate 生成视频。
func (b *newAPIVideoBackend) Generate(
	ctx context.Context, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	width, height := newAPIVideoResolveSize(req.Resolution, req.AspectRatio)
	payload := map[string]any{
		"model":    b.model,
		"prompt":   req.Prompt,
		"width":    width,
		"height":   height,
		"duration": req.DurationSeconds,
		"n":        1,
	}
	if req.Seed != nil {
		payload["seed"] = *req.Seed
	}
	if req.StartImage != "" {
		if newAPIVideoFileExists(req.StartImage) {
			if info, err := os.Stat(req.StartImage); err == nil && info.Size() > newAPIVideoLargeImageWarnBytes {
				slog.Warn("NewAPI start_image 较大，Base64 编码后可能触发服务端请求体限制",
					"size_mb", float64(info.Size())/1024/1024)
			}
			dataURI, err := imageToBase64DataURI(req.StartImage)
			if err != nil {
				return provider.VideoGenerationResult{}, err
			}
			payload["image"] = dataURI
		} else {
			slog.Warn("start_image 文件不存在，已忽略", "path", req.StartImage)
		}
	}
	if len(req.ReferenceImages) > 0 {
		slog.Warn("NewAPIVideoBackend 不支持多张参考图，已忽略", "count", len(req.ReferenceImages))
	}

	slog.Info("NewAPI 视频生成开始", "model", b.model, "duration", req.DurationSeconds)

	taskID, err := b.createTask(ctx, payload)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	slog.Info("NewAPI 任务创建", "task_id", taskID)
	if err := PersistProviderJobIDFor(ctx, b.opts, req, taskID, b.baseURL); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	return b.pollAndBuild(ctx, taskID, req, false)
}

// ResumeVideo 接续已 submit 的 NewAPI task：仅 poll + 下载。
func (b *newAPIVideoBackend) ResumeVideo(
	ctx context.Context, jobID string, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	return b.pollAndBuild(ctx, jobID, req, true)
}

// createTask 提交生成请求取 task_id（带提交重试保护）。
func (b *newAPIVideoBackend) createTask(ctx context.Context, payload map[string]any) (string, error) {
	var taskID string
	err := provider.RunWithRetry(
		ctx, provider.DefaultRetryPolicy, provider.SubmitRetryable,
		func(ctx context.Context) error {
			resp, err := SubmitPost(ctx, newAPIVideoProvider, func(ctx context.Context) (*httpc.Response, error) {
				return b.client.PostJSON(
					ctx, httpc.JoinURL(b.baseURL, "/video/generations"), b.headers(), payload,
				)
			})
			if err != nil {
				return err
			}
			var body map[string]any
			if err := decodeJSON(resp.Body, &body); err != nil {
				return err
			}
			id, _ := body["task_id"].(string)
			if strings.TrimSpace(id) == "" {
				return &provider.NonRetryableError{
					Err: fmt.Errorf("NewAPI 创建任务返回体缺少 task_id: %v", body),
				}
			}
			taskID = id
			return nil
		},
	)
	return taskID, err
}

// pollOnce 查询任务状态；resume 路径下 404 直接转 ResumeExpiredError。
//
// 轮询 404 在 should_retry_poll 里被当作「短暂未就绪」重试，对已过期的 resume 任务会一直重到
// max_wait 超时、永不落 [resume_expired]，故在此一击转终态异常。非 resume 的 4xx 重新抛出，
// 由 PollRetryable 按 status_code 分流。
func (b *newAPIVideoBackend) pollOnce(
	ctx context.Context, taskID string, isResume bool,
) (map[string]any, error) {
	resp, err := b.client.Get(ctx, httpc.JoinURL(b.baseURL, "/video/generations/"+taskID), b.headers())
	if err != nil {
		if isResume {
			var statusErr *provider.HTTPStatusError
			if errors.As(err, &statusErr) && statusErr.StatusCode == 404 {
				return nil, &provider.ResumeExpiredError{JobID: taskID, Provider: newAPIVideoProvider}
			}
		}
		return nil, err
	}
	var state map[string]any
	if err := decodeJSON(resp.Body, &state); err != nil {
		return nil, err
	}
	return state, nil
}

// pollAndBuild 轮询任务至终态并下载成片。
func (b *newAPIVideoBackend) pollAndBuild(
	ctx context.Context, taskID string, req provider.VideoGenerationRequest, isResume bool,
) (provider.VideoGenerationResult, error) {
	// IsDone 纯谓词：completed / failed / expired 均视为终态；caller 按 isResume 决定 expired 抛
	// RuntimeError（generate）还是 ResumeExpiredError（resume）。IsFailed 只识别 failed。
	final, err := PollWithRetry(ctx, PollConfig[map[string]any]{
		PollFn: func(ctx context.Context) (map[string]any, error) {
			return b.pollOnce(ctx, taskID, isResume)
		},
		IsDone: func(state map[string]any) bool {
			status, _ := state["status"].(string)
			return status == "completed" || status == "failed" || status == "expired"
		},
		IsFailed:     newAPIVideoExtractFailure,
		PollInterval: newAPIVideoPollInterval,
		MaxWait:      b.maxWait(req.DurationSeconds),
		Label:        "NewAPI",
	})
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}

	status, _ := final["status"].(string)
	if status == "expired" {
		if isResume {
			return provider.VideoGenerationResult{}, &provider.ResumeExpiredError{
				JobID:    taskID,
				Provider: newAPIVideoProvider,
				Message:  fmt.Sprintf("NewAPI task expired: %s", taskID),
			}
		}
		return provider.VideoGenerationResult{}, fmt.Errorf("NewAPI task expired during generate: %s", taskID)
	}

	videoURL, _ := final["url"].(string)
	if strings.TrimSpace(videoURL) == "" {
		return provider.VideoGenerationResult{}, fmt.Errorf("NewAPI 任务完成但缺少 url 字段: %v", final)
	}
	// 流式下载，不携带 Authorization 头（视频 URL 常为 CDN/OSS，避免 API Key 泄露）。
	if err := DownloadVideo(ctx, videoURL, req.OutputPath, 0); err != nil {
		return provider.VideoGenerationResult{}, err
	}

	meta, _ := final["metadata"].(map[string]any)
	duration := req.DurationSeconds
	if raw, ok := meta["duration"]; ok && raw != nil {
		if parsed, ok := newAPIVideoToFloat(raw); ok {
			duration = int(parsed)
		}
	}
	return provider.VideoGenerationResult{
		VideoPath:       req.OutputPath,
		Provider:        newAPIVideoProvider,
		Model:           b.model,
		DurationSeconds: duration,
		TaskID:          taskID,
		Seed:            newAPIVideoToInt64(meta["seed"]),
	}, nil
}

// maxWait 按输出时长线性放宽轮询总时限，下限 600 秒。
func (b *newAPIVideoBackend) maxWait(durationSeconds int) time.Duration {
	maxWait := newAPIVideoMinPollTimeout
	if linear := time.Duration(durationSeconds) * newAPIVideoPollTimeoutPerSecond; linear > maxWait {
		maxWait = linear
	}
	return maxWait
}

// newAPIVideoExtractFailure 把 failed 状态渲染成失败消息；非 failed 返回空串。
func newAPIVideoExtractFailure(state map[string]any) string {
	status, _ := state["status"].(string)
	if status != "failed" {
		return ""
	}
	errMsg := "unknown"
	if errObj, ok := state["error"].(map[string]any); ok {
		if message, ok := errObj["message"].(string); ok && message != "" {
			errMsg = message
		}
	}
	return "NewAPI 视频生成失败: " + errMsg
}

// newAPIVideoToFloat 把 JSON 数字 / 数字字符串转成 float64。
func newAPIVideoToFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

// newAPIVideoToInt64 把 JSON 数字 / 数字字符串转成 *int64；非数值返回 nil。
func newAPIVideoToInt64(value any) *int64 {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case float64:
		seed := int64(v)
		return &seed
	case int:
		seed := int64(v)
		return &seed
	case int64:
		seed := v
		return &seed
	case string:
		if parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return &parsed
		}
	}
	return nil
}

func init() {
	create := func(opts provider.BackendOptions) (provider.VideoBackend, error) {
		backend, err := newNewAPIVideoBackend(opts)
		if err != nil {
			return nil, err
		}
		return backend, nil
	}
	capsForModel := func(modelID string) (provider.VideoCapabilities, error) {
		return newAPIVideoCapabilitiesForModel(modelID), nil
	}
	provider.RegisterVideoBackend("newapi", provider.VideoBackendRegistration{
		Create:               create,
		CapabilitiesForModel: capsForModel,
	})
}
