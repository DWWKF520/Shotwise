package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// Gemini (Veo) 视频生成后端（Python GeminiVideoBackend）。
//
// 约束的单一真相源是 registry 的 ModelInfo（reference_image_durations /
// duration_resolution_constraints），下面两个常量只在 registry 查不到该型号时兜底——中转站、
// 自定义供应商包装、已下线型号都会走到未登记路径，那里同样不能把必然失败的请求发出去。
// 兜底值复刻 Veo 全系现行约束：参考图路径与 1080p/4k 分辨率下只接受 8 秒。
const (
	geminiRequiredDurationSeconds         = 8
	geminiDurationConstrainedResolution   = "1080p"
	geminiDurationConstrainedResolution4K = "4k"

	// geminiVideoPollIntervalSeconds / geminiVideoMaxWaitSeconds 与 Python 轮询参数一致
	// （与 Google 官方推荐 20s 间隔一致，最长等待 600s）。
	geminiVideoPollIntervalSeconds = 20
	geminiVideoMaxWaitSeconds      = 600
)

// geminiVideoBackend 是 Gemini (Veo) 视频生成后端。
type geminiVideoBackend struct {
	auth  *geminiAuth
	model string
	opts  provider.BackendOptions
}

// geminiVideoRef 是 operation 响应里视频引用的承载结构，兼容 AI Studio 与 Vertex 的形状。
type geminiVideoRef struct {
	URI                string `json:"uri"`
	GCSURI             string `json:"gcsUri"`
	VideoBytes         string `json:"videoBytes"`
	BytesBase64Encoded string `json:"bytesBase64Encoded"`
}

// geminiGeneratedVideo 是一条生成视频条目（SDK 侧 generated_videos[i]）。
type geminiGeneratedVideo struct {
	Video geminiVideoRef `json:"video"`
}

// geminiOperationResponse 是 operation.response 的形状，兼容多种生成的视频承载位置。
type geminiOperationResponse struct {
	GeneratedVideos       []geminiGeneratedVideo `json:"generatedVideos"`
	Videos                []geminiVideoRef       `json:"videos"`
	GenerateVideoResponse *struct {
		GeneratedSamples []geminiGeneratedVideo `json:"generatedSamples"`
		Videos           []geminiVideoRef       `json:"videos"`
	} `json:"generateVideoResponse"`
}

// geminiOperation 是一条长任务（LRO）的状态。
type geminiOperation struct {
	Name     string                   `json:"name"`
	Done     bool                     `json:"done"`
	Error    json.RawMessage          `json:"error"`
	Response *geminiOperationResponse `json:"response"`
	Metadata json.RawMessage          `json:"metadata"`
}

// newGeminiVideoBackend 按通用信封构造。模型名优先取具名参数 video_model，其次取通用 Model 字段。
func newGeminiVideoBackend(opts provider.BackendOptions) (*geminiVideoBackend, error) {
	auth, err := newGeminiAuth(opts)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(opts.Extra["video_model"])
	if model == "" {
		model = strings.TrimSpace(opts.Model)
	}
	if model == "" {
		model = geminiDefaultVideoModel
	}
	return &geminiVideoBackend{auth: auth, model: model, opts: opts}, nil
}

// Name 返回 backend 标识（Python name = f"gemini-{backend_type}"）。
func (b *geminiVideoBackend) Name() string { return "gemini-" + b.auth.backendType }

// Model 返回实际下发的模型名。
func (b *geminiVideoBackend) Model() string { return b.model }

// VideoCapabilities 返回本实例模型的静态能力声明。
func (b *geminiVideoBackend) VideoCapabilities() provider.VideoCapabilities {
	caps, _ := geminiVideoCapabilitiesForModel(b.model)
	return caps
}

// geminiVideoCapabilitiesForModel 按 model_id 纯计算 caps（Python video_capabilities_for_model）。
//
// 当前全系模型能力一致，不按 model_id 分支：尾帧可用、参考图最多 3 张。
func geminiVideoCapabilitiesForModel(_ string) (provider.VideoCapabilities, error) {
	caps := provider.DefaultVideoCapabilities()
	caps.LastFrame = true
	caps.MaxReferenceImages = 3
	return caps, nil
}

// Generate 生成视频。任务创建与轮询阶段分离重试，避免瞬态错误导致重建任务。
func (b *geminiVideoBackend) Generate(
	ctx context.Context, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	// 能力校验放在重试之外：约束违规必然复现，重试只是重复失败。
	if err := b.validateDurationConstraints(req); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	op, err := b.createTask(ctx, req)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	if strings.TrimSpace(op.Name) == "" {
		// fail-fast：缺 operation.name 意味着 submit 成功但无法持久化 provider_job_id，
		// 进程中断后孤儿处理会走 [restart_lost] 回退到重新提交路径——正是要避免的重复扣费场景。
		return provider.VideoGenerationResult{}, errors.New(
			"Gemini 提交成功但未返回 operation.name，无法持久化 provider_job_id",
		)
	}
	if err := PersistProviderJobIDFor(ctx, b.opts, req, op.Name, ""); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	return b.pollUntilDone(ctx, op, req)
}

