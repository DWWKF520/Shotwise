package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// DashScopeVideoBackend 是阿里百炼 HappyHorse / 万相视频生成后端（异步两步式）。
//
// 走原生 video-generation/video-synthesis 异步端点：submit 取 task_id → 轮询 GET /tasks/{id} 至
// SUCCEEDED → 下载 video_url。覆盖 happyhorse-1.0 / happyhorse-1.1 与 wan2.7 系列的 t2v / i2v / r2v。
//
// 注：t2v / i2v 起始帧用 media[{type:"first_frame"}]；尾帧 / 续写字段在一手 docs 未确权，故 i2v
// 仅声明首帧能力。

// DashScope 视频后端常量。
const (
	dashscopeVideoDefaultModel = "happyhorse-1.1-i2v"

	dashscopeVideoEndpoint = "/services/aigc/video-generation/video-synthesis"

	dashscopeVideoMinPollTimeout    = 900 * time.Second
	dashscopeVideoPollTimeoutSecond = 60.0

	// dashscopeVideoPollInterval 是轮询间隔（百炼建议 15 秒）。
	dashscopeVideoPollInterval = 15 * time.Second

	// dashscopeVideoRequestTimeout 是单次请求的墙钟预算（Python http_timeout 默认 60s）。
	dashscopeVideoRequestTimeout = 60 * time.Second

	// dashscopeMaxBilledDurationSeconds 是计费时长合理上限（24h），防止超大数值写入 DB Integer 列。
	dashscopeMaxBilledDurationSeconds = 86400
)

// wan2.7-r2v 的 reference_voice 逐段挂在参考素材项上，故音频段数上限等同参考素材总数上限
// （官方：参考图像 + 参考视频 ≤ 5）。
const dashscopeWan27R2VMaxReference = 5

// wan2.7 全家族 prompt 上限 5000 字符（官方参数表原文「超过部分自动截断」，照常计费）。
const dashscopeWan27MaxPromptChars = 5000

// wan3.0 单模型覆盖文生 / 图生 / 参考生三条路径：首帧 + 尾帧，参考图 10 张，参考音频 5 段、
// 总时长 15 秒，prompt 上限 20000 字符。
const (
	dashscopeWan3MaxReferenceImages           = 10
	dashscopeWan3MaxReferenceAudio            = 5
	dashscopeWan3MaxReferenceAudioTotalSecond = 15.0
	dashscopeWan3MaxPromptChars               = 20000
	// dashscopeWan3ModelKey 是 wan3.0 的 model id 片段——单模型通吃三条路径、音轨可控、可走独立 maas 域名。
	dashscopeWan3ModelKey = "wan3.0-video"
)

// dashscopeReferenceAudioMIMETypes 是 wan2.7 / wan3.0 参考音频支持的扩展名 → MIME。
// URL 形态与 media.url 同为 http / oss / base64 data URI。
var dashscopeReferenceAudioMIMETypes = map[string]string{
	".wav": "audio/wav",
	".mp3": "audio/mpeg",
}

// dashscopeVideoProfiles 是按 model id 派发的能力声明表：happyhorse-r2v 仅 reference_image（无
// first_frame）；wan2.7-r2v 额外支持首帧与参考音色。
//
// 各 profile key 互不为子串，故 map 遍历顺序无歧义。
var dashscopeVideoProfiles = buildDashscopeVideoProfiles()

