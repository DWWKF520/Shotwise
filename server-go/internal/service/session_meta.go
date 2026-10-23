package service

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
)

// SessionStatus 是会话状态类型。
type SessionStatus string

const (
	SessionStatusIdle        SessionStatus = "idle"
	SessionStatusRunning     SessionStatus = "running"
	SessionStatusCompleted   SessionStatus = "completed"
	SessionStatusError       SessionStatus = "error"
	SessionStatusInterrupted SessionStatus = "interrupted"
)

// SessionMeta 是会话元数据（对应 Python SessionMeta）。
type SessionMeta struct {
	ID                  string        `json:"id"`
	UserID              string        `json:"user_id"`
	ProjectName         string        `json:"project_name"`
	Title               string        `json:"title"`
	Status              SessionStatus `json:"status"`
	SupersededBy        *string       `json:"superseded_by"`
	ForkParentSessionID *string       `json:"fork_parent_session_id"`
	ForkAnchorUUID      *string       `json:"fork_anchor_uuid"`
	SDKType             string        `json:"sdk_type"`
	ClaudeResumeID      *string       `json:"claude_resume_id"`
	CreatedAt           time.Time     `json:"created_at"`
	UpdatedAt           time.Time     `json:"updated_at"`
}

// SessionMetaStore 是会话元数据的 SQL 存取层（对应 Python SessionMetaStore）。
type SessionMetaStore struct {
	db *gorm.DB
}

// NewSessionMetaStore 构造元数据存储。
func NewSessionMetaStore(db *gorm.DB) *SessionMetaStore {
	return &SessionMetaStore{db: db}
}

// rowToMeta 把 DB 行转为 SessionMeta。
func rowToMeta(row *repository.AgentSession) *SessionMeta {
	if row == nil {
		return nil
	}
	return &SessionMeta{
		ID:                  row.SDKSessionID,
		UserID:              row.UserID,
		ProjectName:         row.ProjectName,
		Title:               row.Title,
		Status:              SessionStatus(row.Status),
		SupersededBy:        row.SupersededBy,
		ForkParentSessionID: row.ForkParentSessionID,
		ForkAnchorUUID:      row.ForkAnchorUUID,
		SDKType:             row.SDKType,
		ClaudeResumeID:      row.ClaudeResumeID,
		CreatedAt:           row.CreatedAt.Time,
		UpdatedAt:           row.UpdatedAt.Time,
	}
}

// Create 创建新会话记录。
func (s *SessionMetaStore) Create(ctx context.Context, project, sdkSessionID, sdkType, userID string, forkParent *string, forkAnchor *string) (*SessionMeta, error) {
	now := time.Now().UTC()
	row := &repository.AgentSession{
		SDKSessionID:        sdkSessionID,
		ProjectName:         project,
		Title:               "",
		Status:              string(SessionStatusIdle),
		SDKType:             sdkType,
		CreatedAt:           db.NewFlexibleTime(now),
		UpdatedAt:           db.NewFlexibleTime(now),
		UserID:              userID,
		ForkParentSessionID: forkParent,
		ForkAnchorUUID:      forkAnchor,
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		return nil, err
	}
	return rowToMeta(row), nil
}

// Get 按 sdk_session_id 查会话；可选限定 user_id。
func (s *SessionMetaStore) Get(ctx context.Context, sessionID string, userID *string) (*SessionMeta, error) {
	q := s.db.WithContext(ctx).Where("sdk_session_id = ?", sessionID)
	if userID != nil {
		q = q.Where("user_id = ?", *userID)
	}
	var row repository.AgentSession
	if err := q.First(&row).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return rowToMeta(&row), nil
}

