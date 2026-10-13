package providers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// ArkVideoBackend —— 火山方舟 Ark 视频生成后端（端口自 lib/video_backends/ark.py）。
//
// 走 Ark 原生 REST 端点：submit 到 POST /contents/generations/tasks 取 id → 轮询
// GET /contents/generations/tasks/{id} 至 status=succeeded 取 content.video_url → 下载本地。
// submit 与 poll 分离重试（submit 走 SubmitRetryable，避免歧义态重复建任务重复计费）。

const (
	// arkDefaultVideoModel 是 model 缺省时的 Seedance 型号。
	arkDefaultVideoModel = "doubao-seedance-1-5-pro-251215"
	// arkVideoRequestTimeout 单次 create / poll 请求的墙钟预算；轮询总时限由 maxWait 控制。
	arkVideoRequestTimeout = 120 * time.Second
)

// Seedance 2.0 系列每请求最多 3 段参考音频，且总时长不超过 15 秒。
const (
	arkSeedance2MaxReferenceAudio             = 3
	arkSeedance2MaxReferenceAudioTotalSeconds = 15.0
)

// Seedance 1.5 pro 的参考图上限。
const arkSeedance15MaxReferenceImages = 9

// Seedance 2.5 的多模态素材上限：参考图 30 张，参考音频 10 段、总时长 30 秒。三项都比 2.0
// 宽，故与 2.0 各存一组常量而非共享——共享会在其中一档调整时静默改动另一档的放行范围。
const (
	arkSeedance25MaxReferenceImages            = 30
	arkSeedance25MaxReferenceAudio             = 10
	arkSeedance25MaxReferenceAudioTotalSeconds = 30.0
)

// arkReferenceAudioMIME 是参考音频扩展名 → data URI MIME：官方接受 wav / mp3 两种格式，要求
// data:audio/<格式>;base64,<内容> 且格式小写。未知扩展名不猜测直接拒绝。
var arkReferenceAudioMIME = map[string]string{".wav": "audio/wav", ".mp3": "audio/mp3"}

// arkReferenceAudioSupportedHint 是拒绝未知格式时回显的支持清单（对应 Python sorted 后 join）。
const arkReferenceAudioSupportedHint = ".mp3, .wav"

// 模型族识别用子串：宽松族群判定供 service_tier 剔除与参考图能力复用。
var (
	arkSeedance2Substrings   = []string{"seedance-2-0", "seedance-2.0"}
	arkSeedance25Substrings  = []string{"seedance-2-5", "seedance-2.5"}
	arkNoFirstFrameSubstrings = []string{"seedance-1-0-lite-t2v", "seedance-1.0-lite-t2v"}
	arkLastFrameAllowSubstrings = []string{
		"seedance-1-5-pro",
		"seedance-1.5-pro",
		"seedance-1-0-pro",
		"seedance-1.0-pro",
		"seedance-1-0-lite-i2v",
		"seedance-1.0-lite-i2v",
	}
	arkNoLastFrameSubstrings = []string{
		"seedance-1-0-pro-fast",
		"seedance-1.0-pro-fast",
		"seedance-1-0-lite-t2v",
		"seedance-1.0-lite-t2v",
	}
	arkReferenceImageAllowSubstrings = []string{"seedance-1-5-pro", "seedance-1.5-pro"}
	// Seedance 2.0 系列已验证支持首尾帧的三个变体（fast / mini / 基础），走边界匹配。
	arkSeedance2LastFrameAllowSubstrings = []string{
		"seedance-2-0-fast",
		"seedance-2.0-fast",
		"seedance-2-0-mini",
		"seedance-2.0-mini",
		"seedance-2-0",
		"seedance-2.0",
	}
	// Seedance 2.5 已验证支持首尾帧与参考音频的型号，走边界匹配。
	arkSeedance25AllowSubstrings = []string{"seedance-2-5", "seedance-2.5"}
)

// arkKnownModelSuffixRe：白名单命中后要求剩余部分为空或纯数字日期戳（如 "-251215"）——单纯
// 子串匹配会让未上表的未知变体（如 "-future"）继承已验证型号的尾帧能力。
var arkKnownModelSuffixRe = regexp.MustCompile(`^(-\d+)?$`)

