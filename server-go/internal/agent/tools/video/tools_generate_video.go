package video

import "github.com/shotwise/shotwise/internal/agent/tools"

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/domain/media"
	"github.com/shotwise/shotwise/internal/domain/project"
	domainscript "github.com/shotwise/shotwise/internal/domain/script"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	serviceref "github.com/shotwise/shotwise/internal/service/reference"
)

// 视频生成工具（对应 server/agent_runtime/sdk_tools/enqueue_videos.py 的 4 个工具）：
// generate_video_episode / generate_video_scene / generate_video_all / generate_video_selected。
//
// 每条入口先按项目生成路线分流：storyboard（分镜图驱动，i2v）、reference_video 的 episode 分支
// （narration/drama 的 video_units）与 ad 分支（reference_units 派生索引）。参考路线的 unit 时长
// 取档、音频闸门与 pending duration 确认在入队前统一收口（见 service/video_caps.go）。

// VideoGenerationQueue 是视频生成工具的队列能力（*queue.Queue 满足）：除入队/等待外还要探测
// 同一去重键下的在途任务（点名重做前的冲突检查）。
type VideoGenerationQueue interface {
	tools.AssetQueue
	GetActiveTasksForResources(ctx context.Context, projectName, taskType string, resourceIDs []string,
		scriptFile, resourceType *string) ([]repository.TaskData, error)
}

// Deps 是视频生成工具的一组依赖（闭包绑定）；由 agent 运行时经 RuntimeOption 注入，
// 四项齐备（Ready）时 buildTools 才装配这组工具。
type Deps struct {
	Queue    VideoGenerationQueue
	Projects tools.GenerationProjects
	Caps     service.VideoCapabilityResolver
	Scripts  tools.ScriptStore
}

// Ready 判定四项依赖是否齐备。
func (d Deps) Ready() bool {
	return d.Queue != nil && d.Projects != nil && d.Caps != nil && d.Scripts != nil
}

// ---------------------------------- 入参 ----------------------------------

type generateVideoEpisodeIn struct {
	Script          string `json:"script" jsonschema:"description=剧本文件名（如 episode_1.json），必须是纯文件名，禁止任何路径分隔符"`
	Resume          bool   `json:"resume,omitempty" jsonschema:"description=是否从上次中断处继续"`
	ConfirmDuration bool   `json:"confirm_duration,omitempty" jsonschema:"description=参考生视频时长确认：unit 申请时长与剧本编排不一致时首次调用不入队，返回待确认清单；用户同意后带 confirm_duration=true 再次调用完成入队。"`
}

type generateVideoSceneIn struct {
	Script          string `json:"script" jsonschema:"description=剧本文件名（如 episode_1.json），必须是纯文件名，禁止任何路径分隔符"`
	SceneID         string `json:"scene_id" jsonschema:"description=场景或片段 ID；ad 参考直出项目传 video_unit 的 unit_id（如 E1U2）"`
	ConfirmDuration bool   `json:"confirm_duration,omitempty" jsonschema:"description=参考生视频时长确认；同 generate_video_episode"`
}

type generateVideoAllIn struct {
	Script          string `json:"script" jsonschema:"description=剧本文件名（如 episode_1.json），必须是纯文件名，禁止任何路径分隔符"`
	ConfirmDuration bool   `json:"confirm_duration,omitempty" jsonschema:"description=参考生视频时长确认；同 generate_video_episode"`
}

type generateVideoSelectedIn struct {
	Script          string   `json:"script" jsonschema:"description=剧本文件名（如 episode_1.json），必须是纯文件名，禁止任何路径分隔符"`
	SceneIDs        []string `json:"scene_ids" jsonschema:"description=场景或片段 ID 列表；ad 参考直出项目传 video_unit 的 unit_id 列表"`
	Resume          bool     `json:"resume,omitempty" jsonschema:"description=是否从上次中断处继续；ad 参考直出项目的点名重新生成会忽略此参数"`
	ConfirmDuration bool     `json:"confirm_duration,omitempty" jsonschema:"description=参考生视频时长确认；同 generate_video_episode"`
}

// ------------------------------- 工具构造 -------------------------------

// NewGenerateVideoEpisodeTool 构造 generate_video_episode。
func NewGenerateVideoEpisodeTool(project string, deps Deps) (tool.InvokableTool, error) {
	return toolutils.InferTool("generate_video_episode",
		"为剧本对应的整集生成所有场景视频。resume=true 时从 checkpoint 续传。reference_video 模式会自动按 video_units 处理。",
		func(ctx context.Context, in generateVideoEpisodeIn) (string, error) {
			if err := tools.ValidateScriptFilename(in.Script); err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			if !deps.Ready() {
				return tools.DenyEnvelope("video generation not configured"), nil
			}
			out, err := runGenerateVideoEpisode(ctx, project, deps, in)
			if err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			return tools.OKEnvelope(out), nil
		})
}

// NewGenerateVideoSceneTool 构造 generate_video_scene。
func NewGenerateVideoSceneTool(project string, deps Deps) (tool.InvokableTool, error) {
	return toolutils.InferTool("generate_video_scene",
		"生成单个场景/片段的视频。ad 参考直出项目传 unit_id 即对该 unit 重新生成（覆盖已有成片）；"+
			"其余 reference_video 项目会忽略 scene_id 转为整集生成。",
		func(ctx context.Context, in generateVideoSceneIn) (string, error) {
			if err := tools.ValidateScriptFilename(in.Script); err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			if !deps.Ready() {
				return tools.DenyEnvelope("video generation not configured"), nil
			}
			out, err := runGenerateVideoScene(ctx, project, deps, in)
			if err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			return tools.OKEnvelope(out), nil
		})
}

// NewGenerateVideoAllTool 构造 generate_video_all。
func NewGenerateVideoAllTool(project string, deps Deps) (tool.InvokableTool, error) {
	return toolutils.InferTool("generate_video_all",
		"为剧本批量生成所有缺视频的场景/片段（独立模式，不拼接）。reference_video 模式等同 episode 模式。",
		func(ctx context.Context, in generateVideoAllIn) (string, error) {
			if err := tools.ValidateScriptFilename(in.Script); err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			if !deps.Ready() {
				return tools.DenyEnvelope("video generation not configured"), nil
			}
			out, failed, err := runGenerateVideoAll(ctx, project, deps, in)
			if err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			if failed {
				return tools.DenyEnvelope(out), nil
			}
			return tools.OKEnvelope(out), nil
		})
}

// NewGenerateVideoSelectedTool 构造 generate_video_selected。
func NewGenerateVideoSelectedTool(project string, deps Deps) (tool.InvokableTool, error) {
	return toolutils.InferTool("generate_video_selected",
		"生成指定多个场景的视频。storyboard 项目用按 scene_ids 哈希的独立 checkpoint，支持 resume 续传。"+
			"ad 参考直出项目传 unit_id 列表即对这些 unit 重新生成（覆盖已有成片），不落 checkpoint、不支持 resume；"+
			"其余 reference_video 项目会忽略 scene_ids 转整集生成。",
		func(ctx context.Context, in generateVideoSelectedIn) (string, error) {
			if err := tools.ValidateScriptFilename(in.Script); err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			if !deps.Ready() {
				return tools.DenyEnvelope("video generation not configured"), nil
			}
			out, err := runGenerateVideoSelected(ctx, project, deps, in)
			if err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			return tools.OKEnvelope(out), nil
		})
}

// ------------------------------- 入口分发 -------------------------------

