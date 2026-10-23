package media

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func refShotsFixture() []any {
	return []any{
		map[string]any{"shot_id": "S1", "duration_seconds": float64(3),
			"characters_in_shot": []any{"小明"}, "scenes": []any{"客厅"}},
		map[string]any{"shot_id": "S2", "duration_seconds": float64(4),
			"products_in_shot": []any{"咖啡"}, "characters_in_shot": []any{"小明"}},
		map[string]any{"shot_id": "S3", "duration_seconds": float64(2), "props": []any{"杯子"}},
	}
}

func TestAdUnitReferencesProductFirstAndDedup(t *testing.T) {
	shots := []map[string]any{
		{"shot_id": "S1", "characters_in_shot": []any{"小明"}, "scenes": []any{"客厅"}},
		{"shot_id": "S2", "products_in_shot": []any{"咖啡"}, "characters_in_shot": []any{"小明"}},
	}
	refs := AdUnitReferences(shots)
	want := []AdReference{
		{Type: "product", Name: "咖啡"},
		{Type: "character", Name: "小明"},
		{Type: "scene", Name: "客厅"},
	}
	if len(refs) != len(want) {
		t.Fatalf("refs=%+v want=%+v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("refs[%d]=%+v want %+v", i, refs[i], want[i])
		}
	}
}

func TestDeriveAdReferenceUnitsConsecutiveGrouping(t *testing.T) {
	units := DeriveAdReferenceUnits(refShotsFixture(), 1, nil)
	if len(units) != 1 {
		t.Fatalf("三镜头未超 4 上限应合成一个 unit，得到 %d", len(units))
	}
	if units[0]["unit_id"] != "E1U1" {
		t.Fatalf("unit_id 命名不符: %v", units[0]["unit_id"])
	}
	if len(units[0]["shot_ids"].([]any)) != 3 {
		t.Fatalf("成员镜头应保序全含: %+v", units[0]["shot_ids"])
	}
}

func TestDeriveAdReferenceUnitsHonorsMaxShotsAndDuration(t *testing.T) {
	shots := make([]any, 0, 6)
	for i := 1; i <= 6; i++ {
		shots = append(shots, map[string]any{"shot_id": "S" + string(rune('0'+i)), "duration_seconds": float64(2)})
	}
	units := DeriveAdReferenceUnits(shots, 2, nil)
	if len(units) != 2 || len(units[0]["shot_ids"].([]any)) != 4 || len(units[1]["shot_ids"].([]any)) != 2 {
		t.Fatalf("每 unit 最多 4 镜头，得到 %+v", units)
	}

	maxDuration := 5
	units = DeriveAdReferenceUnits(shots, 2, &maxDuration)
	for _, u := range units {
		if len(u["shot_ids"].([]any)) > 3 {
			t.Fatalf("时长上限 5 秒（每镜头 2 秒）应至多 3 镜头：%+v", u)
		}
	}
	if units[0]["unit_id"] != "E2U1" {
		t.Fatalf("集号应写入 unit_id: %v", units[0]["unit_id"])
	}
}

func TestResolveAdUnitShotsReportsStaleIndex(t *testing.T) {
	script := map[string]any{"shots": refShotsFixture()}
	unit := map[string]any{"unit_id": "E1U1", "shot_ids": []any{"S1", "NOPE"}}
	if _, err := ResolveAdUnitShots(script, unit); err == nil {
		t.Fatal("悬空索引应报错")
	}
	unitOK := map[string]any{"unit_id": "E1U1", "shot_ids": []any{"S1", "S2"}}
	shots, err := ResolveAdUnitShots(script, unitOK)
	if err != nil || len(shots) != 2 {
		t.Fatalf("正常索引应水合两镜头: %v %v", shots, err)
	}
}

func TestAssembleShotsTextSkipsDirtyEntries(t *testing.T) {
	shots := []any{
		map[string]any{"text": "镜头一"},
		"not-a-map",
		map[string]any{"text": nil},
		map[string]any{"text": "镜头二"},
	}
	if got := AssembleShotsText(shots); got != "镜头一\n\n镜头二" {
		t.Fatalf("拼接结果=%q", got)
	}
}

