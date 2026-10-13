package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// OpenAIVideoBackend —— OpenAI Sora 视频生成后端（端口自 lib/video_backends/openai.py）。
//
// 官方 Go SDK 不覆盖 videos 接口，故直接走 REST：submit POST /videos 取 id → 轮询
// GET /videos/{id}（终态 completed / failed / expired）→ GET /videos/{id}/content 取字节落盘。
// 任务查询与内容下载分离重试，避免下载失败重新触发（计费）生成。

const (
	// openAIVideoDefaultModel 是 model 缺省时的 Sora 型号。
	openAIVideoDefaultModel = "sora-2"

	// openAIVideoPollInterval 两次轮询之间的间隔。
	openAIVideoPollInterval = 5 * time.Second
	// openAIVideoMinPollTimeout 轮询总时限下限（不足则按输出时长线性放宽）。
	openAIVideoMinPollTimeout = 600 * time.Second
	// openAIVideoPollTimeoutPerSecond 每输出秒折算的轮询总时限增量。
	openAIVideoPollTimeoutPerSecond = 30 * time.Second
	// openAIVideoRequestTimeout 单次 create / poll / 下载请求的墙钟预算。
	openAIVideoRequestTimeout = 600 * time.Second
)

// sora 合法 size 按 model 能力 + 分辨率档分级（OpenAI 官方 changelog / 模型页）：
//   - sora-2（base）：仅 720p —— 9:16 720x1280 / 16:9 1280x720。
//   - sora-2-pro：720p 同上 + 1080p —— 9:16 1080x1920 / 16:9 1920x1080。
//
// 非任意 WxH，只能吸附合法档：比例优先选档，分辨率档只决定 720p vs 1080p 子集。size 必传以锁定
// 比例，绝不出现「不传 size → 上游默认比例」。
var (
	openAIVideoSizes720p  = []string{"720x1280", "1280x720"}
	openAIVideoSizes1080p = []string{"1080x1920", "1920x1080"}
)

// openAIVideo1080pMinShort 是 1080p 档的短边像素。
const openAIVideo1080pMinShort = 1080

// openAIVideoResolveSize：比例优先，在 model+分辨率档对应的 sora 合法档中选比例最接近
// aspect_ratio 的；并列取像素更高者。
//
// 分辨率档只决定 720p vs 1080p 子集、不决定比例：sora-2-pro 选 1080p 时 9:16→1080x1920。
// sora-2（base）或缺分辨率时落 720p（缺分辨率默认 720P，不擅自升 1080p 以免超额计费）。其它
// 比例（1:1 / 21:9 等）sora 无对应档，吸附后告警。
func openAIVideoResolveSize(model, resolution, aspectRatio string) string {
	aw, ah := domain.ParseAspectRatio(aspectRatio)
	target := float64(aw) / float64(ah)
	isPro := strings.Contains(strings.ToLower(model), "pro")
	short := domain.ResolutionToShortEdge(resolution, domain.VideoTierShortEdge, domain.DefaultShortEdge)

	// sora 支持的短边档：base 仅 720；pro 增 1080。按「最近档」选（等距取更高档），避免把
	// short=1000 这类自定义分辨率值无故降到 720p。
	supportedShorts := []int{720}
	if isPro {
		supportedShorts = []int{720, openAIVideo1080pMinShort}
	}
	achievedShort := 0
	bestDiff := 0
	for i, s := range supportedShorts {
		diff := s - short
		if diff < 0 {
			diff = -diff
		}
		if i == 0 || diff < bestDiff || (diff == bestDiff && s > achievedShort) {
			achievedShort, bestDiff = s, diff
		}
	}

	legal := openAIVideoSizes720p
	if achievedShort == openAIVideo1080pMinShort {
		legal = openAIVideoSizes1080p
	}
	if short > achievedShort {
		// 请求高于模型可达档（base 请 1080p、或 pro 请 4K）：封顶并提示清晰度让位。
		slog.Warn("OpenAI video: 无法满足分辨率请求（输出封顶，清晰度让位比例）",
			"model", model, "resolution", resolution, "short_edge", short, "achieved_short", achievedShort)
	}

	// 比例差小优先；并列取像素更多。
	chosen := legal[0]
	chosenDiff := math.MaxFloat64
	chosenPixels := 0
	for _, size := range legal {
		w, h := splitWxH(size)
		diff := absFloat(float64(w)/float64(h) - target)
		pixels := w * h
		if diff < chosenDiff-1e-9 || (absFloat(diff-chosenDiff) <= 1e-9 && pixels > chosenPixels) {
			chosen, chosenDiff, chosenPixels = size, diff, pixels
		}
	}

	// 9:16 / 16:9 精确命中；其它比例 sora 无对应档，吸附后比例必然偏差，告警避免静默产出错比例。
	cw, ch := splitWxH(chosen)
	if absFloat(float64(cw)/float64(ch)-target) > 0.01 {
		slog.Warn("OpenAI video: aspect_ratio 无精确 sora 档，吸附到合法档（输出非项目设定比例）",
			"aspect_ratio", aspectRatio, "chosen", chosen)
	}
	return chosen
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// splitWxH 解析 "WxH"；非法输入返回 0,0。
func splitWxH(size string) (int, int) {
	parts := strings.SplitN(size, "x", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	w, _ := strconv.Atoi(parts[0])
	h, _ := strconv.Atoi(parts[1])
	return w, h
}

// openAIVideoCapabilitiesForModel 按 model_id 纯计算 caps —— 不构造 client（无需 api_key）。
//
// Sora input_reference 为单张首帧图，参考图上限为 1；首帧与参考共享该单槽位。当前全系模型能力
// 一致，不按 model_id 分支；instance 方法委托至此，保持 backend 为单一真相源。
func openAIVideoCapabilitiesForModel(model string) provider.VideoCapabilities {
	caps := provider.DefaultVideoCapabilities()
	caps.MaxReferenceImages = 1
	return caps
}

// openAIVideoFileExists 判断本地路径是否存在（首帧 / 参考图打不开即视为不存在）。
func openAIVideoFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// openAIVideoJob 是 Sora 任务创建 / 查询接口的响应形状（只取用得上的字段）。
type openAIVideoJob struct {
	ID      string          `json:"id"`
	Status  string          `json:"status"`
	Seconds *int            `json:"seconds"`
	Error   json.RawMessage `json:"error"`
}

// openAIVideoErrorText 把 error 字段渲染成失败消息；缺失 / null 回退 "None"（与 Python str(None) 一致）。
func openAIVideoErrorText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "None"
	}
	var obj struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Message != "" {
		return obj.Message
	}
	return trimmed
}

