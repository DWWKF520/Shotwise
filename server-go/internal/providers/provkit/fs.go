package provkit

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic 先写同目录临时文件再 rename，避免中途失败留下半截产物。
func WriteFileAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create output dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".shotwise-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// rename 成功后该文件已不存在，删除失败无需处理。
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp image: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp image: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename image into place: %w", err)
	}
	return nil
}

// IsFile 报告路径是否指向一个常规文件（对应 Python pathlib.Path.is_file）。
func IsFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
