package providers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// MiniMax（海螺）视频后端（端口自 lib/video_backends/minimax.py），两代 API 并存。
//
// v1（海螺系列）两步取 URL：submit POST /v1/video_generation 取 task_id → 轮询
// GET /v1/query/video_generation?task_id= 至 status=Success 取 file_id →
// GET /v1/files/retrieve?file_id= 取 download_url → 下载本地。覆盖 MiniMax-Hailuo-2.3
// （t2v+i2v）、MiniMax-Hailuo-2.3-Fast（仅 i2v）与 S2V-01（单脸参考生视频）。
//
// v2（MiniMax-H3）单步取 URL：submit POST /v2/video_generation 取 task_id → 轮询
// GET /v2/query/video_generation/{task_id} 至 task.status=succeeded 直接取 task.content.url。
// 请求体是多模态 content[] 数组，条目按 role 区分首帧/尾帧/参考图/参考音频。
//
// 走哪一代在构造期按 model 定下（两代 base、提交体、轮询响应形状、取件方式全不同），各步据此派发。
const (
	// 海螺图生视频系列：走 first_frame_image 首帧；Fast 仅图生视频。
	minimaxVideoHailuo     = "MiniMax-Hailuo-2.3"
	minimaxVideoHailuoFast = "MiniMax-Hailuo-2.3-Fast"
	// S2V-01：单张人脸驱动的角色一致性参考生视频（R2V），不接受首帧 / resolution / duration。
	minimaxVideoS2V = "S2V-01"
	// H3：多模态 content[] 端点（v2），唯一走 v2 分支的型号，也是默认型号。
	minimaxVideoH3 = "MiniMax-H3"

	minimaxVideoDefaultModel = minimaxVideoH3

	minimaxVideoSubmitEndpoint   = "/video_generation"
	minimaxVideoQueryEndpoint    = "/query/video_generation"
	minimaxVideoRetrieveEndpoint = "/files/retrieve"

	// 轮询间隔：海螺视频生成通常数十秒至数分钟，10s 一次平衡时延与请求量。
	minimaxVideoPollInterval = 10 * time.Second
	// 轮询超时下界与每秒增量：max(900, duration*60)。
	minimaxVideoMinPollTimeoutSeconds    = 900.0
	minimaxVideoPollTimeoutPerSecond     = 60.0
	minimaxVideoDefaultHTTPTimeout       = 60 * time.Second
	// minimaxVideoV1DefaultResolution 缺 resolution 时的默认档（小写）。
	minimaxVideoV1DefaultResolution = "768p"

	// H3 输入上限（出处：官方《创建视频生成任务 (V2)》）。输出规格不在此声明，registry 是
	// resolutions / supported_durations 的唯一真相源。
	minimaxH3MaxReferenceImages           = 9
	minimaxH3MaxReferenceAudio            = 3
	minimaxH3MaxReferenceAudioTotalSecond = 15.0
	minimaxH3MaxPromptChars               = 7000
)

// minimaxVideoImageMIME 图片后缀 → MIME（首帧/参考图 data URI）。取海螺视频各型号受理格式的
// 并集，heic/heif 仅 H3 受理；未知后缀回落 image/png。
var minimaxVideoImageMIME = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".heic": "image/heic",
	".heif": "image/heif",
}

// minimaxReferenceAudioMIME 参考音频扩展名 → MIME。官方接受 wav / mp3，要求
// data:audio/<格式>;base64,<内容> 且格式小写。
var minimaxReferenceAudioMIME = map[string]string{".wav": "audio/wav", ".mp3": "audio/mp3"}

// minimaxV1ResolutionDurations v1 分支的分辨率 → 允许时长集合：1080P 仅 6s，768P 支持 6s/10s。
// 仅 v1 适用——H3 两档分辨率共用同一段连续时长，直接读 registry 声明。
var minimaxV1ResolutionDurations = map[string]map[int]bool{
	"768p":  {6: true, 10: true},
	"1080p": {6: true},
}

