// Agent 凭证与预设供应商目录（对应 server/routers/agent_config.py，收敛版）。
//
// 收敛口径（本轮范围）：预设目录只保留 anthropic 官方（sdk_type=claude）与 openai 官方
// （sdk_type=openai）两条 + __custom__ 哨兵。Python 侧还列了十几个中转站预设，本部署不需要，
// 但**响应结构保持一致**——前端按同一形状渲染，加预设只需往 catalog 里补一条。
//
// 两条不变式：
//   - 每个 (user, sdk_type) 至多一条 active，由部分唯一索引保证；切换必须「先清后置」；
//   - api_key 只进不出：读路径一律回 mask，明文只在写入方向出现。
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shotwise/shotwise/internal/service"
	"io"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
)

// AgentSDK 类型常量。
const (
	AgentSDKClaude = "claude"
	AgentSDKOpenAI = "openai"

	// CustomPresetID 是「自定义供应商」哨兵 id。
	CustomPresetID = "__custom__"
)

// AgentPreset 是预设供应商目录的一条。
type AgentPreset struct {
	ID                string   `json:"id"`
	SDKType           string   `json:"sdk_type"`
	DisplayName       string   `json:"display_name"`
	IconKey           string   `json:"icon_key"`
	MessagesURL       string   `json:"messages_url"`
	DiscoveryURL      *string  `json:"discovery_url"`
	DefaultModel      string   `json:"default_model"`
	SuggestedModels   []string `json:"suggested_models"`
	DocsURL           *string  `json:"docs_url"`
	APIKeyURL         *string  `json:"api_key_url"`
	Notes             *string  `json:"notes"`
	APIKeyPattern     *string  `json:"api_key_pattern"`
	IsRecommended     bool     `json:"is_recommended"`
	SupportsDiscovery bool     `json:"supports_discovery"`
}

// agentPresets 是本部署实际使用的预设目录。
var agentPresets = []AgentPreset{
	{
		ID:                "anthropic",
		SDKType:           AgentSDKClaude,
		DisplayName:       "Anthropic",
		IconKey:           "anthropic",
		MessagesURL:       "https://api.anthropic.com/v1/messages",
		DefaultModel:      "claude-sonnet-4-5",
		SuggestedModels:   []string{"claude-opus-4-1", "claude-sonnet-4-5", "claude-haiku-4-5"},
		IsRecommended:     true,
		APIKeyPattern:     stringPtr("^sk-ant-"),
		SupportsDiscovery: false,
	},
	{
		ID:                "openai",
		SDKType:           AgentSDKOpenAI,
		DisplayName:       "OpenAI",
		IconKey:           "openai",
		MessagesURL:       "https://api.openai.com/v1/chat/completions",
		DefaultModel:      "gpt-5",
		SuggestedModels:   []string{"gpt-5", "gpt-5-mini", "gpt-4.1"},
		IsRecommended:     true,
		APIKeyPattern:     stringPtr("^sk-"),
		SupportsDiscovery: false,
	},
}

// ListAgentPresets 返回指定 SDK 类型的预设（未指定返回全部）。
func ListAgentPresets(sdkType string) []AgentPreset {
	out := make([]AgentPreset, 0, len(agentPresets))
	for _, preset := range agentPresets {
		if sdkType != "" && preset.SDKType != sdkType {
			continue
		}
		out = append(out, preset)
	}
	return out
}

// AgentPresetByID 按 id 取预设。
func AgentPresetByID(id string) (AgentPreset, bool) {
	for _, preset := range agentPresets {
		if preset.ID == id {
			return preset, true
		}
	}
	return AgentPreset{}, false
}

// AgentCredentialService 承载 agent 凭证的读写。
type AgentCredentialService struct{ db *gorm.DB }

// NewAgentCredentialService 构造。
func NewAgentCredentialService(gdb *gorm.DB) *AgentCredentialService {
	return &AgentCredentialService{db: gdb}
}