func runGenerateVideoEpisode(ctx context.Context, project string, deps Deps, in generateVideoEpisodeIn) (string, error) {
	log := []string{}
	pf, projectDir, script, err := loadVideoContext(project, deps, in.Script)
	if err != nil {
		return "", err
	}
	route, err := resolveReferenceRoute(pf, script)
	if err != nil {
		return "", err
	}
	if route == "episode" {
		return runReferenceEpisode(ctx, project, deps, pf, projectDir, script, in.Script, in.Resume, in.ConfirmDuration, log)
	}
	if route == "ad" {
		return runAdReferenceEpisode(ctx, project, deps, pf, projectDir, in.Script, in.Resume, in.ConfirmDuration, log)
	}
	return runStoryboardEpisode(ctx, project, deps, pf, projectDir, script, in.Script, in.Resume, log)
}

func runGenerateVideoScene(ctx context.Context, project string, deps Deps, in generateVideoSceneIn) (string, error) {
	log := []string{}
	pf, projectDir, script, err := loadVideoContext(project, deps, in.Script)
	if err != nil {
		return "", err
	}
	route, err := resolveReferenceRoute(pf, script)
	if err != nil {
		return "", err
	}
	if route == "episode" {
		log = append(log, fmt.Sprintf(
			"⚠️  narration / drama 的 reference_video 项目暂不支持单 unit 精确选择；scene_id=%s 被忽略，转整集生成。", in.SceneID))
		return runReferenceEpisode(ctx, project, deps, pf, projectDir, script, in.Script, false, in.ConfirmDuration, log)
	}
	if route == "ad" {
		return runAdReferenceUnits(ctx, project, deps, pf, projectDir, in.Script, []string{in.SceneID}, in.ConfirmDuration, log)
	}
	return runStoryboardScene(ctx, project, deps, pf, projectDir, script, in.Script, in.SceneID, log)
}

func runGenerateVideoAll(ctx context.Context, project string, deps Deps, in generateVideoAllIn) (string, bool, error) {
	log := []string{}
	pf, projectDir, script, err := loadVideoContext(project, deps, in.Script)
	if err != nil {
		return "", true, err
	}
	route, err := resolveReferenceRoute(pf, script)
	if err != nil {
		return "", true, err
	}
	if route == "episode" {
		text, err := runReferenceEpisode(ctx, project, deps, pf, projectDir, script, in.Script, false, in.ConfirmDuration, log)
		return text, false, err
	}
	if route == "ad" {
		text, err := runAdReferenceEpisode(ctx, project, deps, pf, projectDir, in.Script, false, in.ConfirmDuration, log)
		return text, false, err
	}
	return runStoryboardAll(ctx, project, deps, pf, projectDir, script, in.Script, log)
}

func runGenerateVideoSelected(ctx context.Context, project string, deps Deps, in generateVideoSelectedIn) (string, error) {
	log := []string{}
	sceneIDs := tools.DedupStrings(in.SceneIDs)
	pf, projectDir, script, err := loadVideoContext(project, deps, in.Script)
	if err != nil {
		return "", err
	}
	route, err := resolveReferenceRoute(pf, script)
	if err != nil {
		return "", err
	}
	if route == "episode" {
		log = append(log, fmt.Sprintf(
			"⚠️  narration / drama 的 reference_video 项目暂不支持多 unit 精确选择；scene_ids=%s 被忽略，转整集生成。",
			strings.Join(sceneIDs, ",")))
		return runReferenceEpisode(ctx, project, deps, pf, projectDir, script, in.Script, in.Resume, in.ConfirmDuration, log)
	}
	if route == "ad" {
		if in.Resume {
			log = append(log, "⚠️  点名重新生成不支持续传，resume 已忽略。")
		}
		return runAdReferenceUnits(ctx, project, deps, pf, projectDir, in.Script, sceneIDs, in.ConfirmDuration, log)
	}
	return runStoryboardSelected(ctx, project, deps, pf, projectDir, script, in.Script, sceneIDs, in.Resume, log)
}

// loadVideoContext 读取项目、项目目录与剧本。
func loadVideoContext(project string, deps Deps, scriptFilename string) (
	*project.ProjectFile, string, map[string]any, error,
) {
	pf, err := deps.Projects.Get(project)
	if err != nil {
		return nil, "", nil, err
	}
	projectDir, err := deps.Projects.Dir(project)
	if err != nil {
		return nil, "", nil, err
	}
	script, err := deps.Projects.ReadScriptJSON(project, scriptFilename)
	if err != nil {
		return nil, "", nil, fmt.Errorf("剧本 %s 不可读取: %w", scriptFilename, err)
	}
	return pf, projectDir, script, nil
}

// contentModeOf 取剧本内容模式，缺失时回落到项目配置，最终默认 narration。
func contentModeOf(script map[string]any, pf *project.ProjectFile) string {
	if mode, ok := script["content_mode"].(string); ok && mode != "" {
		return mode
	}
	if pf != nil && pf.ContentMode != "" {
		return pf.ContentMode
	}
	return string(project.ContentNarration)
}

// resolveReferenceRoute 定生成路线并把守骨架闸门：参考路线返回子分支（"ad" / "episode"），
// 分镜路线返回空串。剧本骨架与路线失配时返回错误（generation 被拒）。
func resolveReferenceRoute(pf *project.ProjectFile, script map[string]any) (string, error) {
	contentMode := contentModeOf(script, pf)
	if _, err := domainscript.EnsureRouteSkeleton(script, contentMode, pf.GenerationMode); err != nil {
		return "", err
	}
	if !pf.IsReferenceVideo() {
		return "", nil
	}
	if contentMode == string(project.ContentAd) {
		return "ad", nil
	}
	return "episode", nil
}

// ------------------------- 参考路线：narration / drama -------------------------

func runReferenceEpisode(
	ctx context.Context, project string, deps Deps, pf *project.ProjectFile, projectDir string,
	script map[string]any, scriptFilename string, resume, confirmDuration bool, log []string,
) (string, error) {
	episode, err := service.ResolveEpisodeFromScript(script, scriptFilename)
	if err != nil {
		return "", err
	}
	units, err := referenceUnitsOf(script, "video_units", episode, scriptFilename)
	if err != nil {
		return "", err
	}
	if len(units) == 0 {
		return "", fmt.Errorf("第 %d 集 video_units 为空：%s", episode, scriptFilename)
	}
	paths, pending, err := generateReferenceUnits(ctx, project, deps, pf, projectDir, units, episode, resume, &log,
		episodeCheckpointPath(projectDir, episode),
		func(u []map[string]any, skip []string, lg *[]string) ([]tools.GenerationTask, map[string]int) {
			return buildReferenceSpecs(u, scriptFilename, skip, lg)
		},
		func(u map[string]any) error {
			_, err := referenceUnitTask(u, scriptFilename)
			return err
		},
		confirmDuration, nil, nil, nil)
	if err != nil {
		return "", err
	}
	if len(pending) > 0 {
		return durationConfirmationResponse(pending, log), nil
	}
	header := fmt.Sprintf("第 %d 集参考视频生成完成，共 %d 个 unit", episode, len(paths))
	return strings.Join(append([]string{header}, log...), "\n"), nil
}

// referenceUnitsOf 读取剧本的 unit 数组：键在场但类型非数组时报错（容器校验落在这里）。
func referenceUnitsOf(script map[string]any, key string, episode int, scriptFilename string) ([]map[string]any, error) {
	raw, present := script[key]
	if present && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			if key == "video_units" {
				return nil, fmt.Errorf("第 %d 集 video_units 必须是数组：%s", episode, scriptFilename)
			}
			return nil, fmt.Errorf("%s 必须是数组：%s", key, scriptFilename)
		}
		out := make([]map[string]any, 0, len(list))
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out, nil
	}
	return nil, nil
}

