package vidu

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/providers/provkit"
)

// Vidu 视频后端专属常量。
const (
	// viduVideoPollInterval 是视频任务轮询间隔。
	viduVideoPollInterval = 5 * time.Second
	// viduVideoMinPollTimeout 是轮询最大等待时长的下限。
	viduVideoMinPollTimeout = 900 * time.Second
	// viduVideoPollTimeoutPerSecond 是每 1 秒成片换算的轮询预算（秒）。
	viduVideoPollTimeoutPerSecond = 90.0
	// viduVideoPromptMaxT2V 是多端点视频请求的提示词字符上限。
	viduVideoPromptMaxT2V = 5000
	// viduVideoPromptMaxR2V 是参考生视频端点的提示词字符上限（比模型级上限更窄）。
	viduVideoPromptMaxR2V = 2000
	// viduVideoDefaultResolution 是分辨率兜底默认值。
	viduVideoDefaultResolution = "720p"
)

// viduQueryModels 是 q3 系列模型：默认开启音视频直出，audio 仅对 q3 生效。
var viduQueryModels = map[string]bool{
	"viduq3-pro":      true,
	"viduq3-turbo":    true,
	"viduq3-pro-fast": true,
	"viduq3":          true,
	"viduq3-mix":      true,
}

// viduDurationRules 按 (model, endpoint) 列出合法 duration —— 文档逐端点列出，差异很大。
var viduDurationRules = map[[2]string][]int{
	// /text2video
	{"viduq3-pro", "/text2video"}:   viduRange(1, 16),
	{"viduq3-turbo", "/text2video"}: viduRange(1, 16),
	{"viduq2", "/text2video"}:       viduRange(1, 11),
	{"viduq1", "/text2video"}:       {5},
	// /img2video
	{"viduq3-pro", "/img2video"}:      viduRange(1, 16),
	{"viduq3-turbo", "/img2video"}:    viduRange(1, 16),
	{"viduq3-pro-fast", "/img2video"}: viduRange(1, 16),
	// q2 系在 /img2video 为 1–10；1–8 是 /start-end2video（首尾帧）端点的值，两端点不同。
	{"viduq2-pro-fast", "/img2video"}: viduRange(1, 11),
	{"viduq2-pro", "/img2video"}:      viduRange(1, 11),
	{"viduq2-turbo", "/img2video"}:    viduRange(1, 11),
	{"viduq1", "/img2video"}:          {5},
	{"viduq1-classic", "/img2video"}:  {5},
	{"vidu2.0", "/img2video"}:         {4, 8},
	// /start-end2video
	{"viduq3-pro", "/start-end2video"}:      viduRange(1, 16),
	{"viduq3-turbo", "/start-end2video"}:    viduRange(1, 16),
	{"viduq2-pro-fast", "/start-end2video"}: viduRange(1, 9),
	{"viduq2-pro", "/start-end2video"}:      viduRange(1, 9),
	{"viduq2-turbo", "/start-end2video"}:    viduRange(1, 9),
	{"viduq1", "/start-end2video"}:          {5},
	{"viduq1-classic", "/start-end2video"}:  {5},
	{"vidu2.0", "/start-end2video"}:         {4, 8},
	// /reference2video（非主体）
	{"viduq3-turbo", "/reference2video"}: viduRange(3, 16),
	{"viduq3", "/reference2video"}:       viduRange(3, 16),
	{"viduq3-mix", "/reference2video"}:   viduRange(3, 16),
	{"viduq2-pro", "/reference2video"}:   viduRange(0, 11), // 0=自动
	{"viduq2", "/reference2video"}:       viduRange(1, 11),
	{"viduq1", "/reference2video"}:       {5},
	{"vidu2.0", "/reference2video"}:      {4},
}

// viduEndpointModels 是端点支持的模型集合 —— 用于早期失败提示（依据 Vidu 官方文档）。
var viduEndpointModels = map[string]map[string]bool{
	"/text2video": {
		"viduq3-turbo": true, "viduq3-pro": true, "viduq2": true, "viduq1": true,
	},
	"/img2video": {
		"viduq3-turbo": true, "viduq3-pro": true, "viduq3-pro-fast": true,
		"viduq2-pro-fast": true, "viduq2-pro": true, "viduq2-turbo": true,
		"viduq1": true, "viduq1-classic": true, "vidu2.0": true,
	},
	"/start-end2video": {
		"viduq3-turbo": true, "viduq3-pro": true,
		"viduq2-pro-fast": true, "viduq2-pro": true, "viduq2-turbo": true,
		"viduq1": true, "viduq1-classic": true, "vidu2.0": true,
	},
	"/reference2video": {
		"viduq3-mix": true, "viduq3-turbo": true, "viduq3": true,
		"viduq2-pro": true, "viduq2": true, "viduq1": true, "vidu2.0": true,
	},
}

