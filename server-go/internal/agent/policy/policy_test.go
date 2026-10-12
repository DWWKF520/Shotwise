package policy

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
)

func testPolicy(t *testing.T) *AgentAccessPolicy {
	t.Helper()
	return NewAgentAccessPolicy(t.TempDir(), []string{"SHOTWISE_API_KEY"})
}

// --- 写围栏 ---

func TestCheckWrite_ProtectedRules(t *testing.T) {
	p := testPolicy(t)
	cases := []struct {
		rel     string
		denied  bool
		ruleTag string
	}{
		{"project.json", true, "project_json"},
		{"scripts/render.py", true, "project_json"},
		{"scripts/sub/deep.json", true, "project_json"},
		{"drafts/episode_1/step1_reference_units.json", true, "reference_step1"},
		{"drafts/episode_12/step1_reference_units.json", true, "reference_step1"},
		// 同目录 .invalid.json 隔离草稿是内置 Edit 的编辑工位，必须放行。
		{"drafts/episode_1/step1_reference_units.invalid.json", false, ""},
		{"assets/characters/foo.png", false, ""},
		{"episodes/ep1.json", false, ""},
	}
	for _, c := range cases {
		err := p.CheckWrite(c.rel)
		if c.denied && err == nil {
			t.Errorf("CheckWrite(%q) = nil, want denied", c.rel)
		}
		if !c.denied && err != nil {
			t.Errorf("CheckWrite(%q) = %v, want allow", c.rel, err)
		}
		if c.denied && c.ruleTag != "" && err != nil {
			var re *ruleError
			if !asRuleError(err, &re) || re.Rule != c.ruleTag {
				t.Errorf("CheckWrite(%q) err=%v, want rule %s", c.rel, err, c.ruleTag)
			}
		}
	}
}

func TestCheckWrite_CodeExtensions(t *testing.T) {
	p := testPolicy(t)
	for _, rel := range []string{"a.py", "lib/b.js", "x.tsx", "y.sh", "cfg.yaml", "cfg.yml", "cfg.toml", "m.ts"} {
		if err := p.CheckWrite(rel); err == nil {
			t.Errorf("CheckWrite(%q) = nil, want denied", rel)
		}
	}
	// 大小写不敏感
	if err := p.CheckWrite("A.PY"); err == nil {
		t.Error("CheckWrite(A.PY) = nil, want denied")
	}
}

func TestCheckWrite_OutsideRoot(t *testing.T) {
	p := testPolicy(t)
	paths := []string{`C:\Windows\system32\evil.json`, "..\\..\\escape.json", "../../escape.json"}
	if runtime.GOOS != "windows" {
		// POSIX 绝对路径在 Windows 语义下不是绝对路径（无盘符会被 join 进项目根），仅 POSIX 下参与断言。
		paths = append(paths, "/etc/passwd")
	}
	for _, pth := range paths {
		if err := p.CheckWrite(pth); err == nil {
			t.Errorf("CheckWrite(%q) = nil, want denied", pth)
		}
	}
}

// --- 敏感读路径 ---

func TestCheckRead_SensitivePaths(t *testing.T) {
	p := testPolicy(t)
	denied := []string{".env", ".env.local", "vertex_keys/key.json", "vertex_keys",
		".system_config.json", ".system_config.json.bak", ".shotwise.db", ".shotwise.db-wal",
		".claude/settings.json", "logs/run.log"}
	for _, rel := range denied {
		if err := p.CheckRead(rel); err == nil {
			t.Errorf("CheckRead(%q) = nil, want denied", rel)
		}
	}
	allowed := []string{"project.json", "assets/a.png", "episodes/ep1.json", ".claude/skills/x/SKILL.md"}
	for _, rel := range allowed {
		if err := p.CheckRead(rel); err != nil {
			t.Errorf("CheckRead(%q) = %v, want allow", rel, err)
		}
	}
}

// --- Bash 白名单（四步判定）---

