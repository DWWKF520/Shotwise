package providers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// Agnes 供应商标识与共享 base / 鉴权工具（图片 / 视频 backend 共用）。
//
// Agnes 经 apihub 网关提供 OpenAI 风格端点，单 /v1 base，Bearer 单 key 鉴权；网关并非所有端点
// 都挂在 /v1 下（成片查询 /agnesapi 直接挂 host 根），故 host 段单独归一化。
const (
	// agnesProviderID 与 registry 的 provider_id / 注册键同名。
	agnesProviderID = "agnes"

	// agnesDefaultBaseURL 默认 base（含 /v1）；用户可经配置覆盖指向自建中转。
	agnesDefaultBaseURL = "https://apihub.agnes-ai.com/v1"
	agnesV1Suffix       = "/v1"

	// agnesImageDefaultTimeout 单次图像生成请求的墙钟预算（对应 Python http_timeout 默认 120s）。
	agnesImageDefaultTimeout = 120 * time.Second
)

// agnesResolveAPIKey 解析 Bearer API Key；缺失 / 纯空白即报错（不走 env fallback）。
func agnesResolveAPIKey(apiKey string) (string, error) {
	key := strings.TrimSpace(apiKey)
	if key == "" {
		return "", errors.New("请到系统配置页填写 Agnes API Key")
	}
	return key, nil
}

// agnesHost 从配置的 base_url 提取 host 段（剥除 /v1 后缀），缺省回落默认 host。
//
// 网关并非所有端点都挂在 /v1 下——成片查询 /agnesapi 直接挂在 host 根，需要这个不带版本段的 base。
func agnesHost(configured string) string {
	base := strings.TrimSpace(configured)
	if base == "" {
		base = agnesDefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, agnesV1Suffix) {
		return strings.TrimSuffix(base, agnesV1Suffix)
	}
	return base
}

// agnesBaseURL OpenAI 兼容 base：{host}/v1。
func agnesBaseURL(configured string) string { return agnesHost(configured) + agnesV1Suffix }

// agnesHeaders Bearer 鉴权头。
func agnesHeaders(apiKey string) map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + apiKey,
		"Content-Type":  "application/json",
	}
}

// agnesHTTPTimeout 读取 Extra["http_timeout"]（秒）覆盖模板默认；缺失或非法回落 fallback。
func agnesHTTPTimeout(opts provider.BackendOptions, fallback time.Duration) time.Duration {
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

// Agnes 图像后端常量。
const (
	agnesImageDefaultModel = "agnes-image-2.5-flash"
	agnesImageEndpoint     = "/images/generations"

	// 尺寸约束：长宽被 8 整除、长边收口 2048（覆盖常见 flash 图像档）。缺 image_size 时按 2K
	// 短边兜底，经长边收口后竖屏得 1152x2048。
	agnesImageRoundTo      = 8
	agnesImageMaxLongEdge  = 2048
	agnesImageDefaultShort = 1440

	// 档位式尺寸（1K/2K/3K/4K）必须与 ratio 配合使用；未指定时用文档中的 2K 档。
	agnesImageDefaultSize = "2K"
)

// agnesImageSizeTiers Agnes 文档支持的档位式尺寸。
var agnesImageSizeTiers = map[string]bool{"1K": true, "2K": true, "3K": true, "4K": true}

// agnesImageBackend 是 Agnes 图像后端（单步同步 OpenAI 兼容 images/generations 端点）。
type agnesImageBackend struct {
	model   string
	baseURL string
	apiKey  string
	client  *httpc.Client
}

// newAgnesImageBackend 按通用信封构造：api_key 缺失即 fail loud；model / base_url 留空时由本
// 后端兜底（默认 model、默认端点）。
func newAgnesImageBackend(opts provider.BackendOptions) (*agnesImageBackend, error) {
	apiKey, err := agnesResolveAPIKey(opts.APIKey)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = agnesImageDefaultModel
	}
	return &agnesImageBackend{
		model:   model,
		baseURL: agnesBaseURL(opts.BaseURL),
		apiKey:  apiKey,
		client:  httpc.NewClient(agnesHTTPTimeout(opts, agnesImageDefaultTimeout)),
	}, nil
}

// Name 返回供应商标识。
func (b *agnesImageBackend) Name() string { return agnesProviderID }

// Model 返回实际下发的模型名。
func (b *agnesImageBackend) Model() string { return b.model }

// Capabilities 返回能力位：T2I 与 I2I（参考图经 extra_body.image 下发）共用同一端点。
func (b *agnesImageBackend) Capabilities() provider.ImageCapabilitySet {
	return provider.NewImageCapabilitySet(provider.CapTextToImage, provider.CapImageToImage)
}

