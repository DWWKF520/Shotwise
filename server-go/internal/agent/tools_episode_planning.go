package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/service"
)

// 分集规划工具（对应 Python sdk_tools/episode_planning.py）。

// WithEpisodePlanning 注入分集规划工具的项目依赖；未注入时不注册这组工具。
func WithEpisodePlanning(store service.EpisodeStore) RuntimeOption {
	return func(r *Runtime) { r.episodeStore = store }
}

type resetEpisodePlanningIn struct {
	FromEpisode     *int `json:"from_episode" jsonschema:"description=重置起点集号；1 为全量重置，大于 1 为部分重置（保留其前的集）"`
	ConfirmConsumed bool `json:"confirm_consumed,omitempty" jsonschema:"description=已向用户确认波及的已消费集后置 true"`
}

// newResetEpisodePlanningTool 构造 reset_episode_planning。
func newResetEpisodePlanningTool(project string, store service.EpisodeStore) (tool.InvokableTool, error) {
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
				return denyEnvelope(fmt.Sprintf("❌ 参数错误：from_episode 必须是正整数，收到 %v", in.FromEpisode)), nil
			}
			if store == nil {
				return denyEnvelope("episode planning not configured"), nil
			}
			fromEpisode := *in.FromEpisode
			result, confirmation, err := service.ResetEpisodePlanning(store, project, fromEpisode, in.ConfirmConsumed)
			if err != nil {
				return denyEnvelope("❌ 分集规划重置失败：" + err.Error()), nil
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
				return okEnvelope(strings.Join(lines, "\n")), nil
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
			return okEnvelope(strings.Join(lines, "\n")), nil
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
