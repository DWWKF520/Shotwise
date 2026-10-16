package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/queue"
	"github.com/shotwise/shotwise/internal/repository"
)

// resumeStubBackend 是注册进 openai 键的假视频后端：ResumeVideo 落一个最小文件即成功，
// 不做真实轮询；resumeErr 非 nil 时按给定错误返回，供过期 / 不支持分支断言。
type resumeStubBackend struct {
	model       string
	caps        provider.VideoCapabilities
	resumeErr   error
	resumeCalls int
	lastJobID   string
	lastReq     provider.VideoGenerationRequest
}

func (b *resumeStubBackend) Name() string                                  { return "openai" }
func (b *resumeStubBackend) Model() string                                 { return b.model }
func (b *resumeStubBackend) VideoCapabilities() provider.VideoCapabilities { return b.caps }

func (b *resumeStubBackend) Generate(_ context.Context, req provider.VideoGenerationRequest) (provider.VideoGenerationResult, error) {
	if err := os.WriteFile(req.OutputPath, []byte("mp4"), 0o644); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	return provider.VideoGenerationResult{VideoPath: req.OutputPath, Provider: "openai", Model: b.model, DurationSeconds: req.DurationSeconds}, nil
}

func (b *resumeStubBackend) ResumeVideo(_ context.Context, jobID string, req provider.VideoGenerationRequest) (provider.VideoGenerationResult, error) {
	b.resumeCalls++
	b.lastJobID = jobID
	b.lastReq = req
	if b.resumeErr != nil {
		return provider.VideoGenerationResult{}, b.resumeErr
	}
	if err := os.WriteFile(req.OutputPath, []byte("mp4"), 0o644); err != nil {
		return provider.VideoGenerationResult{}, err
	}
	return provider.VideoGenerationResult{
		VideoPath:       req.OutputPath,
		Provider:        "openai",
		Model:           b.model,
		DurationSeconds: req.DurationSeconds,
		VideoURI:        "https://file.example/v.mp4",
	}, nil
}

func resumeStrPtr(s string) *string { return &s }

// newResumeFixture 在视频夹具基础上换装 resume-capable 后端，并把项目 provider 配置清空——
// 让执行身份的唯一来源是 payload 锁定键，从而验证 resume 按提交时身份解析而非当前项目配置。
func newResumeFixture(
	t *testing.T, contentMode domain.ContentMode, genMode domain.GenerationMode,
) (*TaskDispatcher, *ResumeExecutor, *ProjectStore, *gorm.DB, *resumeStubBackend, string, string) {
	t.Helper()
	dispatcher, store, gdb, projectName := newVideoFixture(t, contentMode, genMode)
	backend := &resumeStubBackend{caps: stubVideoCaps()}
	provider.RegisterVideoBackend("openai", provider.VideoBackendRegistration{
		Create: func(opts provider.BackendOptions) (provider.VideoBackend, error) {
			backend.model = opts.Model
			return backend, nil
		},
		CapabilitiesForModel: func(string) (provider.VideoCapabilities, error) { return stubVideoCaps(), nil },
	})
	t.Cleanup(func() { provider.RegisterVideoBackend("openai", provider.VideoBackendRegistration{}) })
	if err := store.Update(projectName, func(p *domain.ProjectFile) error {
		p.VideoBackend = ""
		p.VideoProviderI2V = ""
		return nil
	}); err != nil {
		t.Fatalf("clear project video backend: %v", err)
	}
	projectDir, err := store.Dir(projectName)
	if err != nil {
		t.Fatalf("project dir: %v", err)
	}
	return dispatcher, NewResumeExecutor(dispatcher), store, gdb, backend, projectDir, projectName
}

