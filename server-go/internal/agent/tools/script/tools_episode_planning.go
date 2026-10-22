package script

import "github.com/shotwise/shotwise/internal/agent/tools"

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/domain"
	servicescript "github.com/shotwise/shotwise/internal/service/script"
)


type planEpisodesIn struct {
	Instructions string `json:"instructions,omitempty" jsonschema:"description=用户分集意见原文（可选，如「按章节对齐切分」）；原样注入规划 prompt 的「用户意见」分节，遵循强度由正文表达。每批调用都要重复带上，缺省/空白视同未传，最长 4000 字符"`
}

// NewPlanEpisodesTool 构造 plan_episodes。
func NewPlanEpisodesTool(
	project string, store servicescript.EpisodeStore, gen servicescript.TextCompletionFactory,
) (tool.InvokableTool, error) {
	return toolutils.InferTool("plan_episodes",
		"分集规划：从账本 planning_cursor 起读一个源文窗口，调用项目配置的文本模型一次规划出"+
			"窗口内所有剧情弧完整的集（标题/钩子/原文范围；drama 另含分集大纲），在同一把项目锁内"+
			"写账本、派生 source/episode_N.txt 并清理残留派生文件。返回账本摘要（每集标题+钩子+体量）。"+
			"窗口字数与每批集数上限为内部默认，project.json 顶层 planning_window_chars / "+
			"planning_max_episodes 可覆盖，每集目标体量沿用 episode_target_units。"+
			"用户表达分集意见（如按章节对齐切分、指定某处收尾）时经 instructions 传入原文；意见原样注入"+
			"规划 prompt 的「用户意见」分节（遵循强度由正文表达，需要强约束时在正文写明），并附带已规划"+
			"集数、未规划余量、本窗口体量供换算切分节奏。规划按窗口分多批（长篇会多次调用本工具），"+
			"instructions 不持久化，规划全部完成前每一批调用都要重复带上同一偏好。末批即全部源文规划完毕、"+
			"或再次调用已无新内容时，返回会额外附全局体量核对材料（累计集数、体量最小几集、体量中位数、"+
			"目标体量）：若用户给过总集数、按章节对齐等结构性偏好，须对照核对、有偏差明确告知用户；"+
			"常规批次只报累计已规划集数。提交时按参与源文件记录内容指纹；若检测到已记录的源文件内容被改动"+
			"或消失（源文被替换/编辑/删除，即使账本坐标仍在界内），会拒绝规划并指名变动文件，此时需调用 "+
			"reset_episode_planning 做全量重置后才能重新规划。",
		func(ctx context.Context, in planEpisodesIn) (string, error) {
			instructions := in.Instructions
			if len([]rune(instructions)) > tools.MaxInstructionsLen {
				return tools.DenyEnvelope(fmt.Sprintf(
					"❌ 参数错误：instructions 过长（%d 字符，上限 %d），请精简后重试",
					len([]rune(instructions)), tools.MaxInstructionsLen)), nil
			}
			if store == nil || gen == nil {
				return tools.DenyEnvelope("episode planning not configured"), nil
			}
			pf, err := store.Get(project)
			if err != nil {
				return tools.DenyEnvelope("❌ 分集规划失败：" + err.Error()), nil
			}
			generator, err := gen(ctx, pf)
			if err != nil {
				return tools.DenyEnvelope("❌ 分集规划失败：" + err.Error()), nil
			}
			planner, err := servicescript.NewEpisodePlanner(store, project, generator)
			if err != nil {
				return tools.DenyEnvelope("❌ 分集规划失败：" + err.Error()), nil
			}
			result, err := planner.Plan(ctx, instructions)
			if err != nil {
				return tools.DenyEnvelope("❌ 分集规划失败：" + err.Error()), nil
			}
			if len(result.Episodes) == 0 && result.SourceExhausted {
				lines := []string{"源文已全部规划完毕，没有可规划的新内容。"}
				lines = append(lines, renderLedgerStats(result.LedgerStats)...)
				return tools.OKEnvelope(strings.Join(lines, "\n")), nil
			}
			return tools.OKEnvelope(formatPlanSummary(result)), nil
		})
}

