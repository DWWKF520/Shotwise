package character

import (
	"github.com/shotwise/shotwise/internal/domain/project"
	"strings"
	"testing"
)

func relationCharacterBucket(names ...string) project.AssetBucket {
	bucket := project.AssetBucket{}
	for _, name := range names {
		entry := project.NewAssetEntry()
		entry.SetString("description", name+" 的描述")
		bucket[name] = entry
	}
	return bucket
}

func TestNormalizeCharacterRelationsResolvesNamesAndRejectsBadData(t *testing.T) {
	characters := relationCharacterBucket("阿明", "小红")
	data, err := NormalizeCharacterRelations(map[string]any{
		"revision":        float64(2),
		"analysis_status": "ready",
		"edges": []any{map[string]any{
			"id": "e1", "source": " 阿明 ", "target": "小红", "type": "friend",
			"label": "青梅竹马", "directed": false, "confidence": 0.8,
		}},
		"suppressed_pairs": []any{map[string]any{"source": "阿明", "target": "小红", "directed": false}},
		"node_positions":   map[string]any{"阿明": map[string]any{"x": float64(10), "y": float64(20)}},
	}, characters)
	if err != nil {
		t.Fatal(err)
	}
	if data.Revision != 2 || data.AnalysisStatus != RelationStatusReady {
		t.Fatalf("revision/status = %d/%s", data.Revision, data.AnalysisStatus)
	}
	if len(data.Edges) != 1 || data.Edges[0].Source != "阿明" || data.Edges[0].Origin != RelationOriginAI {
		t.Fatalf("edge not normalized: %+v", data.Edges)
	}
	if len(data.SuppressedPairs) != 1 || data.NodePositions["阿明"].X != 10 {
		t.Fatalf("suppressed/positions wrong: %+v %+v", data.SuppressedPairs, data.NodePositions)
	}

	cases := map[string]map[string]any{
		"未知顶层字段":        {"unknown": 1},
		"未知角色":          {"edges": []any{map[string]any{"source": "无名", "target": "阿明", "type": "friend"}}},
		"自环":            {"edges": []any{map[string]any{"source": "阿明", "target": "阿明", "type": "friend"}}},
		"未知类型":          {"edges": []any{map[string]any{"source": "阿明", "target": "小红", "type": "enemy_of"}}},
		"重复 id":         {"edges": []any{map[string]any{"id": "x", "source": "阿明", "target": "小红", "type": "friend"}, map[string]any{"id": "x", "source": "小红", "target": "阿明", "type": "enemy"}}},
		"非法状态":          {"analysis_status": "done"},
		"confidence 越界": {"edges": []any{map[string]any{"source": "阿明", "target": "小红", "type": "friend", "confidence": 1.5}}},
		"边内未知字段":        {"edges": []any{map[string]any{"source": "阿明", "target": "小红", "type": "friend", "color": "red"}}},
	}
	for name, value := range cases {
		if _, err := NormalizeCharacterRelations(value, characters); err == nil {
			t.Errorf("%s: 期望报错", name)
		}
	}

	// 同对同类型的重复边：非有向对按名称排序后归一，directed 参与身份。
	if _, err := NormalizeCharacterRelations(map[string]any{"edges": []any{
		map[string]any{"id": "a", "source": "阿明", "target": "小红", "type": "friend"},
		map[string]any{"id": "b", "source": "小红", "target": "阿明", "type": "friend"},
	}}, characters); err == nil {
		t.Error("同对同类型重复边应报错")
	}
}

func TestMergeAIRelationshipsPreservesManualEditsAndDeletions(t *testing.T) {
	characters := relationCharacterBucket("阿明", "小红", "老王")
	existing := map[string]any{
		"revision": float64(3),
		"edges": []any{
			map[string]any{"id": "m1", "source": "阿明", "target": "小红", "type": "family", "origin": "manual",
				"label": "兄妹", "directed": false},
			map[string]any{"id": "a1", "source": "老王", "target": "阿明", "type": "friend", "origin": "ai",
				"label": "旧边", "directed": false},
		},
		"suppressed_pairs": []any{map[string]any{"source": "小红", "target": "老王", "directed": false}},
	}

	aiEdges := []RelationEdge{
		{Source: "小红", Target: "老王", Type: "enemy", Directed: false},  // 被抑制，不得复活
		{Source: "老王", Target: "阿明", Type: "friend", Directed: false}, // 未受保护，应刷新
		{Source: "阿明", Target: "小红", Type: "ally", Directed: false},   // 同对被人工保护，跳过
		{Source: "小红", Target: "老王", Type: "rival", Directed: false},  // 同被抑制对，跳过
	}
	merged, err := MergeAIRelationships(existing, characters, aiEdges, "fp1")
	if err != nil {
		t.Fatal(err)
	}
	if merged.Revision != 4 || merged.AnalysisStatus != RelationStatusReady {
		t.Fatalf("revision/status = %d/%s", merged.Revision, merged.AnalysisStatus)
	}
	if merged.SourceFingerprint == nil || *merged.SourceFingerprint != "fp1" {
		t.Fatalf("fingerprint = %v", merged.SourceFingerprint)
	}
	if merged.AnalyzedAt == nil || !strings.HasSuffix(*merged.AnalyzedAt, "+00:00") {
		t.Fatalf("analyzed_at = %v", merged.AnalyzedAt)
	}

	var manual, ai int
	for _, edge := range merged.Edges {
		switch edge.Origin {
		case RelationOriginManual:
			manual++
			if edge.ID != "m1" || edge.Label != "兄妹" {
				t.Fatalf("人工边被改写: %+v", edge)
			}
		case RelationOriginAI:
			ai++
			if edge.Source == "小红" && edge.Target == "老王" {
				t.Fatalf("被抑制的对不得复活: %+v", edge)
			}
			if edge.Type == "ally" {
				t.Fatalf("受人工保护的对不得新增 AI 边: %+v", edge)
			}
			if edge.ID == "a1" {
				t.Fatalf("未被保护的旧 AI 边应被替换: %+v", edge)
			}
		}
	}
	if manual != 1 || ai != 1 {
		t.Fatalf("manual=%d ai=%d edges=%+v", manual, ai, merged.Edges)
	}
}

