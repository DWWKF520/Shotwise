package script

import (
	"context"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/provider"
)

// fakeVideoCapsResolver 按注入结果返回项目视频能力与音频开关（与根包 video_caps_test 同源副本）。
type fakeVideoCapsResolver struct {
	view       config.VideoCapabilityView
	viewErr    error
	generateAu bool
	audioErr   error
	resolution string
	byBucket   map[provider.VideoBucket]config.VideoCapabilityView
}

func (f *fakeVideoCapsResolver) VideoCapabilitiesForProjectBucket(
	_ context.Context, _ *domain.ProjectFile, bucket provider.VideoBucket,
) (config.VideoCapabilityView, error) {
	if f.viewErr != nil {
		return config.VideoCapabilityView{}, f.viewErr
	}
	if f.byBucket != nil {
		if view, ok := f.byBucket[bucket]; ok {
			return view, nil
		}
	}
	return f.view, nil
}

func (f *fakeVideoCapsResolver) VideoGenerateAudioForProject(context.Context, *domain.ProjectFile) (bool, error) {
	return f.generateAu, f.audioErr
}

func (f *fakeVideoCapsResolver) ResolveResolution(context.Context, *domain.ProjectFile, string, string) (string, error) {
	return f.resolution, nil
}

func veoView() config.VideoCapabilityView {
	return config.VideoCapabilityView{
		ProviderID:         "gemini-aistudio",
		ModelID:            "veo-3.1-generate-preview",
		SupportedDurations: []int{4, 6, 8},
		Caps:               provider.DefaultVideoCapabilities(),
	}
}
