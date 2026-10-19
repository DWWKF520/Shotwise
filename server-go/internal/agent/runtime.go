// Package agent 是用 Eino 重建的 Shotwise 智能体运行时（设计文档
// docs/go-migration/03-agent-runtime-eino.md §9–§16）。
//
// 装配总图：transport(SSE) ← eventAdapter ← TurnLoop(每会话一个) ← Runner ← ChatModelAgent。
//   - TurnLoop 替代 Python 的 SessionActor：新 query 排队非抢占（Push），
//     interrupt 走 Stop(WithGraceful) 安全点收口，checkpoint 保留可恢复。
//   - ask_user HITL：工具内 compose.Interrupt 挂起 → checkpoint 落 SQL →
//     Runtime.Answer 用 ResumeParams{Targets:{interruptID: answer}} 恢复。
//   - guard middleware 先于 filesystem 接入（ADR 0025 硬失败围栏）。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/shotwise/shotwise/internal/agent/checkpoint"
	"github.com/shotwise/shotwise/internal/agent/eventlog"
	"github.com/shotwise/shotwise/internal/agent/policy"
	"github.com/shotwise/shotwise/internal/agent/skills"
	"github.com/shotwise/shotwise/internal/agent/subagents"
	"github.com/shotwise/shotwise/internal/agent/workspace"
	"github.com/shotwise/shotwise/internal/service"
)

// 进入 checkpoint / 事件流的 Shotwise 自定义类型在此统一注册 gob 名。
// key 一旦发布不可改（Eino checkpoint 序列化断裂风险，设计文档 §7），
// 命名规范 "shotwise.agent.<Type>"，v1 一次定死。
func init() {
	schema.RegisterName[InboxItem]("shotwise.agent.InboxItem")
	schema.RegisterName[AnswerPayload]("shotwise.agent.AnswerPayload")
	schema.RegisterName[AskUserInput]("shotwise.agent.AskUserInput")
}

// --- 收件箱与 HITL 类型 ---

// InboxKind 标识 TurnLoop 收件箱 item 的类别。
type InboxKind string

const (
	InboxUserMessage      InboxKind = "user_message"      // 用户新输入
	InboxTaskNotification InboxKind = "task_notification" // 后台任务完成通知
	InboxAnswer           InboxKind = "question_answer"   // HITL 恢复：用户对 ask_user 的回答
)

// InboxItem 是 TurnLoop 的收件箱 item（对应 Python SessionActor 的命令队列元素）。
type InboxItem struct {
	Kind   InboxKind
	Text   string // InboxUserMessage / InboxTaskNotification 的正文
	Answer *AnswerPayload
	// SkillHint 非空时表示用户显式触发该 skill（§17.5）：轮输入前缀明示，模型按名调用 skill 工具。
	SkillHint string
}

// userTurnText 把 user_message 渲染为送入模型的文本（显式 skill 触发加前缀）。
func userTurnText(it InboxItem) string {
	if it.SkillHint == "" {
		return it.Text
	}
	return fmt.Sprintf("[用户请求触发 skill: %s] %s", it.SkillHint, it.Text)
}

// AnswerPayload 是用户对 ask_user 的回答（HITL 恢复参数）。
// Answers 按 question_id 给出用户选择/输入文本，与 Python 侧 answers dict 同构。
type AnswerPayload struct {
	Answers map[string]string `json:"answers"`
}

// Text 把回答摊平成给模型看的文本（按 question_id 标注）。
func (p AnswerPayload) Text() string {
	ids := make([]string, 0, len(p.Answers))
	for id := range p.Answers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%s: %s", id, p.Answers[id]))
	}
	return strings.Join(parts, "\n")
}

// PendingQuestion 是待用户回答的问题快照：interruptID 是 Eino 侧的中断上下文
// ID（恢复时作为 ResumeParams.Targets 的 key，对外即 question_id）。
// Seq 是 question 条目在事件日志里的 seq——重连冷订阅时按它把问题补发回去，
// 前端不必再从条目正文里反解。
type PendingQuestion struct {
	SessionID string        `json:"session_id"`
	ID        string        `json:"question_id"`
	Seq       int64         `json:"seq"`
	Questions []AskQuestion `json:"questions"`
}

// AskQuestion 描述 ask_user 的单个问题。
type AskQuestion struct {
	Question    string      `json:"question"`
	Header      string      `json:"header,omitempty"`
	Options     []AskOption `json:"options"`
	MultiSelect bool        `json:"multi_select,omitempty"`
}

// AskOption 是问题的候选项。
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// AskUserInput 是 ask_user 工具的入参（compose.Interrupt 挂起时透出到事件流）。
type AskUserInput struct {
	Questions []AskQuestion `json:"questions"`
}

// --- 事件广播 ---

// eventBus 是会话内 SSE 事件的订阅总线：Query / ReceiveResponse 各自订阅，
// loop 退出时统一关闭。溢出策略 v1 为非阻塞丢弃（Python 侧
// EvictNonCriticalAndSignal 随 SSE 通道重构增量实现）。
type eventBus struct {
	mu     sync.Mutex
	subs   map[chan<- Event]struct{}
	closed bool
}

func newEventBus() *eventBus {
	return &eventBus{subs: map[chan<- Event]struct{}{}}
}

// Subscribe 注册订阅者，返回事件 channel 与退订函数。
func (b *eventBus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return ch, func() {}
	}
	b.subs[ch] = struct{}{}
	unsub := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
	}
	return ch, unsub
}

