package agent

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// --- read_project_json：读取某项目的 project.json（KindRead，走 SafeJoin 围栏） ---

type readProjectJSONArgs struct {
	Project string `json:"project" jsonschema:"description=项目名（须匹配 ^[A-Za-z0-9-]+$）"`
}

type readProjectJSONTool struct{ dataDir string }

func (t *readProjectJSONTool) Name() string { return "read_project_json" }
func (t *readProjectJSONTool) Kind() Kind   { return KindRead }
func (t *readProjectJSONTool) Info() *schema.ToolInfo {
	return toolInfo("read_project_json",
		"读取指定项目的 project.json（单一真相源），返回其完整 JSON 内容。",
		&readProjectJSONArgs{})
}
func (t *readProjectJSONTool) Run(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a readProjectJSONArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return denyResult("invalid args: " + err.Error()), nil
	}
	if a.Project == "" {
		return denyResult("project is required"), nil
	}
	p, err := fs.SafeJoin(t.dataDir, false, a.Project, "project.json")
	if err != nil {
		return denyResult("cannot resolve project file: " + err.Error()), nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return denyResult("read failed: " + err.Error()), nil
	}
	return okResult(string(raw)), nil
}

// --- list_projects：列出数据目录下所有项目（KindRead） ---

type listProjectsTool struct{ dataDir string }

func (t *listProjectsTool) Name() string { return "list_projects" }
func (t *listProjectsTool) Kind() Kind   { return KindRead }
func (t *listProjectsTool) Info() *schema.ToolInfo {
	return toolInfo("list_projects", "列出数据目录下所有项目名（即顶层目录名）。", nil)
}
func (t *listProjectsTool) Run(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	entries, err := os.ReadDir(t.dataDir)
	if err != nil {
		return denyResult("read dir failed: " + err.Error()), nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	b, _ := json.Marshal(names)
	return okResult(string(b)), nil
}

// --- echo：最简单的业务动作工具，证明 工具调用 → 结果回填 闭环（KindAction） ---

type echoArgs struct {
	Message string `json:"message" jsonschema:"description=要回显的内容"`
}

type echoTool struct{}

func (t *echoTool) Name() string { return "echo" }
func (t *echoTool) Kind() Kind   { return KindAction }
func (t *echoTool) Info() *schema.ToolInfo {
	return toolInfo("echo", "把传入的 message 原样返回，用于链路自检。", &echoArgs{})
}
func (t *echoTool) Run(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a echoArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return denyResult("invalid args: " + err.Error()), nil
	}
	return okResult(a.Message), nil
}

// DefaultToolSet 构造内置工具集。guard 传 nil 表示暂不启用权限围栏（演示用），
// 生产环境必须传入实现了 PROTECTED_WRITE_RULES 的 AgentAccessPolicy。
func DefaultToolSet(dataDir string, guard Guard) *ToolSet {
	ts := NewToolSet()
	ts.Register(&readProjectJSONTool{dataDir: dataDir}, guard)
	ts.Register(&listProjectsTool{dataDir: dataDir}, guard)
	ts.Register(&echoTool{}, guard)
	return ts
}
