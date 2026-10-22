// 项目级视频能力解析与时长取档（对应 server/services/video_caps.py 与
// server/services/reference_video_tasks.py 的读侧面、server/agent_runtime/sdk_tools/_context.py
// 的能力辅助）。
//
// 按「项目当前配置的视频后端」解析 model 粒度能力，供入队前的预检使用（时长取档、音频开关
// 判定、Voice_Profiles 注入判定）。入队时机尚未 resolve 任务实际的 provider/model——该解析
// 延后到 worker 执行时。
package service

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/provider"
)

// VideoCapabilityResolver 是项目视频能力与音频开关的解析能力（*config.Resolver 满足）。
type VideoCapabilityResolver interface {
	VideoCapabilitiesForProjectBucket(
		ctx context.Context, project *domain.ProjectFile, bucket provider.VideoBucket,
	) (config.VideoCapabilityView, error)
	VideoGenerateAudioForProject(ctx context.Context, project *domain.ProjectFile) (bool, error)
	ResolveResolution(ctx context.Context, project *domain.ProjectFile, providerID, modelID string) (string, error)
}

// FallbackUnitDuration 是 unit 时长缺值时的兜底秒数：执行层取档、入队前预检与新建 unit 的默认
// 时长共用此口径。
const FallbackUnitDuration = 8

// 取档相对剧本总时长的偏移方向（对应 lib/reference_video/duration_slots.py）。
const (
	AdjustmentExact         = "exact"
	AdjustmentUp            = "up"
	AdjustmentDown          = "down"
	AdjustmentUnconstrained = "unconstrained"
)

// DurationSlot 是取档结果：Seconds 是向 backend 申请的秒数，TotalSeconds 是剧本总时长。
type DurationSlot struct {
	Seconds      int
	TotalSeconds float64
	Adjustment   string
}

// NeedsConfirmation 报告申请秒数与剧本总时长不一致（能力不可解析时沿用现状放行）。
func (s DurationSlot) NeedsConfirmation() bool {
	return s.Adjustment == AdjustmentUp || s.Adjustment == AdjustmentDown
}

// ProjectDurationContext 是项目视频能力的一次性 IO 解析结果：档位全集（未按单个 unit 条件收窄）
// + 分辨率 + provider/model 身份 + 单次生成时长上限。
type ProjectDurationContext struct {
	SupportedDurations []int
	Resolution         string
	ProviderID         string
	ModelName          string
	MaxDuration        *int
}

// ProjectVideoCaps 是降级口径的项目视频能力解析结果。
type ProjectVideoCaps struct {
	View config.VideoCapabilityView
	// Resolved 报告能力解析是否成功；失败时调用方按各字段缺省（soft 档、无参考音频）处理。
	Resolved bool
	// RequestedGenerateAudio 是本集无声开关（独立解析，双失败才落 false）。
	RequestedGenerateAudio bool
}

// ResolveProjectVideoCaps 解析项目视频能力；解析失败返回部分结果，由调用方各自降级。
func ResolveProjectVideoCaps(
	ctx context.Context, r VideoCapabilityResolver, project *domain.ProjectFile, bucket provider.VideoBucket,
) ProjectVideoCaps {
	caps := ProjectVideoCaps{}
	if view, err := r.VideoCapabilitiesForProjectBucket(ctx, project, bucket); err == nil {
		caps.View = view
		caps.Resolved = true
	} else {
		slog.Info("无法解析 video_capabilities", "error", err)
	}
	// requested_generate_audio 不依赖能力接口，独立解析：能力解析失败不连带把用户的无声意图丢回默认。
	if generateAudio, err := r.VideoGenerateAudioForProject(ctx, project); err == nil {
		caps.RequestedGenerateAudio = generateAudio
	} else {
		slog.Info("video_generate_audio 独立解析也失败", "error", err)
		caps.RequestedGenerateAudio = false
	}
	return caps
}

