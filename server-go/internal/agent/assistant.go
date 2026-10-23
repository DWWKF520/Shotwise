package agent

// AssistantService 是持久会话契约的编排层（对应 Python AssistantService 的 HTTP 面）：
// meta store（agent_sessions）+ 事件日志（agent_session_event_log）+ client_key 幂等 +
// rewrite 前缀分叉。live 执行复用 *Runtime（TurnLoop）；会话身份由本层铸造（uuid4），
// 受理条目由本层先写日志（分配 seq 与身份），Runtime 经 QueryPrelogged 入队不重复落库。

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/agent/eventlog"
	"github.com/shotwise/shotwise/internal/agent/skills"
	"github.com/shotwise/shotwise/internal/service"
)

// 持久会话层的业务错误；transport 据此映射状态码（与 Python 异常 → HTTP 同口径）。
var (
	ErrSessionNotFound   = errors.New("assistant: session not found")
	ErrSessionBusy       = errors.New("assistant: session is busy")
	ErrSessionSuperseded = errors.New("assistant: session already superseded")
	ErrRewriteAnchor     = errors.New("assistant: rewrite anchor is not a user message")
	ErrPendingQuestion   = errors.New("assistant: session has pending questions")
	ErrInterruptSettle   = errors.New("assistant: session did not settle after interrupt")
	ErrMessageEmpty      = errors.New("assistant: message content is empty")
	ErrImagesUnsupported = errors.New("assistant: image attachments are not supported yet")
	ErrSDKTypeUnknown    = errors.New("assistant: unknown sdk_type")
)

// SendResult 是 send / rewrite 的受理响应（与 Python 响应同形）。
type SendResult struct {
	Status          string          `json:"status"`
	SessionID       string          `json:"session_id"`
	OriginSessionID string          `json:"origin_session_id,omitempty"`
	Entry           *eventlog.Entry `json:"entry"`
}

// interruptSettleTimeout / poll 与 Python _INTERRUPT_SETTLE_* 同值。
const (
	interruptSettleTimeout = 30 * time.Second
	interruptSettlePoll    = 50 * time.Millisecond
	// newSessionKeyLimit 是新会话 client_key 进程内映射的容量上限（LRU 淘汰）。
	newSessionKeyLimit = 256
)

// AssistantService 编排持久会话的受理、分叉与读取。
type AssistantService struct {
	meta    *service.SessionMetaStore
	log     *eventlog.SQLStore
	rt      *Runtime
	store   *service.ProjectStore
	dataDir string

	mu              sync.Mutex
	newSessionKeys  map[string]string // client_key → session_id（快路径）
	newSessionOrder []string          // 插入序（LRU 淘汰用）
	newLocks        keyedLocks
	rewriteLocks    keyedLocks
}

// NewAssistantService 构造编排层。ev 必须与 Runtime 共用同一 *eventlog.SQLStore
// 实例：seq 分配的互斥在实例内，双实例并发 Append 会撞主键。
func NewAssistantService(gdb *gorm.DB, ev *eventlog.SQLStore, rt *Runtime, store *service.ProjectStore, dataDir string) *AssistantService {
	return &AssistantService{
		meta:           service.NewSessionMetaStore(gdb),
		log:            ev,
		rt:             rt,
		store:          store,
		dataDir:        dataDir,
		newSessionKeys: map[string]string{},
	}
}

