package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// 代码类扩展名禁写（设计文档 §4.2）：智能体不得写代码文件，写入口收归 Skill 脚本装配。
var codeWriteExtensions = map[string]bool{
	".py": true, ".js": true, ".ts": true, ".tsx": true, ".sh": true,
	".yaml": true, ".yml": true, ".toml": true,
}

// protectedRule 对应 Python 侧 PROTECTED_WRITE_RULES 的单行规则。
// 每条规则同时声明 hook 谓词（Matches）与 sandbox 投影——Go v1 只落 hook 谓词，
// sandbox 投影随沙箱执行器增量接入；两层刻意不对称（设计文档 §4.2）。
type protectedRule struct {
	Name    string
	Matches func(relSlash string) bool
	Reason  string
}

// referenceStep1RE 只拒正式文件；同目录的 .invalid.json 隔离草稿是内置 Edit 的编辑工位，放行。
var referenceStep1RE = regexp.MustCompile(`^drafts/episode_[0-9]+/step1_reference_units\.json$`)

// pythonSkillScriptRE 限定 python 入口必须是 Skill 自带脚本（四步判定的第④步）。
var pythonSkillScriptRE = regexp.MustCompile(`^\.claude/skills/[A-Za-z0-9_-]+/scripts/[A-Za-z0-9_.-]+\.py$`)

// protectedWriteRules 单一真相源，与 Python 侧语义一一对应。
var protectedWriteRules = []protectedRule{
	{
		Name: "project_json",
		Matches: func(rel string) bool {
			// 拒根 project.json + scripts/ 整子树：写入口收归工具。
			return rel == "project.json" || strings.HasPrefix(rel, "scripts/") || rel == "scripts"
		},
		Reason: "project.json 与 scripts/ 只能经由工具写入",
	},
	{
		Name: "reference_step1",
		Matches: func(rel string) bool {
			// 只拒正式文件：三条路径持同一把 per-path 锁，Write/Edit 取不到锁会丢更新。
			return referenceStep1RE.MatchString(rel)
		},
		Reason: "step1_reference_units.json 须经由 open_reference_step1_for_edit / validate_and_promote 工具写入",
	},
}

// AgentAccessPolicy 是智能体访问围栏的 Go 版实现（Python agent_access_policy.py 的 port）。
// 覆盖三类裁决：文件写围栏、敏感读路径、Bash 命令白名单 + env 剥离。
type AgentAccessPolicy struct {
	rawBase      string // 项目根原样路径
	resolvedBase string // EvalSymlinks 后的真实路径（防 macOS /var↔/private/var、Linux symlink 根绕过）

	secretEnvKeys []string // Bash 执行前需剥离的环境变量名（API Key 等）
}

// NewAgentAccessPolicy 构造围栏策略。projectRoot 是项目根目录；
// secretEnvKeys 是 Bash 执行前需从环境剥离的密钥变量名。
func NewAgentAccessPolicy(projectRoot string, secretEnvKeys []string) *AgentAccessPolicy {
	raw := filepath.Clean(projectRoot)
	resolved := raw
	if rp, err := filepath.EvalSymlinks(raw); err == nil {
		resolved = rp
	}
	return &AgentAccessPolicy{
		rawBase:       raw,
		resolvedBase:  resolved,
		secretEnvKeys: append([]string(nil), secretEnvKeys...),
	}
}

// writeDispatchTools / readDispatchTools 是文件围栏的触发工具名集合：
// 覆盖 Claude SDK 内置命名（Write/Edit/Read/Glob/Grep）与 Go filesystem
// middleware 将采用的 snake_case 命名（write_file/edit_file/read_file/ls）。
var writeDispatchTools = map[string]bool{
	"write": true, "edit": true, "write_file": true, "edit_file": true, "multiedit": true, "notebookedit": true,
}

var readDispatchTools = map[string]bool{
	"read": true, "read_file": true, "glob": true, "grep": true, "ls": true,
}