func buildDashscopeVideoProfiles() map[string]provider.VideoCapabilities {
	wan27Prompt := dashscopeWan27MaxPromptChars
	wan3Prompt := dashscopeWan3MaxPromptChars
	wan3AudioTotal := dashscopeWan3MaxReferenceAudioTotalSecond

	noFirst := func() provider.VideoCapabilities {
		caps := provider.DefaultVideoCapabilities()
		caps.FirstFrame = false
		return caps
	}
	withWan27Prompt := func(caps provider.VideoCapabilities) provider.VideoCapabilities {
		caps.MaxPromptChars = &wan27Prompt
		return caps
	}

	wan27t2v := withWan27Prompt(noFirst())
	wan27i2v := withWan27Prompt(provider.DefaultVideoCapabilities())

	wan27r2v := provider.DefaultVideoCapabilities()
	wan27r2v.MaxReferenceImages = dashscopeWan27R2VMaxReference
	wan27r2v.ReferenceAudioMode = provider.ReferenceAudioDirect
	wan27r2v.MaxReferenceAudioCount = dashscopeWan27R2VMaxReference
	// 音色挂在具体参考素材项上（attachReferenceVoices），不是独立音色输入通道，编排层必须显式
	// 给出「谁的声音配哪张图」的映射，不能假设与 ReferenceAudioFiles 同序。
	wan27r2v.ReferenceAudioPerImage = true
	wan27r2v.MaxPromptChars = &wan27Prompt

	wan3 := provider.DefaultVideoCapabilities()
	wan3.LastFrame = true
	wan3.MaxReferenceImages = dashscopeWan3MaxReferenceImages
	wan3.ReferenceAudioMode = provider.ReferenceAudioDirect
	wan3.MaxReferenceAudioCount = dashscopeWan3MaxReferenceAudio
	wan3.MaxReferenceAudioTotalSeconds = &wan3AudioTotal
	wan3.MaxPromptChars = &wan3Prompt

	return map[string]provider.VideoCapabilities{
		"happyhorse-1.1-t2v": noFirst(),
		"happyhorse-1.1-i2v": provider.DefaultVideoCapabilities(),
		"happyhorse-1.1-r2v": func() provider.VideoCapabilities {
			caps := noFirst()
			caps.MaxReferenceImages = 9
			return caps
		}(),
		"happyhorse-1.0-t2v": noFirst(),
		"happyhorse-1.0-i2v": provider.DefaultVideoCapabilities(),
		"happyhorse-1.0-r2v": func() provider.VideoCapabilities {
			caps := noFirst()
			caps.MaxReferenceImages = 9
			return caps
		}(),
		"wan2.7-t2v":          wan27t2v,
		"wan2.7-i2v":          wan27i2v,
		"wan2.7-r2v":          wan27r2v,
		dashscopeWan3ModelKey: wan3,
	}
}

// dashscopeIsWan3 识别 wan3.0 系列：它与其余型号在请求形态上有三处结构差异（单模型通吃三条路径、
// 音轨由请求参数控制而非恒开、可走独立 maas 域名），均按型号名分派。
func dashscopeIsWan3(model string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(model)), dashscopeWan3ModelKey)
}

// dashscopeProfileForModel 按 model_id 解析能力档：先精确命中，再容忍代理中转的前后缀装饰。
//
// infer_endpoint 用子串（"happyhorse" / "wan2."）路由到 dashscope-async-video，故此处也须子串容忍，
// 否则 "proxy/happyhorse-1.0-r2v" / "wan2.7-r2v-0715" 这类装饰名会退回默认档、丢掉 r2v 的
// max_reference_images。仅带系列名而无变体后缀（如裸 "happyhorse"）无法判别，按设计回落通用默认。
func dashscopeProfileForModel(model string) provider.VideoCapabilities {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return provider.DefaultVideoCapabilities()
	}
	if caps, ok := dashscopeVideoProfiles[normalized]; ok {
		return caps
	}
	for known, caps := range dashscopeVideoProfiles {
		if strings.Contains(normalized, known) {
			return caps
		}
	}
	return provider.DefaultVideoCapabilities()
}

// dashscopeVideoBackend 是阿里百炼视频后端（异步 video-synthesis 端点）。
type dashscopeVideoBackend struct {
	opts        provider.BackendOptions
	apiKey      string
	baseURL     string
	wan3BaseURL string
	model       string
	client      *httpc.Client
}

// newDashscopeVideoBackend 按通用信封构造；model / base_url 留空时由本后端兜底，wan3_base_url 从
// opts.Extra 读取（未配置时回落通用 base_url）。
func newDashscopeVideoBackend(opts provider.BackendOptions) (*dashscopeVideoBackend, error) {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = dashscopeVideoDefaultModel
	}
	// wan3.0 专用 maas 域名含地域与 workspace，推不出也归一化不了，故按用户填写的完整 URL 原样使用
	// （仅去掉尾部斜杠）。
	wan3 := strings.TrimRight(strings.TrimSpace(opts.Extra["wan3_base_url"]), "/")
	return &dashscopeVideoBackend{
		opts:        opts,
		apiKey:      opts.APIKey,
		baseURL:     dashscopeNativeBaseURL(opts.BaseURL),
		wan3BaseURL: wan3,
		model:       model,
		client:      httpc.NewClient(dashscopeVideoRequestTimeout),
	}, nil
}

