// 把遗留项目媒体路径回填进可选的 MediaAsset 索引（对应 server/services/media_indexing.py）。
package service

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// mediaDirectoryNames 是扫描时用于识别预期媒体目录（出现不支持文件即告警）。
var mediaDirectoryNames = map[string]bool{
	"assets": true, "audio": true, "characters": true, "edited": true, "episodes": true,
	"extracted": true, "frames": true, "generated": true, "media": true, "products": true,
	"props": true, "reference_videos": true, "renders": true, "scenes": true, "shots": true,
	"storyboards": true, "uploads": true, "videos": true, "voiceovers": true,
}

// scanIgnoredDirs 是扫描时跳过的目录名。
var scanIgnoredDirs = map[string]bool{".git": true, "node_modules": true, ".venv": true, "__pycache__": true}

// collectStringValues 递归收集 JSON 文档里的字符串（对应 _string_values）。
func collectStringValues(value any, out *[]string) {
	switch v := value.(type) {
	case string:
		*out = append(*out, v)
	case map[string]any:
		for _, nested := range v {
			collectStringValues(nested, out)
		}
	case []any:
		for _, nested := range v {
			collectStringValues(nested, out)
		}
	}
}

// projectJSONPaths 返回 project.json + scripts/*.json（对应 _project_json_paths）。
func projectJSONPaths(projectRoot string) []string {
	candidates := []string{filepath.Join(projectRoot, "project.json")}
	scriptsDir := filepath.Join(projectRoot, "scripts")
	entries, err := os.ReadDir(scriptsDir)
	if err == nil {
		names := []string{}
		for _, entry := range entries {
			if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
				names = append(names, entry.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			candidates = append(candidates, filepath.Join(scriptsDir, name))
		}
	}
	return candidates
}

// legacyMediaReferenceReport 收集媒体引用并保留清单的读取/解析失败（对应 legacy_media_reference_report）。
func legacyMediaReferenceReport(projectRoot string) ([]string, []string) {
	seen := map[string]bool{}
	references := []string{}
	errors := []string{}
	for _, candidate := range projectJSONPaths(projectRoot) {
		info, statErr := os.Stat(candidate)
		if statErr != nil || info.IsDir() {
			continue
		}
		raw, readErr := os.ReadFile(candidate)
		if readErr != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", candidate, readErr))
			continue
		}
		var decoded any
		if jsonErr := json.Unmarshal(raw, &decoded); jsonErr != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", candidate, jsonErr))
			continue
		}
		values := []string{}
		collectStringValues(decoded, &values)
		for _, value := range values {
			if ClassifyMediaPath(value) != "" && !seen[value] {
				seen[value] = true
				references = append(references, value)
			}
		}
	}
	return references, errors
}

// legacyMediaReferences 只收集遗留文档里的相对媒体路径（对应 legacy_media_references）。
func legacyMediaReferences(projectRoot string) []string {
	references, _ := legacyMediaReferenceReport(projectRoot)
	return references
}

// legacyMediaAssetReferences 查找指向某物理资产的遗留引用；出错即报错不猜测。
func legacyMediaAssetReferences(projectRoot, physicalPath string) ([]string, []string) {
	references, errors := legacyMediaReferenceReport(projectRoot)
	target := canonicalMediaPath(physicalPath)
	matches := []string{}
	for _, reference := range references {
		if filepath.IsAbs(reference) {
			if canonicalMediaPath(reference) == target {
				matches = append(matches, reference)
			}
			continue
		}
		if containsDotDot(reference) {
			errors = append(errors, fmt.Sprintf("reference %s: path escapes project root", reference))
			continue
		}
		if canonicalMediaPath(filepath.Join(projectRoot, filepath.FromSlash(reference))) == target {
			matches = append(matches, reference)
		}
	}
	return matches, errors
}

// BackfillProjectMediaAssets 默认关闭地回填遗留引用（对应 backfill_project_media_assets）。
func BackfillProjectMediaAssets(projectID, projectRoot string) ([]MediaAsset, error) {
	if !MediaIndexEnabled() {
		return []MediaAsset{}, nil
	}
	catalog := ProjectMediaCatalog(projectRoot)
	references, manifestErrors := legacyMediaReferenceReport(projectRoot)
	for _, detail := range manifestErrors {
		pathPart := detail
		if idx := strings.Index(detail, ":"); idx >= 0 {
			pathPart = detail[:idx]
		}
		if err := catalog.RecordDiagnostic(projectID, pathPart, "unreadable_manifest", detail); err != nil {
			return nil, err
		}
	}
	return catalog.Backfill(projectID, projectRoot, references)
}