// ------------------------------- 参考路线：ad -------------------------------

func runAdReferenceEpisode(
	ctx context.Context, project string, deps Deps, pf *project.ProjectFile, projectDir string,
	scriptFilename string, resume, confirmDuration bool, log []string,
) (string, error) {
	maxUnitDuration := service.ResolveMaxUnitDuration(ctx, deps.Caps, pf)
	var script map[string]any
	var units []map[string]any
	episode := 0
	err := deps.Scripts.UpdateScript(project, scriptFilename, func(s map[string]any) error {
		ep, err := service.ResolveEpisodeFromScript(s, scriptFilename)
		if err != nil {
			return err
		}
		episode = ep
		units = media.SyncAdReferenceUnits(s, ep, maxUnitDuration)
		script = s
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(units) == 0 {
		return "", fmt.Errorf("剧本没有可分组的镜头：%s", scriptFilename)
	}
	log = append(log, fmt.Sprintf("已派生 %d 个 video_unit（连续镜头分组，索引已写入剧本）", len(units)))
	staleIDs := media.AdStaleUnitIDs(script, toAnyMaps(units))
	if len(staleIDs) > 0 {
		log = append(log, fmt.Sprintf(
			"⚠️  以下 unit 的剧本已变更但保留既有成片（stale）：%s。如需更新，用 generate_video_selected 点名这些 unit 重新生成。",
			strings.Join(staleIDs, ", ")))
	}
	style := pf.Style
	paths, pending, err := generateReferenceUnits(ctx, project, deps, pf, projectDir, units, episode, resume, &log,
		episodeCheckpointPath(projectDir, episode),
		func(u []map[string]any, skip []string, lg *[]string) ([]tools.GenerationTask, map[string]int) {
			return buildAdReferenceSpecs(script, u, scriptFilename, style, skip, lg)
		},
		func(u map[string]any) error {
			_, err := adReferenceTask(script, u, scriptFilename, style)
			return err
		},
		confirmDuration,
		// 剧本编辑不作废产物：有成片指针的 unit（含 stale）一律按现有产物复用；指针为空的孤儿同名文件不可信。
		func(u map[string]any) bool {
			clip, _ := media.GeneratedAssets(u)["video_clip"].(string)
			return clip != ""
		},
		func(u map[string]any) ([]map[string]any, error) {
			return media.ResolveAdUnitShots(script, u)
		},
		script)
	if err != nil {
		return "", err
	}
	if len(pending) > 0 {
		return durationConfirmationResponse(pending, log), nil
	}
	header := fmt.Sprintf("参考直出生成完成，共 %d 个 unit", len(paths))
	return strings.Join(append([]string{header}, log...), "\n"), nil
}

// runAdReferenceUnits 对点名的 unit 重新生成成片，不重新派生分组。点名即强制：已有成片一律覆盖。
func runAdReferenceUnits(
	ctx context.Context, project string, deps Deps, pf *project.ProjectFile, projectDir string,
	scriptFilename string, unitIDs []string, confirmDuration bool, log []string,
) (string, error) {
	script, err := deps.Projects.ReadScriptJSON(project, scriptFilename)
	if err != nil {
		return "", fmt.Errorf("剧本 %s 不可读取: %w", scriptFilename, err)
	}
	episode, err := service.ResolveEpisodeFromScript(script, scriptFilename)
	if err != nil {
		return "", err
	}
	selected, err := selectAdUnits(script, unitIDs, &log)
	if err != nil {
		return "", err
	}
	style := pf.Style
	// 结构校验先于在途任务探测：结构不合法的 unit 等在途任务跑完也依然生成不了。
	if err := assertAdUnitsGeneratable(script, selected, scriptFilename, style); err != nil {
		return "", err
	}
	if err := assertNoActiveTasks(ctx, deps, project, scriptFilename, selected); err != nil {
		return "", err
	}
	ids := make([]string, 0, len(selected))
	for _, u := range selected {
		id, _ := u["unit_id"].(string)
		ids = append(ids, id)
	}
	log = append(log, fmt.Sprintf("重新生成 %d 个 unit（已有成片一律覆盖）：%s", len(selected), strings.Join(ids, ", ")))

	paths, pending, err := generateReferenceUnits(ctx, project, deps, pf, projectDir, selected, episode, false, &log, "",
		func(u []map[string]any, skip []string, lg *[]string) ([]tools.GenerationTask, map[string]int) {
			return buildAdReferenceSpecs(script, u, scriptFilename, style, skip, lg)
		},
		func(u map[string]any) error {
			_, err := adReferenceTask(script, u, scriptFilename, style)
			return err
		},
		confirmDuration,
		func(map[string]any) bool { return false },
		func(u map[string]any) ([]map[string]any, error) {
			return media.ResolveAdUnitShots(script, u)
		},
		script)
	if err != nil {
		return "", err
	}
	if len(pending) > 0 {
		return durationConfirmationResponse(pending, log), nil
	}
	reportResidualStaleness(project, deps, scriptFilename, ids, &log)
	header := fmt.Sprintf("参考直出重新生成完成，共 %d 个 unit", len(paths))
	return strings.Join(append([]string{header}, log...), "\n"), nil
}

// selectAdUnits 按 unit_id 从持久化的分组索引里点名取 unit，重复 ID 只取一次。
func selectAdUnits(script map[string]any, unitIDs []string, log *[]string) ([]map[string]any, error) {
	byID := map[string]map[string]any{}
	if indexed, ok := script["reference_units"].([]any); ok {
		for _, raw := range indexed {
			u, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if id, ok := u["unit_id"].(string); ok && id != "" {
				if _, exists := byID[id]; !exists {
					byID[id] = u
				}
			}
		}
	}

	var selected []map[string]any
	for _, unitID := range tools.DedupStrings(unitIDs) {
		unit, ok := byID[unitID]
		if !ok {
			*log = append(*log, fmt.Sprintf("⚠️  unit %s 不在分组索引中，跳过", unitID))
			continue
		}
		selected = append(selected, unit)
	}
	if len(selected) == 0 {
		known := "（索引为空，请先整集生成或重新派生分组）"
		if len(byID) > 0 {
			keys := make([]string, 0, len(byID))
			for id := range byID {
				keys = append(keys, id)
			}
			sort.Strings(keys)
			known = strings.Join(keys, "、")
		}
		return nil, fmt.Errorf("没有匹配到任何 unit：%s；索引中现有 %s", strings.Join(unitIDs, ", "), known)
	}
	return selected, nil
}

// assertAdUnitsGeneratable 点名的 unit 逐个当场校验可入队性，不合法即抛错。
func assertAdUnitsGeneratable(script map[string]any, units []map[string]any, scriptFilename, style string) error {
	for _, unit := range units {
		if _, err := adReferenceTask(script, unit, scriptFilename, style); err != nil {
			return fmt.Errorf("unit %v 无法生成：%w", unit["unit_id"], err)
		}
	}
	return nil
}

// reportResidualStaleness 生成后按签名复核点名 unit 是否仍偏离编排，仍偏离的明说；复核失败只记日志。
func reportResidualStaleness(project string, deps Deps, scriptFilename string, unitIDs []string, log *[]string) {
	fresh, err := deps.Projects.ReadScriptJSON(project, scriptFilename)
	if err != nil {
		*log = append(*log, fmt.Sprintf("⚠️  无法复核重新生成后的 stale 状态：%v", err))
		return
	}
	indexed, ok := fresh["reference_units"].([]any)
	if !ok {
		*log = append(*log, fmt.Sprintf("⚠️  无法复核重新生成后的 stale 状态：reference_units 必须是数组"))
		return
	}
	wanted := map[string]bool{}
	for _, id := range unitIDs {
		wanted[id] = true
	}
	found := map[string]bool{}
	var units []any
	for _, raw := range indexed {
		u, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := u["unit_id"].(string)
		if wanted[id] {
			found[id] = true
			units = append(units, u)
		}
	}
	var missing []string
	for id := range wanted {
		if !found[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		*log = append(*log, fmt.Sprintf("⚠️  无法复核重新生成后的 stale 状态：以下 unit 已不在分组索引中：%s",
			strings.Join(missing, "、")))
		return
	}
	if residual := media.AdStaleUnitIDs(fresh, units); len(residual) > 0 {
		*log = append(*log, fmt.Sprintf(
			"⚠️  以下 unit 重新生成后仍偏离当前编排（stale），可能是生成期间剧本又被改动：%s", strings.Join(residual, ", ")))
	}
}

// assertNoActiveTasks 点名重做前探测同 unit 是否已有在途任务：命中即拒绝整批。
func assertNoActiveTasks(ctx context.Context, deps Deps, project, scriptFilename string, units []map[string]any) error {
	ids := make([]string, 0, len(units))
	for _, u := range units {
		id, _ := u["unit_id"].(string)
		ids = append(ids, id)
	}
	scriptFile := scriptFilename
	active, err := deps.Queue.GetActiveTasksForResources(ctx, project, "reference_video", ids, &scriptFile, nil)
	if err != nil {
		return err
	}
	if len(active) == 0 {
		return nil
	}
	details := make([]string, 0, len(active))
	for _, task := range active {
		details = append(details, fmt.Sprintf("%s（状态：%s）", task.ResourceID, task.Status))
	}
	return fmt.Errorf("以下 unit 已有在途任务，请等待其完成后再重做：%s", strings.Join(details, "、"))
}

// ----------------------------- 参考路线：共享骨架 -----------------------------

// referenceSpecBuilder 把 unit 列表构造为入队任务与 order 表；坏 unit 跳过并记日志，不中断整批。
type referenceSpecBuilder func(units []map[string]any, skipIDs []string, log *[]string) ([]tools.GenerationTask, map[string]int)

// durationPendingItem 是一条待确认的 unit 时长（申请秒数与剧本编排不一致）。
type durationPendingItem struct {
	UnitID          string
	ScriptDuration  float64
	RequestDuration int
	Adjustment      string
}

// durationConfirmationResponse 把待确认清单连同本次已产生的 log 一并转述。
func durationConfirmationResponse(pending []durationPendingItem, log []string) string {
	lines := append([]string{}, log...)
	lines = append(lines, "以下 unit 申请时长与剧本编排不一致，需先向用户确认，本次未入队任何任务：")
	for _, item := range pending {
		word := "更短"
		if item.Adjustment == service.AdjustmentUp {
			word = "更长"
		}
		lines = append(lines, fmt.Sprintf("- %s：剧本总时长 %vs，将申请 %ds（成片%s）",
			item.UnitID, formatSeconds(item.ScriptDuration), item.RequestDuration, word))
	}
	lines = append(lines, "用户同意后，带 confirm_duration=true 再次调用本工具完成入队。")
	return strings.Join(lines, "\n")
}

// formatSeconds 按 Python 的 int|float 展示口径渲染秒数（整数值不带小数点）。
func formatSeconds(value float64) string {
	if value == float64(int(value)) {
		return fmt.Sprintf("%d", int(value))
	}
	return fmt.Sprintf("%v", value)
}

// generateReferenceUnits 是 unit 批量生成的共享骨架：时长确认 + checkpoint 续传 + 已产出扫描 + 入队等待。
//
// confirmDuration 为 false 且存在申请时长与剧本编排不一致的 unit 时，本次不产生任何任务，返回待确认清单。
// checkpointPath 为空表示不落 checkpoint（点名重新生成一律强制覆盖，没有可续传语义）。
func generateReferenceUnits(
	ctx context.Context, project string, deps Deps, pf *project.ProjectFile, projectDir string,
	units []map[string]any, episode int, resume bool, log *[]string, checkpointPath string,
	buildSpecs referenceSpecBuilder, specFor func(map[string]any) error, confirmDuration bool,
	reuseExisting func(map[string]any) bool, adShotsFor func(map[string]any) ([]map[string]any, error),
	preflightScript map[string]any,
) ([]string, []durationPendingItem, error) {
	completed := []string{}
	startedAt := time.Now().UTC().Format(time.RFC3339)
	if resume && checkpointPath != "" {
		if ckpt, ok := loadVideoCheckpoint(checkpointPath); ok {
			completed = ckpt.CompletedScenes
			if ckpt.StartedAt != "" {
				startedAt = ckpt.StartedAt
			}
		}
	}

	outputDir := filepath.Join(projectDir, "reference_videos")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, nil, err
	}

	orderedPaths := make([]string, len(units))
	alreadyDone := []string{}
	for idx, unit := range units {
		unitID, _ := unit["unit_id"].(string)
		candidate := filepath.Join(outputDir, unitID+".mp4")
		if fileExists(candidate) && (reuseExisting == nil || reuseExisting(unit)) {
			orderedPaths[idx] = candidate
			alreadyDone = append(alreadyDone, unitID)
			if !tools.Contains(completed, unitID) {
				completed = append(completed, unitID)
			}
		} else if idx := indexOf(completed, unitID); idx >= 0 {
			completed = append(completed[:idx], completed[idx+1:]...)
		}
	}

	skipSet := stringSet(alreadyDone)
	if message, err := serviceref.ValidateReferenceUnitsPreflight(pf, projectDir, units, preflightScript, skipSet); err != nil {
		return nil, nil, err
	} else if message != "" {
		return nil, nil, errors.New(message)
	}

	if err := assertAudioSwitchForUnits(ctx, deps, pf, units, skipSet, specFor, adShotsFor); err != nil {
		return nil, nil, err
	}

	if !confirmDuration {
		pending, err := pendingDurationConfirmations(ctx, deps, pf, units, skipSet, specFor, adShotsFor)
		if err != nil {
			return nil, nil, err
		}
		if len(pending) > 0 {
			return nil, pending, nil
		}
	}

	specs, orderMap := buildSpecs(units, alreadyDone, log)
	if len(specs) > 0 {
		save := func() {
			if checkpointPath == "" {
				return
			}
			saveVideoCheckpoint(checkpointPath, completed, startedAt, map[string]any{"episode": episode})
		}
		failures, err := submitVideoBatch(ctx, deps, project, "reference_video", projectDir, specs, orderMap,
			orderedPaths, &completed, referenceFallbackRelpath, save, log)
		if err != nil {
			return nil, nil, err
		}
		if len(failures) > 0 {
			return nil, nil, fmt.Errorf("%d 个 unit 生成失败", len(failures))
		}
	}

	final := nonEmptyPaths(orderedPaths)
	if len(final) == 0 {
		return nil, nil, errors.New("没有生成任何 video_unit")
	}
	if checkpointPath != "" {
		clearVideoCheckpoint(checkpointPath)
	}
	return final, nil, nil
}

// assertAudioSwitchForUnits 是参考路线入队前的音频闸门，按本批真正要入队的 unit 所属能力桶逐桶检查。
func assertAudioSwitchForUnits(
	ctx context.Context, deps Deps, pf *project.ProjectFile, units []map[string]any,
	skipIDs map[string]bool, specFor func(map[string]any) error,
	adShotsFor func(map[string]any) ([]map[string]any, error),
) error {
	checked := map[string]bool{}
	for _, unit := range units {
		unitID, _ := unit["unit_id"].(string)
		if unitID == "" || skipIDs[unitID] {
			continue
		}
		if err := specFor(unit); err != nil {
			continue
		}
		adShots, err := resolveAdShots(adShotsFor, unit)
		if err != nil {
			continue
		}
		bucket := media.ReferenceUnitVideoBucket(unit, adShots)
		if checked[bucket] {
			continue
		}
		checked[bucket] = true
		if err := service.AssertAudioSwitchSupported(ctx, deps.Caps, pf, videoBucketOf(bucket)); err != nil {
			return err
		}
	}
	return nil
}

// pendingDurationConfirmations 收集本批将入队的 unit 中，申请时长与剧本编排不一致的清单。
func pendingDurationConfirmations(
	ctx context.Context, deps Deps, pf *project.ProjectFile, units []map[string]any,
	skipIDs map[string]bool, specFor func(map[string]any) error,
	adShotsFor func(map[string]any) ([]map[string]any, error),
) ([]durationPendingItem, error) {
	ctxs := map[string]service.ProjectDurationContext{}
	var items []durationPendingItem
	for _, unit := range units {
		unitID, _ := unit["unit_id"].(string)
		if unitID == "" || skipIDs[unitID] {
			continue
		}
		if err := specFor(unit); err != nil {
			continue
		}
		adShots, err := resolveAdShots(adShotsFor, unit)
		if err != nil {
			continue
		}
		bucket := media.ReferenceUnitVideoBucket(unit, adShots)
		dctx, ok := ctxs[bucket]
		if !ok {
			dctx = service.ResolveProjectDurationContext(ctx, deps.Caps, pf, videoBucketOf(bucket))
			ctxs[bucket] = dctx
		}
		slot, err := service.PrecheckUnit(dctx, unit, adShots)
		if err != nil {
			continue
		}
		if slot.NeedsConfirmation() {
			items = append(items, durationPendingItem{
				UnitID:          unitID,
				ScriptDuration:  slot.TotalSeconds,
				RequestDuration: slot.Seconds,
				Adjustment:      slot.Adjustment,
			})
		}
	}
	return items, nil
}

func resolveAdShots(
	adShotsFor func(map[string]any) ([]map[string]any, error), unit map[string]any,
) ([]map[string]any, error) {
	if adShotsFor == nil {
		return nil, nil
	}
	return adShotsFor(unit)
}

func videoBucketOf(bucket string) provider.VideoBucket {
	if bucket == "r2v" {
		return provider.BucketR2V
	}
	return provider.BucketI2V
}

// buildReferenceSpecs 为 narration/drama 的 unit 构造入队任务；坏 unit 跳过并告警，不中断整批。
func buildReferenceSpecs(
	units []map[string]any, scriptFilename string, skipIDs []string, log *[]string,
) ([]tools.GenerationTask, map[string]int) {
	skipSet := stringSet(skipIDs)
	var tasks []tools.GenerationTask
	orderMap := map[string]int{}
	for idx, unit := range units {
		unitID, _ := unit["unit_id"].(string)
		unitID = strings.TrimSpace(unitID)
		if skipSet[unitID] {
			continue
		}
		task, err := referenceUnitTask(unit, scriptFilename)
		if err != nil {
			*log = append(*log, fmt.Sprintf("⚠️  %s 入队校验未通过，跳过：%v", unitID, err))
			continue
		}
		tasks = append(tasks, task)
		orderMap[task.ResourceID] = idx
	}
	return tasks, orderMap
}

// buildAdReferenceSpecs 为 ad 派生索引构造入队任务；悬空索引 / 空画面提示词的 unit 跳过并告警。
func buildAdReferenceSpecs(
	script map[string]any, units []map[string]any, scriptFilename, style string, skipIDs []string, log *[]string,
) ([]tools.GenerationTask, map[string]int) {
	skipSet := stringSet(skipIDs)
	var tasks []tools.GenerationTask
	orderMap := map[string]int{}
	for idx, unit := range units {
		unitID, _ := unit["unit_id"].(string)
		if skipSet[unitID] {
			continue
		}
		task, err := adReferenceTask(script, unit, scriptFilename, style)
		if err != nil {
			*log = append(*log, fmt.Sprintf("⚠️  %s 入队校验未通过，跳过：%v", unitID, err))
			continue
		}
		tasks = append(tasks, task)
		orderMap[task.ResourceID] = idx
	}
	return tasks, orderMap
}

// referenceUnitTask 是单 unit 的 narration/drama 入队任务构造（含与预检共用的结构校验）。
func referenceUnitTask(unit map[string]any, scriptFilename string) (tools.GenerationTask, error) {
	unitID := strings.TrimSpace(stringOf(unit["unit_id"]))
	shots, _ := unit["shots"].([]any)
	if len(shots) == 0 {
		return tools.GenerationTask{}, errors.New("没有 shots")
	}
	prompt := media.AssembleShotsText(unit["shots"])
	if err := validateVideoTaskSpec(unitID, prompt); err != nil {
		return tools.GenerationTask{}, err
	}
	return tools.GenerationTask{ResourceID: unitID, Prompt: prompt, ScriptFile: scriptFilename}, nil
}

// adReferenceTask 是单 unit 的 ad 入队任务构造；成员镜头从 shots 水合后渲染 prompt。
func adReferenceTask(script map[string]any, unit map[string]any, scriptFilename, style string) (tools.GenerationTask, error) {
	unitID := strings.TrimSpace(stringOf(unit["unit_id"]))
	shots, err := media.ResolveAdUnitShots(script, unit)
	if err != nil {
		return tools.GenerationTask{}, err
	}
	stylePtr := &style
	prompt := media.RenderAdUnitPrompt(shots, stylePtr)
	if err := validateVideoTaskSpec(unitID, prompt); err != nil {
		return tools.GenerationTask{}, err
	}
	return tools.GenerationTask{ResourceID: unitID, Prompt: prompt, ScriptFile: scriptFilename}, nil
}

// validateVideoTaskSpec 复刻 TaskSpec.from_request 的可入队性判据（空 resource_id / 空提示词拒绝）。
func validateVideoTaskSpec(resourceID, prompt string) error {
	if strings.TrimSpace(resourceID) == "" {
		return errors.New("resource_id 不能为空")
	}
	if strings.TrimSpace(prompt) == "" {
		return errors.New("提示词不能为空")
	}
	return nil
}

// ------------------------------- 分镜路线 -------------------------------

func runStoryboardEpisode(
	ctx context.Context, project string, deps Deps, pf *project.ProjectFile, projectDir string,
	script map[string]any, scriptFilename string, resume bool, log []string,
) (string, error) {
	episode, err := service.ResolveEpisodeFromScript(script, scriptFilename)
	if err != nil {
		return "", err
	}
	items, idField, fields := media.StoryboardItems(script)
	contentMode := contentModeOf(script, pf)
	if len(items) == 0 {
		return "", fmt.Errorf("第 %d 集剧本为空：%s", episode, scriptFilename)
	}

	ckptPath := episodeCheckpointPath(projectDir, episode)
	completed := []string{}
	startedAt := time.Now().UTC().Format(time.RFC3339)
	if resume {
		if ckpt, ok := loadVideoCheckpoint(ckptPath); ok {
			completed = ckpt.CompletedScenes
			if ckpt.StartedAt != "" {
				startedAt = ckpt.StartedAt
			}
		}
	}

	videosDir := filepath.Join(projectDir, "videos")
	if err := os.MkdirAll(videosDir, 0o755); err != nil {
		return "", err
	}
	orderedPaths, alreadyDone, completed := scanCompletedVideoItems(items, idField, completed, videosDir)
	if err := preflightVideoItems(pf, projectDir, items, fields, idField, stringSet(alreadyDone)); err != nil {
		return "", err
	}
	voiceCharacters := voiceContextFor(ctx, deps, pf, contentMode)
	specs, orderMap := buildStoryboardVideoSpecs(items, idField, contentMode, scriptFilename, projectDir,
		alreadyDone, &log, voiceCharacters)

	if len(specs) == 0 && countNonEmpty(orderedPaths) == 0 {
		return "", errors.New("没有可生成的视频片段")
	}

	if len(specs) > 0 {
		if err := service.AssertAudioSwitchSupported(ctx, deps.Caps, pf, videoBucketForRoute(pf)); err != nil {
			return "", err
		}
		failures, err := submitVideoBatch(ctx, deps, project, "video", projectDir, specs, orderMap, orderedPaths,
			&completed, sceneFallbackRelpath, func() {
				saveVideoCheckpoint(ckptPath, completed, startedAt, map[string]any{"episode": episode})
			}, &log)
		if err != nil {
			return "", err
		}
		if len(failures) > 0 {
			return "", fmt.Errorf("%d 个视频生成失败（使用 resume=true 续传）", len(failures))
		}
	}

	sceneVideos := nonEmptyPaths(orderedPaths)
	clearVideoCheckpoint(ckptPath)
	header := fmt.Sprintf("第 %d 集视频生成完成，共 %d 个片段", episode, len(sceneVideos))
	return strings.Join(append([]string{header}, log...), "\n"), nil
}

func runStoryboardScene(
	ctx context.Context, project string, deps Deps, pf *project.ProjectFile, projectDir string,
	script map[string]any, scriptFilename, sceneID string, log []string,
) (string, error) {
	items, idField, fields := media.StoryboardItems(script)
	item, _, ok := findStoryboardItemByAlias(items, idField, sceneID)
	if !ok {
		return "", fmt.Errorf("场景/片段 '%s' 不存在", sceneID)
	}
	// 入队 / 文件名 / fallback 必须用脚本里的规范 id_field 值。
	itemID := fmt.Sprint(item[idField])
	if err := preflightVideoItems(pf, projectDir, []any{item}, fields, idField, map[string]bool{}); err != nil {
		return "", err
	}
	storyboardPath, err := storyboardPathFor(item, projectDir)
	if err != nil {
		return "", err
	}
	if storyboardPath == "" {
		return "", fmt.Errorf("场景/片段 '%s' 没有分镜图，请先运行 generate_storyboards", itemID)
	}
	if !fileExists(storyboardPath) {
		return "", fmt.Errorf("分镜图不存在: %s", filepath.Base(storyboardPath))
	}

	contentMode := contentModeOf(script, pf)
	voiceCharacters := voiceContextFor(ctx, deps, pf, contentMode)
	prompt, err := videoPromptForItem(item, contentMode, voiceCharacters)
	if err != nil {
		return "", err
	}
	task := tools.GenerationTask{ResourceID: itemID, Prompt: prompt, ScriptFile: scriptFilename,
		ExtraPayload: durationPayload(item)}
	if err := service.AssertAudioSwitchSupported(ctx, deps.Caps, pf, videoBucketForRoute(pf)); err != nil {
		return "", err
	}
	taskIDs, err := tools.EnqueueGenerationTasks(ctx, deps.Queue, project, "video", []tools.GenerationTask{task})
	if err != nil {
		return "", err
	}
	successes, failures, err := tools.AwaitGenerationTasks(ctx, deps.Queue, []tools.GenerationTask{task}, taskIDs)
	if err != nil {
		return "", err
	}
	if len(failures) > 0 {
		return "", fmt.Errorf("%s: %s", itemID, failures[0].Message)
	}
	rel, _ := successes[0].Details["file_path"].(string)
	if rel == "" {
		rel = sceneFallbackRelpath(itemID)
	}
	outputPath := filepath.Join(projectDir, filepath.FromSlash(rel))
	return fmt.Sprintf("✅ 视频已保存: %s", outputPath), nil
}

func runStoryboardAll(
	ctx context.Context, project string, deps Deps, pf *project.ProjectFile, projectDir string,
	script map[string]any, scriptFilename string, log []string,
) (string, bool, error) {
	items, idField, fields := media.StoryboardItems(script)
	contentMode := contentModeOf(script, pf)
	pending := pendingVideoItems(items)
	if len(pending) == 0 {
		return "✨ 所有场景/片段的视频都已生成", false, nil
	}
	if err := preflightVideoItems(pf, projectDir, pending, fields, idField, map[string]bool{}); err != nil {
		return "", true, err
	}
	voiceCharacters := voiceContextFor(ctx, deps, pf, contentMode)
	specs, _ := buildStoryboardVideoSpecs(pending, idField, contentMode, scriptFilename, projectDir, nil, &log, voiceCharacters)
	if len(specs) == 0 {
		return strings.Join(append(log, "⚠️  没有任何可生成的视频任务"), "\n"), false, nil
	}
	if err := service.AssertAudioSwitchSupported(ctx, deps.Caps, pf, videoBucketForRoute(pf)); err != nil {
		return "", true, err
	}
	taskIDs, err := tools.EnqueueGenerationTasks(ctx, deps.Queue, project, "video", specs)
	if err != nil {
		return "", true, err
	}
	successes, failures, err := tools.AwaitGenerationTasks(ctx, deps.Queue, specs, taskIDs)
	if err != nil {
		return "", true, err
	}
	lines := []string{fmt.Sprintf("generate_video_all summary: %d succeeded, %d failed", len(successes), len(failures))}
	lines = append(lines, log...)
	for _, s := range successes {
		rel, _ := s.Details["file_path"].(string)
		if rel == "" {
			rel = sceneFallbackRelpath(s.ResourceID)
		}
		lines = append(lines, fmt.Sprintf("  ✓ %s → %s", s.ResourceID, rel))
	}
	for _, f := range failures {
		lines = append(lines, fmt.Sprintf("  ✗ %s: %s", f.ResourceID, f.Message))
	}
	return strings.Join(lines, "\n"), len(failures) > 0, nil
}

func runStoryboardSelected(
	ctx context.Context, project string, deps Deps, pf *project.ProjectFile, projectDir string,
	script map[string]any, scriptFilename string, sceneIDs []string, resume bool, log []string,
) (string, error) {
	items, idField, fields := media.StoryboardItems(script)
	contentMode := contentModeOf(script, pf)

	itemsByID := map[string]map[string]any{}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		itemsByID[fmt.Sprint(item[idField])] = item
		if sceneID, ok := item["scene_id"].(string); ok {
			itemsByID[sceneID] = item
		}
	}

	var selected []map[string]any
	seenCanonical := map[string]bool{}
	for _, sid := range sceneIDs {
		item, ok := itemsByID[sid]
		if !ok {
			log = append(log, fmt.Sprintf("⚠️  场景/片段 '%s' 不存在，跳过", sid))
			continue
		}
		canonical := fmt.Sprint(item[idField])
		if canonical != "" && seenCanonical[canonical] {
			continue
		}
		seenCanonical[canonical] = true
		selected = append(selected, item)
	}
	if len(selected) == 0 {
		return "", errors.New("没有找到任何有效的场景/片段")
	}

	canonicalIDs := make([]string, 0, len(seenCanonical))
	for id := range seenCanonical {
		canonicalIDs = append(canonicalIDs, id)
	}
	sort.Strings(canonicalIDs)
	sum := md5.Sum([]byte(strings.Join(canonicalIDs, ",")))
	scenesHash := hex.EncodeToString(sum[:])[:8]
	ckptPath := selectedCheckpointPath(projectDir, scenesHash)

	completed := []string{}
	startedAt := time.Now().UTC().Format(time.RFC3339)
	if resume {
		if ckpt, ok := loadVideoCheckpoint(ckptPath); ok {
			completed = ckpt.CompletedScenes
			if ckpt.StartedAt != "" {
				startedAt = ckpt.StartedAt
			}
		}
	}

	videosDir := filepath.Join(projectDir, "videos")
	if err := os.MkdirAll(videosDir, 0o755); err != nil {
		return "", err
	}
	selectedAny := make([]any, 0, len(selected))
	for _, item := range selected {
		selectedAny = append(selectedAny, item)
	}
	orderedPaths, alreadyDone, completed := scanCompletedVideoItems(selectedAny, idField, completed, videosDir)
	if err := preflightVideoItems(pf, projectDir, selectedAny, fields, idField, stringSet(alreadyDone)); err != nil {
		return "", err
	}
	voiceCharacters := voiceContextFor(ctx, deps, pf, contentMode)
	specs, orderMap := buildStoryboardVideoSpecs(selectedAny, idField, contentMode, scriptFilename, projectDir,
		alreadyDone, &log, voiceCharacters)

	if len(specs) == 0 && countNonEmpty(orderedPaths) == 0 {
		return "", errors.New("没有任何可生成的视频任务（全部 selected 都被跳过）")
	}

	if len(specs) > 0 {
		if err := service.AssertAudioSwitchSupported(ctx, deps.Caps, pf, videoBucketForRoute(pf)); err != nil {
			return "", err
		}
		failures, err := submitVideoBatch(ctx, deps, project, "video", projectDir, specs, orderMap, orderedPaths,
			&completed, sceneFallbackRelpath, func() {
				saveVideoCheckpoint(ckptPath, completed, startedAt, map[string]any{"scene_ids": toAnyStrings(sceneIDs)})
			}, &log)
		if err != nil {
			return "", err
		}
		if len(failures) > 0 {
			return "", fmt.Errorf("%d 个视频生成失败（使用 resume=true 续传）", len(failures))
		}
	}

	final := nonEmptyPaths(orderedPaths)
	clearVideoCheckpoint(ckptPath)
	header := fmt.Sprintf("generate_video_selected 完成：%d 个", len(final))
	return strings.Join(append([]string{header}, log...), "\n"), nil
}

// ----------------------------- 分镜路线：共用件 -----------------------------

// buildStoryboardVideoSpecs 为条目构造入队任务；缺分镜图或 video_prompt 无效的条目跳过并记日志。
func buildStoryboardVideoSpecs(
	items []any, idField, contentMode, scriptFilename, projectDir string, skipIDs []string,
	log *[]string, voiceCharacters *project.AssetBucket,
) ([]tools.GenerationTask, map[string]int) {
	itemType := "片段"
	if contentMode != string(project.ContentNarration) {
		itemType = "场景"
	}
	skipSet := stringSet(skipIDs)
	var tasks []tools.GenerationTask
	orderMap := map[string]int{}
	for idx, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		itemID := storyboardItemID(item, idField, idx)
		if skipSet[itemID] {
			continue
		}
		storyboardImage := media.GeneratedAssets(item)["storyboard_image"]
		storyboardPath, err := service.ResolveStoryboardImageRef(projectDir, storyboardImage)
		if err != nil {
			*log = append(*log, fmt.Sprintf("⚠️  %s %s 的分镜图引用无效，跳过: %v", itemType, itemID, err))
			continue
		}
		if storyboardPath == "" {
			*log = append(*log, fmt.Sprintf("⚠️  %s %s 没有分镜图，跳过", itemType, itemID))
			continue
		}
		if !fileExists(storyboardPath) {
			*log = append(*log, fmt.Sprintf("⚠️  分镜图不存在: %s，跳过", filepath.Base(storyboardPath)))
			continue
		}
		prompt, err := videoPromptForItem(item, contentMode, voiceCharacters)
		if err != nil {
			*log = append(*log, fmt.Sprintf("⚠️  %s %s 的 video_prompt 无效，跳过: %v", itemType, itemID, err))
			continue
		}
		tasks = append(tasks, tools.GenerationTask{
			ResourceID:   itemID,
			Prompt:       prompt,
			ScriptFile:   scriptFilename,
			ExtraPayload: durationPayload(item),
		})
		orderMap[itemID] = idx
	}
	return tasks, orderMap
}