// AgentCredentialView 是对外投影（密钥只回掩码）。
type AgentCredentialView struct {
	ID           int64  `json:"id"`
	SDKType      string `json:"sdk_type"`
	Protocol     string `json:"protocol"`
	PresetID     string `json:"preset_id"`
	DisplayName  string `json:"display_name"`
	IconKey      any    `json:"icon_key"`
	BaseURL      string `json:"base_url"`
	APIKeyMasked string `json:"api_key_masked"`
	Model        any    `json:"model"`
	HaikuModel   any    `json:"haiku_model"`
	SonnetModel  any    `json:"sonnet_model"`
	OpusModel    any    `json:"opus_model"`
	SubagentMdl  any    `json:"subagent_model"`
	ModelMap     any    `json:"model_map"`
	IsActive     bool   `json:"is_active"`
	CreatedAt    string `json:"created_at"`
}

// AgentCredentialInput 是创建 / 更新入参（nil 表示不修改）。
type AgentCredentialInput struct {
	SDKType       *string
	Protocol      *string
	PresetID      *string
	DisplayName   *string
	BaseURL       *string
	APIKey        *string
	Model         *string
	HaikuModel    *string
	SonnetModel   *string
	OpusModel     *string
	SubagentMdl   *string
	ModelMap      []map[string]any
	Activate      *bool
	ClearModelMap bool
}

// List 列出该用户的 agent 凭证（可按 sdk_type 过滤）。
func (s *AgentCredentialService) List(ctx context.Context, userID, sdkType string) ([]AgentCredentialView, error) {
	var rows []repository.AgentAnthropicCredential
	query := s.db.WithContext(ctx).Where("user_id = ?", userID)
	if sdkType != "" {
		query = query.Where("sdk_type = ?", sdkType)
	}
	if err := query.Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]AgentCredentialView, 0, len(rows))
	for _, row := range rows {
		out = append(out, agentCredentialView(row))
	}
	return out, nil
}

// Create 新增一条凭证；activate 为 nil 时「该 sdk_type 还没有活跃凭证」自动激活。
func (s *AgentCredentialService) Create(
	ctx context.Context, userID string, in AgentCredentialInput,
) (AgentCredentialView, error) {
	sdkType := ""
	if in.SDKType != nil {
		sdkType = *in.SDKType
	}
	if sdkType == "" {
		sdkType = AgentSDKClaude
	}
	if sdkType != AgentSDKClaude && sdkType != AgentSDKOpenAI {
		return AgentCredentialView{}, &service.ValueError{Msg: "invalid sdk_type"}
	}
	if in.PresetID == nil || strings.TrimSpace(*in.PresetID) == "" {
		return AgentCredentialView{}, &service.ValueError{Msg: "preset_id is required"}
	}
	if in.APIKey == nil || strings.TrimSpace(*in.APIKey) == "" {
		return AgentCredentialView{}, &service.ValueError{Msg: "api_key is required"}
	}
	if in.BaseURL == nil || strings.TrimSpace(*in.BaseURL) == "" {
		return AgentCredentialView{}, &service.ValueError{Msg: "base_url is required"}
	}

	presetID := strings.TrimSpace(*in.PresetID)
	displayName := ""
	if in.DisplayName != nil {
		displayName = strings.TrimSpace(*in.DisplayName)
	}
	if displayName == "" {
		if preset, ok := AgentPresetByID(presetID); ok {
			displayName = preset.DisplayName
		} else {
			displayName = presetID
		}
	}
	protocol := "chat_completions"
	if in.Protocol != nil && strings.TrimSpace(*in.Protocol) != "" {
		protocol = strings.TrimSpace(*in.Protocol)
	}
	if presetID == "__custom__" && sdkType == AgentSDKClaude && in.Protocol == nil {
		// 自定义 Claude 端点默认走 anthropic 协议的 messages 接口。
		protocol = "anthropic_messages"
	}

	now := db.NewFlexibleTime(time.Now())
	row := repository.AgentAnthropicCredential{
		UserID:      userID,
		SDKType:     sdkType,
		Protocol:    protocol,
		PresetID:    presetID,
		DisplayName: displayName,
		BaseURL:     strings.TrimSpace(*in.BaseURL),
		APIKey:      strings.TrimSpace(*in.APIKey),
		IsActive:    false,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	row.Model = optionalTrimmed(in.Model)
	row.HaikuModel = optionalTrimmed(in.HaikuModel)
	row.SonnetModel = optionalTrimmed(in.SonnetModel)
	row.OpusModel = optionalTrimmed(in.OpusModel)
	row.SubagentModel = optionalTrimmed(in.SubagentMdl)
	if in.ModelMap != nil {
		payload, err := json.Marshal(in.ModelMap)
		if err != nil {
			return AgentCredentialView{}, err
		}
		text := string(payload)
		row.ModelMap = &text
	}

	activate := false
	if in.Activate != nil {
		activate = *in.Activate
	} else {
		var count int64
		if err := s.db.WithContext(ctx).Model(&repository.AgentAnthropicCredential{}).
			Where("user_id = ? AND sdk_type = ? AND is_active = ?", userID, sdkType, true).
			Count(&count).Error; err != nil {
			return AgentCredentialView{}, err
		}
		activate = count == 0
	}

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if activate {
			// 先清后置：部分唯一索引不允许同 (user, sdk_type) 两条同时活跃。
			if err := clearActive(tx, userID, sdkType); err != nil {
				return err
			}
			row.IsActive = true
		}
		return tx.Create(&row).Error
	})
	if err != nil {
		return AgentCredentialView{}, err
	}
	return agentCredentialView(row), nil
}

