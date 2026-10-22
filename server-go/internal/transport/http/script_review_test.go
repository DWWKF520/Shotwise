// step1→step2 审核 gate 路由测试（对应 tests/test_script_review_router.py）与
// pending-authoring 路由测试（对应 tests/test_pending_authoring.py 的 TestPendingAuthoringRouter）：
// 审阅读取、内容编辑、确认动作、隔离草稿呈现与待编写推导的可测状态流转。
//
// Python 侧「存在性检查与读取之间隔离草稿被清除」的竞态用例经 monkeypatch read_quarantine
// 实现；Go 侧 ReadQuarantine 是包级函数、无注入缝，该竞态分支由 servicescript.GetQuarantineInfo
// 的二次存在性检查兜住（读跨越清除那一刻按「无隔离草稿」处理），不在本层重复覆盖。
package http

import (
	servicereference "github.com/shotwise/shotwise/internal/service/reference"
	servicescript "github.com/shotwise/shotwise/internal/service/script"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/service"
)

// --- 替身与环境 ---

// reviewCapsStub 是路由测试的视频能力替身：零值等价「解析不到型号」（caps 无档位且项目未
// 配置后端时两条档位来源都取不到）。
type reviewCapsStub struct {
	view       config.VideoCapabilityView
	resolveErr error
}

func (s *reviewCapsStub) VideoCapabilitiesForProjectBucket(
	_ context.Context, _ *domain.ProjectFile, _ provider.VideoBucket,
) (config.VideoCapabilityView, error) {
	if s.resolveErr != nil {
		return config.VideoCapabilityView{}, s.resolveErr
	}
	return s.view, nil
}

func (s *reviewCapsStub) VideoGenerateAudioForProject(_ context.Context, _ *domain.ProjectFile) (bool, error) {
	return false, nil
}

func (s *reviewCapsStub) ResolveResolution(_ context.Context, _ *domain.ProjectFile, _, _ string) (string, error) {
	return "", nil
}

// reviewRouterEnv 是审核 gate handler 测试环境：真实 ProjectStore + ScriptReviewService。
type reviewRouterEnv struct {
	r     *gin.Engine
	store *service.ProjectStore
	dir   string
}

