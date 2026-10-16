package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

// fakeVideoQueue 在 fakeAssetQueue 之上补在途任务探测（满足 VideoGenerationQueue）。
type fakeVideoQueue struct {
	fakeAssetQueue
	active []repository.TaskData
}

func (f *fakeVideoQueue) GetActiveTasksForResources(
	context.Context, string, string, []string, *string, *string,
) ([]repository.TaskData, error) {
	return f.active, nil
}

// fakeVideoScripts 提供剧本锁内读-改-写（满足 ScriptStore）。
type fakeVideoScripts struct {
	projects *fakeGenerationProjects
}

func (f *fakeVideoScripts) UpdateScript(project, filename string, mutate func(map[string]any) error) error {
	script, err := f.projects.ReadScriptJSON(project, filename)
	if err != nil {
		return err
	}
	if err := mutate(script); err != nil {
		return err
	}
	f.projects.scripts[filename] = script
	return nil
}

// fakeVideoCapResolver 返回固定的视频能力视图与音频开关（满足 service.VideoCapabilityResolver）。
type fakeVideoCapResolver struct {
	view        config.VideoCapabilityView
	generateAud bool
}

func (f *fakeVideoCapResolver) VideoCapabilitiesForProjectBucket(
	context.Context, *domain.ProjectFile, provider.VideoBucket,
) (config.VideoCapabilityView, error) {
	return f.view, nil
}

func (f *fakeVideoCapResolver) VideoGenerateAudioForProject(context.Context, *domain.ProjectFile) (bool, error) {
	return f.generateAud, nil
}

func (f *fakeVideoCapResolver) ResolveResolution(context.Context, *domain.ProjectFile, string, string) (string, error) {
	return "", nil
}

// newVideoFixture 造一个 narration+storyboard 项目：两个片段（E1S1 已完成、E1S2 待生成），
// 一个已登记且已出设计图的角色。
func newVideoFixture(t *testing.T) (*fakeVideoQueue, *fakeGenerationProjects, videoGenDeps) {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"characters", "storyboards", "videos"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile := func(rel string) {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("characters/阿明.png")
	writeFile("storyboards/scene_E1S1.png")
	writeFile("storyboards/scene_E1S2.png")
	writeFile("videos/scene_E1S1.mp4")

	entry := domain.NewAssetEntry()
	entry.SetString("description", "少年")
	entry.SetString("character_sheet", "characters/阿明.png")
	pf := &domain.ProjectFile{
		ContentMode: "narration", GenerationMode: "storyboard", Style: "写实",
		Characters: domain.AssetBucket{"阿明": entry},
	}
	script := map[string]any{
		"title": "第1集", "content_mode": "narration", "episode": float64(1),
		"segments": []any{
			map[string]any{"segment_id": "E1S1", "duration_seconds": float64(4),
				"characters_in_segment": []any{"阿明"},
				"video_prompt":          map[string]any{"action": "走路", "camera_motion": "Static", "ambiance_audio": "风声"},
				"generated_assets":      map[string]any{"storyboard_image": "storyboards/scene_E1S1.png"}},
			map[string]any{"segment_id": "E1S2", "duration_seconds": float64(5),
				"characters_in_segment": []any{"阿明"},
				"video_prompt":          map[string]any{"action": "坐下", "camera_motion": "Static", "ambiance_audio": "静"},
				"generated_assets":      map[string]any{"storyboard_image": "storyboards/scene_E1S2.png"}},
		},
	}
	projects := &fakeGenerationProjects{
		pf: pf, dir: dir, scripts: map[string]map[string]any{"episode_1.json": script},
	}
	q := &fakeVideoQueue{}
	deps := videoGenDeps{
		queue: q, projects: projects, scripts: &fakeVideoScripts{projects: projects},
		caps: &fakeVideoCapResolver{view: config.VideoCapabilityView{
			ProviderID: "gemini-aistudio", ModelID: "veo-3.1-generate-preview",
			SupportedDurations: []int{8}, Caps: provider.DefaultVideoCapabilities(),
		}, generateAud: true},
	}
	return q, projects, deps
}

func TestGenerateVideoEpisodeResumesAndSkipsCompleted(t *testing.T) {
	q, projects, deps := newVideoFixture(t)
	// 写一份 checkpoint：E1S1 声称完成且磁盘有成片 → 跳过。
	ckpt := filepath.Join(projects.dir, "videos", ".checkpoint_ep1.json")
	if err := fs.WriteJSONAtomic(ckpt, map[string]any{"completed_scenes": []any{"E1S1"}, "started_at": "t0"}); err != nil {
		t.Fatal(err)
	}
	tl, err := newGenerateVideoEpisodeTool("proj", deps)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json","resume":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 1 || q.enqueued[0].ResourceID != "E1S2" {
		t.Fatalf("只应为缺视频的 E1S2 入队: %+v", q.enqueued)
	}
	if q.enqueued[0].TaskType != "video" || q.enqueued[0].MediaType != "video" {
		t.Fatalf("任务身份有误: %+v", q.enqueued[0])
	}
	prompt, _ := q.enqueued[0].Payload["prompt"].(string)
	if !strings.Contains(prompt, "Action: 坐下") || !strings.Contains(prompt, "Avoid: BGM、文字字幕、水印") {
		t.Fatalf("视频 YAML 提示词不完整: %q", prompt)
	}
	if !strings.Contains(out, `"is_error":false`) || !strings.Contains(out, "第 1 集视频生成完成") {
		t.Fatalf("成功回包不符: %s", out)
	}
	// 完成时 checkpoint 应被清除。
	if _, err := os.Stat(ckpt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkpoint 应被清除: %v", err)
	}
}

