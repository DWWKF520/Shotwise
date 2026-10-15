// 生成执行门面（对应 lib/media_generator.py 的视频 / TTS 分支）：串联「解析身份 → 构造
// backend → 能力闸门 → 记账括号 → 生成 → 结算 → 版本登记」。
//
// 记账括号不变式：能力闸门与 backend 构造都在开括号之前，失败不落 pending 行；括号一旦
// 开启，`defer FinishFailure(ctx, call, &err)` 兜底，取消（context.Canceled）穿透不记账。
//
// 结算语义转写（对应 Python _settlement_from_result 的 union 分发）：
//   - video：backend 回报的 duration_seconds 覆盖 start 时的请求时长（BilledDurationSeconds）；
//   - audio：合成字符数（Characters）进 usage_tokens，驱动按字符计费。
package service

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/pricing"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	// 视频 / 音频 backend 在装载期（init）把自己注册进 provider 的 backend registry，
	// 本文件依赖它以拿到可执行的实现；providers 不反向依赖 service。
	"github.com/shotwise/shotwise/internal/providers"
	"gorm.io/gorm"
)

// VideoGenerator 是视频生成路径的执行门面。
type VideoGenerator struct {
	db       *gorm.DB
	resolver *config.Resolver
	cfg      *config.ConfigService
	ledger   *Ledger
	tasks    *repository.TaskRepo
}

// NewVideoGenerator 构造；ledger 为 nil 时只生成不记账（供冒烟 / 诊断路径）。
func NewVideoGenerator(db *gorm.DB, ledger *Ledger) *VideoGenerator {
	return &VideoGenerator{
		db:       db,
		resolver: config.NewResolver(db),
		cfg:      config.NewConfigService(db),
		ledger:   ledger,
		tasks:    repository.NewTaskRepo(db),
	}
}

// VideoTarget 是一次视频生成的解析产物（对应 Python 侧 resolve_generation_context 的
// video lane）：任务路径解析一次后，身份、backend 与时长声明同源供时长守卫与实际生成
// 共用，消除「守卫按 A 身份校验、生成按 B 身份执行」的口径分叉窗口。
type VideoTarget struct {
	ProviderModel config.ProviderModel
	Backend       provider.VideoBackend
	durations     []int
}

// SupportedDurations 返回该身份的声明时长（秒）；空 = 未声明，由时长守卫 fail loud。
func (t *VideoTarget) SupportedDurations() []int { return t.durations }

// Resolve 解析视频执行身份并构造 backend。taskID 非空时注入 submit 后的 job_id /
// api_call_id 持久化（worker 路径）；自定义供应商的 job_id 持久化注入随 resume 链路补齐，
// api_call_id 由记账括号显式写回，不依赖该注入点。
func (g *VideoGenerator) Resolve(
	ctx context.Context, project *domain.ProjectFile, payload map[string]any,
	bucket provider.VideoBucket, taskID string,
) (*VideoTarget, error) {
	selected, err := g.resolver.ResolveVideoBackend(ctx, project, payload, &bucket)
	if err != nil {
		return nil, err
	}
	if config.IsCustomProvider(selected.ProviderID) {
		input, err := config.LoadCustomBackendInput(ctx, g.db, selected.ProviderID, selected.ModelID)
		if err != nil {
			return nil, err
		}
		backend, err := providers.CreateCustomBackend(providers.CustomProviderConfig{
			ProviderID: input.ProviderID,
			BaseURL:    input.BaseURL,
			APIKey:     input.APIKey,
		}, selected.ModelID, input.Endpoint, input.EndpointDeclaration, input.CapabilityOverrides)
		if err != nil {
			return nil, err
		}
		if backend.MediaType != provider.MediaVideo || backend.Video == nil {
			return nil, fmt.Errorf("custom endpoint %q is not a video endpoint", input.Endpoint)
		}
		// 自定义模型的时长声明读 DB 列；启发式预设（duration_presets）随自定义能力波次补齐。
		// 空声明不在此报错——由时长守卫按「能力未解析」统一 fail loud。
		return &VideoTarget{
			ProviderModel: selected,
			Backend:       backend.Video,
			durations:     parseDurationsCSV(input.SupportedDurations),
		}, nil
	}
	spec, ok := provider.ProviderSpecFor(selected.ProviderID, provider.MediaVideo)
	if !ok {
		return nil, fmt.Errorf("no video backend spec for provider %s", selected.ProviderID)
	}
	creds, err := g.cfg.ProviderConfig(ctx, selected.ProviderID)
	if err != nil {
		return nil, err
	}
	model := selected.ModelID
	var durations []int
	if info, ok := provider.ModelInfoFor(selected.ProviderID, selected.ModelID); ok {
		if info.APIModelName != "" {
			model = info.APIModelName
		}
		durations = info.SupportedDurations
	}
	opts := provider.BackendOptions{Model: model, APIKey: creds["api_key"], BaseURL: creds["base_url"]}
	if taskID != "" {
		// submit 拿到 job_id 后立即落库（fail-fast，ADR 0007）：未持久化的 submit 无法续跑。
		opts.PersistJobID = g.tasks.PersistProviderJobID
		opts.PersistAPICallID = g.tasks.PersistAPICallID
	}
	backend, err := provider.CreateVideoBackend(spec.RegistryBackend, opts)
	if err != nil {
		return nil, err
	}
	return &VideoTarget{ProviderModel: selected, Backend: backend, durations: durations}, nil
}

