package script

import "github.com/shotwise/shotwise/internal/agent/tools"

import (
	serviceref "github.com/shotwise/shotwise/internal/service/reference"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/service"
	servicescript "github.com/shotwise/shotwise/internal/service/script"
)

// 剧本生成类文本工具（对应 server/agent_runtime/sdk_tools/text_generation.py）。
//
// 这些工具把源文经项目配置的文本模型拆为结构化 step1 中间态，落 drafts/episode_N/ 供
// generate_episode_script 透传消费。

// narrationSplitIn 是 split_narration_segments 的入参。
type narrationSplitIn struct {
	Episode      int    `json:"episode" jsonschema:"description=剧集编号"`
	Source       string `json:"source,omitempty" jsonschema:"description=指定小说源文件路径（相对项目目录）；默认读取 source/ 下所有文本"`
	Instructions string `json:"instructions,omitempty" jsonschema:"description=用户对本次生成的意见原文（可选）；原样注入 prompt 末尾的「用户意见」分节，遵循强度由正文表达，缺省/空白视同未传，最长 4000 字符"`
	DryRun       bool   `json:"dry_run,omitempty" jsonschema:"description=仅显示 prompt，不调用模型"`
}

// NewSplitNarrationSegmentsTool 构造 split_narration_segments。
func NewSplitNarrationSegmentsTool(
	project string, gen servicescript.TextCompletionFactory, projects tools.TextGenerationProjects, caps tools.VideoCapabilityReader,
) (tool.InvokableTool, error) {
	return toolutils.InferTool("split_narration_segments",
		"把本集小说原文按朗读节奏拆分为说书片段表（逐字 novel_text + 时长 + segment_break + 出场资产），"+
			"保存到 drafts/episode_N/step1_segments.json，供 generate_episode_script（narration 模式）消费。"+
			"novel_text 逐字保留原文、由 step2 透传，不经 step2 的 LLM 重出。dry_run=true 时仅返回 prompt。",
		func(ctx context.Context, in narrationSplitIn) (string, error) {
			instructions, bad := tools.NormalizeInstructions(in.Instructions)
			if bad != "" {
				return tools.DenyEnvelope(bad), nil
			}
			if gen == nil || projects == nil {
				return tools.DenyEnvelope("text generation not configured"), nil
			}
			pf, err := projects.Get(project)
			if err != nil {
				return tools.DenyEnvelope("❌ 加载项目失败：" + err.Error()), nil
			}
			dir, err := projects.Dir(project)
			if err != nil {
				return tools.DenyEnvelope("❌ 加载项目失败：" + err.Error()), nil
			}

			source, err := service.ResolveEpisodeSource(dir, in.Episode, in.Source, pf)
			if err != nil {
				return tools.DenyEnvelope("❌ " + err.Error()), nil
			}

			defaultDuration, supportedDurations, err := tools.FetchVideoDurations(ctx, caps, pf)
			if err != nil {
				return tools.DenyEnvelope("❌ 无法解析视频模型能力：" + err.Error()), nil
			}

			prompt, err := servicescript.BuildNarrationSplitPrompt(servicescript.NarrationSplitPromptOptions{
				NovelText:          source.Text,
				ProjectOverview:    pf.Overview,
				Characters:         &pf.Characters,
				Scenes:             &pf.Scenes,
				Props:              &pf.Props,
				DefaultDuration:    defaultDuration,
				SupportedDurations: supportedDurations,
				Episode:            in.Episode,
				TargetLanguage:     tools.TargetLanguage(pf),
			})
			if err != nil {
				return tools.DenyEnvelope("❌ 构建 prompt 失败：" + err.Error()), nil
			}
			prompt = servicescript.AppendUserInstructions(prompt, instructions)

			if in.DryRun {
				return tools.OKEnvelope(fmt.Sprintf("DRY RUN — 以下是将发送给文本模型的 Prompt:\n\n%s\n\nPrompt 长度: %d 字符",
					prompt, len([]rune(prompt)))), nil
			}

			generator, err := gen(ctx, pf)
			if err != nil {
				return tools.DenyEnvelope("❌ 文本生成器未就绪：" + err.Error()), nil
			}
			result, err := generator.Generate(ctx, prompt, servicescript.NarrationStep1DraftSchema(), tools.TextMaxOutputTokens)
			if err != nil {
				return tools.DenyEnvelope("❌ 文本生成失败：" + err.Error()), nil
			}
			segments, err := servicescript.ParseNarrationStep1Draft(result)
			if err != nil {
				return tools.DenyEnvelope("❌ " + err.Error()), nil
			}
			if err := servicescript.ValidateNarrationSegments(segments, supportedDurations); err != nil {
				return tools.DenyEnvelope("❌ " + err.Error()), nil
			}
			if err := servicescript.ValidateNarrationAssetReferences(segments,
				assetNamesOf(&pf.Characters), assetNamesOf(&pf.Scenes), assetNamesOf(&pf.Props)); err != nil {
				return tools.DenyEnvelope("❌ " + err.Error()), nil
			}
			if err := servicescript.ValidateNarrationNovelTextCoverage(segments, source.Text); err != nil {
				return tools.DenyEnvelope("❌ " + err.Error()), nil
			}

			step1Name, _ := servicescript.Step1Filename("narration")
			step1Path := filepath.Join(service.EpisodeDraftsDir(dir, in.Episode), step1Name)
			if err := os.MkdirAll(filepath.Dir(step1Path), 0o755); err != nil {
				return tools.DenyEnvelope("❌ 创建草稿目录失败：" + err.Error()), nil
			}
			if err := fs.WriteJSONAtomic(step1Path, map[string]any{"segments": segments}); err != nil {
				return tools.DenyEnvelope("❌ 写入 step1 失败：" + err.Error()), nil
			}

			totalChars := 0
			totalSeconds := 0
			breakCount := 0
			for _, segment := range segments {
				totalChars += len([]rune(fmt.Sprint(segment["novel_text"])))
				totalSeconds += step1Int(segment["duration_seconds"])
				if b, ok := segment["segment_break"].(bool); ok && b {
					breakCount++
				}
			}
			return tools.OKEnvelope(fmt.Sprintf("✅ 说书片段拆分（结构化 step1）已保存: %s\n"+
				"📊 生成统计: %d 个片段 / %d 字，预计总时长 %d 秒；segment_break 标记 %d 个",
				step1Path, len(segments), totalChars, totalSeconds, breakCount)), nil
		})
}

