// 官方剧本里各条目的「待编写」(pending authoring) 状态，读时纯计算（对应 lib/pending_authoring.py）。
//
// 不引入持久化标记：一个条目的视觉层（image_prompt / video_prompt）是与当前正文配套的，当正文
// 自上次编写后变了，该条目的视觉层即与正文脱钩、须重新生成提示词。正文的「当前值」取自该变体的
// step1 结构化中间态（drafts/episode_N/step1_*.json）；两侧按条目 id（scene_id / segment_id /
// unit_id）对齐，只比对 step1 定稿、step2 不重写的字段（内容投影）。
package service

import (
	"os"
	"path/filepath"

	"github.com/shotwise/shotwise/internal/domain"
)

// step1VisualFields 是条目的视觉层字段：判定「已编写」所依据的字段，不参与内容投影比对
// （对应 Python 的 VISUAL_FIELDS）。
var step1VisualFields = map[string]bool{"image_prompt": true, "video_prompt": true}

// 待编写原因常量（对应 Python 的 PendingReason）。
const (
	PendingReasonMissingVisual = "missing_visual"
	PendingReasonStaleVisual   = "stale_visual"
	PendingReasonOrphanItem    = "orphan_item"
)

// pendingAuthoringModeSpec 是某变体的条目定位契约：两侧列表键、id 字段、不参与内容投影的字段。
type pendingAuthoringModeSpec struct {
	step1Key  string
	scriptKey string
	idField   string
	// step1-only（合并时被剔除）或由 step2 重写的字段，两侧取值本就不同、不构成「正文变了」。
	ignoredFields map[string]bool
}

// pendingAuthoringModes 按 step1 变体声明条目定位契约（对应 Python 的 _MODES）。
var pendingAuthoringModes = map[string]pendingAuthoringModeSpec{
	"drama": {
		step1Key: "scenes", scriptKey: "scenes", idField: "scene_id",
		ignoredFields: map[string]bool{"scene_description": true},
	},
	"narration": {
		step1Key: "segments", scriptKey: "segments", idField: "segment_id",
		ignoredFields: map[string]bool{},
	},
	// shots 正文由 step2 视觉展开改写（两侧必然不同）、source_text 落地时被剔除，
	// 剩下的 duration_seconds / references 才是 step1 定稿、step2 不动的契约。
	"reference_video": {
		step1Key: "units", scriptKey: "video_units", idField: "unit_id",
		ignoredFields: map[string]bool{"shots": true, "source_text": true},
	},
}

// PendingItem 是一个待编写条目：条目 id 与判定原因。
type PendingItem struct {
	ItemID string
	Reason string
}

// PendingAuthoringReport 是某集的待编写报告。
//
// Applicable 为 false 时该集不走结构化 step1（如 ad），Kind 为空、Items 为空。ScriptPresent 为
// false 时官方剧本尚未产出，此时不做逐条比对、Items 为空——「还没生成过剧本」与「剧本里的视觉
// 层过期」是两件事，前者不该被报成一整集的待编写。
type PendingAuthoringReport struct {
	Applicable    bool
	Kind          string
	ScriptPresent bool
	Items         []PendingItem
}

// HasPending 报告是否存在待编写条目。
func (r PendingAuthoringReport) HasPending() bool { return len(r.Items) > 0 }

// ItemIDs 返回全部待编写条目 id（保持判定顺序）。
func (r PendingAuthoringReport) ItemIDs() []string {
	ids := make([]string, 0, len(r.Items))
	for _, item := range r.Items {
		ids = append(ids, item.ItemID)
	}
	return ids
}

// ToDict 返回 API 响应形状（对应 Python 的 PendingAuthoringReport.to_dict）。
func (r PendingAuthoringReport) ToDict() map[string]any {
	var kind any
	if r.Kind != "" {
		kind = r.Kind
	}
	items := make([]map[string]string, 0, len(r.Items))
	for _, item := range r.Items {
		items = append(items, map[string]string{"item_id": item.ItemID, "reason": item.Reason})
	}
	return map[string]any{
		"applicable":     r.Applicable,
		"kind":           kind,
		"script_present": r.ScriptPresent,
		"has_pending":    r.HasPending(),
		"items":          items,
	}
}

