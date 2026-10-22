package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/migrations"
	"gorm.io/gorm/logger"
)

// globalAssetFixture 造一个项目资产 + 全局库服务。
type globalAssetFixture struct {
	svc   *GlobalAssetService
	store *ProjectStore
}

func newGlobalAssetFixture(t *testing.T) globalAssetFixture {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "t.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Migrate(gdb, dialect, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if pool, err := gdb.DB(); err == nil {
			_ = pool.Close()
		}
	})
	store := NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "测试项目", domain.ContentNarration, domain.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	// 落一张场景 sheet 并登记进桶。
	dir, err := store.Dir("p1")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "scenes"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scenes", "客厅.png"), []byte("png-bytes"), 0o644); err != nil {
		t.Fatalf("write sheet: %v", err)
	}
	if err := store.Update("p1", func(pf *domain.ProjectFile) error {
		if pf.Scenes == nil {
			pf.Scenes = domain.AssetBucket{}
		}
		pf.Scenes["客厅"] = domain.WrapAssetEntry(map[string]any{
			"scene_sheet":  "scenes/客厅.png",
			"description":  "测试场景",
		})
		return nil
	}); err != nil {
		t.Fatalf("seed scene: %v", err)
	}
	repo := repository.NewGlobalAssetRepository(gdb)
	// 根目录里建 _global_assets（globalAssetsDir 依赖 store.Root）。
	if err := os.MkdirAll(filepath.Join(store.Root(), "_global_assets", "scene"), 0o755); err != nil {
		t.Fatalf("mkdir global: %v", err)
	}
	return globalAssetFixture{svc: NewGlobalAssetService(repo, store), store: store}
}

// TestFromProjectImportsSheet 校验入库：sheet 拷进 _global_assets、DB 行带源项目。
func TestFromProjectImportsSheet(t *testing.T) {
	f := newGlobalAssetFixture(t)
	out, err := f.svc.FromProject(context.Background(), FromProjectInput{
		ProjectName:  "p1",
		ResourceType: "scene",
		ResourceID:   "客厅",
	})
	if err != nil {
		t.Fatalf("from project: %v", err)
	}
	if out.Name != "客厅" || out.Description != "测试场景" {
		t.Fatalf("asset = %+v", out)
	}
	if out.ImagePath == nil || !fileExistsAt(filepath.Join(f.store.Root(), *out.ImagePath)) {
		t.Fatalf("sheet 应拷入全局库: %+v", out)
	}
	if out.SourceProject == nil || *out.SourceProject != "p1" {
		t.Fatalf("source_project = %+v", out)
	}
}

// TestFromProjectConflictWithoutOverwrite 校验同名冲突：不带 overwrite 报冲突且带既有资产。
func TestFromProjectConflictWithoutOverwrite(t *testing.T) {
	f := newGlobalAssetFixture(t)
	if _, err := f.svc.FromProject(context.Background(), FromProjectInput{
		ProjectName: "p1", ResourceType: "scene", ResourceID: "客厅",
	}); err != nil {
		t.Fatalf("first import: %v", err)
	}
	_, err := f.svc.FromProject(context.Background(), FromProjectInput{
		ProjectName: "p1", ResourceType: "scene", ResourceID: "客厅",
	})
	var conflict *GlobalAssetConflictError
	if err == nil || !asGlobalConflict(err, &conflict) {
		t.Fatalf("应报冲突: %v", err)
	}
	if conflict.Existing == nil {
		t.Fatal("冲突应带既有资产")
	}
}

// asGlobalConflict 是 errors.As 的本包内简写。
func asGlobalConflict(err error, target **GlobalAssetConflictError) bool {
	if c, ok := err.(*GlobalAssetConflictError); ok {
		*target = c
		return true
	}
	return false
}

// TestApplyToProjectWritesBucket 校验应用到项目：sheet 拷进项目桶、条目带描述。
func TestApplyToProjectWritesBucket(t *testing.T) {
	f := newGlobalAssetFixture(t)
	created, err := f.svc.FromProject(context.Background(), FromProjectInput{
		ProjectName: "p1", ResourceType: "scene", ResourceID: "客厅",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// 目标项目 p2
	if _, err := f.store.Create("p2", "目标项目", domain.ContentNarration, domain.GenerationStoryboard, ""); err != nil {
		t.Fatalf("create p2: %v", err)
	}
	result, err := f.svc.ApplyToProject(context.Background(), ApplyToProjectInput{
		AssetIDs:       []string{created.ID},
		TargetProject:  "p2",
		ConflictPolicy: "skip",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.Succeeded) != 1 || result.Succeeded[0]["name"] != "客厅" {
		t.Fatalf("result = %+v", result)
	}
	pf, err := f.store.Get("p2")
	if err != nil {
		t.Fatalf("get p2: %v", err)
	}
	entry, ok := pf.Scenes["客厅"]
	if !ok || entry.MustString("scene_sheet") == "" {
		t.Fatalf("p2 应有场景条目: %+v", pf.Scenes)
	}
	if entry.MustString("description") != "测试场景" {
		t.Fatalf("描述未带过去: %+v", entry)
	}
}

// TestApplyToProjectRenamePolicy 校验 rename 策略：同名自动加后缀。
func TestApplyToProjectRenamePolicy(t *testing.T) {
	f := newGlobalAssetFixture(t)
	created, err := f.svc.FromProject(context.Background(), FromProjectInput{
		ProjectName: "p1", ResourceType: "scene", ResourceID: "客厅",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// 目标项目里先有一个同名场景。
	if _, err := f.store.Create("p2", "目标项目", domain.ContentNarration, domain.GenerationStoryboard, ""); err != nil {
		t.Fatalf("create p2: %v", err)
	}
	if err := f.store.Update("p2", func(pf *domain.ProjectFile) error {
		if pf.Scenes == nil {
			pf.Scenes = domain.AssetBucket{}
		}
		pf.Scenes["客厅"] = domain.WrapAssetEntry(map[string]any{"description": "已有的"})
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	result, err := f.svc.ApplyToProject(context.Background(), ApplyToProjectInput{
		AssetIDs: []string{created.ID}, TargetProject: "p2", ConflictPolicy: "rename",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.Succeeded) != 1 || result.Succeeded[0]["name"] != "客厅 (2)" {
		t.Fatalf("result = %+v", result)
	}
}

// TestApplyToProjectSkipPolicy 校验 skip 策略：同名跳过不写。
func TestApplyToProjectSkipPolicy(t *testing.T) {
	f := newGlobalAssetFixture(t)
	created, err := f.svc.FromProject(context.Background(), FromProjectInput{
		ProjectName: "p1", ResourceType: "scene", ResourceID: "客厅",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := f.store.Create("p2", "目标项目", domain.ContentNarration, domain.GenerationStoryboard, ""); err != nil {
		t.Fatalf("create p2: %v", err)
	}
	if err := f.store.Update("p2", func(pf *domain.ProjectFile) error {
		if pf.Scenes == nil {
			pf.Scenes = domain.AssetBucket{}
		}
		pf.Scenes["客厅"] = domain.WrapAssetEntry(map[string]any{"description": "已有的"})
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	result, err := f.svc.ApplyToProject(context.Background(), ApplyToProjectInput{
		AssetIDs: []string{created.ID}, TargetProject: "p2", ConflictPolicy: "skip",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.Skipped) != 1 || len(result.Succeeded) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

// fileExistsAt 判定文件存在。
func fileExistsAt(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
