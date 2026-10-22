// 供应商管理面（对应 server/routers/providers.py 的 12 个端点，收敛版）。
//
// 收敛口径（本轮明确的范围）：管理面只做「通用 CRUD + 状态聚合」，不按厂商写专属分支。
// 真要用到的供应商是 openai / claude（文本）、seedance（ark 视频）、seedream（ark 图片）、
// openai video / openai image——这些在注册表里本来就有元数据，所以本层直接读注册表，
// 不为任何一家写 if-else；连接测试也只对「能一条 HTTPS 请求验完」的供应商提供。
//
// 三条不变式：
//   - 状态是**派生**的（有活跃凭证即 ready），不落库，避免出现「库里 ready、实际没 key」；
//   - 密钥只进不出：GET 只回 masked 值，明文只在写入请求的方向上出现；
//   - 启用开关落 system_setting（`provider_enabled:<id>`），解析链与 UI 读同一处。
package admin

import (
	"github.com/shotwise/shotwise/internal/service"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

// ProviderAdminService 是供应商配置的读写服务。
type ProviderAdminService struct {
	db     *gorm.DB
	config *config.ConfigService
}

// NewProviderAdminService 构造。
func NewProviderAdminService(gdb *gorm.DB) *ProviderAdminService {
	return &ProviderAdminService{db: gdb, config: config.NewConfigService(gdb)}
}

// ProviderStatusView 是一个供应商在管理面上的投影。
type ProviderStatusView struct {
	ID             string         `json:"id"`
	DisplayName    string         `json:"display_name"`
	Description    string         `json:"description"`
	Status         string         `json:"status"` // ready / unconfigured
	MediaTypes     []string       `json:"media_types"`
	Capabilities   []string       `json:"capabilities"`
	RequiredKeys   []string       `json:"required_keys"`
	ConfiguredKeys []string       `json:"configured_keys"`
	MissingKeys    []string       `json:"missing_keys"`
	Models         map[string]any `json:"models"`
	ConsoleURL     string         `json:"console_url,omitempty"`
	Enabled        bool           `json:"enabled"`
}

// List 返回全部内置供应商的状态。
func (s *ProviderAdminService) List(ctx context.Context) ([]ProviderStatusView, error) {
	enabledMap, err := s.enabledMap(ctx)
	if err != nil {
		return nil, err
	}
	configured, err := s.configuredKeysBulk(ctx)
	if err != nil {
		return nil, err
	}
	active, err := s.activeCredentialProviders(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]ProviderStatusView, 0, len(provider.ProviderIDs()))
	for _, providerID := range provider.ProviderIDs() {
		meta, ok := provider.ProviderMetaFor(providerID)
		if !ok {
			continue
		}
		keys := configured[providerID]
		status := "unconfigured"
		missing := append([]string(nil), meta.RequiredKeys...)
		if active[providerID] {
			status = "ready"
			missing = []string{}
		}
		out = append(out, ProviderStatusView{
			ID:             providerID,
			DisplayName:    firstNonEmpty(meta.DisplayName, providerID),
			Description:    meta.Description,
			Status:         status,
			MediaTypes:     mediaTypesOf(meta),
			Capabilities:   capabilitiesOf(meta),
			RequiredKeys:   append([]string(nil), meta.RequiredKeys...),
			ConfiguredKeys: keys,
			MissingKeys:    missing,
			Models:         modelsView(providerID, meta),
			ConsoleURL:     meta.ConsoleURL,
			Enabled:        enabledMap[providerID],
		})
	}
	return out, nil
}

// SetEnabled 设置启用开关。
func (s *ProviderAdminService) SetEnabled(ctx context.Context, providerID string, enabled bool) error {
	if _, ok := provider.ProviderMetaFor(providerID); !ok {
		return ErrUnknownProviderID
	}
	value := "0"
	if enabled {
		value = "1"
	}
	return s.config.SetSetting(ctx, providerEnabledSettingKey(providerID), value)
}

// ErrUnknownProviderID 表示 provider 未在注册表登记。
var ErrUnknownProviderID = fmt.Errorf("unknown provider")

