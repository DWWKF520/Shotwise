// 参考生视频 unit 路由测试（对应 tests/test_reference_videos.py 的路由面）：
// 路由树注册、分镜文稿预览、生成提交（202 / 资产预检 400 / 能力闸 400 / 404 / 409）与
// 成片上传链路（422 / 400 / 404 / 200 + finalize 写回）。
package generation

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain/media"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit/httpkittest"
)

// --- 替身 ---

// refResolverStub 是参考生视频路由测试的解析替身：capsErr 驱动能力视图解析，
// backendErr 驱动视频后端解析闸（能力视图独立解析，两路互不影响）。
type refResolverStub struct {
	view       config.VideoCapabilityView
	capsErr    error
	backend    config.ProviderModel
	backendErr error
}

func (s *refResolverStub) VideoCapabilitiesForProjectBucket(
	_ context.Context, _ *project.ProjectFile, _ provider.VideoBucket,
) (config.VideoCapabilityView, error) {
	if s.capsErr != nil {
		return config.VideoCapabilityView{}, s.capsErr
	}
	return s.view, nil
}

func (s *refResolverStub) VideoGenerateAudioForProject(_ context.Context, _ *project.ProjectFile) (bool, error) {
	return false, nil
}

func (s *refResolverStub) ResolveResolution(_ context.Context, _ *project.ProjectFile, _, _ string) (string, error) {
	return "", nil
}

func (s *refResolverStub) ResolveVideoBackend(
	_ context.Context, _ *project.ProjectFile, _ map[string]any, _ *provider.VideoBucket,
) (config.ProviderModel, error) {
	if s.backendErr != nil {
		return config.ProviderModel{}, s.backendErr
	}
	return s.backend, nil
}

// refQueueStub 记录入队参数，返回固定任务 ID。
type refQueueStub struct {
	calls []repository.EnqueueParams
	err   error
}

func (q *refQueueStub) EnqueueTask(
	_ context.Context, p repository.EnqueueParams,
) (repository.EnqueueResult, error) {
	q.calls = append(q.calls, p)
	if q.err != nil {
		return repository.EnqueueResult{}, q.err
	}
	return repository.EnqueueResult{TaskID: "task-1"}, nil
}

// --- 环境 ---

// refVideoEnv 是参考生视频路由测试环境：真实 ProjectStore + stub 解析器 / 队列。
type refVideoEnv struct {
	r        *gin.Engine
	store    *service.ProjectStore
	dir      string
	resolver *refResolverStub
	queue    *refQueueStub
}