// Pre 实现 ToolGuard：按工具名分派裁决。
//
// 文件工具的入参字段名按通用约定宽松解析（file_path / path / notebook_path / pattern）；
// filesystem middleware 接入后如有差异在此处对齐。业务工具（generate_* 等）
// 不在文件围栏范围内，直接放行。
func (p *AgentAccessPolicy) Pre(_ context.Context, call ToolCall) (json.RawMessage, error) {
	name := strings.ToLower(call.Name)
	switch name {
	case "bash":
		return p.preBash(call.Args)
	}
	if writeDispatchTools[name] {
		pArg, ok := extractStringArg(call.Args, "file_path", "path", "notebook_path")
		if !ok {
			return nil, nil // 无路径入参的调用不在此裁决（由真实工具自身的参数校验兜底）
		}
		if err := p.CheckWrite(pArg); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if readDispatchTools[name] {
		pArg, ok := extractStringArg(call.Args, "file_path", "path", "notebook_path")
		if !ok {
			return nil, nil
		}
		if err := p.CheckRead(pArg); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return nil, nil
}

// Post 实现 ToolGuard。Python 侧 _json_validation_hook（Write/Edit 的 JSON 预校验 +
// 失败回写备份）依赖备份机制，随 filesystem middleware 增量接入；
// v1 Post 仅作为审计与回写钩位点。
func (p *AgentAccessPolicy) Post(_ context.Context, _ ToolCall, _ string) error {
	return nil
}

// --- Bash 白名单（四步判定，设计文档 §4.1）---

// bashBlockedChars 是第①步拒绝的 shell 元字符（含反引号）。
const bashBlockedChars = ";&|<>$\r\n"

// IsBashCommandWhitelisted 判定一条 Bash 命令是否可通过白名单：
//  1. 拒 shell 元字符
//  2. 拒 `..` 路径段（原串、剥引号、反斜杠归一化三种形态各查一遍）
//  3. token 边界匹配白名单前缀
//  4. python 入口必须是 .claude/skills/<skill>/scripts/<script>.py
//
// 白名单前缀：python .claude/skills/、ffmpeg、ffprobe。
// BashOutput / KillBash 不在此判定（它们不产生新命令），由调用方直接放行。
func IsBashCommandWhitelisted(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	// ① shell 元字符
	if strings.ContainsAny(cmd, bashBlockedChars) || strings.Contains(cmd, "`") {
		return false
	}
	// ② `..` 路径段：原串 + 剥引号 + 反斜杠归一化
	for _, variant := range bashPathVariants(cmd) {
		if hasDotDotSegment(variant) {
			return false
		}
	}
	// ③ token 边界匹配
	tokens := strings.Fields(cmd)
	if len(tokens) == 0 {
		return false
	}
	switch {
	case tokens[0] == "python" || tokens[0] == "python3":
		// ④ python 入口必须是 Skill 自带脚本
		if len(tokens) < 2 {
			return false
		}
		return pythonSkillScriptRE.MatchString(tokens[1])
	case tokens[0] == "ffmpeg" || tokens[0] == "ffprobe":
		return true
	}
	return false
}

// bashPathVariants 生成第②步需要检查的路径形态：原串、剥双引号、剥单引号（各叠加反斜杠归一化）。
func bashPathVariants(cmd string) []string {
	variants := []string{cmd, strings.ReplaceAll(cmd, `"`, ""), strings.ReplaceAll(cmd, "'", "")}
	for _, v := range append([]string(nil), variants...) {
		variants = append(variants, strings.ReplaceAll(v, `\`, "/"))
	}
	return variants
}

// hasDotDotSegment 检查路径串中是否含 `..` 路径段：
// 先按空白分词，再对每个 token 按 `/` 切段（兼容反斜杠归一化后的形态）。
func hasDotDotSegment(s string) bool {
	for _, token := range strings.Fields(s) {
		for _, seg := range strings.Split(token, "/") {
			if seg == ".." {
				return true
			}
		}
	}
	return false
}

// preBash 对 Bash 调用做白名单判定 + env 剥离改写。
// 判定用原始命令；改写（env -u）发生在判定之后，改写结果不再过白名单。
func (p *AgentAccessPolicy) preBash(args json.RawMessage) (json.RawMessage, error) {
	cmd, ok := extractStringArg(args, "command")
	if !ok {
		return nil, fmt.Errorf("access denied: bash tool requires a command argument")
	}
	if !IsBashCommandWhitelisted(cmd) {
		return nil, fmt.Errorf("access denied: command not allowed by bash whitelist (allowed: python .claude/skills/<skill>/scripts/<script>.py, ffmpeg, ffprobe)")
	}
	scrubbed := ScrubBashEnv(cmd, p.secretEnvKeys)
	if scrubbed == cmd {
		return nil, nil // 无需改写
	}
	// 保留其余字段，仅替换 command。
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, nil // 入参不是对象时无法改写，保守放行原始命令
	}
	b, err := json.Marshal(scrubbed)
	if err != nil {
		return nil, nil
	}
	m["command"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return nil, nil
	}
	return out, nil
}

// ScrubBashEnv 把命令包成 `env -u <KEY> ... sh -c '<cmd>'` 以剥离密钥环境变量。
// 仅 POSIX 生效：Windows 无 env/sh，走白名单降级路径（进程环境构造由沙箱执行器增量实现）。
func ScrubBashEnv(cmd string, secretKeys []string) string {
	if len(secretKeys) == 0 || runtime.GOOS == "windows" {
		return cmd
	}
	var sb strings.Builder
	sb.WriteString("env")
	for _, k := range secretKeys {
		sb.WriteString(" -u ")
		sb.WriteString(shellSafeName(k))
	}
	sb.WriteString(" sh -c ")
	sb.WriteString(shellSingleQuote(cmd))
	return sb.String()
}

// shellSafeName 校验变量名安全后原样返回；不合法名跳过（防御性，调用方传入的 key 应为常量）。
func shellSafeName(k string) string {
	for _, r := range k {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return ""
		}
	}
	return k
}

// shellSingleQuote POSIX 单引号包裹，内部单引号按 `'\''` 转义。
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ruleError 是带规则名的 deny 错误，供审计与测试断言命中的规则。
type ruleError struct {
	Rule    string
	Message string
}

func (e *ruleError) Error() string { return e.Message }

// --- 文件围栏 ---

// CheckWrite 校验一个绝对路径是否允许写入。
func (p *AgentAccessPolicy) CheckWrite(absPath string) error {
	rel, err := p.relativize(absPath)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	for _, rule := range protectedWriteRules {
		if rule.Matches(rel) {
			// 返回带规则名的错误类型，调用方可按规则审计；message 原样回灌模型。
			return &ruleError{Rule: rule.Name, Message: fmt.Sprintf("access denied: %s is protected by rule %s: %s", rel, rule.Name, rule.Reason)}
		}
	}
	if ext := strings.ToLower(path.Ext(rel)); codeWriteExtensions[ext] {
		return fmt.Errorf("access denied: writing code files (%s) is not allowed; scripts are provisioned by skills", ext)
	}
	return nil
}

// CheckRead 校验一个绝对路径是否允许读取（敏感路径 deny，其余放行）。
func (p *AgentAccessPolicy) CheckRead(absPath string) error {
	rel, err := p.relativize(absPath)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if isSensitiveReadPath(rel) {
		return fmt.Errorf("access denied: %s is a sensitive path", rel)
	}
	return nil
}

// isSensitiveReadPath 判定项目根内的敏感读路径（设计文档 §4.2）。
// 读放行例外（~/.claude/projects/.../tool-results、/tmp/claude-*/tasks、仓库根参考资料）
// 涉及项目根之外的绝对路径白名单，随沙箱执行器增量接入；项目根内默认读放行。
func isSensitiveReadPath(rel string) bool {
	base := path.Base(rel)
	switch {
	case base == ".env" || strings.HasPrefix(base, ".env."):
		return true
	case rel == "vertex_keys" || strings.HasPrefix(rel, "vertex_keys/"):
		return true
	case strings.HasPrefix(base, ".system_config.json"):
		return true
	case strings.HasPrefix(base, ".shotwise.db"):
		return true
	case rel == ".claude/settings.json" || rel == path.Join(".claude", "settings.local.json"):
		return true // profile settings.json
	case rel == "logs" || strings.HasPrefix(rel, "logs/"):
		return true
	}
	return false
}

// relativize 把任意路径解析为相对项目根的相对路径：
// 相对路径先 join 到 rawBase；随后对 raw / resolved 双 base 枚举（防 symlink 根绕过）；
// 解析结果落在任一 base 之下即返回相对段，否则拒绝。
func (p *AgentAccessPolicy) relativize(pth string) (string, error) {
	pth = filepath.Clean(pth)
	if !filepath.IsAbs(pth) {
		pth = filepath.Join(p.rawBase, pth)
	}
	// 候选：入参原样 + EvalSymlinks 解析后的真实路径。
	candidates := []string{pth}
	if rp, err := filepath.EvalSymlinks(pth); err == nil && rp != pth {
		candidates = append(candidates, rp)
	}
	for _, base := range []string{p.rawBase, p.resolvedBase} {
		for _, c := range candidates {
			if withinBase(base, c) {
				rel, err := filepath.Rel(base, c)
				if err != nil {
					continue
				}
				return rel, nil
			}
		}
	}
	return "", fmt.Errorf("access denied: %s is outside the project root", pth)
}

// withinBase 判断 target 是否在 base 目录之下（或恰好等于 base）。
func withinBase(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && rel != "")
}

// extractStringArg 从 JSON 入参里按候选键取第一个字符串值。
func extractStringArg(args json.RawMessage, keys ...string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return "", false
	}
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v, true
		}
	}
	return "", false
}
