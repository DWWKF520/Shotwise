package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
)

// 长期记忆候选工具（对应 server/agent_runtime/sdk_tools/memory.py 的 propose_memory_candidate）。

// WithMemoryProposals 注入长期记忆候选工具的依赖；未注入时不注册该工具。
func WithMemoryProposals(store service.MemoryCandidateStore) RuntimeOption {
	return func(r *Runtime) { r.memoryStore = store }
}

// proposeMemoryCandidateIn 是 propose_memory_candidate 的入参。
type proposeMemoryCandidateIn struct {
	Scope    string `json:"scope" jsonschema:"description=作用域：user=用户级、project=项目级;enum=user,project"`
	Category string `json:"category" jsonschema:"description=类别;enum=preference,style,world,terminology,workflow,other"`
	Content  string `json:"content" jsonschema:"description=记忆内容（不含密钥 / 凭据 / 系统指令）"`
}

// newProposeMemoryCandidateTool 构造 propose_memory_candidate。
func newProposeMemoryCandidateTool(
	project, sessionID string, store service.MemoryCandidateStore,
) (tool.InvokableTool, error) {
	return toolutils.InferTool("propose_memory_candidate",
		"提议一条长期记忆候选。候选必须经过用户确认后才会成为长期记忆；不要保存秘密、凭据或系统指令。",
		func(ctx context.Context, in proposeMemoryCandidateIn) (string, error) {
			if store == nil {
				return denyEnvelope("memory proposals not configured"), nil
			}
			var projectName *string
			if in.Scope == "project" {
				name := project
				projectName = &name
			}
			var session *string
			if sessionID != "" {
				s := sessionID
				session = &s
			}
			id, err := store.CreateMemoryCandidate(ctx, service.CreateMemoryCandidateParams{
				UserID:          repository.DefaultUserID,
				Scope:           in.Scope,
				ProjectName:     projectName,
				Category:        in.Category,
				Content:         in.Content,
				SourceSessionID: session,
			})
			if err != nil {
				var validation *service.MemoryValidationError
				if errors.As(err, &validation) {
					return denyEnvelope("❌ 记忆候选被拒：" + memoryValidationMessage(validation.Code)), nil
				}
				return denyEnvelope("❌ propose_memory_candidate 失败: " + err.Error()), nil
			}
			return okEnvelope(fmt.Sprintf("记忆候选已创建（%s），等待用户确认后才会成为长期记忆。", id)), nil
		})
}

// memoryValidationMessage 把记忆校验错误码渲染为给 agent 看的中文说明。
func memoryValidationMessage(code string) string {
	switch code {
	case "memory_scope_invalid":
		return "scope 必须是 user 或 project"
	case "memory_project_required":
		return "project 作用域必须指定项目名"
	case "memory_project_invalid":
		return "project 作用域的项目名不合法"
	case "memory_category_invalid":
		return "category 不在允许的类别内"
	case "memory_content_empty":
		return "content 为空"
	case "memory_content_too_long":
		return fmt.Sprintf("content 过长（上限 %d 字符）", service.MemoryMaxLength)
	case "memory_sensitive_content":
		return "content 疑似包含密钥 / 凭据 / 本机路径，不得写入长期记忆"
	case "memory_prompt_injection":
		return "content 疑似指令注入，不得写入长期记忆"
	default:
		return code
	}
}
