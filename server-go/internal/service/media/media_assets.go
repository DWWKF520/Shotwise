// 可选 MediaAsset 索引的应用缝（对应 server/services/media_assets.py）。
//
// 本服务只把持久化委托给 media_catalog，绝不改变既有的上传 / 生成 / 公开文件 URL /
// project.json 路径契约。索引本身在 SHOTWISE_MEDIA_ASSET_INDEX 关闭时是空操作。
package service

import (
	"path/filepath"
	"strings"
)

// MediaAssetNotFoundError 表示索引里没有这条媒体资产（对应 Python 的 KeyError）。
type MediaAssetNotFoundError struct{ MediaAssetID string }

func (e *MediaAssetNotFoundError) Error() string { return e.MediaAssetID }

// RegisterMediaAssetParams 是 RegisterMediaAsset 的入参。
type RegisterMediaAssetParams struct {
	ProjectID       string
	ProjectRoot     string
	RelativePath    string
	Origin          string
	WorkflowRunID   *string
	WorkflowNodeKey *string
	ProviderID      *string
	ModelID         *string
	PromptSnapshot  *string
	OriginalName    *string
}

// RegisterMediaAsset 索引一个新上传或生成的文件而不改其遗留路径。
//
// 索引在 SHOTWISE_MEDIA_ASSET_INDEX 关闭时是空操作，返回 (nil, nil)。
func RegisterMediaAsset(p RegisterMediaAssetParams) (*MediaAsset, error) {
	if !MediaIndexEnabled() {
		return nil, nil
	}
	if filepath.IsAbs(p.RelativePath) || containsDotDot(p.RelativePath) {
		return nil, &ValueError{Msg: "relative_path must stay inside the project root"}
	}
	path := filepath.Join(p.ProjectRoot, filepath.FromSlash(p.RelativePath))
	return ProjectMediaCatalog(p.ProjectRoot).Register(catalogRegisterParams{
		ProjectID:       p.ProjectID,
		Path:            path,
		Origin:          p.Origin,
		WorkflowRunID:   p.WorkflowRunID,
		WorkflowNodeKey: p.WorkflowNodeKey,
		ProviderID:      p.ProviderID,
		ModelID:         p.ModelID,
		PromptSnapshot:  p.PromptSnapshot,
		OriginalName:    p.OriginalName,
	})
}

// ValueError 对应 Python 的 ValueError（如非法相对路径）。
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// ListProjectMediaAssetsParams 是 ListProjectMediaAssets 的过滤条件。
type ListProjectMediaAssetsParams struct {
	ProjectID     *string
	ProjectRoot   string
	Kind          *string
	Origin        *string
	WorkflowRunID *string
	BindingKind   *string
	TargetID      *string
	Purpose       *string
	Archived      *bool
}

// ListProjectMediaAssets 把项目媒体库返回为语义索引数据（对应 list_project_media_assets）。
func ListProjectMediaAssets(p ListProjectMediaAssetsParams) (map[string]any, error) {
	catalog := ProjectMediaCatalog(p.ProjectRoot)
	assets, err := catalog.ListAssets(ListMediaAssetsParams{
		ProjectID:     p.ProjectID,
		Kind:          p.Kind,
		Origin:        p.Origin,
		WorkflowRunID: p.WorkflowRunID,
		Archived:      p.Archived,
	})
	if err != nil {
		return nil, err
	}
	items := []map[string]any{}
	for _, asset := range assets {
		bindings, err := catalog.BindingsFor(asset.ID)
		if err != nil {
			return nil, err
		}
		if p.BindingKind != nil {
			filtered := bindings[:0]
			for _, binding := range bindings {
				if binding.BindingKind == *p.BindingKind {
					filtered = append(filtered, binding)
				}
			}
			bindings = filtered
			if len(bindings) == 0 {
				continue
			}
		}
		if p.TargetID != nil {
			filtered := bindings[:0]
			for _, binding := range bindings {
				if binding.TargetID != nil && *binding.TargetID == *p.TargetID {
					filtered = append(filtered, binding)
				}
			}
			bindings = filtered
			if len(bindings) == 0 {
				continue
			}
		}
		if p.Purpose != nil {
			filtered := bindings[:0]
			for _, binding := range bindings {
				if binding.Purpose == *p.Purpose {
					filtered = append(filtered, binding)
				}
			}
			bindings = filtered
			if len(bindings) == 0 {
				continue
			}
		}
		derivations, err := catalog.DerivationsFor(asset.ID)
		if err != nil {
			return nil, err
		}
		item := mediaAssetMap(asset)
		bindingMaps := make([]map[string]any, 0, len(bindings))
		for _, binding := range bindings {
			bindingMaps = append(bindingMaps, mediaBindingMap(binding))
		}
		derivationMaps := make([]map[string]any, 0, len(derivations))
		for _, derivation := range derivations {
			derivationMaps = append(derivationMaps, mediaDerivationMap(derivation))
		}
		item["bindings"] = bindingMaps
		item["derivations"] = derivationMaps
		items = append(items, item)
	}
	return map[string]any{"items": items, "count": len(items)}, nil
}

