// 运行时配置解析：回答「这次生成用哪个 provider 的哪个 model」（对应 Python lib/config/resolver.py
// 的解析链）。本文件只做选择与身份校验，不构造任何 backend 客户端。
//
// 层级骨架（docs/adr/0054）：payload 锁定 > 项目桶 > 项目默认 > 全局桶 > 全局默认 > 自动推断。
// 桶是可选覆盖：桶层取不到值（含显式清空）就回退默认层，不把「没配桶」当成「配了空」。
//
// 供应商级禁用在骨架内统一拦截：显式引用（项目配置 / 全局设置 / payload 锁定）指向被禁用的
// 供应商时一律报错，不静默回退到别的供应商——换供应商等于静默改用户意图。
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	"gorm.io/gorm"
)

// ImageCapability 是图片能力桶：t2i（文生图）/ i2i（图生图）。与视频桶同构，决定图片
// 任务走哪条覆盖槽（docs/adr/0001）。
type ImageCapability string

const (
	// ImageCapT2I 文生图桶。
	ImageCapT2I ImageCapability = "t2i"
	// ImageCapI2I 图生图桶。
	ImageCapI2I ImageCapability = "i2i"
)

// ProviderModel 是解析结果值对象：一对 (规范 provider_id, model_id)。
//
// 这是「选了哪个 provider 及其 model」，**不是** backend——解析不构造任何客户端，命名刻意
// 避开 *Backend 以保持身份与执行体的区分。provider_id 一律为规范形态（内置 registry 键或
// custom-<db_id>），解析链假设输入即规范形态，不做归一化。
type ProviderModel struct {
	ProviderID string
	ModelID    string
}

// PairKey 复合键形态 "<provider>/<model>"，与 splitPair 互逆。
//
// 配置层与 payload 层的 provider 字段、按执行模型取键的 per-model 存储都用这一形态；
// 写入方一律经此方法成形，格式只此一处定义。
func (p ProviderModel) PairKey() string { return p.ProviderID + "/" + p.ModelID }

// ErrProviderDisabled 供应商级禁用（预置 / 自定义）。解析链不静默回退换供应商。
var ErrProviderDisabled = errors.New("provider disabled")

// ErrNoUsableProvider 自动推断扫不到任何 ready + 启用 + 该媒体的供应商。
var ErrNoUsableProvider = errors.New("no usable provider")

// VideoBucketCapabilityError 是视频解析闸报错：解析出的模型缺所属能力桶要求的能力，
// 或配置引用已不可用（供应商下线 / 模型被删 / endpoint 改成别的媒体类型）。
//
// Code 是 i18n errors 目录 key、Params 是其渲染参数：router 可直接本地化，worker 落库经
// provider.EncodeFailureMessage 结构化编码。Error() 返回英文技术消息，供日志与非用户可见
// 路径使用。
type VideoBucketCapabilityError struct {
	Code       provider.FailureCode
	Bucket     provider.VideoBucket
	ProviderID string
	ModelID    string
	Message    string
}

// Error 实现 error。
func (e *VideoBucketCapabilityError) Error() string { return e.Message }

// Params 渲染参数。
func (e *VideoBucketCapabilityError) Params() map[string]string {
	return map[string]string{"provider": e.ProviderID, "model": e.ModelID}
}

// videoBucketCapabilityMissing 模型缺该桶所需能力。
func videoBucketCapabilityMissing(bucket provider.VideoBucket, providerID, modelID string) error {
	code := provider.FailureVideoCapabilityMissingI2V
	if bucket == provider.BucketR2V {
		code = provider.FailureVideoCapabilityMissingR2V
	}
	return &VideoBucketCapabilityError{
		Code:       code,
		Bucket:     bucket,
		ProviderID: providerID,
		ModelID:    modelID,
		Message:    fmt.Sprintf("video model %s/%s lacks the capability required by the %s bucket", providerID, modelID, bucket),
	}
}

// videoBucketReferenceUnavailable 配置引用已不可用。
func videoBucketReferenceUnavailable(bucket provider.VideoBucket, providerID, modelID string) error {
	return &VideoBucketCapabilityError{
		Code:       provider.FailureVideoCapabilityReferenceUnavailable,
		Bucket:     bucket,
		ProviderID: providerID,
		ModelID:    modelID,
		Message: fmt.Sprintf(
			"configured video model %s/%s is no longer resolvable for the %s bucket", providerID, modelID, bucket,
		),
	}
}

