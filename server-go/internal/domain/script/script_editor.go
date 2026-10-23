// 剧本编辑核心（纯函数）。对应 lib/script_editor.py。
//
// 把「如何按 id 安全地编辑一份剧本 dict」收敛到唯一一处：喂入剧本 dict + 一个编辑操作，
// 就地改 dict，或对非法操作（id 未命中、拆分份数不足、字段路径不存在）返回 ScriptEditError。
// 不读盘、不依赖项目状态、不做结构良构校验——结构是否合法交给写盘统一入口的「不更坏」校验兜底，
// 本模块只负责数组手术、id 分配与资产作废。
package script

import (
	"fmt"
	"github.com/shotwise/shotwise/internal/domain/project"
	"strings"
)

// 剧本编辑错误的翻译坐标 key（与 provider/failures.go 登记的脚本编辑错误码同源）。
const (
	ScriptEditErrorKey               = "script_edit_error"
	ScriptEditItemsNotList           = "script_edit_items_not_list"
	ScriptEditUnitListsInvalid       = "script_edit_unit_lists_invalid"
	ScriptEditGeneratedAssetsInvalid = "script_edit_generated_assets_invalid"
)

// ScriptEditError 表示剧本编辑操作非法（id 未命中、数组越界、拆分份数不足、字段路径不存在等）。
//
// Key / Params 是给用户可见路径准备的翻译坐标；只被 agent 工具与日志消费的调用点沿用默认 key。
type ScriptEditError struct {
	Message string
	Key     string
	Params  map[string]any
}

func (e *ScriptEditError) Error() string { return e.Message }

func newScriptEditError(message string) *ScriptEditError {
	return &ScriptEditError{Message: message, Key: ScriptEditErrorKey}
}

// scriptIDFields 是四类骨架的分镜 id 字段名集合（patch 不可改这些字段）。
var scriptIDFields = map[string]bool{
	"segment_id": true, "scene_id": true, "unit_id": true, "shot_id": true,
}

// ResolveScriptItems 按内容/生成模式选出当前剧本的分镜数组、其 id 字段名与骨架种类。
//
// 键缺失视为空数组；键存在但类型非数组（含值为 null）时 fail loud——不静默降级为 nil，
// 避免把数据损坏掩盖成「未找到 id」。返回的切片在键存在时即剧本内的实际元素集合，
// 元素为 map 引用可就地编辑；增删元素由调用方写回 script[kind]。
func ResolveScriptItems(script map[string]any) ([]any, string, string, error) {
	kind := ResolveScriptKind(script)
	skeleton, ok := ScriptSkeletonOf(kind)
	if !ok {
		return nil, "", kind, newScriptEditError(fmt.Sprintf("未知骨架种类: %s", kind))
	}
	raw, present := script[kind]
	if !present {
		return nil, skeleton.IDField, kind, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, skeleton.IDField, kind, &ScriptEditError{
			Message: fmt.Sprintf("%s 必须是列表，当前为 %s", kind, jsonTypeName(raw)),
			Key:     ScriptEditItemsNotList,
			Params:  map[string]any{"kind": kind, "type_name": jsonTypeName(raw)},
		}
	}
	return items, skeleton.IDField, kind, nil
}

// ScriptItemIDs 返回剧本当前全部分镜 id（按数组顺序）。
func ScriptItemIDs(script map[string]any) ([]string, error) {
	items, idField, _, err := ResolveScriptItems(script)
	if err != nil {
		return nil, err
	}
	return scriptIDsOf(items, idField), nil
}

// PatchScriptField 按 id 定位一个分镜并设置其（可嵌套的）字段。纯 setter，不触碰 generated_assets。
func PatchScriptField(script map[string]any, itemID, fieldPath string, value any) error {
	items, idField, _, err := ResolveScriptItems(script)
	if err != nil {
		return err
	}
	idx, err := findScriptItemIndex(items, idField, itemID)
	if err != nil {
		return err
	}
	item, ok := items[idx].(map[string]any)
	if !ok {
		return newScriptEditError(fmt.Sprintf("分镜 %s 不是对象", itemID))
	}
	return setScriptNested(item, fieldPath, value)
}

