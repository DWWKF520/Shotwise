// Package service 承载业务服务。
package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// ProjectStore 管理 projects/ 下的项目目录与 project.json。
//
// 所有路径拼接都过 fs.SafeJoin，杜绝穿越；project.json 写入一律原子替换。
type ProjectStore struct {
	root  string
	locks *fs.KeyedLocks
}

// NewProjectStore 构造项目存储。root 已由 config.ResolveDataDir 决定。
func NewProjectStore(root string) *ProjectStore {
	return &ProjectStore{root: root, locks: fs.NewKeyedLocks()}
}

// Root 返回项目根目录。
func (s *ProjectStore) Root() string { return s.root }

// Dir 返回项目目录的绝对路径。
func (s *ProjectStore) Dir(name string) (string, error) {
	return fs.SafeJoin(s.root, true, name)
}

// ErrProjectExists 项目已存在。
var ErrProjectExists = errors.New("project already exists")

// Create 创建项目：建目录骨架 + 写 project.json。
func (s *ProjectStore) Create(name, title string, mode domain.ContentMode, genMode domain.GenerationMode, style string) (*domain.ProjectFile, error) {
	normalized, err := domain.ValidateProjectName(name)
	if err != nil {
		return nil, err
	}
	dir, err := s.Dir(normalized)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(filepath.Join(dir, "project.json")); statErr == nil {
		return nil, ErrProjectExists
	}

	pf := domain.NewProjectFile(title, mode, genMode)
	pf.Style = style
	if style == "" {
		pf.Style = "写实"
	}

	s.locks.WithLock(normalized, func() {
		for _, sub := range domain.ProjectSubdirs {
			err = os.MkdirAll(filepath.Join(dir, sub), 0o755)
			if err != nil {
				return
			}
		}
		if err != nil {
			return
		}
		err = s.writeProject(normalized, pf)
	})
	if err != nil {
		return nil, fmt.Errorf("create project %s: %w", normalized, err)
	}
	return pf, nil
}

// Exists 判断项目是否存在。
func (s *ProjectStore) Exists(name string) bool {
	dir, err := s.Dir(name)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, "project.json"))
	return err == nil
}

// Get 读取 project.json。
func (s *ProjectStore) Get(name string) (*domain.ProjectFile, error) {
	dir, err := s.Dir(name)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "project.json")
	pf := &domain.ProjectFile{}
	if err := fs.ReadJSON(path, pf); err != nil {
		return nil, err
	}
	return pf, nil
}

// Save 原子写回 project.json，并刷新 updated_at。
func (s *ProjectStore) Save(name string, pf *domain.ProjectFile) error {
	if err := pf.Validate(); err != nil {
		return err
	}
	pf.Metadata.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.locks.WithLock(name, func() {
		_ = s.writeProject(name, pf)
	})
	return nil
}

func (s *ProjectStore) writeProject(name string, pf *domain.ProjectFile) error {
	dir, err := s.Dir(name)
	if err != nil {
		return err
	}
	return fs.WriteJSONAtomic(filepath.Join(dir, "project.json"), pf)
}

// List 列出项目目录名（只认含 project.json 的目录），按名称排序。
func (s *ProjectStore) List() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n := e.Name()
		if len(n) > 0 && n[0] == '.' {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.root, n, "project.json")); err == nil {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// Delete 删除整个项目目录。
func (s *ProjectStore) Delete(name string) error {
	dir, err := s.Dir(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "project.json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fs.ErrNotFound
		}
		return err
	}
	s.locks.WithLock(name, func() {
		err = os.RemoveAll(dir)
	})
	return err
}