// videoPromptForItem 取条目的 video prompt：结构化 prompt 走 YAML 渲染（drama 注入台词与声线），
// 纯字符串原样返回。
func videoPromptForItem(item map[string]any, contentMode string, voiceCharacters *project.AssetBucket) (string, error) {
	prompt := item["video_prompt"]
	if !promptPresent(prompt) {
		return "", fmt.Errorf("片段/场景缺少 video_prompt 字段: %s", itemPromptID(item))
	}
	if service.IsStructuredVideoPrompt(prompt) {
		m := service.StripVoiceProfiles(prompt.(map[string]any))
		if contentMode == string(project.ContentDrama) {
			if _, hasUtterances := item["utterances"]; hasUtterances {
				m = service.BuildDramaVideoPrompt(m, item["utterances"], voiceCharacters)
			} else {
				m = service.BuildDramaVideoPromptFromLegacyDialogue(m, voiceCharacters)
			}
		}
		return service.VideoPromptToYAML(m)
	}
	if _, isMap := prompt.(map[string]any); isMap {
		return "", fmt.Errorf("片段/场景 video_prompt 为对象但格式不符合结构化规范: %s", itemPromptID(item))
	}
	if text, isString := prompt.(string); isString {
		return text, nil
	}
	return "", fmt.Errorf("片段/场景 video_prompt 类型无效（期望 str 或 dict）: %s", itemPromptID(item))
}

