// 系统配置（非供应商专项）：默认后端、旁白、Agent 文本模型档位与版本检查。
// 对应 server/routers/system_config.py。供应商专项（密钥 / 限速 / 连接测试）在 provider_admin。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

// SystemConfigService 承载 /system/config* 与 /system/version 的服务面。
type SystemConfigService struct {
	db     *gorm.DB
	config *config.ConfigService
	admin  *ProviderAdminService
}

// NewSystemConfigService 构造服务。
func NewSystemConfigService(gdb *gorm.DB, cfg *config.ConfigService) *SystemConfigService {
	return &SystemConfigService{db: gdb, config: cfg, admin: NewProviderAdminService(gdb)}
}

// modelCandidate 是一个可选模型及其能力桶归属（Python _ModelCandidate）。
type modelCandidate struct {
	option    string // "provider_id/model_id"
	mediaType provider.MediaType
	buckets   map[string]bool
}

// enumerateCandidates 列出 ready 且启用的内置供应商模型（hidden 剔除）并附能力桶。
//
// 自定义供应商模型按范围收敛暂不参与（custom_providers 面未迁）：Python 侧这块是
// non-fatal 的 try/except，缺失只影响下拉多几条，不影响端点可用性。
func (s *SystemConfigService) enumerateCandidates(ctx context.Context) ([]modelCandidate, map[string]string, error) {
	statuses, err := s.admin.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	ready := map[string]bool{}
	for _, view := range statuses {
		if view.Status == "ready" && view.Enabled {
			ready[view.ID] = true
		}
	}
	providerNames := map[string]string{}
	candidates := make([]modelCandidate, 0, 32)
	for _, providerID := range provider.ProviderIDs() {
		if !ready[providerID] {
			continue
		}
		meta, ok := provider.ProviderMetaFor(providerID)
		if !ok {
			continue
		}
		for _, entry := range meta.Models {
			if entry.Info.Hidden {
				continue
			}
			candidates = append(candidates, modelCandidate{
				option:    providerID + "/" + entry.ID,
				mediaType: entry.Info.MediaType,
				buckets:   builtinModelBuckets(providerID, entry.ID, entry.Info),
			})
		}
	}
	// 响应里 key 顺序无关，但测试与 diff 需要确定性：排序。
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].option < candidates[j].option })
	return candidates, providerNames, nil
}

// builtinModelBuckets 把能力声明翻译成桶（t2i / i2i / i2v / r2v），不新增第二份声明。
// 文本 / 音频模型恒为空集；视频桶由 backend 的 VideoCapabilities 推导。
func builtinModelBuckets(providerID, modelID string, info provider.ModelInfo) map[string]bool {
	buckets := map[string]bool{}
	switch info.MediaType {
	case provider.MediaImage:
		for _, cap := range info.Capabilities {
			switch cap {
			case provider.ModelCapTextToImage:
				buckets["t2i"] = true
			case provider.ModelCapImageToImage:
				buckets["i2i"] = true
			}
		}
	case provider.MediaVideo:
		spec, ok := provider.ProviderSpecFor(providerID, provider.MediaVideo)
		if !ok {
			return buckets
		}
		caps, err := provider.VideoCapabilitiesForModel(spec.RegistryBackend, modelID)
		if err != nil {
			return buckets
		}
		for _, bucket := range provider.SatisfiedVideoBuckets(caps) {
			buckets[string(bucket)] = true
		}
	}
	return buckets
}

// Options 是 GET /system/config 的 options 块。
type Options struct {
	VideoBackends []string          `json:"video_backends"`
	ImageBackends []string          `json:"image_backends"`
	TextBackends  []string          `json:"text_backends"`
	AudioBackends []string          `json:"audio_backends"`
	ProviderNames map[string]string `json:"provider_names"`
}

// mediaOptionKey 是 media_type → options 键。
var mediaOptionKey = map[provider.MediaType]string{
	provider.MediaVideo: "video_backends",
	provider.MediaImage: "image_backends",
	provider.MediaText:  "text_backends",
	provider.MediaAudio: "audio_backends",
}

