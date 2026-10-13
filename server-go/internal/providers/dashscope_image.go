package providers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// 阿里百炼 DashScope (Model Studio) 图像后端（端口自 lib/image_backends/dashscope.py）。
//
// 走原生 multimodal-generation/generation 同步端点，T2I 与 I2I 共用同一请求体，只差 content
// 是否含 image 元素。覆盖 qwen-image-2.0 融合系列、qwen-image-edit 编辑系列与 wan2.7-image 系列。
//
// 本文件同时承载三家 dashscope backend（图像 / 视频 / 音频）共用的 host 派生、鉴权头、任务状态
// 判定与失败原因工具，与 Python 侧 lib/dashscope_shared.py 对应。

// dashscopeProviderName 与 registry 的 provider_id 同名。
const dashscopeProviderName = "dashscope"

// dashscopeDefaultBaseURL 是百炼 host 段（不含路径后缀），北京地域起点。
const dashscopeDefaultBaseURL = "https://dashscope.aliyuncs.com"

// dashscopeKnownSuffixes 是派生 host 时剥除的已知路径后缀，容忍用户填入完整 base（含地域切换）。
var dashscopeKnownSuffixes = []string{"/compatible-mode/v1", "/api/v1"}

// dashscopeHost 从配置的 base_url 提取 host 段（剥除已知路径后缀），缺省回落北京 host。
//
// 先 TrimSpace 再判空：纯空白串是真值会绕过空判定，回落必须在 trim 之后，否则 base 变空串、
// 派生出 "/api/v1" 这类非法相对 URL。
func dashscopeHost(configured string) string {
	base := strings.TrimSpace(configured)
	if base == "" {
		base = dashscopeDefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	for _, suffix := range dashscopeKnownSuffixes {
		if strings.HasSuffix(base, suffix) {
			return base[:len(base)-len(suffix)]
		}
	}
	return base
}

// dashscopeNativeBaseURL 是原生（图像 / 视频 / 音频）base：{host}/api/v1。
func dashscopeNativeBaseURL(configured string) string {
	return dashscopeHost(configured) + "/api/v1"
}

// dashscopeHeaders 构造 Bearer 鉴权头；视频异步须带 X-DashScope-Async: enable，同步不带。
func dashscopeHeaders(apiKey string, asyncMode bool) map[string]string {
	headers := map[string]string{
		"Authorization": "Bearer " + apiKey,
		"Content-Type":  "application/json",
	}
	if asyncMode {
		headers["X-DashScope-Async"] = "enable"
	}
	return headers
}

// 任务状态机（图像 / 视频异步两步式）。
const (
	dashscopeStatusSucceeded = "SUCCEEDED"
	dashscopeStatusFailed    = "FAILED"
	dashscopeStatusCanceled  = "CANCELED"
	// dashscopeStatusUnknown：task_id 超过 24h 有效期后查询返回 UNKNOWN（resume 路径据此判过期）。
	dashscopeStatusUnknown = "UNKNOWN"
)

// dashscopeTaskStatus 取出 output.task_status；上游给出非 dict 结构时归一化为空串。
func dashscopeTaskStatus(payload map[string]any) string {
	output, _ := payload["output"].(map[string]any)
	status, _ := output["task_status"].(string)
	return status
}

// dashscopeIsTerminal 判断是否终态（SUCCEEDED / FAILED / CANCELED / UNKNOWN），停止轮询。
func dashscopeIsTerminal(payload map[string]any) bool {
	switch dashscopeTaskStatus(payload) {
	case dashscopeStatusSucceeded, dashscopeStatusFailed, dashscopeStatusCanceled, dashscopeStatusUnknown:
		return true
	}
	return false
}

// dashscopeIsExpired 判断 task_id 是否过期（24h）——查询返回 UNKNOWN；resume 路径据此判过期。
func dashscopeIsExpired(payload map[string]any) bool {
	return dashscopeTaskStatus(payload) == dashscopeStatusUnknown
}

// dashscopeFailureReason 返回 FAILED / CANCELED 的错误描述，或提交阶段顶层错误描述；其余情形空串。
//
// UNKNOWN 不算失败（由 expired 单独处理）。提交阶段顶层错误响应（{code, message, request_id}
// 无 output）也在此兜底。
func dashscopeFailureReason(payload map[string]any) string {
	output, _ := payload["output"].(map[string]any)
	statusVal, hasStatus := output["task_status"]
	status, _ := statusVal.(string)
	if status == dashscopeStatusFailed || status == dashscopeStatusCanceled {
		code := "unknown"
		if c, ok := output["code"].(string); ok && c != "" {
			code = c
		}
		message, _ := output["message"].(string)
		return strings.TrimSpace(fmt.Sprintf("DashScope 任务失败 status=%s code=%s: %s", status, code, message))
	}
	if !hasStatus || statusVal == nil {
		if code, ok := payload["code"].(string); ok && code != "" {
			message, _ := payload["message"].(string)
			return strings.TrimSpace(fmt.Sprintf("DashScope 提交失败 code=%s: %s", code, message))
		}
	}
	return ""
}

// dashscopeReadImageURI 把本地图片读成 data URI；非普通文件或读取失败返回 ok=false。
func dashscopeReadImageURI(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	uri, err := imageToBase64DataURI(path)
	if err != nil {
		slog.Warn("DashScope 图片读取失败", "path", path, "error", err)
		return "", false
	}
	return uri, true
}

// dashscopeExtractImageURL 从同步图像响应 output.choices[0].message.content[*].image 取首个 URL。
func dashscopeExtractImageURL(body []byte) (string, error) {
	var payload map[string]any
	if err := decodeJSON(body, &payload); err != nil {
		return "", err
	}
	output, _ := payload["output"].(map[string]any)
	choices, _ := output["choices"].([]any)
	if len(choices) == 0 {
		if reason := dashscopeFailureReason(payload); reason != "" {
			return "", errors.New(reason)
		}
		return "", fmt.Errorf("DashScope 图像响应缺少 choices: %v", payload)
	}
	choice, _ := choices[0].(map[string]any)
	// 上游异常结构（choices[0] / message 非 dict）归一化为空 dict，避免 .get 等取键失败。
	message, _ := choice["message"].(map[string]any)
	content, _ := message["content"].([]any)
	for _, item := range content {
		if m, ok := item.(map[string]any); ok {
			if url, ok := m["image"].(string); ok && url != "" {
				return url, nil
			}
		}
	}
	return "", fmt.Errorf("DashScope 图像响应 content 无 image 字段: %v", payload)
}

// DashScope 图像生成相关常量。
const (
	dashscopeImageDefaultModel = "qwen-image-2.0"

	dashscopeImageEndpoint = "/services/aigc/multimodal-generation/generation"

	// 参考图上限：qwen 系 1~3 张、wan 系 0~9 张。
	dashscopeQwenRefLimit = 3
	dashscopeWanRefLimit  = 9

	// 缺分辨率时各族默认短边。比例永远来自 aspect_ratio，不再用带比例偏差的静态像素表，
	// 改由 domain.AspectSize 在各族像素约束内算精确比例。
	dashscopeDefaultWanBudget = "2K"
	dashscopeDefaultShortWan  = 1440
	dashscopeDefaultShortEdit = 2048
	// qwen 融合系列 native 默认 2048²。
	dashscopeDefaultShortFusion = 2048

	// 标准档总像素预算（非 pro / 非文生图上限）= 2048×2048；4K=4096×4096（仅 wan2.7-image-pro 文生图）。
	dashscopeStandardPixelBudget = 2048 * 2048
	dashscopeFourkPixelBudget    = 4096 * 4096
	// 编辑系列（qwen-image-edit-plus / -max）宽高均 ∈ [512, 2048]。
	dashscopeEditMaxLongEdge = 2048
	// 生成系列比例支持区间 1:8 ~ 8:1。
	dashscopeMaxRatio = 8.0
	// 编辑系列宽高均 ∈ [512, 2048]，可表达比例上限为 2048/512 = 4:1（比生成系列更窄）。
	dashscopeEditMaxRatio = 4.0

	// dashscopeImageRequestTimeout 是图像后端单次请求的墙钟预算（Python http_timeout 默认 120s）。
	dashscopeImageRequestTimeout = 120 * time.Second
)

// dashscopeImageBackend 是阿里百炼图像后端（同步 multimodal 端点）。
type dashscopeImageBackend struct {
	apiKey  string
	baseURL string
	model   string
	client  *httpc.Client
	isWan   bool
	isEdit  bool
	caps    provider.ImageCapabilitySet
}

// newDashscopeImageBackend 按通用信封构造；model / base_url 留空时由本后端兜底。
func newDashscopeImageBackend(opts provider.BackendOptions) (*dashscopeImageBackend, error) {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = dashscopeImageDefaultModel
	}
	mid := strings.ToLower(model)
	return &dashscopeImageBackend{
		apiKey:  opts.APIKey,
		baseURL: dashscopeNativeBaseURL(opts.BaseURL),
		model:   model,
		client:  httpc.NewClient(dashscopeImageRequestTimeout),
		isWan:   strings.HasPrefix(mid, "wan"),
		isEdit:  strings.Contains(mid, "qwen-image-edit"),
		caps:    dashscopeImageCapsForModel(model),
	}, nil
}

