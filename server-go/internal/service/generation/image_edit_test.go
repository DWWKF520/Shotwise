package generation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service/asset"
)

func TestEditableResourceTypesOrder(t *testing.T) {
	want := []string{"character", "scene", "prop", "product", "storyboard"}
	if len(EditableResourceTypes) != len(want) {
		t.Fatalf("可编辑类型数量不符: %v", EditableResourceTypes)
	}
	for i, name := range want {
		if EditableResourceTypes[i] != name {
			t.Fatalf("顺序不符: %v", EditableResourceTypes)
		}
	}
	if !IsEditableResourceType("storyboard") || IsEditableResourceType("clue") {
		t.Fatal("白名单判定有误")
	}
}

func TestEditVersionResourceTypeMapping(t *testing.T) {
	cases := map[string]string{
		"character":  "characters",
		"scene":      "scenes",
		"prop":       "props",
		"product":    "products",
		"storyboard": "storyboards",
	}
	for input, want := range cases {
		got, ok := EditVersionResourceType(input)
		if !ok || got != want {
			t.Fatalf("%s → %q,%v; want %q", input, got, ok, want)
		}
	}
	if _, ok := EditVersionResourceType("clue"); ok {
		t.Fatal("未登记类型不应映射成功")
	}
}

func TestResolveCurrentImageRelAssetAndStoryboard(t *testing.T) {
	entry := domain.NewAssetEntry()
	entry.SetString("description", "少年")
	entry.SetString("character_sheet", "characters/阿明.png")
	pf := &domain.ProjectFile{Characters: domain.AssetBucket{"阿明": entry}}

	if got, found := ResolveCurrentImageRel(pf, "character", "阿明", nil); !found || got != "characters/阿明.png" {
		t.Fatalf("资产当前图解析有误: %q,%v", got, found)
	}
	// 未登记资产 → found=false（对应 Python KeyError 分支）。
	if _, found := ResolveCurrentImageRel(pf, "character", "不存在", nil); found {
		t.Fatal("未登记资产不应解析成功")
	}
	// 已登记但未出图 → ("", true)：与「资源不存在」区分。
	blank := domain.NewAssetEntry()
	blank.SetString("description", "空")
	pf2 := &domain.ProjectFile{Characters: domain.AssetBucket{"空": blank}}
	if got, found := ResolveCurrentImageRel(pf2, "character", "空", nil); !found || got != "" {
		t.Fatalf("无设计图应返回空串且 found=true: %q,%v", got, found)
	}

	script := map[string]any{
		"segments": []any{
			map[string]any{
				"segment_id":       "E1S1",
				"generated_assets": map[string]any{"storyboard_image": "storyboards/scene_E1S1_first.png"},
			},
			map[string]any{"segment_id": "E1S2"},
		},
	}
	if got, found := ResolveCurrentImageRel(nil, "storyboard", "E1S1", script); !found || got != "storyboards/scene_E1S1_first.png" {
		t.Fatalf("分镜图指针解析有误: %q,%v", got, found)
	}
	// 无 storyboard_image 时回退 canonical 路径。
	if got, found := ResolveCurrentImageRel(nil, "storyboard", "E1S2", script); !found || got != "storyboards/scene_E1S2.png" {
		t.Fatalf("canonical 回退有误: %q,%v", got, found)
	}
	if _, found := ResolveCurrentImageRel(nil, "storyboard", "E9S9", script); found {
		t.Fatal("不存在的分镜不应解析成功")
	}
}

func TestResolveCurrentImageRelResolvesNFDAssetName(t *testing.T) {
	entry := domain.NewAssetEntry()
	entry.SetString("description", "x")
	entry.SetString("character_sheet", "characters/e.png")
	pf := &domain.ProjectFile{Characters: domain.AssetBucket{"\u00e9": entry}} // NFC é
	if got, found := ResolveCurrentImageRel(pf, "character", "e\u0301", nil); !found || got != "characters/e.png" {
		t.Fatalf("NFD 输入应按坐标系解析到 NFC key: %q,%v", got, found)
	}
}

