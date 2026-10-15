// Package workspace 提供智能体的项目工作区能力（设计文档 03-agent-runtime-eino.md §4.3、§11.4）：
//   - Backend：eino filesystem.Backend 实现，所有路径约束在项目根之内（SafeJoin）
//   - Shell：execute 工具执行器，只接受 Bash 白名单命令，且不经过 shell 解释
//
// 文件读写围栏（只读/保护文件/代码扩展名）仍由 guard middleware 的 policy 裁决；
// 本包的根约束是纵深防御，二者任一拒绝即失败。
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/filesystem"
	fsmw "github.com/cloudwego/eino/adk/middlewares/filesystem"

	"github.com/shotwise/shotwise/internal/agent/policy"
	infrafs "github.com/shotwise/shotwise/internal/infra/fs"
)

// 文件系统中间件注册的工具名（eino 默认命名，与 policy 的分派表一致）。
const (
	ToolLs      = "ls"
	ToolRead    = "read_file"
	ToolWrite   = "write_file"
	ToolEdit    = "edit_file"
	ToolGlob    = "glob"
	ToolGrep    = "grep"
	ToolExecute = "execute"
)

// AllTools 是全部文件系统工具名。
var AllTools = []string{ToolLs, ToolRead, ToolWrite, ToolEdit, ToolGlob, ToolGrep, ToolExecute}

const (
	maxGrepMatches = 500
	maxOutputBytes = 50_000
	shellTimeout   = 10 * time.Minute
	maxBinaryProbe = 8000
)

// Backend 是项目根内的文件后端。root 必须是绝对路径。
type Backend struct {
	root string
}

// NewBackend 构造以 root 为根的后端；root 解析符号链接后作为约束基准。
func NewBackend(root string) (*Backend, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return &Backend{root: abs}, nil
}

// resolve 把用户给出的路径（相对或绝对）解析为根内的真实路径。越界返回错误。
func (b *Backend) resolve(p string) (string, error) {
	if p == "" {
		p = "."
	}
	rel := p
	if filepath.IsAbs(p) {
		r, err := filepath.Rel(b.root, p)
		if err != nil {
			return "", fmt.Errorf("access denied: %s is outside the project", p)
		}
		rel = r
	}
	resolved, err := infrafs.SafeJoin(b.root, true, rel)
	if err != nil {
		return "", fmt.Errorf("access denied: %s is outside the project", p)
	}
	return resolved, nil
}

// relOf 返回相对项目根的斜杠路径（用于回显给模型）。
func (b *Backend) relOf(abs string) string {
	rel, err := filepath.Rel(b.root, abs)
	if err != nil {
		return filepath.ToSlash(abs)
	}
	return filepath.ToSlash(rel)
}

func fileInfoOf(b *Backend, abs string, st fs.FileInfo) filesystem.FileInfo {
	return filesystem.FileInfo{
		Path:       b.relOf(abs),
		IsDir:      st.IsDir(),
		Size:       st.Size(),
		ModifiedAt: st.ModTime().UTC().Format(time.RFC3339),
	}
}

// LsInfo 列出目录内容。
func (b *Backend) LsInfo(_ context.Context, req *filesystem.LsInfoRequest) ([]filesystem.FileInfo, error) {
	dir, err := b.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]filesystem.FileInfo, 0, len(entries))
	for _, e := range entries {
		st, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, fileInfoOf(b, filepath.Join(dir, e.Name()), st))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Read 读取文件；Offset/Limit 按 1-based 行号切片，Limit=0 表示到末尾。
func (b *Backend) Read(_ context.Context, req *filesystem.ReadRequest) (*filesystem.FileContent, error) {
	p, err := b.resolve(req.FilePath)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s is a directory", req.FilePath)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if req.Offset <= 1 && req.Limit <= 0 {
		return &filesystem.FileContent{Content: string(raw)}, nil
	}
	lines := strings.SplitAfter(string(raw), "\n")
	start := req.Offset - 1
	if start < 0 {
		start = 0
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if req.Limit > 0 && start+req.Limit < end {
		end = start + req.Limit
	}
	return &filesystem.FileContent{Content: strings.Join(lines[start:end], "")}, nil
}

// Write 写入文件（父目录按需创建）。
func (b *Backend) Write(_ context.Context, req *filesystem.WriteRequest) error {
	p, err := b.resolve(req.FilePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(req.Content), 0o644)
}

// Edit 替换文本：默认要求 OldString 恰好出现一次；ReplaceAll 时替换全部。
func (b *Backend) Edit(_ context.Context, req *filesystem.EditRequest) error {
	if req.OldString == "" {
		return errors.New("old_string must not be empty")
	}
	p, err := b.resolve(req.FilePath)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	content := string(raw)
	count := strings.Count(content, req.OldString)
	switch {
	case count == 0:
		return errors.New("old_string not found in file")
	case count > 1 && !req.ReplaceAll:
		return fmt.Errorf("old_string appears %d times; set replace_all or provide more context", count)
	}
	n := 1
	if req.ReplaceAll {
		n = -1
	}
	updated := strings.Replace(content, req.OldString, req.NewString, n)
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(updated), st.Mode().Perm())
}

