package pricing

import (
	"testing"
)

// testCatalog 是按 lib/config/registry.py 的费率数值裁剪出的静态目录：只保留对拍所需
// provider/模型，键与费率逐项取自 registry 声明。
func testCatalog() MapCatalog {
	veoStandard := map[MatrixKey]float64{
		{Resolution: "720p", Audio: AudioOn}:   0.40,
		{Resolution: "720p", Audio: AudioOff}:  0.20,
		{Resolution: "1080p", Audio: AudioOn}:  0.40,
		{Resolution: "1080p", Audio: AudioOff}: 0.20,
		{Resolution: "4k", Audio: AudioOn}:     0.60,
		{Resolution: "4k", Audio: AudioOff}:    0.40,
	}
	veoFast := map[MatrixKey]float64{
		{Resolution: "720p", Audio: AudioOn}:   0.10,
		{Resolution: "720p", Audio: AudioOff}:  0.08,
		{Resolution: "1080p", Audio: AudioOn}:  0.12,
		{Resolution: "1080p", Audio: AudioOff}: 0.10,
		{Resolution: "4k", Audio: AudioOn}:     0.30,
		{Resolution: "4k", Audio: AudioOff}:    0.25,
	}
	veoLite := map[MatrixKey]float64{
		{Resolution: "720p", Audio: AudioOn}:   0.05,
		{Resolution: "720p", Audio: AudioOff}:  0.03,
		{Resolution: "1080p", Audio: AudioOn}:  0.08,
		{Resolution: "1080p", Audio: AudioOff}: 0.05,
	}
	klingRates := map[TierKey]float64{
		{Tier: "std", Audio: false}: 0.6,
		{Tier: "std", Audio: true}:  0.8,
		{Tier: "pro", Audio: false}: 0.8,
		{Tier: "pro", Audio: true}:  1.0,
		{Tier: "4k", Audio: false}:  3.0,
		{Tier: "4k", Audio: true}:   3.0,
	}

	geminiText := func(model string, in, out float64) *Pricing {
		p := NewPerToken(map[string]map[string]float64{model: {"input": in, "output": out}}, model, "USD")
		return &p
	}
	gptImage := Pricing{}
	{
		p := NewPerImageOpenAIToken(
			map[string]map[string]float64{
				"gpt-image-2": {
					"image_in": 8.0, "image_cached_in": 2.0, "image_out": 30.0,
					"text_in": 5.0, "text_cached_in": 1.25, "text_out": 0.0,
				},
			},
			map[string]map[QualitySizeKey]float64{
				"gpt-image-2": {
					{Quality: "low", Size: "1024x1024"}:    0.006,
					{Quality: "low", Size: "1024x1792"}:    0.012,
					{Quality: "low", Size: "1792x1024"}:    0.012,
					{Quality: "medium", Size: "1024x1024"}: 0.053,
					{Quality: "medium", Size: "1024x1792"}: 0.106,
					{Quality: "medium", Size: "1792x1024"}: 0.106,
					{Quality: "high", Size: "1024x1024"}:   0.211,
					{Quality: "high", Size: "1024x1792"}:   0.317,
					{Quality: "high", Size: "1792x1024"}:   0.317,
				},
			},
			"gpt-image-2", "USD",
		)
		gptImage = p
	}
	arkVideo := func(model string, rates map[TierKey]float64) *Pricing {
		p := NewPerTokenVideo(map[string]map[TierKey]float64{model: rates}, model, "CNY")
		return &p
	}

	catalog := MapCatalog{
		"gemini-aistudio": {
			Models: map[string]ModelInfo{
				"gemini-3.1-pro-preview":        {MediaType: "text", Pricing: geminiText("gemini-3.1-pro-preview", 2.00, 12.00)},
				"gemini-3-flash-preview":        {MediaType: "text", Pricing: geminiText("gemini-3-flash-preview", 0.50, 3.00)},
				"gemini-3.1-flash-lite-preview": {MediaType: "text", Pricing: geminiText("gemini-3.1-flash-lite-preview", 0.25, 1.50)},
				"gemini-3-pro-image-preview": {
					MediaType: "image",
					Pricing: func() *Pricing {
						p := NewPerImageByResolution(map[string]map[string]float64{
							"gemini-3-pro-image-preview": {"1K": 0.134, "2K": 0.134, "4K": 0.24},
						}, "gemini-3-pro-image-preview", "USD")
						return &p
					}(),
				},
				"gemini-3.1-flash-image-preview": {
					MediaType: "image",
					Pricing: func() *Pricing {
						p := NewPerImageByResolution(map[string]map[string]float64{
							"gemini-3.1-flash-image-preview": {"512PX": 0.045, "1K": 0.067, "2K": 0.101, "4K": 0.151},
						}, "gemini-3.1-flash-image-preview", "USD")
						return &p
					}(),
				},
				"veo-3.1-generate-preview": {
					MediaType: "video",
					Pricing: func() *Pricing {
						p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{"veo-3.1-generate-preview": veoStandard},
							"veo-3.1-generate-preview", DimensionsResolutionAudio, "USD", "")
						return &p
					}(),
				},
				"veo-3.1-fast-generate-preview": {
					MediaType: "video",
					Pricing: func() *Pricing {
						p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{"veo-3.1-fast-generate-preview": veoFast},
							"veo-3.1-fast-generate-preview", DimensionsResolutionAudio, "USD", "")
						return &p
					}(),
				},
				"veo-3.1-lite-generate-preview": {
					MediaType: "video",
					Pricing: func() *Pricing {
						p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{"veo-3.1-lite-generate-preview": veoLite},
							"veo-3.1-lite-generate-preview", DimensionsResolutionAudio, "USD", "")
						return &p
					}(),
				},
				// 已下线（hidden）模型仍可命中：目录视图只登记媒体类型与定价。
				"gemini-3-flash-retired": {MediaType: "text", Pricing: geminiText("gemini-3-flash-retired", 0.50, 3.00)},
			},
			DefaultModel: map[string]string{
				"text":  "gemini-3-flash-preview",
				"image": "gemini-3.1-flash-image-preview",
				"video": "veo-3.1-lite-generate-preview",
			},
		},
		"gemini-vertex": {
			Models: map[string]ModelInfo{
				"gemini-3-flash-preview": {MediaType: "text", Pricing: geminiText("gemini-3-flash-preview", 0.50, 3.00)},
				"veo-3.1-generate-001": {
					MediaType: "video",
					Pricing: func() *Pricing {
						p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{"veo-3.1-generate-001": veoStandard},
							"veo-3.1-generate-001", DimensionsResolutionAudio, "USD", "")
						return &p
					}(),
				},
				"veo-3.1-fast-generate-001": {
					MediaType: "video",
					Pricing: func() *Pricing {
						p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{"veo-3.1-fast-generate-001": veoFast},
							"veo-3.1-fast-generate-001", DimensionsResolutionAudio, "USD", "")
						return &p
					}(),
				},
			},
			DefaultModel: map[string]string{
				"text": "gemini-3-flash-preview",
				// vertex 视频默认是 fast-001，但通用兜底取 aistudio 的 lite，不取本默认。
				"video": "veo-3.1-fast-generate-001",
			},
		},
		"ark": {
			Models: map[string]ModelInfo{
				"doubao-seed-2-0-lite-260215": {MediaType: "text", Pricing: geminiTextCurrency("doubao-seed-2-0-lite-260215", 0.60, 3.60, "CNY")},
				"doubao-seedream-5-0-lite-260128": {
					MediaType: "image",
					Pricing: func() *Pricing {
						p := NewPerImageFlat(map[string]float64{"doubao-seedream-5-0-lite-260128": 0.22}, "doubao-seedream-5-0-lite-260128", "CNY")
						return &p
					}(),
				},
				"doubao-seedream-4-5-251128": {
					MediaType: "image",
					Pricing: func() *Pricing {
						p := NewPerImageFlat(map[string]float64{"doubao-seedream-4-5-251128": 0.25}, "doubao-seedream-4-5-251128", "CNY")
						return &p
					}(),
				},
				"doubao-seedance-1-5-pro-251215": {MediaType: "video", Pricing: arkVideo("doubao-seedance-1-5-pro-251215", map[TierKey]float64{
					{Tier: "default", Audio: true}:  16.00,
					{Tier: "default", Audio: false}: 8.00,
					{Tier: "flex", Audio: true}:     8.00,
					{Tier: "flex", Audio: false}:    4.00,
				})},
				"doubao-seedance-2-0-260128": {MediaType: "video", Pricing: arkVideo("doubao-seedance-2-0-260128", map[TierKey]float64{
					{Tier: "default", Audio: true}:  46.00,
					{Tier: "default", Audio: false}: 46.00,
				})},
				"doubao-seedance-2-0-fast-260128": {MediaType: "video", Pricing: arkVideo("doubao-seedance-2-0-fast-260128", map[TierKey]float64{
					{Tier: "default", Audio: true}:  37.00,
					{Tier: "default", Audio: false}: 37.00,
				})},
				"doubao-seedance-2-0-mini-260615": {MediaType: "video", Pricing: arkVideo("doubao-seedance-2-0-mini-260615", map[TierKey]float64{
					{Tier: "default", Audio: true}:  23.00,
					{Tier: "default", Audio: false}: 23.00,
				})},
			},
			DefaultModel: map[string]string{
				"text":  "doubao-seed-2-0-lite-260215",
				"image": "doubao-seedream-5-0-lite-260128",
				"video": "doubao-seedance-2-0-mini-260615",
			},
		},
		"ark-agent-plan": {
			Models: map[string]ModelInfo{
				// 套餐价无独立费率表：已登记但 pricing 缺省，查找时继续回落。
				"doubao-seed-2.0-lite":     {MediaType: "text"},
				"doubao-seedance-2.0":      {MediaType: "video"},
				"doubao-seedream-5.0-lite": {MediaType: "image"},
			},
			DefaultModel: map[string]string{
				"text":  "doubao-seed-2.0-lite",
				"image": "doubao-seedream-5.0-lite",
				"video": "doubao-seedance-2.0-mini",
			},
		},
		"openai": {
			Models: map[string]ModelInfo{
				"gpt-5.5":      {MediaType: "text", Pricing: geminiTextCurrency("gpt-5.5", 5.00, 30.00, "USD")},
				"gpt-5.4":      {MediaType: "text", Pricing: geminiTextCurrency("gpt-5.4", 2.50, 15.00, "USD")},
				"gpt-5.4-mini": {MediaType: "text", Pricing: geminiTextCurrency("gpt-5.4-mini", 0.75, 4.50, "USD")},
				"gpt-image-2":  {MediaType: "image", Pricing: &gptImage},
				"sora-2": {
					MediaType: "video",
					Pricing: func() *Pricing {
						p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{
							"sora-2": {{Resolution: "720p", Audio: AudioUnspecified}: 0.10},
						}, "sora-2", DimensionsResolutionOnly, "USD", "")
						return &p
					}(),
				},
				"sora-2-pro": {
					MediaType: "video",
					Pricing: func() *Pricing {
						p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{
							"sora-2-pro": {
								{Resolution: "720p", Audio: AudioUnspecified}:  0.30,
								{Resolution: "1024p", Audio: AudioUnspecified}: 0.50,
								{Resolution: "1080p", Audio: AudioUnspecified}: 0.70,
							},
						}, "sora-2-pro", DimensionsResolutionOnly, "USD", "")
						return &p
					}(),
				},
			},
			DefaultModel: map[string]string{
				"text":  "gpt-5.4-mini",
				"image": "gpt-image-2",
				"video": "sora-2",
			},
		},
		"grok": {
			Models: map[string]ModelInfo{
				"grok-4-1-fast-reasoning": {MediaType: "text", Pricing: geminiTextCurrency("grok-4-1-fast-reasoning", 0.20, 0.50, "USD")},
				"grok-imagine-image": {
					MediaType: "image",
					Pricing: func() *Pricing {
						p := NewPerImageFlat(map[string]float64{"grok-imagine-image": 0.02}, "grok-imagine-image", "USD")
						return &p
					}(),
				},
				"grok-imagine-image-pro": {
					MediaType: "image",
					Pricing: func() *Pricing {
						p := NewPerImageFlat(map[string]float64{"grok-imagine-image-pro": 0.07}, "grok-imagine-image-pro", "USD")
						return &p
					}(),
				},
				"grok-imagine-video": {
					MediaType: "video",
					Pricing: func() *Pricing {
						p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{
							"grok-imagine-video": {{Audio: AudioUnspecified}: 0.050},
						}, "grok-imagine-video", DimensionsFlat, "USD", "")
						return &p
					}(),
				},
			},
			DefaultModel: map[string]string{
				"text":  "grok-4-1-fast-reasoning",
				"image": "grok-imagine-image",
				"video": "grok-imagine-video",
			},
		},
		"kling": {
			Models: map[string]ModelInfo{
				"kling-v2-5-turbo": {
					MediaType: "video",
					Pricing: func() *Pricing {
						p := NewPerSecondTiered(map[string]map[TierKey]float64{"kling-v2-5-turbo": klingRates}, "kling-v2-5-turbo", "CNY")
						return &p
					}(),
				},
			},
			DefaultModel: map[string]string{"video": "kling-v2-5-turbo"},
		},
		"minimax": {
			Models: map[string]ModelInfo{
				"MiniMax-M3": {MediaType: "text", Pricing: geminiTextCurrency("MiniMax-M3", 2.1, 8.4, "CNY")},
				"MiniMax-H3": {
					MediaType: "video",
					Pricing: func() *Pricing {
						p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{
							"MiniMax-H3": {
								{Resolution: "768p", Audio: AudioUnspecified}: 0.50,
								{Resolution: "2k", Audio: AudioUnspecified}:   0.80,
							},
						}, "MiniMax-H3", DimensionsResolutionOnly, "CNY", "768p")
						return &p
					}(),
				},
			},
			DefaultModel: map[string]string{"text": "MiniMax-M3", "video": "MiniMax-H3"},
		},
		"agnes": {
			Models: map[string]ModelInfo{
				"agnes-2.0-flash": {MediaType: "text", Pricing: geminiTextCurrency("agnes-2.0-flash", 0.03, 0.15, "USD")},
				"agnes-2.5-flash": {MediaType: "text", Pricing: geminiTextCurrency("agnes-2.5-flash", 0.03, 0.15, "USD")},
				"agnes-image-2.1-flash": {
					MediaType: "image",
					Pricing: func() *Pricing {
						p := NewPerImageFlat(map[string]float64{"agnes-image-2.1-flash": 0.003}, "agnes-image-2.1-flash", "USD")
						return &p
					}(),
				},
				"agnes-image-2.5-flash": {
					MediaType: "image",
					Pricing: func() *Pricing {
						p := NewPerImageFlat(map[string]float64{"agnes-image-2.5-flash": 0.003}, "agnes-image-2.5-flash", "USD")
						return &p
					}(),
				},
				"agnes-video-v2.0": {
					MediaType: "video",
					Pricing: func() *Pricing {
						p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{
							"agnes-video-v2.0": {{Audio: AudioUnspecified}: 0.005},
						}, "agnes-video-v2.0", DimensionsFlat, "USD", "")
						return &p
					}(),
				},
				"agnes-video-2.5": {
					MediaType: "video",
					Pricing: func() *Pricing {
						p := NewPerSecondMatrix(map[string]map[MatrixKey]float64{
							"agnes-video-2.5": {{Audio: AudioUnspecified}: 0.005},
						}, "agnes-video-2.5", DimensionsFlat, "USD", "")
						return &p
					}(),
				},
			},
			DefaultModel: map[string]string{
				"text":  "agnes-2.5-flash",
				"image": "agnes-image-2.5-flash",
				"video": "agnes-video-2.5",
			},
		},
		"vidu": {
			Models: map[string]ModelInfo{
				"viduq2": {MediaType: "image", Pricing: func() *Pricing {
					p := NewViduDelegate("CNY")
					return &p
				}()},
			},
			DefaultModel: map[string]string{"image": "viduq2"},
		},
	}
	return catalog
}