// VideoGenerateParams 是一次视频生成的入参。身份解析已前置（VideoTarget），
// prompt 须为已归一化文本（含反向提示词尾巴）。
type VideoGenerateParams struct {
	ProjectName string
	// ProjectDir 项目目录：产物路径与版本账本都落在这里。
	ProjectDir string
	UserID     string
	// TaskID worker 任务 id；非空时 call_id 先于 backend 调用写回 task payload。
	TaskID string
	// ResourceType 参与记账 segment_id 白名单（videos / storyboards / reference_videos）。
	ResourceType string
	ResourceID   string
	Prompt       string
	StartImage   string
	EndImage     string
	// ReferenceImages 多参考图；ReferenceAudioFiles 参考音频（音色复刻），顺序即 prompt
	// 中「音频N」的指认顺序，任何重排都会把 A 角色的音色安到 B 角色头上。
	ReferenceImages       []string
	ReferenceAudioFiles   []string
	ReferenceAudioTargets []int
	// AspectRatio 是用户比例意图：记账与分镜图生成沿用原值；实际下发值可能被
	// FirstFrameRatioAdaptiveOnly 后端改写。
	AspectRatio     string
	DurationSeconds int
	Resolution      string
	Seed            *int64
	// ServiceTier 空串按 "default"；GenerateAudio 为 nil 时读全局 video_generate_audio 设置。
	ServiceTier   string
	GenerateAudio *bool
	// VersionMetadata 进 versions.json 的额外元数据（duration_seconds / service_tier /
	// seed 由门面统一补齐，调用方无须重复传）。
	VersionMetadata map[string]any
}

// VideoGenerateResult 是视频生成的产物摘要。
type VideoGenerateResult struct {
	OutputPath string
	Version    int
	VideoURI   string
}

