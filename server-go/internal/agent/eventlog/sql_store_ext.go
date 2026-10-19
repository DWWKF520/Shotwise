package eventlog

// 本文件是持久会话契约的事件日志扩展：client_key 幂等、消息身份映射、
// 会话级联删除与前缀分叉（rewrite）。这些能力服务 HTTP 会话面
// （send / rewrite / entries / delete），不参与 eventAdapter 的 live 写入
// 路径——Store 接口保持不变，全部以 SQLStore 的附加方法暴露。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/infra/db"
)

// ErrAnchorNotFound 表示锚点条目在原会话的事件日志中不存在（或不可作为分叉锚）。
var ErrAnchorNotFound = errors.New("eventlog: anchor entry not found")

// userMessageLinkRow 是 agent_session_user_message_links 表的 ORM 模型：
// 事件日志用户条目 uuid ↔ 续接域条目 uuid 的身份映射（幂等，首写即权威）。
// Go 侧事件日志即权威 transcript（无独立 SDK transcript 域），该表主要承载
// Python 侧历史数据的兼容读。
type userMessageLinkRow struct {
	SessionID     string          `gorm:"column:session_id;primaryKey"`
	UserEntryUUID string          `gorm:"column:user_entry_uuid;primaryKey"`
	SDKEntryUUID  string          `gorm:"column:sdk_entry_uuid"`
	CreatedAt     db.FlexibleTime `gorm:"column:created_at"`
	UpdatedAt     db.FlexibleTime `gorm:"column:updated_at"`
	UserID        string          `gorm:"column:user_id"`
}

func (userMessageLinkRow) TableName() string { return "agent_session_user_message_links" }

// isUniqueViolation 判定唯一约束冲突：gorm 翻译错误优先，错误文本兜底
// （SQLite "UNIQUE constraint failed" / PostgreSQL "duplicate key ... unique
// constraint"）。与 Python 侧 SQLSTATE 优先、文案仅兜底的判定同语义。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value violates unique constraint") ||
		strings.Contains(msg, "23505")
}

// decodeEntry 反序列化 payload 为 Entry。
func decodeEntry(payload string) (*Entry, error) {
	e := &Entry{}
	if err := json.Unmarshal([]byte(payload), e); err != nil {
		return nil, err
	}
	return e, nil
}

// FindByClientKey 按幂等键查会话内的既有条目（seq 最小的一条）；无则返回 nil。
func (s *SQLStore) FindByClientKey(ctx context.Context, sessionID, clientKey string) (*Entry, error) {
	if clientKey == "" {
		return nil, nil
	}
	var row eventLogRow
	err := s.db.WithContext(ctx).
		Where("session_id = ? AND client_key = ?", sessionID, clientKey).
		Order("seq ASC").First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return decodeEntry(row.Payload)
}

// FindNewSessionByClientKey 跨会话按幂等键定位新会话的受理条目（该会话首条），
// 返回 (session_id, 权威条目)；无命中返回 ("", nil)。
//
// client_key 唯一索引按 (session_id, client_key) 分区，覆盖不到 session_id
// 尚不存在的新会话受理；进程内映射丢失时由本查询兜底让重试命中既有会话，
// 而非重复建会话。判定「首条」按该会话 min(seq) 语义匹配——Go 侧 seq 从 1
// 起（Python 为 0），不硬编码 seq 值。
func (s *SQLStore) FindNewSessionByClientKey(ctx context.Context, clientKey string) (string, *Entry, error) {
	if clientKey == "" {
		return "", nil, nil
	}
	var row eventLogRow
	err := s.db.WithContext(ctx).Raw(
		`SELECT e.* FROM agent_session_event_log e
		 WHERE e.client_key = ?
		   AND e.seq = (SELECT MIN(m.seq) FROM agent_session_event_log m WHERE m.session_id = e.session_id)
		 ORDER BY e.created_at ASC
		 LIMIT 1`, clientKey).Scan(&row).Error
	if err != nil {
		return "", nil, err
	}
	if row.SessionID == "" {
		return "", nil, nil
	}
	e, err := decodeEntry(row.Payload)
	if err != nil {
		return "", nil, err
	}
	return row.SessionID, e, nil
}