// Update 更新凭证字段（nil 不覆盖）。
func (s *AgentCredentialService) Update(
	ctx context.Context, userID string, credID int64, in AgentCredentialInput,
) (AgentCredentialView, error) {
	row, err := s.credentialOrNotFound(ctx, userID, credID)
	if err != nil {
		return AgentCredentialView{}, err
	}
	updates := map[string]any{"updated_at": db.NewFlexibleTime(time.Now())}
	setIfPresent := func(column string, value *string) {
		if value == nil {
			return
		}
		trimmed := strings.TrimSpace(*value)
		if trimmed == "" {
			updates[column] = nil
			return
		}
		updates[column] = trimmed
	}
	if in.Protocol != nil && strings.TrimSpace(*in.Protocol) != "" {
		updates["protocol"] = strings.TrimSpace(*in.Protocol)
	}
	if in.DisplayName != nil && strings.TrimSpace(*in.DisplayName) != "" {
		updates["display_name"] = strings.TrimSpace(*in.DisplayName)
	}
	if in.BaseURL != nil && strings.TrimSpace(*in.BaseURL) != "" {
		updates["base_url"] = strings.TrimSpace(*in.BaseURL)
	}
	if in.APIKey != nil && strings.TrimSpace(*in.APIKey) != "" {
		updates["api_key"] = strings.TrimSpace(*in.APIKey)
	}
	setIfPresent("model", in.Model)
	setIfPresent("haiku_model", in.HaikuModel)
	setIfPresent("sonnet_model", in.SonnetModel)
	setIfPresent("opus_model", in.OpusModel)
	setIfPresent("subagent_model", in.SubagentMdl)
	if in.ClearModelMap {
		updates["model_map"] = nil
	} else if in.ModelMap != nil {
		payload, marshalErr := json.Marshal(in.ModelMap)
		if marshalErr != nil {
			return AgentCredentialView{}, marshalErr
		}
		updates["model_map"] = string(payload)
	}
	if err := s.db.WithContext(ctx).Model(&repository.AgentAnthropicCredential{}).
		Where("id = ?", row.ID).Updates(updates).Error; err != nil {
		return AgentCredentialView{}, err
	}
	updated, err := s.credentialOrNotFound(ctx, userID, credID)
	if err != nil {
		return AgentCredentialView{}, err
	}
	return agentCredentialView(updated), nil
}

