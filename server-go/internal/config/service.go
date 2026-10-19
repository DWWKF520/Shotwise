package config

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	"gorm.io/gorm"
)

// ConfigService 是配置服务：provider_config / system_setting / provider_credential
// 三张表的读写侧（对应 Python lib/config/service.py）。写入侧目前只有设置项的通用写入口
// SetSetting，provider 配置的写入面随 providers router 波次落地。
type ConfigService struct {
	db *gorm.DB
}

// NewConfigService 构造。
func NewConfigService(db *gorm.DB) *ConfigService { return &ConfigService{db: db} }

// 解析链各媒体的兜底 backend（Python _DEFAULT_*_BACKEND 同值）。
const (
	DefaultVideoBackend = "gemini-aistudio/veo-3.1-lite-generate-preview"
	DefaultImageBackend = "gemini-aistudio/gemini-3.1-flash-image-preview"
	DefaultTextBackend  = "gemini-aistudio/gemini-3-flash-preview"
	DefaultAudioBackend = "dashscope/qwen3-tts-flash"

	// DefaultNarrationVoice 旁白默认音色（DashScope 预设）。
	DefaultNarrationVoice = "Cherry"

	// providerEnabledKeyPrefix 预置供应商启用开关在 system_setting 的 key 前缀
	// （值 "1"/"0"，缺省视为启用）。
	providerEnabledKeyPrefix = "provider_enabled:"
)

// 参考上传副本上限的保守通用默认（与 Python 侧常量一致）。
const (
	DefaultReferenceTotalMaxBytes  = 8 * 1024 * 1024
	DefaultReferenceSingleMaxBytes = 4 * 1024 * 1024
)

// ProviderConfigValueError provider 配置值校验失败（i18n code + 渲染参数）。
type ProviderConfigValueError struct {
	Provider string
	Key      string
	Value    string
	Code     string
}

func (e *ProviderConfigValueError) Error() string {
	return fmt.Sprintf("invalid value for %s.%s: %q (%s)", e.Provider, e.Key, e.Value, e.Code)
}

// Params 渲染参数（与 Python 侧 params dict 同构）。
func (e *ProviderConfigValueError) Params() map[string]string {
	return map[string]string{"field": e.Key, "value": e.Value}
}

// ErrUnknownProvider 未知内置 provider（fail loud，不回落）。
var ErrUnknownProvider = errors.New("unknown provider")

// ParseBackend 解析 "provider/model" 复合值；无 "/" 时按 fallback 拆分补齐
// （Python ConfigService._parse_backend 同语义）。
func ParseBackend(raw, fallback string) (string, string) {
	if strings.Contains(raw, "/") {
		providerID, modelID, _ := strings.Cut(raw, "/")
		return providerID, modelID
	}
	providerID, modelID, _ := strings.Cut(fallback, "/")
	return providerID, modelID
}

// GetSetting 读单条 system_setting；缺省返回 fallback。
func (s *ConfigService) GetSetting(ctx context.Context, key, fallback string) (string, error) {
	var row repository.SystemSetting
	err := s.db.WithContext(ctx).Where("key = ?", key).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fallback, nil
		}
		return "", err
	}
	return row.Value, nil
}

// GetAllSettings 读全部 system_setting。
func (s *ConfigService) GetAllSettings(ctx context.Context) (map[string]string, error) {
	var rows []repository.SystemSetting
	if err := s.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.Key] = row.Value
	}
	return out, nil
}

// ProviderConfig 读单供应商配置（含活跃凭证 overlay，与 Python _resolve_provider_config
// 同源：凭证字段非空覆盖 config 同名键）。
func (s *ConfigService) ProviderConfig(ctx context.Context, providerID string) (map[string]string, error) {
	if !IsCustomProvider(providerID) {
		if _, ok := provider.ProviderMetaFor(providerID); !ok {
			return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, providerID)
		}
	}
	var rows []repository.ProviderConfig
	if err := s.db.WithContext(ctx).Where("provider = ?", providerID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.Key] = row.Value
	}
	if cred := s.activeCredential(ctx, providerID); cred != nil {
		overlayCredential(cred, out)
	}
	return out, nil
}

// AllProviderConfigs 批量读全部供应商配置（含活跃凭证 overlay）。
func (s *ConfigService) AllProviderConfigs(ctx context.Context) (map[string]map[string]string, error) {
	var rows []repository.ProviderConfig
	if err := s.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	for _, row := range rows {
		if out[row.Provider] == nil {
			out[row.Provider] = map[string]string{}
		}
		out[row.Provider][row.Key] = row.Value
	}
	for providerID, cred := range s.activeCredentialsBulk(ctx) {
		if out[providerID] == nil {
			out[providerID] = map[string]string{}
		}
		overlayCredential(&cred, out[providerID])
	}
	return out, nil
}