// Name 返回供应商标识。
func (b *dashscopeVideoBackend) Name() string { return dashscopeProviderName }

// Model 返回实际下发的模型名。
func (b *dashscopeVideoBackend) Model() string { return b.model }

// VideoCapabilities 返回本实例的能力声明。
func (b *dashscopeVideoBackend) VideoCapabilities() provider.VideoCapabilities {
	return dashscopeProfileForModel(b.model)
}

// requestBaseURL 返回本型号请求实际走的域名：wan3.0 且配置了独立域名时用 maas 域名，否则通用域名。
//
// 提交与轮询共用同一个：任务 id 只在创建它的 endpoint 上可查，两者分家会让 wan3.0 任务提交成功后
// 轮询到 404。
func (b *dashscopeVideoBackend) requestBaseURL() string {
	if b.wan3BaseURL != "" && dashscopeIsWan3(b.model) {
		return b.wan3BaseURL
	}
	return b.baseURL
}

// Generate 组装 payload、创建任务、落地 job_id、轮询至终态并下载成片。
func (b *dashscopeVideoBackend) Generate(
	ctx context.Context, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	payload, err := b.buildPayload(req)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	slog.Info("调用 DashScope 视频 API", "model", b.model)

	base := b.requestBaseURL()
	taskID, err := b.createTask(ctx, base, payload)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	slog.Info("DashScope 视频任务已创建", "task_id", taskID, "model", b.model)
	// 一并写回实际提交域名（wan3.0 走独立 maas 域名）：续跑据此回放原域名，不然改配置后轮询会打到
	// 查不到该任务的主机。
	if err := PersistProviderJobIDFor(ctx, b.opts, req, taskID, base); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	return b.pollAndBuild(ctx, taskID, req, false)
}

// ResumeVideo 接续已 submit 的 DashScope task：仅 poll + 下载，绝不重新 submit。
func (b *dashscopeVideoBackend) ResumeVideo(
	ctx context.Context, jobID string, req provider.VideoGenerationRequest,
) (provider.VideoGenerationResult, error) {
	return b.pollAndBuild(ctx, jobID, req, true)
}

// createTask 提交视频合成请求并提取 task_id（非幂等「建任务 + 计费」POST，按提交谓词重试）。
func (b *dashscopeVideoBackend) createTask(
	ctx context.Context, base string, payload map[string]any,
) (string, error) {
	var taskID string
	err := provider.RunWithRetry(ctx, provider.DefaultRetryPolicy, provider.SubmitRetryable,
		func(ctx context.Context) error {
			resp, err := SubmitPost(ctx, dashscopeProviderName, func(ctx context.Context) (*httpc.Response, error) {
				return b.client.PostJSON(ctx, base+dashscopeVideoEndpoint, dashscopeHeaders(b.apiKey, true), payload)
			})
			if err != nil {
				return err
			}
			id, err := dashscopeExtractTaskID(resp.Body)
			if err != nil {
				return err
			}
			taskID = id
			return nil
		})
	if err != nil {
		return "", err
	}
	return taskID, nil
}