// Delete 删除凭证；删的是活跃凭证时把最新一条提升为活跃。
func (s *AgentCredentialService) Delete(ctx context.Context, userID string, credID int64) error {
	row, err := s.credentialOrNotFound(ctx, userID, credID)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id = ?", row.ID).Delete(&repository.AgentAnthropicCredential{}).Error; err != nil {
			return err
		}
		if !row.IsActive {
			return nil
		}
		var next repository.AgentAnthropicCredential
		if err := tx.Where("user_id = ? AND sdk_type = ?", userID, row.SDKType).
			Order("id DESC").First(&next).Error; err != nil {
			return nil
		}
		return tx.Model(&repository.AgentAnthropicCredential{}).Where("id = ?", next.ID).
			Updates(map[string]any{
				"is_active":  true,
				"updated_at": db.NewFlexibleTime(time.Now()),
			}).Error
	})
}

// Activate 把凭证设为该 sdk_type 的唯一活跃凭证。
func (s *AgentCredentialService) Activate(
	ctx context.Context, userID string, credID int64,
) (AgentCredentialView, error) {
	row, err := s.credentialOrNotFound(ctx, userID, credID)
	if err != nil {
		return AgentCredentialView{}, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := clearActive(tx, userID, row.SDKType); err != nil {
			return err
		}
		return tx.Model(&repository.AgentAnthropicCredential{}).Where("id = ?", row.ID).
			Updates(map[string]any{
				"is_active":  true,
				"updated_at": db.NewFlexibleTime(time.Now()),
			}).Error
	})
	if err != nil {
		return AgentCredentialView{}, err
	}
	row.IsActive = true
	return agentCredentialView(row), nil
}

// ConnectionTest 是连接测试结果（结构对齐 Python 的 TestConnectionResponse）。
type ConnectionTest struct {
	OK         bool           `json:"ok"`
	Diagnosis  map[string]any `json:"diagnosis,omitempty"`
	Models     []string       `json:"models,omitempty"`
	LatencyMS  *int64         `json:"latency_ms,omitempty"`
	HTTPStatus *int           `json:"http_status,omitempty"`
}

// TestDraft 用入参里的端点与密钥直接试连（不落库）。
func (s *AgentCredentialService) TestDraft(
	ctx context.Context, in AgentCredentialInput,
) (ConnectionTest, error) {
	if in.BaseURL == nil || in.APIKey == nil {
		return ConnectionTest{}, &service.ValueError{Msg: "base_url and api_key are required"}
	}
	protocol := ""
	if in.Protocol != nil {
		protocol = *in.Protocol
	}
	return probeAgentEndpoint(ctx, protocol, *in.BaseURL, *in.APIKey)
}

// TestCredential 用已存的凭证试连。
func (s *AgentCredentialService) TestCredential(
	ctx context.Context, userID string, credID int64,
) (ConnectionTest, error) {
	row, err := s.credentialOrNotFound(ctx, userID, credID)
	if err != nil {
		return ConnectionTest{}, err
	}
	return probeAgentEndpoint(ctx, row.Protocol, row.BaseURL, row.APIKey)
}

// ResolvedAgentModel 是可驱动 Go 智能体运行时的活跃凭证投影。
type ResolvedAgentModel struct {
	APIKey  string
	Model   string
	BaseURL string
}

