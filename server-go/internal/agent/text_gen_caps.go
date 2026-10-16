package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/provider"
)

// 视频能力解析辅助（对应 server/agent_runtime/sdk_tools/_context.py 的 fetch_video_caps /
// resolve_video_caps）。

// videoResolutionReader 是可选能力：能解析项目生效分辨率的解析器据其收窄时长档位。
// *config.Resolver 满足；测试替身未实现时跳过分辨率联动约束。
type videoResolutionReader interface {
	ResolveResolution(ctx context.Context, project *domain.ProjectFile, providerID, modelID string) (string, error)
}

// fetchVideoDurations 解析项目当前视频身份的 (default_duration, supported_durations)。
//
// supported_durations 已按项目分辨率经「分辨率↔时长」联动约束收窄：型号声明的全集不含该约束，
// 未收窄的集合交给 LLM 会产出执行期必然被拒的时长。default_duration 是用户配置的原样值，
// 成员性由调用方按各自口径判定。durations 为空时报错（调用方决定是硬错还是软回退）。
func fetchVideoDurations(
	ctx context.Context, reader VideoCapabilityReader, project *domain.ProjectFile,
) (*int, []int, error) {
	if reader == nil || project == nil {
		return nil, nil, fmt.Errorf("video capabilities not configured")
	}
	view, err := reader.VideoCapabilitiesForProject(ctx, project)
	if err != nil {
		return nil, nil, err
	}
	durations := constrainDurationsByResolution(project, view, view.SupportedDurations)
	if len(durations) == 0 {
		return nil, nil, fmt.Errorf("video_capabilities_unresolved: 模型 %s/%s 未声明 supported_durations",
			view.ProviderID, view.ModelID)
	}
	return project.DefaultDuration, durations, nil
}

// constrainDurationsByResolution 按「分辨率↔时长」联动约束收窄候选（对应 constrain_durations
// 的视频侧口径）：交集为空时返回原候选——空集说明声明自相矛盾，清空会让上游拿不到任何可用时长。
// 型号不在注册表（自定义供应商）时不施加联动约束。
func constrainDurationsByResolution(
	project *domain.ProjectFile, view config.VideoCapabilityView, candidates []int,
) []int {
	if len(candidates) == 0 {
		return candidates
	}
	info, ok := provider.ModelInfoFor(view.ProviderID, view.ModelID)
	if !ok {
		return candidates
	}
	resolution := ""
	if project != nil {
		if settings, ok := project.ModelSettings[view.ProviderID+"/"+view.ModelID]; ok {
			resolution = strings.TrimSpace(settings["resolution"])
		}
	}
	if resolution == "" {
		return candidates
	}
	byResolution := info.DurationResolutionConstraints[strings.ToLower(resolution)]
	if len(byResolution) == 0 {
		return candidates
	}
	allowed := make([]int, 0, len(candidates))
	for _, d := range candidates {
		for _, a := range byResolution {
			if d == a {
				allowed = append(allowed, d)
				break
			}
		}
	}
	if len(allowed) == 0 {
		return candidates
	}
	return allowed
}