// IsVideoBucketCapabilityError 判断错误链上是否有视频解析闸错误。
func IsVideoBucketCapabilityError(err error) (*VideoBucketCapabilityError, bool) {
	var target *VideoBucketCapabilityError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// splitPair 解析 "<provider>/<model>" → (provider, model)；不合法返回 false。
//
// provider 或 model 为空 / 纯空白（如 "openai/" / "/m"）一律视为不合法，交由调用方走裸
// provider 补默认 model 或回退——避免把空 model 带到执行层。
func splitPair(raw string) (string, string, bool) {
	if !strings.Contains(raw, "/") {
		return "", "", false
	}
	providerID, modelID, _ := strings.Cut(raw, "/")
	providerID, modelID = strings.TrimSpace(providerID), strings.TrimSpace(modelID)
	if providerID == "" || modelID == "" {
		return "", "", false
	}
	return providerID, modelID, true
}

// parseProjectProvider 解析项目层字段，兼容裸 provider 覆盖。
//
//   - "provider/model" → (provider, model)
//   - 裸 "provider"（registry 中存在且有该 media 默认 model）→ (provider, 默认 model)
//   - 其余 → false（交由更低优先级层解析）
//
// 裸 provider 经写边界保证是规范 id，这里 pin 住该 provider 并补全其默认 model，避免静默
// 回退到全局默认的**另一**供应商。
func parseProjectProvider(raw string, media provider.MediaType) (ProviderModel, bool) {
	if providerID, modelID, ok := splitPair(raw); ok {
		return ProviderModel{ProviderID: providerID, ModelID: modelID}, true
	}
	// 裸 provider，或带尾斜杠缺 model 的脏值（如 "openai/"）→ 取该 provider 默认 model。
	providerID := strings.Trim(strings.TrimSpace(raw), "/")
	if providerID == "" {
		return ProviderModel{}, false
	}
	modelID, ok := provider.DefaultModelForProvider(providerID, media)
	if !ok {
		return ProviderModel{}, false
	}
	return ProviderModel{ProviderID: providerID, ModelID: modelID}, true
}

// trustedPayloadProvider 返回可信任的规范 provider_id，否则空串。
//
// payload 是解析链唯一绕过写边界校验的输入来源（in-flight 队列任务在旧代码入队时即序列化）。
// 据此守卫：非字符串 / 空白 / 不可识别的 provider 一律不予信任，让解析回退到已迁移的
// project/global——不做 legacy→规范映射，仅拒绝不可信输入。
func trustedPayloadProvider(raw string) string {
	providerID := strings.TrimSpace(raw)
	if providerID == "" {
		return ""
	}
	if _, ok := provider.ProviderMetaFor(providerID); ok {
		return providerID
	}
	if IsCustomProvider(providerID) {
		return providerID
	}
	return ""
}

// payloadModelOrDefault payload 显式 model 优先；缺失则补该 provider 的 registry 默认 model。
//
// 避免「半截 payload」（只有 provider、缺 model）把空 model 带到执行层。
func payloadModelOrDefault(rawModel, providerID string, media provider.MediaType) string {
	if model := strings.TrimSpace(rawModel); model != "" {
		return model
	}
	model, _ := provider.DefaultModelForProvider(providerID, media)
	return model
}

// payloadString 读 payload 的字符串键；非字符串 / 缺失一律空串。
func payloadString(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	raw, ok := payload[key].(string)
	if !ok {
		return ""
	}
	return raw
}

// payloadVideoPinnedPair 读 payload 里入队时锁定的视频执行身份（桶键 video_provider_<cap>），
// 连同命中的桶一并返回。
//
// 入队只为任务所属的那一个桶写键，故 bucket 未声明（resume 等不承诺桶的调用方）时按固定桶序
// 扫两个桶键——至多命中一个，桶序不产生歧义。
func payloadVideoPinnedPair(payload map[string]any, bucket *provider.VideoBucket) (provider.VideoBucket, ProviderModel, bool) {
	buckets := provider.VideoBucketOrder
	if bucket != nil {
		buckets = []provider.VideoBucket{*bucket}
	}
	for _, cap := range buckets {
		if providerID, modelID, ok := splitPair(payloadString(payload, "video_provider_"+string(cap))); ok {
			return cap, ProviderModel{ProviderID: providerID, ModelID: modelID}, true
		}
	}
	return "", ProviderModel{}, false
}

// layeredKeys 是「默认 + 能力桶」四级解析骨架的键位声明，媒体类型无关（docs/adr/0054）。
//
// 键为空串表示该层不存在、直接跳过——新媒体 / 新桶接入只需补一份键位声明，不改骨架本身。
type layeredKeys struct {
	mediaType         provider.MediaType
	parseFallback     string
	projectBucketKey  string
	projectDefaultKey string
	globalBucketKey   string
	globalDefaultKey  string
}

// imageLayeredKeys 图片桶（t2i / i2i）键位。项目默认层用 project.json 的
// default_image_backend 字段，全局默认层用 default_image_backend 设置键。
func imageLayeredKeys(cap ImageCapability) layeredKeys {
	return layeredKeys{
		mediaType:         provider.MediaImage,
		parseFallback:     DefaultImageBackend,
		projectBucketKey:  fmt.Sprintf("image_provider_%s", cap),
		projectDefaultKey: "default_image_backend",
		globalBucketKey:   fmt.Sprintf("default_image_backend_%s", cap),
		globalDefaultKey:  "default_image_backend",
	}
}

// videoLayeredKeys 视频桶（i2v / r2v）键位。默认层零迁移：项目默认层复用既有 video_backend
// 字段，全局默认层复用 default_video_backend 键。
func videoLayeredKeys(bucket provider.VideoBucket) layeredKeys {
	return layeredKeys{
		mediaType:         provider.MediaVideo,
		parseFallback:     DefaultVideoBackend,
		projectBucketKey:  fmt.Sprintf("video_provider_%s", bucket),
		projectDefaultKey: "video_backend",
		globalBucketKey:   fmt.Sprintf("default_video_backend_%s", bucket),
		globalDefaultKey:  "default_video_backend",
	}
}

// videoDefaultLayeredKeys 不定桶视频键位：与 videoLayeredKeys 同源同层，只去掉桶层。供不
// 承诺能力桶的调用方（费用估算、限流路由兜底、配置展示）解析。
var videoDefaultLayeredKeys = layeredKeys{
	mediaType:         provider.MediaVideo,
	parseFallback:     DefaultVideoBackend,
	projectDefaultKey: "video_backend",
	globalDefaultKey:  "default_video_backend",
}

// audioLayeredKeys 音频键位：音频无能力桶，桶层留空由骨架跳过。
var audioLayeredKeys = layeredKeys{
	mediaType:         provider.MediaAudio,
	parseFallback:     DefaultAudioBackend,
	projectDefaultKey: "audio_backend",
	globalDefaultKey:  "default_audio_backend",
}

// projectProviderValue 按配置键名读 project.json 的对应字段；未知键返回空串。
//
// 键名与 layeredKeys 的 project 层键位一一对应，新增键位时同步补此表（漏配即该层恒空、
// 静默降级到全局层，是解析链最难察觉的一类缺陷）。
func projectProviderValue(project *project.ProjectFile, key string) string {
	if project == nil {
		return ""
	}
	switch key {
	case "video_provider_i2v":
		return project.VideoProviderI2V
	case "video_provider_r2v":
		return project.VideoProviderR2V
	case "video_backend":
		return project.VideoBackend
	case "image_provider_t2i":
		return project.ImageProviderT2I
	case "image_provider_i2i":
		return project.ImageProviderI2I
	case "default_image_backend":
		return project.DefaultImageBackend
	case "audio_backend":
		return project.AudioBackend
	case "default_text_backend":
		return project.DefaultTextBackend
	default:
		return ""
	}
}

// Resolver 是运行时配置解析器：ConfigService 的上层薄封装，提供唯一的默认值定义点、
// 类型化输出与内置优先级解析。
type Resolver struct {
	db  *gorm.DB
	svc *ConfigService
}

// NewResolver 构造解析器（DB 即配置读取源，不做缓存——本地 SQLite 开销可忽略）。
func NewResolver(db *gorm.DB) *Resolver {
	return &Resolver{db: db, svc: NewConfigService(db)}
}

// ProjectVideoBackendIDs 返回 project.json 自报的视频模型身份：按 generation_mode 定桶取桶键，
// 缺则取项目默认键（对应 Python project_video_backend_ids）。
//
// 纯读 project.json、不查 DB，供 caps 解析失败（DB / migration 故障等）时的降级路径复用：
// 桶键与默认键都在同一个明文文件里，降级只该丢掉 DB 那部分，不该顺带把桶口径也降成项目
// 默认层——否则配了 video_provider_r2v 的参考视频项目会拿 video_backend 的档位收编剧本。
// 层内取值口径与 resolveLayered 的项目层一致（含裸 provider 覆盖）。
func ProjectVideoBackendIDs(project *project.ProjectFile) (ProviderModel, bool) {
	if project == nil {
		return ProviderModel{}, false
	}
	keys := videoLayeredKeys(provider.VideoBucketForGenerationMode(project.GenerationMode))
	for _, key := range []string{keys.projectBucketKey, keys.projectDefaultKey} {
		if key == "" {
			continue
		}
		if selected, ok := parseProjectProvider(projectProviderValue(project, key), provider.MediaVideo); ok {
			return selected, true
		}
	}
	return ProviderModel{}, false
}

// ResolveRawSupportedDurations 返回收窄前的时长全集：caps → registry 两级解析（对应 Python
// resolve_raw_supported_durations）。
//
// 两级都取不到时返回 nil，表示「该项目尚未配置可解析的视频型号」。caps 是自定义供应商
// （custom- 前缀）唯一的档位来源——registry 只收录内建供应商，能拿到 caps 的调用方都应先解析
// caps 再调本函数，不带 caps 调用对这类项目恒为 nil。caps 为 nil 或无档位时退回 registry 级
// 解析：项目自报身份按 generation_mode 定桶取（ProjectVideoBackendIDs），不直取项目默认层。
//
// 返回值不含「分辨率↔时长」「参考图↔时长」联动约束，收窄在 service 侧
// EffectiveReferenceDurations。
func ResolveRawSupportedDurations(project *project.ProjectFile, caps *VideoCapabilityView) []int {
	if caps != nil && len(caps.SupportedDurations) > 0 {
		return append([]int(nil), caps.SupportedDurations...)
	}
	if ids, ok := ProjectVideoBackendIDs(project); ok {
		if info, ok := provider.ModelInfoFor(ids.ProviderID, ids.ModelID); ok && len(info.SupportedDurations) > 0 {
			return append([]int(nil), info.SupportedDurations...)
		}
	}
	return nil
}

// ResolveImageBackend 解析图片任务应使用的身份。
//
// 优先级：payload > 项目桶（image_provider_<cap>）> 项目默认（default_image_backend）>
// 全局桶（default_image_backend_<cap>）> 全局默认（default_image_backend）> 自动推断。
// 图片任务不锁定执行身份（任务周期短，排队期间配置漂移的窗口小），故 payload 层无桶键，
// 只认 image_provider / image_model 的历史格式。
func (r *Resolver) ResolveImageBackend(
	ctx context.Context, project *project.ProjectFile, payload map[string]any, cap ImageCapability,
) (ProviderModel, error) {
	if providerID := trustedPayloadProvider(payloadString(payload, "image_provider")); providerID != "" {
		if model := payloadModelOrDefault(payloadString(payload, "image_model"), providerID, provider.MediaImage); model != "" {
			if err := r.ensureProviderEnabled(ctx, providerID, model); err != nil {
				return ProviderModel{}, err
			}
			return ProviderModel{ProviderID: providerID, ModelID: model}, nil
		}
	}
	return r.resolveLayered(ctx, project, imageLayeredKeys(cap))
}

// ResolveVideoBackend 解析视频任务应使用的身份。
//
// bucket 为 nil 时走不定桶骨架（不过能力闸，自定义供应商收敛到默认启用 model）；非 nil 时走
// 桶骨架并对结果施加能力闸：模型缺该桶所需能力、或配置引用已不可用时返回
// *VideoBucketCapabilityError，不静默换模型（docs/adr/0054）。
//
// payload 恒为最高优先级：入队时锁进桶键 video_provider_<cap> 的执行身份原样返回、不过能力闸，
// 只过身份可用性校验（悬空即报错）——已入队任务按 payload 照常执行，不回头补校验。
func (r *Resolver) ResolveVideoBackend(
	ctx context.Context, project *project.ProjectFile, payload map[string]any, bucket *provider.VideoBucket,
) (ProviderModel, error) {
	if pinnedBucket, pinned, ok := payloadVideoPinnedPair(payload, bucket); ok {
		if _, err := r.ensureVideoIdentityResolvable(ctx, pinned, pinnedBucket); err != nil {
			return ProviderModel{}, err
		}
		return pinned, nil
	}
	if bucket == nil {
		selected, err := r.resolveLayered(ctx, project, videoDefaultLayeredKeys)
		if err != nil {
			return ProviderModel{}, err
		}
		return r.resolveEffectiveVideoProviderModel(ctx, selected)
	}
	selected, err := r.resolveLayered(ctx, project, videoLayeredKeys(*bucket))
	if err != nil {
		return ProviderModel{}, err
	}
	// 身份闸在此显式过一遍（能力闸内部也会过）：桶路径要求「引用不可解析」先于
	// 「能力不匹配」报错，两次调用幂等且无副作用。
	if _, err := r.ensureVideoIdentityResolvable(ctx, selected, *bucket); err != nil {
		return ProviderModel{}, err
	}
	if err := r.ensureVideoBucketCapability(ctx, selected, *bucket); err != nil {
		return ProviderModel{}, err
	}
	return selected, nil
}

// ResolveAudioBackend 解析音频任务应使用的身份：payload > 项目默认 > 全局默认 > 自动推断。
//
// payload 层服务于历史任务（携带 audio_provider + audio_model）的排空。
func (r *Resolver) ResolveAudioBackend(
	ctx context.Context, project *project.ProjectFile, payload map[string]any,
) (ProviderModel, error) {
	if providerID := trustedPayloadProvider(payloadString(payload, "audio_provider")); providerID != "" {
		if model := payloadModelOrDefault(payloadString(payload, "audio_model"), providerID, provider.MediaAudio); model != "" {
			if err := r.ensureProviderEnabled(ctx, providerID, model); err != nil {
				return ProviderModel{}, err
			}
			return ProviderModel{ProviderID: providerID, ModelID: model}, nil
		}
	}
	return r.resolveLayered(ctx, project, audioLayeredKeys)
}

// DefaultVideoBackend 仅全局层解析视频默认身份（全局默认键 > 自动推断），不含项目级覆盖。
func (r *Resolver) DefaultVideoBackend(ctx context.Context) (ProviderModel, error) {
	selected, err := r.resolveLayered(ctx, nil, videoDefaultLayeredKeys)
	if err != nil {
		return ProviderModel{}, err
	}
	return r.resolveEffectiveVideoProviderModel(ctx, selected)
}

// DefaultImageBackend 仅全局层解析图片默认身份。
func (r *Resolver) DefaultImageBackend(ctx context.Context, cap ImageCapability) (ProviderModel, error) {
	return r.resolveLayered(ctx, nil, imageLayeredKeys(cap))
}

// DefaultAudioBackend 仅全局层解析音频默认身份。
func (r *Resolver) DefaultAudioBackend(ctx context.Context) (ProviderModel, error) {
	return r.resolveLayered(ctx, nil, audioLayeredKeys)
}

// VideoBackend 解析项目当前配置的视频身份（不定桶，不做有效身份收敛），供配置展示与
// 「当前选的是哪个」类判断使用。
func (r *Resolver) VideoBackend(ctx context.Context, project *project.ProjectFile) (ProviderModel, error) {
	return r.resolveLayered(ctx, project, videoDefaultLayeredKeys)
}

// ResolveResolution 解析项目生效的图像/视频分辨率档（对应 Python resolve_resolution）。
//
// 优先级：project.json 的 model_settings["provider/model"].resolution。空串表示调用时不传
// SDK resolution 参数，由调用方按各自保底档处理。
//
// 迁移边界（不静默降级、也不臆造数据）：Python 侧还有两级来源——legacy 字段
// video_model_settings[model].resolution、以及自定义供应商模型行声明的默认分辨率。
// Go 侧 project.json 域结构未保留该 legacy 字段、custom_provider_model 亦无 resolution 列，
// 两级待对应字段迁移后补齐；当前只落 model_settings 一级。
func (r *Resolver) ResolveResolution(
	ctx context.Context, project *project.ProjectFile, providerID, modelID string,
) (string, error) {
	if project == nil || providerID == "" || modelID == "" {
		return "", nil
	}
	settings := project.ModelSettings[providerID+"/"+modelID]
	if settings == nil {
		return "", nil
	}
	return strings.TrimSpace(settings["resolution"]), nil
}

// resolveLayered 是「默认 + 能力桶」四级解析骨架：项目桶 > 项目默认 > 全局桶 > 全局默认 >
// 自动推断。媒体类型无关，各层键位由 layeredKeys 声明。
//
// 项目层字段兼容裸 provider 覆盖（parseProjectProvider）；全局层要求 "provider/model" 完整
// 形态，含 "/" 才作数。供应商级禁用在此统一拦截。
func (r *Resolver) resolveLayered(ctx context.Context, project *project.ProjectFile, keys layeredKeys) (ProviderModel, error) {
	if project != nil {
		for _, key := range []string{keys.projectBucketKey, keys.projectDefaultKey} {
			if key == "" {
				continue
			}
			selected, ok := parseProjectProvider(projectProviderValue(project, key), keys.mediaType)
			if !ok {
				continue
			}
			if err := r.ensureProviderEnabled(ctx, selected.ProviderID, selected.ModelID); err != nil {
				return ProviderModel{}, err
			}
			return selected, nil
		}
	}
	settings, err := r.svc.GetAllSettings(ctx)
	if err != nil {
		return ProviderModel{}, err
	}
	for _, key := range []string{keys.globalBucketKey, keys.globalDefaultKey} {
		if key == "" {
			continue
		}
		raw := settings[key]
		if !strings.Contains(raw, "/") {
			continue
		}
		providerID, modelID := ParseBackend(raw, keys.parseFallback)
		if err := r.ensureProviderEnabled(ctx, providerID, modelID); err != nil {
			return ProviderModel{}, err
		}
		return ProviderModel{ProviderID: providerID, ModelID: modelID}, nil
	}
	return r.autoResolveBackend(ctx, keys.mediaType)
}

// ensureProviderEnabled 生成链路拦截：供应商级禁用（预置 / 自定义）直接报错，禁止解析为
// 执行身份。
//
// 自定义供应商须同时满足供应商级与模型级启用；模型级检查由各调用点（如视频身份闸）负责。
// 畸形 custom id（db id 非整数，如 custom-abc）无 DB 事实可查，保持历史宽容语义放行不拦截。
func (r *Resolver) ensureProviderEnabled(ctx context.Context, providerID, modelID string) error {
	if IsCustomProvider(providerID) {
		dbID, err := ParseProviderID(providerID)
		if err != nil {
			return nil
		}
		model, prov, err := customModelWithProvider(r.db.WithContext(ctx), dbID, modelID)
		if err != nil {
			return err
		}
		if model != nil && prov != nil {
			if !prov.IsEnabled {
				return fmt.Errorf("%w: %s", ErrProviderDisabled, providerID)
			}
			return nil
		}
		// join 未命中（模型或供应商不存在）：回退查供应商行确认启用状态；模型存在性由各
		// 媒体调用点负责。
		prov, err = customProviderByID(r.db.WithContext(ctx), dbID)
		if err != nil {
			return err
		}
		if prov == nil || !prov.IsEnabled {
			return fmt.Errorf("%w: %s", ErrProviderDisabled, providerID)
		}
		return nil
	}
	enabled, err := r.svc.ProviderEnabled(ctx, providerID)
	if err != nil {
		return err
	}
	if !enabled {
		return fmt.Errorf("%w: %s", ErrProviderDisabled, providerID)
	}
	return nil
}

// autoResolveBackend 遍历内置 registry（按注册顺序）找第一个 ready + 启用 + 该媒体有默认
// model 的供应商；内置全不可用再扫自定义供应商的默认启用 model。
func (r *Resolver) autoResolveBackend(ctx context.Context, media provider.MediaType) (ProviderModel, error) {
	for _, providerID := range provider.ProviderIDs() {
		enabled, err := r.svc.ProviderEnabled(ctx, providerID)
		if err != nil || !enabled {
			continue
		}
		if !r.svc.ProviderReady(ctx, providerID) {
			continue
		}
		if modelID, ok := provider.DefaultModelForProvider(providerID, media); ok {
			return ProviderModel{ProviderID: providerID, ModelID: modelID}, nil
		}
	}
	for _, model := range enabledCustomModelsByMedia(r.db.WithContext(ctx), string(media)) {
		if !model.IsDefault {
			continue
		}
		prov, err := customProviderByID(r.db.WithContext(ctx), model.ProviderID)
		if err != nil || prov == nil || !prov.IsEnabled {
			continue
		}
		return ProviderModel{ProviderID: MakeProviderID(model.ProviderID), ModelID: model.ModelID}, nil
	}
	return ProviderModel{}, fmt.Errorf("%w for media %s", ErrNoUsableProvider, media)
}

// ensureVideoIdentityResolvable 校验视频身份现在仍解析得到，悬空即报
// [video_capability_reference_unavailable]。
//
// 判的是身份存在性而非能力声明：内置模型看 registry（供应商下线 / 模型被删 / 所引模型压根
// 不是视频模型），自定义模型看 DB（供应商被删、模型被删或禁用、endpoint 改成其它媒体类型）。
// backend caps 函数不都校验 model 存在性、也不都校验 media_type（部分 provider 对任意 model id
// 返回静态能力），故身份需单独判。自定义模型命中时返回该行，供调用方就地合成能力、不重复查库。
func (r *Resolver) ensureVideoIdentityResolvable(
	ctx context.Context, selected ProviderModel, bucket provider.VideoBucket,
) (*repository.CustomProviderModel, error) {
	if !IsCustomProvider(selected.ProviderID) {
		info, ok := provider.ModelInfoFor(selected.ProviderID, selected.ModelID)
		if !ok || info.MediaType != provider.MediaVideo {
			return nil, videoBucketReferenceUnavailable(bucket, selected.ProviderID, selected.ModelID)
		}
		enabled, err := r.svc.ProviderEnabled(ctx, selected.ProviderID)
		if err != nil {
			return nil, err
		}
		if !enabled {
			return nil, videoBucketReferenceUnavailable(bucket, selected.ProviderID, selected.ModelID)
		}
		return nil, nil
	}
	dbID, err := ParseProviderID(selected.ProviderID)
	if err != nil {
		return nil, videoBucketReferenceUnavailable(bucket, selected.ProviderID, selected.ModelID)
	}
	model, prov, err := customModelWithProvider(r.db.WithContext(ctx), dbID, selected.ModelID)
	if err != nil {
		return nil, err
	}
	if model == nil || prov == nil || !model.IsEnabled || !prov.IsEnabled {
		return nil, videoBucketReferenceUnavailable(bucket, selected.ProviderID, selected.ModelID)
	}
	media, err := EndpointMediaType(model.Endpoint)
	if err != nil || media != string(provider.MediaVideo) {
		return nil, videoBucketReferenceUnavailable(bucket, selected.ProviderID, selected.ModelID)
	}
	return model, nil
}

// ensureVideoBucketCapability 能力闸：校验解析出的模型具备该桶所需能力，不满足直接报错、
// 不静默换模型。
//
// 判定经 provider.VideoCapabilitySatisfied，与桶候选下拉共用一份口径：内置模型两维都取
// backend 的 VideoCapabilities（与请求构造同源），自定义模型取 endpoint 系统判定 ⊕ 用户覆盖
// 的合成结果。
func (r *Resolver) ensureVideoBucketCapability(
	ctx context.Context, selected ProviderModel, bucket provider.VideoBucket,
) error {
	model, err := r.ensureVideoIdentityResolvable(ctx, selected, bucket)
	if err != nil {
		return err
	}
	var caps provider.VideoCapabilities
	if model != nil {
		caps, err = synthesizeVideoCapabilities(model.Endpoint, model.ModelID, model.CapabilityOverrides)
		if err != nil {
			return videoBucketReferenceUnavailable(bucket, selected.ProviderID, selected.ModelID)
		}
	} else {
		spec, ok := provider.ProviderSpecFor(selected.ProviderID, provider.MediaVideo)
		if !ok {
			return videoBucketReferenceUnavailable(bucket, selected.ProviderID, selected.ModelID)
		}
		caps, err = provider.VideoCapabilitiesForModel(spec.RegistryBackend, selected.ModelID)
		if err != nil {
			return videoBucketReferenceUnavailable(bucket, selected.ProviderID, selected.ModelID)
		}
	}
	if !provider.VideoCapabilitySatisfied(bucket, caps.FirstFrame, caps.MaxReferenceImages) {
		return videoBucketCapabilityMissing(bucket, selected.ProviderID, selected.ModelID)
	}
	return nil
}

// resolveEffectiveVideoProviderModel 把选择身份收敛为 backend 构造时会实际使用的视频身份。
//
// 内置 provider 的 registry 身份已是有效身份；自定义 provider 需与构造侧共用同一规则：model
// 不存在、禁用或 endpoint 已改成其它 media 时，回退到该供应商默认启用的 video model。
// 入队锁定的身份不走这条收敛（换 model 执行等于静默换模型）。
func (r *Resolver) resolveEffectiveVideoProviderModel(ctx context.Context, selected ProviderModel) (ProviderModel, error) {
	if !IsCustomProvider(selected.ProviderID) {
		enabled, err := r.svc.ProviderEnabled(ctx, selected.ProviderID)
		if err != nil {
			return ProviderModel{}, err
		}
		if !enabled {
			return ProviderModel{}, fmt.Errorf("%w: %s", ErrProviderDisabled, selected.ProviderID)
		}
		return selected, nil
	}
	dbID, err := ParseProviderID(selected.ProviderID)
	if err != nil {
		return ProviderModel{}, fmt.Errorf("invalid custom provider id: %s", selected.ProviderID)
	}
	model, prov, err := customModelWithProvider(r.db.WithContext(ctx), dbID, selected.ModelID)
	if err != nil {
		return ProviderModel{}, err
	}
	if model != nil && prov != nil {
		if !prov.IsEnabled {
			return ProviderModel{}, fmt.Errorf("%w: %s", ErrProviderDisabled, selected.ProviderID)
		}
		if media, err := EndpointMediaType(model.Endpoint); err == nil && model.IsEnabled &&
			media == string(provider.MediaVideo) {
			return selected, nil
		}
	} else {
		prov, err = customProviderByID(r.db.WithContext(ctx), dbID)
		if err != nil {
			return ProviderModel{}, err
		}
		if prov == nil || !prov.IsEnabled {
			return ProviderModel{}, fmt.Errorf("%w: %s", ErrProviderDisabled, selected.ProviderID)
		}
	}
	// 模型已不存在 / 已禁用 / 媒体类型不符：收敛到该供应商的默认启用 video model。
	defaultModel := customDefaultModelByMedia(r.db.WithContext(ctx), dbID, string(provider.MediaVideo))
	if defaultModel == nil {
		return ProviderModel{}, fmt.Errorf("custom model not found: %s", selected.PairKey())
	}
	return ProviderModel{ProviderID: selected.ProviderID, ModelID: defaultModel.ModelID}, nil
}

// synthesizeVideoCapabilities 自定义供应商的视频能力：系统判定 ⊕ 用户覆盖 → 生效能力。
//
// 系统判定当前只覆盖 endpoint 维度硬上限型（endpointVideoMaxReferenceImages）；其余 video
// endpoint 的参考图上限随 model 异质、声明在各 backend 的 per-model caps 函数里，那部分
// endpoint→backend 映射随 backend 波次落地，此前一律 fail loud、不猜测。
//
// 覆盖字典从 JSON 列读出，逐键按类型校验：类型不符的键忽略并回退系统判定，不把脏值并进能力。
func synthesizeVideoCapabilities(endpoint, modelID string, overrides *string) (provider.VideoCapabilities, error) {
	media, err := EndpointMediaType(endpoint)
	if err != nil || media != string(provider.MediaVideo) {
		return provider.VideoCapabilities{}, fmt.Errorf("endpoint %q is not a video endpoint", endpoint)
	}
	limit, ok := endpointVideoMaxReferenceImages[endpoint]
	if !ok {
		return provider.VideoCapabilities{}, fmt.Errorf(
			"video endpoint %q declares no system capability source", endpoint,
		)
	}
	caps := provider.DefaultVideoCapabilities()
	caps.MaxReferenceImages = limit
	if overrides == nil || strings.TrimSpace(*overrides) == "" {
		return caps, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(*overrides), &raw); err != nil || raw == nil {
		return caps, nil
	}
	if v, ok := raw["first_frame"].(bool); ok {
		caps.FirstFrame = v
	}
	if v, ok := raw["last_frame"].(bool); ok {
		caps.LastFrame = v
	}
	if v, ok := raw["max_reference_images"].(float64); ok && v >= 0 {
		caps.MaxReferenceImages = int(v)
	}
	return caps, nil
}
