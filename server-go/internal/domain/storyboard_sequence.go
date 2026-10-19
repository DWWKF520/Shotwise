// 分镜序列依赖规划（纯函数）。对应 lib/storyboard_sequence.py 的 get_storyboard_items /
// build_storyboard_dependency_plan，以及 enqueue_storyboards.py::_select_items 的「不传过滤」分支。
//
// 把「一份剧本 dict 里有哪些分镜条目、每条的资产引用字段叫什么、按什么分组串行入队」收敛到
// 一处：不读磁盘、不依赖项目状态，供 generate_storyboards 工具构造分镜图入队计划（含依赖分组）。
package domain

import (
	"fmt"
	"strings"
)

// 三类资产引用字段名在四种骨架下恒为 scenes / props：脚本模型的 NarrationSegment / DramaScene /
// AdShot 同名字段都是这两个键。只有角色字段名随骨架变（见 ScriptSkeletons 的 CharsField）。
const (
	storyboardSceneField = "scenes"
	storyboardPropField  = "props"
)

// StoryboardItemFields 是条目里三类资产引用的字段名（CharField 缺失时为空串）。
type StoryboardItemFields struct {
	CharField  string
	SceneField string
	PropField  string
}

// StoryboardItems 取剧本的分镜条目、id 字段名与资产引用字段名。
//
// 与 Python get_storyboard_items 同语义：骨架数组缺失视为空列表。video_units 骨架没有分镜一说
// （视频按 unit 直出），条目强制返回空列表是「该骨架下不存在 storyboard 任务」的明示，调用方
// 据此跳过；其角色字段名取骨架声明的缺位（空串），不返回假字段名让调用方静默取空。判别只看剧本
// 实际骨架、不看项目路线。
//
// 骨架数组存在但类型非数组（含 null）时，Python resolve_items 会抛 ScriptEditError fail loud；
// 本函数无错误返回通道，故降级为空条目（id 字段名仍照骨架给出）。需要 fail loud 的调用方应在
// 调用本函数前自行校验剧本结构。
func StoryboardItems(script map[string]any) (items []any, idField string, fields StoryboardItemFields) {
	items, idField, kind, _ := ResolveScriptItems(script)
	skeleton, _ := ScriptSkeletonOf(kind)
	fields = StoryboardItemFields{
		CharField:  skeleton.CharsField,
		SceneField: storyboardSceneField,
		PropField:  storyboardPropField,
	}
	if kind == "video_units" {
		items = nil
	}
	return items, idField, fields
}

// StoryboardTaskPlan 是单个待入队分镜及其依赖信息。
//
// DependencyResourceID 为空串表示该分镜无依赖（Python 侧为 None）。依赖语义：同一依赖组内
// 后一项必须等前一项的分镜图先生成，用于同组参考图的串行约束。
type StoryboardTaskPlan struct {
	ResourceID           string
	DependencyResourceID string
	DependencyGroup      string
	DependencyIndex      int
}

// BuildStoryboardDependencyPlan 为 targetIDs 构造入队计划（与 Python 同语义，保序）。
//
// 按 items 原序遍历，只保留 id 命中 targetIDs 的条目；targetIDs 为空集时返回空计划。
// 分组按「segment_break 为真 / 组内前驱缺失 / 前驱不在 targetIDs 中」切分，组名形如
// {scriptFilename 或 "storyboard"}:group:{自增序号}（序号从 1 起），DependencyIndex 是组内下标
// （组首为 0）。组首无依赖（DependencyResourceID 为空串），组内其余项依赖其直接前驱。
func BuildStoryboardDependencyPlan(items []any, idField string, targetIDs []string, scriptFilename string) []StoryboardTaskPlan {
	selected := make(map[string]bool, len(targetIDs))
	for _, id := range targetIDs {
		selected[fmt.Sprint(id)] = true
	}
	if len(selected) == 0 {
		return nil
	}

	plans := make([]StoryboardTaskPlan, 0, len(items))
	groupCounter := 0
	currentGroup := ""
	currentGroupIndex := 0

	for index, item := range items {
		resourceID := storyboardResourceID(item, idField)
		if resourceID == "" || !selected[resourceID] {
			continue
		}

		previousResourceID := ""
		if index > 0 {
			previousResourceID = storyboardResourceID(items[index-1], idField)
		}

		startsNewGroup := storyboardSegmentBreak(item) ||
			previousResourceID == "" || !selected[previousResourceID]

		var dependencyResourceID string
		if startsNewGroup {
			groupCounter++
			prefix := scriptFilename
			if prefix == "" {
				prefix = "storyboard"
			}
			currentGroup = fmt.Sprintf("%s:group:%d", prefix, groupCounter)
			currentGroupIndex = 0
		} else {
			currentGroupIndex++
			dependencyResourceID = previousResourceID
		}

		plans = append(plans, StoryboardTaskPlan{
			ResourceID:           resourceID,
			DependencyResourceID: dependencyResourceID,
			DependencyGroup:      currentGroup,
			DependencyIndex:      currentGroupIndex,
		})
	}
	return plans
}

