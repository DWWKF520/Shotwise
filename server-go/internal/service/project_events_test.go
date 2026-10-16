package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/repository"
)

// newProjectEventsFixture 建一个真实落盘的项目目录，返回 store 与项目名。
func newProjectEventsFixture(t *testing.T) (*ProjectStore, string) {
	t.Helper()
	store := NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "测试项目", domain.ContentNarration, domain.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return store, "p1"
}

// writeProjectFile 在项目目录下写一个文件（父目录按需创建）。
func writeProjectFile(t *testing.T, projectDir, rel string) {
	t.Helper()
	full := filepath.Join(projectDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestComputeAffectedFingerprintsPerTaskType(t *testing.T) {
	store, projectName := newProjectEventsFixture(t)
	projectDir, err := store.Dir(projectName)
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, projectDir, "storyboards/scene_E1S01.png")
	writeProjectFile(t, projectDir, "videos/scene_E1S01.mp4")
	writeProjectFile(t, projectDir, "thumbnails/scene_E1S01.jpg")
	writeProjectFile(t, projectDir, "characters/阿明.png")
	writeProjectFile(t, projectDir, "characters/阿明_avatar.png")
	writeProjectFile(t, projectDir, "scenes/客厅.png")
	writeProjectFile(t, projectDir, "props/长剑.png")
	writeProjectFile(t, projectDir, "products/保温杯.png")
	writeProjectFile(t, projectDir, "reference_videos/E1U01.mp4")
	writeProjectFile(t, projectDir, "reference_videos/thumbnails/E1U01.jpg")
	writeProjectFile(t, projectDir, "audio/segment_E1S01.wav")

	cases := []struct {
		taskType   string
		resourceID string
		wantKeys   []string
	}{
		{"storyboard", "E1S01", []string{"storyboards/scene_E1S01.png"}},
		{"video", "E1S01", []string{"videos/scene_E1S01.mp4", "thumbnails/scene_E1S01.jpg"}},
		{"character", "阿明", []string{"characters/阿明.png", "characters/阿明_avatar.png"}},
		{"scene", "客厅", []string{"scenes/客厅.png"}},
		{"prop", "长剑", []string{"props/长剑.png"}},
		{"product", "保温杯", []string{"products/保温杯.png"}},
		{"reference_video", "E1U01", []string{"reference_videos/E1U01.mp4", "reference_videos/thumbnails/E1U01.jpg"}},
		{"tts", "E1S01", []string{"audio/segment_E1S01.wav"}},
	}
	for _, tc := range cases {
		t.Run(tc.taskType, func(t *testing.T) {
			got := ComputeAffectedFingerprints(store, projectName, tc.taskType, tc.resourceID)
			if len(got) != len(tc.wantKeys) {
				t.Fatalf("fingerprints = %v, want keys %v", got, tc.wantKeys)
			}
			for _, key := range tc.wantKeys {
				if _, ok := got[key]; !ok {
					t.Fatalf("missing fingerprint key %q in %v", key, got)
				}
			}
		})
	}
}

func TestComputeAffectedFingerprintsCharacterAvatarPreferred(t *testing.T) {
	store, projectName := newProjectEventsFixture(t)
	projectDir, err := store.Dir(projectName)
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, projectDir, "characters/阿明.png")
	writeProjectFile(t, projectDir, "characters/avatars/阿明.png")
	if err := store.Update(projectName, func(p *domain.ProjectFile) error {
		if p.Characters == nil {
			p.Characters = domain.AssetBucket{}
		}
		p.Characters["阿明"] = domain.WrapAssetEntry(map[string]any{
			"description":      "阿明",
			"character_sheet":  "characters/阿明.png",
			"character_avatar": "characters/avatars/阿明.png",
		})
		return nil
	}); err != nil {
		t.Fatalf("set avatar: %v", err)
	}
	got := ComputeAffectedFingerprints(store, projectName, "character", "阿明")
	if _, ok := got["characters/avatars/阿明.png"]; !ok {
		t.Fatalf("configured avatar path should be fingerprinted, got %v", got)
	}
	if _, ok := got["characters/阿明_avatar.png"]; ok {
		t.Fatalf("fallback avatar must not appear when configured avatar is set: %v", got)
	}
}

