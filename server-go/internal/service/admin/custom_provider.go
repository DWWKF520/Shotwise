// 自定义供应商的读写服务（对应 server/routers/custom_providers.py 的 CRUD 面）。
//
// 自定义供应商与内置供应商是两套东西：内置走 provider 注册表 + provider_config /
// provider_credential 三张表，自定义走 custom_provider / custom_provider_model 两张表，
// base_url 与 api_key 直接存在供应商行上。因此本层不复用 ProviderAdminService 的凭证与
// 连接测试路径，只共用它的掩码与行投影工具。
//
// 五条不变式：
//   - endpoint 键的合法性由 providers 注册表判定，不在本层硬编码白名单；
//   - 能力覆盖在写入侧拒、在回显侧再过滤一次，回显永远是执行层真会采用的那一份；
//   - 模型列表是整体替换语义（PUT /models 与 PUT /{id}），不是逐行增量；
//   - 删除供应商要级联清掉指向它的全局默认配置与项目级配置，避免留下永远解析不出模型的悬空引用；
//   - 悬空引用在删除时不拦截，只作为读取时的提示回显（见 default_model_conflict 之外的
//     global_bucket_refs），真正的拦截闸在解析期。
package admin

import (
	"github.com/shotwise/shotwise/internal/service"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/providers"
	"github.com/shotwise/shotwise/internal/repository"
)

// customCascadeSettingKeys 是删除供应商时需要清空的全局默认后端设置键。
//
// 刻意不含 default_video_backend_i2v / _r2v 两个视频桶键：桶引用的悬空成因（模型被删、
// endpoint 换媒体类型）统一由解析期的能力闸报错兜底，写入侧反向清理是另一种静默改配置。
// 这两个键仍会在读取时作为 global_bucket_refs 提示回显。
var customCascadeSettingKeys = []string{
	"default_video_backend", "default_image_backend",
	"default_image_backend_t2i", "default_image_backend_i2i",
	"default_text_backend", "default_audio_backend",
	"text_backend_simple", "text_backend_complex",
}

// customDiscoveryFormats 是可落库的发现协议。anthropic 只能用于无状态的探测入口，
// 不能作为供应商的持久 discovery_format（它没有独立的供应商配置面）。
var customDiscoveryFormats = map[string]bool{"openai": true, "google": true}

// CustomCapacityRefresher 是写后需要刷新并发上限表的窄接口（由队列 Worker 满足）。
type CustomCapacityRefresher interface {
	ReloadLimits(ctx context.Context)
}

// CustomProviderService 是自定义供应商的配置读写服务。
type CustomProviderService struct {
	db     *gorm.DB
	config *config.ConfigService
	store  *service.ProjectStore
	limits CustomCapacityRefresher
}

// NewCustomProviderService 构造。store 为 nil 时跳过项目级级联清理，limits 为 nil 时跳过
// 并发上限刷新——两者都只影响清理动作，不影响写入本身。
func NewCustomProviderService(
	gdb *gorm.DB, store *service.ProjectStore, limits CustomCapacityRefresher,
) *CustomProviderService {
	return &CustomProviderService{
		db:     gdb,
		config: config.NewConfigService(gdb),
		store:  store,
		limits: limits,
	}
}

// ---------------------------------------------------------------------------
// 出入参
// ---------------------------------------------------------------------------

// CustomModelInput 是单个自定义模型的写入形态。
type CustomModelInput struct {
	ModelID             string
	DisplayName         string
	Endpoint            string
	IsDefault           bool
	IsEnabled           bool
	PriceUnit           *string
	PriceInput          *float64
	PriceOutput         *float64
	Currency            *string
	SupportedDurations  []int
	Resolution          *string
	CapabilityOverrides map[string]any
	EndpointDeclaration map[string]any
}

// CustomProviderCreateInput 是新建供应商的入参。
type CustomProviderCreateInput struct {
	DisplayName                string
	DiscoveryFormat            string
	BaseURL                    string
	APIKey                     string
	Models                     []CustomModelInput
	ImageMaxWorkers            *int64
	VideoMaxWorkers            *int64
	AudioMaxWorkers            *int64
	ImageRequestTimeoutSeconds *float64
	IsEnabled                  *bool
}

