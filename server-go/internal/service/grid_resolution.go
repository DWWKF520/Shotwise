// 宫格档位门控（对应 server/services/grid_resolution.py）。
//
// 4×4 / 5×5 档只在图像分辨率档为 4K 时放行。判定必须与实际入队、费用估算共用同一份解析，
// 否则前端预览、估价与真正生成的宫格张数会漂移。
package service

import (
	"context"
	"log/slog"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
)

// ImageCapabilityResolver 是图片身份 + 分辨率档的解析能力（*config.Resolver 满足）。
type ImageCapabilityResolver interface {
	ResolveImageBackend(
		ctx context.Context, project *domain.ProjectFile, payload map[string]any, capability config.ImageCapability,
	) (config.ProviderModel, error)
	ResolveResolution(
		ctx context.Context, project *domain.ProjectFile, providerID, modelID string,
	) (string, error)
}

// ResolveImageResolution 取项目 T2I 槽生效的图像分辨率档；空串表示未配置（由调用方按各自
// 保底档处理）。
//
// 以 T2I 槽解析身份：执行侧在有参考图时走 I2I 槽，两槽解析到不同型号时门控与实际渲染可能
// 取到不同档位——门控须在入队时就定档，那时还没有参考图收集结果，取正交能力槽中代表宫格主
// 路径的 T2I。解析失败（未配置图像供应商等）按未配置处理：门控是收紧方向，取不到档位即不放
// 行大宫格。
func ResolveImageResolution(ctx context.Context, resolver ImageCapabilityResolver, pf *domain.ProjectFile) string {
	return resolveImageResolutionFor(ctx, resolver, pf, nil, config.ImageCapT2I)
}

// resolveImageResolutionFor 按给定能力槽与载荷解析生效分辨率档（执行侧按实际槽取档）。
func resolveImageResolutionFor(
	ctx context.Context, resolver ImageCapabilityResolver, pf *domain.ProjectFile,
	payload map[string]any, capability config.ImageCapability,
) string {
	selected, err := resolver.ResolveImageBackend(ctx, pf, payload, capability)
	if err != nil {
		slog.Warn("解析图像身份失败，分辨率档按未配置处理", "err", err)
		return ""
	}
	resolution, err := resolver.ResolveResolution(ctx, pf, selected.ProviderID, selected.ModelID)
	if err != nil {
		slog.Warn("解析图像分辨率档失败，按未配置处理",
			"provider", selected.ProviderID, "model", selected.ModelID, "err", err)
		return ""
	}
	return resolution
}

// ResolveLargeGridAllowed 判定大宫格（4×4 / 5×5）是否放行。
func ResolveLargeGridAllowed(ctx context.Context, resolver ImageCapabilityResolver, pf *domain.ProjectFile) bool {
	return domain.LargeGridAllowed(ResolveImageResolution(ctx, resolver, pf))
}
