package skills

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/skill"
)

// ModeAwareBackend 实现 skill.Backend：按会话 content_mode 解析变体（§17.1）。
//
// mode 为空时只暴露通用 SKILL.md；变体 skill 不可见。
type ModeAwareBackend struct {
	root string // <project>/.claude/skills
	mode string // content_mode
}

// NewModeAwareBackend 构造 mode 感知的 skill 后端。
func NewModeAwareBackend(root, mode string) *ModeAwareBackend {
	return &ModeAwareBackend{root: root, mode: mode}
}

// List 返回当前 mode 下可见的 skill 元数据（供工具描述渲染，模型自主发现入口）。
func (b *ModeAwareBackend) List(_ context.Context) ([]skill.FrontMatter, error) {
	visible, err := b.visible()
	if err != nil {
		return nil, err
	}
	out := make([]skill.FrontMatter, 0, len(visible))
	for _, m := range visible {
		out = append(out, skill.FrontMatter{Name: m.Name, Description: m.Description})
	}
	return out, nil
}

// Get 返回指定 skill 的正文与基目录（BuildContent 据此拼注入前缀）。
func (b *ModeAwareBackend) Get(_ context.Context, name string) (skill.Skill, error) {
	visible, err := b.visible()
	if err != nil {
		return skill.Skill{}, err
	}
	for _, m := range visible {
		if m.Name != name {
			continue
		}
		abs, err := filepath.Abs(filepath.Dir(m.Path))
		if err != nil {
			return skill.Skill{}, err
		}
		return skill.Skill{
			FrontMatter:   skill.FrontMatter{Name: m.Name, Description: m.Description},
			Content:       m.Body,
			BaseDirectory: abs,
		}, nil
	}
	return skill.Skill{}, fmt.Errorf("skill %q not found", name)
}

// visible 解析当前 mode 下所有可见 skill。
func (b *ModeAwareBackend) visible() ([]Meta, error) {
	dirs, err := skillDirs(b.root)
	if err != nil {
		return nil, err
	}
	var out []Meta
	for _, dir := range dirs {
		path, ok, err := resolveEntry(dir, b.mode)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		meta, err := ParseFile(path, filepath.Base(dir))
		if err != nil {
			return nil, err
		}
		out = append(out, meta)
	}
	return out, nil
}

// NewMiddleware 构造 skill 中间件（inline 模式）。
// BuildContent 钉死 "Base directory for this skill:" 首行，与 event_log 的 skill_invocation 契约一致。
func NewMiddleware(ctx context.Context, backend skill.Backend) (adk.ChatModelAgentMiddleware, error) {
	return skill.NewMiddleware(ctx, &skill.Config{
		Backend: backend,
		BuildContent: func(_ context.Context, s skill.Skill, _ string) (string, error) {
			return fmt.Sprintf("%s %s\n\n%s", baseDirectoryPrefix, s.BaseDirectory, strings.TrimLeft(s.Content, "\n")), nil
		},
	})
}