// seedPendingCall 落一条 pending 记账行，返回其 id 供 payload["api_call_id"] 引用。
func seedPendingCall(t *testing.T, gdb *gorm.DB, projectName string) int64 {
	t.Helper()
	now := db.NewFlexibleTime(time.Now())
	row := repository.APICall{
		ProjectName: projectName,
		CallType:    "video",
		Model:       "sora-2",
		Status:      StatusPending,
		Currency:    "USD",
		Provider:    "openai",
		UserID:      repository.DefaultUserID,
		StartedAt:   now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := gdb.Create(&row).Error; err != nil {
		t.Fatalf("seed pending call: %v", err)
	}
	return row.ID
}

// loadCall 读回单条记账行。
func loadCall(t *testing.T, gdb *gorm.DB, id int64) repository.APICall {
	t.Helper()
	var row repository.APICall
	if err := gdb.Where("id = ?", id).First(&row).Error; err != nil {
		t.Fatalf("load call %d: %v", id, err)
	}
	return row
}

func TestResumeVideoTaskHappyPath(t *testing.T) {
	_, exec, store, gdb, backend, projectDir, projectName := newResumeFixture(t, domain.ContentNarration, domain.GenerationStoryboard)
	writeSegmentScript(t, store, projectDir, "storyboards/scene_E1S01.png")

	callID := seedPendingCall(t, gdb, projectName)
	endpoint := "https://api.example.invalid/v1"
	task := &repository.TaskData{
		TaskID:           "task-resume-1",
		ProjectName:      projectName,
		TaskType:         "video",
		MediaType:        "video",
		ResourceID:       "E1S01",
		ProviderID:       resumeStrPtr("openai"),
		ProviderJobID:    resumeStrPtr("job-123"),
		ProviderEndpoint: &endpoint,
		Payload: map[string]any{
			"script_file":        "episode_1.json",
			"prompt":             "夜色中的天台",
			"video_provider_i2v": "openai/sora-2",
			"duration_seconds":   int64(8),
			"api_call_id":        callID,
			"seed":               int64(42),
		},
		UserID: repository.DefaultUserID,
	}

	result, err := exec.ResumeVideo(context.Background(), task, "job-123")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if result["file_path"] != "videos/scene_E1S01.mp4" || result["resource_type"] != "videos" {
		t.Fatalf("unexpected result: %v", result)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "videos", "scene_E1S01.mp4")); err != nil {
		t.Fatalf("resume output missing: %v", err)
	}
	if backend.resumeCalls != 1 || backend.lastJobID != "job-123" {
		t.Fatalf("backend resume calls=%d job=%q", backend.resumeCalls, backend.lastJobID)
	}
	// 提交域名回放：内置供应商的 provider_endpoint 是请求域名，注入 SubmittedBaseURL。
	if backend.lastReq.SubmittedBaseURL != endpoint {
		t.Fatalf("submitted base url = %q, want %q", backend.lastReq.SubmittedBaseURL, endpoint)
	}
	if backend.lastReq.Seed == nil || *backend.lastReq.Seed != 42 {
		t.Fatalf("seed not propagated: %v", backend.lastReq.Seed)
	}
	if backend.lastReq.DurationSeconds != 8 || backend.lastReq.AspectRatio != "9:16" {
		t.Fatalf("request duration=%d ratio=%q", backend.lastReq.DurationSeconds, backend.lastReq.AspectRatio)
	}

	// 剧本 generated_assets 回写。
	script, err := store.ReadScriptJSON(projectName, "episode_1.json")
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	assets := script["segments"].([]any)[0].(map[string]any)["generated_assets"].(map[string]any)
	if assets["video_clip"] != "videos/scene_E1S01.mp4" || assets["video_uri"] != "https://file.example/v.mp4" {
		t.Fatalf("assets not written back: %v", assets)
	}

	// 结算补账：pending 精准翻 success。
	if row := loadCall(t, gdb, callID); row.Status != StatusSuccess {
		t.Fatalf("ledger status = %s, want success", row.Status)
	}
	// 版本登记：resume 必 bump 新版本。
	if _, err := NewVersions(projectDir).LatestCreatedAt("videos", "E1S01"); err != nil {
		t.Fatalf("version not registered: %v", err)
	}
}

