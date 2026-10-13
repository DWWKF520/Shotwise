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
	"fmt"
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
}

// AnswerPayload 是用户对 ask_user 的回答（HITL 恢复参数）。
type AnswerPayload struct {
	// Answers 按问题顺序给出用户选择/输入文本。
	Answers []string `json:"answers"`
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

	mu                sync.Mutex
	active            map[string]*sessionLoop
	pendingInterrupts map[string]string // session → 未答 question 的 interruptID
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
		pendingInterrupts: map[string]string{},
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
	if message == "" {
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
	sl.adapter.appendUserEntries(ctx, message)
	if ok, _ := sl.loop.Push(InboxItem{Kind: InboxUserMessage, Text: message}); !ok {
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
	interruptID, ok := r.pendingInterrupts[sessionID]
	delete(r.pendingInterrupts, sessionID) // 消费即清除：重复 Answer 报无待决问题
	r.mu.Unlock()
	if !ok {
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
	sl.interruptID = interruptID
	sl.answer = &answer
	r.mu.Lock()
	r.active[sessionID] = sl
	r.mu.Unlock()
	ch, unsub := sl.bus.Subscribe()
	sl.adapter.appendSystemNotice(ctx, eventlog.SubtypeQuestionAnswer, fmt.Sprintf("%v", answer.Answers))
	if ok, _ := sl.loop.Push(InboxItem{Kind: InboxAnswer, Answer: &answer}); !ok {
		unsub()
		return nil, nil, fmt.Errorf("agent: session %s is shutting down", sessionID)
	}
	return ch, unsub, nil
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
		sessionID: sessionID,
		store:     r.evStore,
		emit:      sl.bus.Broadcast,
		onInterrupted: func(interruptID string) {
			r.mu.Lock()
			r.pendingInterrupts[sessionID] = interruptID
			r.mu.Unlock()
		},
	}
	cfg := adk.TurnLoopConfig[InboxItem, *schema.Message]{
		GenInput:     r.genInput(sl),
		GenResume:    r.genResume(sl),
		PrepareAgent: r.prepareAgent(project),
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
				msgs = append(msgs, schema.UserMessage(it.Text))
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
func (r *Runtime) prepareAgent(project string) func(context.Context, *adk.TurnLoop[InboxItem, *schema.Message], []InboxItem) (adk.Agent, error) {
	return func(ctx context.Context, _ *adk.TurnLoop[InboxItem, *schema.Message], _ []InboxItem) (adk.Agent, error) {
		return r.buildAgent(ctx, project)
	}
}

// buildAgent 装配 ChatModelAgent（设计文档 §11）。
func (r *Runtime) buildAgent(ctx context.Context, project string) (adk.Agent, error) {
	m, err := r.chatModel(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent: chat model: %w", err)
	}
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

	guard := policy.NewAgentAccessPolicy(r.projectRoot(project), r.secretKeys)
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
			NewGuardMiddleware(guard),
		},
	})
}

// projectRoot 返回项目的绝对根目录（围栏双 base 的 raw 侧）。
func (r *Runtime) projectRoot(project string) string {
	return r.dataDir // v1：dataDir 即项目根；多项目隔离随 project 维度增强收敛
}

// newAskUserTool 构造 ask_user HITL 工具（设计文档 §13.1）。
func newAskUserTool() (tool.InvokableTool, error) {
	return toolutils.InferOptionableTool("ask_user", "向用户提出澄清问题并等待回答；提出问题后本轮会暂停，用户回答后自动继续。",
		func(ctx context.Context, input AskUserInput, _ ...tool.Option) (string, error) {
			if isResume, hasData, answer := compose.GetResumeContext[AnswerPayload](ctx); isResume && hasData {
				// 恢复：把用户回答交回模型。
				return okEnvelope("用户回答：" + joinTexts(answer.Answers)), nil
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