// Generate 执行一次视频生成：产物路径 → 旧产物补版本 → 能力闸门 → 记账括号 → 生成 →
// 结算 → 版本登记。
func (g *VideoGenerator) Generate(
	ctx context.Context, target *VideoTarget, p VideoGenerateParams,
) (res VideoGenerateResult, err error) {
	if target == nil || target.Backend == nil {
		return VideoGenerateResult{}, fmt.Errorf("video target 未解析，先调 Resolve")
	}
	backend := target.Backend
	outRel, err := resourceRelativePath(p.ResourceType, p.ResourceID)
	if err != nil {
		return VideoGenerateResult{}, err
	}
	outPath, err := fs.SafeJoin(p.ProjectDir, false, filepath.FromSlash(outRel))
	if err != nil {
		return VideoGenerateResult{}, fmt.Errorf("非法资源 ID: %s", p.ResourceID)
	}

	serviceTier := p.ServiceTier
	if serviceTier == "" {
		serviceTier = "default"
	}
	meta := map[string]any{"duration_seconds": p.DurationSeconds, "service_tier": serviceTier}
	if p.Seed != nil {
		meta["seed"] = *p.Seed
	}
	for k, v := range p.VersionMetadata {
		if v != nil {
			meta[k] = v
		}
	}
	versions := NewVersions(p.ProjectDir)
	if _, err := versions.EnsureCurrentTracked(p.ResourceType, p.ResourceID, p.Prompt, outPath, meta); err != nil {
		return VideoGenerateResult{}, err
	}

	// 能力闸门先于记账括号：违约直接失败，不扣费、不留 failed ApiCall 行。
	caps := provider.ResolveVideoCapabilities(backend, serviceTier, p.Resolution)
	if err := provider.GateVideoRequest(&caps, backend.Name(), backend.Model(), provider.VideoGateInput{
		Prompt:              &p.Prompt,
		EndImage:            p.EndImage != "",
		ReferenceImages:     p.ReferenceImages,
		ReferenceAudioFiles: p.ReferenceAudioFiles,
		// 参考音频总时长探测（ffprobe）随参考音频编排迁移补齐；nil 按既定降级口径跳过该单项校验。
	}); err != nil {
		return VideoGenerateResult{}, err
	}

	generateAudio := p.GenerateAudio
	if generateAudio == nil {
		configured, err := g.videoGenerateAudio(ctx)
		if err != nil {
			return VideoGenerateResult{}, err
		}
		generateAudio = &configured
	}

	request := provider.NewVideoGenerationRequest(p.Prompt, outPath)
	// FirstFrameRatioAdaptiveOnly 后端的下发比例改写；记账沿用调用方原始比例（上方面）。
	request.AspectRatio = provider.ResolveFirstFrameAspectRatio(&caps, p.AspectRatio, p.StartImage != "")
	request.DurationSeconds = p.DurationSeconds
	request.Resolution = p.Resolution
	request.StartImage = p.StartImage
	request.EndImage = p.EndImage
	request.ReferenceImages = p.ReferenceImages
	request.ReferenceAudioFiles = p.ReferenceAudioFiles
	request.ReferenceAudioTargets = p.ReferenceAudioTargets
	request.GenerateAudio = *generateAudio
	request.ProjectName = p.ProjectName
	request.TaskID = p.TaskID
	request.ServiceTier = serviceTier
	request.Seed = p.Seed

	var out provider.VideoGenerationResult
	var call *Call
	if g.ledger != nil {
		call, err = g.ledger.Start(ctx, StartParams{
			ProjectName:     p.ProjectName,
			CallType:        "video",
			Model:           backend.Model(),
			Prompt:          p.Prompt,
			Resolution:      p.Resolution,
			DurationSeconds: &p.DurationSeconds,
			AspectRatio:     p.AspectRatio,
			GenerateAudio:   generateAudio,
			Provider:        target.ProviderModel.ProviderID,
			UserID:          p.UserID,
			SegmentID:       segmentIDFor("video", p.ResourceType, p.ResourceID),
		})
		if err != nil {
			return VideoGenerateResult{}, err
		}
		defer func() { g.ledger.FinishFailure(ctx, call, &err) }()

		// call_id 先于 backend 调用写回 task payload：崩溃重启后 resume 按精准 id 翻这条
		// pending 记账；写回失败经兜底翻 failed 重抛，不留永久 pending（ADR 0007）。
		if p.TaskID != "" {
			if err = g.tasks.PersistAPICallID(ctx, p.TaskID, call.ID); err != nil {
				return VideoGenerateResult{}, err
			}
		}
	}
	if out, err = backend.Generate(ctx, request); err != nil {
		return VideoGenerateResult{}, err
	}
	if call != nil {
		path := out.VideoPath
		if err = g.ledger.FinishSuccess(ctx, call, FinishParams{
			Settlement: pricing.Settlement{
				UsageTokens:           int64ToInt(out.UsageTokens),
				GenerateAudio:         out.GenerateAudio,
				BilledDurationSeconds: intPtr(out.DurationSeconds),
				ServiceTier:           serviceTier,
			},
			OutputPath:  &path,
			CustomPrice: resolveCustomPrice(ctx, g.db, target.ProviderModel.ProviderID, target.ProviderModel.ModelID),
		}); err != nil {
			return VideoGenerateResult{}, err
		}
	}

	version, _, err := versions.AddVersion(p.ResourceType, p.ResourceID, p.Prompt, outPath, meta)
	if err != nil {
		return VideoGenerateResult{}, err
	}
	return VideoGenerateResult{OutputPath: outPath, Version: version, VideoURI: out.VideoURI}, nil
}

