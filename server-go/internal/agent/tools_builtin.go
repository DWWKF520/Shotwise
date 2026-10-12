package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/infra/fs"
)

// v1 内置业务工具（对应设计文档 37 工具清单中的只读子集 + 链路自检）。
// 全量 37 工具与 generate_* 动作工具随后续增量接入；本文件只保留
// 与文件系统/项目真相源相关的最小闭环，供 TurnLoop 端到端验证。

// --- read_project_json ---

type readProjectJSONIn struct {
	Project string `json:"project" jsonschema:"description=项目名（须匹配 ^[A-Za-z0-9-]+$）"`
}

func newReadProjectJSONTool(dataDir string) (tool.InvokableTool, error) {
	return toolutils.InferTool("read_project_json",
		"读取指定项目的 project.json（单一真相源），返回其完整 JSON 内容。",
		func(ctx context.Context, in readProjectJSONIn) (string, error) {
			if in.Project == "" {
				return denyEnvelope("project is required"), nil
			}
			p, err := fs.SafeJoin(dataDir, false, in.Project, "project.json")
			if err != nil {
				return denyEnvelope("cannot resolve project file: " + err.Error()), nil
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				return denyEnvelope("read failed: " + err.Error()), nil
			}
			return okEnvelope(string(raw)), nil
		})
}

// --- list_projects ---

type listProjectsIn struct{}

func newListProjectsTool(dataDir string) (tool.InvokableTool, error) {
	return toolutils.InferTool("list_projects",
		"列出数据目录下所有项目名（即顶层目录名）。",
		func(ctx context.Context, _ listProjectsIn) (string, error) {
			entries, err := os.ReadDir(dataDir)
			if err != nil {
				return denyEnvelope("read dir failed: " + err.Error()), nil
			}
			var names []string
			for _, e := range entries {
				if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
					names = append(names, e.Name())
				}
			}
			sort.Strings(names)
			b, _ := json.Marshal(names)
			return okEnvelope(string(b)), nil
		})
}

// --- echo（链路自检）---

type echoIn struct {
	Message string `json:"message" jsonschema:"description=要回显的内容"`
}

func newEchoTool() (tool.InvokableTool, error) {
	return toolutils.InferTool("echo",
		"把传入的 message 原样返回，用于链路自检。",
		func(ctx context.Context, in echoIn) (string, error) {
			return okEnvelope(in.Message), nil
		})
}

// BuiltinTools 构造 v1 内置工具集。project 参数用于后续工具的项目绑定
// （37 工具的 project_name 闭包绑定，防 prompt injection 改项目）。
func BuiltinTools(dataDir, project string) ([]tool.BaseTool, error) {
	readJSON, err := newReadProjectJSONTool(dataDir)
	if err != nil {
		return nil, fmt.Errorf("build read_project_json: %w", err)
	}
	listProjects, err := newListProjectsTool(dataDir)
	if err != nil {
		return nil, fmt.Errorf("build list_projects: %w", err)
	}
	echo, err := newEchoTool()
	if err != nil {
		return nil, fmt.Errorf("build echo: %w", err)
	}
	return []tool.BaseTool{readJSON, listProjects, echo}, nil
}