func TestMergeAIRelationshipsRejectsUnknownCharacterInAIOutput(t *testing.T) {
	characters := relationCharacterBucket("阿明", "小红")
	_, err := MergeAIRelationships(nil, characters,
		[]RelationEdge{{Source: "阿明", Target: "幽灵", Type: "friend"}}, "fp")
	if err == nil || !strings.Contains(err.Error(), "unknown character") {
		t.Fatalf("err = %v", err)
	}
}

func TestMergeAIRelationshipsDedupsSamePairAndType(t *testing.T) {
	characters := relationCharacterBucket("阿明", "小红")
	merged, err := MergeAIRelationships(nil, characters, []RelationEdge{
		{Source: "阿明", Target: "小红", Type: "friend", Label: "a"},
		{Source: "小红", Target: "阿明", Type: "friend", Label: "b"},
		{Source: "阿明", Target: "小红", Type: "enemy", Label: "c"},
	}, "fp")
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Edges) != 2 {
		t.Fatalf("同对同类型只留一条：%+v", merged.Edges)
	}
}

func TestMarkAnalysisStartedAndFailed(t *testing.T) {
	characters := relationCharacterBucket("阿明", "小红")
	started, err := MarkAnalysisStarted(map[string]any{"analysis_status": "failed", "error": "旧错误"}, characters)
	if err != nil {
		t.Fatal(err)
	}
	if started.AnalysisStatus != RelationStatusAnalyzing || started.Error != nil {
		t.Fatalf("started = %+v", started)
	}

	failed, err := MarkAnalysisFailed(map[string]any{}, characters, strings.Repeat("x", 1200))
	if err != nil {
		t.Fatal(err)
	}
	if failed.AnalysisStatus != RelationStatusFailed || failed.Error == nil || len(*failed.Error) != 1000 {
		t.Fatalf("failed = %+v", failed)
	}
}

func TestRelationsPayloadRoundTrips(t *testing.T) {
	characters := relationCharacterBucket("阿明", "小红")
	data, err := MergeAIRelationships(nil, characters,
		[]RelationEdge{{Source: "阿明", Target: "小红", Type: "friend", Directed: false, Confidence: nil}}, "fp")
	if err != nil {
		t.Fatal(err)
	}
	payload := RelationsPayload(data)
	for _, key := range []string{"revision", "analysis_status", "analyzed_at", "source_fingerprint", "error", "edges", "suppressed_pairs", "node_positions"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("payload 缺字段 %s", key)
		}
	}
	back, err := NormalizeCharacterRelations(payload, characters)
	if err != nil {
		t.Fatalf("落盘结构无法回读: %v", err)
	}
	if len(back.Edges) != 1 || back.Edges[0].Type != "friend" || back.Revision != data.Revision {
		t.Fatalf("round trip = %+v", back)
	}
}

func TestRenameAndRemoveRelationReferencesStayConsistent(t *testing.T) {
	// 既有的改名/删除级联与关系图结构必须继续兼容（同一份 payload 结构）。
	relations := map[string]any{
		"edges":          []any{map[string]any{"source": "阿明", "target": "小红", "type": "friend"}},
		"node_positions": map[string]any{"阿明": map[string]any{"x": float64(1), "y": float64(2)}},
	}
	project.RenameRelationReferences(relations, "阿明", "小明")
	if edge := relations["edges"].([]any)[0].(map[string]any); edge["source"] != "小明" {
		t.Fatalf("改名未同步关系边: %+v", edge)
	}
	project.RemoveRelationReferences(relations, "小红")
	if len(relations["edges"].([]any)) != 0 {
		t.Fatalf("删角色未清理关系边: %+v", relations["edges"])
	}
}
