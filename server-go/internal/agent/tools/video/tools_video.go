package video

import "github.com/shotwise/shotwise/internal/agent/tools"

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
)


type getVideoCapabilitiesIn struct{}

// NewGetVideoCapabilitiesTool 返回项目视频模型能力 JSON（只读，对应 Python get_video_capabilities）。
func NewGetVideoCapabilitiesTool(project string, reader tools.VideoCapabilityReader, projects tools.AssetProjects) (tool.InvokableTool, error) {
	return toolutils.InferTool("get_video_capabilities",
		"查视频模型能力（model 粒度）+ 用户项目偏好。返回 JSON。能力按项目生成路线定轴，全项目同一口径，无需指定剧集。",
		func(ctx context.Context, _ getVideoCapabilitiesIn) (string, error) {
			if reader == nil || projects == nil {
				return tools.DenyEnvelope("video capabilities not configured"), nil
			}
			pf, err := projects.Get(project)
			if err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			view, err := reader.VideoCapabilitiesForProject(ctx, pf)
			if err != nil {
				return tools.DenyEnvelope("无法解析视频模型能力: " + err.Error()), nil
			}
			if len(view.SupportedDurations) == 0 {
				return tools.DenyEnvelope(fmt.Sprintf("无法解析视频模型能力: 模型 %s/%s 未声明 supported_durations", view.ProviderID, view.ModelID)), nil
			}
			payload := videoCapabilityPayload(pf, view)
			b, err := json.MarshalIndent(payload, "", "  ")
			if err != nil {
				return tools.DenyEnvelope("marshal failed: " + err.Error()), nil
			}
			return tools.OKEnvelope(string(b)), nil
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
