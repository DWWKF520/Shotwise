package skills

import (
	"context"
	"fmt"
	"os"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/adk/middlewares/skill"
	"github.com/cloudwego/eino/schema"
)

// defaultSkillToolName 是 skill 工具的注册名（官方默认 "skill"，显式传入以固化契约）。
var defaultSkillToolName = SkillToolName

// NewFilesystemMiddleware 用官方 skill.NewBackendFromFilesystem + skill.NewTyped 组装
// skill 中间件（对应 Eino quickstart 第九章的推荐用法）。
//
// fsBackend 的根必须等于 skillsRoot：官方 backend 以 Backend.GlobInfo 返回的路径拼接
// BaseDir，只有「GlobInfo 返回相对 skillsRoot 的路径」时才能解析回正确文件
// （workspace.NewBackend(skillsRoot) 满足此约定）。
//
// 不自定义 BuildContent：沿用官方的 `Launching skill: <name>` +
// `Base directory for this skill: <abs>` 两段返回，事件日志的 skill_invocation
// 由 agent.parseSkillName 兼容中英文两种前缀后提取名字。
//
// 返回的 skill.Backend 供 subagent 注入 skill 正文复用；skillsRoot 不存在时先建空目录，
// 保证「没有 skill 也能正常对话」（与 quickstart 的降级语义一致）。
func NewFilesystemMiddleware(ctx context.Context, fsBackend filesystem.Backend, skillsRoot string) (adk.ChatModelAgentMiddleware, skill.Backend, error) {
	if fsBackend == nil {
		return nil, nil, fmt.Errorf("skills: filesystem backend is required")
	}
	if err := os.MkdirAll(skillsRoot, 0o755); err != nil {
		return nil, nil, fmt.Errorf("skills: prepare dir: %w", err)
	}
	backend, err := skill.NewBackendFromFilesystem(ctx, &skill.BackendFromFilesystemConfig{
		Backend: fsBackend,
		BaseDir: skillsRoot,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("skills: filesystem backend: %w", err)
	}
	mw, err := skill.NewTyped[*schema.Message](ctx, &skill.TypedConfig[*schema.Message]{
		Backend:       backend,
		SkillToolName: &defaultSkillToolName,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("skills: middleware: %w", err)
	}
	return mw, backend, nil
}
