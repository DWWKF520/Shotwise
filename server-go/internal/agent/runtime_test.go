package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"github.com/shotwise/shotwise/internal/agent/checkpoint"
	"github.com/shotwise/shotwise/internal/agent/eventlog"
)

// --- 测试替身 ---

// scriptedModel 按脚本顺序回放 assistant 消息（streaming 单 chunk）；
// 脚本耗尽后返回兜底文本，防止测试挂死。跨 TurnLoop 实例共享计数
// （Runtime.chatModel 返回同一实例），HITL 恢复轮继续消费后续脚本。
type scriptedModel struct {
	mu     sync.Mutex
	script []*schema.Message
	calls  int
}

func (m *scriptedModel) next() *schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if len(m.script) == 0 {
		return &schema.Message{Role: schema.Assistant, Content: "script exhausted"}
	}
	msg := m.script[0]
	m.script = m.script[1:]
	return msg
}

func (m *scriptedModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return m.next(), nil
}

func (m *scriptedModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{m.next()}), nil
}

func (m *scriptedModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func toolCallMsg(id, name, args string) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID: id, Type: "function",
			Function: schema.FunctionCall{Name: name, Arguments: args},
		}},
	}
}

func textMsg(content string) *schema.Message {
	return &schema.Message{Role: schema.Assistant, Content: content}
}

// newTestRuntime 构造注入模型 + 内存存储的 Runtime；空闲 100ms 自动退出，
// 让每轮结束后 loop 干净退出、bus 关闭，drainEvents 得以返回。
func newTestRuntime(t *testing.T, m model.ToolCallingChatModel) *Runtime {
	t.Helper()
	return &Runtime{
		dataDir:           t.TempDir(),
		idleTimeout:       100 * time.Millisecond,
		sessions:          NewSessionManager(0),
		cpStore:           checkpoint.NewMemStore(),
		evStore:           eventlog.NewMemStore(),
		active:            map[string]*sessionLoop{},
		pendingInterrupts: map[string]string{},
		chatModelOverride: m,
	}
}

// drainEvents 消费事件流直到关闭（loop 空闲退出 → bus.Close），5s 超时兜底。
func drainEvents(t *testing.T, ch <-chan Event) []Event {
	t.Helper()
	var out []Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(5 * time.Second):
			t.Fatalf("drainEvents timeout after %d events", len(out))
		}
	}
}

// writeFileTool 是受围栏约束的真实写文件工具（guard deny 场景需要工具真实存在）。
type writeFileIn struct {
	FilePath string `json:"file_path" jsonschema:"description=相对项目根的路径"`
	Content  string `json:"content" jsonschema:"description=写入内容"`
}

func newWriteFileTool(root string) (tool.BaseTool, error) {
	return toolutils.InferTool("write_file",
		"把内容写入项目内的文件（相对路径）。",
		func(_ context.Context, in writeFileIn) (string, error) {
			p := filepath.Join(root, filepath.FromSlash(in.FilePath))
			if err := os.WriteFile(p, []byte(in.Content), 0o644); err != nil {
				return denyEnvelope(err.Error()), nil
			}
			return okEnvelope("written"), nil
		})
}

// --- 场景 1：工具调用闭环 ---

func TestRuntime_ToolLoop(t *testing.T) {
	m := &scriptedModel{script: []*schema.Message{
		toolCallMsg("call_1", "echo", `{"message":"hi"}`),
		textMsg("final answer"),
	}}
	r := newTestRuntime(t, m)
	_, ch, unsub, err := r.Query(context.Background(), "s1", "proj", "use echo")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer unsub()
	evs := drainEvents(t, ch)

	var gotToolUse, gotToolResult, gotFinalDelta bool
	for _, ev := range evs {
		switch ev.Type {
		case EventToolUse:
			if ev.ToolName == "echo" {
				gotToolUse = true
			}
		case EventToolResult:
			if ev.ToolName == "echo" && !ev.IsError {
				gotToolResult = true
			}
		case EventDelta:
			if ev.Text == "final answer" {
				gotFinalDelta = true
			}
		}
	}
	if !gotToolUse || !gotToolResult || !gotFinalDelta {
		t.Fatalf("missing events: toolUse=%v toolResult=%v finalDelta=%v; events=%+v",
			gotToolUse, gotToolResult, gotFinalDelta, evs)
	}

	// 事件日志应包含 user / assistant / tool_result 完整 transcript。
	entries, err := r.evStore.List(context.Background(), "s1", 0)
	if err != nil {
		t.Fatalf("event log list: %v", err)
	}
	var types []eventlog.EntryType
	for _, e := range entries {
		types = append(types, e.Type)
	}
	want := []eventlog.EntryType{
		eventlog.EntryUser, eventlog.EntryAssistant, eventlog.EntryToolResult, eventlog.EntryAssistant,
	}
	if len(types) != len(want) {
		t.Fatalf("transcript = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("transcript = %v, want %v", types, want)
		}
	}
}

