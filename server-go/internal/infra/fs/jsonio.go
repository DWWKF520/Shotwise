package fs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ErrNotFound 表示 JSON 文件不存在。
var ErrNotFound = errors.New("json file not found")

// ReadJSON 严格读取：文件缺失或解析失败都返回错误。
func ReadJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// ReadJSONOrNil 容错读取：文件缺失返回 (nil, nil)。
func ReadJSONOrNil(path string, dst any) (bool, error) {
	if err := ReadJSON(path, dst); err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// WriteJSONAtomic 原子写入：同目录临时文件 + os.Rename。
// 与 Python 侧 atomic_write_json 一致：ensure_ascii=false, indent=2。
func WriteJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	b = append(b, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".project.*.tmp")
	if err != nil {
		return fmt.Errorf("temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// 任何失败路径都要清掉临时文件，否则会留下 .project.*.tmp 垃圾。
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename to %s: %w", path, err)
	}
	tmpName = ""
	return nil
}

// KeyedLocks 是按字符串键复用的互斥锁集合。
// 对应 Python 侧的 _project_lock / _script_lock / versions.json 全局锁表。
type KeyedLocks struct {
	mu sync.Mutex
	m  map[string]*sync.RWMutex
}

func NewKeyedLocks() *KeyedLocks {
	return &KeyedLocks{m: make(map[string]*sync.RWMutex)}
}

func (k *KeyedLocks) Lock(key string) *sync.RWMutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	lk, ok := k.m[key]
	if !ok {
		lk = &sync.RWMutex{}
		k.m[key] = lk
	}
	return lk
}

// WithLock 在锁保护下执行 fn。
func (k *KeyedLocks) WithLock(key string, fn func()) {
	lk := k.Lock(key)
	lk.Lock()
	defer lk.Unlock()
	fn()
}
