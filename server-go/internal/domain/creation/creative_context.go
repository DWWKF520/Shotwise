package creation

// 创意板创建上下文的 fail-closed 解析（对应 lib/creative_context.py）。
//
// 自然语言引用只对照显式给出的项目与创意板候选解析；一条引用必须恰好命中
// 一个稳定 ID，才能进入带门禁的创建上下文解析器。

import (
	"encoding/json"
	"fmt"
	"github.com/shotwise/shotwise/internal/domain/media"
	"strings"
)

// creativeResourceInputs 是资源类型 → 输入槽位（对应 _RESOURCE_INPUTS）。
var creativeResourceInputs = map[string]string{
	"document":    "document",
	"brief":       "brief",
	"image":       "image",
	"media_asset": "media_asset",
	"media_image": "image",
	"character":   "image",
	"scene":       "image",
	"prop":        "image",
	"product":     "image",
	"video":       "video",
	"audio":       "audio",
	"shot":        "image",
	"storyboard":  "image",
}

var creativeMediaResourceTypes = map[string]bool{
	"image": true, "media_image": true, "media_asset": true, "video": true, "audio": true,
}

var creativeApprovedStatuses = map[string]bool{
	"approved": true, "confirmed": true, "passed": true, "succeeded": true,
}

var creativePreviewedPlanStatuses = map[string]bool{
	"previewed": true, "confirmed": true, "started": true, "running": true, "succeeded": true,
}

// CreativeContextResolutionError 表示上下文无法解析为无歧义的资源 ID。
type CreativeContextResolutionError struct {
	Code    string
	Details map[string]any
}

func (e *CreativeContextResolutionError) Error() string { return e.Code }

func newResolutionError(code string, details map[string]any) *CreativeContextResolutionError {
	if details == nil {
		details = map[string]any{}
	}
	return &CreativeContextResolutionError{Code: code, Details: details}
}

// SelectedResource 是一个已选资源（稳定 ID + 资源类型）。
type SelectedResource struct {
	ID           string
	ResourceType string
}

// ContextReference 是必须解析到稳定项目 ID 的用户可见引用。
type ContextReference struct {
	Text         string
	ExpectedType *string
}

// --- 通用取值小工具（对应 _value/_decoded/_mapping/_sequence） ---

func ctxValue(record any, key string) any {
	if m, ok := record.(map[string]any); ok {
		return m[key]
	}
	return nil
}

func ctxValueOr(record any, key string, def any) any {
	if m, ok := record.(map[string]any); ok {
		if v, present := m[key]; present {
			return v
		}
	}
	return def
}

func ctxDecoded(value any) any {
	s, ok := value.(string)
	if !ok {
		return value
	}
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func ctxMapping(value any) map[string]any {
	decoded := ctxDecoded(value)
	if m, ok := decoded.(map[string]any); ok {
		return m
	}
	return nil
}

func ctxSequence(value any) []any {
	if list, ok := value.([]any); ok {
		return list
	}
	return nil
}

// ctxStringSet 对应 _ids：从 Mapping 或序列收集稳定 ID 集合。
func ctxStringSet(records any, field string) map[string]bool {
	var values []any
	if m, ok := records.(map[string]any); ok {
		values = make([]any, 0, len(m))
		for _, v := range m {
			values = append(values, v)
		}
	} else {
		values = ctxSequence(records)
	}
	result := map[string]bool{}
	for _, record := range values {
		switch v := record.(type) {
		case string:
			result[v] = true
		case float64:
			result[strconvFormatFloat(v)] = true
		default:
			identifier := ctxValue(record, "id")
			if identifier == nil {
				identifier = ctxValue(record, field+"_id")
			}
			if identifier == nil {
				identifier = ctxValue(record, field)
			}
			if identifier != nil {
				result[ctxToString(identifier)] = true
			}
		}
	}
	return result
}

func ctxToString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return strconvFormatFloat(v)
	case bool:
		if v {
			return "True"
		}
		return "False"
	case nil:
		return "None"
	default:
		return fmt.Sprint(v)
	}
}

func strconvFormatFloat(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%v", v)
}

// projectEntities 对应 _project_entities：project[field] 或 project[field+"s"]。
func projectEntities(project map[string]any, field string) any {
	if v, ok := project[field]; ok {
		return v
	}
	return project[field+"s"]
}

func validateCreativeProjectID(projectID string, project map[string]any) error {
	declared := ctxValue(project, "id")
	if isFalsy(declared) {
		declared = ctxValue(project, "project_id")
	}
	if declared != nil && ctxToString(declared) != projectID {
		return newResolutionError("project_context_mismatch", map[string]any{
			"project_id": projectID, "declared_project_id": ctxToString(declared),
		})
	}
	return nil
}

// isFalsy 对应 Python 的 or 短路：None / 空串 / 0 / false 视为缺省。
func isFalsy(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case bool:
		return !v
	case float64:
		return v == 0
	default:
		return false
	}
}

func validateEpisodeAndShot(project map[string]any, episodeID, shotID *string) error {
	if shotID != nil && *shotID != "" && (episodeID == nil || *episodeID == "") {
		return newResolutionError("episode_id_required_for_shot", map[string]any{"shot_id": *shotID})
	}
	episodes := projectEntities(project, "episode")
	if episodeID != nil && *episodeID != "" && episodes != nil {
		if !ctxStringSet(episodes, "episode")[*episodeID] {
			return newResolutionError("episode_not_found", map[string]any{"episode_id": *episodeID})
		}
	}
	var shots any
	if s, ok := projectEntities(project, "shot").([]any); ok && s != nil {
		shots = s
	} else if sm, ok := projectEntities(project, "shot").(map[string]any); ok && sm != nil {
		shots = sm
	}
	if shots == nil && episodes != nil {
		nested := []any{}
		var episodeValues []any
		if em, ok := episodes.(map[string]any); ok {
			episodeValues = make([]any, 0, len(em))
			for _, v := range em {
				episodeValues = append(episodeValues, v)
			}
		} else {
			episodeValues = ctxSequence(episodes)
		}
		for _, episode := range episodeValues {
			raw := ctxValueOr(episode, "shots", ctxValueOr(episode, "shot", []any{}))
			if rm, ok := raw.(map[string]any); ok {
				for _, v := range rm {
					nested = append(nested, v)
				}
			} else {
				nested = append(nested, ctxSequence(raw)...)
			}
		}
		shots = nested
	}
	if shotID != nil && *shotID != "" && shots != nil {
		if !ctxStringSet(shots, "shot")[*shotID] {
			return newResolutionError("shot_not_found", map[string]any{
				"shot_id": *shotID, "episode_id": episodeID,
			})
		}
	}
	return nil
}

