// 宫格联合图 prompt 组装（对应 lib/grid/prompt_builder.py）。
//
// 宫格采用首尾帧链式结构：格 0 是首个场景开场，格 1~n-1 是相邻场景的过渡帧
// （前一场景的结束 = 后一场景的开始），其余为占位格。
package domain

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// GridReferenceLabel 是【参考图说明】里的一行（标签 → 角色名），顺序即渲染顺序。
type GridReferenceLabel struct {
	Label string
	Name  string
}

// gridCompositionKeyOrder 是 composition 子字段的渲染顺序。
//
// Python 按 dict 插入顺序迭代；Go 的 map 无顺序，故按已知字段固定顺序渲染，
// 其余未知字段按字典序补尾——保证 prompt 在多次调用间稳定可比。
var gridCompositionKeyOrder = []string{"shot_type", "lighting", "ambiance"}

// BuildGridPrompt 组装宫格联合图 prompt。
//
// aspectRatio 是单格比例，gridAspectRatio 为整图比例；后者为空时回落到前者。
func BuildGridPrompt(
	scenes []map[string]any, idField string, rows, cols int,
	style, aspectRatio, gridAspectRatio string, refMapping []GridReferenceLabel,
) string {
	total := rows * cols
	nScenes := len(scenes)

	effectiveGridAR := gridAspectRatio
	if effectiveGridAR == "" {
		effectiveGridAR = aspectRatio
	}
	panelAR := computePanelAspect(effectiveGridAR, rows, cols)

	var lines []string
	lines = append(lines, fmt.Sprintf(
		"你是一位专业的分镜画师。请严格按照 %d×%d 宫格布局生成一张包含恰好 %d 个等大画格的联合图。",
		rows, cols, total))
	lines = append(lines, "")
	lines = append(lines, "【布局要求】")
	lines = append(lines, fmt.Sprintf("- 恰好 %d 行 %d 列，共 %d 个画格，阅读顺序：从左到右，从上到下", rows, cols, total))
	lines = append(lines, fmt.Sprintf("- 整体图片比例：%s", effectiveGridAR))
	lines = append(lines, fmt.Sprintf("- 每个画格比例：%s，所有画格大小完全相同", panelAR))
	lines = append(lines, "- 画格之间无边框、无间隙、无留白，紧密排列")
	lines = append(lines, "- 不得合并画格、不得遗漏画格、不得错位排列")
	lines = append(lines, "- 所有画格保持一致的角色外观、光线和色彩风格")
	lines = append(lines, "")
	lines = append(lines, "【帧链节奏】")
	lines = append(lines, "本宫格采用首尾帧链式结构：")
	lines = append(lines, "- 格0 是第一个场景的开场画面")
	lines = append(lines, fmt.Sprintf("- 格1~格%d 是相邻场景的过渡帧（前一场景的结束 = 后一场景的开始）", nScenes-1))
	lines = append(lines, "- 相邻格之间应体现画面的自然过渡和动作延续")
	lines = append(lines, "")

	if len(refMapping) > 0 {
		lines = append(lines, "【参考图说明】")
		for _, pair := range refMapping {
			lines = append(lines, fmt.Sprintf("- %s：%s", pair.Label, pair.Name))
		}
		lines = append(lines, "")
	}

	lines = append(lines, "【各格内容】")
	for cellIdx := 0; cellIdx < total; cellIdx++ {
		rowNum := cellIdx/cols + 1
		colNum := cellIdx%cols + 1
		position := fmt.Sprintf("row%d col%d", rowNum, colNum)
		switch {
		case cellIdx == 0:
			scene := scenes[0]
			sceneID := gridSceneText(scene[idField])
			lines = append(lines, fmt.Sprintf("格%d（%s）— %s开场：", cellIdx, position, sceneID))
			lines = append(lines, "  "+extractImageDesc(scene))
		case cellIdx < nScenes:
			prevScene := scenes[cellIdx-1]
			nextScene := scenes[cellIdx]
			prevID := gridSceneText(prevScene[idField])
			nextID := gridSceneText(nextScene[idField])
			lines = append(lines, fmt.Sprintf("格%d（%s）— %s→%s过渡：", cellIdx, position, prevID, nextID))
			lines = append(lines, fmt.Sprintf("  %s，过渡到 %s", extractAction(prevScene), extractImageDesc(nextScene)))
		default:
			lines = append(lines, fmt.Sprintf("格%d（%s）— 空占位：纯灰色背景，无任何内容", cellIdx, position))
		}
	}
	lines = append(lines, "")
	lines = append(lines, "【风格要求】")
	lines = append(lines, style)
	lines = append(lines, "")
	lines = append(lines, "【负面约束】")
	lines = append(lines, "禁止出现以下任何元素：")
	lines = append(lines,
		"- 文字、字幕、标签、标题、数字编号、时间戳",
		"- 水印、logo、签名",
		"- 白色边框、黑色边框、粗边框、装饰性边框",
		"- 分隔线、间隙、间距、留白、padding、margin",
		"- 白色背景、纯色背景条",
		"- 合并的画格、缺失的画格、错位的画格",
		"- 连续全景图（非分格）、单张大图",
		"- 模糊、低画质、噪点",
		"- 拼贴感、蒙太奇拼接感",
		"- 画格大小不一致、画格比例不一致",
	)
	return strings.Join(lines, "\n")
}

