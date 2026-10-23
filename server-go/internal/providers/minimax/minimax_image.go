package minimax

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/providers/provkit"
)

// MiniMax（海螺）供应商标识与共享端点工具（图片 / 视频 backend 共用）。
//
// MiniMax 本身 OpenAI 兼容，单一 /v1 base，文本与原生视频端点共用同一 base；v2 多模态视频
// 端点与 /v1 同 host、不同版本前缀。本文件同时承载两家 backend 复用的 host 归一化与鉴权头。
const (
	// minimaxProviderID 与 registry 的 provider_id / 注册键同名。
	minimaxProviderID = "minimax"

	// minimaxDefaultBaseURL 国内站默认 base（含 /v1）；国际站 https://api.minimax.io/v1 经
	// 配置覆盖 base_url 切换。
	minimaxDefaultBaseURL = "https://api.minimaxi.com/v1"
	minimaxV1Suffix       = "/v1"
	minimaxV2Suffix       = "/v2"

	// minimaxImageDefaultModel 缺省图像模型。
	minimaxImageDefaultModel = "image-01"
	minimaxImageEndpoint     = "/image_generation"
	// minimaxImageDefaultTimeout 单次图像生成请求的墙钟预算（对应 Python http_timeout 默认 120s）。
	minimaxImageDefaultTimeout = 120 * time.Second

	// image-01 宽高均 ∈ [512, 2048] 且须被 8 整除；可表达的最极端比例为 2048/512 = 4:1。
	minimaxImageMinEdge  = 512
	minimaxImageMaxEdge  = 2048
	minimaxImageRoundTo  = 8
	minimaxImageMaxRatio = 4.0
	// minimaxImageDefaultShort 缺 image_size 时的默认短边（2K 档），经长边收口后竖屏得 1152*2048。
	minimaxImageDefaultShort = 1440
	// minimaxImageSubjectRefLimit image-01 单脸参考仅取首张参考图。
	minimaxImageSubjectRefLimit = 1
)

// minimaxResolveAPIKey 解析 Bearer API Key；缺失 / 纯空白即报错（不走 env fallback）。
func minimaxResolveAPIKey(apiKey string) (string, error) {
	key := strings.TrimSpace(apiKey)
	if key == "" {
		return "", errors.New("请到系统配置页填写 MiniMax API Key")
	}
	return key, nil
}

// minimaxHost 从配置的 base_url 提取 host 段（剥除 /v1 后缀），缺省回落国内站 host。
func minimaxHost(configured string) string {
	base := strings.TrimSpace(configured)
	if base == "" {
		base = minimaxDefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, minimaxV1Suffix) {
		return strings.TrimSuffix(base, minimaxV1Suffix)
	}
	return base
}

// minimaxTextBaseURL 文本（OpenAI 兼容）base：{host}/v1。
func minimaxTextBaseURL(configured string) string { return minimaxHost(configured) + minimaxV1Suffix }

// minimaxVideoBaseURL 原生视频端点 base：{host}/v1，与文本共用单一 base，仅命名区分用途。
func minimaxVideoBaseURL(configured string) string { return minimaxHost(configured) + minimaxV1Suffix }

// minimaxVideoV2BaseURL 多模态视频端点 base：{host}/v2，供 H3 的 content[] 端点使用。
func minimaxVideoV2BaseURL(configured string) string {
	return minimaxHost(configured) + minimaxV2Suffix
}

// minimaxHeaders Bearer 鉴权头。
func minimaxHeaders(apiKey string) map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + apiKey,
		"Content-Type":  "application/json",
	}
}

// minimaxHTTPTimeout 读取 Extra["http_timeout"]（秒）覆盖模板默认；缺失或非法回落 fallback。
func minimaxHTTPTimeout(opts provider.BackendOptions, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(opts.Extra["http_timeout"])
	if raw == "" {
		return fallback
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds * float64(time.Second))
}