// durationPayload 取条目显式指定的 duration_seconds（原样透传，不在入队侧做归一化）。
func durationPayload(item map[string]any) map[string]any {
	if duration, present := item["duration_seconds"]; present && duration != nil {
		return map[string]any{"duration_seconds": duration}
	}
	return nil
}

// voiceContextFor 供 Voice_Profiles 注入的角色资产（nil 表示不注入）：非 drama 不注入；
// drama 按无声判据排除。
func voiceContextFor(ctx context.Context, deps Deps, pf *project.ProjectFile, contentMode string) *project.AssetBucket {
	if contentMode != string(project.ContentDrama) {
		return nil
	}
	if service.ResolveProjectIsSilent(ctx, deps.Caps, pf) {
		return nil
	}
	return &pf.Characters
}

// videoBucketForRoute 按项目生成路线定桶（分镜路线恒 i2v）。
func videoBucketForRoute(pf *project.ProjectFile) provider.VideoBucket {
	return provider.VideoBucketForGenerationMode(pf.GenerationMode)
}

// preflightVideoItems 在入队前拒绝整批：任一待入队条目引用了无可用设计图的资产即阻断。
func preflightVideoItems(
	pf *project.ProjectFile, projectDir string, items []any, fields media.StoryboardItemFields,
	idField string, skipIDs map[string]bool,
) error {
	var results []service.PreflightResult
	for idx, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if skipIDs[storyboardItemID(item, idField, idx)] {
			continue
		}
		results = append(results, service.ValidateItemAssetReferences(pf, projectDir, item, fields))
	}
	if message := service.FormatPreflightError(service.MergePreflightResults(results...)); message != "" {
		return errors.New(message)
	}
	return nil
}