// geminiTextCurrency 与 geminiText 同形，但可指定币种（ark/minimax 表的 CNY 文本模型）。
func geminiTextCurrency(model string, in, out float64, currency string) *Pricing {
	p := NewPerToken(map[string]map[string]float64{model: {"input": in, "output": out}}, model, currency)
	return &p
}

func TestLookupRegistryHit(t *testing.T) {
	// 向量来自 tests/test_pricing_lookup.py::TestRegistryHit。
	catalog := testCatalog()

	img, err := LookupPricing(catalog, "gemini-aistudio", "gemini-3.1-flash-image-preview", "image")
	if err != nil {
		t.Fatalf("gemini image: %v", err)
	}
	if img.Kind != KindPerImageByResolution {
		t.Fatalf("kind = %q, 期望 per_image_by_resolution", img.Kind)
	}
	if got := img.ResolutionRates["gemini-3.1-flash-image-preview"]["1K"]; got != 0.067 {
		t.Errorf("1K 费率 = %v, 期望 0.067", got)
	}

	v001, err := LookupPricing(catalog, "gemini-vertex", "veo-3.1-fast-generate-001", "video")
	if err != nil {
		t.Fatalf("vertex video: %v", err)
	}
	if v001.Kind != KindPerSecondMatrix || v001.Dimensions != DimensionsResolutionAudio {
		t.Errorf("vertex 视频命中 = (%q, %q), 期望 (per_second_matrix, resolution_audio)", v001.Kind, v001.Dimensions)
	}

	oai, err := LookupPricing(catalog, "openai", "gpt-image-2", "image")
	if err != nil {
		t.Fatalf("openai image: %v", err)
	}
	if oai.Kind != KindPerImageOpenAIToken {
		t.Errorf("kind = %q, 期望 per_image_openai_token", oai.Kind)
	}

	ark, err := LookupPricing(catalog, "ark", "doubao-seedance-1-5-pro-251215", "video")
	if err != nil {
		t.Fatalf("ark video: %v", err)
	}
	if ark.Kind != KindPerTokenVideo || ark.Currency != "CNY" {
		t.Errorf("ark 视频命中 = (%q, %q), 期望 (per_token_video, CNY)", ark.Kind, ark.Currency)
	}

	arkMini, err := LookupPricing(catalog, "ark", "doubao-seedance-2-0-mini-260615", "video")
	if err != nil {
		t.Fatalf("ark mini video: %v", err)
	}
	if arkMini.Kind != KindPerTokenVideo || arkMini.Currency != "CNY" {
		t.Errorf("ark mini 命中 = (%q, %q), 期望 (per_token_video, CNY)", arkMini.Kind, arkMini.Currency)
	}

	agnes, err := LookupPricing(catalog, "agnes", "agnes-2.5-flash", "text")
	if err != nil {
		t.Fatalf("agnes text: %v", err)
	}
	if agnes.Kind != KindPerToken || agnes.Currency != "USD" {
		t.Fatalf("agnes 文本命中 = (%q, %q), 期望 (per_token, USD)", agnes.Kind, agnes.Currency)
	}
	if rate := agnes.TokenRates["agnes-2.5-flash"]; rate["input"] != 0.03 || rate["output"] != 0.15 {
		t.Errorf("agnes-2.5-flash 费率 = %v, 期望 input 0.03 / output 0.15", rate)
	}

	agnesVideo, err := LookupPricing(catalog, "agnes", "agnes-video-2.5", "video")
	if err != nil {
		t.Fatalf("agnes video: %v", err)
	}
	if agnesVideo.Kind != KindPerSecondMatrix || agnesVideo.Dimensions != DimensionsFlat || agnesVideo.Currency != "USD" {
		t.Fatalf("agnes 视频命中 = (%q, %q, %q)", agnesVideo.Kind, agnesVideo.Dimensions, agnesVideo.Currency)
	}
	amount, _ := mustCalculate(t, agnesVideo, Params{CallType: "video", Model: "agnes-video-2.5", DurationSeconds: IntPtr(10)})
	checkAmount(t, amount, 0.05, "agnes flat 按秒 0.005 × 10s")
}

