package tools

import (
	"context"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/repository"
)

// 跨分类共享的依赖接口。各工具分类子包按需引用；满足方均为 service / queue / config
// 层的具体实现（注释标注），子包内自有的窄接口不在此登记。

// AssetQueue 是生成类工具的任务队列能力（*queue.Queue 满足）。
type AssetQueue interface {
	EnqueueTask(ctx context.Context, p repository.EnqueueParams) (repository.EnqueueResult, error)
	GetTask(ctx context.Context, taskID string) (*repository.TaskData, error)
	IsWorkerOnline(ctx context.Context, name string) (bool, error)
}

// AssetProjects 是资产类工具读取项目与定位项目目录的能力（*service.ProjectStore 满足）。
type AssetProjects interface {
	Get(name string) (*project.ProjectFile, error)
	Dir(name string) (string, error)
}

// GenerationProjects 是生成类工具的项目读取能力（*service.ProjectStore 满足）。
type GenerationProjects interface {
	Get(name string) (*project.ProjectFile, error)
	Dir(name string) (string, error)
	ReadScriptJSON(name, filename string) (map[string]any, error)
}

// ScriptStore 是剧本锁内读-改-写能力（*service.ProjectStore 满足）。
type ScriptStore interface {
	UpdateScript(project, scriptFilename string, mutate func(script map[string]any) error) error
}

// VideoCapabilityReader 解析项目当前视频身份的能力（*config.Resolver 满足）。
type VideoCapabilityReader interface {
	VideoCapabilitiesForProject(ctx context.Context, project *project.ProjectFile) (config.VideoCapabilityView, error)
}

// TextGenerationProjects 是剧本生成类工具的项目依赖（*service.ProjectStore 满足）。
type TextGenerationProjects interface {
	Get(name string) (*project.ProjectFile, error)
	Dir(name string) (string, error)
	SaveScript(project, scriptFilename string, script map[string]any) error
	// Update 供 step1 存量迁移在项目锁内平移 / 失效审阅确认记录（service.ScriptGenStore 要求）。
	Update(name string, mutate func(*project.ProjectFile) error) error
}
