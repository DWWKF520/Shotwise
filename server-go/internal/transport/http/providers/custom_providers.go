// 自定义供应商路由（对应 server/routers/custom_providers.py 的 CRUD 面）。
//
// 前缀：/api/v1/custom-providers
//
// 与内置供应商管理面（providers.go）分家：自定义供应商的 base_url / api_key 存在
// custom_provider 表上，不走 provider_config / provider_credential，也没有注册表元数据可读。
//
// 两处口径与 Python 一致：
//   - 整组需登录，但只有「删除」与「读明文凭证」叠管理员闸——逐端点挂 AdminUser，
//     不是整组管理员；
//   - 模型列表是整体替换语义，PUT /{id} 与 PUT /{id}/models 都重插模型行，
//     调用方不能缓存模型行 id。
//
// 模型发现（/discover、/discover-anthropic、/discover-openai、/{id}/discover）与连接测试
// （/test、/{id}/test）不在本文件：它们依赖尚不存在的发现服务，本组不注册这些路由，
// 命中即 404，而不是留一个恒定失败的端点冒充已支持。
package providers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/service/admin"
)

// CustomProviderDeps 是自定义供应商路由的依赖。
type CustomProviderDeps struct {
	Providers *admin.CustomProviderService
}

// RegisterCustomProviders 注册自定义供应商路由。
func RegisterCustomProviders(group *gin.RouterGroup, d CustomProviderDeps) {
	if d.Providers == nil {
		return
	}
	const base = "/custom-providers"

	// 静态子路径必须先于 /:provider_id 注册：gin 按段优先级匹配，静态段优先于参数段，
	// 但注册顺序仍按「具体在前」书写，读起来才与实际解析顺序一致。
	group.GET(base, listCustomProviders(d))
	group.GET(base+"/endpoints", listCustomEndpointCatalog(d))
	group.POST(base, createCustomProvider(d))

	group.GET(base+"/:provider_id", getCustomProvider(d))
	group.PATCH(base+"/:provider_id", patchCustomProvider(d))
	group.PUT(base+"/:provider_id", replaceCustomProvider(d))
	group.PUT(base+"/:provider_id/models", replaceCustomProviderModels(d))

	admin := group.Group("", auth.RequireAdmin())
	admin.DELETE(base+"/:provider_id", deleteCustomProvider(d))
	admin.GET(base+"/:provider_id/credentials", getCustomProviderCredentials(d))
}

// ── 入参 ─────────────────────────────────────────────────────────────────

// customModelBody 是模型写入形态的传输层载体。
type customModelBody struct {
	ModelID             string         `json:"model_id"`
	DisplayName         string         `json:"display_name"`
	Endpoint            string         `json:"endpoint"`
	IsDefault           bool           `json:"is_default"`
	IsEnabled           bool           `json:"is_enabled"`
	PriceUnit           *string        `json:"price_unit"`
	PriceInput          *float64       `json:"price_input"`
	PriceOutput         *float64       `json:"price_output"`
	Currency            *string        `json:"currency"`
	SupportedDurations  []int          `json:"supported_durations"`
	Resolution          *string        `json:"resolution"`
	CapabilityOverrides map[string]any `json:"capability_overrides"`
	EndpointDeclaration map[string]any `json:"endpoint_declaration"`
}

func (b customModelBody) toInput() admin.CustomModelInput {
	return admin.CustomModelInput{
		ModelID:             b.ModelID,
		DisplayName:         b.DisplayName,
		Endpoint:            b.Endpoint,
		IsDefault:           b.IsDefault,
		IsEnabled:           b.IsEnabled,
		PriceUnit:           b.PriceUnit,
		PriceInput:          b.PriceInput,
		PriceOutput:         b.PriceOutput,
		Currency:            b.Currency,
		SupportedDurations:  b.SupportedDurations,
		Resolution:          b.Resolution,
		CapabilityOverrides: b.CapabilityOverrides,
		EndpointDeclaration: b.EndpointDeclaration,
	}
}

func customModelInputs(bodies []customModelBody) []admin.CustomModelInput {
	if bodies == nil {
		return nil
	}
	out := make([]admin.CustomModelInput, 0, len(bodies))
	for _, body := range bodies {
		out = append(out, body.toInput())
	}
	return out
}

type customProviderCreateBody struct {
	DisplayName                string            `json:"display_name"`
	DiscoveryFormat            string            `json:"discovery_format"`
	BaseURL                    string            `json:"base_url"`
	APIKey                     string            `json:"api_key"`
	Models                     []customModelBody `json:"models"`
	ImageMaxWorkers            *int64            `json:"image_max_workers"`
	VideoMaxWorkers            *int64            `json:"video_max_workers"`
	AudioMaxWorkers            *int64            `json:"audio_max_workers"`
	ImageRequestTimeoutSeconds *float64          `json:"image_request_timeout_seconds"`
	IsEnabled                  *bool             `json:"is_enabled"`
}

// customProviderPatchBody 的字段全为可选：缺席即不改。
//
// discovery_format 不在其中——它决定模型发现与 URL 归一化的方式，属于供应商身份的一部分，
// 要换走重建。
type customProviderPatchBody struct {
	DisplayName *string `json:"display_name"`
	BaseURL     *string `json:"base_url"`
	APIKey      *string `json:"api_key"`
	IsEnabled   *bool   `json:"is_enabled"`
}

