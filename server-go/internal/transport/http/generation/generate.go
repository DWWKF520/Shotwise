// 生成入队路由（对应 server/routers/generate.py）。
//
// Mount prefix: /api/v1/projects/:name
//
// 路由树注意：/characters/:entry/voice-sample 与资产路由共用 characters 子树，参数名
// 必须同为 :entry，否则 gin 在注册期就会 panic。
//
// 响应里的 message 由 message_key / message_params 在传输层按请求 locale 渲染——服务层
// 不持有 locale，任务文案只有这一个出口。
package generation

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/i18n"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/generation"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"
)

// GenerateDeps 是生成路由的依赖。
type GenerateDeps struct {
	Store    *service.ProjectStore
	Generate *generation.GenerateService
}

// RegisterGenerate 注册生成入队路由。
func RegisterGenerate(group *gin.RouterGroup, d GenerateDeps) {
	if d.Generate == nil {
		return
	}
	group.POST("/projects/:name/generate/storyboard/:segment_id", generateStoryboard(d))
	group.POST("/projects/:name/generate/video/:segment_id", generateVideo(d))
	group.POST("/projects/:name/generate/tts", generateTTSBatch(d))
	group.POST("/projects/:name/generate/tts/:segment_id", generateTTS(d))
	group.GET("/projects/:name/audio-backend/voices", audioBackendVoices(d))
	group.POST("/projects/:name/characters/:entry/voice-sample", generateVoiceSample(d))
	group.POST("/projects/:name/characters/:entry/voice-sample/confirm", confirmVoiceSample(d))
	group.POST("/projects/:name/generate/character/:entry", generateAsset(d, project.AssetCharacter))
	group.POST("/projects/:name/generate/scene/:entry", generateAsset(d, project.AssetScene))
	group.POST("/projects/:name/generate/prop/:entry", generateAsset(d, project.AssetProp))
	group.POST("/projects/:name/generate/product/:entry", generateAsset(d, project.AssetProduct))
	group.POST("/projects/:name/edit/image", editImage(d))
	group.POST("/projects/:name/canvas-images/split", splitCanvasImage(d))
	group.POST("/projects/:name/canvas-images/advanced", advancedCanvasImage(d))
}

// canvasImageBody 是两个画布端点共用的提交体。
//
// 全部用指针接：FastAPI 的 422 需要区分「字段缺失」（missing）与「类型不对」
// （int_parsing 等），零值无法区分这两者。
type canvasImageBody struct {
	SourceKind        *string        `json:"source_kind"`
	ResourceType      *string        `json:"resource_type"`
	ResourceID        *string        `json:"resource_id"`
	MediaAssetID      *string        `json:"media_asset_id"`
	ScriptFile        *string        `json:"script_file"`
	Rows              *int           `json:"rows"`
	Cols              *int           `json:"cols"`
	IncludeSplitLines *bool          `json:"include_split_lines"`
	Operation         *string        `json:"operation"`
	Instruction       *string        `json:"instruction"`
	Count             *int           `json:"count"`
	Region            map[string]any `json:"region"`
	AspectRatio       *string        `json:"aspect_ratio"`
	Multiplier        *int           `json:"multiplier"`
	Quality           *string        `json:"quality"`
}

// toRequest 归一为服务层请求。
func (b canvasImageBody) toRequest() generation.CanvasImageRequest {
	sourceKind := ""
	if b.SourceKind != nil {
		sourceKind = *b.SourceKind
	}
	return generation.CanvasImageRequest{
		SourceKind:        sourceKind,
		ResourceType:      b.ResourceType,
		ResourceID:        b.ResourceID,
		MediaAssetID:      b.MediaAssetID,
		ScriptFile:        b.ScriptFile,
		Rows:              b.Rows,
		Cols:              b.Cols,
		IncludeSplitLines: b.IncludeSplitLines,
		Operation:         b.Operation,
		Instruction:       b.Instruction,
		Count:             b.Count,
		Region:            b.Region,
		AspectRatio:       b.AspectRatio,
		Multiplier:        b.Multiplier,
		Quality:           b.Quality,
	}
}

