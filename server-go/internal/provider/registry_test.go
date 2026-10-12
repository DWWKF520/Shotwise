package provider

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestProviderIDsAndMeta(t *testing.T) {
	want := []string{
		"gemini-aistudio", "gemini-vertex", "ark", "ark-agent-plan", "grok",
		"openai", "vidu", "dashscope", "minimax", "kling", "agnes",
	}
	if got := ProviderIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ProviderIDs() = %v，期望 %v", got, want)
	}
	if _, ok := ProviderMetaFor("unknown-provider"); ok {
		t.Fatal("未登记的 provider 应返回 ok=false，不回落默认")
	}
	for _, providerID := range ProviderIDs() {
		meta, ok := ProviderMetaFor(providerID)
		if !ok {
			t.Fatalf("provider %q 未登记", providerID)
		}
		if meta.DisplayName == "" || meta.ConsoleURL == "" {
			t.Errorf("provider %q 缺少展示名或控制台地址", providerID)
		}
		if len(meta.RequiredKeys) == 0 {
			t.Errorf("provider %q 未声明必填凭证键", providerID)
		}
		if len(meta.Models) == 0 {
			t.Errorf("provider %q 无模型条目", providerID)
		}
		seen := map[string]struct{}{}
		for _, entry := range meta.Models {
			if entry.ID == "" {
				t.Errorf("provider %q 存在空 model id", providerID)
			}
			if _, dup := seen[entry.ID]; dup {
				t.Errorf("provider %q 模型键重复：%q", providerID, entry.ID)
			}
			seen[entry.ID] = struct{}{}
			if _, ok := validMediaTypes[entry.Info.MediaType]; !ok {
				t.Errorf("provider %q 模型 %q 媒体类型非法：%q", providerID, entry.ID, entry.Info.MediaType)
			}
		}
		if _, ok := meta.Model("no-such-model"); ok {
			t.Errorf("provider %q 未登记模型查询应返回 ok=false", providerID)
		}
	}
}

func TestDefaultModelForProvider(t *testing.T) {
	cases := []struct {
		provider string
		media    MediaType
		want     string
		wantOK   bool
	}{
		{"gemini-aistudio", MediaVideo, "veo-3.1-lite-generate-preview", true},
		{"gemini-vertex", MediaVideo, "veo-3.1-fast-generate-001", true},
		{"ark", MediaVideo, "doubao-seedance-2-0-mini-260615", true},
		{"ark-agent-plan", MediaVideo, "doubao-seedance-2.0-mini", true},
		{"grok", MediaVideo, "grok-imagine-video", true},
		{"openai", MediaVideo, "sora-2", true},
		{"vidu", MediaVideo, "viduq3-turbo", true},
		{"dashscope", MediaVideo, "happyhorse-1.1-i2v", true},
		{"minimax", MediaVideo, "MiniMax-H3", true},
		{"kling", MediaVideo, "kling-v2-5-turbo", true},
		{"agnes", MediaVideo, "agnes-video-2.5", true},

		{"gemini-aistudio", MediaImage, "gemini-3.1-flash-image-preview", true},
		{"gemini-vertex", MediaImage, "gemini-3.1-flash-image-preview", true},
		{"ark", MediaImage, "doubao-seedream-5-0-lite-260128", true},
		{"ark-agent-plan", MediaImage, "doubao-seedream-5.0-lite", true},
		{"grok", MediaImage, "grok-imagine-image", true},
		{"openai", MediaImage, "gpt-image-2", true},
		{"vidu", MediaImage, "viduq2", true},
		{"dashscope", MediaImage, "qwen-image-2.0", true},
		{"minimax", MediaImage, "image-01", true},
		{"kling", MediaImage, "kling-image-o1", true},
		{"agnes", MediaImage, "agnes-image-2.5-flash", true},

		{"gemini-aistudio", MediaText, "gemini-3-flash-preview", true},
		{"gemini-vertex", MediaText, "gemini-3-flash-preview", true},
		{"ark", MediaText, "doubao-seed-2-0-lite-260215", true},
		{"ark-agent-plan", MediaText, "doubao-seed-2.0-lite", true},
		{"grok", MediaText, "grok-4-1-fast-reasoning", true},
		{"openai", MediaText, "gpt-5.4-mini", true},
		{"dashscope", MediaText, "qwen-plus", true},
		{"minimax", MediaText, "MiniMax-M3", true},
		{"agnes", MediaText, "agnes-2.5-flash", true},

		{"dashscope", MediaAudio, "qwen3-tts-flash", true},

		// kling / vidu 无文本模型，其余 provider 无音频模型。
		{"kling", MediaText, "", false},
		{"vidu", MediaText, "", false},
		{"kling", MediaAudio, "", false},
		{"vidu", MediaAudio, "", false},
		{"openai", MediaAudio, "", false},
		{"unknown-provider", MediaVideo, "", false},
	}
	for _, c := range cases {
		got, ok := DefaultModelForProvider(c.provider, c.media)
		if ok != c.wantOK || got != c.want {
			t.Errorf("DefaultModelForProvider(%q, %q) = (%q, %v)，期望 (%q, %v)",
				c.provider, c.media, got, ok, c.want, c.wantOK)
		}
	}
}

