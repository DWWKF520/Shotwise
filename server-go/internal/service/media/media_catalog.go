// Package service 中的媒体索引目录（对应 lib/media_catalog.py）。
//
// MediaAsset 只给既有媒体文件分配稳定 ID，绝不移动/改名/删除/转码/覆盖任何文件；
// 遗留的 project.json 路径仍是兼容契约。目录以 project 根下的
// .media-assets.json 为唯一真相源，数据库镜像表只是显式同步边界。
package media

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/service"
)

// 媒体种类取值。
const (
	MediaKindImage = "image"
	MediaKindVideo = "video"
	MediaKindAudio = "audio"
)

// 媒体来源取值。
const (
	MediaOriginUpload   = "upload"
	MediaOriginEdited   = "edited"
	MediaOriginImported = "imported"
)

// 索引摘要状态取值。
const (
	MediaIndexStatusReady   = "ready"
	MediaIndexStatusSyncing = "syncing"
	MediaIndexStatusStale   = "stale"
	MediaIndexStatusFailed  = "failed"
)

// mediaExtensions 是各媒体种类可索引的后缀集合。
var mediaExtensions = map[string]map[string]bool{
	MediaKindImage: {".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true},
	MediaKindVideo: {".mp4": true, ".mov": true, ".webm": true, ".mkv": true},
	MediaKindAudio: {".mp3": true, ".wav": true, ".m4a": true, ".aac": true, ".ogg": true},
}

// MediaIndexEnabled 判断可选的 MediaAsset 登记是否开启（对应 media_index_enabled）。
func MediaIndexEnabled() bool {
	return config.FeatureEnabled("media_asset_index")
}

// ClassifyMediaPath 按后缀判定媒体种类；不可索引返回空串。
func ClassifyMediaPath(path string) string {
	suffix := strings.ToLower(filepath.Ext(path))
	for kind, extensions := range mediaExtensions {
		if extensions[suffix] {
			return kind
		}
	}
	return ""
}

// MediaAsset 是索引条目（对应 lib/media_catalog.py 的 MediaAsset 数据类）。
type MediaAsset struct {
	ID              string   `json:"id"`
	ProjectID       string   `json:"project_id"`
	Kind            string   `json:"kind"`
	OriginalName    string   `json:"original_name"`
	MimeType        *string  `json:"mime_type"`
	Extension       string   `json:"extension"`
	PhysicalPath    string   `json:"physical_path"`
	SizeBytes       int64    `json:"size_bytes"`
	Fingerprint     string   `json:"fingerprint"`
	Origin          string   `json:"origin"`
	CreatedAt       string   `json:"created_at"`
	Width           *int     `json:"width"`
	Height          *int     `json:"height"`
	DurationSeconds *float64 `json:"duration_seconds"`
	WorkflowRunID   *string  `json:"workflow_run_id"`
	WorkflowNodeKey *string  `json:"workflow_node_key"`
	ProviderID      *string  `json:"provider_id"`
	ModelID         *string  `json:"model_id"`
	PromptSnapshot  *string  `json:"prompt_snapshot"`
	Archived        bool     `json:"archived"`
}

// MediaBinding 是资产与语义目标的绑定（对应 MediaBinding）。
type MediaBinding struct {
	ID           string  `json:"id"`
	MediaAssetID string  `json:"media_asset_id"`
	ProjectID    string  `json:"project_id"`
	BindingKind  string  `json:"binding_kind"`
	TargetID     *string `json:"target_id"`
	Purpose      string  `json:"purpose"`
	CreatedAt    string  `json:"created_at"`
}

// MediaDerivation 是父子资产的派生关系（对应 MediaDerivation）。
type MediaDerivation struct {
	ID                 string `json:"id"`
	ParentMediaAssetID string `json:"parent_media_asset_id"`
	ChildMediaAssetID  string `json:"child_media_asset_id"`
	Operation          string `json:"operation"`
	CreatedAt          string `json:"created_at"`
}

// MediaMigrationDiagnostic 是不可索引/冲突的持久诊断（对应 MediaMigrationDiagnostic）。
type MediaMigrationDiagnostic struct {
	ProjectID string `json:"project_id"`
	Path      string `json:"path"`
	Code      string `json:"code"`
	Detail    string `json:"detail"`
	CreatedAt string `json:"created_at"`
}

// MediaReconciliationItem 是索引/绑定操作的持久重试记录（对应 MediaReconciliationItem）。
type MediaReconciliationItem struct {
	ID                  string  `json:"id"`
	ProjectID           string  `json:"project_id"`
	RelativePath        string  `json:"relative_path"`
	WorkflowRunID       *string `json:"workflow_run_id"`
	WorkflowNodeKey     *string `json:"workflow_node_key"`
	Reason              string  `json:"reason"`
	CreatedAt           string  `json:"created_at"`
	ResolvedAt          *string `json:"resolved_at"`
	Origin              string  `json:"origin"`
	Operation           string  `json:"operation"`
	MediaAssetID        *string `json:"media_asset_id"`
	BindingKind         *string `json:"binding_kind"`
	TargetID            *string `json:"target_id"`
	Purpose             *string `json:"purpose"`
	ParentMediaAssetID  *string `json:"parent_media_asset_id"`
	DerivationOperation *string `json:"derivation_operation"`
}

// MediaIndexSummary 是给项目列表读取的、持久化且不触盘的文件系统无关摘要。
type MediaIndexSummary struct {
	AssetCount     int     `json:"asset_count"`
	LastIndexedAt  *string `json:"last_indexed_at"`
	Status         string  `json:"status"`
	SummaryVersion int     `json:"summary_version"`
	Error          *string `json:"error"`
}

// MediaAssetReferencedError 表示被索引资产仍有语义引用（对应 MediaAssetReferencedError）。
type MediaAssetReferencedError struct {
	MediaAssetID string
	References   []string
}

func (e *MediaAssetReferencedError) Error() string {
	detail := strings.Join(e.References, ", ")
	if detail == "" {
		detail = "unknown reference"
	}
	return fmt.Sprintf("MediaAsset is still referenced: %s (%s)", e.MediaAssetID, detail)
}

// mediaCatalogState 是内存态的目录状态（对应 _State）。
type mediaCatalogState struct {
	assets         map[string]MediaAsset
	bindings       map[string]MediaBinding
	derivations    map[string]MediaDerivation
	diagnostics    []MediaMigrationDiagnostic
	reconciliation []MediaReconciliationItem
	summary        MediaIndexSummary
}

type mediaCatalogPayload struct {
	Assets         map[string]MediaAsset      `json:"assets"`
	Bindings       map[string]MediaBinding    `json:"bindings"`
	Derivations    map[string]MediaDerivation `json:"derivations"`
	Diagnostics    []MediaMigrationDiagnostic `json:"diagnostics"`
	Reconciliation []MediaReconciliationItem  `json:"reconciliation"`
	Summary        MediaIndexSummary          `json:"summary"`
}

// MediaCatalog 是面向调用方的单文件持久化索引适配器（对应 MediaCatalog）。
type MediaCatalog struct {
	indexFile string
}

// ProjectMediaCatalog 返回项目根下的目录实例（对应 project_media_catalog）。
func ProjectMediaCatalog(projectRoot string) *MediaCatalog {
	return &MediaCatalog{indexFile: filepath.Join(projectRoot, ".media-assets.json")}
}

// IndexFile 暴露索引文件路径（测试与诊断用）。
func (m *MediaCatalog) IndexFile() string { return m.indexFile }

// fingerprint 计算文件的 sha256（对应 _fingerprint）。
func fingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func newMediaCatalogState() mediaCatalogState {
	return mediaCatalogState{
		assets:      map[string]MediaAsset{},
		bindings:    map[string]MediaBinding{},
		derivations: map[string]MediaDerivation{},
		summary:     MediaIndexSummary{Status: MediaIndexStatusStale, SummaryVersion: 1},
	}
}

// normalizeMediaSummary 复刻 _normalize_summary 的容错口径。
func normalizeMediaSummary(raw *MediaIndexSummary, fallbackAssetCount int) MediaIndexSummary {
	out := MediaIndexSummary{AssetCount: fallbackAssetCount, Status: MediaIndexStatusStale, SummaryVersion: 1}
	if raw == nil {
		return out
	}
	switch raw.Status {
	case MediaIndexStatusReady, MediaIndexStatusSyncing, MediaIndexStatusStale, MediaIndexStatusFailed:
		out.Status = raw.Status
	}
	if raw.AssetCount != 0 || fallbackAssetCount == 0 {
		out.AssetCount = raw.AssetCount
	}
	if raw.SummaryVersion != 0 {
		out.SummaryVersion = raw.SummaryVersion
	}
	out.LastIndexedAt = raw.LastIndexedAt
	out.Error = raw.Error
	return out
}

// load 读取索引文件；缺失返回空状态（对应 _load）。
func (m *MediaCatalog) load() (mediaCatalogState, error) {
	state := newMediaCatalogState()
	b, err := os.ReadFile(m.indexFile)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return state, err
	}
	var raw mediaCatalogPayload
	if err := json.Unmarshal(b, &raw); err != nil {
		return state, fmt.Errorf("parse %s: %w", m.indexFile, err)
	}
	if raw.Assets != nil {
		state.assets = raw.Assets
	}
	if raw.Bindings != nil {
		state.bindings = raw.Bindings
	}
	if raw.Derivations != nil {
		state.derivations = raw.Derivations
	}
	state.diagnostics = raw.Diagnostics
	state.reconciliation = raw.Reconciliation
	state.summary = normalizeMediaSummary(&raw.Summary, len(state.assets))
	return state, nil
}