// customProviderReplaceBody 是 PUT 的权威形态：并发上限 / 超时 / 启用开关即使缺席也照写
// （nil = 清空回全局默认），只有 api_key 的缺席表示不改。
type customProviderReplaceBody struct {
	DisplayName                string            `json:"display_name"`
	BaseURL                    string            `json:"base_url"`
	APIKey                     *string           `json:"api_key"`
	Models                     []customModelBody `json:"models"`
	ImageMaxWorkers            *int64            `json:"image_max_workers"`
	VideoMaxWorkers            *int64            `json:"video_max_workers"`
	AudioMaxWorkers            *int64            `json:"audio_max_workers"`
	ImageRequestTimeoutSeconds *float64          `json:"image_request_timeout_seconds"`
	IsEnabled                  *bool             `json:"is_enabled"`
}

type customModelsReplaceBody struct {
	Models []customModelBody `json:"models"`
}

// ── 处理器 ───────────────────────────────────────────────────────────────

// listCustomProviders 返回全部自定义供应商及其模型。
func listCustomProviders(d CustomProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := d.Providers.ListProviders(c.Request.Context())
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"providers": items})
	}
}

// listCustomEndpointCatalog 返回 endpoint 目录，供设置页渲染 endpoint 选择器。
func listCustomEndpointCatalog(d CustomProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"endpoints": d.Providers.EndpointCatalog()})
	}
}

// createCustomProvider 新建供应商及其模型列表。
func createCustomProvider(d CustomProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body customProviderCreateBody
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		out, err := d.Providers.CreateProvider(c.Request.Context(), admin.CustomProviderCreateInput{
			DisplayName:                body.DisplayName,
			DiscoveryFormat:            body.DiscoveryFormat,
			BaseURL:                    body.BaseURL,
			APIKey:                     body.APIKey,
			Models:                     customModelInputs(body.Models),
			ImageMaxWorkers:            body.ImageMaxWorkers,
			VideoMaxWorkers:            body.VideoMaxWorkers,
			AudioMaxWorkers:            body.AudioMaxWorkers,
			ImageRequestTimeoutSeconds: body.ImageRequestTimeoutSeconds,
			IsEnabled:                  body.IsEnabled,
		})
		if err != nil {
			apperr.Abort(c, err)
			return
		}
		c.JSON(http.StatusCreated, out)
	}
}

// getCustomProvider 返回单个自定义供应商。
func getCustomProvider(d CustomProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpkit.PathInt64(c, "provider_id")
		if !ok {
			return
		}
		out, err := d.Providers.GetProvider(c.Request.Context(), id)
		if err != nil {
			apperr.Abort(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// patchCustomProvider 局部更新供应商。
func patchCustomProvider(d CustomProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpkit.PathInt64(c, "provider_id")
		if !ok {
			return
		}
		var body customProviderPatchBody
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		out, err := d.Providers.PatchProvider(c.Request.Context(), id, admin.CustomProviderPatchInput{
			DisplayName: body.DisplayName,
			BaseURL:     body.BaseURL,
			APIKey:      body.APIKey,
			IsEnabled:   body.IsEnabled,
		})
		if err != nil {
			apperr.Abort(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// replaceCustomProvider 全量更新供应商并整体替换模型列表。
func replaceCustomProvider(d CustomProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpkit.PathInt64(c, "provider_id")
		if !ok {
			return
		}
		var body customProviderReplaceBody
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		out, err := d.Providers.ReplaceProvider(c.Request.Context(), id, admin.CustomProviderReplaceInput{
			DisplayName:                body.DisplayName,
			BaseURL:                    body.BaseURL,
			APIKey:                     body.APIKey,
			Models:                     customModelInputs(body.Models),
			ImageMaxWorkers:            body.ImageMaxWorkers,
			VideoMaxWorkers:            body.VideoMaxWorkers,
			AudioMaxWorkers:            body.AudioMaxWorkers,
			ImageRequestTimeoutSeconds: body.ImageRequestTimeoutSeconds,
			IsEnabled:                  body.IsEnabled,
		})
		if err != nil {
			apperr.Abort(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// replaceCustomProviderModels 整体替换模型列表（响应为裸数组）。
func replaceCustomProviderModels(d CustomProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpkit.PathInt64(c, "provider_id")
		if !ok {
			return
		}
		var body customModelsReplaceBody
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		out, err := d.Providers.ReplaceModels(c.Request.Context(), id, customModelInputs(body.Models))
		if err != nil {
			apperr.Abort(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// deleteCustomProvider 删除供应商并级联清理引用。
func deleteCustomProvider(d CustomProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpkit.PathInt64(c, "provider_id")
		if !ok {
			return
		}
		if err := d.Providers.DeleteProvider(c.Request.Context(), id); err != nil {
			apperr.Abort(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// getCustomProviderCredentials 返回明文凭证（管理员限定）。
func getCustomProviderCredentials(d CustomProviderDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpkit.PathInt64(c, "provider_id")
		if !ok {
			return
		}
		out, err := d.Providers.Credentials(c.Request.Context(), id)
		if err != nil {
			apperr.Abort(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}