// Broadcast 广播事件给全部订阅者（channel 满则丢弃该事件，不阻塞 agent）。
func (b *eventBus) Broadcast(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Close 关闭总线：所有订阅 channel 关闭，后续 Broadcast / Subscribe 无效。
func (b *eventBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.subs {
		close(ch)
	}
	b.subs = map[chan<- Event]struct{}{}
}

// --- Runtime ---

// sessionLoop 是一个会话的活跃 TurnLoop 及其事件总线。
type sessionLoop struct {
	sessionID string
	project   string
	loop      *adk.TurnLoop[InboxItem, *schema.Message]
	bus       *eventBus
	adapter   *eventAdapter
	// 恢复参数（GenResume 闭包读取）：仅 Answer 重建的 loop 使用。
	interruptID string
	answer      *AnswerPayload
}

// Runtime 是智能体运行时对外门面（对应 Python 的 AssistantService + SessionManager）。
type Runtime struct {
	cfg         ModelConfig
	dataDir     string // 项目数据根目录
	idleTimeout time.Duration
	secretKeys  []string

	sessions *SessionManager
	cpStore  CheckPointStore
	evStore  eventlog.Store

	chatModelOnce sync.Once
	chatModelInst model.ToolCallingChatModel
	chatModelErr  error
	// chatModelOverride 优先于惰性构造（测试注入 / 未来 provider 链扩展位）。
	chatModelOverride model.ToolCallingChatModel
	// extraTools 追加在 BuiltinTools 之后的扩展工具（测试 / 后续增量注入位）。
	extraTools []tool.BaseTool
	// assetWriter / projectWriter 为 patch_project 的写入依赖（WithProjectWriters 注入）。
	assetWriter   AssetWriter
	projectWriter ProjectWriter
	// assetQueue / assetProjects 为 generate_assets 的依赖（WithAssetGeneration 注入）。
	assetQueue    AssetQueue
	assetProjects AssetProjects
	// videoCaps 为 get_video_capabilities 的能力解析器（WithVideoCapabilities 注入）。
	videoCaps VideoCapabilityReader
	// relationGenerator / relationProjects 为 analyze_character_relations 的依赖（WithRelationAnalysis 注入）。
	relationGenerator RelationGeneratorFactory
	relationProjects  RelationProjects
	// scriptStore / assetRenamer 为剧本编辑与级联改名工具的依赖（WithScriptEditing / WithAssetRename 注入）。
	scriptStore  ScriptStore
	assetRenamer AssetRenamer
	// generationQueue / generationProjects 为生成类工具（分镜图 / 旁白配音）的依赖。
	generationQueue    AssetQueue
	generationProjects GenerationProjects
	// imageEditQueue / imageEditProjects / imageEditCaps 为 edit_images 的依赖（WithImageEditTools 注入）。
	imageEditQueue    AssetQueue
	imageEditProjects GenerationProjects
	imageEditCaps     ImageEditCapabilityReader
	// gridQueue / gridProjects / gridCaps / gridSplitter 为 generate_grid 的依赖（WithGridTools 注入）。
	gridQueue    AssetQueue
	gridProjects GenerationProjects
	gridCaps     service.ImageCapabilityResolver
	gridSplitter GridSplitter
	// episodeStore / episodeTextGen 为分集规划工具的依赖（WithEpisodePlanning 注入）。
	episodeStore   service.EpisodeStore
	episodeTextGen service.TextCompletionFactory
	// textGen / scriptProjects 为剧本生成类工具（说书片段拆分 / 剧本规范化等）的依赖
	// （WithTextGenerationTools 注入）。
	textGen        service.TextCompletionFactory
	scriptProjects TextGenerationProjects
	// reviewProjects 为 step1 审核确认工具的依赖（WithScriptReviewTools 注入）。
	reviewProjects ScriptReviewProjects
	// memoryStore 为 propose_memory_candidate 的依赖（WithMemoryProposals 注入）。
	memoryStore service.MemoryCandidateStore
	// textGate 记录各项目最近一次文本清单签名，供 read_project_text 门禁使用。
	textGate textFileGate

	mu                sync.Mutex
	active            map[string]*sessionLoop
	pendingInterrupts map[string]*PendingQuestion // session → 待答问题
}

// CheckPointStore 是 checkpoint 存储接口（兼容 adk.CheckPointStore）。
type CheckPointStore = interface {
	Set(ctx context.Context, checkPointID string, checkPoint []byte) error
	Get(ctx context.Context, checkPointID string) ([]byte, bool, error)
}

// RuntimeOption 是 Runtime 的可选配置项。
type RuntimeOption func(*Runtime)

// WithIdleTimeout 设置会话 loop 的空闲自动退出时长（0 = 不自动退出）。
func WithIdleTimeout(d time.Duration) RuntimeOption {
	return func(r *Runtime) { r.idleTimeout = d }
}

// WithSecretEnvKeys 设置 Bash 执行前需剥离的密钥环境变量名。
func WithSecretEnvKeys(keys []string) RuntimeOption {
	return func(r *Runtime) { r.secretKeys = append([]string(nil), keys...) }
}

// WithChatModel 注入会话主模型，跳过 provider 链的惰性构造（测试替身 / 固定模型部署）。
func WithChatModel(m model.ToolCallingChatModel) RuntimeOption {
	return func(r *Runtime) { r.chatModelOverride = m }
}

// NewRuntime 构造运行时。cpStore / evStore 传 nil 时使用内存实现（演示用）。
func NewRuntime(ctx context.Context, cfg ModelConfig, dataDir string, cpStore CheckPointStore, evStore eventlog.Store, opts ...RuntimeOption) (*Runtime, error) {
	if cpStore == nil {
		cpStore = checkpoint.NewMemStore()
	}
	if evStore == nil {
		evStore = eventlog.NewMemStore()
	}
	r := &Runtime{
		cfg:               cfg,
		dataDir:           dataDir,
		sessions:          NewSessionManager(0),
		cpStore:           cpStore,
		evStore:           evStore,
		active:            map[string]*sessionLoop{},
		pendingInterrupts: map[string]*PendingQuestion{},
	}
	for _, opt := range opts {
		opt(r)
	}
	// 内置 middleware 的注入文案（skill 加载提示等）统一中文。
	_ = adk.SetLanguage(adk.LanguageChinese)
	return r, nil
}

// chatModel 返回会话模型：override 优先（测试注入），否则惰性构造（失败缓存错误）。
func (r *Runtime) chatModel(ctx context.Context) (model.ToolCallingChatModel, error) {
	if r.chatModelOverride != nil {
		return r.chatModelOverride, nil
	}
	r.chatModelOnce.Do(func() {
		r.chatModelInst, r.chatModelErr = NewChatModel(ctx, r.cfg)
	})
	return r.chatModelInst, r.chatModelErr
}

// Query 提交一条用户消息（对应 Python client_factory.Query）。
// 会话已有活跃 loop 时排队（非抢占）；否则新建 loop。返回 SSE 事件流与退订函数。
func (r *Runtime) Query(ctx context.Context, sessionID, project, message string) (string, <-chan Event, func(), error) {
	return r.submitQuery(ctx, sessionID, project, InboxItem{Kind: InboxUserMessage, Text: message}, false)
}

// ErrSkillNotInvocable 表示显式触发的 skill 在项目中不存在或不可由用户触发。
var ErrSkillNotInvocable = errors.New("agent: skill is not user-invocable in this project")

// QueryWithSkill 提交一条用户消息并显式触发 skill（§17.5）。skill 为空时等价于 Query。
func (r *Runtime) QueryWithSkill(ctx context.Context, sessionID, project, skill, message string) (string, <-chan Event, func(), error) {
	if skill == "" {
		return r.Query(ctx, sessionID, project, message)
	}
	entries, err := skills.ListUserInvocable(filepath.Join(r.dataDir, project, ".claude", "skills"))
	if err != nil {
		return sessionID, nil, nil, err
	}
	if !slices.ContainsFunc(entries, func(e skills.CatalogEntry) bool { return e.Name == skill }) {
		return sessionID, nil, nil, fmt.Errorf("%w: %s", ErrSkillNotInvocable, skill)
	}
	return r.submitQuery(ctx, sessionID, project, InboxItem{Kind: InboxUserMessage, Text: message, SkillHint: skill}, false)
}

// QueryPrelogged 提交一条已由调用方写入事件日志的用户消息（持久会话受理路径）：
// 服务层先以 client_key 幂等落库拿权威条目，再经本方法入队，避免 submitQuery
// 内置的用户条目写入造成重复。
func (r *Runtime) QueryPrelogged(ctx context.Context, sessionID, project, message string) (string, <-chan Event, func(), error) {
	return r.submitQuery(ctx, sessionID, project, InboxItem{Kind: InboxUserMessage, Text: message}, true)
}

// submitQuery 把一条用户输入排入会话 loop（Query / QueryWithSkill / QueryPrelogged 共用）。
// preLogged 为 true 时消息已由调用方写入事件日志，此处不再重复落库。
func (r *Runtime) submitQuery(ctx context.Context, sessionID, project string, item InboxItem, preLogged bool) (string, <-chan Event, func(), error) {
	if item.Text == "" {
		return "", nil, nil, fmt.Errorf("agent: message is required")
	}
	if sessionID == "" {
		sessionID = NewSessionID()
	}
	r.sessions.GetOrCreate(sessionID, project)

	sl, err := r.ensureLoop(sessionID, project)
	if err != nil {
		return sessionID, nil, nil, err
	}
	ch, unsub := sl.bus.Subscribe()
	if !preLogged {
		sl.adapter.appendUserEntries(ctx, item.Text)
	}
	if ok, _ := sl.loop.Push(item); !ok {
		unsub()
		return sessionID, nil, nil, fmt.Errorf("agent: session %s is shutting down", sessionID)
	}
	return sessionID, ch, unsub, nil
}

// ReceiveResponse 订阅一个活跃会话的事件流（重连 / 追加监听）。
func (r *Runtime) ReceiveResponse(_ context.Context, sessionID string) (<-chan Event, func(), error) {
	r.mu.Lock()
	sl := r.active[sessionID]
	r.mu.Unlock()
	if sl == nil {
		return nil, nil, fmt.Errorf("agent: session %s is not active", sessionID)
	}
	ch, unsub := sl.bus.Subscribe()
	return ch, unsub, nil
}

// Interrupt 中断会话当前轮（安全点收口，checkpoint 保留可恢复）。
func (r *Runtime) Interrupt(_ context.Context, sessionID string) error {
	r.mu.Lock()
	sl := r.active[sessionID]
	r.mu.Unlock()
	if sl == nil {
		return fmt.Errorf("agent: session %s is not active", sessionID)
	}
	sl.loop.Stop(adk.WithGraceful())
	return nil
}

// Answer 回答待决问题并从 checkpoint 恢复轮次（HITL 续跑，设计文档 §13.2）。
// 返回恢复轮的事件订阅：订阅在 Push 前建立，保证恢复事件不丢。
func (r *Runtime) Answer(ctx context.Context, sessionID string, answer AnswerPayload) (<-chan Event, func(), error) {
	r.mu.Lock()
	pending := r.pendingInterrupts[sessionID]
	delete(r.pendingInterrupts, sessionID) // 消费即清除：重复 Answer 报无待决问题
	r.mu.Unlock()
	if pending == nil {
		return nil, nil, fmt.Errorf("agent: session %s has no pending question", sessionID)
	}
	sess := r.sessions.Get(sessionID)
	if sess == nil {
		return nil, nil, fmt.Errorf("agent: session %s not found", sessionID)
	}
	// 业务中断后旧 loop 已退出；重建 loop 触发 GenResume → ResumeParams 恢复。
	r.mu.Lock()
	delete(r.active, sessionID) // 保险：Wait 清理竞态下重复注册
	r.mu.Unlock()
	sl := r.newSessionLoop(sessionID, sess.Project)
	sl.interruptID = pending.ID
	sl.answer = &answer
	r.mu.Lock()
	r.active[sessionID] = sl
	r.mu.Unlock()
	ch, unsub := sl.bus.Subscribe()
	sl.adapter.appendSystemNotice(ctx, eventlog.SubtypeQuestionAnswer, answer.Text())
	if ok, _ := sl.loop.Push(InboxItem{Kind: InboxAnswer, Answer: &answer}); !ok {
		unsub()
		return nil, nil, fmt.Errorf("agent: session %s is shutting down", sessionID)
	}
	return ch, unsub, nil
}

// AnswerQuestion 按 question_id 回答待决问题（HTTP 入口）。question_id 必须与
// 当前待答问题一致，避免旧标签页的迟到回答恢复新一轮的 checkpoint。
func (r *Runtime) AnswerQuestion(ctx context.Context, sessionID, questionID string, answers map[string]string) (<-chan Event, func(), error) {
	if len(answers) == 0 {
		return nil, nil, fmt.Errorf("agent: answers are required")
	}
	r.mu.Lock()
	pending := r.pendingInterrupts[sessionID]
	r.mu.Unlock()
	if pending == nil {
		return nil, nil, fmt.Errorf("agent: session %s has no pending question", sessionID)
	}
	if pending.ID != questionID {
		return nil, nil, fmt.Errorf("agent: question %s is not pending on session %s", questionID, sessionID)
	}
	return r.Answer(ctx, sessionID, AnswerPayload{Answers: answers})
}

// IsActive 报告会话当前是否有活跃的 TurnLoop（running 状态判定）。
func (r *Runtime) IsActive(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.active[sessionID]
	return ok
}

// PendingQuestionOf 返回会话当前待答的问题快照；没有待答问题时 ok=false。
// 待决问题只存在于内存：进程重启后 checkpoint 仍在，但问题结构需从事件日志
// 的 question 条目重建，重建随分支能力一并接入。
func (r *Runtime) PendingQuestionOf(sessionID string) (PendingQuestion, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pendingInterrupts[sessionID]
	if p == nil {
		return PendingQuestion{}, false
	}
	return *p, true
}

// Entries 冷读会话事件日志（历史回放，afterSeq 为 seq 游标，-1 表示从头）。
func (r *Runtime) Entries(ctx context.Context, sessionID string, afterSeq int64) ([]*eventlog.Entry, error) {
	return r.evStore.List(ctx, sessionID, afterSeq)
}

// Stream 订阅会话事件流：先按 afterSeq 游标回放事件日志存量条目，再接直播。
//
// 订阅先于库读建立（无缝衔接）；重复投递的条目由 seq 门槛过滤——身份比对，
// 不是内容比对。带 seq 的事件（entry / tool_use / tool_result / question）在
// lastSeq 之下会被丢弃，delta 等无条目事件（seq=0）一律放行。
// 会话当前无活跃 loop 时，回放完存量条目即产出终态 status 并关闭。
func (r *Runtime) Stream(ctx context.Context, sessionID string, afterSeq int64) (<-chan Event, func(), error) {
	r.mu.Lock()
	sl := r.active[sessionID]
	r.mu.Unlock()

	var live <-chan Event
	unsub := func() {}
	if sl != nil {
		ch, cancel := sl.bus.Subscribe()
		live = ch
		unsub = cancel
	}

	entries, err := r.evStore.List(ctx, sessionID, afterSeq)
	if err != nil {
		unsub()
		return nil, nil, err
	}

	out := make(chan Event, len(entries)+64)
	stop := make(chan struct{})
	var once sync.Once
	stopAll := func() {
		once.Do(func() { close(stop); unsub() })
	}

	go func() {
		defer close(out)
		lastSeq := afterSeq
		for _, e := range entries {
			if e.Seq > lastSeq {
				lastSeq = e.Seq
			}
			select {
			case out <- Event{SessionID: sessionID, Seq: e.Seq, Type: EventEntry, Raw: mustJSON(e)}:
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
		// 存量条目之后补发未决问题：重连的订阅方要能继续答题，不必等下一次挂起。
		if pending, ok := r.PendingQuestionOf(sessionID); ok {
			select {
			case out <- pendingQuestionEvent(sessionID, pending):
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
		if sl == nil {
			// 非 running 会话：存量条目播完，未决问题补发后给终态即结束。
			select {
			case out <- Event{SessionID: sessionID, Type: EventStatus, Text: "idle"}:
			case <-stop:
			case <-ctx.Done():
			}
			return
		}
		for {
			select {
			case ev, ok := <-live:
				if !ok {
					return // loop 退出，bus 关闭
				}
				if ev.Seq > 0 {
					if ev.Seq <= lastSeq {
						continue // 库读与直播重复投递
					}
					lastSeq = ev.Seq
				}
				select {
				case out <- ev:
				case <-stop:
					return
				case <-ctx.Done():
					return
				}
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, stopAll, nil
}

// pendingQuestionEvent 把待决问题渲染成 question 事件（冷订阅补发用）。
func pendingQuestionEvent(sessionID string, p PendingQuestion) Event {
	return Event{
		SessionID: sessionID,
		Seq:       p.Seq,
		Type:      EventQuestion,
		Raw: mustJSON(questionPayload{
			SessionID:  sessionID,
			QuestionID: p.ID,
			Questions:  p.Questions,
		}),
	}
}

// StopSession 显式停止一个会话（测试与运维入口）。
func (r *Runtime) StopSession(sessionID string) {
	r.mu.Lock()
	sl := r.active[sessionID]
	r.mu.Unlock()
	if sl != nil {
		sl.loop.Stop()
	}
}

// Close 停止全部会话 loop。
func (r *Runtime) Close() {
	r.mu.Lock()
	loops := make([]*sessionLoop, 0, len(r.active))
	for _, sl := range r.active {
		loops = append(loops, sl)
	}
	r.mu.Unlock()
	for _, sl := range loops {
		sl.loop.Stop(adk.WithGraceful())
	}
}

// ListSessions 返回所有会话的轻量信息。
func (r *Runtime) ListSessions() []SessionInfo { return r.sessions.List() }

// ensureLoop 取活跃 loop，不存在则新建（含并发注册互斥）。
func (r *Runtime) ensureLoop(sessionID, project string) (*sessionLoop, error) {
	r.mu.Lock()
	sl := r.active[sessionID]
	r.mu.Unlock()
	if sl != nil {
		return sl, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if sl = r.active[sessionID]; sl != nil {
		return sl, nil
	}
	sl = r.newSessionLoop(sessionID, project)
	r.active[sessionID] = sl
	return sl, nil
}

// newSessionLoop 构造并启动一个会话的 TurnLoop（含事件适配与空闲自动退出）。
func (r *Runtime) newSessionLoop(sessionID, project string) *sessionLoop {
	sl := &sessionLoop{sessionID: sessionID, project: project, bus: newEventBus()}
	sl.adapter = &eventAdapter{
		sessionID:    sessionID,
		store:        r.evStore,
		emit:         sl.bus.Broadcast,
		skillTool:    skills.SkillToolName,
		subagents:    r.subagentNames(project),
		pendingCalls: map[string][]string{},
		onInterrupted: func(q *PendingQuestion) {
			r.mu.Lock()
			r.pendingInterrupts[sessionID] = q
			r.mu.Unlock()
		},
	}
	cfg := adk.TurnLoopConfig[InboxItem, *schema.Message]{
		GenInput:     r.genInput(sl),
		GenResume:    r.genResume(sl),
		PrepareAgent: r.prepareAgent(sessionID, project),
		OnAgentEvents: func(ctx context.Context, _ *adk.TurnContext[InboxItem, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error {
			return sl.adapter.consume(ctx, events)
		},
		Store:        r.cpStore,
		CheckpointID: sessionID, // checkpointID = sessionID（设计文档 §9.2）
	}
	sl.loop = adk.NewTurnLoop(cfg)
	sl.loop.Run(context.Background())
	if r.idleTimeout > 0 {
		// 空闲自动退出：新 Push 会重置计时；退出后 Wait 清理并关 bus。
		sl.loop.Stop(adk.UntilIdleFor(r.idleTimeout))
	}
	go func() {
		state := sl.loop.Wait()
		r.mu.Lock()
		if cur := r.active[sessionID]; cur == sl {
			delete(r.active, sessionID)
		}
		r.mu.Unlock()
		// 中断退出后 checkpoint 已由 TurnLoop 保存；未处理 item v1 记入事件日志兜底。
		if len(state.UnhandledItems) > 0 {
			for _, it := range state.UnhandledItems {
				sl.adapter.appendSystemNotice(context.Background(), eventlog.SubtypeTaskNotification,
					"unhandled inbox item: "+string(it.Kind))
			}
		}
		sl.bus.Close()
	}()
	return sl
}

// genInput 构造普通轮输入：user_message 为主文本，task_notification 折叠附在同轮。
func (r *Runtime) genInput(sl *sessionLoop) func(context.Context, *adk.TurnLoop[InboxItem, *schema.Message], []InboxItem) (*adk.GenInputResult[InboxItem, *schema.Message], error) {
	return func(ctx context.Context, _ *adk.TurnLoop[InboxItem, *schema.Message], items []InboxItem) (*adk.GenInputResult[InboxItem, *schema.Message], error) {
		msgs := make([]*schema.Message, 0, len(items))
		consumed := make([]InboxItem, 0, len(items))
		var notices []string
		for _, it := range items {
			switch it.Kind {
			case InboxUserMessage:
				msgs = append(msgs, schema.UserMessage(userTurnText(it)))
				consumed = append(consumed, it)
			case InboxTaskNotification:
				// 折叠为系统提示附在同轮（对应 Python _process_inbox）。
				notices = append(notices, it.Text)
				consumed = append(consumed, it)
			case InboxAnswer:
				// GenInput 不消费 answer（恢复走 GenResume）；留在 Remaining。
			}
		}
		if len(consumed) == 0 {
			// 本批全是 answer：不该走 GenInput，交给 GenResume 处理。
			return &adk.GenInputResult[InboxItem, *schema.Message]{
				Consumed:  nil,
				Remaining: items,
				Input:     &adk.AgentInput{Messages: []*schema.Message{schema.UserMessage("")}, EnableStreaming: true},
			}, nil
		}
		if len(notices) > 0 {
			msgs = append(msgs, schema.UserMessage("[系统通知] "+joinTexts(notices)))
		}
		return &adk.GenInputResult[InboxItem, *schema.Message]{
			// RunCtx 显式传回：TurnLoop 要求派生自入参 ctx 以保留取消语义。
			RunCtx:   ctx,
			Input:    &adk.AgentInput{Messages: msgs, EnableStreaming: true},
			Consumed: consumed,
		}, nil
	}
}

// genResume 构造恢复轮计划：历史已在 checkpoint 的 agent state 里，
// interrupted / unhandled 的 inbox items 不重放（drop），只消费本轮 answer。
func (r *Runtime) genResume(sl *sessionLoop) func(context.Context, *adk.TurnLoop[InboxItem, *schema.Message], []InboxItem, []InboxItem, []InboxItem) (*adk.GenResumeResult[InboxItem, *schema.Message], error) {
	return func(ctx context.Context, _ *adk.TurnLoop[InboxItem, *schema.Message], interrupted, unhandled, newItems []InboxItem) (*adk.GenResumeResult[InboxItem, *schema.Message], error) {
		consumed := make([]InboxItem, 0, len(newItems))
		for _, it := range newItems {
			if it.Kind == InboxAnswer {
				consumed = append(consumed, it)
			}
		}
		rp := &adk.ResumeParams{}
		if sl.answer != nil && sl.interruptID != "" {
			rp.Targets = map[string]any{sl.interruptID: *sl.answer}
		}
		return &adk.GenResumeResult[InboxItem, *schema.Message]{
			RunCtx:       ctx,
			ResumeParams: rp,
			Consumed:     consumed,
			// interrupted/unhandled 落在 Remaining 之外即被 drop：不重放已入历史的消息。
			Remaining: filterItems(newItems, func(it InboxItem) bool { return it.Kind != InboxAnswer }),
		}, nil
	}
}

// prepareAgent 返回每轮装配回调：CLAUDE.md / 长期记忆 / SessionValues 热更新
// 在后续增量接入；v1 只装配系统提示 + 工具 + guard middleware。
func (r *Runtime) prepareAgent(sessionID, project string) func(context.Context, *adk.TurnLoop[InboxItem, *schema.Message], []InboxItem) (adk.Agent, error) {
	return func(ctx context.Context, _ *adk.TurnLoop[InboxItem, *schema.Message], _ []InboxItem) (adk.Agent, error) {
		return r.buildAgent(ctx, sessionID, project)
	}
}

// buildTools 装配项目绑定的全部业务工具（不含 middleware 与 subagent）。
//
// 独立于 buildAgent：注册校验（如「37 个工具全部在册」的断言）只需这一层，不必构造 chat model
// 与 middleware 链。
func (r *Runtime) buildTools(ctx context.Context, sessionID, project string) ([]tool.BaseTool, error) {
	tools, err := BuiltinTools(r.dataDir, project)
	if err != nil {
		return nil, err
	}
	askUser, err := newAskUserTool()
	if err != nil {
		return nil, fmt.Errorf("agent: build ask_user: %w", err)
	}
	tools = append(tools, askUser)
	tools = append(tools, r.extraTools...)
	listTextFiles, err := newListProjectTextFilesTool(project, r.projectRoot(project), &r.textGate)
	if err != nil {
		return nil, fmt.Errorf("agent: build list_project_text_files: %w", err)
	}
	readText, err := newReadProjectTextTool(project, r.projectRoot(project), &r.textGate)
	if err != nil {
		return nil, fmt.Errorf("agent: build read_project_text: %w", err)
	}
	tools = append(tools, listTextFiles, readText)
	if r.assetProjects != nil {
		pending, err := newListPendingAssetsTool(project, r.assetProjects)
		if err != nil {
			return nil, fmt.Errorf("agent: build list_pending_assets: %w", err)
		}
		tools = append(tools, pending)
	}
	if r.videoCaps != nil && r.assetProjects != nil {
		caps, err := newGetVideoCapabilitiesTool(project, r.videoCaps, r.assetProjects)
		if err != nil {
			return nil, fmt.Errorf("agent: build get_video_capabilities: %w", err)
		}
		tools = append(tools, caps)
	}
	if r.relationGenerator != nil && r.relationProjects != nil {
		relations, err := newAnalyzeCharacterRelationsTool(project, r.relationGenerator, r.relationProjects)
		if err != nil {
			return nil, fmt.Errorf("agent: build analyze_character_relations: %w", err)
		}
		tools = append(tools, relations)
	}
	if r.assetProjects != nil {
		review, err := newInspectDirectorReviewTool(project, r.assetProjects)
		if err != nil {
			return nil, fmt.Errorf("agent: build inspect_director_review: %w", err)
		}
		tools = append(tools, review)
	}
	if r.scriptStore != nil {
		builders := []struct {
			name string
			ctor func() (tool.InvokableTool, error)
		}{
			{"patch_episode_script", func() (tool.InvokableTool, error) { return newPatchEpisodeScriptTool(project, r.scriptStore) }},
			{"insert_segment", func() (tool.InvokableTool, error) { return newInsertSegmentTool(project, r.scriptStore) }},
			{"remove_segment", func() (tool.InvokableTool, error) { return newRemoveSegmentTool(project, r.scriptStore) }},
			{"split_segment", func() (tool.InvokableTool, error) { return newSplitSegmentTool(project, r.scriptStore) }},
			{"patch_episode_meta", func() (tool.InvokableTool, error) { return newPatchEpisodeMetaTool(project, r.scriptStore) }},
		}
		for _, b := range builders {
			built, err := b.ctor()
			if err != nil {
				return nil, fmt.Errorf("agent: build %s: %w", b.name, err)
			}
			tools = append(tools, built)
		}
	}
	if r.assetRenamer != nil {
		rename, err := newRenameAssetTool(project, r.assetRenamer)
		if err != nil {
			return nil, fmt.Errorf("agent: build rename_asset: %w", err)
		}
		tools = append(tools, rename)
	}
	if r.generationQueue != nil && r.generationProjects != nil {
		generators := []struct {
			name string
			ctor func() (tool.InvokableTool, error)
		}{
			{"generate_storyboards", func() (tool.InvokableTool, error) {
				return newGenerateStoryboardsTool(project, r.generationQueue, r.generationProjects)
			}},
			{"generate_narration_audio", func() (tool.InvokableTool, error) {
				return newGenerateNarrationAudioTool(project, r.generationQueue, r.generationProjects)
			}},
		}
		for _, g := range generators {
			built, err := g.ctor()
			if err != nil {
				return nil, fmt.Errorf("agent: build %s: %w", g.name, err)
			}
			tools = append(tools, built)
		}
	}
	if r.imageEditQueue != nil && r.imageEditProjects != nil && r.imageEditCaps != nil {
		edit, err := newEditImagesTool(project, r.imageEditQueue, r.imageEditProjects, r.imageEditCaps)
		if err != nil {
			return nil, fmt.Errorf("agent: build edit_images: %w", err)
		}
		tools = append(tools, edit)
	}
	if r.gridQueue != nil && r.gridProjects != nil && r.gridCaps != nil {
		grid, err := newGenerateGridTool(project, r.gridQueue, r.gridProjects, r.gridCaps, r.gridSplitter)
		if err != nil {
			return nil, fmt.Errorf("agent: build generate_grid: %w", err)
		}
		tools = append(tools, grid)
	}
	if r.episodeStore != nil {
		reset, err := newResetEpisodePlanningTool(project, r.episodeStore)
		if err != nil {
			return nil, fmt.Errorf("agent: build reset_episode_planning: %w", err)
		}
		tools = append(tools, reset)
		if r.episodeTextGen != nil {
			plan, err := newPlanEpisodesTool(project, r.episodeStore, r.episodeTextGen)
			if err != nil {
				return nil, fmt.Errorf("agent: build plan_episodes: %w", err)
			}
			tools = append(tools, plan)
		}
	}
	if r.textGen != nil && r.scriptProjects != nil {
		builders := []struct {
			name string
			ctor func() (tool.InvokableTool, error)
		}{
			{"split_narration_segments", func() (tool.InvokableTool, error) {
				return newSplitNarrationSegmentsTool(project, r.textGen, r.scriptProjects, r.videoCaps)
			}},
			{"normalize_drama_script", func() (tool.InvokableTool, error) {
				return newNormalizeDramaScriptTool(project, r.textGen, r.scriptProjects, r.videoCaps)
			}},
			{"generate_episode_script", func() (tool.InvokableTool, error) {
				return newGenerateEpisodeScriptTool(project, r.textGen, r.scriptProjects, r.videoCaps)
			}},
		}
		for _, b := range builders {
			built, err := b.ctor()
			if err != nil {
				return nil, fmt.Errorf("agent: build %s: %w", b.name, err)
			}
			tools = append(tools, built)
		}
	}
	if r.reviewProjects != nil {
		confirm, err := newConfirmScriptReviewTool(project, r.reviewProjects)
		if err != nil {
			return nil, fmt.Errorf("agent: build confirm_script_review: %w", err)
		}
		tools = append(tools, confirm)
	}
	// 参考生视频工具链（拆分 / 取回编辑 / 校验晋升）；依赖与剧本生成类工具同源。
	if r.textGen != nil && r.scriptProjects != nil {
		refTools, err := ReferenceTools(project, r.textGen, r.scriptProjects, r.videoCaps)
		if err != nil {
			return nil, fmt.Errorf("agent: build reference tools: %w", err)
		}
		for _, built := range refTools {
			tools = append(tools, built)
		}
	}
	if r.memoryStore != nil {
		memory, err := newProposeMemoryCandidateTool(project, sessionID, r.memoryStore)
		if err != nil {
			return nil, fmt.Errorf("agent: build propose_memory_candidate: %w", err)
		}
		tools = append(tools, memory)
	}
	if deps, ok := r.VideoGenerationDeps(); ok {
		videoBuilders := []struct {
			name string
			ctor func() (tool.InvokableTool, error)
		}{
			{"generate_video_episode", func() (tool.InvokableTool, error) { return newGenerateVideoEpisodeTool(project, deps) }},
			{"generate_video_scene", func() (tool.InvokableTool, error) { return newGenerateVideoSceneTool(project, deps) }},
			{"generate_video_all", func() (tool.InvokableTool, error) { return newGenerateVideoAllTool(project, deps) }},
			{"generate_video_selected", func() (tool.InvokableTool, error) { return newGenerateVideoSelectedTool(project, deps) }},
		}
		for _, b := range videoBuilders {
			built, err := b.ctor()
			if err != nil {
				return nil, fmt.Errorf("agent: build %s: %w", b.name, err)
			}
			tools = append(tools, built)
		}
	}
	if built, err := r.timelineTools(project); err != nil {
		return nil, err
	} else {
		tools = append(tools, built...)
	}
	if r.assetWriter != nil && r.projectWriter != nil {
		patch, err := newPatchProjectTool(project, r.assetWriter, r.projectWriter)
		if err != nil {
			return nil, fmt.Errorf("agent: build patch_project: %w", err)
		}
		tools = append(tools, patch)
	}
	if r.assetQueue != nil && r.assetProjects != nil {
		gen, err := newGenerateAssetsTool(project, r.assetQueue, r.assetProjects)
		if err != nil {
			return nil, fmt.Errorf("agent: build generate_assets: %w", err)
		}
		tools = append(tools, gen)
	}
	return tools, nil
}

// buildAgent 装配 ChatModelAgent（设计文档 §11）。
func (r *Runtime) buildAgent(ctx context.Context, sessionID, project string) (adk.Agent, error) {
	m, err := r.chatModel(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent: chat model: %w", err)
	}
	tools, err := r.buildTools(ctx, sessionID, project)
	if err != nil {
		return nil, err
	}

	guard := policy.NewAgentAccessPolicy(r.projectRoot(project), r.secretKeys)
	guardMW := NewGuardMiddleware(guard)

	// 文件系统中间件（§11.4）：根为项目目录；主代理启用全部工具。
	allTools := make(map[string]bool, len(workspace.AllTools))
	for _, n := range workspace.AllTools {
		allTools[n] = true
	}
	fsMW, err := workspace.Middleware(ctx, r.projectRoot(project), r.secretKeys, allTools)
	if err != nil {
		return nil, fmt.Errorf("agent: filesystem middleware: %w", err)
	}

	// Skills / Subagent 源目录与项目 .claude 同构（§17）。
	claudeDir := filepath.Join(r.projectRoot(project), ".claude")
	skillsRoot := filepath.Join(claudeDir, "skills")
	if err := skills.ValidateDir(skillsRoot); err != nil {
		return nil, fmt.Errorf("agent: skills: %w", err)
	}
	skillBackend := skills.NewModeAwareBackend(skillsRoot, readContentMode(filepath.Join(r.projectRoot(project), "project.json")))
	skillMW, err := skills.NewMiddleware(ctx, skillBackend)
	if err != nil {
		return nil, fmt.Errorf("agent: skill middleware: %w", err)
	}

	defs, err := subagents.LoadDir(filepath.Join(claudeDir, "agents"))
	if err != nil {
		return nil, fmt.Errorf("agent: subagents: %w", err)
	}
	pool, err := subagents.ToolPool(ctx, tools)
	if err != nil {
		return nil, fmt.Errorf("agent: subagent tool pool: %w", err)
	}
	subBuilder := &subagents.Builder{
		Model:    m,
		Pool:     pool,
		Handlers: []adk.ChatModelAgentMiddleware{guardMW},
		Filesystem: func(enabled map[string]bool) (adk.ChatModelAgentMiddleware, error) {
			return workspace.Middleware(ctx, r.projectRoot(project), r.secretKeys, enabled)
		},
		SkillBody: func(name string) (string, error) {
			s, err := skillBackend.Get(ctx, name)
			if err != nil {
				return "", err
			}
			return s.Content, nil
		},
	}
	subTools, _, err := subBuilder.BuildAll(ctx, defs)
	if err != nil {
		return nil, fmt.Errorf("agent: build subagents: %w", err)
	}
	tools = append(tools, subTools...)

	return adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        "shotwise-director",
		Description: "Shotwise AI 导演助手：辅助短剧/说书/广告的脚本、分镜与资产生成。",
		Instruction: SystemPrompt(project),
		Model:       m,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools},
			// Subagent（AgentTool）事件透传到父流，SSE 才能看到子代理动态。
			EmitInternalEvents: true,
		},
		// 默认 20 对 Skill 长流程（generate-video 批量生成）偏紧，显式设 40。
		MaxIterations: 40,
		Handlers: []adk.ChatModelAgentMiddleware{
			// 围栏是硬失败设计（ADR 0025），文件工具在无围栏状态下不得上线：
			// guard 必须先于 filesystem/skill middleware 接入（设计文档 §16）。
			guardMW,
			fsMW,
			skillMW,
		},
	})
}

// subagentNames 返回项目 subagent 名字集合（事件归属判定用）。定义加载失败时记录告警并返回空集，
// 严格校验仍由 buildAgent 在每轮装配时执行。
func (r *Runtime) subagentNames(project string) map[string]bool {
	defs, err := subagents.LoadDir(filepath.Join(r.projectRoot(project), ".claude", "agents"))
	if err != nil {
		slog.Warn("subagent 定义加载失败", "project", project, "err", err)
		return map[string]bool{}
	}
	names := make(map[string]bool, len(defs))
	for _, d := range defs {
		names[d.Name] = true
	}
	return names
}

// readContentMode 读取 project.json 的 content_mode；缺失或非法时返回空（只暴露通用 skill）。
func readContentMode(projectJSON string) string {
	raw, err := os.ReadFile(projectJSON)
	if err != nil {
		return ""
	}
	var p struct {
		ContentMode string `json:"content_mode"`
	}
	if json.Unmarshal(raw, &p) != nil || !skills.IsValidMode(p.ContentMode) {
		return ""
	}
	return p.ContentMode
}

// projectRoot 返回项目的绝对根目录：文件围栏、文件系统工具与项目资源（project.json、.claude）共用此根。
func (r *Runtime) projectRoot(project string) string {
	return filepath.Join(r.dataDir, project)
}

// newAskUserTool 构造 ask_user HITL 工具（设计文档 §13.1）。
func newAskUserTool() (tool.InvokableTool, error) {
	return toolutils.InferOptionableTool("ask_user", "向用户提出澄清问题并等待回答；提出问题后本轮会暂停，用户回答后自动继续。",
		func(ctx context.Context, input AskUserInput, _ ...tool.Option) (string, error) {
			if isResume, hasData, answer := compose.GetResumeContext[AnswerPayload](ctx); isResume && hasData {
				// 恢复：把用户回答交回模型。
				return okEnvelope("用户回答：" + answer.Text()), nil
			}
			// 挂起：问题结构（questions/options/multiSelect）原样透出到事件流。
			return "", compose.Interrupt(ctx, input)
		})
}

// SystemPrompt 是身份 + 项目文档工作流 + 语言规范的基线系统提示
// （设计文档 §4.6 顺序的 v1 精简版；完整组装随 profile 增量接入）。
func SystemPrompt(project string) string {
	return fmt.Sprintf(`你是 Shotwise 的 AI 导演助手，负责辅助短视频/短剧的脚本、分镜与资产生成。

# 行为准则
1. 始终先理解用户的项目上下文，再给出可执行的建议或调用工具。
2. 需要读取项目真相源时，优先调用 read_project_json / list_projects，不要凭空杜撰项目结构。
3. 调用任何工具前，先在内心确认参数合理；工具失败会以其回包形式反馈，要据此修正重试。
4. 用与用户相同的语言回答（中文提问用中文，英文用英文）。
5. 需要用户决策且无法自行判断时，调用 ask_user 提出结构化问题。

# 当前项目
%s

# 安全约束
- 你只能在项目目录内工作；写操作经由受围栏约束的工具，越界与保护路径会被拒绝。
- 不要泄露任何 API Key 或密钥。`, project)
}

func joinTexts(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n"
		}
		out += s
	}
	return out
}

func filterItems(items []InboxItem, keep func(InboxItem) bool) []InboxItem {
	out := make([]InboxItem, 0, len(items))
	for _, it := range items {
		if keep(it) {
			out = append(out, it)
		}
	}
	return out
}

// mustJSON 序列化兜底：失败时返回 null 字面量，不中断事件出口。
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