// registryModelPin 钉住一个模型条目的静态声明（能力 token / 音轨 / 时长 / 分辨率）。
type registryModelPin struct {
	provider    string
	model       string
	media       MediaType
	caps        []ModelCapability
	alwaysOn    bool
	isDefault   bool
	durations   []int
	resolutions []string
}

func TestRegistryModelPins(t *testing.T) {
	cases := []registryModelPin{
		{
			provider: "gemini-aistudio", model: "veo-3.1-lite-generate-preview", media: MediaVideo,
			caps: nil, alwaysOn: true, isDefault: true,
			durations: []int{4, 6, 8}, resolutions: []string{"720p", "1080p"},
		},
		{
			provider: "gemini-vertex", model: "veo-3.1-fast-generate-001", media: MediaVideo,
			caps: []ModelCapability{ModelCapGenerateAudio}, alwaysOn: false, isDefault: true,
			durations: []int{4, 6, 8}, resolutions: []string{"720p", "1080p"},
		},
		{
			provider: "ark", model: "doubao-seedance-2-0-mini-260615", media: MediaVideo,
			caps: []ModelCapability{ModelCapGenerateAudio}, isDefault: true,
			durations: durationRange(4, 15), resolutions: []string{"480p", "720p"},
		},
		{
			provider: "ark", model: "doubao-seedance-2-5-260628", media: MediaVideo,
			caps:      []ModelCapability{ModelCapGenerateAudio},
			durations: durationRange(4, 30), resolutions: []string{"480p", "720p"},
		},
		{
			provider: "ark", model: "doubao-seedream-5-0-lite-260128", media: MediaImage,
			caps: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage}, isDefault: true,
		},
		{
			provider: "ark-agent-plan", model: "doubao-seedance-2.0-mini", media: MediaVideo,
			caps: []ModelCapability{ModelCapGenerateAudio}, isDefault: true,
			durations: durationRange(4, 15), resolutions: []string{"480p", "720p"},
		},
		{
			provider: "grok", model: "grok-imagine-video", media: MediaVideo,
			caps: nil, alwaysOn: true, isDefault: true,
			durations: durationRange(1, 15), resolutions: []string{"480p", "720p"},
		},
		{
			provider: "openai", model: "sora-2", media: MediaVideo,
			caps: nil, alwaysOn: true, isDefault: true,
			durations: []int{4, 8, 12}, resolutions: []string{"720p"},
		},
		{
			provider: "vidu", model: "viduq3-turbo", media: MediaVideo,
			caps: []ModelCapability{ModelCapGenerateAudio}, isDefault: true,
			durations: durationRange(1, 16), resolutions: []string{"540p", "720p", "1080p"},
		},
		{
			provider: "vidu", model: "vidu2.0", media: MediaVideo,
			caps: nil, durations: []int{4, 8}, resolutions: []string{"360p", "720p", "1080p"},
		},
		{
			provider: "dashscope", model: "happyhorse-1.1-i2v", media: MediaVideo,
			caps: nil, alwaysOn: true, isDefault: true,
			durations: durationRange(3, 15), resolutions: []string{"480p", "720p", "1080p"},
		},
		{
			provider: "dashscope", model: "wan3.0-video", media: MediaVideo,
			caps:      []ModelCapability{ModelCapGenerateAudio},
			durations: durationRange(2, 30), resolutions: []string{"480p", "720p", "1080p"},
		},
		{
			provider: "dashscope", model: "qwen-image-edit-plus", media: MediaImage,
			caps:        []ModelCapability{ModelCapImageToImage},
			resolutions: []string{"2048*2048", "2048*1152", "1152*2048", "2048*1536", "1536*2048"},
		},
		{
			provider: "dashscope", model: "qwen3-tts-flash", media: MediaAudio,
			caps: []ModelCapability{ModelCapTextToSpeech}, isDefault: true,
		},
		{
			provider: "minimax", model: "MiniMax-Hailuo-2.3", media: MediaVideo,
			caps: nil, durations: []int{6, 10}, resolutions: []string{"768p", "1080p"},
		},
		{
			provider: "minimax", model: "S2V-01", media: MediaVideo,
			caps: nil, durations: []int{6}, resolutions: []string{"768p"},
		},
		{
			provider: "kling", model: "kling-v2-5-turbo", media: MediaVideo,
			caps: nil, isDefault: true,
			durations: []int{5, 10}, resolutions: []string{"720p", "1080p"},
		},
		{
			provider: "kling", model: "kling-v3-omni-image", media: MediaImage,
			caps:        []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
			resolutions: []string{"1K", "2K", "4K"},
		},
		{
			provider: "kling", model: "kling-v3-turbo", media: MediaVideo,
			caps: nil, durations: durationRange(3, 15), resolutions: []string{"720p", "1080p"},
		},
		{
			provider: "agnes", model: "agnes-video-2.5-flash", media: MediaVideo,
			caps: nil, durations: durationRange(4, 12), resolutions: []string{"720p"},
		},
	}
	for _, c := range cases {
		info, ok := ModelInfoFor(c.provider, c.model)
		if !ok {
			t.Errorf("ModelInfoFor(%q, %q) 未命中", c.provider, c.model)
			continue
		}
		if info.MediaType != c.media {
			t.Errorf("%s/%s MediaType = %q，期望 %q", c.provider, c.model, info.MediaType, c.media)
		}
		if !reflect.DeepEqual(info.Capabilities, c.caps) {
			t.Errorf("%s/%s Capabilities = %v，期望 %v", c.provider, c.model, info.Capabilities, c.caps)
		}
		if info.AudioAlwaysOn != c.alwaysOn {
			t.Errorf("%s/%s AudioAlwaysOn = %v，期望 %v", c.provider, c.model, info.AudioAlwaysOn, c.alwaysOn)
		}
		if info.Default != c.isDefault {
			t.Errorf("%s/%s Default = %v，期望 %v", c.provider, c.model, info.Default, c.isDefault)
		}
		if !reflect.DeepEqual(info.SupportedDurations, c.durations) {
			t.Errorf("%s/%s SupportedDurations = %v，期望 %v", c.provider, c.model, info.SupportedDurations, c.durations)
		}
		if !reflect.DeepEqual(info.Resolutions, c.resolutions) {
			t.Errorf("%s/%s Resolutions = %v，期望 %v", c.provider, c.model, info.Resolutions, c.resolutions)
		}
	}
	if _, ok := ModelInfoFor("unknown-provider", "m"); ok {
		t.Fatal("未登记 provider 的模型查询应返回 ok=false")
	}
	if _, ok := ModelInfoFor("ark", "no-such-model"); ok {
		t.Fatal("未登记模型的查询应返回 ok=false")
	}
}