// videoGenerateAudio 读全局 video_generate_audio 设置；空白按 Python 侧默认 True。
func (g *VideoGenerator) videoGenerateAudio(ctx context.Context) (bool, error) {
	raw, err := g.cfg.GetSetting(ctx, "video_generate_audio", "")
	if err != nil {
		return false, err
	}
	if raw == "" {
		return true, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("video_generate_audio 设置非法: %q", raw)
	}
	return value, nil
}

// AudioGenerator 是语音合成路径的执行门面（TTS 同步调用，无 submit-poll-resume）。
type AudioGenerator struct {
	db       *gorm.DB
	resolver *config.Resolver
	cfg      *config.ConfigService
	ledger   *Ledger
}

// NewAudioGenerator 构造；ledger 为 nil 时只合成不记账。
func NewAudioGenerator(db *gorm.DB, ledger *Ledger) *AudioGenerator {
	return &AudioGenerator{
		db:       db,
		resolver: config.NewResolver(db),
		cfg:      config.NewConfigService(db),
		ledger:   ledger,
	}
}

// AudioGenerateParams 是一次语音合成的入参。
type AudioGenerateParams struct {
	ProjectName string
	Project     *domain.ProjectFile
	ProjectDir  string
	UserID      string
	// ResourceType / ResourceID：audio 通道的记账 segment_id 无条件透传 resource_id。
	ResourceType string
	ResourceID   string
	Text         string
	// Voice 空串时按「项目 narration_voice > 全局设置 > 默认」解析（对应 Python 侧
	// narration_voice 解析链）。
	Voice        string
	LanguageType string // 空串按 Chinese
	Speed        *float64
	// Payload 携带历史任务的 audio_provider / audio_model 覆盖（排空场景），无则 nil。
	Payload         map[string]any
	VersionMetadata map[string]any
}

// AudioGenerateResult 是语音合成的产物摘要。
type AudioGenerateResult struct {
	OutputPath string
	Version    int
}

// Generate 执行一次语音合成：产物路径 → 旧产物补版本 → 记账括号 → 合成 → 结算 → 版本登记。
func (g *AudioGenerator) Generate(
	ctx context.Context, p AudioGenerateParams,
) (res AudioGenerateResult, err error) {
	selected, err := g.resolver.ResolveAudioBackend(ctx, p.Project, p.Payload)
	if err != nil {
		return AudioGenerateResult{}, err
	}
	backend, err := g.buildAudioBackend(ctx, selected)
	if err != nil {
		return AudioGenerateResult{}, err
	}
	outRel, err := resourceRelativePath(p.ResourceType, p.ResourceID)
	if err != nil {
		return AudioGenerateResult{}, err
	}
	outPath, err := fs.SafeJoin(p.ProjectDir, false, filepath.FromSlash(outRel))
	if err != nil {
		return AudioGenerateResult{}, fmt.Errorf("非法资源 ID: %s", p.ResourceID)
	}
	versions := NewVersions(p.ProjectDir)
	if _, err := versions.EnsureCurrentTracked(p.ResourceType, p.ResourceID, p.Text, outPath, p.VersionMetadata); err != nil {
		return AudioGenerateResult{}, err
	}

	voice := p.Voice
	if voice == "" && p.Project != nil {
		voice = p.Project.NarrationVoice
	}
	if voice == "" {
		if voice, err = g.cfg.NarrationVoice(ctx); err != nil {
			return AudioGenerateResult{}, err
		}
	}

	request := provider.NewAudioSynthesisRequest(p.Text, outPath, voice)
	if p.LanguageType != "" {
		request.LanguageType = p.LanguageType
	}
	request.Speed = p.Speed

	var out provider.AudioSynthesisResult
	var call *Call
	if g.ledger != nil {
		call, err = g.ledger.Start(ctx, StartParams{
			ProjectName: p.ProjectName,
			CallType:    "audio",
			Model:       backend.Model(),
			Prompt:      p.Text,
			Provider:    selected.ProviderID,
			UserID:      p.UserID,
			SegmentID:   segmentIDFor("audio", p.ResourceType, p.ResourceID),
		})
		if err != nil {
			return AudioGenerateResult{}, err
		}
		defer func() { g.ledger.FinishFailure(ctx, call, &err) }()
	}
	if out, err = backend.Synthesize(ctx, request); err != nil {
		return AudioGenerateResult{}, err
	}
	if call != nil {
		if err = g.ledger.FinishSuccess(ctx, call, FinishParams{
			Settlement: pricing.Settlement{
				// 合成字符数 → usage_tokens：按字符计费的语义转写唯一落点。
				UsageTokens: intPtr(out.Characters),
			},
			OutputPath:  &outPath,
			CustomPrice: resolveCustomPrice(ctx, g.db, selected.ProviderID, selected.ModelID),
		}); err != nil {
			return AudioGenerateResult{}, err
		}
	}

	version, _, err := versions.AddVersion(p.ResourceType, p.ResourceID, p.Text, outPath, p.VersionMetadata)
	if err != nil {
		return AudioGenerateResult{}, err
	}
	return AudioGenerateResult{OutputPath: outPath, Version: version}, nil
}

