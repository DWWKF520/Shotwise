package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/pricing"
	"github.com/shotwise/shotwise/internal/provider"
)

// Agnes 视频后端（端口自 lib/video_backends/agnes.py）：裸 base64 + 异步轮询 + resume。
//
// 走 apihub 网关上的 OpenAI 风格异步端点：submit POST /v1/videos（JSON）取 task_id →
// 轮询 GET /v1/videos/{task_id} 至 status=completed。成片 URL 分两级取：完成态响应自带直接
// URL 字段（url / video_url / metadata.url，或 remixed_from_video_id 恰好是 URL 形态）即直接
// 下载；只有 video_id 时以其向网关根下的成片查询端点 GET /agnesapi?video_id= 二次查询。
//
// 关键帧 / 多图映射：无图 → mode=text；首帧/尾帧 → mode=keyframe + first_frame / last_frame；
// 参考图 → mode=reference + 顶层 images。
const (
	agnesVideoDefaultModel = "agnes-video-2.5"

	agnesVideoVideosEndpoint = "/videos"
	// 成片查询端点挂在网关根（不在 /v1 下），按 video_id 查询。
	agnesVideoQueryEndpoint = "/agnesapi"

	// 后端防御时长边界，与 registry Agnes 视频型号的 supported_durations 同步。越界请求
	// fail-loud，而非静默截断后按原请求秒数计费。
	agnesVideoMinDurationSeconds      = 1
	agnesVideoMaxDurationSeconds      = 18
	agnesVideoFlashMinDurationSeconds = 4
	agnesVideoFlashMaxDurationSeconds = 12

	// 普通 Agnes 与 Flash 的参考图限制不同；Flash 上限以官方文档为准。
	agnesVideoMaxReferenceImages      = 4
	agnesVideoFlashMaxReferenceImages = 5

	// 尺寸约束：长宽被 8 整除、长边收口 1920（覆盖上游 480p/720p/1080p 三档标准化）。
	agnesVideoRoundTo     = 8
	agnesVideoMaxLongEdge = 1920

	// Flash 接受共享请求 schema 但仅支持 720p。
	agnesVideoFlashMaxResolution = "720p"

	// submit 超时 ~300s：覆盖上游争用时的长阻塞，避免可重试的繁忙被读超时包成终态歧义失败；
	// 轮询 / 下载用较短超时（幂等 GET 正常秒级返回）。
	agnesVideoSubmitTimeoutSeconds = 300.0
	agnesVideoPollHTTPTimeout      = 60 * time.Second

	agnesVideoPollInterval      = 5 * time.Second
	agnesVideoMinPollTimeout    = 900.0
	agnesVideoPollTimeoutPerSec = 60.0
)

// Agnes 视频请求的三种 mode。
const (
	agnesVideoTextMode      = "text"
	agnesVideoKeyframeMode  = "keyframe"
	agnesVideoReferenceMode = "reference"
)

// agnesVideoFailedStatuses 失败终态集合：除文档化的 failed 外，纳入 error / cancelled /
// canceled，避免上游以非标准失败态收尾时被当「仍在进行」轮询到超时。
var agnesVideoFailedStatuses = map[string]bool{
	"failed": true, "error": true, "cancelled": true, "canceled": true,
}

// agnesVideoFlashModels：接受共享请求 schema 但仅支持 720p 的型号集合。
var agnesVideoFlashModels = map[string]bool{"agnes-video-2.5-flash": true}

// 完成态响应中可能承载成片 URL 的权威字段，按优先级探测（顶层与 metadata 同权，顶层优先）。
var agnesVideoPrimaryURLFields = []string{"url", "video_url"}

// agnesVideoCompatURLField 语义是 remix 来源视频 ID（非 URL 形态时不当下载地址），列在此处仅为
// 兼容部分网关把成片 URL 直接回填在该字段的行为；优先级低于权威字段。
const agnesVideoCompatURLField = "remixed_from_video_id"

// agnesVideoBackend 是 Agnes 视频后端（异步 submit/poll，裸 base64 图像，支持 resume）。
type agnesVideoBackend struct {
	model        string
	baseURL      string
	host         string
	apiKey       string
	pollClient   *httpc.Client
	submitClient *httpc.Client
	opts         provider.BackendOptions
}