// GroupScenesBySegmentBreak 把连续分镜按 segment_break 切分为分组（对应 Python 同名函数）。
//
// 判据：条目 segment_break 为真且当前组非空时收口当前组、开启新组；末组在遍历结束后收口。
// idField 参数仅为与 Python 侧签名对齐（该函数不读 id），保留以降低调用方迁移成本。
func GroupScenesBySegmentBreak(items []any, idField string) [][]map[string]any {
	_ = idField
	var groups [][]map[string]any
	var current []map[string]any
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if storyboardTruthy(m["segment_break"]) && len(current) > 0 {
			groups = append(groups, current)
			current = nil
		}
		current = append(current, m)
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

// FindStoryboardItem 按 id 查找分镜条目，返回条目与下标（对应 Python find_storyboard_item）。
//
// 判等用 id 字段的 Python str() 文本形态（见 storyboardString），未命中返回 ok=false。
func FindStoryboardItem(items []any, idField, resourceID string) (map[string]any, int, bool) {
	for index, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if storyboardString(m[idField]) == resourceID {
			return m, index, true
		}
	}
	return nil, 0, false
}

// StoryboardItemsMissingImage 返回尚无分镜图的条目 id（保序），对应 Python _select_items 的
// 「不传过滤」分支：generated_assets 归一为空 dict 后取 storyboard_image，其值按 Python 真值
// 判定为空即视为缺图。
//
// 该字段来自磁盘上的剧本 JSON，不可信任：外部编辑可能把 generated_assets 损坏成非 dict
// （list / 字符串等），归一化为空 dict 而非报错，让脏数据走「未生成」的既有分支，不因一条脏
// 数据中断整批入队。
func StoryboardItemsMissingImage(items []any, idField string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if storyboardTruthy(storyboardImage(m)) {
			continue
		}
		out = append(out, storyboardString(m[idField]))
	}
	return out
}

// storyboardImage 取归一化后的 generated_assets.storyboard_image；容器非 dict（脏数据）时返回 nil。
func storyboardImage(m map[string]any) any {
	assets, ok := m["generated_assets"].(map[string]any)
	if !ok {
		return nil
	}
	return assets["storyboard_image"]
}

// storyboardResourceID 复刻 Python “str(item.get(id_field) or "").strip()“：条目 id 的归一文本。
// 非对象条目按空串处理（Python 在此假定条目为 dict，非 dict 会抛 AttributeError）。
func storyboardResourceID(item any, idField string) string {
	m, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	return strings.TrimSpace(storyboardString(storyboardOrEmpty(m[idField])))
}

// storyboardSegmentBreak 取 segment_break 的 Python 真值（缺失 / 脏值按假处理）。
func storyboardSegmentBreak(item any) bool {
	m, ok := item.(map[string]any)
	if !ok {
		return false
	}
	return storyboardTruthy(m["segment_break"])
}

// storyboardOrEmpty 复刻 Python 的 “x or ""“：假值归为 nil（再由 storyboardString 记空串）。
func storyboardOrEmpty(value any) any {
	if !storyboardTruthy(value) {
		return nil
	}
	return value
}

// storyboardString 复刻 Python str(value)：null 记空串，布尔按 True/False，其余取 Go 文本形式。
// 数字脏数据（JSON 解出为 float64）与 Python str() 的格式不完全一致（如 3.0 在 Python 为 "3.0"、
// 这里为 "3"），仅影响 id 非字符串的异常数据。
func storyboardString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
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

// storyboardTruthy 复刻 Python bool(value) 的真值判定。
func storyboardTruthy(value any) bool {
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

// NewStoryboardGeneratedAssets 返回分镜条目的标准 generated_assets 模板。
//
// 与 Python create_generated_assets 同形：补齐缺失键用，写回时只填「原本不存在」的键，
// 已有值一律保留（旧剧本可能带着上一轮生成的产物路径）。
func NewStoryboardGeneratedAssets() map[string]any {
	return map[string]any{
		"storyboard_image":      nil,
		"storyboard_last_image": nil,
		"video_clip":            nil,
		"video_thumbnail":       nil,
		"video_uri":             nil,
		"narration_audio":       nil,
		"grid_id":               nil,
		"grid_cell_index":       nil,
		"status":                "pending",
		"video_generated_at":    nil,
	}
}

// UpdateStoryboardItemStatus 按 generated_assets 内容重算并写回条目状态。
//
// pending / storyboard_ready / completed 三态判定与 Python update_scene_status 一致：
// 有视频即 completed，仅有分镜图即 storyboard_ready，其余 pending。
func UpdateStoryboardItemStatus(item map[string]any) string {
	assets, ok := item["generated_assets"].(map[string]any)
	if !ok {
		assets = map[string]any{}
		item["generated_assets"] = assets
	}
	status := "pending"
	switch {
	case storyboardTruthy(assets["video_clip"]):
		status = "completed"
	case storyboardTruthy(assets["storyboard_image"]):
		status = "storyboard_ready"
	}
	assets["status"] = status
	return status
}