// pollAndBuild 轮询至终态、判过期、下载成片并按 usage.duration 计费。
func (b *dashscopeVideoBackend) pollAndBuild(
	ctx context.Context, taskID string, req provider.VideoGenerationRequest, isResume bool,
) (provider.VideoGenerationResult, error) {
	// 续跑轮询回放提交时的域名：任务 id 只在创建它的 endpoint 上可查，用户在途改 base_url 后按当下
	// 配置解析出的新域名去轮旧任务会 404，被下方的 404 分支误判成过期。
	base := req.SubmittedBaseURL
	if base == "" {
		base = b.requestBaseURL()
	}

	pollFn := func(ctx context.Context) (map[string]any, error) {
		resp, err := b.client.Get(ctx, base+"/tasks/"+taskID, dashscopeHeaders(b.apiKey, false))
		if err != nil {
			// resume 路径下 404（task 完全不存在）直接转 ResumeExpiredError，不走重试。
			if isResume {
				var statusErr *provider.HTTPStatusError
				if errors.As(err, &statusErr) && statusErr.StatusCode == 404 {
					return nil, &provider.ResumeExpiredError{JobID: taskID, Provider: dashscopeProviderName}
				}
			}
			return nil, err
		}
		var payload map[string]any
		if err := decodeJSON(resp.Body, &payload); err != nil {
			return nil, err
		}
		return payload, nil
	}

	final, err := PollWithRetry(ctx, PollConfig[map[string]any]{
		PollFn:       pollFn,
		IsDone:       dashscopeIsTerminal,
		IsFailed:     dashscopeFailureReason,
		PollInterval: dashscopeVideoPollInterval,
		MaxWait:      dashscopeMaxWait(req.DurationSeconds),
		RetryIf:      provider.PollRetryable,
		Label:        "DashScope",
	})
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}

	// task_id 24h 过期表现为 200 + task_status=UNKNOWN。
	if dashscopeIsExpired(final) {
		if isResume {
			return provider.VideoGenerationResult{}, &provider.ResumeExpiredError{
				JobID:    taskID,
				Provider: dashscopeProviderName,
				Message:  fmt.Sprintf("DashScope task expired: %s", taskID),
			}
		}
		return provider.VideoGenerationResult{}, fmt.Errorf("DashScope task expired during generate: %s", taskID)
	}

	videoURL, err := dashscopeExtractVideoURL(final)
	if err != nil {
		return provider.VideoGenerationResult{}, err
	}
	if err := DownloadVideo(ctx, videoURL, req.OutputPath, 0); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	slog.Info("DashScope 视频下载完成", "output", req.OutputPath)

	// usage.duration 是真实计费时长（wan2.7-r2v 含输入视频时长），缺失回落请求时长。
	duration := req.DurationSeconds
	if billing := dashscopeExtractBillingDuration(final); billing != nil {
		duration = *billing
	}
	generateAudio := req.GenerateAudio
	return provider.VideoGenerationResult{
		VideoPath:       req.OutputPath,
		Provider:        dashscopeProviderName,
		Model:           b.model,
		DurationSeconds: duration,
		VideoURI:        videoURL,
		TaskID:          taskID,
		GenerateAudio:   &generateAudio,
	}, nil
}

// buildPayload 组装 submit 请求体。
func (b *dashscopeVideoBackend) buildPayload(
	req provider.VideoGenerationRequest,
) (map[string]any, error) {
	media, err := b.buildMedia(req)
	if err != nil {
		return nil, err
	}
	input := map[string]any{"prompt": req.Prompt}
	if len(media) > 0 {
		input["media"] = media
	}

	resolution := req.Resolution
	if resolution == "" {
		resolution = "720p"
	}
	parameters := map[string]any{
		"resolution": strings.ToUpper(resolution),
		"duration":   req.DurationSeconds,
		// HappyHorse 默认带 "Happy Horse" 水印，显式关闭。
		"watermark": false,
	}
	// ratio 仅在无首帧时下传：图生视频 / 带首帧的参考生视频按首帧定宽高比，上游会忽略 ratio
	// （wan2.7「传 first_frame 时自动忽略」），HappyHorse 图生视频更直接把 ratio 当非法参数拒绝。
	hasFirstFrame := false
	for _, m := range media {
		if m["type"] == "first_frame" {
			hasFirstFrame = true
			break
		}
	}
	if req.AspectRatio != "" && !hasFirstFrame {
		parameters["ratio"] = req.AspectRatio
	}
	if req.Seed != nil {
		parameters["seed"] = *req.Seed
	}
	// 音轨开关只对 wan3.0 下发：其余型号恒有声（registry 侧 audio_always_on），下发会被上游拒绝。
	if dashscopeIsWan3(b.model) {
		parameters["audio"] = req.GenerateAudio
	}

	return map[string]any{"model": b.model, "input": input, "parameters": parameters}, nil
}

