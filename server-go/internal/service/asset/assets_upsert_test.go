package asset

import "github.com/shotwise/shotwise/internal/service"

import (
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
)

// Upsert 不存在则新建，存在则合并改字段；sheet 字段由调用方（工具层）过滤，这里只验证落盘语义。
func TestAssetUpsertCreatesThenMerges(t *testing.T) {
	root := t.TempDir()
	store := service.NewProjectStore(root)
	if _, err := store.Create("p1", "测试项目", domain.ContentNarration, domain.GenerationStoryboard, "水墨"); err != nil {
		t.Fatal(err)
	}
	assets := NewAssetService(store)

	created, err := assets.Upsert("p1", domain.AssetCharacter, "阿明", map[string]any{"description": "少年，黑发"})
	if err != nil || !created {
		t.Fatalf("first upsert: created=%v err=%v", created, err)
	}
	created, err = assets.Upsert("p1", domain.AssetCharacter, "阿明", map[string]any{"description": "少年，白发"})
	if err != nil || created {
		t.Fatalf("second upsert must merge: created=%v err=%v", created, err)
	}

	pf := &domain.ProjectFile{}
	if err := store.Update("p1", func(p *domain.ProjectFile) error { *pf = *p; return nil }); err != nil {
		t.Fatal(err)
	}
	entry := pf.Characters["阿明"]
	if entry == nil || entry.Description() != "少年，白发" {
		t.Fatalf("merged description not persisted: %+v", entry)
	}
}

// 新建条目缺描述时写盘守卫拒绝（不写坏 project.json）。
func TestAssetUpsertRejectsMissingDescriptionOnCreate(t *testing.T) {
	root := t.TempDir()
	store := service.NewProjectStore(root)
	if _, err := store.Create("p1", "测试项目", domain.ContentNarration, domain.GenerationStoryboard, "水墨"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAssetService(store).Upsert("p1", domain.AssetScene, "客厅", map[string]any{}); err == nil {
		t.Fatal("create without description must be rejected by the write guard")
	}
}
