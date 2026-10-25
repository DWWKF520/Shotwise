package agentrt

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit/httpkittest"

	"github.com/shotwise/shotwise/internal/agent"
	"github.com/shotwise/shotwise/internal/agent/checkpoint"
	"github.com/shotwise/shotwise/internal/agent/eventlog"
)

// scriptedModel 按脚本回放 assistant 消息（streaming 单 chunk），脚本耗尽后返回兜底文本；
// 同时记录每次调用收到的用户轮文本，供断言 / 前缀改写结果。
type scriptedModel struct {
	script []*schema.Message

	mu        sync.Mutex
	userTexts []string
}

func (m *scriptedModel) record(in []*schema.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range in {
		if msg.Role == schema.User {
			m.userTexts = append(m.userTexts, msg.Content)
		}
	}
}

// userTurns 返回历次模型调用收集到的用户轮文本。
func (m *scriptedModel) userTurns() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.userTexts...)
}

func (m *scriptedModel) next() *schema.Message {
	if len(m.script) == 0 {
		return &schema.Message{Role: schema.Assistant, Content: "done"}
	}
	msg := m.script[0]
	m.script = m.script[1:]
	return msg
}

func (m *scriptedModel) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.record(in)
	return m.next(), nil
}

func (m *scriptedModel) Stream(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.record(in)
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

// newAgentFixture 起一个不带鉴权的 agent 路由壳（认证中间件由 auth 自己的测试覆盖），
// 并为项目 p1 落一个 user-invocable 的 demo skill（/ 前缀路由测试用）。
func newAgentFixture(t *testing.T, m model.ToolCallingChatModel) (*gin.Engine, *agent.Runtime) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dataDir := t.TempDir()
	writeAgentSkill(t, dataDir, "p1", "demo")
	rt, err := agent.NewRuntime(context.Background(), agent.ModelConfig{}, dataDir,
		checkpoint.NewMemStore(), eventlog.NewMemStore(),
		agent.WithChatModel(m), agent.WithIdleTimeout(100*time.Millisecond),
		// 隔离仓库真实 profile：否则内置 skill 会被投影进临时项目，污染列表断言。
		agent.WithProfileDir(""))
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	r := gin.New()
	v1 := r.Group("/api/v1")
	RegisterAgentRoutes(v1, rt)
	RegisterAgentStreamRoutes(v1, rt)
	return r, rt
}

// writeAgentSkill 在 dataDir/<project>/.claude/skills/<name> 下写一个通用 skill。
func writeAgentSkill(t *testing.T, dataDir, project, name string) {
	t.Helper()
	dir := filepath.Join(dataDir, project, ".claude", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + name + " skill\n---\n# " + name + "\nstep one\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
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
	rec := httpkittest.PostJSON(t, router, "/api/v1/agent/query", map[string]any{"message": ""})
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
	bad := httpkittest.PostJSON(t, router, "/api/v1/agent/sessions/s4/questions/stale/answer",
		map[string]any{"answers": map[string]string{"stale": "A"}})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("stale answer status = %d, want 400", bad.Code)
	}
	// 空 answers：400。
	empty := httpkittest.PostJSON(t, router, "/api/v1/agent/sessions/s4/questions/"+questionID+"/answer",
		map[string]any{"answers": map[string]string{}})
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty answers status = %d, want 400", empty.Code)
	}

	ok := httpkittest.PostJSON(t, router, "/api/v1/agent/sessions/s4/questions/"+questionID+"/answer",
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

// postQuery 提交一条 /agent/query 并返回响应记录。
func postQuery(t *testing.T, router *gin.Engine, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return httpkittest.PostJSON(t, router, "/api/v1/agent/query", body)
}

// / 前缀输入：正文只送 /name 之后的参数，轮输入带 skill 触发前缀。
func TestAgentQuery_SlashInvokesSkill(t *testing.T) {
	m := &scriptedModel{script: []*schema.Message{{Role: schema.Assistant, Content: "ok"}}}
	router, _ := newAgentFixture(t, m)

	rec := postQuery(t, router, map[string]any{"session_id": "slash-1", "project": "p1", "message": "/demo 生成视频"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var found bool
	for _, turn := range m.userTurns() {
		if strings.Contains(turn, "[用户请求触发 skill: demo] 生成视频") {
			found = true
		}
	}
	if !found {
		t.Fatalf("模型用户轮未带 skill 前缀: %v", m.userTurns())
	}
}

// skill 不存在：400 结构化错误，附项目内可用 skill 列表。
func TestAgentQuery_SlashUnknownSkillRejected(t *testing.T) {
	router, _ := newAgentFixture(t, &scriptedModel{})

	rec := postQuery(t, router, map[string]any{"session_id": "slash-2", "project": "p1", "message": "/nope go"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code      string   `json:"code"`
			Message   string   `json:"message"`
			Available []string `json:"available"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "skill_not_invocable" {
		t.Fatalf("code = %q", body.Error.Code)
	}
	if len(body.Error.Available) != 1 || body.Error.Available[0] != "demo" {
		t.Fatalf("available = %v, want [demo]", body.Error.Available)
	}
	if body.Error.Message == "" {
		t.Fatal("message 不能为空")
	}
}

// / 不在行首或首 token 含 /：按普通文本透传，不触发 skill。
func TestAgentQuery_SlashNotACommandPassesThrough(t *testing.T) {
	m := &scriptedModel{script: []*schema.Message{{Role: schema.Assistant, Content: "ok"}}}
	router, _ := newAgentFixture(t, m)

	rec := postQuery(t, router, map[string]any{"session_id": "slash-3", "project": "p1", "message": "/api/v1/x hello"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	turns := m.userTurns()
	if len(turns) == 0 || !strings.Contains(turns[0], "/api/v1/x hello") {
		t.Fatalf("非命令输入应原样透传: %v", turns)
	}
	for _, turn := range turns {
		if strings.Contains(turn, "[用户请求触发 skill:") {
			t.Fatalf("非命令输入不应带 skill 前缀: %v", turns)
		}
	}
}

// GET /agent/skills?project= → user-invocable skill 列表；缺 project → 400。
func TestAgentSkillsList(t *testing.T) {
	router, _ := newAgentFixture(t, &scriptedModel{})

	rec := get(t, router, "/api/v1/agent/skills?project=p1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Skills []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Skills) != 1 || body.Skills[0].Name != "demo" {
		t.Fatalf("skills = %+v, want [demo]", body.Skills)
	}
	if body.Skills[0].Description == "" {
		t.Fatal("skill description 不能为空")
	}

	// 无 project 参数：400。
	bad := get(t, router, "/api/v1/agent/skills", nil)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("missing project status = %d, want 400", bad.Code)
	}

	// 未知项目：空列表而非报错（目录不存在）。
	empty := get(t, router, "/api/v1/agent/skills?project=nope", nil)
	if empty.Code != http.StatusOK {
		t.Fatalf("unknown project status = %d, want 200", empty.Code)
	}
	if !strings.Contains(empty.Body.String(), `"skills":[]`) {
		t.Fatalf("unknown project body = %s, want empty list", empty.Body.String())
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