// save 原子写入索引文件（对应 _save）。
func (m *MediaCatalog) save(state mediaCatalogState) error {
	payload := mediaCatalogPayload{
		Assets:         state.assets,
		Bindings:       state.bindings,
		Derivations:    state.derivations,
		Diagnostics:    state.diagnostics,
		Reconciliation: state.reconciliation,
		Summary:        state.summary,
	}
	if payload.Assets == nil {
		payload.Assets = map[string]MediaAsset{}
	}
	if payload.Bindings == nil {
		payload.Bindings = map[string]MediaBinding{}
	}
	if payload.Derivations == nil {
		payload.Derivations = map[string]MediaDerivation{}
	}
	if payload.Diagnostics == nil {
		payload.Diagnostics = []MediaMigrationDiagnostic{}
	}
	if payload.Reconciliation == nil {
		payload.Reconciliation = []MediaReconciliationItem{}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.indexFile), 0o755); err != nil {
		return err
	}
	temporary := m.indexFile + ".tmp"
	if err := os.WriteFile(temporary, b, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, m.indexFile)
}

// refreshSummary 更新摘要的资产数与可选字段（对应 _refresh_summary）。
func refreshMediaSummary(state *mediaCatalogState, status, errText, indexedAt *string) {
	state.summary.AssetCount = len(state.assets)
	if status != nil {
		state.summary.Status = *status
	}
	state.summary.Error = errText
	if indexedAt != nil {
		state.summary.LastIndexedAt = indexedAt
	}
}

