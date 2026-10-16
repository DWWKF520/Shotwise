package service

import (
	"encoding/json"
	"sync"
	"time"
)

// Change 是项目变更流里的一条变更记录。
// 字段与 Python 侧 _build_entity_change 的输出保持一致，前端才能逐字段对齐。
type Change struct {
	EntityType string `json:"entity_type"`
	Action     string `json:"action"`
	EntityID   string `json:"entity_id"`
	Label      string `json:"label"`
	Focus      any    `json:"focus"`
	Important  bool   `json:"important"`
	ScriptFile string `json:"script_file,omitempty"`
	Episode    *int   `json:"episode,omitempty"`
	// AssetFingerprints 是生成完成事件专有的附加字段（受影响文件的 mtime 指纹，键为项目相对路径），
	// 供前端 cache-bust。
	AssetFingerprints map[string]int64 `json:"asset_fingerprints,omitempty"`
}

// ChangeBatch 是一次广播的增量。
type ChangeBatch struct {
	ProjectName string   `json:"project_name"`
	BatchID     string   `json:"batch_id"`
	Fingerprint string   `json:"fingerprint"`
	GeneratedAt string   `json:"generated_at"`
	Source      string   `json:"source"` // worker | webui | filesystem
	Changes     []Change `json:"changes"`
}

// Snapshot 是订阅建立时的首帧。
type Snapshot struct {
	ProjectName string `json:"project_name"`
	Fingerprint string `json:"fingerprint"`
	GeneratedAt string `json:"generated_at"`
}

// EventHub 广播项目变更。
//
// 溢出策略与 Python 侧 DropSubscriber 一致：订阅者队列满时静默移除，
// 不阻塞发布方——变更流是尽力而为的刷新信号，不是可靠投递通道。
type EventHub struct {
	mu   sync.RWMutex
	subs map[string]map[*subscriber]struct{}
}

type subscriber struct {
	ch   chan []byte
	proj string
}

// NewEventHub 构造事件中心。
func NewEventHub() *EventHub {
	return &EventHub{subs: make(map[string]map[*subscriber]struct{})}
}

// Subscribe 订阅某个项目的变更，返回接收通道与取消函数。
func (h *EventHub) Subscribe(project string) (<-chan []byte, func()) {
	sub := &subscriber{ch: make(chan []byte, 64), proj: project}
	h.mu.Lock()
	if h.subs[project] == nil {
		h.subs[project] = make(map[*subscriber]struct{})
	}
	h.subs[project][sub] = struct{}{}
	h.mu.Unlock()

	done := make(chan struct{})
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(done)
			h.mu.Lock()
			delete(h.subs[project], sub)
			if len(h.subs[project]) == 0 {
				delete(h.subs, project)
			}
			h.mu.Unlock()
		})
	}
	return sub.ch, cancel
}

// Publish 广播一批变更。
func (h *EventHub) Publish(batch ChangeBatch) {
	payload, err := json.Marshal(batch)
	if err != nil {
		return
	}
	frame := []byte("event: changes\ndata: " + string(payload) + "\n\n")

	h.mu.RLock()
	targets := make([]*subscriber, 0, len(h.subs[batch.ProjectName]))
	for s := range h.subs[batch.ProjectName] {
		targets = append(targets, s)
	}
	h.mu.RUnlock()

	for _, s := range targets {
		select {
		case s.ch <- frame:
		default:
			// 队列满 → 静默移除订阅者（DropSubscriber）。
			h.mu.Lock()
			delete(h.subs[batch.ProjectName], s)
			h.mu.Unlock()
			close(s.ch)
		}
	}
}

// PublishDeleted 广播项目删除，流随后结束。
func (h *EventHub) PublishDeleted(project string) {
	payload, _ := json.Marshal(map[string]string{"project_name": project})
	frame := []byte("event: project_deleted\ndata: " + string(payload) + "\n\n")
	h.mu.RLock()
	targets := make([]*subscriber, 0, len(h.subs[project]))
	for s := range h.subs[project] {
		targets = append(targets, s)
	}
	h.mu.RUnlock()
	for _, s := range targets {
		select {
		case s.ch <- frame:
		default:
		}
		// 项目已删除，流到此结束：发完即关闭，订阅端靠 !ok 收尾。
		h.mu.Lock()
		delete(h.subs[project], s)
		h.mu.Unlock()
		close(s.ch)
	}
}

// SnapshotFrame 构造首帧 snapshot 事件的 SSE 帧。
func SnapshotFrame(s Snapshot) []byte {
	payload, _ := json.Marshal(s)
	return []byte("event: snapshot\ndata: " + string(payload) + "\n\n")
}

// HeartbeatFrame 是心跳帧。Python 侧用 {"type":"_idle"} 内部哨兵且不下发，
// 这里直接发注释行保持连接活跃（注释行对 EventSource 客户端不可见）。
func HeartbeatFrame(at time.Time) []byte {
	return []byte(": heartbeat " + at.UTC().Format(time.RFC3339) + "\n\n")
}
