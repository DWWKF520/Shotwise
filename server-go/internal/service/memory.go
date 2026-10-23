// 用户 / 项目级长期记忆的业务规则（对应 server/services/memory_service.py）：
// 校验、查重、候选确认与导入导出。
//
// 记忆是参考材料：写入时拒绝疑似密钥或指令形内容，避免后续 prompt 渲染把密钥 / 注入指令
// 变成持久上下文。刻意不做隐去（redact），而是直接拒绝。
package service

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
	"golang.org/x/text/unicode/norm"
)

// 记忆字段上限与取值集合（与 Python 侧常量对齐）。
const (
	MemoryMaxLength         = 8000
	MemoryCategoryMaxLength = 32
	MemorySourceMaxLength   = 32
)

var memoryScopes = map[string]bool{"user": true, "project": true}

var memoryCategories = map[string]bool{
	"preference": true, "style": true, "world": true, "terminology": true, "workflow": true, "other": true,
}

var memorySourcePattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

var memoryControlChars = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)

var memorySensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:api[_ -]?key|access[_ -]?token|refresh[_ -]?token|client[_ -]?secret)\b`),
	regexp.MustCompile(`(?i)\b(?:authorization|bearer)\s*[:=]`),
	regexp.MustCompile(`(?i)\b(?:password|passwd|secret)\s*[:=]`),
	regexp.MustCompile(`(?i)(?:^|[\\/])(?:users?|home|\.ssh|\.env)(?:[\\/]|$)`),
}

var memoryPromptInjectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bignore\s+(?:all\s+)?(?:previous|prior|above)\s+instructions?\b`),
	regexp.MustCompile(`(?i)\b(?:system|developer)\s+(?:message|prompt|instruction)s?\b`),
	regexp.MustCompile(`(?i)\byou\s+are\s+now\s+(?:a|an)\b`),
	regexp.MustCompile(`(?i)\b(?:override|bypass)\s+(?:the\s+)?(?:system|safety|security)\b`),
	regexp.MustCompile(`<\s*/?\s*(?:system|developer|tool)\b`),
}

// MemoryValidationError 是记忆写入校验失败（HTTP 400）；Code 是机读标识（agent 工具回包用）。
type MemoryValidationError struct{ Code string }

func (e *MemoryValidationError) Error() string { return e.Code }

// MemoryNotFoundError 表示记忆条目或候选不存在（HTTP 404）。
type MemoryNotFoundError struct{ Code string }

func (e *MemoryNotFoundError) Error() string { return e.Code }

// MemoryConflictError 表示与记忆当前状态冲突（HTTP 409）。
type MemoryConflictError struct{ Code string }

func (e *MemoryConflictError) Error() string { return e.Code }

// ValidateMemoryScope 校验 scope；project 作用域要求非空且长度合规的项目名。
func ValidateMemoryScope(scope, projectName string) (string, *string, error) {
	if !memoryScopes[scope] {
		return "", nil, &MemoryValidationError{Code: "memory_scope_invalid"}
	}
	if scope != "project" {
		return scope, nil, nil
	}
	if projectName == "" {
		return "", nil, &MemoryValidationError{Code: "memory_project_required"}
	}
	trimmed := strings.TrimSpace(projectName)
	if trimmed == "" || len(trimmed) > 200 {
		return "", nil, &MemoryValidationError{Code: "memory_project_invalid"}
	}
	return scope, &trimmed, nil
}

// ValidateMemoryCategory 归一并校验类别。
func ValidateMemoryCategory(category string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(category))
	if normalized == "" || len(normalized) > MemoryCategoryMaxLength || !memoryCategories[normalized] {
		return "", &MemoryValidationError{Code: "memory_category_invalid"}
	}
	return normalized, nil
}

// ValidateMemorySource 归一并校验来源标识。
func ValidateMemorySource(source string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(source))
	if normalized == "" || len(normalized) > MemorySourceMaxLength || !memorySourcePattern.MatchString(normalized) {
		return "", &MemoryValidationError{Code: "memory_source_invalid"}
	}
	return normalized, nil
}