// buildAudioBackend 按解析出的身份构造音频 backend（与 buildImageBackend 同构）。
func (g *AudioGenerator) buildAudioBackend(ctx context.Context, selected config.ProviderModel) (provider.AudioBackend, error) {
	if config.IsCustomProvider(selected.ProviderID) {
		input, err := config.LoadCustomBackendInput(ctx, g.db, selected.ProviderID, selected.ModelID)
		if err != nil {
			return nil, err
		}
		backend, err := providers.CreateCustomBackend(providers.CustomProviderConfig{
			ProviderID: input.ProviderID,
			BaseURL:    input.BaseURL,
			APIKey:     input.APIKey,
		}, selected.ModelID, input.Endpoint, input.EndpointDeclaration, input.CapabilityOverrides)
		if err != nil {
			return nil, err
		}
		if backend.MediaType != provider.MediaAudio || backend.Audio == nil {
			return nil, fmt.Errorf("custom endpoint %q is not an audio endpoint", input.Endpoint)
		}
		return backend.Audio, nil
	}
	spec, ok := provider.ProviderSpecFor(selected.ProviderID, provider.MediaAudio)
	if !ok {
		return nil, fmt.Errorf("no audio backend spec for provider %s", selected.ProviderID)
	}
	creds, err := g.cfg.ProviderConfig(ctx, selected.ProviderID)
	if err != nil {
		return nil, err
	}
	model := selected.ModelID
	if info, ok := provider.ModelInfoFor(selected.ProviderID, selected.ModelID); ok && info.APIModelName != "" {
		model = info.APIModelName
	}
	return provider.CreateAudioBackend(spec.RegistryBackend, provider.BackendOptions{
		Model:   model,
		APIKey:  creds["api_key"],
		BaseURL: creds["base_url"],
	})
}

// ---------------------------------------------------------------------------
// 自定义供应商价格预查与时长守卫
// ---------------------------------------------------------------------------

// resolveCustomPrice 预查自定义供应商的声明价格（对应 CustomProviderRepository.resolve_price），
// 供结算侧 CustomPrice 入参。非自定义 / 畸形 id / 查询异常 / 查无模型统一降级为 nil
// （零费用结算），绝不阻断记账。刻意不校验 enabled：记账按实际调用的那个模型的声明价计费，
// 与该模型当前是否启用无关——过滤会让对「事后被停用模型」的历史调用丢价。
func resolveCustomPrice(ctx context.Context, db *gorm.DB, providerID, modelID string) *pricing.CustomPrice {
	if db == nil || !config.IsCustomProvider(providerID) {
		return nil
	}
	dbID, err := config.ParseProviderID(providerID)
	if err != nil {
		return nil
	}
	var model repository.CustomProviderModel
	if err := db.WithContext(ctx).
		Where("provider_id = ? AND model_id = ?", dbID, modelID).
		First(&model).Error; err != nil {
		slog.Debug("自定义供应商价格查询失败", "provider", providerID, "model", modelID, "error", err)
		return nil
	}
	return &pricing.CustomPrice{
		PriceInput:  model.PriceInput,
		PriceOutput: model.PriceOutput,
		Currency:    stringOr(model.Currency, ""),
	}
}