// openAIVideoIsNotFound 识别 OpenAI/Sora「job 不存在」响应（NotFoundError / HTTP 404）。
//
// 不做 "not found" / "expired" 子串兜底：status='expired' 已在轮询终态里处理，宽泛字串会把诸如
// "file not found in storage" 等业务错误误判为幽灵任务。
func openAIVideoIsNotFound(err error) bool {
	var statusErr *provider.HTTPStatusError
	return errors.As(err, &statusErr) && statusErr.StatusCode == 404
}

// openAIVideoBackend 是 OpenAI Sora 视频生成后端实例。
type openAIVideoBackend struct {
	model   string
	baseURL string
	apiKey  string
	client  *httpc.Client
	opts    provider.BackendOptions
}

// newOpenAIVideoBackend 按通用信封构造；model / base_url 留空时由本后端自行兜底。
func newOpenAIVideoBackend(opts provider.BackendOptions) *openAIVideoBackend {
	baseURL := strings.TrimSpace(opts.BaseURL)
	if baseURL == "" {
		baseURL = openAIDefaultBaseURL
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = openAIVideoDefaultModel
	}
	return &openAIVideoBackend{
		model:   model,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  opts.APIKey,
		client:  httpc.NewClient(openAIVideoRequestTimeout),
		opts:    opts,
	}
}

// Name 返回供应商标识。
func (b *openAIVideoBackend) Name() string { return ProviderOpenAI }

// Model 返回实际下发的模型名。
func (b *openAIVideoBackend) Model() string { return b.model }

// VideoCapabilities 返回本 model 的能力声明（委托给纯函数，保持单一真相源）。
func (b *openAIVideoBackend) VideoCapabilities() provider.VideoCapabilities {
	return openAIVideoCapabilitiesForModel(b.model)
}

// authHeaders 构造鉴权头。
func (b *openAIVideoBackend) authHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + b.apiKey}
}

