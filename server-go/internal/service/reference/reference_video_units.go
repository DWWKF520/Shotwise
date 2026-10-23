package reference

import "github.com/shotwise/shotwise/internal/service"

// 参考生视频 unit 的 router 侧领域逻辑（对应 server/routers/reference_videos.py 的
// 集解析、锁内读-改-写与读时派生注入）。纯函数与锁语义收敛在此，transport 只做装配。

import (
	"fmt"
	"strings"

	"github.com/shotwise/shotwise/internal/domain/media"
	domainproject "github.com/shotwise/shotwise/internal/domain/project"
)

// UnregisteredReferencesError 表示 references 引用了未登记的资产（400 ref_not_registered）。
type UnregisteredReferencesError struct{ Missing []string }

func (e *UnregisteredReferencesError) Error() string {
	return "unregistered references: " + strings.Join(e.Missing, ", ")
}

// AdOnlyDeriveError 表示非 ad 项目调用了派生端点（409 ref_derive_ad_only）。
type AdOnlyDeriveError struct{}

func (e *AdOnlyDeriveError) Error() string { return "derive is only available for ad projects" }

// AdUnitsDerivedError 表示 ad 项目的手工增删改端点被调用（409 ref_ad_units_derived）。
type AdUnitsDerivedError struct{}

func (e *AdUnitsDerivedError) Error() string { return "ad units are derived from shots" }

// ReferenceUnitNotFoundError 表示目标 unit 不存在（404 ref_unit_not_found）。
type ReferenceUnitNotFoundError struct{ UnitID string }

func (e *ReferenceUnitNotFoundError) Error() string { return "video unit not found: " + e.UnitID }

// ReorderValidationError 表示 reorder 的 unit_ids 不是现有 id 集合的完整排列（400）。
//
// Kind ∈ length / duplicate / mismatch，transport 映射到各自的 i18n key。
type ReorderValidationError struct{ Kind string }

func (e *ReorderValidationError) Error() string { return "unit_ids validation failed: " + e.Kind }

// assetKindForRefType 把参考类型映射到资产种类。
func assetKindForRefType(refType string) (domainproject.AssetKind, bool) {
	switch refType {
	case "character":
		return domainproject.AssetCharacter, true
	case "scene":
		return domainproject.AssetScene, true
	case "prop":
		return domainproject.AssetProp, true
	}
	return "", false
}

// UnregisteredReferences 返回 references 中未在 project.json 对应资产桶登记的条目（"type:name"）。
func UnregisteredReferences(project *domainproject.ProjectFile, refs []media.AdReference) []string {
	var missing []string
	for _, ref := range refs {
		kind, ok := assetKindForRefType(ref.Type)
		if !ok {
			missing = append(missing, ref.Type+":"+ref.Name)
			continue
		}
		bucket, ok := project.BucketRef(kind)
		if !ok {
			missing = append(missing, ref.Type+":"+ref.Name)
			continue
		}
		if _, found := bucket.ResolveKey(domainproject.NormalizeAssetName(ref.Name)); !found {
			missing = append(missing, ref.Type+":"+ref.Name)
		}
	}
	return missing
}

// requireAdProject 守卫端点适用的项目类型：ad 的 unit 是 shots 的派生索引（手工增删改重排走不通），
// narration/drama 的 unit 内容自包含（派生走不通），互斥拒绝。
func requireAdProject(project *domainproject.ProjectFile, required bool) error {
	isAd := project.ContentMode == string(domainproject.ContentAd)
	if required && !isAd {
		return &AdOnlyDeriveError{}
	}
	if !required && isAd {
		return &AdUnitsDerivedError{}
	}
	return nil
}

// EpisodeScriptResolverFor 构造解析器：从 project.json 解析并校验指定集，返回其 script_file。
//
// 校验顺序与 Python _episode_script_resolver 一致：可选 ad 守卫 → 找集 → 参考模式 → 可选引用存在性。
// requireAd 为 nil 时跳过 ad 守卫；refs 为 nil 时跳过引用存在性校验。
func EpisodeScriptResolverFor(episode int, requireAd *bool, refs []media.AdReference) service.EpisodeScriptResolver {
	return func(project *domainproject.ProjectFile) (string, error) {
		if requireAd != nil {
			if err := requireAdProject(project, *requireAd); err != nil {
				return "", err
			}
		}
		scriptFile, err := service.EpisodeScriptFile(project, episode)
		if err != nil {
			return "", err
		}
		if !project.IsReferenceVideo() {
			return "", &service.NotReferenceVideoModeError{}
		}
		if refs != nil {
			if missing := UnregisteredReferences(project, refs); len(missing) > 0 {
				return "", &UnregisteredReferencesError{Missing: missing}
			}
		}
		return scriptFile, nil
	}
}

// AnnotateAdUnitStaleness 给对外透出的 unit 列表注入读时派生的 stale（返回浅拷贝，不落盘）。
//
// 剥除剧本残留的 stale 键；偏离的 unit 仅在返回副本上携带 stale: true。脏条目（非对象）原样透传。
func AnnotateAdUnitStaleness(script map[string]any, units any) []any {
	list, ok := units.([]any)
	if !ok {
		return []any{}
	}
	byID := media.AdShotsByID(script)
	out := make([]any, 0, len(list))
	for _, raw := range list {
		unit, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		entry := make(map[string]any, len(unit))
		for k, v := range unit {
			if k == "stale" {
				continue
			}
			entry[k] = v
		}
		if media.AdUnitStaleByID(byID, unit) {
			entry["stale"] = true
		}
		out = append(out, entry)
	}
	return out
}