func TestRegistryDurationConstraints(t *testing.T) {
	cases := []struct {
		provider     string
		model        string
		constraints  map[string][]int
		refDurations []int
	}{
		{"gemini-aistudio", "veo-3.1-lite-generate-preview",
			map[string][]int{"1080p": {8}}, []int{8}},
		{"gemini-aistudio", "veo-3.1-generate-preview",
			map[string][]int{"1080p": {8}, "4k": {8}}, []int{8}},
		{"gemini-vertex", "veo-3.1-fast-generate-001",
			map[string][]int{"1080p": {8}}, []int{8}},
		{"vidu", "vidu2.0",
			map[string][]int{"360p": {4}, "1080p": {4}}, []int{4}},
		{"vidu", "viduq3-turbo", nil, durationRange(3, 16)},
		{"minimax", "MiniMax-Hailuo-2.3", map[string][]int{"1080p": {6}}, nil},
	}
	for _, c := range cases {
		info, ok := ModelInfoFor(c.provider, c.model)
		if !ok {
			t.Errorf("ModelInfoFor(%q, %q) 未命中", c.provider, c.model)
			continue
		}
		if !reflect.DeepEqual(info.DurationResolutionConstraints, c.constraints) {
			t.Errorf("%s/%s DurationResolutionConstraints = %v，期望 %v",
				c.provider, c.model, info.DurationResolutionConstraints, c.constraints)
		}
		if !reflect.DeepEqual(info.ReferenceImageDurations, c.refDurations) {
			t.Errorf("%s/%s ReferenceImageDurations = %v，期望 %v",
				c.provider, c.model, info.ReferenceImageDurations, c.refDurations)
		}
	}
}

