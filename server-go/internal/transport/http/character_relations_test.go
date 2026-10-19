package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/migrations"
)

// relationsFixture 起一个角色关系壳（不注入分析工厂，分析端点验 503 语义）。
type relationsFixture struct {
	router *gin.Engine
	store  *service.ProjectStore
}

func newRelationsFixture(t *testing.T) relationsFixture {
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
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "测试项目", domain.ContentDrama, domain.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Update("p1", func(pf *domain.ProjectFile) error {
		if pf.Characters == nil {
			pf.Characters = domain.AssetBucket{}
		}
		pf.Characters["阿明"] = domain.WrapAssetEntry(map[string]any{"description": "少年"})
		pf.Characters["阿花"] = domain.WrapAssetEntry(map[string]any{"description": "少女"})
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterCharacterRelations(r.Group("/api/v1"), CharacterRelationsDeps{Store: store})
	return relationsFixture{router: r, store: store}
}

func (f relationsFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
	}
	return w.Code, out
}

// TestRelationsEmptyState 校验初始状态：revision 0、无边、空坐标。
func TestRelationsEmptyState(t *testing.T) {
	f := newRelationsFixture(t)
	status, body := f.do(t, http.MethodGet, "/api/v1/projects/p1/character-relations", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["revision"] != float64(0) || body["analysis_status"] != "idle" {
		t.Fatalf("body = %v", body)
	}
	edges, _ := body["edges"].([]any)
	if len(edges) != 0 {
		t.Fatalf("edges = %v", edges)
	}
}

// TestRelationsSaveWithRevision 校验保存：revision 递增、新边归 manual。
func TestRelationsSaveWithRevision(t *testing.T) {
	f := newRelationsFixture(t)
	status, body := f.do(t, http.MethodPut, "/api/v1/projects/p1/character-relations", map[string]any{
		"base_revision": 0,
		"edges": []map[string]any{{
			"id": "edge-1", "source": "阿明", "target": "阿花",
			"type": "ally", "label": "盟友", "directed": false,
		}},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["revision"] != float64(1) {
		t.Fatalf("revision = %v", body["revision"])
	}
	edges, _ := body["edges"].([]any)
	if len(edges) != 1 {
		t.Fatalf("edges = %v", edges)
	}
	edge := edges[0].(map[string]any)
	if edge["origin"] != "manual" || edge["manual_override"] != true {
		t.Fatalf("新边应归人工: %v", edge)
	}

	// 过期修订号 → 409
	status, body = f.do(t, http.MethodPut, "/api/v1/projects/p1/character-relations", map[string]any{
		"base_revision": 0, "edges": []map[string]any{},
	})
	if status != http.StatusConflict {
		t.Fatalf("过期修订应 409: status = %d body=%v", status, body)
	}
}

// TestRelationsDeletionSuppressesAIEdge 校验删除 AI 边写入抑制对，人工边直接消失。
func TestRelationsDeletionSuppressesAIEdge(t *testing.T) {
	f := newRelationsFixture(t)
	// 先造一条 AI 边 + 一条人工边。
	if err := f.store.Update("p1", func(pf *domain.ProjectFile) error {
		if pf.Characters == nil {
			pf.Characters = domain.AssetBucket{}
		}
		pf.Characters["小刚"] = domain.WrapAssetEntry(map[string]any{"description": "男孩"})
		if pf.CharacterRelations == nil {
			pf.CharacterRelations = map[string]any{}
		}
		pf.CharacterRelations["edges"] = []any{
			map[string]any{
				"id": "ai-1", "source": "阿明", "target": "阿花", "type": "rival",
				"origin": "ai", "manual_override": false,
			},
			map[string]any{
				"id": "ai-2", "source": "阿花", "target": "小刚", "type": "ally",
				"origin": "ai", "manual_override": false,
			},
			map[string]any{
				"id": "man-1", "source": "阿明", "target": "小刚", "type": "ally",
				"origin": "manual", "manual_override": true,
			},
		}
		pf.CharacterRelations["revision"] = 3
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 提交里只保留人工边 → 两条被删除的 AI 边都进抑制对。
	status, body := f.do(t, http.MethodPut, "/api/v1/projects/p1/character-relations", map[string]any{
		"base_revision": 3,
		"edges": []map[string]any{{
			"id": "man-1", "source": "阿明", "target": "小刚", "type": "ally",
			"directed": false,
		}},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	suppressed, _ := body["suppressed_pairs"].([]any)
	if len(suppressed) != 2 {
		t.Fatalf("suppressed = %v", body)
	}
	// 抑制对按规范序（无向对名称排序）落库。
	foundRival, foundAlly := false, false
	for _, raw := range suppressed {
		pair := raw.(map[string]any)
		if pair["source"] == "阿明" && pair["target"] == "阿花" {
			foundRival = true
		}
		if pair["source"] == "阿花" && pair["target"] == "小刚" {
			foundAlly = true
		}
	}
	if !foundRival || !foundAlly {
		t.Fatalf("suppressed = %v", suppressed)
	}
}

// TestRelationsNodePositionsUpdate 校验只传坐标也能保存（边不变）。
func TestRelationsNodePositionsUpdate(t *testing.T) {
	f := newRelationsFixture(t)
	status, body := f.do(t, http.MethodPut, "/api/v1/projects/p1/character-relations", map[string]any{
		"base_revision": 0,
		"edges":         []map[string]any{},
		"node_positions": map[string]any{
			"阿明": map[string]any{"x": 10.5, "y": 20.0},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	positions, _ := body["node_positions"].(map[string]any)
	node, _ := positions["阿明"].(map[string]any)
	if node == nil || node["x"] != 10.5 {
		t.Fatalf("positions = %v", positions)
	}
}

// TestRelationsAnalyzeUnavailable 校验未配置生成器时分析端点返回 503 语义。
func TestRelationsAnalyzeUnavailable(t *testing.T) {
	f := newRelationsFixture(t)
	status, body := f.do(t, http.MethodPost, "/api/v1/projects/p1/character-relations/analyze", nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%v", status, body)
	}
}