// newRefVideoEnv 建测试项目（指定内容模式与生成路线）、登记第 1 集与带设计图的角色阿离。
func newRefVideoEnv(
	t *testing.T, contentMode project.ContentMode, generationMode project.GenerationMode,
) *refVideoEnv {
	t.Helper()
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("demo", "Demo", contentMode, generationMode, "Anime"); err != nil {
		t.Fatal(err)
	}
	if err := store.Update("demo", func(pf *project.ProjectFile) error {
		if pf.Characters == nil {
			pf.Characters = project.AssetBucket{}
		}
		pf.Characters["阿离"] = project.WrapAssetEntry(map[string]any{
			"name": "阿离", "description": "少女", "character_sheet": "characters/阿离/sheet.png",
		})
		pf.Episodes = append(pf.Episodes, project.EpisodeMeta{
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
	if err := os.MkdirAll(filepath.Join(dir, "characters", "阿离"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "characters", "阿离", "sheet.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &refVideoEnv{
		store:    store,
		dir:      dir,
		resolver: &refResolverStub{},
		queue:    &refQueueStub{},
	}
	gin.SetMode(gin.TestMode)
	env.r = gin.New()
	RegisterReferenceVideos(env.r.Group("/api/v1"), ReferenceVideoDeps{
		Store: store, Resolver: env.resolver, Queue: env.queue, Hub: service.NewEventHub(),
	})
	return env
}

// dropAilSheet 把阿离的登记改成无设计图（触发 missing_sheet 口径的用例共用）。
func (e *refVideoEnv) dropAilSheet(t *testing.T) {
	t.Helper()
	if err := e.store.Update("demo", func(pf *project.ProjectFile) error {
		pf.Characters["阿离"] = project.WrapAssetEntry(map[string]any{"name": "阿离", "description": "少女"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// --- 夹具 ---

func refNarrationUnit() map[string]any {
	return map[string]any{
		"unit_id":          "E1U01",
		"shots":            []any{map[string]any{"text": "@[阿离] 立于屋檐下。"}},
		"duration_seconds": float64(4),
		"references":       []any{map[string]any{"type": "character", "name": "阿离"}},
		"generated_assets": media.EmptyGeneratedAssets(),
	}
}

func refNarrationScript() map[string]any {
	return map[string]any{"title": "第一集", "video_units": []any{refNarrationUnit()}}
}

func refAdScript() map[string]any {
	return map[string]any{
		"title": "第一集",
		"shots": []any{map[string]any{
			"shot_id":            "E1S01",
			"duration_seconds":   float64(5),
			"characters_in_shot": []any{"阿离"},
			"image_prompt":       map[string]any{"scene": "雨夜屋檐"},
			"video_prompt":       map[string]any{"action": "伫立"},
		}},
		"reference_units": []any{map[string]any{
			"unit_id":          "E1U01",
			"shot_ids":         []any{"E1S01"},
			"references":       []any{map[string]any{"type": "character", "name": "阿离"}},
			"generated_assets": media.EmptyGeneratedAssets(),
		}},
	}
}

func writeRefScript(t *testing.T, env *refVideoEnv, script map[string]any) {
	t.Helper()
	httpkittest.WriteRouterJSONFile(t, filepath.Join(env.dir, "scripts", "episode_1.json"), script)
}

// --- 请求辅助 ---

func refJSONReq(
	t *testing.T, env *refVideoEnv, method, path string, body any,
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
	rec := httptest.NewRecorder()
	env.r.ServeHTTP(rec, req)
	return rec
}

// refUploadReq 构造 multipart 上传请求；filename 为空表示不带 file 字段。
func refUploadReq(
	t *testing.T, env *refVideoEnv, path, filename string, content []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if filename != "" {
		fw, err := w.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	env.r.ServeHTTP(rec, req)
	return rec
}

func decodeRefJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body.String(), err)
	}
	return out
}

func refDetail(t *testing.T, body map[string]any) string {
	t.Helper()
	detail, ok := body["detail"].(string)
	if !ok {
		t.Fatalf("detail 不是字符串: %T", body["detail"])
	}
	return detail
}

// --- 路由注册 ---

// TestRegisterReferenceVideosRoutes 校验路由树能容纳 /units/reorder（静态）与
// /units/:unit_id（参数）同层、以及 :unit_id 下的三条子路径（注册期不 panic 即证明契约可落地）。
func TestRegisterReferenceVideosRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterReferenceVideos(r.Group("/api/v1"), ReferenceVideoDeps{})

	registered := map[string]bool{}
	for _, route := range r.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	base := "/api/v1/projects/:name/reference-videos/episodes/:episode"
	want := []string{
		http.MethodGet + " " + base + "/units",
		http.MethodPost + " " + base + "/derive-units",
		http.MethodPost + " " + base + "/units",
		http.MethodPost + " " + base + "/units/reorder",
		http.MethodPatch + " " + base + "/units/:unit_id",
		http.MethodDelete + " " + base + "/units/:unit_id",
		http.MethodGet + " " + base + "/units/:unit_id/duration-precheck",
		http.MethodPost + " " + base + "/script-preview",
		http.MethodPost + " " + base + "/units/:unit_id/generate",
		http.MethodPost + " " + base + "/units/:unit_id/upload-video",
	}
	for _, route := range want {
		if !registered[route] {
			t.Errorf("route not registered: %s", route)
		}
	}
}

// --- script-preview ---

func TestPreviewReferenceScriptBasic(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentNarration, project.GenerationReferenceVideo)
	writeRefScript(t, env, refNarrationScript())

	rec := refJSONReq(t, env, http.MethodPost,
		"/api/v1/projects/demo/reference-videos/episodes/1/script-preview",
		map[string]any{"prompt": "@[阿离] 立于屋檐下。"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeRefJSON(t, rec)
	shots, ok := body["shots"].([]any)
	if !ok || len(shots) == 0 {
		t.Fatalf("shots 应非空数组: %T", body["shots"])
	}
	first, _ := shots[0].(map[string]any)
	if text, _ := first["text"].(string); strings.TrimSpace(text) == "" {
		t.Errorf("shots[0].text 为空")
	}
	refs, ok := body["references"].([]any)
	if !ok || len(refs) == 0 {
		t.Fatalf("references 应包含已登记的阿离: %T", body["references"])
	}
	if _, ok := body["utterances"].([]any); !ok {
		t.Errorf("utterances 应为数组")
	}
	if _, ok := body["warnings"].([]any); !ok {
		t.Errorf("warnings 应为数组")
	}
}

// TestPreviewReferenceScriptUnregisteredWarning 验证未登记 @提及 只降级为 warning 不阻断预览。
func TestPreviewReferenceScriptUnregisteredWarning(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentNarration, project.GenerationReferenceVideo)
	writeRefScript(t, env, refNarrationScript())

	rec := refJSONReq(t, env, http.MethodPost,
		"/api/v1/projects/demo/reference-videos/episodes/1/script-preview",
		map[string]any{"prompt": "@[幽灵] 出场。"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeRefJSON(t, rec)
	warnings, _ := body["warnings"].([]any)
	found := false
	for _, raw := range warnings {
		w, _ := raw.(map[string]any)
		if key, _ := w["key"].(string); key == "ref_warn_unregistered_mention" {
			found = true
			if message, _ := w["message"].(string); !strings.Contains(message, "幽灵") {
				t.Errorf("warning message 应包含提及名: %q", message)
			}
		}
	}
	if !found {
		t.Errorf("warnings 应含 ref_warn_unregistered_mention: %v", warnings)
	}
}

// --- generate ---

func TestGenerateReferenceUnitNarrationAccepted(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentNarration, project.GenerationReferenceVideo)
	writeRefScript(t, env, refNarrationScript())

	rec := refJSONReq(t, env, http.MethodPost,
		"/api/v1/projects/demo/reference-videos/episodes/1/units/E1U01/generate", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeRefJSON(t, rec)
	if body["task_id"] != "task-1" {
		t.Errorf("task_id = %v", body["task_id"])
	}
	if deduped, _ := body["deduped"].(bool); deduped {
		t.Errorf("deduped 应为 false")
	}
	if len(env.queue.calls) != 1 {
		t.Fatalf("入队次数 = %d", len(env.queue.calls))
	}
	call := env.queue.calls[0]
	if call.TaskType != "reference_video" || call.MediaType != "video" {
		t.Errorf("task_type/media_type = %s/%s", call.TaskType, call.MediaType)
	}
	if call.ResourceID != "E1U01" || call.Source != "webui" {
		t.Errorf("resource_id/source = %s/%s", call.ResourceID, call.Source)
	}
	if call.ScriptFile == nil || *call.ScriptFile != "scripts/episode_1.json" {
		t.Errorf("script_file = %v", call.ScriptFile)
	}
	if prompt, _ := call.Payload["prompt"].(string); prompt != "@[阿离] 立于屋檐下。" {
		t.Errorf("payload.prompt = %q", prompt)
	}
	if scriptFile, _ := call.Payload["script_file"].(string); scriptFile != "scripts/episode_1.json" {
		t.Errorf("payload.script_file = %q", scriptFile)
	}
}

// TestGenerateReferenceUnitNarrationUnregistered 验证 narration 的引用存在性是硬口径：
// 未登记资产在提交期 400，并渲染 preflight 三段文案。
func TestGenerateReferenceUnitNarrationUnregistered(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentNarration, project.GenerationReferenceVideo)
	unit := refNarrationUnit()
	unit["references"] = []any{map[string]any{"type": "character", "name": "幽灵"}}
	writeRefScript(t, env, map[string]any{"title": "第一集", "video_units": []any{unit}})

	rec := refJSONReq(t, env, http.MethodPost,
		"/api/v1/projects/demo/reference-videos/episodes/1/units/E1U01/generate", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	detail := refDetail(t, decodeRefJSON(t, rec))
	if !strings.Contains(detail, "character:幽灵") {
		t.Errorf("detail 应含未登记标签: %q", detail)
	}
	if !strings.Contains(detail, "缺少资产图：—") {
		t.Errorf("detail 的 missing_images 段应为占位符: %q", detail)
	}
	if len(env.queue.calls) != 0 {
		t.Errorf("预检失败不应入队")
	}
}

// TestGenerateReferenceUnitNarrationMissingSheet 验证 narration 缺设计图同样在提交期拒绝。
func TestGenerateReferenceUnitNarrationMissingSheet(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentNarration, project.GenerationReferenceVideo)
	env.dropAilSheet(t)
	writeRefScript(t, env, refNarrationScript())

	rec := refJSONReq(t, env, http.MethodPost,
		"/api/v1/projects/demo/reference-videos/episodes/1/units/E1U01/generate", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	detail := refDetail(t, decodeRefJSON(t, rec))
	if !strings.Contains(detail, "character:阿离") || !strings.Contains(detail, "缺少资产图") {
		t.Errorf("detail 应含缺图标签: %q", detail)
	}
}

// TestGenerateReferenceUnitEmptyPrompt 验证空提示词在提交入口当场 400（与 SDK 入队同守卫口径）。
func TestGenerateReferenceUnitEmptyPrompt(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentNarration, project.GenerationReferenceVideo)
	unit := refNarrationUnit()
	unit["shots"] = []any{map[string]any{"text": "   "}}
	unit["references"] = []any{}
	writeRefScript(t, env, map[string]any{"title": "第一集", "video_units": []any{unit}})

	rec := refJSONReq(t, env, http.MethodPost,
		"/api/v1/projects/demo/reference-videos/episodes/1/units/E1U01/generate", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if detail := refDetail(t, decodeRefJSON(t, rec)); detail != "prompt 不能为空" {
		t.Errorf("detail = %q", detail)
	}
}

// TestGenerateReferenceUnitCapabilityGate 验证解析闸让能力缺失在提交入口返回修复指引。
func TestGenerateReferenceUnitCapabilityGate(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentNarration, project.GenerationReferenceVideo)
	writeRefScript(t, env, refNarrationScript())
	env.resolver.backendErr = &config.VideoBucketCapabilityError{
		Code: provider.FailureVideoCapabilityMissingR2V, Bucket: provider.BucketR2V,
		ProviderID: "prov-x", ModelID: "mdl-x", Message: "cap",
	}

	rec := refJSONReq(t, env, http.MethodPost,
		"/api/v1/projects/demo/reference-videos/episodes/1/units/E1U01/generate", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	detail := refDetail(t, decodeRefJSON(t, rec))
	if !strings.Contains(detail, "prov-x/mdl-x") {
		t.Errorf("detail 应含 provider/model 指引: %q", detail)
	}
	if len(env.queue.calls) != 0 {
		t.Errorf("能力闸失败不应入队")
	}
}

func TestGenerateReferenceUnitNotFound(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentNarration, project.GenerationReferenceVideo)
	writeRefScript(t, env, refNarrationScript())

	rec := refJSONReq(t, env, http.MethodPost,
		"/api/v1/projects/demo/reference-videos/episodes/1/units/E1U99/generate", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if detail := refDetail(t, decodeRefJSON(t, rec)); !strings.Contains(detail, "E1U99") {
		t.Errorf("detail = %q", detail)
	}
}

func TestGenerateReferenceUnitEpisodeNotFound(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentNarration, project.GenerationReferenceVideo)
	writeRefScript(t, env, refNarrationScript())

	rec := refJSONReq(t, env, http.MethodPost,
		"/api/v1/projects/demo/reference-videos/episodes/9/units/E1U01/generate", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestGenerateReferenceUnitNotReferenceMode 验证分镜路线项目在参考生入口被 409 拒绝。
func TestGenerateReferenceUnitNotReferenceMode(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentNarration, project.GenerationStoryboard)
	writeRefScript(t, env, refNarrationScript())

	rec := refJSONReq(t, env, http.MethodPost,
		"/api/v1/projects/demo/reference-videos/episodes/1/units/E1U01/generate", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if detail := refDetail(t, decodeRefJSON(t, rec)); !strings.Contains(detail, "参考生视频模式") {
		t.Errorf("detail = %q", detail)
	}
}

// TestGenerateReferenceUnitAdAccepted 验证 ad 路线按镜头水合渲染守卫 prompt 并入队。
func TestGenerateReferenceUnitAdAccepted(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentAd, project.GenerationReferenceVideo)
	writeRefScript(t, env, refAdScript())

	rec := refJSONReq(t, env, http.MethodPost,
		"/api/v1/projects/demo/reference-videos/episodes/1/units/E1U01/generate", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(env.queue.calls) != 1 {
		t.Fatalf("入队次数 = %d", len(env.queue.calls))
	}
	call := env.queue.calls[0]
	if call.ResourceID != "E1U01" {
		t.Errorf("resource_id = %s", call.ResourceID)
	}
	prompt, _ := call.Payload["prompt"].(string)
	if !strings.HasPrefix(prompt, "Style: Anime") {
		t.Errorf("payload.prompt 应带项目风格头行: %q", prompt)
	}
	if !strings.Contains(prompt, "Shot 1 (5s)") {
		t.Errorf("payload.prompt 应含镜头行: %q", prompt)
	}
}

// TestGenerateReferenceUnitAdMissingSheetSoft 验证 ad 参考集缺图按软口径放行（只拦未登记）。
func TestGenerateReferenceUnitAdMissingSheetSoft(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentAd, project.GenerationReferenceVideo)
	env.dropAilSheet(t)
	writeRefScript(t, env, refAdScript())

	rec := refJSONReq(t, env, http.MethodPost,
		"/api/v1/projects/demo/reference-videos/episodes/1/units/E1U01/generate", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(env.queue.calls) != 1 {
		t.Errorf("ad 缺图应放行入队")
	}
}

// TestGenerateReferenceUnitAdStaleIndex 验证派生索引悬空时 409 提示重新派生。
func TestGenerateReferenceUnitAdStaleIndex(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentAd, project.GenerationReferenceVideo)
	script := refAdScript()
	units, _ := script["reference_units"].([]any)
	unit, _ := units[0].(map[string]any)
	unit["shot_ids"] = []any{"E1SXX"}
	writeRefScript(t, env, script)

	rec := refJSONReq(t, env, http.MethodPost,
		"/api/v1/projects/demo/reference-videos/episodes/1/units/E1U01/generate", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if detail := refDetail(t, decodeRefJSON(t, rec)); !strings.Contains(detail, "重新派生") {
		t.Errorf("detail = %q", detail)
	}
}

// --- upload-video ---

// TestUploadReferenceUnitVideoMissingFile 验证缺 file 字段按 FastAPI 形状 422。
func TestUploadReferenceUnitVideoMissingFile(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentNarration, project.GenerationReferenceVideo)
	writeRefScript(t, env, refNarrationScript())

	rec := refUploadReq(t, env,
		"/api/v1/projects/demo/reference-videos/episodes/1/units/E1U01/upload-video", "", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "file") {
		t.Errorf("body 应指向 file 字段: %s", rec.Body.String())
	}
}

// TestUploadReferenceUnitVideoUnsupportedType 验证非 ISO BMFF 扩展名 400。
func TestUploadReferenceUnitVideoUnsupportedType(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentNarration, project.GenerationReferenceVideo)
	writeRefScript(t, env, refNarrationScript())

	rec := refUploadReq(t, env,
		"/api/v1/projects/demo/reference-videos/episodes/1/units/E1U01/upload-video",
		"a.txt", []byte("data"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	detail := refDetail(t, decodeRefJSON(t, rec))
	if !strings.Contains(detail, ".txt") {
		t.Errorf("detail 应含被拒扩展名: %q", detail)
	}
}

func TestUploadReferenceUnitVideoUnitNotFound(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentNarration, project.GenerationReferenceVideo)
	writeRefScript(t, env, refNarrationScript())

	rec := refUploadReq(t, env,
		"/api/v1/projects/demo/reference-videos/episodes/1/units/E1U99/upload-video",
		"a.mp4", []byte("data"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestUploadReferenceUnitVideoAccepted 覆盖 200 全链路：落盘 → 版本登记 → finalize 写回
// generated_assets → 项目变更事件指纹随响应返回。
func TestUploadReferenceUnitVideoAccepted(t *testing.T) {
	env := newRefVideoEnv(t, project.ContentNarration, project.GenerationReferenceVideo)
	writeRefScript(t, env, refNarrationScript())
	content := []byte("fake-mp4-bytes")

	rec := refUploadReq(t, env,
		"/api/v1/projects/demo/reference-videos/episodes/1/units/E1U01/upload-video",
		"clip.mp4", content)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeRefJSON(t, rec)
	if ok, _ := body["success"].(bool); !ok {
		t.Errorf("success = %v", body["success"])
	}
	if body["path"] != "reference_videos/E1U01.mp4" {
		t.Errorf("path = %v", body["path"])
	}
	if version, _ := body["version"].(float64); version != 1 {
		t.Errorf("version = %v", body["version"])
	}
	if _, ok := body["asset_fingerprints"].(map[string]any); !ok {
		t.Errorf("asset_fingerprints 应为对象: %T", body["asset_fingerprints"])
	}

	written, err := os.ReadFile(filepath.Join(env.dir, "reference_videos", "E1U01.mp4"))
	if err != nil {
		t.Fatalf("成片应落盘: %v", err)
	}
	if !bytes.Equal(written, content) {
		t.Errorf("落盘内容与上传不一致")
	}

	script, err := env.store.ReadScriptJSON("demo", "scripts/episode_1.json")
	if err != nil {
		t.Fatal(err)
	}
	units, _ := script["video_units"].([]any)
	unit, _ := units[0].(map[string]any)
	assets, _ := unit["generated_assets"].(map[string]any)
	if assets["video_clip"] != "reference_videos/E1U01.mp4" {
		t.Errorf("video_clip = %v", assets["video_clip"])
	}
	if assets["status"] != "completed" {
		t.Errorf("status = %v", assets["status"])
	}
}
