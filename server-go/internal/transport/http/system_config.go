// 系统配置与版本路由。对应 server/routers/system_config.py 的 4 个端点。
package http

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/service/admin"
)

// SystemConfigDeps 聚合系统配置依赖（诊断日志下载在 system.go 另有 SystemDeps）。
type SystemConfigDeps struct {
	Config *admin.SystemConfigService
}

// systemConfigAllowedKeys 是 PATCH /system/config 的合法字段集（extra="forbid"）。
var systemConfigAllowedKeys = map[string]bool{
	"default_video_backend": true, "default_video_backend_i2v": true, "default_video_backend_r2v": true,
	"default_image_backend": true, "default_image_backend_t2i": true, "default_image_backend_i2i": true,
	"default_text_backend": true, "default_audio_backend": true,
	"narration_voice": true, "narration_speed": true, "video_generate_audio": true,
	"anthropic_api_key": true, "anthropic_base_url": true, "anthropic_model": true,
	"anthropic_default_haiku_model": true, "anthropic_default_opus_model": true,
	"anthropic_default_sonnet_model": true, "claude_code_subagent_model": true,
	"agent_session_cleanup_delay_seconds": true, "agent_max_concurrent_sessions": true,
	"text_backend_simple": true, "text_backend_complex": true,
}

// RegisterSystemConfig 注册系统配置端点（GET 公开、PATCH 管理员）。
func RegisterSystemConfig(group *gin.RouterGroup, deps SystemConfigDeps) {
	h := &systemConfigHandlers{deps: deps}
	group.GET("/system/config", h.getConfig)
	group.GET("/system/config/model-candidates", h.modelCandidates)
	group.GET("/system/version", h.version)
	group.PATCH("/system/config", auth.RequireAdmin(), h.patchConfig)
}

type systemConfigHandlers struct{ deps SystemConfigDeps }

func (h *systemConfigHandlers) getConfig(c *gin.Context) {
	payload, err := h.deps.Config.GetConfig(c.Request.Context())
	if err != nil {
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	c.JSON(http.StatusOK, payload)
}

func (h *systemConfigHandlers) modelCandidates(c *gin.Context) {
	payload, err := h.deps.Config.BuildModelCandidates(c.Request.Context())
	if err != nil {
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	c.JSON(http.StatusOK, payload)
}

func (h *systemConfigHandlers) version(c *gin.Context) {
	c.JSON(http.StatusOK, admin.GetVersion(c.Request.Context()))
}

// patchConfig 只收已知字段（extra="forbid"）；字段集校验在原始键值表上做，
// 因为「字段是否出现」本身就是语义（Python model_fields_set）。
func (h *systemConfigHandlers) patchConfig(c *gin.Context) {
	body, err := readRawJSONBody(c)
	if err != nil {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
			Loc: []string{"body"}, Msg: "invalid request body", Type: "json_invalid",
		}}})
		return
	}
	fields := map[string]json.RawMessage{}
	for key, value := range body {
		if !systemConfigAllowedKeys[key] {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"body", key}, Msg: "Extra inputs are not permitted", Type: "extra_forbidden",
			}}})
			return
		}
		fields[key] = value
	}
	payload, err := h.deps.Config.PatchConfig(c.Request.Context(), admin.ConfigPatchInput{Fields: fields})
	if err != nil {
		var invalid *admin.ConfigValidationError
		if asConfigValidation(err, &invalid) {
			apperr.Abort(c, apperr.NewText(invalid.Status, invalid.Msg, nil))
			return
		}
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	c.JSON(http.StatusOK, payload)
}

// readRawJSONBody 读请求体为键值表；空体 / 畸形 JSON 报错。
func readRawJSONBody(c *gin.Context) (map[string]json.RawMessage, error) {
	raw, err := c.GetRawData()
	if err != nil {
		return nil, err
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	return body, nil
}

// asConfigValidation 是 errors.As 的本地薄封装（避免 import errors 链扩散到每个分支）。
func asConfigValidation(err error, target **admin.ConfigValidationError) bool {
	for err != nil {
		if typed, ok := err.(*admin.ConfigValidationError); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