// BuildOptions 从 ready 供应商推导可选后端。
func (s *SystemConfigService) BuildOptions(ctx context.Context) (Options, error) {
	candidates, providerNames, err := s.enumerateCandidates(ctx)
	if err != nil {
		return Options{}, err
	}
	byMedia := map[string][]string{
		"video_backends": {}, "image_backends": {}, "text_backends": {}, "audio_backends": {},
	}
	for _, candidate := range candidates {
		key, ok := mediaOptionKey[candidate.mediaType]
		if ok {
			byMedia[key] = append(byMedia[key], candidate.option)
		}
	}
	return Options{
		VideoBackends: byMedia["video_backends"],
		ImageBackends: byMedia["image_backends"],
		TextBackends:  byMedia["text_backends"],
		AudioBackends: byMedia["audio_backends"],
		ProviderNames: providerNames,
	}, nil
}

// MediaCandidates 的 JSON 形状在 BuildModelCandidates 里直接以 map 组装
// （桶键是动态的 t2i/i2i/i2v/r2v，不必引入中间结构体）。

// BuildModelCandidates 组装能力桶下拉的数据源。
//
// 默认层不过滤（默认层不承诺能力，能力不满足由解析闸报错兜底）；只有桶层承诺
// 「配进去的组合执行得了」，故按桶过滤。
func (s *SystemConfigService) BuildModelCandidates(ctx context.Context) (map[string]any, error) {
	candidates, providerNames, err := s.enumerateCandidates(ctx)
	if err != nil {
		return nil, err
	}
	mediaBuckets := map[string][]string{
		"image": {"t2i", "i2i"},
		"video": {"i2v", "r2v"},
	}
	mediaTypes := map[string]provider.MediaType{"image": provider.MediaImage, "video": provider.MediaVideo}
	out := map[string]any{"provider_names": providerNames}
	for mediaName, buckets := range mediaBuckets {
		var sameMedia []modelCandidate
		for _, candidate := range candidates {
			if candidate.mediaType == mediaTypes[mediaName] {
				sameMedia = append(sameMedia, candidate)
			}
		}
		defaults := make([]string, 0, len(sameMedia))
		for _, candidate := range sameMedia {
			defaults = append(defaults, candidate.option)
		}
		bucketMap := map[string]any{}
		for _, bucket := range buckets {
			options := []string{}
			for _, candidate := range sameMedia {
				if candidate.buckets[bucket] {
					options = append(options, candidate.option)
				}
			}
			bucketMap[bucket] = options
		}
		out[mediaName] = map[string]any{"default": defaults, "buckets": bucketMap}
	}
	return out, nil
}