// SanitizeMemoryContent 归一记忆文本并拒绝疑似密钥 / 指令形内容。
func SanitizeMemoryContent(content string) (string, error) {
	normalized := norm.NFC.String(strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n"))
	normalized = memoryControlChars.ReplaceAllString(normalized, "")
	normalized = strings.TrimSpace(normalized)
	if normalized == "" {
		return "", &MemoryValidationError{Code: "memory_content_empty"}
	}
	if len([]rune(normalized)) > MemoryMaxLength {
		return "", &MemoryValidationError{Code: "memory_content_too_long"}
	}
	for _, pattern := range memorySensitivePatterns {
		if pattern.MatchString(normalized) {
			return "", &MemoryValidationError{Code: "memory_sensitive_content"}
		}
	}
	for _, pattern := range memoryPromptInjectionPatterns {
		if pattern.MatchString(normalized) {
			return "", &MemoryValidationError{Code: "memory_prompt_injection"}
		}
	}
	return normalized, nil
}

// CreateMemoryCandidateParams 是一次候选记忆创建的入参。
type CreateMemoryCandidateParams struct {
	UserID          string
	Scope           string
	ProjectName     *string
	Category        string
	Content         string
	SourceSessionID *string
	SourceMessageID *string
	Metadata        map[string]any
}

// CreateMemoryParams 是 create_memory 的入参。
type CreateMemoryParams struct {
	UserID          string
	Scope           string
	ProjectName     *string
	Category        string
	Content         string
	Source          string
	SourceSessionID *string
	SourceMessageID *string
	Metadata        map[string]any
}

// MemoryCandidateStore 是候选记忆的持久化能力（*MemoryService 满足）。
type MemoryCandidateStore interface {
	CreateMemoryCandidate(ctx context.Context, p CreateMemoryCandidateParams) (string, error)
}

// MemoryService 是记忆读写服务。
type MemoryService struct {
	db   *gorm.DB
	repo *repository.MemoryRepo
}

// NewMemoryService 构造记忆服务。
func NewMemoryService(db *gorm.DB) *MemoryService {
	return &MemoryService{db: db, repo: repository.NewMemoryRepo(db)}
}

// CreateMemoryCandidate 校验后写入一条候选记忆（status=pending、confirmed=false、source=agent）。
func (s *MemoryService) CreateMemoryCandidate(ctx context.Context, p CreateMemoryCandidateParams) (string, error) {
	row, err := s.CreateCandidate(ctx, p)
	if err != nil {
		return "", err
	}
	id, _ := row["id"].(string)
	return id, nil
}

// validateRecord 归一 scope / project / category / content / source，任一步失败即返回错误。
func (s *MemoryService) validateRecord(
	scope string, projectName *string, category, content, source string,
) (string, *string, string, string, string, error) {
	normScope, normProject, err := ValidateMemoryScope(scope, DerefString(projectName))
	if err != nil {
		return "", nil, "", "", "", err
	}
	normCategory, err := ValidateMemoryCategory(category)
	if err != nil {
		return "", nil, "", "", "", err
	}
	normContent, err := SanitizeMemoryContent(content)
	if err != nil {
		return "", nil, "", "", "", err
	}
	normSource, err := ValidateMemorySource(source)
	if err != nil {
		return "", nil, "", "", "", err
	}
	return normScope, normProject, normCategory, normContent, normSource, nil
}

// newEntryRow 构造一条待写入的记忆条目。
func newEntryRow(
	userID, scope string, projectName *string, category, content, source string,
	confirmed bool, sessionID, messageID *string, metadata map[string]any,
) (*repository.MemoryEntry, error) {
	meta, err := marshalMetadataJSON(metadata)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	return &repository.MemoryEntry{
		ID:              uuid.NewString(),
		UserID:          userID,
		Scope:           scope,
		ProjectName:     projectName,
		Category:        category,
		Content:         content,
		Source:          source,
		Confirmed:       confirmed,
		SourceSessionID: sessionID,
		SourceMessageID: messageID,
		MetadataJSON:    meta,
		CreatedAt:       db.NewFlexibleTime(now),
		UpdatedAt:       db.NewFlexibleTime(now),
	}, nil
}

// ListMemories 列出已确认的记忆条目。
func (s *MemoryService) ListMemories(
	ctx context.Context, userID, scope string, projectName *string,
) ([]map[string]any, error) {
	normScope, normProject, err := ValidateMemoryScope(scope, DerefString(projectName))
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.ListEntries(ctx, userID, normScope, normProject)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(rows))
	for i := range rows {
		items = append(items, memoryEntryPayload(&rows[i]))
	}
	return items, nil
}