// submitVideoBatch 入队一批任务并逐个等待终态：每个成功的任务更新 checkpoint 与 ordered 表。
func submitVideoBatch(
	ctx context.Context, deps Deps, project, taskType, projectDir string, specs []tools.GenerationTask,
	orderMap map[string]int, orderedPaths []string, completed *[]string,
	fallback func(string) string, save func(), log *[]string,
) ([]tools.GenerationOutcome, error) {
	taskIDs, err := tools.EnqueueGenerationTasks(ctx, deps.Queue, project, taskType, specs)
	if err != nil {
		return nil, err
	}
	var failures []tools.GenerationOutcome
	for i, taskID := range taskIDs {
		outcome, err := tools.AwaitAssetTask(ctx, deps.Queue, taskID)
		if err != nil {
			return nil, err
		}
		result := tools.GenerationOutcome{ResourceID: specs[i].ResourceID, Status: outcome.Status, Details: outcome.Result}
		if outcome.ErrorMessage != nil {
			result.Message = *outcome.ErrorMessage
		}
		if outcome.Status == repository.TaskStatusSucceeded {
			rel, _ := outcome.Result["file_path"].(string)
			if rel == "" {
				rel = fallback(specs[i].ResourceID)
			}
			outputPath := filepath.Join(projectDir, filepath.FromSlash(rel))
			if idx, ok := orderMap[specs[i].ResourceID]; ok && idx < len(orderedPaths) {
				orderedPaths[idx] = outputPath
			}
			*completed = append(*completed, specs[i].ResourceID)
			save()
			*log = append(*log, fmt.Sprintf("    ✓ %s", filepath.Base(outputPath)))
			continue
		}
		if result.Message == "" {
			result.Message = outcome.Status
		}
		failures = append(failures, result)
		*log = append(*log, fmt.Sprintf("    ✗ %s: %s", specs[i].ResourceID, result.Message))
	}
	return failures, nil
}