// providerEnabledSettingKey 与 config 包读侧同一前缀（单一真相源在 config）。
func providerEnabledSettingKey(providerID string) string { return "provider_enabled:" + providerID }

// FieldView 是一个配置字段的展示形态（secret 只给 masked）。
type FieldView struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	IsSet       bool   `json:"is_set"`
	Value       string `json:"value,omitempty"`
	ValueMasked any    `json:"value_masked,omitempty"`
}

// ConfigView 是 GET /providers/:id/config 的响应体。
type ConfigView struct {
	Provider    string         `json:"provider"`
	DisplayName string         `json:"display_name"`
	Description string         `json:"description"`
	Enabled     bool           `json:"enabled"`
	Status      string         `json:"status"`
	Fields      []FieldView    `json:"fields"`
	Models      map[string]any `json:"models"`
	ConsoleURL  string         `json:"console_url,omitempty"`
}

// GetConfig 返回字段化配置（密钥只回掩码）。
func (s *ProviderAdminService) GetConfig(ctx context.Context, providerID string) (ConfigView, error) {
	meta, ok := provider.ProviderMetaFor(providerID)
	if !ok {
		return ConfigView{}, ErrUnknownProviderID
	}
	values, err := s.configValues(ctx, providerID)
	if err != nil {
		return ConfigView{}, err
	}
	secret := map[string]bool{}
	for _, key := range meta.SecretKeys {
		secret[key] = true
	}
	keys := append(append([]string{}, meta.RequiredKeys...), meta.OptionalKeys...)
	fields := make([]FieldView, 0, len(keys))
	for _, key := range keys {
		value, present := values[key]
		required := service.ContainsString(meta.RequiredKeys, key)
		field := FieldView{
			Key:      key,
			Label:    key,
			Type:     "text",
			Required: required,
			IsSet:    present && strings.TrimSpace(value) != "",
		}
		if secret[key] {
			field.Type = "secret"
			if field.IsSet {
				field.ValueMasked = maskSecret(value)
			} else {
				field.ValueMasked = nil
			}
		} else if field.IsSet {
			field.Value = value
		}
		fields = append(fields, field)
	}
	enabled, err := s.config.ProviderEnabled(ctx, providerID)
	if err != nil {
		return ConfigView{}, err
	}
	status := "unconfigured"
	if s.config.ProviderReady(ctx, providerID) {
		status = "ready"
	}
	return ConfigView{
		Provider:    providerID,
		DisplayName: firstNonEmpty(meta.DisplayName, providerID),
		Description: meta.Description,
		Enabled:     enabled,
		Status:      status,
		Fields:      fields,
		Models:      modelsView(providerID, meta),
		ConsoleURL:  meta.ConsoleURL,
	}, nil
}

// PatchConfig 写入非密钥字段（密钥走凭证端点，避免明文进 provider_config 的可见面）。
func (s *ProviderAdminService) PatchConfig(
	ctx context.Context, providerID string, updates map[string]string,
) error {
	meta, ok := provider.ProviderMetaFor(providerID)
	if !ok {
		return ErrUnknownProviderID
	}
	allowed := map[string]bool{}
	for _, key := range meta.OptionalKeys {
		allowed[key] = true
	}
	secret := map[string]bool{}
	for _, key := range meta.SecretKeys {
		secret[key] = true
	}
	now := db.NewFlexibleTime(time.Now())
	for key, value := range updates {
		if !allowed[key] {
			return &service.ValueError{Msg: fmt.Sprintf("unsupported config key: %s", key)}
		}
		if secret[key] {
			// 密钥写这里会绕过凭证表，让「是否有活跃凭证」的判定失真。
			return &service.ValueError{Msg: fmt.Sprintf("secret key must be set through credentials: %s", key)}
		}
		if err := s.upsertProviderConfig(ctx, providerID, key, value, false, now); err != nil {
			return err
		}
	}
	return nil
}

// ModelTypesView 是 PATCH /providers/:id/model-types 的响应。
type ModelTypesView struct {
	Provider   string            `json:"provider"`
	ModelTypes map[string]string `json:"model_types"`
}

