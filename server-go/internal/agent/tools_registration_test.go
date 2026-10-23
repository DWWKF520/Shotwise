package agent

import (
	"context"
	servicescript "github.com/shotwise/shotwise/internal/service/script"
	"os"
	"path/filepath"
	"testing"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain/grid"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/asset"
	"github.com/shotwise/shotwise/internal/service/media"
)

// 本文件验证「37 个工具在依赖齐备时全部注册」——迁移完成的收口证据。
// 依赖全部用最小替身（方法集只覆盖注册所需），断言是子集断言，不约束 BuiltinTools 的其它工具。

// regProjects 一次性满足 tools.AssetProjects / tools.GenerationProjects / tools.TextGenerationProjects /
// ScriptReviewProjects / ProjectWriter / RelationProjects / EpisodeStore。
type regProjects struct {
	dir string
	pf  *project.ProjectFile
}

func (p *regProjects) Get(string) (*project.ProjectFile, error) { return p.pf, nil }
func (p *regProjects) Dir(string) (string, error)               { return p.dir, nil }
func (p *regProjects) Update(_ string, mutate func(*project.ProjectFile) error) error {
	return mutate(p.pf)
}
func (p *regProjects) ReadScriptJSON(_, _ string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (p *regProjects) SaveScript(_, _ string, _ map[string]any) error { return nil }
func (p *regProjects) ScriptFileNames(string) ([]string, error)       { return nil, nil }

// regVideoCaps 同时满足 tools.VideoCapabilityReader 与 service.VideoCapabilityResolver。
type regVideoCaps struct{}

func (regVideoCaps) VideoCapabilitiesForProject(
	context.Context, *project.ProjectFile,
) (config.VideoCapabilityView, error) {
	return config.VideoCapabilityView{ProviderID: "p", ModelID: "m", SupportedDurations: []int{4, 6, 8}}, nil
}

func (regVideoCaps) VideoCapabilitiesForProjectBucket(
	context.Context, *project.ProjectFile, provider.VideoBucket,
) (config.VideoCapabilityView, error) {
	return config.VideoCapabilityView{ProviderID: "p", ModelID: "m", SupportedDurations: []int{4, 6, 8}}, nil
}

func (regVideoCaps) VideoGenerateAudioForProject(context.Context, *project.ProjectFile) (bool, error) {
	return true, nil
}

func (regVideoCaps) ResolveResolution(context.Context, *project.ProjectFile, string, string) (string, error) {
	return "", nil
}

// regImageCaps 同时满足 service.ImageCapabilityResolver 与 ImageEditCapabilityReader。
type regImageCaps struct{}

func (regImageCaps) ResolveImageBackend(
	context.Context, *project.ProjectFile, map[string]any, config.ImageCapability,
) (config.ProviderModel, error) {
	return config.ProviderModel{ProviderID: "p", ModelID: "m"}, nil
}

func (regImageCaps) ResolveResolution(context.Context, *project.ProjectFile, string, string) (string, error) {
	return "", nil
}

// regGenerator 同时满足 RelationTextGenerator 与 servicescript.TextCompletion。
type regGenerator struct{}

func (regGenerator) Generate(context.Context, string, map[string]any, int) (string, error) {
	return "{}", nil
}

type regQueue struct{}

func (regQueue) EnqueueTask(context.Context, repository.EnqueueParams) (repository.EnqueueResult, error) {
	return repository.EnqueueResult{}, nil
}

func (regQueue) GetTask(context.Context, string) (*repository.TaskData, error) { return nil, nil }

func (regQueue) IsWorkerOnline(context.Context, string) (bool, error) { return true, nil }

func (regQueue) GetActiveTasksForResources(
	context.Context, string, string, []string, *string, *string,
) ([]repository.TaskData, error) {
	return nil, nil
}

type regScripts struct{}

func (regScripts) UpdateScript(_ string, _ string, mutate func(map[string]any) error) error {
	return mutate(map[string]any{})
}

type regRenamer struct{}

func (regRenamer) Rename(string, project.AssetKind, string, string, bool) (asset.RenameReport, error) {
	return asset.RenameReport{}, nil
}

type regSplitter struct{}

func (regSplitter) ApplySplit(context.Context, string, *grid.GridGeneration) (service.GridSplitResult, error) {
	return service.GridSplitResult{}, nil
}

type regAssets struct{}

func (regAssets) Upsert(string, project.AssetKind, string, map[string]any) (bool, error) {
	return false, nil
}

type regMemory struct{}

func (regMemory) CreateMemoryCandidate(context.Context, service.CreateMemoryCandidateParams) (string, error) {
	return "cand", nil
}

type regTimeline struct{}

func (regTimeline) CreatePlan(context.Context, media.CreatePlanParams) (map[string]any, error) {
	return map[string]any{}, nil
}
func (regTimeline) GetPlan(context.Context, string, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (regTimeline) CreateRevision(context.Context, string, media.CreateRevisionParams) (map[string]any, error) {
	return map[string]any{}, nil
}

// expectedToolNames 是迁移完成的 37 个智能体工具（32 对外 + 5 内部 timeline）。
var expectedToolNames = []string{
	"list_project_text_files", "read_project_text", "list_pending_assets", "generate_assets",
	"generate_storyboards", "edit_images", "generate_grid", "generate_narration_audio",
	"plan_episodes", "reset_episode_planning", "patch_project", "get_video_capabilities",
	"analyze_character_relations", "inspect_director_review", "patch_episode_script",
	"insert_segment", "remove_segment", "split_segment", "patch_episode_meta", "rename_asset",
	"split_narration_segments", "normalize_drama_script", "generate_episode_script",
	"confirm_script_review", "split_reference_video_units", "open_reference_step1_for_edit",
	"validate_and_promote_reference_draft", "generate_video_episode", "generate_video_scene",
	"generate_video_all", "generate_video_selected", "propose_memory_candidate",
	"get_episode_media_manifest", "create_timeline_plan", "get_timeline_plan",
	"update_timeline_plan", "validate_timeline_plan",
}

func TestAllExpectedToolsRegistered(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "p1"), 0o755); err != nil {
		t.Fatal(err)
	}
	pf := &project.ProjectFile{Title: "t", ContentMode: "narration", Style: "s", GenerationMode: "storyboard"}
	projects := &regProjects{dir: filepath.Join(dir, "p1"), pf: pf}

	rt, err := NewRuntime(context.Background(), ModelConfig{}, dir, nil, nil,
		WithAssetProjects(projects),
		WithVideoCapabilities(regVideoCaps{}),
		WithRelationAnalysis(func(context.Context, *project.ProjectFile) (RelationTextGenerator, error) {
			return regGenerator{}, nil
		}, projects),
		WithProjectWriters(regAssets{}, projects),
		WithScriptEditing(regScripts{}),
		WithAssetRename(regRenamer{}),
		WithAssetGeneration(&regQueue{}, projects),
		WithGenerationTools(&regQueue{}, projects),
		WithImageEditTools(&regQueue{}, projects, regImageCaps{}),
		WithGridTools(&regQueue{}, projects, regImageCaps{}, regSplitter{}),
		WithEpisodePlanning(projects, func(context.Context, *project.ProjectFile) (servicescript.TextCompletion, error) {
			return regGenerator{}, nil
		}),
		WithTextGenerationTools(func(context.Context, *project.ProjectFile) (servicescript.TextCompletion, error) {
			return regGenerator{}, nil
		}, projects),
		WithScriptReviewTools(projects),
		WithReferenceTools(func(context.Context, *project.ProjectFile) (servicescript.TextCompletion, error) {
			return regGenerator{}, nil
		}, projects, regVideoCaps{}),
		WithVideoGeneration(&regQueue{}, projects, regVideoCaps{}, regScripts{}),
		WithTimelineTools(regTimeline{}),
		WithMemoryProposals(regMemory{}),
	)
	if err != nil {
		t.Fatal(err)
	}

	built, err := rt.buildTools(context.Background(), "sess-1", "p1")
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	registered := map[string]bool{}
	for _, tl := range built {
		info, err := tl.Info(context.Background())
		if err != nil || info == nil {
			continue
		}
		registered[info.Name] = true
	}

	var missing []string
	for _, name := range expectedToolNames {
		if !registered[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("以下工具未注册：%v\n已注册：%v", missing, sortedNames(registered))
	}
}

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	return out
}
