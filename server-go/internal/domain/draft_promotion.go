package domain

// 草稿晋升（对应 server/draft_promotion.py）。
//
// Agent / Upload / Online-AI 三源草稿在「正式剧本」之外的隔离评审流：草稿只存评审记录，
// 正式剧本仍是唯一内容真相源。领域层只做纯函数式决策（快照、校验、三方合并、乐观并发
// 比较），磁盘读写经 DraftPromotionRepository 接缝注入。
//
// 三个公开操作：Create 快照当前正式剧本；PreparePromotion 校验 + 三方合并；
// ConfirmPromotion 复校验后让仓储原子 compare-and-write。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// DraftOrigin 草稿来源（服务端自有，客户端不能伪造）。
type DraftOrigin string

const (
	DraftOriginAgent    DraftOrigin = "agent"
	DraftOriginUpload   DraftOrigin = "upload"
	DraftOriginOnlineAI DraftOrigin = "online_ai"
)

// DraftStatus 草稿状态。
type DraftStatus string

const (
	DraftStatusActive    DraftStatus = "active"
	DraftStatusAbandoned DraftStatus = "abandoned"
)

// 晋升状态机取值。
const (
	PreparationInvalid              = "invalid"
	PreparationConflicted           = "conflicted"
	PreparationReadyForConfirmation = "ready_for_confirmation"

	ResultPromoted            = "promoted"
	ResultInvalid             = "invalid"
	ResultStaleConfirmation   = "stale_confirmation"
	ResultInvalidConfirmation = "invalid_confirmation"
)

// ErrDraftNotFound 草稿不存在（或不属于该 actor）。
var ErrDraftNotFound = errors.New("draft not found")

// ErrDraftAbandoned 草稿已作废，不能再改内容。
var ErrDraftAbandoned = errors.New("draft is abandoned")

// DraftTarget 草稿意图修改的正式剧本。
type DraftTarget struct {
	ProjectName string `json:"project_name"`
	ScriptFile  string `json:"script_file"`
}

// OfficialScript 仓储适配器给出的正式剧本快照。
type OfficialScript struct {
	Content     map[string]any `json:"content"`
	Revision    int64          `json:"revision"`
	Fingerprint string         `json:"fingerprint"`
}

// DraftValidationIssue 一条校验问题；Code 是稳定机器码，Path 是 JSON Pointer。
type DraftValidationIssue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path"`
}

// PromotionConflict 以 JSON Pointer 表达的三方合并冲突。
type PromotionConflict struct {
	Path         string `json:"path"`
	BaseValue    any    `json:"base_value"`
	CurrentValue any    `json:"current_value"`
	DraftValue   any    `json:"draft_value"`
}

// PreparedPromotion 绑定某一正式剧本版本的已评审晋升计划。
type PreparedPromotion struct {
	Content             map[string]any `json:"content"`
	ExpectedRevision    int64          `json:"expected_revision"`
	ExpectedFingerprint string         `json:"expected_fingerprint"`
	ConfirmationToken   string         `json:"confirmation_token"`
	AutoMergedPaths     []string       `json:"auto_merged_paths"`
}

// Draft 一条可评审草稿；BaseContent/BaseRevision/BaseFingerprint 是创建时的基线快照。
type Draft struct {
	ID              string             `json:"id"`
	Target          DraftTarget        `json:"target"`
	Content         map[string]any     `json:"content"`
	Origin          DraftOrigin        `json:"origin"`
	ActorID         string             `json:"actor_id"`
	BaseContent     map[string]any     `json:"base_content"`
	BaseRevision    int64              `json:"base_revision"`
	BaseFingerprint string             `json:"base_fingerprint"`
	Prepared        *PreparedPromotion `json:"prepared"`
	Status          DraftStatus        `json:"status"`
}

// PromotionPreparation 校验 + 乐观并发比较的结果。
type PromotionPreparation struct {
	Status            string                 `json:"status"`
	ValidationIssues  []DraftValidationIssue `json:"validation_issues"`
	Conflicts         []PromotionConflict    `json:"conflicts"`
	AutoMergedPaths   []string               `json:"auto_merged_paths"`
	ConfirmationToken string                 `json:"confirmation_token"`
	PreviewContent    map[string]any         `json:"preview_content"`
}

// PromotionResult 确认晋升的结果。
type PromotionResult struct {
	Status           string                 `json:"status"`
	ValidationIssues []DraftValidationIssue `json:"validation_issues"`
	PromotedRevision *int64                 `json:"promoted_revision"`
}