// newReviewRouterEnv 建测试项目（drama 内容模式 + 指定生成路线）、登记第 1 集与角色阿离，
// 注册审核 gate 路由。
func newReviewRouterEnv(
	t *testing.T, generationMode domain.GenerationMode, resolver service.VideoCapabilityResolver,
) *reviewRouterEnv {
	t.Helper()
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("demo", "Demo", domain.ContentDrama, generationMode, "Anime"); err != nil {
		t.Fatal(err)
	}
	if err := store.Update("demo", func(pf *domain.ProjectFile) error {
		if pf.Characters == nil {
			pf.Characters = domain.AssetBucket{}
		}
		pf.Characters["阿离"] = domain.WrapAssetEntry(map[string]any{"name": "阿离", "description": "少女"})
		pf.Episodes = append(pf.Episodes, domain.EpisodeMeta{
			Episode: 1, Title: "第一集", ScriptFile: "scripts/episode_1.json",
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dir, err := store.Dir("demo")
	if err != nil {
		t.Fatal(err)
	}
	reviews := servicescript.NewScriptReviewService(store, resolver)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterScriptReview(r.Group("/api/v1"), ScriptReviewDeps{Store: store, Reviews: reviews})
	return &reviewRouterEnv{r: r, store: store, dir: dir}
}

// gateBlocks 报告该集 step2 是否仍被审核 gate 阻塞。
func (e *reviewRouterEnv) gateBlocks(t *testing.T) bool {
	t.Helper()
	pf, err := e.store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	return servicescript.GateBlocksStep2(e.dir, pf, 1)
}

// reviewQuarantinePath 返回第 1 集 step1 隔离草稿路径并确保父目录存在。
func (e *reviewRouterEnv) reviewQuarantinePath(t *testing.T) string {
	t.Helper()
	pf, err := e.store.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	path, ok := servicescript.Step1QuarantinePath(e.dir, pf, 1)
	if !ok {
		t.Fatal("reference_video 变体下隔离草稿路径应可解析")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func reviewLinePtr(v int) *int { return &v }

// --- 夹具 ---

func routerDramaStep1() map[string]any {
	return map[string]any{
		"title": "第一集",
		"scenes": []any{map[string]any{
			"scene_id":            "E1S01",
			"duration_seconds":    float64(8),
			"segment_break":       false,
			"characters_in_scene": []any{"阿离"},
			"scenes":              []any{},
			"props":               []any{},
			"scene_description":   "雨夜，阿离立于屋檐下",
			"utterances": []any{
				map[string]any{"kind": "voiceover", "speaker": nil, "text": "三年后。"},
				map[string]any{"kind": "dialogue", "speaker": "阿离", "text": "你终于回来了。"},
			},
			"source_text": "三年后，阿离立于屋檐下：你终于回来了。",
		}},
	}
}

func routerRVStep1() map[string]any {
	return map[string]any{
		"units": []any{map[string]any{
			"unit_id":          "E1U01",
			"shots":            []any{map[string]any{"text": "@[阿离] 立于屋檐下。"}},
			"duration_seconds": float64(4),
			"references":       []any{map[string]any{"type": "character", "name": "阿离"}},
		}},
	}
}

// routerPendingDramaStep1Scene / routerPendingDramaScene 是 pending-authoring 用例的
// step1 / 官方剧本场景夹具（同源正文，step1 带场景描述、剧本带视觉层）。
func routerPendingDramaStep1Scene(sceneID, text string) map[string]any {
	return map[string]any{
		"scene_id":            sceneID,
		"duration_seconds":    float64(8),
		"segment_break":       false,
		"characters_in_scene": []any{"阿离"},
		"scenes":              []any{},
		"props":               []any{},
		"utterances": []any{
			map[string]any{"kind": "voiceover", "speaker": nil, "text": "三年后。"},
			map[string]any{"kind": "dialogue", "speaker": "阿离", "text": text},
		},
		"source_text":       "三年后，阿离立于屋檐下：你终于回来了。",
		"scene_description": "雨夜，阿离立于屋檐下",
	}
}

func routerPendingDramaScene(sceneID, text string) map[string]any {
	return map[string]any{
		"scene_id":            sceneID,
		"duration_seconds":    float64(8),
		"segment_break":       false,
		"characters_in_scene": []any{"阿离"},
		"scenes":              []any{},
		"props":               []any{},
		"utterances": []any{
			map[string]any{"kind": "voiceover", "speaker": nil, "text": "三年后。"},
			map[string]any{"kind": "dialogue", "speaker": "阿离", "text": text},
		},
		"source_text":  "三年后，阿离立于屋檐下：你终于回来了。",
		"image_prompt": map[string]any{"scene": "雨夜屋檐"},
		"video_prompt": map[string]any{"action": "定格"},
	}
}

func writeRouterStep1File(t *testing.T, dir, filename string, content map[string]any) {
	t.Helper()
	writeRouterJSONFile(t, filepath.Join(dir, "drafts", "episode_1", filename), content)
}

func writeRouterScriptFile(t *testing.T, dir string, content map[string]any) {
	t.Helper()
	writeRouterJSONFile(t, filepath.Join(dir, "scripts", "episode_1.json"), content)
}

func writeRouterJSONFile(t *testing.T, path string, content map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRouterSourceText(t *testing.T, dir, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source", "episode_1.txt"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- 请求与断言辅助 ---

func reviewReq(
	t *testing.T, env *reviewRouterEnv, method, path string, body any, headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = strings.NewReader(string(raw))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	env.r.ServeHTTP(rec, req)
	return rec
}

func decodeReviewJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body.String(), err)
	}
	return out
}

// reviewViolationList 取 quarantine.violations 的对象条目列表。
func reviewViolationList(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	quarantine, ok := body["quarantine"].(map[string]any)
	if !ok {
		t.Fatalf("quarantine 不是对象: %T", body["quarantine"])
	}
	rawList, ok := quarantine["violations"].([]any)
	if !ok {
		t.Fatalf("quarantine.violations 不是数组: %T", quarantine["violations"])
	}
	out := make([]map[string]any, 0, len(rawList))
	for index, raw := range rawList {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("violations[%d] 不是对象", index)
		}
		out = append(out, item)
	}
	return out
}

// reviewFloatInts 把 JSON 数组还原成 int 列表（数字经反序列化为 float64）。
func reviewFloatInts(t *testing.T, raw any) []int {
	t.Helper()
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("期望数组: %T", raw)
	}
	out := make([]int, 0, len(list))
	for _, item := range list {
		value, ok := item.(float64)
		if !ok {
			t.Fatalf("期望数字: %T", item)
		}
		out = append(out, int(value))
	}
	return out
}

func reviewBodyFingerprint(t *testing.T, body map[string]any) string {
	t.Helper()
	value, ok := body["fingerprint"].(string)
	if !ok {
		t.Fatalf("fingerprint 缺失: %v", body["fingerprint"])
	}
	return value
}

// reviewRVShotText 取 body.content.units[unitIndex].shots[shotIndex].text。
func reviewRVShotText(t *testing.T, body map[string]any, unitIndex, shotIndex int) string {
	t.Helper()
	content, ok := body["content"].(map[string]any)
	if !ok {
		t.Fatalf("content 不是对象: %T", body["content"])
	}
	units, ok := content["units"].([]any)
	if !ok || unitIndex >= len(units) {
		t.Fatalf("content.units[%d] 不可用", unitIndex)
	}
	unit, ok := units[unitIndex].(map[string]any)
	if !ok {
		t.Fatalf("content.units[%d] 不是对象", unitIndex)
	}
	shots, ok := unit["shots"].([]any)
	if !ok || shotIndex >= len(shots) {
		t.Fatalf("units[%d].shots[%d] 不可用", unitIndex, shotIndex)
	}
	shot, ok := shots[shotIndex].(map[string]any)
	if !ok {
		t.Fatalf("units[%d].shots[%d] 不是对象", unitIndex, shotIndex)
	}
	text, _ := shot["text"].(string)
	return text
}

// --- drama 路由 ---

func TestScriptReviewRouterGateFlow(t *testing.T) {
	env := newReviewRouterEnv(t, domain.GenerationStoryboard, &reviewCapsStub{})
	base := "/api/v1/projects/demo/episodes/1/script-review"

	// step1 未产出。
	rec := reviewReq(t, env, http.MethodGet, base, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("no_step1 GET = %d body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeReviewJSON(t, rec); body["status"] != "no_step1" {
		t.Fatalf("step1 未产出时状态 = %v", body["status"])
	}

	// step1 产出 → pending_review，结构化内容可见。
	writeRouterStep1File(t, env.dir, "step1_normalized_script.json", routerDramaStep1())
	rec = reviewReq(t, env, http.MethodGet, base, nil, nil)
	body := decodeReviewJSON(t, rec)
	if body["status"] != "pending_review" {
		t.Fatalf("step1 产出后状态 = %v", body["status"])
	}
	scene := body["content"].(map[string]any)["scenes"].([]any)[0].(map[string]any)
	speaker := scene["utterances"].([]any)[1].(map[string]any)["speaker"]
	if speaker != "阿离" {
		t.Fatalf("utterances[1].speaker = %v", speaker)
	}

	// 确认前 step2 被阻塞；确认 → confirmed，step2 放行。
	if !env.gateBlocks(t) {
		t.Fatal("pending 状态应阻塞 step2")
	}
	rec = reviewReq(t, env, http.MethodPost, base+"/confirm", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm = %d body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeReviewJSON(t, rec); body["status"] != "confirmed" {
		t.Fatalf("确认后状态 = %v", body["status"])
	}
	if env.gateBlocks(t) {
		t.Fatal("confirmed 状态应放行 step2")
	}
}

func TestScriptReviewRouterEditContentRepends(t *testing.T) {
	env := newReviewRouterEnv(t, domain.GenerationStoryboard, &reviewCapsStub{})
	base := "/api/v1/projects/demo/episodes/1/script-review"
	writeRouterStep1File(t, env.dir, "step1_normalized_script.json", routerDramaStep1())
	reviewReq(t, env, http.MethodPost, base+"/confirm", nil, nil)

	edited := routerDramaStep1()
	edited["scenes"].([]any)[0].(map[string]any)["utterances"].([]any)[1].(map[string]any)["text"] = "你怎么才回来。"
	rec := reviewReq(t, env, http.MethodPut, base+"/content", edited, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeReviewJSON(t, rec); body["status"] != "pending_review" {
		t.Fatalf("编辑保存后状态 = %v", body["status"])
	}

	rec = reviewReq(t, env, http.MethodGet, base, nil, nil)
	body := decodeReviewJSON(t, rec)
	scene := body["content"].(map[string]any)["scenes"].([]any)[0].(map[string]any)
	if text := scene["utterances"].([]any)[1].(map[string]any)["text"]; text != "你怎么才回来。" {
		t.Fatalf("utterances[1].text = %v", text)
	}
}

func TestScriptReviewRouterPutInvalidContent422(t *testing.T) {
	env := newReviewRouterEnv(t, domain.GenerationStoryboard, &reviewCapsStub{})
	base := "/api/v1/projects/demo/episodes/1/script-review"
	writeRouterStep1File(t, env.dir, "step1_normalized_script.json", routerDramaStep1())

	bad := routerDramaStep1()
	bad["scenes"].([]any)[0].(map[string]any)["utterances"].([]any)[1] = map[string]any{
		"kind": "dialogue", "speaker": nil, "text": "无人",
	}
	rec := reviewReq(t, env, http.MethodPut, base+"/content", bad, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid PUT = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestScriptReviewRouterConfirmWithoutStep1Is409(t *testing.T) {
	env := newReviewRouterEnv(t, domain.GenerationStoryboard, &reviewCapsStub{})
	rec := reviewReq(
		t, env, http.MethodPost, "/api/v1/projects/demo/episodes/1/script-review/confirm", nil, nil,
	)
	if rec.Code != http.StatusConflict {
		t.Fatalf("confirm without step1 = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestScriptReviewRouterUnregisteredEpisodeIs404(t *testing.T) {
	// 未在 project.json 登记的分集 → 404，而非误报 no_step1 的 200。
	env := newReviewRouterEnv(t, domain.GenerationStoryboard, &reviewCapsStub{})
	rec := reviewReq(t, env, http.MethodGet, "/api/v1/projects/demo/episodes/99/script-review", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unregistered episode = %d body=%s", rec.Code, rec.Body.String())
	}
}

// --- reference_video 路由 ---

func TestScriptReviewRVRouterGateFlow(t *testing.T) {
	// rv 走同一 HTTP gate：结构化 units 可读、可编辑、web 确认放行 step2。
	env := newReviewRouterEnv(t, domain.GenerationReferenceVideo, &reviewCapsStub{})
	base := "/api/v1/projects/demo/episodes/1/script-review"

	rec := reviewReq(t, env, http.MethodGet, base, nil, nil)
	body := decodeReviewJSON(t, rec)
	if body["status"] != "no_step1" || body["quarantine"] != nil {
		t.Fatalf("no_step1 响应 = %v", body)
	}

	writeRouterStep1File(t, env.dir, service.ReferenceVideoStep1Filename, routerRVStep1())
	rec = reviewReq(t, env, http.MethodGet, base, nil, nil)
	body = decodeReviewJSON(t, rec)
	if body["status"] != "pending_review" || body["quarantine"] != nil {
		t.Fatalf("pending 响应 = %v", body)
	}
	unit := body["content"].(map[string]any)["units"].([]any)[0].(map[string]any)
	if unit["unit_id"] != "E1U01" {
		t.Fatalf("unit_id = %v", unit["unit_id"])
	}
	if !env.gateBlocks(t) {
		t.Fatal("pending 状态应阻塞 step2")
	}

	// 编辑 shot 文本 → 重新待审。
	edited := routerRVStep1()
	edited["units"].([]any)[0].(map[string]any)["shots"].([]any)[0].(map[string]any)["text"] = "@[阿离] 转身离去。"
	rec = reviewReq(t, env, http.MethodPut, base+"/content", edited, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeReviewJSON(t, rec); body["status"] != "pending_review" {
		t.Fatalf("编辑保存后状态 = %v", body["status"])
	}

	rec = reviewReq(t, env, http.MethodPost, base+"/confirm", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm = %d body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeReviewJSON(t, rec); body["status"] != "confirmed" {
		t.Fatalf("确认后状态 = %v", body["status"])
	}
	if env.gateBlocks(t) {
		t.Fatal("confirmed 状态应放行 step2")
	}
}

func TestScriptReviewRVQuarantineSurfacedWithRecomputedViolations(t *testing.T) {
	// 隔离草稿在场时 GET 附带 quarantine 字段：违约按产出时那套校验器读时重算，
	// 不信任草稿里上一轮的快照（快照消息故意写成 "stale"）。
	env := newReviewRouterEnv(t, domain.GenerationReferenceVideo, &reviewCapsStub{
		view: config.VideoCapabilityView{
			ProviderID: "custom-acme", ModelID: "acme-video", SupportedDurations: []int{4, 6, 8},
		},
	})
	novel := "阿离站在屋檐下。"
	writeRouterSourceText(t, env.dir, novel)

	flatUnits := []any{map[string]any{
		"duration_seconds": float64(4), "source_text": novel,
		"text": "镜头1：门开了\n@[阿离]：｛我来了。｝",
	}}
	if _, err := servicereference.WriteQuarantine(env.dir, 1, servicereference.QuarantineKindStep1,
		map[string]any{"units": flatUnits},
		[]servicereference.DraftViolation{{
			Code: "fullwidth_braces", Label: "unit E1U01", Message: "stale", Line: reviewLinePtr(1),
		}},
		map[string]any{"source": "source/episode_1.txt"}, nil,
	); err != nil {
		t.Fatal(err)
	}

	base := "/api/v1/projects/demo/episodes/1/script-review"
	rec := reviewReq(t, env, http.MethodGet, base, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeReviewJSON(t, rec)
	if body["status"] != "pending_review" {
		t.Fatalf("隔离态状态 = %v", body["status"])
	}
	violations := reviewViolationList(t, body)
	if len(violations) != 1 {
		t.Fatalf("violations = %v，期望恰一条", violations)
	}
	if violations[0]["code"] != "fullwidth_braces" {
		t.Fatalf("code = %v", violations[0]["code"])
	}
	if line, _ := violations[0]["line"].(float64); int(line) != 1 {
		t.Fatalf("line = %v", violations[0]["line"])
	}
	if message, _ := violations[0]["message"].(string); message == "stale" {
		t.Fatal("message 不应取草稿快照")
	}

	// 隔离草稿在场时确认被拒：正式 step1 还没有一份可放行的内容。
	rec = reviewReq(t, env, http.MethodPost, base+"/confirm", nil, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("隔离态 confirm = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestScriptReviewRVQuarantineSchemaInvalidKeepsRawContent(t *testing.T) {
	// 草稿 units 被改成非数组：违约报 schema_invalid，content 原样回传（不做收编）。
	env := newReviewRouterEnv(t, domain.GenerationReferenceVideo, &reviewCapsStub{
		view: config.VideoCapabilityView{
			ProviderID: "custom-acme", ModelID: "acme-video", SupportedDurations: []int{4, 6, 8},
		},
	})
	writeRouterSourceText(t, env.dir, "阿离站在屋檐下。")
	if _, err := servicereference.WriteQuarantine(env.dir, 1, servicereference.QuarantineKindStep1,
		map[string]any{"units": "被改坏了"},
		[]servicereference.DraftViolation{{Code: "schema_invalid", Message: "stale"}},
		map[string]any{"source": "source/episode_1.txt"}, nil,
	); err != nil {
		t.Fatal(err)
	}

	rec := reviewReq(t, env, http.MethodGet, "/api/v1/projects/demo/episodes/1/script-review", nil, nil)
	body := decodeReviewJSON(t, rec)
	quarantine, ok := body["quarantine"].(map[string]any)
	if !ok {
		t.Fatalf("quarantine 不是对象: %T", body["quarantine"])
	}
	if got := quarantine["content"].(map[string]any)["units"]; got != "被改坏了" {
		t.Fatalf("content.units = %v，期望原样回传", got)
	}
	violations := reviewViolationList(t, body)
	if len(violations) != 1 || violations[0]["code"] != "schema_invalid" {
		t.Fatalf("violations = %v", violations)
	}
	if message, _ := violations[0]["message"].(string); message == "stale" {
		t.Fatal("message 不应取草稿快照")
	}
}

func TestScriptReviewRVQuarantineMetaBrokenReportsRecomputeFailure(t *testing.T) {
	// meta.source 缺失 → 无从重算：报「无法重算」本身，而不是退回草稿里那份上一轮快照。
	env := newReviewRouterEnv(t, domain.GenerationReferenceVideo, &reviewCapsStub{})
	if _, err := servicereference.WriteQuarantine(env.dir, 1, servicereference.QuarantineKindStep1,
		map[string]any{"units": []any{map[string]any{
			"duration_seconds": float64(4), "source_text": "原文", "text": "镜头1：门开了",
		}}},
		[]servicereference.DraftViolation{{
			Code: "fullwidth_braces", Label: "unit E1U01", Message: "stale", Line: reviewLinePtr(1),
		}},
		map[string]any{}, nil,
	); err != nil {
		t.Fatal(err)
	}

	rec := reviewReq(t, env, http.MethodGet, "/api/v1/projects/demo/episodes/1/script-review", nil, nil)
	body := decodeReviewJSON(t, rec)
	violations := reviewViolationList(t, body)
	if len(violations) != 1 || violations[0]["code"] != "quarantine_unreadable" {
		t.Fatalf("violations = %v", violations)
	}
	if message, _ := violations[0]["message"].(string); strings.Contains(message, "stale") {
		t.Fatalf("message 不应含快照文本: %q", message)
	}
	if violations[0]["label"] != "" {
		t.Fatalf("label = %v，期望空", violations[0]["label"])
	}
}

func TestScriptReviewRVSupportedDurationsExposedForReferenceVideoOnly(t *testing.T) {
	// rv 变体的 GET 带出档位表供 web 渲染时长选择；drama 变体下为 null。
	rvEnv := newReviewRouterEnv(t, domain.GenerationReferenceVideo, &reviewCapsStub{})
	writeRouterStep1File(t, rvEnv.dir, service.ReferenceVideoStep1Filename, routerRVStep1())
	rec := reviewReq(t, rvEnv, http.MethodGet, "/api/v1/projects/demo/episodes/1/script-review", nil, nil)
	body := decodeReviewJSON(t, rec)
	if raw := body["supported_durations"]; raw != nil {
		reviewFloatInts(t, raw) // 非空时必须是整数列表
	}
	// duration_tiers 字段始终存在（未收窄或无法解析型号时为 null），供前端区分「未收窄全集」
	// 与「收窄后的逐 unit 生效档位」——不能靠缺键兜底。
	if _, ok := body["duration_tiers"]; !ok {
		t.Fatal("rv GET 响应缺 duration_tiers 字段")
	}

	dramaEnv := newReviewRouterEnv(t, domain.GenerationStoryboard, &reviewCapsStub{})
	writeRouterStep1File(t, dramaEnv.dir, "step1_normalized_script.json", routerDramaStep1())
	rec = reviewReq(t, dramaEnv, http.MethodGet, "/api/v1/projects/demo/episodes/1/script-review", nil, nil)
	body = decodeReviewJSON(t, rec)
	if body["supported_durations"] != nil {
		t.Fatalf("drama supported_durations = %v，期望 null", body["supported_durations"])
	}
	if body["duration_tiers"] != nil {
		t.Fatalf("drama duration_tiers = %v，期望 null", body["duration_tiers"])
	}
}

func TestScriptReviewRVQuarantineCorruptedEnvelopeReported(t *testing.T) {
	// 隔离草稿文件存在但信封本身损坏（非法 JSON）：GET 不能把这等同于「无隔离草稿」——
	// 那会让面板显示干净态、放行确认，而 confirm 仍会按文件存在性 409。
	env := newReviewRouterEnv(t, domain.GenerationReferenceVideo, &reviewCapsStub{})
	writeRouterStep1File(t, env.dir, service.ReferenceVideoStep1Filename, routerRVStep1())
	if err := os.WriteFile(env.reviewQuarantinePath(t), []byte("{ 这不是合法 JSON"), 0o644); err != nil {
		t.Fatal(err)
	}

	base := "/api/v1/projects/demo/episodes/1/script-review"
	rec := reviewReq(t, env, http.MethodGet, base, nil, nil)
	body := decodeReviewJSON(t, rec)
	quarantine, ok := body["quarantine"].(map[string]any)
	if !ok {
		t.Fatalf("quarantine 不是对象: %T", body["quarantine"])
	}
	if quarantine["content"] != nil {
		t.Fatalf("content = %v，期望 null", quarantine["content"])
	}
	violations := reviewViolationList(t, body)
	if len(violations) != 1 || violations[0]["code"] != "quarantine_unreadable" {
		t.Fatalf("violations = %v", violations)
	}

	rec = reviewReq(t, env, http.MethodPost, base+"/confirm", nil, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("confirm = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestScriptReviewRVFutureQuarantineSchemaReported(t *testing.T) {
	// 未来版本的隔离草稿应显示可操作诊断，而不是让审阅接口返回 500。
	env := newReviewRouterEnv(t, domain.GenerationReferenceVideo, &reviewCapsStub{})
	writeRouterStep1File(t, env.dir, service.ReferenceVideoStep1Filename, routerRVStep1())
	envelope := map[string]any{
		"schema_version": servicereference.QuarantineSchemaVersion + 1,
		"kind":           servicereference.QuarantineKindStep1,
		"episode":        1,
		"content":        map[string]any{"units": []any{}},
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.reviewQuarantinePath(t), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	base := "/api/v1/projects/demo/episodes/1/script-review"
	rec := reviewReq(t, env, http.MethodGet, base, nil, map[string]string{"Accept-Language": "en"})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeReviewJSON(t, rec)
	quarantine, ok := body["quarantine"].(map[string]any)
	if !ok {
		t.Fatalf("quarantine 不是对象: %T", body["quarantine"])
	}
	if quarantine["content"] != nil {
		t.Fatalf("content = %v，期望 null", quarantine["content"])
	}
	violations := reviewViolationList(t, body)
	if len(violations) != 1 || violations[0]["code"] != "quarantine_schema_unsupported" {
		t.Fatalf("violations = %v", violations)
	}
	if version, _ := violations[0]["schema_version"].(float64); int(version) != servicereference.QuarantineSchemaVersion+1 {
		t.Fatalf("schema_version = %v", violations[0]["schema_version"])
	}
	if message, _ := violations[0]["message"].(string); !strings.Contains(message, "unsupported schema version") {
		t.Fatalf("message = %q", message)
	}

	rec = reviewReq(t, env, http.MethodPost, base+"/confirm", nil, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("confirm = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestScriptReviewRVQuarantineUnreadableMessageLocalizedByAcceptLanguage(t *testing.T) {
	// quarantine_unreadable 违约的 message 走 i18n 按 Accept-Language 本地化；其它违约 code 的
	// message 是产出时渲染好插值的中文模板，不做本地化。
	env := newReviewRouterEnv(t, domain.GenerationReferenceVideo, &reviewCapsStub{})
	writeRouterStep1File(t, env.dir, service.ReferenceVideoStep1Filename, routerRVStep1())
	if err := os.WriteFile(env.reviewQuarantinePath(t), []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := reviewReq(
		t, env, http.MethodGet, "/api/v1/projects/demo/episodes/1/script-review", nil,
		map[string]string{"Accept-Language": "en"},
	)
	violations := reviewViolationList(t, decodeReviewJSON(t, rec))
	if len(violations) != 1 {
		t.Fatalf("violations = %v", violations)
	}
	want := "The quarantined draft file is corrupted or malformed and can't be read; " +
		"ask the agent to re-split this episode"
	if message, _ := violations[0]["message"].(string); message != want {
		t.Fatalf("message = %q，期望 %q", message, want)
	}
}

func TestScriptReviewRVDurationTiersSurviveSaveAndConfirmResponses(t *testing.T) {
	// PUT / confirm 的响应同样带 duration_tiers：前端 adopt() 用保存后的响应覆盖 GET 读到的
	// 收窄结果，不带的话会退回未收窄的 supported_durations。
	env := newReviewRouterEnv(t, domain.GenerationReferenceVideo, &reviewCapsStub{})
	writeRouterStep1File(t, env.dir, service.ReferenceVideoStep1Filename, routerRVStep1())
	base := "/api/v1/projects/demo/episodes/1/script-review"

	rec := reviewReq(t, env, http.MethodGet, base, nil, nil)
	if _, ok := decodeReviewJSON(t, rec)["duration_tiers"]; !ok {
		t.Fatal("GET 响应缺 duration_tiers 字段")
	}
	rec = reviewReq(t, env, http.MethodPut, base+"/content", routerRVStep1(), nil)
	if _, ok := decodeReviewJSON(t, rec)["duration_tiers"]; !ok {
		t.Fatal("PUT 响应缺 duration_tiers 字段")
	}
	rec = reviewReq(t, env, http.MethodPost, base+"/confirm", nil, nil)
	if _, ok := decodeReviewJSON(t, rec)["duration_tiers"]; !ok {
		t.Fatal("confirm 响应缺 duration_tiers 字段")
	}
}

func TestScriptReviewRVDurationTiersResolvedForCustomProvider(t *testing.T) {
	// 自定义供应商不在 registry：caps 是它唯一的档位来源；supported_durations（未收窄全集）
	// 与 duration_tiers（收窄后的逐 unit 可选项）都要经 caps 解析出真实档位。
	env := newReviewRouterEnv(t, domain.GenerationReferenceVideo, &reviewCapsStub{
		view: config.VideoCapabilityView{
			ProviderID: "custom-acme", ModelID: "acme-video", SupportedDurations: []int{5, 10},
		},
	})
	writeRouterStep1File(t, env.dir, service.ReferenceVideoStep1Filename, routerRVStep1())

	rec := reviewReq(t, env, http.MethodGet, "/api/v1/projects/demo/episodes/1/script-review", nil, nil)
	body := decodeReviewJSON(t, rec)
	if got := reviewFloatInts(t, body["supported_durations"]); !reflect.DeepEqual(got, []int{5, 10}) {
		t.Fatalf("supported_durations = %v", got)
	}
	tiers, ok := body["duration_tiers"].(map[string]any)
	if !ok {
		t.Fatalf("duration_tiers 不是对象: %T", body["duration_tiers"])
	}
	if got := reviewFloatInts(t, tiers["with_references"]); !reflect.DeepEqual(got, []int{5, 10}) {
		t.Fatalf("with_references = %v", got)
	}
	if got := reviewFloatInts(t, tiers["without_references"]); !reflect.DeepEqual(got, []int{5, 10}) {
		t.Fatalf("without_references = %v", got)
	}
}

func TestScriptReviewRVQuarantineNonStringMetaSourceDegrades(t *testing.T) {
	// meta.source 被改成非字符串：重算链路按「无法重算」降级，不让类型错误冒穿成 500。
	env := newReviewRouterEnv(t, domain.GenerationReferenceVideo, &reviewCapsStub{})
	if _, err := servicereference.WriteQuarantine(env.dir, 1, servicereference.QuarantineKindStep1,
		map[string]any{"units": []any{map[string]any{
			"duration_seconds": float64(4), "source_text": "x", "text": "镜头1：门开了",
		}}},
		nil,
		map[string]any{"source": 12345}, nil,
	); err != nil {
		t.Fatal(err)
	}

	rec := reviewReq(t, env, http.MethodGet, "/api/v1/projects/demo/episodes/1/script-review", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d body=%s", rec.Code, rec.Body.String())
	}
	violations := reviewViolationList(t, decodeReviewJSON(t, rec))
	if len(violations) != 1 || violations[0]["code"] != "quarantine_unreadable" {
		t.Fatalf("violations = %v", violations)
	}
}

func TestScriptReviewRVQuarantineDirectoryMetaSourceDegrades(t *testing.T) {
	// meta.source 是字符串但指向一个目录：同样降级成 quarantine_unreadable，不让读目录的
	// OSError 子类冒穿成 500。
	env := newReviewRouterEnv(t, domain.GenerationReferenceVideo, &reviewCapsStub{})
	if err := os.MkdirAll(filepath.Join(env.dir, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := servicereference.WriteQuarantine(env.dir, 1, servicereference.QuarantineKindStep1,
		map[string]any{"units": []any{map[string]any{
			"duration_seconds": float64(4), "source_text": "x", "text": "镜头1：门开了",
		}}},
		nil,
		map[string]any{"source": "source"}, nil,
	); err != nil {
		t.Fatal(err)
	}

	rec := reviewReq(t, env, http.MethodGet, "/api/v1/projects/demo/episodes/1/script-review", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d body=%s", rec.Code, rec.Body.String())
	}
	violations := reviewViolationList(t, decodeReviewJSON(t, rec))
	if len(violations) != 1 || violations[0]["code"] != "quarantine_unreadable" {
		t.Fatalf("violations = %v", violations)
	}
}

func TestScriptReviewRVPutResponseIncludesQuarantine(t *testing.T) {
	// 保存作用于正式草稿，隔离草稿是另一份文件：PUT 响应缺 quarantine 字段的话，面板 adopt()
	// 会把它当成「无隔离草稿」而放行确认（用「保存时隔离草稿已存在」模拟保存在途产出的时序）。
	env := newReviewRouterEnv(t, domain.GenerationReferenceVideo, &reviewCapsStub{
		view: config.VideoCapabilityView{
			ProviderID: "custom-acme", ModelID: "acme-video", SupportedDurations: []int{4, 6, 8},
		},
	})
	writeRouterSourceText(t, env.dir, "阿离站在屋檐下。")
	writeRouterStep1File(t, env.dir, service.ReferenceVideoStep1Filename, routerRVStep1())
	if _, err := servicereference.WriteQuarantine(env.dir, 1, servicereference.QuarantineKindStep1,
		map[string]any{"units": []any{map[string]any{
			"duration_seconds": float64(4), "source_text": "x", "text": "镜头1：门开了",
		}}},
		[]servicereference.DraftViolation{{Code: "empty_text", Label: "unit E1U01", Message: "坏"}},
		map[string]any{"source": "source/episode_1.txt"}, nil,
	); err != nil {
		t.Fatal(err)
	}

	rec := reviewReq(
		t, env, http.MethodPut, "/api/v1/projects/demo/episodes/1/script-review/content",
		routerRVStep1(), nil,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d body=%s", rec.Code, rec.Body.String())
	}
	violations := reviewViolationList(t, decodeReviewJSON(t, rec))
	if len(violations) == 0 {
		t.Fatal("PUT 响应应带隔离草稿的违约条目")
	}
	// meta.source 完整、重算能正常跑：断言到的是重算算出的真实违约，不是 meta 缺失时
	// 降级出的 quarantine_unreadable 兜底条目。
	for _, violation := range violations {
		if violation["code"] == "quarantine_unreadable" {
			t.Fatalf("violations 不应含兜底条目: %v", violations)
		}
	}
}

func TestScriptReviewRVPutStaleBaseFingerprintConflicts(t *testing.T) {
	// PUT 携带的 base_fingerprint 与盘上现值不一致（编辑期间另一方已保存）→ 409、不落盘；
	// 拿最新指纹重试放行。缺省不带指纹的调用维持原语义（不比对）。
	env := newReviewRouterEnv(t, domain.GenerationReferenceVideo, &reviewCapsStub{})
	writeRouterStep1File(t, env.dir, service.ReferenceVideoStep1Filename, routerRVStep1())
	base := "/api/v1/projects/demo/episodes/1/script-review"

	stale := reviewBodyFingerprint(t, decodeReviewJSON(t, reviewReq(t, env, http.MethodGet, base, nil, nil)))

	other := routerRVStep1()
	other["units"].([]any)[0].(map[string]any)["shots"].([]any)[0].(map[string]any)["text"] = "@[阿离] 转身离开。"
	if rec := reviewReq(t, env, http.MethodPut, base+"/content", other, nil); rec.Code != http.StatusOK {
		t.Fatalf("先写方 PUT = %d body=%s", rec.Code, rec.Body.String())
	}

	mine := routerRVStep1()
	rec := reviewReq(t, env, http.MethodPut, base+"/content?base_fingerprint="+stale, mine, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale PUT = %d body=%s", rec.Code, rec.Body.String())
	}
	// 冲突未覆盖：盘上仍是另一方保存的内容。
	body := decodeReviewJSON(t, reviewReq(t, env, http.MethodGet, base, nil, nil))
	if text := reviewRVShotText(t, body, 0, 0); text != "@[阿离] 转身离开。" {
		t.Fatalf("冲突后 shots[0].text = %q", text)
	}

	fresh := reviewBodyFingerprint(t, decodeReviewJSON(t, reviewReq(t, env, http.MethodGet, base, nil, nil)))
	rec = reviewReq(t, env, http.MethodPut, base+"/content?base_fingerprint="+fresh, mine, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh PUT = %d body=%s", rec.Code, rec.Body.String())
	}
	if text := reviewRVShotText(t, decodeReviewJSON(t, rec), 0, 0); text != "@[阿离] 立于屋檐下。" {
		t.Fatalf("重试后 shots[0].text = %q", text)
	}
}

// --- pending-authoring 路由 ---

func TestPendingAuthoringRouterReportsStaleItems(t *testing.T) {
	env := newReviewRouterEnv(t, domain.GenerationStoryboard, &reviewCapsStub{})
	writeRouterStep1File(t, env.dir, "step1_normalized_script.json", map[string]any{
		"title": "第一集", "scenes": []any{routerPendingDramaStep1Scene("E1S01", "改了")},
	})
	writeRouterScriptFile(t, env.dir, map[string]any{
		"title": "第一集", "scenes": []any{routerPendingDramaScene("E1S01", "你终于回来了。")},
	})

	rec := reviewReq(t, env, http.MethodGet, "/api/v1/projects/demo/episodes/1/pending-authoring", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeReviewJSON(t, rec)
	want := map[string]any{
		"applicable":     true,
		"kind":           "drama",
		"script_present": true,
		"has_pending":    true,
		"items":          []any{map[string]any{"item_id": "E1S01", "reason": "stale_visual"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("body = %v，期望 %v", got, want)
	}
}

func TestPendingAuthoringRouterCleanEpisodeReportsNoItems(t *testing.T) {
	env := newReviewRouterEnv(t, domain.GenerationStoryboard, &reviewCapsStub{})
	writeRouterStep1File(t, env.dir, "step1_normalized_script.json", map[string]any{
		"title": "第一集", "scenes": []any{routerPendingDramaStep1Scene("E1S01", "你终于回来了。")},
	})
	writeRouterScriptFile(t, env.dir, map[string]any{
		"title": "第一集", "scenes": []any{routerPendingDramaScene("E1S01", "你终于回来了。")},
	})

	rec := reviewReq(t, env, http.MethodGet, "/api/v1/projects/demo/episodes/1/pending-authoring", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeReviewJSON(t, rec)
	if items, ok := body["items"].([]any); !ok || len(items) != 0 {
		t.Fatalf("items = %v，期望空", body["items"])
	}
	if body["has_pending"] != false {
		t.Fatalf("has_pending = %v", body["has_pending"])
	}
}

func TestPendingAuthoringRouterUnknownProjectIs404(t *testing.T) {
	env := newReviewRouterEnv(t, domain.GenerationStoryboard, &reviewCapsStub{})
	rec := reviewReq(
		t, env, http.MethodGet, "/api/v1/projects/missing/episodes/1/pending-authoring", nil, nil,
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown project = %d body=%s", rec.Code, rec.Body.String())
	}
}