// newAgnesVideoBackend 按通用信封构造：api_key 缺失即 fail loud；model / base_url 留空时由本
// 后端兜底（默认 model、默认端点）。
func newAgnesVideoBackend(opts provider.BackendOptions) (*agnesVideoBackend, error) {
	apiKey, err := agnesResolveAPIKey(opts.APIKey)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = agnesVideoDefaultModel
	}
	return &agnesVideoBackend{
		model:        model,
		baseURL:      agnesBaseURL(opts.BaseURL),
		host:         agnesHost(opts.BaseURL),
		apiKey:       apiKey,
		pollClient:   httpc.NewClient(agnesHTTPTimeout(opts, agnesVideoPollHTTPTimeout)),
		submitClient: httpc.NewClient(time.Duration(agnesVideoSubmitTimeoutSeconds * float64(time.Second))),
		opts:         opts,
	}, nil
}

// Name 返回供应商标识。
func (b *agnesVideoBackend) Name() string { return agnesProviderID }

// Model 返回实际下发的模型名。
func (b *agnesVideoBackend) Model() string { return b.model }

// VideoCapabilities 返回本实例在自身 model 下的能力声明。
func (b *agnesVideoBackend) VideoCapabilities() provider.VideoCapabilities {
	caps, _ := agnesVideoCapabilitiesForModel(b.model)
	return caps
}

// agnesVideoCapabilitiesForModel 按 model_id 纯计算能力声明：不构造客户端、无需 api_key。
func agnesVideoCapabilitiesForModel(model string) (provider.VideoCapabilities, error) {
	caps := provider.DefaultVideoCapabilities()
	caps.LastFrame = true
	caps.MaxReferenceImages = agnesVideoMaxReferenceImages
	if agnesVideoFlashModels[strings.TrimSpace(model)] {
		caps.MaxReferenceImages = agnesVideoFlashMaxReferenceImages
	}
	return caps, nil
}

// Generate 生成视频：构建请求体 → 提交 → 持久化 job_id → 轮询 → 取件 → 下载。
func (b *agnesVideoBackend) Generate(
	ctx context.Context, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	payload, err := b.buildPayload(req)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	taskID, err := b.createTask(ctx, payload)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	slog.Info("Agnes 视频任务已创建", "task_id", taskID, "model", b.model)
	if err := PersistProviderJobIDFor(ctx, b.opts, req, taskID, b.baseURL); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	return b.pollAndBuild(ctx, taskID, req, false)
}

// ResumeVideo 接续已 submit 的 Agnes task：仅轮询 + 下载，不重新提交（避免二次计费）。
func (b *agnesVideoBackend) ResumeVideo(
	ctx context.Context, jobID string, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	return b.pollAndBuild(ctx, jobID, req, true)
}

// buildPayload 构建 Agnes 官方视频请求体，媒体通道互斥且显式声明 mode。
func (b *agnesVideoBackend) buildPayload(req provider.VideoGenerationRequest) (map[string]any, error) {
	if err := b.rejectOutOfRangeDuration(req.DurationSeconds); err != nil {
		return nil, err
	}
	width, height, err := b.resolveModelSize(req.Resolution, req.AspectRatio)
	if err != nil {
		return nil, err
	}
	isFlash := agnesVideoFlashModels[b.model]
	size := fmt.Sprintf("%dx%d", width, height)
	if isFlash {
		size = "720P"
	}
	payload := map[string]any{
		"model":        b.model,
		"prompt":       req.Prompt,
		"size":         size,
		"seconds":      strconv.Itoa(req.DurationSeconds),
		"aspect_ratio": req.AspectRatio,
	}
	if req.Seed != nil {
		payload["seed"] = *req.Seed
	}

	referenceImages := agnesValidPaths(req.ReferenceImages)
	startImage := agnesSinglePath(req.StartImage)
	endImage := agnesSinglePath(req.EndImage)

	// 参考图与首/尾帧走互斥的单通道。两者同时给出时 fail-loud，而非静默走参考图分支丢掉首/尾帧。
	if len(referenceImages) > 0 && (startImage != "" || endImage != "") {
		return nil, provider.NewVideoCapabilityError(provider.FailureVideoReferenceImagesWithFramesUnsupported, map[string]any{
			"model": b.model,
		})
	}

	switch {
	case len(referenceImages) > 0:
		limit := agnesVideoMaxReferenceImages
		if isFlash {
			limit = agnesVideoFlashMaxReferenceImages
		}
		if len(referenceImages) > limit {
			return nil, provider.NewVideoCapabilityError(provider.FailureVideoReferenceImagesExceeded, map[string]any{
				"model": b.model,
				"count": len(referenceImages),
				"limit": limit,
			})
		}
		encoded := make([]string, 0, len(referenceImages))
		for _, path := range referenceImages {
			value, err := b.encodeImage(path, provider.FailureVideoReferenceImagesUnreadable, "names")
			if err != nil {
				return nil, err
			}
			encoded = append(encoded, value)
		}
		payload["mode"] = agnesVideoReferenceMode
		payload["images"] = encoded
	case startImage != "" || endImage != "":
		payload["mode"] = agnesVideoKeyframeMode
		if startImage != "" {
			value, err := b.encodeImage(startImage, provider.FailureVideoStartImageUnreadable, "name")
			if err != nil {
				return nil, err
			}
			payload["first_frame"] = value
		}
		if endImage != "" {
			value, err := b.encodeImage(endImage, provider.FailureVideoEndImageUnreadable, "name")
			if err != nil {
				return nil, err
			}
			payload["last_frame"] = value
		}
	default:
		payload["mode"] = agnesVideoTextMode
	}

	return payload, nil
}