// minimaxNoTextToVideoModels 无首帧的文生视频不受支持：2.3-Fast 仅图生视频；S2V-01 由
// subject_reference 驱动。未登记型号按支持处理，与其余能力维度的「未知即通用默认」一致。
var minimaxNoTextToVideoModels = map[string]bool{
	minimaxVideoHailuoFast: true,
	minimaxVideoS2V:        true,
}

// minimaxVideoBackend 是 MiniMax 视频后端（异步轮询）。
type minimaxVideoBackend struct {
	model string
	// isV2 在构造期按 model 定下，后续各步据此派发 v1 / v2 分支。
	isV2                bool
	supportsTextToVideo bool
	baseURL             string
	apiKey              string
	client              *httpc.Client
	opts                provider.BackendOptions
}

// newMinimaxVideoBackend 按通用信封构造；api_key 缺失即 fail loud；model / base_url 留空时由本
// 后端兜底（默认 H3、国内站端点）。
func newMinimaxVideoBackend(opts provider.BackendOptions) (*minimaxVideoBackend, error) {
	apiKey, err := minimaxResolveAPIKey(opts.APIKey)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = minimaxVideoDefaultModel
	}
	isV2 := minimaxIsH3Model(model)
	baseURL := minimaxVideoBaseURL(opts.BaseURL)
	if isV2 {
		baseURL = minimaxVideoV2BaseURL(opts.BaseURL)
	}
	return &minimaxVideoBackend{
		model:               model,
		isV2:                isV2,
		supportsTextToVideo: !minimaxNoTextToVideoModels[strings.TrimSpace(model)],
		baseURL:             baseURL,
		apiKey:              apiKey,
		client:              httpc.NewClient(minimaxHTTPTimeout(opts, minimaxVideoDefaultHTTPTimeout)),
		opts:                opts,
	}, nil
}

// Name 返回供应商标识。
func (b *minimaxVideoBackend) Name() string { return minimaxProviderID }

// Model 返回实际下发的模型名。
func (b *minimaxVideoBackend) Model() string { return b.model }

// VideoCapabilities 返回本实例在自身 model 下的能力声明。
func (b *minimaxVideoBackend) VideoCapabilities() provider.VideoCapabilities {
	caps, _ := minimaxVideoCapabilitiesForModel(b.model)
	return caps
}

// minimaxIsH3Model H3 判定：大小写不敏感、容忍命名空间前缀（中转站可能把型号存成
// "proxy/minimax-h3"），与自定义供应商发现端点的判定口径一致。发现与派发用同一谓词，避免
// 「发现路由到 minimax-video、派发却落回 v1」的裂缝。
func minimaxIsH3Model(model string) bool {
	return strings.Contains(strings.ToLower(model), "minimax-h3")
}

// minimaxVideoCapabilitiesForModel 按 model_id 纯计算能力声明：不构造实例、无需 api_key。
//
// 海螺图生视频走 first_frame_image 首帧；S2V-01 仅接受单张人脸参考、不接受首帧图，故
// first_frame=false + max_reference_images=1。H3 的 content[] 按 role 同时承载首帧、尾帧、
// 参考图与参考音频，各维度上限取官方声明值；首帧任务只接受 ratio=adaptive，故声明
// first_frame_ratio_adaptive_only。
func minimaxVideoCapabilitiesForModel(model string) (provider.VideoCapabilities, error) {
	if strings.TrimSpace(model) == minimaxVideoS2V {
		caps := provider.DefaultVideoCapabilities()
		caps.FirstFrame = false
		caps.MaxReferenceImages = 1
		return caps, nil
	}
	if minimaxIsH3Model(model) {
		caps := provider.DefaultVideoCapabilities()
		caps.LastFrame = true
		caps.MaxReferenceImages = minimaxH3MaxReferenceImages
		caps.ReferenceAudioMode = provider.ReferenceAudioDirect
		// 段数与总时长两个维度独立声明：3 段各 10 秒都在单段合法区间内，合计已超 15 秒。
		caps.MaxReferenceAudioCount = minimaxH3MaxReferenceAudio
		caps.MaxReferenceAudioTotalSeconds = minimaxFloat64Ptr(minimaxH3MaxReferenceAudioTotalSecond)
		caps.MaxPromptChars = minimaxIntPtr(minimaxH3MaxPromptChars)
		caps.FirstFrameRatioAdaptiveOnly = true
		return caps, nil
	}
	return provider.DefaultVideoCapabilities(), nil
}

