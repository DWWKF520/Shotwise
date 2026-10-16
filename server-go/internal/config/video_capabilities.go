package config

import (
	"context"
	"strconv"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/provider"
)

// VideoCapabilityView 是项目当前视频身份的能力视图（对应 Python resolver.video_capabilities 的核心字段）。
type VideoCapabilityView struct {
	ProviderID         string
	ModelID            string
	Source             string // registry | custom
	Bucket             provider.VideoBucket
	Caps               provider.VideoCapabilities
	SupportedDurations []int
}

// VideoCapabilitiesForProject 按项目生成路线定桶，解析执行时会使用的视频身份并返回其能力。
// 身份缺该桶所需能力时返回 *VideoBucketCapabilityError，与执行链一致，不静默换模型。
func (r *Resolver) VideoCapabilitiesForProject(ctx context.Context, project *domain.ProjectFile) (VideoCapabilityView, error) {
	return r.VideoCapabilitiesForProjectBucket(ctx, project, provider.VideoBucketForGenerationMode(project.GenerationMode))
}

// VideoCapabilitiesForProjectBucket 按指定能力桶解析项目视频身份的能力。
//
// bucket 给定时按该桶解析——参考路线内无参考图退化镜头按 i2v 桶模型取档（读侧近似，见
// reference_unit_video_bucket）；未指定时调用方走按路线定桶的 VideoCapabilitiesForProject。
func (r *Resolver) VideoCapabilitiesForProjectBucket(
	ctx context.Context, project *domain.ProjectFile, bucket provider.VideoBucket,
) (VideoCapabilityView, error) {
	selected, err := r.ResolveVideoBackend(ctx, project, nil, &bucket)
	if err != nil {
		return VideoCapabilityView{}, err
	}
	view := VideoCapabilityView{ProviderID: selected.ProviderID, ModelID: selected.ModelID, Bucket: bucket}

	model, err := r.ensureVideoIdentityResolvable(ctx, selected, bucket)
	if err != nil {
		return VideoCapabilityView{}, err
	}
	if model != nil {
		caps, err := synthesizeVideoCapabilities(model.Endpoint, model.ModelID, model.CapabilityOverrides)
		if err != nil {
			return VideoCapabilityView{}, videoBucketReferenceUnavailable(bucket, selected.ProviderID, selected.ModelID)
		}
		view.Source = "custom"
		view.Caps = caps
		if model.SupportedDurations != nil {
			view.SupportedDurations = parseDurationsCSV(*model.SupportedDurations)
		}
		return view, nil
	}

	spec, ok := provider.ProviderSpecFor(selected.ProviderID, provider.MediaVideo)
	if !ok {
		return VideoCapabilityView{}, videoBucketReferenceUnavailable(bucket, selected.ProviderID, selected.ModelID)
	}
	caps, err := provider.VideoCapabilitiesForModel(spec.RegistryBackend, selected.ModelID)
	if err != nil {
		return VideoCapabilityView{}, videoBucketReferenceUnavailable(bucket, selected.ProviderID, selected.ModelID)
	}
	info, _ := provider.ModelInfoFor(selected.ProviderID, selected.ModelID)
	view.Source = "registry"
	view.Caps = caps
	view.SupportedDurations = info.SupportedDurations
	return view, nil
}

// VideoGenerateAudioForProject 解析项目级 generate-audio 开关：admin 设置（缺省 true）叠项目覆盖。
//
// 与视频能力解析相互独立：能力解析失败不连带丢失用户无声意图。
func (r *Resolver) VideoGenerateAudioForProject(ctx context.Context, project *domain.ProjectFile) (bool, error) {
	raw, err := r.svc.GetSetting(ctx, "video_generate_audio", "")
	if err != nil {
		return false, err
	}
	// 未配置时按有声（与执行层 video_generate_audio 的默认 true 同源）。
	value := true
	if raw != "" {
		if parsed, perr := strconv.ParseBool(raw); perr == nil {
			value = parsed
		}
	}
	if project != nil && project.VideoGenerateAudio != nil {
		value = *project.VideoGenerateAudio
	}
	return value, nil
}

// ProviderFallbackResolution 是供应商的兜底下发分辨率（纯查表，不触 DB）。
//
// 仅参考视频路径使用：该路径下发非空档位，故兜底参与时长联动约束求值；普通图生视频路径不配
// 分辨率时「不传 resolution 参数」，不得施加本兜底。registry ID 归一化到短前缀后查表。
func ProviderFallbackResolution(providerID string) string {
	const defaultResolution = "1080p"
	if providerID == "" {
		return defaultResolution
	}
	if resolution, ok := providerFallbackResolutions[providerID]; ok {
		return resolution
	}
	short := providerID
	if idx := strings.Index(providerID, "-"); idx >= 0 {
		short = providerID[:idx]
	}
	if resolution, ok := providerFallbackResolutions[short]; ok {
		return resolution
	}
	return defaultResolution
}

// providerFallbackResolutions 是兜底分辨率表：MiniMax 海螺缺省 768P（1080P 仅 6s，默认落 768P
// 避免与 10s 档冲突）。
var providerFallbackResolutions = map[string]string{
	"gemini":  "1080p",
	"ark":     "720p",
	"grok":    "720p",
	"openai":  "720p",
	"minimax": "768p",
}

// parseDurationsCSV 解析自定义模型声明的时长 CSV（如 "5,10"）；非法项跳过。
func parseDurationsCSV(raw string) []int {
	var out []int
	for _, part := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}