// openAIVideoReferencePaths 收集所有存在的参考图路径：首帧 start_image 在前，reference_images 在后。
func openAIVideoReferencePaths(req provider.VideoGenerationRequest) []string {
	var refs []string
	if req.StartImage != "" && openAIVideoFileExists(req.StartImage) {
		refs = append(refs, req.StartImage)
	}
	for _, ref := range req.ReferenceImages {
		if openAIVideoFileExists(ref) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// Generate 生成视频。任务创建与轮询阶段分离重试，避免瞬态错误导致重建任务。
func (b *openAIVideoBackend) Generate(
	ctx context.Context, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	created, err := b.createVideo(ctx, req)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	// submit 成功立即持久化 job_id；持久化失败上抛 → worker finally mark_failed。
	if err := PersistProviderJobIDFor(ctx, b.opts, req, created.ID, b.baseURL); err != nil {
		return provider.VideoGenerationResult{}, err
	}

	final, err := b.pollUntilComplete(ctx, created.ID, req.DurationSeconds)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	// generate 路径下 expired 是「provider 异常 / 输入参数过期」类失败，抛错让 worker mark_failed。
	if final.Status == "expired" {
		return provider.VideoGenerationResult{}, fmt.Errorf("OpenAI Sora job expired during generate: %s", final.ID)
	}
	return b.downloadAndBuild(ctx, final, req)
}

// ResumeVideo 接续已 submit 的 Sora job：仅 poll + 下载，不调 videos.create。
func (b *openAIVideoBackend) ResumeVideo(
	ctx context.Context, jobID string, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	final, err := b.pollUntilComplete(ctx, jobID, req.DurationSeconds)
	if err != nil {
		if openAIVideoIsNotFound(err) {
			return provider.VideoGenerationResult{}, &provider.ResumeExpiredError{
				JobID: jobID, Provider: ProviderOpenAI,
			}
		}
		return provider.VideoGenerationResult{}, err
	}
	// resume 路径下 expired = provider 端已忘 / 输入资产过期，归类 [resume_expired]。
	if final.Status == "expired" {
		return provider.VideoGenerationResult{}, &provider.ResumeExpiredError{
			JobID:    jobID,
			Provider: ProviderOpenAI,
			Message:  fmt.Sprintf("OpenAI Sora job expired: %s", final.ID),
		}
	}
	return b.downloadAndBuild(ctx, final, req)
}

// createVideo 仅创建视频任务（带 OPENAI_RETRYABLE_ERRORS 等价重试）；轮询交由 pollUntilComplete 自管。
func (b *openAIVideoBackend) createVideo(
	ctx context.Context, req provider.VideoGenerationRequest,
) (openAIVideoJob, error) {
	var created openAIVideoJob
	err := provider.RunWithRetry(
		ctx, provider.DefaultRetryPolicy, provider.IsTransientUpstreamError,
		func(ctx context.Context) error {
			job, err := b.postCreate(ctx, req)
			if err != nil {
				return err
			}
			created = job
			return nil
		},
	)
	return created, err
}

// postCreate 组装并提交 create 请求。
//
// 无参考图走 JSON（与 openai-python SDK 无文件时的编码形态一致）；有参考图走 multipart，
// 单张 / 多张共用 input_reference 字段名（API 兼容）。
func (b *openAIVideoBackend) postCreate(
	ctx context.Context, req provider.VideoGenerationRequest,
) (openAIVideoJob, error) {
	// 始终下传合法 size 以锁定比例（sora 固定档，不传则上游用自家默认比例）。
	size := openAIVideoResolveSize(b.model, req.Resolution, req.AspectRatio)
	seconds := strconv.Itoa(req.DurationSeconds)
	target := httpc.JoinURL(b.baseURL, "/videos")

	refs := openAIVideoReferencePaths(req)
	var (
		resp *httpc.Response
		err  error
	)
	if len(refs) == 0 {
		payload := map[string]any{
			"prompt":  req.Prompt,
			"model":   b.model,
			"seconds": seconds,
			"size":    size,
		}
		resp, err = b.client.PostJSON(ctx, target, b.authHeaders(), payload)
	} else {
		fields := map[string]string{
			"prompt":  req.Prompt,
			"model":   b.model,
			"seconds": seconds,
			"size":    size,
		}
		files := make([]httpc.FilePart, 0, len(refs))
		for _, refPath := range refs {
			content, rerr := os.ReadFile(refPath)
			if rerr != nil {
				return openAIVideoJob{}, fmt.Errorf("read reference image %s: %w", refPath, rerr)
			}
			files = append(files, httpc.FilePart{
				FieldName: "input_reference",
				FileName:  filepath.Base(refPath),
				Content:   content,
			})
		}
		resp, err = b.client.PostMultipart(ctx, target, b.authHeaders(), fields, files)
	}
	if err != nil {
		return openAIVideoJob{}, err
	}

	var created openAIVideoJob
	if err := decodeJSON(resp.Body, &created); err != nil {
		return openAIVideoJob{}, err
	}
	if strings.TrimSpace(created.ID) == "" {
		// 缺 id 会让后续轮询打到空任务路径并被误判为过期，故 fail loud、不重试。
		return openAIVideoJob{}, &provider.NonRetryableError{
			Err: errors.New("OpenAI Sora 视频任务创建响应缺少 id"),
		}
	}
	slog.Info("OpenAI 视频生成开始", "model", b.model, "seconds", seconds)
	return created, nil
}

// getVideo 查询任务状态。
func (b *openAIVideoBackend) getVideo(ctx context.Context, videoID string) (openAIVideoJob, error) {
	resp, err := b.client.Get(ctx, httpc.JoinURL(b.baseURL, "/videos/"+videoID), b.authHeaders())
	if err != nil {
		return openAIVideoJob{}, err
	}
	var job openAIVideoJob
	if err := decodeJSON(resp.Body, &job); err != nil {
		return openAIVideoJob{}, err
	}
	return job, nil
}

// pollUntilComplete 轮询任务直到 status=='completed'。
//
// 不复用 SDK 的 poll：它仅识别 in_progress/queued/completed/failed，对接返回非标状态的 OpenAI
// 兼容网关时会提前退出，导致下载未就绪任务。重试谓词用 OPENAI_RETRYABLE_ERRORS 等价语义
// （IsTransientUpstreamError），404 不重试（与 Python 一致）。
func (b *openAIVideoBackend) pollUntilComplete(
	ctx context.Context, videoID string, durationSeconds int,
) (openAIVideoJob, error) {
	maxWait := openAIVideoMinPollTimeout
	if linear := time.Duration(durationSeconds) * openAIVideoPollTimeoutPerSecond; linear > maxWait {
		maxWait = linear
	}

	// IsDone 是纯谓词：completed / failed / expired 三种状态都视为「已终态」让 poll 返回。
	// caller 拿到 result 后再分流：completed → 下载；failed → IsFailed 已抛错；expired → 按
	// generate vs resume 上下文抛不同异常。关键不变量：IsFailed 不识别 expired。
	return PollWithRetry(ctx, PollConfig[openAIVideoJob]{
		PollFn: func(ctx context.Context) (openAIVideoJob, error) {
			return b.getVideo(ctx, videoID)
		},
		IsDone: func(v openAIVideoJob) bool {
			return v.Status == "completed" || v.Status == "failed" || v.Status == "expired"
		},
		IsFailed: func(v openAIVideoJob) string {
			if v.Status == "failed" {
				return "Sora 视频生成失败: " + openAIVideoErrorText(v.Error)
			}
			return ""
		},
		PollInterval: openAIVideoPollInterval,
		MaxWait:      maxWait,
		RetryIf:      provider.IsTransientUpstreamError,
		Label:        "OpenAI",
		OnProgress: func(v openAIVideoJob, elapsed time.Duration) {
			slog.Info("OpenAI 视频生成中...", "status", v.Status, "elapsed_seconds", int(elapsed.Seconds()))
		},
	})
}

// downloadAndBuild 下载成片字节并落盘，构建结果。
func (b *openAIVideoBackend) downloadAndBuild(
	ctx context.Context, final openAIVideoJob, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	content, err := b.downloadContent(ctx, final.ID)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	if err := writeFileAtomic(req.OutputPath, content); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	slog.Info("OpenAI 视频下载完成", "output_path", req.OutputPath)

	// 计费时长以 provider 回传为准，缺失回退请求时长。
	duration := req.DurationSeconds
	if final.Seconds != nil {
		duration = *final.Seconds
	}
	return provider.VideoGenerationResult{
		VideoPath:       req.OutputPath,
		Provider:        ProviderOpenAI,
		Model:           b.model,
		DurationSeconds: duration,
		TaskID:          final.ID,
	}, nil
}

// downloadContent 单独重试内容下载（5 次、5/10/20/40 秒），避免因下载失败重新触发视频生成。
//
// content 端点需鉴权头，故不复用无鉴权的 DownloadVideo。
func (b *openAIVideoBackend) downloadContent(ctx context.Context, videoID string) ([]byte, error) {
	var content []byte
	err := provider.RunWithRetry(
		ctx, provider.DownloadRetryPolicy, provider.IsTransientUpstreamError,
		func(ctx context.Context) error {
			resp, err := b.client.Get(
				ctx, httpc.JoinURL(b.baseURL, "/videos/"+videoID+"/content"), b.authHeaders(),
			)
			if err != nil {
				return err
			}
			content = resp.Body
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	if len(content) == 0 {
		return nil, errors.New("OpenAI Sora 视频内容为空")
	}
	return content, nil
}

func init() {
	create := func(opts provider.BackendOptions) (provider.VideoBackend, error) {
		return newOpenAIVideoBackend(opts), nil
	}
	capsForModel := func(modelID string) (provider.VideoCapabilities, error) {
		return openAIVideoCapabilitiesForModel(modelID), nil
	}
	provider.RegisterVideoBackend("openai", provider.VideoBackendRegistration{
		Create:               create,
		CapabilitiesForModel: capsForModel,
	})
}