// step1Int 取整数语义的值（JSON 数字的整数值 / 纯数字字符串），否则 0。
func step1Int(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		if v == float64(int(v)) {
			return int(v)
		}
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err == nil {
			return n
		}
	}
	return 0
}

// normalizeDramaScriptIn 是 normalize_drama_script 的入参。
type normalizeDramaScriptIn struct {
	Episode      int    `json:"episode" jsonschema:"description=剧集编号"`
	Source       string `json:"source,omitempty" jsonschema:"description=指定小说源文件路径（相对项目目录）；默认读取 source/ 下所有文本"`
	Instructions string `json:"instructions,omitempty" jsonschema:"description=用户对本次生成的意见原文（可选）；原样注入 prompt 末尾的「用户意见」分节，遵循强度由正文表达，缺省/空白视同未传，最长 4000 字符"`
	DryRun       bool   `json:"dry_run,omitempty" jsonschema:"description=仅显示 prompt，不调用模型"`
}

// NewNormalizeDramaScriptTool 构造 normalize_drama_script。
func NewNormalizeDramaScriptTool(
	project string, gen servicescript.TextCompletionFactory, projects tools.TextGenerationProjects, caps tools.VideoCapabilityReader,
) (tool.InvokableTool, error) {
	return toolutils.InferTool("normalize_drama_script",
		"把 source/ 小说原文（或指定 source 文件）抽取为结构化分镜内容（场景边界、出场资产、"+
			"逐字口播 utterances、原文锚 source_text、视觉改编描述），保存到 "+
			"drafts/episode_N/step1_normalized_script.json，供 generate_episode_script 透传消费。"+
			"dry_run=true 时仅返回 prompt。",
		func(ctx context.Context, in normalizeDramaScriptIn) (string, error) {
			instructions, bad := tools.NormalizeInstructions(in.Instructions)
			if bad != "" {
				return tools.DenyEnvelope(bad), nil
			}
			if gen == nil || projects == nil {
				return tools.DenyEnvelope("text generation not configured"), nil
			}
			pf, err := projects.Get(project)
			if err != nil {
				return tools.DenyEnvelope("❌ 加载项目失败：" + err.Error()), nil
			}
			dir, err := projects.Dir(project)
			if err != nil {
				return tools.DenyEnvelope("❌ 加载项目失败：" + err.Error()), nil
			}
			source, err := service.ResolveEpisodeSource(dir, in.Episode, in.Source, pf)
			if err != nil {
				return tools.DenyEnvelope("❌ " + err.Error()), nil
			}
			defaultDuration, supportedDurations, err := tools.FetchVideoDurations(ctx, caps, pf)
			if err != nil {
				return tools.DenyEnvelope("❌ 无法解析视频模型能力：" + err.Error()), nil
			}
			defaultDuration = memberOrNil(defaultDuration, supportedDurations)

			episodeOutline, nextEpisodeOutline := servicescript.EpisodeOutlineContext(pf, in.Episode)
			sourceKind := pf.SourceKind
			if sourceKind == "" {
				sourceKind = "novel"
			}
			prompt, err := servicescript.BuildNormalizePrompt(servicescript.NormalizePromptOptions{
				NovelText:          source.Text,
				ProjectOverview:    pf.Overview,
				Style:              pf.Style,
				Characters:         &pf.Characters,
				Scenes:             &pf.Scenes,
				Props:              &pf.Props,
				DefaultDuration:    defaultDuration,
				SupportedDurations: supportedDurations,
				Episode:            in.Episode,
				SourceKind:         sourceKind,
				TargetLanguage:     tools.TargetLanguage(pf),
				SourceLanguage:     pf.SourceLanguage,
				SpeechRateOverride: service.ProjectSpeechRateOverride(pf),
				EpisodeOutline:     episodeOutline,
				NextEpisodeOutline: nextEpisodeOutline,
			})
			if err != nil {
				return tools.DenyEnvelope("❌ 构建 prompt 失败：" + err.Error()), nil
			}
			prompt = servicescript.AppendUserInstructions(prompt, instructions)

			if in.DryRun {
				return tools.OKEnvelope(fmt.Sprintf("DRY RUN — 以下是将发送给文本模型的 Prompt:\n\n%s\n\nPrompt 长度: %d 字符",
					prompt, len([]rune(prompt)))), nil
			}

			schema, err := servicescript.DramaNormalizedScriptSchema(supportedDurations)
			if err != nil {
				return tools.DenyEnvelope("❌ 构造 schema 失败：" + err.Error()), nil
			}
			generator, err := gen(ctx, pf)
			if err != nil {
				return tools.DenyEnvelope("❌ 文本生成器未就绪：" + err.Error()), nil
			}
			result, err := generator.Generate(ctx, prompt, schema, tools.TextMaxOutputTokens)
			if err != nil {
				return tools.DenyEnvelope("❌ 文本生成失败：" + err.Error()), nil
			}
			content, err := servicescript.ParseDramaNormalizedScript(result, supportedDurations)
			if err != nil {
				return tools.DenyEnvelope("❌ " + err.Error()), nil
			}

			step1Name, _ := servicescript.Step1Filename("drama")
			step1Path := filepath.Join(service.EpisodeDraftsDir(dir, in.Episode), step1Name)
			if err := os.MkdirAll(filepath.Dir(step1Path), 0o755); err != nil {
				return tools.DenyEnvelope("❌ 创建草稿目录失败：" + err.Error()), nil
			}
			if err := fs.WriteJSONAtomic(step1Path, content); err != nil {
				return tools.DenyEnvelope("❌ 写入 step1 失败：" + err.Error()), nil
			}
			scenes, _ := content["scenes"].([]any)
			return tools.OKEnvelope(fmt.Sprintf("✅ 规范化剧本（结构化内容）已保存: %s\n📊 生成统计: %d 个场景",
				step1Path, len(scenes))), nil
		})
}