// minimaxAsDict 把任意值归一化为 map：非 map（nil / list / str 等异常上游结构）回 nil，
// 避免对错误响应给出的非 dict 真值取字段。nil map 的读取在 Go 中安全。
func minimaxAsDict(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return nil
}

// minimaxWriteBase64Image 解码 base64 图片并写盘；容忍 data URI 前缀（先剥前缀再解码）。
func minimaxWriteBase64Image(b64, outputPath string) error {
	payload := b64
	if strings.HasPrefix(payload, "data:") {
		if idx := strings.Index(payload, ","); idx >= 0 {
			payload = payload[idx+1:]
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return fmt.Errorf("decode base64 image: %w", err)
	}
	return provkit.WriteFileAtomic(outputPath, decoded)
}

// minimaxImageBackend 是 MiniMax 图像后端（单步同步 image_generation 端点）。
//
// 单次 POST 直接返回图片 URL（24h 有效）或 base64，立即落地为本地资产。T2I 与 I2I 共用同一
// 请求体，I2I 经 subject_reference 单脸参考驱动多场景角色一致性立绘。
type minimaxImageBackend struct {
	model   string
	baseURL string
	apiKey  string
	client  *httpc.Client
}

// NewImageBackend 按通用信封构造：api_key 缺失即 fail loud；model / base_url 留空时由
// 本后端兜底（默认 model、国内站端点）。
func NewImageBackend(opts provider.BackendOptions) (*minimaxImageBackend, error) {
	apiKey, err := minimaxResolveAPIKey(opts.APIKey)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = minimaxImageDefaultModel
	}
	return &minimaxImageBackend{
		model:   model,
		baseURL: minimaxTextBaseURL(opts.BaseURL),
		apiKey:  apiKey,
		client:  httpc.NewClient(minimaxHTTPTimeout(opts, minimaxImageDefaultTimeout)),
	}, nil
}

// Name 返回供应商标识。
func (b *minimaxImageBackend) Name() string { return minimaxProviderID }

// Model 返回实际下发的模型名。
func (b *minimaxImageBackend) Model() string { return b.model }

// Capabilities 返回能力位：image-01 文生图 + 图生图（subject_reference）同模型。
func (b *minimaxImageBackend) Capabilities() provider.ImageCapabilitySet {
	return provider.NewImageCapabilitySet(provider.CapTextToImage, provider.CapImageToImage)
}

// Generate 生成一张图片并落盘到 req.OutputPath。
//
// 编排层不带重试：把非幂等的「建图 + 计费」提交与幂等的结果下载隔离到各自的重试范围，避免下载
// 失败回退到重跑生成 POST 造成重复计费。
func (b *minimaxImageBackend) Generate(
	ctx context.Context, req provider.ImageGenerationRequest,
) (provider.ImageGenerationResult, error) {
	width, height := b.resolveDimensions(req)
	payload := map[string]any{
		"model":           b.model,
		"prompt":          req.Prompt,
		"width":           width,
		"height":          height,
		"response_format": "url",
		"n":               1,
		// Shotwise 剧本 prompt 已是 LLM 精炼描述，关闭智能改写保留原意。
		"prompt_optimizer": false,
	}
	if req.Seed != nil {
		payload["seed"] = *req.Seed
	}
	if len(req.ReferenceImages) > 0 {
		refs, err := b.buildSubjectReference(req)
		if err != nil {
			return provider.ImageGenerationResult{}, err
		}
		payload["subject_reference"] = refs
	}

	data, err := b.submit(ctx, payload)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	imageURI, err := b.persistImage(ctx, data, req.OutputPath)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	slog.Info("MiniMax 图片生成完成", "output", req.OutputPath)
	return provider.ImageGenerationResult{
		ImagePath: req.OutputPath,
		Provider:  minimaxProviderID,
		Model:     b.model,
		ImageURI:  imageURI,
	}, nil
}