// Summary 返回摘要（对应 summary，跳过可重建的缓存优化）。
func (m *MediaCatalog) Summary() (MediaIndexSummary, error) {
	state, err := m.load()
	if err != nil {
		return MediaIndexSummary{}, err
	}
	if _, statErr := os.Stat(m.indexFile); statErr != nil {
		return MediaIndexSummary{Status: MediaIndexStatusStale, SummaryVersion: 1}, nil
	}
	return state.summary, nil
}

// MarkSyncing 标记索引正在同步。
func (m *MediaCatalog) MarkSyncing() (MediaIndexSummary, error) {
	state, err := m.load()
	if err != nil {
		return MediaIndexSummary{}, err
	}
	empty := ""
	refreshMediaSummary(&state, mediaStrPtr(MediaIndexStatusSyncing), &empty, nil)
	if err := m.save(state); err != nil {
		return MediaIndexSummary{}, err
	}
	return state.summary, nil
}

// MarkReady 标记索引就绪。
func (m *MediaCatalog) MarkReady() (MediaIndexSummary, error) {
	state, err := m.load()
	if err != nil {
		return MediaIndexSummary{}, err
	}
	empty := ""
	now := service.NowISO()
	refreshMediaSummary(&state, mediaStrPtr(MediaIndexStatusReady), &empty, &now)
	if err := m.save(state); err != nil {
		return MediaIndexSummary{}, err
	}
	return state.summary, nil
}