// memberOrNil 返回该值（若在候选内），否则 nil。
func memberOrNil(value *int, candidates []int) *int {
	if value == nil {
		return nil
	}
	for _, c := range candidates {
		if c == *value {
			return value
		}
	}
	return nil
}

// ScriptReviewProjects 是 step1 审核确认工具的项目依赖（*service.ProjectStore 满足）。
type ScriptReviewProjects interface {
	Get(name string) (*domain.ProjectFile, error)
	Dir(name string) (string, error)
	Update(name string, mutate func(*domain.ProjectFile) error) error
}

// confirmScriptReviewIn 是 confirm_script_review 的入参。
type confirmScriptReviewIn struct {
	Episode int `json:"episode" jsonschema:"description=剧集编号"`
}

// NewConfirmScriptReviewTool 构造 confirm_script_review。
func NewConfirmScriptReviewTool(project string, projects ScriptReviewProjects) (tool.InvokableTool, error) {
	return toolutils.InferTool("confirm_script_review",
		"确认本集 step1 结构化中间态（drama / narration 的逐字口播 / 原文，reference_video 的 "+
			"video_unit 拆分），放行 step2 视觉生成。仅在用户对话中明确同意进入视觉生成、或已在 Web 端审阅"+
			"认可后调用——这是 step2 的显式确认动作，与 Web 端确认等价；未确认时 generate_episode_script "+
			"会被审核 gate 阻塞。",
		func(ctx context.Context, in confirmScriptReviewIn) (string, error) {
			_ = ctx
			if projects == nil {
				return tools.DenyEnvelope("script review not configured"), nil
			}
			pf, err := projects.Get(project)
			if err != nil {
				return tools.DenyEnvelope("❌ 加载项目失败：" + err.Error()), nil
			}
			dir, err := projects.Dir(project)
			if err != nil {
				return tools.DenyEnvelope("❌ 加载项目失败：" + err.Error()), nil
			}
			kind := servicescript.Step1Kind(pf)
			if kind == "" {
				return tools.DenyEnvelope("❌ 无法确认 step1 审核（not_applicable）：本项目没有结构化 step1 中间态"), nil
			}
			if servicescript.Step1Quarantined(dir, pf, in.Episode) {
				path, _ := servicescript.Step1QuarantinePath(dir, pf, in.Episode)
				return tools.DenyEnvelope(fmt.Sprintf(
					"❌ 无法确认 step1 审核（quarantined）：step1 违约产物待处置: %s", path)), nil
			}
			path, _ := servicescript.Step1Path(dir, pf, in.Episode)
			content, readErr := tools.ReadJSONObject(path)
			if readErr != nil {
				return tools.DenyEnvelope("❌ 无法确认 step1 审核（invalid_content）：" + readErr.Error()), nil
			}
			if content == nil {
				if _, statErr := os.Stat(path); statErr != nil {
					return tools.DenyEnvelope("❌ 无法确认 step1 审核（no_step1）：该集尚未产出结构化 step1"), nil
				}
				return tools.DenyEnvelope("❌ 无法确认 step1 审核（invalid_content）：step1 不是合法的 JSON 对象"), nil
			}
			if err := validateStep1Content(kind, content); err != nil {
				return tools.DenyEnvelope("❌ 无法确认 step1 审核（invalid_content）：" + err.Error()), nil
			}
			fingerprint := service.ContentFingerprintOfData(content)
			confirmedAt := time.Now().UTC().Format(time.RFC3339)
			if err := projects.Update(project, func(p *domain.ProjectFile) error {
				if !service.ApplyConfirmation(p, in.Episode, fingerprint, confirmedAt) {
					return errEpisodeNotFound
				}
				return nil
			}); err != nil {
				if err == errEpisodeNotFound {
					return tools.DenyEnvelope("❌ 无法确认 step1 审核（episode_not_found）：该集未在 project.json 登记"), nil
				}
				return tools.DenyEnvelope("❌ 无法确认 step1 审核（" + err.Error() + "）"), nil
			}
			updated, err := projects.Get(project)
			status := servicescript.ReviewStatusConfirmed
			if err == nil {
				status = servicescript.ReviewStatus(dir, updated, in.Episode)
			}
			return tools.OKEnvelope(fmt.Sprintf("✅ 第 %d 集 step1 已确认，step2 视觉生成已放行（status=%s）", in.Episode, status)), nil
		})
}