// GlobInfo 按 doublestar 模式匹配（支持 **）。
func (b *Backend) GlobInfo(_ context.Context, req *filesystem.GlobInfoRequest) ([]filesystem.FileInfo, error) {
	base, err := b.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	matches, err := doublestar.Glob(os.DirFS(base), req.Pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid glob pattern: %w", err)
	}
	out := make([]filesystem.FileInfo, 0, len(matches))
	for _, m := range matches {
		abs := filepath.Join(base, filepath.FromSlash(m))
		st, err := os.Stat(abs)
		if err != nil {
			continue
		}
		out = append(out, fileInfoOf(b, abs, st))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// GrepRaw 逐行正则搜索（不依赖外部 ripgrep）。跳过隐藏目录与二进制文件。
// 限制：EnableMultiline 不支持跨行匹配（按行判定）；Before/AfterLines 按上下文行拼入 Content。
func (b *Backend) GrepRaw(_ context.Context, req *filesystem.GrepRequest) ([]filesystem.GrepMatch, error) {
	base, err := b.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	pattern := req.Pattern
	if req.CaseInsensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex: %w", err)
	}
	ext := ""
	if req.FileType != "" {
		ext = "." + strings.TrimPrefix(req.FileType, ".")
	}

	var out []filesystem.GrepMatch
	walkErr := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != base && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		rel := b.relOf(p)
		if ext != "" && filepath.Ext(p) != ext {
			return nil
		}
		if req.Glob != "" {
			ok, _ := doublestar.Match(req.Glob, rel)
			if !ok {
				return nil
			}
		}
		raw, err := os.ReadFile(p)
		if err != nil || isBinary(raw) {
			return nil
		}
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			out = append(out, filesystem.GrepMatch{
				Path:    rel,
				Line:    i + 1,
				Content: contextBlock(lines, i, req.BeforeLines, req.AfterLines),
			})
			if len(out) >= maxGrepMatches {
				return errStopWalk
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errStopWalk) {
		return nil, walkErr
	}
	return out, nil
}

var errStopWalk = errors.New("stop walk")

func contextBlock(lines []string, i, before, after int) string {
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}
	lo := max(0, i-before)
	hi := min(len(lines), i+after+1)
	return strings.Join(lines[lo:hi], "\n")
}

func isBinary(raw []byte) bool {
	probe := raw
	if len(probe) > maxBinaryProbe {
		probe = probe[:maxBinaryProbe]
	}
	return bytes.IndexByte(probe, 0) >= 0
}

// Shell 执行白名单命令。命令直接以 exec 启动（无 shell 解释），因此白名单的元字符检查即完整的注入防线。
type Shell struct {
	root       string
	secretKeys []string
}

// Execute 实现 filesystem.Shell。
func (s *Shell) Execute(ctx context.Context, req *filesystem.ExecuteRequest) (*filesystem.ExecuteResponse, error) {
	if req.RunInBackendGround {
		return nil, errors.New("background execution is not supported")
	}
	cmdline := strings.TrimSpace(req.Command)
	if !policy.IsBashCommandWhitelisted(cmdline) {
		return nil, errors.New("access denied: command not allowed by bash whitelist (allowed: python .claude/skills/<skill>/scripts/<script>.py, ffmpeg, ffprobe)")
	}
	tokens := strings.Fields(cmdline)

	ctx, cancel := context.WithTimeout(ctx, shellTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tokens[0], tokens[1:]...)
	cmd.Dir = s.root
	cmd.Env = scrubEnv(os.Environ(), s.secretKeys)
	out, err := cmd.CombinedOutput()

	exit := 0
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		exit = exitErr.ExitCode()
	default:
		return nil, err
	}
	text := string(out)
	truncated := false
	if len(text) > maxOutputBytes {
		text = text[:maxOutputBytes]
		truncated = true
	}
	return &filesystem.ExecuteResponse{Output: text, ExitCode: &exit, Truncated: truncated}, nil
}

// scrubEnv 从环境变量中剔除密钥（对应 Python _bash_env_scrub_hook；Windows 也适用，不依赖 env -u）。
func scrubEnv(env []string, secretKeys []string) []string {
	if len(secretKeys) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, s := range secretKeys {
			if strings.EqualFold(key, s) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// Middlewares 为一个 agent 构造文件系统中间件。enabled 之外的工具被禁用（子代理按白名单收敛）。
func Middleware(ctx context.Context, root string, secretKeys []string, enabled map[string]bool) (adk.ChatModelAgentMiddleware, error) {
	b, err := NewBackend(root)
	if err != nil {
		return nil, err
	}
	cfg := &fsmw.MiddlewareConfig{
		Backend:             b,
		LsToolConfig:        &fsmw.ToolConfig{Disable: !enabled[ToolLs]},
		ReadFileToolConfig:  &fsmw.ToolConfig{Disable: !enabled[ToolRead]},
		WriteFileToolConfig: &fsmw.ToolConfig{Disable: !enabled[ToolWrite]},
		EditFileToolConfig:  &fsmw.ToolConfig{Disable: !enabled[ToolEdit]},
		GlobToolConfig:      &fsmw.ToolConfig{Disable: !enabled[ToolGlob]},
		GrepToolConfig:      &fsmw.ToolConfig{Disable: !enabled[ToolGrep]},
	}
	if enabled[ToolExecute] {
		cfg.Shell = &Shell{root: b.root, secretKeys: secretKeys}
	}
	return fsmw.New(ctx, cfg)
}