// keyedLocks 按键串行化：同 key 的并发请求排队，无人持有时锁自动回收。
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedLocks) lock(key string) func() {
	if k.locks == nil {
		k.mu.Lock()
		if k.locks == nil {
			k.locks = map[string]*lockEntry{}
		}
		k.mu.Unlock()
	}
	k.mu.Lock()
	e, ok := k.locks[key]
	if !ok {
		e = &lockEntry{}
		k.locks[key] = e
	}
	e.refs++
	k.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

func strPtr(s string) *string { return &s }

// buildUserEntry 构造用户消息的受理条目（写入点定型，POST 先写日志分配身份再回显）。
func buildUserEntry(text string) *eventlog.Entry {
	return &eventlog.Entry{
		UUID:    "user-" + uuid.NewString(),
		Type:    eventlog.EntryUser,
		Content: []eventlog.Block{{Type: "text", Text: text}},
	}
}

// SendCreate 对应 Python send_or_create：已有会话发消息或新建会话发首条消息。
// clientKey 为请求侧幂等键：同键重试返回既有权威条目，不产生重复会话 / 条目。
// sdkType 仅新会话生效；已有会话沿用 meta 记录的类型（切换走 SwitchAgent）。
func (a *AssistantService) SendCreate(ctx context.Context, project, content, sessionID, clientKey, sdkType, userID string) (*SendResult, error) {
	if !a.store.Exists(project) {
		return nil, ErrSessionNotFound
	}
	text := strings.TrimSpace(content)
	if text == "" {
		return nil, ErrMessageEmpty
	}
	if sessionID != "" {
		return a.sendExisting(ctx, project, sessionID, text, clientKey, userID)
	}
	if clientKey == "" {
		return a.createNewSession(ctx, project, text, clientKey, sdkType, userID)
	}
	if existing, err := a.findAcceptedNewSession(ctx, clientKey, project, userID); err != nil || existing != nil {
		return existing, err
	}
	// 同 client_key 的并发新建在此串行化：在途窗口内后来者等锁而非各自建会话。
	unlock := a.newLocks.lock("new:" + clientKey)
	defer unlock()
	if existing, err := a.findAcceptedNewSession(ctx, clientKey, project, userID); err != nil || existing != nil {
		return existing, err
	}
	result, err := a.createNewSession(ctx, project, text, clientKey, sdkType, userID)
	if err != nil {
		return nil, err
	}
	a.recordNewSessionKey(clientKey, result.SessionID)
	return result, nil
}

// sendExisting 向已有会话投递消息：先幂等写受理条目，再入队执行。
func (a *AssistantService) sendExisting(ctx context.Context, project, sessionID, text, clientKey, userID string) (*SendResult, error) {
	meta, err := a.meta.Get(ctx, sessionID, strPtr(userID))
	if err != nil {
		return nil, err
	}
	if meta == nil || meta.ProjectName != project {
		return nil, ErrSessionNotFound
	}
	if a.rt.IsActive(sessionID) {
		return nil, ErrSessionBusy
	}
	entry, _, err := a.log.AppendUserEntry(ctx, sessionID, buildUserEntry(text), clientKey)
	if err != nil {
		return nil, err
	}
	if err := a.dispatch(ctx, sessionID, project, text, userID); err != nil {
		// 投递失败补偿删除刚写入的条目：同幂等键的重试不能被短路而永不投递。
		_ = a.log.DeleteEntry(ctx, sessionID, entry.Seq)
		return nil, err
	}
	return &SendResult{Status: "accepted", SessionID: sessionID, Entry: entry}, nil
}

// createNewSession 实际创建会话并投递首条消息（seq 1 即受理条目）。
func (a *AssistantService) createNewSession(ctx context.Context, project, text, clientKey, sdkType, userID string) (*SendResult, error) {
	if sdkType == "" {
		sdkType = "claude"
	}
	if sdkType != "claude" && sdkType != "openai" {
		return nil, ErrSDKTypeUnknown
	}
	newID := uuid.NewString()
	meta, err := a.meta.Create(ctx, project, newID, sdkType, userID, nil, nil)
	if err != nil {
		return nil, err
	}
	_ = meta
	entry, _, err := a.log.AppendUserEntry(ctx, newID, buildUserEntry(text), clientKey)
	if err != nil {
		_, _ = a.meta.Delete(ctx, newID, strPtr(userID))
		return nil, err
	}
	if err := a.dispatch(ctx, newID, project, text, userID); err != nil {
		_ = a.log.DeleteEntry(ctx, newID, entry.Seq)
		_, _ = a.meta.Delete(ctx, newID, strPtr(userID))
		return nil, err
	}
	return &SendResult{Status: "accepted", SessionID: newID, Entry: entry}, nil
}

// dispatch 把消息排入会话 loop 并挂状态回写：meta 置 running，轮次终结时按
// 末次 status 事件映射回写终态（error / cancelled→interrupted / 其余 completed）。
func (a *AssistantService) dispatch(ctx context.Context, sessionID, project, text, userID string) error {
	if _, err := a.meta.UpdateStatus(ctx, sessionID, service.SessionStatusRunning, strPtr(userID)); err != nil {
		return err
	}
	_, ch, _, err := a.rt.QueryPrelogged(ctx, sessionID, project, text)
	if err != nil {
		_, _ = a.meta.UpdateStatus(ctx, sessionID, service.SessionStatusError, strPtr(userID))
		return err
	}
	a.watchTurn(sessionID, userID, ch)
	return nil
}

// watchTurn 消费轮次事件流直到关闭（loop 退出即 bus 关闭），把终态写回 meta。
// 无订阅者时事件本会被 bus 丢弃，此 goroutine 保证持久状态与运行态一致。
func (a *AssistantService) watchTurn(sessionID, userID string, ch <-chan Event) {
	go func() {
		last := ""
		for ev := range ch {
			if ev.Type == EventStatus && ev.Text != "" && ev.Text != "running" {
				last = ev.Text
			}
		}
		final := service.SessionStatusCompleted
		switch last {
		case "error":
			final = service.SessionStatusError
		case "cancelled":
			final = service.SessionStatusInterrupted
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = a.meta.UpdateStatus(ctx, sessionID, final, strPtr(userID))
	}()
}

// findAcceptedNewSession 按幂等键定位已受理的新会话：进程内映射为快路径，
// 事件日志跨会话查询（seq 为该会话首条的 client_key 行）兜底重启 / 淘汰场景。
// 命中会话的项目归属须与调用方一致，不一致视为未命中（当前项目新建）。
func (a *AssistantService) findAcceptedNewSession(ctx context.Context, clientKey, project, userID string) (*SendResult, error) {
	var sessionIDs []string
	a.mu.Lock()
	if id, ok := a.newSessionKeys[clientKey]; ok {
		sessionIDs = append(sessionIDs, id)
	}
	a.mu.Unlock()
	recovered, _, err := a.log.FindNewSessionByClientKey(ctx, clientKey)
	if err == nil && recovered != "" {
		sessionIDs = append(sessionIDs, recovered)
	}
	for _, id := range sessionIDs {
		entry, err := a.log.FindByClientKey(ctx, id, clientKey)
		if err != nil {
			continue
		}
		if entry == nil {
			// 映射指向的条目已不存在（会话被删）：映射失效，清掉。
			a.mu.Lock()
			if a.newSessionKeys[clientKey] == id {
				delete(a.newSessionKeys, clientKey)
			}
			a.mu.Unlock()
			continue
		}
		meta, err := a.meta.Get(ctx, id, strPtr(userID))
		if err != nil {
			continue
		}
		if meta != nil && meta.ProjectName != project {
			continue // 属于其他项目：视为未命中，不清映射（对原项目仍有效）
		}
		a.recordNewSessionKey(clientKey, id)
		return &SendResult{Status: "accepted", SessionID: id, Entry: entry}, nil
	}
	return nil, nil
}

// recordNewSessionKey 记录 / 刷新 client_key 映射并按容量淘汰最旧条目。
func (a *AssistantService) recordNewSessionKey(clientKey, sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.newSessionKeys[clientKey]; !exists {
		a.newSessionOrder = append(a.newSessionOrder, clientKey)
	}
	a.newSessionKeys[clientKey] = sessionID
	for len(a.newSessionOrder) > newSessionKeyLimit {
		oldest := a.newSessionOrder[0]
		a.newSessionOrder = a.newSessionOrder[1:]
		delete(a.newSessionKeys, oldest)
	}
}

// SkillsDir 返回项目用户可触发 skill 的目录（profile 已物化到项目 .claude/skills）。
func (a *AssistantService) skillsRoot(project string) string {
	return filepath.Join(a.dataDir, project, ".claude", "skills")
}

// Rewrite 对应 Python rewrite_message：改写原会话锚点处的用户消息，
// 分叉出新会话承接改写并在其上重跑。运行中的轮次由本方法自动中断收口。
// 响应 session_id 是新会话，前端据此整体切换；origin_session_id 为原会话。
func (a *AssistantService) Rewrite(ctx context.Context, project, sessionID, anchorEntryUUID, content, clientKey, userID string) (*SendResult, error) {
	if !a.store.Exists(project) {
		return nil, ErrSessionNotFound
	}
	text := strings.TrimSpace(content)
	if text == "" {
		return nil, ErrMessageEmpty
	}
	unlock := a.rewriteLocks.lock(sessionID + ":" + clientKey)
	defer unlock()
	return a.rewriteOnce(ctx, project, sessionID, anchorEntryUUID, text, clientKey, userID)
}

// rewriteOnce 是一次改写尝试；同一（原会话, client_key）的并发请求被 Rewrite 串行化。
func (a *AssistantService) rewriteOnce(ctx context.Context, project, sessionID, anchorEntryUUID, text, clientKey, userID string) (*SendResult, error) {
	meta, err := a.meta.Get(ctx, sessionID, strPtr(userID))
	if err != nil {
		return nil, err
	}
	if meta == nil || meta.ProjectName != project {
		return nil, ErrSessionNotFound
	}
	// 原会话已被取代：同 client_key 重试在新分支认领自己的权威条目，其余拒绝。
	if meta.SupersededBy != nil {
		if clientKey != "" {
			if entry, err := a.log.FindByClientKey(ctx, *meta.SupersededBy, clientKey); err == nil && entry != nil {
				if forkMeta, gerr := a.meta.Get(ctx, *meta.SupersededBy, strPtr(userID)); gerr == nil && forkMeta != nil {
					return &SendResult{Status: "accepted", SessionID: *meta.SupersededBy,
						OriginSessionID: defork(forkMeta.ForkParentSessionID, sessionID), Entry: entry}, nil
				}
			}
		}
		return nil, ErrSessionSuperseded
	}
	// 问答优先：未决问答卡片存在时不改写。
	if _, ok := a.rt.PendingQuestionOf(sessionID); ok {
		return nil, ErrPendingQuestion
	}
	// 中断运行中的轮次并等终态：运行中的会话分叉不出干净的前缀。
	if err := a.settleRunning(ctx, sessionID, userID); err != nil {
		return nil, err
	}
	newID := uuid.NewString()
	// 前缀复制（锚点校验在写入之前，找不到锚点时未发生任何写入）。
	if err := a.copyPrefixTo(ctx, sessionID, anchorEntryUUID, newID); err != nil {
		if errors.Is(err, eventlog.ErrAnchorNotFound) {
			return nil, ErrRewriteAnchor
		}
		return nil, err
	}
	// 先建新会话行再落指针：指针任何时刻都指向已存在的会话。
	if _, err := a.meta.Create(ctx, project, newID, meta.SDKType, userID, strPtr(sessionID), strPtr(anchorEntryUUID)); err != nil {
		return nil, err
	}
	ok, err := a.meta.MarkSuperseded(ctx, sessionID, newID, strPtr(userID))
	if err != nil {
		return nil, err
	}
	if !ok {
		// 预检与分叉之间输给了另一次改写。
		return nil, ErrSessionSuperseded
	}
	// 受理改写后的消息；失败则整段撤回（指针 + 前缀 + 会话行）。
	entry, _, err := a.log.AppendUserEntry(ctx, newID, buildUserEntry(text), clientKey)
	if err == nil {
		err = a.dispatch(ctx, newID, project, text, userID)
	}
	if err != nil {
		_ = a.log.DeleteSession(ctx, newID)
		_, _ = a.meta.ClearSuperseded(ctx, sessionID, newID, strPtr(userID))
		_, _ = a.meta.Delete(ctx, newID, strPtr(userID))
		return nil, err
	}
	return &SendResult{Status: "accepted", SessionID: newID, OriginSessionID: sessionID, Entry: entry}, nil
}

// copyPrefixTo 把原会话锚点前的条目复制到 newID（rewrite 分叉）。找不到锚点
// 时不产生任何写入，直接报 ErrAnchorNotFound；锚点是首条时为空前缀（合法）。
func (a *AssistantService) copyPrefixTo(ctx context.Context, originID, anchorUUID, newID string) error {
	entries, err := a.log.List(ctx, originID, -1)
	if err != nil {
		return err
	}
	// 先定位锚点：不在任何写入之前完成校验。
	anchorIdx := -1
	for i, e := range entries {
		if e.UUID == anchorUUID && e.Type == eventlog.EntryUser && e.Subtype == "" && e.ParentToolUseID == "" {
			anchorIdx = i
			break
		}
	}
	if anchorIdx < 0 {
		return eventlog.ErrAnchorNotFound
	}
	if anchorIdx == 0 {
		return nil // 空前缀：锚点即首条，新会话就是全新会话
	}
	// 再复制前缀：保留原条目身份（uuid / 时间戳），仅换 session_id。
	batch := make([]*eventlog.Entry, 0, anchorIdx)
	for i := 0; i < anchorIdx; i++ {
		clone := *entries[i]
		clone.SessionID = newID
		batch = append(batch, &clone)
	}
	_, err = a.log.Append(ctx, batch)
	return err
}

func defork(v *string, fallback string) string {
	if v == nil {
		return fallback
	}
	return *v
}

// settleRunning 中断运行中的轮次并等它落到终态。
func (a *AssistantService) settleRunning(ctx context.Context, sessionID, userID string) error {
	if !a.rt.IsActive(sessionID) {
		return nil
	}
	if err := a.rt.Interrupt(ctx, sessionID); err != nil {
		return nil // 会话刚好退出：按已收口处理
	}
	deadline := time.Now().Add(interruptSettleTimeout)
	for a.rt.IsActive(sessionID) {
		if time.Now().After(deadline) {
			return ErrInterruptSettle
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interruptSettlePoll):
		}
	}
	return nil
}

// GetSession 返回会话 meta；活跃运行时以运行态覆盖持久状态。
func (a *AssistantService) GetSession(ctx context.Context, sessionID, userID string) (*service.SessionMeta, error) {
	meta, err := a.meta.Get(ctx, sessionID, strPtr(userID))
	if err != nil || meta == nil {
		return meta, err
	}
	if a.rt.IsActive(sessionID) {
		meta.Status = service.SessionStatusRunning
	}
	return meta, nil
}

// ListSessions 列出项目下未被取代的会话。
func (a *AssistantService) ListSessions(ctx context.Context, project string, status *service.SessionStatus, limit, offset int, userID string) ([]*service.SessionMeta, error) {
	return a.meta.List(ctx, &project, status, limit, offset, strPtr(userID))
}

// DeleteSession 删除会话：停运行时 → 删事件日志 → 删 meta（前身指针由 meta 层接手后继）。
func (a *AssistantService) DeleteSession(ctx context.Context, sessionID, userID string) (bool, error) {
	if a.rt.IsActive(sessionID) {
		a.rt.StopSession(sessionID)
	}
	if err := a.log.DeleteSession(ctx, sessionID); err != nil {
		return false, err
	}
	return a.meta.Delete(ctx, sessionID, strPtr(userID))
}

// EntryPage 是 entries 冷读响应（与 Python list_session_entries 同形）。
type EntryPage struct {
	SessionID string            `json:"session_id"`
	Status    string            `json:"status"`
	Entries   []*eventlog.Entry `json:"entries"`
	Draft     any               `json:"draft"`
	DraftRev  int               `json:"draft_rev"`
}

// ListEntries 冷读事件日志（历史回放，afterSeq 为游标；-1 表示从头）。
func (a *AssistantService) ListEntries(ctx context.Context, sessionID string, afterSeq int64, userID string) (*EntryPage, error) {
	meta, err := a.meta.Get(ctx, sessionID, strPtr(userID))
	if err != nil {
		return nil, err
	}
	if meta == nil {
		return nil, ErrSessionNotFound
	}
	entries, err := a.log.List(ctx, sessionID, afterSeq)
	if err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []*eventlog.Entry{}
	}
	status := string(meta.Status)
	if a.rt.IsActive(sessionID) {
		status = "running"
	}
	return &EntryPage{SessionID: sessionID, Status: status, Entries: entries, Draft: nil, DraftRev: 0}, nil
}