// dashscopeImageCapsForModel 按 model 声明能力位：编辑系列仅图生图，其余融合系列两者皆可。
func dashscopeImageCapsForModel(model string) provider.ImageCapabilitySet {
	mid := strings.ToLower(model)
	if strings.Contains(mid, "qwen-image-edit") {
		return provider.NewImageCapabilitySet(provider.CapImageToImage)
	}
	return provider.NewImageCapabilitySet(provider.CapTextToImage, provider.CapImageToImage)
}

// Name 返回供应商标识。
func (b *dashscopeImageBackend) Name() string { return dashscopeProviderName }

// Model 返回实际下发的模型名。
func (b *dashscopeImageBackend) Model() string { return b.model }

// Capabilities 返回本实例的能力位。
func (b *dashscopeImageBackend) Capabilities() provider.ImageCapabilitySet { return b.caps }

// refLimit 返回参考图上限：wan 系 9 张、qwen 系 3 张。
func (b *dashscopeImageBackend) refLimit() int {
	if b.isWan {
		return dashscopeWanRefLimit
	}
	return dashscopeQwenRefLimit
}

// Generate 生成一张图片并落盘到 req.OutputPath。
//
// 能力闸门先于请求：不支持的路径直接返回 *provider.CapabilityError，不发请求、不计费。
func (b *dashscopeImageBackend) Generate(
	ctx context.Context, req provider.ImageGenerationRequest,
) (provider.ImageGenerationResult, error) {
	hasRefs := len(req.ReferenceImages) > 0
	if hasRefs && !b.caps.Has(provider.CapImageToImage) {
		return provider.ImageGenerationResult{}, provider.NewImageCapabilityError(
			provider.FailureImageEndpointMismatchNoI2I, map[string]any{"model": b.model})
	}
	if !hasRefs && !b.caps.Has(provider.CapTextToImage) {
		return provider.ImageGenerationResult{}, provider.NewImageCapabilityError(
			provider.FailureImageEndpointMismatchNoT2I, map[string]any{"model": b.model})
	}

	size, err := b.resolveSize(req, hasRefs)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	content, err := b.buildContent(req, hasRefs)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}

	parameters := map[string]any{
		"n":         1,
		"watermark": false,
		// Shotwise 剧本 prompt 已是 LLM 精炼描述，关闭智能改写保留原意。
		"prompt_extend": false,
		"size":          size,
	}
	if req.Seed != nil {
		parameters["seed"] = *req.Seed
	}

	payload := map[string]any{
		"model": b.model,
		"input": map[string]any{
			"messages": []map[string]any{{"role": "user", "content": content}},
		},
		"parameters": parameters,
	}

	slog.Info("调用 DashScope 图片 API", "model", b.model, "size", size)

	// 同步图像生成是非幂等的「建图 + 计费」POST：SubmitPost 把歧义传输错误转 AmbiguousSubmitError
	// 终态失败，避免自动重试重复计费；HTTP 状态错误交 SubmitRetryable 按状态码分流——4xx fail-fast、
	// 5xx/429 重试。
	var resp *httpc.Response
	err = provider.RunWithRetry(ctx, provider.DefaultRetryPolicy, provider.SubmitRetryable,
		func(ctx context.Context) error {
			r, err := SubmitPost(ctx, dashscopeProviderName, func(ctx context.Context) (*httpc.Response, error) {
				return b.client.PostJSON(ctx, b.baseURL+dashscopeImageEndpoint, dashscopeHeaders(b.apiKey, false), payload)
			})
			if err != nil {
				return err
			}
			resp = r
			return nil
		})
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}

	url, err := dashscopeExtractImageURL(resp.Body)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	if err := downloadImageToPath(ctx, b.client, url, req.OutputPath); err != nil {
		return provider.ImageGenerationResult{}, err
	}
	slog.Info("DashScope 图片生成完成", "output", req.OutputPath)

	return provider.ImageGenerationResult{
		ImagePath: req.OutputPath,
		Provider:  dashscopeProviderName,
		Model:     b.model,
		ImageURI:  url,
	}, nil
}