// minimaxFloat64Ptr / minimaxIntPtr 取指针，供可选上限字段赋值。
func minimaxFloat64Ptr(v float64) *float64 { return &v }
func minimaxIntPtr(v int) *int             { return &v }

// Generate 生成视频：构建请求体 → 提交 → 持久化 job_id → 轮询 → 取件 → 下载。
func (b *minimaxVideoBackend) Generate(
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
	slog.Info("MiniMax 视频任务已创建", "task_id", taskID, "model", b.model)
	if err := PersistProviderJobIDFor(ctx, b.opts, req, taskID, b.baseURL); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	return b.pollAndBuild(ctx, taskID, req)
}

// ResumeVideo 接续已 submit 的 MiniMax task：仅轮询 + 取回 + 下载，不重新提交（避免二次计费）。
func (b *minimaxVideoBackend) ResumeVideo(
	ctx context.Context, jobID string, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	return b.pollAndBuild(ctx, jobID, req)
}

// buildPayload 按代际派发请求体构建。
func (b *minimaxVideoBackend) buildPayload(req provider.VideoGenerationRequest) (map[string]any, error) {
	switch {
	case b.isV2:
		return b.buildV2Payload(req)
	case strings.TrimSpace(b.model) == minimaxVideoS2V:
		return b.buildS2VPayload(req)
	default:
		return b.buildV1Payload(req)
	}
}

// buildV1Payload 构建海螺/Hailuo 系列的 v1 请求体。
func (b *minimaxVideoBackend) buildV1Payload(req provider.VideoGenerationRequest) (map[string]any, error) {
	resolution := strings.ToLower(strings.TrimSpace(req.Resolution))
	if resolution == "" {
		resolution = minimaxVideoV1DefaultResolution
	}
	duration := req.DurationSeconds
	hasStartImage := strings.TrimSpace(req.StartImage) != ""

	// 无首帧 = 文生视频意图；模型不支持 t2v（如 Fast）即拒绝。
	if !hasStartImage && !b.supportsTextToVideo {
		return nil, provider.NewVideoCapabilityError(provider.FailureVideoCapabilityMissingT2V, map[string]any{
			"provider": b.Name(),
			"model":    b.model,
		})
	}

	allowed := minimaxV1ResolutionDurations[resolution]
	if !allowed[duration] {
		// 空集合（分辨率未知）用语言中性占位符，避免中文兜底漏进非中文界面。
		supported := minimaxSortedDurationSupport(allowed)
		return nil, provider.NewVideoCapabilityError(provider.FailureVideoResolutionDurationUnsupported, map[string]any{
			"model":      b.model,
			"resolution": strings.ToUpper(resolution),
			"duration":   duration,
			"supported":  supported,
		})
	}

	payload := map[string]any{
		"model":      b.model,
		"prompt":     req.Prompt,
		"duration":   duration,
		"resolution": strings.ToUpper(resolution),
	}
	if hasStartImage {
		name := filepath.Base(req.StartImage)
		if !minimaxIsFile(req.StartImage) {
			return nil, provider.NewVideoCapabilityError(provider.FailureVideoStartImageUnreadable, map[string]any{
				"model": b.model, "name": name,
			})
		}
		uri, err := minimaxVideoImageDataURI(req.StartImage)
		if err != nil {
			return nil, provider.NewVideoCapabilityError(provider.FailureVideoStartImageUnreadable, map[string]any{
				"model": b.model, "name": name,
			})
		}
		payload["first_frame_image"] = uri
	}
	return payload, nil
}