func TestLookupUnknownProviderFallsBackToGemini(t *testing.T) {
	// 向量来自 tests/test_pricing_lookup.py::TestUnknownProviderFallsBackToGemini。
	catalog := testCatalog()
	for _, provider := range []string{"gemini", "unknown", "seedance"} {
		p, err := LookupPricing(catalog, provider, "", "text")
		if err != nil {
			t.Fatalf("%s text: %v", provider, err)
		}
		if p.Kind != KindPerToken {
			t.Fatalf("%s: kind = %q, 期望 per_token", provider, p.Kind)
		}
		if rate := p.TokenRates["gemini-3-flash-preview"]; rate["input"] != 0.50 || rate["output"] != 3.00 {
			t.Errorf("%s: 兜底文本费率 = %v, 期望 input 0.50 / output 3.00", provider, rate)
		}
	}

	img, err := LookupPricing(catalog, "unknown", "", "image")
	if err != nil {
		t.Fatalf("unknown image: %v", err)
	}
	if _, ok := img.ResolutionRates["gemini-3.1-flash-image-preview"]; !ok {
		t.Error("未知 provider 图片兜底应含 gemini-3.1-flash-image-preview 费率")
	}

	video, err := LookupPricing(catalog, "unknown", "", "video")
	if err != nil {
		t.Fatalf("unknown video: %v", err)
	}
	if _, ok := video.MatrixRates["veo-3.1-lite-generate-preview"]; !ok {
		t.Error("未知 provider 视频兜底应含 veo-3.1-lite-generate-preview 费率")
	}
}

