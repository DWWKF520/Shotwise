// 短剧 / 说书节奏建议（对应 lib/prompt_rules/episode_pacing.py）。
//
// 短剧体裁特征：开篇 ~4 秒钩子、中段每 ~15 秒一次情绪转折、末镜停在情绪极致瞬间。措辞在
// prompt builder 与 subagent .md 之间共享同一份文本，避免两处漂移。
//
// 文风用「宜 / 例」而非「必须 / 禁止」，给 LLM 在边界条件下的判断空间（如视频模型最短只支持
// 5 秒时，「~4 秒」比「=4 秒」更可执行）。
package script

import "fmt"

// dramaPacingRules 是剧集（drama）节奏建议。
const dramaPacingRules = `分集节奏（短剧体裁建议）：
- 开篇 ~4 秒承担钩子职能：用强冲击 / 悬念 / 危机切入，避免介绍性远景。
- 中段每 ~15 秒宜安排一次转折点（动作转折 / 情绪反差 / 关系撕裂 / 异常事件），
  通过画面权重和景别变化呈现，避免长段平铺。
- 末镜停在情绪极致瞬间，shot_type 倾向 Close-up / Extreme Close-up，
  给观众留下回看的钩子。`

// narrationPacingRules 是说书（narration）节奏建议。
const narrationPacingRules = `说书节奏建议：
- 首段画面（朗读前 ~4 秒）服务于钩子：用强冲击 / 悬念 / 危机匹配钩子台词，
  避免平铺式开场。
- 末段画面服务于卡点留悬（特写人物 / 关键物件 / 极端表情），
  shot_type 倾向 Close-up / Extreme Close-up。`

// RenderPacingSection 按内容模式返回节奏建议段；未知模式报错。
func RenderPacingSection(contentMode string) (string, error) {
	switch contentMode {
	case "drama":
		return dramaPacingRules, nil
	case "narration":
		return narrationPacingRules, nil
	}
	return "", fmt.Errorf("unknown content_mode: %q", contentMode)
}
