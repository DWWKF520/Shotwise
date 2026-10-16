package config

// 环境变量驱动的功能开关（对应 lib/feature_flags.py）。
// 只暴露已知开关，非法取值一律 fail closed（关闭），不静默开启。

import (
	"os"
	"strings"
)

// featureEnvNames 是开关名 → 环境变量名的映射。
var featureEnvNames = map[string]string{
	"official_creation_skills": "SHOTWISE_FEATURE_OFFICIAL_CREATION_SKILLS",
	"media_asset_index":        "SHOTWISE_MEDIA_ASSET_INDEX",
	"media_library":            "SHOTWISE_FEATURE_MEDIA_LIBRARY",
	"creation_plan":            "SHOTWISE_FEATURE_CREATION_PLAN",
	"creative_board":           "SHOTWISE_FEATURE_CREATIVE_BOARD",
	"context_agent":            "SHOTWISE_FEATURE_CONTEXT_AGENT",
}

// featureDefaults 是未设置环境变量时的默认值。
var featureDefaults = map[string]bool{
	"official_creation_skills": true,
	"media_asset_index":        false,
	"media_library":            true,
	"creation_plan":            true,
	"creative_board":           true,
	"context_agent":            true,
}

var featureTrueValues = map[string]bool{"1": true, "true": true, "yes": true, "on": true}
var featureFalseValues = map[string]bool{"0": true, "false": true, "no": true, "off": true}

// FeatureEnabled 读取一个 rollout 开关；未知开关名返回 false。
func FeatureEnabled(name string) bool {
	envName, ok := featureEnvNames[normalizeFeatureName(name)]
	if !ok {
		return false
	}
	raw, present := os.LookupEnv(envName)
	if !present {
		return featureDefaults[normalizeFeatureName(name)]
	}
	value := strings.ToLower(strings.TrimSpace(raw))
	if featureTrueValues[value] {
		return true
	}
	if featureFalseValues[value] {
		return false
	}
	// 非法的 rollout 取值绝不能意外开启功能。
	return false
}

// normalizeFeatureName 复刻 Python 的 _normalize_name：小写、非字母数字转下划线、去首尾下划线。
func normalizeFeatureName(name string) string {
	lowered := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range lowered {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_")
}