// resolveSize 按「比例优先、清晰度其次」算出「宽*高」。
//
// 比例永远来自 aspect_ratio；image_size（档位词 / 自定义 宽*高 / 空）只决定清晰度短边。各族按
// 自身像素约束精确收口：wan 系总像素 ≤ 标准 2048² / 4K 4096²；qwen 融合系列总像素 ∈ [512², 2048²]；
// 编辑系列宽高均 ∈ [512, 2048]。
func (b *dashscopeImageBackend) resolveSize(
	req provider.ImageGenerationRequest, hasRefs bool,
) (string, error) {
	explicit := strings.TrimSpace(req.ImageSize)
	aspect := req.AspectRatio

	if b.isWan {
		budgetWord := explicit
		if budgetWord == "" {
			budgetWord = dashscopeDefaultWanBudget
		}
		exceeds := dashscopeExceedsStandardBudget(budgetWord)
		if exceeds && (!strings.Contains(strings.ToLower(b.model), "pro") || hasRefs) {
			return "", provider.NewImageCapabilityError(
				provider.FailureImageDashscope4KT2IOnly, map[string]any{"model": b.model})
		}
		maxTotal := dashscopeStandardPixelBudget
		if exceeds {
			maxTotal = dashscopeFourkPixelBudget
		}
		short := domain.ResolutionToShortEdge(explicit, domain.ImageTierShortEdge, dashscopeDefaultShortWan)
		ratio := dashscopeMaxRatio
		w, h := domain.AspectSize(aspect, short, domain.AspectSizeOptions{
			RoundTo: 16, MaxTotalPixels: &maxTotal, MaxRatio: &ratio,
		})
		return fmt.Sprintf("%d*%d", w, h), nil
	}

	if b.isEdit {
		short := domain.ResolutionToShortEdge(explicit, domain.ImageTierShortEdge, dashscopeDefaultShortEdit)
		maxLong := dashscopeEditMaxLongEdge
		ratio := dashscopeEditMaxRatio
		w, h := domain.AspectSize(aspect, short, domain.AspectSizeOptions{
			RoundTo: 16, MaxLongEdge: &maxLong, MaxRatio: &ratio,
		})
		return fmt.Sprintf("%d*%d", w, h), nil
	}

	// qwen-image-2.0 融合系列。
	short := domain.ResolutionToShortEdge(explicit, domain.ImageTierShortEdge, dashscopeDefaultShortFusion)
	maxTotal := dashscopeStandardPixelBudget
	ratio := dashscopeMaxRatio
	w, h := domain.AspectSize(aspect, short, domain.AspectSizeOptions{
		RoundTo: 16, MaxTotalPixels: &maxTotal, MaxRatio: &ratio,
	})
	return fmt.Sprintf("%d*%d", w, h), nil
}

