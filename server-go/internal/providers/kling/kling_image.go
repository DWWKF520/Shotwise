package kling

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/providers/provkit"
)

// klingImageBackend 是可灵 Kling 图像生成后端（JWT 直连 / Bearer 中转双模式，异步轮询）。
//
// 走可灵原生图像端点：普通模型 POST /v1/images/generations，Omni 模型 POST /v1/images/omni-image；
// 均取 data.task_id 后轮询对应端点至 task_status=succeed，再下载 task_result.images[0].url。
//
// 鉴权 / base_url 装配 / submit-poll 骨架由 klingBackendBase 共享；本类型只填图像侧差异：端点路径、
// payload 构建、参考图编码、静态能力与 api_model_name 解耦。
const (
	klingImageDefaultModel = "kling-image-o1"

	klingImageEndpoint     = "images/generations"
	klingOmniImageEndpoint = "images/omni-image"
	klingOmniModel         = "kling-v3-omni"

	klingImagePollIntervalSeconds = 5
	klingImagePollMaxWaitSeconds  = 600

	klingImageDefaultRefLimit = 10
)

// klingImageRefLimits 各图像模型参考图上限（按 registry 键名）：o1 / v3-omni 均支持多图主体。
var klingImageRefLimits = map[string]int{
	"kling-image-o1":      10,
	"kling-v3-omni-image": 10,
}

// klingImageBackend 见文件头注释。
type klingImageBackend struct {
	*klingBackendBase
	apiModelName string
	caps         provider.ImageCapabilitySet
}

// NewImageBackend 按通用信封构造；model / api_model_name 留空时回落到默认模型。
//
// auth_mode 显式声明优先，否则按 api_key 是否非空推导；access_key / secret_key 从 Extra 读取。
func NewImageBackend(opts provider.BackendOptions) (*klingImageBackend, error) {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = klingImageDefaultModel
	}
	base, err := newKlingBackendBase(opts, model)
	if err != nil {
		return nil, err
	}
	base.mediaLabel = "图像"
	apiModelName := strings.TrimSpace(opts.Extra["api_model_name"])
	if apiModelName == "" {
		// 发给可灵 API 的模型名缺省回退 registry 键名；别名键（kling-v3-omni-image）必须经
		// api_model_name 解耦，不硬发键名。
		apiModelName = model
	}
	return &klingImageBackend{
		klingBackendBase: base,
		apiModelName:     apiModelName,
		// o1 / v3-omni 均支持文生图 + 图生图（多图主体）。
		caps: provider.NewImageCapabilitySet(provider.CapTextToImage, provider.CapImageToImage),
	}, nil
}

// Capabilities 返回本实例的能力位。
func (b *klingImageBackend) Capabilities() provider.ImageCapabilitySet { return b.caps }

// refLimit 参考图上限（按 registry 键名）。
func (b *klingImageBackend) refLimit() int {
	if limit, ok := klingImageRefLimits[b.model]; ok {
		return limit
	}
	return klingImageDefaultRefLimit
}

// isOmni 是否使用 Omni 专用端点。
func (b *klingImageBackend) isOmni() bool { return b.apiModelName == klingOmniModel }

// imageEndpoint 按模型选择图像端点。
func (b *klingImageBackend) imageEndpoint() string {
	if b.isOmni() {
		return klingOmniImageEndpoint
	}
	return klingImageEndpoint
}

// buildPayload 构建普通或 Omni 图像请求体，按 API 模型选择对应字段。
func (b *klingImageBackend) buildPayload(req provider.ImageGenerationRequest) (map[string]any, error) {
	payload := map[string]any{
		"model_name":   b.apiModelName,
		"prompt":       req.Prompt,
		"aspect_ratio": req.AspectRatio,
		"n":            1,
	}
	if b.isOmni() && req.ImageSize != "" {
		resolution := strings.ToLower(strings.TrimSpace(req.ImageSize))
		if resolution == "1k" || resolution == "2k" || resolution == "4k" {
			payload["resolution"] = resolution
		}
	}
	images, err := b.encodeReferences(req.ReferenceImages)
	if err != nil {
		return nil, err
	}
	if len(images) > 0 {
		if b.isOmni() {
			imageList := make([]map[string]any, 0, len(images))
			for _, image := range images {
				imageList = append(imageList, map[string]any{"image": image})
			}
			payload["image_list"] = imageList
		} else {
			payload["image"] = images
		}
	}
	return payload, nil
}

