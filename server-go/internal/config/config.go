// Package config 加载 SHOTWISE 运行配置。
//
// 来源优先级（与 Python 侧一致）：
//
//	DB（system_setting / provider_config）> env/.env > 代码常量
//
// env 只允许持有基础设施项；provider 密钥一旦出现在 env 中，启动即 fail-fast，
// 因为 fork 出去的沙箱子进程会继承父进程环境。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ProviderSecretKeys 是禁止出现在进程环境中的 provider 密钥键名。
// 单一真相源，与 Python 侧 lib/config/env_keys.py 的 PROVIDER_SECRET_KEYS 对应。
var ProviderSecretKeys = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
	"OPENAI_API_KEY", "OPENAI_BASE_URL",
	"DASHSCOPE_API_KEY", "ARK_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY",
	"XAI_API_KEY", "GROK_API_KEY", "MINIMAX_API_KEY", "KLING_API_KEY",
	"VIDU_API_KEY", "AGNES_API_KEY", "NEWAPI_API_KEY",
}

// Config 是进程级配置快照。运行期可变的部分（供应商/模型选择）一律存 DB，不在这里。
type Config struct {
	ListenHost string
	ListenPort int

	DataDir  string // 项目根目录，优先级见 ResolveDataDir
	DatabaseURL string

	AuthEnabled  bool
	AuthUsername string
	AuthPassword string
	TokenSecret  []byte

	PublishingEncryptionKey []byte

	CORSOrigins []string

	InDocker       bool
	SandboxEnabled bool

	LogLevel string
	LogDir   string

	// LLM 连接配置（env 级基础设施项；生产环境真正来源是 DB 的 provider_config / provider_credential）。
	// BaseURL 留空则走各厂商默认网关（OpenAI 兼容）。Anthropic Claude 可填 OpenAI 兼容网关地址。
	LLMModel   string
	LLMAPIKey  string
	LLMBaseURL string
}

// Load 从环境读取配置。返回值不含任何 provider 密钥。
func Load() (*Config, error) {
	cfg := &Config{
		ListenHost:   envOr("LISTEN_HOST", "127.0.0.1"),
		AuthUsername: envOr("AUTH_USERNAME", "admin"),
		AuthPassword: os.Getenv("AUTH_PASSWORD"),
		LogLevel:     envOr("LOG_LEVEL", "info"),
		DatabaseURL:  os.Getenv("DATABASE_URL"),
	}

	port := envOr("LISTEN_PORT", "18080")
	// 1241 在 Windows 上被系统保留，忽略并回落默认值。
	if port == "1241" {
		port = "18080"
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return nil, fmt.Errorf("invalid LISTEN_PORT %q: %w", port, err)
	}
	cfg.ListenPort = p

	cfg.AuthEnabled = parseBoolEnv(os.Getenv("AUTH_ENABLED"), true)
	cfg.DataDir = ResolveDataDir()
	if cfg.DatabaseURL == "" {
		cfg.DatabaseURL = "sqlite://" + filepath.Join(cfg.DataDir, ".shotwise.db")
	}
	cfg.CORSOrigins = splitList(os.Getenv("CORS_ORIGINS"))
	cfg.InDocker = detectDocker()
	cfg.LLMModel = envOr("LLM_MODEL", "gpt-4o-mini")
	cfg.LLMAPIKey = os.Getenv("LLM_API_KEY")
	cfg.LLMBaseURL = os.Getenv("LLM_BASE_URL")
	return cfg, nil
}

// detectDocker 通过 /.dockerenv 或 /proc/1/cgroup 判断是否运行在容器内。
func detectDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "docker") || strings.Contains(string(b), "kubepods")
}

// ResolveDataDir 决定项目根目录：SHOTWISE_DATA_DIR > AI_ANIME_PROJECTS > <cwd>/projects。
func ResolveDataDir() string {
	if v := os.Getenv("SHOTWISE_DATA_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("AI_ANIME_PROJECTS"); v != "" {
		return v
	}
	wd, err := os.Getwd()
	if err != nil {
		return "projects"
	}
	return filepath.Join(wd, "projects")
}

// AssertNoProviderSecrets 在进程环境中发现 provider 密钥时返回错误。
// 必须在任何子进程（ffmpeg / agent 工具 / 沙箱）启动之前调用。
func AssertNoProviderSecrets() error {
	var leaked []string
	for _, k := range ProviderSecretKeys {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			leaked = append(leaked, k)
		}
	}
	if len(leaked) > 0 {
		return fmt.Errorf("provider secrets must not be present in process environment: %s", strings.Join(leaked, ", "))
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseBoolEnv 解析 Python 侧同款开关语义：false/0/no/off 为假，其余为真。
func parseBoolEnv(v string, fallback bool) bool {
	if v == "" {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}

func splitList(v string) []string {
	if v == "" {
		return []string{"*"}
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}
