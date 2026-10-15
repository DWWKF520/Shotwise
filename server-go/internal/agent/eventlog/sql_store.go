package eventlog

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/infra/db"
)

// eventLogRow 是 agent_session_event_log 表的 ORM 模型。
// 表由 migrations/0001_init.sql 建立（开发 SQLite / 生产 PostgreSQL 同构），
// 这里只做读写，不参与 AutoMigrate；payload 存 entry 的完整 JSON。
type eventLogRow struct {
	SessionID string          `gorm:"column:session_id;primaryKey"`
	Seq       int64           `gorm:"column:seq;primaryKey"`
	EntryType string          `gorm:"column:entry_type"`
	Payload   string          `gorm:"column:payload"`
	ClientKey *string         `gorm:"column:client_key"`
	CreatedAt db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt db.FlexibleTime `gorm:"column:updated_at"`
	UserID    string          `gorm:"column:user_id"`
}

// TableName 指定表名。
func (eventLogRow) TableName() string { return "agent_session_event_log" }

// DefaultUserID 是事件日志的默认归属用户（与 agent_session_* 表的 DDL 默认值一致）。
const DefaultUserID = "default"

// SQLStore 是事件日志的 SQL 实现：事件日志即权威 transcript，
// 落库后进程重启不丢会话历史，SSE 断线重连可按 seq 游标续传。
type SQLStore struct {
	db *gorm.DB
	// mu 保护同会话 seq 的「读 max → 分配 → 写入」序列：
	// 单会话的事件由一个适配器 goroutine 顺序产出，但 API 允许任意调用方并发 Append。
	mu sync.Mutex
}

// compile-time 断言：SQLStore 满足 Store。
var _ Store = (*SQLStore)(nil)

// NewSQLStore 构造 SQL 存储（表已由迁移建立，无需 AutoMigrate）。
func NewSQLStore(gdb *gorm.DB) *SQLStore { return &SQLStore{db: gdb} }

// Append 追加一批 entry，为每条分配 per-session 单调递增的 seq 后落库。
// 分配与写入在同一把锁内完成，避免并发调用拿到重复 seq 撞主键。
func (s *SQLStore) Append(ctx context.Context, entries []*Entry) ([]int64, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	seqs := make([]int64, 0, len(entries))
	now := db.NewFlexibleTime(time.Now())
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		seqs = seqs[:0]
		rows := make([]eventLogRow, 0, len(entries))
		// 每会话的当前 seq 只在批首读一次，批内顺延：本批插入尚未发生，
		// 逐条重读 max 会给同一批每条都分到同一个 seq。
		cursors := map[string]int64{}
		for _, e := range entries {
			if e == nil {
				continue
			}
			next, known := cursors[e.SessionID]
			if !known {
				var maxSeq int64
				if err := tx.Model(&eventLogRow{}).
					Where("session_id = ?", e.SessionID).
					Select("COALESCE(MAX(seq), 0)").Scan(&maxSeq).Error; err != nil {
					return err
				}
				next = maxSeq
			}
			next++
			cursors[e.SessionID] = next
			e.Seq = next
			if e.Timestamp.IsZero() {
				e.Timestamp = now.Time
			}
			payload, err := json.Marshal(e)
			if err != nil {
				return err
			}
			seqs = append(seqs, next)
			rows = append(rows, eventLogRow{
				SessionID: e.SessionID,
				Seq:       next,
				EntryType: string(e.Type),
				Payload:   string(payload),
				CreatedAt: now,
				UpdatedAt: now,
				UserID:    DefaultUserID,
			})
		}
		if len(rows) == 0 {
			return nil
		}
		return tx.Create(&rows).Error
	})
	if err != nil {
		return nil, err
	}
	return seqs, nil
}

// List 返回 session 内 seq > afterSeq 的 entries，按 seq 升序（冷读回放 / SSE 续传）。
func (s *SQLStore) List(ctx context.Context, sessionID string, afterSeq int64) ([]*Entry, error) {
	var rows []eventLogRow
	err := s.db.WithContext(ctx).
		Where("session_id = ? AND seq > ?", sessionID, afterSeq).
		Order("seq ASC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]*Entry, 0, len(rows))
	for i := range rows {
		e := &Entry{}
		if err := json.Unmarshal([]byte(rows[i].Payload), e); err != nil {
			// 单条 payload 损坏不应让整段历史读不出来。
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// LastSeq 返回会话当前的 seq 游标；无记录时返回 0。
func (s *SQLStore) LastSeq(ctx context.Context, sessionID string) (int64, error) {
	var maxSeq int64
	err := s.db.WithContext(ctx).Model(&eventLogRow{}).
		Where("session_id = ?", sessionID).
		Select("COALESCE(MAX(seq), 0)").Scan(&maxSeq).Error
	return maxSeq, err
}