func validateCreativeBoard(
	projectID string, boardID string, board map[string]any,
	selected []map[string]string, boardItems []any,
) error {
	if strings.TrimSpace(boardID) == "" {
		return newResolutionError("creative_board_id_required", nil)
	}
	if board == nil {
		return nil
	}
	declaredID := ctxValue(board, "id")
	if isFalsy(declaredID) {
		declaredID = ctxValue(board, "creative_board_id")
	}
	if declaredID != nil && ctxToString(declaredID) != boardID {
		return newResolutionError("creative_board_context_mismatch", map[string]any{"creative_board_id": boardID})
	}
	declaredProject := ctxValue(board, "project_id")
	if declaredProject != nil && ctxToString(declaredProject) != projectID {
		return newResolutionError("creative_board_project_mismatch", map[string]any{"project_id": projectID})
	}
	if len(boardItems) == 0 {
		if items, ok := board["items"].([]any); ok {
			boardItems = items
		}
	}
	for _, resource := range selected {
		matches := 0
		for _, item := range boardItems {
			if ctxToString(ctxValueOr(item, "resource_id", "")) == resource["id"] ||
				ctxToString(ctxValueOr(item, "id", "")) == resource["id"] {
				matches++
			}
		}
		if matches == 0 {
			return newResolutionError("resource_not_on_creative_board", map[string]any{
				"creative_board_id": boardID, "resource_id": resource["id"],
			})
		}
		if matches > 1 {
			return newResolutionError("ambiguous_board_resource", map[string]any{
				"creative_board_id": boardID, "resource_id": resource["id"],
			})
		}
	}
	return nil
}

func validateMediaAssets(project map[string]any, selected []map[string]string, mediaAssets any) error {
	source := mediaAssets
	if source == nil {
		source = project["media_assets"]
	}
	if source == nil {
		return nil
	}
	byID := map[string]any{}
	if m, ok := source.(map[string]any); ok {
		for k, v := range m {
			byID[k] = v
		}
	} else {
		for _, asset := range ctxSequence(source) {
			if id := ctxValue(asset, "id"); id != nil {
				byID[ctxToString(id)] = asset
			}
		}
	}
	for _, resource := range selected {
		if !creativeMediaResourceTypes[resource["resource_type"]] {
			continue
		}
		asset := byID[resource["id"]]
		if asset == nil {
			return newResolutionError("media_asset_not_found", map[string]any{"media_asset_id": resource["id"]})
		}
		kind := strings.ToLower(ctxToString(ctxValueOr(asset, "kind", ctxValueOr(asset, "media_kind", ""))))
		expected := "image"
		switch resource["resource_type"] {
		case "video":
			expected = "video"
		case "audio":
			expected = "audio"
		}
		if kind != "" && resource["resource_type"] != "media_asset" && kind != expected {
			return newResolutionError("media_asset_kind_mismatch", map[string]any{"media_asset_id": resource["id"]})
		}
	}
	return nil
}

func planSkillID(plan any) *string {
	if skillID := ctxValue(plan, "skill_id"); skillID != nil && ctxToString(skillID) != "" {
		s := ctxToString(skillID)
		return &s
	}
	versionID := ctxToString(ctxValueOr(plan, "creation_skill_version_id", ""))
	if versionID != "" {
		if idx := strings.Index(versionID, ":"); idx >= 0 {
			s := versionID[:idx]
			return &s
		}
		return &versionID
	}
	return nil
}

func planSnapshotMapping(plan any) map[string]any {
	for _, key := range []string{"project_snapshot", "project_context", "project_snapshot_json"} {
		if snapshot := ctxMapping(ctxValue(plan, key)); snapshot != nil {
			return snapshot
		}
	}
	return nil
}

func planResourceIDs(plan any) map[string]bool {
	raw := ctxValueOr(plan, "resource_ids", ctxValueOr(plan, "resource_ids_json", []any{}))
	decoded := ctxDecoded(raw)
	if m, ok := decoded.(map[string]any); ok {
		decoded = ctxValueOr(m, "resource_ids", []any{})
	}
	out := map[string]bool{}
	for _, item := range ctxSequence(decoded) {
		id := ctxValueOr(item, "id", item)
		out[ctxToString(id)] = true
	}
	return out
}

func planPreviewed(plan any, explicit *bool) bool {
	if explicit != nil {
		return *explicit
	}
	if v, ok := ctxValue(plan, "previewed").(bool); ok && v {
		return true
	}
	if creativePreviewedPlanStatuses[strings.ToLower(ctxToString(ctxValueOr(plan, "status", "")))] {
		return true
	}
	return ctxValue(plan, "preview") != nil || ctxValue(plan, "preview_json") != nil
}