// encodeReferences 参考图 → 纯 base64 列表；超上限截断，缺失 / 不可读 fail-loud。
func (b *klingImageBackend) encodeReferences(references []provider.ReferenceImage) ([]string, error) {
	if len(references) == 0 {
		return nil, nil
	}
	encoded := make([]string, 0, len(references))
	unreadable := make([]string, 0)
	for idx, ref := range references {
		// 无文件名的路径用序号 #N 标识第几张，避免空 token 漏进报错。
		name := klingBaseName(ref.Path)
		if name == "" {
			name = fmt.Sprintf("#%d", idx+1)
		}
		info, err := os.Stat(ref.Path)
		if err != nil || !info.Mode().IsRegular() {
			unreadable = append(unreadable, name)
			continue
		}
		data, err := klingImageToBase64(ref.Path)
		if err != nil {
			slog.Warn("Kling 参考图读取失败", "path", ref.Path, "error", err)
			unreadable = append(unreadable, name)
			continue
		}
		encoded = append(encoded, data)
	}
	if len(unreadable) > 0 {
		// fail-loud：声明的参考图缺失即中止，不静默用子集生成出错误结果还照常计费。
		return nil, provider.NewImageCapabilityError(provider.FailureImageReferenceImagesUnreadable, map[string]any{
			"model": b.model,
			"names": strings.Join(unreadable, ", "),
		})
	}
	if limit := b.refLimit(); len(encoded) > limit {
		slog.Warn("Kling 参考图数量超过上限，截断", "count", len(encoded), "model", b.model, "limit", limit)
		encoded = encoded[:limit]
	}
	return encoded, nil
}

// safeLogView 预脱敏标量视图：base64 参考图 / prompt 一律不展开，仅记是否存在 + 数量 + prompt 长度。
func klingImageSafeLogView(payload map[string]any) []any {
	endpoint := klingImageEndpoint
	if klingAsStr(payload["model_name"]) == klingOmniModel {
		endpoint = klingOmniImageEndpoint
	}
	referenceCount := 0
	if list, ok := payload["image_list"].([]map[string]any); ok {
		referenceCount = len(list)
	} else if list, ok := payload["image"].([]string); ok {
		referenceCount = len(list)
	}
	fields := []any{
		"endpoint", endpoint,
		"model_name", payload["model_name"],
		"aspect_ratio", payload["aspect_ratio"],
		"n", payload["n"],
		"reference_count", referenceCount,
		"prompt_len", len(klingAsStr(payload["prompt"])),
	}
	if resolution, ok := payload["resolution"]; ok {
		fields = append(fields, "resolution", resolution)
	}
	return fields
}

// Generate 生成一张图片并落盘到 req.OutputPath。
func (b *klingImageBackend) Generate(
	ctx context.Context, req provider.ImageGenerationRequest,
) (provider.ImageGenerationResult, error) {
	payload, err := b.buildPayload(req)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	slog.Info("调用 Kling 图像 API", klingImageSafeLogView(payload)...)

	endpoint := b.imageEndpoint()
	taskID, err := b.submitTask(ctx, endpoint, payload)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	slog.Info("Kling 图像任务已创建", "task_id", taskID, "model", b.model)

	final, err := b.pollUntilTerminal(ctx, func(ctx context.Context) (map[string]any, error) {
		return b.pollQuery(ctx, endpoint+"/"+taskID)
	}, klingImagePollIntervalSeconds*time.Second, klingImagePollMaxWaitSeconds*time.Second)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	// 24h 有效的 image_url 必须在失效前落地：取首张转存本地（组图按张产出，单输出取 [0]）。
	urls, err := klingExtractImageURLs(final)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	downloadURL := urls[0]

	if err := provkit.DownloadImageToPath(ctx, b.client, downloadURL, req.OutputPath); err != nil {
		return provider.ImageGenerationResult{}, err
	}
	slog.Info("Kling 图像下载完成", "path", req.OutputPath)

	return provider.ImageGenerationResult{
		ImagePath: req.OutputPath,
		Provider:  klingProvider,
		Model:     b.model,
		ImageURI:  downloadURL,
		Seed:      req.Seed,
	}, nil
}

func init() {
	provider.RegisterImageBackend("kling", provider.ImageBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.ImageBackend, error) {
			return NewImageBackend(opts)
		},
	})
}
