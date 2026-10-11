// Package fs 提供文件系统安全原语：路径越界防护、原子 JSON 读写、按路径的键控锁。
package fs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// PathTraversalError 表示路径越界或非法。
// 继承语义上等价于 Python 侧的 PathTraversalError(ValueError)：
// 调用方既能精确捕获，也能被通用错误处理兜住。
type PathTraversalError struct{ Msg string }

func (e *PathTraversalError) Error() string { return e.Msg }

// ErrPathTraversal 供 errors.Is 使用。
var ErrPathTraversal = &PathTraversalError{Msg: "path traversal"}

// SafeJoin 把 parts 拼到 base 下，并保证结果仍位于 base 之内。
//
// 刻意用 realpath + 前缀字符串比较，而非 filepath.Rel：
// 前者是静态扫描工具能识别的 sanitizer 收敛模式，防御强度等价。
//
// 若 parts 中出现绝对路径，等价于「校验该绝对路径是否在 base 内」。
func SafeJoin(base string, allowBase bool, parts ...string) (string, error) {
	baseReal, err := realpath(base)
	if err != nil {
		return "", &PathTraversalError{Msg: "invalid base path"}
	}
	// 预补末尾分隔符，避免 base 是文件系统根或 Windows 盘符根时拼出 "//"，
	// 导致所有子路径被误判越界。
	basePrefix := baseReal
	if !strings.HasSuffix(basePrefix, string(os.PathSeparator)) {
		basePrefix += string(os.PathSeparator)
	}

	candidate := filepath.Join(append([]string{baseReal}, parts...)...)
	candidateReal, err := realpath(candidate)
	if err != nil {
		// NUL 字节等非法字符会在这里抛原生错误，统一收敛，避免各调用点退化成 500。
		return "", &PathTraversalError{Msg: "invalid path"}
	}
	if candidateReal == baseReal {
		if !allowBase {
			return "", &PathTraversalError{Msg: "path resolves to base"}
		}
		return candidateReal, nil
	}
	if !strings.HasPrefix(candidateReal, basePrefix) {
		return "", &PathTraversalError{Msg: "path escapes base directory"}
	}
	return candidateReal, nil
}

// TrySafeJoin 是静默版：越界、不存在或脏数据一律返回空串。
func TrySafeJoin(base string, allowBase bool, parts ...string) string {
	p, err := SafeJoin(base, allowBase, parts...)
	if err != nil {
		return ""
	}
	return p
}

// SafeResolve 要求结果必须是已存在的文件。
func SafeResolve(base, rel string) (string, error) {
	p, err := SafeJoin(base, false, rel)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", &PathTraversalError{Msg: "path does not exist"}
	}
	if st.IsDir() {
		return "", &PathTraversalError{Msg: "path is a directory"}
	}
	return p, nil
}

// SafeExists 布尔版。
func SafeExists(base, rel string) bool {
	_, err := SafeResolve(base, rel)
	return err == nil
}

// realpath 解析符号链接；不存在时返回拼接结果本身而非报错，
// 因为 SafeJoin 需要支持「目标尚不存在」的写入场景。
func realpath(p string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if errors.Is(err, os.ErrNotExist) {
		abs, absErr := filepath.Abs(p)
		if absErr != nil {
			return "", absErr
		}
		return filepath.Clean(abs), nil
	}
	return "", err
}