// CustomProviderPatchInput 是局部更新的入参，nil 字段表示不修改。
//
// discovery_format 不在其中：发现协议决定模型发现与 URL 归一化的方式，属于供应商身份的一部分。
type CustomProviderPatchInput struct {
	DisplayName *string
	BaseURL     *string
	APIKey      *string
	IsEnabled   *bool
}

// CustomProviderReplaceInput 是全量更新的入参（PUT）。
//
// 与 PATCH 的差别是权威性：并发上限、超时、启用开关即使未提供也照写（nil = 清空回全局默认），
// 只有 api_key 的 nil 表示「不改」。这是前端整表提交的语义，不是逐字段合并。
type CustomProviderReplaceInput struct {
	DisplayName                string
	BaseURL                    string
	APIKey                     *string
	Models                     []CustomModelInput
	ImageMaxWorkers            *int64
	VideoMaxWorkers            *int64
	AudioMaxWorkers            *int64
	ImageRequestTimeoutSeconds *float64
	IsEnabled                  *bool
}

// CustomModelView 是自定义模型对外的投影。
type CustomModelView struct {
	ID                  int64                       `json:"id"`
	ModelID             string                      `json:"model_id"`
	DisplayName         string                      `json:"display_name"`
	Endpoint            string                      `json:"endpoint"`
	IsDefault           bool                        `json:"is_default"`
	IsEnabled           bool                        `json:"is_enabled"`
	PriceUnit           *string                     `json:"price_unit"`
	PriceInput          *float64                    `json:"price_input"`
	PriceOutput         *float64                    `json:"price_output"`
	Currency            *string                     `json:"currency"`
	SupportedDurations  []int                       `json:"supported_durations"`
	Resolution          *string                     `json:"resolution"`
	SystemCapabilities  *provider.VideoCapabilities `json:"system_capabilities"`
	CapabilityOverrides map[string]any              `json:"capability_overrides"`
	EndpointDeclaration map[string]any              `json:"endpoint_declaration"`
	GlobalBucketRefs    []string                    `json:"global_bucket_refs"`
}

// CustomProviderView 是自定义供应商对外的投影（api_key 只给掩码）。
type CustomProviderView struct {
	ID                         int64             `json:"id"`
	DisplayName                string            `json:"display_name"`
	DiscoveryFormat            string            `json:"discovery_format"`
	BaseURL                    string            `json:"base_url"`
	APIKeyMasked               string            `json:"api_key_masked"`
	Models                     []CustomModelView `json:"models"`
	CreatedAt                  *string           `json:"created_at"`
	ImageMaxWorkers            *int64            `json:"image_max_workers"`
	VideoMaxWorkers            *int64            `json:"video_max_workers"`
	AudioMaxWorkers            *int64            `json:"audio_max_workers"`
	ImageRequestTimeoutSeconds *float64          `json:"image_request_timeout_seconds"`
	IsEnabled                  bool              `json:"is_enabled"`
}

// CustomCredentialsView 是明文凭证，仅管理员可读。
type CustomCredentialsView struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// ---------------------------------------------------------------------------
// 读取
// ---------------------------------------------------------------------------

// ListProviders 返回全部自定义供应商及其模型。
//
// 全局设置只读一次供所有供应商复用：桶引用提示要逐个供应商比对同一个键值集合，
// 按供应商各读一遍是把 N 次查询放大成 N 次。
func (s *CustomProviderService) ListProviders(ctx context.Context) ([]CustomProviderView, error) {
	var rows []repository.CustomProvider
	if err := s.db.WithContext(ctx).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []CustomProviderView{}, nil
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	var models []repository.CustomProviderModel
	if err := s.db.WithContext(ctx).
		Where("provider_id IN ?", ids).Order("id ASC").Find(&models).Error; err != nil {
		return nil, err
	}
	settings, err := s.config.GetAllSettings(ctx)
	if err != nil {
		return nil, err
	}
	grouped := make(map[int64][]repository.CustomProviderModel, len(ids))
	for _, model := range models {
		grouped[model.ProviderID] = append(grouped[model.ProviderID], model)
	}
	out := make([]CustomProviderView, 0, len(rows))
	for _, row := range rows {
		refs := extractCustomBucketRefs(settings, row.ID)
		out = append(out, customProviderView(row, grouped[row.ID], refs))
	}
	return out, nil
}