// ResolveAudioSwitchConflict 判定项目的「关闭音频」意图是否落在一个收不到音轨开关的模型上；
// 冲突时返回 (provider, model, true)。解析失败一律返回无冲突（不把配置解析问题升级为提交期拒绝）。
func ResolveAudioSwitchConflict(
	ctx context.Context, r VideoCapabilityResolver, project *domain.ProjectFile, bucket provider.VideoBucket,
) (string, string, bool) {
	view, err := r.VideoCapabilitiesForProjectBucket(ctx, project, bucket)
	if err != nil {
		return "", "", false
	}
	info, ok := provider.ModelInfoFor(view.ProviderID, view.ModelID)
	if !ok || !provider.ModelAudioAlwaysOn(view.ProviderID, info) {
		return "", "", false
	}
	generateAudio, err := r.VideoGenerateAudioForProject(ctx, project)
	if err != nil {
		return "", "", false
	}
	if generateAudio {
		return "", "", false
	}
	return view.ProviderID, view.ModelID, true
}

// AssertAudioSwitchSupported 是智能体视频入队前的音频开关预检，冲突时返回错误。
func AssertAudioSwitchSupported(
	ctx context.Context, r VideoCapabilityResolver, project *domain.ProjectFile, bucket provider.VideoBucket,
) error {
	providerID, modelID, conflict := ResolveAudioSwitchConflict(ctx, r, project, bucket)
	if !conflict {
		return nil
	}
	return &AudioSwitchConflictError{ProviderID: providerID, ModelID: modelID}
}

// AudioSwitchConflictError 是音频开关冲突（成片恒有声但本集关了音频）。消息面向智能体转述，不走 i18n。
type AudioSwitchConflictError struct {
	ProviderID string
	ModelID    string
}

func (e *AudioSwitchConflictError) Error() string {
	return e.ProviderID + "/" + e.ModelID + " 的成片恒有声，无法关闭音频；" +
		"请让用户在设置中把音频开关改回开启后重试（当前配置会让声音照常出现，但音色约束被裁掉）"
}

// ResolveProjectIsSilent 判定这一集是否听不到声音——模型不产音或本集关闭音频，两条路径同口径。
func ResolveProjectIsSilent(ctx context.Context, r VideoCapabilityResolver, project *domain.ProjectFile) bool {
	bucket := provider.VideoBucketForGenerationMode(project.GenerationMode)
	caps := ResolveProjectVideoCaps(ctx, r, project, bucket)
	if !caps.Resolved {
		// 能力不可解析时档位按 soft 兜底（无信号不判定为真无声），无声与否只由开关决定。
		return !caps.RequestedGenerateAudio
	}
	if DeriveVoiceConsistency(caps.View, project.GenerationMode) == "none" {
		return true
	}
	return !caps.RequestedGenerateAudio
}

// DeriveVoiceConsistency 派生三级声音一致性标识（native / soft / none）。
func DeriveVoiceConsistency(view config.VideoCapabilityView, generationMode string) string {
	if view.Caps.ReferenceAudioMode == provider.ReferenceAudioDirect &&
		generationMode == string(domain.GenerationReferenceVideo) {
		return "native"
	}
	if info, ok := provider.ModelInfoFor(view.ProviderID, view.ModelID); ok &&
		provider.ModelHasAudioTrack(view.ProviderID, info) {
		return "soft"
	}
	return "none"
}

// VoiceConsistencyForCaps 与 DeriveVoiceConsistency 同口径，但直接取「已构造 backend 的能力声明」：
// 执行层按实际身份（含自定义供应商合成能力）派生，不另走一次项目能力视图查询。
func VoiceConsistencyForCaps(caps provider.VideoCapabilities, model config.ProviderModel, generationMode string) string {
	if caps.ReferenceAudioMode == provider.ReferenceAudioDirect &&
		generationMode == string(domain.GenerationReferenceVideo) {
		return "native"
	}
	if info, ok := provider.ModelInfoFor(model.ProviderID, model.ModelID); ok &&
		provider.ModelHasAudioTrack(model.ProviderID, info) {
		return "soft"
	}
	return "none"
}