func TestGenerateVideoEpisodeRejectsRouteMismatch(t *testing.T) {
	q, projects, deps := newVideoFixture(t)
	projects.pf.GenerationMode = "reference_video"
	tl, _ := newGenerateVideoEpisodeTool("proj", deps)
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "不属同一族") || len(q.enqueued) != 0 {
		t.Fatalf("跨族剧本应被拒且不入队: %s", out)
	}
}

func TestGenerateVideoEpisodeReferencePath(t *testing.T) {
	q, projects, deps := newVideoFixture(t)
	projects.pf.GenerationMode = "reference_video"
	projects.scripts["episode_1.json"] = map[string]any{
		"title": "第1集", "content_mode": "narration", "episode": float64(1),
		"video_units": []any{map[string]any{"unit_id": "E1U1", "duration_seconds": float64(8),
			"shots": []any{map[string]any{"text": "天空"}, map[string]any{"text": "大地"}}}},
	}
	tl, _ := newGenerateVideoEpisodeTool("proj", deps)
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 1 || q.enqueued[0].TaskType != "reference_video" || q.enqueued[0].MediaType != "video" {
		t.Fatalf("参考路线应为 reference_video 任务: %+v", q.enqueued)
	}
	if got, _ := q.enqueued[0].Payload["prompt"].(string); got != "天空\n大地" {
		t.Fatalf("unit prompt 应为拼接镜头文本: %q", got)
	}
	if !strings.Contains(out, "第 1 集参考视频生成完成") {
		t.Fatalf("回包不符: %s", out)
	}
}

