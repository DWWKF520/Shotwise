// Package checkpoint 提供 Eino compose.CheckPointStore 的持久化实现，
// 供 adk.Runner / adk.TurnLoop 保存中断态（HITL ask_user、Stop 中断恢复）。
//
// 表结构（设计文档 03-agent-runtime-eino.md §13.3）：
//
//	agent_checkpoints(session_id TEXT PK, payload BLOB, updated_at)
//
// checkpoint 的 payload 由 Eino 内部 gob 序列化；Shotwise 侧进入 checkpoint 的
// 自定义类型（InboxItem / AnswerPayload / AskUserInput 等）由主包在 init() 里
// schema.RegisterName 注册，命名规范 "shotwise.agent.<Type>"——key 一旦发布不可改。
package checkpoint

import (
	"context"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// checkpointRow 是 agent_checkpoints 表的 ORM 模型。
type checkpointRow struct {
	SessionID string    `gorm:"column:session_id;primaryKey"`
	Payload   []byte    `gorm:"column:payload"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName 指定表名。
func (checkpointRow) TableName() string { return "agent_checkpoints" }

// SQLStore 基于 gorm 的 CheckPointStore 实现：开发 SQLite / 生产 PostgreSQL 同一套代码。
type SQLStore struct {
	db *gorm.DB
}

// compile-time 断言：SQLStore 满足 CheckPointStore（Get 三返回值语义）与可选的 Delete。
var (
	_ interface {
		Set(ctx context.Context, checkPointID string, checkPoint []byte) error
		Get(ctx context.Context, checkPointID string) ([]byte, bool, error)
	} = (*SQLStore)(nil)
	_ interface {
		Delete(ctx context.Context, checkPointID string) error
	} = (*SQLStore)(nil)
)

// NewSQLStore 构造 SQL 存储并确保表存在（v1 内联建表；DDL 收编进迁移体系时移除 AutoMigrate）。
func NewSQLStore(db *gorm.DB) (*SQLStore, error) {
	if err := db.AutoMigrate(&checkpointRow{}); err != nil {
		return nil, err
	}
	return &SQLStore{db: db}, nil
}

// Set 保存 / 覆盖一个 checkpoint。
func (s *SQLStore) Set(_ context.Context, checkPointID string, checkPoint []byte) error {
	row := checkpointRow{SessionID: checkPointID, Payload: checkPoint, UpdatedAt: time.Now()}
	err := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "session_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"payload", "updated_at"}),
	}).Create(&row).Error
	return err
}

// Get 读取一个 checkpoint；不存在时 existed=false。
func (s *SQLStore) Get(_ context.Context, checkPointID string) ([]byte, bool, error) {
	var row checkpointRow
	err := s.db.Where("session_id = ?", checkPointID).First(&row).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	return row.Payload, true, nil
}

// Delete 删除一个 checkpoint（TurnLoop 干净退出时清理，避免陈旧恢复）。
func (s *SQLStore) Delete(_ context.Context, checkPointID string) error {
	return s.db.Where("session_id = ?", checkPointID).Delete(&checkpointRow{}).Error
}

// MemStore 是进程内存实现，供测试与无持久化场景使用。
type MemStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

// NewMemStore 构造内存存储。
func NewMemStore() *MemStore {
	return &MemStore{data: map[string][]byte{}}
}

// Set 保存 / 覆盖一个 checkpoint。
func (m *MemStore) Set(_ context.Context, checkPointID string, checkPoint []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[checkPointID] = append([]byte(nil), checkPoint...)
	return nil
}

// Get 读取一个 checkpoint；不存在时 existed=false。
func (m *MemStore) Get(_ context.Context, checkPointID string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[checkPointID]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), v...), true, nil
}

// Delete 删除一个 checkpoint。
func (m *MemStore) Delete(_ context.Context, checkPointID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, checkPointID)
	return nil
}
