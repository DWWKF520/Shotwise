package providers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// V2VideoGenerationsBackend —— 通用「流派 C」/v2/video/generations 视频后端
// （端口自 lib/video_backends/v2_video_generations.py）。
//
// 对接「单端点 + model 字段切换」的中转协议（aimlapi / xAI / getimg.ai / APIMart / CometAPI
// 等事实标准）：POST /v2/video/generations 提交拿 generation_id，GET /v2/video/generations
// ?generation_id={id} 轮询，终态后取视频 URL 下载。
//
// 只编码一份通用 canonical I/O 契约 + 多路径容错解析（不背 per-model schema）。本后端不注册，
// 仅作自定义供应商声明式端点的 delegate 使用。

const (
	// v2VideoProvider 是结果里的 provider 标识。
	v2VideoProvider = "v2-video-generations"
	// v2VideoSubmitPath 是提交与轮询共用的端点路径。
	v2VideoSubmitPath = "/v2/video/generations"

	v2VideoPollInterval         = 5 * time.Second
	v2VideoMinPollTimeout       = 600 * time.Second
	v2VideoPollTimeoutPerSecond = 30 * time.Second
	// v2VideoDefaultHTTPTimeout 是构造时未声明 http_timeout 的兜底单次请求超时。
	v2VideoDefaultHTTPTimeout = 60 * time.Second
	// v2VideoDefaultMaxReferenceImages：generic 端点跨多模型，参考图上限无单一供应商真相；
	// 取保守默认，由 resolver 在 endpoint cap 未声明时 fallthrough 读取。
	v2VideoDefaultMaxReferenceImages = 4
	// v2VideoLargeImageWarnBytes：超过此阈值的图触发 warning（base64 编码后易触发中转站请求体上限）。
	v2VideoLargeImageWarnBytes = 4 * 1024 * 1024
	// v2VideoPromptLogMax 是日志摘要里 prompt 的截断长度。
	v2VideoPromptLogMax = 200
)

// v2VideoStatusSynonyms 状态串 → canonical（lowercase 后查表）。覆盖 aimlapi 官方枚举
// queued/generating/completed/error，并并入跨厂商同义词。
var v2VideoStatusSynonyms = map[string]string{
	"completed":   "succeeded",
	"succeeded":   "succeeded",
	"succeed":     "succeeded",
	"success":     "succeeded",
	"failed":      "failed",
	"fail":        "failed",
	"error":       "failed",
	"expired":     "failed",
	"canceled":    "failed",
	"cancelled":   "failed",
	"generating":  "running",
	"in_progress": "running",
	"running":     "running",
	"processing":  "running",
	"queued":      "queued",
	"queueing":    "queued",
	"preparing":   "queued",
	"submitted":   "queued",
	"pending":     "queued",
	"created":     "queued",
}

// 多路径优先级表，配 v2Dig 逐层走 dict key / list 下标（int 段表 list 下标）。
var (
	v2VideoURLPaths = [][]any{
		{"video", "url"},
		{"assets", "video"},
		{"output", "video_url"},
		{"content", "video_url"},
		{"data", "task_result", "videos", 0, "url"},
		{"url"},
	}
	v2VideoTaskIDPaths = [][]any{
		{"generation_id"},
		{"id"},
		{"task_id"},
		{"data", "task_id"},
		{"request_id"},
		{"data", "taskId"},
	}
	v2VideoStatusPaths = [][]any{
		{"status"},
		{"state"},
		{"data", "status"},
		{"data", "state"},
		{"output", "status"},
	}
)

// v2VideoVersionSuffixRe 匹配结尾的版本段（/v1、/v2、/v2beta、/v2.1 等）。
var v2VideoVersionSuffixRe = regexp.MustCompile(`/v\d+(?:\.\d+)?[a-zA-Z]*$`)

// v2Dig 按 path 逐层走 dict key / list 下标，任一层缺失返回 nil。
func v2Dig(payload any, path []any) any {
	cur := payload
	for _, seg := range path {
		switch key := seg.(type) {
		case int:
			arr, ok := cur.([]any)
			if !ok || key >= len(arr) {
				return nil
			}
			cur = arr[key]
		case string:
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			value, ok := obj[key]
			if !ok {
				return nil
			}
			cur = value
		default:
			return nil
		}
	}
	return cur
}