// gateResult 对应 _gate 的返回：(result, required)。
func gateResult(value any) (*bool, bool) {
	decoded := ctxDecoded(value)
	switch v := decoded.(type) {
	case map[string]any:
		required := false
		switch r := v["required"].(type) {
		case bool:
			required = r
		}
		if !required {
			switch e := v["enabled"].(type) {
			case bool:
				required = e
			}
		}
		var result *bool
		raw := v["passed"]
		if raw == nil {
			raw = v["ok"]
		}
		if b, ok := raw.(bool); ok {
			result = &b
		}
		return result, required
	case bool:
		return &v, true
	case string:
		b := creativeApprovedStatuses[strings.ToLower(v)]
		return &b, true
	default:
		return nil, false
	}
}

type planGateParams struct {
	projectID               string
	project                 map[string]any
	selected                []map[string]string
	currentSkillID          *string
	creationPlanID          string
	creationPlan            any
	workflowRunID           *string
	workflowRun             any
	previewed               *bool
	confirmed               bool
	qualityGatePassed       *bool
	approvalStatus          *string
	allowGateBypass         bool
	requestedGenerationMode *string
}

func validatePlanAndGates(p planGateParams) (string, map[string]any, error) {
	if strings.TrimSpace(p.creationPlanID) == "" {
		return "", nil, newResolutionError("creation_plan_required", nil)
	}
	if p.allowGateBypass {
		return "", nil, newResolutionError("gate_bypass_forbidden", nil)
	}
	if p.creationPlan == nil {
		return "", nil, newResolutionError("creation_plan_details_required",
			map[string]any{"creation_plan_id": p.creationPlanID})
	}
	planID := ctxValueOr(p.creationPlan, "id", ctxValue(p.creationPlan, "plan_id"))
	if planID != nil && ctxToString(planID) != p.creationPlanID {
		return "", nil, newResolutionError("creation_plan_context_mismatch", nil)
	}
	planProjectID := ctxValue(p.creationPlan, "project_id")
	if planProjectID != nil && ctxToString(planProjectID) != p.projectID {
		return "", nil, newResolutionError("creation_plan_project_mismatch", nil)
	}
	status := strings.ToLower(ctxToString(ctxValueOr(p.creationPlan, "status", "")))
	if status == "cancelled" || status == "invalidated" {
		return "", nil, newResolutionError("creation_plan_unavailable", map[string]any{"status": status})
	}
	if !planPreviewed(p.creationPlan, p.previewed) {
		return "", nil, newResolutionError("creation_plan_preview_required", nil)
	}
	if p.requestedGenerationMode != nil {
		return "", nil, newResolutionError("generation_mode_override_forbidden", nil)
	}

	if snapshot := planSnapshotMapping(p.creationPlan); snapshot != nil {
		if snapshotProjectID := ctxValue(snapshot, "project_id"); snapshotProjectID != nil &&
			ctxToString(snapshotProjectID) != p.projectID {
			return "", nil, newResolutionError("creation_plan_project_mismatch", nil)
		}
		for _, field := range []string{"content_mode", "generation_mode", "grid_storyboard", "aspect_ratio", "style", "model_config"} {
			expected := ctxValue(snapshot, field)
			if expected == nil {
				continue
			}
			actual := p.project[field]
			if field == "model_config" && actual == nil {
				actual = p.project["model_config_snapshot"]
			}
			if !ctxJSONEqual(actual, expected) {
				code := "project_snapshot_changed"
				if field == "generation_mode" {
					code = "generation_mode_changed"
				}
				return "", nil, newResolutionError(code, map[string]any{"field": field})
			}
		}
	}

	planSkill := planSkillID(p.creationPlan)
	if p.currentSkillID != nil && *p.currentSkillID != "" && planSkill != nil && *p.currentSkillID != *planSkill {
		return "", nil, newResolutionError("creation_plan_skill_mismatch", map[string]any{
			"creation_plan_skill_id": *planSkill, "current_skill_id": *p.currentSkillID,
		})
	}
	effectiveSkillID := ""
	if p.currentSkillID != nil && *p.currentSkillID != "" {
		effectiveSkillID = *p.currentSkillID
	} else if planSkill != nil {
		effectiveSkillID = *planSkill
	}
	if effectiveSkillID == "" {
		return "", nil, newResolutionError("creation_plan_skill_required", nil)
	}
	skill := CreationSkillByID(effectiveSkillID)
	if skill == nil {
		return "", nil, newResolutionError("unknown_creation_skill", map[string]any{"skill_id": effectiveSkillID})
	}
	availableInputs := map[string]bool{}
	for _, item := range p.selected {
		if input, ok := creativeResourceInputs[item["resource_type"]]; ok {
			availableInputs[input] = true
		}
	}
	report := CompatibilityReport(skill, p.project, availableInputs)
	if persisted := ctxMapping(ctxValue(p.creationPlan, "compatibility_report")); persisted != nil {
		if compatible, ok := persisted["compatible"].(bool); ok && !compatible {
			report = persisted
		}
	}
	if compatible, ok := report["compatible"].(bool); !ok || !compatible {
		return "", nil, newResolutionError("creation_skill_incompatible", report)
	}
	plannedIDs := planResourceIDs(p.creationPlan)
	selectedIDs := map[string]bool{}
	for _, item := range p.selected {
		selectedIDs[item["id"]] = true
	}
	if len(plannedIDs) > 0 && !stringSetEqual(plannedIDs, selectedIDs) {
		return "", nil, newResolutionError("creation_plan_resources_mismatch", nil)
	}

	parameters := ctxMapping(ctxValueOr(p.creationPlan, "parameters", ctxValue(p.creationPlan, "parameters_json")))
	if parameters == nil {
		parameters = map[string]any{}
	}
	forbidden := map[string]bool{
		"generation_mode_override": true, "content_mode_override": true, "grid_storyboard_override": true,
	}
	for key := range parameters {
		if forbidden[key] || strings.HasSuffix(key, "_override") {
			return "", nil, newResolutionError("gate_bypass_forbidden", nil)
		}
	}

	rawCost := ctxValueOr(p.creationPlan, "estimated_cost", float64(0))
	estimatedCost := 0.0
	switch v := rawCost.(type) {
	case float64:
		estimatedCost = v
	case string:
		var err error
		if _, err = fmt.Sscanf(v, "%g", &estimatedCost); err != nil {
			estimatedCost = 0
		}
	}
	requiresConfirmation := false
	if rc, ok := ctxValue(p.creationPlan, "requires_confirmation").(bool); ok && rc {
		requiresConfirmation = true
	}
	if estimatedCost > 0 {
		requiresConfirmation = true
	}
	planConfirmed := false
	if c, ok := ctxValue(p.creationPlan, "confirmed").(bool); ok && c {
		planConfirmed = true
	}
	if status == "confirmed" {
		planConfirmed = true
	}
	if requiresConfirmation && !p.confirmed && !planConfirmed {
		return "", nil, newResolutionError("confirmation_required", map[string]any{
			"estimated_cost": estimatedCost,
		})
	}

	qualityValue := ctxValueOr(p.creationPlan, "quality_gate", ctxValue(p.creationPlan, "quality_gate_status"))
	qualityResult, qualityRequired := gateResult(qualityValue)
	if p.qualityGatePassed != nil {
		qualityResult = p.qualityGatePassed
	}
	if qualityResult != nil && !*qualityResult {
		return "", nil, newResolutionError("quality_gate_failed", nil)
	}
	if qualityRequired && (qualityResult == nil || !*qualityResult) {
		return "", nil, newResolutionError("quality_gate_required", nil)
	}

	reviewPoints := ctxDecoded(ctxValueOr(p.creationPlan, "review_points", []any{}))
	approvalValue := ctxValueOr(p.creationPlan, "approval", ctxValue(p.creationPlan, "approval_status"))
	approvalResult, approvalRequired := gateResult(approvalValue)
	if len(ctxSequence(reviewPoints)) > 0 {
		approvalRequired = true
	}
	if p.approvalStatus != nil {
		b := creativeApprovedStatuses[strings.ToLower(*p.approvalStatus)]
		approvalResult = &b
	}
	if approvalRequired && (approvalResult == nil || !*approvalResult) {
		return "", nil, newResolutionError("approval_required", nil)
	}

	runInfo := map[string]any{}
	if p.workflowRunID != nil && *p.workflowRunID != "" {
		if p.workflowRun == nil {
			return "", nil, newResolutionError("workflow_run_details_required", nil)
		}
		runID := ctxValueOr(p.workflowRun, "id", ctxValue(p.workflowRun, "workflow_run_id"))
		if runID != nil && ctxToString(runID) != *p.workflowRunID {
			return "", nil, newResolutionError("workflow_run_context_mismatch", nil)
		}
		if runProjectID := ctxValue(p.workflowRun, "project_id"); runProjectID != nil &&
			ctxToString(runProjectID) != p.projectID {
			return "", nil, newResolutionError("workflow_run_project_mismatch", nil)
		}
		if ctxValue(p.workflowRun, "error_code") == "quality_gate_failed" {
			return "", nil, newResolutionError("quality_gate_failed", nil)
		}
		if strings.ToLower(ctxToString(ctxValueOr(p.workflowRun, "status", ""))) == "waiting_review" &&
			(approvalResult == nil || !*approvalResult) {
			return "", nil, newResolutionError("approval_required", nil)
		}
		runMode := ctxValue(p.workflowRun, "generation_mode")
		if runMode != nil && runMode != p.project["generation_mode"] {
			return "", nil, newResolutionError("generation_mode_changed", nil)
		}
		runInfo = map[string]any{"id": *p.workflowRunID, "status": ctxValue(p.workflowRun, "status")}
	}

	confirmed := p.confirmed || planConfirmed || !requiresConfirmation
	approvalStatusOut := "not_required"
	if p.approvalStatus != nil {
		approvalStatusOut = *p.approvalStatus
	} else if approvalResult != nil && *approvalResult {
		approvalStatusOut = "approved"
	}
	var qualityOut any
	if qualityResult != nil {
		qualityOut = *qualityResult
	}
	return effectiveSkillID, map[string]any{
		"previewed":             true,
		"confirmed":             confirmed,
		"quality_gate_passed":   qualityOut,
		"approval_status":       approvalStatusOut,
		"requires_confirmation": requiresConfirmation,
		"workflow_run":          runInfo,
	}, nil
}

func stringSetEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// ctxJSONEqual 用规范化 JSON 比较两个任意值（Python 侧直接 == 比较 dict/list）。
func ctxJSONEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ab, errA := media.CanonicalJSON(a)
	bb, errB := media.CanonicalJSON(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ab) == string(bb)
}

// --- 引用解析（候选构建与匹配） ---

func normalizeReference(value any) string {
	if value == nil {
		return ""
	}
	text := ""
	if s, ok := value.(string); ok {
		text = s
	} else {
		text = ctxToString(value)
	}
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(text))), " ")
}

func canonicalResourceType(resourceType, itemType, mediaKind any) string {
	value := strings.ToLower(strings.TrimSpace(orEmptyString(resourceType)))
	item := strings.ToLower(strings.TrimSpace(orEmptyString(itemType)))
	kind := strings.ToLower(strings.TrimSpace(orEmptyString(mediaKind)))
	switch {
	case value == "character" || value == "characters" || item == "character" || item == "characters":
		return "character"
	case value == "scene" || value == "scenes" || item == "scene" || item == "scenes":
		return "scene"
	case value == "prop" || value == "props" || item == "prop" || item == "props":
		return "prop"
	case value == "shot" || value == "shots" || item == "shot" || item == "shots":
		return "shot"
	case value == "video" || value == "media_video" || kind == "video":
		return "video"
	case value == "audio" || value == "media_audio" || kind == "audio":
		return "audio"
	case value == "image" || value == "media_image" || kind == "image":
		return "image"
	case value == "media_asset":
		return "media_asset"
	default:
		return value
	}
}