// ResumeVideo 接续已 submit 的 Gemini operation：按 name 重新查询走 poll + 下载。
//
// NOT_FOUND 既可能在初次查询抛，也可能在 mid-poll 抛（远端 GC 了 LRO）；两处都归类为
// ResumeExpiredError 走 [resume_expired]。
func (b *geminiVideoBackend) ResumeVideo(
	ctx context.Context, jobID string, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	op, err := b.getOperation(ctx, jobID)
	if err != nil {
		return b.classifyResumeError(jobID, err)
	}
	result, err := b.pollUntilDone(ctx, op, req)
	if err != nil {
		return b.classifyResumeError(jobID, err)
	}
	return result, nil
}

// classifyResumeError 把「operation 不存在 / 已过期」归类为 ResumeExpiredError。
func (b *geminiVideoBackend) classifyResumeError(
	jobID string, err error,
) (provider.VideoGenerationResult, error) {
	if geminiIsNotFound(err) {
		return provider.VideoGenerationResult{}, &provider.ResumeExpiredError{
			JobID:    jobID,
			Provider: geminiProviderID,
		}
	}
	return provider.VideoGenerationResult{}, err
}

// validateDurationConstraints 参考图 / 受约束分辨率两条路径的时长约束，违反即拒绝。
func (b *geminiVideoBackend) validateDurationConstraints(req provider.VideoGenerationRequest) error {
	info, ok := provider.ModelInfoFor(b.Name(), b.model)

	if len(req.ReferenceImages) > 0 {
		// 已登记型号一律采信其声明：空声明的语义是「参考图路径不额外约束时长」，与前端同口径；
		// 只有型号未登记才回落兜底常量。用 or 折叠两者会让空声明被兜底覆盖成 [8]。
		var allowed []int
		if ok {
			allowed = info.ReferenceImageDurations
		} else {
			allowed = []int{geminiRequiredDurationSeconds}
		}
		if len(allowed) > 0 && !geminiContainsDuration(allowed, req.DurationSeconds) {
			return provider.NewVideoCapabilityError(
				provider.FailureVideoReferenceImagesDurationUnsupported, map[string]any{
					"model":     b.model,
					"duration":  req.DurationSeconds,
					"supported": geminiFormatDurations(allowed),
				},
			)
		}
	}

	resolution := strings.ToLower(strings.TrimSpace(req.Resolution))
	var constraints map[string][]int
	if ok {
		constraints = info.DurationResolutionConstraints
	}
	// 声明缺位处兜底常量继续生效：防线要覆盖所有会被送到供应商的取值（如未支持 4k 的型号
	// 声明里没有 4k 档，但真收到 4k 请求时放行只会换来一次必然失败的调用）。
	allowed := constraints[resolution]
	if allowed == nil &&
		(resolution == geminiDurationConstrainedResolution || resolution == geminiDurationConstrainedResolution4K) {
		allowed = []int{geminiRequiredDurationSeconds}
	}
	if len(allowed) > 0 && !geminiContainsDuration(allowed, req.DurationSeconds) {
		return provider.NewVideoCapabilityError(
			provider.FailureVideoResolutionDurationUnsupported, map[string]any{
				"model":      b.model,
				"resolution": strings.ToUpper(resolution),
				"duration":   req.DurationSeconds,
				"supported":  geminiFormatDurations(allowed),
			},
		)
	}
	return nil
}