// ResolveProjectDurationContext 一次性解析视频能力（档位全集 + 单次生成时长上限 + 分辨率 + 身份）。
// 解析失败按空档位处理（沿用现状放行）；分辨率仅在档位非空时解析。
func ResolveProjectDurationContext(
	ctx context.Context, r VideoCapabilityResolver, project *domain.ProjectFile, bucket provider.VideoBucket,
) ProjectDurationContext {
	out := ProjectDurationContext{}
	view, err := r.VideoCapabilitiesForProjectBucket(ctx, project, bucket)
	if err != nil {
		slog.Info("时长取档不施加档位约束", "error", err)
		return out
	}
	out.SupportedDurations = view.SupportedDurations
	out.ProviderID = view.ProviderID
	out.ModelName = view.ModelID
	if maxDuration, ok := MaxDurationOf(view.SupportedDurations); ok {
		out.MaxDuration = &maxDuration
	}
	if len(out.SupportedDurations) > 0 {
		out.Resolution = projectResolution(ctx, r, project, out.ProviderID, out.ModelName)
	}
	return out
}

// projectResolution 解析项目视频后端实际下发的分辨率；解析失败返回空串（该条约束随之不收窄）。
//
// 未显式配置时取 provider fallback，与执行层同源——预检若停在这里，就会漏掉「按 fallback
// 分辨率才生效」的档位约束。
func projectResolution(
	ctx context.Context, r VideoCapabilityResolver, project *domain.ProjectFile, providerID, modelID string,
) string {
	if providerID == "" || modelID == "" {
		return ""
	}
	resolution, err := r.ResolveResolution(ctx, project, providerID, modelID)
	if err != nil {
		slog.Info("无法解析 video resolution，时长取档不施加分辨率约束", "error", err)
		return ""
	}
	if resolution == "" {
		return config.ProviderFallbackResolution(providerID)
	}
	return resolution
}

// EffectiveReferenceDurations 返回参考视频路径实际可申请的时长档位：全集与该请求条件的约束求交。
//
// withReferenceImages 为 false 时不施加参考图约束（通用单元允许空 references、ad 缺图会退化为
// 纯文本，backend 同样只在 reference_images 非空时施加该约束）。无声明或交集为空时不收窄。
func EffectiveReferenceDurations(
	providerID, modelID string, durations []int, resolution string, withReferenceImages bool,
) []int {
	if len(durations) == 0 || providerID == "" || modelID == "" {
		return durations
	}
	info, ok := provider.ModelInfoFor(providerID, modelID)
	if !ok {
		return durations
	}
	allowed := append([]int(nil), durations...)
	if withReferenceImages && len(info.ReferenceImageDurations) > 0 {
		allowed = filterDurations(allowed, info.ReferenceImageDurations)
	}
	if resolution != "" {
		byResolution := info.DurationResolutionConstraints[strings.ToLower(strings.TrimSpace(resolution))]
		if len(byResolution) > 0 {
			allowed = filterDurations(allowed, byResolution)
		}
	}
	if len(allowed) == 0 {
		return durations
	}
	return allowed
}

// PrecheckUnit 按已解析的项目能力 context 为单个 unit 取档（纯函数，无 IO）。
//
// 「是否带参考图」的判据与调用方定桶同源：ad 传入成员镜头就从镜头现算参考集，其余按 unit 声明
// 的 references。unit 时长为脏数据（无法归一为整数秒）时返回错误，调用方按跳过处理。
func PrecheckUnit(
	dctx ProjectDurationContext, unit map[string]any, adShots []map[string]any,
) (DurationSlot, error) {
	withReferences := referenceImagePresence(unit, adShots)
	var durations []int
	if len(dctx.SupportedDurations) > 0 {
		durations = EffectiveReferenceDurations(
			dctx.ProviderID, dctx.ModelName, dctx.SupportedDurations, dctx.Resolution, withReferences,
		)
	}
	total, err := UnitScriptDuration(unit, adShots)
	if err != nil {
		return DurationSlot{}, err
	}
	return ResolveDurationSlot(float64(total), durations), nil
}

// referenceImagePresence 判定 unit 是否带参考图：ad 传成员镜头时现算参考集，否则读 unit 声明。
func referenceImagePresence(unit map[string]any, adShots []map[string]any) bool {
	if adShots != nil {
		return len(domain.AdUnitReferences(adShots)) > 0
	}
	if unit == nil {
		return false
	}
	return RefPresenceTruthy(unit["references"])
}