// splitCanvasImage 提交宫格切分任务（不触碰分镜 Grid 记录）。
func splitCanvasImage(d GenerateDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		var body canvasImageBody
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		var items []apperr.ValidationItem
		if body.SourceKind == nil {
			items = append(items, apperr.ValidationItem{Loc: []string{"body", "source_kind"}, Msg: "field required", Type: "missing"})
		}
		if body.Rows == nil {
			items = append(items, apperr.ValidationItem{Loc: []string{"body", "rows"}, Msg: "field required", Type: "missing"})
		}
		if body.Cols == nil {
			items = append(items, apperr.ValidationItem{Loc: []string{"body", "cols"}, Msg: "field required", Type: "missing"})
		}
		if len(items) > 0 {
			apperr.Abort(c, &apperr.Validation{Items: items})
			return
		}
		out, err := d.Generate.SplitCanvasImage(
			c.Request.Context(), c.Param("name"), body.toRequest(), httpkit.CurrentUserID(c))
		writeGenerateResult(c, out, err)
	}
}

// advancedCanvasImage 提交创意画布图片操作。
func advancedCanvasImage(d GenerateDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		var body canvasImageBody
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		var items []apperr.ValidationItem
		if body.SourceKind == nil {
			items = append(items, apperr.ValidationItem{Loc: []string{"body", "source_kind"}, Msg: "field required", Type: "missing"})
		}
		if body.Operation == nil {
			items = append(items, apperr.ValidationItem{Loc: []string{"body", "operation"}, Msg: "field required", Type: "missing"})
		}
		if len(items) > 0 {
			apperr.Abort(c, &apperr.Validation{Items: items})
			return
		}
		out, err := d.Generate.AdvancedCanvasImage(
			c.Request.Context(), c.Param("name"), body.toRequest(), httpkit.CurrentUserID(c))
		writeGenerateResult(c, out, err)
	}
}

// generateStoryboard 提交分镜图生成任务。
func generateStoryboard(d GenerateDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		var body struct {
			Prompt     any    `json:"prompt"`
			ScriptFile string `json:"script_file"`
		}
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		out, err := d.Generate.GenerateStoryboard(
			c.Request.Context(), c.Param("name"), c.Param("segment_id"), body.Prompt, body.ScriptFile, httpkit.CurrentUserID(c),
		)
		writeGenerateResult(c, out, err)
	}
}

// generateVideo 提交分镜图生视频任务。
func generateVideo(d GenerateDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		var body struct {
			Prompt          any    `json:"prompt"`
			ScriptFile      string `json:"script_file"`
			DurationSeconds *int64 `json:"duration_seconds"`
			Seed            *int64 `json:"seed"`
		}
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		out, err := d.Generate.GenerateVideo(
			c.Request.Context(), c.Param("name"), c.Param("segment_id"), body.Prompt, body.ScriptFile,
			body.DurationSeconds, body.Seed, httpkit.CurrentUserID(c),
		)
		writeGenerateResult(c, out, err)
	}
}

// generateTTS 提交单段旁白配音任务。
func generateTTS(d GenerateDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		var body struct {
			ScriptFile string `json:"script_file"`
		}
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		out, err := d.Generate.GenerateTTS(
			c.Request.Context(), c.Param("name"), c.Param("segment_id"), body.ScriptFile, httpkit.CurrentUserID(c))
		writeGenerateResult(c, out, err)
	}
}

// generateTTSBatch 批量提交旁白配音任务（断点补缺）。
func generateTTSBatch(d GenerateDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		var body struct {
			ScriptFile string `json:"script_file"`
		}
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		out, err := d.Generate.GenerateTTSBatch(
			c.Request.Context(), c.Param("name"), body.ScriptFile, httpkit.CurrentUserID(c))
		writeGenerateResult(c, out, err)
	}
}

