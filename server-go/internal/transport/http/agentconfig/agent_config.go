// Agent 凭证与预设目录路由（对应 server/routers/agent_config.py，收敛版 8 端点）。
//
// 前缀：/api/v1/agent
//
// 收敛口径：预设目录只给 anthropic（sdk_type=claude）与 openai（sdk_type=openai）两条 +
// __custom__ 哨兵。响应结构与 Python 一致，前端不用改渲染。
//
// 整组要求管理员身份（与 Python 的 AdminUser 一致）：这些凭证能改智能体的全部出入网行为。
package agentconfig

import (
	"errors"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/service/admin"
)

// AgentConfigDeps 是 Agent 配置路由的依赖。
type AgentConfigDeps struct {
	Credentials *admin.AgentCredentialService
}

// RegisterAgentConfig 注册 Agent 配置路由。
func RegisterAgentConfig(group *gin.RouterGroup, d AgentConfigDeps) {
	if d.Credentials == nil {
		return
	}
	admin := group.Group("/agent", auth.RequireAdmin())
	admin.GET("/preset-providers", listAgentPresets())
	admin.GET("/credentials", listAgentCredentials(d))
	admin.POST("/credentials", createAgentCredential(d))
	admin.PATCH("/credentials/:cred_id", updateAgentCredential(d))
	admin.DELETE("/credentials/:cred_id", deleteAgentCredential(d))
	admin.POST("/credentials/:cred_id/activate", activateAgentCredential(d))
	admin.POST("/credentials/:cred_id/test", testAgentCredential(d))
	admin.POST("/test-connection", testAgentConnectionDraft(d))
}

// listAgentPresets 返回预设供应商目录（按 sdk_type 过滤）。
func listAgentPresets() gin.HandlerFunc {
	return func(c *gin.Context) {
		sdkType := c.Query("sdk_type")
		if sdkType == "" {
			sdkType = admin.AgentSDKClaude
		}
		if sdkType != admin.AgentSDKClaude && sdkType != admin.AgentSDKOpenAI {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"query", "sdk_type"},
				Msg: "String should match pattern '^(claude|openai)$'", Type: "string_pattern_mismatch",
			}}})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"providers":          admin.ListAgentPresets(sdkType),
			"custom_sentinel_id": admin.CustomPresetID,
		})
	}
}

// listAgentCredentials 列出当前用户的 agent 凭证。
func listAgentCredentials(d AgentConfigDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		sdkType := c.Query("sdk_type")
		if sdkType != "" && sdkType != admin.AgentSDKClaude && sdkType != admin.AgentSDKOpenAI {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"query", "sdk_type"},
				Msg: "String should match pattern '^(claude|openai)$'", Type: "string_pattern_mismatch",
			}}})
			return
		}
		items, err := d.Credentials.List(c.Request.Context(), httpkit.CurrentUserID(c), sdkType)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"credentials": items})
	}
}

// agentCredentialBody 是创建 / 更新的入参。
type agentCredentialBody struct {
	SDKType     *string          `json:"sdk_type"`
	Protocol    *string          `json:"protocol"`
	PresetID    *string          `json:"preset_id"`
	DisplayName *string          `json:"display_name"`
	BaseURL     *string          `json:"base_url"`
	APIKey      *string          `json:"api_key"`
	Model       *string          `json:"model"`
	HaikuModel  *string          `json:"haiku_model"`
	SonnetModel *string          `json:"sonnet_model"`
	OpusModel   *string          `json:"opus_model"`
	SubagentMdl *string          `json:"subagent_model"`
	ModelMap    []map[string]any `json:"model_map"`
	Activate    *bool            `json:"activate"`
}

func (b agentCredentialBody) toInput() admin.AgentCredentialInput {
	return admin.AgentCredentialInput{
		SDKType:     b.SDKType,
		Protocol:    b.Protocol,
		PresetID:    b.PresetID,
		DisplayName: b.DisplayName,
		BaseURL:     b.BaseURL,
		APIKey:      b.APIKey,
		Model:       b.Model,
		HaikuModel:  b.HaikuModel,
		SonnetModel: b.SonnetModel,
		OpusModel:   b.OpusModel,
		SubagentMdl: b.SubagentMdl,
		ModelMap:    b.ModelMap,
		Activate:    b.Activate,
	}
}

// createAgentCredential 新增一条 agent 凭证。
func createAgentCredential(d AgentConfigDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body agentCredentialBody
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		var items []apperr.ValidationItem
		if body.PresetID == nil {
			items = append(items, apperr.ValidationItem{
				Loc: []string{"body", "preset_id"}, Msg: "field required", Type: "missing"})
		}
		if body.APIKey == nil {
			items = append(items, apperr.ValidationItem{
				Loc: []string{"body", "api_key"}, Msg: "field required", Type: "missing"})
		}
		if len(items) > 0 {
			apperr.Abort(c, &apperr.Validation{Items: items})
			return
		}
		out, err := d.Credentials.Create(c.Request.Context(), httpkit.CurrentUserID(c), body.toInput())
		if err != nil {
			abortAgentConfigErr(c, err)
			return
		}
		c.JSON(http.StatusCreated, out)
	}
}

// updateAgentCredential 更新凭证（nil 字段不覆盖）。
func updateAgentCredential(d AgentConfigDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		credID, ok := httpkit.PathInt64(c, "cred_id")
		if !ok {
			return
		}
		var body agentCredentialBody
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		out, err := d.Credentials.Update(c.Request.Context(), httpkit.CurrentUserID(c), credID, body.toInput())
		if err != nil {
			abortAgentConfigErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// deleteAgentCredential 删除凭证。
func deleteAgentCredential(d AgentConfigDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		credID, ok := httpkit.PathInt64(c, "cred_id")
		if !ok {
			return
		}
		if err := d.Credentials.Delete(c.Request.Context(), httpkit.CurrentUserID(c), credID); err != nil {
			abortAgentConfigErr(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// activateAgentCredential 把凭证设为该 sdk_type 的唯一活跃凭证。
func activateAgentCredential(d AgentConfigDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		credID, ok := httpkit.PathInt64(c, "cred_id")
		if !ok {
			return
		}
		out, err := d.Credentials.Activate(c.Request.Context(), httpkit.CurrentUserID(c), credID)
		if err != nil {
			abortAgentConfigErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"id":        out.ID,
			"sdk_type":  out.SDKType,
			"is_active": out.IsActive,
		})
	}
}

// testAgentCredential 用已存凭证试连。
func testAgentCredential(d AgentConfigDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		credID, ok := httpkit.PathInt64(c, "cred_id")
		if !ok {
			return
		}
		out, err := d.Credentials.TestCredential(c.Request.Context(), httpkit.CurrentUserID(c), credID)
		if err != nil {
			abortAgentConfigErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// testAgentConnectionDraft 用入参直接试连（不落库）。
func testAgentConnectionDraft(d AgentConfigDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body agentCredentialBody
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		out, err := d.Credentials.TestDraft(c.Request.Context(), body.toInput())
		if err != nil {
			abortAgentConfigErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// abortAgentConfigErr 映射 Agent 配置域错误。
func abortAgentConfigErr(c *gin.Context, err error) {
	var valueErr *service.ValueError
	switch {
	case errors.Is(err, admin.ErrAgentCredentialNotFound):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "credentials_not_found", nil))
	case errors.As(err, &valueErr):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", map[string]any{
			"details": valueErr.Msg,
		}))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
