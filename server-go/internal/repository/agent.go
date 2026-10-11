package repository

// 本文件的结构体由既有库的实测 DDL 生成，列名与可空性与 migrations/0001_init.sql 一致。
// schema 的真相源是那份 SQL，这里不打索引/约束 tag，也不参与 AutoMigrate。
// 时间列一律用 db.FlexibleTime：既有库里同一语义的列混用 TEXT / DATETIME，
// 指针式可空由 FlexibleTime.Valid 表达，不用 *FlexibleTime。

import (
	"github.com/shotwise/shotwise/internal/infra/db"
)

// AgentSession 对应 agent_sessions 表。
type AgentSession struct {
	ID                  string          `gorm:"column:id;primaryKey"`
	SDKSessionID        string          `gorm:"column:sdk_session_id"`
	ProjectName         string          `gorm:"column:project_name"`
	Title               string          `gorm:"column:title"`
	Status              string          `gorm:"column:status"`
	CreatedAt           db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt           db.FlexibleTime `gorm:"column:updated_at"`
	UserID              string          `gorm:"column:user_id"`
	SDKType             string          `gorm:"column:sdk_type"`
	ClaudeResumeID      *string         `gorm:"column:claude_resume_id"`
	SupersededBy        *string         `gorm:"column:superseded_by"`
	ForkParentSessionID *string         `gorm:"column:fork_parent_session_id"`
	ForkAnchorUUID      *string         `gorm:"column:fork_anchor_uuid"`
}

func (AgentSession) TableName() string { return "agent_sessions" }

// AgentSessionEventLog 对应 agent_session_event_log 表。
type AgentSessionEventLog struct {
	SessionID string          `gorm:"column:session_id;primaryKey"`
	Seq       int64           `gorm:"column:seq;primaryKey"`
	EntryType string          `gorm:"column:entry_type"`
	Payload   string          `gorm:"column:payload"`
	ClientKey *string         `gorm:"column:client_key"`
	CreatedAt db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt db.FlexibleTime `gorm:"column:updated_at"`
	UserID    string          `gorm:"column:user_id"`
}

func (AgentSessionEventLog) TableName() string { return "agent_session_event_log" }

// AgentSessionEntry 对应 agent_session_entries 表。
type AgentSessionEntry struct {
	ProjectKey string          `gorm:"column:project_key;primaryKey"`
	SessionID  string          `gorm:"column:session_id;primaryKey"`
	Subpath    string          `gorm:"column:subpath;primaryKey"`
	Seq        int64           `gorm:"column:seq;primaryKey"`
	UUID       *string         `gorm:"column:uuid"`
	EntryType  string          `gorm:"column:entry_type"`
	Payload    string          `gorm:"column:payload"`
	MtimeMs    int64           `gorm:"column:mtime_ms"`
	CreatedAt  db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt  db.FlexibleTime `gorm:"column:updated_at"`
	UserID     string          `gorm:"column:user_id"`
}

func (AgentSessionEntry) TableName() string { return "agent_session_entries" }

// AgentSessionSummary 对应 agent_session_summaries 表。
type AgentSessionSummary struct {
	ProjectKey string          `gorm:"column:project_key;primaryKey"`
	SessionID  string          `gorm:"column:session_id;primaryKey"`
	MtimeMs    int64           `gorm:"column:mtime_ms"`
	Data       string          `gorm:"column:data"`
	CreatedAt  db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt  db.FlexibleTime `gorm:"column:updated_at"`
	UserID     string          `gorm:"column:user_id"`
}

func (AgentSessionSummary) TableName() string { return "agent_session_summaries" }

// AgentSessionUserMessageLink 对应 agent_session_user_message_links 表。
type AgentSessionUserMessageLink struct {
	SessionID     string          `gorm:"column:session_id;primaryKey"`
	UserEntryUUID string          `gorm:"column:user_entry_uuid;primaryKey"`
	SDKEntryUUID  string          `gorm:"column:sdk_entry_uuid"`
	CreatedAt     db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt     db.FlexibleTime `gorm:"column:updated_at"`
	UserID        string          `gorm:"column:user_id"`
}

func (AgentSessionUserMessageLink) TableName() string { return "agent_session_user_message_links" }

// MemoryEntry 对应 memory_entries 表。
type MemoryEntry struct {
	ID              string          `gorm:"column:id;primaryKey"`
	UserID          string          `gorm:"column:user_id"`
	Scope           string          `gorm:"column:scope"`
	ProjectName     *string         `gorm:"column:project_name"`
	Category        string          `gorm:"column:category"`
	Content         string          `gorm:"column:content"`
	Source          string          `gorm:"column:source"`
	Confirmed       bool            `gorm:"column:confirmed"`
	SourceSessionID *string         `gorm:"column:source_session_id"`
	SourceMessageID *string         `gorm:"column:source_message_id"`
	MetadataJSON    *string         `gorm:"column:metadata_json"`
	CreatedAt       db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt       db.FlexibleTime `gorm:"column:updated_at"`
}

func (MemoryEntry) TableName() string { return "memory_entries" }

// MemoryCandidate 对应 memory_candidates 表。
type MemoryCandidate struct {
	ID              string          `gorm:"column:id;primaryKey"`
	UserID          string          `gorm:"column:user_id"`
	Scope           string          `gorm:"column:scope"`
	ProjectName     *string         `gorm:"column:project_name"`
	Category        string          `gorm:"column:category"`
	Content         string          `gorm:"column:content"`
	Source          string          `gorm:"column:source"`
	Confirmed       bool            `gorm:"column:confirmed"`
	Status          string          `gorm:"column:status"`
	SourceSessionID *string         `gorm:"column:source_session_id"`
	SourceMessageID *string         `gorm:"column:source_message_id"`
	MetadataJSON    *string         `gorm:"column:metadata_json"`
	CreatedAt       db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt       db.FlexibleTime `gorm:"column:updated_at"`
}

func (MemoryCandidate) TableName() string { return "memory_candidates" }
