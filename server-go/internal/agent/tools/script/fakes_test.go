package script

import (
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// 与生成类测试共享的最小替身副本（Go 测试不能跨包复用未导出类型）。

// fakeGenerationProjects 提供生成类工具所需的项目读取面。
type fakeGenerationProjects struct {
	pf      *project.ProjectFile
	dir     string
	scripts map[string]map[string]any
}

func (f *fakeGenerationProjects) Get(string) (*project.ProjectFile, error) { return f.pf, nil }
func (f *fakeGenerationProjects) Dir(string) (string, error)               { return f.dir, nil }

// Update 就地 mutate 项目，使该替身同时满足分集规划工具的 servicescript.EpisodeStore。
func (f *fakeGenerationProjects) Update(_ string, mutate func(*project.ProjectFile) error) error {
	return mutate(f.pf)
}

func (f *fakeGenerationProjects) ReadScriptJSON(_, filename string) (map[string]any, error) {
	script, ok := f.scripts[filename]
	if !ok {
		return nil, fs.ErrNotFound
	}
	return script, nil
}
