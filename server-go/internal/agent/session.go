package agent

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Session 对应 Python 侧的 ManagedSession：单会话的轻量运行态。
// 权威历史在 event_log（设计文档 03-agent-runtime-eino.md §16），
// 这里只保留身份与活跃度信息，供容量管理 / 巡检 / 驱逐使用。
type Session struct {
	ID         string
	Project    string
	LastActive time.Time
}

// SessionManager 对应 Python 侧的 SessionManager：容量 / 巡检 / 驱逐。
// （sdk_id 懒生成等跨 SDK 续接机制在 Eino 下整体作废，见设计文档 §15.3。）
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	capacity int
}

// NewSessionManager 构造管理器，capacity 为最大并发会话数（0 表示不设上限）。
func NewSessionManager(capacity int) *SessionManager {
	if capacity <= 0 {
		capacity = 64
	}
	return &SessionManager{sessions: map[string]*Session{}, capacity: capacity}
}

// GetOrCreate 取已有会话或新建；超出容量时驱逐最久未活跃的会话。
func (m *SessionManager) GetOrCreate(id, project string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		s.LastActive = time.Now()
		return s
	}
	if len(m.sessions) >= m.capacity {
		m.evictLocked()
	}
	s := &Session{ID: id, Project: project, LastActive: time.Now()}
	m.sessions[id] = s
	return s
}

// Get 按 id 取会话；不存在返回 nil。
func (m *SessionManager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// List 返回当前所有会话的轻量信息（不含历史）。
func (m *SessionManager) List() []SessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, SessionInfo{ID: s.ID, Project: s.Project, LastActive: s.LastActive})
	}
	return out
}

// evictLocked 驱逐最久未活跃的会话（调用方须持锁）。
func (m *SessionManager) evictLocked() {
	var oldestID string
	var oldest time.Time
	for id, s := range m.sessions {
		if oldestID == "" || s.LastActive.Before(oldest) {
			oldestID, oldest = id, s.LastActive
		}
	}
	if oldestID != "" {
		delete(m.sessions, oldestID)
	}
}

// NewSessionID 生成新的会话标识。
func NewSessionID() string { return "sess_" + uuid.NewString() }

// SessionInfo 是会话的列表视图。
type SessionInfo struct {
	ID         string    `json:"id"`
	Project    string    `json:"project"`
	LastActive time.Time `json:"last_active"`
}
