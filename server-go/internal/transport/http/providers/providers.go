// 供应商管理路由（对应 server/routers/providers.py，收敛版 12 端点）。
//
// 前缀：/api/v1/providers
//
// 与 Python 的差异只在一处：连接测试只对 OpenAI 兼容端点（openai / ark / agnes）实现，
// 其余供应商返回 success=false + 明确原因，而不是假装通过。管理面上「测试通过」必须
// 真意味着能连上——这是用户在配置面板点「测试」时唯一的信息来源。
package providers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/admin"
)

// ProviderDeps 是供应商管理路由的依赖。
type ProviderDeps struct {
	Providers *admin.ProviderAdminService
}

// RegisterProviders 注册供应商管理路由。
func RegisterProviders(group *gin.RouterGroup, d ProviderDeps) {
	if d.Providers == nil {
		return
	}
	base := "/providers"
	group.GET(base, listProviders(d))
	group.PATCH(base+"/:provider_id/enabled", setProviderEnabled(d))
	group.GET(base+"/:provider_id/config", getProviderConfig(d))
	group.PATCH(base+"/:provider_id/config", patchProviderConfig(d))
	group.PATCH(base+"/:provider_id/model-types", patchProviderModelTypes(d))
	group.GET(base+"/:provider_id/credentials", listProviderCredentials(d))
	group.POST(base+"/:provider_id/credentials", createProviderCredential(d))
	group.PATCH(base+"/:provider_id/credentials/:cred_id", updateProviderCredential(d))
	group.DELETE(base+"/:provider_id/credentials/:cred_id", deleteProviderCredential(d))
	group.POST(base+"/:provider_id/credentials/:cred_id/activate", activateProviderCredential(d))
	group.POST(base+"/:provider_id/test", testProviderConnection(d))
	group.POST("/providers/gemini-vertex/credentials/upload", uploadVertexCredential(d))
}

// listProviders 返回全部内置供应商的状态。
func listProviders(d ProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := d.Providers.List(c.Request.Context())
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		// 序列化在传输层完成：服务层结构体带 json tag，前端直接按字段读。
		c.JSON(http.StatusOK, gin.H{"providers": items})
	}
}

// setProviderEnabled 设置启用开关。
func setProviderEnabled(d ProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		if body.Enabled == nil {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"body", "enabled"}, Msg: "field required", Type: "missing",
			}}})
			return
		}
		if err := d.Providers.SetEnabled(c.Request.Context(), c.Param("provider_id"), *body.Enabled); err != nil {
			abortProviderErr(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// getProviderConfig 返回字段化配置（密钥只回掩码）。
func getProviderConfig(d ProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := d.Providers.GetConfig(c.Request.Context(), c.Param("provider_id"))
		if err != nil {
			abortProviderErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// patchProviderConfig 写入非密钥配置字段。
func patchProviderConfig(d ProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body map[string]any
		if err := c.ShouldBindJSON(&body); err != nil {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"body"}, Msg: "invalid request body", Type: "json_invalid",
			}}})
			return
		}
		updates := map[string]string{}
		for key, value := range body {
			text, ok := value.(string)
			if !ok {
				apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
					Loc: []string{"body", key}, Msg: "Input should be a valid string", Type: "string_type",
				}}})
				return
			}
			updates[key] = text
		}
		if err := d.Providers.PatchConfig(c.Request.Context(), c.Param("provider_id"), updates); err != nil {
			abortProviderErr(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// patchProviderModelTypes 覆盖模型 → 媒体类型的推断结果。
func patchProviderModelTypes(d ProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body map[string]string
		if err := c.ShouldBindJSON(&body); err != nil {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"body"}, Msg: "invalid request body", Type: "json_invalid",
			}}})
			return
		}
		out, err := d.Providers.PatchModelTypes(c.Request.Context(), c.Param("provider_id"), body)
		if err != nil {
			abortProviderErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// listProviderCredentials 列出该供应商的凭证。
func listProviderCredentials(d ProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := d.Providers.ListCredentials(c.Request.Context(), c.Param("provider_id"))
		if err != nil {
			abortProviderErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"credentials": items})
	}
}

// providerCredentialBody 是凭证创建 / 更新的入参。
type providerCredentialBody struct {
	Name    *string `json:"name"`
	APIKey  *string `json:"api_key"`
	BaseURL *string `json:"base_url"`
}

func (b providerCredentialBody) toInput() admin.CredentialInput {
	return admin.CredentialInput{Name: b.Name, APIKey: b.APIKey, BaseURL: b.BaseURL}
}

// createProviderCredential 新增一条凭证（该供应商首条自动激活）。
func createProviderCredential(d ProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body providerCredentialBody
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		out, err := d.Providers.CreateCredential(c.Request.Context(), c.Param("provider_id"), body.toInput())
		if err != nil {
			abortProviderErr(c, err)
			return
		}
		c.JSON(http.StatusCreated, out)
	}
}

// updateProviderCredential 更新凭证（空字段不覆盖）。
func updateProviderCredential(d ProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		credID, ok := httpkit.PathInt64(c, "cred_id")
		if !ok {
			return
		}
		var body providerCredentialBody
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		if err := d.Providers.UpdateCredential(
			c.Request.Context(), c.Param("provider_id"), credID, body.toInput()); err != nil {
			abortProviderErr(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// deleteProviderCredential 删除凭证。
func deleteProviderCredential(d ProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		credID, ok := httpkit.PathInt64(c, "cred_id")
		if !ok {
			return
		}
		if err := d.Providers.DeleteCredential(c.Request.Context(), c.Param("provider_id"), credID); err != nil {
			abortProviderErr(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// activateProviderCredential 把凭证设为唯一活跃凭证。
func activateProviderCredential(d ProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		credID, ok := httpkit.PathInt64(c, "cred_id")
		if !ok {
			return
		}
		if err := d.Providers.ActivateCredential(c.Request.Context(), c.Param("provider_id"), credID); err != nil {
			abortProviderErr(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// testProviderConnection 做一次最小连通性验证。
func testProviderConnection(d ProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := d.Providers.TestConnection(c.Request.Context(), c.Param("provider_id"))
		if err != nil {
			abortProviderErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// uploadVertexCredential Vertex 服务账号文件上传。
//
// 本轮供应商范围收敛到 openai / ark（seedance、seedream），Vertex 不在其中：
// 明确回 400 而不是落一个用不上的凭证。
func uploadVertexCredential(d ProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", map[string]any{
			"details": "vertex credential upload is not supported",
		}))
	}
}

// abortProviderErr 把供应商域错误映射成统一信封。
func abortProviderErr(c *gin.Context, err error) {
	var valueErr *service.ValueError
	switch {
	case errors.Is(err, admin.ErrUnknownProviderID):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "unknown_provider",
			map[string]any{"provider_id": c.Param("provider_id")}))
	case errors.Is(err, admin.ErrCredentialNotFound):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "credentials_not_found", nil))
	case errors.As(err, &valueErr):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", map[string]any{
			"details": valueErr.Msg,
		}))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