// pendingAuthoringNotApplicable 返回不适用的空报告。
func pendingAuthoringNotApplicable() PendingAuthoringReport {
	return PendingAuthoringReport{Applicable: false, Items: []PendingItem{}}
}

// loadJSONMapOrNone 读 JSON 文件并要求顶层为对象；文件缺失 / 损坏 / 顶层非对象 / 读失败一律
// 返回 nil（对应 Python 的 load_json_or_none 在本模块的降级语义：只读提示路径不做 fail-loud，
// 读失败复用 quarantine.go 的 readJSONObjectOrNone 并吞掉非缺失类错误）。
func loadJSONMapOrNone(path string) map[string]any {
	data, _ := readJSONObjectOrNone(path)
	return data
}

// pendingAuthoringItemList 取列表键下的对象条目；无从比对时 ok=false，列表在场时返回其中的对象条目。
//
// 顶层非对象、键缺失、键不是列表（脏数据 / 损坏剧本）都返回 ok=false，而不是空列表：空列表会被
// 上层读成「另一侧一个条目都没有」，把损坏剧本报成整集孤儿，或把缺失基线报成整集待编写。元素非
// 对象一律跳过——这是只读提示，单个脏条目不值得让整条路径 fail-loud。
func pendingAuthoringItemList(payload map[string]any, key string) ([]map[string]any, bool) {
	if payload == nil {
		return nil, false
	}
	raw, ok := payload[key].([]any)
	if !ok {
		return nil, false
	}
	items := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if entry, ok := item.(map[string]any); ok {
			items = append(items, entry)
		}
	}
	return items, true
}

// pendingAuthoringIndexByID 按 id 建索引，跳过 id 缺失 / 非字符串 / 空的条目；重复 id 保留首个。
func pendingAuthoringIndexByID(items []map[string]any, idField string) map[string]map[string]any {
	indexed := make(map[string]map[string]any, len(items))
	for _, item := range items {
		itemID, ok := item[idField].(string)
		if !ok || itemID == "" {
			continue
		}
		if _, exists := indexed[itemID]; exists {
			continue
		}
		indexed[itemID] = item
	}
	return indexed
}

// pendingAuthoringContentProjection 是内容投影：step1 条目的非视觉、非豁免字段。
//
// 以 step1 声明过的键为契约（而非两侧键的并集）：官方剧本条目带的 note / transition_to_next /
// generated_assets 是 step1 没有的 UI / 运行时字段，取并集会把它们算成差异、让每个条目恒判待
// 编写。视觉层字段同样剔除——它们正是「是否已编写」的客体，不是正文。
func pendingAuthoringContentProjection(step1Item map[string]any, ignored map[string]bool) map[string]any {
	projection := make(map[string]any, len(step1Item))
	for key, value := range step1Item {
		if step1VisualFields[key] || ignored[key] {
			continue
		}
		projection[key] = value
	}
	return projection
}

// pendingAuthoringSameContent 判定两侧内容投影是否一致：对 step1 声明的键逐一取值，规范化后比对。
//
// 用 step1 的键在剧本条目上取值：剧本缺某个 step1 声明过的字段时取到 nil、与 step1 的实值不等，
// 判为不一致而非静默通过。规范化复用 ContentFingerprintOfData（键序 / 空白重排不改判定）。
func pendingAuthoringSameContent(step1Item, scriptItem map[string]any, ignored map[string]bool) bool {
	projection := pendingAuthoringContentProjection(step1Item, ignored)
	counterpart := make(map[string]any, len(projection))
	for key := range projection {
		counterpart[key] = scriptItem[key]
	}
	return ContentFingerprintOfData(projection) == ContentFingerprintOfData(counterpart)
}

