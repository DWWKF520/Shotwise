package http

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/agent"
	"github.com/shotwise/shotwise/internal/agent/checkpoint"
	"github.com/shotwise/shotwise/internal/agent/eventlog"
)

// scriptedModel 按脚本回放 assistant 消息（streaming 单 chunk），脚本耗尽后返回兜底文本。
type scriptedModel struct {
	script []*schema.Message
}

func (m *scriptedModel) next() *schema.Message {
	if len(m.script) == 0 {
		return &schema.Message{Role: schema.Assistant, Content: "done"}
	}
	msg := m.script[0]
	m.script = m.script[1:]
	return msg
}

func (m *scriptedModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return m.next(), nil
}

func (m *scriptedModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{m.next()}), nil
}

func (m *scriptedModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func toolCallMsg(id, name, args string) *schema.Message {
	return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
		ID: id, Type: "function", Function: schema.FunctionCall{Name: name, Arguments: args},
	}}}
}

// newAgentFixture 起一个不带鉴权的 agent 路由壳（认证中间件由 auth 自己的测试覆盖）。
func newAgentFixture(t *testing.T, m model.ToolCallingChatModel) (*gin.Engine, *agent.Runtime) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rt, err := agent.NewRuntime(context.Background(), agent.ModelConfig{}, t.TempDir(),
		checkpoint.NewMemStore(), eventlog.NewMemStore(),
		agent.WithChatModel(m), agent.WithIdleTimeout(100*time.Millisecond))
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	r := gin.New()
	v1 := r.Group("/api/v1")
	RegisterAgentRoutes(v1, rt)
	RegisterAgentStreamRoutes(v1, rt)
	return r, rt
}