func TestRegistryAPIModelName(t *testing.T) {
	cases := []struct {
		provider string
		model    string
		want     string
	}{
		{"kling", "kling-v3-omni-image", "kling-v3-omni"},
		{"kling", "kling-v3-turbo", "kling-3.0-turbo"},
		{"kling", "kling-v2-5-turbo", ""},
		{"kling", "kling-v3-omni", ""},
		{"ark", "doubao-seedance-2-0-mini-260615", ""},
	}
	for _, c := range cases {
		info, ok := ModelInfoFor(c.provider, c.model)
		if !ok {
			t.Errorf("ModelInfoFor(%q, %q) 未命中", c.provider, c.model)
			continue
		}
		if info.APIModelName != c.want {
			t.Errorf("%s/%s APIModelName = %q，期望 %q", c.provider, c.model, info.APIModelName, c.want)
		}
		if info.Hidden {
			t.Errorf("%s/%s 不应声明 Hidden", c.provider, c.model)
		}
	}
}

func TestProviderMetaDerivedViews(t *testing.T) {
	kling, ok := ProviderMetaFor("kling")
	if !ok {
		t.Fatal("kling 未登记")
	}
	if got := kling.MediaTypes(); !reflect.DeepEqual(got, []MediaType{MediaImage, MediaVideo}) {
		t.Fatalf("kling MediaTypes() = %v", got)
	}
	wantCaps := []ModelCapability{ModelCapGenerateAudio, ModelCapImageToImage, ModelCapTextToImage}
	if got := kling.Capabilities(); !reflect.DeepEqual(got, wantCaps) {
		t.Fatalf("kling Capabilities() = %v，期望 %v", got, wantCaps)
	}
	if got := kling.CredentialGroups; !reflect.DeepEqual(got, [][]string{{"api_key"}, {"access_key", "secret_key"}}) {
		t.Fatalf("kling CredentialGroups = %v", got)
	}
	if got := kling.RequiredKeys; !reflect.DeepEqual(got, []string{"api_key", "access_key", "secret_key"}) {
		t.Fatalf("kling RequiredKeys = %v", got)
	}
	if got := kling.DefaultBaseURL; got != "https://api-beijing.klingai.com/v1" {
		t.Fatalf("kling DefaultBaseURL = %q", got)
	}

	ark, _ := ProviderMetaFor("ark")
	if got := ark.MediaTypes(); !reflect.DeepEqual(got, []MediaType{MediaImage, MediaText, MediaVideo}) {
		t.Fatalf("ark MediaTypes() = %v", got)
	}
	if ark.DefaultBaseURL != "https://ark.cn-beijing.volces.com/api/v3" {
		t.Fatalf("ark DefaultBaseURL = %q", ark.DefaultBaseURL)
	}
	if _, ok := ark.Model("doubao-seedance-2-0-mini-260615"); !ok {
		t.Fatal("ark 默认视频模型应可经 ProviderMeta.Model 查到")
	}

	agnes, _ := ProviderMetaFor("agnes")
	if agnes.DefaultConcurrency["video"] != 1 {
		t.Fatalf("agnes video 默认并发应为 1，实得 %v", agnes.DefaultConcurrency)
	}

	dashscope, _ := ProviderMetaFor("dashscope")
	if dashscope.DefaultBaseURL != "https://dashscope.aliyuncs.com" {
		t.Fatalf("dashscope DefaultBaseURL = %q", dashscope.DefaultBaseURL)
	}
	minimax, _ := ProviderMetaFor("minimax")
	if minimax.DefaultBaseURL != "https://api.minimaxi.com/v1" {
		t.Fatalf("minimax DefaultBaseURL = %q", minimax.DefaultBaseURL)
	}
	aap, _ := ProviderMetaFor("ark-agent-plan")
	if aap.DefaultBaseURL != "https://ark.cn-beijing.volces.com/api/plan/v3" {
		t.Fatalf("ark-agent-plan DefaultBaseURL = %q", aap.DefaultBaseURL)
	}
	vertex, _ := ProviderMetaFor("gemini-vertex")
	if !reflect.DeepEqual(vertex.RequiredKeys, []string{"credentials_path"}) || len(vertex.SecretKeys) != 0 {
		t.Fatalf("gemini-vertex 凭证声明异常：%+v", vertex)
	}
}

