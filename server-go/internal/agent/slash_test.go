package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/shotwise/shotwise/internal/agent/eventlog"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
)

func TestParseSlashCommand(t *testing.T) {
	cases := []struct {
		in    string
		skill string
		text  string
	}{
		{in: "/demo 生成视频", skill: "demo", text: "生成视频"},
		{in: "/demo", skill: "demo", text: "/demo"},          // 无参数：保留原输入，避免空消息
		{in: "/demo\tgo now", skill: "demo", text: "go now"}, // 制表符同样是分隔
		{in: "普通消息", skill: "", text: "普通消息"},
		{in: "看 /demo", skill: "", text: "看 /demo"},                 // 非行首不拦截
		{in: "/api/v1/x hello", skill: "", text: "/api/v1/x hello"}, // 路径形 token 不拦截
		{in: "/", skill: "", text: "/"},                             // 没有名字
		{in: "//demo x", skill: "", text: "//demo x"},               // 名字含 / 不拦截
		{in: "", skill: "", text: ""},
	}
	for _, c := range cases {
		skill, text := ParseSlashCommand(c.in)
		if skill != c.skill || text != c.text {
			t.Errorf("ParseSlashCommand(%q) = (%q, %q), want (%q, %q)", c.in, skill, text, c.skill, c.text)
		}
	}
}

// recordModel 记录每次模型调用收到的用户轮文本，固定回一句收尾文本。
type recordModel struct {
	mu        sync.Mutex
	userTexts []string
}

func (m *recordModel) record(in []*schema.Message) *schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range in {
		if msg.Role == schema.User {
			m.userTexts = append(m.userTexts, msg.Content)
		}
	}
	return textMsg("ok")
}

func (m *recordModel) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return m.record(in), nil
}

func (m *recordModel) Stream(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{m.record(in)}), nil
}

func (m *recordModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *recordModel) snapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.userTexts...)
}

// newAssistantFixture 起一个带真实 sqlite（全量迁移 + 默认用户）、真实项目与 demo skill 的编排层。
func newAssistantFixture(t *testing.T, m model.ToolCallingChatModel) *AssistantService {
	t.Helper()
	gdb := newTimelineTestDB(t)
	dataDir := t.TempDir()
	writeReviewJSON(t, filepath.Join(dataDir, "proj", "project.json"), map[string]any{
		"content_mode":    "drama",
		"generation_mode": "storyboard",
		"current_episode": float64(1),
		"episodes":        []any{},
	})
	writeProjectClaude(t, dataDir, "proj")

	rt := newTestRuntime(t, m)
	rt.dataDir = dataDir
	return NewAssistantService(gdb, eventlog.NewSQLStore(gdb), rt, service.NewProjectStore(dataDir), dataDir)
}

// waitForUserTurn 轮询等模型收到至少一条用户轮文本。
func waitForUserTurn(t *testing.T, m *recordModel) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if texts := m.snapshot(); len(texts) > 0 {
			return texts
		}
		if time.Now().After(deadline) {
			t.Fatal("模型未被调用")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// /skill-name 参数形式：正文只送参数，轮输入带 skill 触发前缀。
func TestAssistantSendSlashInvokesSkill(t *testing.T) {
	m := &recordModel{}
	a := newAssistantFixture(t, m)

	if _, err := a.SendCreate(context.Background(), "proj", "/demo 生成视频",
		"", "", "claude", repository.DefaultUserID); err != nil {
		t.Fatalf("send: %v", err)
	}
	texts := waitForUserTurn(t, m)
	if !containsPrefix(texts, "[用户请求触发 skill: demo] 生成视频") {
		t.Fatalf("模型用户轮未带 skill 前缀: %v", texts)
	}
}

// skill 不存在：fail loud，不降级成普通聊天，模型不被调用。
func TestAssistantSendSlashUnknownSkillRejected(t *testing.T) {
	m := &recordModel{}
	a := newAssistantFixture(t, m)

	_, err := a.SendCreate(context.Background(), "proj", "/nope go",
		"", "", "claude", repository.DefaultUserID)
	if !errors.Is(err, ErrSkillNotInvocable) {
		t.Fatalf("err = %v, want ErrSkillNotInvocable", err)
	}
	var skillErr *SkillNotInvocableError
	if !errors.As(err, &skillErr) || skillErr.Skill != "nope" {
		t.Fatalf("error should carry skill name: %v", err)
	}
	if len(skillErr.Available) == 0 || skillErr.Available[0] != "demo" {
		t.Fatalf("available = %v, want [demo]", skillErr.Available)
	}
	if texts := m.snapshot(); len(texts) != 0 {
		t.Fatalf("模型不应被调用，却收到 %v", texts)
	}
}

// 不符合 / 前缀规则的输入按普通文本透传（原文进模型，无 skill 前缀）。
func TestAssistantSendNonCommandPassThrough(t *testing.T) {
	m := &recordModel{}
	a := newAssistantFixture(t, m)

	if _, err := a.SendCreate(context.Background(), "proj", "/api/v1/x hello",
		"", "", "claude", repository.DefaultUserID); err != nil {
		t.Fatalf("send: %v", err)
	}
	texts := waitForUserTurn(t, m)
	if !containsPrefix(texts, "/api/v1/x hello") {
		t.Fatalf("非命令输入应原样透传: %v", texts)
	}
	if containsPrefix(texts, "[用户请求触发 skill:") {
		t.Fatalf("非命令输入不应带 skill 前缀: %v", texts)
	}
}

func containsPrefix(texts []string, want string) bool {
	for _, s := range texts {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