// orEmptyString 对应 Python 的 str(x or "")：None → ""。
func orEmptyString(v any) string {
	if isFalsy(v) {
		return ""
	}
	return ctxToString(v)
}

type creativeCandidate struct {
	ID           string
	ResourceType string
	Labels       map[string]bool
	EpisodeID    any
	Gender       string
}

func buildCandidate(resourceID any, resourceType string, labels []any, episodeID any, gender any) *creativeCandidate {
	if resourceID == nil || strings.TrimSpace(ctxToString(resourceID)) == "" {
		return nil
	}
	normalized := map[string]bool{}
	for _, label := range labels {
		if n := normalizeReference(label); n != "" {
			normalized[n] = true
		}
	}
	return &creativeCandidate{
		ID:           ctxToString(resourceID),
		ResourceType: resourceType,
		Labels:       normalized,
		EpisodeID:    episodeID,
		Gender:       normalizeReference(gender),
	}
}

// entityEntries 把 Mapping 或序列统一成 (key, entity) 流（key 对应 Mapping 键或 nil）。
func entityEntries(entities any) []struct {
	Key    any
	Entity any
} {
	out := []struct {
		Key    any
		Entity any
	}{}
	if m, ok := entities.(map[string]any); ok {
		for k, v := range m {
			out = append(out, struct {
				Key    any
				Entity any
			}{k, v})
		}
		return out
	}
	for _, item := range ctxSequence(entities) {
		out = append(out, struct {
			Key    any
			Entity any
		}{nil, item})
	}
	return out
}

func namedEntityCandidates(project map[string]any, field string) []*creativeCandidate {
	entities := projectEntities(project, field)
	if entities == nil {
		return nil
	}
	var result []*creativeCandidate
	for _, entry := range entityEntries(entities) {
		entity := entry.Entity
		if entity == nil {
			continue
		}
		identifier := ctxValue(entity, "id")
		if identifier == nil {
			identifier = ctxValue(entity, field+"_id")
		}
		if identifier == nil {
			identifier = entry.Key
		}
		labels := []any{entry.Key, ctxValue(entity, "name"), ctxValue(entity, "label"), ctxValue(entity, "title")}
		if aliases := ctxValue(entity, "aliases"); aliases != nil {
			labels = append(labels, ctxSequence(aliases)...)
		}
		if item := buildCandidate(identifier, field, labels, nil, ctxValue(entity, "gender")); item != nil {
			result = append(result, item)
		}
	}
	return result
}

func shotCandidates(project map[string]any) []*creativeCandidate {
	episodes := projectEntities(project, "episode")
	var result []*creativeCandidate
	seen := map[string]bool{}
	if episodes != nil {
		for _, entry := range entityEntries(episodes) {
			episode := entry.Entity
			if episode == nil {
				continue
			}
			episodeID := ctxValue(episode, "id")
			if episodeID == nil {
				episodeID = ctxValue(episode, "episode_id")
			}
			if episodeID == nil {
				episodeID = entry.Key
			}
			shots := ctxValueOr(episode, "shots", ctxValueOr(episode, "shot", []any{}))
			for _, shotEntry := range entityEntries(shots) {
				shot := shotEntry.Entity
				if shot == nil {
					continue
				}
				identifier := ctxValue(shot, "id")
				if identifier == nil {
					identifier = ctxValue(shot, "shot_id")
				}
				if identifier == nil {
					identifier = shotEntry.Key
				}
				labels := []any{shotEntry.Key, ctxValue(shot, "name"), ctxValue(shot, "label"), ctxValue(shot, "title")}
				if item := buildCandidate(identifier, "shot", labels, episodeID, nil); item != nil {
					result = append(result, item)
				}
			}
		}
	}
	if topLevel := projectEntities(project, "shot"); topLevel != nil {
		for _, entry := range entityEntries(topLevel) {
			shot := entry.Entity
			if shot == nil {
				continue
			}
			identifier := ctxValue(shot, "id")
			if identifier == nil {
				identifier = ctxValue(shot, "shot_id")
			}
			if identifier == nil {
				identifier = entry.Key
			}
			labels := []any{entry.Key, ctxValue(shot, "name"), ctxValue(shot, "label"), ctxValue(shot, "title")}
			if item := buildCandidate(identifier, "shot", labels, ctxValue(shot, "episode_id"), nil); item != nil {
				if !seen[item.ID] {
					seen[item.ID] = true
					result = append(result, item)
				}
			}
		}
	}
	return result
}

func mediaAssetsByID(mediaAssets any) map[string]any {
	byID := map[string]any{}
	if m, ok := mediaAssets.(map[string]any); ok {
		for k, v := range m {
			byID[k] = v
		}
		return byID
	}
	for _, asset := range ctxSequence(mediaAssets) {
		if id := ctxValue(asset, "id"); id != nil {
			byID[ctxToString(id)] = asset
		}
	}
	return byID
}

func boardCandidates(boardItems []any, mediaAssets any) []*creativeCandidate {
	assetsByID := mediaAssetsByID(mediaAssets)
	var result []*creativeCandidate
	for _, item := range boardItems {
		metadata := ctxMapping(ctxValueOr(item, "display_settings_json", ctxValue(item, "display_settings")))
		if metadata == nil {
			metadata = map[string]any{}
		}
		resourceID := ctxValue(item, "resource_id")
		if resourceID == nil {
			resourceID = ctxValue(item, "id")
		}
		asset := assetsByID[ctxToString(resourceID)]
		var mediaKind any
		if asset != nil {
			mediaKind = ctxValueOr(asset, "kind", ctxValue(asset, "media_kind"))
		} else {
			mediaKind = metadata["kind"]
		}
		resourceType := canonicalResourceType(ctxValue(item, "resource_type"), ctxValue(item, "item_type"), mediaKind)
		episodeID := ctxValue(item, "episode_id")
		if episodeID == nil {
			episodeID = metadata["episode_id"]
		}
		if candidate := buildCandidate(resourceID, resourceType, []any{
			ctxValue(item, "name"), ctxValue(item, "label"), ctxValue(item, "title"),
			metadata["name"], metadata["label"], metadata["title"],
		}, episodeID, metadata["gender"]); candidate != nil {
			result = append(result, candidate)
		}
	}
	return result
}