// originForPath 按路径片段推断媒体来源（对应 _origin_for_path）。
func originForPath(relativePath string) string {
	parts := map[string]bool{}
	for _, part := range strings.Split(filepath.ToSlash(relativePath), "/") {
		parts[strings.ToLower(part)] = true
	}
	switch {
	case parts["uploads"]:
		return MediaOriginUpload
	case parts["edited"]:
		return MediaOriginEdited
	case parts["extracted"] || parts["frames"]:
		return MediaOriginExtracted
	case parts["generated"] || parts["renders"] || parts["storyboards"]:
		return MediaOriginGenerated
	default:
		return MediaOriginImported
	}
}

// scanPaths 返回受支持的引用与媒体目录内未被识别的文件（对应 _scan_paths）。
func scanPaths(projectRoot string) ([]string, []string, error) {
	supportedSet := map[string]bool{}
	for _, reference := range legacyMediaReferences(projectRoot) {
		supportedSet[reference] = true
	}
	unsupportedSet := map[string]bool{}
	walkErr := filepath.WalkDir(projectRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		relative, relErr := filepath.Rel(projectRoot, path)
		if relErr != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		for _, part := range parts[:len(parts)-1] {
			if scanIgnoredDirs[part] {
				return nil
			}
		}
		relativePosix := filepath.ToSlash(relative)
		if ClassifyMediaPath(path) != "" {
			supportedSet[relativePosix] = true
			return nil
		}
		for _, part := range parts[:len(parts)-1] {
			if mediaDirectoryNames[strings.ToLower(part)] {
				unsupportedSet[relativePosix] = true
				break
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	supported := make([]string, 0, len(supportedSet))
	for key := range supportedSet {
		supported = append(supported, key)
	}
	sort.Strings(supported)
	unsupported := make([]string, 0, len(unsupportedSet))
	for key := range unsupportedSet {
		unsupported = append(unsupported, key)
	}
	sort.Strings(unsupported)
	return supported, unsupported, nil
}

// AuditProjectMediaAssets 只检查候选而不写索引、不改文件（对应 audit_project_media_assets）。
func AuditProjectMediaAssets(projectID, projectRoot string) (map[string]any, error) {
	supported, unsupported, err := scanPaths(projectRoot)
	if err != nil {
		return nil, err
	}
	_, manifestErrors := legacyMediaReferenceReport(projectRoot)
	diagnostics := []map[string]any{}
	for _, item := range manifestErrors {
		pathPart := item
		if idx := strings.Index(item, ":"); idx >= 0 {
			pathPart = item[:idx]
		}
		diagnostics = append(diagnostics, map[string]any{
			"code": "unreadable_manifest", "path": pathPart, "detail": item,
		})
	}
	unionSet := map[string]bool{}
	for _, p := range supported {
		unionSet[p] = true
	}
	for _, p := range unsupported {
		unionSet[p] = true
	}
	union := make([]string, 0, len(unionSet))
	for key := range unionSet {
		union = append(union, key)
	}
	sort.Strings(union)
	validPaths := []string{}
	for _, relativePath := range union {
		if filepath.IsAbs(relativePath) || containsDotDot(relativePath) {
			diagnostics = append(diagnostics, map[string]any{
				"code": "invalid_reference", "path": relativePath, "detail": "path escapes project root",
			})
			continue
		}
		if ClassifyMediaPath(relativePath) == "" {
			diagnostics = append(diagnostics, map[string]any{
				"code": "unsupported_media", "path": relativePath, "detail": "extension is not indexed",
			})
			continue
		}
		path := filepath.Join(projectRoot, filepath.FromSlash(relativePath))
		info, statErr := os.Stat(path)
		if statErr != nil || info.IsDir() {
			diagnostics = append(diagnostics, map[string]any{
				"code": "missing_file", "path": relativePath, "detail": "referenced file does not exist",
			})
			continue
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			diagnostics = append(diagnostics, map[string]any{
				"code": "unreadable_file", "path": relativePath, "detail": openErr.Error(),
			})
			continue
		}
		buf := make([]byte, 1)
		_, readErr := f.Read(buf)
		f.Close()
		if readErr != nil && readErr.Error() != "EOF" {
			diagnostics = append(diagnostics, map[string]any{
				"code": "unreadable_file", "path": relativePath, "detail": readErr.Error(),
			})
			continue
		}
		validPaths = append(validPaths, relativePath)
	}
	reconciliationItems, err := ProjectMediaCatalog(projectRoot).ReconciliationItems()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"enabled":           MediaIndexEnabled(),
		"dry_run":           true,
		"project_id":        projectID,
		"project_root":      projectRoot,
		"scanned_paths":     len(supported) + len(unsupported),
		"would_index_count": len(validPaths),
		"valid_paths":       validPaths,
		"diagnostics":       diagnostics,
		"reconciliation":    mediaReconciliationMaps(reconciliationItems),
	}, nil
}

// ScanProjectMediaAssets 幂等地扫描已知媒体位置与遗留 JSON 引用（对应 scan_project_media_assets）。
func ScanProjectMediaAssets(projectID, projectRoot string, dryRun bool) (map[string]any, error) {
	if dryRun {
		return AuditProjectMediaAssets(projectID, projectRoot)
	}
	if !MediaIndexEnabled() {
		return map[string]any{
			"enabled": false, "project_id": projectID, "project_root": projectRoot,
			"scanned_paths": 0, "indexed_count": 0, "asset_count": 0,
			"diagnostics": []any{}, "reconciliation": []any{},
		}, nil
	}
	catalog := ProjectMediaCatalog(projectRoot)
	if _, err := catalog.MarkSyncing(); err != nil {
		return nil, err
	}
	_, manifestErrors := legacyMediaReferenceReport(projectRoot)
	for _, detail := range manifestErrors {
		pathPart := detail
		if idx := strings.Index(detail, ":"); idx >= 0 {
			pathPart = detail[:idx]
		}
		if err := catalog.RecordDiagnostic(projectID, pathPart, "unreadable_manifest", detail); err != nil {
			return nil, err
		}
	}
	supported, unsupported, err := scanPaths(projectRoot)
	if err != nil {
		if _, markErr := catalog.MarkFailed(err.Error()); markErr != nil {
			return nil, markErr
		}
		return nil, err
	}
	indexed := []MediaAsset{}
	for _, relativePath := range supported {
		asset, regErr := catalog.Register(catalogRegisterParams{
			ProjectID: projectID, Path: filepath.Join(projectRoot, filepath.FromSlash(relativePath)),
			Origin: originForPath(relativePath),
		})
		if regErr != nil {
			if _, markErr := catalog.MarkFailed(regErr.Error()); markErr != nil {
				return nil, markErr
			}
			return nil, regErr
		}
		if asset != nil {
			indexed = append(indexed, *asset)
		}
	}
	for _, relativePath := range unsupported {
		if _, regErr := catalog.Register(catalogRegisterParams{
			ProjectID: projectID, Path: filepath.Join(projectRoot, filepath.FromSlash(relativePath)),
			Origin: MediaOriginImported,
		}); regErr != nil {
			if _, markErr := catalog.MarkFailed(regErr.Error()); markErr != nil {
				return nil, markErr
			}
			return nil, regErr
		}
	}
	summary, err := catalog.MarkReady()
	if err != nil {
		return nil, err
	}
	diagnostics, err := catalog.Diagnostics()
	if err != nil {
		return nil, err
	}
	allAssets, err := catalog.ListAssets(ListMediaAssetsParams{})
	if err != nil {
		return nil, err
	}
	reconciliationItems, err := catalog.ReconciliationItems()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"enabled":        true,
		"project_id":     projectID,
		"project_root":   projectRoot,
		"scanned_paths":  len(supported) + len(unsupported),
		"indexed_count":  len(indexed),
		"asset_count":    len(allAssets),
		"summary":        mediaSummaryMap(summary),
		"diagnostics":    mediaDiagnosticMaps(diagnostics),
		"reconciliation": mediaReconciliationMaps(reconciliationItems),
	}, nil
}

// RetryProjectMediaReconciliation 重试持久的重建工作（对应 retry_project_media_reconciliation）。
func RetryProjectMediaReconciliation(projectID, projectRoot string, itemID *string) (map[string]any, error) {
	if !MediaIndexEnabled() {
		return map[string]any{
			"enabled": false, "project_id": projectID, "repaired": []any{}, "reconciliation": []any{},
		}, nil
	}
	catalog := ProjectMediaCatalog(projectRoot)
	repaired, err := catalog.RetryReconciliation(projectRoot, itemID)
	if err != nil {
		return nil, err
	}
	reconciliationItems, err := catalog.ReconciliationItems()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"project_id":     projectID,
		"repaired":       mediaAssetMaps(repaired),
		"reconciliation": mediaReconciliationMaps(reconciliationItems),
	}, nil
}

// --- asdict 等价工具 ---

func toJSONMap(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func mediaAssetMap(a MediaAsset) map[string]any { return toJSONMap(a) }

func mediaAssetMaps(list []MediaAsset) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, mediaAssetMap(a))
	}
	return out
}

func mediaBindingMap(b MediaBinding) map[string]any { return toJSONMap(b) }

func mediaDerivationMap(d MediaDerivation) map[string]any { return toJSONMap(d) }

func mediaDiagnosticMaps(list []MediaMigrationDiagnostic) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		out = append(out, toJSONMap(item))
	}
	return out
}

func mediaReconciliationMaps(list []MediaReconciliationItem) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		out = append(out, toJSONMap(item))
	}
	return out
}

func mediaSummaryMap(s MediaIndexSummary) map[string]any { return toJSONMap(s) }
