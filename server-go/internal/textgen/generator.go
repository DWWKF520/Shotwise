// Package textgen 是 Go 侧最小文本生成后端：按项目与任务类型解析文本 provider/model，
// 再经 Generator 接口发起一次文本生成。当前只实现 gemini-aistudio。
//
// 解析语义对应 Python lib/config/resolver.py 的 text_backend_for_task（同一条分层骨架）：
// 项目档位 > 项目默认 > 全局档位 > 全局默认 > 兜底 config.DefaultTextBackend。
package textgen

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/provider"
)

// Generator 是文本生成的统一接口。
//
// schema 为 JSON Schema，nil 表示自由文本、不要求结构化输出；maxOutputTokens <= 0 表示不设输出上限。
// 返回模型输出的纯文本，JSON 文本由调用方自行解析。
type Generator interface {
	Generate(ctx context.Context, prompt string, schema map[string]any, maxOutputTokens int) (string, error)
}

// TaskType 是文本任务类型，取值与 Python TextTaskType 一致。
type TaskType string

const (
	// TaskScript 剧本生成（复杂档）。
	TaskScript TaskType = "script"
	// TaskOverview 概览生成（简单档）。
	TaskOverview TaskType = "overview"
	// TaskStyleAnalysis 风格分析（简单档）。
	TaskStyleAnalysis TaskType = "style"
)

// ErrUnsupportedProvider 解析出的文本 provider 当前 Go 侧未实现。解析层不回落到其它供应商。
var ErrUnsupportedProvider = errors.New("unsupported text provider")

// ErrOutputTruncated 表示结构化输出被输出上限截断、内容不完整。
//
// 调用方据它短路重试循环：重发同一份必然再截断的请求没有意义。
var ErrOutputTruncated = errors.New("text output truncated")

// tier 是文本任务档位，与 Python TextTaskTier 对应。
type tier string

const (
	tierSimple  tier = "simple"
	tierComplex tier = "complex"
)

// taskTiers 是任务类型到档位的映射，与 Python TEXT_TASK_TIERS 同步。
var taskTiers = map[TaskType]tier{
	TaskScript:        tierComplex,
	TaskOverview:      tierSimple,
	TaskStyleAnalysis: tierSimple,
}

// tierSettingKeys 是档位对应的配置键。项目级 project.json 字段与全局 system_setting 同名。
var tierSettingKeys = map[tier]string{
	tierSimple:  "text_backend_simple",
	tierComplex: "text_backend_complex",
}

const (
	settingDefaultTextBackend = "default_text_backend"
	providerGeminiAistudio    = "gemini-aistudio"
)

// NewGenerator 按任务类型与项目解析文本后端，返回可直接调用的 Generator。
//
// project 可为 nil，此时只参照全局配置。解析结果的 provider 不受支持或已被禁用时直接报错。
// API key 与 base_url 从 provider 配置（含活跃凭证）读取；api_key 缺失时报错。
func NewGenerator(ctx context.Context, db *gorm.DB, project *project.ProjectFile, task TaskType) (Generator, error) {
	target, err := resolveTarget(ctx, db, project, task)
	if err != nil {
		return nil, err
	}
	cfg, err := config.NewConfigService(db).ProviderConfig(ctx, target.ProviderID)
	if err != nil {
		return nil, err
	}
	return newGeminiAistudio(cfg["api_key"], cfg["base_url"], target.ModelID)
}

// resolveTarget 解析任务应使用的 (provider, model)。
//
// 项目层字段兼容裸 provider（取该 provider 的文本默认模型）；全局层要求 "provider/model" 完整形态。
// 选中的候选即使被禁用也直接报错，不继续向下一层回落。
func resolveTarget(
	ctx context.Context, db *gorm.DB, project *project.ProjectFile, task TaskType,
) (config.ProviderModel, error) {
	t, ok := taskTiers[task]
	if !ok {
		return config.ProviderModel{}, fmt.Errorf("未知文本任务类型: %q", task)
	}
	tierKey := tierSettingKeys[t]

	var target config.ProviderModel
	picked := false
	if project != nil {
		for _, raw := range []string{projectTierValue(project, t), project.DefaultTextBackend} {
			if target, picked = parseProjectValue(raw); picked {
				break
			}
		}
	}
	if !picked {
		settings, err := config.NewConfigService(db).GetAllSettings(ctx)
		if err != nil {
			return config.ProviderModel{}, err
		}
		for _, key := range []string{tierKey, settingDefaultTextBackend} {
			if target, picked = parseGlobalValue(settings[key]); picked {
				break
			}
		}
	}
	if !picked {
		providerID, modelID := config.ParseBackend(config.DefaultTextBackend, config.DefaultTextBackend)
		target = config.ProviderModel{ProviderID: providerID, ModelID: modelID}
	}

	if target.ProviderID != providerGeminiAistudio {
		return config.ProviderModel{}, fmt.Errorf(
			"%w: 当前只支持 %s，解析结果为 %s", ErrUnsupportedProvider, providerGeminiAistudio, target.ProviderID,
		)
	}
	enabled, err := config.NewConfigService(db).ProviderEnabled(ctx, target.ProviderID)
	if err != nil {
		return config.ProviderModel{}, err
	}
	if !enabled {
		return config.ProviderModel{}, fmt.Errorf("%w: %s", config.ErrProviderDisabled, target.ProviderID)
	}
	return target, nil
}

// projectTierValue 读项目 project.json 中指定档位的字段。
func projectTierValue(project *project.ProjectFile, t tier) string {
	if t == tierSimple {
		return project.TextBackendSimple
	}
	return project.TextBackendComplex
}

// parseProjectValue 解析项目层取值：完整形态 "provider/model" 直接采用；裸 provider 补其文本默认模型。
func parseProjectValue(raw string) (config.ProviderModel, bool) {
	if providerID, modelID, ok := splitPair(raw); ok {
		return config.ProviderModel{ProviderID: providerID, ModelID: modelID}, true
	}
	providerID := strings.Trim(strings.TrimSpace(raw), "/")
	if providerID == "" {
		return config.ProviderModel{}, false
	}
	modelID, ok := provider.DefaultModelForProvider(providerID, provider.MediaText)
	if !ok {
		return config.ProviderModel{}, false
	}
	return config.ProviderModel{ProviderID: providerID, ModelID: modelID}, true
}

// parseGlobalValue 解析全局层取值：必须含 "/" 且 provider、model 均非空才作数。
func parseGlobalValue(raw string) (config.ProviderModel, bool) {
	if !strings.Contains(raw, "/") {
		return config.ProviderModel{}, false
	}
	providerID, modelID := config.ParseBackend(raw, config.DefaultTextBackend)
	if providerID == "" || modelID == "" {
		return config.ProviderModel{}, false
	}
	return config.ProviderModel{ProviderID: providerID, ModelID: modelID}, true
}

// splitPair 拆分 "provider/model"；任一侧为空白时视为不合法。
func splitPair(raw string) (string, string, bool) {
	providerID, modelID, found := strings.Cut(strings.TrimSpace(raw), "/")
	providerID, modelID = strings.TrimSpace(providerID), strings.TrimSpace(modelID)
	return providerID, modelID, found && providerID != "" && modelID != ""
}