func selectedCandidates(selected []SelectedResource, mediaAssets any) []*creativeCandidate {
	assetsByID := mediaAssetsByID(mediaAssets)
	var result []*creativeCandidate
	for _, resource := range selected {
		asset := assetsByID[resource.ID]
		var mediaKind any
		if asset != nil {
			mediaKind = ctxValueOr(asset, "kind", ctxValue(asset, "media_kind"))
		}
		resourceType := canonicalResourceType(resource.ResourceType, nil, mediaKind)
		if item := buildCandidate(resource.ID, resourceType, nil, nil, nil); item != nil {
			result = append(result, item)
		}
	}
	return result
}

func uniqueCandidates(candidates []*creativeCandidate) []*creativeCandidate {
	type key struct {
		id           string
		resourceType string
	}
	unique := map[key]*creativeCandidate{}
	var order []key
	for _, candidate := range candidates {
		k := key{candidate.ID, candidate.ResourceType}
		existing, ok := unique[k]
		if !ok {
			clone := &creativeCandidate{
				ID: candidate.ID, ResourceType: candidate.ResourceType,
				Labels: map[string]bool{}, EpisodeID: candidate.EpisodeID, Gender: candidate.Gender,
			}
			for l := range candidate.Labels {
				clone.Labels[l] = true
			}
			unique[k] = clone
			order = append(order, k)
			continue
		}
		for l := range candidate.Labels {
			existing.Labels[l] = true
		}
		if existing.EpisodeID == nil {
			existing.EpisodeID = candidate.EpisodeID
		}
		if existing.Gender == "" {
			existing.Gender = candidate.Gender
		}
	}
	out := make([]*creativeCandidate, 0, len(order))
	for _, k := range order {
		out = append(out, unique[k])
	}
	return out
}

func referenceCandidates(
	reference ContextReference,
	project map[string]any,
	selectedResources []SelectedResource,
	boardItems []any,
	mediaAssets any,
) []*creativeCandidate {
	selected := selectedCandidates(selectedResources, mediaAssets)
	board := boardCandidates(boardItems, mediaAssets)
	expected := canonicalResourceType(derefString(reference.ExpectedType), nil, nil)
	text := normalizeReference(reference.Text)

	concat := func(a, b []*creativeCandidate) []*creativeCandidate {
		return append(append([]*creativeCandidate{}, a...), b...)
	}
	switch text {
	case "当前镜头", "current shot", "this shot":
		var out []*creativeCandidate
		for _, item := range concat(selected, board) {
			if item.ResourceType == "shot" {
				out = append(out, item)
			}
		}
		return uniqueCandidates(out)
	case "这个视频", "当前视频", "this video", "current video":
		var out []*creativeCandidate
		for _, item := range concat(selected, board) {
			if item.ResourceType == "video" {
				out = append(out, item)
			}
		}
		return uniqueCandidates(out)
	case "她", "他", "她的", "his", "her", "the character":
		var pool []*creativeCandidate
		for _, item := range concat(selected, board) {
			if item.ResourceType == "character" {
				pool = append(pool, item)
			}
		}
		candidates := uniqueCandidates(pool)
		if len(candidates) == 0 {
			candidates = uniqueCandidates(namedEntityCandidates(project, "character"))
		}
		switch text {
		case "她", "她的", "her":
			var female []*creativeCandidate
			for _, item := range candidates {
				if item.Gender == "female" || item.Gender == "女" || item.Gender == "woman" {
					female = append(female, item)
				}
			}
			if len(female) > 0 {
				candidates = female
			}
		}
		return candidates
	}
	var candidates []*creativeCandidate
	if expected == "" || expected == "character" {
		candidates = append(candidates, namedEntityCandidates(project, "character")...)
	}
	candidates = append(candidates, selected...)
	candidates = append(candidates, board...)
	candidates = uniqueCandidates(candidates)
	var out []*creativeCandidate
	for _, item := range candidates {
		if item.Labels[text] && (expected == "" || item.ResourceType == expected) {
			out = append(out, item)
		}
	}
	return out
}

// ResolveContextReferencesParams 是 ResolveContextReferences 的入参。
type ResolveContextReferencesParams struct {
	ProjectID         string
	Project           map[string]any
	References        []ContextReference
	SelectedResources []SelectedResource
	BoardItems        []any
	MediaAssets       any
	EpisodeID         *string
	ShotID            *string
}