// v2FirstStrByPaths 按优先级逐个试取第一个非空字符串值（整数容忍并 str 化）。
func v2FirstStrByPaths(payload any, paths [][]any) string {
	for _, path := range paths {
		switch value := v2Dig(payload, path).(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		case int:
			return strconv.Itoa(value)
		case int64:
			return strconv.FormatInt(value, 10)
		case float64:
			// JSON 数字解码为 float64：仅整数值按 Python int 的 str 形态返回，非整数跳过。
			if value == math.Trunc(value) {
				return strconv.FormatInt(int64(value), 10)
			}
		}
	}
	return ""
}

// v2NormalizeStatus 把 raw 状态值归一成 canonical：queued | running | succeeded | failed。
// 未知串当 running。
func v2NormalizeStatus(raw any) string {
	status, ok := raw.(string)
	if !ok {
		return "running"
	}
	if canonical, ok := v2VideoStatusSynonyms[strings.ToLower(strings.TrimSpace(status))]; ok {
		return canonical
	}
	return "running"
}

// v2WarnIfLarge 图片过大时告警（base64 编码后易触发中转站请求体上限）。
func v2WarnIfLarge(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Size() > v2VideoLargeImageWarnBytes {
		slog.Warn("图片较大，base64 编码后可能触发中转站请求体上限",
			"size_mb", float64(info.Size())/1024/1024, "path", path)
	}
}

// v2VideoFileExists 判断本地路径是否存在。
func v2VideoFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// v2VideoBuildRequestBody 按流派 C canonical 拼请求体；缺省字段一律省略。
//
// 图像走 base64 data URI 内嵌（与 newapi 一致）：首帧 image_url、尾帧 last_image_url、参考数组
// image_urls。画幅恒有值（默认 9:16），表达项目朝向意图；漏发会让竖屏项目在按 aspect_ratio
// 出片的供应商上变横屏。
func v2VideoBuildRequestBody(model string, req provider.VideoGenerationRequest) (map[string]any, error) {
	body := map[string]any{"model": model, "prompt": req.Prompt, "duration": req.DurationSeconds}
	if req.AspectRatio != "" {
		body["aspect_ratio"] = req.AspectRatio
	}
	if req.Resolution != "" {
		body["resolution"] = req.Resolution
	}
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}
	if req.StartImage != "" {
		if v2VideoFileExists(req.StartImage) {
			v2WarnIfLarge(req.StartImage)
			dataURI, err := imageToBase64DataURI(req.StartImage)
			if err != nil {
				return nil, err
			}
			body["image_url"] = dataURI
		} else {
			slog.Warn("start_image 文件不存在，已忽略", "path", req.StartImage)
		}
	}
	if req.EndImage != "" {
		if v2VideoFileExists(req.EndImage) {
			v2WarnIfLarge(req.EndImage)
			dataURI, err := imageToBase64DataURI(req.EndImage)
			if err != nil {
				return nil, err
			}
			body["last_image_url"] = dataURI
		} else {
			slog.Warn("end_image 文件不存在，已忽略", "path", req.EndImage)
		}
	}
	if len(req.ReferenceImages) > 0 {
		refs := make([]string, 0, len(req.ReferenceImages))
		for _, ref := range req.ReferenceImages {
			if v2VideoFileExists(ref) {
				v2WarnIfLarge(ref)
				dataURI, err := imageToBase64DataURI(ref)
				if err != nil {
					return nil, err
				}
				refs = append(refs, dataURI)
			} else {
				slog.Warn("reference_image 文件不存在，已忽略", "path", ref)
			}
		}
		if len(refs) > 0 {
			body["image_urls"] = refs
		}
	}
	return body, nil
}

// v2VideoNormalizeRoot 归一化为 root 形态：补协议 + 去尾斜杠 + 去末尾版本段（/v1、/v2beta 等）。
//
// 后续显式拼 /v2/video/generations。无 scheme 的纯域名先补 https://，否则 httpx / net/http 拒收
// 缺协议的相对 URL。
func v2VideoNormalizeRoot(baseURL string) string {
	s := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if s != "" && !strings.Contains(s, "://") {
		s = "https://" + s
	}
	return v2VideoVersionSuffixRe.ReplaceAllString(s, "")
}