// arkIsSeedance2 按 model_id 子串识别已验证的 seedance-2-0 / seedance-2.0 系列（含 fast 变体）。
func arkIsSeedance2(model string) bool {
	lower := strings.ToLower(model)
	for _, sub := range arkSeedance2Substrings {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

// arkIsSeedance25 按 model_id 子串识别 seedance-2-5 / seedance-2.5 系列。
func arkIsSeedance25(model string) bool {
	lower := strings.ToLower(model)
	for _, sub := range arkSeedance25Substrings {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

// arkMatchesKnownModel 在 modelLower 中查前缀，命中且其后缀为空或纯数字日期戳时返回 true。
func arkMatchesKnownModel(modelLower string, prefixes []string) bool {
	for _, prefix := range prefixes {
		idx := strings.Index(modelLower, prefix)
		if idx == -1 {
			continue
		}
		if arkKnownModelSuffixRe.MatchString(modelLower[idx+len(prefix):]) {
			return true
		}
	}
	return false
}

// arkContainsAny 判断 modelLower 是否含任一子串。
func arkContainsAny(modelLower string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(modelLower, sub) {
			return true
		}
	}
	return false
}

// arkFloat64Ptr 取 float64 地址（能力声明的可选上限字段需要指针语义）。
func arkFloat64Ptr(v float64) *float64 { return &v }

// arkVideoCapabilitiesForModel 按 model_id 纯计算参考图等 caps —— 不构造 client（无需 api_key）。
//
// 对应 Python ArkVideoBackend.video_capabilities_for_model；instance 方法委托至此，保持 backend
// 为单一真相源。
func arkVideoCapabilitiesForModel(model string) provider.VideoCapabilities {
	lower := strings.ToLower(model)

	if arkIsSeedance25(model) {
		// 2.5 与 2.0 的差别不止数值：素材上限放宽到参考图 30 / 音频 10 段、总时长 30 秒，且首帧
		// 在场时比例必须交给上游按首帧自适应，下发固定 ratio 会与首帧尺寸冲突。故独立分支。
		onVerifiedAllowlist := arkMatchesKnownModel(lower, arkSeedance25AllowSubstrings)
		caps := provider.DefaultVideoCapabilities()
		caps.LastFrame = onVerifiedAllowlist
		caps.MaxReferenceImages = arkSeedance25MaxReferenceImages
		if onVerifiedAllowlist {
			caps.ReferenceAudioMode = provider.ReferenceAudioDirect
			caps.MaxReferenceAudioCount = arkSeedance25MaxReferenceAudio
			caps.MaxReferenceAudioTotalSeconds = arkFloat64Ptr(arkSeedance25MaxReferenceAudioTotalSeconds)
		} else {
			caps.ReferenceAudioMode = provider.ReferenceAudioNone
		}
		caps.FirstFrameRatioAdaptiveOnly = true
		return caps
	}

	if arkIsSeedance2(model) {
		// 尾帧与参考音频都单独走边界校验的已验证型号白名单：未验证的 2.0 系列未来变体不应继承
		// 这两项能力；两者当前覆盖同一组已上表型号，故共用同一份白名单。
		onVerifiedAllowlist := arkMatchesKnownModel(lower, arkSeedance2LastFrameAllowSubstrings)
		caps := provider.DefaultVideoCapabilities()
		caps.LastFrame = onVerifiedAllowlist
		caps.MaxReferenceImages = arkSeedance15MaxReferenceImages
		if onVerifiedAllowlist {
			caps.ReferenceAudioMode = provider.ReferenceAudioDirect
			// 官方限制：每请求最多 3 段参考音频，且总时长不超过 15 秒。段数与总时长两个维度独立
			// 声明，由能力闸门分别校验。
			caps.MaxReferenceAudioCount = arkSeedance2MaxReferenceAudio
			caps.MaxReferenceAudioTotalSeconds = arkFloat64Ptr(arkSeedance2MaxReferenceAudioTotalSeconds)
		} else {
			caps.ReferenceAudioMode = provider.ReferenceAudioNone
		}
		return caps
	}

	// 非 2.0 系列：白名单覆盖能力表已验证支持首尾帧的三个型号，未命中白名单的一律 last_frame=False。
	noFirstFrame := arkContainsAny(lower, arkNoFirstFrameSubstrings)
	allowedLastFrame := arkMatchesKnownModel(lower, arkLastFrameAllowSubstrings)
	deniedLastFrame := arkContainsAny(lower, arkNoLastFrameSubstrings)
	caps := provider.DefaultVideoCapabilities()
	caps.FirstFrame = !noFirstFrame
	caps.LastFrame = allowedLastFrame && !deniedLastFrame
	// reference_images 在 1.5 pro 上是既有可用路径；未上表的型号仍保守判 0。
	if arkMatchesKnownModel(lower, arkReferenceImageAllowSubstrings) {
		caps.MaxReferenceImages = arkSeedance15MaxReferenceImages
	}
	return caps
}

// arkVideoBackend 是 Ark 视频生成后端实例。
type arkVideoBackend struct {
	model               string
	baseURL             string
	apiKey              string
	client              *httpc.Client
	supportsServiceTier bool
	opts                provider.BackendOptions
}

// newArkVideoBackend 按通用信封构造。
//
// service_tier 参数仅 seedance-1.x 等老模型支持；2.0 / 2.5 上游在 r2v 下会 400 拒绝该参数，
// 必须不下传。判定粒度是世代而非具体型号：同代的未知后缀照样按族群剔除。
func newArkVideoBackend(opts provider.BackendOptions) *arkVideoBackend {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = arkDefaultVideoModel
	}
	return &arkVideoBackend{
		model:               model,
		baseURL:             arkEffectiveBaseURL(opts.BaseURL),
		apiKey:              opts.APIKey,
		client:              httpc.NewClient(arkVideoRequestTimeout),
		supportsServiceTier: !(arkIsSeedance2(model) || arkIsSeedance25(model)),
		opts:                opts,
	}
}

// Name 返回供应商标识。
func (b *arkVideoBackend) Name() string { return arkProvider }

// Model 返回实际下发的模型名。
func (b *arkVideoBackend) Model() string { return b.model }

// VideoCapabilities 返回本 model 的能力声明（委托给纯函数，保持单一真相源）。
func (b *arkVideoBackend) VideoCapabilities() provider.VideoCapabilities {
	return arkVideoCapabilitiesForModel(b.model)
}

// Generate 生成视频。任务创建与轮询阶段分离重试，避免瞬态错误导致重建任务。
func (b *arkVideoBackend) Generate(
	ctx context.Context, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	taskID, err := b.createTask(ctx, req)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	if err := PersistProviderJobIDFor(ctx, b.opts, req, taskID, b.baseURL); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	return b.pollUntilDone(ctx, taskID, req)
}

// ResumeVideo 接续已 submit 的 Ark task：仅 poll + 下载。
//
// Ark 端 task 不存在/已过期通常表现为 404 类错误或 status=expired；后者在 pollUntilDone 内被
// 识别为失败消息，前者由本方法拦截转 ResumeExpiredError。
func (b *arkVideoBackend) ResumeVideo(
	ctx context.Context, jobID string, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	result, err := b.pollUntilDone(ctx, jobID, req)
	if err != nil {
		if arkIsNotFound(err) {
			return provider.VideoGenerationResult{}, &provider.ResumeExpiredError{
				JobID: jobID, Provider: arkProvider,
			}
		}
		return provider.VideoGenerationResult{}, err
	}
	return result, nil
}

// createTask 创建 Ark 视频生成任务（带提交重试保护）。
func (b *arkVideoBackend) createTask(
	ctx context.Context, req provider.VideoGenerationRequest,
) (string, error) {
	var taskID string
	err := provider.RunWithRetry(ctx, provider.DefaultRetryPolicy, provider.SubmitRetryable, func(ctx context.Context) error {
		id, err := b.submitCreateTask(ctx, req)
		if err != nil {
			return err
		}
		taskID = id
		return nil
	})
	return taskID, err
}

// submitCreateTask 组装 content 与创建参数并 POST。
func (b *arkVideoBackend) submitCreateTask(
	ctx context.Context, req provider.VideoGenerationRequest,
) (string, error) {
	content := []map[string]any{{"type": "text", "text": req.Prompt}}

	// Ark 视频 API 要求每个 image_url 条目在顶层带 role 字段（first_frame / last_frame /
	// reference_image），否则 400 InvalidParameter。
	if req.StartImage != "" {
		dataURI, err := imageToBase64DataURI(req.StartImage)
		if err != nil {
			return "", err
		}
		content = append(content, map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": dataURI}, "role": "first_frame",
		})
	}

	if req.EndImage != "" {
		if !arkFileExists(req.EndImage) {
			// 尾帧缺失不静默跳过：跳过后任务照常提交并计费，用户拿到一段没有尾帧、与分镜衔接不上
			// 的视频却无从知道原因。
			return "", provider.NewVideoCapabilityError(provider.FailureVideoEndImageUnreadable, map[string]any{
				"model": b.model, "name": filepath.Base(req.EndImage),
			})
		}
		dataURI, err := imageToBase64DataURI(req.EndImage)
		if err != nil {
			return "", err
		}
		content = append(content, map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": dataURI}, "role": "last_frame",
		})
	}

	if len(req.ReferenceImages) > 0 {
		var missing []string
		for _, ref := range req.ReferenceImages {
			if !arkFileExists(ref) {
				missing = append(missing, filepath.Base(ref))
			}
		}
		if len(missing) > 0 {
			// 与尾帧同理：少一张参考图仍会出片并计费，成片里的角色/场景却已跑偏。
			return "", provider.NewVideoCapabilityError(
				provider.FailureVideoReferenceImagesUnreadable, map[string]any{
					"model": b.model, "names": strings.Join(missing, ", "),
				},
			)
		}
		for _, ref := range req.ReferenceImages {
			dataURI, err := imageToBase64DataURI(ref)
			if err != nil {
				return "", err
			}
			content = append(content, map[string]any{
				"type": "image_url", "image_url": map[string]any{"url": dataURI}, "role": "reference_image",
			})
		}
	}

	if len(req.ReferenceAudioFiles) > 0 {
		// 音频条目与图片条目同列 content 数组，各自按类型独立编号：prompt 里的「音频N」指的是
		// 第 N 个 audio_url 条目。故这里必须保持原始顺序、不跳过任何一段。
		for _, audioPath := range req.ReferenceAudioFiles {
			dataURI, err := arkReferenceAudioDataURI(audioPath, b.model)
			if err != nil {
				return "", err
			}
			content = append(content, map[string]any{
				"type": "audio_url", "audio_url": map[string]any{"url": dataURI}, "role": "reference_audio",
			})
		}
	}

	// 比例优先：ratio 是独立字段，由 aspect_ratio 直接决定；resolution 仅清晰度档位，与比例正交。
	params := map[string]any{
		"model":          b.model,
		"content":        content,
		"ratio":          req.AspectRatio,
		"duration":       req.DurationSeconds,
		"generate_audio": req.GenerateAudio,
		"watermark":      false,
	}
	if req.Resolution != "" {
		params["resolution"] = req.Resolution
	}
	// seedance-2.0 / 2.5 等模型不接受 service_tier。
	if b.supportsServiceTier {
		params["service_tier"] = req.ServiceTier
	}
	if req.Seed != nil {
		params["seed"] = *req.Seed
	}

	resp, err := SubmitPost(ctx, arkProvider, func(ctx context.Context) (*httpc.Response, error) {
		return b.client.PostJSON(
			ctx, httpc.JoinURL(b.baseURL, "/contents/generations/tasks"), arkAuthHeaders(b.apiKey), params,
		)
	})
	if err != nil {
		return "", err
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(resp.Body, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		// 缺 id 会导致后续轮询打到空 task 路径而被误判为过期，故 fail loud。
		return "", &provider.NonRetryableError{Err: errors.New("Ark 视频任务创建响应缺少 id")}
	}
	slog.Info("Ark 任务已创建", "task_id", created.ID)
	return created.ID, nil
}

// arkVideoTask 是任务查询接口的响应形状。error 可能是字符串或对象，用 any 承载以免解析失败。
type arkVideoTask struct {
	Status  string `json:"status"`
	Error   any    `json:"error"`
	Content struct {
		VideoURL string `json:"video_url"`
	} `json:"content"`
	Seed  *int64 `json:"seed"`
	Usage *struct {
		CompletionTokens *int64 `json:"completion_tokens"`
	} `json:"usage"`
}

// arkTaskErrorMessage 把 error 字段渲染成失败消息；缺失或空串回退 "Unknown error"。
func arkTaskErrorMessage(value any) string {
	switch v := value.(type) {
	case nil:
		return "Unknown error"
	case string:
		if v == "" {
			return "Unknown error"
		}
		return v
	default:
		return fmt.Sprint(v)
	}
}

// getTask 查询任务状态。
func (b *arkVideoBackend) getTask(ctx context.Context, taskID string) (arkVideoTask, error) {
	resp, err := b.client.Get(
		ctx, httpc.JoinURL(b.baseURL, "/contents/generations/tasks/"+taskID), arkAuthHeaders(b.apiKey),
	)
	if err != nil {
		return arkVideoTask{}, err
	}
	var task arkVideoTask
	if err := decodeJSON(resp.Body, &task); err != nil {
		return arkVideoTask{}, err
	}
	return task, nil
}

// pollUntilDone 轮询任务状态直到完成，瞬态错误仅重试当次轮询请求。
func (b *arkVideoBackend) pollUntilDone(
	ctx context.Context, taskID string, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	// 轮询节奏按实际提交的档位定：supportsServiceTier 为假的型号根本不下传 service_tier，方舟
	// 按默认档建任务，此时若照 request 里残留的 flex 值走 60s 节奏会白等。
	effectiveTier := req.ServiceTier
	if !b.supportsServiceTier {
		effectiveTier = "default"
	}
	pollInterval := 60 * time.Second
	if effectiveTier == "default" {
		pollInterval = 10 * time.Second
	}
	// 生成耗时大体随输出时长线性增长：按 60s/输出秒取下限兜底，固定 600s 只够到 10 秒档；
	// flex 队列本就按小时级排队，保持 3600 不变。
	maxWait := 3600 * time.Second
	if effectiveTier == "default" {
		maxWait = 600 * time.Second
		if linear := time.Duration(60*req.DurationSeconds) * time.Second; linear > maxWait {
			maxWait = linear
		}
	}

	result, err := PollWithRetry(ctx, PollConfig[arkVideoTask]{
		PollFn: func(ctx context.Context) (arkVideoTask, error) {
			return b.getTask(ctx, taskID)
		},
		IsDone: func(task arkVideoTask) bool { return task.Status == "succeeded" },
		IsFailed: func(task arkVideoTask) string {
			if task.Status == "failed" || task.Status == "expired" {
				return fmt.Sprintf(
					"Ark 视频生成失败(status=%s): %s", task.Status, arkTaskErrorMessage(task.Error),
				)
			}
			return ""
		},
		PollInterval: pollInterval,
		MaxWait:      maxWait,
		Label:        "Ark",
		OnProgress: func(task arkVideoTask, elapsed time.Duration) {
			slog.Info("Ark 视频生成中...", "status", task.Status, "elapsed_seconds", int(elapsed.Seconds()))
		},
	})
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}

	videoURL := result.Content.VideoURL
	if err := DownloadVideo(ctx, videoURL, req.OutputPath, 0); err != nil {
		return provider.VideoGenerationResult{}, err
	}

	generated := provider.VideoGenerationResult{
		VideoPath:       req.OutputPath,
		Provider:        arkProvider,
		Model:           b.model,
		DurationSeconds: req.DurationSeconds,
		VideoURI:        videoURL,
		TaskID:          taskID,
	}
	if result.Seed != nil {
		generated.Seed = result.Seed
	}
	if result.Usage != nil && result.Usage.CompletionTokens != nil {
		generated.UsageTokens = result.Usage.CompletionTokens
	}
	generateAudio := req.GenerateAudio
	generated.GenerateAudio = &generateAudio

	return generated, nil
}