func TestLookupAnthropicException(t *testing.T) {
	// 向量来自 tests/test_pricing_lookup.py::TestAnthropicException。
	catalog := testCatalog()

	p, err := LookupPricing(catalog, "anthropic", "claude-sonnet-4", "text")
	if err != nil {
		t.Fatalf("anthropic: %v", err)
	}
	if p.Kind != KindPerToken || p.Currency != "USD" {
		t.Fatalf("命中 = (%q, %q), 期望 (per_token, USD)", p.Kind, p.Currency)
	}
	if rate := p.TokenRates["claude-sonnet-4"]; rate["input"] != 3.00 || rate["output"] != 15.00 {
		t.Errorf("费率 = %v, 期望 input 3.00 / output 15.00", rate)
	}

	unknown, err := LookupPricing(catalog, "anthropic", "unknown-claude", "text")
	if err != nil {
		t.Fatalf("anthropic unknown: %v", err)
	}
	if unknown.DefaultModel != "claude-sonnet-4" {
		t.Errorf("默认模型 = %q, 期望 claude-sonnet-4（不回落到 Gemini）", unknown.DefaultModel)
	}
}

func TestLookupViduDelegate(t *testing.T) {
	// 向量来自 tests/test_pricing_lookup.py::TestViduDelegate：任意 model 都返回委托标记。
	catalog := testCatalog()
	for _, tc := range []struct{ model, callType string }{
		{"viduq3-turbo", "video"},
		{"totally-unknown", "image"},
	} {
		p, err := LookupPricing(catalog, "vidu", tc.model, tc.callType)
		if err != nil {
			t.Fatalf("vidu %s: %v", tc.model, err)
		}
		if p.Kind != KindViduDelegate {
			t.Errorf("vidu %s/%s: kind = %q, 期望 vidu_delegate", tc.model, tc.callType, p.Kind)
		}
	}
}