// v2VideoExtractFailure 把 failed 状态渲染成失败消息；非 failed 返回空串。
func v2VideoExtractFailure(state map[string]any) string {
	if v2NormalizeStatus(v2FirstStrByPaths(state, v2VideoStatusPaths)) != "failed" {
		return ""
	}
	msg := "unknown"
	switch errValue := v2Dig(state, []any{"error"}).(type) {
	case map[string]any:
		if message, ok := errValue["message"].(string); ok && message != "" {
			msg = message
		} else if name, ok := errValue["name"].(string); ok && name != "" {
			msg = name
		}
	case string:
		if strings.TrimSpace(errValue) != "" {
			msg = errValue
		}
	}
	return "V2 视频生成失败: " + msg
}

// v2VideoGenerationsCapabilitiesForModel 按 model_id 纯计算 caps —— 不构造 client。
//
// 保留 model 形参仅为跨 backend 接口统一；generic 端点跨多模型无单一供应商真相，取保守默认。
func v2VideoGenerationsCapabilitiesForModel(model string) provider.VideoCapabilities {
	caps := provider.DefaultVideoCapabilities()
	caps.LastFrame = true
	caps.MaxReferenceImages = v2VideoDefaultMaxReferenceImages
	return caps
}

// v2VideoGenerationsBackend 是流派 C 通用视频后端实例。
type v2VideoGenerationsBackend struct {
	model   string
	root    string
	apiKey  string
	client  *httpc.Client
	opts    provider.BackendOptions
}

// newV2VideoGenerationsBackend 按通用信封构造；api_key / base_url / model 均必填。
//
// 不注册，仅作自定义供应商声明式端点的 delegate 构造闭包使用。
func newV2VideoGenerationsBackend(opts provider.BackendOptions) (provider.VideoBackend, error) {
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, errors.New("V2VideoGenerationsBackend 需要 api_key")
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("V2VideoGenerationsBackend 需要 base_url")
	}
	if strings.TrimSpace(opts.Model) == "" {
		return nil, errors.New("V2VideoGenerationsBackend 需要 model")
	}
	timeout := v2VideoDefaultHTTPTimeout
	if raw := strings.TrimSpace(opts.Extra["http_timeout"]); raw != "" {
		if seconds, err := strconv.ParseFloat(raw, 64); err == nil && seconds > 0 {
			timeout = time.Duration(seconds * float64(time.Second))
		}
	}
	return &v2VideoGenerationsBackend{
		model:  strings.TrimSpace(opts.Model),
		root:   v2VideoNormalizeRoot(opts.BaseURL),
		apiKey: opts.APIKey,
		client: httpc.NewClient(timeout),
		opts:   opts,
	}, nil
}

// Name 返回供应商标识。
func (b *v2VideoGenerationsBackend) Name() string { return v2VideoProvider }

// Model 返回实际下发的模型名。
func (b *v2VideoGenerationsBackend) Model() string { return b.model }

// VideoCapabilities 返回本 model 的能力声明（委托给纯函数，保持单一真相源）。
func (b *v2VideoGenerationsBackend) VideoCapabilities() provider.VideoCapabilities {
	return v2VideoGenerationsCapabilitiesForModel(b.model)
}

// headers 构造鉴权头。
func (b *v2VideoGenerationsBackend) headers() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + b.apiKey,
		"Content-Type":  "application/json",
	}
}

// Generate 生成视频。
func (b *v2VideoGenerationsBackend) Generate(
	ctx context.Context, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	body, err := v2VideoBuildRequestBody(b.model, req)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	slog.Info("V2 视频生成开始", "model", b.model, "duration", req.DurationSeconds)

	generationID, err := b.createTask(ctx, body)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	slog.Info("V2 任务创建", "generation_id", generationID)
	if err := PersistProviderJobIDFor(ctx, b.opts, req, generationID, b.root); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	return b.pollAndBuild(ctx, generationID, req, false)
}