func TestComputeAffectedFingerprintsGridCells(t *testing.T) {
	store, projectName := newProjectEventsFixture(t)
	projectDir, err := store.Dir(projectName)
	if err != nil {
		t.Fatal(err)
	}
	gridID := "grid_0123456789ab"
	writeProjectFile(t, projectDir, "grids/"+gridID+".png")
	writeProjectFile(t, projectDir, "storyboards/scene_E1S02.png")

	cellRel := "storyboards/scene_E1S02.png"
	escapeRel := "../outside.png"
	absRel := filepath.Join(t.TempDir(), "abs.png")
	manager, err := NewGridManager(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Save(&domain.GridGeneration{
		ID:         gridID,
		FrameChain: []domain.GridFrameCell{{ImagePath: &cellRel}, {ImagePath: &escapeRel}, {ImagePath: &absRel}},
	}); err != nil {
		t.Fatalf("save grid: %v", err)
	}

	got := ComputeAffectedFingerprints(store, projectName, "grid", gridID)
	if _, ok := got["grids/"+gridID+".png"]; !ok {
		t.Fatalf("grid composite missing, got %v", got)
	}
	if _, ok := got[cellRel]; !ok {
		t.Fatalf("in-project cell path should be fingerprinted, got %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("escaping/absolute cells must be skipped, got %v", got)
	}
}

func TestEmitGenerationSuccessBatchPublishesWorkerChange(t *testing.T) {
	store, projectName := newProjectEventsFixture(t)
	projectDir, err := store.Dir(projectName)
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, projectDir, "storyboards/scene_E1S01.png")
	if err := store.WriteScriptJSON(projectName, "episode_1.json", map[string]any{
		"episode":      1,
		"content_mode": "narration",
		"segments":     []any{map[string]any{"segment_id": "E1S01"}},
	}); err != nil {
		t.Fatalf("write script: %v", err)
	}

	hub := NewEventHub()
	ch, cancel := hub.Subscribe(projectName)
	defer cancel()

	task := &repository.TaskData{
		TaskID:      "task-1",
		ProjectName: projectName,
		TaskType:    "storyboard",
		MediaType:   "image",
		ResourceID:  "E1S01",
		Payload:     map[string]any{"script_file": "episode_1.json"},
	}
	fingerprints := EmitGenerationSuccessBatch(hub, store, task)
	if _, ok := fingerprints["storyboards/scene_E1S01.png"]; !ok {
		t.Fatalf("returned fingerprints missing artifact: %v", fingerprints)
	}

	batch := readChangeBatch(t, ch)
	if batch.Source != "worker" {
		t.Fatalf("source = %q, want worker", batch.Source)
	}
	if batch.ProjectName != projectName || batch.BatchID == "" || batch.Fingerprint == "" || batch.GeneratedAt == "" {
		t.Fatalf("batch envelope incomplete: %+v", batch)
	}
	if len(batch.Changes) != 1 {
		t.Fatalf("want one change, got %d", len(batch.Changes))
	}
	change := batch.Changes[0]
	if change.EntityType != "segment" || change.Action != "storyboard_ready" || change.EntityID != "E1S01" {
		t.Fatalf("unexpected change identity: %+v", change)
	}
	if change.Label != "分镜「E1S01」" || !change.Important {
		t.Fatalf("unexpected change label/important: %+v", change)
	}
	if change.ScriptFile != "episode_1.json" || change.Episode == nil || *change.Episode != 1 {
		t.Fatalf("unexpected script/episode: %+v", change)
	}
	if _, ok := change.AssetFingerprints["storyboards/scene_E1S01.png"]; !ok {
		t.Fatalf("change asset_fingerprints missing artifact: %v", change.AssetFingerprints)
	}
}

func TestEmitGenerationSuccessBatchReferenceVideoNoun(t *testing.T) {
	store, projectName := newProjectEventsFixture(t)
	hub := NewEventHub()
	ch, cancel := hub.Subscribe(projectName)
	defer cancel()

	task := &repository.TaskData{
		TaskID:      "task-rv",
		ProjectName: projectName,
		TaskType:    "reference_video",
		MediaType:   "video",
		ResourceID:  "E1U01",
		Payload:     map[string]any{},
	}
	EmitGenerationSuccessBatch(hub, store, task)
	change := readChangeBatch(t, ch).Changes[0]
	if change.EntityType != "reference_unit" || change.Action != "reference_video_ready" || change.Label != "参考视频「E1U01」" {
		t.Fatalf("unexpected reference_video change: %+v", change)
	}
}

func TestEmitGenerationSuccessBatchAssetSpecDerived(t *testing.T) {
	store, projectName := newProjectEventsFixture(t)
	hub := NewEventHub()
	ch, cancel := hub.Subscribe(projectName)
	defer cancel()

	task := &repository.TaskData{
		TaskID:      "task-c",
		ProjectName: projectName,
		TaskType:    "character",
		MediaType:   "image",
		ResourceID:  "阿明",
		Payload:     map[string]any{},
	}
	EmitGenerationSuccessBatch(hub, store, task)
	change := readChangeBatch(t, ch).Changes[0]
	if change.EntityType != "character" || change.Action != "updated" || change.Label != "角色「阿明」设计图" {
		t.Fatalf("unexpected character change: %+v", change)
	}
}

func TestEmitGenerationSuccessBatchUnknownTypeIsSilent(t *testing.T) {
	store, projectName := newProjectEventsFixture(t)
	hub := NewEventHub()
	ch, cancel := hub.Subscribe(projectName)
	defer cancel()

	fingerprints := EmitGenerationSuccessBatch(hub, store, &repository.TaskData{
		TaskID:      "task-x",
		ProjectName: projectName,
		TaskType:    "totally_unknown",
		MediaType:   "image",
		ResourceID:  "x",
	})
	if len(fingerprints) != 0 {
		t.Fatalf("unknown task type must return empty fingerprints, got %v", fingerprints)
	}
	select {
	case frame := <-ch:
		t.Fatalf("unknown task type must not broadcast, got %s", frame)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestTerminalEmitterSkipsFailuresAndEmitsSuccess(t *testing.T) {
	gdb := newServiceTestDB(t)
	store, projectName := newProjectEventsFixture(t)
	projectDir, err := store.Dir(projectName)
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, projectDir, "props/长剑.png")

	repo := repository.NewTaskRepo(gdb)
	enqueued, err := repo.Enqueue(context.Background(), repository.EnqueueParams{
		ProjectName: projectName,
		TaskType:    "prop",
		MediaType:   "image",
		ResourceID:  "长剑",
		Payload:     map[string]any{"script_file": "episode_1.json"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	hub := NewEventHub()
	ch, cancel := hub.Subscribe(projectName)
	defer cancel()
	emitter := NewTerminalEmitter(hub, store, gdb)

	// 失败终态不发变更事件。
	emitter([]repository.TerminalEvent{{
		TaskID: enqueued.TaskID, ProjectName: projectName, Status: repository.TaskStatusFailed, TaskType: "prop",
	}})
	select {
	case frame := <-ch:
		t.Fatalf("failed terminal must not broadcast, got %s", frame)
	case <-time.After(50 * time.Millisecond):
	}

	// 成功终态广播 prop 完成事件。
	emitter([]repository.TerminalEvent{{
		TaskID: enqueued.TaskID, ProjectName: projectName, Status: repository.TaskStatusSucceeded, TaskType: "prop",
	}})
	change := readChangeBatch(t, ch).Changes[0]
	if change.EntityType != "prop" || change.Action != "updated" || change.Label != "道具「长剑」设计图" {
		t.Fatalf("unexpected emitted change: %+v", change)
	}
}

// readChangeBatch 从 hub 的 SSE 帧里解出 ChangeBatch。
func readChangeBatch(t *testing.T, ch <-chan []byte) ChangeBatch {
	t.Helper()
	select {
	case frame := <-ch:
		trimmed := strings.TrimPrefix(string(frame), "event: changes\ndata: ")
		trimmed = strings.TrimSuffix(trimmed, "\n\n")
		var batch ChangeBatch
		if err := json.Unmarshal([]byte(trimmed), &batch); err != nil {
			t.Fatalf("decode batch frame %q: %v", frame, err)
		}
		return batch
	case <-time.After(time.Second):
		t.Fatal("no change event broadcast")
		return ChangeBatch{}
	}
}
