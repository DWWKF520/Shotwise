package review

import (
	"github.com/shotwise/shotwise/internal/domain/project"
)

// 与 assets 测试共享的最小替身副本（Go 测试不能跨包复用未导出类型）。

type fakeAssetProjects struct {
	pf  *project.ProjectFile
	dir string
}

func (f *fakeAssetProjects) Get(string) (*project.ProjectFile, error) { return f.pf, nil }
func (f *fakeAssetProjects) Dir(string) (string, error)               { return f.dir, nil }