// buildS2VPayload 把 reference_images[0] 映射成单脸 subject_reference。
//
// fail-loud：未提供参考图 → required；声明的参考图缺失/不可读 → unreadable，不静默退化为
// 无参考生成（会产出错误结果且照常计费）。
func (b *minimaxVideoBackend) buildS2VPayload(req provider.VideoGenerationRequest) (map[string]any, error) {
	provided := minimaxNonEmptyPaths(req.ReferenceImages)
	if len(provided) == 0 {
		return nil, provider.NewVideoCapabilityError(provider.FailureVideoReferenceImagesRequired, map[string]any{
			"model": b.model,
		})
	}
	face := provided[0]
	name := filepath.Base(face)
	if !minimaxIsFile(face) {
		return nil, provider.NewVideoCapabilityError(provider.FailureVideoReferenceImagesUnreadable, map[string]any{
			"model": b.model, "names": name,
		})
	}
	uri, err := minimaxVideoImageDataURI(face)
	if err != nil {
		return nil, provider.NewVideoCapabilityError(provider.FailureVideoReferenceImagesUnreadable, map[string]any{
			"model": b.model, "names": name,
		})
	}
	return map[string]any{
		"model":  b.model,
		"prompt": req.Prompt,
		"subject_reference": []map[string]any{
			{"type": "character", "image": []string{uri}},
		},
	}, nil
}

// buildV2Payload 把首帧/尾帧/参考图/参考音频按 role 摊进多模态 content[] 数组。
//
// prompt 恒为首条 text 条目（官方要求所有场景都带一个非空 text）；素材条目顺序即 prompt 中
// 「音频N」等指认编号的依据，故任何一段素材缺失都 fail-loud，不静默跳过。
func (b *minimaxVideoBackend) buildV2Payload(req provider.VideoGenerationRequest) (map[string]any, error) {
	resolution := strings.ToLower(strings.TrimSpace(req.Resolution))
	if resolution == "" {
		resolution = minimaxVideoV1DefaultResolution
	}
	duration := req.DurationSeconds
	resolutions, durations, err := minimaxV2OutputSpecs()
	if err != nil {
		return nil, err
	}
	if !resolutions[resolution] || !durations[duration] {
		// v2 的分辨率与时长是两个独立集合，越界可能来自任一维度；supported 一并列出两维。
		return nil, provider.NewVideoCapabilityError(provider.FailureVideoResolutionDurationUnsupported, map[string]any{
			"model":      b.model,
			"resolution": strings.ToUpper(resolution),
			"duration":   duration,
			"supported":  minimaxV2SupportedSummary(resolutions, durations),
		})
	}

	startImage := strings.TrimSpace(req.StartImage)
	endImage := strings.TrimSpace(req.EndImage)
	references := minimaxNonEmptyPaths(req.ReferenceImages)
	audioFiles := minimaxNonEmptyPaths(req.ReferenceAudioFiles)
	// 官方口径：图生视频与多模态参考生视频互斥，参考音频同属参考生视频维度。
	if (len(references) > 0 || len(audioFiles) > 0) && (startImage != "" || endImage != "") {
		return nil, provider.NewVideoCapabilityError(provider.FailureVideoReferenceImagesWithFramesUnsupported, map[string]any{
			"model": b.model,
		})
	}
	if endImage != "" && startImage == "" {
		return nil, provider.NewVideoCapabilityError(provider.FailureVideoEndImageRequiresStartImage, map[string]any{
			"model": b.model,
		})
	}

	content := []map[string]any{{"type": "text", "text": req.Prompt}}
	if startImage != "" {
		uri, err := minimaxExistingImageDataURI(startImage)
		if err != nil {
			return nil, provider.NewVideoCapabilityError(provider.FailureVideoStartImageUnreadable, map[string]any{
				"model": b.model, "name": filepath.Base(startImage),
			})
		}
		content = append(content, minimaxImageContentItem(uri, "first_frame"))
	}
	if endImage != "" {
		uri, err := minimaxExistingImageDataURI(endImage)
		if err != nil {
			return nil, provider.NewVideoCapabilityError(provider.FailureVideoEndImageUnreadable, map[string]any{
				"model": b.model, "name": filepath.Base(endImage),
			})
		}
		content = append(content, minimaxImageContentItem(uri, "last_frame"))
	}
	for _, reference := range references {
		uri, err := minimaxExistingImageDataURI(reference)
		if err != nil {
			return nil, provider.NewVideoCapabilityError(provider.FailureVideoReferenceImagesUnreadable, map[string]any{
				"model": b.model, "names": filepath.Base(reference),
			})
		}
		content = append(content, minimaxImageContentItem(uri, "reference_image"))
	}
	for _, audioPath := range audioFiles {
		uri, err := minimaxReferenceAudioDataURI(audioPath, b.model)
		if err != nil {
			return nil, err
		}
		content = append(content, map[string]any{
			"type":      "audio_url",
			"audio_url": map[string]any{"url": uri},
			"role":      "reference_audio",
		})
	}

	return map[string]any{
		"model":      b.model,
		"content":    content,
		"resolution": strings.ToUpper(resolution),
		"duration":   duration,
		"ratio":      req.AspectRatio,
	}, nil
}