// OfficialRevisionConflict 仓储的原子 compare-and-write 检测到正式剧本已变。
type OfficialRevisionConflict struct {
	Current OfficialScript
}

func (e *OfficialRevisionConflict) Error() string {
	return fmt.Sprintf("official script changed at revision %d", e.Current.Revision)
}

// DraftPromotionRepository 草稿与原子晋升的仓储接缝。
//
// PromoteAtomically 必须在同一临界区内比对 revision+fingerprint 再写盘。
type DraftPromotionRepository interface {
	ReadOfficial(target DraftTarget) (OfficialScript, error)
	LoadDraft(draftID, actorID string) (*Draft, error)
	ListDrafts(target DraftTarget, actorID string) ([]*Draft, error)
	ListAll(projectName, actorID string) ([]*Draft, error)
	SaveDraft(draft *Draft) error
	PromoteAtomically(target DraftTarget, content map[string]any, expectedRevision int64, expectedFingerprint string) (OfficialScript, error)
}

// DraftValidator 注入的项目级结构校验；返回空表示通过。
type DraftValidator func(content map[string]any) []DraftValidationIssue

// CanonicalFingerprint 正式剧本内容的稳定指纹（与 Python canonical_fingerprint 同口径：
// sort_keys / separators / ensure_ascii=False，浮点按 Python repr 序列化）。
//
// 数字口径说明：Go 侧 JSON 一律解码成 float64（1 与 1.0 同形），Python 的 json.load 会区分
// int 与 float。因此同一份内容在两侧算出的指纹对整数位数字可能不同——指纹只由同一运行时
// 产出与消费（草稿 store 与正式剧本同部署），自洽即可；Python 时代遗留的草稿在 Go 侧最多
// 退化成一次无害的自动合并，不会误判冲突。
func CanonicalFingerprint(content map[string]any) (string, error) {
	canonical, err := WorkflowCanonicalJSON(content)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:]), nil
}

// DraftPromotionService 草稿晋升领域服务。
type DraftPromotionService struct {
	repo         DraftPromotionRepository
	validator    DraftValidator
	idFactory    func() string
	tokenFactory func() string
}

// NewDraftPromotionService 构造服务；validator 为 nil 时用基线校验（仅查是否对象）。
func NewDraftPromotionService(repo DraftPromotionRepository, validator DraftValidator) *DraftPromotionService {
	return &DraftPromotionService{repo: repo, validator: validator, idFactory: newDraftID, tokenFactory: newConfirmationToken}
}

// newDraftID 对应 Python token_urlsafe(18)：18 随机字节 → 24 字符 base64url。
func newDraftID() string {
	return tokenURLSafe(18)
}

func newConfirmationToken() string {
	return tokenURLSafe(24)
}