func TestFullyCoveredCredentialGroups(t *testing.T) {
	kling, _ := ProviderMetaFor("kling")
	cases := []struct {
		values map[string]string
		want   [][]string
	}{
		{map[string]string{"api_key": "k"}, [][]string{{"api_key"}}},
		{map[string]string{"access_key": "a", "secret_key": "s"}, [][]string{{"access_key", "secret_key"}}},
		{map[string]string{"api_key": "k", "access_key": "a", "secret_key": "s"},
			[][]string{{"api_key"}, {"access_key", "secret_key"}}},
		{map[string]string{"access_key": "a"}, nil},
		{nil, nil},
	}
	for _, c := range cases {
		got := kling.FullyCoveredCredentialGroups(c.values)
		if len(got) != len(c.want) {
			t.Fatalf("values=%v 覆盖组 = %v，期望 %v", c.values, got, c.want)
		}
		for i := range got {
			if !reflect.DeepEqual(got[i], c.want[i]) {
				t.Fatalf("values=%v 覆盖组[%d] = %v，期望 %v", c.values, i, got[i], c.want[i])
			}
		}
	}
	ark, _ := ProviderMetaFor("ark")
	if got := ark.FullyCoveredCredentialGroups(map[string]string{"api_key": "k"}); got != nil {
		t.Fatalf("未声明 CredentialGroups 的 provider 应恒返回空，实得 %v", got)
	}
}

func TestAudioTrackSemantics(t *testing.T) {
	get := func(provider, model string) ModelInfo {
		t.Helper()
		info, ok := ModelInfoFor(provider, model)
		if !ok {
			t.Fatalf("ModelInfoFor(%q, %q) 未命中", provider, model)
		}
		return info
	}
	cases := []struct {
		name         string
		provider     string
		model        string
		hasTrack     bool
		controllable bool
		alwaysOn     bool
	}{
		// generate_audio token：开关可控，可开关。
		{"ark seedance", "ark", "doubao-seedance-2-0-mini-260615", true, true, false},
		{"vertex veo", "gemini-vertex", "veo-3.1-fast-generate-001", true, true, false},
		{"dashscope wan3", "dashscope", "wan3.0-video", true, true, false},
		// AudioAlwaysOn：恒有声且不可控。
		{"openai sora", "openai", "sora-2", true, false, true},
		{"aistudio veo", "gemini-aistudio", "veo-3.1-lite-generate-preview", true, false, true},
		{"grok imagine", "grok", "grok-imagine-video", true, false, true},
		{"minimax h3", "minimax", "MiniMax-H3", true, false, true},
		// 例外供应商：无 token 但有音轨（恒有声不可控）。
		{"dashscope happyhorse", "dashscope", "happyhorse-1.1-i2v", true, false, true},
		// 无音轨。
		{"kling v2.5", "kling", "kling-v2-5-turbo", false, false, false},
		{"vidu2.0", "vidu", "vidu2.0", false, false, false},
		{"minimax hailuo", "minimax", "MiniMax-Hailuo-2.3", false, false, false},
	}
	for _, c := range cases {
		info := get(c.provider, c.model)
		if got := ModelHasAudioTrack(c.provider, info); got != c.hasTrack {
			t.Errorf("%s: ModelHasAudioTrack = %v，期望 %v", c.name, got, c.hasTrack)
		}
		if got := ModelAudioSwitchControllable(info); got != c.controllable {
			t.Errorf("%s: ModelAudioSwitchControllable = %v，期望 %v", c.name, got, c.controllable)
		}
		if got := ModelAudioAlwaysOn(c.provider, info); got != c.alwaysOn {
			t.Errorf("%s: ModelAudioAlwaysOn = %v，期望 %v", c.name, got, c.alwaysOn)
		}
	}
	// 非视频模型恒 false。
	imageInfo := get("ark", "doubao-seedream-5-0-lite-260128")
	if ModelHasAudioTrack("ark", imageInfo) || ModelAudioSwitchControllable(imageInfo) || ModelAudioAlwaysOn("ark", imageInfo) {
		t.Fatal("非视频模型的音轨判定应恒为 false")
	}
	// gemini-vertex 无 token 且不在恒有声例外名单内：无音轨。
	vertexInfo := get("gemini-vertex", "veo-3.1-fast-generate-001")
	if !ModelAudioSwitchControllable(vertexInfo) {
		t.Fatal("vertex veo 声明 generate_audio token，开关应可控")
	}
}