// --- 场景 2：guard deny（硬失败围栏） ---

func TestRuntime_GuardDeny(t *testing.T) {
	dir := t.TempDir()
	m := &scriptedModel{script: []*schema.Message{
		toolCallMsg("call_1", "write_file", `{"file_path":"project.json","content":"{}"}`),
		textMsg("acknowledged"),
	}}
	r := newTestRuntime(t, m)
	r.dataDir = dir // write_file 与 policy 的项目根同源
	wt, err := newWriteFileTool(dir)
	if err != nil {
		t.Fatalf("build write_file: %v", err)
	}
	r.extraTools = []tool.BaseTool{wt}

	_, ch, unsub, err := r.Query(context.Background(), "s2", "proj", "overwrite the truth source")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer unsub()
	evs := drainEvents(t, ch)

	var denied bool
	for _, ev := range evs {
		if ev.Type == EventToolResult && ev.ToolName == "write_file" && ev.IsError {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("expected denied tool_result for write_file; events=%+v", evs)
	}
	// 保护文件必须未被落盘。
	if _, statErr := os.Stat(filepath.Join(dir, "project.json")); !os.IsNotExist(statErr) {
		t.Fatalf("project.json was written despite guard deny (stat err=%v)", statErr)
	}
}

// --- 场景 3：ask_user HITL 挂起与恢复 ---

func TestRuntime_AskUserResume(t *testing.T) {
	m := &scriptedModel{script: []*schema.Message{
		toolCallMsg("call_1", "ask_user",
			`{"questions":[{"question":"选哪个方案？","header":"方案","options":[{"label":"A"},{"label":"B"}]}]}`),
		textMsg("user chose A"),
	}}
	r := newTestRuntime(t, m)
	ctx := context.Background()
	_, ch, unsub, err := r.Query(ctx, "s3", "proj", "ask me")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	evs := drainEvents(t, ch)
	unsub()

	var interruptID string
	for _, ev := range evs {
		if ev.Type == EventQuestion {
			var meta struct {
				InterruptID string `json:"interrupt_id"`
			}
			if jsonErr := json.Unmarshal(ev.Meta, &meta); jsonErr == nil {
				interruptID = meta.InterruptID
			}
		}
	}
	if interruptID == "" {
		t.Fatalf("no question event with interrupt_id; events=%+v", evs)
	}
	r.mu.Lock()
	pending := r.pendingInterrupts["s3"]
	r.mu.Unlock()
	if pending != interruptID {
		t.Fatalf("pendingInterrupts = %q, want %q", pending, interruptID)
	}

	// 回答后从 checkpoint 恢复：ask_user 工具回吐答案，模型续跑出最终文本。
	rch, runsub, err := r.Answer(ctx, "s3", AnswerPayload{Answers: []string{"A"}})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	defer runsub()
	revs := drainEvents(t, rch)

	var resumed bool
	for _, ev := range revs {
		if ev.Type == EventDelta && ev.Text == "user chose A" {
			resumed = true
		}
	}
	if !resumed {
		t.Fatalf("resume turn missing final delta; events=%+v", revs)
	}
	// 恢复轮模型应只被再调用一次（脚本第二条被消费）。
	m.mu.Lock()
	calls := m.calls
	m.mu.Unlock()
	if calls != 2 {
		t.Fatalf("model calls = %d, want 2", calls)
	}
}
