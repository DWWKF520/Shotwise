// 创作稿文本生成端点。对应 server/routers/creative.py（单端点）。
//
// 这是对浏览器里未保存草稿的显式文本操作，不落库、不做结构化输出——持久化是
// 前端的另一个动作。
package creative

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/textgen"
	"gorm.io/gorm"
)

// creativeOperations 是操作白名单（Python Literal）。
var creativeOperations = map[string]bool{
	"generate": true, "continue": true, "expand": true,
	"rewrite": true, "polish": true, "outline": true, "split": true,
}

// creativeMaxContent / creativeMaxInstruction 是请求体长度上限（与 pydantic 一致）。
const (
	creativeMaxContent     = 120_000
	creativeMaxInstruction = 4_000
)

// creativeDefaultMaxOutputTokens 对应 Python DEFAULT_MAX_OUTPUT_TOKENS。
const creativeDefaultMaxOutputTokens = 64000

// CreativeDeps 聚合创作稿依赖。
type CreativeDeps struct {
	Store *service.ProjectStore
	DB    *gorm.DB
}

// RegisterCreative 注册创作稿生成端点（authed 组内）。
func RegisterCreative(group *gin.RouterGroup, deps CreativeDeps) {
	group.POST("/projects/:name/creative-draft/generate", func(c *gin.Context) {
		var body struct {
			Operation   string `json:"operation"`
			Content     string `json:"content"`
			Instruction string `json:"instruction"`
		}
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		if !creativeOperations[body.Operation] {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
			return
		}
		if len(body.Content) > creativeMaxContent || len(body.Instruction) > creativeMaxInstruction {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
			return
		}

		pf, err := deps.Store.Get(c.Param("name"))
		if err != nil {
			apperr.Abort(c, apperr.NewText(http.StatusNotFound, "project_not_found",
				map[string]any{"name": c.Param("name")}))
			return
		}
		contentMode := pf.ContentMode
		if contentMode == "" {
			contentMode = string(project.ContentNarration)
		}
		// ad 项目没有创作稿工作台。
		if contentMode == string(project.ContentAd) {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
			return
		}
		instruction := strings.TrimSpace(body.Instruction)
		content := strings.TrimSpace(body.Content)
		if body.Operation == "generate" && instruction == "" {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
			return
		}
		if body.Operation != "generate" && content == "" {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
			return
		}

		generator, err := textgen.NewGenerator(c.Request.Context(), deps.DB, pf, textgen.TaskScript)
		if err != nil {
			// 后端未配置 / 凭证缺失等 → Python ValueError → request_invalid（400）。
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "request_invalid", nil))
			return
		}
		text, err := generator.Generate(c.Request.Context(),
			buildCreativeDraftPrompt(body.Operation, contentMode, string(pf.SourceKind), content, instruction),
			nil, creativeDefaultMaxOutputTokens)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"operation": body.Operation,
			"content":   strings.TrimSpace(text),
		})
	})
}

// buildCreativeDraftPrompt 构造模式感知的自由文本请求（与 Python build_creative_draft_prompt 同形）。
func buildCreativeDraftPrompt(operation, contentMode, sourceKind, content, instruction string) string {
	modeInstruction := "这是小说改编的剧集项目。输出故事正文，保持人物动机、冲突和场景推进可供后续分集与改编使用。"
	if contentMode == string(project.ContentNarration) {
		modeInstruction = "这是短视频旁白项目。输出应是自然、可口播的叙事文本，画面信息只在确有帮助时简洁提示。"
	} else if sourceKind == "screenplay" {
		modeInstruction = "这是剧集剧本项目。保持场次、角色、对白和旁白清晰；修改既有文本时不得无故丢失台词。"
	}
	operationInstructions := map[string]string{
		"generate": "根据创作要求写出一个可继续编辑的完整初稿。",
		"continue": "在不重复已有内容的前提下续写下一段，延续人物、语气和叙事线索。",
		"expand":   "在不改变核心事实和既有结构的前提下扩写草稿，补足必要的动作、情绪和场景细节。",
		"rewrite":  "按创作要求重写这份草稿，保留没有被要求改变的核心事实。",
		"polish":   "润色这份草稿，使表达更清楚、有节奏，避免改变剧情事实。",
		"outline":  "输出层级清晰的故事大纲，不要重写正文。",
		"split":    "把草稿拆成可供后续分集规划的章节或场景列表，并为每项给出简短摘要。",
	}
	parts := []string{
		"你是项目创作稿编辑器中的写作助手。",
		modeInstruction,
		operationInstructions[operation],
		"只输出用户可直接阅读和编辑的内容；不要输出解释、JSON、代码块或自我说明。",
	}
	if instruction != "" {
		parts = append(parts, "# 创作要求", instruction)
	}
	if content != "" {
		parts = append(parts, "# 当前创作稿", "<draft>", content, "</draft>")
	}
	return strings.Join(parts, "\n\n")
}