// Interrupt 中断会话当前轮（未在运行时按当前状态幂等返回）。
func (a *AssistantService) Interrupt(ctx context.Context, sessionID, userID string) (string, error) {
	meta, err := a.meta.Get(ctx, sessionID, strPtr(userID))
	if err != nil {
		return "", err
	}
	if meta == nil {
		return "", ErrSessionNotFound
	}
	if !a.rt.IsActive(sessionID) {
		return string(meta.Status), nil
	}
	if err := a.rt.Interrupt(ctx, sessionID); err != nil {
		return string(meta.Status), nil
	}
	if _, err := a.meta.UpdateStatus(ctx, sessionID, service.SessionStatusInterrupted, strPtr(userID)); err != nil {
		return "", err
	}
	return "interrupted", nil
}

// SwitchAgent 切换会话活跃 SDK 类型（记录在 meta 上；Go 运行时单通道，行为面即时生效于新轮）。
func (a *AssistantService) SwitchAgent(ctx context.Context, sessionID, sdkType, userID string) (*service.SessionMeta, error) {
	if sdkType != "claude" && sdkType != "openai" {
		return nil, ErrSDKTypeUnknown
	}
	meta, err := a.meta.Get(ctx, sessionID, strPtr(userID))
	if err != nil {
		return nil, err
	}
	if meta == nil {
		return nil, ErrSessionNotFound
	}
	if _, err := a.meta.UpdateSDKType(ctx, sessionID, sdkType, nil, strPtr(userID)); err != nil {
		return nil, err
	}
	meta.SDKType = sdkType
	return meta, nil
}

