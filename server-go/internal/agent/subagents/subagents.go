// Package subagents 把 agent_runtime_profile/.claude/agents/*.md 装配为 Eino AgentTool
// （设计文档 03-agent-runtime-eino.md §11.5、§17.3）。
package subagents

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
)

// Definition 是一个 subagent 的 frontmatter + 正文。
type Definition struct {
	Name        string
	Description string
	Skills      []string // frontmatter skills：装配期内联进 Instruction（§17.3）
	Instruction string
}

// toolWhitelist 是每个 subagent 可用的工具名白名单（单一真相源，§17.3）。
// 名字未在工具池中注册（尚未实现）时该工具不注册，不报错。
var toolWhitelist = map[string][]string{
	"analyze-assets":              {"patch_project", "generate_assets", "read_file", "glob"},
	"create-episode-script":       {"generate_episode_script", "read_file"},
	"normalize-drama-script":      {"normalize_drama_script", "read_file", "write_file", "edit_file"},
	"split-narration-segments":    {"split_narration_segments", "read_file", "write_file", "edit_file"},
	"split-reference-video-units": {"split_reference_video_units", "read_file", "write_file", "edit_file"},
	"generate-assets":             {"generate_assets", "generate_storyboards", "generate_grid", "generate_narration_audio", "read_file", "execute"},
}

// ParseFile 解析 subagent md 的 frontmatter（name / description / skills 列表）与正文。
func ParseFile(path string) (Definition, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Definition{}, err
	}
	content := string(raw)
	if !strings.HasPrefix(content, "---") {
		return Definition{}, fmt.Errorf("subagent %s: 缺少 frontmatter", filepath.Base(path))
	}
	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return Definition{}, fmt.Errorf("subagent %s: frontmatter 未闭合", filepath.Base(path))
	}

	def := Definition{Instruction: strings.TrimSpace(parts[2])}
	inSkills := false
	for _, line := range strings.Split(parts[1], "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if inSkills && strings.HasPrefix(line, " ") {
			item := strings.TrimSpace(line)
			if after, ok := strings.CutPrefix(item, "- "); ok {
				def.Skills = append(def.Skills, strings.Trim(strings.TrimSpace(after), `"'`))
			}
			continue
		}
		inSkills = false
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch key {
		case "name":
			def.Name = value
		case "description":
			def.Description = value
		case "skills":
			inSkills = true
		}
	}
	if def.Name == "" || def.Description == "" {
		return Definition{}, fmt.Errorf("subagent %s: name 与 description 必填", filepath.Base(path))
	}
	return def, nil
}

// LoadDir 解析目录下全部 *.md subagent 定义（目录不存在返回空）。
func LoadDir(dir string) ([]Definition, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	defs := make([]Definition, 0, len(files))
	for _, f := range files {
		def, err := ParseFile(f)
		if err != nil {
			return nil, err
		}
		defs = append(defs, def)
	}
	return defs, nil
}

// Builder 把 Definition 装配为 AgentTool。
type Builder struct {
	Model model.ToolCallingChatModel
	// Pool 是可供白名单挑选的工具池（按工具名索引）。
	Pool map[string]tool.BaseTool
	// Handlers 是子 agent 必须接入的 middleware 链（guard 等，§17.3：围栏不豁免）。
	Handlers []adk.ChatModelAgentMiddleware
	// SkillBody 返回 skill 正文（装配期内联 skills 字段用）。
	SkillBody func(name string) (string, error)
}

// Build 装配单个 subagent 为 AgentTool。未登记白名单的 subagent 直接报错（fail loud）。
func (b *Builder) Build(ctx context.Context, def Definition) (tool.BaseTool, error) {
	names, ok := toolWhitelist[def.Name]
	if !ok {
		return nil, fmt.Errorf("subagent %s: 未登记工具白名单", def.Name)
	}
	var tools []tool.BaseTool
	for _, n := range names {
		if t, ok := b.Pool[n]; ok {
			tools = append(tools, t)
		}
	}
	instruction, err := b.withSkills(def)
	if err != nil {
		return nil, err
	}
	sub, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        def.Name,
		Description: def.Description,
		Instruction: instruction,
		Model:       b.Model,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools},
		},
		Handlers: b.Handlers,
	})
	if err != nil {
		return nil, fmt.Errorf("subagent %s: %w", def.Name, err)
	}
	return adk.NewAgentTool(ctx, sub), nil
}

// BuildAll 装配目录下全部 subagent，返回 AgentTool 列表与 名字集合（供事件适配器归属判定）。
func (b *Builder) BuildAll(ctx context.Context, defs []Definition) ([]tool.BaseTool, map[string]bool, error) {
	tools := make([]tool.BaseTool, 0, len(defs))
	names := make(map[string]bool, len(defs))
	for _, def := range defs {
		t, err := b.Build(ctx, def)
		if err != nil {
			return nil, nil, err
		}
		tools = append(tools, t)
		names[def.Name] = true
	}
	return tools, names, nil
}

func (b *Builder) withSkills(def Definition) (string, error) {
	if len(def.Skills) == 0 {
		return def.Instruction, nil
	}
	if b.SkillBody == nil {
		return "", fmt.Errorf("subagent %s: 声明了 skills 但未提供 SkillBody", def.Name)
	}
	var sb strings.Builder
	sb.WriteString(def.Instruction)
	for _, name := range def.Skills {
		body, err := b.SkillBody(name)
		if err != nil {
			return "", fmt.Errorf("subagent %s: 内联 skill %s: %w", def.Name, name, err)
		}
		sb.WriteString("\n\n# 参考 skill: " + name + "\n\n" + body)
	}
	return sb.String(), nil
}

// ToolPool 以工具名索引工具列表（工具名取自 Info）。
func ToolPool(ctx context.Context, tools []tool.BaseTool) (map[string]tool.BaseTool, error) {
	pool := make(map[string]tool.BaseTool, len(tools))
	for _, t := range tools {
		info, err := t.Info(ctx)
		if err != nil {
			return nil, err
		}
		pool[info.Name] = t
	}
	return pool, nil
}
