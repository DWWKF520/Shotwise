package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/shotwise/shotwise/internal/repository"
	"gorm.io/gorm"
)

// 自定义供应商的身份约定与 DB 查询。
//
// provider_id 形如 "custom-<db_id>"（lib/custom_provider/__init__.py 同源）；endpoint 键 →
// media_type 的映射与 Python 侧 ENDPOINT_REGISTRY 一一对应（这里只迁移身份判定所需的
// media_type 与 int 型参考图上限两个维度；请求模板与 delegate 构造随 backend 波次落地）。

// CustomProviderPrefix 自定义供应商 provider_id 前缀。
const CustomProviderPrefix = "custom-"

// MakeProviderID 由 custom_provider 表主键构造 provider_id。
func MakeProviderID(dbID int64) string {
	return fmt.Sprintf("%s%d", CustomProviderPrefix, dbID)
}

// IsCustomProvider 判定是否自定义供应商 id（仅前缀检查，与 Python 侧同语义）。
func IsCustomProvider(providerID string) bool {
	return strings.HasPrefix(providerID, CustomProviderPrefix)
}

// ParseProviderID 解析 "custom-<id>" → DB 主键；非整数形态返回错误。
func ParseProviderID(providerID string) (int64, error) {
	if !IsCustomProvider(providerID) {
		return 0, fmt.Errorf("not a custom provider id: %s", providerID)
	}
	raw := strings.TrimPrefix(providerID, CustomProviderPrefix)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid custom provider id: %s", providerID)
	}
	return id, nil
}

// endpointMediaTypes 是 endpoint 键 → media_type 的完整映射（Python ENDPOINT_REGISTRY
// 的注册表不变式保证每个键恰有一个 media_type）。
var endpointMediaTypes = map[string]string{
	"openai-chat":               "text",
	"gemini-generate":           "text",
	"openai-images":             "image",
	"openai-images-generations": "image",
	"openai-images-edits":       "image",
	"gemini-image":              "image",
	"dashscope-image":           "image",
	"minimax-image":             "image",
	"kling-image":               "image",
	"openai-video":              "video",
	"newapi-video":              "video",
	"v2-video-generations":      "video",
	"ark-seedance":              "video",
	"vidu-video":                "video",
	"dashscope-async-video":     "video",
	"minimax-video":             "video",
	"kling-video":               "video",
	"openai-tts":                "audio",
}

// EndpointMediaType 返回 endpoint 的媒体类型；未知 endpoint 报错（fail loud）。
func EndpointMediaType(endpoint string) (string, error) {
	media, ok := endpointMediaTypes[endpoint]
	if !ok {
		return "", fmt.Errorf("unknown endpoint: %q", endpoint)
	}
	return media, nil
}

// endpointVideoMaxReferenceImages 是 int 硬上限型 video endpoint 的参考图上限。其余 video
// endpoint 的上限随 model 异质，声明在各 backend 的 per-model 纯 caps 函数里（provider
// registry 的 VideoCapabilitiesForModel）；对这类 endpoint 合成能力时 fail loud，不猜测。
var endpointVideoMaxReferenceImages = map[string]int{
	"openai-video": 1,
	"newapi-video": 0,
}

// customModelWithProvider 一次取模型行 + 供应商行；模型不存在时两者均为 nil（调用方
// 自行回退查供应商行，语义与 Python get_model_with_provider 的 join 未命中分支一致）。
func customModelWithProvider(db *gorm.DB, dbID int64, modelID string) (*repository.CustomProviderModel, *repository.CustomProvider, error) {
	var model repository.CustomProviderModel
	err := db.Where("provider_id = ? AND model_id = ?", dbID, modelID).First(&model).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	prov, err := customProviderByID(db, dbID)
	if err != nil {
		return nil, nil, err
	}
	return &model, prov, nil
}

func customProviderByID(db *gorm.DB, dbID int64) (*repository.CustomProvider, error) {
	var prov repository.CustomProvider
	if err := db.First(&prov, dbID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &prov, nil
}

// enabledCustomModelsByMedia 列出全部「供应商启用 + 模型启用 + endpoint 媒体类型匹配」
// 的自定义模型（auto-resolve 的自定义回退用），按 provider_id / model_id 排序保证确定性。
// 查询失败返回 nil——auto-resolve 在此时段内自然回落到报错路径，不因一次 DB 抖动放大故障。
func enabledCustomModelsByMedia(db *gorm.DB, media string) []repository.CustomProviderModel {
	var rows []repository.CustomProviderModel
	err := db.Model(&repository.CustomProviderModel{}).
		Joins("JOIN custom_provider ON custom_provider.id = custom_provider_model.provider_id").
		Where("custom_provider.is_enabled = ? AND custom_provider_model.is_enabled = ?", true, true).
		Order("custom_provider_model.provider_id").
		Order("custom_provider_model.model_id").
		Find(&rows).Error
	if err != nil {
		return nil
	}
	out := make([]repository.CustomProviderModel, 0, len(rows))
	for _, row := range rows {
		if m, err := EndpointMediaType(row.Endpoint); err == nil && m == media {
			out = append(out, row)
		}
	}
	return out
}

// customDefaultModelByMedia 取该自定义供应商在该媒体类型的默认启用模型；无则 nil。
func customDefaultModelByMedia(db *gorm.DB, dbID int64, media string) *repository.CustomProviderModel {
	for _, row := range enabledCustomModelsByMedia(db, media) {
		if row.ProviderID == dbID && row.IsDefault {
			m := row
			return &m
		}
	}
	return nil
}