// GetProvider 返回单个自定义供应商及其模型。
func (s *CustomProviderService) GetProvider(ctx context.Context, providerDBID int64) (CustomProviderView, error) {
	row, err := s.providerRow(ctx, providerDBID)
	if err != nil {
		return CustomProviderView{}, err
	}
	models, err := s.listModels(ctx, providerDBID)
	if err != nil {
		return CustomProviderView{}, err
	}
	settings, err := s.config.GetAllSettings(ctx)
	if err != nil {
		return CustomProviderView{}, err
	}
	return customProviderView(*row, models, extractCustomBucketRefs(settings, providerDBID)), nil
}

// Credentials 返回明文凭证。调用方须自行完成管理员校验。
func (s *CustomProviderService) Credentials(
	ctx context.Context, providerDBID int64,
) (CustomCredentialsView, error) {
	row, err := s.providerRow(ctx, providerDBID)
	if err != nil {
		return CustomCredentialsView{}, err
	}
	return CustomCredentialsView{BaseURL: row.BaseURL, APIKey: row.APIKey}, nil
}

// EndpointCatalog 返回全部 endpoint 描述，供设置页渲染 endpoint 选择器。
func (s *CustomProviderService) EndpointCatalog() []providers.CustomEndpointDescriptor {
	return providers.CustomEndpointCatalog()
}

// ---------------------------------------------------------------------------
// 写入
// ---------------------------------------------------------------------------

// CreateProvider 新建供应商并整体写入其模型列表。
func (s *CustomProviderService) CreateProvider(
	ctx context.Context, in CustomProviderCreateInput,
) (CustomProviderView, error) {
	if !customDiscoveryFormats[in.DiscoveryFormat] {
		return CustomProviderView{}, apperr.NewText(http422, "invalid_discovery_format",
			map[string]any{"discovery_format": in.DiscoveryFormat})
	}
	if err := validateCustomModels(in.Models); err != nil {
		return CustomProviderView{}, err
	}
	now := db.NewFlexibleTime(time.Now())
	row := repository.CustomProvider{
		DisplayName:                strings.TrimSpace(in.DisplayName),
		DiscoveryFormat:            in.DiscoveryFormat,
		BaseURL:                    strings.TrimSpace(in.BaseURL),
		APIKey:                     strings.TrimSpace(in.APIKey),
		ImageMaxWorkers:            in.ImageMaxWorkers,
		VideoMaxWorkers:            in.VideoMaxWorkers,
		AudioMaxWorkers:            in.AudioMaxWorkers,
		ImageRequestTimeoutSeconds: in.ImageRequestTimeoutSeconds,
		IsEnabled:                  customBoolOrTrue(in.IsEnabled),
		CreatedAt:                  now,
		UpdatedAt:                  now,
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return insertCustomModels(tx, row.ID, in.Models, now)
	})
	if err != nil {
		return CustomProviderView{}, err
	}
	s.reloadLimits(ctx)
	return s.GetProvider(ctx, row.ID)
}

// PatchProvider 局部更新供应商。未提供任何字段即拒绝，避免把一次空提交当成成功写入。
func (s *CustomProviderService) PatchProvider(
	ctx context.Context, providerDBID int64, in CustomProviderPatchInput,
) (CustomProviderView, error) {
	updates := map[string]any{"updated_at": db.NewFlexibleTime(time.Now())}
	if in.DisplayName != nil {
		updates["display_name"] = strings.TrimSpace(*in.DisplayName)
	}
	if in.BaseURL != nil {
		updates["base_url"] = strings.TrimSpace(*in.BaseURL)
	}
	if in.APIKey != nil {
		updates["api_key"] = strings.TrimSpace(*in.APIKey)
	}
	if in.IsEnabled != nil {
		updates["is_enabled"] = *in.IsEnabled
	}
	if len(updates) == 1 {
		return CustomProviderView{}, apperr.NewText(http400, "at_least_one_field_required", nil)
	}
	if _, err := s.providerRow(ctx, providerDBID); err != nil {
		return CustomProviderView{}, err
	}
	err := s.db.WithContext(ctx).Model(&repository.CustomProvider{}).
		Where("id = ?", providerDBID).Updates(updates).Error
	if err != nil {
		return CustomProviderView{}, err
	}
	s.reloadLimits(ctx)
	return s.GetProvider(ctx, providerDBID)
}