// AnswerQuestion 回答待决问题（HITL 续跑入口；复用 Runtime 的恢复机制）。
func (a *AssistantService) AnswerQuestion(ctx context.Context, sessionID, questionID string, answers map[string]string, userID string) error {
	if len(answers) == 0 {
		return ErrMessageEmpty
	}
	meta, err := a.meta.Get(ctx, sessionID, strPtr(userID))
	if err != nil {
		return err
	}
	if meta == nil {
		return ErrSessionNotFound
	}
	if ch, _, err := a.rt.AnswerQuestion(ctx, sessionID, questionID, answers); err != nil {
		return err
	} else {
		// 恢复轮同样挂终态回写：受理侧已置 running，这里保证流关闭后落终态。
		a.watchTurn(sessionID, userID, ch)
	}
	// 恢复轮同样挂状态回写（受理侧已置 running）。
	if _, err := a.meta.UpdateStatus(ctx, sessionID, service.SessionStatusRunning, strPtr(userID)); err != nil {
		return err
	}
	return nil
}

// ListSkills 列出项目可用的用户可触发 skill（对应 Python list_available_skills）。
func (a *AssistantService) ListSkills(project string) ([]skills.CatalogEntry, error) {
	if !a.store.Exists(project) {
		return nil, ErrSessionNotFound
	}
	entries, err := skills.ListUserInvocable(a.skillsRoot(project))
	if err != nil {
		return []skills.CatalogEntry{}, nil
	}
	return entries, nil
}

// AnswerOf 返回 runtime 待决问题快照（stream 冷订阅补发用）。
func (a *AssistantService) AnswerOf(sessionID string) (PendingQuestion, bool) {
	return a.rt.PendingQuestionOf(sessionID)
}

// Stream 订阅会话 entry 流（SSE 续传）：存量条目按 seq 门槛去重后接直播，
// 非运行会话回放后以终态收口。游标语义与 Runtime.Stream 一致。
func (a *AssistantService) Stream(ctx context.Context, sessionID string, afterSeq int64) (<-chan Event, func(), error) {
	return a.rt.Stream(ctx, sessionID, afterSeq)
}

// SubscribeLive 订阅活跃会话的直播事件（agent/chat 同步收集用）。
func (a *AssistantService) SubscribeLive(ctx context.Context, sessionID string) (<-chan Event, func(), error) {
	return a.rt.ReceiveResponse(ctx, sessionID)
}