// GetConfig 读全部设置并派生 GET /system/config 响应。
func (s *SystemConfigService) GetConfig(ctx context.Context) (map[string]any, error) {
	all, err := s.config.GetAllSettings(ctx)
	if err != nil {
		return nil, err
	}
	anthropicKey := all["anthropic_api_key"]
	// 兼容新凭证目录：旧 system_settings 没填但 agent_anthropic_credentials 有 active
	// 时也算 is_set，避免 dashboard「未配置」红点误报。
	if anthropicKey == "" {
		var cred repository.AgentAnthropicCredential
		if err := s.db.WithContext(ctx).
			Where("is_active = ?", true).Order("updated_at DESC").First(&cred).Error; err == nil {
			anthropicKey = cred.APIKey
		}
	}
	narrationSpeed := parseNarrationSpeed(all["narration_speed"])
	videoGenerateAudio := defaultVideoGenerateAudio
	if raw := all["video_generate_audio"]; raw != "" {
		videoGenerateAudio = strings.EqualFold(raw, "true") || raw == "1" || strings.EqualFold(raw, "yes")
	}
	settings := map[string]any{
		"default_video_backend":               all["default_video_backend"],
		"default_video_backend_i2v":           all["default_video_backend_i2v"],
		"default_video_backend_r2v":           all["default_video_backend_r2v"],
		"default_image_backend":               all["default_image_backend"],
		"default_image_backend_t2i":           all["default_image_backend_t2i"],
		"default_image_backend_i2i":           all["default_image_backend_i2i"],
		"default_text_backend":                all["default_text_backend"],
		"default_audio_backend":               all["default_audio_backend"],
		"narration_voice":                     all["narration_voice"],
		"narration_speed":                     narrationSpeed,
		"video_generate_audio":                videoGenerateAudio,
		"anthropic_api_key":                   anthropicKeyView(anthropicKey),
		"anthropic_base_url":                  nullIfEmpty(all["anthropic_base_url"]),
		"anthropic_model":                     nullIfEmpty(all["anthropic_model"]),
		"anthropic_default_haiku_model":       nullIfEmpty(all["anthropic_default_haiku_model"]),
		"anthropic_default_opus_model":        nullIfEmpty(all["anthropic_default_opus_model"]),
		"anthropic_default_sonnet_model":      nullIfEmpty(all["anthropic_default_sonnet_model"]),
		"claude_code_subagent_model":          nullIfEmpty(all["claude_code_subagent_model"]),
		"agent_session_cleanup_delay_seconds": intSetting(all["agent_session_cleanup_delay_seconds"], 300),
		"agent_max_concurrent_sessions":       intSetting(all["agent_max_concurrent_sessions"], 5),
		"text_backend_simple":                 all["text_backend_simple"],
		"text_backend_complex":                all["text_backend_complex"],
	}
	options, err := s.BuildOptions(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"settings": settings, "options": options}, nil
}

// ConfigPatchInput 是 PATCH /system/config 入参；nil 字段 = 未提供（Python model_fields_set）。
type ConfigPatchInput struct {
	Fields map[string]json.RawMessage
}

// backendKeys 是需要校验后端引用的字符串设置。
var backendKeys = []string{
	"default_video_backend", "default_video_backend_i2v", "default_video_backend_r2v",
	"default_image_backend", "default_image_backend_t2i", "default_image_backend_i2i",
	"default_text_backend", "default_audio_backend", "text_backend_simple", "text_backend_complex",
}

// stringSettings 直接映射 DB 字符串设置（anthropic_api_key 单独处理，不在此列）。
var stringSettings = []string{
	"anthropic_base_url", "anthropic_model", "anthropic_default_haiku_model",
	"anthropic_default_opus_model", "anthropic_default_sonnet_model", "claude_code_subagent_model",
}

// ConfigValidationError 是 PATCH 的入参错误（422 / 400）。
type ConfigValidationError struct {
	Status int
	Msg    string
}

func (e *ConfigValidationError) Error() string { return e.Msg }