// resolveModelSize 解析尺寸并守住型号专属上限（应对过期的已存设置）。
func (b *agnesVideoBackend) resolveModelSize(resolution, aspectRatio string) (int, int, error) {
	if agnesVideoFlashModels[b.model] {
		trimmed := strings.TrimSpace(resolution)
		if trimmed != "" && trimmed != agnesVideoFlashMaxResolution {
			return 0, 0, provider.NewVideoCapabilityError(provider.FailureVideoResolutionNotSupported, map[string]any{
				"model":      b.model,
				"resolution": resolution,
				"supported":  agnesVideoFlashMaxResolution,
			})
		}
	}
	width, height := agnesResolveVideoSize(resolution, aspectRatio)
	return width, height, nil
}

// rejectOutOfRangeDuration 在供应商建单前按型号时长范围 fail-loud。
func (b *agnesVideoBackend) rejectOutOfRangeDuration(durationSeconds int) error {
	minimum, maximum := agnesVideoMinDurationSeconds, agnesVideoMaxDurationSeconds
	if agnesVideoFlashModels[b.model] {
		minimum, maximum = agnesVideoFlashMinDurationSeconds, agnesVideoFlashMaxDurationSeconds
	}
	if durationSeconds < minimum || durationSeconds > maximum {
		return provider.NewVideoCapabilityError(provider.FailureVideoDurationNotSupported, map[string]any{
			"model":     b.model,
			"duration":  durationSeconds,
			"supported": fmt.Sprintf("%d-%d", minimum, maximum),
		})
	}
	return nil
}

// agnesSinglePath 把请求里的图像字段归一化成路径；空 / 空串 / 空 Path（Path("") 会塌成 Path(".")）→ 空串。
func agnesSinglePath(value string) string {
	text := strings.TrimSpace(value)
	if text == "" || text == "." {
		return ""
	}
	return value
}

// agnesValidPaths 归一化参考图列表：剔除空 / 空 Path。
func agnesValidPaths(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if path := agnesSinglePath(value); path != "" {
			out = append(out, path)
		}
	}
	return out
}

// encodeImage 裸 base64 编码图像；缺失或不可读时按通道 error_code / 参数名 fail-loud。
//
// nameKey 取 "name"（单槽位，首/尾帧）或 "names"（多槽位，参考图），与 Python 各通道的报错
// 参数名一致。
func (b *agnesVideoBackend) encodeImage(path string, code provider.FailureCode, nameKey string) (string, error) {
	name := filepath.Base(path)
	if name == "" {
		name = path
	}
	if !minimaxIsFile(path) {
		return "", provider.NewVideoCapabilityError(code, map[string]any{"model": b.model, nameKey: name})
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", provider.NewVideoCapabilityError(code, map[string]any{"model": b.model, nameKey: name})
	}
	// Agnes 视频端对整串做 base64 解码，带 data: 前缀会在生成期触发 padding 错误，故用裸 base64
	// （图像端接受 data-URI，视频端不接受，二者不可混用）。
	return base64.StdEncoding.EncodeToString(raw), nil
}