// buildMedia 组装 media 数组：首帧 / 尾帧 + 参考图 + 参考音频条目。
func (b *dashscopeVideoBackend) buildMedia(
	req provider.VideoGenerationRequest,
) ([]map[string]any, error) {
	caps := dashscopeProfileForModel(b.model)
	media := make([]map[string]any, 0, 2)

	if caps.FirstFrame && req.StartImage != "" {
		// fail-loud：声明了首帧图却缺失或读取失败即中止，不静默忽略——否则用户拿到一个没用上首帧
		// 的结果却不知情。
		uri, ok := dashscopeReadImageURI(req.StartImage)
		if !ok {
			return nil, provider.NewVideoCapabilityError(
				provider.FailureVideoStartImageUnreadable,
				map[string]any{"model": b.model, "name": filepath.Base(req.StartImage)})
		}
		media = append(media, map[string]any{"type": "first_frame", "url": uri})
	}
	if caps.LastFrame && req.EndImage != "" {
		uri, ok := dashscopeReadImageURI(req.EndImage)
		if !ok {
			return nil, provider.NewVideoCapabilityError(
				provider.FailureVideoEndImageUnreadable,
				map[string]any{"model": b.model, "name": filepath.Base(req.EndImage)})
		}
		media = append(media, map[string]any{"type": "last_frame", "url": uri})
	}

	referenceItems := make([]map[string]any, 0)
	if caps.MaxReferenceImages > 0 {
		provided := make([]string, 0, len(req.ReferenceImages))
		for _, r := range req.ReferenceImages {
			if r != "" {
				provided = append(provided, r)
			}
		}
		// r2v 必须有参考图。fail-loud：未提供（非 wan3.0）→ required；任一声明的参考图缺失 / 不可读
		// → 报错列出文件名中止，不静默退化为无参考 / 子集生成。
		if len(provided) == 0 && !dashscopeIsWan3(b.model) {
			return nil, provider.NewVideoCapabilityError(
				provider.FailureVideoReferenceImagesRequired, map[string]any{"model": b.model})
		}
		dataURIs := make([]string, 0, len(provided))
		unreadable := make([]string, 0)
		for _, r := range provided {
			uri, ok := dashscopeReadImageURI(r)
			if !ok {
				unreadable = append(unreadable, filepath.Base(r))
			} else {
				dataURIs = append(dataURIs, uri)
			}
		}
		if len(unreadable) > 0 {
			return nil, provider.NewVideoCapabilityError(
				provider.FailureVideoReferenceImagesUnreadable,
				map[string]any{"model": b.model, "names": strings.Join(unreadable, ", ")})
		}
		if len(dataURIs) > caps.MaxReferenceImages {
			slog.Warn("DashScope 参考图数量超过上限，截断",
				"count", len(dataURIs), "model", b.model, "limit", caps.MaxReferenceImages)
			dataURIs = dataURIs[:caps.MaxReferenceImages]
		}
		for _, uri := range dataURIs {
			referenceItems = append(referenceItems, map[string]any{"type": "reference_image", "url": uri})
		}
	}

	// 音频判定在参考素材循环之外：无参考素材可挂时也要走一遍，否则无参考图能力的 model 收到音频会
	// 静默丢弃、照常扣费。两种挂载形态：per_image 的挂在参考素材项上（wan2.7-r2v），其余走 media
	// 数组里的独立 reference_audio 条目（wan3.0）。
	standaloneAudio := make([]map[string]any, 0)
	if caps.ReferenceAudioPerImage {
		if err := b.attachReferenceVoices(referenceItems, req); err != nil {
			return nil, err
		}
	} else {
		items, err := b.buildReferenceAudioItems(req)
		if err != nil {
			return nil, err
		}
		standaloneAudio = items
	}
	media = append(media, referenceItems...)
	media = append(media, standaloneAudio...)
	return media, nil
}

// buildReferenceAudioItems 把参考音频转成 media 数组里的独立 reference_audio 条目（wan3.0 形态）。
func (b *dashscopeVideoBackend) buildReferenceAudioItems(
	req provider.VideoGenerationRequest,
) ([]map[string]any, error) {
	uris, err := b.decodeReferenceAudioURIs(req)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(uris))
	for _, uri := range uris {
		items = append(items, map[string]any{"type": "reference_audio", "url": uri})
	}
	return items, nil
}