// minimaxV2OutputSpecs 返回 H3 允许的（分辨率档集合，时长集合），取自 registry 声明。
//
// 一律按 canonical 名 minimaxVideoH3 查 registry——self.model 可能是中转站发现的大小写/
// 命名空间变体，字面值不一定与 registry key 一致。查询固定 canonical 名，缺失只可能是 registry
// 条目被误删/改名，故 fail loud 而非回落兜底常量。
func minimaxV2OutputSpecs() (map[string]bool, map[int]bool, error) {
	info, ok := provider.ModelInfoFor(minimaxProviderID, minimaxVideoH3)
	if !ok {
		return nil, nil, fmt.Errorf("registry 缺少 %s/%s 条目，无法确定 H3 输出规格", minimaxProviderID, minimaxVideoH3)
	}
	resolutions := make(map[string]bool, len(info.Resolutions))
	for _, r := range info.Resolutions {
		resolutions[strings.ToLower(r)] = true
	}
	durations := make(map[int]bool, len(info.SupportedDurations))
	for _, d := range info.SupportedDurations {
		durations[d] = true
	}
	return resolutions, durations, nil
}

// minimaxV2SupportedSummary 把两维声明拼成 "2K, 768P × 4s, 5s, ..." 形态。
func minimaxV2SupportedSummary(resolutions map[string]bool, durations map[int]bool) string {
	upper := make([]string, 0, len(resolutions))
	for r := range resolutions {
		upper = append(upper, strings.ToUpper(r))
	}
	sort.Strings(upper)
	secs := minimaxSortedDurationSupport(durations)
	return strings.Join(upper, ", ") + " × " + secs
}

// minimaxSortedDurationSupport 把允许时长集合拼成 "6s, 10s"；空集合用 "-" 占位。
func minimaxSortedDurationSupport(allowed map[int]bool) string {
	if len(allowed) == 0 {
		return "-"
	}
	items := make([]int, 0, len(allowed))
	for d := range allowed {
		items = append(items, d)
	}
	sort.Ints(items)
	parts := make([]string, len(items))
	for i, d := range items {
		parts[i] = fmt.Sprintf("%ds", d)
	}
	return strings.Join(parts, ", ")
}

// minimaxNonEmptyPaths 过滤掉空串路径。
func minimaxNonEmptyPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			out = append(out, path)
		}
	}
	return out
}

// minimaxImageContentItem 图片 data URI → v2 content[] 条目。
func minimaxImageContentItem(dataURI, role string) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURI}, "role": role}
}

// minimaxExistingImageDataURI 图片 → data URI；缺失或不可读返回错误，由调用方按槽位抛对应码。
func minimaxExistingImageDataURI(path string) (string, error) {
	if !minimaxIsFile(path) {
		return "", errors.New("image not readable")
	}
	return minimaxVideoImageDataURI(path)
}

