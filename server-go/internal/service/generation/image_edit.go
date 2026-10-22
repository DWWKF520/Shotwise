// 图片指令式编辑（image edit）的资源解析口径（对应 server/services/image_edit_tasks.py 的
// EDITABLE_RESOURCE_TYPES / edit_version_resource_type / resolve_current_image_rel）。
//
// 同一份口径同时供工具层（入队前校验待编辑项是否真有当前图）与执行层（读图）使用，
// 两个入口不各自维护一份解析，避免「工具说可编辑、执行时找不到图」的口径分叉。
package generation

import (
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
)

// StoryboardResourceType 是分镜图的对外资源类型标识（不属于 ASSET_SPECS 四类）。
const StoryboardResourceType = "storyboard"

// EditableResourceTypes 是可做指令式编辑的资源类型，顺序与 ASSET_SPECS 一致（对应 Python
// EDITABLE_RESOURCE_TYPES）：四类设计图 + 分镜图。
var EditableResourceTypes = append(append([]string{}, assetTypeNames()...), StoryboardResourceType)

// assetTypeNames 按 AssetTypeOrder 列出四类资产类型标识。
func assetTypeNames() []string {
	out := make([]string, 0, len(domain.AssetTypeOrder))
	for _, kind := range domain.AssetTypeOrder {
		out = append(out, string(kind))
	}
	return out
}

// IsEditableResourceType 判定资源类型是否属于可编辑白名单。
func IsEditableResourceType(resourceType string) bool {
	for _, allowed := range EditableResourceTypes {
		if resourceType == allowed {
			return true
		}
	}
	return false
}

// EditVersionResourceType 把编辑契约的单数 resource_type 映射为版本/事件层的复数资源类型
// （对应 Python edit_version_resource_type）；未登记类型返回 ok=false。
func EditVersionResourceType(resourceType string) (string, bool) {
	if resourceType == StoryboardResourceType {
		return "storyboards", true
	}
	spec, ok := domain.Spec(domain.AssetKind(resourceType))
	if !ok {
		return "", false
	}
	return spec.BucketKey, true
}

// ResolveCurrentImageRel 解析资源当前图的项目相对路径（不校验文件存在，交由调用方判定）。
//
//   - 资产（character / scene / prop / product）：读 project.json 对应 bucket 的 sheet 字段；
//     资产不存在返回 found=false（对应 Python KeyError），sheet 未设置返回 ("", true)。
//   - storyboard：读剧本条目的 generated_assets.storyboard_image，缺失回退 canonical 路径
//     storyboards/scene_{id}.png；条目不存在返回 found=false（对应 Python KeyError）。
//
// found=false 与 rel 为空是两种语义：前者表示「资源不存在」，后者表示「资源在但没有当前图」，
// 调用方按 Python 侧的两条分支分别告警。
func ResolveCurrentImageRel(
	pf *domain.ProjectFile, resourceType, resourceID string, script map[string]any,
) (string, bool) {
	if resourceType == StoryboardResourceType {
		items, idField, _ := domain.StoryboardItems(script)
		item, _, ok := domain.FindStoryboardItem(items, idField, resourceID)
		if !ok {
			return "", false
		}
		assets, _ := item["generated_assets"].(map[string]any)
		if pointer, ok := assets["storyboard_image"].(string); ok && pointer != "" {
			return pointer, true
		}
		canonical, err := service.ResourceRelativePath("storyboards", resourceID)
		if err != nil {
			return "", true
		}
		return canonical, true
	}

	spec, ok := domain.Spec(domain.AssetKind(resourceType))
	if !ok {
		return "", false
	}
	bucket, ok := pf.BucketRef(domain.AssetKind(resourceType))
	if !ok || bucket == nil {
		return "", false
	}
	// 请求里的资产名与桶 key 可能是 NFC/NFD 中的任一形态，按比对坐标系解析真实落盘 key。
	key, ok := bucket.ResolveKey(resourceID)
	if !ok {
		return "", false
	}
	entry := (*bucket)[key]
	if entry == nil {
		return "", false
	}
	sheet := entry.MustString(spec.SheetKey)
	if sheet == "" {
		return "", true
	}
	return sheet, true
}