// ReplaceProvider 全量更新供应商并整体替换其模型列表，两件事在同一事务内完成。
func (s *CustomProviderService) ReplaceProvider(
	ctx context.Context, providerDBID int64, in CustomProviderReplaceInput,
) (CustomProviderView, error) {
	if err := validateCustomModels(in.Models); err != nil {
		return CustomProviderView{}, err
	}
	if _, err := s.providerRow(ctx, providerDBID); err != nil {
		return CustomProviderView{}, err
	}
	now := db.NewFlexibleTime(time.Now())
	oldModelIDs, err := s.modelIDSet(ctx, providerDBID)
	if err != nil {
		return CustomProviderView{}, err
	}
	newModelIDs := make(map[string]bool, len(in.Models))
	for _, model := range in.Models {
		if model.ModelID != "" {
			newModelIDs[model.ModelID] = true
		}
	}
	deleted := difference(oldModelIDs, newModelIDs)

	updates := map[string]any{
		"display_name":                  strings.TrimSpace(in.DisplayName),
		"base_url":                      strings.TrimSpace(in.BaseURL),
		"image_max_workers":             in.ImageMaxWorkers,
		"video_max_workers":             in.VideoMaxWorkers,
		"audio_max_workers":             in.AudioMaxWorkers,
		"image_request_timeout_seconds": in.ImageRequestTimeoutSeconds,
		"is_enabled":                    customBoolOrTrue(in.IsEnabled),
		"updated_at":                    now,
	}
	if in.APIKey != nil {
		updates["api_key"] = strings.TrimSpace(*in.APIKey)
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&repository.CustomProvider{}).
			Where("id = ?", providerDBID).Updates(updates).Error; err != nil {
			return err
		}
		if err := deleteCustomModels(tx, providerDBID); err != nil {
			return err
		}
		if err := insertCustomModels(tx, providerDBID, in.Models, now); err != nil {
			return err
		}
		return clearStaleSettings(tx, providerDBID, deleted)
	})
	if err != nil {
		return CustomProviderView{}, err
	}
	s.reloadLimits(ctx)
	return s.GetProvider(ctx, providerDBID)
}

// ReplaceModels 整体替换该供应商的模型列表，并清理指向被删模型的全局配置。
func (s *CustomProviderService) ReplaceModels(
	ctx context.Context, providerDBID int64, models []CustomModelInput,
) ([]CustomModelView, error) {
	if err := validateCustomModels(models); err != nil {
		return nil, err
	}
	if _, err := s.providerRow(ctx, providerDBID); err != nil {
		return nil, err
	}
	oldModelIDs, err := s.modelIDSet(ctx, providerDBID)
	if err != nil {
		return nil, err
	}
	newModelIDs := make(map[string]bool, len(models))
	for _, model := range models {
		if model.ModelID != "" {
			newModelIDs[model.ModelID] = true
		}
	}
	deleted := difference(oldModelIDs, newModelIDs)

	now := db.NewFlexibleTime(time.Now())
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := deleteCustomModels(tx, providerDBID); err != nil {
			return err
		}
		if err := insertCustomModels(tx, providerDBID, models, now); err != nil {
			return err
		}
		return clearStaleSettings(tx, providerDBID, deleted)
	})
	if err != nil {
		return nil, err
	}
	s.reloadLimits(ctx)

	rows, err := s.listModels(ctx, providerDBID)
	if err != nil {
		return nil, err
	}
	settings, err := s.config.GetAllSettings(ctx)
	if err != nil {
		return nil, err
	}
	refs := extractCustomBucketRefs(settings, providerDBID)
	out := make([]CustomModelView, 0, len(rows))
	for _, row := range rows {
		out = append(out, customModelView(row, refs[row.ModelID]))
	}
	return out, nil
}