// viduEndpointsWithAspectRatio 是接受 aspect_ratio 的端点集合。
var viduEndpointsWithAspectRatio = map[string]bool{
	"/text2video":      true,
	"/reference2video": true,
}

// viduVideoResolutionWhitelist 按 model 列分辨率白名单（按 /img2video 与 /reference2video 的并集，
// 最宽松）——这是 backend 内独立于 registry 的执行期白名单。
var viduVideoResolutionWhitelist = map[string][]string{
	"viduq3-pro":      {"540p", "720p", "1080p"},
	"viduq3-pro-fast": {"540p", "720p", "1080p"},
	"viduq3-turbo":    {"540p", "720p", "1080p"},
	"viduq3":          {"540p", "720p", "1080p"},
	"viduq3-mix":      {"720p", "1080p"},
	"viduq2":          {"540p", "720p", "1080p"},
	"viduq2-pro":      {"540p", "720p", "1080p"},
	"viduq2-pro-fast": {"720p", "1080p"},
	"viduq2-turbo":    {"540p", "720p", "1080p"},
	"viduq1":          {"1080p"},
	"viduq1-classic":  {"1080p"},
	"vidu2.0":         {"360p", "720p", "1080p"},
}

// viduVideoBackend 是 Vidu 视频生成后端，按 request 字段分派到不同端点。
type viduVideoBackend struct {
	apiKey  string
	baseURL string
	model   string
	client  *httpc.Client
}

// NewVideoBackend 按通用信封构造；model / base_url 留空时由本后端兜底。
func NewVideoBackend(opts provider.BackendOptions) (*viduVideoBackend, error) {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = viduDefaultVideoModel
	}
	return &viduVideoBackend{
		apiKey:  opts.APIKey,
		baseURL: viduResolveBaseURL(opts.BaseURL),
		model:   model,
		client:  httpc.NewClient(viduClientTimeout),
	}, nil
}

// Name 返回供应商标识。
func (b *viduVideoBackend) Name() string { return viduProviderName }

// Model 返回实际下发的模型名。
func (b *viduVideoBackend) Model() string { return b.model }

// VideoCapabilities 返回本实例的能力声明。
func (b *viduVideoBackend) VideoCapabilities() provider.VideoCapabilities {
	return VideoCapabilitiesForModel(b.model)
}

// VideoCapabilitiesForModel 按 model_id 纯计算 caps —— 不构造 client。
//
// first_frame / last_frame / reference_images 直接查 viduEndpointModels 的端点白名单，与
// viduSelectEndpoint / buildRequest 实际能否派发到 /img2video、/start-end2video、/reference2video
// 同源；max_reference_images 随 reference_images 联动归零——不在 /reference2video 白名单的
// model 不可能提交参考图，声明非零容量会误导展示层。
//
// max_prompt_chars 无端点上下文，只能声明「该 model 无论走哪条端点都成立」的值：只有专供
// /reference2video 的 model 才按更窄值声明，其余取宽值；多端点 model 走 r2v 的窄值收窄改由
// buildRequest 按实际端点 fail-loud。未登记 model（中转自定义命名）不在任何白名单内，取宽值：
// 静态声明只该拒绝必然违约的请求。
func VideoCapabilitiesForModel(model string) provider.VideoCapabilities {
	referenceImages := viduEndpointModels["/reference2video"][model]
	firstFrame := viduEndpointModels["/img2video"][model]
	lastFrame := viduEndpointModels["/start-end2video"][model]

	maxRefs := 0
	if referenceImages {
		maxRefs = viduMaxReferenceImages
	}
	promptMax := viduVideoPromptMaxT2V
	if viduServesOnlyReference2Video(model) {
		promptMax = viduVideoPromptMaxR2V
	}
	return provider.VideoCapabilities{
		FirstFrame:         firstFrame,
		LastFrame:          lastFrame,
		MaxReferenceImages: maxRefs,
		ReferenceAudioMode: provider.ReferenceAudioNone,
		MaxPromptChars:     &promptMax,
	}
}

// viduServesOnlyReference2Video 判断该 model 除 /reference2video 外不派发到任何端点。
func viduServesOnlyReference2Video(model string) bool {
	if !viduEndpointModels["/reference2video"][model] {
		return false
	}
	for endpoint, models := range viduEndpointModels {
		if endpoint == "/reference2video" {
			continue
		}
		if models[model] {
			return false
		}
	}
	return true
}

