package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain/media"
	domainproject "github.com/shotwise/shotwise/internal/domain/project"
)

// preflightFields 是三类资产在分镜条目里的引用字段名。
func preflightFields() media.StoryboardItemFields {
	return media.StoryboardItemFields{
		CharField:  "characters_in_segment",
		SceneField: "scenes",
		PropField:  "props",
	}
}

// setupPreflightProject 造一个含各色资产的 project.json 表示：
// 阿明 / 客厅 / 长剑 有可用设计图；无图角色 设计图字段为空；丢失角色 指向不存在文件；
// 越界角色 指向项目外路径。返回的 dir 内已写入可用设计图文件。
func setupPreflightProject(t *testing.T) (*domainproject.ProjectFile, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "characters"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "scenes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "props"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"characters/阿明.png", "scenes/客厅.png", "props/长剑.png"} {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte("png"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	project := &domainproject.ProjectFile{
		Characters: domainproject.AssetBucket{
			"阿明":   domainproject.WrapAssetEntry(map[string]any{"character_sheet": "characters/阿明.png"}),
			"无图角色": domainproject.WrapAssetEntry(map[string]any{"character_sheet": ""}),
			"丢失角色": domainproject.WrapAssetEntry(map[string]any{"character_sheet": "characters/missing.png"}),
			"越界角色": domainproject.WrapAssetEntry(map[string]any{"character_sheet": "../outside.png"}),
		},
		Scenes: domainproject.AssetBucket{
			"客厅": domainproject.WrapAssetEntry(map[string]any{"scene_sheet": "scenes/客厅.png"}),
		},
		Props: domainproject.AssetBucket{
			"长剑": domainproject.WrapAssetEntry(map[string]any{"prop_sheet": "props/长剑.png"}),
		},
	}
	return project, dir
}

func TestPreflightAllPass(t *testing.T) {
	project, dir := setupPreflightProject(t)
	item := map[string]any{
		"characters_in_segment": []any{"阿明"},
		"scenes":                []any{"客厅"},
		"props":                 []any{"长剑"},
	}
	result := ValidateItemAssetReferences(project, dir, item, preflightFields())
	if len(result.Issues) != 0 {
		t.Fatalf("expected no issues, got %+v", result.Issues)
	}
	if got := FormatPreflightError(result); got != "" {
		t.Fatalf("expected empty error text, got %q", got)
	}
}

func TestPreflightUnregistered(t *testing.T) {
	project, dir := setupPreflightProject(t)
	item := map[string]any{"characters_in_segment": []any{"幽灵"}}
	result := ValidateItemAssetReferences(project, dir, item, preflightFields())
	if len(result.Issues) != 1 {
		t.Fatalf("expected 1 issue, got %+v", result.Issues)
	}
	issue := result.Issues[0]
	if issue.Kind != PreflightKindUnregistered {
		t.Errorf("kind = %q, want %q", issue.Kind, PreflightKindUnregistered)
	}
	if issue.Field != "characters_in_segment" {
		t.Errorf("field = %q", issue.Field)
	}
	if issue.AssetName != "幽灵" {
		t.Errorf("asset name = %q", issue.AssetName)
	}
	if issue.AssetType != domainproject.AssetCharacter {
		t.Errorf("asset type = %q", issue.AssetType)
	}
}

func TestPreflightMissingSheetEmpty(t *testing.T) {
	project, dir := setupPreflightProject(t)
	item := map[string]any{"characters_in_segment": []any{"无图角色"}}
	result := ValidateItemAssetReferences(project, dir, item, preflightFields())
	if len(result.Issues) != 1 || result.Issues[0].Kind != PreflightKindMissingSheet {
		t.Fatalf("expected one missing_sheet issue, got %+v", result.Issues)
	}
}