// InsertScriptSegment 在 afterID 之后插入一个新分镜，分配派生自锚点 id 的稳定新 id。
//
// 新分镜的 id 字段被强制改写为 {afterID}_{k}（唯一），generated_assets 与 end_frame_image 清空。
// 其余字段由 agent 提供，结构是否合法由写盘统一入口校验。
func InsertScriptSegment(script map[string]any, afterID string, newItem map[string]any) error {
	if newItem == nil {
		return newScriptEditError("new_item 必须是对象")
	}
	items, idField, kind, err := ResolveScriptItems(script)
	if err != nil {
		return err
	}
	idx, err := findScriptItemIndex(items, idField, afterID)
	if err != nil {
		return err
	}
	item := project.CloneMap(newItem)
	item[idField] = nextSuffixedScriptID(afterID, scriptIDSet(items, idField))
	item["generated_assets"] = map[string]any{}
	// 尾帧快照按镜头 id 命名，新 id 名下还没有快照；agent 自带的值只会指向别人的快照。
	delete(item, "end_frame_image")

	script[kind] = insertScriptSlice(items, idx+1, []any{item})
	return nil
}

// RemoveScriptSegment 按 id 删除一个分镜。被删分镜的资产随之消失；不改动其余分镜的 id。
func RemoveScriptSegment(script map[string]any, itemID string) error {
	items, idField, kind, err := ResolveScriptItems(script)
	if err != nil {
		return err
	}
	idx, err := findScriptItemIndex(items, idField, itemID)
	if err != nil {
		return err
	}
	script[kind] = append(append([]any{}, items[:idx]...), items[idx+1:]...)
	return nil
}

// SplitScriptSegment 把 itemID 分镜按 agent 提供的各部分内容拆成多个（至少 2 份）。
//
// 首个部分保留原 id 且保留 generated_assets 与 end_frame_image（锚点延续，与 insert 的资产
// 不动语义对齐）；其余部分取 {itemID}_{k} 后缀的新 id 并清空这两个字段。
// reference 模式下拆的是顶层 video_units，每个 part 是一个完整新 unit。
func SplitScriptSegment(script map[string]any, itemID string, parts []map[string]any) error {
	if len(parts) < 2 {
		return newScriptEditError("split 至少需要 2 个部分")
	}
	for _, part := range parts {
		if part == nil {
			return newScriptEditError("split 的每个部分必须是对象")
		}
	}
	items, idField, kind, err := ResolveScriptItems(script)
	if err != nil {
		return err
	}
	idx, err := findScriptItemIndex(items, idField, itemID)
	if err != nil {
		return err
	}
	anchor, _ := items[idx].(map[string]any)
	var anchorAssets map[string]any
	anchorHasEndFrame := false
	if anchor != nil {
		anchorAssets, _ = anchor["generated_assets"].(map[string]any)
		_, anchorHasEndFrame = anchor["end_frame_image"]
	}

	taken := scriptIDSet(items, idField)
	newParts := make([]any, 0, len(parts))
	for offset, raw := range parts {
		part := project.CloneMap(raw)
		if offset == 0 {
			part[idField] = itemID
			// 锚点延续：以原分镜实际值为准，不让 agent 在 parts[0] 凭空写资产路径。
			if anchorAssets != nil {
				part["generated_assets"] = project.CloneMap(anchorAssets)
			} else {
				part["generated_assets"] = map[string]any{}
			}
			if anchorHasEndFrame {
				part["end_frame_image"] = anchor["end_frame_image"]
			} else {
				delete(part, "end_frame_image")
			}
		} else {
			newID := nextSuffixedScriptID(itemID, taken)
			taken[newID] = true
			part[idField] = newID
			part["generated_assets"] = map[string]any{}
			// 新 id 名下没有快照，agent 自带的值只会指向锚点的快照。
			delete(part, "end_frame_image")
		}
		newParts = append(newParts, part)
	}

	merged := append(append([]any{}, items[:idx]...), newParts...)
	merged = append(merged, items[idx+1:]...)
	script[kind] = merged
	return nil
}