// List 列出会话（被取代的不进列表）。
func (s *SessionMetaStore) List(ctx context.Context, project *string, status *SessionStatus, limit, offset int, userID *string) ([]*SessionMeta, error) {
	q := s.db.WithContext(ctx).Model(&repository.AgentSession{}).Where("superseded_by IS NULL")
	if project != nil && *project != "" {
		q = q.Where("project_name = ?", *project)
	}
	if status != nil && *status != "" {
		q = q.Where("status = ?", string(*status))
	}
	if userID != nil {
		q = q.Where("user_id = ?", *userID)
	}
	if limit < 1 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var rows []repository.AgentSession
	if err := q.Order("updated_at DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*SessionMeta, 0, len(rows))
	for i := range rows {
		out = append(out, rowToMeta(&rows[i]))
	}
	return out, nil
}

// UpdateStatus 更新会话状态。
func (s *SessionMetaStore) UpdateStatus(ctx context.Context, sessionID string, status SessionStatus, userID *string) (bool, error) {
	q := s.db.WithContext(ctx).Model(&repository.AgentSession{}).Where("sdk_session_id = ?", sessionID)
	if userID != nil {
		q = q.Where("user_id = ?", *userID)
	}
	result := q.Updates(map[string]any{
		"status":     string(status),
		"updated_at": db.NewFlexibleTime(time.Now().UTC()),
	})
	return result.RowsAffected > 0, result.Error
}

// MarkSuperseded 标记被取代；已有指针时不改写。
func (s *SessionMetaStore) MarkSuperseded(ctx context.Context, sessionID, supersededBy string, userID *string) (bool, error) {
	q := s.db.WithContext(ctx).Model(&repository.AgentSession{}).
		Where("sdk_session_id = ? AND superseded_by IS NULL", sessionID)
	if userID != nil {
		q = q.Where("user_id = ?", *userID)
	}
	result := q.Updates(map[string]any{
		"superseded_by": supersededBy,
		"updated_at":    db.NewFlexibleTime(time.Now().UTC()),
	})
	return result.RowsAffected > 0, result.Error
}

// ClearSuperseded 撤回取代指针。
func (s *SessionMetaStore) ClearSuperseded(ctx context.Context, sessionID, supersededBy string, userID *string) (bool, error) {
	q := s.db.WithContext(ctx).Model(&repository.AgentSession{}).
		Where("sdk_session_id = ? AND superseded_by = ?", sessionID, supersededBy)
	if userID != nil {
		q = q.Where("user_id = ?", *userID)
	}
	result := q.Updates(map[string]any{
		"superseded_by": nil,
		"updated_at":    db.NewFlexibleTime(time.Now().UTC()),
	})
	return result.RowsAffected > 0, result.Error
}

// Delete 删除会话（前置指针接手后继）。
func (s *SessionMetaStore) Delete(ctx context.Context, sessionID string, userID *string) (bool, error) {
	var deleted bool
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 查后继
		var successor *string
		q := tx.Model(&repository.AgentSession{}).Where("sdk_session_id = ?", sessionID)
		if userID != nil {
			q = q.Where("user_id = ?", *userID)
		}
		if err := q.Select("superseded_by").Scan(&successor).Error; err != nil {
			return err
		}
		// 前身指针改指后继
		pq := tx.Model(&repository.AgentSession{}).Where("superseded_by = ?", sessionID)
		if userID != nil {
			pq = pq.Where("user_id = ?", *userID)
		}
		if err := pq.Updates(map[string]any{
			"superseded_by": successor,
			"updated_at":    db.NewFlexibleTime(time.Now().UTC()),
		}).Error; err != nil {
			return err
		}
		// 删除本行
		dq := tx.Where("sdk_session_id = ?", sessionID)
		if userID != nil {
			dq = dq.Where("user_id = ?", *userID)
		}
		result := dq.Delete(&repository.AgentSession{})
		deleted = result.RowsAffected > 0
		return result.Error
	})
	return deleted, err
}

// InterruptRunning 把所有 running 状态改为 interrupted。
func (s *SessionMetaStore) InterruptRunning(ctx context.Context, userID *string) (int64, error) {
	q := s.db.WithContext(ctx).Model(&repository.AgentSession{}).Where("status = ?", string(SessionStatusRunning))
	if userID != nil {
		q = q.Where("user_id = ?", *userID)
	}
	result := q.Updates(map[string]any{
		"status":     string(SessionStatusInterrupted),
		"updated_at": db.NewFlexibleTime(time.Now().UTC()),
	})
	return result.RowsAffected, result.Error
}

// UpdateSDKType 更新会话的 SDK 类型。
func (s *SessionMetaStore) UpdateSDKType(ctx context.Context, sessionID, sdkType string, claudeResumeID *string, userID *string) (bool, error) {
	updates := map[string]any{
		"sdk_type":   sdkType,
		"updated_at": db.NewFlexibleTime(time.Now().UTC()),
	}
	if claudeResumeID != nil {
		updates["claude_resume_id"] = *claudeResumeID
	}
	q := s.db.WithContext(ctx).Model(&repository.AgentSession{}).Where("sdk_session_id = ?", sessionID)
	if userID != nil {
		q = q.Where("user_id = ?", *userID)
	}
	result := q.Updates(updates)
	return result.RowsAffected > 0, result.Error
}