// ResolveActiveModel 取可驱动 Go 运行时的活跃凭证。
//
// Go 运行时是单一 Eino 循环，只讲 OpenAI 兼容 chat_completions 协议
// （agent/provider.go）：活跃凭证按 protocol 过滤——chat_completions 可直接驱动
// （含经 OpenAI 兼容网关接入的 Claude）；anthropic_messages 是 Claude SDK 子进程
// 协议，Go 运行时不会说，跳过。本部署为单用户鉴权，不按 user 过滤，按 id 升序取
// 第一条凭证齐全的活跃行；model 列空时回落预设默认模型。
// 无可用凭证返回 ok=false，调用方回落 env 配置（dev 部署形态）。
func (s *AgentCredentialService) ResolveActiveModel(ctx context.Context) (ResolvedAgentModel, bool, error) {
	var rows []repository.AgentAnthropicCredential
	if err := s.db.WithContext(ctx).
		Where("is_active = ? AND protocol = ?", true, "chat_completions").
		Order("id ASC").Find(&rows).Error; err != nil {
		return ResolvedAgentModel{}, false, err
	}
	for _, row := range rows {
		if strings.TrimSpace(row.BaseURL) == "" || strings.TrimSpace(row.APIKey) == "" {
			continue
		}
		model := strings.TrimSpace(service.DerefString(row.Model))
		if model == "" {
			if preset, ok := AgentPresetByID(row.PresetID); ok {
				model = preset.DefaultModel
			}
		}
		return ResolvedAgentModel{APIKey: row.APIKey, Model: model, BaseURL: row.BaseURL}, true, nil
	}
	return ResolvedAgentModel{}, false, nil
}

// ---- 内部 ----

// clearActive 清掉该 (user, sdk_type) 的活跃标记。
func clearActive(tx *gorm.DB, userID, sdkType string) error {
	return tx.Model(&repository.AgentAnthropicCredential{}).
		Where("user_id = ? AND sdk_type = ? AND is_active = ?", userID, sdkType, true).
		Updates(map[string]any{
			"is_active":  false,
			"updated_at": db.NewFlexibleTime(time.Now()),
		}).Error
}

// credentialOrNotFound 取凭证并校验归属（跨用户一律 404，不做存在性承诺）。
func (s *AgentCredentialService) credentialOrNotFound(
	ctx context.Context, userID string, credID int64,
) (repository.AgentAnthropicCredential, error) {
	var row repository.AgentAnthropicCredential
	err := s.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", credID, userID).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return repository.AgentAnthropicCredential{}, ErrAgentCredentialNotFound
		}
		return repository.AgentAnthropicCredential{}, err
	}
	return row, nil
}

// ErrAgentCredentialNotFound 表示凭证不存在。
var ErrAgentCredentialNotFound = fmt.Errorf("agent credential not found")

// agentCredentialView 把表行投影为对外结构。
func agentCredentialView(row repository.AgentAnthropicCredential) AgentCredentialView {
	view := AgentCredentialView{
		ID:           row.ID,
		SDKType:      row.SDKType,
		Protocol:     row.Protocol,
		PresetID:     row.PresetID,
		DisplayName:  row.DisplayName,
		BaseURL:      row.BaseURL,
		APIKeyMasked: maskSecret(row.APIKey),
		Model:        optionalValue(row.Model),
		HaikuModel:   optionalValue(row.HaikuModel),
		SonnetModel:  optionalValue(row.SonnetModel),
		OpusModel:    optionalValue(row.OpusModel),
		SubagentMdl:  optionalValue(row.SubagentModel),
		IsActive:     row.IsActive,
		CreatedAt:    isoFromFlexible(row.CreatedAt),
	}
	if preset, ok := AgentPresetByID(row.PresetID); ok {
		view.IconKey = preset.IconKey
	}
	if row.ModelMap != nil && strings.TrimSpace(*row.ModelMap) != "" {
		var decoded []map[string]any
		if err := json.Unmarshal([]byte(*row.ModelMap), &decoded); err == nil {
			view.ModelMap = decoded
		}
	}
	return view
}

