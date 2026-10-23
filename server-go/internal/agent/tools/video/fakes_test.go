package video

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/repository"
	servicescript "github.com/shotwise/shotwise/internal/service/script"
)

// 与 assets / 生成类测试共享的最小替身副本（Go 测试不能跨包复用未导出类型）。

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
		Result: map[string]any{"file_path": "videos/" + name + ".mp4", "version": 2},
	}, nil
}

func (f *fakeAssetQueue) IsWorkerOnline(context.Context, string) (bool, error) { return true, nil }

type fakeAssetProjects struct {
	pf  *project.ProjectFile
	dir string
}

func (f *fakeAssetProjects) Get(string) (*project.ProjectFile, error) { return f.pf, nil }
func (f *fakeAssetProjects) Dir(string) (string, error)               { return f.dir, nil }

// fakeGenerationProjects 提供生成类工具所需的项目读取面。
type fakeGenerationProjects struct {
	pf      *project.ProjectFile
	dir     string
	scripts map[string]map[string]any
}

func (f *fakeGenerationProjects) Get(string) (*project.ProjectFile, error) { return f.pf, nil }
func (f *fakeGenerationProjects) Dir(string) (string, error)               { return f.dir, nil }

func (f *fakeGenerationProjects) ReadScriptJSON(_, filename string) (map[string]any, error) {
	script, ok := f.scripts[filename]
	if !ok {
		return nil, fs.ErrNotFound
	}
	return script, nil
}

// SaveScript 满足 tools.TextGenerationProjects（剧本落盘写入项目 scripts/ 目录，与
// script 包测试的共享替身同口径）。
func (f *fakeGenerationProjects) SaveScript(_, scriptFilename string, script map[string]any) error {
	scriptsDir := filepath.Join(f.dir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(script, "", "  ")
	if err != nil {
		return err
	}
	if f.scripts == nil {
		f.scripts = map[string]map[string]any{}
	}
	f.scripts[scriptFilename] = script
	return os.WriteFile(filepath.Join(scriptsDir, scriptFilename), raw, 0o644)
}

// Update 就地 mutate 项目（TextGenerationProjects 的 step1 存量迁移要求）。
func (f *fakeGenerationProjects) Update(_ string, mutate func(*project.ProjectFile) error) error {
	return mutate(f.pf)
}

// fakeTextCompletion 按序返回预置输出。
type fakeTextCompletion struct {
	outputs []string
	calls   int
}

func (f *fakeTextCompletion) Generate(_ context.Context, _ string, _ map[string]any, _ int) (string, error) {
	f.calls++
	if f.calls <= len(f.outputs) {
		return f.outputs[f.calls-1], nil
	}
	return "", errors.New("no more outputs")
}

// newTextGenProject 造一个含 source/ 与 scripts/ 目录的项目（与 script 包测试同口径）。
func newTextGenProject(t *testing.T, contentMode string, pf *project.ProjectFile) (*fakeGenerationProjects, string) {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"source", "scripts"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pf.ContentMode = contentMode
	pf.Style = "写实"
	pf.Title = "测试项目"
	return &fakeGenerationProjects{pf: pf, dir: dir}, dir
}

// staticGenFactory 构造固定输出的文本生成工厂。
func staticGenFactory(out string) servicescript.TextCompletionFactory {
	return func(context.Context, *project.ProjectFile) (servicescript.TextCompletion, error) {
		return &fakeTextCompletion{outputs: []string{out}}, nil
	}
}