// CreateMemory 校验、查重后写入一条已确认的记忆条目。
// Source 缺省为 "user"（对齐 Python 侧 create_memory 的默认值）。
func (s *MemoryService) CreateMemory(ctx context.Context, p CreateMemoryParams) (map[string]any, error) {
	source := p.Source
	if source == "" {
		source = "user"
	}
	scope, project, category, content, source, err := s.validateRecord(
		p.Scope, p.ProjectName, p.Category, p.Content, source,
	)
	if err != nil {
		return nil, err
	}
	dup, err := s.repo.FindDuplicate(ctx, p.UserID, scope, project, category, content)
	if err != nil {
		return nil, err
	}
	if dup != nil {
		return nil, &MemoryConflictError{Code: "memory_duplicate"}
	}
	row, err := newEntryRow(
		p.UserID, scope, project, category, content, source, true,
		p.SourceSessionID, p.SourceMessageID, p.Metadata,
	)
	if err != nil {
		return nil, err
	}
	if err := s.repo.CreateEntry(ctx, row); err != nil {
		return nil, err
	}
	return memoryEntryPayload(row), nil
}

// UpdateMemory 更新记忆条目的类别 / 内容 / metadata。
func (s *MemoryService) UpdateMemory(
	ctx context.Context, userID, memoryID string, category, content *string, metadata map[string]any,
) (map[string]any, error) {
	if category == nil && content == nil && metadata == nil {
		return nil, &MemoryValidationError{Code: "memory_update_empty"}
	}
	values := map[string]any{}
	if category != nil {
		normCategory, err := ValidateMemoryCategory(*category)
		if err != nil {
			return nil, err
		}
		values["category"] = normCategory
	}
	if content != nil {
		normContent, err := SanitizeMemoryContent(*content)
		if err != nil {
			return nil, err
		}
		values["content"] = normContent
	}
	if metadata != nil {
		meta, err := marshalMetadataJSON(metadata)
		if err != nil {
			return nil, err
		}
		values["metadata_json"] = *meta
	}
	row, err := s.repo.UpdateEntry(ctx, memoryID, userID, values)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, &MemoryNotFoundError{Code: "memory_not_found"}
	}
	return memoryEntryPayload(row), nil
}

// DeleteMemory 删除单条记忆条目。
func (s *MemoryService) DeleteMemory(ctx context.Context, userID, memoryID string) error {
	ok, err := s.repo.DeleteEntry(ctx, memoryID, userID)
	if err != nil {
		return err
	}
	if !ok {
		return &MemoryNotFoundError{Code: "memory_not_found"}
	}
	return nil
}

// ClearMemories 清空给定作用域的记忆条目，返回删除条数。
func (s *MemoryService) ClearMemories(ctx context.Context, userID, scope string, projectName *string) (int64, error) {
	normScope, normProject, err := ValidateMemoryScope(scope, DerefString(projectName))
	if err != nil {
		return 0, err
	}
	return s.repo.ClearEntries(ctx, userID, normScope, normProject)
}