func TestBackendInventories(t *testing.T) {
	videoInv := VideoBackendInventory()
	if len(videoInv) != 12 {
		t.Fatalf("视频 backend 清单应为 12 项，实得 %d", len(videoInv))
	}
	registered := 0
	for _, entry := range videoInv {
		if entry.Registered {
			registered++
		}
		if entry.Key == "v2_video_generations" {
			if entry.Registered || entry.ClassName != "V2VideoGenerationsBackend" {
				t.Fatalf("v2_video_generations 应为未注册的 delegate：%+v", entry)
			}
		}
	}
	if registered != 11 {
		t.Fatalf("已注册视频 backend 应为 11 个，实得 %d", registered)
	}
	imageInv := ImageBackendInventory()
	if len(imageInv) != 10 {
		t.Fatalf("图片 backend 清单应为 10 项，实得 %d", len(imageInv))
	}
	for _, entry := range imageInv {
		if !entry.Registered {
			t.Fatalf("图片 backend %q 应全部注册", entry.Key)
		}
	}
	audioInv := AudioBackendInventory()
	if len(audioInv) != 2 {
		t.Fatalf("音频 backend 清单应为 2 项，实得 %d", len(audioInv))
	}
	if entry, ok := AudioBackendEntry("openai"); !ok || entry.Registered {
		t.Fatalf("openai 音频应为未注册 delegate：%+v %v", entry, ok)
	}
	if entry, ok := AudioBackendEntry("dashscope"); !ok || !entry.Registered {
		t.Fatalf("dashscope 音频应为已注册：%+v %v", entry, ok)
	}

	if entry, ok := VideoBackendEntry("newapi"); !ok || entry.ClassName != "NewAPIVideoBackend" || !entry.Registered {
		t.Fatalf("newapi 条目异常：%+v %v", entry, ok)
	}
	if _, ok := VideoBackendEntry("no-such-backend"); ok {
		t.Fatal("未登记的 backend 键应返回 ok=false")
	}
	if _, ok := ImageBackendEntry("no-such-backend"); ok {
		t.Fatal("未登记的图片 backend 键应返回 ok=false")
	}

	// 返回副本：改动不应影响内部清单。
	videoInv[0].Key = "mutated"
	if again := VideoBackendInventory(); again[0].Key != "gemini" {
		t.Fatalf("VideoBackendInventory 应返回副本，实得 %q", again[0].Key)
	}
}