// formatPlanSummary 渲染账本摘要：每集标题 + 钩子 + 体量（阅读单位）。
func formatPlanSummary(result *servicescript.PlanResult) string {
	lines := []string{fmt.Sprintf("✅ 已规划 %d 集：", len(result.Episodes))}
	for _, ep := range result.Episodes {
		statusNote := ""
		if ep.LedgerStatus == domain.LedgerStatusStale {
			statusNote = "（stale，需重做下游产物）"
		}
		lines = append(lines, fmt.Sprintf("- 第 %d 集《%s》%s｜体量约 %d｜钩子：%s",
			ep.Episode, ep.Title, statusNote, ep.ReadingUnits, ep.Hook))
	}
	if result.SourceExhausted {
		lines = append(lines, "源文已全部规划完毕。")
	} else if result.Cursor != nil {
		lines = append(lines, fmt.Sprintf("下一批规划起点：%s 偏移 %d",
			result.Cursor.SourceFile, result.Cursor.Offset))
	}
	if result.LedgerStats != nil {
		lines = append(lines, renderLedgerStats(result.LedgerStats)...)
	} else {
		lines = append(lines, fmt.Sprintf("累计已规划 %d 集。", result.TotalPlanned))
	}
	lines = append(lines, "请把以上摘要展示给用户做批级审阅；需要调整时先调用 reset_episode_planning 退回到"+
		"最早受影响的集，再带 instructions 重新调用本工具。")
	return strings.Join(lines, "\n")
}

// renderLedgerStats 渲染全局核对材料。
func renderLedgerStats(stats *servicescript.LedgerStats) []string {
	if stats == nil {
		return nil
	}
	lines := []string{fmt.Sprintf("累计总集数：%d", stats.TotalEpisodes)}
	if len(stats.Smallest) > 0 {
		parts := make([]string, 0, len(stats.Smallest))
		for _, item := range stats.Smallest {
			parts = append(parts, fmt.Sprintf("第 %d 集（约 %d）", item[0], item[1]))
		}
		lines = append(lines, "体量最小的几集："+strings.Join(parts, "、"))
	}
	if stats.MedianUnits != nil {
		lines = append(lines, fmt.Sprintf("全账本体量中位数：约 %d", *stats.MedianUnits))
	}
	if stats.TargetUnits != nil {
		lines = append(lines, fmt.Sprintf("每集目标体量设置：约 %d", *stats.TargetUnits))
	}
	lines = append(lines, "若用户给过总集数、按章节对齐等结构性偏好，请对照以上分布核实，有偏差须向用户明确说明。")
	return lines
}

type resetEpisodePlanningIn struct {
	FromEpisode     *int `json:"from_episode" jsonschema:"description=重置起点集号；1 为全量重置，大于 1 为部分重置（保留其前的集）"`
	ConfirmConsumed bool `json:"confirm_consumed,omitempty" jsonschema:"description=已向用户确认波及的已消费集后置 true"`
}