// pendingAuthoringCompare 按 id 对齐两侧条目，产出待编写列表。
//
// 结果按 step1 顺序排列（剧本侧的孤儿条目随后），使同一份输入的输出稳定、便于前端逐条渲染。
func pendingAuthoringCompare(step1Items, scriptItems []map[string]any, spec pendingAuthoringModeSpec) []PendingItem {
	step1ByID := pendingAuthoringIndexByID(step1Items, spec.idField)
	scriptByID := pendingAuthoringIndexByID(scriptItems, spec.idField)

	pending := make([]PendingItem, 0, len(step1Items))
	for _, step1Item := range step1Items {
		itemID, ok := step1Item[spec.idField].(string)
		if !ok || itemID == "" {
			continue
		}
		scriptItem, exists := scriptByID[itemID]
		if !exists {
			pending = append(pending, PendingItem{ItemID: itemID, Reason: PendingReasonMissingVisual})
		} else if !pendingAuthoringSameContent(step1Item, scriptItem, spec.ignoredFields) {
			pending = append(pending, PendingItem{ItemID: itemID, Reason: PendingReasonStaleVisual})
		}
	}
	for _, scriptItem := range scriptItems {
		itemID, ok := scriptItem[spec.idField].(string)
		if !ok || itemID == "" {
			continue
		}
		if _, exists := step1ByID[itemID]; !exists {
			pending = append(pending, PendingItem{ItemID: itemID, Reason: PendingReasonOrphanItem})
		}
	}
	return pending
}

// PendingAuthoringReportFor 返回该集官方剧本的待编写报告（读时纯计算，不落盘、不改任何状态）。
//
// step1 文件缺失 / 损坏、或官方剧本不存在时退化为空列表（ScriptPresent 如实反映剧本在不在场）。
// episode 条目缺失时按默认剧本路径 scripts/episode_N.json 兜底，与 Step2Generated 同一口径。
func PendingAuthoringReportFor(projectDir string, pf *domain.ProjectFile, episode int) PendingAuthoringReport {
	kind := Step1Kind(pf)
	if kind == "" {
		return pendingAuthoringNotApplicable()
	}
	spec := pendingAuthoringModes[kind]

	var step1Payload map[string]any
	if path, ok := Step1Path(projectDir, pf, episode); ok {
		step1Payload = loadJSONMapOrNone(path)
	}

	scriptFile := ""
	if entry := FindEpisodeEntry(pf, episode); entry != nil {
		scriptFile = entry.ScriptFile
	}
	if scriptFile == "" {
		scriptFile = EpisodeScriptRelpath(episode)
	}
	scriptPresent := false
	var scriptPayload map[string]any
	if scriptFile != "" {
		resolved := filepath.Join(projectDir, filepath.FromSlash(scriptFile))
		if info, err := os.Stat(resolved); err == nil && info.Mode().IsRegular() {
			scriptPresent = true
			scriptPayload = loadJSONMapOrNone(resolved)
		}
	}

	if !scriptPresent {
		return PendingAuthoringReport{Applicable: true, Kind: kind, Items: []PendingItem{}}
	}

	step1Items, step1OK := pendingAuthoringItemList(step1Payload, spec.step1Key)
	scriptItems, scriptOK := pendingAuthoringItemList(scriptPayload, spec.scriptKey)
	if !step1OK || !scriptOK {
		// 一侧没有可比对的条目表（step1 缺失 / 损坏，或剧本列表键脏）：没有基线就不做判定，
		// 既不报整集待编写、也不报整集孤儿。
		return PendingAuthoringReport{Applicable: true, Kind: kind, ScriptPresent: true, Items: []PendingItem{}}
	}

	return PendingAuthoringReport{
		Applicable:    true,
		Kind:          kind,
		ScriptPresent: true,
		Items:         pendingAuthoringCompare(step1Items, scriptItems, spec),
	}
}