// MarkFailed 标记索引失败（错误截断到 500 字符）。
func (m *MediaCatalog) MarkFailed(errText string) (MediaIndexSummary, error) {
	state, err := m.load()
	if err != nil {
		return MediaIndexSummary{}, err
	}
	trimmed := errText
	if len(trimmed) > 500 {
		trimmed = trimmed[:500]
	}
	refreshMediaSummary(&state, mediaStrPtr(MediaIndexStatusFailed), &trimmed, nil)
	if err := m.save(state); err != nil {
		return MediaIndexSummary{}, err
	}
	return state.summary, nil
}

// catalogRegisterParams 是 Register 的入参。
type catalogRegisterParams struct {
	ProjectID       string
	Path            string
	Origin          string
	WorkflowRunID   *string
	WorkflowNodeKey *string
	ProviderID      *string
	ModelID         *string
	PromptSnapshot  *string
	OriginalName    *string
}

// Register 返回既有或新建的索引条目，不修改文件（对应 register）。
func (m *MediaCatalog) Register(p catalogRegisterParams) (*MediaAsset, error) {
	state, err := m.load()
	if err != nil {
		return nil, err
	}
	kind := ClassifyMediaPath(p.Path)
	if kind == "" {
		state.diagnostics = append(state.diagnostics, MediaMigrationDiagnostic{
			ProjectID: p.ProjectID, Path: p.Path, Code: "unsupported_media",
			Detail: "extension is not indexed", CreatedAt: service.NowISO(),
		})
		if err := m.save(state); err != nil {
			return nil, err
		}
		return nil, nil
	}
	info, statErr := os.Stat(p.Path)
	if statErr != nil || info.IsDir() {
		state.diagnostics = append(state.diagnostics, MediaMigrationDiagnostic{
			ProjectID: p.ProjectID, Path: p.Path, Code: "missing_file",
			Detail: "referenced file does not exist", CreatedAt: service.NowISO(),
		})
		if err := m.save(state); err != nil {
			return nil, err
		}
		return nil, nil
	}
	fp, err := fingerprint(p.Path)
	if err != nil {
		state.diagnostics = append(state.diagnostics, MediaMigrationDiagnostic{
			ProjectID: p.ProjectID, Path: p.Path, Code: "unreadable_file",
			Detail: fmt.Sprintf("cannot read file: %v", err), CreatedAt: service.NowISO(),
		})
		if err := m.save(state); err != nil {
			return nil, err
		}
		return nil, nil
	}
	pathCanonical := canonicalMediaPath(p.Path)
	for _, asset := range state.assets {
		if asset.ProjectID != p.ProjectID {
			continue
		}
		samePath := canonicalMediaPath(asset.PhysicalPath) == pathCanonical
		if samePath && asset.Fingerprint == fp {
			return &asset, nil
		}
		if samePath {
			state.diagnostics = append(state.diagnostics, MediaMigrationDiagnostic{
				ProjectID: p.ProjectID, Path: p.Path, Code: "conflicting_file",
				Detail: fmt.Sprintf("the indexed path has different content (%s)", asset.ID), CreatedAt: service.NowISO(),
			})
		} else if asset.Fingerprint == fp {
			state.diagnostics = append(state.diagnostics, MediaMigrationDiagnostic{
				ProjectID: p.ProjectID, Path: p.Path, Code: "duplicate_file",
				Detail: fmt.Sprintf("same content is already indexed as %s", asset.ID), CreatedAt: service.NowISO(),
			})
		}
	}
	if len(state.diagnostics) > 0 {
		if err := m.save(state); err != nil {
			return nil, err
		}
	}
	mimeType := guessMediaMimeType(filepath.Base(p.Path))
	originalName := filepath.Base(p.Path)
	if p.OriginalName != nil && *p.OriginalName != "" {
		originalName = *p.OriginalName
	}
	asset := MediaAsset{
		ID:              uuid.NewString(),
		ProjectID:       p.ProjectID,
		Kind:            kind,
		OriginalName:    originalName,
		MimeType:        mimeType,
		Extension:       strings.ToLower(filepath.Ext(p.Path)),
		PhysicalPath:    p.Path,
		SizeBytes:       info.Size(),
		Fingerprint:     fp,
		Origin:          p.Origin,
		CreatedAt:       service.NowISO(),
		WorkflowRunID:   p.WorkflowRunID,
		WorkflowNodeKey: p.WorkflowNodeKey,
		ProviderID:      p.ProviderID,
		ModelID:         p.ModelID,
		PromptSnapshot:  p.PromptSnapshot,
	}
	state.assets[asset.ID] = asset
	stale := ""
	refreshMediaSummary(&state, &stale, &stale, nil)
	if err := m.save(state); err != nil {
		return nil, err
	}
	return &asset, nil
}