// NewResetEpisodePlanningTool 构造 reset_episode_planning。
func NewResetEpisodePlanningTool(project string, store servicescript.EpisodeStore) (tool.InvokableTool, error) {
	return toolutils.InferTool("reset_episode_planning",
		"重置分集规划：把账本退回未规划状态的逃生口，不调用文本模型，不受供应商配置影响。"+
			"from_episode=1 是全量重置（清空整个账本与 planning_cursor），源文被替换或删除重建、"+
			"账本写坏导致规划报「起点越界」「范围无效」等错误并永久失败时用它恢复，零前置校验、任何损坏状态都能执行成功。"+
			"from_episode>1 是部分重置：保留第 1..from_episode-1 集，只清除 from_episode 起的条目，"+
			"游标退到第 from_episode-1 集原文范围末尾，下次 plan_episodes 从第 from_episode 集续接编号；"+
			"这条路径有前置校验（全部已记录源文指纹须与当前源文一致，且保留段坐标须完整、连续、落在当前源文界内），"+
			"任一不满足会拒绝执行并指明具体原因，此时改用 from_episode=1 做全量重置。"+
			"两种模式都对波及已消费集（已有 step1/剧本/媒体产物）时不执行并返回受影响清单，须告知用户、"+
			"确认后带 confirm_consumed=true 重新调用。任何下游产物（剧本、媒体）都不会被删除；"+
			"重置范围内可由账本重造的 source/episode_N.txt 会被删除，无原文范围记录的集文件改名留底"+
			"（不会丢内容），保留段的派生文件不受影响。",
		func(ctx context.Context, in resetEpisodePlanningIn) (string, error) {
			_ = ctx
			if in.FromEpisode == nil || *in.FromEpisode < 1 {
				return tools.DenyEnvelope(fmt.Sprintf("❌ 参数错误：from_episode 必须是正整数，收到 %v", in.FromEpisode)), nil
			}
			if store == nil {
				return tools.DenyEnvelope("episode planning not configured"), nil
			}
			fromEpisode := *in.FromEpisode
			result, confirmation, err := servicescript.ResetEpisodePlanning(store, project, fromEpisode, in.ConfirmConsumed)
			if err != nil {
				return tools.DenyEnvelope("❌ 分集规划重置失败：" + err.Error()), nil
			}

			if confirmation != nil {
				episodes := intsToText(confirmation.ConsumedEpisodes)
				aftermath := "账本清空后这些集需要重新规划"
				if fromEpisode != 1 {
					aftermath = fmt.Sprintf("这些集的账本条目被清除后需要重新规划，第 1..%d 集保留不动", fromEpisode-1)
				}
				lines := []string{
					fmt.Sprintf("⚠️ 本次重置会波及已消费集（已有 step1/剧本/媒体产物）：第 %s 集。尚未执行任何改动。", episodes),
					"请把影响范围告知用户；用户确认后带 confirm_consumed=true 重新调用" +
						fmt.Sprintf("（剧本与媒体产物不会被删除，但%s）。", aftermath),
				}
				if len(confirmation.ArchivedFiles) > 0 {
					lines = append(lines, fmt.Sprintf("其中无原文范围记录的集文件会改名留底：%s",
						strings.Join(confirmation.ArchivedFiles, "、")))
				}
				return tools.OKEnvelope(strings.Join(lines, "\n")), nil
			}

			var lines []string
			if fromEpisode == 1 {
				lines = append(lines, fmt.Sprintf("✅ 已全量重置分集规划：清空 %d 集，planning_cursor 已置空。",
					len(result.RemovedEpisodes)))
			} else {
				lines = append(lines, fmt.Sprintf(
					"✅ 已部分重置分集规划：清空第 %d 集起共 %d 集，planning_cursor 已退到第 %d 集原文范围末尾。",
					fromEpisode, len(result.RemovedEpisodes), fromEpisode-1))
			}
			if len(result.DeletedFiles) > 0 {
				lines = append(lines, fmt.Sprintf("已删除可重造的派生集文件 %d 个。", len(result.DeletedFiles)))
			}
			if len(result.ArchivedFiles) > 0 {
				pairs := make([]string, 0, len(result.ArchivedFiles))
				for _, pair := range result.ArchivedFiles {
					pairs = append(pairs, pair[0]+" → "+pair[1])
				}
				lines = append(lines, fmt.Sprintf("无原文范围记录的集文件已改名留底（内容保留）：%s",
					strings.Join(pairs, "、")))
			}
			if len(result.ConsumedEpisodes) > 0 {
				lines = append(lines, fmt.Sprintf("第 %s 集的剧本 / 媒体产物仍在磁盘，未删除。",
					intsToText(result.ConsumedEpisodes)))
			}
			if fromEpisode == 1 {
				lines = append(lines, "账本已空，请调用 plan_episodes 从头重新规划（集号从第 1 集起）。")
			} else {
				lines = append(lines, fmt.Sprintf("请调用 plan_episodes 继续规划（新集号从第 %d 集起）。", fromEpisode))
			}
			return tools.OKEnvelope(strings.Join(lines, "\n")), nil
		})
}

// intsToText 把集号列表渲染为「1、2、3」。
func intsToText(nums []int) string {
	parts := make([]string, 0, len(nums))
	for _, num := range nums {
		parts = append(parts, fmt.Sprint(num))
	}
	return strings.Join(parts, "、")
}