func TestResumeVideoTaskExpiredSettlesFailed(t *testing.T) {
	_, exec, store, gdb, backend, projectDir, projectName := newResumeFixture(t, domain.ContentNarration, domain.GenerationStoryboard)
	writeSegmentScript(t, store, projectDir, "storyboards/scene_E1S01.png")
	backend.resumeErr = &provider.ResumeExpiredError{JobID: "job-9", Provider: "openai"}

	callID := seedPendingCall(t, gdb, projectName)
	task := &repository.TaskData{
		TaskID: "task-resume-2", ProjectName: projectName, TaskType: "video", MediaType: "video",
		ResourceID: "E1S01",
		Payload: map[string]any{
			"script_file":        "episode_1.json",
			"prompt":             "x",
			"video_provider_i2v": "openai/sora-2",
			"duration_seconds":   int64(8),
			"api_call_id":        callID,
		},
	}

	_, err := exec.ResumeVideo(context.Background(), task, "job-9")
	if !errors.Is(err, queue.ErrResumeExpired) {
		t.Fatalf("err = %v, want ErrResumeExpired", err)
	}
	row := loadCall(t, gdb, callID)
	if row.Status != StatusFailed || row.CostAmount != 0 {
		t.Fatalf("ledger row = %s cost=%v, want failed/zero", row.Status, row.CostAmount)
	}
}

func TestResumeVideoTaskUnsupportedMapsSentinel(t *testing.T) {
	_, exec, store, gdb, backend, projectDir, projectName := newResumeFixture(t, domain.ContentNarration, domain.GenerationStoryboard)
	writeSegmentScript(t, store, projectDir, "storyboards/scene_E1S01.png")
	backend.resumeErr = provider.ErrResumeUnsupported

	callID := seedPendingCall(t, gdb, projectName)
	task := &repository.TaskData{
		TaskID: "task-resume-3", ProjectName: projectName, TaskType: "video", MediaType: "video",
		ResourceID: "E1S01",
		Payload: map[string]any{
			"script_file":        "episode_1.json",
			"prompt":             "x",
			"video_provider_i2v": "openai/sora-2",
			"api_call_id":        callID,
		},
	}
	_, err := exec.ResumeVideo(context.Background(), task, "job-1")
	if !errors.Is(err, queue.ErrResumeUnsupported) {
		t.Fatalf("err = %v, want ErrResumeUnsupported", err)
	}
	// 不支持不等于过期：不补账，pending 行保留（与 Python 口径一致）。
	if row := loadCall(t, gdb, callID); row.Status != StatusPending {
		t.Fatalf("ledger status = %s, want pending untouched", row.Status)
	}
}

func TestResumeVideoTaskMissingAPICallIDStillSucceeds(t *testing.T) {
	_, exec, store, _, _, projectDir, projectName := newResumeFixture(t, domain.ContentNarration, domain.GenerationStoryboard)
	writeSegmentScript(t, store, projectDir, "storyboards/scene_E1S01.png")

	task := &repository.TaskData{
		TaskID: "task-resume-4", ProjectName: projectName, TaskType: "video", MediaType: "video",
		ResourceID: "E1S01",
		Payload: map[string]any{
			"script_file":        "episode_1.json",
			"prompt":             "x",
			"video_provider_i2v": "openai/sora-2",
			"duration_seconds":   int64(8),
		},
	}
	result, err := exec.ResumeVideo(context.Background(), task, "job-1")
	if err != nil {
		t.Fatalf("resume without api_call_id should succeed: %v", err)
	}
	if result["file_path"] != "videos/scene_E1S01.mp4" {
		t.Fatalf("unexpected result: %v", result)
	}
}

func TestResumeReferenceVideoTaskHappyPath(t *testing.T) {
	_, exec, store, _, _, projectDir, projectName := newResumeFixture(t, domain.ContentNarration, domain.GenerationReferenceVideo)
	writeUnitScript(t, store, projectDir)

	task := &repository.TaskData{
		TaskID: "task-resume-5", ProjectName: projectName, TaskType: "reference_video", MediaType: "video",
		ResourceID: "E1U01",
		Payload: map[string]any{
			"script_file":        "episode_1.json",
			"prompt":             "占位",
			"video_provider_r2v": "openai/sora-2",
			"duration_seconds":   int64(8),
		},
	}
	result, err := exec.ResumeVideo(context.Background(), task, "job-1")
	if err != nil {
		t.Fatalf("resume reference_video: %v", err)
	}
	if result["file_path"] != "reference_videos/E1U01.mp4" || result["resource_type"] != "reference_videos" {
		t.Fatalf("unexpected result: %v", result)
	}
	script, err := store.ReadScriptJSON(projectName, "episode_1.json")
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	unit := script["video_units"].([]any)[0].(map[string]any)
	assets := unit["generated_assets"].(map[string]any)
	if assets["video_clip"] != "reference_videos/E1U01.mp4" || assets["status"] != "completed" {
		t.Fatalf("unit assets not written back: %v", assets)
	}
}

