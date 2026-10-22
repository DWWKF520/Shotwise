package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/asset"
)

type assetFixture struct {
	router *gin.Engine
	store  *service.ProjectStore
	root   string
}

// newAssetFixture 起一个不带鉴权的路由壳：这里验的是资产层契约，
// 认证中间件由 auth 自己的测试覆盖。
func newAssetFixture(t *testing.T) assetFixture {
	t.Helper()
	root := t.TempDir()
	store := service.NewProjectStore(root)
	if _, err := store.Create("p1", "测试项目", domain.ContentNarration, domain.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 与 app.New 一致：未命中路由也要出 {"detail": ...} 信封，而非 gin 的纯文本 404。
	r.NoRoute(func(c *gin.Context) { c.JSON(http.StatusNotFound, gin.H{"detail": "Not Found"}) })
	r.NoMethod(func(c *gin.Context) { c.JSON(http.StatusMethodNotAllowed, gin.H{"detail": "Method Not Allowed"}) })
	v1 := r.Group("/api/v1")

	// 项目路由与资产路由注册在同一棵路由树上：doc 01 §0 提醒 /projects/:name/... 下同前缀的
	// 静态段与参数段会争抢，构造期就会 panic，因此这里也是路由冲突的守门测试。
	deps := ProjectDeps{Store: store, Hub: service.NewEventHub()}
	v1.GET("/projects", ListProjects(deps))
	v1.POST("/projects", CreateProject(deps))
	v1.GET("/projects/:name", GetProject(deps))
	v1.PATCH("/projects/:name", UpdateProject(deps))
	v1.DELETE("/projects/:name", DeleteProject(deps))
	v1.GET("/projects/:name/events/stream", ProjectEventStream(deps))

	RegisterAssets(v1, AssetDeps{Assets: asset.NewAssetService(store), Store: store})
	return assetFixture{router: r, store: store, root: root}
}

func (f assetFixture) call(t *testing.T, method, path string, body any, lang string) (int, map[string]any) {
	t.Helper()
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		payload = b
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s → %d, unparseable body %q", method, path, w.Code, w.Body.String())
		}
	}
	return w.Code, out
}

// detail 取统一错误信封里的 detail 字符串。
func detail(t *testing.T, body map[string]any) string {
	t.Helper()
	s, ok := body["detail"].(string)
	if !ok {
		t.Fatalf("detail 不是字符串: %#v", body)
	}
	return s
}

func TestCreateCharacterSmoke(t *testing.T) {
	f := newAssetFixture(t)

	status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/characters", gin.H{
		"name": "  阿明 ", "description": "少年剑客", "voice_style": "低沉", "reference_images": []any{1},
	}, "en")
	if status != http.StatusOK {
		t.Fatalf("create = %d %v, want 200", status, body)
	}
	if body["success"] != true {
		t.Fatalf("响应形状应为 {success, character}: %#v", body)
	}
	entry := body["character"].(map[string]any)
	if entry["description"] != "少年剑客" || entry["voice_style"] != "低沉" {
		t.Errorf("entry = %v", entry)
	}
	// 未登记的额外字段（reference_images 不是 character 的字段）应被静默丢弃，不污染 project.json
	if _, leaked := entry["reference_images"]; leaked {
		t.Error("spec 之外的键不该落进 project.json")
	}
	if _, ok := entry["character_sheet"]; !ok {
		t.Error("sheet 字段应初始化为空串")
	}

	pf, err := f.store.Get("p1")
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	// name 在边界被 strip + NFC 归一后才是桶 key
	if _, ok := pf.Characters["阿明"]; !ok {
		t.Fatalf("桶 key 应为归一后的 阿明，实际 %v", keysOf(pf.Characters))
	}

	status, body = f.call(t, http.MethodPost, "/api/v1/projects/p1/characters", gin.H{
		"name": "阿明", "description": "重复",
	}, "en")
	if status != http.StatusConflict {
		t.Fatalf("duplicate = %d %v, want 409", status, body)
	}
	if got := detail(t, body); got != "Character already exists: 阿明" {
		t.Errorf("detail = %q", got)
	}
}