// computePanelAspect 从整体宫格比例推算单格比例（交叉相乘避免浮点）。
// 例：grid 4:3, 3 行 2 列 → panel (4/2):(3/3) = 2:1。
func computePanelAspect(gridAspectRatio string, rows, cols int) string {
	parts := strings.Split(gridAspectRatio, ":")
	if len(parts) != 2 {
		return gridAspectRatio
	}
	gw, errW := strconv.Atoi(strings.TrimSpace(parts[0]))
	gh, errH := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errW != nil || errH != nil {
		return gridAspectRatio
	}
	pw := gw * rows
	ph := gh * cols
	g := gcdInt(pw, ph)
	if g == 0 {
		return gridAspectRatio
	}
	return strconv.Itoa(pw/g) + ":" + strconv.Itoa(ph/g)
}

func gcdInt(a, b int) int {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// extractImageDesc 取场景的画面描述：image_prompt 为对象时拼 scene + composition，
// 为字符串时原样返回（对应 Python _extract_image_desc）。
func extractImageDesc(scene map[string]any) string {
	imagePrompt := scene["image_prompt"]
	if m, ok := imagePrompt.(map[string]any); ok {
		var parts []string
		if sceneText, _ := m["scene"].(string); sceneText != "" {
			parts = append(parts, sceneText)
		}
		if composition, ok := m["composition"].(map[string]any); ok {
			var compParts []string
			for _, key := range gridCompositionKeys(composition) {
				value := composition[key]
				if gridTruthy(value) {
					compParts = append(compParts, fmt.Sprintf("%s: %v", key, value))
				}
			}
			if len(compParts) > 0 {
				parts = append(parts, strings.Join(compParts, "，"))
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "；")
		}
		return ""
	}
	return pythonStr(imagePrompt)
}

// extractAction 取 video_prompt 里的收尾动作：对象取 action 字段，其余原样转文本。
func extractAction(scene map[string]any) string {
	videoPrompt, present := scene["video_prompt"]
	if !present {
		return ""
	}
	if m, ok := videoPrompt.(map[string]any); ok {
		return pythonStr(m["action"])
	}
	return pythonStr(videoPrompt)
}

// gridCompositionKeys 返回 composition 的渲染键序：已知字段在前（固定序），未知字段按字典序补尾。
func gridCompositionKeys(composition map[string]any) []string {
	keys := make([]string, 0, len(composition))
	for _, known := range gridCompositionKeyOrder {
		if _, ok := composition[known]; ok {
			keys = append(keys, known)
		}
	}
	seen := map[string]bool{}
	for _, known := range keys {
		seen[known] = true
	}
	var rest []string
	for key := range composition {
		if !seen[key] {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

// gridSceneText 复刻 Python str(item.get(id_field, "")) 的文本形态。
func gridSceneText(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return pythonStr(value)
}

// pythonStr 复刻 Python str(value)：null 记 "None"，布尔按 True/False。
//
// 数字脏数据（JSON 解出为 float64）与 Python str() 的格式不完全一致（如 3.0 在 Python
// 为 "3.0"、这里为 "3"），仅影响非字符串的异常数据。
func pythonStr(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(value)
}

// gridTruthy 复刻 Python bool(value) 的真值判定。
func gridTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}