// Bind 给资产加一条语义绑定；重复元组直接返回既有绑定（对应 bind）。
func (m *MediaCatalog) Bind(mediaAssetID string, projectID, bindingKind string, targetID *string, purpose string) (MediaBinding, error) {
	state, err := m.load()
	if err != nil {
		return MediaBinding{}, err
	}
	asset, ok := state.assets[mediaAssetID]
	if !ok || asset.ProjectID != projectID {
		return MediaBinding{}, fmt.Errorf("unknown MediaAsset: %s", mediaAssetID)
	}
	for _, binding := range state.bindings {
		if binding.MediaAssetID == mediaAssetID && binding.ProjectID == projectID &&
			binding.BindingKind == bindingKind && ptrEqual(binding.TargetID, targetID) && binding.Purpose == purpose {
			return binding, nil
		}
	}
	binding := MediaBinding{
		ID: uuid.NewString(), MediaAssetID: mediaAssetID, ProjectID: projectID,
		BindingKind: bindingKind, TargetID: targetID, Purpose: purpose, CreatedAt: service.NowISO(),
	}
	state.bindings[binding.ID] = binding
	if err := m.save(state); err != nil {
		return MediaBinding{}, err
	}
	return binding, nil
}

// Derive 记录父子资产的派生关系；重复直接返回既有记录（对应 derive）。
func (m *MediaCatalog) Derive(parentMediaAssetID, childMediaAssetID, operation string) (MediaDerivation, error) {
	state, err := m.load()
	if err != nil {
		return MediaDerivation{}, err
	}
	parent, okParent := state.assets[parentMediaAssetID]
	child, okChild := state.assets[childMediaAssetID]
	if !okParent || !okChild {
		return MediaDerivation{}, fmt.Errorf("both MediaAssets must exist")
	}
	if parent.ProjectID != child.ProjectID {
		return MediaDerivation{}, fmt.Errorf("MediaAssets must belong to the same project")
	}
	for _, derivation := range state.derivations {
		if derivation.ParentMediaAssetID == parentMediaAssetID &&
			derivation.ChildMediaAssetID == childMediaAssetID && derivation.Operation == operation {
			return derivation, nil
		}
	}
	derivation := MediaDerivation{
		ID: uuid.NewString(), ParentMediaAssetID: parentMediaAssetID,
		ChildMediaAssetID: childMediaAssetID, Operation: operation, CreatedAt: service.NowISO(),
	}
	state.derivations[derivation.ID] = derivation
	if err := m.save(state); err != nil {
		return MediaDerivation{}, err
	}
	return derivation, nil
}

// Get 取一条资产；不存在返回 nil（对应 get）。
func (m *MediaCatalog) Get(mediaAssetID string) (*MediaAsset, error) {
	state, err := m.load()
	if err != nil {
		return nil, err
	}
	asset, ok := state.assets[mediaAssetID]
	if !ok {
		return nil, nil
	}
	return &asset, nil
}

// EnqueueMediaReconciliationParams 是 EnqueueReconciliation 的入参。
type EnqueueMediaReconciliationParams struct {
	ProjectID           string
	RelativePath        string
	Reason              string
	WorkflowRunID       *string
	WorkflowNodeKey     *string
	Origin              string
	Operation           string
	MediaAssetID        *string
	BindingKind         *string
	TargetID            *string
	Purpose             *string
	ParentMediaAssetID  *string
	DerivationOperation *string
}