// Generate 生成一张图片并落盘到 req.OutputPath。
//
// 编排层不带重试：把非幂等的「建图 + 计费」提交与幂等的结果下载隔离到各自的重试范围，避免下载
// 失败回退到重跑生成 POST 造成重复计费。
func (b *agnesImageBackend) Generate(
	ctx context.Context, req provider.ImageGenerationRequest,
) (provider.ImageGenerationResult, error) {
	size, ratio := b.resolveSize(req)

	// Agnes 文档要求 URL 输出放在 extra_body.response_format，不能放在顶层；同一 extra_body 也
	// 承载图生图的 image 参数。
	extraBody := map[string]any{"response_format": "url"}
	payload := map[string]any{
		"model":      b.model,
		"prompt":     req.Prompt,
		"size":       size,
		"extra_body": extraBody,
	}
	if ratio != "" {
		payload["ratio"] = ratio
	}
	if len(req.ReferenceImages) > 0 {
		refs, err := b.buildReferenceImages(req)
		if err != nil {
			return provider.ImageGenerationResult{}, err
		}
		extraBody["image"] = refs
	}

	data, err := b.submit(ctx, payload)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	imageURI, err := b.persistImage(ctx, data, req.OutputPath)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	slog.Info("Agnes 图片生成完成", "output", req.OutputPath)
	return provider.ImageGenerationResult{
		ImagePath: req.OutputPath,
		Provider:  agnesProviderID,
		Model:     b.model,
		ImageURI:  imageURI,
	}, nil
}

// submit 单步图像生成 POST（非幂等「建图 + 计费」），返回解析后的响应体。
//
// SubmitPost 把歧义传输错误转 AmbiguousSubmitError 终态失败避免重复计费；重试范围严格限定在
// 本方法内、不含下载——下载失败不会触发整流程重试导致重复建图与重复计费。
func (b *agnesImageBackend) submit(ctx context.Context, payload map[string]any) (map[string]any, error) {
	var parsed map[string]any
	err := provider.RunWithRetry(ctx, provider.DefaultRetryPolicy, provider.SubmitRetryable, func(ctx context.Context) error {
		resp, err := SubmitPost(ctx, agnesProviderID, func(ctx context.Context) (*httpc.Response, error) {
			return b.client.PostJSON(ctx, httpc.JoinURL(b.baseURL, agnesImageEndpoint), agnesHeaders(b.apiKey), payload)
		})
		if err != nil {
			return err
		}
		parsed = nil
		return decodeJSON(resp.Body, &parsed)
	})
	if err != nil {
		return nil, err
	}
	return parsed, nil
}

// resolveSize 将通用图片请求转换为 Agnes 文档规定的 size/ratio 参数。
//
// 档位式 size（1K/2K/3K/4K）必须配合 ratio 使用；自定义像素尺寸继续用兼容的 宽x高 写法，并让
// 尺寸自身表达比例（此时不返回 ratio，避免同时发送冲突的比例）。未指定尺寸时用文档中的 2K 档。
func (b *agnesImageBackend) resolveSize(req provider.ImageGenerationRequest) (string, string) {
	rawSize := strings.ToUpper(strings.TrimSpace(req.ImageSize))
	if rawSize == "" {
		return agnesImageDefaultSize, agnesCanonicalRatio(req.AspectRatio)
	}
	if agnesImageSizeTiers[rawSize] {
		return rawSize, agnesCanonicalRatio(req.AspectRatio)
	}
	width, height := b.resolveDimensions(req)
	return fmt.Sprintf("%dx%d", width, height), ""
}

// agnesCanonicalRatio 把比例规范化成约简的 "w:h"。
func agnesCanonicalRatio(aspectRatio string) string {
	aw, ah := project.ParseAspectRatio(aspectRatio)
	return fmt.Sprintf("%d:%d", aw, ah)
}

// resolveDimensions 按「比例优先、清晰度其次」算出自定义尺寸的 (宽, 高)。
//
// 自定义 image_size 会剥离其自带比例（取短边），比例由 aspect_ratio 决定；结果被 8 整除、
// 长边受 2048 收口。
func (b *agnesImageBackend) resolveDimensions(req provider.ImageGenerationRequest) (int, int) {
	short := project.ResolutionToShortEdge(req.ImageSize, project.ImageTierShortEdge, agnesImageDefaultShort)
	maxLong := agnesImageMaxLongEdge
	return project.AspectSize(req.AspectRatio, short, project.AspectSizeOptions{
		RoundTo:     agnesImageRoundTo,
		MaxLongEdge: &maxLong,
	})
}