// CreateCandidate 校验后写入一条候选记忆（source 固定为 agent）。
func (s *MemoryService) CreateCandidate(ctx context.Context, p CreateMemoryCandidateParams) (map[string]any, error) {
	scope, project, category, content, _, err := s.validateRecord(
		p.Scope, p.ProjectName, p.Category, p.Content, "agent",
	)
	if err != nil {
		return nil, err
	}
	meta, err := marshalMetadataJSON(p.Metadata)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	row := &repository.MemoryCandidate{
		ID:              uuid.NewString(),
		UserID:          p.UserID,
		Scope:           scope,
		ProjectName:     project,
		Category:        category,
		Content:         content,
		Source:          "agent",
		Confirmed:       false,
		Status:          "pending",
		SourceSessionID: p.SourceSessionID,
		SourceMessageID: p.SourceMessageID,
		MetadataJSON:    meta,
		CreatedAt:       db.NewFlexibleTime(now),
		UpdatedAt:       db.NewFlexibleTime(now),
	}
	if err := s.repo.CreateCandidate(ctx, row); err != nil {
		return nil, err
	}
	return memoryCandidatePayload(row), nil
}

// ListCandidates 列出待决候选；指定项目时必须同时指定作用域。
func (s *MemoryService) ListCandidates(
	ctx context.Context, userID string, scope, projectName *string,
) ([]map[string]any, error) {
	if scope != nil {
		normScope, normProject, err := ValidateMemoryScope(*scope, DerefString(projectName))
		if err != nil {
			return nil, err
		}
		scope, projectName = &normScope, normProject
	} else if projectName != nil {
		return nil, &MemoryValidationError{Code: "memory_scope_required"}
	}
	rows, err := s.repo.ListCandidates(ctx, userID, scope, projectName)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(rows))
	for i := range rows {
		items = append(items, memoryCandidatePayload(&rows[i]))
	}
	return items, nil
}

// AcceptCandidate 把候选提升为长期记忆（重复则直接复用已有条目）。
func (s *MemoryService) AcceptCandidate(ctx context.Context, userID, candidateID string) (map[string]any, error) {
	candidate, err := s.repo.GetCandidate(ctx, candidateID, userID)
	if err != nil {
		return nil, err
	}
	if candidate == nil {
		return nil, &MemoryNotFoundError{Code: "memory_candidate_not_found"}
	}
	if candidate.Status != "pending" {
		return nil, &MemoryConflictError{Code: "memory_candidate_resolved"}
	}
	dup, err := s.repo.FindDuplicate(
		ctx, userID, candidate.Scope, candidate.ProjectName, candidate.Category, candidate.Content,
	)
	if err != nil {
		return nil, err
	}
	var memory map[string]any
	if dup == nil {
		row, err := newEntryRow(
			userID, candidate.Scope, candidate.ProjectName, candidate.Category, candidate.Content,
			"agent_confirmed", true, candidate.SourceSessionID, candidate.SourceMessageID,
			memoryMetadataPayload(candidate.MetadataJSON),
		)
		if err != nil {
			return nil, err
		}
		if err := s.repo.CreateEntry(ctx, row); err != nil {
			return nil, err
		}
		memory = memoryEntryPayload(row)
	} else {
		memory = memoryEntryPayload(dup)
	}
	if _, err := s.repo.SetCandidateStatus(ctx, candidateID, userID, "accepted", true); err != nil {
		return nil, err
	}
	return map[string]any{"memory": memory, "candidate_id": candidateID, "status": "accepted"}, nil
}

// RejectCandidate 拒绝候选。
func (s *MemoryService) RejectCandidate(ctx context.Context, userID, candidateID string) (map[string]any, error) {
	candidate, err := s.repo.GetCandidate(ctx, candidateID, userID)
	if err != nil {
		return nil, err
	}
	if candidate == nil {
		return nil, &MemoryNotFoundError{Code: "memory_candidate_not_found"}
	}
	if candidate.Status != "pending" {
		return nil, &MemoryConflictError{Code: "memory_candidate_resolved"}
	}
	if _, err := s.repo.SetCandidateStatus(ctx, candidateID, userID, "rejected", false); err != nil {
		return nil, err
	}
	return map[string]any{"candidate_id": candidateID, "status": "rejected"}, nil
}