func TestPreflightMissingSheetAbsentFile(t *testing.T) {
	project, dir := setupPreflightProject(t)
	item := map[string]any{"characters_in_segment": []any{"丢失角色"}}
	result := ValidateItemAssetReferences(project, dir, item, preflightFields())
	if len(result.Issues) != 1 || result.Issues[0].Kind != PreflightKindMissingSheet {
		t.Fatalf("expected one missing_sheet issue, got %+v", result.Issues)
	}
}

func TestPreflightSheetEscapesProjectDir(t *testing.T) {
	project, dir := setupPreflightProject(t)
	item := map[string]any{"characters_in_segment": []any{"越界角色"}}
	result := ValidateItemAssetReferences(project, dir, item, preflightFields())
	if len(result.Issues) != 1 || result.Issues[0].Kind != PreflightKindMissingSheet {
		t.Fatalf("escaping sheet must be reported missing, got %+v", result.Issues)
	}
}

func TestPreflightNFCResolvesEquivalentName(t *testing.T) {
	project, dir := setupPreflightProject(t)
	// 桶 key 落 NFC（caf\u00e9），条目引用用 NFD（cafe + 组合附加符）。
	nfc := "caf\u00e9"
	nfd := "cafe\u0301"
	if err := os.WriteFile(filepath.Join(dir, "scenes", nfc+".png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	project.Scenes[nfc] = domainproject.WrapAssetEntry(map[string]any{"scene_sheet": "scenes/" + nfc + ".png"})

	if result := ValidateItemAssetReferences(project, dir, map[string]any{"scenes": []any{nfd}}, preflightFields()); len(result.Issues) != 0 {
		t.Fatalf("NFD reference must resolve to NFC key, got %+v", result.Issues)
	}

	// 反向：桶 key 落 NFD，引用用 NFC 也应命中。
	nfdKey := "role_" + nfd
	if err := os.WriteFile(filepath.Join(dir, "scenes", nfdKey+".png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	project.Scenes[nfdKey] = domainproject.WrapAssetEntry(map[string]any{"scene_sheet": "scenes/" + nfdKey + ".png"})

	if result := ValidateItemAssetReferences(project, dir, map[string]any{"scenes": []any{"role_" + nfc}}, preflightFields()); len(result.Issues) != 0 {
		t.Fatalf("NFC reference must resolve to NFD key, got %+v", result.Issues)
	}
}

func TestPreflightDedupesWithinItem(t *testing.T) {
	project, dir := setupPreflightProject(t)
	item := map[string]any{"characters_in_segment": []any{"幽灵", "幽灵"}}
	result := ValidateItemAssetReferences(project, dir, item, preflightFields())
	if len(result.Issues) != 1 {
		t.Fatalf("duplicate name must be reported once, got %+v", result.Issues)
	}
}

func TestPreflightEmptyCharFieldSkipsCharacter(t *testing.T) {
	project, dir := setupPreflightProject(t)
	fields := preflightFields()
	fields.CharField = ""
	item := map[string]any{
		"characters_in_segment": []any{"幽灵"},
		"scenes":                []any{"客厅"},
	}
	result := ValidateItemAssetReferences(project, dir, item, fields)
	if len(result.Issues) != 0 {
		t.Fatalf("empty char field must skip character references, got %+v", result.Issues)
	}
}

func TestPreflightIgnoresNonListAndNonStringValues(t *testing.T) {
	project, dir := setupPreflightProject(t)
	item := map[string]any{
		"characters_in_segment": "阿明",                   // 非列表：整段跳过
		"scenes":                []any{123, "  ", "客厅"}, // 非字符串与空白：逐元素跳过
	}
	result := ValidateItemAssetReferences(project, dir, item, preflightFields())
	if len(result.Issues) != 0 {
		t.Fatalf("malformed values must be skipped, got %+v", result.Issues)
	}
}

func TestPreflightNilProjectReportsUnregistered(t *testing.T) {
	item := map[string]any{"scenes": []any{"客厅"}}
	result := ValidateItemAssetReferences(nil, t.TempDir(), item, preflightFields())
	if len(result.Issues) != 1 || result.Issues[0].Kind != PreflightKindUnregistered {
		t.Fatalf("nil project must yield unregistered, got %+v", result.Issues)
	}
}

func TestMergePreflightResultsOrderAndDedup(t *testing.T) {
	first := PreflightResult{Issues: []PreflightIssue{
		{Field: "scenes", AssetName: "客厅", Kind: PreflightKindMissingSheet, AssetType: domainproject.AssetScene},
		{Field: "characters_in_segment", AssetName: "幽灵", Kind: PreflightKindUnregistered, AssetType: domainproject.AssetCharacter},
	}}
	second := PreflightResult{Issues: []PreflightIssue{
		// 与 first 首条同身份（种类/类型/名相同），应被去重，字段保留首次出现的值。
		{Field: "scenes", AssetName: "客厅", Kind: PreflightKindMissingSheet, AssetType: domainproject.AssetScene},
		{Field: "props", AssetName: "长剑", Kind: PreflightKindUnregistered, AssetType: domainproject.AssetProp},
	}}
	merged := MergePreflightResults(first, second)
	if len(merged.Issues) != 3 {
		t.Fatalf("expected 3 merged issues, got %+v", merged.Issues)
	}
	want := []string{"客厅", "幽灵", "长剑"}
	for i, name := range want {
		if merged.Issues[i].AssetName != name {
			t.Fatalf("merged order[%d] = %q, want %q", i, merged.Issues[i].AssetName, name)
		}
	}
}

func TestMergePreflightResultsFieldKeepsFirstSeen(t *testing.T) {
	first := PreflightResult{Issues: []PreflightIssue{
		{Field: "characters_in_segment", AssetName: "幽灵", Kind: PreflightKindUnregistered, AssetType: domainproject.AssetCharacter},
	}}
	second := PreflightResult{Issues: []PreflightIssue{
		{Field: "characters_in_scene", AssetName: "幽灵", Kind: PreflightKindUnregistered, AssetType: domainproject.AssetCharacter},
	}}
	merged := MergePreflightResults(first, second)
	if len(merged.Issues) != 1 || merged.Issues[0].Field != "characters_in_segment" {
		t.Fatalf("first-seen field must win, got %+v", merged.Issues)
	}
}

func TestFormatPreflightErrorRendering(t *testing.T) {
	result := PreflightResult{Issues: []PreflightIssue{
		{Field: "characters_in_segment", AssetName: "幽灵", Kind: PreflightKindUnregistered, AssetType: domainproject.AssetCharacter},
		{Field: "scenes", AssetName: "废弃", Kind: PreflightKindUnregistered, AssetType: domainproject.AssetScene},
		{Field: "characters_in_segment", AssetName: "无图角色", Kind: PreflightKindMissingSheet, AssetType: domainproject.AssetCharacter},
	}}
	got := FormatPreflightError(result)
	const prefix = "生成前检查失败。未登记资产："
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("unexpected prefix: %q", got)
	}
	if !strings.Contains(got, "character:幽灵, scene:废弃") {
		t.Errorf("unregistered entry format wrong: %q", got)
	}
	if !strings.Contains(got, "；缺少设计图：character:无图角色") {
		t.Errorf("missing sheet segment wrong: %q", got)
	}
}

func TestFormatPreflightErrorPlaceholderForEmptyCategory(t *testing.T) {
	result := PreflightResult{Issues: []PreflightIssue{
		{Field: "characters_in_segment", AssetName: "幽灵", Kind: PreflightKindUnregistered, AssetType: domainproject.AssetCharacter},
	}}
	got := FormatPreflightError(result)
	want := "生成前检查失败。未登记资产：character:幽灵；缺少设计图：—"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFormatPreflightErrorEmptyResult(t *testing.T) {
	if got := FormatPreflightError(PreflightResult{}); got != "" {
		t.Fatalf("empty result must render empty string, got %q", got)
	}
}