// AppendUserEntry 幂等追加单条用户条目：同一 client_key 重试返回既有权威条目，
// 不产生重复。client_key 为空时退化为普通追加。返回 (权威条目, 是否新写入)。
//
// seq 分配与 Append 共用一把进程内互斥锁；client_key 冲突由唯一索引兜底
// （uq_agent_event_log_client_key ON (session_id, client_key) WHERE 非空）。
func (s *SQLStore) AppendUserEntry(ctx context.Context, sessionID string, entry *Entry, clientKey string) (*Entry, bool, error) {
	if clientKey != "" {
		if existing, err := s.FindByClientKey(ctx, sessionID, clientKey); err != nil {
			return nil, false, err
		} else if existing != nil {
			return existing, false, nil
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var maxSeq int64
	if err := s.db.WithContext(ctx).Model(&eventLogRow{}).
		Where("session_id = ?", sessionID).
		Select("COALESCE(MAX(seq), 0)").Scan(&maxSeq).Error; err != nil {
		return nil, false, err
	}
	entry.SessionID = sessionID
	entry.Seq = maxSeq + 1
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return nil, false, err
	}
	now := db.NewFlexibleTime(time.Now().UTC())
	row := eventLogRow{
		SessionID: sessionID,
		Seq:       entry.Seq,
		EntryType: string(entry.Type),
		Payload:   string(payload),
		CreatedAt: now,
		UpdatedAt: now,
		UserID:    DefaultUserID,
	}
	if clientKey != "" {
		ck := clientKey
		row.ClientKey = &ck
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		// 幂等键并发冲突：另一请求已写入同键条目，返回其权威条目。
		if clientKey != "" && isUniqueViolation(err) {
			if existing, ferr := s.FindByClientKey(ctx, sessionID, clientKey); ferr == nil && existing != nil {
				return existing, false, nil
			}
		}
		return nil, false, err
	}
	return entry, true, nil
}

// DeleteEntry 补偿删除单条条目（受理失败回滚：投递失败时撤销刚写入的用户条目，
// 否则同幂等键重试会短路而永不投递）。
func (s *SQLStore) DeleteEntry(ctx context.Context, sessionID string, seq int64) error {
	return s.db.WithContext(ctx).
		Where("session_id = ? AND seq = ?", sessionID, seq).
		Delete(&eventLogRow{}).Error
}

// DeleteSession 级联删除会话的事件日志与消息身份映射（会话删除的收口）。
func (s *SQLStore) DeleteSession(ctx context.Context, sessionID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("session_id = ?", sessionID).Delete(&eventLogRow{}).Error; err != nil {
			return err
		}
		return tx.Where("session_id = ?", sessionID).Delete(&userMessageLinkRow{}).Error
	})
}

// HasEntries 报告会话是否已有条目（前缀分叉 / 懒生成的判重依据）。
func (s *SQLStore) HasEntries(ctx context.Context, sessionID string) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).Model(&eventLogRow{}).
		Where("session_id = ?", sessionID).
		Count(&count).Error
	return count > 0, err
}

// RecordUserMessageLink 幂等记录用户条目 uuid ↔ 续接域条目 uuid 的身份映射：
// 同一条目重复记录不产生第二行，首写即权威。
func (s *SQLStore) RecordUserMessageLink(ctx context.Context, sessionID, userEntryUUID, sdkEntryUUID string) error {
	now := db.NewFlexibleTime(time.Now().UTC())
	row := userMessageLinkRow{
		SessionID:     sessionID,
		UserEntryUUID: userEntryUUID,
		SDKEntryUUID:  sdkEntryUUID,
		CreatedAt:     now,
		UpdatedAt:     now,
		UserID:        DefaultUserID,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		if isUniqueViolation(err) {
			return nil
		}
		return err
	}
	return nil
}

// FindUserMessageLink 按用户条目 uuid 查映射的续接域条目 uuid；无映射返回空。
func (s *SQLStore) FindUserMessageLink(ctx context.Context, sessionID, userEntryUUID string) (string, error) {
	var row userMessageLinkRow
	err := s.db.WithContext(ctx).
		Where("session_id = ? AND user_entry_uuid = ?", sessionID, userEntryUUID).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", err
	}
	return row.SDKEntryUUID, nil
}

// ResolveUserMessageAnchor 把事件日志里的用户条目 uuid 解析为可作分叉锚的
// 身份。两段：映射表优先；未命中时按恒等性回退——只接受主线 plain user 条目
// （无 subtype、无 parent_tool_use_id），问答回复 / tool_result / skill 注入等
// 派生条目作锚点没有意义。解析不出返回 ErrAnchorNotFound。
func (s *SQLStore) ResolveUserMessageAnchor(ctx context.Context, sessionID, entryUUID string) (string, error) {
	if linked, err := s.FindUserMessageLink(ctx, sessionID, entryUUID); err != nil {
		return "", err
	} else if linked != "" {
		return linked, nil
	}
	entries, err := s.List(ctx, sessionID, -1)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.UUID != entryUUID {
			continue
		}
		if e.Type != EntryUser || e.Subtype != "" || e.ParentToolUseID != "" {
			return "", ErrAnchorNotFound
		}
		return entryUUID, nil
	}
	return "", ErrAnchorNotFound
}

// CopySessionPrefix 把原会话锚点之前的条目前缀复制到新 session_id 下
// （rewrite 分叉）。锚点按 uuid 解析；锚点不是主线 plain user 条目时拒绝
// （ErrAnchorNotFound）。复制保留原条目身份（uuid / 时间戳），仅重排 seq。
// 返回复制条数；锚点即首条时为 0（空前缀，新会话就是全新会话）。
func (s *SQLStore) CopySessionPrefix(ctx context.Context, originSessionID, anchorEntryUUID, newSessionID string) (int, error) {
	entries, err := s.List(ctx, originSessionID, -1)
	if err != nil {
		return 0, err
	}
	// 定位锚点在日志中的位置（主线 user 条目，带 seq）。
	anchorIdx := -1
	for i, e := range entries {
		if e.UUID == anchorEntryUUID &&
			e.Type == EntryUser && e.Subtype == "" && e.ParentToolUseID == "" {
			anchorIdx = i
			break
		}
	}
	if anchorIdx < 0 {
		return 0, ErrAnchorNotFound
	}
	if anchorIdx == 0 {
		return 0, nil // 空前缀
	}
	prefix := make([]*Entry, anchorIdx)
	for i := 0; i < anchorIdx; i++ {
		clone := *entries[i]
		clone.SessionID = newSessionID
		prefix[i] = &clone
	}
	_, err = s.Append(ctx, prefix)
	if err != nil {
		return 0, err
	}
	return len(prefix), nil
}