// RefPresenceTruthy 复刻 Python bool(value)：非 nil 且非空集合为真。
func RefPresenceTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

// ResolveDurationSlot 按容量语义为剧本总时长选择申请档位：申请能装下总时长的最小合法档位。
// 档位集为空（能力不可解析）时原样透传总时长。
func ResolveDurationSlot(totalSeconds float64, supportedDurations []int) DurationSlot {
	slots := SortedUniqueInts(supportedDurations)
	if len(slots) == 0 {
		return DurationSlot{Seconds: int(totalSeconds), TotalSeconds: totalSeconds, Adjustment: AdjustmentUnconstrained}
	}
	for _, candidate := range slots {
		if float64(candidate) >= totalSeconds {
			adjustment := AdjustmentUp
			if float64(candidate) == totalSeconds {
				adjustment = AdjustmentExact
			}
			return DurationSlot{Seconds: candidate, TotalSeconds: totalSeconds, Adjustment: adjustment}
		}
	}
	return DurationSlot{Seconds: slots[len(slots)-1], TotalSeconds: totalSeconds, Adjustment: AdjustmentDown}
}

// UnitScriptDuration 取 unit 的剧本编排时长（秒）：ad 取成员镜头求和，narration/drama 取 unit 字段。
//
// 字段值来自磁盘剧本 JSON，非整数秒/不可解析的脏数据返回错误，由调用方按跳过处理。
func UnitScriptDuration(unit map[string]any, adShots []map[string]any) (int, error) {
	if adShots != nil {
		if total := domain.AdScriptTotalDuration(adShots); total != 0 {
			return total, nil
		}
		return FallbackUnitDuration, nil
	}
	if unit != nil {
		if raw, present := unit["duration_seconds"]; present && RefPresenceTruthy(raw) {
			return intSeconds(raw)
		}
	}
	return FallbackUnitDuration, nil
}

// intSeconds 把外部时长值归一为整数秒（Python int() 语义：数值截断、字符串仅接整数形式）。
func intSeconds(raw any) (int, error) {
	switch v := raw.(type) {
	case float64:
		return int(v), nil
	case int:
		return v, nil
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, &DurationValueError{Value: raw}
		}
		return parsed, nil
	}
	return 0, &DurationValueError{Value: raw}
}

// DurationValueError 表示 unit 时长字段不是可解析的数值。
type DurationValueError struct{ Value any }

func (e *DurationValueError) Error() string {
	return "unit duration_seconds 不是可解析的数值"
}

// ResolveMaxUnitDuration 解析项目视频后端的单次生成时长上限（秒）；解析失败返回 nil（派生分组
// 退化为仅按镜头数切分）。
func ResolveMaxUnitDuration(ctx context.Context, r VideoCapabilityResolver, project *domain.ProjectFile) *int {
	caps := ResolveProjectVideoCaps(ctx, r, project, provider.VideoBucketForGenerationMode(project.GenerationMode))
	if !caps.Resolved {
		return nil
	}
	if maxDuration, ok := MaxDurationOf(caps.View.SupportedDurations); ok {
		return &maxDuration
	}
	return nil
}

// DefaultUnitDuration 是新建 unit 的默认时长（秒）：项目偏好 > 收窄后的最短档位 > 兜底。
func DefaultUnitDuration(dctx ProjectDurationContext, project *domain.ProjectFile, withReferences bool) int {
	if project == nil {
		return FallbackUnitDuration
	}
	durations := EffectiveReferenceDurations(
		dctx.ProviderID, dctx.ModelName, dctx.SupportedDurations, dctx.Resolution, withReferences,
	)
	if len(durations) == 0 {
		return FallbackUnitDuration
	}
	if project.DefaultDuration != nil && ContainsInt(durations, *project.DefaultDuration) {
		return *project.DefaultDuration
	}
	minValue, _ := minDurationOf(durations)
	return minValue
}