// decodeReferenceAudioURIs 把请求里的参考音频逐段读成 data URI，顺序与入参一一对应。
//
// 能力档不支持音频、扩展名不在受支持格式内、任一段读不出来，都在此 fail-loud。不跳过任何一段——
// 顺序即 prompt 中「音频N」的指认契约，静默少发一段会让该角色的音色声明无声失效且照常计费。
func (b *dashscopeVideoBackend) decodeReferenceAudioURIs(
	req provider.VideoGenerationRequest,
) ([]string, error) {
	audioFiles := req.ReferenceAudioFiles
	if len(audioFiles) == 0 {
		return []string{}, nil
	}
	caps := dashscopeProfileForModel(b.model)
	if caps.ReferenceAudioMode == provider.ReferenceAudioNone {
		return nil, provider.NewVideoCapabilityError(
			provider.FailureVideoReferenceAudioUnsupported,
			map[string]any{"provider": dashscopeProviderName, "model": b.model})
	}
	uris := make([]string, 0, len(audioFiles))
	unreadable := make([]string, 0)
	for _, audio := range audioFiles {
		suffix := strings.ToLower(filepath.Ext(audio))
		mime, ok := dashscopeReferenceAudioMIMETypes[suffix]
		if !ok {
			return nil, provider.NewVideoCapabilityError(
				provider.FailureVideoReferenceAudioFormatUnsupported,
				map[string]any{"name": filepath.Base(audio), "supported": strings.Join(dashscopeSortedAudioExtensions(), ", ")})
		}
		uri, ok := dashscopeReadAudioURI(audio, mime)
		if !ok {
			unreadable = append(unreadable, filepath.Base(audio))
		} else {
			uris = append(uris, uri)
		}
	}
	if len(unreadable) > 0 {
		return nil, provider.NewVideoCapabilityError(
			provider.FailureVideoReferenceAudioUnreadable,
			map[string]any{"model": b.model, "names": strings.Join(unreadable, ", ")})
	}
	return uris, nil
}

// attachReferenceVoices 把参考音频逐段挂到参考素材项的 reference_voice 字段上（就地修改）。
//
// 对齐优先用 req.ReferenceAudioTargets（第 i 段音频对应 referenceItems 的哪个下标）。targets 为 nil
// 时回退按位置对齐。音频段数多于可挂载的参考素材、或 targets 携带越界 / 重复下标时硬失败而非丢弃
// 多余段——丢弃会让某个角色的音色声明无声失效，用户直到成片才发现且已照常扣费。
func (b *dashscopeVideoBackend) attachReferenceVoices(
	referenceItems []map[string]any, req provider.VideoGenerationRequest,
) error {
	audioFiles := req.ReferenceAudioFiles
	if len(audioFiles) == 0 {
		return nil
	}
	caps := dashscopeProfileForModel(b.model)
	if caps.ReferenceAudioMode == provider.ReferenceAudioNone {
		return provider.NewVideoCapabilityError(
			provider.FailureVideoReferenceAudioUnsupported,
			map[string]any{"provider": dashscopeProviderName, "model": b.model})
	}

	var valid bool
	if req.ReferenceAudioTargets != nil {
		targets := req.ReferenceAudioTargets
		valid = len(targets) == len(audioFiles) && dashscopeTargetsUniqueInRange(targets, len(referenceItems))
	} else {
		valid = len(audioFiles) <= len(referenceItems)
	}
	if !valid {
		// 上限是该请求实际有几个可挂载的参考素材（加参考图也能过），与 gate 的
		// video_reference_audio_exceeded（模型能力上限）分成两个 code。
		return provider.NewVideoCapabilityError(
			provider.FailureVideoReferenceAudioSlotsInsufficient,
			map[string]any{
				"provider": dashscopeProviderName,
				"model":    b.model,
				"slots":    len(referenceItems),
				"count":    len(audioFiles),
			})
	}
	// 解码放在槽位校验之后：槽位不足比某段文件读不出来更靠前地说明卡点。
	uris, err := b.decodeReferenceAudioURIs(req)
	if err != nil {
		return err
	}
	if req.ReferenceAudioTargets != nil {
		for i, idx := range req.ReferenceAudioTargets {
			referenceItems[idx]["reference_voice"] = uris[i]
		}
	} else {
		for i, uri := range uris {
			referenceItems[i]["reference_voice"] = uri
		}
	}
	return nil
}