func tokenURLSafe(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 不可用属于进程级故障，直接 panic 好过退化成可预测 ID。
		panic(fmt.Sprintf("draft promotion: read random: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// Create 从 Agent / Upload / Online-AI 输出创建草稿，先快照当前正式剧本。
func (s *DraftPromotionService) Create(target DraftTarget, content map[string]any, origin DraftOrigin, actorID string) (*Draft, error) {
	official, err := s.repo.ReadOfficial(target)
	if err != nil {
		return nil, err
	}
	draft := &Draft{
		ID:              s.idFactory(),
		Target:          target,
		Content:         deepCopyJSONObject(content),
		Origin:          origin,
		ActorID:         actorID,
		BaseContent:     deepCopyJSONObject(official.Content),
		BaseRevision:    official.Revision,
		BaseFingerprint: official.Fingerprint,
		Status:          DraftStatusActive,
	}
	if err := s.repo.SaveDraft(draft); err != nil {
		return nil, err
	}
	return draft, nil
}

// Update 替换活跃草稿内容，不动评审基线。
func (s *DraftPromotionService) Update(draftID string, content map[string]any, actorID string) (*Draft, error) {
	draft, err := s.requireActiveDraft(draftID, actorID)
	if err != nil {
		return nil, err
	}
	updated := *draft
	updated.Content = deepCopyJSONObject(content)
	updated.Prepared = nil
	if err := s.repo.SaveDraft(&updated); err != nil {
		return nil, err
	}
	return &updated, nil
}

// Abandon 作废草稿但保留审计记录。
func (s *DraftPromotionService) Abandon(draftID, actorID string) (*Draft, error) {
	draft, err := s.requireDraft(draftID, actorID)
	if err != nil {
		return nil, err
	}
	if draft.Status == DraftStatusAbandoned {
		return draft, nil
	}
	abandoned := *draft
	abandoned.Status = DraftStatusAbandoned
	abandoned.Prepared = nil
	if err := s.repo.SaveDraft(&abandoned); err != nil {
		return nil, err
	}
	return &abandoned, nil
}

// ListDrafts 列出某项目全部草稿，按 id 字典序（Python sorted by id 同口径）。
func (s *DraftPromotionService) ListDrafts(target DraftTarget, actorID string) ([]*Draft, error) {
	drafts, err := s.repo.ListDrafts(target, actorID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(drafts, func(i, j int) bool { return drafts[i].ID < drafts[j].ID })
	return drafts, nil
}

// GetDraft 经同一仓储接缝读取草稿。
func (s *DraftPromotionService) GetDraft(draftID, actorID string) (*Draft, error) {
	return s.requireDraft(draftID, actorID)
}

// PreparePromotion 校验草稿并产出冲突或确认令牌。
func (s *DraftPromotionService) PreparePromotion(draftID, actorID string) (*PromotionPreparation, error) {
	draft, err := s.requireDraft(draftID, actorID)
	if err != nil {
		return nil, err
	}
	if draft.Status != DraftStatusActive {
		return &PromotionPreparation{
			Status:           PreparationInvalid,
			ValidationIssues: []DraftValidationIssue{{Code: "draft_abandoned", Message: "Draft has been abandoned."}},
			Conflicts:        []PromotionConflict{},
			AutoMergedPaths:  []string{},
		}, nil
	}
	issues := s.validate(draft.Content)
	if issues == nil {
		issues = []DraftValidationIssue{}
	}
	if len(issues) > 0 {
		cleared := *draft
		cleared.Prepared = nil
		if err := s.repo.SaveDraft(&cleared); err != nil {
			return nil, err
		}
		return &PromotionPreparation{
			Status:           PreparationInvalid,
			ValidationIssues: issues,
			Conflicts:        []PromotionConflict{},
			AutoMergedPaths:  []string{},
		}, nil
	}

	current, err := s.repo.ReadOfficial(draft.Target)
	if err != nil {
		return nil, err
	}
	candidate := deepCopyJSONObject(draft.Content)
	autoMerged := []string{}
	if current.Revision != draft.BaseRevision || current.Fingerprint != draft.BaseFingerprint {
		merge := threeWayMerge(draft.BaseContent, current.Content, draft.Content)
		if len(merge.conflicts) > 0 {
			cleared := *draft
			cleared.Prepared = nil
			if err := s.repo.SaveDraft(&cleared); err != nil {
				return nil, err
			}
			return &PromotionPreparation{
				Status:           PreparationConflicted,
				ValidationIssues: []DraftValidationIssue{},
				Conflicts:        merge.conflicts,
				AutoMergedPaths:  []string{},
			}, nil
		}
		candidate = merge.content
		autoMerged = merge.autoMergedPaths
	}

	prepared := &PreparedPromotion{
		Content:             candidate,
		ExpectedRevision:    current.Revision,
		ExpectedFingerprint: current.Fingerprint,
		ConfirmationToken:   s.tokenFactory(),
		AutoMergedPaths:     autoMerged,
	}
	reviewed := *draft
	reviewed.Prepared = prepared
	if err := s.repo.SaveDraft(&reviewed); err != nil {
		return nil, err
	}
	return &PromotionPreparation{
		Status:            PreparationReadyForConfirmation,
		ValidationIssues:  []DraftValidationIssue{},
		Conflicts:         []PromotionConflict{},
		AutoMergedPaths:   append([]string{}, prepared.AutoMergedPaths...),
		ConfirmationToken: prepared.ConfirmationToken,
		PreviewContent:    deepCopyJSONObject(prepared.Content),
	}, nil
}

// ConfirmPromotion 复校验并原子晋升已评审草稿。
func (s *DraftPromotionService) ConfirmPromotion(draftID, confirmationToken, actorID string) (*PromotionResult, error) {
	draft, err := s.requireDraft(draftID, actorID)
	if err != nil {
		return nil, err
	}
	if draft.Status != DraftStatusActive {
		return &PromotionResult{Status: ResultInvalidConfirmation, ValidationIssues: []DraftValidationIssue{}}, nil
	}
	if draft.Prepared == nil || confirmationToken != draft.Prepared.ConfirmationToken {
		return &PromotionResult{Status: ResultInvalidConfirmation, ValidationIssues: []DraftValidationIssue{}}, nil
	}

	issues := s.validate(draft.Prepared.Content)
	if issues == nil {
		issues = []DraftValidationIssue{}
	}
	if len(issues) > 0 {
		cleared := *draft
		cleared.Prepared = nil
		if err := s.repo.SaveDraft(&cleared); err != nil {
			return nil, err
		}
		return &PromotionResult{Status: ResultInvalid, ValidationIssues: issues}, nil
	}

	promoted, err := s.repo.PromoteAtomically(draft.Target, deepCopyJSONObject(draft.Prepared.Content), draft.Prepared.ExpectedRevision, draft.Prepared.ExpectedFingerprint)
	if err != nil {
		var conflict *OfficialRevisionConflict
		if errors.As(err, &conflict) {
			cleared := *draft
			cleared.Prepared = nil
			if saveErr := s.repo.SaveDraft(&cleared); saveErr != nil {
				return nil, saveErr
			}
			return &PromotionResult{Status: ResultStaleConfirmation, ValidationIssues: []DraftValidationIssue{}}, nil
		}
		return nil, err
	}

	cleared := *draft
	cleared.Prepared = nil
	if err := s.repo.SaveDraft(&cleared); err != nil {
		return nil, err
	}
	revision := promoted.Revision
	return &PromotionResult{Status: ResultPromoted, ValidationIssues: []DraftValidationIssue{}, PromotedRevision: &revision}, nil
}

// Repo 返回仓储接缝。
//
// list_drafts 不带 script_file 时 Python 侧直接调 repository.list_all（不过领域服务），
// 这里把接缝暴露给传输层走同一条路。
func (s *DraftPromotionService) Repo() DraftPromotionRepository { return s.repo }

func (s *DraftPromotionService) validate(content map[string]any) []DraftValidationIssue {
	if s.validator != nil {
		return s.validator(content)
	}
	if content == nil {
		return []DraftValidationIssue{{Code: "draft_not_object", Message: "Draft content must be a JSON object."}}
	}
	return nil
}

func (s *DraftPromotionService) requireDraft(draftID, actorID string) (*Draft, error) {
	draft, err := s.repo.LoadDraft(draftID, actorID)
	if err != nil {
		return nil, err
	}
	if draft == nil {
		return nil, ErrDraftNotFound
	}
	return draft, nil
}

func (s *DraftPromotionService) requireActiveDraft(draftID, actorID string) (*Draft, error) {
	draft, err := s.requireDraft(draftID, actorID)
	if err != nil {
		return nil, err
	}
	if draft.Status != DraftStatusActive {
		return nil, ErrDraftAbandoned
	}
	return draft, nil
}

// --- 三方合并（对应 server/draft_promotion.py 的 _three_way_merge 一族） ---

// draftMissing 是「该键/元素在某一侧不存在」的哨兵（对应 Python _MISSING）。
// 冲突值里的 missing 一律序列化成 null。
type draftMissing struct{}

var draftMissingValue = draftMissing{}

type mergeResult struct {
	content         map[string]any
	conflicts       []PromotionConflict
	autoMergedPaths []string
}

// draftStableIDFields 已知剧本集合的稳定 ID 字段名（Python _STABLE_ID_FIELDS）。
var draftStableIDFields = map[string]string{
	"scenes":      "scene_id",
	"segments":    "segment_id",
	"shots":       "shot_id",
	"video_units": "unit_id",
}

func threeWayMerge(base, current, draft map[string]any) mergeResult {
	merged, conflicts, paths := mergeJSONValue(base, current, draft, "")
	content, ok := merged.(map[string]any)
	if !ok {
		// 顶层内容必须仍是 JSON 对象（Python 侧 AssertionError）。
		panic("draft promotion: top-level promotion content must remain a JSON object")
	}
	if conflicts == nil {
		// Python 的空 tuple 序列化为 []；Go 的 nil slice 会序列化成 null，响应形状会漂移。
		conflicts = []PromotionConflict{}
	}
	if paths == nil {
		paths = []string{}
	}
	return mergeResult{content: content, conflicts: conflicts, autoMergedPaths: paths}
}

func mergeJSONValue(base, current, draft any, path string) (any, []PromotionConflict, []string) {
	if jsonEqual(draft, base) {
		paths := []string{}
		if !jsonEqual(current, base) {
			paths = append(paths, displayDraftPath(path))
		}
		return deepCopyJSONValue(current), nil, paths
	}
	if jsonEqual(current, base) {
		return deepCopyJSONValue(draft), nil, nil
	}
	if jsonEqual(current, draft) {
		return deepCopyJSONValue(current), nil, nil
	}

	baseMap, baseIsMap := base.(map[string]any)
	currentMap, currentIsMap := current.(map[string]any)
	draftMap, draftIsMap := draft.(map[string]any)
	if baseIsMap && currentIsMap && draftIsMap {
		merged := map[string]any{}
		var conflicts []PromotionConflict
		var autoMerged []string
		for _, key := range sortedUnionKeys(baseMap, currentMap, draftMap) {
			item, itemConflicts, itemPaths := mergeJSONValue(
				mapValueOrMissing(baseMap, key),
				mapValueOrMissing(currentMap, key),
				mapValueOrMissing(draftMap, key),
				jsonPointerSegment(path, key),
			)
			conflicts = append(conflicts, itemConflicts...)
			autoMerged = append(autoMerged, itemPaths...)
			if !isDraftMissing(item) {
				merged[key] = item
			}
		}
		return merged, conflicts, autoMerged
	}

	baseList, baseIsList := base.([]any)
	currentList, currentIsList := current.([]any)
	draftList, draftIsList := draft.([]any)
	if baseIsList && currentIsList && draftIsList {
		return mergeJSONArray(baseList, currentList, draftList, path)
	}

	return draftMissingValue, []PromotionConflict{{
		Path:         displayDraftPath(path),
		BaseValue:    conflictValue(base),
		CurrentValue: conflictValue(current),
		DraftValue:   conflictValue(draft),
	}}, nil
}

func mergeJSONArray(base, current, draft []any, path string) (any, []PromotionConflict, []string) {
	field := path
	if index := strings.LastIndex(path, "/"); index >= 0 {
		field = path[index+1:]
	}
	idField, known := draftStableIDFields[field]
	if !known {
		return draftMissingValue, []PromotionConflict{{
			Path:         displayDraftPath(path),
			BaseValue:    deepCopyJSONValue(base),
			CurrentValue: deepCopyJSONValue(current),
			DraftValue:   deepCopyJSONValue(draft),
		}}, nil
	}

	baseIdx, ok := indexStableArray(base, idField)
	if !ok {
		return draftMissingValue, arrayConflict(path, base, current, draft), nil
	}
	currentIdx, ok := indexStableArray(current, idField)
	if !ok {
		return draftMissingValue, arrayConflict(path, base, current, draft), nil
	}
	draftIdx, ok := indexStableArray(draft, idField)
	if !ok {
		return draftMissingValue, arrayConflict(path, base, current, draft), nil
	}

	baseIDs, currentIDs, draftIDs := baseIdx.ids, currentIdx.ids, draftIdx.ids
	currentOrderChanged := relativeOrderChanged(baseIDs, currentIDs)
	draftOrderChanged := relativeOrderChanged(baseIDs, draftIDs)
	if currentOrderChanged && draftOrderChanged &&
		!equalStrings(relativeOrder(baseIDs, currentIDs), relativeOrder(baseIDs, draftIDs)) {
		return draftMissingValue, arrayConflict(path, base, current, draft), nil
	}

	mergedByID := map[string]any{}
	var conflicts []PromotionConflict
	var autoMerged []string
	for _, itemID := range orderedUnique(concatStrings(baseIDs, currentIDs, draftIDs)) {
		item, itemConflicts, itemPaths := mergeJSONValue(
			baseIdx.lookup(itemID),
			currentIdx.lookup(itemID),
			draftIdx.lookup(itemID),
			path+"/"+escapePointerSegment(itemID),
		)
		conflicts = append(conflicts, itemConflicts...)
		autoMerged = append(autoMerged, itemPaths...)
		if !isDraftMissing(item) {
			mergedByID[itemID] = item
		}
	}
	if len(conflicts) > 0 {
		return draftMissingValue, conflicts, autoMerged
	}

	preferredOrder := currentIDs
	if draftOrderChanged && !currentOrderChanged {
		preferredOrder = draftIDs
	}
	mergedOrder := orderedUnique(concatStrings(preferredOrder, currentIDs, draftIDs, baseIDs))
	out := make([]any, 0, len(mergedOrder))
	for _, itemID := range mergedOrder {
		if item, ok := mergedByID[itemID]; ok {
			out = append(out, item)
		}
	}
	return out, nil, autoMerged
}

// stableIndex 稳定 ID 索引：ids 保留数组顺序（Python dict 的插入序语义），
// byID 供按 ID 取元素。
type stableIndex struct {
	ids  []string
	byID map[string]map[string]any
}

func (index stableIndex) lookup(id string) any {
	if object, ok := index.byID[id]; ok {
		return object
	}
	return draftMissingValue
}

// indexStableArray 按稳定 ID 建索引；任一条目不合法（非对象 / 缺 ID / 重复 ID）即失败。
func indexStableArray(items []any, idField string) (stableIndex, bool) {
	index := stableIndex{byID: map[string]map[string]any{}}
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return stableIndex{}, false
		}
		rawID, ok := object[idField].(string)
		if !ok || rawID == "" {
			return stableIndex{}, false
		}
		if _, duplicate := index.byID[rawID]; duplicate {
			return stableIndex{}, false
		}
		index.ids = append(index.ids, rawID)
		index.byID[rawID] = object
	}
	return index, true
}

// arrayConflict 整个数组作为一个冲突单元（非稳定 ID 数组 / 索引失败 / 顺序双改）。
func arrayConflict(path string, base, current, draft []any) []PromotionConflict {
	return []PromotionConflict{{
		Path:         displayDraftPath(path),
		BaseValue:    deepCopyJSONValue(base),
		CurrentValue: deepCopyJSONValue(current),
		DraftValue:   deepCopyJSONValue(draft),
	}}
}

func relativeOrderChanged(baseIDs, candidateIDs []string) bool {
	baseOrder := relativeOrder(baseIDs, candidateIDs)
	candidateOrder := relativeOrder(candidateIDs, baseIDs)
	return !equalStrings(baseOrder, candidateOrder)
}

func relativeOrder(baseIDs, candidateIDs []string) []string {
	inBase := map[string]bool{}
	for _, id := range baseIDs {
		inBase[id] = true
	}
	out := []string{}
	for _, id := range candidateIDs {
		if inBase[id] {
			out = append(out, id)
		}
	}
	return out
}

func sortedUnionKeys(maps ...map[string]any) []string {
	seen := map[string]bool{}
	var keys []string
	for _, m := range maps {
		for key := range m {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func orderedUnique(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func concatStrings(lists ...[]string) []string {
	out := []string{}
	for _, list := range lists {
		out = append(out, list...)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mapValueOrMissing(m map[string]any, key string) any {
	if value, ok := m[key]; ok {
		return value
	}
	return draftMissingValue
}

func isDraftMissing(value any) bool {
	_, ok := value.(draftMissing)
	return ok
}

func conflictValue(value any) any {
	if isDraftMissing(value) {
		return nil
	}
	return deepCopyJSONValue(value)
}

func jsonPointerSegment(parent, key string) string {
	return parent + "/" + escapePointerSegment(key)
}

func escapePointerSegment(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	return strings.ReplaceAll(value, "/", "~1")
}

func displayDraftPath(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

// --- JSON 值工具（深拷贝 + Python 口径相等） ---

func deepCopyJSONObject(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	out := make(map[string]any, len(value))
	for key, item := range value {
		out[key] = deepCopyJSONValue(item)
	}
	return out
}

func deepCopyJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return deepCopyJSONObject(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = deepCopyJSONValue(item)
		}
		return out
	default:
		return value
	}
}

// jsonEqual 复刻 Python 的 JSON 值相等：数字按数值比（1 == 1.0），map 按键值比，
// 缺失哨兵只与自身相等。注意 Go 的 == 对 map/slice 会 panic，必须走这里。
func jsonEqual(a, b any) bool {
	if isDraftMissing(a) || isDraftMissing(b) {
		return isDraftMissing(a) && isDraftMissing(b)
	}
	return reflect.DeepEqual(draftNormalizeJSON(a), draftNormalizeJSON(b))
}

// draftNormalizeJSON 把数字统一成 float64；其余结构原样保留（map/slice 由 reflect 深比）。
func draftNormalizeJSON(value any) any {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int8:
		return float64(typed)
	case int16:
		return float64(typed)
	case int32:
		return float64(typed)
	case int64:
		return float64(typed)
	case uint:
		return float64(typed)
	case uint8:
		return float64(typed)
	case uint16:
		return float64(typed)
	case uint32:
		return float64(typed)
	case uint64:
		return float64(typed)
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = draftNormalizeJSON(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = draftNormalizeJSON(item)
		}
		return out
	default:
		return value
	}
}