func TestAssetValidationErrorsAtTheBoundary(t *testing.T) {
	f := newAssetFixture(t)

	// 名字会被拼进文件路径与单段路由参数，边界即拒
	status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/characters", gin.H{"name": "a/b"}, "")
	if status != http.StatusBadRequest {
		t.Fatalf("illegal name = %d %v, want 400", status, body)
	}
	if got := detail(t, body); !strings.HasPrefix(got, "资产名不合法") {
		t.Errorf("缺省语言应出中文文案: %q", got)
	}

	status, body = f.call(t, http.MethodPost, "/api/v1/projects/p1/characters", gin.H{"description": "缺名字"}, "en")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("missing name = %d %v, want 422", status, body)
	}
	items, ok := body["detail"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("422 应是 FastAPI 的校验项数组: %#v", body["detail"])
	}
	item := items[0].(map[string]any)
	if item["type"] != "value_error.missing" {
		t.Errorf("校验项 = %v", item)
	}

	// 类型不对的 extra 字段：Python 侧是不走翻译的英文原文
	status, body = f.call(t, http.MethodPost, "/api/v1/projects/p1/characters",
		gin.H{"name": "阿红", "voice_style": 123}, "zh")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("wrong type = %d %v, want 422", status, body)
	}
	if got := detail(t, body); got != "field 'voice_style' must be a string" {
		t.Errorf("detail = %q", got)
	}

	// 新建时禁止携带 voice_notice_dismissed_at：否则绕过 PATCH 侧的等值校验
	status, body = f.call(t, http.MethodPost, "/api/v1/projects/p1/characters",
		gin.H{"name": "阿红", "description": "d", "voice_notice_dismissed_at": "2099-01-01T00:00:00Z"}, "en")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("stale notice on create = %d %v, want 422", status, body)
	}
	if got := detail(t, body); got != "The voice changed again; reload and confirm." {
		t.Errorf("detail = %q", got)
	}

	// 未知资产类型不会命中任何路由（路由表由 spec 驱动）
	status, _ = f.call(t, http.MethodPost, "/api/v1/projects/p1/widgets", gin.H{"name": "x"}, "")
	if status != http.StatusNotFound {
		t.Errorf("unregistered subdir = %d, want 404", status)
	}

	// 项目不存在与路径越界同样出 404
	status, _ = f.call(t, http.MethodPost, "/api/v1/projects/nope/characters", gin.H{"name": "x"}, "")
	if status != http.StatusNotFound {
		t.Errorf("missing project = %d, want 404", status)
	}
}

func TestPatchCharacterVoiceFields(t *testing.T) {
	f := newAssetFixture(t)
	if status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/characters",
		gin.H{"name": "阿明", "description": "d"}, ""); status != http.StatusOK {
		t.Fatalf("create = %d %v", status, body)
	}

	status, body := f.call(t, http.MethodPatch, "/api/v1/projects/p1/characters/阿明",
		gin.H{"voice_notice_dismissed_at": "whatever"}, "en")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("stale notice = %d %v, want 422", status, body)
	}

	status, body = f.call(t, http.MethodPatch, "/api/v1/projects/p1/characters/阿明",
		gin.H{"description": "新描述", "reference_audio": "characters/refs_audio/阿明.wav"}, "")
	if status != http.StatusOK {
		t.Fatalf("patch = %d %v", status, body)
	}
	entry := body["character"].(map[string]any)
	if entry["description"] != "新描述" {
		t.Errorf("description = %v", entry["description"])
	}
	stamp, _ := entry["voice_updated_at"].(string)
	if stamp == "" {
		t.Fatal("改 reference_audio 必须机械戳 voice_updated_at，否则存量过渡横幅感知不到变化")
	}

	status, body = f.call(t, http.MethodPatch, "/api/v1/projects/p1/characters/阿明",
		gin.H{"voice_notice_dismissed_at": stamp}, "")
	if status != http.StatusOK {
		t.Fatalf("confirm with current stamp = %d %v, want 200", status, body)
	}

	status, _ = f.call(t, http.MethodPatch, "/api/v1/projects/p1/characters/不存在", gin.H{"description": "x"}, "")
	if status != http.StatusNotFound {
		t.Errorf("missing entry = %d, want 404", status)
	}
}