// ExportMemories 导出用户级（及可选项目级）记忆。
func (s *MemoryService) ExportMemories(ctx context.Context, userID string, projectName *string) (map[string]any, error) {
	userRows, err := s.repo.ListEntries(ctx, userID, "user", nil)
	if err != nil {
		return nil, err
	}
	userMemories := make([]map[string]any, 0, len(userRows))
	for i := range userRows {
		userMemories = append(userMemories, memoryEntryPayload(&userRows[i]))
	}
	projectMemories := []map[string]any{}
	if projectName != nil {
		rows, err := s.repo.ListEntries(ctx, userID, "project", projectName)
		if err != nil {
			return nil, err
		}
		for i := range rows {
			projectMemories = append(projectMemories, memoryEntryPayload(&rows[i]))
		}
	}
	return map[string]any{
		"format":           "shotwise-agent-memory",
		"version":          1,
		"user_memories":    userMemories,
		"project_memories": projectMemories,
	}, nil
}

// ImportMemories 增量导入记忆导出文件：用户级保留全局作用域，项目级映射到显式目标项目。
func (s *MemoryService) ImportMemories(
	ctx context.Context, userID string, payload map[string]any, projectName *string,
) (map[string]int, error) {
	userEntries, ok := memoryEntryList(payload, "user_memories")
	if !ok {
		return nil, &MemoryValidationError{Code: "memory_import_invalid"}
	}
	projectEntries, ok := memoryEntryList(payload, "project_memories")
	if !ok {
		return nil, &MemoryValidationError{Code: "memory_import_invalid"}
	}
	if projectEntries == nil {
		projectEntries, ok = memoryEntryList(payload, "entries")
		if !ok {
			return nil, &MemoryValidationError{Code: "memory_import_invalid"}
		}
	}
	if len(projectEntries) > 0 && projectName == nil {
		return nil, &MemoryValidationError{Code: "memory_project_required"}
	}

	imported, skipped := 0, 0
	plan := []struct {
		scope   string
		rows    []map[string]any
		project *string
	}{
		{"user", userEntries, nil},
		{"project", projectEntries, projectName},
	}
	for _, group := range plan {
		for _, raw := range group.rows {
			content, ok := rawContentString(raw["content"])
			if !ok {
				return nil, &MemoryValidationError{Code: "memory_content_invalid"}
			}
			scope, project, category, content, _, err := s.validateRecord(
				group.scope, group.project, pythonStr(raw["category"], "other"), content, "import",
			)
			if err != nil {
				return nil, &MemoryValidationError{Code: "memory_import_invalid"}
			}
			dup, err := s.repo.FindDuplicate(ctx, userID, scope, project, category, content)
			if err != nil {
				return nil, err
			}
			if dup != nil {
				skipped++
				continue
			}
			row, err := newEntryRow(userID, scope, project, category, content, "import", true, nil, nil,
				map[string]any{"imported": true})
			if err != nil {
				return nil, err
			}
			if err := s.repo.CreateEntry(ctx, row); err != nil {
				return nil, err
			}
			imported++
		}
	}
	return map[string]int{"imported": imported, "skipped": skipped}, nil
}