// createTask 创建 Gemini 视频生成任务（带提交阶段重试保护）。
func (b *geminiVideoBackend) createTask(
	ctx context.Context, req provider.VideoGenerationRequest,
) (geminiOperation, error) {
	instance := map[string]any{"prompt": req.Prompt}
	if strings.TrimSpace(req.StartImage) != "" {
		image, err := geminiInlineImagePart(req.StartImage)
		if err != nil {
			return geminiOperation{}, err
		}
		instance["image"] = image
	}
	// end_image → lastFrame（帧插值）。
	if strings.TrimSpace(req.EndImage) != "" {
		lastFrame, err := geminiInlineImagePart(req.EndImage)
		if err != nil {
			return geminiOperation{}, err
		}
		instance["lastFrame"] = lastFrame
	}
	// reference_images → referenceImages（参考图列表，referenceType=asset）。
	if len(req.ReferenceImages) > 0 {
		refs := make([]any, 0, len(req.ReferenceImages))
		for _, path := range req.ReferenceImages {
			image, err := geminiInlineImagePart(path)
			if err != nil {
				return geminiOperation{}, err
			}
			refs = append(refs, map[string]any{"image": image, "referenceType": "asset"})
		}
		instance["referenceImages"] = refs
	}

	parameters := map[string]any{
		"aspectRatio":     req.AspectRatio,
		"durationSeconds": req.DurationSeconds,
	}
	if strings.TrimSpace(req.Resolution) != "" {
		parameters["resolution"] = req.Resolution
	}
	// 音轨开关仅 Vertex 支持下发；AI Studio 恒有声、无开关通道。
	if b.auth.backendType == geminiBackendVertex {
		parameters["generateAudio"] = req.GenerateAudio
	}

	payload := map[string]any{"instances": []any{instance}, "parameters": parameters}
	headers, err := b.auth.headers(ctx)
	if err != nil {
		return geminiOperation{}, err
	}
	resp, err := geminiRequestJSON(
		ctx, b.auth.client, provider.DefaultRetryPolicy, b.auth.videoURL(b.model), headers, payload,
	)
	if err != nil {
		return geminiOperation{}, err
	}
	var op geminiOperation
	if err := decodeJSON(resp.Body, &op); err != nil {
		return geminiOperation{}, err
	}
	return op, nil
}

// getOperation 查询一条长任务状态。
func (b *geminiVideoBackend) getOperation(ctx context.Context, name string) (geminiOperation, error) {
	headers, err := b.auth.headers(ctx)
	if err != nil {
		return geminiOperation{}, err
	}
	resp, err := b.auth.client.Get(ctx, b.auth.operationURL(name), headers)
	if err != nil {
		return geminiOperation{}, err
	}
	var op geminiOperation
	if err := decodeJSON(resp.Body, &op); err != nil {
		return geminiOperation{}, err
	}
	return op, nil
}

// pollUntilDone 轮询任务状态直到完成，瞬态错误仅重试当次轮询请求。
func (b *geminiVideoBackend) pollUntilDone(
	ctx context.Context, op geminiOperation, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	opName := op.Name
	if !op.Done {
		polled, err := PollWithRetry(ctx, PollConfig[geminiOperation]{
			PollFn: func(ctx context.Context) (geminiOperation, error) {
				return b.getOperation(ctx, opName)
			},
			IsDone:       func(o geminiOperation) bool { return o.Done },
			IsFailed:     func(geminiOperation) string { return "" }, // Gemini 在轮询完成后检查失败
			PollInterval: geminiVideoPollIntervalSeconds * time.Second,
			MaxWait:      geminiVideoMaxWaitSeconds * time.Second,
			RetryIf:      geminiPollRetryable,
			Label:        "Gemini",
			OnProgress: func(_ geminiOperation, elapsed time.Duration) {
				slog.Info("视频生成中...", "operation", opName, "elapsed_seconds", int(elapsed.Seconds()))
			},
		})
		if err != nil {
			return provider.VideoGenerationResult{}, err
		}
		op = polled
	}

	ref, ok := geminiFirstVideo(op)
	if !ok {
		detail := geminiOperationError(op)
		slog.Error("视频生成返回空结果", "operation", opName, "error", detail)
		if detail != "" {
			return provider.VideoGenerationResult{}, fmt.Errorf("视频生成失败: %s", detail)
		}
		return provider.VideoGenerationResult{}, errors.New("视频生成失败: API 返回空结果")
	}

	if err := b.downloadVideo(ctx, ref, req.OutputPath); err != nil {
		return provider.VideoGenerationResult{}, err
	}

	videoURI := ref.URI
	if videoURI == "" {
		videoURI = ref.GCSURI
	}
	// AI Studio 成片恒有声（无开关通道），Vertex 按请求值。
	generateAudio := req.GenerateAudio
	if b.auth.backendType != geminiBackendVertex {
		generateAudio = true
	}
	return provider.VideoGenerationResult{
		VideoPath:       req.OutputPath,
		Provider:        geminiProviderID,
		Model:           b.model,
		DurationSeconds: req.DurationSeconds,
		VideoURI:        videoURI,
		GenerateAudio:   &generateAudio,
	}, nil
}