// optionalTrimmed 把指针串去空白；空串返回 nil（列可空）。
func optionalTrimmed(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// optionalValue 把可空列转成 JSON 友好值（nil → null）。
func optionalValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// stringPtr 构造字符串指针（目录静态数据用）。
func stringPtr(value string) *string { return &value }

// probeAgentEndpoint 对 Agent 端点做一次最小连通性验证。
//
// 两种协议各用「最便宜且不产生内容」的请求：
//   - openai / chat_completions：GET {base}/models（列出模型即说明 key 与 base_url 都可用）；
//   - anthropic_messages：POST {base}/messages，带 max_tokens=1 的最小消息——
//     Anthropic 不提供 GET /models，只能真发一次最小请求；401/403 归「鉴权失败」，
//     其它 4xx 归「端点可达但请求被拒」，同样说明网络与密钥格式没问题。
//
// 失败一律返回 ok=false + diagnosis.code，不向调用方抛错：管理面上的「测试」按钮
// 需要的是可读结论，而不是 500。
func probeAgentEndpoint(ctx context.Context, protocol, baseURL, apiKey string) (ConnectionTest, error) {
	trimmedBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmedBase == "" {
		return ConnectionTest{OK: false, Diagnosis: map[string]any{"code": "invalid_base_url"}}, nil
	}
	started := time.Now()
	if protocol == "anthropic_messages" {
		return probeAnthropicEndpoint(ctx, trimmedBase, apiKey, started)
	}
	return probeOpenAIAgentEndpoint(ctx, trimmedBase, apiKey, started)
}

// probeOpenAIAgentEndpoint 走 GET /models。
func probeOpenAIAgentEndpoint(
	ctx context.Context, baseURL, apiKey string, started time.Time,
) (ConnectionTest, error) {
	url := baseURL
	if !strings.HasSuffix(url, "/models") {
		url += "/models"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ConnectionTest{OK: false, Diagnosis: map[string]any{"code": "invalid_base_url"}}, nil
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return ConnectionTest{OK: false, Diagnosis: map[string]any{"code": "network_error"}}, nil
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	latency := time.Since(started).Milliseconds()
	status := response.StatusCode
	switch {
	case status >= 200 && status < 300:
		return ConnectionTest{
			OK:         true,
			Models:     parseOpenAIModelIDs(body),
			LatencyMS:  &latency,
			HTTPStatus: &status,
		}, nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ConnectionTest{
			OK:         false,
			Diagnosis:  map[string]any{"code": "auth_failed"},
			LatencyMS:  &latency,
			HTTPStatus: &status,
		}, nil
	default:
		return ConnectionTest{
			OK:         false,
			Diagnosis:  map[string]any{"code": "unexpected_status"},
			LatencyMS:  &latency,
			HTTPStatus: &status,
		}, nil
	}
}

// probeAnthropicEndpoint 发一条最小 messages 请求。
func probeAnthropicEndpoint(
	ctx context.Context, baseURL, apiKey string, started time.Time,
) (ConnectionTest, error) {
	url := baseURL
	if !strings.HasSuffix(url, "/messages") {
		url += "/messages"
	}
	payload := []byte(`{"model":"claude-3-5-haiku-latest","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return ConnectionTest{OK: false, Diagnosis: map[string]any{"code": "invalid_base_url"}}, nil
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", apiKey)
	request.Header.Set("anthropic-version", "2023-06-01")
	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return ConnectionTest{OK: false, Diagnosis: map[string]any{"code": "network_error"}}, nil
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	latency := time.Since(started).Milliseconds()
	status := response.StatusCode
	switch {
	case status >= 200 && status < 300:
		return ConnectionTest{OK: true, LatencyMS: &latency, HTTPStatus: &status}, nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ConnectionTest{
			OK: false, Diagnosis: map[string]any{"code": "auth_failed"},
			LatencyMS: &latency, HTTPStatus: &status,
		}, nil
	case status == http.StatusNotFound || status == http.StatusBadRequest:
		// 端点可达、密钥格式被接受，只是探针请求不被受理（模型名/协议差异）。
		return ConnectionTest{
			OK: false, Diagnosis: map[string]any{"code": "endpoint_reachable_but_rejected"},
			LatencyMS: &latency, HTTPStatus: &status,
		}, nil
	default:
		return ConnectionTest{
			OK: false, Diagnosis: map[string]any{"code": "unexpected_status"},
			LatencyMS: &latency, HTTPStatus: &status,
		}, nil
	}
}

// parseOpenAIModelIDs 从 /models 响应里取模型 id 列表；解析失败返回 nil。
func parseOpenAIModelIDs(body []byte) []string {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	out := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.ID != "" {
			out = append(out, item.ID)
		}
	}
	return out
}