// submit 单步图像生成 POST（非幂等「建图 + 计费」），返回解析后的响应体。
//
// provkit.SubmitPost 把歧义传输错误转 AmbiguousSubmitError 终态失败避免重复计费；重试范围严格限定在
// 本方法内、不含下载——下载失败不会触发整流程重试导致重复建图与重复计费。
func (b *minimaxImageBackend) submit(
	ctx context.Context, payload map[string]any,
) (map[string]any, error) {
	var parsed map[string]any
	err := provider.RunWithRetry(ctx, provider.DefaultRetryPolicy, provider.SubmitRetryable, func(ctx context.Context) error {
		resp, err := provkit.SubmitPost(ctx, minimaxProviderID, func(ctx context.Context) (*httpc.Response, error) {
			return b.client.PostJSON(ctx, httpc.JoinURL(b.baseURL, minimaxImageEndpoint), minimaxHeaders(b.apiKey), payload)
		})
		if err != nil {
			return err
		}
		parsed = nil
		return provkit.DecodeJSON(resp.Body, &parsed)
	})
	if err != nil {
		return nil, err
	}
	return parsed, nil
}

// resolveDimensions 按「比例优先、清晰度其次」算出 (宽, 高)。
//
// 比例永远来自 aspect_ratio；image_size 只决定清晰度短边。短边先夹到 ≥ _MIN_EDGE 再算尺寸，
// 否则过小的短边会让 aspect_size 产出 <512 的边，随后对宽高独立夹取会破坏比例。结果被 8 整除、
// 长边受 2048 收口，每边再经 clamp 夹进 [512, 2048] 兜底。
func (b *minimaxImageBackend) resolveDimensions(req provider.ImageGenerationRequest) (int, int) {
	short := project.ResolutionToShortEdge(req.ImageSize, project.ImageTierShortEdge, minimaxImageDefaultShort)
	if short < minimaxImageMinEdge {
		short = minimaxImageMinEdge
	}
	maxLong := minimaxImageMaxEdge
	ratio := minimaxImageMaxRatio
	w, h := project.AspectSize(req.AspectRatio, short, project.AspectSizeOptions{
		RoundTo:     minimaxImageRoundTo,
		MaxLongEdge: &maxLong,
		MaxRatio:    &ratio,
	})
	return minimaxClampEdge(w), minimaxClampEdge(h)
}

// minimaxClampEdge 把单边像素夹进 [512, 2048]。两端点均为 8 的倍数，夹取后仍满足 8 整除约束。
func minimaxClampEdge(value int) int {
	if value < minimaxImageMinEdge {
		return minimaxImageMinEdge
	}
	if value > minimaxImageMaxEdge {
		return minimaxImageMaxEdge
	}
	return value
}

// buildSubjectReference 构建 subject_reference（单脸参考）。image-01 仅取一张人脸参考；多张时
// 截断为首张并告警。首张缺失 / 读取失败即 fail-loud，报 image_reference_images_unreadable。
func (b *minimaxImageBackend) buildSubjectReference(
	req provider.ImageGenerationRequest,
) ([]map[string]any, error) {
	refs := req.ReferenceImages
	if len(refs) > minimaxImageSubjectRefLimit {
		slog.Warn("MiniMax subject_reference 仅支持 1 张参考图，截断取首张", "count", len(refs))
	}
	path := refs[0].Path
	if path == "" || !provkit.IsFile(path) {
		return nil, provider.NewImageCapabilityError(provider.FailureImageReferenceImagesUnreadable, map[string]any{
			"model": b.model,
			"names": minimaxImageRefName(path),
		})
	}
	uri, err := provkit.ImageToBase64DataURI(path)
	if err != nil {
		slog.Warn("MiniMax 参考图读取失败", "path", path, "error", err)
		return nil, provider.NewImageCapabilityError(provider.FailureImageReferenceImagesUnreadable, map[string]any{
			"model": b.model,
			"names": filepath.Base(path),
		})
	}
	return []map[string]any{{"type": "character", "image_file": uri}}, nil
}

