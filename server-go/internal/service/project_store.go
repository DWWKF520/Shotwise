// Package service 承载业务服务。
package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
		err = s.writeProject(dir, pf)
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
	return s.load(dir)
}

// WithProjectLock 在项目锁内执行 fn。
//
// 资产重命名要跨 project.json + scripts/ + drafts/ + 磁盘文件一起改，Python 侧按
// 「剧本锁 → 草稿锁 → 项目锁」的顺序加锁；这里的锁是进程内 keyed mutex，
// 用一把项目锁覆盖整个级联即可保证同一进程内不并发写，跨进程协同仍靠原子写。
func (s *ProjectStore) WithProjectLock(name string, fn func() error) error {
	var err error
	s.locks.WithLock(name, func() { err = fn() })
	return err
}

// Update 在项目锁内做「读 → 改 → 不更坏校验 → 原子写」，对应 Python 的 update_project。
func (s *ProjectStore) Update(name string, mutate func(*domain.ProjectFile) error) error {
	return s.WithProjectLock(name, func() error {
		dir, err := s.Dir(name)
		if err != nil {
			return err
		}
		pf, err := s.load(dir)
		if err != nil {
			return err
		}
		allow := pf.ValidateErrors()
		if err := mutate(pf); err != nil {
			return err
		}
		return s.persistUnlocked(dir, pf, allow)
	})
}

// ProjectInvalidError 表示本次写入会让 project.json 的结构比改之前更坏。
//
// 判定是差集而不是「有没有错」：存量项目可能自带历史遗留问题（迁移链里就有专门修补
// 事故的那批），若要求全绿才允许写，老项目的资产就再也改不动了。
type ProjectInvalidError struct{ Errors []string }

func (e *ProjectInvalidError) Error() string { return strings.Join(e.Errors, "; ") }

// Save 原子写回 project.json，并刷新 updated_at。新建路径专用（严格校验）。
func (s *ProjectStore) Save(name string, pf *domain.ProjectFile) error {
	dir, err := s.Dir(name)
	if err != nil {
		return err
	}
	return s.WithProjectLock(name, func() error {
		if err := pf.Validate(); err != nil {
			return err
		}
		return s.touchAndWrite(dir, pf)
	})
}

// persistUnlocked 供已持项目锁的跨文件级联（资产改名）复用同一套「不更坏」判定。
func (s *ProjectStore) persistUnlocked(dir string, pf *domain.ProjectFile, allow []string) error {
	if fresh := domain.NewErrors(pf.ValidateErrors(), allow); len(fresh) > 0 {
		return &ProjectInvalidError{Errors: fresh}
	}
	return s.touchAndWrite(dir, pf)
}

func (s *ProjectStore) touchAndWrite(dir string, pf *domain.ProjectFile) error {
	pf.Metadata.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return fs.WriteJSONAtomic(filepath.Join(dir, "project.json"), pf)
}

// load 读取 project.json，不取锁（调用方须已持锁）。
func (s *ProjectStore) load(dir string) (*domain.ProjectFile, error) {
	pf := &domain.ProjectFile{}
	if err := fs.ReadJSON(filepath.Join(dir, "project.json"), pf); err != nil {
		return nil, err
	}
	return pf, nil
}

// writeProject 原子写 project.json，不做校验也不戳 updated_at —— 创建路径上
// style/aspect_ratio 等字段还没补齐，校验会误拒。
func (s *ProjectStore) writeProject(dir string, pf *domain.ProjectFile) error {
	return fs.WriteJSONAtomic(filepath.Join(dir, "project.json"), pf)
}

// ScriptFileNames 列出 scripts/ 下的剧本文件名（已排序）。
func (s *ProjectStore) ScriptFileNames(name string) ([]string, error) {
	dir, err := s.ScriptsDir(name)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// ScriptsDir 返回 scripts/ 的绝对路径。
func (s *ProjectStore) ScriptsDir(name string) (string, error) {
	dir, err := s.Dir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "scripts"), nil
}

// ReadScriptJSON 按文件名读取剧本（含存量迁移，迁移结果在剧本锁内回写落盘），
// 文件名必须是单段（防穿越）。
//
// 对应 Python ProjectManager.load_script：先无锁读一次——绝大多数剧本早已完成收编，
// 这条路径不取锁、不建锁文件；发生了迁移才进剧本锁，锁内重读一次再回写，读-改-写
// 不给并发写者留覆盖窗口。不走 writeScript：迁移只是格式收编，不应刷新
// metadata.updated_at、不触发 project.json 同步。
func (s *ProjectStore) ReadScriptJSON(name, filename string) (map[string]any, error) {
	// 先归一再防穿越（与 Python load_script 的 normalize_script_filename → _safe_subpath 同序）：
	// project.json 的 episodes[].script_file 持久形态带 scripts/ 前缀，归一后剩余的多段
	// / 穿越片段仍被 Base 检查拒绝。
	filename = NormalizeScriptFilename(filename)
	if filename == "" || filepath.Base(filename) != filename {
		return nil, &fs.PathTraversalError{Msg: "invalid script file name"}
	}
	dir, err := s.ScriptsDir(name)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, filename)
	script, migrated, err := readScriptMigrated(path)
	if err != nil {
		return nil, err
	}
	if !migrated {
		return script, nil
	}
	err = s.WithScriptLock(name, filename, func(_, _ string) error {
		script, migrated, err = readScriptMigrated(path)
		if err != nil {
			return err
		}
		if migrated {
			return fs.WriteJSONAtomic(path, script)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return script, nil
}

// WriteScriptJSON 原子写回剧本。
func (s *ProjectStore) WriteScriptJSON(name, filename string, payload map[string]any) error {
	if filepath.Base(filename) != filename {
		return &fs.PathTraversalError{Msg: "invalid script file name"}
	}
	dir, err := s.ScriptsDir(name)
	if err != nil {
		return err
	}
	return fs.WriteJSONAtomic(filepath.Join(dir, filename), payload)
}

// DraftJSONPaths 列出 drafts/<episode>/ 下的草稿 JSON 绝对路径（已排序）。
// 草稿按名命名的产物同样引用资产名，改名要一并改写。
func (s *ProjectStore) DraftJSONPaths(name string) ([]string, error) {
	dir, err := s.Dir(name)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(dir, "drafts")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".json") {
				out = append(out, filepath.Join(root, e.Name(), f.Name()))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *ProjectStore) readJSONObject(p string) (map[string]any, error) {
	var payload map[string]any
	if err := fs.ReadJSON(p, &payload); err != nil {
		return nil, err
	}
	return payload, nil
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
