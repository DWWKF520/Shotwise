// 参考生视频入队前的引用校验（对应 lib/generation_preflight.py 的 reference / ad 面与
// enqueue_videos.py 的 _preflight_reference_units）。
//
// 只覆盖三类待检查资产（character / scene / prop）；ad 的参考集从成员镜头（内容唯一真相）现算，
// 索引里持久化的 references 只是展示缓存。任一 unit 引用无效即整批阻断，不留半批已入队状态。
package reference

import (
	"github.com/shotwise/shotwise/internal/service"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
)

// ValidateReferenceUnitsPreflight 对将入队的 unit 逐个校验引用资产，返回格式化错误串
// （空串 = 通过）。script 非 nil 时按 ad 成员镜头现算参考集（索引悬空时报错），否则按 unit
// 声明的 references。skipIDs 内的 unit 已产出、不入队，不参与校验。
func ValidateReferenceUnitsPreflight(
	project *domain.ProjectFile, projectDir string, units []map[string]any, script map[string]any, skipIDs map[string]bool,
) (string, error) {
	if project == nil {
		return "", nil
	}
	var results []service.PreflightResult
	for _, unit := range units {
		unitID, _ := unit["unit_id"].(string)
		if skipIDs[unitID] {
			continue
		}
		var references []service.ItemReference
		if script != nil {
			shots, err := domain.ResolveAdUnitShots(script, unit)
			if err != nil {
				return "", err
			}
			references = collectAdShotReferences(shots)
		} else {
			references = collectReferenceEntries(unit["references"])
		}
		results = append(results, service.ValidateAssetReferences(project, projectDir, references))
	}
	return service.FormatPreflightError(service.MergePreflightResults(results...)), nil
}

// collectReferenceEntries 从 “[{"type":..,"name":..}]“ 数据收集类型化引用（按类型+归一名去重）。
func collectReferenceEntries(entries any) []service.ItemReference {
	list, ok := entries.([]any)
	if !ok {
		return nil
	}
	var references []service.ItemReference
	seen := map[string]bool{}
	for _, raw := range list {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		assetType, _ := entry["type"].(string)
		name, _ := entry["name"].(string)
		if assetType == "" || strings.TrimSpace(name) == "" {
			continue
		}
		key := assetType + "\x00" + domain.NormalizeAssetName(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		references = append(references, service.ItemReference{
			Kind:  domain.AssetKind(assetType),
			Field: "references",
			Name:  domain.NormalizeAssetName(name),
		})
	}
	return references
}

// collectAdShotReferences 从 ad 成员镜头的字段（内容唯一真相）收集类型化引用，不读索引缓存。
func collectAdShotReferences(shots []map[string]any) []service.ItemReference {
	fields := []struct {
		kind  domain.AssetKind
		field string
	}{
		{domain.AssetProduct, "products_in_shot"},
		{domain.AssetCharacter, "characters_in_shot"},
		{domain.AssetScene, "scenes"},
		{domain.AssetProp, "props"},
	}
	var references []service.ItemReference
	seen := map[string]bool{}
	for _, shot := range shots {
		for _, f := range fields {
			names, ok := shot[f.field].([]any)
			if !ok {
				continue
			}
			for _, raw := range names {
				value, ok := raw.(string)
				if !ok || strings.TrimSpace(value) == "" {
					continue
				}
				name := domain.NormalizeAssetName(value)
				key := string(f.kind) + "\x00" + name
				if seen[key] {
					continue
				}
				seen[key] = true
				references = append(references, service.ItemReference{Kind: f.kind, Field: f.field, Name: name})
			}
		}
	}
	return references
}