// minimaxVideoImageDataURI 本地图片 → base64 data URI（7 项 MIME，含 heic/heif，未知后缀回落
// image/png）。不复用图片侧 helper：海螺视频受理的格式集比通用图片后端更宽。
func minimaxVideoImageDataURI(path string) (string, error) {
	mime, ok := minimaxVideoImageMIME[strings.ToLower(filepath.Ext(path))]
	if !ok {
		mime = "image/png"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

// minimaxReferenceAudioDataURI 参考音频 → base64 data URI；格式不受支持或文件不可读一律抛错。
//
// 与参考图同为 fail-loud：prompt 里的「音频N」按 content 数组中音频条目的出现顺序编号，跳过
// 一段会让其后所有编号整体前移，把某个角色的音色安到另一个角色头上。
func minimaxReferenceAudioDataURI(path, model string) (string, error) {
	mime, ok := minimaxReferenceAudioMIME[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return "", provider.NewVideoCapabilityError(provider.FailureVideoReferenceAudioFormatUnsupported, map[string]any{
			"model":     model,
			"name":      filepath.Base(path),
			"supported": minimaxSortedMIMEKeys(minimaxReferenceAudioMIME),
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

// minimaxSortedMIMEKeys 返回 MIME 表键的字典序拼接（供报错列出支持的格式）。
func minimaxSortedMIMEKeys(table map[string]string) string {
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// createTask 提交非幂等「建任务 + 计费」POST，返回 task_id。
//
// SubmitPost 把歧义传输错误转 AmbiguousSubmitError 终态失败，避免重试重复建任务 + 重复计费；
// >=400 抛 HTTPStatusError 交 SubmitRetryable 按状态码分流。
func (b *minimaxVideoBackend) createTask(ctx context.Context, payload map[string]any) (string, error) {
	var taskID string
	err := provider.RunWithRetry(ctx, provider.DefaultRetryPolicy, provider.SubmitRetryable, func(ctx context.Context) error {
		resp, err := SubmitPost(ctx, minimaxProviderID, func(ctx context.Context) (*httpc.Response, error) {
			return b.client.PostJSON(ctx, httpc.JoinURL(b.baseURL, minimaxVideoSubmitEndpoint), minimaxHeaders(b.apiKey), payload)
		})
		if err != nil {
			return err
		}
		var body map[string]any
		if err := decodeJSON(resp.Body, &body); err != nil {
			return err
		}
		id, err := minimaxExtractVideoTaskID(body)
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

// minimaxExtractVideoTaskID 从提交响应提取顶层 task_id；缺失则按 base_resp 错误或原样抛出。
func minimaxExtractVideoTaskID(payload map[string]any) (string, error) {
	taskID := minimaxRawString(payload["task_id"])
	if taskID != "" {
		return taskID, nil
	}
	if reason := minimaxBaseRespError(payload); reason != "" {
		return "", errors.New(reason)
	}
	return "", fmt.Errorf("MiniMax 视频提交响应缺少 task_id")
}

// minimaxBaseRespError 在 base_resp.status_code 非零时返回错误描述；0 或缺失返回空串。
func minimaxBaseRespError(payload map[string]any) string {
	base := minimaxAsDict(payload["base_resp"])
	code, ok := base["status_code"]
	if !ok || code == nil {
		return ""
	}
	if f, isNum := code.(float64); isNum && f == 0 {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf(
		"MiniMax base_resp status_code=%s: %s", minimaxRawString(code), minimaxRawString(base["status_msg"]),
	))
}

// pollQuery 发起幂等 GET 查询任务状态。
//
// v2 把 task_id 放路径段，v1 放 query string。task_id 来自上游响应/持久化记录，非本地生成
// 的受控值，编码后再拼入，避免被改写指向非预期端点。
func (b *minimaxVideoBackend) pollQuery(ctx context.Context, taskID string) (map[string]any, error) {
	target := httpc.JoinURL(b.baseURL, minimaxVideoQueryEndpoint)
	if b.isV2 {
		target = httpc.JoinURL(b.baseURL, minimaxVideoQueryEndpoint+"/"+url.PathEscape(taskID))
	} else {
		target = target + "?task_id=" + url.QueryEscape(taskID)
	}
	resp, err := b.client.Get(ctx, target, minimaxHeaders(b.apiKey))
	if err != nil {
		return nil, err
	}
	var payload any
	if err := decodeJSON(resp.Body, &payload); err != nil {
		return nil, err
	}
	return minimaxAsDict(payload), nil
}

// retrieveDownloadURL 取回 v1 的 download_url（幂等 GET，不计费，瞬态错误重试无副作用）。
func (b *minimaxVideoBackend) retrieveDownloadURL(ctx context.Context, fileID string) (string, error) {
	var downloadURL string
	err := provider.RunWithRetry(ctx, provider.DefaultRetryPolicy, provider.PollRetryable, func(ctx context.Context) error {
		target := httpc.JoinURL(b.baseURL, minimaxVideoRetrieveEndpoint) + "?file_id=" + url.QueryEscape(fileID)
		resp, err := b.client.Get(ctx, target, minimaxHeaders(b.apiKey))
		if err != nil {
			return err
		}
		var body map[string]any
		if err := decodeJSON(resp.Body, &body); err != nil {
			return err
		}
		found, err := minimaxExtractRetrieveURL(body)
		if err != nil {
			return err
		}
		downloadURL = found
		return nil
	})
	if err != nil {
		return "", err
	}
	return downloadURL, nil
}

// minimaxExtractRetrieveURL 从 files/retrieve 响应提取 file.download_url。
func minimaxExtractRetrieveURL(payload map[string]any) (string, error) {
	value := minimaxRawString(minimaxAsDict(payload["file"])["download_url"])
	if value != "" {
		return value, nil
	}
	if reason := minimaxBaseRespError(payload); reason != "" {
		return "", errors.New(reason)
	}
	return "", errors.New("MiniMax files/retrieve 响应缺少 download_url")
}

// pollAndBuild 轮询至终态，取件并下载成片。
func (b *minimaxVideoBackend) pollAndBuild(
	ctx context.Context, taskID string, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	isV2 := b.isV2
	final, err := PollWithRetry(ctx, PollConfig[map[string]any]{
		PollFn: func(ctx context.Context) (map[string]any, error) {
			return b.pollQuery(ctx, taskID)
		},
		IsDone: func(payload map[string]any) bool {
			if isV2 {
				return minimaxV2VideoTerminal(payload)
			}
			return minimaxVideoTerminal(payload)
		},
		IsFailed: func(payload map[string]any) string {
			if isV2 {
				return minimaxV2VideoFailureReason(payload)
			}
			return minimaxVideoFailureReason(payload)
		},
		PollInterval: minimaxVideoPollInterval,
		MaxWait:      minimaxVideoMaxWait(req.DurationSeconds),
		Label:        "MiniMax",
		OnProgress: func(payload map[string]any, elapsed time.Duration) {
			status := minimaxRawString(payload["status"])
			if isV2 {
				status = minimaxV2VideoStatus(payload)
			}
			slog.Info("MiniMax 视频生成中...", "status", status, "elapsed", int(elapsed.Seconds()))
		},
	})
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}

	var downloadURL string
	if isV2 {
		// v2 查询响应直接带限时下载地址，没有 file_id → files/retrieve 这一步。
		downloadURL, err = minimaxExtractV2DownloadURL(final)
	} else {
		fileID, extractErr := minimaxExtractFileID(final)
		if extractErr != nil {
			return provider.VideoGenerationResult{}, extractErr
		}
		downloadURL, err = b.retrieveDownloadURL(ctx, fileID)
	}
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}

	// 下载与生成分离：下载走自带退避重试的 DownloadVideo，失败不会回退到重跑生成 POST。
	if err := DownloadVideo(ctx, downloadURL, req.OutputPath, 0); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	slog.Info("MiniMax 视频下载完成", "output", req.OutputPath)

	generateAudio := req.GenerateAudio
	return provider.VideoGenerationResult{
		VideoPath:       req.OutputPath,
		Provider:        minimaxProviderID,
		Model:           b.model,
		DurationSeconds: req.DurationSeconds,
		VideoURI:        downloadURL,
		TaskID:          taskID,
		GenerateAudio:   &generateAudio,
	}, nil
}

// minimaxVideoTerminal v1 终态：Success / Fail，其余（含未知/空）继续轮询。
func minimaxVideoTerminal(payload map[string]any) bool {
	switch minimaxRawString(payload["status"]) {
	case "Success", "Fail":
		return true
	}
	return false
}

// minimaxVideoFailureReason v1：status=Fail 或查询接口顶层 base_resp 硬错误 → 错误描述。
func minimaxVideoFailureReason(payload map[string]any) string {
	if minimaxRawString(payload["status"]) == "Fail" {
		base := minimaxAsDict(payload["base_resp"])
		return strings.TrimSpace(fmt.Sprintf(
			"MiniMax 视频任务失败 task_id=%s status_code=%s: %s",
			minimaxRawString(payload["task_id"]), minimaxRawString(base["status_code"]), minimaxRawString(base["status_msg"]),
		))
	}
	return minimaxBaseRespError(payload)
}

// minimaxExtractFileID 从 status=Success 的查询响应提取 file_id。
func minimaxExtractFileID(payload map[string]any) (string, error) {
	fileID := minimaxRawString(payload["file_id"])
	if fileID == "" {
		return "", errors.New("MiniMax 视频任务完成但缺少 file_id")
	}
	return fileID, nil
}

// minimaxV2VideoStatus v2 查询响应 task.status（queued/running/succeeded/failed/cancelled）。
func minimaxV2VideoStatus(payload map[string]any) string {
	return minimaxRawString(minimaxAsDict(payload["task"])["status"])
}

// minimaxV2VideoTerminal v2 终态：succeeded / failed / cancelled，其余继续轮询。
func minimaxV2VideoTerminal(payload map[string]any) bool {
	switch minimaxV2VideoStatus(payload) {
	case "succeeded", "failed", "cancelled":
		return true
	}
	return false
}

// minimaxV2VideoFailureReason v2：status=failed/cancelled 或顶层 base_resp 硬错误 → 错误描述。
//
// 取消态一并按失败处理：任务不会再产出视频，继续轮询只会等到超时。
func minimaxV2VideoFailureReason(payload map[string]any) string {
	status := minimaxV2VideoStatus(payload)
	if status == "failed" || status == "cancelled" {
		task := minimaxAsDict(payload["task"])
		detail := minimaxRawString(task["error"])
		if detail == "" {
			detail = minimaxRawString(task["status_msg"])
		}
		return strings.TrimSpace(fmt.Sprintf(
			"MiniMax 视频任务%s task_id=%s: %s", status, minimaxRawString(task["id"]), detail,
		))
	}
	return minimaxBaseRespError(payload)
}

// minimaxExtractV2DownloadURL 从 status=succeeded 的查询响应提取 task.content.url。
func minimaxExtractV2DownloadURL(payload map[string]any) (string, error) {
	value := minimaxRawString(minimaxAsDict(minimaxAsDict(payload["task"])["content"])["url"])
	if value == "" {
		return "", errors.New("MiniMax 视频任务完成但缺少 content.url")
	}
	return value, nil
}

// minimaxVideoMaxWait 轮询上限：max(900, duration*60) 秒。
func minimaxVideoMaxWait(durationSeconds int) time.Duration {
	seconds := minimaxVideoMinPollTimeoutSeconds
	if per := float64(durationSeconds) * minimaxVideoPollTimeoutPerSecond; per > seconds {
		seconds = per
	}
	return time.Duration(seconds * float64(time.Second))
}

func init() {
	provider.RegisterVideoBackend(minimaxProviderID, provider.VideoBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.VideoBackend, error) {
			return newMinimaxVideoBackend(opts)
		},
		CapabilitiesForModel: minimaxVideoCapabilitiesForModel,
	})
}