// PatchModelTypes 覆盖模型 → 媒体类型的推断结果。
//
// 只允许登记本供应商注册表里确实存在的模型：模型清单是注册表真相源，落库的只是「类型推断」，
// 不是「新增模型」。未登记的模型名一律拒绝，避免拼错的名字在报表里凭空多出一个模型。
func (s *ProviderAdminService) PatchModelTypes(
	ctx context.Context, providerID string, overrides map[string]string,
) (ModelTypesView, error) {
	meta, ok := provider.ProviderMetaFor(providerID)
	if !ok {
		return ModelTypesView{}, ErrUnknownProviderID
	}
	known := map[string]string{}
	for _, entry := range meta.Models {
		known[entry.ID] = string(entry.Info.MediaType)
	}
	merged := map[string]string{}
	for modelID, mediaType := range known {
		merged[modelID] = mediaType
	}
	for modelID, mediaType := range overrides {
		if _, ok := known[modelID]; !ok {
			return ModelTypesView{}, &service.ValueError{Msg: "unknown model: " + modelID}
		}
		switch mediaType {
		case "image", "video", "audio", "text":
		default:
			return ModelTypesView{}, &service.ValueError{Msg: "invalid media type: " + mediaType}
		}
		merged[modelID] = mediaType
	}
	payload, err := json.Marshal(overrides)
	if err != nil {
		return ModelTypesView{}, err
	}
	now := db.NewFlexibleTime(time.Now())
	if err := s.upsertProviderConfig(ctx, providerID, "model_types", string(payload), false, now); err != nil {
		return ModelTypesView{}, err
	}
	return ModelTypesView{Provider: providerID, ModelTypes: merged}, nil
}

