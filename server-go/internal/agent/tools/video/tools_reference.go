// 参考生视频工具链（对应 server/agent_runtime/sdk_tools/text_generation.py 的
// split_reference_video_units / open_reference_step1_for_edit / validate_and_promote_reference_draft）。
//
// 三个工具把源文拆分为 video_unit 表、把已落盘 step1 取回可编辑草稿、把违约草稿按产出时同一份
// 校验器全量重判后晋升。违约一律走「隔离草稿 + 重判晋升」，不丢弃重抽。
package video

import "github.com/shotwise/shotwise/internal/agent/tools"

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
)

// referenceSplitIn 是 split_reference_video_units 的入参。
type referenceSplitIn struct {
	Episode      int    `json:"episode" jsonschema:"description=剧集编号"`
	Source       string `json:"source,omitempty" jsonschema:"description=指定小说源文件路径（相对项目目录）；默认读取 source/ 下所有文本"`
	Instructions string `json:"instructions,omitempty" jsonschema:"description=用户对本次生成的意见原文（可选）；原样注入 prompt 末尾的「用户意见」分节，缺省/空白视同未传，最长 4000 字符"`
	DryRun       bool   `json:"dry_run,omitempty" jsonschema:"description=仅显示 prompt，不调用模型"`
}

// newSplitReferenceVideoUnitsTool 构造 split_reference_video_units。
func newSplitReferenceVideoUnitsTool(
	project string, gen service.TextCompletionFactory, projects tools.TextGenerationProjects, caps tools.VideoCapabilityReader,
) (tool.InvokableTool, error) {
	return toolutils.InferTool("split_reference_video_units",
		"把本集小说原文拆分为参考生视频 video_unit 表（unit → 时长 + 原文锚 + 书写层正文），"+
			"保存到 drafts/episode_N/step1_reference_units.json，供 generate_episode_script（reference_video 模式）消费。"+
			"unit_id / shots / references 由工具从正文机械派生，并校验原文锚、正文语法、资产引用与台词量。dry_run=true 时仅返回 prompt。",
		func(ctx context.Context, in referenceSplitIn) (string, error) {
			instructions, bad := tools.NormalizeInstructions(in.Instructions)
			if bad != "" {
				return tools.DenyEnvelope(bad), nil
			}
			if gen == nil || projects == nil {
				return tools.DenyEnvelope("text generation not configured"), nil
			}
			resolver, ok := tools.ReferenceResolver(caps)
			if !ok {
				return tools.DenyEnvelope("❌ 无法解析视频模型能力：参考拆分需要完整视频能力解析器"), nil
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
			splitCaps, err := service.FetchReferenceSplitCaps(ctx, resolver, pf)
			if err != nil {
				return tools.DenyEnvelope("❌ " + err.Error()), nil
			}
			episodeOutline, nextEpisodeOutline := service.EpisodeOutlineContext(pf, in.Episode)
			prompt, err := service.BuildReferenceUnitsSplitPrompt(service.ReferenceUnitsSplitPromptOptions{
				NovelText:          source.Text,
				ProjectOverview:    pf.Overview,
				Characters:         &pf.Characters,
				Scenes:             &pf.Scenes,
				Props:              &pf.Props,
				SupportedDurations: splitCaps.Durations,
				ReferenceDurations: splitCaps.ReferenceDurations,
				TextDurations:      splitCaps.TextDurations,
				MaxDuration:        splitCaps.MaxDuration,
				MaxReferenceImages: splitCaps.MaxRefs,
				DefaultDuration:    splitCaps.DefaultDuration,
				Episode:            in.Episode,
				TargetLanguage:     tools.TargetLanguage(pf),
				SourceLanguage:     pf.SourceLanguage,
				SpeechRateOverride: service.ProjectSpeechRateOverride(pf),
				EpisodeOutline:     episodeOutline,
				NextEpisodeOutline: nextEpisodeOutline,
			})
			if err != nil {
				return tools.DenyEnvelope("❌ 构建 prompt 失败：" + err.Error()), nil
			}
			prompt = service.AppendUserInstructions(prompt, instructions)

			if in.DryRun {
				return tools.OKEnvelope(fmt.Sprintf("DRY RUN — 以下是将发送给文本模型的 Prompt:\n\n%s\n\nPrompt 长度: %d 字符",
					prompt, len([]rune(prompt)))), nil
			}

			schema, err := service.BuildReferenceUnitsStep1Schema(splitCaps.Durations)
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
			flatUnits, err := service.ParseReferenceStep1Flat(result, splitCaps.Durations)
			if err != nil {
				return tools.DenyEnvelope("❌ " + err.Error()), nil
			}
			violations := service.CollectReferenceFlatViolations(
				flatUnits, pf, in.Episode, source.Text, splitCaps, pf.SourceLanguage)
			if len(violations) > 0 {
				var report string
				lockErr := service.Step1WriteLock(dir, in.Episode, func(step1Path string) error {
					fingerprint, ferr := service.ContentFingerprint(step1Path)
					if ferr != nil {
						return ferr
					}
					meta := map[string]any{"source": nil, "base_fingerprint": nil}
					if in.Source != "" {
						meta["source"] = in.Source
					}
					if fingerprint != nil {
						meta["base_fingerprint"] = *fingerprint
					}
					rendered, qerr := service.QuarantineAndReport(
						dir, in.Episode, service.QuarantineKindStep1,
						map[string]any{"units": toAnyMaps(flatUnits)}, violations, meta, nil)
					report = rendered
					return qerr
				})
				if lockErr != nil {
					return tools.DenyEnvelope("❌ 隔离草稿写入失败：" + lockErr.Error()), nil
				}
				return tools.DenyEnvelope(report), nil
			}

			units, err := service.BuildReferenceUnitsFromFlat(flatUnits, pf, in.Episode, splitCaps.MaxRefs)
			if err != nil {
				return tools.DenyEnvelope("❌ " + err.Error()), nil
			}
			var step1Path string
			lockErr := service.Step1WriteLock(dir, in.Episode, func(path string) error {
				step1Path = path
				if _, werr := service.WriteStep1Locked(dir, in.Episode,
					map[string]any{"units": toAnyMaps(units)}, service.UncheckedFingerprintExpectation(), true); werr != nil {
					return werr
				}
				return service.ClearQuarantine(dir, in.Episode, service.QuarantineKindStep1)
			})
			if lockErr != nil {
				return tools.DenyEnvelope("❌ 写入 step1 失败：" + lockErr.Error()), nil
			}
			texts := make([]string, 0, len(flatUnits))
			for _, flat := range flatUnits {
				texts = append(texts, fmt.Sprint(flat["text"]))
			}
			warnings := service.ReferenceVoiceWarningLines(texts, pf, splitCaps.Voice)
			return tools.OKEnvelope(service.ReferenceResultText(step1Path, units, warnings, "拆分")), nil
		})
}

// referenceEditIn 是 open_reference_step1_for_edit 的入参。
type referenceEditIn struct {
	Episode int    `json:"episode" jsonschema:"description=剧集编号"`
	Source  string `json:"source,omitempty" jsonschema:"description=本集小说源文件路径（相对项目目录，如 source/episode_1.txt）；晋升时按它重判原文锚，不传则按整个 source/ 目录重解析（判定更松）"`
}

// newOpenReferenceStep1ForEditTool 构造 open_reference_step1_for_edit。
func newOpenReferenceStep1ForEditTool(project string, projects tools.TextGenerationProjects) (tool.InvokableTool, error) {
	return toolutils.InferTool(service.Step1EditToolName,
		"把本集已落盘的参考生视频 step1 拆分取回可编辑的隔离草稿（扁平书写层：时长 + 原文锚 + 正文），"+
			"用于修改已有拆分。改完调用 "+service.PromoteToolName+" 全量校验并晋升回正式文件。"+
			"正式 step1 不可用 Write/Edit 直改——它与 Web 端保存、迁移、重拆分共享一把文件锁，只能经工具写盘。",
		func(ctx context.Context, in referenceEditIn) (string, error) {
			_ = ctx
			if projects == nil {
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
			if !tools.UsesReferenceVideoUnits(pf) {
				return tools.DenyEnvelope(fmt.Sprintf("❌ 第 %d 集当前不走参考生视频路径，无 step1 拆分可编辑", in.Episode)), nil
			}
			if in.Source != "" {
				if _, err := service.ResolveEpisodeSource(dir, in.Episode, in.Source, pf); err != nil {
					return tools.DenyEnvelope("❌ " + err.Error()), nil
				}
			}
			var draftPath, step1Path, denyMsg string
			lockErr := service.Step1WriteLock(dir, in.Episode, func(path string) error {
				step1Path = path
				if service.QuarantineExists(dir, in.Episode, service.QuarantineKindStep1) {
					denyMsg = fmt.Sprintf("❌ 第 %d 集已有 step1 隔离草稿在场：%s\n"+
						"不覆盖它（可能已含未晋升的修改）；请直接编辑该草稿的 content.units[i]，"+
						"改完调用 %s({\"episode\": %d}) 晋升。",
						in.Episode, service.QuarantinePath(dir, in.Episode, service.QuarantineKindStep1),
						service.PromoteToolName, in.Episode)
					return nil
				}
				data, readErr := tools.ReadJSONObject(step1Path)
				if readErr != nil || data == nil {
					denyMsg = fmt.Sprintf("❌ 第 %d 集没有可编辑的正式 step1（%s 不存在或不是合法 JSON）；"+
						"首次生成请调用 split_reference_video_units", in.Episode, step1Path)
					return nil
				}
				rawUnits, ok := data["units"].([]any)
				if !ok || len(rawUnits) == 0 {
					denyMsg = fmt.Sprintf("❌ %s 的 units 不是非空数组，无法取回编辑", step1Path)
					return nil
				}
				meta := map[string]any{"source": nil, "base_fingerprint": service.ContentFingerprintOfData(data)}
				if in.Source != "" {
					meta["source"] = in.Source
				}
				written, werr := service.WriteQuarantine(dir, in.Episode, service.QuarantineKindStep1,
					map[string]any{"units": toAnyMaps(service.FlattenReferenceStep1Units(rawUnits))}, nil, meta, nil)
				if werr != nil {
					return werr
				}
				draftPath = written
				return nil
			})
			if lockErr != nil {
				return tools.DenyEnvelope("❌ 写入隔离草稿失败：" + lockErr.Error()), nil
			}
			if denyMsg != "" {
				return tools.DenyEnvelope(denyMsg), nil
			}
			unitCount := 0
			if data, readErr := tools.ReadJSONObject(step1Path); readErr == nil && data != nil {
				if rawUnits, ok := data["units"].([]any); ok {
					unitCount = len(rawUnits)
				}
			}
			return tools.OKEnvelope(fmt.Sprintf("✅ 第 %d 集 step1 已取回可编辑草稿：%s\n"+
				"📊 %d 个 unit（正式文件 %s 保持原样，未改动）\n\n"+
				"编辑口径：改 content.units[i] 的 text / source_text / duration_seconds；"+
				"unit_id / shots / references 是派生物，不在草稿里、也不要手写。增删 unit 即增删数组元素，unit_id 按新顺序重编。\n"+
				"改完调用 %s({\"episode\": %d}) 全量校验并晋升回正式文件；违约时返回逐条报告，继续改再晋升，无轮次上限。\n"+
				"草稿在场期间审阅门与 step2 生成被阻塞；放弃修改就原样晋升（内容未变即等于回写原稿）。",
				in.Episode, draftPath, unitCount, step1Path, service.PromoteToolName, in.Episode)), nil
		})
}

// referencePromoteIn 是 validate_and_promote_reference_draft 的入参。
type referencePromoteIn struct {
	Episode int `json:"episode" jsonschema:"description=剧集编号"`
}

// newValidateAndPromoteReferenceDraftTool 构造 validate_and_promote_reference_draft。
func newValidateAndPromoteReferenceDraftTool(
	project string, gen service.TextCompletionFactory, projects tools.TextGenerationProjects, caps tools.VideoCapabilityReader,
) (tool.InvokableTool, error) {
	return toolutils.InferTool(service.PromoteToolName,
		"重新全量校验本集的参考生视频隔离草稿（step1 拆分或 step2 视觉展开的违约产物），"+
			"通过则晋升为正式文件并清除草稿，不通过则返回刷新后的违约报告。"+
			"在按上一份报告修改过隔离草稿的 content 之后调用；可反复调用，无轮次上限。",
		func(ctx context.Context, in referencePromoteIn) (string, error) {
			if projects == nil {
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
			if !tools.UsesReferenceVideoUnits(pf) {
				return tools.DenyEnvelope(fmt.Sprintf("❌ 第 %d 集当前不走参考生视频路径，盘上的参考路径隔离草稿已与该集无关，不作晋升",
					in.Episode)), nil
			}

			step1Draft, err := service.ReadQuarantine(dir, in.Episode, service.QuarantineKindStep1)
			if err != nil {
				if version, ok := unsupportedVersion(err); ok {
					return tools.DenyEnvelope(fmt.Sprintf("❌ 第 %d 集 step1 隔离草稿使用了不支持的 schema_version=%d；请先升级应用后再继续",
						in.Episode, version)), nil
				}
				return tools.DenyEnvelope("❌ " + err.Error()), nil
			}
			if step1Draft != nil {
				message, isErr := promoteReferenceStep1(ctx, dir, pf, in.Episode, step1Draft, caps)
				if isErr {
					return tools.DenyEnvelope(message), nil
				}
				return tools.OKEnvelope(message), nil
			}
			if service.QuarantineExists(dir, in.Episode, service.QuarantineKindStep1) {
				return tools.DenyEnvelope(fmt.Sprintf("❌ step1 隔离草稿 %s 不是合法的 JSON 信封（顶层须为对象且含 content 对象）；"+
					"请修正该文件的 JSON 结构后重试", service.QuarantinePath(dir, in.Episode, service.QuarantineKindStep1))), nil
			}

			if service.QuarantineExists(dir, in.Episode, service.QuarantineKindStep2) {
				if _, err := service.ReadQuarantine(dir, in.Episode, service.QuarantineKindStep2); err != nil {
					if version, ok := unsupportedVersion(err); ok {
						return tools.DenyEnvelope(fmt.Sprintf("❌ step2 隔离草稿使用了不支持的 schema_version=%d；请先升级应用后再继续", version)), nil
					}
				}
				if service.GateBlocksStep2(dir, pf, in.Episode) {
					return tools.DenyEnvelope("⏸️ 本集 step1 尚未经 web 审核确认（或确认后内容又被改），step2 草稿暂不晋升。" +
						"请在 Web 端审阅并确认本集 step1 内容后再调用本工具。"), nil
				}
				rc, rcErr := tools.ReferenceContextForTool(ctx, pf, caps)
				if rcErr != nil {
					return tools.DenyEnvelope("❌ " + rcErr.Error()), nil
				}
				generator, genErr := gen(ctx, pf)
				if genErr != nil {
					return tools.DenyEnvelope("❌ 文本生成器未就绪：" + genErr.Error()), nil
				}
				scriptGen, genErr := service.NewScriptGenerator(projects, project, generator, rc.Caps.Durations,
					service.WithReferenceContext(rc))
				if genErr != nil {
					return tools.DenyEnvelope("❌ 初始化剧本生成器失败：" + genErr.Error()), nil
				}
				resultPath, promoErr := scriptGen.PromoteReferenceStep2Draft(in.Episode)
				if promoErr != nil {
					return tools.DenyEnvelope(promoErr.Error()), nil
				}
				return tools.OKEnvelope("✅ step2 视觉展开已校验通过并晋升: " + resultPath), nil
			}

			return tools.DenyEnvelope(fmt.Sprintf("❌ 第 %d 集没有待处置的隔离草稿", in.Episode)), nil
		})
}

// promoteReferenceStep1 按产出时那套校验器全量重判 step1 隔离草稿，通过则晋升为正式 step1 并清除草稿。
//
// 返回 (消息文本, 是否为错误)。isError 为 true 时消息以 deny 信封回传；为 false 时是晋升成功的摘要。
func promoteReferenceStep1(
	ctx context.Context, dir string, pf *domain.ProjectFile, episode int,
	draft *service.QuarantinedDraft, caps tools.VideoCapabilityReader,
) (string, bool) {
	resolver, ok := tools.ReferenceResolver(caps)
	if !ok {
		return "❌ 无法解析视频模型能力：参考晋升需要完整视频能力解析器", true
	}
	revalidation, err := service.RevalidateReferenceStep1Draft(ctx, dir, pf, episode, draft, resolver)
	if err != nil {
		return "❌ " + err.Error(), true
	}
	if revalidation.SchemaFailed {
		report, qerr := service.QuarantineAndReport(dir, episode, service.QuarantineKindStep1,
			draft.Content, revalidation.Violations, draft.Meta, draft.Extra)
		if qerr != nil {
			return "❌ 写入隔离草稿失败：" + qerr.Error(), true
		}
		return report, true
	}
	if len(revalidation.Violations) > 0 {
		report, qerr := service.QuarantineAndReport(dir, episode, service.QuarantineKindStep1,
			map[string]any{"units": toAnyMaps(revalidation.FlatUnits)}, revalidation.Violations, draft.Meta, draft.Extra)
		if qerr != nil {
			return "❌ 写入隔离草稿失败：" + qerr.Error(), true
		}
		return report, true
	}

	units, err := service.BuildReferenceUnitsFromFlat(revalidation.FlatUnits, pf, episode, revalidation.Caps.MaxRefs)
	if err != nil {
		return "❌ " + err.Error(), true
	}
	expected := service.UncheckedFingerprintExpectation()
	if raw, present := draft.Meta["base_fingerprint"]; present {
		if raw == nil {
			expected = service.BaselineFingerprint(nil)
		} else if value, ok := raw.(string); ok {
			expected = service.BaselineFingerprint(&value)
		}
	}
	var conflict *service.Step1WriteConflict
	var step1Path string
	lockErr := service.Step1WriteLock(dir, episode, func(path string) error {
		step1Path = path
		_, werr := service.WriteStep1Locked(dir, episode, map[string]any{"units": toAnyMaps(units)}, expected, true)
		if werr != nil {
			var conflictErr *service.Step1WriteConflict
			if errors.As(werr, &conflictErr) {
				conflict = conflictErr
				return nil
			}
			return werr
		}
		return service.ClearQuarantine(dir, episode, service.QuarantineKindStep1)
	})
	if lockErr != nil {
		return "❌ 晋升失败：" + lockErr.Error(), true
	}
	if conflict != nil {
		return service.RenderStep1ConflictReport(episode, draft, conflict), true
	}
	texts := make([]string, 0, len(revalidation.FlatUnits))
	for _, flat := range revalidation.FlatUnits {
		texts = append(texts, fmt.Sprint(flat["text"]))
	}
	warnings := service.ReferenceVoiceWarningLines(texts, pf, revalidation.Caps.Voice)
	return service.ReferenceResultText(step1Path, units, warnings, "晋升"), false
}


// unsupportedVersion 提取隔离信封的未来版本号（非该错误时 ok=false）。
func unsupportedVersion(err error) (int, bool) {
	var unsupported *service.UnsupportedQuarantineSchemaError
	if errors.As(err, &unsupported) {
		return unsupported.Version, true
	}
	return 0, false
}

// ReferenceTools 构造参考生视频工具链（由 agent 运行时在依赖齐备时追加）。
func ReferenceTools(
	project string, gen service.TextCompletionFactory, projects tools.TextGenerationProjects, caps tools.VideoCapabilityReader,
) ([]tool.InvokableTool, error) {
	split, err := newSplitReferenceVideoUnitsTool(project, gen, projects, caps)
	if err != nil {
		return nil, err
	}
	open, err := newOpenReferenceStep1ForEditTool(project, projects)
	if err != nil {
		return nil, err
	}
	promote, err := newValidateAndPromoteReferenceDraftTool(project, gen, projects, caps)
	if err != nil {
		return nil, err
	}
	return []tool.InvokableTool{split, open, promote}, nil
}
