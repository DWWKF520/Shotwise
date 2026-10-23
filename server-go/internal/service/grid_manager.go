// 宫格记录的落盘 CRUD（对应 lib/grid_manager.py）：一条记录一个
// {project}/grids/{grid_id}.json，联合图落 {project}/grids/{grid_id}.png。
package service

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/shotwise/shotwise/internal/domain/grid"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// GridManager 是单个项目目录上的宫格记录账本。
type GridManager struct {
	dir string
}

// NewGridManager 构造；grids 目录按需创建（与 Python 侧 __init__ 一致）。
func NewGridManager(projectDir string) (*GridManager, error) {
	dir := filepath.Join(projectDir, "grids")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &GridManager{dir: dir}, nil
}

// path 取 grids 下的记录/图片路径。gridID 来自 URL 路径参数，先卡格式白名单再过越界校验。
func (m *GridManager) path(gridID, suffix string) (string, error) {
	if !grid.ValidGridID(gridID) {
		return "", fmt.Errorf("非法宫格 ID: %q", gridID)
	}
	resolved, err := fs.SafeJoin(m.dir, false, gridID+suffix)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// ImagePath 取联合图路径；调用方一律经此取路径，不自行拼接。
func (m *GridManager) ImagePath(gridID string) (string, error) {
	return m.path(gridID, ".png")
}

// Save 写入记录 JSON。
func (m *GridManager) Save(grid *grid.GridGeneration) error {
	path, err := m.path(grid.ID, ".json")
	if err != nil {
		return err
	}
	return fs.WriteJSONAtomic(path, grid)
}

// Get 读一条记录；不存在返回 ok=false。
func (m *GridManager) Get(gridID string) (*grid.GridGeneration, bool, error) {
	path, err := m.path(gridID, ".json")
	if err != nil {
		return nil, false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	grid, err := grid.GridGenerationFromJSON(raw)
	if err != nil {
		return nil, false, err
	}
	return grid, true, nil
}

// Delete 删除记录与其联合图；记录不存在返回 ok=false。
func (m *GridManager) Delete(gridID string) (bool, error) {
	path, err := m.path(gridID, ".json")
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if imagePath, err := m.ImagePath(gridID); err == nil {
		if err := os.Remove(imagePath); err != nil && !os.IsNotExist(err) {
			return false, err
		}
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}

// ListAll 返回全部记录，按 created_at 升序；损坏记录跳过（与 Python 侧告警后跳过同语义）。
func (m *GridManager) ListAll() ([]*grid.GridGeneration, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var grids []*grid.GridGeneration
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if len(name) < 10 || name[:5] != "grid_" || filepath.Ext(name) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(m.dir, name))
		if err != nil {
			continue
		}
		grid, err := grid.GridGenerationFromJSON(raw)
		if err != nil {
			continue
		}
		grids = append(grids, grid)
	}
	sort.SliceStable(grids, func(i, j int) bool { return grids[i].CreatedAt < grids[j].CreatedAt })
	return grids, nil
}