// downloadVideo 把生成结果落到本地：Vertex 优先内联字节，其次按 URI 下载；AI Studio 走 URI 下载。
func (b *geminiVideoBackend) downloadVideo(
	ctx context.Context, ref geminiVideoRef, outputPath string,
) error {
	if b.auth.backendType == geminiBackendVertex {
		if data, err := geminiDecodeVideoBytes(ref); err == nil && len(data) > 0 {
			return writeFileAtomic(outputPath, data)
		}
		uri := ref.URI
		if uri == "" {
			uri = ref.GCSURI
		}
		if uri == "" {
			return errors.New("视频生成成功但无法获取视频数据")
		}
		return b.downloadVideoFromURI(ctx, uri, outputPath)
	}
	if strings.TrimSpace(ref.URI) == "" {
		return errors.New("视频生成成功但无法获取视频数据")
	}
	return b.downloadVideoFromURI(ctx, ref.URI, outputPath)
}

// geminiDecodeVideoBytes 解码内联视频字节（vertex 响应可能直接带回 bytes）。
func geminiDecodeVideoBytes(ref geminiVideoRef) ([]byte, error) {
	for _, encoded := range []string{ref.VideoBytes, ref.BytesBase64Encoded} {
		if strings.TrimSpace(encoded) == "" {
			continue
		}
		return base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	}
	return nil, errors.New("no inline video bytes")
}

// downloadVideoFromURI 带鉴权下载结果视频（幂等 GET，自带退避重试）。
func (b *geminiVideoBackend) downloadVideoFromURI(ctx context.Context, rawURI, outputPath string) error {
	return provider.RunWithRetry(ctx, provider.DownloadRetryPolicy, provider.DownloadRetryable,
		func(ctx context.Context) error {
			validated, err := httpc.ValidateOutboundURL(rawURI)
			if err != nil {
				return err
			}
			headers, err := b.auth.headers(ctx)
			if err != nil {
				return err
			}
			resp, err := b.auth.client.Get(ctx, validated, headers)
			if err != nil {
				return err
			}
			if len(resp.Body) == 0 {
				return fmt.Errorf("downloaded video is empty: %s", validated)
			}
			return writeFileAtomic(outputPath, resp.Body)
		})
}

// geminiFirstVideo 取第一条生成视频，兼容多种承载位置。
func geminiFirstVideo(op geminiOperation) (geminiVideoRef, bool) {
	if op.Response == nil {
		return geminiVideoRef{}, false
	}
	resp := op.Response
	if resp.GenerateVideoResponse != nil {
		if len(resp.GenerateVideoResponse.GeneratedSamples) > 0 {
			return resp.GenerateVideoResponse.GeneratedSamples[0].Video, true
		}
		if len(resp.GenerateVideoResponse.Videos) > 0 {
			return resp.GenerateVideoResponse.Videos[0], true
		}
	}
	if len(resp.GeneratedVideos) > 0 {
		return resp.GeneratedVideos[0].Video, true
	}
	if len(resp.Videos) > 0 {
		return resp.Videos[0], true
	}
	return geminiVideoRef{}, false
}

// geminiOperationError 把 operation.error 渲染成可读串（无则空串）。
func geminiOperationError(op geminiOperation) string {
	if len(op.Error) == 0 || string(op.Error) == "null" {
		return ""
	}
	return string(op.Error)
}

// geminiPollRetryable 是轮询阶段的重试谓词：幂等 GET 口径，但 404 不重试。
//
// Python 的轮询谓词不把 NOT_FOUND 当作可重试瞬态；resume 路径据此把 mid-poll 的 404
// 归类为 ResumeExpiredError，而非拖到 max_wait 超时。
func geminiPollRetryable(err error) bool {
	if geminiIsNotFound(err) {
		return false
	}
	return provider.PollRetryable(err)
}

// geminiIsNotFound 识别「operation 不存在 / 已过期」响应。
//
// INVALID_ARGUMENT 不归过期：Gemini 用它表达入参不合法，与 NOT_FOUND（资源不存在）语义不同；
// 归过期会把客户端 bug 当成幽灵任务静默吞掉。
func geminiIsNotFound(err error) bool {
	if err == nil {
		return false
	}
	var statusErr *provider.HTTPStatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == 404 {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not found") || strings.Contains(message, "expired")
}

// geminiContainsDuration 报告时长是否在允许集合内。
func geminiContainsDuration(allowed []int, duration int) bool {
	for _, d := range allowed {
		if d == duration {
			return true
		}
	}
	return false
}

// geminiFormatDurations 把允许时长列表渲染进拒绝文案，如 [8] → "8s"。
func geminiFormatDurations(durations []int) string {
	sorted := append([]int(nil), durations...)
	sort.Ints(sorted)
	parts := make([]string, 0, len(sorted))
	for _, d := range sorted {
		parts = append(parts, fmt.Sprintf("%ds", d))
	}
	return strings.Join(parts, ", ")
}

func init() {
	provider.RegisterVideoBackend("gemini", provider.VideoBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.VideoBackend, error) {
			return newGeminiVideoBackend(opts)
		},
		CapabilitiesForModel: geminiVideoCapabilitiesForModel,
	})
}