func TestProviderSpecs(t *testing.T) {
	specs := ProviderSpecs()
	if len(specs) != 32 {
		t.Fatalf("内置构造规格应为 32 条（唯一 provider×media 对），实得 %d", len(specs))
	}
	seen := map[providerMediaKey]struct{}{}
	for _, spec := range specs {
		key := providerMediaKey{ProviderID: spec.ProviderID, MediaType: spec.MediaType}
		if _, dup := seen[key]; dup {
			t.Fatalf("规格重复登记：%+v", key)
		}
		seen[key] = struct{}{}
		if spec.RegistryBackend == "" {
			t.Fatalf("规格 %+v 缺 registry_backend", spec)
		}
	}

	cases := []struct {
		provider string
		media    MediaType
		backend  string
		shape    ProviderShape
		extra    []string
	}{
		{"ark", MediaVideo, "ark", ShapeSimple, nil},
		{"dashscope", MediaVideo, "dashscope", ShapeSimple, []string{"wan3_base_url"}},
		{"dashscope", MediaImage, "dashscope", ShapeSimple, nil},
		{"gemini-aistudio", MediaVideo, "gemini", ShapeGemini, nil},
		{"gemini-vertex", MediaImage, "gemini", ShapeGemini, nil},
		{"kling", MediaVideo, "kling", ShapeKling, nil},
		{"openai", MediaImage, "openai", ShapeOpenAIImage, nil},
		{"openai", MediaText, "openai", ShapeTextOpenAICompat, nil},
		{"dashscope", MediaText, "openai", ShapeTextOpenAICompat, nil},
		{"minimax", MediaText, "openai", ShapeTextOpenAICompat, nil},
		{"gemini-aistudio", MediaText, "gemini", ShapeTextGemini, nil},
		{"gemini-vertex", MediaText, "gemini", ShapeTextGemini, nil},
		{"ark", MediaText, "ark", ShapeTextSimple, nil},
		{"agnes", MediaText, "agnes", ShapeTextSimple, nil},
	}
	for _, c := range cases {
		spec, ok := ProviderSpecFor(c.provider, c.media)
		if !ok {
			t.Errorf("ProviderSpecFor(%q, %q) 未命中", c.provider, c.media)
			continue
		}
		if spec.RegistryBackend != c.backend || spec.Shape != c.shape {
			t.Errorf("ProviderSpecFor(%q, %q) = %s/%s，期望 %s/%s",
				c.provider, c.media, spec.RegistryBackend, spec.Shape, c.backend, c.shape)
		}
		if !reflect.DeepEqual(spec.ExtraKeys, c.extra) {
			t.Errorf("ProviderSpecFor(%q, %q) ExtraKeys = %v，期望 %v", c.provider, c.media, spec.ExtraKeys, c.extra)
		}
	}
	for _, miss := range []struct {
		provider string
		media    MediaType
	}{
		{"kling", MediaText},
		{"vidu", MediaText},
		{"vidu", MediaAudio},
		{"agnes", MediaAudio},
		{"unknown-provider", MediaVideo},
	} {
		if _, ok := ProviderSpecFor(miss.provider, miss.media); ok {
			t.Errorf("ProviderSpecFor(%q, %q) 不应命中（fail-loud 不回落）", miss.provider, miss.media)
		}
	}
}

// ---- 运行时 registry ----

type fakeVideoBackend struct {
	opts BackendOptions
}

func (b *fakeVideoBackend) Name() string  { return "fake-video" }
func (b *fakeVideoBackend) Model() string { return b.opts.Model }

func (b *fakeVideoBackend) VideoCapabilities() VideoCapabilities { return DefaultVideoCapabilities() }

func (b *fakeVideoBackend) Generate(context.Context, VideoGenerationRequest) (VideoGenerationResult, error) {
	return VideoGenerationResult{VideoPath: "out.mp4"}, nil
}

func (b *fakeVideoBackend) ResumeVideo(context.Context, string, VideoGenerationRequest) (VideoGenerationResult, error) {
	return VideoGenerationResult{}, ErrResumeUnsupported
}

type fakeImageBackend struct{}

func (b *fakeImageBackend) Name() string  { return "fake-image" }
func (b *fakeImageBackend) Model() string { return "m" }
func (b *fakeImageBackend) Capabilities() ImageCapabilitySet {
	return NewImageCapabilitySet(CapTextToImage)
}

func (b *fakeImageBackend) Generate(context.Context, ImageGenerationRequest) (ImageGenerationResult, error) {
	return ImageGenerationResult{ImagePath: "out.png"}, nil
}

type fakeAudioBackend struct{}

func (b *fakeAudioBackend) Name() string  { return "fake-audio" }
func (b *fakeAudioBackend) Model() string { return "m" }
func (b *fakeAudioBackend) Capabilities() AudioCapabilitySet {
	return NewAudioCapabilitySet(CapTextToSpeech)
}

func (b *fakeAudioBackend) ListVoices() []VoiceOption {
	return []VoiceOption{{ID: "v", Label: "音色"}}
}