// dashscopeTargetsUniqueInRange 判断 targets 是否互不重复且全部落在 [0, n) 内。
func dashscopeTargetsUniqueInRange(targets []int, n int) bool {
	seen := make(map[int]struct{}, len(targets))
	for _, t := range targets {
		if t < 0 || t >= n {
			return false
		}
		if _, dup := seen[t]; dup {
			return false
		}
		seen[t] = struct{}{}
	}
	return true
}

// dashscopeReadAudioURI 把参考音频读成 base64 data URI；文件缺失或读取失败返回 ok=false。
func dashscopeReadAudioURI(path, mime string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("DashScope 参考音频读取失败", "path", path, "error", err)
		return "", false
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), true
}

// dashscopeSortedAudioExtensions 返回受支持音频扩展名的字典序切片（对齐 Python sorted）。
func dashscopeSortedAudioExtensions() []string {
	exts := make([]string, 0, len(dashscopeReferenceAudioMIMETypes))
	for ext := range dashscopeReferenceAudioMIMETypes {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	return exts
}

// dashscopeMaxWait 返回轮询最大等待时长：max(900s, duration*60s)。
func dashscopeMaxWait(durationSeconds int) time.Duration {
	seconds := float64(durationSeconds) * dashscopeVideoPollTimeoutSecond
	if seconds < float64(dashscopeVideoMinPollTimeout)/float64(time.Second) {
		seconds = float64(dashscopeVideoMinPollTimeout) / float64(time.Second)
	}
	return time.Duration(seconds) * time.Second
}

// dashscopeExtractTaskID 从提交响应提取 output.task_id，缺失时报错。
func dashscopeExtractTaskID(body []byte) (string, error) {
	var payload map[string]any
	if err := decodeJSON(body, &payload); err != nil {
		return "", err
	}
	output, _ := payload["output"].(map[string]any)
	taskID, _ := output["task_id"].(string)
	if taskID == "" {
		if reason := dashscopeFailureReason(payload); reason != "" {
			return "", errors.New(reason)
		}
		return "", fmt.Errorf("DashScope 提交响应缺少 task_id: %v", payload)
	}
	return taskID, nil
}

// dashscopeExtractVideoURL 从 SUCCEEDED 轮询响应提取 output.video_url。
func dashscopeExtractVideoURL(payload map[string]any) (string, error) {
	output, _ := payload["output"].(map[string]any)
	url, _ := output["video_url"].(string)
	if url == "" {
		return "", fmt.Errorf("DashScope 任务完成但缺少 video_url: %v", payload)
	}
	return url, nil
}

// dashscopeExtractBillingDuration 从 usage.duration 取真实计费时长（wan2.7-r2v 含输入视频时长）。
//
// 容忍数字类型与数字字符串；按 half-up 取整（4.5→5）避免少计费秒数。非正值（0 / 负 / 无法解析）
// 与超出上限（24h，防超大数值写入 DB Integer 列溢出）一律回 nil，由 caller 回落请求时长。
func dashscopeExtractBillingDuration(payload map[string]any) *int {
	usage, _ := payload["usage"].(map[string]any)
	raw, has := usage["duration"]
	if !has || raw == nil {
		return nil
	}
	value, ok := dashscopeNumericValue(raw)
	if !ok {
		return nil
	}
	// 上限按取整前的原始数值判断：86400.4 已超 24h，不得因 half-up 落回上限内被接受。
	// NaN 参与比较恒为 false，与解析失败同路径回 nil。
	if !(value > 0 && value <= float64(dashscopeMaxBilledDurationSeconds)) {
		return nil
	}
	rounded := int(math.Floor(value + 0.5))
	if rounded <= 0 {
		return nil
	}
	return &rounded
}

// dashscopeNumericValue 把 usage.duration 归一化为浮点：接受 JSON 数字、json.Number 与数字字符串。
func dashscopeNumericValue(raw any) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}

func init() {
	provider.RegisterVideoBackend(dashscopeProviderName, provider.VideoBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.VideoBackend, error) {
			return newDashscopeVideoBackend(opts)
		},
		CapabilitiesForModel: func(modelID string) (provider.VideoCapabilities, error) {
			return dashscopeProfileForModel(modelID), nil
		},
	})
}