// DeleteProvider 删除供应商：级联删模型、清全局默认配置、清项目级配置。
//
// 项目级清理在 DB 提交之后做且逐项目吞掉错误：磁盘 I/O 不该让一次已经提交的删除回滚，
// 单个项目损坏也不该阻断其余项目的清理。
func (s *CustomProviderService) DeleteProvider(ctx context.Context, providerDBID int64) error {
	if _, err := s.providerRow(ctx, providerDBID); err != nil {
		return err
	}
	prefix := config.MakeProviderID(providerDBID) + "/"
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 显式删模型而不依赖外键级联：SQLite 默认不开启 foreign_keys pragma。
		if err := deleteCustomModels(tx, providerDBID); err != nil {
			return err
		}
		if err := tx.Delete(&repository.CustomProvider{}, providerDBID).Error; err != nil {
			return err
		}
		for _, key := range customCascadeSettingKeys {
			value, err := customSettingValue(tx, key)
			if err != nil {
				return err
			}
			if strings.HasPrefix(value, prefix) {
				if err := customSetSetting(tx, key, ""); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.reloadLimits(ctx)
	s.cleanupProjectRefs(prefix)
	return nil
}

// ---------------------------------------------------------------------------
// 校验
// ---------------------------------------------------------------------------

// validateCustomModels 校验整批模型的写入形态。
func validateCustomModels(models []CustomModelInput) error {
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if model.IsEnabled && strings.TrimSpace(model.ModelID) == "" {
			return apperr.NewText(http422, "model_id_required", nil)
		}
		if model.IsEnabled && strings.TrimSpace(model.Endpoint) == "" {
			return apperr.NewText(http422, "endpoint_required", nil)
		}
		if model.PriceOutput != nil && model.PriceInput == nil {
			return apperr.NewText(http422, "price_input_required", nil)
		}
		if model.ModelID != "" {
			if seen[model.ModelID] {
				return apperr.NewText(http422, "duplicate_model_id",
					map[string]any{"model_id": model.ModelID})
			}
			seen[model.ModelID] = true
		}
		if err := validateCustomModelFields(model); err != nil {
			return err
		}
	}
	return validateCustomUniqueDefaults(models)
}

// validateCustomModelFields 校验单模型的 endpoint 合法性与该 endpoint 下的字段约束。
func validateCustomModelFields(model CustomModelInput) error {
	media, err := providers.CustomEndpointMediaType(model.Endpoint)
	if err != nil {
		return apperr.NewText(http422, "unknown_endpoint", map[string]any{"endpoint": model.Endpoint})
	}
	if media == provider.MediaVideo && len(model.SupportedDurations) == 0 {
		return apperr.NewLiteral(http422, "video model supported_durations must be explicitly declared")
	}
	if _, hasPoll := model.EndpointDeclaration["poll"]; hasPoll && media != provider.MediaImage {
		return apperr.NewLiteral(http422, "endpoint_declaration.poll is only supported on image endpoints")
	}
	if len(model.EndpointDeclaration) > 0 {
		declaration, err := providers.ParseCustomEndpointDeclaration(model.EndpointDeclaration)
		if err != nil {
			return apperr.NewLiteral(http422, err.Error())
		}
		if err := providers.ValidateCustomEndpointDeclaration(declaration); err != nil {
			return apperr.NewLiteral(http422, err.Error())
		}
	}
	// 覆盖先收窄再校验：白名单外的键只能来自手工改库，写入侧剔除是它唯一的出口。
	narrowed := providers.CustomNarrowOverridesToAllowlist(model.CapabilityOverrides)
	if verr := providers.CustomValidateOverridesForWrite(model.Endpoint, model.ModelID, narrowed); verr != nil {
		return apperr.NewText(http422, verr.Code, verr.Params)
	}
	return nil
}

// validateCustomUniqueDefaults 校验默认模型互斥。
//
// 非 image endpoint 是「同一媒体类型至多一个默认」；image endpoint 的默认是按能力槽位认领的
// （文生图与图生图可以各有各的默认），因此判的是能力集合两两不相交。图片 endpoint 若没声明
// 任何能力位，则不参与互斥判定——无从知道它该认领哪个槽位。
func validateCustomUniqueDefaults(models []CustomModelInput) error {
	var mediaOrder []string
	byMedia := make(map[string][]string)
	capToModelIDs := make(map[string][]string)

	for _, model := range models {
		if !model.IsDefault {
			continue
		}
		media, err := providers.CustomEndpointMediaType(model.Endpoint)
		if err != nil {
			continue // 未知 endpoint 已在上一步拒掉，这里只是不重复报错
		}
		if media != provider.MediaImage {
			if _, seen := byMedia[string(media)]; !seen {
				mediaOrder = append(mediaOrder, string(media))
			}
			byMedia[string(media)] = append(byMedia[string(media)], model.ModelID)
			continue
		}
		for _, capability := range providers.CustomEndpointImageCapabilities(model.Endpoint) {
			capToModelIDs[capability] = append(capToModelIDs[capability], model.ModelID)
		}
	}

	parts := make([]string, 0, len(mediaOrder)+1)
	for _, media := range mediaOrder {
		if ids := byMedia[media]; len(ids) > 1 {
			parts = append(parts, fmt.Sprintf("%s(%s)", media, strings.Join(ids, ", ")))
		}
	}
	// 图片冲突按能力槽位反向索引汇总（任一槽位被两个默认认领即冲突），恒排在非图片媒体类型之后。
	var conflictIDs []string
	for _, ids := range capToModelIDs {
		if len(ids) > 1 {
			conflictIDs = append(conflictIDs, ids...)
		}
	}
	if len(conflictIDs) > 0 {
		parts = append(parts, fmt.Sprintf("image(%s)", strings.Join(uniqueInOrder(conflictIDs), ", ")))
	}
	if len(parts) == 0 {
		return nil
	}
	return apperr.NewText(http422, "default_model_conflict",
		map[string]any{"conflict": strings.Join(parts, "; ")})
}

// ---------------------------------------------------------------------------
// 序列化
// ---------------------------------------------------------------------------

// customProviderView 把供应商行与其模型投影为对外结构。
func customProviderView(
	row repository.CustomProvider, models []repository.CustomProviderModel, refs map[string][]string,
) CustomProviderView {
	views := make([]CustomModelView, 0, len(models))
	for _, model := range models {
		views = append(views, customModelView(model, refs[model.ModelID]))
	}
	view := CustomProviderView{
		ID:                         row.ID,
		DisplayName:                row.DisplayName,
		DiscoveryFormat:            row.DiscoveryFormat,
		BaseURL:                    row.BaseURL,
		APIKeyMasked:               maskSecret(row.APIKey),
		Models:                     views,
		ImageMaxWorkers:            row.ImageMaxWorkers,
		VideoMaxWorkers:            row.VideoMaxWorkers,
		AudioMaxWorkers:            row.AudioMaxWorkers,
		ImageRequestTimeoutSeconds: row.ImageRequestTimeoutSeconds,
		IsEnabled:                  row.IsEnabled,
	}
	if !row.CreatedAt.IsZero() {
		created := isoFromFlexible(row.CreatedAt)
		view.CreatedAt = &created
	}
	return view
}

// customModelView 把模型行投影为对外结构。
//
// 三个派生字段都按「执行层真会采用什么」来算，而不是把 DB 里的原样吐出去：
// 系统能力非视频模型为 nil；能力覆盖经与写入侧同源的过滤；声明重解析后再序列化，
// 解析不了的存量脏数据降级为 nil 而不是让整个列表 500。
func customModelView(row repository.CustomProviderModel, bucketRefs []string) CustomModelView {
	view := CustomModelView{
		ID:               row.ID,
		ModelID:          row.ModelID,
		DisplayName:      row.DisplayName,
		Endpoint:         row.Endpoint,
		IsDefault:        row.IsDefault,
		IsEnabled:        row.IsEnabled,
		PriceUnit:        row.PriceUnit,
		PriceInput:       row.PriceInput,
		PriceOutput:      row.PriceOutput,
		Currency:         row.Currency,
		Resolution:       row.Resolution,
		GlobalBucketRefs: bucketRefs,
	}
	if row.SupportedDurations != nil {
		view.SupportedDurations = parseCustomDurations(*row.SupportedDurations)
	}
	view.SystemCapabilities = providers.CustomSystemCapabilitiesForResponse(row.Endpoint, row.ModelID)
	if row.CapabilityOverrides != nil {
		view.CapabilityOverrides = providers.CustomEffectiveOverridesForResponse(
			row.Endpoint, row.ModelID, *row.CapabilityOverrides)
	}
	if row.EndpointDeclaration != nil {
		var raw map[string]any
		if err := json.Unmarshal([]byte(*row.EndpointDeclaration), &raw); err == nil {
			if declaration, err := providers.ParseCustomEndpointDeclaration(raw); err == nil {
				view.EndpointDeclaration = providers.CustomEndpointDeclarationToMap(declaration)
			}
		}
	}
	return view
}

// extractCustomBucketRefs 从全局设置里挑出引用该供应商模型的键，按 model_id 分组。
//
// 引用形态是 "custom-<id>/<model_id>" 的字面前缀，不需要解析，也不会去核对那个模型是否还在：
// 悬空引用交给解析期报错，这里只负责把「你改的这个模型正被哪些全局桶指着」告诉设置页。
// 项目级配置不在扫描范围内——那要逐个开 project.json，代价与并发窗口都不划算。
func extractCustomBucketRefs(settings map[string]string, providerDBID int64) map[string][]string {
	prefix := config.MakeProviderID(providerDBID) + "/"
	refs := make(map[string][]string)
	for _, key := range backendKeys {
		value := settings[key]
		if value == "" || !strings.HasPrefix(value, prefix) {
			continue
		}
		modelID := strings.TrimPrefix(value, prefix)
		refs[modelID] = append(refs[modelID], key)
	}
	return refs
}

// ---------------------------------------------------------------------------
// 引用提取与级联清理
// ---------------------------------------------------------------------------

// clearStaleSettings 清空指向已删除模型的全局默认配置。
func clearStaleSettings(tx *gorm.DB, providerDBID int64, deletedModelIDs map[string]bool) error {
	if len(deletedModelIDs) == 0 {
		return nil
	}
	prefix := config.MakeProviderID(providerDBID) + "/"
	for _, key := range customCascadeSettingKeys {
		value, err := customSettingValue(tx, key)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(value, prefix) {
			continue
		}
		if deletedModelIDs[strings.TrimPrefix(value, prefix)] {
			if err := customSetSetting(tx, key, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// cleanupProjectRefs 清空所有项目 project.json 里指向该供应商的媒体后端字段。
//
// 字段值清空而非留悬空字符串：ProjectFile 的这些字段带 omitempty，清空即从落盘 JSON 中消失。
func (s *CustomProviderService) cleanupProjectRefs(prefix string) {
	if s.store == nil {
		return
	}
	names, err := s.store.List()
	if err != nil {
		return
	}
	for _, name := range names {
		if err := s.store.Update(name, func(pf *domain.ProjectFile) error {
			for _, field := range customProjectBackendFields(pf) {
				if strings.HasPrefix(*field, prefix) {
					*field = ""
				}
			}
			return nil
		}); err != nil {
			continue // 读取失败或项目不可写：跳过而非中断其余项目的清理
		}
	}
}

// customProjectBackendFields 返回 project.json 上会被级联清理的后端字段。
//
// 不含 video_provider_i2v / _r2v：与全局设置同理，桶引用刻意不做写入侧级联。
func customProjectBackendFields(pf *domain.ProjectFile) map[string]*string {
	return map[string]*string{
		"video_backend":         &pf.VideoBackend,
		"audio_backend":         &pf.AudioBackend,
		"image_provider_t2i":    &pf.ImageProviderT2I,
		"image_provider_i2i":    &pf.ImageProviderI2I,
		"default_image_backend": &pf.DefaultImageBackend,
		"text_backend_simple":   &pf.TextBackendSimple,
		"text_backend_complex":  &pf.TextBackendComplex,
		"default_text_backend":  &pf.DefaultTextBackend,
	}
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

const (
	http400 = http.StatusBadRequest
	http422 = http.StatusUnprocessableEntity
)

func (s *CustomProviderService) providerRow(ctx context.Context, providerDBID int64) (*repository.CustomProvider, error) {
	var row repository.CustomProvider
	err := s.db.WithContext(ctx).First(&row, providerDBID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NewText(http.StatusNotFound, "provider_not_found", nil)
		}
		return nil, err
	}
	return &row, nil
}

func (s *CustomProviderService) listModels(ctx context.Context, providerDBID int64) ([]repository.CustomProviderModel, error) {
	var rows []repository.CustomProviderModel
	if err := s.db.WithContext(ctx).
		Where("provider_id = ?", providerDBID).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *CustomProviderService) modelIDSet(ctx context.Context, providerDBID int64) (map[string]bool, error) {
	rows, err := s.listModels(ctx, providerDBID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		out[row.ModelID] = true
	}
	return out, nil
}

func (s *CustomProviderService) reloadLimits(ctx context.Context) {
	if s.limits == nil {
		return
	}
	s.limits.ReloadLimits(ctx)
}

// deleteCustomModels 显式删除供应商的全部模型行。
func deleteCustomModels(tx *gorm.DB, providerDBID int64) error {
	return tx.Where("provider_id = ?", providerDBID).Delete(&repository.CustomProviderModel{}).Error
}

// insertCustomModels 把写入形态落成模型行。
func insertCustomModels(tx *gorm.DB, providerDBID int64, models []CustomModelInput, now db.FlexibleTime) error {
	for _, model := range models {
		row, err := customModelRow(providerDBID, model, now)
		if err != nil {
			return err
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

func customModelRow(
	providerDBID int64, in CustomModelInput, now db.FlexibleTime,
) (repository.CustomProviderModel, error) {
	row := repository.CustomProviderModel{
		ProviderID:  providerDBID,
		ModelID:     in.ModelID,
		DisplayName: in.DisplayName,
		Endpoint:    in.Endpoint,
		IsDefault:   in.IsDefault,
		IsEnabled:   in.IsEnabled,
		PriceUnit:   in.PriceUnit,
		PriceInput:  in.PriceInput,
		PriceOutput: in.PriceOutput,
		Currency:    in.Currency,
		Resolution:  in.Resolution,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if len(in.SupportedDurations) > 0 {
		raw := joinCustomDurations(in.SupportedDurations)
		row.SupportedDurations = &raw
	}
	if narrowed := providers.CustomNarrowOverridesToAllowlist(in.CapabilityOverrides); len(narrowed) > 0 {
		raw, err := json.Marshal(narrowed)
		if err != nil {
			return repository.CustomProviderModel{}, err
		}
		text := string(raw)
		row.CapabilityOverrides = &text
	}
	if len(in.EndpointDeclaration) > 0 {
		raw, err := json.Marshal(in.EndpointDeclaration)
		if err != nil {
			return repository.CustomProviderModel{}, err
		}
		text := string(raw)
		row.EndpointDeclaration = &text
	}
	return row, nil
}

// customSettingValue 读单条全局设置（缺省空串）。
//
// 走 tx 而非 ConfigService：级联清理要与删除同事务，避免提交后才发现某条配置没清掉。
func customSettingValue(tx *gorm.DB, key string) (string, error) {
	var row repository.SystemSetting
	err := tx.Where("key = ?", key).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", err
	}
	return row.Value, nil
}

// customSetSetting 写一条全局设置。该表在 key 上没有唯一索引，先 UPDATE 再 INSERT。
func customSetSetting(tx *gorm.DB, key, value string) error {
	now := db.NewFlexibleTime(time.Now())
	res := tx.Model(&repository.SystemSetting{}).
		Where("key = ?", key).
		Updates(map[string]any{"value": value, "updated_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	return tx.Create(&repository.SystemSetting{
		Key: key, Value: value, UpdatedAt: now,
	}).Error
}

// parseCustomDurations 读回 supported_durations 声明列。
//
// 两种形态都认：Go 写入的逗号分隔（"5,10"）与 Python 写入的 JSON 数组（"[5, 10]"）。
// 两端复用同一个库，存量行是后者的形态，只认一种会让设置页把它们显示成「未声明」。
func parseCustomDurations(raw string) []int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "[") {
		var decoded []int
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			return nil
		}
		return decoded
	}
	var out []int
	for _, part := range strings.Split(raw, ",") {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		out = append(out, value)
	}
	return out
}

func joinCustomDurations(values []int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, ",")
}

func customBoolOrTrue(value *bool) bool {
	if value == nil {
		return true
	}
	return *value
}

// difference 返回只在 left 中出现的键。
func difference(left, right map[string]bool) map[string]bool {
	out := make(map[string]bool)
	for key := range left {
		if !right[key] {
			out[key] = true
		}
	}
	return out
}

// uniqueInOrder 去重并保持首次出现的顺序。
func uniqueInOrder(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