func TestGenerateVideoEpisodePendingDurationConfirmation(t *testing.T) {
	q, projects, deps := newVideoFixture(t)
	projects.pf.GenerationMode = "reference_video"
	projects.scripts["episode_1.json"] = map[string]any{
		"title": "第1集", "content_mode": "narration", "episode": float64(1),
		"video_units": []any{map[string]any{"unit_id": "E1U1", "duration_seconds": float64(5),
			"shots": []any{map[string]any{"text": "天空"}}}},
	}
	tl, _ := newGenerateVideoEpisodeTool("proj", deps)
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 0 {
		t.Fatalf("待确认时不得入队: %+v", q.enqueued)
	}
	if !strings.Contains(out, "需先向用户确认") || !strings.Contains(out, "E1U1：剧本总时长 5s，将申请 8s（成片更长）") {
		t.Fatalf("待确认清单不符: %s", out)
	}
	// 用户同意后带 confirm_duration=true 完成入队。
	out, err = tl.InvokableRun(context.Background(), `{"script":"episode_1.json","confirm_duration":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 1 {
		t.Fatalf("确认后应入队: %+v", q.enqueued)
	}
	if !strings.Contains(out, "生成完成") {
		t.Fatalf("确认后回包不符: %s", out)
	}
}

func TestGenerateVideoAllReportsNoPending(t *testing.T) {
	q, projects, deps := newVideoFixture(t)
	segments := projects.scripts["episode_1.json"]["segments"].([]any)
	for _, item := range segments {
		item.(map[string]any)["generated_assets"] = map[string]any{
			"storyboard_image": item.(map[string]any)["generated_assets"].(map[string]any)["storyboard_image"],
			"video_clip":       "videos/scene_x.mp4",
		}
	}
	tl, _ := newGenerateVideoAllTool("proj", deps)
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 0 || !strings.Contains(out, "所有场景/片段的视频都已生成") {
		t.Fatalf("全部已生成不应入队: %s", out)
	}
}

func TestGenerateVideoSelectedAdRegeneration(t *testing.T) {
	q, projects, deps := newVideoFixture(t)
	projects.pf.ContentMode = "ad"
	projects.pf.GenerationMode = "reference_video"
	projects.pf.DefaultDuration = nil
	shot := map[string]any{"shot_id": "S1", "duration_seconds": float64(8),
		"image_prompt": map[string]any{"scene": "客厅"},
		"video_prompt": map[string]any{"action": "坐下", "camera_motion": "Static", "ambiance_audio": "静"},
		"generated_assets": map[string]any{
			"video_clip": "reference_videos/E1U1.mp4", "source_signature": "old"}}
	projects.scripts["episode_1.json"] = map[string]any{
		"title": "广告", "content_mode": "ad", "episode": float64(1),
		"shots":           []any{shot},
		"reference_units": []any{map[string]any{"unit_id": "E1U1", "shot_ids": []any{"S1"}}},
	}
	tl, _ := newGenerateVideoSelectedTool("proj", deps)
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json","scene_ids":["E1U1"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 1 || q.enqueued[0].TaskType != "reference_video" {
		t.Fatalf("点名应重新生成一个 unit: %s %+v", out, q.enqueued)
	}
	prompt, _ := q.enqueued[0].Payload["prompt"].(string)
	if !strings.Contains(prompt, "Shot 1 (8s): 客厅；坐下") {
		t.Fatalf("ad 提示词应为多镜头渲染: %q", prompt)
	}
	if !strings.Contains(out, "参考直出重新生成完成") {
		t.Fatalf("回包不符: %s", out)
	}
}

func TestGenerateVideoSelectedRejectsActiveTasks(t *testing.T) {
	q, projects, deps := newVideoFixture(t)
	projects.pf.ContentMode = "ad"
	projects.pf.GenerationMode = "reference_video"
	projects.scripts["episode_1.json"] = map[string]any{
		"title": "广告", "content_mode": "ad", "episode": float64(1),
		"shots": []any{map[string]any{"shot_id": "S1", "duration_seconds": float64(3),
			"image_prompt": map[string]any{"scene": "客厅"},
			"video_prompt": map[string]any{"action": "坐下", "camera_motion": "Static", "ambiance_audio": "静"}}},
		"reference_units": []any{map[string]any{"unit_id": "E1U1", "shot_ids": []any{"S1"}}},
	}
	q.active = []repository.TaskData{{ResourceID: "E1U1", Status: "running"}}
	tl, _ := newGenerateVideoSelectedTool("proj", deps)
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json","scene_ids":["E1U1"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "已有在途任务") || len(q.enqueued) != 0 {
		t.Fatalf("在途任务应拒绝整批: %s", out)
	}
}

func TestGenerateVideoToolsDenyWithoutDeps(t *testing.T) {
	tl, err := newGenerateVideoEpisodeTool("proj", videoGenDeps{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"script":"episode_1.json"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "video generation not configured") {
		t.Fatalf("未注入依赖应 deny: %s", out)
	}
	// 非法脚本名先于依赖检查被拒。
	out, _ = tl.InvokableRun(context.Background(), `{"script":"../evil.json"}`)
	if !strings.Contains(out, "纯文件名") {
		t.Fatalf("非法脚本名应被拒: %s", out)
	}
}
