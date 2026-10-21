// envfile.go — .env 加载与 AUTH_PASSWORD 自动生成。
//
// 对齐 Python 侧两处行为：
//   - lib/env_init.py：启动时从仓库根加载 .env，已存在的进程环境变量优先；
//   - server/auth.py::ensure_auth_password：认证开启且 AUTH_PASSWORD 为空时
//     自动生成 16 位随机密码，注入进程环境并回写 .env，日志警告提示查看。
package config

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// passwordAlphabet 是自动生成密码的字符集（字母 + 数字，与 Python 侧一致）。
const passwordAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// LoadDotenv 从 cwd 向上回溯查找 .env，把其中尚未出现在进程环境里的键注入。
// 已存在的环境变量优先（python-dotenv 默认语义）。找不到或不可读时静默跳过：
// .env 是可选配置来源，不是启动前提。
//
// 注意：provider 密钥若随 .env 进入进程环境，会被 AssertNoProviderSecrets
// 在启动期 fail-fast —— 与 Python 侧红线一致，调用方须在本函数之后断言。
func LoadDotenv() {
	path := findEnvFile()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		line = strings.TrimPrefix(line, "export ")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value = unquoteEnvValue(strings.TrimSpace(value))
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
}

// EnsureAuthPassword 确保认证开启时有可用密码：
//
//   - 认证关闭：仅打警告（任意凭据可通过校验，勿暴露到不可信网络），返回空串；
//   - AUTH_PASSWORD 已设置：原样返回，不做任何写入；
//   - 为空：生成 16 位随机密码，注入进程环境并回写 envPath，随后警告提示
//     用户到 .env 查看（不把密码本身打进日志）。
//
// envPath 为空时自动定位：优先已存在的 .env；否则回溯仓库根（含
// pyproject.toml / .git 的目录，对应 Python 侧 PROJECT_ROOT 锚点）；再退回 cwd。
func EnsureAuthPassword(enabled bool, envPath string, log *slog.Logger) (string, error) {
	if !enabled {
		log.Warn("AUTH_ENABLED=false：认证已关闭，任意凭据均可通过校验，请勿暴露到不可信网络")
		return "", nil
	}
	if pwd := os.Getenv("AUTH_PASSWORD"); pwd != "" {
		return pwd, nil
	}
	pwd, err := generatePassword(16)
	if err != nil {
		return "", fmt.Errorf("generate auth password: %w", err)
	}
	if err := os.Setenv("AUTH_PASSWORD", pwd); err != nil {
		return "", fmt.Errorf("set AUTH_PASSWORD: %w", err)
	}
	if envPath == "" {
		envPath = resolveEnvPathForWrite()
	}
	writeAuthPasswordToEnv(envPath, pwd, log)
	log.Warn("已自动生成认证密码，请查看 .env 文件中的 AUTH_PASSWORD 字段", "path", envPath)
	return pwd, nil
}

// generatePassword 生成 n 位字母数字随机密码（crypto/rand，无取模偏差）。
func generatePassword(n int) (string, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = passwordAlphabet[idx.Int64()]
	}
	return string(out), nil
}

// writeAuthPasswordToEnv 把密码回写 .env：替换首个 AUTH_PASSWORD= 行，没有则追加。
// 历史 .env 可能是本地编码（非 UTF-8）——不覆写以免丢失用户内容，仅警告跳过；
// 此时进程内密码已生效，只是不持久化。所有写失败都不阻断启动。
func writeAuthPasswordToEnv(path, pwd string, log *slog.Logger) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Warn("无法写入 .env 文件", "path", path)
			return
		}
		if werr := os.WriteFile(path, []byte("AUTH_PASSWORD="+pwd+"\n"), 0o600); werr != nil {
			log.Warn("无法写入 .env 文件", "path", path)
		}
		return
	}
	if !utf8.Valid(data) {
		log.Warn("无法以 UTF-8 解码 .env，跳过 AUTH_PASSWORD 自动回写；"+
			"请将该文件转存为 UTF-8 后重启以持久化生成的密码", "path", path)
		return
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	replaced := false
	for i, line := range lines {
		if !replaced && strings.HasPrefix(strings.TrimSpace(line), "AUTH_PASSWORD=") {
			lines[i] = "AUTH_PASSWORD=" + pwd
			replaced = true
		}
	}
	if !replaced {
		// 去掉末尾空行再追加，避免引入多余空行。
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "AUTH_PASSWORD="+pwd)
	}
	content := strings.Join(lines, "\n") + "\n"
	// 原地截断写入保留 inode，兼容 Docker bind mount。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		log.Warn("无法写入 .env 文件", "path", path)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(content); err != nil {
		log.Warn("无法写入 .env 文件", "path", path)
	}
}

// unquoteEnvValue 去掉成对的引号（.env 惯例：带空格的值用引号包裹）。
func unquoteEnvValue(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// findEnvFile 从 cwd 向上回溯查找已存在的 .env。
func findEnvFile() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	dir := wd
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, ".env")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// resolveEnvPathForWrite 决定自动生成密码的回写位置：
// 已有 .env 原地写；否则回溯仓库根（含 pyproject.toml / .git 的目录）；
// 都没有则退回 cwd。
func resolveEnvPathForWrite() string {
	if p := findEnvFile(); p != "" {
		return p
	}
	wd, err := os.Getwd()
	if err != nil {
		return ".env"
	}
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "pyproject.toml")); err == nil {
			return filepath.Join(dir, ".env")
		}
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
			return filepath.Join(dir, ".env")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Join(wd, ".env")
}
