package imageedit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/repository"
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
		Result: map[string]any{"file_path": name + ".png", "version": 2},
	}, nil
}

func (f *fakeAssetQueue) IsWorkerOnline(context.Context, string) (bool, error) { return true, nil }

// fakeGenerationProjects 提供生成类工具所需的项目读取面。
type fakeGenerationProjects struct {
	pf      *domain.ProjectFile
	dir     string
	scripts map[string]map[string]any
}

func (f *fakeGenerationProjects) Get(string) (*domain.ProjectFile, error) { return f.pf, nil }
func (f *fakeGenerationProjects) Dir(string) (string, error)              { return f.dir, nil }

func (f *fakeGenerationProjects) ReadScriptJSON(_, filename string) (map[string]any, error) {
	script, ok := f.scripts[filename]
	if !ok {
		return nil, fs.ErrNotFound
	}
	return script, nil
}

// newGenerationFixture 造一个 narration 项目：两个片段（E1S1 已有图、E1S2 缺图），
// 一个已登记且已出设计图的角色，用于通过 preflight。
func newGenerationFixture(t *testing.T) (*fakeAssetQueue, *fakeGenerationProjects) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "characters"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "characters", "阿明.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteJSONAtomic(filepath.Join(dir, "project.json"), map[string]any{
		"title": "测试", "content_mode": "narration", "generation_mode": "storyboard",
		"style": "画风：写实", "style_description": "柔和光线",
	}); err != nil {
		t.Fatal(err)
	}

	entry := domain.NewAssetEntry()
	entry.SetString("description", "少年")
	entry.SetString("character_sheet", "characters/阿明.png")

	script := map[string]any{
		"title": "第1集", "content_mode": "narration", "episode": float64(1),
		"segments": []any{
			map[string]any{
				"segment_id": "E1S1", "duration_seconds": float64(4), "novel_text": "第一段原文",
				"characters_in_segment": []any{"阿明"},
				"image_prompt":          "纯字符串提示词",
				"generated_assets":      map[string]any{"storyboard_image": "storyboards/scene_E1S1.png"},
			},
			map[string]any{
				"segment_id": "E1S2", "duration_seconds": float64(4), "novel_text": "  ",
				"characters_in_segment": []any{"阿明"},
				"image_prompt": map[string]any{
					"scene":       "桥上的两人",
					"composition": map[string]any{"shot_type": "Medium Shot", "lighting": "暖", "ambiance": "静"},
				},
			},
		},
	}
	return &fakeAssetQueue{}, &fakeGenerationProjects{
		pf: &domain.ProjectFile{
			ContentMode: "narration", GenerationMode: "storyboard", Style: "写实",
			Characters: domain.AssetBucket{"阿明": entry},
		},
		dir:     dir,
		scripts: map[string]map[string]any{"episode_1.json": script},
	}
}