// GetProjectMediaSummary 返回不触盘的持久索引摘要（对应 get_project_media_summary）。
func GetProjectMediaSummary(projectRoot string) (map[string]any, error) {
	summary, err := ProjectMediaCatalog(projectRoot).Summary()
	if err != nil {
		return nil, err
	}
	return mediaSummaryMap(summary), nil
}

// GetProjectMediaAsset 返回单条资产及其绑定、派生与相关修复项。
func GetProjectMediaAsset(projectRoot, mediaAssetID string) (map[string]any, error) {
	catalog := ProjectMediaCatalog(projectRoot)
	asset, err := catalog.Get(mediaAssetID)
	if err != nil {
		return nil, err
	}
	if asset == nil {
		return nil, &MediaAssetNotFoundError{MediaAssetID: mediaAssetID}
	}
	bindings, err := catalog.BindingsFor(mediaAssetID)
	if err != nil {
		return nil, err
	}
	derivations, err := catalog.DerivationsFor(mediaAssetID)
	if err != nil {
		return nil, err
	}
	reconciliationItems, err := catalog.ReconciliationItems()
	if err != nil {
		return nil, err
	}
	result := mediaAssetMap(*asset)
	bindingMaps := make([]map[string]any, 0, len(bindings))
	for _, binding := range bindings {
		bindingMaps = append(bindingMaps, mediaBindingMap(binding))
	}
	derivationMaps := make([]map[string]any, 0, len(derivations))
	for _, derivation := range derivations {
		derivationMaps = append(derivationMaps, mediaDerivationMap(derivation))
	}
	reconciliationMaps := []map[string]any{}
	for _, item := range reconciliationItems {
		if (item.MediaAssetID != nil && *item.MediaAssetID == mediaAssetID) || item.RelativePath == asset.PhysicalPath {
			reconciliationMaps = append(reconciliationMaps, toJSONMap(item))
		}
	}
	result["bindings"] = bindingMaps
	result["derivations"] = derivationMaps
	result["reconciliation"] = reconciliationMaps
	return result, nil
}

// BindProjectMediaAsset 给资产加一条绑定（对应 bind_project_media_asset）。
func BindProjectMediaAsset(projectRoot, projectID, mediaAssetID, bindingKind string, targetID *string, purpose string) (map[string]any, error) {
	binding, err := ProjectMediaCatalog(projectRoot).Bind(mediaAssetID, projectID, bindingKind, targetID, purpose)
	if err != nil {
		if strings.HasPrefix(err.Error(), "unknown MediaAsset") {
			return nil, &MediaAssetNotFoundError{MediaAssetID: mediaAssetID}
		}
		return nil, err
	}
	return mediaBindingMap(binding), nil
}

// ArchiveProjectMediaAsset 设置资产归档标记（对应 archive_project_media_asset）。
func ArchiveProjectMediaAsset(projectRoot, mediaAssetID string, archived bool) (map[string]any, error) {
	asset, err := ProjectMediaCatalog(projectRoot).SetArchived(mediaAssetID, archived)
	if err != nil {
		if strings.HasPrefix(err.Error(), "unknown MediaAsset") {
			return nil, &MediaAssetNotFoundError{MediaAssetID: mediaAssetID}
		}
		return nil, err
	}
	return mediaAssetMap(asset), nil
}

// DeleteProjectMediaAsset 只删除索引行；物理媒体文件保持不动（对应 delete_project_media_asset）。
func DeleteProjectMediaAsset(projectRoot, mediaAssetID string) (map[string]any, error) {
	catalog := ProjectMediaCatalog(projectRoot)
	asset, err := catalog.Get(mediaAssetID)
	if err != nil {
		return nil, err
	}
	if asset == nil {
		return nil, &MediaAssetNotFoundError{MediaAssetID: mediaAssetID}
	}
	legacyReferences, inspectionErrors := legacyMediaAssetReferences(projectRoot, asset.PhysicalPath)
	if len(inspectionErrors) > 0 {
		refs := make([]string, 0, len(inspectionErrors))
		for _, item := range inspectionErrors {
			refs = append(refs, "legacy_data_unreadable:"+item)
		}
		return nil, &MediaAssetReferencedError{MediaAssetID: asset.ID, References: refs}
	}
	if len(legacyReferences) > 0 {
		refs := make([]string, 0, len(legacyReferences))
		for _, item := range legacyReferences {
			refs = append(refs, "legacy_reference:"+item)
		}
		return nil, &MediaAssetReferencedError{MediaAssetID: asset.ID, References: refs}
	}
	deleted, err := catalog.Delete(mediaAssetID)
	if err != nil {
		if strings.HasPrefix(err.Error(), "unknown MediaAsset") {
			return nil, &MediaAssetNotFoundError{MediaAssetID: mediaAssetID}
		}
		return nil, err
	}
	return map[string]any{
		"deleted":        true,
		"media_asset_id": deleted.ID,
		"physical_path":  deleted.PhysicalPath,
	}, nil
}