// errEpisodeNotFound 是确认时集条目缺失的哨兵错误。
var errEpisodeNotFound = errors.New("episode_not_found")

// validateStep1Content 按 step1 变体做结构校验（内容层必需容器为非空数组）。
func validateStep1Content(kind string, content map[string]any) error {
	field := ""
	switch kind {
	case "drama":
		field = "scenes"
	case "narration":
		field = "segments"
	case "reference_video":
		field = "units"
	default:
		return fmt.Errorf("未知 step1 变体 %q", kind)
	}
	items, ok := content[field].([]any)
	if !ok || len(items) == 0 {
		return fmt.Errorf("step1 的 %s 必须是非空数组", field)
	}
	return nil
}



// assetNamesOf 返回资产桶的登记名（排序）。
func assetNamesOf(bucket *domain.AssetBucket) []string {
	if bucket == nil || len(*bucket) == 0 {
		return nil
	}
	names := make([]string, 0, len(*bucket))
	for name := range *bucket {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}


// generateEpisodeScriptIn 是 generate_episode_script 的入参。
type generateEpisodeScriptIn struct {
	Episode      int    `json:"episode" jsonschema:"description=剧集编号"`
	Instructions string `json:"instructions,omitempty" jsonschema:"description=用户对本次生成的意见原文（可选）；原样注入 prompt 末尾的「用户意见」分节，遵循强度由正文表达，缺省/空白视同未传，最长 4000 字符"`
	DryRun       bool   `json:"dry_run,omitempty" jsonschema:"description=仅显示 prompt，不调用模型"`
}

// NewGenerateEpisodeScriptTool 构造 generate_episode_script。
func NewGenerateEpisodeScriptTool(
	project string, gen servicescript.TextCompletionFactory, projects tools.TextGenerationProjects, caps tools.VideoCapabilityReader,
) (tool.InvokableTool, error) {
	return toolutils.InferTool("generate_episode_script",
		"调用项目配置的文本模型生成 JSON 剧本。输出固定写入 {project}/scripts/episode_N.json，"+
			"dry_run=true 时仅返回 prompt 不调用 API。",
		func(ctx context.Context, in generateEpisodeScriptIn) (string, error) {
			instructions, bad := tools.NormalizeInstructions(in.Instructions)
			if bad != "" {
				return tools.DenyEnvelope(bad), nil
			}
			if projects == nil || gen == nil {
				return tools.DenyEnvelope("text generation not configured"), nil
			}
			pf, err := projects.Get(project)
			if err != nil {
				return tools.DenyEnvelope("❌ 加载项目失败：" + err.Error()), nil
			}
			dir, err := projects.Dir(project)
			if err != nil {
				return tools.DenyEnvelope("❌ 加载项目失败：" + err.Error()), nil
			}

			// 隔离草稿在场先于「缺 step1」与审核 gate 报出：三者都判「未放行」，但出路各不相同。
			if tools.UsesReferenceVideoUnits(pf) {
				for _, kind := range []string{serviceref.QuarantineKindStep1, serviceref.QuarantineKindStep2} {
					if serviceref.QuarantineExists(dir, in.Episode, kind) {
						path := serviceref.QuarantinePath(dir, in.Episode, kind)
						return tools.DenyEnvelope(fmt.Sprintf(
							"⏸️ 本集有违约产物待处置（%s），step2 视觉生成已中止。"+
								"请按草稿内 violations 的定位修正 content，再调用 %s 晋升。", path, serviceref.PromoteToolName)), nil
					}
				}
			}

			if step1Path, hint, check := resolveGenerateStep1(dir, pf, in.Episode); check {
				if _, statErr := os.Stat(step1Path); statErr != nil {
					return tools.DenyEnvelope(fmt.Sprintf("❌ 未找到 Step 1 文件: %s\n   请先完成 %s", step1Path, hint)), nil
				}
			}

			_, supportedDurations, err := tools.FetchVideoDurations(ctx, caps, pf)
			if err != nil {
				return tools.DenyEnvelope("❌ 无法解析视频模型能力：" + err.Error()), nil
			}

			// 参考路径额外解析拆分档位与逐 unit 取档 context；非参考路径无需。
			var genOpts []servicescript.ScriptGenOption
			if tools.UsesReferenceVideoUnits(pf) {
				rc, rcErr := tools.ReferenceContextForTool(ctx, pf, caps)
				if rcErr != nil {
					return tools.DenyEnvelope("❌ " + rcErr.Error()), nil
				}
				genOpts = append(genOpts, servicescript.WithReferenceContext(rc))
			}

			// dry-run 与真实生成先各自构造一次生成器：dry-run 不调用模型（gen 传 nil）。
			builder, err := servicescript.NewScriptGenerator(projects, project, nil, supportedDurations, genOpts...)
			if err != nil {
				return tools.DenyEnvelope("❌ 初始化剧本生成器失败：" + err.Error()), nil
			}
			prompt, err := builder.BuildPrompt(in.Episode, instructions)
			if err != nil {
				return tools.DenyEnvelope("❌ 剧本生成失败：" + err.Error()), nil
			}
			if in.DryRun {
				return tools.OKEnvelope(fmt.Sprintf("DRY RUN — 以下是将发送给文本模型的 Prompt:\n\n%s", prompt)), nil
			}

			// step1→step2 审核 gate：结构化 step1 中间态须经 web 显式确认才放行 step2 视觉生成。
			if servicescript.GateBlocksStep2(dir, pf, in.Episode) {
				return tools.DenyEnvelope("⏸️ step1 结构化中间态尚未经 web 审核确认，step2 视觉生成被 gate 阻塞。" +
					"请在 Web 端审阅并确认本集 step1 内容后再生成剧本。"), nil
			}

			generator, err := gen(ctx, pf)
			if err != nil {
				return tools.DenyEnvelope("❌ 文本生成器未就绪：" + err.Error()), nil
			}
			scriptGen, err := servicescript.NewScriptGenerator(projects, project, generator, supportedDurations, genOpts...)
			if err != nil {
				return tools.DenyEnvelope("❌ 初始化剧本生成器失败：" + err.Error()), nil
			}
			resultPath, err := scriptGen.Generate(ctx, in.Episode, instructions)
			if err != nil {
				return tools.DenyEnvelope("❌ 剧本生成失败：" + err.Error()), nil
			}
			return tools.OKEnvelope("✅ 剧本生成完成: " + resultPath), nil
		})
}


// resolveGenerateStep1 解析该集 step1 路径与缺失提示；ad 一键生成不依赖 step1 时 check=false。
func resolveGenerateStep1(dir string, pf *domain.ProjectFile, episode int) (path, hint string, check bool) {
	if pf == nil || pf.ContentMode == "ad" {
		return "", "", false
	}
	drafts := service.EpisodeDraftsDir(dir, episode)
	if pf.IsReferenceVideo() {
		rvJSON := filepath.Join(drafts, service.ReferenceVideoStep1Filename)
		if _, err := os.Stat(rvJSON); err != nil {
			legacy := filepath.Join(drafts, service.ReferenceVideoStep1LegacyFilename)
			if _, err2 := os.Stat(legacy); err2 == nil {
				return rvJSON, fmt.Sprintf("重跑 split_reference_video_units 把旧 %s 重新拆分为结构化 %s",
					service.ReferenceVideoStep1LegacyFilename, service.ReferenceVideoStep1Filename), true
			}
		}
		return rvJSON, "split_reference_video_units (Step 1)", true
	}
	if pf.ContentMode != "narration" {
		if name, ok := servicescript.Step1Filename(pf.ContentMode); ok {
			return filepath.Join(drafts, name), "normalize_drama_script tool", true
		}
	}
	candidates := servicescript.Step1ReadCandidates("narration")
	if len(candidates) == 0 {
		return "", "", false
	}
	jsonPath := filepath.Join(drafts, candidates[0])
	if _, err := os.Stat(jsonPath); err != nil && len(candidates) > 1 {
		legacy := filepath.Join(drafts, candidates[1])
		if _, err2 := os.Stat(legacy); err2 == nil {
			return jsonPath, fmt.Sprintf("重跑 split_narration_segments 把旧 %s 重新拆分为结构化 %s",
				candidates[1], candidates[0]), true
		}
	}
	return jsonPath, "split_narration_segments (Step 1)", true
}