// PatchConfig 校验并写入设置；返回更新后的完整配置。
func (s *SystemConfigService) PatchConfig(ctx context.Context, input ConfigPatchInput) (map[string]any, error) {
	patch := input.Fields
	// 后端引用：空串 = 自动解析。
	for _, key := range backendKeys {
		raw, ok := patch[key]
		if !ok {
			continue
		}
		var value *string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, &ConfigValidationError{Status: 422, Msg: key + " 应为字符串或 null"}
		}
		text := ""
		if value != nil {
			text = strings.TrimSpace(*value)
		}
		if err := s.config.SetSetting(ctx, key, text); err != nil {
			return nil, err
		}
	}
	if raw, ok := patch["narration_voice"]; ok {
		var value *string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, &ConfigValidationError{Status: 422, Msg: "narration_voice 应为字符串或 null"}
		}
		text := ""
		if value != nil {
			text = strings.TrimSpace(*value)
		}
		if err := s.config.SetSetting(ctx, "narration_voice", text); err != nil {
			return nil, err
		}
	}
	if raw, ok := patch["narration_speed"]; ok {
		var value *float64
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, &ConfigValidationError{Status: 422, Msg: "narration_speed 应为数字或 null"}
		}
		if value == nil {
			if err := s.config.SetSetting(ctx, "narration_speed", ""); err != nil {
				return nil, err
			}
		} else {
			speed := *value
			// 只做正有限数卫生校验；具体取值范围由各供应商自行约束。
			// NaN 不等于自身、Inf 大于 MaxFloat64，两者一并拒绝。
			if speed != speed || speed > math.MaxFloat64 || speed <= 0 {
				return nil, &ConfigValidationError{Status: 422, Msg: "narration_speed_must_be_positive"}
			}
			if err := s.config.SetSetting(ctx, "narration_speed", formatSpeedFloat(speed)); err != nil {
				return nil, err
			}
		}
	}
	if raw, ok := patch["video_generate_audio"]; ok {
		var value *bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, &ConfigValidationError{Status: 422, Msg: "video_generate_audio 应为布尔"}
		}
		if value != nil {
			text := "false"
			if *value {
				text = "true"
			}
			if err := s.config.SetSetting(ctx, "video_generate_audio", text); err != nil {
				return nil, err
			}
		}
	}
	if raw, ok := patch["anthropic_api_key"]; ok {
		var value *string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, &ConfigValidationError{Status: 422, Msg: "anthropic_api_key 应为字符串或 null"}
		}
		text := ""
		if value != nil {
			text = strings.TrimSpace(*value)
		}
		if err := s.config.SetSetting(ctx, "anthropic_api_key", text); err != nil {
			return nil, err
		}
	}
	// 整数设置的范围校验（Python _INT_SETTINGS_RANGES）。
	intRanges := map[string][2]int{
		"agent_session_cleanup_delay_seconds": {10, 3600},
		"agent_max_concurrent_sessions":       {1, 20},
	}
	for key, bounds := range intRanges {
		raw, ok := patch[key]
		if !ok {
			continue
		}
		var value *int
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, &ConfigValidationError{Status: 422, Msg: key + " 应为整数或 null"}
		}
		if value == nil {
			continue
		}
		if *value < bounds[0] || *value > bounds[1] {
			return nil, &ConfigValidationError{
				Status: 422, Msg: fmt.Sprintf("%s 应在 %d-%d 之间", key, bounds[0], bounds[1]),
			}
		}
		if err := s.config.SetSetting(ctx, key, strconv.Itoa(*value)); err != nil {
			return nil, err
		}
	}
	for _, key := range stringSettings {
		raw, ok := patch[key]
		if !ok {
			continue
		}
		var value *string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, &ConfigValidationError{Status: 422, Msg: key + " 应为字符串或 null"}
		}
		text := ""
		if value != nil {
			text = strings.TrimSpace(*value)
		}
		if err := s.config.SetSetting(ctx, key, text); err != nil {
			return nil, err
		}
	}
	return s.GetConfig(ctx)
}

// --- 助手 ---

// defaultVideoGenerateAudio 与 Python ConfigResolver._DEFAULT_VIDEO_GENERATE_AUDIO 一致。
const defaultVideoGenerateAudio = true

// parseNarrationSpeed 解析语速 setting；损坏值（手工改库）按未设置处理。
func parseNarrationSpeed(raw string) any {
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil
	}
	return value
}

// anthropicKeyView 密钥只出掩码不出原文。
func anthropicKeyView(key string) map[string]any {
	if key == "" {
		return map[string]any{"is_set": false, "masked": nil}
	}
	return map[string]any{"is_set": true, "masked": maskSecret(key)}
}