// ProviderEnabled 读预置供应商启用开关；未设置视为启用。未知 provider 同样报错——
// 调用方（解析链）依赖它拦截「引用已下线供应商」的配置。
func (s *ConfigService) ProviderEnabled(ctx context.Context, providerID string) (bool, error) {
	if _, ok := provider.ProviderMetaFor(providerID); !ok {
		return false, fmt.Errorf("%w: %s", ErrUnknownProvider, providerID)
	}
	raw, err := s.GetSetting(ctx, providerEnabledKeyPrefix+providerID, "1")
	if err != nil {
		return false, err
	}
	return raw != "0", nil
}

// ProviderReady 供应商是否已配置就绪（有活跃凭证即 ready，Python 同口径）。
func (s *ConfigService) ProviderReady(ctx context.Context, providerID string) bool {
	return s.activeCredential(ctx, providerID) != nil
}

// NarrationVoice 解析全局旁白音色（空白视为未配置回落默认）。
func (s *ConfigService) NarrationVoice(ctx context.Context) (string, error) {
	raw, err := s.GetSetting(ctx, "narration_voice", "")
	if err != nil {
		return "", err
	}
	voice := strings.TrimSpace(raw)
	if voice == "" {
		return DefaultNarrationVoice, nil
	}
	return voice, nil
}

// NarrationSpeed 解析全局旁白语速；未设置 / 损坏值返回 nil。
func (s *ConfigService) NarrationSpeed(ctx context.Context) (*float64, error) {
	raw, err := s.GetSetting(ctx, "narration_speed", "")
	if err != nil {
		return nil, err
	}
	return ParseNarrationSpeed(raw), nil
}

// ParseNarrationSpeed 把存储态语速字符串解析为有效正有限数；非法一律 nil。
func ParseNarrationSpeed(raw string) *float64 {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil
	}
	speed, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(speed, 0) || math.IsNaN(speed) || speed <= 0 {
		return nil
	}
	return &speed
}

// activeCredential 读单供应商活跃凭证。
func (s *ConfigService) activeCredential(ctx context.Context, providerID string) *repository.ProviderCredential {
	var row repository.ProviderCredential
	err := s.db.WithContext(ctx).
		Where("provider = ? AND is_active = ?", providerID, true).
		First(&row).Error
	if err != nil {
		return nil
	}
	return &row
}

// activeCredentialsBulk 批量读全部活跃凭证。
func (s *ConfigService) activeCredentialsBulk(ctx context.Context) map[string]repository.ProviderCredential {
	var rows []repository.ProviderCredential
	if err := s.db.WithContext(ctx).Where("is_active = ?", true).Find(&rows).Error; err != nil {
		return nil
	}
	out := make(map[string]repository.ProviderCredential, len(rows))
	for _, row := range rows {
		out[row.Provider] = row
	}
	return out
}

// overlayCredential 把凭证字段合并进 config（列名即 config key，非空才覆盖）。
func overlayCredential(cred *repository.ProviderCredential, config map[string]string) {
	if cred.APIKey != nil && *cred.APIKey != "" {
		config["api_key"] = *cred.APIKey
	}
	if cred.CredentialsPath != nil && *cred.CredentialsPath != "" {
		config["credentials_path"] = *cred.CredentialsPath
	}
	if cred.BaseURL != nil && *cred.BaseURL != "" {
		config["base_url"] = *cred.BaseURL
	}
	if cred.AccessKey != nil && *cred.AccessKey != "" {
		config["access_key"] = *cred.AccessKey
	}
	if cred.SecretKey != nil && *cred.SecretKey != "" {
		config["secret_key"] = *cred.SecretKey
	}
}

// SetSetting 写入（或覆盖）一条 system_setting。
//
// 先 UPDATE 再按需 INSERT：该表没有 key 上的唯一索引，用 ORM 的 Save 会依赖主键；
// 只有 (key) 这一条业务身份，按 key 匹配才不会写出重复行。
func (s *ConfigService) SetSetting(ctx context.Context, key, value string) error {
	now := db.NewFlexibleTime(time.Now())
	res := s.db.WithContext(ctx).Model(&repository.SystemSetting{}).
		Where("key = ?", key).
		Updates(map[string]any{"value": value, "updated_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	return s.db.WithContext(ctx).Create(&repository.SystemSetting{
		Key: key, Value: value, UpdatedAt: now,
	}).Error
}