// EnqueueReconciliation 持久化一条重试项，不触碰生成文件（对应 enqueue_reconciliation）。
func (m *MediaCatalog) EnqueueReconciliation(p EnqueueMediaReconciliationParams) (MediaReconciliationItem, error) {
	state, err := m.load()
	if err != nil {
		return MediaReconciliationItem{}, err
	}
	for _, existing := range state.reconciliation {
		if existing.ResolvedAt == nil &&
			existing.ProjectID == p.ProjectID && existing.RelativePath == p.RelativePath &&
			ptrEqual(existing.WorkflowRunID, p.WorkflowRunID) && ptrEqual(existing.WorkflowNodeKey, p.WorkflowNodeKey) &&
			existing.Operation == p.Operation && ptrEqual(existing.MediaAssetID, p.MediaAssetID) &&
			ptrEqual(existing.BindingKind, p.BindingKind) && ptrEqual(existing.TargetID, p.TargetID) &&
			ptrEqual(existing.Purpose, p.Purpose) && ptrEqual(existing.ParentMediaAssetID, p.ParentMediaAssetID) &&
			ptrEqual(existing.DerivationOperation, p.DerivationOperation) {
			return existing, nil
		}
	}
	origin := p.Origin
	if origin == "" {
		origin = MediaOriginImported
	}
	operation := p.Operation
	if operation == "" {
		operation = "register"
	}
	reason := p.Reason
	if len(reason) > 500 {
		reason = reason[:500]
	}
	item := MediaReconciliationItem{
		ID: uuid.NewString(), ProjectID: p.ProjectID, RelativePath: p.RelativePath,
		WorkflowRunID: p.WorkflowRunID, WorkflowNodeKey: p.WorkflowNodeKey, Reason: reason,
		CreatedAt: service.NowISO(), Origin: origin, Operation: operation, MediaAssetID: p.MediaAssetID,
		BindingKind: p.BindingKind, TargetID: p.TargetID, Purpose: p.Purpose,
		ParentMediaAssetID: p.ParentMediaAssetID, DerivationOperation: p.DerivationOperation,
	}
	state.reconciliation = append(state.reconciliation, item)
	if err := m.save(state); err != nil {
		return MediaReconciliationItem{}, err
	}
	return item, nil
}

// ReconciliationItems 返回未解决的修复项，按创建顺序（对应 reconciliation_items）。
func (m *MediaCatalog) ReconciliationItems() ([]MediaReconciliationItem, error) {
	state, err := m.load()
	if err != nil {
		return nil, err
	}
	items := make([]MediaReconciliationItem, 0, len(state.reconciliation))
	for _, item := range state.reconciliation {
		if item.ResolvedAt == nil {
			items = append(items, item)
		}
	}
	return items, nil
}

// ResolveReconciliation 成功重试后把匹配的修复记录标记为已解决，返回标记条数。
func (m *MediaCatalog) ResolveReconciliation(operation string, mediaAssetID *string) (int, error) {
	state, err := m.load()
	if err != nil {
		return 0, err
	}
	resolvedAt := service.NowISO()
	count := 0
	updated := make([]MediaReconciliationItem, 0, len(state.reconciliation))
	for _, item := range state.reconciliation {
		if item.ResolvedAt != nil || item.Operation != operation {
			updated = append(updated, item)
			continue
		}
		if mediaAssetID != nil && (item.MediaAssetID == nil || *item.MediaAssetID != *mediaAssetID) {
			updated = append(updated, item)
			continue
		}
		item.ResolvedAt = &resolvedAt
		updated = append(updated, item)
		count++
	}
	if count > 0 {
		state.reconciliation = updated
		if err := m.save(state); err != nil {
			return 0, err
		}
	}
	return count, nil
}

// Diagnostics 返回持久诊断，按创建顺序（对应 diagnostics）。
func (m *MediaCatalog) Diagnostics() ([]MediaMigrationDiagnostic, error) {
	state, err := m.load()
	if err != nil {
		return nil, err
	}
	return state.diagnostics, nil
}

// RecordDiagnostic 幂等写入一条诊断（对应 record_diagnostic）。
func (m *MediaCatalog) RecordDiagnostic(projectID, path, code, detail string) error {
	state, err := m.load()
	if err != nil {
		return err
	}
	for _, item := range state.diagnostics {
		if item.ProjectID == projectID && item.Path == path && item.Code == code && item.Detail == detail {
			return nil
		}
	}
	state.diagnostics = append(state.diagnostics, MediaMigrationDiagnostic{
		ProjectID: projectID, Path: path, Code: code, Detail: detail, CreatedAt: service.NowISO(),
	})
	return m.save(state)
}