// nullIfEmpty 空串转 null（Python `or None`）。
func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// intSetting 整数 setting；损坏值回落默认。
func intSetting(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

// formatSpeedFloat 与 Python str(float) 对齐的最短表示（避免与 prompt_builders 的同名助手冲突）。
func formatSpeedFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// AppVersion 是当前应用版本（Go 二进制的单一真相源；Python 侧读 pyproject.toml）。
const AppVersion = "0.13.0"

// latestReleaseCache 是 GitHub 最新 release 的 5 分钟进程内缓存。
var latestReleaseCache struct {
	expiresAt time.Time
	payload   map[string]string
	fetchedAt time.Time
}

// VersionView 是 GET /system/version 响应。
type VersionView struct {
	Current          map[string]any    `json:"current"`
	Latest           map[string]string `json:"latest"`
	HasUpdate        bool              `json:"has_update"`
	CheckedAt        string            `json:"checked_at"`
	UpdateCheckError *string           `json:"update_check_error"`
}

// GetVersion 读当前版本并尽力检查 GitHub 最新 release。
// 检查失败不视为端点失败：update_check_error 带回给前端展示。
func GetVersion(ctx context.Context) VersionView {
	now := time.Now().UTC()
	view := VersionView{
		Current:   map[string]any{"version": AppVersion},
		CheckedAt: now.Format("2006-01-02T15:04:05.999999+00:00"),
	}
	latest, fetchedAt, err := fetchLatestRelease(ctx, now)
	if err != nil {
		message := "about_update_check_failed"
		view.UpdateCheckError = &message
		return view
	}
	view.Latest = latest
	view.CheckedAt = fetchedAt.Format("2006-01-02T15:04:05.999999+00:00")
	if latestV := parseSemver(latest["version"]); latestV != "" {
		if currentV := parseSemver(AppVersion); currentV != "" && semverLess(currentV, latestV) {
			view.HasUpdate = true
		}
	}
	return view
}

// fetchLatestRelease 带缓存地取 GitHub 最新 release。
func fetchLatestRelease(ctx context.Context, now time.Time) (map[string]string, time.Time, error) {
	if now.Before(latestReleaseCache.expiresAt) && latestReleaseCache.payload != nil {
		return latestReleaseCache.payload, latestReleaseCache.fetchedAt, nil
	}
	client := &http.Client{Timeout: 15 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, gitHubEndpoint, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "Shotwise")
	response, err := client.Do(request)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, time.Time{}, fmt.Errorf("github status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, time.Time{}, err
	}
	var payload struct {
		Name        string `json:"name"`
		TagName     string `json:"tag_name"`
		Body        string `json:"body"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, time.Time{}, err
	}
	version := strings.TrimSpace(payload.Name)
	if version == "" {
		version = strings.TrimSpace(payload.TagName)
	}
	built := map[string]string{
		"version":      strings.TrimPrefix(version, "v"),
		"tag_name":     payload.TagName,
		"name":         payload.Name,
		"body":         payload.Body,
		"html_url":     payload.HTMLURL,
		"published_at": payload.PublishedAt,
	}
	latestReleaseCache.payload = built
	latestReleaseCache.fetchedAt = now
	latestReleaseCache.expiresAt = now.Add(5 * time.Minute)
	return built, now, nil
}

// parseSemver 取「主.次.补丁」数值元组（宽松：忽略 v 前缀与后缀）；非法返回空。
func parseSemver(raw string) string {
	text := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	parts := strings.SplitN(text, ".", 3)
	if len(parts) < 2 {
		return ""
	}
	nums := make([]int, 0, 3)
	for _, part := range parts {
		digits := part
		for i, r := range part {
			if r < '0' || r > '9' {
				digits = part[:i]
				break
			}
		}
		if digits == "" {
			return ""
		}
		value, err := strconv.Atoi(digits)
		if err != nil {
			return ""
		}
		nums = append(nums, value)
	}
	out := ""
	for _, value := range nums {
		out += fmt.Sprintf("%02d.", value)
	}
	return strings.TrimSuffix(out, ".")
}

// semverLess 比较规范化后的点分数字串。
func semverLess(a, b string) bool {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := 0, 0
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av != bv {
			return av < bv
		}
	}
	return false
}

// gitHubEndpoint 是最新 release 的查询地址；UA 必须带（GitHub API 要求）。
const gitHubEndpoint = "https://api.github.com/repos/Shotwise/Shotwise/releases/latest"