// FetchVideoCaps 解析 (default_duration, supported_durations) 供 MCP 工具调用。
//
// supported_durations 已按项目分辨率与生成模式经时长联动约束收窄（未收窄的集合交给 LLM 会产出
// 执行期必然被拒的时长）。default_duration 是用户配置的原样值。
func FetchVideoCaps(
	ctx context.Context, r VideoCapabilityResolver, project *domain.ProjectFile, generationMode string,
) (*int, []int) {
	bucket := provider.VideoBucketForGenerationMode(project.GenerationMode)
	view, err := r.VideoCapabilitiesForProjectBucket(ctx, project, bucket)
	var durations []int
	if err == nil {
		durations = view.SupportedDurations
	}
	durations = constrainedCapsDurations(ctx, r, project, view, durations, generationMode, nil)
	return project.DefaultDuration, durations
}

// ReferenceUnitDurationTiers 返回参考视频路径逐 unit 的两套生效档位：带参考图、不带参考图。
//
// 不带图集按 i2v 桶模型求值（无引用 unit 执行期降级到 i2v 桶执行）；i2v 桶解析失败或无档位声明
// 时回退按同一份 r2v 输入求值。两套集合之间不假定包含关系。
func ReferenceUnitDurationTiers(
	ctx context.Context, r VideoCapabilityResolver, project *domain.ProjectFile,
	view config.VideoCapabilityView, durations []int,
) (withReferences []int, withoutReferences []int) {
	withReferences = constrainedCapsDurations(ctx, r, project, view, durations, string(domain.GenerationReferenceVideo), boolPtr(true))
	i2vView, i2vDurations := view, durations
	if resolved, err := r.VideoCapabilitiesForProjectBucket(ctx, project, provider.BucketI2V); err == nil {
		if len(resolved.SupportedDurations) > 0 {
			i2vView, i2vDurations = resolved, resolved.SupportedDurations
		}
	}
	withoutReferences = constrainedCapsDurations(
		ctx, r, project, i2vView, i2vDurations, string(domain.GenerationReferenceVideo), boolPtr(false),
	)
	return withReferences, withoutReferences
}

// constrainedCapsDurations 按 caps 里的模型身份对 durations 施加时长联动约束。
func constrainedCapsDurations(
	ctx context.Context, r VideoCapabilityResolver, project *domain.ProjectFile,
	view config.VideoCapabilityView, durations []int, generationMode string, usesReferenceImages *bool,
) []int {
	if len(durations) == 0 {
		return durations
	}
	withReferences := generationMode == string(domain.GenerationReferenceVideo)
	if usesReferenceImages != nil {
		withReferences = *usesReferenceImages
	}
	resolution := resolutionForConstraints(ctx, r, project, view.ProviderID, view.ModelID, generationMode)
	return EffectiveReferenceDurations(view.ProviderID, view.ModelID, durations, resolution, withReferences)
}

// resolutionForConstraints 求约束求值用的生效分辨率：项目已保存档位，参考视频模式下补 provider 兜底。
func resolutionForConstraints(
	ctx context.Context, r VideoCapabilityResolver, project *domain.ProjectFile,
	providerID, modelID, generationMode string,
) string {
	if providerID == "" || modelID == "" {
		return ""
	}
	saved, err := r.ResolveResolution(ctx, project, providerID, modelID)
	if err != nil {
		saved = ""
	}
	if saved != "" || generationMode != string(domain.GenerationReferenceVideo) {
		return saved
	}
	return config.ProviderFallbackResolution(providerID)
}

// filterDurations 取 candidates 中同时落在 allowed 里的项（保序、保重）。
func filterDurations(candidates, allowed []int) []int {
	out := make([]int, 0, len(candidates))
	for _, d := range candidates {
		if ContainsInt(allowed, d) {
			out = append(out, d)
		}
	}
	return out
}

func SortedUniqueInts(values []int) []int {
	set := map[int]bool{}
	for _, v := range values {
		set[v] = true
	}
	out := make([]int, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func MaxDurationOf(values []int) (int, bool) {
	if len(values) == 0 {
		return 0, false
	}
	best := values[0]
	for _, v := range values[1:] {
		if v > best {
			best = v
		}
	}
	return best, true
}

func minDurationOf(values []int) (int, bool) {
	if len(values) == 0 {
		return 0, false
	}
	best := values[0]
	for _, v := range values[1:] {
		if v < best {
			best = v
		}
	}
	return best, true
}

func boolPtr(v bool) *bool { return &v }
