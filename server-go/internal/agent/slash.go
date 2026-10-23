package agent

import (
	"strings"
	"unicode"
)

// ParseSlashCommand 解析用户输入里的 /skill-name 前缀（对应 Claude Code 的 slash command）。
//
// 判定条件：消息以 / 开头，且首 token 形如 /name（name 非空且不含 /）。命中时返回
// skill 名与应送入模型的正文（首 token 之后的剩余参数；无参数时保留原输入，避免空消息
// 被拒）。其余输入 skill 为空串、text 原样返回——普通对话中的 /xxx 不做拦截，路径形
// token（/api/v1/…）也因 name 含 / 而不会被误判。
//
// 这里只做词法拆分：skill 是否存在、是否 user-invocable 由调用方按项目目录 fail loud，
// 不在解析阶段静默降级成普通聊天。
func ParseSlashCommand(raw string) (skill, text string) {
	if !strings.HasPrefix(raw, "/") {
		return "", raw
	}
	token, rest := raw, ""
	if i := strings.IndexFunc(raw, unicode.IsSpace); i >= 0 {
		token, rest = raw[:i], raw[i:]
	}
	name := strings.TrimPrefix(token, "/")
	if name == "" || strings.Contains(name, "/") {
		return "", raw
	}
	if rest = strings.TrimSpace(rest); rest == "" {
		rest = raw
	}
	return name, rest
}
