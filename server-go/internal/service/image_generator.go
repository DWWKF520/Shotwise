package service

import (
	"context"
	"fmt"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/pricing"
	"github.com/shotwise/shotwise/internal/provider"
	// 图片 backend 在装载期（init）把自己注册进 provider 的 backend registry，本文件
	// 依赖它以拿到可执行的实现；providers 不反向依赖 service。
	"github.com/shotwise/shotwise/internal/providers"
	"gorm.io/gorm"
)

// ImageGenerator 串联「解析身份 → 取凭证 → 构造 backend → 记账括号 → 生成 → 结算」，
// 是图片生成路径的最小闭环（对应 Python lib/media_generator.py 的图片分支）。
//
// 记账括号的不变式在此落地：能力闸门与 backend 构造都在开括号之前，失败不落 pending 行；
// 括号一旦开启，正常返回必须显式声明成功（FinishFailure 兜底会把「未声明终局」写回调用方）。
type ImageGenerator struct {
	db       *gorm.DB
	resolver *config.Resolver
	cfg      *config.ConfigService
	ledger   *Ledger
}

// NewImageGenerator 构造；ledger 为 nil 时只生成不记账（供冒烟 / 诊断路径）。
func NewImageGenerator(db *gorm.DB, ledger *Ledger) *ImageGenerator {
	return &ImageGenerator{
		db:       db,
		resolver: config.NewResolver(db),
		cfg:      config.NewConfigService(db),
		ledger:   ledger,
	}
}

// ImageGenerateParams 是一次图片生成的入参。
type ImageGenerateParams struct {
	// ProjectName 记账归属项目；空串表示无项目上下文（不发用量变更事件）。
	ProjectName string
	// Project 已加载的 project.json（nil 表示只按全局配置解析）。
	Project *domain.ProjectFile
	// Capability 决定走 t2i 还是 i2i 覆盖槽。
	Capability config.ImageCapability
	// Request 生成请求（prompt / output_path / aspect_ratio 等）。
	Request provider.ImageGenerationRequest
	// UserID 记账归属用户；空按 default。
	UserID string
	// SegmentID 分镜标识，供用量聚合。
	SegmentID string
}

// Generate 解析身份 → 构造 backend → 生成 → 结算记账。
//
// 返回 (生成结果, error)：生成失败时错误已写进 failed 记账（若有 ledger）。
func (g *ImageGenerator) Generate(ctx context.Context, p ImageGenerateParams) (provider.ImageGenerationResult, error) {
	selected, err := g.resolver.ResolveImageBackend(ctx, p.Project, nil, p.Capability)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	backend, err := g.buildImageBackend(ctx, selected)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}

	// 记账括号：backend 已就绪才开，构造失败不留 pending 行。
	if g.ledger == nil {
		return backend.Generate(ctx, p.Request)
	}
	call, err := g.ledger.Start(ctx, StartParams{
		ProjectName: p.ProjectName,
		CallType:    "image",
		Model:       selected.ModelID,
		Prompt:      p.Request.Prompt,
		Resolution:  p.Request.ImageSize,
		AspectRatio: p.Request.AspectRatio,
		Provider:    selected.ProviderID,
		UserID:      p.UserID,
		SegmentID:   p.SegmentID,
	})
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	var out provider.ImageGenerationResult
	defer func() { g.ledger.FinishFailure(ctx, call, &err) }()

	out, err = backend.Generate(ctx, p.Request)
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	path := out.ImagePath
	err = g.ledger.FinishSuccess(ctx, call, FinishParams{
		Settlement: pricing.Settlement{
			Quality:           out.Quality,
			ImageInputTokens:  int64ToInt(out.ImageInputTokens),
			ImageOutputTokens: int64ToInt(out.ImageOutputTokens),
			TextInputTokens:   int64ToInt(out.TextInputTokens),
			TextOutputTokens:  int64ToInt(out.TextOutputTokens),
			UsageTokens:       int64ToInt(out.UsageTokens),
		},
		OutputPath: &path,
	})
	if err != nil {
		return provider.ImageGenerationResult{}, err
	}
	return out, nil
}

// buildImageBackend 按解析出的身份构造图片 backend。
//
// 内置供应商走 registry 规格（registry_backend 决定实现、APIModelName 决定下发的模型名）；
// 自定义供应商按模型行的 endpoint/声明走声明式包装层工厂，endpoint 媒体类型与 lane 不符
// 即 fail loud——拿错协议的 delegate 包装自定义供应商会静默发出形态错误的请求。
func (g *ImageGenerator) buildImageBackend(ctx context.Context, selected config.ProviderModel) (provider.ImageBackend, error) {
	if config.IsCustomProvider(selected.ProviderID) {
		input, err := config.LoadCustomBackendInput(ctx, g.db, selected.ProviderID, selected.ModelID)
		if err != nil {
			return nil, err
		}
		backend, err := providers.CreateCustomBackend(providers.CustomProviderConfig{
			ProviderID: input.ProviderID,
			BaseURL:    input.BaseURL,
			APIKey:     input.APIKey,
		}, selected.ModelID, input.Endpoint, input.EndpointDeclaration, input.CapabilityOverrides)
		if err != nil {
			return nil, err
		}
		if backend.MediaType != provider.MediaImage || backend.Image == nil {
			return nil, fmt.Errorf("custom endpoint %q is not an image endpoint", input.Endpoint)
		}
		return backend.Image, nil
	}
	spec, ok := provider.ProviderSpecFor(selected.ProviderID, provider.MediaImage)
	if !ok {
		return nil, fmt.Errorf("no image backend spec for provider %s", selected.ProviderID)
	}
	creds, err := g.cfg.ProviderConfig(ctx, selected.ProviderID)
	if err != nil {
		return nil, err
	}
	model := selected.ModelID
	if info, ok := provider.ModelInfoFor(selected.ProviderID, selected.ModelID); ok && info.APIModelName != "" {
		model = info.APIModelName
	}
	return provider.CreateImageBackend(spec.RegistryBackend, provider.BackendOptions{
		Model:   model,
		APIKey:  creds["api_key"],
		BaseURL: creds["base_url"],
	})
}

// int64ToInt 把 backend 回报的 token 数转成计费侧口径；nil 保持 nil（区分「未提供」与 0）。
func int64ToInt(v *int64) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}