func (b *fakeAudioBackend) Synthesize(context.Context, AudioSynthesisRequest) (AudioSynthesisResult, error) {
	return AudioSynthesisResult{Characters: 3}, nil
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func TestRuntimeBackendRegistry(t *testing.T) {
	RegisterVideoBackend("test-video-core", VideoBackendRegistration{
		Create: func(opts BackendOptions) (VideoBackend, error) {
			return &fakeVideoBackend{opts: opts}, nil
		},
		CapabilitiesForModel: func(modelID string) (VideoCapabilities, error) {
			caps := DefaultVideoCapabilities()
			caps.LastFrame = true
			return caps, nil
		},
	})
	RegisterVideoBackend("test-video-no-static", VideoBackendRegistration{
		Create: func(opts BackendOptions) (VideoBackend, error) {
			return &fakeVideoBackend{opts: opts}, nil
		},
	})
	RegisterVideoBackend("test-video-audio-override", VideoBackendRegistration{
		Create: func(opts BackendOptions) (VideoBackend, error) {
			return &fakeVideoBackend{opts: opts}, nil
		},
		EffectiveGenerateAudioForModel: func(modelID string) (bool, error) {
			return modelID != "silent", nil
		},
	})
	RegisterImageBackend("test-image-core", ImageBackendRegistration{
		Create: func(BackendOptions) (ImageBackend, error) { return &fakeImageBackend{}, nil },
	})
	RegisterAudioBackend("test-audio-core", AudioBackendRegistration{
		Create: func(BackendOptions) (AudioBackend, error) { return &fakeAudioBackend{}, nil },
	})

	keys := RegisteredVideoBackendKeys()
	for _, key := range []string{"test-video-core", "test-video-no-static", "test-video-audio-override"} {
		if !containsString(keys, key) {
			t.Fatalf("已注册键列表缺 %q：%v", key, keys)
		}
	}
	if !containsString(RegisteredImageBackendKeys(), "test-image-core") {
		t.Fatal("图片 backend 注册未生效")
	}
	if !containsString(RegisteredAudioBackendKeys(), "test-audio-core") {
		t.Fatal("音频 backend 注册未生效")
	}

	backend, err := CreateVideoBackend("test-video-core", BackendOptions{Model: "m1"})
	if err != nil {
		t.Fatalf("CreateVideoBackend 失败：%v", err)
	}
	if backend.Model() != "m1" || backend.Name() != "fake-video" {
		t.Fatalf("构造入参未透传：%s/%s", backend.Name(), backend.Model())
	}
	if _, err := backend.ResumeVideo(context.Background(), "job", VideoGenerationRequest{}); !errors.Is(err, ErrResumeUnsupported) {
		t.Fatalf("ResumeVideo 应返回 ErrResumeUnsupported，实得 %v", err)
	}
	if _, err := CreateVideoBackend("no-such-backend", BackendOptions{}); err == nil {
		t.Fatal("未注册的视频 backend 键应报错")
	}
	if _, err := CreateImageBackend("no-such-backend", BackendOptions{}); err == nil {
		t.Fatal("未注册的图片 backend 键应报错")
	}
	if _, err := CreateAudioBackend("no-such-backend", BackendOptions{}); err == nil {
		t.Fatal("未注册的音频 backend 键应报错")
	}
	if img, err := CreateImageBackend("test-image-core", BackendOptions{}); err != nil || img.Capabilities().Has(CapImageToImage) {
		t.Fatalf("图片 backend 构造异常：%v %v", img, err)
	}
	if audio, err := CreateAudioBackend("test-audio-core", BackendOptions{}); err != nil || len(audio.ListVoices()) != 1 {
		t.Fatalf("音频 backend 构造异常：%v %v", audio, err)
	}

	caps, err := VideoCapabilitiesForModel("test-video-core", "m1")
	if err != nil || !caps.LastFrame {
		t.Fatalf("CapabilitiesForModel 查询异常：%+v %v", caps, err)
	}
	if _, err := VideoCapabilitiesForModel("test-video-no-static", "m1"); err == nil {
		t.Fatal("未声明静态能力查询的 backend 应报错")
	}
	if _, err := VideoCapabilitiesForModel("no-such-backend", "m1"); err == nil {
		t.Fatal("未注册 backend 的能力查询应报错")
	}
	if got, err := EffectiveGenerateAudioForModel("test-video-no-static", "m1"); err != nil || !got {
		t.Fatalf("未声明专属规则时应沿用 true，实得 %v %v", got, err)
	}
	if got, err := EffectiveGenerateAudioForModel("test-video-audio-override", "silent"); err != nil || got {
		t.Fatalf("专属规则未生效：%v %v", got, err)
	}
	if _, err := EffectiveGenerateAudioForModel("no-such-backend", "m1"); err == nil {
		t.Fatal("未注册 backend 的音频参数查询应报错")
	}
}

func TestDurationRange(t *testing.T) {
	if got := durationRange(3, 5); !reflect.DeepEqual(got, []int{3, 4, 5}) {
		t.Fatalf("durationRange(3, 5) = %v", got)
	}
	if got := durationRange(4, 3); got != nil {
		t.Fatalf("逆序区间应返回 nil，实得 %v", got)
	}
}