// ResumeVideo：Vidu 不支持 resume（poll 完全内联在 Generate，没有可接续的独立轮询入口），
// orphan handler 据 ErrResumeUnsupported 标 [resume_unsupported]。
func (b *viduVideoBackend) ResumeVideo(
	ctx context.Context, jobID string, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	return provider.VideoGenerationResult{}, provider.ErrResumeUnsupported
}

// Generate 按请求字段分派端点、创建任务、轮询至终态并下载成片。
func (b *viduVideoBackend) Generate(
	ctx context.Context, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	headers, err := viduAuthHeaders(b.apiKey)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}

	endpoint, body, err := b.buildRequest(req)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	// buildRequest 已把 duration 归一化到 body["duration"]，统一使用以避免结果不一致。
	coercedDuration, _ := body["duration"].(int)
	maxWait := viduVideoMinPollTimeout
	if computed := time.Duration(float64(coercedDuration) * viduVideoPollTimeoutPerSecond * float64(time.Second)); computed > maxWait {
		maxWait = computed
	}

	payload, err := viduCreateTask(ctx, b.client, b.baseURL, endpoint, headers, body, "视频")
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	taskID, _ := viduTaskID(payload)
	// Vidu 不可 resume：不持久化 job_id。

	final, err := viduPollTask(ctx, b.client, b.baseURL, headers, taskID, viduVideoPollInterval, maxWait)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	url, err := viduExtractURL(final)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	// credits=0 是合法值，不能用「空即是无」合并；显式判 nil 兜底为顶层 credits。
	actual := viduCredits(final)
	if actual == nil {
		actual = viduCredits(payload)
	}

	if err := provkit.DownloadVideo(ctx, url, req.OutputPath, 0); err != nil {
		return provider.VideoGenerationResult{}, err
	}

	generateAudio := false
	if viduQueryModels[b.model] {
		generateAudio = req.GenerateAudio
	}
	result := provider.VideoGenerationResult{
		VideoPath:       req.OutputPath,
		Provider:        viduProviderName,
		Model:           b.model,
		DurationSeconds: coercedDuration,
		Seed:            req.Seed,
		TaskID:          taskID,
		GenerateAudio:   &generateAudio,
	}
	if actual != nil {
		tokens := int64(*actual)
		result.UsageTokens = &tokens
	}
	return result, nil
}

// buildRequest 选择端点并组装请求体。
func (b *viduVideoBackend) buildRequest(req provider.VideoGenerationRequest) (string, map[string]any, error) {
	endpoint, err := viduSelectEndpoint(req)
	if err != nil {
		return "", nil, err
	}

	// 1) 校验模型 × 端点（提前失败比 400 含义更清晰）。
	if models := viduEndpointModels[endpoint]; len(models) > 0 && !models[b.model] {
		return "", nil, fmt.Errorf(
			"Vidu 模型 %s 不支持 %s 端点；该端点支持: %v", b.model, endpoint, viduSortedModels(models),
		)
	}

	// 2) duration 在合法集合内取最近值。
	duration := viduCoerceDuration(b.model, endpoint, req.DurationSeconds)

	// 3) prompt 长度（参考生视频上限更窄）。超限一律拒绝、不客户端截断：截断会产出与意图不符的
	// 成片却照常计费。model 级上限已由 gate 在付费前拦下，此处是端点级的执行期防御。
	prompt := req.Prompt
	promptMax := viduVideoPromptMaxT2V
	if endpoint == "/reference2video" {
		promptMax = viduVideoPromptMaxR2V
	}
	if count := utf8.RuneCountInString(prompt); count > promptMax {
		return "", nil, provider.NewVideoCapabilityError(provider.FailureVideoPromptTooLong, map[string]any{
			"provider": viduProviderName,
			"model":    b.model,
			"limit":    promptMax,
			"count":    count,
		})
	}

	body := map[string]any{
		"model":    b.model,
		"prompt":   prompt,
		"duration": duration,
	}

	// 4) resolution 白名单兜底。
	if resolution := viduCoerceResolution(b.model, req.Resolution); resolution != "" {
		body["resolution"] = resolution
	}

	// 5) seed
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}

	// 6) audio 仅 q3 系列接受。
	if viduQueryModels[b.model] {
		body["audio"] = req.GenerateAudio
	}

	// 7) aspect_ratio 仅 text2video / reference2video 接受。
	if viduEndpointsWithAspectRatio[endpoint] && req.AspectRatio != "" {
		body["aspect_ratio"] = req.AspectRatio
	}

	// 8) images 按端点形态填充。
	switch endpoint {
	case "/reference2video":
		refs := make([]string, 0, len(req.ReferenceImages))
		for _, p := range req.ReferenceImages {
			if p != "" {
				refs = append(refs, p)
			}
		}
		if len(refs) > viduMaxReferenceImages {
			slog.Warn("Vidu 参考图数量超过上限，截断", "count", len(refs), "limit", viduMaxReferenceImages)
			refs = refs[:viduMaxReferenceImages]
		}
		images := make([]string, 0, len(refs))
		for _, p := range refs {
			uri, err := provkit.ImageToBase64DataURI(p)
			if err != nil {
				return "", nil, err
			}
			images = append(images, uri)
		}
		body["images"] = images
	case "/start-end2video":
		startURI, err := provkit.ImageToBase64DataURI(req.StartImage)
		if err != nil {
			return "", nil, err
		}
		endURI, err := provkit.ImageToBase64DataURI(req.EndImage)
		if err != nil {
			return "", nil, err
		}
		body["images"] = []string{startURI, endURI}
	case "/img2video":
		startURI, err := provkit.ImageToBase64DataURI(req.StartImage)
		if err != nil {
			return "", nil, err
		}
		body["images"] = []string{startURI}
		// /text2video 不带 images
	}

	return endpoint, body, nil
}