// agnesResolveVideoSize 比例优先、清晰度其次：短边来自 resolution（档位 / 自定义 / 兜底 720p），
// 比例精确来自 aspect_ratio、长宽被 8 整除、长边收口 1920。
func agnesResolveVideoSize(resolution, aspectRatio string) (int, int) {
	short := project.ResolutionToShortEdge(resolution, project.VideoTierShortEdge, project.DefaultShortEdge)
	maxLong := agnesVideoMaxLongEdge
	return project.AspectSize(aspectRatio, short, project.AspectSizeOptions{
		RoundTo:     agnesVideoRoundTo,
		MaxLongEdge: &maxLong,
	})
}

// createTask 提交非幂等「建任务 + 计费」POST，返回 task_id。
//
// 账户额度 / 免费用户限流（429 rate_limit_exceeded）不重试，避免重复请求和误导性网络错误。
func (b *agnesVideoBackend) createTask(ctx context.Context, payload map[string]any) (string, error) {
	var taskID string
	err := provider.RunWithRetry(ctx, provider.DefaultRetryPolicy, agnesShouldRetrySubmit, func(ctx context.Context) error {
		resp, err := SubmitPost(ctx, agnesProviderID, func(ctx context.Context) (*httpc.Response, error) {
			return b.submitClient.PostJSON(ctx, httpc.JoinURL(b.baseURL, agnesVideoVideosEndpoint), agnesHeaders(b.apiKey), payload)
		})
		if err != nil {
			return err
		}
		var body map[string]any
		if err := decodeJSON(resp.Body, &body); err != nil {
			return err
		}
		id, err := agnesExtractTaskID(body)
		if err != nil {
			return err
		}
		taskID = id
		return nil
	})
	if err != nil {
		return "", err
	}
	return taskID, nil
}

// agnesExtractTaskID 从提交响应取轮询用 task_id（task_id 优先，回落 id）。
//
// 仅暴露字段名，不回显整串响应（可能含 prompt / 签名 URL 等敏感字段）。
func agnesExtractTaskID(body map[string]any) (string, error) {
	for _, key := range []string{"task_id", "id"} {
		if value := agnesStringValue(body[key]); value != "" {
			return value, nil
		}
	}
	return "", fmt.Errorf("Agnes 视频提交返回体缺少 task_id（字段: %v）", agnesSortedKeys(body))
}

// agnesShouldRetrySubmit 使用通用提交重试规则，但对 Agnes 账户限流快速失败。
func agnesShouldRetrySubmit(err error) bool {
	return !agnesIsAccountRateLimit(err) && provider.SubmitRetryable(err)
}

// agnesIsAccountRateLimit 识别 Agnes 账户额度限流；该 429 重试不会恢复额度且可能制造重复建单。
func agnesIsAccountRateLimit(err error) bool {
	var statusErr *provider.HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != 429 {
		return false
	}
	var body map[string]any
	if json.Unmarshal([]byte(statusErr.Body), &body) == nil {
		code := agnesStringValue(body["code"])
		if errObj, ok := body["error"].(map[string]any); ok && code == "" {
			code = agnesStringValue(errObj["code"])
		}
		if code == "rate_limit_exceeded" {
			return true
		}
		if detail, ok := body["detail"].(string); ok && strings.Contains(strings.ToLower(detail), "rate limit") {
			return true
		}
	}
	return strings.Contains(strings.ToLower(statusErr.Body), "rate limit")
}

// pollOnce 查询一次任务状态。
func (b *agnesVideoBackend) pollOnce(ctx context.Context, taskID string) (map[string]any, error) {
	resp, err := b.pollClient.Get(ctx, httpc.JoinURL(b.baseURL, agnesVideoVideosEndpoint+"/"+taskID), agnesHeaders(b.apiKey))
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := decodeJSON(resp.Body, &body); err != nil {
		return nil, err
	}
	return body, nil
}