// arkReferenceAudioDataURI 把参考音频转成 base64 data URI；格式不受支持或文件不可读一律抛错。
//
// 与参考图的「缺失即跳过」不同，音频不能静默跳过：prompt 里的「音频N」按 content 数组中音频
// 条目的出现顺序编号，跳过一段会让其后所有编号整体前移，把某个角色的音色安到另一个角色头上。
func arkReferenceAudioDataURI(path, model string) (string, error) {
	mime, ok := arkReferenceAudioMIME[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return "", provider.NewVideoCapabilityError(provider.FailureVideoReferenceAudioFormatUnsupported, map[string]any{
			"model": model, "name": filepath.Base(path), "supported": arkReferenceAudioSupportedHint,
		})
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", provider.NewVideoCapabilityError(provider.FailureVideoReferenceAudioUnreadable, map[string]any{
			"model": model, "names": filepath.Base(path),
		})
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

// arkIsNotFound 识别 Ark 任务「不存在 / 已过期」响应。
//
// 精确匹配官方稳定 task_not_found 错误码，不落宽泛的 "not found" 子串兜底（避免业务侧错误被误判
// 为 provider 端任务过期）；"expired" 字串保留，用以识别 pollUntilDone 对 status=expired 生成的
// 失败消息。
func arkIsNotFound(err error) bool {
	var statusErr *provider.HTTPStatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == 404 {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "task_not_found") ||
		strings.Contains(msg, "tasknotfound") ||
		strings.Contains(msg, "expired")
}

func init() {
	create := func(opts provider.BackendOptions) (provider.VideoBackend, error) {
		return newArkVideoBackend(opts), nil
	}
	capsForModel := func(modelID string) (provider.VideoCapabilities, error) {
		return arkVideoCapabilitiesForModel(modelID), nil
	}
	// ark 与 ark-agent-plan 同用本实现；两键的域名差异由装配层经 BackendOptions.BaseURL 注入。
	// EffectiveGenerateAudioForModel 留空：Python Ark 后端未声明该静态规则，按默认值处理。
	registration := provider.VideoBackendRegistration{
		Create:               create,
		CapabilitiesForModel: capsForModel,
	}
	provider.RegisterVideoBackend("ark", registration)
	provider.RegisterVideoBackend("ark-agent-plan", registration)
}