// FullPermutationError 判定 requested 是否为 existing 的完整排列；不满足时返回错误种类。
//
// 返回 "length"（数量不符）/ "duplicate"（有重复）/ "mismatch"（集合不一致）/ ""（合法）。
func FullPermutationError(existing, requested []string) string {
	if len(existing) != len(requested) {
		return "length"
	}
	seen := make(map[string]bool, len(requested))
	for _, id := range requested {
		if seen[id] {
			return "duplicate"
		}
		seen[id] = true
	}
	for _, id := range existing {
		if !seen[id] {
			return "mismatch"
		}
	}
	return ""
}

// UnitIDs 取 unit 列表里的 unit_id 序列（非对象或缺 id 的条目按空串计入，与 Python 口径一致）。
func UnitIDs(units []any) []string {
	out := make([]string, 0, len(units))
	for _, raw := range units {
		unit, ok := raw.(map[string]any)
		if !ok {
			out = append(out, "")
			continue
		}
		id, _ := unit["unit_id"].(string)
		out = append(out, id)
	}
	return out
}

// FindUnit 在 video_units 中定位 unit_id；未找到返回 ReferenceUnitNotFoundError。
func FindUnit(script map[string]any, unitID string) (map[string]any, error) {
	return FindUnitInKey(script, "video_units", unitID)
}

// FindAdUnit 在 reference_units 派生索引中定位 unit_id。
func FindAdUnit(script map[string]any, unitID string) (map[string]any, error) {
	return FindUnitInKey(script, "reference_units", unitID)
}

// FindUnitInKey 在指定键的 unit 列表中定位 unit_id。
func FindUnitInKey(script map[string]any, key, unitID string) (map[string]any, error) {
	units, _ := script[key].([]any)
	for _, raw := range units {
		unit, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := unit["unit_id"].(string); id == unitID {
			return unit, nil
		}
	}
	return nil, &ReferenceUnitNotFoundError{UnitID: unitID}
}

// FindUnitForProject 按项目内容模式选 unit 所在列表：ad 在 reference_units 派生索引，其余在 video_units。
func FindUnitForProject(project *domainproject.ProjectFile, script map[string]any, unitID string) (map[string]any, error) {
	if project.ContentMode == string(domainproject.ContentAd) {
		return FindAdUnit(script, unitID)
	}
	return FindUnit(script, unitID)
}

// NextUnitID 生成下一个可用的 unit_id（E{episode}U{n}，基于现有 video_units 序号递增）。
func NextUnitID(script map[string]any, episode int) string {
	existing := map[string]bool{}
	if units, ok := script["video_units"].([]any); ok {
		for _, raw := range units {
			unit, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := unit["unit_id"].(string); id != "" {
				existing[id] = true
			}
		}
	}
	for idx := 1; ; idx++ {
		candidate := fmt.Sprintf("E%dU%d", episode, idx)
		if !existing[candidate] {
			return candidate
		}
	}
}

// NewUnitGeneratedAssets 返回新建 unit 的 produced-assets 容器（与 Python _build_unit_dict 同形，
// 只含 7 个键——不套用 EmptyGeneratedAssets 的完整字段集，避免落盘结构与 Python 分叉）。
func NewUnitGeneratedAssets() map[string]any {
	return map[string]any{
		"storyboard_image":      nil,
		"storyboard_last_image": nil,
		"grid_id":               nil,
		"grid_cell_index":       nil,
		"video_clip":            nil,
		"video_uri":             nil,
		"status":                "pending",
	}
}

// BuildReferenceUnit 构造一个新 unit（对应 Python _build_unit_dict）：shots 由 prompt 书写层派生。
func BuildReferenceUnit(
	unitID, prompt string, refs []media.AdReference, durationSeconds int, transition string, note *string,
) map[string]any {
	shots, _ := parsePrompt(prompt)
	shotsAny := make([]any, 0, len(shots))
	for _, shot := range shots {
		shotsAny = append(shotsAny, map[string]any{"text": shot["text"]})
	}
	refList := make([]any, 0, len(refs))
	for _, ref := range refs {
		refList = append(refList, map[string]any{"type": ref.Type, "name": ref.Name})
	}
	var noteValue any
	if note != nil {
		noteValue = *note
	}
	return map[string]any{
		"unit_id":            unitID,
		"shots":              shotsAny,
		"references":         refList,
		"duration_seconds":   durationSeconds,
		"transition_to_next": transition,
		"note":               noteValue,
		"generated_assets":   NewUnitGeneratedAssets(),
	}
}

// ToAnyList 把 map 切片转成 []any（供需要弱类型 JSON 形态的调用方使用）。
func ToAnyList(in []map[string]any) []any {
	out := make([]any, 0, len(in))
	for _, item := range in {
		out = append(out, item)
	}
	return out
}

// ParsePrompt 导出书写层解析（parsePrompt），供 transport 复用为 unit 的 shots 派生入口。
func ParsePrompt(text string) ([]map[string]any, []string) { return parsePrompt(text) }
