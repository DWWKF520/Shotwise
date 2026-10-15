package policy

import "testing"

// 回归（§17.6）：compose-video 真实脚本路径必须通过 Windows 白名单第 ④ 步。
func TestIsBashCommandWhitelistedRealComposeVideoScript(t *testing.T) {
	cmd := "python .claude/skills/compose-video/scripts/compose_video.py --project proj"
	if !IsBashCommandWhitelisted(cmd) {
		t.Fatalf("IsBashCommandWhitelisted(%q) = false, want true", cmd)
	}
}