// dashscopeExceedsStandardBudget 判断 size 是否超出标准档总像素预算（2048×2048）。
//
// 档位「1K」/「2K」在预算内、「4K」超预算；像素值按「总像素 > 预算」判定（故 "4096*512" 这类窄幅
// 合法尺寸不会被误拒），避免只认 "4K" 字面而让数字写法绕过门控。
func dashscopeExceedsStandardBudget(size string) bool {
	normalized := strings.ToUpper(strings.TrimSpace(size))
	if normalized == "1K" || normalized == "2K" {
		return false
	}
	if normalized == "4K" {
		return true
	}
	for _, sep := range []string{"*", "X", "×"} {
		if strings.Contains(normalized, sep) {
			parts := strings.SplitN(normalized, sep, 2)
			w, errW := strconv.Atoi(parts[0])
			h, errH := strconv.Atoi(parts[1])
			if errW != nil || errH != nil {
				return false
			}
			return w*h > dashscopeStandardPixelBudget
		}
	}
	return false
}

// buildContent 组装 messages content：参考图以 base64 data URI 内联，末尾追加文本。
//
// fail-loud：任一声明的参考图缺失（含空串解析出的 "#N"）或读取失败即中止并列出文件名，让用户感知
// 到有图未被使用，而非静默丢弃、用子集生成出错误结果还照常计费。
func (b *dashscopeImageBackend) buildContent(
	req provider.ImageGenerationRequest, hasRefs bool,
) ([]map[string]any, error) {
	content := make([]map[string]any, 0, len(req.ReferenceImages)+1)
	if hasRefs {
		dataURIs := make([]string, 0, len(req.ReferenceImages))
		unreadable := make([]string, 0)
		for idx, ref := range req.ReferenceImages {
			if strings.TrimSpace(ref.Path) == "" {
				// 空路径无文件名可显示，用序号 #N 标识第几张参考图。
				unreadable = append(unreadable, fmt.Sprintf("#%d", idx+1))
				continue
			}
			uri, ok := dashscopeReadImageURI(ref.Path)
			if !ok {
				unreadable = append(unreadable, filepath.Base(ref.Path))
				continue
			}
			dataURIs = append(dataURIs, uri)
		}
		if len(unreadable) > 0 {
			return nil, provider.NewImageCapabilityError(
				provider.FailureImageReferenceImagesUnreadable,
				map[string]any{"model": b.model, "names": strings.Join(unreadable, ", ")})
		}
		if len(dataURIs) > b.refLimit() {
			slog.Warn("DashScope 参考图数量超过上限，截断",
				"count", len(dataURIs), "model", b.model, "limit", b.refLimit())
			dataURIs = dataURIs[:b.refLimit()]
		}
		for _, uri := range dataURIs {
			content = append(content, map[string]any{"image": uri})
		}
	}
	content = append(content, map[string]any{"text": req.Prompt})
	return content, nil
}

func init() {
	provider.RegisterImageBackend(dashscopeProviderName, provider.ImageBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.ImageBackend, error) {
			return newDashscopeImageBackend(opts)
		},
	})
}