func TestRenameCharacterCascade(t *testing.T) {
	f := newAssetFixture(t)
	root := f.root

	if status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/characters",
		gin.H{"name": "阿明", "description": "d"}, ""); status != http.StatusOK {
		t.Fatalf("create = %d %v", status, body)
	}
	// 手工把路径字段指向按名落盘的生成物
	if err := f.store.Update("p1", func(pf *domain.ProjectFile) error {
		pf.Characters["阿明"].SetString("character_sheet", "characters/阿明.png")
		pf.CharacterRelations = map[string]any{
			"revision":         1.0,
			"edges":            []any{map[string]any{"source": "阿明", "target": "阿红"}},
			"suppressed_pairs": []any{},
			"node_positions":   map[string]any{"阿明": map[string]any{"x": 1.0}},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	charDir := filepath.Join(root, "p1", "characters")
	if err := os.MkdirAll(charDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(charDir, "阿明.png")
	if err := os.WriteFile(oldFile, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 兄弟资产的文件，改名不得卷走
	sibling := filepath.Join(charDir, "阿明_2.png")
	if err := os.WriteFile(sibling, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := `{
	  "segments": [
	    {"segment_id": "E1S1", "characters_in_segment": ["阿明", "阿红"],
	     "utterances": [{"kind": "dialogue", "speaker": "阿明", "text": "hi"}]}
	  ]
	}`
	scriptPath := filepath.Join(root, "p1", "scripts", "episode_1.json")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}

	// —— dry_run：只给预览，一处都不许落盘 ——
	status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/characters/阿明/rename",
		gin.H{"new_name": "阿明那", "dry_run": true}, "")
	if status != http.StatusOK {
		t.Fatalf("dry run = %d %v", status, body)
	}
	if body["dry_run"] != true || body["old_name"] != "阿明" || body["new_name"] != "阿明那" {
		t.Errorf("report shape = %#v", body)
	}
	// 2 处引用（列表 + speaker）+ 关系图不算引用
	if got := body["references"]; got != 2.0 {
		t.Errorf("references = %v, want 2", got)
	}
	if got := body["episodes"]; got != 1.0 {
		t.Errorf("episodes = %v, want 1", got)
	}
	if got := body["files"]; got != 1.0 {
		t.Errorf("files = %v, want 1", got)
	}
	if _, err := os.Stat(oldFile); err != nil {
		t.Error("dry_run 把磁盘文件动了")
	}
	onDisk, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(onDisk, []byte(`"阿明"`)) {
		t.Error("dry_run 改了剧本")
	}
	pf, _ := f.store.Get("p1")
	if _, ok := pf.Characters["阿明"]; !ok {
		t.Error("dry_run 改了 project.json")
	}

	// —— 执行 ——
	status, body = f.call(t, http.MethodPost, "/api/v1/projects/p1/characters/阿明/rename",
		gin.H{"new_name": "阿明那"}, "")
	if status != http.StatusOK {
		t.Fatalf("rename = %d %v", status, body)
	}
	if body["dry_run"] != false {
		t.Errorf("dry_run flag = %v", body["dry_run"])
	}

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("旧名文件残留")
	}
	if _, err := os.Stat(filepath.Join(charDir, "阿明那.png")); err != nil {
		t.Errorf("新名文件未落位: %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Error("兄弟资产「阿明_2.png」被误卷走")
	}

	after, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(after, []byte(`"阿明那"`)) || bytes.Contains(after, []byte(`"阿明",`)) {
		t.Errorf("剧本引用未同步: %s", after)
	}

	pf, err = f.store.Get("p1")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry, ok := pf.Characters["阿明那"]
	if !ok {
		t.Fatalf("project.json 桶 key 未改: %v", keysOf(pf.Characters))
	}
	if got := entry.MustString("character_sheet"); got != "characters/阿明那.png" {
		t.Errorf("sheet 路径未同步: %q", got)
	}
	edge := pf.CharacterRelations["edges"].([]any)[0].(map[string]any)
	if edge["source"] != "阿明那" {
		t.Errorf("关系边仍指向旧角色，画布会悬空: %v", edge["source"])
	}
	if _, stale := pf.CharacterRelations["node_positions"].(map[string]any)["阿明"]; stale {
		t.Error("旧节点坐标残留")
	}

	// 重跑同一次改名：旧名已不在桶里 → 404，且此前所有步骤幂等，不会二次损伤
	status, _ = f.call(t, http.MethodPost, "/api/v1/projects/p1/characters/阿明/rename",
		gin.H{"new_name": "阿明那"}, "")
	if status != http.StatusNotFound {
		t.Errorf("re-running rename = %d, want 404", status)
	}
}

func TestRenameRejectsCollisionBeforeAnyWrite(t *testing.T) {
	f := newAssetFixture(t)
	if status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/characters",
		gin.H{"name": "阿明", "description": "d"}, ""); status != http.StatusOK {
		t.Fatalf("create = %d %v", status, body)
	}
	if status, _ := f.call(t, http.MethodPost, "/api/v1/projects/p1/characters",
		gin.H{"name": "阿明那", "description": "d"}, ""); status != http.StatusOK {
		t.Fatalf("create target = %d", status)
	}

	status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/characters/阿明/rename",
		gin.H{"new_name": "阿明那"}, "en")
	if status != http.StatusConflict {
		t.Fatalf("collision = %d %v, want 409", status, body)
	}
	if got := detail(t, body); got != "Character already exists: 阿明那" {
		t.Errorf("detail = %q", got)
	}

	// 磁盘目标被孤儿文件占用：桶里先没有 阿明，才能让「目标文件已存在」这条分支露出来。
	// 迁移源也必须存在（按目录扫描规划），否则压根没有待迁移文件、走不到占用判定。
	// 规划早于写入，因此整体拒绝。
	if status, body := f.call(t, http.MethodDelete, "/api/v1/projects/p1/characters/阿明", nil, ""); status != http.StatusOK {
		t.Fatalf("delete = %d %v", status, body)
	}
	charDir := filepath.Join(f.root, "p1", "characters")
	if err := os.MkdirAll(charDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"阿明那.png", "阿明.png"} {
		if err := os.WriteFile(filepath.Join(charDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	status, body = f.call(t, http.MethodPost, "/api/v1/projects/p1/characters/阿明那/rename",
		gin.H{"new_name": "阿明"}, "en")
	if status != http.StatusConflict {
		t.Fatalf("file collision = %d %v, want 409", status, body)
	}
	if got := detail(t, body); got != "Rename target already exists: 阿明.png" {
		t.Errorf("detail = %q", got)
	}
	pf, err := f.store.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pf.Characters["阿明那"]; !ok {
		t.Error("冲突被拒后桶被改了")
	}
}

func TestDeleteCharacterRemovesRelationReferences(t *testing.T) {
	f := newAssetFixture(t)
	for _, name := range []string{"阿明", "阿红"} {
		if status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/characters",
			gin.H{"name": name, "description": "d"}, ""); status != http.StatusOK {
			t.Fatalf("create %s = %d %v", name, status, body)
		}
	}
	if err := f.store.Update("p1", func(pf *domain.ProjectFile) error {
		pf.CharacterRelations = map[string]any{
			"edges":            []any{map[string]any{"source": "阿明", "target": "阿红"}},
			"suppressed_pairs": []any{map[string]any{"source": "阿明", "target": "阿红"}},
			"node_positions":   map[string]any{"阿明": map[string]any{}, "阿红": map[string]any{}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	status, body := f.call(t, http.MethodDelete, "/api/v1/projects/p1/characters/阿明", nil, "en")
	if status != http.StatusOK {
		t.Fatalf("delete = %d %v", status, body)
	}
	if got := body["message"]; got != "Character deleted: 阿明" {
		t.Errorf("message = %v", got)
	}

	pf, err := f.store.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pf.Characters["阿明"]; ok {
		t.Error("角色未被删除")
	}
	if edges := pf.CharacterRelations["edges"].([]any); len(edges) != 0 {
		t.Errorf("关系边残留，画布会有无法渲染的孤儿节点: %v", edges)
	}
	if pairs := pf.CharacterRelations["suppressed_pairs"].([]any); len(pairs) != 0 {
		t.Error("AI 删除抑制记录未清理")
	}
	if pos := pf.CharacterRelations["node_positions"].(map[string]any); len(pos) != 1 {
		t.Errorf("节点坐标残留: %v", pos)
	}
}

func TestProductRenameMovesSequencedRefs(t *testing.T) {
	f := newAssetFixture(t)
	status, body := f.call(t, http.MethodPost, "/api/v1/projects/p1/products", gin.H{
		"name": "甲", "description": "d", "brand": "brandX",
		"reference_images": []any{"products/refs/甲_1.png", "products/refs/甲_2.png"},
		"selling_points":   []any{"轻巧"},
	}, "")
	if status != http.StatusOK {
		t.Fatalf("create product = %d %v", status, body)
	}
	entry := body["product"].(map[string]any)
	if len(entry["reference_images"].([]any)) != 2 {
		t.Errorf("多图列表未落盘: %v", entry["reference_images"])
	}

	refs := filepath.Join(f.root, "p1", "products", "refs")
	if err := os.MkdirAll(refs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"甲_1.png", "甲_2.png"} {
		if err := os.WriteFile(filepath.Join(refs, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	status, body = f.call(t, http.MethodPost, "/api/v1/projects/p1/products/甲/rename", gin.H{"new_name": "乙"}, "")
	if status != http.StatusOK {
		t.Fatalf("rename = %d %v", status, body)
	}
	if body["files"] != 2.0 {
		t.Errorf("files = %v, want 2", body["files"])
	}
	if _, err := os.Stat(filepath.Join(refs, "乙_1.png")); err != nil {
		t.Errorf("序号形态的文件应随改名迁移: %v", err)
	}
	pf, _ := f.store.Get("p1")
	images, ok := pf.Products["乙"].List("reference_images")
	if !ok || images[0] != "products/refs/乙_1.png" {
		t.Errorf("条目里的多图路径未同步: %v %v", images, ok)
	}

	// 列表字段类型校验
	if status, body = f.call(t, http.MethodPost, "/api/v1/projects/p1/products",
		gin.H{"name": "丙", "description": "d", "selling_points": []any{"好", 1}}, "en"); status != http.StatusUnprocessableEntity {
		t.Fatalf("bad list = %d %v, want 422", status, body)
	} else if got := detail(t, body); got != "field 'selling_points' must be a list of strings" {
		t.Errorf("detail = %q", got)
	}
}

// TestAssetRouteTableMatchesSpec 把已注册路由钉成「4 类 × 4 端点」的清单。
//
// 这是 Python 侧 build_asset_router 的契约面：新增资产类型时应当只有 spec 注册表一处改动，
// 路由数量随之增长；这里的期望值用 spec 推导而非硬编码，避免变成第二份真相源。
func TestAssetRouteTableMatchesSpec(t *testing.T) {
	f := newAssetFixture(t)

	got := map[string]bool{}
	for _, entry := range f.router.Routes() {
		got[entry.Method+" "+entry.Path] = true
	}
	var want []string
	for _, kind := range domain.AssetTypeOrder {
		subdir := domain.AssetSpecs[kind].Subdir
		base := "/api/v1/projects/:name/" + subdir
		want = append(want,
			"POST "+base,
			"PATCH "+base+"/:entry",
			"POST "+base+"/:entry/rename",
			"DELETE "+base+"/:entry",
		)
	}
	for _, path := range want {
		if !got[path] {
			t.Errorf("路由未注册: %s", path)
		}
	}
	if len(want) != 16 {
		t.Fatalf("期望 16 条端点，实际推导出 %d 条", len(want))
	}
}

func keysOf(b domain.AssetBucket) []string {
	out := make([]string, 0, len(b))
	for k := range b {
		out = append(out, k)
	}
	return out
}