// CredentialView 是凭证的对外投影（密钥只回掩码）。
type CredentialView struct {
	ID         int64  `json:"id"`
	Provider   string `json:"provider"`
	Name       string `json:"name"`
	IsActive   bool   `json:"is_active"`
	BaseURL    string `json:"base_url,omitempty"`
	APIKeyMask string `json:"api_key_masked,omitempty"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// CredentialInput 是创建/更新凭证的入参（空指针表示不修改）。
type CredentialInput struct {
	Name    *string
	APIKey  *string
	BaseURL *string
}

// ListCredentials 列出该供应商的全部凭证。
func (s *ProviderAdminService) ListCredentials(ctx context.Context, providerID string) ([]CredentialView, error) {
	if _, ok := provider.ProviderMetaFor(providerID); !ok {
		return nil, ErrUnknownProviderID
	}
	var rows []repository.ProviderCredential
	if err := s.db.WithContext(ctx).Where("provider = ?", providerID).
		Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]CredentialView, 0, len(rows))
	for _, row := range rows {
		out = append(out, credentialView(row))
	}
	return out, nil
}

// CreateCredential 新增一条凭证；该供应商此前没有活跃凭证时自动置为活跃。
func (s *ProviderAdminService) CreateCredential(
	ctx context.Context, providerID string, in CredentialInput,
) (CredentialView, error) {
	meta, ok := provider.ProviderMetaFor(providerID)
	if !ok {
		return CredentialView{}, ErrUnknownProviderID
	}
	name := ""
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
	}
	if name == "" {
		name = providerID
	}
	if in.APIKey == nil || strings.TrimSpace(*in.APIKey) == "" {
		// 注册表声明了 required ∩ secret 的供应商，缺密钥就是不可用凭证。
		if len(meta.RequiredKeys) > 0 {
			return CredentialView{}, &service.ValueError{Msg: "api_key is required"}
		}
	}
	now := db.NewFlexibleTime(time.Now())
	row := repository.ProviderCredential{
		Provider:  providerID,
		Name:      name,
		IsActive:  false,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if in.APIKey != nil {
		value := strings.TrimSpace(*in.APIKey)
		row.APIKey = &value
	}
	if in.BaseURL != nil {
		value := strings.TrimSpace(*in.BaseURL)
		if value != "" {
			row.BaseURL = &value
		}
	}
	// 第一条凭证自动激活：否则「配好了却 still unconfigured」，用户得再点一次激活。
	var count int64
	if err := s.db.WithContext(ctx).Model(&repository.ProviderCredential{}).
		Where("provider = ?", providerID).Count(&count).Error; err != nil {
		return CredentialView{}, err
	}
	row.IsActive = count == 0
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return CredentialView{}, err
	}
	return credentialView(row), nil
}

// UpdateCredential 更新凭证；空字段不覆盖。
func (s *ProviderAdminService) UpdateCredential(
	ctx context.Context, providerID string, credID int64, in CredentialInput,
) error {
	row, err := s.credentialOrNotFound(ctx, providerID, credID)
	if err != nil {
		return err
	}
	updates := map[string]any{"updated_at": db.NewFlexibleTime(time.Now())}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		updates["name"] = strings.TrimSpace(*in.Name)
	}
	if in.APIKey != nil {
		value := strings.TrimSpace(*in.APIKey)
		if value == "" {
			updates["api_key"] = nil
		} else {
			updates["api_key"] = value
		}
	}
	if in.BaseURL != nil {
		value := strings.TrimSpace(*in.BaseURL)
		if value == "" {
			updates["base_url"] = nil
		} else {
			updates["base_url"] = value
		}
	}
	return s.db.WithContext(ctx).Model(&repository.ProviderCredential{}).
		Where("id = ?", row.ID).Updates(updates).Error
}

// DeleteCredential 删除凭证；删掉的是活跃凭证时，把最新一条提升为活跃。
func (s *ProviderAdminService) DeleteCredential(ctx context.Context, providerID string, credID int64) error {
	row, err := s.credentialOrNotFound(ctx, providerID, credID)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id = ?", row.ID).Delete(&repository.ProviderCredential{}).Error; err != nil {
			return err
		}
		if !row.IsActive {
			return nil
		}
		var next repository.ProviderCredential
		if err := tx.Where("provider = ?", providerID).Order("id DESC").First(&next).Error; err != nil {
			// 没有剩余凭证：该供应商回到未配置状态，无需再动。
			return nil
		}
		return tx.Model(&repository.ProviderCredential{}).Where("id = ?", next.ID).
			Updates(map[string]any{
				"is_active":  true,
				"updated_at": db.NewFlexibleTime(time.Now()),
			}).Error
	})
}

// ActivateCredential 把指定凭证设为该供应商的唯一活跃凭证。
//
// 先清后置：部分唯一索引（provider WHERE is_active）不允许两条同时活跃，
// 顺序反了会撞约束。
func (s *ProviderAdminService) ActivateCredential(ctx context.Context, providerID string, credID int64) error {
	row, err := s.credentialOrNotFound(ctx, providerID, credID)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := db.NewFlexibleTime(time.Now())
		if err := tx.Model(&repository.ProviderCredential{}).
			Where("provider = ? AND is_active = ?", providerID, true).
			Updates(map[string]any{"is_active": false, "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Model(&repository.ProviderCredential{}).Where("id = ?", row.ID).
			Updates(map[string]any{"is_active": true, "updated_at": now}).Error
	})
}

// TestResult 是连接测试结果。
type TestResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// TestConnection 做一次最小连通性验证。
//
// 只对「一次 GET /models 或等价轻量请求就能验完」的供应商实现；其余返回 unsupported，
// 而不是假装成功——管理面上「测试通过」必须真意味着能连上。
func (s *ProviderAdminService) TestConnection(ctx context.Context, providerID string) (TestResult, error) {
	meta, ok := provider.ProviderMetaFor(providerID)
	if !ok {
		return TestResult{}, ErrUnknownProviderID
	}
	values, err := s.config.ProviderConfig(ctx, providerID)
	if err != nil {
		return TestResult{}, err
	}
	apiKey := strings.TrimSpace(values["api_key"])
	if apiKey == "" {
		return TestResult{Success: false, Message: "api_key is not configured"}, nil
	}
	baseURL := strings.TrimSpace(values["base_url"])
	if baseURL == "" {
		baseURL = meta.DefaultBaseURL
	}
	switch providerID {
	case "openai":
		return testOpenAICompatible(ctx, firstNonEmpty(baseURL, "https://api.openai.com/v1"), apiKey)
	case "ark", "ark-agent-plan":
		return testOpenAICompatible(ctx, firstNonEmpty(baseURL, "https://ark.cn-beijing.volces.com/api/v3"), apiKey)
	default:
		return TestResult{
			Success: false,
			Message: "connection test is not implemented for this provider",
		}, nil
	}
}

// ---- 内部 ----

// enabledMap 读全部供应商的启用状态（未设置视为启用）。
func (s *ProviderAdminService) enabledMap(ctx context.Context) (map[string]bool, error) {
	settings, err := s.config.GetAllSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, providerID := range provider.ProviderIDs() {
		raw, present := settings[providerEnabledSettingKey(providerID)]
		out[providerID] = !present || raw != "0"
	}
	return out, nil
}

// configuredKeysBulk 批量读「已配置的键名」（不含值）。
func (s *ProviderAdminService) configuredKeysBulk(ctx context.Context) (map[string][]string, error) {
	var rows []repository.ProviderConfig
	if err := s.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, row := range rows {
		if row.Key == "model_types" {
			continue
		}
		if strings.TrimSpace(row.Value) == "" {
			continue
		}
		out[row.Provider] = append(out[row.Provider], row.Key)
	}
	for providerID := range out {
		sort.Strings(out[providerID])
	}
	return out, nil
}

// activeCredentialProviders 返回「有活跃凭证」的供应商集合。
func (s *ProviderAdminService) activeCredentialProviders(ctx context.Context) (map[string]bool, error) {
	var rows []repository.ProviderCredential
	if err := s.db.WithContext(ctx).Where("is_active = ?", true).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, row := range rows {
		out[row.Provider] = true
	}
	return out, nil
}

// configValues 读该供应商的配置键值（含活跃凭证的覆盖）。
func (s *ProviderAdminService) configValues(ctx context.Context, providerID string) (map[string]string, error) {
	return s.config.ProviderConfig(ctx, providerID)
}

// upsertProviderConfig 写一条 provider_config（provider+key 唯一）。
func (s *ProviderAdminService) upsertProviderConfig(
	ctx context.Context, providerID, key, value string, isSecret bool, now db.FlexibleTime,
) error {
	res := s.db.WithContext(ctx).Model(&repository.ProviderConfig{}).
		Where("provider = ? AND key = ?", providerID, key).
		Updates(map[string]any{"value": value, "is_secret": isSecret, "updated_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	return s.db.WithContext(ctx).Create(&repository.ProviderConfig{
		Provider: providerID, Key: key, Value: value, IsSecret: isSecret, UpdatedAt: now,
	}).Error
}

// credentialOrNotFound 取凭证并校验归属。
func (s *ProviderAdminService) credentialOrNotFound(
	ctx context.Context, providerID string, credID int64,
) (repository.ProviderCredential, error) {
	if _, ok := provider.ProviderMetaFor(providerID); !ok {
		return repository.ProviderCredential{}, ErrUnknownProviderID
	}
	var row repository.ProviderCredential
	err := s.db.WithContext(ctx).
		Where("id = ? AND provider = ?", credID, providerID).First(&row).Error
	if err != nil {
		return repository.ProviderCredential{}, ErrCredentialNotFound
	}
	return row, nil
}

// ErrCredentialNotFound 表示凭证不存在（或不属于该供应商）。
var ErrCredentialNotFound = fmt.Errorf("credential not found")

// credentialView 把表行投影为对外结构（密钥只回掩码）。
func credentialView(row repository.ProviderCredential) CredentialView {
	view := CredentialView{
		ID:        row.ID,
		Provider:  row.Provider,
		Name:      row.Name,
		IsActive:  row.IsActive,
		CreatedAt: isoFromFlexible(row.CreatedAt),
		UpdatedAt: isoFromFlexible(row.UpdatedAt),
	}
	if row.BaseURL != nil {
		view.BaseURL = *row.BaseURL
	}
	if row.APIKey != nil && *row.APIKey != "" {
		view.APIKeyMask = maskSecret(*row.APIKey)
	}
	return view
}

// maskSecret 只保留尾部 4 位；过短的密钥整体遮蔽（不留可还原线索）。
func maskSecret(value string) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= 4 {
		return "••••"
	}
	return "••••" + trimmed[len(trimmed)-4:]
}

// mediaTypesOf 汇总该供应商声明了哪些媒体类型。
func mediaTypesOf(meta provider.ProviderMeta) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	for _, entry := range meta.Models {
		media := string(entry.Info.MediaType)
		if seen[media] {
			continue
		}
		seen[media] = true
		out = append(out, media)
	}
	sort.Strings(out)
	return out
}

// capabilitiesOf 汇总该供应商声明了哪些能力 token。
func capabilitiesOf(meta provider.ProviderMeta) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, entry := range meta.Models {
		for _, capability := range entry.Info.Capabilities {
			token := string(capability)
			if seen[token] {
				continue
			}
			seen[token] = true
			out = append(out, token)
		}
	}
	sort.Strings(out)
	return out
}

// modelsView 把注册表模型序列化成 {model_id: {...}}。
//
// 字段与 Python ModelInfoResponse 契约对齐：集合字段恒出现（未声明渲染为空集合/空对象），
// 前端不做缺字段兜底（ProviderDetail 直接读 supported_durations.length）。音轨两位与
// 声音一致性由 registry 的派生函数产出，不在本层复制公式。目录场景无项目上下文，
// generation_mode 未知 → native 恒不出现（对应 Python derive_voice_consistency 以
// generation_mode=None 调用），声音一致性退化为 has_audio_track 的两值派生。
func modelsView(providerID string, meta provider.ProviderMeta) map[string]any {
	out := make(map[string]any, len(meta.Models))
	for _, entry := range meta.Models {
		info := entry.Info
		hasAudioTrack := provider.ModelHasAudioTrack(providerID, info)
		item := map[string]any{
			"id":           entry.ID,
			"display_name": firstNonEmpty(info.DisplayName, entry.ID),
			"media_type":   string(info.MediaType),
			"default":      info.Default,
		}
		capabilities := make([]string, 0, len(info.Capabilities))
		for _, capability := range info.Capabilities {
			capabilities = append(capabilities, string(capability))
		}
		item["capabilities"] = capabilities
		durations := info.SupportedDurations
		if durations == nil {
			durations = []int{}
		}
		item["supported_durations"] = durations
		constraints := info.DurationResolutionConstraints
		if constraints == nil {
			constraints = map[string][]int{}
		}
		item["duration_resolution_constraints"] = constraints
		refDurations := info.ReferenceImageDurations
		if refDurations == nil {
			refDurations = []int{}
		}
		item["reference_image_durations"] = refDurations
		resolutions := info.Resolutions
		if resolutions == nil {
			resolutions = []string{}
		}
		item["resolutions"] = resolutions
		if info.AudioAlwaysOn {
			item["audio_always_on"] = true
		}
		item["has_audio_track"] = hasAudioTrack
		item["audio_switch_controllable"] = provider.ModelAudioSwitchControllable(info)
		voiceConsistency := "none"
		if hasAudioTrack {
			voiceConsistency = "soft"
		}
		item["voice_consistency"] = voiceConsistency
		out[entry.ID] = item
	}
	return out
}

// isoFromFlexible 渲染 isoformat；零值渲染空串。
func isoFromFlexible(t db.FlexibleTime) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.999999+00:00")
}

// firstNonEmpty 取第一个非空串。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// testOpenAICompatible 用 GET /models 验证 OpenAI 兼容端点：能列出模型即说明
// base_url 与 api_key 都可用，且这条请求不计费、不产生生成副作用。
func testOpenAICompatible(ctx context.Context, baseURL, apiKey string) (TestResult, error) {
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return TestResult{Success: false, Message: err.Error()}, nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return TestResult{Success: false, Message: "connection failed"}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return TestResult{Success: true, Message: "connection ok"}, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return TestResult{Success: false, Message: "authentication failed"}, nil
	default:
		return TestResult{Success: false, Message: fmt.Sprintf("unexpected status: %d", resp.StatusCode)}, nil
	}
}

// envOrFallback 读环境变量，缺省回退（供连接测试的 base URL 覆盖）。
func envOrFallback(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
