// 放量开关的公开状态与诊断（对应 lib/feature_flags.py + server/routers/features.py）。
//
// 三层取值口径必须一致：
//   - 生效值：`config.FeatureEnabled`（env 覆盖 → 默认值，非法值 fail closed）；
//   - 诊断面：只暴露「开关名 / 是否生效 / 来源 / 是否合法」，**绝不回传原始 env 值**——
//     放量配置里可能夹带用户误填的密钥，诊断接口不能变成泄漏通道；
//   - 指标面：进程内的粗粒度计数（只保留枚举化维度，不含稿件文本、提示词、路径、资产 ID）。
package http

import (
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/service"
)

// featureFlagEnvNames 是开关名 → env 变量名（与 Python _ENV_NAMES 逐条对应）。
var featureFlagEnvNames = map[string]string{
	"official_creation_skills": "SHOTWISE_FEATURE_OFFICIAL_CREATION_SKILLS",
	"media_asset_index":        "SHOTWISE_MEDIA_ASSET_INDEX",
	"media_library":            "SHOTWISE_FEATURE_MEDIA_LIBRARY",
	"creation_plan":            "SHOTWISE_FEATURE_CREATION_PLAN",
	"creative_board":           "SHOTWISE_FEATURE_CREATIVE_BOARD",
	"context_agent":            "SHOTWISE_FEATURE_CONTEXT_AGENT",
}

// featureFlagDefaults 是各开关未设置 env 时的默认值。
var featureFlagDefaults = map[string]bool{
	"official_creation_skills": true,
	"media_asset_index":        false,
	"media_library":            true,
	"creation_plan":            true,
	"creative_board":           true,
	"context_agent":            true,
}

// featureFlagOrder 是响应里的稳定顺序（Python 用 dict 插入序，同样稳定）。
var featureFlagOrder = []string{
	"official_creation_skills",
	"media_asset_index",
	"media_library",
	"creation_plan",
	"creative_board",
	"context_agent",
}

// featureFlagState 是一个开关的解析结果。
type featureFlagState struct {
	Enabled bool
	Source  string
	Valid   bool
}

// readFeatureFlagState 解析单个开关：set 时按布尔值解析（非法值 fail closed 且标记无效）。
func readFeatureFlagState(name string) featureFlagState {
	raw, present := os.LookupEnv(featureFlagEnvNames[name])
	if !present {
		return featureFlagState{Enabled: featureFlagDefaults[name], Source: "default", Valid: true}
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return featureFlagState{Enabled: true, Source: "env", Valid: true}
	case "0", "false", "no", "off":
		return featureFlagState{Enabled: false, Source: "env", Valid: true}
	default:
		// 非法值按关闭处理（fail closed），只标记「无效」而不回传原始值。
		return featureFlagState{Enabled: false, Source: "env", Valid: false}
	}
}

// FeaturesDeps 是放量诊断路由的依赖。
type FeaturesDeps struct {
	Plans *service.CreationPlanService
}

// RegisterFeatures 注册放量诊断路由。
func RegisterFeatures(group *gin.RouterGroup, d FeaturesDeps) {
	group.GET("/feature-flags", getFeatureFlags())
	group.GET("/feature-flags/metrics", getFeatureMetrics(d))
	group.GET("/feature-flags/validate", validateFeatureFlags())
}

// getFeatureFlags 返回前端做导航裁剪用的公开状态。
func getFeatureFlags() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"features": featureSnapshot()})
	}
}

// getFeatureMetrics 返回放量诊断 + 兼容性聚合（不含任何内容维度）。
func getFeatureMetrics(d FeaturesDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		out := gin.H{
			"features":      featureSnapshot(),
			"configuration": featureAuditSnapshot(),
			// creation 指标由创建技能流程在进程内计数；Go 侧尚无记录点，
			// 这里返回空集而不是省略字段——前端按固定形状渲染。
			"creation": gin.H{"items": []any{}},
		}
		if d.Plans != nil {
			metrics, err := d.Plans.CompatibilityMetrics(c.Request.Context())
			if err != nil {
				apperr.Abort(c, apperr.Internal(err))
				return
			}
			out["compatibility"] = metrics
		} else {
			out["compatibility"] = gin.H{"items": []any{}}
		}
		c.JSON(http.StatusOK, out)
	}
}

// validateFeatureFlags 返回可执行的配置诊断（未知变量名 / 非法布尔值）。
func validateFeatureFlags() gin.HandlerFunc {
	return func(c *gin.Context) {
		errors := []any{}
		known := map[string]bool{}
		for _, envName := range featureFlagEnvNames {
			known[envName] = true
		}
		unknown := make([]string, 0, 4)
		for _, envName := range os.Environ() {
			name := envName
			if idx := strings.IndexByte(envName, '='); idx >= 0 {
				name = envName[:idx]
			}
			if name == "SHOTWISE_MEDIA_ASSET_INDEX" || strings.HasPrefix(name, "SHOTWISE_FEATURE_") {
				if !known[name] {
					unknown = append(unknown, name)
				}
			}
		}
		sort.Strings(unknown)
		for _, name := range unknown {
			errors = append(errors, "unknown feature flag environment variable: "+name)
		}

		flags := gin.H{}
		for _, name := range featureFlagOrder {
			state := readFeatureFlagState(name)
			flags[name] = gin.H{"enabled": state.Enabled, "source": state.Source, "valid": state.Valid}
			if !state.Valid {
				errors = append(errors, "invalid boolean for "+featureFlagEnvNames[name])
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"valid":  len(errors) == 0,
			"errors": errors,
			"flags":  flags,
		})
	}
}

// featureSnapshot 返回稳定的开关生效值。
func featureSnapshot() map[string]bool {
	out := make(map[string]bool, len(featureFlagOrder))
	for _, name := range featureFlagOrder {
		out[name] = config.FeatureEnabled(name)
	}
	return out
}

// featureAuditSnapshot 返回不含原始值的诊断快照。
func featureAuditSnapshot() map[string]any {
	flags := make(map[string]any, len(featureFlagOrder))
	invalid := make([]string, 0, len(featureFlagOrder))
	for _, name := range featureFlagOrder {
		state := readFeatureFlagState(name)
		flags[name] = map[string]any{"enabled": state.Enabled, "source": state.Source, "valid": state.Valid}
		if !state.Valid {
			invalid = append(invalid, name)
		}
	}
	sort.Strings(invalid)
	return map[string]any{
		"flags":         flags,
		"invalid_flags": invalid,
		"invalid_count": len(invalid),
	}
}