// ImportProjectMemories 把项目归档只导入到指定项目（忽略来源项目 / 用户字段）。
func (s *MemoryService) ImportProjectMemories(
	ctx context.Context, userID, projectName string, payload map[string]any,
) (map[string]int, error) {
	entries, ok := memoryEntryList(payload, "project_memories")
	if !ok {
		return nil, &MemoryValidationError{Code: "memory_import_invalid"}
	}
	if entries == nil {
		entries, ok = memoryEntryList(payload, "entries")
		if !ok {
			return nil, &MemoryValidationError{Code: "memory_import_invalid"}
		}
	}
	destination := projectName
	imported, skipped := 0, 0
	for _, raw := range entries {
		content, ok := rawContentString(raw["content"])
		if !ok {
			return nil, &MemoryValidationError{Code: "memory_content_invalid"}
		}
		scope, project, category, content, source, err := s.validateRecord(
			"project", &destination, pythonStr(raw["category"], "other"), content, "import",
		)
		if err != nil {
			return nil, &MemoryValidationError{Code: "memory_import_invalid"}
		}
		dup, err := s.repo.FindDuplicate(ctx, userID, scope, project, category, content)
		if err != nil {
			return nil, err
		}
		if dup != nil {
			skipped++
			continue
		}
		row, err := newEntryRow(userID, scope, project, category, content, source, true, nil, nil,
			map[string]any{"imported": true})
		if err != nil {
			return nil, err
		}
		if err := s.repo.CreateEntry(ctx, row); err != nil {
			return nil, err
		}
		imported++
	}
	return map[string]int{"imported": imported, "skipped": skipped}, nil
}

// memoryEntryList 读取导入载荷里的一个条目数组；键缺失返回 (nil, true)，
// 键存在但类型不对返回 (nil, false)。
func memoryEntryList(payload map[string]any, key string) ([]map[string]any, bool) {
	raw, present := payload[key]
	if !present || raw == nil {
		return nil, true
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	items := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		item, ok := entry.(map[string]any)
		if !ok {
			return nil, false
		}
		items = append(items, item)
	}
	return items, true
}

// pythonStr 复刻 Python 的 str(raw.get(key, default)) 语义：缺失 / null 走 fallback，
// 其余标量转字符串。
func pythonStr(v any, fallback string) string {
	if v == nil {
		return fallback
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fallback
		}
		return string(b)
	}
}

// rawContentString 复刻 Python 的 raw.get("content", "") 语义：缺失 / null 视为空串，
// 非字符串返回 ok=false（对应 Python 侧 sanitize_content 的 memory_content_invalid）。
func rawContentString(v any) (string, bool) {
	if v == nil {
		return "", true
	}
	if s, ok := v.(string); ok {
		return s, true
	}
	return "", false
}

func memoryEntryPayload(row *repository.MemoryEntry) map[string]any {
	return map[string]any{
		"id":                row.ID,
		"user_id":           row.UserID,
		"scope":             row.Scope,
		"project_name":      row.ProjectName,
		"category":          row.Category,
		"content":           row.Content,
		"source":            row.Source,
		"confirmed":         row.Confirmed,
		"source_session_id": row.SourceSessionID,
		"source_message_id": row.SourceMessageID,
		"metadata":          memoryMetadataPayload(row.MetadataJSON),
		"created_at":        IsoformatTime(row.CreatedAt.Time),
		"updated_at":        IsoformatTime(row.UpdatedAt.Time),
	}
}

func memoryCandidatePayload(row *repository.MemoryCandidate) map[string]any {
	payload := memoryEntryPayload(&repository.MemoryEntry{
		ID:              row.ID,
		UserID:          row.UserID,
		Scope:           row.Scope,
		ProjectName:     row.ProjectName,
		Category:        row.Category,
		Content:         row.Content,
		Source:          row.Source,
		Confirmed:       row.Confirmed,
		SourceSessionID: row.SourceSessionID,
		SourceMessageID: row.SourceMessageID,
		MetadataJSON:    row.MetadataJSON,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	})
	payload["status"] = row.Status
	return payload
}

// memoryMetadataPayload 解析库里的 metadata JSON 文本；空或非法一律回落空对象。
func memoryMetadataPayload(raw *string) map[string]any {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(*raw), &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// marshalMetadataJSON 把记忆 metadata 序列化为库里的 JSON 文本；空 map 按 Python 口径落 "{}"。
func marshalMetadataJSON(m map[string]any) (*string, error) {
	if m == nil {
		m = map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	text := string(b)
	return &text, nil
}
