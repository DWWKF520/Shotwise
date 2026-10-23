package review

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/shotwise/shotwise/internal/agent/tools"
)

// writeReviewFixture 在临时目录写入 project.json 与 scripts/，并返回项目目录。
func writeReviewFixture(t *testing.T, project map[string]any, scripts map[string]map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	writeReviewJSON(t, filepath.Join(dir, "project.json"), project)
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, script := range scripts {
		writeReviewJSON(t, filepath.Join(dir, "scripts", name), script)
	}
	return dir
}

func writeReviewJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeReviewVideo 写入占位视频文件（内容不是合法视频，仅用于存在性判定）。
func writeReviewVideo(t *testing.T, dir, rel string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("not a real video"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runReviewTool 调用审阅工具并返回多模态结果。
func runReviewTool(t *testing.T, dir, argsJSON string) *schema.ToolResult {
	t.Helper()
	tl, err := NewInspectDirectorReviewTool("demo", &fakeAssetProjects{dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.InvokableRun(context.Background(), &schema.ToolArgument{Text: argsJSON})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// reviewPayload 解析结果中的 JSON 部件（第二个文本部件）。
func reviewPayload(t *testing.T, res *schema.ToolResult) map[string]any {
	t.Helper()
	if len(res.Parts) < 2 {
		t.Fatalf("want summary + json parts, got %d parts", len(res.Parts))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Parts[1].Text), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v\n%s", err, res.Parts[1].Text)
	}
	return payload
}

// reviewUnitByID 在 episodes[0].units 中按 unit_id 查找单元。
func reviewUnitByID(t *testing.T, payload map[string]any, id string) map[string]any {
	t.Helper()
	episodes := payload["episodes"].([]any)
	for _, u := range episodes[0].(map[string]any)["units"].([]any) {
		unit := u.(map[string]any)
		if unit["unit_id"] == id {
			return unit
		}
	}
	t.Fatalf("unit %s not found", id)
	return nil
}

// reviewFindingCodes 收集结果顶层发现项的 code。
func reviewFindingCodes(payload map[string]any) []string {
	var codes []string
	for _, f := range payload["deterministic_findings"].([]any) {
		codes = append(codes, f.(map[string]any)["code"].(string))
	}
	return codes
}

func containsCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

func TestReviewChooseScriptByCurrentEpisode(t *testing.T) {
	project := map[string]any{
		"current_episode": float64(2),
		"episodes": []any{
			map[string]any{"episode": float64(1), "script_file": "scripts/episode_1.json"},
			map[string]any{"episode": float64(2), "script_file": "episode_2.json"},
		},
	}
	got, err := tools.ChooseScript(project, nil)
	if err != nil || got != "episode_2.json" {
		t.Fatalf("chooseScript = %q, %v; want episode_2.json", got, err)
	}

	ambiguous := map[string]any{"episodes": project["episodes"]}
	if _, err := tools.ChooseScript(ambiguous, nil); err == nil || !strings.Contains(err.Error(), "无法确定当前剧集") {
		t.Fatalf("ambiguous project should require script, got %v", err)
	}

	single := map[string]any{"episodes": []any{map[string]any{"script_file": "episode_9.json"}}}
	if got, err := tools.ChooseScript(single, nil); err != nil || got != "episode_9.json" {
		t.Fatalf("single episode = %q, %v", got, err)
	}

	bad := "../secret.json"
	if _, err := tools.ChooseScript(project, &bad); err == nil {
		t.Fatal("requested path with separators must be rejected")
	}
}

func TestReviewRejectsInvalidArguments(t *testing.T) {
	dir := writeReviewFixture(t, map[string]any{"title": "t"}, nil)
	res := runReviewTool(t, dir, `{"scope":"units"}`)
	if !strings.Contains(res.Parts[0].Text, `"is_error":true`) || !strings.Contains(res.Parts[0].Text, "scope=units 时必须传入 unit_ids") {
		t.Fatalf("units scope without unit_ids should fail with is_error envelope: %s", res.Parts[0].Text)
	}
	res = runReviewTool(t, dir, `{"scope":"bogus"}`)
	if !strings.Contains(res.Parts[0].Text, "scope 只支持") {
		t.Fatalf("invalid scope not rejected: %s", res.Parts[0].Text)
	}
}

func TestReviewAdSourceSignatureMatchesPythonCanonicalJSON(t *testing.T) {
	byID := map[string]map[string]any{
		"S1": {"shot_id": "S1", "characters_in_shot": []any{"小明"}},
	}
	unit := map[string]any{"shot_ids": []any{"S1"}}
	// Python: json.dumps({"shot_ids":["S1"],"references":[["character","小明"]]}, sort_keys=True,
	// ensure_ascii=False, separators=(",",":"))
	canonical := `{"references":[["character","小明"]],"shot_ids":["S1"]}`
	sum := sha256.Sum256([]byte(canonical))
	want := hexOf(sum[:])
	if got := adSourceSignature(byID, unit); got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

func TestReviewStaleAdUnitMarksStaleSource(t *testing.T) {
	script := map[string]any{
		"content_mode": "ad",
		"shots": []any{
			map[string]any{"shot_id": "S1", "duration_seconds": float64(3), "characters_in_shot": []any{"小明"}},
			map[string]any{"shot_id": "S2", "duration_seconds": float64(3), "scenes": []any{"客厅"}},
		},
	}
	unit := map[string]any{"unit_id": "E1U1", "shot_ids": []any{"S1", "S2"}}
	signature := adSourceSignature(adShotsByID(script), unit)
	unit["generated_assets"] = map[string]any{"video_clip": "videos/a.mp4", "source_signature": signature}
	script["reference_units"] = []any{unit}

	project := map[string]any{
		"content_mode":    "ad",
		"generation_mode": "reference_video",
		"episodes":        []any{map[string]any{"episode": float64(1), "script_file": "episode_1.json"}},
	}
	dir := writeReviewFixture(t, project, map[string]map[string]any{"episode_1.json": script})
	writeReviewVideo(t, dir, "videos/a.mp4")

	fresh := reviewUnitByID(t, reviewPayload(t, runReviewTool(t, dir, `{"include_frames":false}`)), "E1U1")
	if fresh["review_status"] == "stale" {
		t.Fatalf("unchanged source must not be stale: %v", fresh["review_status"])
	}

	// 编辑镜头参考集后，记录的签名与当前编排不再一致。
	script["shots"].([]any)[0].(map[string]any)["characters_in_shot"] = []any{"小红"}
	writeReviewJSON(t, filepath.Join(dir, "scripts", "episode_1.json"), script)

	payload := reviewPayload(t, runReviewTool(t, dir, `{"include_frames":false}`))
	stale := reviewUnitByID(t, payload, "E1U1")
	if stale["review_status"] != "stale" {
		t.Fatalf("review_status = %v, want stale", stale["review_status"])
	}
	if !containsCode(reviewFindingCodes(payload), "stale_source") {
		t.Fatalf("stale_source finding missing: %v", reviewFindingCodes(payload))
	}
}

func TestReviewProbeUnavailableWhenFFprobeMissing(t *testing.T) {
	project := map[string]any{
		"content_mode":    "drama",
		"generation_mode": "reference_video",
		"episodes":        []any{map[string]any{"episode": float64(1), "script_file": "episode_1.json"}},
	}
	script := map[string]any{
		"video_units": []any{
			map[string]any{"unit_id": "E1U1", "duration_seconds": float64(5),
				"generated_assets": map[string]any{"video_clip": "videos/a.mp4", "status": "completed"}},
		},
	}
	dir := writeReviewFixture(t, project, map[string]map[string]any{"episode_1.json": script})
	writeReviewVideo(t, dir, "videos/a.mp4")

	t.Setenv("PATH", t.TempDir())
	payload := reviewPayload(t, runReviewTool(t, dir, `{"include_frames":false}`))
	unit := reviewUnitByID(t, payload, "E1U1")
	if unit["review_status"] != "manual_review" {
		t.Fatalf("review_status = %v, want manual_review", unit["review_status"])
	}
	probe := unit["media_probe"].(map[string]any)
	if probe["probe_available"] != false || probe["exists"] != true {
		t.Fatalf("media_probe = %v", probe)
	}
	for _, f := range payload["deterministic_findings"].([]any) {
		finding := f.(map[string]any)
		if finding["code"] != "probe_unavailable" {
			continue
		}
		if finding["severity"] != "info" || finding["confidence"] != 0.5 || finding["action"] != "manual_review" {
			t.Fatalf("probe_unavailable finding shape wrong: %v", finding)
		}
		return
	}
	t.Fatalf("probe_unavailable finding missing: %v", reviewFindingCodes(payload))
}

func TestReviewMissingVideoAndRequestedPlaceholder(t *testing.T) {
	project := map[string]any{
		"content_mode":    "drama",
		"generation_mode": "reference_video",
		"episodes":        []any{map[string]any{"episode": float64(1), "script_file": "episode_1.json"}},
	}
	script := map[string]any{
		"video_units": []any{
			map[string]any{"unit_id": "E1U1", "generated_assets": map[string]any{}},
		},
	}
	dir := writeReviewFixture(t, project, map[string]map[string]any{"episode_1.json": script})

	payload := reviewPayload(t, runReviewTool(t, dir, `{"scope":"units","unit_ids":["E1U1","E1U9"],"include_frames":false}`))
	unit := reviewUnitByID(t, payload, "E1U1")
	if unit["review_status"] != "missing" {
		t.Fatalf("E1U1 review_status = %v, want missing", unit["review_status"])
	}
	placeholder := reviewUnitByID(t, payload, "E1U9")
	if placeholder["review_status"] != "missing" {
		t.Fatalf("requested-but-absent unit must be reported missing, got %v", placeholder["review_status"])
	}
	if !containsCode(reviewFindingCodes(payload), "video_missing") {
		t.Fatalf("video_missing finding missing: %v", reviewFindingCodes(payload))
	}
}

func TestReviewSequenceFindings(t *testing.T) {
	dup := sequenceFindings([]string{"E1U2", "E1U1", "E1U2"})
	if !containsCode(findingCodes(dup), "duplicate_unit") || !containsCode(findingCodes(dup), "out_of_order") {
		t.Fatalf("duplicate/out_of_order not detected: %v", findingCodes(dup))
	}
	for _, f := range dup {
		if f.Code == "duplicate_unit" && (f.Severity != "blocker" || len(f.UnitIDs) != 1 || f.UnitIDs[0] != "E1U2") {
			t.Fatalf("duplicate finding = %+v", f)
		}
	}

	gap := sequenceFindings([]string{"E1U1", "E1U3"})
	if len(gap) != 1 || gap[0].Code != "missing_unit" || gap[0].Severity != "blocker" {
		t.Fatalf("gap findings = %+v", gap)
	}
	if len(gap[0].UnitIDs) != 1 || gap[0].UnitIDs[0] != "U02" {
		t.Fatalf("missing unit ids = %v, want [U02]", gap[0].UnitIDs)
	}

	// 序号不从 1 开始时不判缺口（与 Python 一致：只在 min == 1 时检查缺口）。
	if got := sequenceFindings([]string{"E1U2", "E1U3"}); len(got) != 0 {
		t.Fatalf("sequence starting above 1 should not report gaps: %+v", got)
	}
}

func findingCodes(list []reviewFinding) []string {
	var codes []string
	for _, f := range list {
		codes = append(codes, f.Code)
	}
	return codes
}

func TestReviewExtractsFramesAsImageParts(t *testing.T) {
	if !tools.LookPathOK("ffmpeg") || !tools.LookPathOK("ffprobe") {
		t.Skip("ffmpeg/ffprobe 不可用，跳过真实抽帧集成测试")
	}
	project := map[string]any{
		"content_mode":    "drama",
		"generation_mode": "reference_video",
		"episodes":        []any{map[string]any{"episode": float64(1), "script_file": "episode_1.json"}},
	}
	script := map[string]any{
		"video_units": []any{
			map[string]any{"unit_id": "E1U1", "duration_seconds": float64(2),
				"generated_assets": map[string]any{"video_clip": "videos/a.mp4", "status": "completed"}},
		},
	}
	dir := writeReviewFixture(t, project, map[string]map[string]any{"episode_1.json": script})
	clip := filepath.Join(dir, "videos", "a.mp4")
	if err := os.MkdirAll(filepath.Dir(clip), 0o755); err != nil {
		t.Fatal(err)
	}
	gen := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=10",
		"-pix_fmt", "yuv420p", clip)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("生成测试视频失败，跳过: %v\n%s", err, out)
	}

	res := runReviewTool(t, dir, `{"scope":"units","unit_ids":["E1U1"]}`)
	payload := reviewPayload(t, res)
	unit := reviewUnitByID(t, payload, "E1U1")
	if unit["review_status"] == "technical_error" {
		t.Fatalf("valid clip should be readable: %v", unit["media_probe"])
	}

	var images []schema.ToolOutputPart
	var labels []string
	for _, p := range res.Parts {
		switch p.Type {
		case schema.ToolPartTypeImage:
			images = append(images, p)
		case schema.ToolPartTypeText:
			if strings.Contains(p.Text, " frame") {
				labels = append(labels, p.Text)
			}
		}
	}
	if len(images) != 3 || len(labels) != 3 {
		t.Fatalf("want 3 start/middle/end frames with labels, got %d images, %d labels", len(images), len(labels))
	}
	if labels[0] != "episode_1.json / E1U1 / start frame" {
		t.Fatalf("label = %q", labels[0])
	}
	for _, img := range images {
		if img.Image == nil || img.Image.MIMEType != "image/jpeg" || img.Image.Base64Data == nil {
			t.Fatalf("image part malformed: %+v", img.Image)
		}
		data, err := base64.StdEncoding.DecodeString(*img.Image.Base64Data)
		if err != nil || len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
			t.Fatalf("image payload is not a JPEG (err=%v)", err)
		}
	}
}

// reviewAllowGuard 放行所有调用并记录 Post 收到的结果文本（实现已随 guard 测试移入 agent 包）。
