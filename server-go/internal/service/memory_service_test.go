package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/shotwise/shotwise/internal/repository"
)

func memoryValidationCode(t *testing.T, err error) string {
	t.Helper()
	var validation *MemoryValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("expected *MemoryValidationError, got %v", err)
	}
	return validation.Code
}

func TestMemoryCreateListClear(t *testing.T) {
	ctx := context.Background()
	svc := NewMemoryService(newServiceTestDB(t))
	user := repository.DefaultUserID

	item, err := svc.CreateMemory(ctx, CreateMemoryParams{
		UserID: user, Scope: "user", Category: "Preference", Content: "偏好中景",
	})
	if err != nil {
		t.Fatalf("create memory: %v", err)
	}
	if item["category"] != "preference" || item["source"] != "user" || item["confirmed"] != true {
		t.Fatalf("unexpected payload: %v", item)
	}

	// 同作用域同类别同内容再次写入应触发 memory_duplicate。
	var conflict *MemoryConflictError
	if _, err := svc.CreateMemory(ctx, CreateMemoryParams{
		UserID: user, Scope: "user", Category: "preference", Content: "偏好中景",
	}); !errors.As(err, &conflict) || conflict.Code != "memory_duplicate" {
		t.Fatalf("want memory_duplicate, got %v", err)
	}

	items, err := svc.ListMemories(ctx, user, "user", nil)
	if err != nil || len(items) != 1 {
		t.Fatalf("list user memories: %v len=%d", err, len(items))
	}

	deleted, err := svc.ClearMemories(ctx, user, "user", nil)
	if err != nil || deleted != 1 {
		t.Fatalf("clear memories: %v deleted=%d", err, deleted)
	}
}

func TestMemorySensitiveContentRejected(t *testing.T) {
	ctx := context.Background()
	svc := NewMemoryService(newServiceTestDB(t))
	_, err := svc.CreateMemory(ctx, CreateMemoryParams{
		UserID: repository.DefaultUserID, Scope: "user", Category: "other", Content: "api_key: abc123",
	})
	if code := memoryValidationCode(t, err); code != "memory_sensitive_content" {
		t.Fatalf("want memory_sensitive_content, got %s", code)
	}
}

func TestMemoryUpdateAndDelete(t *testing.T) {
	ctx := context.Background()
	svc := NewMemoryService(newServiceTestDB(t))
	user := repository.DefaultUserID

	if _, err := svc.UpdateMemory(ctx, user, "missing", nil, nil, nil); err == nil {
		t.Fatal("empty update should be rejected")
	} else if code := memoryValidationCode(t, err); code != "memory_update_empty" {
		t.Fatalf("want memory_update_empty, got %s", code)
	}

	item, err := svc.CreateMemory(ctx, CreateMemoryParams{
		UserID: user, Scope: "user", Category: "other", Content: "原始内容",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := item["id"].(string)
	newCategory := "style"
	updated, err := svc.UpdateMemory(ctx, user, id, &newCategory, nil, nil)
	if err != nil || updated["category"] != "style" {
		t.Fatalf("update: %v payload=%v", err, updated)
	}
	if err := svc.DeleteMemory(ctx, user, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var notFound *MemoryNotFoundError
	if err := svc.DeleteMemory(ctx, user, id); !errors.As(err, &notFound) || notFound.Code != "memory_not_found" {
		t.Fatalf("want memory_not_found, got %v", err)
	}
}

func TestMemoryCandidateAcceptAndReject(t *testing.T) {
	ctx := context.Background()
	svc := NewMemoryService(newServiceTestDB(t))
	user := repository.DefaultUserID

	candidate, err := svc.CreateCandidate(ctx, CreateMemoryCandidateParams{
		UserID: user, Scope: "user", Category: "workflow", Content: "先出分镜再配音",
	})
	if err != nil {
		t.Fatalf("create candidate: %v", err)
	}
	if candidate["status"] != "pending" || candidate["source"] != "agent" {
		t.Fatalf("unexpected candidate: %v", candidate)
	}

	pending, err := svc.ListCandidates(ctx, user, nil, nil)
	if err != nil || len(pending) != 1 {
		t.Fatalf("list candidates: %v len=%d", err, len(pending))
	}

	accepted, err := svc.AcceptCandidate(ctx, user, candidate["id"].(string))
	if err != nil || accepted["status"] != "accepted" {
		t.Fatalf("accept: %v payload=%v", err, accepted)
	}

	// 候选接受后不可再次处理。
	var conflict *MemoryConflictError
	if _, err := svc.RejectCandidate(ctx, user, candidate["id"].(string)); !errors.As(err, &conflict) ||
		conflict.Code != "memory_candidate_resolved" {
		t.Fatalf("want memory_candidate_resolved, got %v", err)
	}

	// 另一条候选走拒绝路径。
	other, err := svc.CreateCandidate(ctx, CreateMemoryCandidateParams{
		UserID: user, Scope: "user", Category: "other", Content: "另一条候选",
	})
	if err != nil {
		t.Fatalf("create second candidate: %v", err)
	}
	rejected, err := svc.RejectCandidate(ctx, user, other["id"].(string))
	if err != nil || rejected["status"] != "rejected" {
		t.Fatalf("reject: %v payload=%v", err, rejected)
	}
}

func TestMemoryExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc := NewMemoryService(newServiceTestDB(t))
	user := repository.DefaultUserID

	if _, err := svc.CreateMemory(ctx, CreateMemoryParams{
		UserID: user, Scope: "user", Category: "other", Content: "用户级记忆",
	}); err != nil {
		t.Fatalf("seed user memory: %v", err)
	}
	project := "demo"
	if _, err := svc.CreateMemory(ctx, CreateMemoryParams{
		UserID: user, Scope: "project", ProjectName: &project, Category: "other", Content: "项目级记忆",
	}); err != nil {
		t.Fatalf("seed project memory: %v", err)
	}

	exported, err := svc.ExportMemories(ctx, user, &project)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(exported["user_memories"].([]map[string]any)) != 1 ||
		len(exported["project_memories"].([]map[string]any)) != 1 {
		t.Fatalf("unexpected export: %v", exported)
	}

	// 导入载荷经 JSON 往返，模拟 HTTP 请求体解码后的形状。
	raw, err := json.Marshal(exported)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal export: %v", err)
	}

	// 重复导入应全部跳过（按内容查重）。
	result, err := svc.ImportMemories(ctx, user, payload, &project)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result["imported"] != 0 || result["skipped"] != 2 {
		t.Fatalf("want imported=0 skipped=2, got %v", result)
	}
}

func TestMemoryImportProjectRequiresScope(t *testing.T) {
	ctx := context.Background()
	svc := NewMemoryService(newServiceTestDB(t))
	payload := map[string]any{
		"project_memories": []any{map[string]any{"category": "other", "content": "x"}},
	}
	_, err := svc.ImportMemories(ctx, repository.DefaultUserID, payload, nil)
	if code := memoryValidationCode(t, err); code != "memory_project_required" {
		t.Fatalf("want memory_project_required, got %s", code)
	}
}

func TestMemoryListCandidatesRequiresScopeForProject(t *testing.T) {
	ctx := context.Background()
	svc := NewMemoryService(newServiceTestDB(t))
	project := "demo"
	_, err := svc.ListCandidates(ctx, repository.DefaultUserID, nil, &project)
	if code := memoryValidationCode(t, err); code != "memory_scope_required" {
		t.Fatalf("want memory_scope_required, got %s", code)
	}
}