// minimaxImageRefName 参考图缺失时用于报错的名称：无路径可显示时用序号 #1 标识。
func minimaxImageRefName(path string) string {
	if path == "" {
		return "#1"
	}
	return filepath.Base(path)
}

// persistImage 把 image_generation 响应落地为本地文件，返回远端 URL（base64 路径返回空串）。
//
// 先查 base_resp 业务错误（200 + 非零 status_code），再优先 URL（立即下载，24h 失效前落地），
// URL 缺失降级 base64 解码写盘；两者皆空即报错。
func (b *minimaxImageBackend) persistImage(
	ctx context.Context, data map[string]any, outputPath string,
) (string, error) {
	if reason := minimaxImageFailureReason(data); reason != "" {
		return "", errors.New(reason)
	}
	if url := minimaxExtractImageURL(data); url != "" {
		if err := minimaxDownloadImage(ctx, b.client, url, outputPath); err != nil {
			return "", err
		}
		return url, nil
	}
	if b64 := minimaxExtractImageBase64(data); b64 != "" {
		if err := minimaxWriteBase64Image(b64, outputPath); err != nil {
			return "", err
		}
		return "", nil
	}
	// 完整响应体记日志便于诊断，但不嵌进异常消息——避免 body 里的 "503"/"timeout" 等子串被
	// 误判为可重试（仓库已确立按状态码而非字符串判重试）。
	slog.Error("MiniMax 图像响应缺少 image_urls/image_base64", "body", data)
	return "", errors.New("MiniMax 图像响应缺少 image_urls/image_base64")
}

// minimaxDownloadImage 下载已签发的结果图 URL（幂等 GET），独立的下载重试范围。
//
// 瞬态失败在本层重试，绝不回退到重跑非幂等的生成 POST；4xx（URL 失效等确定性错误）快速失败。
func minimaxDownloadImage(ctx context.Context, client *httpc.Client, rawURL, outputPath string) error {
	return provider.RunWithRetry(ctx, provider.DownloadRetryPolicy, provider.DownloadRetryable, func(ctx context.Context) error {
		return provkit.DownloadImageToPath(ctx, client, rawURL, outputPath)
	})
}

// minimaxExtractImageURL 从响应 data.image_urls 取首个 URL；无可用 URL 返回空串。
func minimaxExtractImageURL(payload map[string]any) string {
	urls, ok := minimaxAsDict(payload["data"])["image_urls"].([]any)
	if !ok {
		return ""
	}
	for _, url := range urls {
		if s, ok := url.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// minimaxExtractImageBase64 从响应 data.image_base64 取首个 base64；无可用值返回空串。
func minimaxExtractImageBase64(payload map[string]any) string {
	items, ok := minimaxAsDict(payload["data"])["image_base64"].([]any)
	if !ok {
		return ""
	}
	for _, item := range items {
		if s, ok := item.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// minimaxImageFailureReason 在 base_resp.status_code 非零时返回错误描述；成功（0）或缺失返回空串。
//
// MiniMax 业务错误以 HTTP 200 + base_resp.status_code 非零承载，故同步图像响应须先查 base_resp
// 再取图。
func minimaxImageFailureReason(payload map[string]any) string {
	base := minimaxAsDict(payload["base_resp"])
	status, ok := base["status_code"]
	if !ok || status == nil {
		return ""
	}
	if f, isNum := status.(float64); isNum && f == 0 {
		return ""
	}
	msg := minimaxRawString(base["status_msg"])
	return strings.TrimSpace(fmt.Sprintf("MiniMax 图像生成失败 status_code=%s: %s", minimaxRawString(status), msg))
}

// minimaxRawString 把任意 JSON 值渲染成展示串：nil → 空串，数字 → 去尾零，其余 → %v。
func minimaxRawString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func init() {
	provider.RegisterImageBackend(minimaxProviderID, provider.ImageBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.ImageBackend, error) {
			return NewImageBackend(opts)
		},
	})
}
