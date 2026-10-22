package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/repository"
)

func TestExistingReferenceImagesSkipsMissingAndEscapes(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "products"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "products", "a.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := domain.NewAssetEntry()
	entry.SetList("reference_images", []string{"products/a.png", "products/missing.png", "../escape.png"})

	refs := existingReferenceImages(dir, entry)
	if len(refs) != 1 || filepath.Base(refs[0].Path) != "a.png" {
		t.Fatalf("refs = %+v", refs)
	}
	if existingReferenceImages(dir, domain.NewAssetEntry()) != nil {
		t.Fatal("no reference_images must yield nil (text-only generation)")
	}
}

func TestDesignTaskRequiresPromptBeforeTouchingStore(t *testing.T) {
	d := NewTaskDispatcher(nil, nil)
	_, err := d.Execute(context.Background(), &repository.TaskData{
		ProjectName: "p", TaskType: "scene", ResourceID: "客厅", Payload: map[string]any{},
	})
	if err == nil || !strings.Contains(err.Error(), "prompt is required") {
		t.Fatalf("expected prompt-required error, got %v", err)
	}
}