// RetryReconciliation 重试持久登记/绑定工作，不删除物理文件（对应 retry_reconciliation）。
func (m *MediaCatalog) RetryReconciliation(projectRoot string, itemID *string) ([]MediaAsset, error) {
	repaired := []MediaAsset{}
	pending, err := m.ReconciliationItems()
	if err != nil {
		return nil, err
	}
	if itemID != nil {
		filtered := pending[:0]
		for _, item := range pending {
			if item.ID == *itemID {
				filtered = append(filtered, item)
			}
		}
		pending = filtered
	}
	for _, item := range pending {
		candidate := item.RelativePath
		if filepath.IsAbs(candidate) || containsDotDot(candidate) {
			continue
		}
		var asset *MediaAsset
		if item.MediaAssetID != nil {
			asset, err = m.Get(*item.MediaAssetID)
			if err != nil {
				return nil, err
			}
		}
		if asset == nil {
			origin := item.Origin
			if origin == "" {
				origin = MediaOriginImported
			}
			asset, err = m.Register(catalogRegisterParams{
				ProjectID: item.ProjectID, Path: filepath.Join(projectRoot, filepath.FromSlash(candidate)),
				Origin: origin, WorkflowRunID: item.WorkflowRunID, WorkflowNodeKey: item.WorkflowNodeKey,
			})
			if err != nil {
				return nil, err
			}
		}
		if asset == nil {
			continue
		}
		if m.retryReconciliationItem(item, asset) != nil {
			continue
		}
		state, err := m.load()
		if err != nil {
			return nil, err
		}
		now := service.NowISO()
		for i := range state.reconciliation {
			if state.reconciliation[i].ID == item.ID {
				state.reconciliation[i].ResolvedAt = &now
			}
		}
		if err := m.save(state); err != nil {
			return nil, err
		}
		repaired = append(repaired, *asset)
	}
	return repaired, nil
}

// retryReconciliationItem 执行单条修复动作；失败返回错误（调用方跳过并保留待重试）。
func (m *MediaCatalog) retryReconciliationItem(item MediaReconciliationItem, asset *MediaAsset) error {
	if item.Operation == "derivation" && item.ParentMediaAssetID != nil {
		operation := "generated"
		if item.DerivationOperation != nil {
			operation = *item.DerivationOperation
		}
		_, err := m.Derive(*item.ParentMediaAssetID, asset.ID, operation)
		return err
	}
	if item.BindingKind != nil {
		purpose := "reconciled"
		if item.Purpose != nil {
			purpose = *item.Purpose
		}
		_, err := m.Bind(asset.ID, item.ProjectID, *item.BindingKind, item.TargetID, purpose)
		return err
	}
	if item.Operation == "binding" || strings.HasPrefix(item.Reason, "binding_failed:") {
		_, err := m.Bind(asset.ID, item.ProjectID, "project", nil, "reconciled_output")
		return err
	}
	return nil
}

// SetArchived 设置资产的归档标记（对应 set_archived）。
func (m *MediaCatalog) SetArchived(mediaAssetID string, archived bool) (MediaAsset, error) {
	state, err := m.load()
	if err != nil {
		return MediaAsset{}, err
	}
	asset, ok := state.assets[mediaAssetID]
	if !ok {
		return MediaAsset{}, fmt.Errorf("unknown MediaAsset: %s", mediaAssetID)
	}
	asset.Archived = archived
	state.assets[mediaAssetID] = asset
	if err := m.save(state); err != nil {
		return MediaAsset{}, err
	}
	return asset, nil
}

// ListMediaAssetsParams 是 ListAssets 的过滤条件。
type ListMediaAssetsParams struct {
	ProjectID     *string
	Kind          *string
	Origin        *string
	WorkflowRunID *string
	Archived      *bool
}