// ResolveContextReferences 在不创建、不修改任何东西的前提下把引用解析为稳定 ID。
func ResolveContextReferences(p ResolveContextReferencesParams) (map[string]any, error) {
	if strings.TrimSpace(p.ProjectID) == "" {
		return nil, newResolutionError("project_id_required", nil)
	}
	if err := validateCreativeProjectID(p.ProjectID, p.Project); err != nil {
		return nil, err
	}
	resolvedResources := append([]SelectedResource{}, p.SelectedResources...)
	var resolvedEpisodeID, resolvedShotID *string
	if p.EpisodeID != nil && strings.TrimSpace(*p.EpisodeID) != "" {
		v := strings.TrimSpace(*p.EpisodeID)
		resolvedEpisodeID = &v
	}
	if p.ShotID != nil && strings.TrimSpace(*p.ShotID) != "" {
		v := strings.TrimSpace(*p.ShotID)
		resolvedShotID = &v
	}
	shotCatalog := shotCandidates(p.Project)
	resolutions := []any{}
	for _, reference := range p.References {
		if strings.TrimSpace(reference.Text) == "" {
			return nil, newResolutionError("context_reference_required", nil)
		}
		text := normalizeReference(reference.Text)
		var candidates []*creativeCandidate
		if (text == "当前镜头" || text == "current shot" || text == "this shot") && resolvedShotID != nil {
			for _, item := range shotCatalog {
				if item.ID == *resolvedShotID {
					candidates = append(candidates, item)
				}
			}
		} else {
			candidates = referenceCandidates(reference, p.Project, resolvedResources, p.BoardItems, p.MediaAssets)
		}
		if len(candidates) == 0 {
			return nil, newResolutionError("context_reference_unresolved", map[string]any{
				"reference": reference.Text, "expected_type": derefString(reference.ExpectedType),
			})
		}
		if len(candidates) > 1 {
			candidatePayloads := make([]any, 0, len(candidates))
			for _, item := range candidates {
				candidatePayloads = append(candidatePayloads, map[string]any{
					"id": item.ID, "resource_type": item.ResourceType,
				})
			}
			return nil, newResolutionError("ambiguous_context_reference", map[string]any{
				"reference":     reference.Text,
				"expected_type": derefString(reference.ExpectedType),
				"candidates":    candidatePayloads,
			})
		}
		match := candidates[0]
		matchedID := match.ID
		matchedType := match.ResourceType
		if matchedType == "shot" {
			candidateEpisodeID := match.EpisodeID
			if resolvedShotID != nil && *resolvedShotID != matchedID {
				return nil, newResolutionError("context_reference_conflict", map[string]any{"reference": reference.Text})
			}
			if resolvedEpisodeID != nil && candidateEpisodeID != nil &&
				*resolvedEpisodeID != ctxToString(candidateEpisodeID) {
				return nil, newResolutionError("context_reference_conflict", map[string]any{"reference": reference.Text})
			}
			resolvedShotID = &matchedID
			if resolvedEpisodeID == nil && candidateEpisodeID != nil {
				v := ctxToString(candidateEpisodeID)
				resolvedEpisodeID = &v
			}
			if resolvedEpisodeID == nil {
				episodeMatches := map[string]bool{}
				for _, item := range shotCatalog {
					if item.ID == matchedID && item.EpisodeID != nil {
						episodeMatches[ctxToString(item.EpisodeID)] = true
					}
				}
				if len(episodeMatches) == 1 {
					for k := range episodeMatches {
						v := k
						resolvedEpisodeID = &v
					}
				}
			}
		}
		already := false
		for _, resource := range resolvedResources {
			if resource.ID == matchedID && canonicalResourceType(resource.ResourceType, nil, nil) == matchedType {
				already = true
				break
			}
		}
		if !already {
			resolvedResources = append(resolvedResources, SelectedResource{ID: matchedID, ResourceType: matchedType})
		}
		resolutions = append(resolutions, map[string]any{
			"reference": reference.Text, "id": matchedID, "resource_type": matchedType,
		})
	}
	selectedPayload := make([]any, 0, len(resolvedResources))
	mediaAssetIDs := []any{}
	for _, item := range resolvedResources {
		selectedPayload = append(selectedPayload, map[string]any{"id": item.ID, "resource_type": item.ResourceType})
		switch canonicalResourceType(item.ResourceType, nil, nil) {
		case "image", "video", "audio", "media_asset":
			mediaAssetIDs = append(mediaAssetIDs, item.ID)
		}
	}
	var episodeOut, shotOut any
	if resolvedEpisodeID != nil {
		episodeOut = *resolvedEpisodeID
	}
	if resolvedShotID != nil {
		shotOut = *resolvedShotID
	}
	return map[string]any{
		"project_id":               p.ProjectID,
		"episode_id":               episodeOut,
		"shot_id":                  shotOut,
		"selected_resources":       selectedPayload,
		"selected_media_asset_ids": mediaAssetIDs,
		"references":               resolutions,
		"disambiguated":            true,
	}, nil
}

func derefString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// ResolveCreationContextParams 是 ResolveCreationContext 的入参。
type ResolveCreationContextParams struct {
	ProjectID              string
	Project                map[string]any
	SelectedResources      []SelectedResource
	CreativeBoardID        string
	EpisodeID              *string
	ShotID                 *string
	CurrentSkillID         *string
	CreationPlanID         string
	WorkflowRunID          *string
	Board                  map[string]any
	BoardItems             []any
	MediaAssets            any
	CreationPlan           any
	WorkflowRun            any
	Previewed              *bool
	Confirmed              bool
	QualityGatePassed      *bool
	ApprovalStatus         *string
	AllowGateBypass        bool
	GenerationMode         *string
	GenerationModeOverride *string
	ContextReferences      []ContextReference
}