func TestIsBashCommandWhitelisted(t *testing.T) {
	allowed := []string{
		"python .claude/skills/generate-video/scripts/main.py",
		"python3 .claude/skills/manga-workflow/scripts/split.py",
		"ffmpeg -i in.mp4 out.mp4",
		"ffprobe -v quiet in.mp4",
	}
	denied := []string{
		"", // 空
		"python .claude/skills/evil/scripts/x.py; rm -rf /",            // ① 元字符
		"ffmpeg -i a.mp4 b.mp4 && echo done",                           // ① 元字符
		"ffmpeg -i ../secret.mp4 out.mp4",                              // ② `..` 路径段
		`python .claude/skills/x/scripts/"../evil.py"`,                 // ② 剥引号后 `..`
		"ffmpeg -i ..\\secret.mp4 out.mp4",                             // ② 反斜杠归一化后 `..`
		"python script.py",                                             // ④ 非 skill 脚本
		"python .claude/skills/x/scripts/evil.sh",                      // ④ 非 .py
		"python",                                                       // ④ 缺脚本参数
		"ls -la",                                                       // ③ 白名单外
		"cat project.json",                                             // ③ 白名单外
		"echo `whoami`",                                                // ① 反引号
	}
	for _, cmd := range allowed {
		if !IsBashCommandWhitelisted(cmd) {
			t.Errorf("IsBashCommandWhitelisted(%q) = false, want true", cmd)
		}
	}
	for _, cmd := range denied {
		if IsBashCommandWhitelisted(cmd) {
			t.Errorf("IsBashCommandWhitelisted(%q) = true, want false", cmd)
		}
	}
}

// --- Pre 分派 ---

func TestPre_BashRewrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("env 剥离仅 POSIX 生效")
	}
	p := testPolicy(t)
	args := mustJSON(map[string]any{"command": "ffmpeg -i a.mp4 b.mp4"})
	out, err := p.Pre(context.Background(), ToolCall{Name: "Bash", Args: args})
	if err != nil {
		t.Fatalf("Pre(Bash) err = %v, want allow", err)
	}
	if out == nil {
		t.Fatal("Pre(Bash) out = nil, want rewritten args with env -u")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("rewritten args not json: %v", err)
	}
	cmd, _ := m["command"].(string)
	want := "env -u SHOTWISE_API_KEY sh -c 'ffmpeg -i a.mp4 b.mp4'"
	if cmd != want {
		t.Errorf("scrubbed command = %q, want %q", cmd, want)
	}

	// 白名单外命令 → deny
	deny, err := p.Pre(context.Background(), ToolCall{Name: "bash", Args: mustJSON(map[string]any{"command": "ls -la"})})
	if err == nil {
		t.Error("Pre(bash ls) err = nil, want deny")
	}
	if deny != nil {
		t.Error("Pre(bash ls) out != nil, want nil")
	}
}

func TestPre_WriteDispatch(t *testing.T) {
	p := testPolicy(t)
	// file_path 指向保护文件 → deny
	_, err := p.Pre(context.Background(), ToolCall{
		Name: "Write",
		Args: mustJSON(map[string]any{"file_path": "project.json", "content": "{}"}),
	})
	if err == nil {
		t.Error("Pre(Write project.json) err = nil, want deny")
	}
	// 普通资产路径 → allow
	out, err := p.Pre(context.Background(), ToolCall{
		Name: "write",
		Args: mustJSON(map[string]any{"file_path": "assets/a.png", "content": "x"}),
	})
	if err != nil || out != nil {
		t.Errorf("Pre(write assets) = (%v, %v), want (nil, nil)", out, err)
	}
	// 业务工具 → 放行
	out, err = p.Pre(context.Background(), ToolCall{Name: "generate_storyboards", Args: mustJSON(map[string]any{"episode": 1})})
	if err != nil || out != nil {
		t.Errorf("Pre(business tool) = (%v, %v), want (nil, nil)", out, err)
	}
}

// --- helpers ---

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func asRuleError(err error, target **ruleError) bool {
	if re, ok := err.(*ruleError); ok {
		*target = re
		return true
	}
	return false
}