// setScriptNested 按点分路径设置嵌套字段。
//
// 三类越权字段直接拒绝：generated_assets（资产生命周期与剧本编辑解耦）、end_frame_image
// （值是本服务写出的快照相对路径，只由尾帧端点写入）、分镜 id 字段（id 由 insert/split 派生）。
// 中间路径段不存在时报错（那是真的拼写错误），叶子字段不存在则允许创建（补 LLM 漏写的可选字段）。
func setScriptNested(obj map[string]any, fieldPath string, value any) error {
	parts := strings.Split(fieldPath, ".")
	for _, part := range parts {
		if part == "" {
			return newScriptEditError(fmt.Sprintf("非法字段路径: %q", fieldPath))
		}
	}
	switch parts[0] {
	case "generated_assets":
		return newScriptEditError("patch_episode_script 不可改 generated_assets；资产的生成/重生是独立的显式动作")
	case "end_frame_image":
		return newScriptEditError("patch_episode_script 不可改 end_frame_image；尾帧的设置/清除是独立的显式动作")
	}
	if scriptIDFields[parts[0]] {
		return newScriptEditError(fmt.Sprintf(
			"patch_episode_script 不可改分镜 id 字段 (%s)；id 由 insert/split 派生，不允许直接修改", parts[0]))
	}

	cur := obj
	for _, part := range parts[:len(parts)-1] {
		next, ok := cur[part]
		if !ok {
			return newScriptEditError(fmt.Sprintf("字段路径不存在: %q", fieldPath))
		}
		child, ok := next.(map[string]any)
		if !ok {
			return newScriptEditError(fmt.Sprintf(
				"父节点非对象 (键 %q 类型为 %s): %q", part, jsonTypeName(next), fieldPath))
		}
		cur = child
	}
	cur[parts[len(parts)-1]] = value
	return nil
}

// findScriptItemIndex 按 id 定位分镜下标；未命中报错。
func findScriptItemIndex(items []any, idField, itemID string) (int, error) {
	for idx, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if fmt.Sprint(m[idField]) == itemID {
			return idx, nil
		}
	}
	return 0, newScriptEditError(fmt.Sprintf("未找到 id=%q 的分镜（%s）", itemID, idField))
}

func scriptIDsOf(items []any, idField string) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		ids = append(ids, fmt.Sprint(m[idField]))
	}
	return ids
}

// nextSuffixedScriptID 在 base 后追加 _{k} 生成不与 taken 冲突的稳定新 id（k 从 1 起）。
//
// 先把 base 收敛到 stem（首个 _ 之前的部分）再追加子序号，否则锚点本身已含后缀
// （如 E1S01_1）时会产生 E1S01_1_1 这种多层嵌套，违反剧集 id 的形态约束。
func nextSuffixedScriptID(base string, taken map[string]bool) string {
	stem := strings.SplitN(base, "_", 2)[0]
	for k := 1; ; k++ {
		candidate := fmt.Sprintf("%s_%d", stem, k)
		if !taken[candidate] {
			return candidate
		}
	}
}

// scriptIDSet 把分镜 id 列表转成判重集合。
func scriptIDSet(items []any, idField string) map[string]bool {
	taken := map[string]bool{}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		taken[fmt.Sprint(m[idField])] = true
	}
	return taken
}

func insertScriptSlice(items []any, at int, add []any) []any {
	out := make([]any, 0, len(items)+len(add))
	out = append(out, items[:at]...)
	out = append(out, add...)
	out = append(out, items[at:]...)
	return out
}

// jsonTypeName 取 JSON 值的类型名，用于报错文案（与 Python type().__name__ 对应的中文口径）。
func jsonTypeName(value any) string {
	switch value.(type) {
	case nil:
		return "NoneType"
	case string:
		return "str"
	case float64, float32, int, int64:
		return "number"
	case bool:
		return "bool"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return fmt.Sprintf("%T", value)
}