// scanCompletedVideoItems 用磁盘上的成片核对 checkpoint 声称已完成的分镜，返回
// (ordered_paths, already_done, completed_filtered)。checkpoint 声称完成但文件已缺失的 id 被剔除，
// 由调用方写回。
func scanCompletedVideoItems(
	items []any, idField string, completedScenes []string, videosDir string,
) (orderedPaths []string, alreadyDone []string, completedFiltered []string) {
	orderedPaths = make([]string, len(items))
	stale := map[string]bool{}
	for idx, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		itemID := storyboardItemID(item, idField, idx)
		if !tools.Contains(completedScenes, itemID) {
			continue
		}
		videoOutput := filepath.Join(videosDir, "scene_"+itemID+".mp4")
		if fileExists(videoOutput) {
			orderedPaths[idx] = videoOutput
			alreadyDone = append(alreadyDone, itemID)
		} else {
			stale[itemID] = true
		}
	}
	for _, id := range completedScenes {
		if !stale[id] {
			completedFiltered = append(completedFiltered, id)
		}
	}
	return orderedPaths, alreadyDone, completedFiltered
}

// ------------------------------- checkpoint -------------------------------

type videoCheckpoint struct {
	CompletedScenes []string `json:"completed_scenes"`
	StartedAt       string   `json:"started_at"`
}