// queryVideo 按 video_id 向成片查询端点二次查询（完成态只含 video_id、无直接 URL 字段时）。
//
// 该端点挂在网关根而非 /v1 下，且只认 video_id——拿 task_id 打它会排队异常。幂等 GET，复用
// 轮询同一套重试判定。
func (b *agnesVideoBackend) queryVideo(ctx context.Context, videoID string) (map[string]any, error) {
	var body map[string]any
	err := provider.RunWithRetry(ctx, provider.DefaultRetryPolicy, provider.PollRetryable, func(ctx context.Context) error {
		query := url.Values{}
		query.Set("video_id", videoID)
		query.Set("model_name", b.model)
		target := httpc.JoinURL(b.host, agnesVideoQueryEndpoint) + "?" + query.Encode()
		resp, err := b.pollClient.Get(ctx, target, agnesHeaders(b.apiKey))
		if err != nil {
			return err
		}
		body = nil
		return decodeJSON(resp.Body, &body)
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// resolveVideoURL 成片 URL 两级来源：完成态直接字段命中即用；否则用 video_id 二次查询取 URL。
//
// 命中二次查询时一并返回该查询响应体（未查询则 nil），供调用方从中补解析成片时长。
func (b *agnesVideoBackend) resolveVideoURL(ctx context.Context, final map[string]any) (string, map[string]any, error) {
	if videoURL := agnesFirstURLField(final); videoURL != "" {
		return videoURL, nil, nil
	}
	videoID := agnesStringValue(final["video_id"])
	if videoID == "" {
		return "", nil, fmt.Errorf("Agnes 任务完成但缺少成片 URL 与 video_id（字段: %v）", agnesSortedKeys(final))
	}
	queried, err := b.queryVideo(ctx, videoID)
	if err != nil {
		return "", nil, err
	}
	if videoURL := agnesFirstURLField(queried); videoURL != "" {
		return videoURL, queried, nil
	}
	return "", nil, fmt.Errorf("Agnes 任务完成但 video_id 查询响应缺少成片 URL（字段: %v）", agnesSortedKeys(queried))
}

// pollAndBuild 轮询至终态，取件并下载成片。
func (b *agnesVideoBackend) pollAndBuild(
	ctx context.Context, taskID string, req provider.VideoGenerationRequest, isResume bool,
) (provider.VideoGenerationResult, error) {
	// resume 路径下 404 直接转 ResumeExpiredError：轮询谓词把 404 当「短暂未就绪」重试，对已过期
	// 的 resume 任务会一直重到超时、永不落终态，故在此一击转终态异常。非 resume 的 4xx 原样抛出。
	gatedPoll := func(ctx context.Context) (map[string]any, error) {
		state, err := b.pollOnce(ctx, taskID)
		if err != nil {
			var statusErr *provider.HTTPStatusError
			if isResume && errors.As(err, &statusErr) && statusErr.StatusCode == 404 {
				return nil, &provider.ResumeExpiredError{JobID: taskID, Provider: agnesProviderID}
			}
			return nil, err
		}
		return state, nil
	}

	final, err := PollWithRetry(ctx, PollConfig[map[string]any]{
		PollFn: gatedPoll,
		IsDone: func(state map[string]any) bool {
			status := agnesStringValue(state["status"])
			return status == "completed" || status == "failed"
		},
		IsFailed:     agnesVideoFailureReason,
		PollInterval: agnesVideoPollInterval,
		MaxWait:      agnesVideoMaxWait(req.DurationSeconds),
		Label:        "Agnes",
		OnProgress: func(state map[string]any, elapsed time.Duration) {
			slog.Info("Agnes 视频生成中...",
				"status", agnesStringValue(state["status"]),
				"progress", agnesStringValue(state["progress"]),
				"elapsed", int(elapsed.Seconds()))
		},
	})
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}

	videoURL, queried, err := b.resolveVideoURL(ctx, final)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}

	// 下载走自带退避重试的 DownloadVideo，独立的下载重试范围，不回退到重跑生成 POST。
	if err := DownloadVideo(ctx, videoURL, req.OutputPath, 0); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	slog.Info("Agnes 视频下载完成", "output", req.OutputPath)

	// Agnes 视频无音频能力，成片恒无声；固定 false 与其余无声模型一致，避免下游误判有声。
	generateAudio := false
	return provider.VideoGenerationResult{
		VideoPath:       req.OutputPath,
		Provider:        agnesProviderID,
		Model:           b.model,
		DurationSeconds: agnesExtractDurationSeconds(final, queried, req.DurationSeconds),
		VideoURI:        videoURL,
		TaskID:          taskID,
		Seed:            req.Seed,
		GenerateAudio:   &generateAudio,
	}, nil
}

// agnesVideoFailureReason 失败终态（failed / error / cancelled / canceled）→ 错误描述；否则空串。
//
// 不止认 failed：上游若以其他失败态收尾，仅认 completed/failed 会把它当「仍在进行」轮询到
// max_wait 才抛误导性超时。显式枚举失败态让其快速失败。
func agnesVideoFailureReason(state map[string]any) string {
	status := agnesStringValue(state["status"])
	if !agnesVideoFailedStatuses[status] {
		return ""
	}
	message := ""
	if errObj, ok := state["error"].(map[string]any); ok {
		message = agnesStringValue(errObj["message"])
		if message == "" {
			message = agnesStringValue(errObj["code"])
		}
	} else if text := agnesStringValue(state["error"]); text != "" {
		message = text
	}
	if message == "" {
		message = "unknown"
	}
	return "Agnes 视频生成失败: " + message
}

// agnesFirstURLField 探测响应体中形态为 URL 的成片地址，按权威字段优先、兼容字段兜底；每一级
// 顶层与 metadata 同权、顶层优先，无命中返回空串。
func agnesFirstURLField(body map[string]any) string {
	metadata, _ := body["metadata"].(map[string]any)
	for _, key := range agnesVideoPrimaryURLFields {
		for _, source := range []map[string]any{body, metadata} {
			if source == nil {
				continue
			}
			if value, ok := source[key].(string); ok && agnesLooksLikeURL(value) {
				return value
			}
		}
	}
	for _, source := range []map[string]any{body, metadata} {
		if source == nil {
			continue
		}
		if value, ok := source[agnesVideoCompatURLField].(string); ok && agnesLooksLikeURL(value) {
			return value
		}
	}
	return ""
}

// agnesLooksLikeURL 粗粒度 URL 形态校验：http/https scheme + 非空 host。用于把 remix 来源 ID
// 这类语义不是 URL 的字段与真正的下载地址区分开。
func agnesLooksLikeURL(value string) bool {
	if value == "" {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// agnesExtractDurationSeconds 从轮询终态取实际成片时长（顶层 seconds），缺失时改读 video_id
// 二次查询响应的 seconds，两处均缺失或不可解析才回落请求时长。
//
// 不读 usage.duration_seconds——该字段是任务处理耗时，与成片时长无关，读它会错记计费与元数据。
func agnesExtractDurationSeconds(final, queried map[string]any, fallback int) int {
	if parsed, ok := agnesCoerceDuration(final["seconds"]); ok {
		return parsed
	}
	if queried != nil {
		if parsed, ok := agnesCoerceDuration(queried["seconds"]); ok {
			return parsed
		}
	}
	return fallback
}

// agnesCoerceDuration 把 "10.0" / 10 这类时长值归一化为计费秒数：half-up 取整，非正值 /
// 超 24h 上限（防 DB Integer 列溢出）/ 不可解析一律回落。
func agnesCoerceDuration(value any) (int, bool) {
	if value == nil {
		return 0, false
	}
	var f float64
	switch v := value.(type) {
	case float64:
		f = v
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if !(f > 0 && f <= float64(pricing.MaxBilledDurationSeconds)) {
		return 0, false
	}
	return int(math.Floor(f + 0.5)), true
}

// agnesVideoMaxWait 轮询上限：max(900, duration*60) 秒。
func agnesVideoMaxWait(durationSeconds int) time.Duration {
	seconds := agnesVideoMinPollTimeout
	if per := float64(durationSeconds) * agnesVideoPollTimeoutPerSec; per > seconds {
		seconds = per
	}
	return time.Duration(seconds * float64(time.Second))
}

// agnesStringValue 把任意 JSON 值渲染成串：nil → 空串，数字去尾零，其余 %v。
func agnesStringValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func init() {
	provider.RegisterVideoBackend(agnesProviderID, provider.VideoBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.VideoBackend, error) {
			return newAgnesVideoBackend(opts)
		},
		CapabilitiesForModel: agnesVideoCapabilitiesForModel,
	})
}