// viduSelectEndpoint 按请求字段选择端点；显式传入但不存在的 start/end 图直接失败，不静默降级。
func viduSelectEndpoint(req provider.VideoGenerationRequest) (string, error) {
	hasRefs := false
	for _, p := range req.ReferenceImages {
		if p != "" {
			hasRefs = true
			break
		}
	}
	if req.StartImage != "" {
		if _, err := os.Stat(req.StartImage); err != nil {
			return "", fmt.Errorf("start_image 文件不存在: %s", req.StartImage)
		}
	}
	if req.EndImage != "" {
		if _, err := os.Stat(req.EndImage); err != nil {
			return "", fmt.Errorf("end_image 文件不存在: %s", req.EndImage)
		}
	}
	hasStart := req.StartImage != ""
	hasEnd := req.EndImage != ""
	switch {
	case hasRefs:
		return "/reference2video", nil
	case hasStart && hasEnd:
		return "/start-end2video", nil
	case hasStart:
		return "/img2video", nil
	default:
		return "/text2video", nil
	}
}

// viduCoerceDuration 按 (model, endpoint) 的合法集合把请求时长校正到最近值。
func viduCoerceDuration(model, endpoint string, requested int) int {
	allowed := viduDurationRules[[2]string{model, endpoint}]
	if len(allowed) == 0 {
		// 表里无项时不强校（让接口按默认处理），但若给了值就透传。
		if requested != 0 {
			return requested
		}
		return 5
	}
	if viduContainsInt(allowed, requested) {
		return requested
	}
	nearest := allowed[0]
	for _, v := range allowed {
		if viduAbs(v-requested) < viduAbs(nearest-requested) {
			nearest = v
		}
	}
	slog.Warn("Vidu duration 不在合法集合，使用最近值",
		"duration", requested, "model", model, "endpoint", endpoint, "allowed", allowed, "nearest", nearest)
	return nearest
}

// viduCoerceResolution 白名单内透传，否则降级到模型默认 720p（viduq1 默认 1080p）。
func viduCoerceResolution(model, requested string) string {
	whitelist := viduVideoResolutionWhitelist[model]
	if len(whitelist) == 0 {
		return requested
	}
	if requested != "" && viduContains(whitelist, requested) {
		return requested
	}
	if requested != "" {
		slog.Warn("Vidu resolution 不在白名单，降级", "resolution", requested, "model", model, "whitelist", whitelist)
	}
	if viduContains(whitelist, viduVideoDefaultResolution) {
		return viduVideoDefaultResolution
	}
	return whitelist[0]
}

// viduRange 生成 [start, end) 的整数序列（对齐 Python range）。
func viduRange(start, end int) []int {
	out := make([]int, 0, end-start)
	for i := start; i < end; i++ {
		out = append(out, i)
	}
	return out
}

// viduContainsInt 判断整数切片是否含某值。
func viduContainsInt(values []int, target int) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// viduSortedModels 返回集合的字典序切片（对齐 Python sorted(set)）。
func viduSortedModels(models map[string]bool) []string {
	out := make([]string, 0, len(models))
	for model := range models {
		out = append(out, model)
	}
	sort.Strings(out)
	return out
}

// viduAbs 返回整数绝对值。
func viduAbs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func init() {
	provider.RegisterVideoBackend(viduProviderName, provider.VideoBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.VideoBackend, error) {
			return NewVideoBackend(opts)
		},
		CapabilitiesForModel: func(modelID string) (provider.VideoCapabilities, error) {
			return VideoCapabilitiesForModel(modelID), nil
		},
	})
}