// TestExecuteImageEditCharacterWritesSheetAndVersion 覆盖资产编辑端到端：写 sheet 字段、
// 登记版本、旧图补基线。
func TestExecuteImageEditCharacterWritesSheetAndVersion(t *testing.T) {
	dispatcher, store, projectName := newDispatcherFixture(t, 200)
	projectDir, err := store.Dir(projectName)
	if err != nil {
		t.Fatalf("project dir: %v", err)
	}
	assets := asset.NewAssetService(store)
	if _, err := assets.Create(projectName, domain.AssetCharacter, "阿明", "少年", nil); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, "characters"), 0o755); err != nil {
		t.Fatal(err)
	}
	sheet := filepath.Join(projectDir, "characters", "阿明.png")
	if err := os.WriteFile(sheet, []byte("old-image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := assets.Patch(projectName, domain.AssetCharacter, "阿明", map[string]any{
		"character_sheet": "characters/阿明.png",
	}); err != nil {
		t.Fatalf("patch sheet: %v", err)
	}

	result, err := dispatcher.Execute(context.Background(), editTask("阿明", "character", map[string]any{
		"resource_type": "character",
		"prompt":        "把发色改成红色",
	}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result["resource_type"] != "characters" || result["resource_id"] != "阿明" {
		t.Fatalf("资源身份有误: %v", result)
	}
	if result["file_path"] != "characters/阿明.png" {
		t.Fatalf("产物路径有误: %v", result["file_path"])
	}
	if v, _ := result["version"].(int); v < 2 {
		t.Fatalf("编辑应留下旧图基线与新版本两条: %v", result["version"])
	}

	raw, err := os.ReadFile(filepath.Join(projectDir, "versions", "versions.json"))
	if err != nil {
		t.Fatalf("versions.json missing: %v", err)
	}
	if !containsAll(string(raw), `"source": "image_edit"`, `"aspect_ratio": "16:9"`) {
		t.Fatalf("版本元数据缺编辑标记: %s", raw)
	}
	// 资产 sheet 字段仍指向 canonical 路径（编辑覆盖原地）。
	pf, err := store.Get(projectName)
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	entry := pf.Characters["阿明"]
	if entry == nil || entry.MustString("character_sheet") != "characters/阿明.png" {
		t.Fatalf("sheet 写回有误: %+v", entry)
	}
}

func TestExecuteImageEditRejectsBadPayload(t *testing.T) {
	dispatcher, _, _ := newDispatcherFixture(t, 200)

	if _, err := dispatcher.Execute(context.Background(), editTask("x", "clue", map[string]any{
		"resource_type": "clue", "prompt": "y",
	})); err == nil {
		t.Fatal("未知 resource_type 应 fail loud")
	}
	if _, err := dispatcher.Execute(context.Background(), editTask("x", "character", map[string]any{
		"resource_type": "character", "prompt": "   ",
	})); err == nil {
		t.Fatal("空指令应 fail loud")
	}
	if _, err := dispatcher.Execute(context.Background(), editTask("E1S1", "storyboard", map[string]any{
		"resource_type": "storyboard", "prompt": "y",
	})); err == nil {
		t.Fatal("storyboard 缺 script_file 应 fail loud")
	}
	if _, err := dispatcher.Execute(context.Background(), editTask("x", "character", map[string]any{
		"resource_type": "character", "prompt": "y", "reference_media_asset_id": "m1",
	})); err == nil {
		t.Fatal("未迁移的参考媒体资产分支应 fail loud")
	}
	if _, err := dispatcher.Execute(context.Background(), editTask("不存在", "character", map[string]any{
		"resource_type": "character", "prompt": "y",
	})); err == nil {
		t.Fatal("无当前图应 fail loud")
	}
}

func editTask(resourceID, media string, payload map[string]any) *repository.TaskData {
	return &repository.TaskData{
		TaskID:      "task-edit",
		ProjectName: "p1",
		TaskType:    "image_edit",
		MediaType:   media,
		ResourceID:  resourceID,
		Payload:     payload,
		UserID:      repository.DefaultUserID,
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	return true
}