func TestLookupUnknownModelFallsBackToOwnDefault(t *testing.T) {
	// 向量来自 tests/test_pricing_lookup.py::TestUnknownModelFallback。
	catalog := testCatalog()

	ark, err := LookupPricing(catalog, "ark", "no-such-model", "video")
	if err != nil {
		t.Fatalf("ark unknown: %v", err)
	}
	if ark.Kind != KindPerTokenVideo {
		t.Fatalf("ark 未知模型 kind = %q, 期望 per_token_video", ark.Kind)
	}
	if _, ok := ark.TieredRates["doubao-seedance-2-0-mini-260615"]; !ok {
		t.Error("ark 未知模型应回落 ark 默认视频模型（mini）")
	}

	agnesImg, err := LookupPricing(catalog, "agnes", "agnes-image-2.0-unregistered", "image")
	if err != nil {
		t.Fatalf("agnes unknown image: %v", err)
	}
	if agnesImg.Kind != KindPerImageFlat || agnesImg.Currency != "USD" {
		t.Fatalf("agnes 未知图片 = (%q, %q), 期望 (per_image_flat, USD)", agnesImg.Kind, agnesImg.Currency)
	}
	if got := agnesImg.FlatRates["agnes-image-2.5-flash"]; got != 0.003 {
		t.Errorf("agnes 未知图片应回落自有默认 0.003, 实际 %v", got)
	}

	agnesText, err := LookupPricing(catalog, "agnes", "agnes-2.0-unregistered", "text")
	if err != nil {
		t.Fatalf("agnes unknown text: %v", err)
	}
	if rate := agnesText.TokenRates["agnes-2.5-flash"]; rate["input"] != 0.03 || rate["output"] != 0.15 {
		t.Errorf("agnes 未知文本应回落自有默认费率, 实际 %v", rate)
	}
}

