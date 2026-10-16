package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/service"
)

// fakeMemoryStore 记录候选记忆创建入参并返回固定 id；复用真实的校验原语（与
// service.MemoryService 落库前同一套），使工具级测试覆盖到拒绝分支。
type fakeMemoryStore struct {
	called bool
	params service.CreateMemoryCandidateParams
}

func (f *fakeMemoryStore) CreateMemoryCandidate(
	_ context.Context, p service.CreateMemoryCandidateParams,
) (string, error) {
	projectName := ""
	if p.ProjectName != nil {
		projectName = *p.ProjectName
	}
	if _, _, err := service.ValidateMemoryScope(p.Scope, projectName); err != nil {
		return "", err
	}
	if _, err := service.ValidateMemoryCategory(p.Category); err != nil {
		return "", err
	}
	if _, err := service.SanitizeMemoryContent(p.Content); err != nil {
		return "", err
	}
	f.called = true
	f.params = p
	return "cand-1", nil
}

func TestProposeMemoryCandidateTool(t *testing.T) {
	store := &fakeMemoryStore{}
	tl, err := newProposeMemoryCandidateTool("proj-1", "sess-1", store)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.InvokableRun(context.Background(), `{"scope":"project","category":"style","content":"偏好竖屏特写"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "记忆候选已创建") {
		t.Fatalf("unexpected result: %s", res)
	}
	if !store.called {
		t.Fatal("store should be called")
	}
	if store.params.ProjectName == nil || *store.params.ProjectName != "proj-1" {
		t.Fatalf("project scope should carry project name: %v", store.params.ProjectName)
	}
	if store.params.Scope != "project" || store.params.Category != "style" {
		t.Fatalf("unexpected params: %+v", store.params)
	}
}

func TestProposeMemoryCandidateToolRejectsSecrets(t *testing.T) {
	store := &fakeMemoryStore{}
	tl, err := newProposeMemoryCandidateTool("proj-1", "sess-1", store)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.InvokableRun(context.Background(), `{"scope":"user","category":"other","content":"api_key: abcdef"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "记忆候选被拒") {
		t.Fatalf("secret content should be rejected: %s", res)
	}
	if store.called {
		t.Fatal("store should not be called on rejection")
	}
}