func TestRenderAdUnitPromptStyleAndEmpty(t *testing.T) {
	style := "画风：写实"
	shots := []map[string]any{
		{"duration_seconds": float64(3), "image_prompt": map[string]any{"scene": "客厅"},
			"video_prompt": map[string]any{"action": "坐下", "camera_motion": "Static",
				"dialogue": []any{map[string]any{"speaker": "小明", "line": "你好"}}}},
	}
	got := RenderAdUnitPrompt(shots, &style)
	if got == "" || got[:7] != "Style: " {
		t.Fatalf("应注入 Style 头行: %q", got)
	}
	if want := "Style: 写实\nShot 1 (3s): 客厅；坐下；运镜：Static；<小明>说 {你好}"; got != want {
		t.Fatalf("渲染=%q want %q", got, want)
	}
	if empty := RenderAdUnitPrompt([]map[string]any{{"duration_seconds": float64(1)}}, nil); empty != "" {
		t.Fatalf("无画面内容应返回空串: %q", empty)
	}
}

func TestAdSourceSignatureMatchesPythonCanonicalJSON(t *testing.T) {
	byID := map[string]map[string]any{
		"S1": {"shot_id": "S1", "characters_in_shot": []any{"小明"}},
	}
	unit := map[string]any{"shot_ids": []any{"S1"}}
	canonical := `{"references":[["character","小明"]],"shot_ids":["S1"]}`
	sum := sha256.Sum256([]byte(canonical))
	if got, want := AdSourceSignature(byID, unit), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("signature=%s want %s", got, want)
	}
}

func TestSyncAdReferenceUnitsPreservesGeneratedAssets(t *testing.T) {
	script := map[string]any{
		"shots": refShotsFixture(),
		"reference_units": []any{
			map[string]any{"unit_id": "E1U1", "shot_ids": []any{"S1", "S2"},
				"generated_assets": map[string]any{"video_clip": "reference_videos/E1U1.mp4"}},
		},
	}
	units := SyncAdReferenceUnits(script, 1, nil)
	if len(units) != 1 {
		t.Fatalf("三镜头派生 1 unit，得到 %d", len(units))
	}
	assets := GeneratedAssets(units[0])
	if clip, _ := assets["video_clip"].(string); clip != "reference_videos/E1U1.mp4" {
		t.Fatalf("既有产物指针应沿用: %+v", assets)
	}
	if _, ok := script["reference_units"].([]any); !ok {
		t.Fatal("合并索引应写回剧本")
	}
}

func TestMergeAdReferenceUnitsDefaultsAssetsForNewUnits(t *testing.T) {
	derived := []map[string]any{{"unit_id": "E1U1", "shot_ids": []any{"S1"}, "references": []any{}}}
	merged := MergeAdReferenceUnits(nil, derived)
	assets, ok := merged[0]["generated_assets"].(map[string]any)
	if !ok || assets["status"] != "pending" {
		t.Fatalf("新 unit 应带默认 generated_assets: %+v", merged[0]["generated_assets"])
	}
}

func TestAdStaleUnitIDsComparesSignature(t *testing.T) {
	script := map[string]any{"shots": []any{
		map[string]any{"shot_id": "S1", "characters_in_shot": []any{"小明"}},
	}}
	unit := map[string]any{"unit_id": "E1U1", "shot_ids": []any{"S1"},
		"generated_assets": map[string]any{
			"video_clip":       "reference_videos/E1U1.mp4",
			"source_signature": AdUnitSourceSignature(script, map[string]any{"unit_id": "E1U1", "shot_ids": []any{"S1"}}),
		}}
	if stale := AdStaleUnitIDs(script, []any{unit}); len(stale) != 0 {
		t.Fatalf("签名一致不应 stale: %v", stale)
	}
	unit["generated_assets"].(map[string]any)["source_signature"] = "deadbeef"
	if stale := AdStaleUnitIDs(script, []any{unit}); len(stale) != 1 || stale[0] != "E1U1" {
		t.Fatalf("签名偏离应 stale: %v", stale)
	}
}

func TestReferenceUnitVideoBucketPresence(t *testing.T) {
	if got := ReferenceUnitVideoBucket(map[string]any{"references": []any{map[string]any{"type": "character"}}}, nil); got != "r2v" {
		t.Fatalf("有参考应落 r2v: %s", got)
	}
	if got := ReferenceUnitVideoBucket(map[string]any{}, nil); got != "i2v" {
		t.Fatalf("无参考应落 i2v: %s", got)
	}
	adShots := []map[string]any{{"shot_id": "S1", "characters_in_shot": []any{"小明"}}}
	if got := ReferenceUnitVideoBucket(nil, adShots); got != "r2v" {
		t.Fatalf("ad 从镜头现算应落 r2v: %s", got)
	}
}