func TestResumeRejectsNonVideoTaskType(t *testing.T) {
	_, exec, _, _, _, _, projectName := newResumeFixture(t, domain.ContentNarration, domain.GenerationStoryboard)
	_, err := exec.ResumeVideo(context.Background(), &repository.TaskData{
		TaskID: "task-resume-6", ProjectName: projectName, TaskType: "storyboard", MediaType: "video",
		ResourceID: "E1S01",
		Payload:    map[string]any{"script_file": "episode_1.json"},
	}, "job-1")
	if !errors.Is(err, queue.ErrResumeUnsupported) {
		t.Fatalf("err = %v, want ErrResumeUnsupported", err)
	}
}

func TestEnsureResumeEndpointUnchanged(t *testing.T) {
	if err := ensureResumeEndpointUnchanged("ep_a", "ep_a", "job", "custom-x"); err != nil {
		t.Fatalf("same endpoint should pass: %v", err)
	}
	if err := ensureResumeEndpointUnchanged("", "ep_b", "job", "custom-x"); err != nil {
		t.Fatalf("empty submitted should skip: %v", err)
	}
	if err := ensureResumeEndpointUnchanged("ep_a", "", "job", "openai"); err != nil {
		t.Fatalf("empty current should skip: %v", err)
	}
	err := ensureResumeEndpointUnchanged("ep_a", "ep_b", "job", "custom-x")
	if !errors.Is(err, queue.ErrResumeEndpointChanged) {
		t.Fatalf("err = %v, want ErrResumeEndpointChanged", err)
	}
}

func TestSubmittedBaseURLForResume(t *testing.T) {
	cases := []struct {
		name      string
		submitted string
		current   string
		want      string
	}{
		{"内置域名回放", "https://api.example/v1", "", "https://api.example/v1"},
		{"大小写不敏感", "HTTPS://api.example/v1", "", "HTTPS://api.example/v1"},
		{"自定义端点不回放", "ep_a", "ep_b", ""},
		{"非域名不回放", "ep_a", "", ""},
	}
	for _, c := range cases {
		if got := submittedBaseURLForResume(c.submitted, c.current); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestResumeDurationSecondsAndAPICallID(t *testing.T) {
	two := 2
	project := &domain.ProjectFile{DefaultDuration: &two}
	if got := resumeDurationSeconds(map[string]any{"duration_seconds": "8.0"}, project); got != 8 {
		t.Fatalf("float string duration = %d, want 8", got)
	}
	if got := resumeDurationSeconds(map[string]any{"duration_seconds": "abc"}, project); got != 8 {
		t.Fatalf("unparseable explicit duration should fall to 8, got %d", got)
	}
	if got := resumeDurationSeconds(map[string]any{}, project); got != 2 {
		t.Fatalf("project default duration = %d, want 2", got)
	}
	if got := resumeDurationSeconds(map[string]any{}, nil); got != 8 {
		t.Fatalf("no payload / no project = %d, want 8", got)
	}

	if id := payloadAPICallID(map[string]any{"api_call_id": float64(7)}); id == nil || *id != 7 {
		t.Fatalf("float api_call_id = %v", id)
	}
	if id := payloadAPICallID(map[string]any{"api_call_id": "9"}); id == nil || *id != 9 {
		t.Fatalf("string api_call_id = %v", id)
	}
	if id := payloadAPICallID(map[string]any{"api_call_id": "abc"}); id != nil {
		t.Fatalf("unparseable api_call_id should be nil, got %v", *id)
	}
}