// audioBackendVoices 返回项目生效的音色枚举；未配置供应商时 configured=false。
func audioBackendVoices(d GenerateDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		out, err := d.Generate.AudioBackendVoices(c.Request.Context(), c.Param("name"))
		if err != nil {
			apperr.Abort(c, err)
			return
		}
		// backend 的 label 对携带描述性文案的供应商存的是 i18n key，按 locale 渲染；
		// 本就等于 id 的供应商（如 OpenAI）渲染结果即原串，无副作用。
		loc := i18n.FromRequest(c.Request)
		voices, _ := out["voices"].([]map[string]any)
		rendered := make([]map[string]any, 0, len(voices))
		for _, voice := range voices {
			key, _ := voice["label_key"].(string)
			id, _ := voice["id"].(string)
			label := id
			if key != "" {
				label = i18n.T(loc, key, nil)
			}
			rendered = append(rendered, map[string]any{"id": id, "label": label})
		}
		out["voices"] = rendered
		c.JSON(http.StatusOK, out)
	}
}

// generateVoiceSample 提交角色 TTS 试听样本生成任务。
func generateVoiceSample(d GenerateDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		var body struct {
			Text  string `json:"text"`
			Voice string `json:"voice"`
		}
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		out, err := d.Generate.GenerateVoiceSample(
			c.Request.Context(), c.Param("name"), c.Param("entry"), body.Text, body.Voice, httpkit.CurrentUserID(c))
		writeGenerateResult(c, out, err)
	}
}

// confirmVoiceSample 把已生成的样本提升为角色 reference_audio。
func confirmVoiceSample(d GenerateDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		var body struct {
			TaskID string `json:"task_id"`
		}
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		out, err := d.Generate.ConfirmVoiceSample(
			c.Request.Context(), c.Param("name"), c.Param("entry"), body.TaskID)
		writeGenerateResult(c, out, err)
	}
}

// generateAsset 提交资产设计图生成任务（四类资产共用）。
func generateAsset(d GenerateDeps, kind project.AssetKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		var body struct {
			Prompt     string `json:"prompt"`
			Regenerate bool   `json:"regenerate"`
		}
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		out, err := d.Generate.GenerateAsset(
			c.Request.Context(), kind, c.Param("name"), c.Param("entry"), body.Prompt, body.Regenerate, httpkit.CurrentUserID(c))
		writeGenerateResult(c, out, err)
	}
}

// editImage 提交图片指令式编辑任务。
func editImage(d GenerateDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		var body struct {
			ResourceType          string  `json:"resource_type"`
			ResourceID            string  `json:"resource_id"`
			Instruction           string  `json:"instruction"`
			ScriptFile            *string `json:"script_file"`
			ReferenceMediaAssetID *string `json:"reference_media_asset_id"`
		}
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		out, err := d.Generate.EditImage(
			c.Request.Context(), c.Param("name"), body.ResourceType, body.ResourceID, body.Instruction,
			body.ScriptFile, body.ReferenceMediaAssetID, httpkit.CurrentUserID(c))
		writeGenerateResult(c, out, err)
	}
}

// ---- 助手 ----

// writeGenerateResult 统一渲染 message 并写响应。
//
// 服务层返回 message_key / message_params（语言中立），这里按请求 locale 渲染成 message
// 后落地——任务提交文案只有这一个出口。
func writeGenerateResult(c *gin.Context, out map[string]any, err error) {
	if err != nil {
		abortGenerateErr(c, err)
		return
	}
	loc := i18n.FromRequest(c.Request)
	if key, ok := out["message_key"].(string); ok {
		params, _ := out["message_params"].(map[string]any)
		out["message"] = i18n.T(loc, key, params)
	}
	delete(out, "message_key")
	delete(out, "message_params")
	c.JSON(http.StatusOK, out)
}

// abortGenerateErr 把生成域错误映射成统一 {"detail": ...} 信封。
//
// 领域异常只带 i18n key 与 params（对应 lib/api_errors.py），detail 必须是翻译后的
// 字符串而不是 {"code": ...} 对象——与 Python 的 app 级 exception handler 一致。
func abortGenerateErr(c *gin.Context, err error) {
	var (
		generateErr *service.GenerateError
		specErr     *generation.TaskSpecValidationError
	)
	switch {
	case errors.As(err, &generateErr):
		apperr.Abort(c, apperr.NewText(generateErr.Status, generateErr.Code, generateErr.Params))
	case errors.As(err, &specErr):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, specErr.Code, specErr.Params))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
