package assets

import "github.com/shotwise/shotwise/internal/agent/tools"

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/repository"
)

// fakeAssetQueue 记录入队请求，并让每个任务按 outcomes 给出终态（缺省成功）。
type fakeAssetQueue struct {
	enqueued []repository.EnqueueParams
	failNext bool
}

func (f *fakeAssetQueue) EnqueueTask(_ context.Context, p repository.EnqueueParams) (repository.EnqueueResult, error) {
	f.enqueued = append(f.enqueued, p)
	return repository.EnqueueResult{TaskID: "task-" + p.ResourceID, Status: "queued"}, nil
}

func (f *fakeAssetQueue) GetTask(_ context.Context, id string) (*repository.TaskData, error) {
	name := strings.TrimPrefix(id, "task-")
	if f.failNext {
		msg := "provider rejected"
		return &repository.TaskData{TaskID: id, Status: repository.TaskStatusFailed, ErrorMessage: &msg}, nil
	}
	return &repository.TaskData{
		TaskID: id,
		Status: repository.TaskStatusSucceeded,
		Result: map[string]any{"file_path": "characters/" + name + ".png", "version": 2},
	}, nil
}

func (f *fakeAssetQueue) IsWorkerOnline(context.Context, string) (bool, error) { return true, nil }

type fakeAssetProjects struct {
	pf  *domain.ProjectFile
	dir string
}

func (f *fakeAssetProjects) Get(string) (*domain.ProjectFile, error) { return f.pf, nil }
func (f *fakeAssetProjects) Dir(string) (string, error)              { return f.dir, nil }

func newAssetFixture(t *testing.T) (*fakeAssetProjects, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "characters"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 小红已有设计图文件；阿明无 sheet；空描述的角色用于验证警告。
	if err := os.WriteFile(filepath.Join(dir, "characters", "小红.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	mk := func(desc string, sheet string) *domain.AssetEntry {
		e := domain.NewAssetEntry()
		e.SetString("description", desc)
		if sheet != "" {
			e.SetString("character_sheet", sheet)
		}
		return e
	}
	pf := &domain.ProjectFile{
		ContentMode: "drama",
		Characters: domain.AssetBucket{
			"阿明": mk("少年，黑发", ""),
			"小红": mk("少女，红裙", "characters/小红.png"),
			"空白": mk("   ", ""),
		},
	}
	return &fakeAssetProjects{pf: pf, dir: dir}, dir
}

func TestGenerateAssetsPendingModeEnqueuesOnlyMissingSheets(t *testing.T) {
	projects, _ := newAssetFixture(t)
	q := &fakeAssetQueue{}

	text, failed, err := runGenerateAssets(context.Background(), "proj",
		[]domain.AssetKind{domain.AssetCharacter}, nil, false, q, projects)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("failed = %d, text=%s", failed, text)
	}
	if len(q.enqueued) != 1 || q.enqueued[0].ResourceID != "阿明" {
		t.Fatalf("expected only 阿明 enqueued, got %+v", q.enqueued)
	}
	if q.enqueued[0].TaskType != "character" || q.enqueued[0].MediaType != "image" {
		t.Fatalf("unexpected task identity: %+v", q.enqueued[0])
	}
	if got := q.enqueued[0].Payload["prompt"]; got != "少年，黑发" {
		t.Fatalf("prompt payload = %v", got)
	}
	if !strings.Contains(text, "缺少描述") || !strings.Contains(text, "阿明") {
		t.Fatalf("summary should warn about blank description and report success: %s", text)
	}
}

func TestGenerateAssetsNamesModeSkipsExistingSheetUnlessRegenerate(t *testing.T) {
	projects, _ := newAssetFixture(t)
	q := &fakeAssetQueue{}

	text, _, err := runGenerateAssets(context.Background(), "proj",
		[]domain.AssetKind{domain.AssetCharacter}, []string{"小红", "不存在"}, false, q, projects)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 0 {
		t.Fatalf("existing sheet must be skipped, enqueued=%+v", q.enqueued)
	}
	if !strings.Contains(text, "已有设计图") || !strings.Contains(text, "不存在于 project.json") {
		t.Fatalf("expected skip + not-found warnings: %s", text)
	}

	if _, _, err := runGenerateAssets(context.Background(), "proj",
		[]domain.AssetKind{domain.AssetCharacter}, []string{"小红"}, true, q, projects); err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 1 || q.enqueued[0].ResourceID != "小红" {
		t.Fatalf("regenerate=true must enqueue 小红, got %+v", q.enqueued)
	}
}

func TestGenerateAssetsReportsFailureAsErrorEnvelope(t *testing.T) {
	projects, _ := newAssetFixture(t)
	tl, err := NewGenerateAssetsTool("proj", &fakeAssetQueue{failNext: true}, projects)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"type":"character","names":["阿明"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"is_error":true`) || !strings.Contains(out, "provider rejected") {
		t.Fatalf("failed task must surface as is_error with message: %s", out)
	}
}

func TestGenerateAssetsValidatesArguments(t *testing.T) {
	projects, _ := newAssetFixture(t)
	tl, err := NewGenerateAssetsTool("proj", &fakeAssetQueue{}, projects)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		`{"names":["阿明"]}`:                               "names 必须配合 type",
		`{"type":"character","names":["阿明"],"all":true}`: "互斥",
		`{"type":"weapon"}`:                              "type 必须是",
	}
	for args, want := range cases {
		out, err := tl.InvokableRun(context.Background(), args)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, want) || !strings.Contains(out, `"is_error":true`) {
			t.Errorf("args %s: want deny containing %q, got %s", args, want, out)
		}
	}
}

func TestListPendingAssetsReportsOnlyMissingSheets(t *testing.T) {
	projects, _ := newAssetFixture(t)
	tl, err := NewListPendingAssetsTool("proj", projects)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"type":"character"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "阿明") || !strings.Contains(out, "空白") || strings.Contains(out, "小红") {
		t.Fatalf("pending list wrong (小红 has sheet, must be excluded): %s", out)
	}
	if !strings.Contains(out, "少年，黑发") {
		t.Fatalf("pending entry should carry description preview: %s", out)
	}

	out, _ = tl.InvokableRun(context.Background(), `{"type":"scene"}`)
	if !strings.Contains(out, "所有场景都已有设计图") {
		t.Fatalf("empty bucket should report all done: %s", out)
	}

	out, _ = tl.InvokableRun(context.Background(), `{"type":"weapon"}`)
	if !strings.Contains(out, `"is_error":true`) {
		t.Fatalf("invalid type must be denied: %s", out)
	}
}

func TestDedupStrings(t *testing.T) {
	got := tools.DedupStrings([]string{"a", "b", "a", "c", "b"})
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("dedup = %v", got)
	}
}
