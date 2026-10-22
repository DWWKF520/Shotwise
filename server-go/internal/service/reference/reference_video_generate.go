package service

// 参考生视频 unit 生成的提交入口预检（对应 server/routers/_validators.py 的
// require_video_bucket_capability / require_audio_switch_supported 与
// server/routers/reference_videos.py generate_unit 的资产预检段）。
//
// 解析闸让能力缺失 / 悬空引用在提交入口即返回修复指引，而非任务面板里的异步失败；
// worker 执行时仍经同一解析闸独立校验，预检失效不放行执行。

import (
	"context"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/provider"
)

// VideoBackendResolver 是视频后端身份解析（*config.Resolver 满足）。独立于
// VideoCapabilityResolver 声明：ResolveVideoBackend 不在能力视图接口里，transport 按
// 此窄接口注入依赖、测试注入替身。
type VideoBackendResolver interface {
	ResolveVideoBackend(
		ctx context.Context, project *domain.ProjectFile, payload map[string]any, bucket *provider.VideoBucket,
	) (config.ProviderModel, error)
}

// ValidateTypedReferences 按类型化引用对（type, name）校验资产登记与（可选）设计图可用性。
//
// requireSheetImages 为 false 时（ad 参考集由镜头字段继承）仍报告未登记资产，但接受
// 已登记而设计图缺失 / 不可读的资产——执行层按软口径跳过并记 warning，不在提交期拒绝。
// Go 侧无角色衍生形态（variant）概念，调用方渲染 missing_variants 段时恒传空占位。
func ValidateTypedReferences(
	project *domain.ProjectFile, projectDir string, refs []domain.AdReference, requireSheetImages bool,
) PreflightResult {
	items := make([]itemReference, 0, len(refs))
	for _, ref := range refs {
		items = append(items, itemReference{
			kind:  domain.AssetKind(ref.Type),
			field: "references",
			name:  domain.NormalizeAssetName(ref.Name),
		})
	}
	result := validateAssetReferences(project, projectDir, items)
	if requireSheetImages {
		return result
	}
	kept := make([]PreflightIssue, 0, len(result.Issues))
	for _, issue := range result.Issues {
		if issue.Kind != PreflightKindMissingSheet {
			kept = append(kept, issue)
		}
	}
	return PreflightResult{Issues: kept}
}

// RequireVideoBucketCapability 视频生成入口预检：按能力桶解析全局 + 项目配置并过解析闸
// （docs/adr/0054）。
//
// 解析出的模型缺该桶所需能力、或配置引用已不可用（模型被删 / 能力被改 / 供应商被删）时
// 原样返回 VideoBucketCapabilityError，调用方转 400 让用户在提交入口即看到修复指引。
// 其余解析失败（如未配置任何供应商）返回 nil 放行入队，由 worker 在任务面板暴露——
// 不把非能力类失败升级为提交期拒绝。
func RequireVideoBucketCapability(
	ctx context.Context, resolver VideoBackendResolver, project *domain.ProjectFile, bucket provider.VideoBucket,
) error {
	_, err := resolver.ResolveVideoBackend(ctx, project, nil, &bucket)
	if err == nil {
		return nil
	}
	if _, ok := config.IsVideoBucketCapabilityError(err); ok {
		return err
	}
	return nil
}