// ResumeVideo 接续已 submit 的 V2 task：仅 poll + 下载（generation_id 已持久化）。
func (b *v2VideoGenerationsBackend) ResumeVideo(
	ctx context.Context, jobID string, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	return b.pollAndBuild(ctx, jobID, req, true)
}

// createTask 提交生成请求取 generation_id（带提交重试保护）。
func (b *v2VideoGenerationsBackend) createTask(ctx context.Context, body map[string]any) (string, error) {
	var generationID string
	err := provider.RunWithRetry(
		ctx, provider.DefaultRetryPolicy, provider.SubmitRetryable,
		func(ctx context.Context) error {
			resp, err := SubmitPost(ctx, v2VideoProvider, func(ctx context.Context) (*httpc.Response, error) {
				return b.client.PostJSON(ctx, b.root+v2VideoSubmitPath, b.headers(), body)
			})
			if err != nil {
				return err
			}
			var payload any
			if err := decodeJSON(resp.Body, &payload); err != nil {
				return err
			}
			id := v2FirstStrByPaths(payload, v2VideoTaskIDPaths)
			if strings.TrimSpace(id) == "" {
				return &provider.NonRetryableError{
					Err: fmt.Errorf("V2 创建任务返回体未能从已知路径提取 task_id: %v", payload),
				}
			}
			generationID = id
			return nil
		},
	)
	return generationID, err
}

// pollOnce 查询任务状态；resume 路径下 404 直接转 ResumeExpiredError。
//
// 轮询 404 在 should_retry_poll 里被当作「短暂未就绪」重试，对已过期的 resume 任务会一直重到
// max_wait 超时、永不落 [resume_expired]，故在此一击转终态异常。非 resume 的 4xx 重新抛出，
// 由 PollRetryable 按 status_code 分流。
func (b *v2VideoGenerationsBackend) pollOnce(
	ctx context.Context, generationID string, isResume bool,
) (map[string]any, error) {
	resp, err := b.client.Get(
		ctx, b.root+v2VideoSubmitPath+"?generation_id="+url.QueryEscape(generationID), b.headers(),
	)
	if err != nil {
		if isResume {
			var statusErr *provider.HTTPStatusError
			if errors.As(err, &statusErr) && statusErr.StatusCode == 404 {
				return nil, &provider.ResumeExpiredError{JobID: generationID, Provider: v2VideoProvider}
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

// pollAndBuild 轮询任务至 succeeded 并下载成片。
func (b *v2VideoGenerationsBackend) pollAndBuild(
	ctx context.Context, generationID string, req provider.VideoGenerationRequest, isResume bool,
) (provider.VideoGenerationResult, error) {
	final, err := PollWithRetry(ctx, PollConfig[map[string]any]{
		PollFn: func(ctx context.Context) (map[string]any, error) {
			return b.pollOnce(ctx, generationID, isResume)
		},
		IsDone: func(state map[string]any) bool {
			return v2NormalizeStatus(v2FirstStrByPaths(state, v2VideoStatusPaths)) == "succeeded"
		},
		IsFailed:     v2VideoExtractFailure,
		PollInterval: v2VideoPollInterval,
		MaxWait:      b.maxWait(req.DurationSeconds),
		Label:        "V2",
	})
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}

	videoURL := v2FirstStrByPaths(final, v2VideoURLPaths)
	if strings.TrimSpace(videoURL) == "" {
		return provider.VideoGenerationResult{}, fmt.Errorf("V2 任务完成但未能从已知路径提取视频 URL: %v", final)
	}
	if err := DownloadVideo(ctx, videoURL, req.OutputPath, 0); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	return provider.VideoGenerationResult{
		VideoPath:       req.OutputPath,
		Provider:        v2VideoProvider,
		Model:           b.model,
		DurationSeconds: req.DurationSeconds,
		VideoURI:        videoURL,
		Seed:            req.Seed,
		TaskID:          generationID,
	}, nil
}

// maxWait 按输出时长线性放宽轮询总时限，下限 600 秒。
func (b *v2VideoGenerationsBackend) maxWait(durationSeconds int) time.Duration {
	maxWait := v2VideoMinPollTimeout
	if linear := time.Duration(durationSeconds) * v2VideoPollTimeoutPerSecond; linear > maxWait {
		maxWait = linear
	}
	return maxWait
}