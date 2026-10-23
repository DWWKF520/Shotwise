package generation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	domaingrid "github.com/shotwise/shotwise/internal/domain/grid"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
)

func TestExecuteGridWritesRecordAndVersion(t *testing.T) {
	dispatcher, store, projectName := newDispatcherFixture(t, 200)
	dir, err := store.Dir(projectName)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := service.NewGridManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	grid := domaingrid.NewGridGeneration(1, "episode_1.json", []string{"E1S1"}, 2, 2, "grid_4", "", "", "16:9", nil)
	if err := manager.Save(grid); err != nil {
		t.Fatal(err)
	}

	result, err := dispatcher.Execute(context.Background(), &repository.TaskData{
		TaskID: "task-grid", ProjectName: projectName, TaskType: "grid", MediaType: "image",
		ResourceID: grid.ID, Payload: map[string]any{"prompt": "宫格联合图"}, UserID: repository.DefaultUserID,
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result["resource_type"] != "grids" || result["resource_id"] != grid.ID {
		t.Fatalf("结果契约有误: %+v", result)
	}
	if result["file_path"] != "grids/"+grid.ID+".png" {
		t.Fatalf("产物路径有误: %v", result["file_path"])
	}
	if result["created_at"] != grid.CreatedAt {
		t.Fatalf("created_at 应取记录值: %v", result["created_at"])
	}
	if _, err := os.Stat(filepath.Join(dir, "grids", grid.ID+".png")); err != nil {
		t.Fatalf("联合图未落盘: %v", err)
	}

	reloaded, ok, err := manager.Get(grid.ID)
	if err != nil || !ok {
		t.Fatalf("读记录失败: %v %v", ok, err)
	}
	if reloaded.Status != domaingrid.GridStatusCompleted || reloaded.SplitAt != nil {
		t.Fatalf("完成后记录状态有误: %+v", reloaded)
	}
	if reloaded.Provider == "" || reloaded.Model == "" {
		t.Fatalf("身份应回填: %+v", reloaded)
	}
}
