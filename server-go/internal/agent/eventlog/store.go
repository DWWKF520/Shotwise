package eventlog

import (
	"context"
	"sort"
	"sync"
)

// Store 是事件日志的持久化接口：append-only，seq 由实现方按 session 单调分配。
// v1 提供内存实现；SQL 实现随后续增量接入（表结构与 Python 侧 event_log 对齐）。
type Store interface {
	// Append 追加一批 entry，为每条分配 per-session 单调 seq 后返回。
	Append(ctx context.Context, entries []*Entry) ([]int64, error)
	// List 返回 session 内 seq > afterSeq 的 entries（按 seq 升序），供 SSE 续传与历史重放。
	List(ctx context.Context, sessionID string, afterSeq int64) ([]*Entry, error)
}

// MemStore 是进程内存实现（锁保护；多 session 互不干扰，seq 各自独立单调）。
type MemStore struct {
	mu      sync.Mutex
	entries map[string][]*Entry
	seqs    map[string]int64
}

// NewMemStore 构造内存存储。
func NewMemStore() *MemStore {
	return &MemStore{
		entries: map[string][]*Entry{},
		seqs:    map[string]int64{},
	}
}

// Append 实现 Store：按 entry.SessionID 分组追加并分配 seq。
func (m *MemStore) Append(_ context.Context, entries []*Entry) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seqs := make([]int64, 0, len(entries))
	for _, e := range entries {
		m.seqs[e.SessionID]++
		e.Seq = m.seqs[e.SessionID]
		m.entries[e.SessionID] = append(m.entries[e.SessionID], e)
		seqs = append(seqs, e.Seq)
	}
	return seqs, nil
}

// List 实现 Store：返回 seq > afterSeq 的升序 entries。
func (m *MemStore) List(_ context.Context, sessionID string, afterSeq int64) ([]*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := m.entries[sessionID]
	out := make([]*Entry, 0, len(all))
	for _, e := range all {
		if e.Seq > afterSeq {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}