// ListAssets 列出索引资产，不触碰或重排物理文件（对应 list_assets）。
func (m *MediaCatalog) ListAssets(p ListMediaAssetsParams) ([]MediaAsset, error) {
	state, err := m.load()
	if err != nil {
		return nil, err
	}
	out := make([]MediaAsset, 0, len(state.assets))
	for _, asset := range state.assets {
		if p.ProjectID != nil && asset.ProjectID != *p.ProjectID {
			continue
		}
		if p.Kind != nil && asset.Kind != *p.Kind {
			continue
		}
		if p.Origin != nil && asset.Origin != *p.Origin {
			continue
		}
		if p.WorkflowRunID != nil && (asset.WorkflowRunID == nil || *asset.WorkflowRunID != *p.WorkflowRunID) {
			continue
		}
		if p.Archived != nil && asset.Archived != *p.Archived {
			continue
		}
		out = append(out, asset)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// Delete 只在所有语义引用消失后移除索引行（对应 delete）。
func (m *MediaCatalog) Delete(mediaAssetID string) (MediaAsset, error) {
	state, err := m.load()
	if err != nil {
		return MediaAsset{}, err
	}
	asset, ok := state.assets[mediaAssetID]
	if !ok {
		return MediaAsset{}, fmt.Errorf("unknown MediaAsset: %s", mediaAssetID)
	}
	references := []string{}
	for _, binding := range state.bindings {
		if binding.MediaAssetID == mediaAssetID {
			references = append(references, "binding:"+binding.ID)
		}
	}
	for _, derivation := range state.derivations {
		if derivation.ParentMediaAssetID == mediaAssetID || derivation.ChildMediaAssetID == mediaAssetID {
			references = append(references, "derivation:"+derivation.ID)
		}
	}
	if asset.WorkflowRunID != nil {
		references = append(references, "workflow_run:"+*asset.WorkflowRunID)
	}
	if len(references) > 0 {
		return MediaAsset{}, &MediaAssetReferencedError{MediaAssetID: mediaAssetID, References: references}
	}
	delete(state.assets, mediaAssetID)
	stale := MediaIndexStatusStale
	refreshMediaSummary(&state, &stale, nil, nil)
	if err := m.save(state); err != nil {
		return MediaAsset{}, err
	}
	return asset, nil
}

// BindingsFor 返回资产的绑定列表（对应 bindings_for）。
func (m *MediaCatalog) BindingsFor(mediaAssetID string) ([]MediaBinding, error) {
	state, err := m.load()
	if err != nil {
		return nil, err
	}
	out := make([]MediaBinding, 0)
	for _, item := range state.bindings {
		if item.MediaAssetID == mediaAssetID {
			out = append(out, item)
		}
	}
	return out, nil
}

// DerivationsFor 返回与资产相连的派生关系（对应 derivations_for）。
func (m *MediaCatalog) DerivationsFor(mediaAssetID string) ([]MediaDerivation, error) {
	state, err := m.load()
	if err != nil {
		return nil, err
	}
	out := make([]MediaDerivation, 0)
	for _, item := range state.derivations {
		if item.ParentMediaAssetID == mediaAssetID || item.ChildMediaAssetID == mediaAssetID {
			out = append(out, item)
		}
	}
	return out, nil
}

// Backfill 幂等索引遗留引用并报告不安全引用（对应 backfill）。
func (m *MediaCatalog) Backfill(projectID, projectRoot string, relativePaths []string) ([]MediaAsset, error) {
	indexed := []MediaAsset{}
	for _, relativePath := range relativePaths {
		if filepath.IsAbs(relativePath) || containsDotDot(relativePath) {
			state, err := m.load()
			if err != nil {
				return nil, err
			}
			state.diagnostics = append(state.diagnostics, MediaMigrationDiagnostic{
				ProjectID: projectID, Path: relativePath, Code: "invalid_reference",
				Detail: "path escapes project root", CreatedAt: service.NowISO(),
			})
			if err := m.save(state); err != nil {
				return nil, err
			}
			continue
		}
		asset, err := m.Register(catalogRegisterParams{
			ProjectID: projectID, Path: filepath.Join(projectRoot, filepath.FromSlash(relativePath)),
			Origin: MediaOriginImported,
		})
		if err != nil {
			return nil, err
		}
		if asset != nil {
			indexed = append(indexed, *asset)
		}
	}
	return indexed, nil
}

// --- 内部小工具 ---

func mediaStrPtr(s string) *string { return &s }

func ptrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func containsDotDot(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// canonicalMediaPath 近似 Python Path.resolve()：绝对化并解析符号链接（尽力而为）。
func canonicalMediaPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs)
}

// guessMediaMimeType 近似 mimetypes.guess_type 的媒体分支。
func guessMediaMimeType(name string) *string {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		return nil
	}
	value := mime.TypeByExtension(ext)
	if value == "" {
		return nil
	}
	if idx := strings.Index(value, ";"); idx >= 0 {
		value = strings.TrimSpace(value[:idx])
	}
	return &value
}
