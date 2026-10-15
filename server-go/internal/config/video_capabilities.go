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
	bucket := provider.VideoBucketForGenerationMode(project.GenerationMode)
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