// buildReferenceImages 构建 I2I 参考图列表（data-URI base64）。
//
// 任一参考图缺失 / 读取失败即 fail-loud，报 image_reference_images_unreadable，让用户感知有图
// 未被使用而非静默丢弃后照常计费。
func (b *agnesImageBackend) buildReferenceImages(req provider.ImageGenerationRequest) ([]string, error) {
	uris := make([]string, 0, len(req.ReferenceImages))
	for idx, ref := range req.ReferenceImages {
		path := strings.TrimSpace(ref.Path)
		if path == "" || !minimaxIsFile(path) {
			// 空路径无文件名可显示，用序号标识，避免中文占位漏进非中文报错模板。
			return nil, provider.NewImageCapabilityError(provider.FailureImageReferenceImagesUnreadable, map[string]any{
				"model": b.model,
				"names": agnesReferenceName(path, idx+1),
			})
		}
		uri, err := imageToBase64DataURI(path)
		if err != nil {
			slog.Warn("Agnes 参考图读取失败", "path", path, "error", err)
			return nil, provider.NewImageCapabilityError(provider.FailureImageReferenceImagesUnreadable, map[string]any{
				"model": b.model,
				"names": filepath.Base(path),
			})
		}
		uris = append(uris, uri)
	}
	return uris, nil
}

// agnesReferenceName 参考图缺失时用于报错的名称：无路径可显示时用序号 #N 标识。
func agnesReferenceName(path string, index int) string {
	if path == "" {
		return fmt.Sprintf("#%d", index)
	}
	return filepath.Base(path)
}

// persistImage 把 images/generations 响应落地为本地文件，返回远端 URL（base64 路径返回空串）。
//
// 优先 URL（立即下载）；URL 缺失或下载失败时降级到同响应内 base64 解码写盘，避免一次已计费的
// 成功生成因下载环节失败而无法落盘；两者皆空即报错。
func (b *agnesImageBackend) persistImage(
	ctx context.Context, data map[string]any, outputPath string,
) (string, error) {
	url := agnesExtractFirstString(data, "url")
	b64 := agnesExtractFirstString(data, "b64_json")
	if url != "" {
		if err := b.downloadResult(ctx, url, outputPath); err != nil {
			if b64 == "" {
				return "", err
			}
			slog.Warn("Agnes 结果 URL 下载失败，改用响应内 b64_json 落盘")
		} else {
			return url, nil
		}
	}
	if b64 != "" {
		if err := agnesWriteBase64Image(b64, outputPath); err != nil {
			return "", err
		}
		return "", nil
	}
	// 完整响应体可能含 prompt / 签名 URL 等敏感字段，只记键名与 data 条数，不落整 body；异常消息
	// 同样不嵌 body——避免 "503"/"timeout" 子串被误判为可重试（仓库按状态码而非字符串判重试）。
	slog.Error("Agnes 图像响应缺少 url/b64_json", "keys", agnesSortedKeys(data), "data_count", agnesDataCount(data))
	return "", errors.New("Agnes 图像响应缺少 url/b64_json")
}

// downloadResult 下载已签发的结果图 URL（幂等 GET），独立的下载重试范围。
//
// 瞬态失败在本层重试，绝不回退到重跑非幂等的生成 POST；4xx 快速失败。
func (b *agnesImageBackend) downloadResult(ctx context.Context, url, outputPath string) error {
	return provider.RunWithRetry(ctx, provider.DownloadRetryPolicy, provider.DownloadRetryable, func(ctx context.Context) error {
		return downloadImageToPath(ctx, b.client, url, outputPath)
	})
}

// agnesExtractFirstString 从 OpenAI 兼容响应 data[].<key> 取首个非空字符串（url / b64_json 共用）。
func agnesExtractFirstString(payload map[string]any, key string) string {
	items, ok := payload["data"].([]any)
	if !ok {
		return ""
	}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if value, ok := entry[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// agnesDataCount 返回 data 数组长度；非数组返回 -1 供日志标注缺失。
func agnesDataCount(payload map[string]any) int {
	if items, ok := payload["data"].([]any); ok {
		return len(items)
	}
	return -1
}

// agnesSortedKeys 返回响应体键的字典序切片（供日志，不回显值）。
func agnesSortedKeys(payload map[string]any) []string {
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// agnesWriteBase64Image 解码 base64 图片并写盘；容忍少数中转返回 data URI（先剥前缀再解码）。
func agnesWriteBase64Image(b64, outputPath string) error {
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
	return writeFileAtomic(outputPath, decoded)
}

func init() {
	provider.RegisterImageBackend(agnesProviderID, provider.ImageBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.ImageBackend, error) {
			return newAgnesImageBackend(opts)
		},
	})
}