// ResolveCreationContext 在不创建、不修改 Creation Plan 或 run 的前提下解析上下文。
func ResolveCreationContext(p ResolveCreationContextParams) (map[string]any, error) {
	if strings.TrimSpace(p.ProjectID) == "" {
		return map[string]any{
			"status": "error", "error": map[string]any{"code": "missing_project_id"}, "resolved": map[string]any{},
		}, nil
	}
	if err := validateCreativeProjectID(p.ProjectID, p.Project); err != nil {
		return nil, err
	}
	referenceResolution := map[string]any{}
	if len(p.ContextReferences) > 0 {
		resolution, err := ResolveContextReferences(ResolveContextReferencesParams{
			ProjectID:         p.ProjectID,
			Project:           p.Project,
			References:        p.ContextReferences,
			SelectedResources: p.SelectedResources,
			BoardItems:        p.BoardItems,
			MediaAssets:       p.MediaAssets,
			EpisodeID:         p.EpisodeID,
			ShotID:            p.ShotID,
		})
		if err != nil {
			return nil, err
		}
		referenceResolution = resolution
		if episodeID, ok := resolution["episode_id"].(string); ok {
			p.EpisodeID = &episodeID
		}
		if shotID, ok := resolution["shot_id"].(string); ok {
			p.ShotID = &shotID
		}
		if resolved, ok := resolution["selected_resources"].([]any); ok {
			resources := make([]SelectedResource, 0, len(resolved))
			for _, raw := range resolved {
				item, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				id, idOK := item["id"].(string)
				resourceType, typeOK := item["resource_type"].(string)
				if idOK && typeOK {
					resources = append(resources, SelectedResource{ID: id, ResourceType: resourceType})
				}
			}
			p.SelectedResources = resources
		}
	}
	if p.WorkflowRunID != nil && *p.WorkflowRunID != "" && p.CreationPlanID == "" {
		return nil, newResolutionError("workflow_run_requires_creation_plan", map[string]any{
			"workflow_run_id": *p.WorkflowRunID,
		})
	}
	if len(p.SelectedResources) == 0 {
		return nil, newResolutionError("resource_selection_required", nil)
	}

	seenIDs := map[string]bool{}
	normalized := []map[string]string{}
	availableInputs := map[string]bool{}
	for _, resource := range p.SelectedResources {
		resourceID := strings.TrimSpace(resource.ID)
		resourceType := strings.ToLower(strings.TrimSpace(resource.ResourceType))
		if resourceID == "" {
			return nil, newResolutionError("resource_id_required", nil)
		}
		if seenIDs[resourceID] {
			return nil, newResolutionError("ambiguous_duplicate_resource", map[string]any{"resource_id": resourceID})
		}
		if _, ok := creativeResourceInputs[resourceType]; !ok {
			return nil, newResolutionError("unknown_resource_type", map[string]any{"resource_type": resourceType})
		}
		seenIDs[resourceID] = true
		availableInputs[creativeResourceInputs[resourceType]] = true
		normalized = append(normalized, map[string]string{"id": resourceID, "resource_type": resourceType})
	}

	if err := validateEpisodeAndShot(p.Project, trimmedOrNull(p.EpisodeID), trimmedOrNull(p.ShotID)); err != nil {
		return nil, err
	}
	if err := validateCreativeBoard(p.ProjectID, p.CreativeBoardID, p.Board, normalized, p.BoardItems); err != nil {
		return nil, err
	}
	if p.CreationPlanID == "" {
		return nil, newResolutionError("creation_plan_required", nil)
	}
	if err := validateMediaAssets(p.Project, normalized, p.MediaAssets); err != nil {
		return nil, err
	}
	available := []any{}
	for _, check := range ListOfficialCreationSkills(p.Project, availableInputs) {
		if check.Reason == "" {
			available = append(available, map[string]any{
				"id":         check.Skill.ID,
				"version_id": check.Skill.LatestVersion.ID,
				"title":      check.Skill.LatestVersion.Title,
				"summary":    check.Skill.LatestVersion.Summary,
			})
		}
	}
	requestedMode := p.GenerationModeOverride
	if requestedMode == nil {
		requestedMode = p.GenerationMode
	}
	effectiveSkillID, gates, err := validatePlanAndGates(planGateParams{
		projectID:               p.ProjectID,
		project:                 p.Project,
		selected:                normalized,
		currentSkillID:          p.CurrentSkillID,
		creationPlanID:          p.CreationPlanID,
		creationPlan:            p.CreationPlan,
		workflowRunID:           p.WorkflowRunID,
		workflowRun:             p.WorkflowRun,
		previewed:               p.Previewed,
		confirmed:               p.Confirmed,
		qualityGatePassed:       p.QualityGatePassed,
		approvalStatus:          p.ApprovalStatus,
		allowGateBypass:         p.AllowGateBypass,
		requestedGenerationMode: requestedMode,
	})
	if err != nil {
		return nil, err
	}
	normalizedPayload := make([]any, 0, len(normalized))
	mediaAssetIDs := []any{}
	for _, item := range normalized {
		normalizedPayload = append(normalizedPayload, map[string]any{
			"id": item["id"], "resource_type": item["resource_type"],
		})
		if creativeMediaResourceTypes[item["resource_type"]] {
			mediaAssetIDs = append(mediaAssetIDs, item["id"])
		}
	}
	var episodeOut, shotOut, runOut any
	if p.EpisodeID != nil {
		episodeOut = *p.EpisodeID
	}
	if p.ShotID != nil {
		shotOut = *p.ShotID
	}
	if p.WorkflowRunID != nil && *p.WorkflowRunID != "" {
		runOut = *p.WorkflowRunID
	}
	contextResolutions, _ := referenceResolution["references"].([]any)
	if contextResolutions == nil {
		contextResolutions = []any{}
	}
	return map[string]any{
		"project_id":                     p.ProjectID,
		"creative_board_id":              p.CreativeBoardID,
		"episode_id":                     episodeOut,
		"shot_id":                        shotOut,
		"selected_resources":             normalizedPayload,
		"selected_media_asset_ids":       mediaAssetIDs,
		"current_skill_id":               effectiveSkillID,
		"creation_plan_id":               p.CreationPlanID,
		"workflow_run_id":                runOut,
		"available_skills":               available,
		"disambiguated":                  true,
		"generation_mode":                p.Project["generation_mode"],
		"gates":                          gates,
		"requires_creation_plan_preview": true,
		"requires_confirmation_for_high_cost_operations": true,
		"context_reference_resolutions":                  contextResolutions,
	}, nil
}

func trimmedOrNull(v *string) *string {
	if v == nil || strings.TrimSpace(*v) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(*v)
	return &trimmed
}
