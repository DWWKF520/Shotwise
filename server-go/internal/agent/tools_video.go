package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
)

// VideoCapabilityReader 解析项目当前视频身份的能力（*config.Resolver 满足）。
type VideoCapabilityReader interface {
	VideoCapabilitiesForProject(ctx context.Context, project *domain.ProjectFile) (config.VideoCapabilityView, error)
}

// WithVideoCapabilities 注入 get_video_capabilities 的能力解析器；未注入时不注册该工具。
func WithVideoCapabilities(reader VideoCapabilityReader) RuntimeOption {
	return func(r *Runtime) {
		r.videoCaps = reader
	}
}

type getVideoCapabilitiesIn struct{}

// newGetVideoCapabilitiesTool 返回项目视频模型能力 JSON（只读，对应 Python get_video_capabilities）。
func newGetVideoCapabilitiesTool(project string, reader VideoCapabilityReader, projects AssetProjects) (tool.InvokableTool, error) {
	return toolutils.InferTool("get_video_capabilities",
		"查视频模型能力（model 粒度）+ 用户项目偏好。返回 JSON。能力按项目生成路线定轴，全项目同一口径，无需指定剧集。",
		func(ctx context.Context, _ getVideoCapabilitiesIn) (string, error) {
			if reader == nil || projects == nil {
				return denyEnvelope("video capabilities not configured"), nil
			}
			pf, err := projects.Get(project)
			if err != nil {
				return denyEnvelope(err.Error()), nil
			}
			view, err := reader.VideoCapabilitiesForProject(ctx, pf)
			if err != nil {
				return denyEnvelope("无法解析视频模型能力: " + err.Error()), nil
			}
			if len(view.SupportedDurations) == 0 {
				return denyEnvelope(fmt.Sprintf("无法解析视频模型能力: 模型 %s/%s 未声明 supported_durations", view.ProviderID, view.ModelID)), nil
			}
			payload := videoCapabilityPayload(pf, view)
			b, err := json.MarshalIndent(payload, "", "  ")
			if err != nil {
				return denyEnvelope("marshal failed: " + err.Error()), nil
			}
			return okEnvelope(string(b)), nil
		})
}

func videoCapabilityPayload(pf *domain.ProjectFile, view config.VideoCapabilityView) map[string]any {
	maxDuration := view.SupportedDurations[0]
	for _, d := range view.SupportedDurations {
		if d > maxDuration {
			maxDuration = d
		}
	}
	return map[string]any{
		"provider_id":               view.ProviderID,
		"model":                     view.ModelID,
		"supported_durations":       view.SupportedDurations,
		"max_duration":              maxDuration,
		"max_reference_images":      view.Caps.MaxReferenceImages,
		"first_frame":               view.Caps.FirstFrame,
		"last_frame":                view.Caps.LastFrame,
		"max_reference_audio_count": view.Caps.MaxReferenceAudioCount,
		"reference_audio_per_image": view.Caps.ReferenceAudioPerImage,
		"source":                    view.Source,
		"default_duration":          pf.DefaultDuration,
		"content_mode":              nilIfEmpty(pf.ContentMode),
		"generation_mode":           nilIfEmpty(pf.GenerationMode),
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