func TestLookupAgentPlanNoPricingFallsBackToGemini(t *testing.T) {
	// 向量来自 tests/test_pricing_lookup.py::TestUnknownModelFallback::test_agent_plan_no_pricing_falls_back_to_gemini_quietly。
	catalog := testCatalog()
	p, err := LookupPricing(catalog, "ark-agent-plan", "doubao-seedance-2.0", "video")
	if err != nil {
		t.Fatalf("agent plan: %v", err)
	}
	if p.Kind != KindPerSecondMatrix {
		t.Fatalf("kind = %q, 期望 per_second_matrix", p.Kind)
	}
	if _, ok := p.MatrixRates["veo-3.1-lite-generate-preview"]; !ok {
		t.Error("Agent Plan 未声明定价应回落 Gemini 默认视频费率")
	}
}

func TestLookupGeminiFamilyVideoFallback(t *testing.T) {
	// 向量来自 tests/test_pricing_lookup.py::TestGeminiFamilyVideoFallback。
	catalog := testCatalog()

	for _, model := range []string{"", "veo-removed-from-registry"} {
		p, err := LookupPricing(catalog, "gemini-vertex", model, "video")
		if err != nil {
			t.Fatalf("vertex %q: %v", model, err)
		}
		if _, ok := p.MatrixRates["veo-3.1-lite-generate-preview"]; !ok {
			t.Errorf("vertex %q 应回落通用 lite 默认，而非 vertex 自身 fast-001 默认", model)
		}
		if got := p.MatrixRates["veo-3.1-lite-generate-preview"][MatrixKey{Resolution: "1080p", Audio: AudioOn}]; got != 0.08 {
			t.Errorf("lite 1080p+audio 费率 = %v, 期望 0.08", got)
		}
	}

	own, err := LookupPricing(catalog, "gemini-vertex", "veo-3.1-generate-001", "video")
	if err != nil {
		t.Fatalf("vertex known: %v", err)
	}
	if got := own.MatrixRates["veo-3.1-generate-001"][MatrixKey{Resolution: "1080p", Audio: AudioOn}]; got != 0.40 {
		t.Errorf("vertex 已知模型 1080p+audio 费率 = %v, 期望 0.40", got)
	}
}