func episodeCheckpointPath(projectDir string, episode int) string {
	return filepath.Join(projectDir, "videos", fmt.Sprintf(".checkpoint_ep%d.json", episode))
}

func selectedCheckpointPath(projectDir, scenesHash string) string {
	return filepath.Join(projectDir, "videos", ".checkpoint_selected_"+scenesHash+".json")
}

func loadVideoCheckpoint(path string) (videoCheckpoint, bool) {
	if !fileExists(path) {
		return videoCheckpoint{}, false
	}
	var ckpt videoCheckpoint
	if err := fs.ReadJSON(path, &ckpt); err != nil {
		return videoCheckpoint{}, false
	}
	return ckpt, true
}

func saveVideoCheckpoint(path string, completed []string, startedAt string, extra map[string]any) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	payload := map[string]any{
		"completed_scenes": completed,
		"started_at":       startedAt,
		"updated_at":       time.Now().UTC().Format(time.RFC3339),
	}
	for key, value := range extra {
		payload[key] = value
	}
	_ = fs.WriteJSONAtomic(path, payload)
}

func clearVideoCheckpoint(path string) {
	if fileExists(path) {
		_ = os.Remove(path)
	}
}

// ------------------------------- 辅助 -------------------------------

func sceneFallbackRelpath(resourceID string) string {
	return "videos/scene_" + resourceID + ".mp4"
}

func referenceFallbackRelpath(resourceID string) string {
	return "reference_videos/" + resourceID + ".mp4"
}

// storyboardItemID 复刻 Python `item.get(id_field) or item.get("scene_id") or item.get("segment_id") or f"item_{idx}"`。
func storyboardItemID(item map[string]any, idField string, idx int) string {
	for _, key := range []string{idField, "scene_id", "segment_id"} {
		if key == "" {
			continue
		}
		if value, ok := item[key].(string); ok && value != "" {
			return value
		}
	}
	return fmt.Sprintf("item_%d", idx)
}

// itemPromptID 复刻 Python `item.get("segment_id") or item.get("scene_id")`（用于报错定位）。
func itemPromptID(item map[string]any) string {
	for _, key := range []string{"segment_id", "scene_id"} {
		if value, ok := item[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// storyboardPathFor 取条目的分镜图绝对路径（"" 表示未设置）。
func storyboardPathFor(item map[string]any, projectDir string) (string, error) {
	return service.ResolveStoryboardImageRef(projectDir, media.GeneratedAssets(item)["storyboard_image"])
}

// findStoryboardItemByAlias 按规范 id 或 scene_id 别名命中条目。
func findStoryboardItemByAlias(items []any, idField, sceneID string) (map[string]any, int, bool) {
	for idx, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fmt.Sprint(item[idField]) == sceneID {
			return item, idx, true
		}
		if alias, ok := item["scene_id"].(string); ok && alias == sceneID {
			return item, idx, true
		}
	}
	return nil, 0, false
}

// pendingVideoItems 返回尚无 video_clip 的条目（保序）。
func pendingVideoItems(items []any) []any {
	var out []any
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if clip, _ := media.GeneratedAssets(item)["video_clip"].(string); clip != "" {
			continue
		}
		out = append(out, item)
	}
	return out
}

// promptPresent 复刻 Python 对 prompt 的真值判定（空串 / 空对象 / nil 均视为缺失）。
func promptPresent(prompt any) bool {
	switch v := prompt.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case map[string]any:
		return len(v) > 0
	}
	return true
}

func stringOf(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

func indexOf(list []string, value string) int {
	for i, item := range list {
		if item == value {
			return i
		}
	}
	return -1
}

func nonEmptyPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func countNonEmpty(paths []string) int { return len(nonEmptyPaths(paths)) }

func toAnyMaps(maps []map[string]any) []any {
	out := make([]any, 0, len(maps))
	for _, m := range maps {
		out = append(out, m)
	}
	return out
}

func toAnyStrings(values []string) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}