// intPtr 返回入参的指针（记账行的可空数值列）。
func intPtr(v int) *int { return &v }

// parseDurationsCSV 解析 custom_provider_model.supported_durations 声明列（逗号分隔整数）。
// 空声明 / 脏条目跳过，全部不可解析时返回空列表，由时长守卫按「能力未解析」报错。
func parseDurationsCSV(raw string) []int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
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

// constrainVideoDurations 按「分辨率 ↔ 时长」联动约束收窄候选（对应 constrain_durations
// 的视频侧口径）：交集为空时返回原候选——空集说明声明自相矛盾，清空候选会让上游拿不到任何
// 可用时长，执行期仍有 backend 校验兜底。「参考图 ↔ 时长」约束随参考视频路径迁移补齐。
// 型号不在注册表（自定义供应商）时不施加联动约束。
func constrainVideoDurations(providerID, modelID string, candidates []int, resolution string) []int {
	if len(candidates) == 0 {
		return candidates
	}
	info, ok := provider.ModelInfoFor(providerID, modelID)
	if !ok {
		return candidates
	}
	if resolution == "" {
		return candidates
	}
	byResolution := info.DurationResolutionConstraints[strings.ToLower(strings.TrimSpace(resolution))]
	if len(byResolution) == 0 {
		return candidates
	}
	allowed := make([]int, 0, len(candidates))
	for _, d := range candidates {
		for _, a := range byResolution {
			if d == a {
				allowed = append(allowed, d)
				break
			}
		}
	}
	if len(allowed) == 0 {
		return candidates
	}
	return allowed
}

// assertDurationSupported 是执行层时长能力守卫：duration 必须落在已解析身份的声明时长内。
// 这是 duration ↔ supported_durations 唯一的权威校验家（provider 执行期才解析，见 ADR-0001）；
// 声明为空时报告能力未解析，禁止把未知能力交给 backend 猜测。
func assertDurationSupported(duration int, supported []int) error {
	if len(supported) == 0 {
		return provider.NewVideoCapabilityError(provider.FailureVideoCapabilitiesUnresolved, map[string]any{
			"name": "selected video model",
		})
	}
	for _, d := range supported {
		if d == duration {
			return nil
		}
	}
	parts := make([]string, 0, len(supported))
	for _, d := range supported {
		parts = append(parts, strconv.Itoa(d))
	}
	return provider.NewVideoCapabilityError(provider.FailureVideoDurationNotSupported, map[string]any{
		"duration":  duration,
		"supported": strings.Join(parts, ", "),
	})
}

// videoDurationSeconds 把外部输入（payload / project.json）的时长值安全归一为整数秒：
// 可解析为整数秒（"6" / 6.0）的归一化比较；非整数秒（4.5）一律拒绝，不做截断式归一化。
func videoDurationSeconds(raw any) (int, error) {
	var numeric float64
	switch v := raw.(type) {
	case int:
		numeric = float64(v)
	case int64:
		numeric = float64(v)
	case float64:
		numeric = v
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, provider.NewVideoCapabilityError(provider.FailureVideoDurationInvalid, map[string]any{
				"duration": v,
			})
		}
		numeric = parsed
	default:
		return 0, provider.NewVideoCapabilityError(provider.FailureVideoDurationInvalid, map[string]any{
			"duration": raw,
		})
	}
	if numeric != float64(int(numeric)) {
		return 0, provider.NewVideoCapabilityError(provider.FailureVideoDurationInvalid, map[string]any{
			"duration": raw,
		})
	}
	return int(numeric), nil
}