func postJSON(t *testing.T, router *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func get(t *testing.T, router *gin.Engine, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestAgentQuery_EmptyMessageRejected(t *testing.T) {
	router, _ := newAgentFixture(t, &scriptedModel{})
	rec := postJSON(t, router, "/api/v1/agent/query", map[string]any{"message": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAgentEntries_ColdRead(t *testing.T) {
	router, rt := newAgentFixture(t, &scriptedModel{script: []*schema.Message{
		toolCallMsg("call_1", "echo", `{"message":"hi"}`),
		{Role: schema.Assistant, Content: "final"},
	}})

	// 跑完一轮：SSE 关闭即代表本轮事件已消费完。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/query",
		strings.NewReader(`{"session_id":"s1","project":"p1","message":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("query status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "id: ") {
		t.Fatalf("query SSE frame has no id field:\n%s", rec.Body.String())
	}

	resp := get(t, router, "/api/v1/agent/sessions/s1/entries", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("entries status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	var body struct {
		SessionID string            `json:"session_id"`
		Status    string            `json:"status"`
		Entries   []*eventlog.Entry `json:"entries"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode entries: %v", err)
	}
	if body.SessionID != "s1" {
		t.Fatalf("session_id = %q, want s1", body.SessionID)
	}
	if len(body.Entries) == 0 {
		t.Fatal("entries is empty")
	}
	for i, e := range body.Entries {
		if e.Seq != int64(i+1) {
			t.Fatalf("entries[%d].Seq = %d, want %d", i, e.Seq, i+1)
		}
	}
	if rt.IsActive("s1") {
		t.Fatal("session still active after the turn ended")
	}
}

// entries/stream：无活跃 loop 时回放存量条目并给出终态 status 后关闭。
func TestAgentEntriesStream_ReplaysWithID(t *testing.T) {
	router, _ := newAgentFixture(t, &scriptedModel{script: []*schema.Message{
		{Role: schema.Assistant, Content: "hello"},
	}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/query",
		strings.NewReader(`{"session_id":"s2","project":"p1","message":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), req)

	resp := get(t, router, "/api/v1/agent/sessions/s2/entries/stream", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.Code)
	}
	if ct := resp.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	frames := parseSSE(t, resp.Body.String())
	var entryIDs []string
	sawStatus := false
	for _, f := range frames {
		switch f.event {
		case "entry":
			entryIDs = append(entryIDs, f.id)
		case "status":
			sawStatus = true
		}
	}
	if len(entryIDs) == 0 {
		t.Fatalf("no entry frames:\n%s", resp.Body.String())
	}
	if entryIDs[0] != "1" {
		t.Fatalf("first entry id = %q, want 1", entryIDs[0])
	}
	if !sawStatus {
		t.Fatalf("no terminal status frame:\n%s", resp.Body.String())
	}
}

// Last-Event-ID 优先于 ?after=<seq>：重连只补游标之后的条目。
func TestAgentEntriesStream_LastEventIDResumes(t *testing.T) {
	router, _ := newAgentFixture(t, &scriptedModel{script: []*schema.Message{
		{Role: schema.Assistant, Content: "one"},
	}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/query",
		strings.NewReader(`{"session_id":"s3","project":"p1","message":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), req)

	resp := get(t, router, "/api/v1/agent/sessions/s3/entries/stream?after=-1",
		map[string]string{"Last-Event-ID": "1"})
	var ids []string
	for _, f := range parseSSE(t, resp.Body.String()) {
		if f.event == "entry" {
			ids = append(ids, f.id)
		}
	}
	for _, id := range ids {
		if id == "1" {
			t.Fatalf("entry 1 replayed despite Last-Event-ID=1: %v", ids)
		}
	}
	if len(ids) == 0 {
		t.Fatalf("no entries after cursor:\n%s", resp.Body.String())
	}
}

// ask_user 挂起 → 按 question_id 回答 → 续跑：HITL 全链路走 HTTP。
func TestAgentAnswerQuestion_FullLoop(t *testing.T) {
	router, _ := newAgentFixture(t, &scriptedModel{script: []*schema.Message{
		toolCallMsg("call_1", "ask_user",
			`{"questions":[{"question":"选哪个方案？","options":[{"label":"A"}]}]}`),
		{Role: schema.Assistant, Content: "user chose A"},
	}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/query",
		strings.NewReader(`{"session_id":"s4","project":"p1","message":"ask me"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), req)

	// question_id 走前端同一条路径：entries/stream 的 question 事件。
	streamResp := get(t, router, "/api/v1/agent/sessions/s4/entries/stream", nil)
	questionID := questionIDFromFrames(t, parseSSE(t, streamResp.Body.String()))
	if questionID == "" {
		t.Fatalf("no question event in stream:\n%s", streamResp.Body.String())
	}

	// 错误的 question_id：400，且不消费待决问题。
	bad := postJSON(t, router, "/api/v1/agent/sessions/s4/questions/stale/answer",
		map[string]any{"answers": map[string]string{"stale": "A"}})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("stale answer status = %d, want 400", bad.Code)
	}
	// 空 answers：400。
	empty := postJSON(t, router, "/api/v1/agent/sessions/s4/questions/"+questionID+"/answer",
		map[string]any{"answers": map[string]string{}})
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty answers status = %d, want 400", empty.Code)
	}

	ok := postJSON(t, router, "/api/v1/agent/sessions/s4/questions/"+questionID+"/answer",
		map[string]any{"answers": map[string]string{questionID: "A"}})
	if ok.Code != http.StatusOK {
		t.Fatalf("answer status = %d, want 200: %s", ok.Code, ok.Body.String())
	}
	if !strings.Contains(ok.Body.String(), "user chose A") {
		t.Fatalf("resume stream missing final delta:\n%s", ok.Body.String())
	}
}

// 中断是幂等的：会话不在运行也返回 200。
func TestAgentInterrupt_Idempotent(t *testing.T) {
	router, _ := newAgentFixture(t, &scriptedModel{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/sessions/unknown/interrupt", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"idle"`) {
		t.Fatalf("body = %s, want idle", rec.Body.String())
	}
}

// questionIDFromFrames 从 SSE 帧里取 question 事件的 question_id。
func questionIDFromFrames(t *testing.T, frames []sseFrame) string {
	t.Helper()
	for _, f := range frames {
		if f.event != "question" {
			continue
		}
		var payload struct {
			QuestionID string `json:"question_id"`
		}
		if err := json.Unmarshal([]byte(f.data), &payload); err != nil {
			continue
		}
		return payload.QuestionID
	}
	return ""
}

type sseFrame struct {
	event string
	id    string
	data  string
}

func parseSSE(t *testing.T, body string) []sseFrame {
	t.Helper()
	var frames []sseFrame
	sc := bufio.NewScanner(strings.NewReader(body))
	cur := sseFrame{}
	flush := func() {
		if cur.event != "" || cur.data != "" {
			frames = append(frames, cur)
		}
		cur = sseFrame{}
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// 注释帧（心跳）。
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	flush()
	return frames
}
