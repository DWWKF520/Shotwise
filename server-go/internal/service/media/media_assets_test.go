package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestMediaCatalogRegisterListBindDelete(t *testing.T) {
	t.Setenv("SHOTWISE_MEDIA_ASSET_INDEX", "1")
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "images", "a.png"), "hello")

	asset, err := RegisterMediaAsset(RegisterMediaAssetParams{
		ProjectID: "p1", ProjectRoot: root, RelativePath: "images/a.png", Origin: MediaOriginGenerated,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if asset == nil {
		t.Fatal("expected asset")
	}
	if asset.Kind != MediaKindImage {
		t.Errorf("kind = %q, want image", asset.Kind)
	}
	if asset.MimeType == nil || *asset.MimeType != "image/png" {
		t.Errorf("mime = %v, want image/png", asset.MimeType)
	}
	if asset.SizeBytes != int64(len("hello")) {
		t.Errorf("size = %d, want 5", asset.SizeBytes)
	}

	// 重复登记同一路径同内容应返回既有条目。
	again, err := RegisterMediaAsset(RegisterMediaAssetParams{
		ProjectID: "p1", ProjectRoot: root, RelativePath: "images/a.png", Origin: MediaOriginGenerated,
	})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if again.ID != asset.ID {
		t.Errorf("re-register id = %q, want %q", again.ID, asset.ID)
	}

	projectID := "p1"
	listed, err := ListProjectMediaAssets(ListProjectMediaAssetsParams{ProjectRoot: root, ProjectID: &projectID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if listed["count"].(int) != 1 {
		t.Fatalf("count = %v, want 1", listed["count"])
	}

	binding, err := BindProjectMediaAsset(root, "p1", asset.ID, "project", nil, "cover")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if binding["purpose"] != "cover" {
		t.Errorf("binding purpose = %v", binding["purpose"])
	}

	// 有绑定引用时不允许删除。
	if _, err := DeleteProjectMediaAsset(root, asset.ID); err == nil {
		t.Fatal("expected referenced error")
	} else {
		var refErr *MediaAssetReferencedError
		if !errors.As(err, &refErr) {
			t.Fatalf("err = %v, want MediaAssetReferencedError", err)
		}
	}

	// 绑定过滤：不匹配的 kind 应过滤掉该资产。
	other := "character"
	filtered, err := ListProjectMediaAssets(ListProjectMediaAssetsParams{
		ProjectRoot: root, ProjectID: &projectID, BindingKind: &other,
	})
	if err != nil {
		t.Fatalf("list filtered: %v", err)
	}
	if filtered["count"].(int) != 0 {
		t.Errorf("filtered count = %v, want 0", filtered["count"])
	}
}

func TestMediaCatalogArchiveAndGet(t *testing.T) {
	t.Setenv("SHOTWISE_MEDIA_ASSET_INDEX", "1")
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a.mp4"), "video")

	asset, err := RegisterMediaAsset(RegisterMediaAssetParams{
		ProjectID: "p1", ProjectRoot: root, RelativePath: "a.mp4", Origin: MediaOriginGenerated,
	})
	if err != nil || asset == nil {
		t.Fatalf("register: %v asset=%v", err, asset)
	}

	archived, err := ArchiveProjectMediaAsset(root, asset.ID, true)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if archived["archived"] != true {
		t.Errorf("archived = %v, want true", archived["archived"])
	}

	got, err := GetProjectMediaAsset(root, asset.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got["bindings"] == nil || got["derivations"] == nil || got["reconciliation"] == nil {
		t.Errorf("get should include nested collections: %v", got)
	}

	if _, err := GetProjectMediaAsset(root, "missing"); err == nil {
		t.Fatal("expected not found error")
	} else {
		var notFound *MediaAssetNotFoundError
		if !errors.As(err, &notFound) {
			t.Fatalf("err = %v, want MediaAssetNotFoundError", err)
		}
	}
}

func TestMediaCatalogDisabledIsNoop(t *testing.T) {
	t.Setenv("SHOTWISE_MEDIA_ASSET_INDEX", "0")
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a.png"), "x")

	asset, err := RegisterMediaAsset(RegisterMediaAssetParams{
		ProjectID: "p1", ProjectRoot: root, RelativePath: "a.png", Origin: MediaOriginUpload,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if asset != nil {
		t.Fatalf("expected nil asset when disabled, got %v", asset)
	}
	// 关闭时不应写出索引文件。
	if _, statErr := os.Stat(filepath.Join(root, ".media-assets.json")); !os.IsNotExist(statErr) {
		t.Errorf("index file should not exist, statErr=%v", statErr)
	}
}

func TestMediaRejectsEscapingRelativePath(t *testing.T) {
	t.Setenv("SHOTWISE_MEDIA_ASSET_INDEX", "1")
	root := t.TempDir()
	_, err := RegisterMediaAsset(RegisterMediaAssetParams{
		ProjectID: "p1", ProjectRoot: root, RelativePath: "../outside.png", Origin: MediaOriginUpload,
	})
	if err == nil {
		t.Fatal("expected ValueError for escaping path")
	}
	var valueErr *ValueError
	if !errors.As(err, &valueErr) {
		t.Fatalf("err = %v, want ValueError", err)
	}
}

func TestScanProjectMediaAssets(t *testing.T) {
	t.Setenv("SHOTWISE_MEDIA_ASSET_INDEX", "1")
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "uploads", "u.png"), "u")
	writeTestFile(t, filepath.Join(root, "generated", "g.mp4"), "g")
	writeTestFile(t, filepath.Join(root, "notes", "readme.txt"), "skip")
	writeTestFile(t, filepath.Join(root, "images", "legacy.png"), "legacy")
	writeTestFile(t, filepath.Join(root, "project.json"), `{"style_image": "images/legacy.png"}`)

	audit, err := ScanProjectMediaAssets("p1", root, true)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if audit["dry_run"] != true {
		t.Errorf("dry_run = %v, want true", audit["dry_run"])
	}
	if audit["would_index_count"].(int) != 3 {
		t.Errorf("would_index_count = %v, want 3", audit["would_index_count"])
	}

	result, err := ScanProjectMediaAssets("p1", root, false)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if result["asset_count"].(int) != 3 {
		t.Errorf("asset_count = %v, want 3", result["asset_count"])
	}
	summary, ok := result["summary"].(map[string]any)
	if !ok || summary["status"] != MediaIndexStatusReady {
		t.Errorf("summary = %v, want status ready", result["summary"])
	}
	// 扫描后索引文件应落盘。
	if _, statErr := os.Stat(filepath.Join(root, ".media-assets.json")); statErr != nil {
		t.Errorf("index file missing: %v", statErr)
	}
}

func TestDeleteProjectMediaAssetDetectsLegacyReference(t *testing.T) {
	t.Setenv("SHOTWISE_MEDIA_ASSET_INDEX", "1")
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "images", "x.png"), "x")
	writeTestFile(t, filepath.Join(root, "project.json"), `{"style_image": "images/x.png"}`)

	asset, err := RegisterMediaAsset(RegisterMediaAssetParams{
		ProjectID: "p1", ProjectRoot: root, RelativePath: "images/x.png", Origin: MediaOriginGenerated,
	})
	if err != nil || asset == nil {
		t.Fatalf("register: %v asset=%v", err, asset)
	}
	_, err = DeleteProjectMediaAsset(root, asset.ID)
	if err == nil {
		t.Fatal("expected referenced error from legacy reference")
	}
	var refErr *MediaAssetReferencedError
	if !errors.As(err, &refErr) {
		t.Fatalf("err = %v, want MediaAssetReferencedError", err)
	}
}

func TestClassifyMediaPath(t *testing.T) {
	cases := map[string]string{
		"a.png": MediaKindImage, "b.MP4": MediaKindVideo, "c.wav": MediaKindAudio, "d.txt": "",
	}
	for path, want := range cases {
		if got := ClassifyMediaPath(path); got != want {
			t.Errorf("ClassifyMediaPath(%q) = %q, want %q", path, got, want)
		}
	}
}