func TestLookupCrossModalFallback(t *testing.T) {
	// 向量来自 tests/test_pricing_lookup.py::TestCrossModalModelMismatch。
	catalog := testCatalog()

	oai, err := LookupPricing(catalog, "openai", "gpt-image-2", "video")
	if err != nil {
		t.Fatalf("openai image model on video: %v", err)
	}
	if oai.Kind != KindPerSecondMatrix {
		t.Fatalf("openai 视频调用带图像模型名 kind = %q, 期望 per_second_matrix", oai.Kind)
	}
	if _, ok := oai.MatrixRates["sora-2"]; !ok {
		t.Error("openai 跨模态应回落其视频默认模型 sora-2")
	}

	gem, err := LookupPricing(catalog, "gemini-aistudio", "gemini-3.1-flash-image-preview", "video")
	if err != nil {
		t.Fatalf("gemini image model on video: %v", err)
	}
	if _, ok := gem.MatrixRates["veo-3.1-lite-generate-preview"]; !ok {
		t.Error("gemini 家族跨模态应回落视频通用默认（lite）")
	}
}

func TestLookupHiddenModelStillResolves(t *testing.T) {
	// 向量来自 tests/test_pricing_lookup.py::TestHiddenModelStillResolves：
	// 模型下线后入队遗留任务仍需算价，查找不受上架状态影响。
	catalog := testCatalog()
	p, err := LookupPricing(catalog, "gemini-aistudio", "gemini-3-flash-retired", "text")
	if err != nil {
		t.Fatalf("hidden model: %v", err)
	}
	if rate := p.TokenRates["gemini-3-flash-retired"]; rate["input"] != 0.50 || rate["output"] != 3.00 {
		t.Errorf("下线模型应命中自身费率, 实际 %v", rate)
	}
}

func TestLookupPanicOnMissingCatalogFallback(t *testing.T) {
	// 未知 provider 且目录缺失时兜底无从查起：报错而非静默计零。
	if _, err := LookupPricing(nil, "unknown", "", "video"); err == nil {
		t.Fatal("无目录且需 Gemini 兜底时必须报错")
	}
}
