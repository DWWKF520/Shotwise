package creative

// Creation service.Plan 预览与生命周期（对应 server/services/creation_plans.py）。
//
// 三条不变式：
//  1. 预览是纯函数式的编译 + 落库，没有任何生成副作用；
//  2. 启动前必须复核项目快照——项目任何独占设置变了就必须重新编译；
//  3. 一次计划最多驱动一条 WorkflowRun，重复 start 走去重而不是新建。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

// CreationPlanNotFoundError 表示计划对当前用户不可见（HTTP 404）。
type CreationPlanNotFoundError struct{ ID string }

func (e *CreationPlanNotFoundError) Error() string { return e.ID }

// CreationPlanStartError 表示计划无法启动（HTTP 409）；Report 随错误码一起回传。
type CreationPlanStartError struct {
	Code   string
	Report map[string]any
}

func (e *CreationPlanStartError) Error() string { return e.Code }

// CreationPlanResolver 是能力预检所需的通道解析能力（*config.Resolver 满足）。
type CreationPlanResolver interface {
	ResolveImageBackend(
		ctx context.Context, project *domain.ProjectFile, payload map[string]any, cap config.ImageCapability,
	) (config.ProviderModel, error)
	ResolveVideoBackend(
		ctx context.Context, project *domain.ProjectFile, payload map[string]any, bucket *provider.VideoBucket,
	) (config.ProviderModel, error)
	ResolveAudioBackend(
		ctx context.Context, project *domain.ProjectFile, payload map[string]any,
	) (config.ProviderModel, error)
}

// CreationPlanService 是创作计划服务。
type CreationPlanService struct {
	db        *gorm.DB
	dialect   db.Dialect
	workflows *WorkflowService
	catalog   *CreationSkillCatalog
	resolver  CreationPlanResolver
}

// NewCreationPlanService 构造创作计划服务。resolver 可为 nil（此时能力预检只在
// 图确实声明了能力时降级为不兼容，不会误报兼容）。
func NewCreationPlanService(
	gdb *gorm.DB, dialect db.Dialect, workflows *WorkflowService, resolver CreationPlanResolver,
) *CreationPlanService {
	return &CreationPlanService{
		db:        gdb,
		dialect:   dialect,
		workflows: workflows,
		catalog:   NewCreationSkillCatalog(gdb, workflows),
		resolver:  resolver,
	}
}

// Catalog 暴露目录服务，供路由层复用。
func (s *CreationPlanService) Catalog() *CreationSkillCatalog { return s.catalog }

// --- 资源 ---

// ListCreationResources 暴露可选的项目实体，不带出创作内容本身。
func ListCreationResources(project map[string]any) []map[string]any {
	fields := []struct {
		resourceType string
		names        []string
	}{
		{"document", []string{"documents", "source_files", "sourceFiles"}},
		{"character", []string{"characters"}},
		{"scene", []string{"scenes"}},
		{"prop", []string{"props"}},
		{"product", []string{"products"}},
		{"episode", []string{"episodes"}},
	}
	result := make([]map[string]any, 0)
	for _, field := range fields {
		var value any
		for _, name := range field.names {
			if raw, ok := project[name]; ok {
				value = raw
				break
			}
		}
		var items [][2]any
		switch typed := value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				items = append(items, [2]any{key, typed[key]})
			}
		case []any:
			for index, item := range typed {
				items = append(items, [2]any{index, item})
			}
		default:
			continue
		}
		for _, entry := range items {
			key, item := entry[0], entry[1]
			resourceID := ""
			label := ""
			if mapping, ok := item.(map[string]any); ok {
				resourceID = firstStringValue(mapping, "id", "file_id")
				if resourceID == "" {
					resourceID = stringify(key)
				}
				label = firstStringValue(mapping, "name", "title", "filename")
				if label == "" {
					label = resourceID
				}
			} else {
				resourceID = stringify(key)
				if item == nil {
					label = resourceID
				} else {
					label = stringify(item)
				}
			}
			if strings.TrimSpace(resourceID) != "" {
				result = append(result, map[string]any{"id": resourceID, "label": label, "type": field.resourceType})
			}
		}
	}
	return result
}

func firstStringValue(mapping map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := mapping[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func stringify(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case int:
		return intToString(int64(typed))
	case float64:
		if typed == float64(int64(typed)) {
			return intToString(int64(typed))
		}
		return strings.TrimRight(strings.TrimRight(jsonNumber(typed), "0"), ".")
	default:
		return ""
	}
}

func jsonNumber(value float64) string {
	encoded, err := domain.WorkflowCanonicalJSON(value)
	if err != nil {
		return "0"
	}
	return encoded
}

// --- 预览 ---

// CreationPlanPreviewParams 是预览编译的入参。
type CreationPlanPreviewParams struct {
	UserID                 string
	WorkspaceID            string
	ProjectID              string
	CreationSkillVersionID string
	Project                map[string]any
	ProjectFile            *domain.ProjectFile
	ResourceIDs            []string
	ResourceTypes          []string
	ResourceMapping        []map[string]any
	Parameters             map[string]any
	WorkflowRevision       *string
	EstimatedCost          *float64
	Steps                  []string
	ReviewPoints           []string
	IdempotencyKey         string
}

// CreationPlanPreview 是面向用户的预览，刻意与"启动"分离。
type CreationPlanPreview struct {
	Plan            *domain.CreationPlan
	ResourceIDs     []string
	ResourceMapping []map[string]any
	Parameters      map[string]any
	EstimatedCost   float64
	Steps           []string
	ReviewPoints    []string
}

// ToDict 输出预览响应体。
func (p *CreationPlanPreview) ToDict() map[string]any {
	result := p.Plan.ToDict()
	context := p.Plan.ProjectContext
	mapping := make([]any, 0, len(p.ResourceMapping))
	for _, item := range p.ResourceMapping {
		mapping = append(mapping, item)
	}
	resourceIDs := make([]any, 0, len(p.ResourceIDs))
	for _, item := range p.ResourceIDs {
		resourceIDs = append(resourceIDs, item)
	}
	steps := make([]any, 0, len(p.Steps))
	for _, item := range p.Steps {
		steps = append(steps, item)
	}
	reviewPoints := make([]any, 0, len(p.ReviewPoints))
	for _, item := range p.ReviewPoints {
		reviewPoints = append(reviewPoints, item)
	}
	result["content_mode_snapshot"] = context.ContentMode
	result["generation_mode_snapshot"] = context.GenerationMode
	result["grid_storyboard_snapshot"] = context.GridStoryboard
	result["aspect_ratio_snapshot"] = context.AspectRatio
	result["style_snapshot"] = context.Style
	result["model_config_snapshot"] = context.ToDict()["model_config"]
	result["resource_ids"] = resourceIDs
	result["resource_mapping"] = mapping
	result["parameters"] = p.Parameters
	result["estimated_cost"] = p.EstimatedCost
	result["steps"] = steps
	result["review_points"] = reviewPoints
	return result
}

// CreateCreationPlanPreview 落一条确定性、仅预览的计划，无任何生成副作用。
func (s *CreationPlanService) CreateCreationPlanPreview(
	ctx context.Context, p CreationPlanPreviewParams,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result, err := s.createPreview(ctx, tx, p)
		out = result
		return err
	})
	return out, err
}

func (s *CreationPlanService) createPreview(
	ctx context.Context, tx *gorm.DB, p CreationPlanPreviewParams,
) (map[string]any, error) {
	var existing repository.CreationPlan
	err := tx.Where("user_id = ? AND project_id = ? AND idempotency_key = ?",
		p.UserID, p.ProjectID, p.IdempotencyKey).Take(&existing).Error
	if err == nil {
		return planResponse(&existing, true), nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	availableInputs := map[string]bool{}
	for _, value := range p.ResourceTypes {
		availableInputs[value] = true
	}
	normalizedIDs := normalizeResourceIDs(p.ResourceIDs)
	if len(normalizedIDs) == 0 {
		return nil, &domain.CreationPlanError{Message: "at least one real resource must be selected"}
	}
	normalizedMapping := normalizeResourceMapping(p.ResourceMapping, normalizedIDs, p.ResourceTypes)
	if len(normalizedMapping) > 0 {
		normalizedIDs = make([]string, 0, len(normalizedMapping))
		availableInputs = map[string]bool{}
		for _, item := range normalizedMapping {
			normalizedIDs = append(normalizedIDs, item["id"])
			availableInputs[item["type"]] = true
		}
	}

	skill, skillVersion, err := s.resolveSkillVersion(ctx, tx, p.CreationSkillVersionID)
	if err != nil {
		return nil, err
	}
	revisionID, err := s.resolveWorkflowRevision(ctx, tx, skillVersion, p.WorkflowRevision)
	if err != nil {
		return nil, err
	}
	if p.EstimatedCost != nil && (!isFinite(*p.EstimatedCost) || *p.EstimatedCost < 0) {
		return nil, &domain.CreationPlanError{Message: "estimated_cost must be finite and non-negative"}
	}
	effectiveCost := p.EstimatedCost
	effectiveSteps := append([]string{}, p.Steps...)
	effectiveReviewPoints := append([]string{}, p.ReviewPoints...)

	var nodeRows []repository.WorkflowNode
	if err := tx.Where("revision_id = ?", revisionID).Order("node_key").Find(&nodeRows).Error; err != nil {
		return nil, err
	}
	if len(nodeRows) > 0 {
		if len(effectiveSteps) == 0 {
			effectiveSteps = make([]string, 0, len(nodeRows))
			for _, row := range nodeRows {
				effectiveSteps = append(effectiveSteps, row.NodeKey)
			}
		}
		if len(effectiveReviewPoints) == 0 {
			for _, row := range nodeRows {
				if row.ApprovalPolicyJSON != nil && *row.ApprovalPolicyJSON != "" && *row.ApprovalPolicyJSON != "{}" {
					effectiveReviewPoints = append(effectiveReviewPoints, row.NodeKey)
				}
			}
		}
		// 已发布的工作流 revision 是成本的唯一真相源：客户端自报的估算不能让
		// 落库计划看起来比可执行图更便宜或更贵。
		total := 0.0
		for _, row := range nodeRows {
			total += row.EstimatedCost
		}
		effectiveCost = &total
	}
	cost := 0.0
	if effectiveCost != nil {
		cost = *effectiveCost
	}

	preview, err := CompileCreationPlanPreview(CompilePreviewParams{
		SkillVersion:     skillVersion,
		ProjectID:        p.ProjectID,
		Project:          p.Project,
		ResourceIDs:      normalizedIDs,
		ResourceTypes:    sortedKeys(availableInputs),
		ResourceMapping:  normalizedMapping,
		Parameters:       p.Parameters,
		WorkflowRevision: revisionID,
		EstimatedCost:    cost,
		Steps:            effectiveSteps,
		ReviewPoints:     effectiveReviewPoints,
	})
	if err != nil {
		return nil, err
	}
	report := domain.CompatibilityReportWithAlternatives(
		skill, p.Project, availableInputs, alternativeSkillIDs(skill, p.Project, availableInputs),
	)
	var compatibilityEventID string
	if !preview.Plan.IsCompatible() {
		compatibilityEventID = uuid.NewString()
		report["event_id"] = compatibilityEventID
	}
	response := preview.ToDict()
	response["compatibility_report"] = report
	response["workflow_revision"] = revisionID
	response["required_capabilities"] = requiredCapabilities(nodeRows)
	capabilityReport := s.capabilityPreflight(ctx, p.ProjectID, p.ProjectFile, nodeRows)
	response["capability_report"] = capabilityReport
	if compatible, _ := capabilityReport["compatible"].(bool); !compatible {
		report["compatible"] = false
		report["capability"] = capabilityReport
	}

	// 内容去重：creation_plans 上有 UNIQUE(fingerprint)，内容完全相同的计划根本
	// 存不进第二行。recompile 在项目快照未变时会算出与源计划同一个 plan_id，
	// 这里按内容命中既有行返回，而不是把唯一约束冲突抛成 500。
	var existingByID repository.CreationPlan
	if err := tx.Where("id = ?", preview.Plan.PlanID).Take(&existingByID).Error; err == nil {
		return planResponse(&existingByID, true), nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	now := db.NewFlexibleTime(wfNow())
	record := repository.CreationPlan{
		ID:                     preview.Plan.PlanID,
		UserID:                 p.UserID,
		WorkspaceID:            p.WorkspaceID,
		ProjectID:              p.ProjectID,
		CreationSkillVersionID: p.CreationSkillVersionID,
		SkillID:                skill.ID,
		WorkflowRevision:       revisionID,
		Fingerprint:            preview.Plan.Fingerprint,
		IdempotencyKey:         p.IdempotencyKey,
		ProjectSnapshotJSON:    catalogJSON(preview.Plan.ProjectContext.ToDict()),
		ResourceIdsJSON:        catalogJSON(preview.ResourceIDs),
		ParametersJSON:         catalogJSON(preview.Parameters),
		PreviewJSON:            catalogJSON(response),
		EstimatedCost:          preview.EstimatedCost,
		Status:                 "previewed",
		CreatedBy:              p.UserID,
		CreatedAt:              now,
	}
	if err := tx.Create(&record).Error; err != nil {
		return nil, err
	}
	if !preview.Plan.IsCompatible() {
		reason := ""
		if len(preview.Plan.Compatibility.Reasons) > 0 {
			reason = preview.Plan.Compatibility.Reasons[0]
		}
		event := repository.CreationCompatibilityEvent{
			ID:                     compatibilityEventID,
			CreationSkillVersionID: p.CreationSkillVersionID,
			ProjectContentMode:     creationStringValue(p.Project["content_mode"]),
			ProjectGenerationMode:  creationStringValue(p.Project["generation_mode"]),
			ResourceTypesJSON:      catalogJSON(sortedKeys(availableInputs)),
			Reason:                 reason,
			Outcome:                "unresolved",
			CreatedAt:              now,
		}
		if err := tx.Create(&event).Error; err != nil {
			return nil, err
		}
	}
	return planResponse(&record, false), nil
}

func creationStringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func planResponse(record *repository.CreationPlan, deduped bool) map[string]any {
	response := map[string]any{}
	if record.PreviewJSON != "" {
		_ = json.Unmarshal([]byte(record.PreviewJSON), &response)
	}
	response["status"] = record.Status
	response["workflow_run_id"] = nilAsAnyString(record.WorkflowRunID)
	response["created_at"] = wfISO(record.CreatedAt)
	response["deduped"] = deduped
	return response
}

func normalizeResourceIDs(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

// normalizeResourceMapping 归一化资源映射；无显式映射时按 resource_types 兜底。
func normalizeResourceMapping(
	mapping []map[string]any, resourceIDs []string, resourceTypes []string,
) []map[string]string {
	seen := map[[3]string]bool{}
	normalized := make([]map[string]string, 0, len(mapping))
	for _, item := range mapping {
		resourceID := strings.TrimSpace(creationStringValue(item["id"]))
		resourceType := strings.TrimSpace(creationStringValue(item["type"]))
		source := strings.TrimSpace(creationStringValue(item["source"]))
		if resourceID == "" || resourceType == "" {
			continue
		}
		key := [3]string{resourceID, resourceType, source}
		if seen[key] {
			continue
		}
		seen[key] = true
		entry := map[string]string{"id": resourceID, "type": resourceType}
		if source != "" {
			entry["source"] = source
		}
		normalized = append(normalized, entry)
	}
	if len(normalized) > 0 {
		return normalized
	}
	fallbackType := ""
	candidates := make([]string, 0, len(resourceTypes))
	for _, value := range resourceTypes {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			candidates = append(candidates, trimmed)
		}
	}
	sort.Strings(candidates)
	if len(candidates) > 0 {
		fallbackType = candidates[0]
	}
	if fallbackType == "" {
		return normalized
	}
	for _, resourceID := range resourceIDs {
		trimmed := strings.TrimSpace(resourceID)
		if trimmed != "" {
			normalized = append(normalized, map[string]string{"id": trimmed, "type": fallbackType})
		}
	}
	return normalized
}

// resolveSkillVersion 解析请求的版本；缺失时先同步官方目录再解析一次。
func (s *CreationPlanService) resolveSkillVersion(
	ctx context.Context, tx *gorm.DB, skillVersionID string,
) (*domain.CreationSkillDefinition, *domain.CreationSkillVersion, error) {
	skill, version, err := s.catalog.GetPersistedCreationSkillVersion(ctx, skillVersionID)
	if err != nil {
		return nil, nil, err
	}
	if skill == nil {
		if _, err := s.catalog.SyncOfficialCreationSkills(ctx); err != nil {
			return nil, nil, err
		}
		skill, version, err = s.catalog.GetPersistedCreationSkillVersion(ctx, skillVersionID)
		if err != nil {
			return nil, nil, err
		}
	}
	if skill == nil {
		return nil, nil, &domain.CreationPlanError{
			Message: "unknown or unavailable creation_skill_version_id: " + skillVersionID,
		}
	}
	return skill, version, nil
}

// resolveWorkflowRevision 解析具体已发布 revision，绝不接受 Skill id / version 别名。
func (s *CreationPlanService) resolveWorkflowRevision(
	ctx context.Context, tx *gorm.DB, version *domain.CreationSkillVersion, requested *string,
) (string, error) {
	boundRevision := ""
	if version.WorkflowRevisionID != nil {
		boundRevision = strings.TrimSpace(*version.WorkflowRevisionID)
	}
	if boundRevision == "" || strings.HasPrefix(boundRevision, "official:") {
		return "", &domain.CreationPlanError{Message: "CreationSkillVersion has no bound Workflow Revision"}
	}
	if boundRevision == version.ID || boundRevision == version.SkillID {
		return "", &domain.CreationPlanError{Message: "CreationPlan requires a concrete Workflow Revision id"}
	}
	if requested != nil && *requested != "" && *requested != boundRevision {
		return "", &domain.CreationPlanError{
			Message: "requested workflow_revision does not match the Skill Version binding",
		}
	}
	var revision repository.WorkflowRevision
	if err := tx.Where("id = ?", boundRevision).Take(&revision).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", &domain.CreationPlanError{Message: "Workflow Revision was not found"}
		}
		return "", err
	}
	if revision.Status != "published" {
		return "", &domain.CreationPlanError{Message: "Workflow Revision must be published"}
	}
	return boundRevision, nil
}

func requiredCapabilities(nodeRows []repository.WorkflowNode) []string {
	required := map[string]bool{}
	for _, row := range nodeRows {
		values, ok := catalogDecode(row.RequiredCapabilitiesJSON, []any{}).([]any)
		if !ok && row.RequiredCapabilitiesJSON != "" {
			continue
		}
		for _, value := range values {
			if text, isString := value.(string); isString && strings.TrimSpace(text) != "" {
				required[strings.TrimSpace(text)] = true
			}
		}
	}
	return sortedKeys(required)
}

// capabilityPreflight 在计划落库或启动前解析可执行的供应商 / 模型通道。
func (s *CreationPlanService) capabilityPreflight(
	ctx context.Context, projectID string, projectFile *domain.ProjectFile, nodeRows []repository.WorkflowNode,
) map[string]any {
	required := map[string]bool{}
	invalid := false
	for _, row := range nodeRows {
		if row.RequiredCapabilitiesJSON == "" {
			continue
		}
		values, ok := catalogDecode(row.RequiredCapabilitiesJSON, []any{}).([]any)
		if !ok {
			invalid = true
			break
		}
		for _, value := range values {
			if text, isString := value.(string); isString && strings.TrimSpace(text) != "" {
				required[strings.TrimSpace(text)] = true
			}
		}
	}
	if invalid {
		return map[string]any{"compatible": false, "code": "workflow_capabilities_invalid", "required": []string{}}
	}
	needsImage := intersects(required, "image", "t2i", "i2i", "text_to_image", "image_to_image")
	needsVideo := intersects(required, "video", "i2v", "r2v", "t2v")
	needsAudio := intersects(required, "audio", "tts", "text_to_speech")
	imageCapability := config.ImageCapT2I
	if required["i2i"] || required["image_to_image"] {
		imageCapability = config.ImageCapI2I
	}
	videoCapability := ""
	switch {
	case required["r2v"]:
		videoCapability = "r2v"
	case required["i2v"], required["t2v"], required["video"]:
		videoCapability = "i2v"
	}
	if !needsImage && !needsVideo && !needsAudio {
		return map[string]any{"compatible": true, "required": []string{}, "resolved": map[string]any{}}
	}
	if s.resolver == nil || projectFile == nil {
		return map[string]any{
			"compatible": false,
			"code":       "generation_configuration_incomplete",
			"message":    "generation context unavailable",
			"required":   sortedKeys(required),
		}
	}
	resolved := map[string]any{}
	if needsImage {
		model, err := s.resolver.ResolveImageBackend(ctx, projectFile, nil, imageCapability)
		if err != nil {
			return capabilityFailure("provider_model_capability_unavailable", err, required)
		}
		resolved["image"] = map[string]any{
			"provider":   model.ProviderID,
			"model":      model.ModelID,
			"capability": string(imageCapability),
		}
	}
	if needsVideo {
		var bucket *provider.VideoBucket
		switch videoCapability {
		case "r2v":
			value := provider.BucketR2V
			bucket = &value
		case "i2v":
			value := provider.BucketI2V
			bucket = &value
		}
		model, err := s.resolver.ResolveVideoBackend(ctx, projectFile, nil, bucket)
		if err != nil {
			return capabilityFailure("provider_model_capability_unavailable", err, required)
		}
		resolved["video"] = map[string]any{
			"provider":   model.ProviderID,
			"model":      model.ModelID,
			"capability": videoCapability,
		}
	}
	if needsAudio {
		model, err := s.resolver.ResolveAudioBackend(ctx, projectFile, nil)
		if err != nil {
			return capabilityFailure("provider_model_capability_unavailable", err, required)
		}
		resolved["audio"] = map[string]any{"provider": model.ProviderID, "model": model.ModelID}
	}
	unavailable := make([]string, 0)
	for _, lane := range resolved {
		mapping, ok := lane.(map[string]any)
		if !ok {
			continue
		}
		providerID := creationStringValue(mapping["provider"])
		modelID := creationStringValue(mapping["model"])
		if _, ok := provider.ModelInfoFor(providerID, modelID); !ok {
			unavailable = append(unavailable, providerID+"/"+modelID)
		}
	}
	if len(unavailable) > 0 {
		sort.Strings(unavailable)
		return map[string]any{
			"compatible": false,
			"code":       "provider_model_unavailable",
			"models":     unavailable,
			"required":   sortedKeys(required),
		}
	}
	return map[string]any{
		"compatible": true,
		"required":   sortedKeys(required),
		"resolved":   resolved,
	}
}

func capabilityFailure(code string, err error, required map[string]bool) map[string]any {
	return map[string]any{
		"compatible": false,
		"code":       code,
		"message":    err.Error(),
		"required":   sortedKeys(required),
	}
}

func intersects(set map[string]bool, keys ...string) bool {
	for _, key := range keys {
		if set[key] {
			return true
		}
	}
	return false
}

// --- 编译 ---

// CompilePreviewParams 是 CompileCreationPlanPreview 的入参。
type CompilePreviewParams struct {
	SkillVersion     *domain.CreationSkillVersion
	ProjectID        string
	Project          map[string]any
	ResourceIDs      []string
	ResourceTypes    []string
	ResourceMapping  []map[string]string
	Parameters       map[string]any
	WorkflowRevision string
	EstimatedCost    float64
	Steps            []string
	ReviewPoints     []string
}

// CompileCreationPlanPreview 由项目自有的设置编译出确定性预览。
func CompileCreationPlanPreview(p CompilePreviewParams) (*CreationPlanPreview, error) {
	parameters := map[string]any{}
	for key, value := range p.Parameters {
		parameters[key] = value
	}
	forbidden := make([]string, 0)
	for _, key := range domain.CreationPlanForbiddenOverrides {
		if _, exists := parameters[key]; exists {
			forbidden = append(forbidden, key)
		}
	}
	if len(forbidden) > 0 {
		sort.Strings(forbidden)
		return nil, &domain.CreationPlanError{
			Message: "CreationPlan does not accept mode overrides: " + strings.Join(forbidden, ", "),
		}
	}
	normalizedIDs := normalizeResourceIDs(p.ResourceIDs)
	availableInputs := map[string]bool{}
	for _, value := range p.ResourceTypes {
		availableInputs[value] = true
	}
	rawMapping := make([]map[string]any, 0, len(p.ResourceMapping))
	for _, item := range p.ResourceMapping {
		entry := map[string]any{}
		for key, value := range item {
			entry[key] = value
		}
		rawMapping = append(rawMapping, entry)
	}
	normalizedMapping := normalizeResourceMapping(rawMapping, normalizedIDs, p.ResourceTypes)
	if len(normalizedMapping) > 0 {
		normalizedIDs = make([]string, 0, len(normalizedMapping))
		availableInputs = map[string]bool{}
		for _, item := range normalizedMapping {
			normalizedIDs = append(normalizedIDs, item["id"])
			availableInputs[item["type"]] = true
		}
	}
	context, err := domain.ProjectContextSnapshotFromProject(p.ProjectID, p.Project)
	if err != nil {
		return nil, err
	}
	mappingPayload := make([]any, 0, len(normalizedMapping))
	for _, item := range normalizedMapping {
		entry := map[string]any{}
		for key, value := range item {
			entry[key] = value
		}
		mappingPayload = append(mappingPayload, entry)
	}
	stablePayload := map[string]any{
		"skill_version_id":  p.SkillVersion.ID,
		"project_id":        p.ProjectID,
		"project_snapshot":  context.ToDict(),
		"resource_ids":      normalizedIDs,
		"resource_mapping":  mappingPayload,
		"parameters":        parameters,
		"workflow_revision": p.WorkflowRevision,
	}
	planID, err := domain.CreationPlanStableID(stablePayload)
	if err != nil {
		return nil, err
	}
	steps := make([]any, 0, len(p.Steps))
	for _, item := range p.Steps {
		steps = append(steps, item)
	}
	reviewPoints := make([]any, 0, len(p.ReviewPoints))
	for _, item := range p.ReviewPoints {
		reviewPoints = append(reviewPoints, item)
	}
	plan, err := domain.CompileCreationPlan(domain.CompileCreationPlanParams{
		PlanID:           planID,
		SkillID:          p.SkillVersion.SkillID,
		WorkflowRevision: p.WorkflowRevision,
		ProjectContext:   context,
		SkillInputs: map[string]any{
			"resource_ids":     normalizedIDs,
			"resource_types":   sortedKeys(availableInputs),
			"resource_mapping": mappingPayload,
			"parameters":       parameters,
			"expected_steps":   steps,
			"review_points":    reviewPoints,
			"estimated_cost":   p.EstimatedCost,
		},
		SupportedContentModes:    p.SkillVersion.Compatibility.ContentModes,
		SupportedGenerationModes: p.SkillVersion.Compatibility.GenerationModes,
	})
	if err != nil {
		return nil, err
	}
	if p.EstimatedCost < 0 {
		return nil, &domain.CreationPlanError{Message: "estimated_cost must be non-negative"}
	}
	return &CreationPlanPreview{
		Plan:            plan,
		ResourceIDs:     normalizedIDs,
		ResourceMapping: rawMapping,
		Parameters:      parameters,
		EstimatedCost:   p.EstimatedCost,
		Steps:           p.Steps,
		ReviewPoints:    p.ReviewPoints,
	}, nil
}

// --- 生命周期 ---

// loadPlanForUpdate 按计划 ID 取行并校验归属；PostgreSQL 上加行锁。
//
// SQLite 没有 FOR UPDATE，写事务本身在 WAL 下是串行化的，故只在 PostgreSQL 上
// 显式加锁，避免 SQLite 解析失败。
func (s *CreationPlanService) loadPlanForUpdate(
	ctx context.Context, tx *gorm.DB, planID, userID string,
) (*repository.CreationPlan, error) {
	query := tx.Where("id = ?", planID)
	if s.dialect == db.Postgres {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var record repository.CreationPlan
	if err := query.Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &CreationPlanNotFoundError{ID: planID}
		}
		return nil, err
	}
	if record.UserID != userID {
		return nil, &CreationPlanNotFoundError{ID: planID}
	}
	return &record, nil
}

// GetCreationPlan 读取计划；运行终态会投影回计划状态。
func (s *CreationPlanService) GetCreationPlan(
	ctx context.Context, planID, userID string,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record, err := s.loadPlanForUpdate(ctx, tx, planID, userID)
		if err != nil {
			return err
		}
		if record.WorkflowRunID != nil && *record.WorkflowRunID != "" {
			var run repository.WorkflowRun
			if err := tx.Where("id = ?", *record.WorkflowRunID).Take(&run).Error; err == nil {
				projected := map[string]string{
					"succeeded": "succeeded", "failed": "failed", "cancelled": "cancelled",
				}[run.Status]
				if projected != "" && record.Status != projected {
					if err := tx.Model(&repository.CreationPlan{}).
						Where("id = ?", planID).Update("status", projected).Error; err != nil {
						return err
					}
					record.Status = projected
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		out = planResponse(record, false)
		return nil
	})
	return out, err
}

// StartCreationPlanParams 是启动计划的入参。
type StartCreationPlanParams struct {
	PlanID           string
	UserID           string
	Project          map[string]any
	Mode             string
	ScriptRevisionID *string
	EpisodeID        *string
	BudgetLimit      *float64
	CostConfirmed    bool
	ReviewConfirmed  bool
}

// StartCreationPlan 复核项目快照后启动且仅启动一条 WorkflowRun。
func (s *CreationPlanService) StartCreationPlan(
	ctx context.Context, p StartCreationPlanParams,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record, err := s.loadPlanForUpdate(ctx, tx, p.PlanID, p.UserID)
		if err != nil {
			return err
		}
		if record.Status == "cancelled" {
			return &CreationPlanStartError{Code: "creation_plan_cancelled"}
		}
		if record.Status == "invalidated" {
			return &CreationPlanStartError{Code: "creation_plan_invalidated"}
		}
		if record.WorkflowRunID != nil && *record.WorkflowRunID != "" {
			runID := *record.WorkflowRunID
			var run repository.WorkflowRun
			if err := tx.Where("id = ?", runID).Take(&run).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return &CreationPlanStartError{Code: "workflow_run_missing"}
				}
				return err
			}
			runStatus := run.Status
			if runStatus == "planned" {
				transitioned, err := s.workflows.transitionRun(ctx, tx, runID, "running", run.Version, p.UserID)
				if err != nil {
					return err
				}
				runStatus, _ = transitioned["status"].(string)
			}
			if runStatus == "running" {
				if err := s.setPlanRunning(ctx, tx, record, runStatus); err != nil {
					return err
				}
			}
			dispatch, err := s.workflows.requestExecution(ctx, tx, runID, p.UserID)
			if err != nil {
				return err
			}
			out = map[string]any{
				"plan_id":         record.ID,
				"workflow_run_id": runID,
				"status":          record.Status,
				"deduped":         true,
				"run_status":      runStatus,
				"dispatch_status": dispatch["dispatch_status"],
			}
			return nil
		}

		if err := assertSnapshotMatches(record, p.Project); err != nil {
			return err
		}
		revision, err := s.requirePublishedRevision(ctx, tx, record.WorkflowRevision)
		if err != nil {
			return err
		}
		_ = revision
		preview := map[string]any{}
		_ = json.Unmarshal([]byte(record.PreviewJSON), &preview)
		report := map[string]any{}
		if raw, ok := preview["compatibility_report"].(map[string]any); ok {
			report = raw
		}
		if compatible, _ := report["compatible"].(bool); !compatible {
			return &CreationPlanStartError{Code: "creation_skill_incompatible", Report: report}
		}
		estimatedCost := 0.0
		if value, ok := preview["estimated_cost"].(float64); ok {
			estimatedCost = value
		}
		reviewPoints := preview["review_points"]
		if p.BudgetLimit != nil && estimatedCost > *p.BudgetLimit {
			return &CreationPlanStartError{
				Code:   "creation_plan_budget_exceeded",
				Report: map[string]any{"estimated_cost": estimatedCost, "budget_limit": *p.BudgetLimit},
			}
		}
		if estimatedCost > 0 && !p.CostConfirmed {
			return &CreationPlanStartError{
				Code:   "creation_plan_cost_confirmation_required",
				Report: map[string]any{"estimated_cost": estimatedCost},
			}
		}
		if list, ok := reviewPoints.([]any); ok && len(list) > 0 && !p.ReviewConfirmed {
			return &CreationPlanStartError{
				Code:   "creation_plan_review_confirmation_required",
				Report: map[string]any{"review_points": list},
			}
		}

		existingRun, err := s.workflows.findRunForPlan(ctx, tx, record.ID, record.ProjectID, p.UserID)
		if err != nil {
			return err
		}
		if existingRun != nil {
			runID := existingRun["id"].(string)
			runStatus := "planned"
			if value, ok := existingRun["status"].(string); ok && value != "" {
				runStatus = value
			}
			if runStatus == "planned" {
				version := int64(1)
				if value, ok := existingRun["version"].(int64); ok {
					version = value
				}
				transitioned, err := s.workflows.transitionRun(ctx, tx, runID, "running", version, p.UserID)
				if err != nil {
					return err
				}
				runStatus, _ = transitioned["status"].(string)
			}
			if err := s.setPlanRunning(ctx, tx, record, runStatus); err != nil {
				return err
			}
			record.WorkflowRunID = &runID
			dispatch, err := s.workflows.requestExecution(ctx, tx, runID, p.UserID)
			if err != nil {
				return err
			}
			out = map[string]any{
				"plan_id":         record.ID,
				"workflow_run_id": runID,
				"status":          record.Status,
				"run_status":      runStatus,
				"deduped":         true,
				"dispatch_status": dispatch["dispatch_status"],
			}
			return nil
		}

		run, err := s.workflows.planRun(ctx, tx, WorkflowPlanRunParams{
			RevisionID:       record.WorkflowRevision,
			WorkspaceID:      record.WorkspaceID,
			ProjectID:        record.ProjectID,
			ScriptRevisionID: p.ScriptRevisionID,
			Mode:             p.Mode,
			InputSnapshot:    map[string]any{"creation_plan_id": record.ID, "creation_plan": preview},
			ActorID:          p.UserID,
			EpisodeID:        p.EpisodeID,
			BudgetLimit:      p.BudgetLimit,
			OfficialRevision: true,
		})
		if err != nil {
			return err
		}
		runID := run["id"].(string)
		runStatus := "planned"
		if value, ok := run["status"].(string); ok && value != "" {
			runStatus = value
		}
		if runStatus == "planned" {
			version := int64(1)
			if value, ok := run["version"].(int64); ok {
				version = value
			}
			transitioned, err := s.workflows.transitionRun(ctx, tx, runID, "running", version, p.UserID)
			if err != nil {
				return err
			}
			runStatus, _ = transitioned["status"].(string)
		}
		if err := s.setPlanRunning(ctx, tx, record, runStatus); err != nil {
			return err
		}
		record.WorkflowRunID = &runID
		dispatch, err := s.workflows.requestExecution(ctx, tx, runID, p.UserID)
		if err != nil {
			return err
		}
		deduped := false
		if value, ok := run["deduped"].(bool); ok {
			deduped = value
		}
		out = map[string]any{
			"plan_id":         record.ID,
			"workflow_run_id": runID,
			"status":          record.Status,
			"deduped":         deduped,
			"run_status":      runStatus,
			"dispatch_status": dispatch["dispatch_status"],
		}
		return nil
	})
	return out, err
}

func (s *CreationPlanService) setPlanRunning(
	ctx context.Context, tx *gorm.DB, record *repository.CreationPlan, runStatus string,
) error {
	status := "started"
	if runStatus == "running" {
		status = "running"
	}
	updates := map[string]any{"status": status}
	if !record.StartedAt.Valid {
		updates["started_at"] = db.NewFlexibleTime(wfNow())
	}
	if err := tx.Model(&repository.CreationPlan{}).Where("id = ?", record.ID).Updates(updates).Error; err != nil {
		return err
	}
	record.Status = status
	if !record.StartedAt.Valid {
		record.StartedAt = db.NewFlexibleTime(wfNow())
	}
	return nil
}

func assertSnapshotMatches(record *repository.CreationPlan, project map[string]any) error {
	context, err := domain.ProjectContextSnapshotFromProject(record.ProjectID, project)
	if err != nil {
		return err
	}
	current, err := domain.WorkflowCanonicalJSON(context.ToDict())
	if err != nil {
		return err
	}
	if current != record.ProjectSnapshotJSON {
		return &CreationPlanStartError{Code: "project_snapshot_changed"}
	}
	return nil
}

func (s *CreationPlanService) requirePublishedRevision(
	ctx context.Context, tx *gorm.DB, revisionID string,
) (*repository.WorkflowRevision, error) {
	var revision repository.WorkflowRevision
	if err := tx.Where("id = ?", revisionID).Take(&revision).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &CreationPlanStartError{Code: "workflow_revision_unavailable"}
		}
		return nil, err
	}
	if revision.Status != "published" {
		return nil, &CreationPlanStartError{Code: "workflow_revision_unavailable"}
	}
	return &revision, nil
}

// CancelCreationPlan 取消计划；已在跑的运行一并跃迁到 cancelled。
func (s *CreationPlanService) CancelCreationPlan(
	ctx context.Context, planID, userID string,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record, err := s.loadPlanForUpdate(ctx, tx, planID, userID)
		if err != nil {
			return err
		}
		now := db.NewFlexibleTime(wfNow())
		if record.WorkflowRunID != nil && *record.WorkflowRunID != "" {
			var run repository.WorkflowRun
			if err := tx.Where("id = ?", *record.WorkflowRunID).Take(&run).Error; err == nil {
				if !domain.WorkflowTerminalRunStatuses[run.Status] {
					if _, err := s.workflows.transitionRun(
						ctx, tx, run.ID, "cancelled", run.Version, userID,
					); err != nil {
						return err
					}
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err := tx.Model(&repository.CreationPlan{}).Where("id = ?", planID).Updates(map[string]any{
				"status":       "cancelled",
				"cancelled_at": now,
			}).Error; err != nil {
				return err
			}
		} else if record.Status != "cancelled" {
			if err := tx.Model(&repository.CreationPlan{}).Where("id = ?", planID).Updates(map[string]any{
				"status":       "cancelled",
				"cancelled_at": now,
			}).Error; err != nil {
				return err
			}
		}
		record.Status = "cancelled"
		record.CancelledAt = now
		out = planResponse(record, false)
		return nil
	})
	return out, err
}

// InvalidateCreationPlan 作废尚未启动的计划。
func (s *CreationPlanService) InvalidateCreationPlan(
	ctx context.Context, planID, userID string,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record, err := s.loadPlanForUpdate(ctx, tx, planID, userID)
		if err != nil {
			return err
		}
		if record.WorkflowRunID != nil && *record.WorkflowRunID != "" {
			return &CreationPlanStartError{Code: "creation_plan_already_started"}
		}
		if record.Status != "cancelled" && record.Status != "invalidated" {
			if err := tx.Model(&repository.CreationPlan{}).
				Where("id = ?", planID).Update("status", "invalidated").Error; err != nil {
				return err
			}
			record.Status = "invalidated"
		}
		out = planResponse(record, false)
		return nil
	})
	return out, err
}

// RecompileCreationPlan 用当前项目快照编译一条全新的不可变计划。
func (s *CreationPlanService) RecompileCreationPlan(
	ctx context.Context, planID, userID string, project map[string]any,
) (map[string]any, error) {
	var result map[string]any
	var sourceID string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record, err := s.loadPlanForUpdate(ctx, tx, planID, userID)
		if err != nil {
			return err
		}
		if record.WorkflowRunID != nil && *record.WorkflowRunID != "" {
			return &CreationPlanStartError{Code: "creation_plan_already_started"}
		}
		preview := map[string]any{}
		if err := json.Unmarshal([]byte(record.PreviewJSON), &preview); err != nil {
			return &CreationPlanStartError{Code: "creation_plan_snapshot_invalid"}
		}
		resourceTypes := []string{}
		if skillInputs, ok := preview["skill_inputs"].(map[string]any); ok {
			if values, ok := skillInputs["resource_types"].([]any); ok {
				for _, value := range values {
					if text, isString := value.(string); isString {
						resourceTypes = append(resourceTypes, text)
					}
				}
			}
		}
		var resourceMapping []map[string]any
		if raw, ok := preview["resource_mapping"].([]any); ok {
			for _, item := range raw {
				if entry, isMap := item.(map[string]any); isMap {
					resourceMapping = append(resourceMapping, entry)
				}
			}
		} else if !ok {
			return &CreationPlanStartError{Code: "creation_plan_snapshot_invalid"}
		}
		var resourceIDs []string
		if err := json.Unmarshal([]byte(record.ResourceIdsJSON), &resourceIDs); err != nil {
			return &CreationPlanStartError{Code: "creation_plan_snapshot_invalid"}
		}
		parameters := map[string]any{}
		if err := json.Unmarshal([]byte(record.ParametersJSON), &parameters); err != nil {
			return &CreationPlanStartError{Code: "creation_plan_snapshot_invalid"}
		}
		context, err := domain.ProjectContextSnapshotFromProject(record.ProjectID, project)
		if err != nil {
			return err
		}
		snapshotKey, err := domain.WorkflowCanonicalJSON(context.ToDict())
		if err != nil {
			return err
		}
		sum := sha256Hex(snapshotKey)
		recompiled, err := s.createPreview(ctx, tx, CreationPlanPreviewParams{
			UserID:                 userID,
			WorkspaceID:            record.WorkspaceID,
			ProjectID:              record.ProjectID,
			CreationSkillVersionID: record.CreationSkillVersionID,
			Project:                project,
			ResourceIDs:            resourceIDs,
			ResourceTypes:          resourceTypes,
			ResourceMapping:        resourceMapping,
			Parameters:             parameters,
			IdempotencyKey:         record.ID + ":recompile:" + sum,
		})
		if err != nil {
			return err
		}
		if err := tx.Model(&repository.CreationPlan{}).
			Where("id = ?", planID).Update("status", "invalidated").Error; err != nil {
			return err
		}
		recompiled["recompiled_from"] = record.ID
		result = recompiled
		sourceID = record.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = sourceID
	return result, nil
}

// RestartCreationPlan 为终态计划建一条新的 WorkflowRun，沿用其冻结输入。
func (s *CreationPlanService) RestartCreationPlan(
	ctx context.Context, p StartCreationPlanParams,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record, err := s.loadPlanForUpdate(ctx, tx, p.PlanID, p.UserID)
		if err != nil {
			return err
		}
		if record.WorkflowRunID == nil || *record.WorkflowRunID == "" {
			return &CreationPlanStartError{Code: "creation_plan_not_started"}
		}
		var previousRun repository.WorkflowRun
		if err := tx.Where("id = ?", *record.WorkflowRunID).Take(&previousRun).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return &CreationPlanStartError{Code: "creation_plan_run_not_found"}
			}
			return err
		}
		if !domain.WorkflowTerminalRunStatuses[previousRun.Status] {
			return &CreationPlanStartError{Code: "creation_plan_run_not_terminal"}
		}
		if err := assertSnapshotMatches(record, p.Project); err != nil {
			return err
		}
		if _, err := s.requirePublishedRevision(ctx, tx, record.WorkflowRevision); err != nil {
			return err
		}
		preview := map[string]any{}
		_ = json.Unmarshal([]byte(record.PreviewJSON), &preview)
		report := map[string]any{}
		if raw, ok := preview["compatibility_report"].(map[string]any); ok {
			report = raw
		}
		if compatible, _ := report["compatible"].(bool); !compatible {
			return &CreationPlanStartError{Code: "creation_skill_incompatible", Report: report}
		}
		run, err := s.workflows.planRun(ctx, tx, WorkflowPlanRunParams{
			RevisionID:       record.WorkflowRevision,
			WorkspaceID:      record.WorkspaceID,
			ProjectID:        record.ProjectID,
			ScriptRevisionID: p.ScriptRevisionID,
			Mode:             p.Mode,
			InputSnapshot: map[string]any{
				"creation_plan_id":           record.ID,
				"creation_plan":              preview,
				"restart_of_workflow_run_id": previousRun.ID,
			},
			ActorID:          p.UserID,
			EpisodeID:        p.EpisodeID,
			BudgetLimit:      p.BudgetLimit,
			OfficialRevision: true,
		})
		if err != nil {
			return err
		}
		runID := run["id"].(string)
		runStatus := "planned"
		if value, ok := run["status"].(string); ok && value != "" {
			runStatus = value
		}
		if runStatus == "planned" {
			version := int64(1)
			if value, ok := run["version"].(int64); ok {
				version = value
			}
			transitioned, err := s.workflows.transitionRun(ctx, tx, runID, "running", version, p.UserID)
			if err != nil {
				return err
			}
			runStatus, _ = transitioned["status"].(string)
		}
		runIDCopy := runID
		status := "started"
		if runStatus == "running" {
			status = "running"
		}
		if err := tx.Model(&repository.CreationPlan{}).Where("id = ?", record.ID).Updates(map[string]any{
			"workflow_run_id": runIDCopy,
			"status":          status,
			"started_at":      db.NewFlexibleTime(wfNow()),
			"cancelled_at":    nil,
		}).Error; err != nil {
			return err
		}
		deduped := false
		if value, ok := run["deduped"].(bool); ok {
			deduped = value
		}
		out = map[string]any{
			"plan_id":         record.ID,
			"workflow_run_id": runID,
			"status":          status,
			"deduped":         deduped,
			"restarted_from":  previousRun.ID,
		}
		return nil
	})
	return out, err
}

// --- 兼容性事件 ---

// CreationCompatibilityOutcomes 是可记录的粗粒度后续选择。
var CreationCompatibilityOutcomes = map[string]bool{
	"cancelled": true, "alternative_skill": true, "new_project": true,
}

// RecordCompatibilityOutcome 只记录用户的粗粒度后续选择，不记录创作内容。
func (s *CreationPlanService) RecordCompatibilityOutcome(
	ctx context.Context, eventID, outcome string,
) (map[string]any, error) {
	if !CreationCompatibilityOutcomes[outcome] {
		return nil, &CreationPlanInvalidOutcomeError{Outcome: outcome}
	}
	var event repository.CreationCompatibilityEvent
	err := s.db.WithContext(ctx).Where("id = ?", eventID).Take(&event).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &CreationPlanNotFoundError{ID: eventID}
		}
		return nil, err
	}
	if err := s.db.WithContext(ctx).Model(&repository.CreationCompatibilityEvent{}).
		Where("id = ?", eventID).Update("outcome", outcome).Error; err != nil {
		return nil, err
	}
	return map[string]any{
		"event_id":                  event.ID,
		"creation_skill_version_id": event.CreationSkillVersionID,
		"project_generation_mode":   event.ProjectGenerationMode,
		"outcome":                   outcome,
	}, nil
}

// CreationPlanInvalidOutcomeError 表示后续选择取值非法（HTTP 400）。
type CreationPlanInvalidOutcomeError struct{ Outcome string }

func (e *CreationPlanInvalidOutcomeError) Error() string { return "invalid compatibility outcome" }

// CompatibilityMetrics 聚合匿名不兼容事件，供放量决策使用。
func (s *CreationPlanService) CompatibilityMetrics(ctx context.Context) (map[string]any, error) {
	type row struct {
		SkillVersionID string
		GenerationMode string
		Reason         string
		Outcome        string
		Count          int64
	}
	var rows []row
	err := s.db.WithContext(ctx).Model(&repository.CreationCompatibilityEvent{}).
		Select("creation_skill_version_id AS skill_version_id, project_generation_mode AS generation_mode, " +
			"reason AS reason, outcome AS outcome, COUNT(*) AS count").
		Group("creation_skill_version_id, project_generation_mode, reason, outcome").
		Order("creation_skill_version_id").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, len(rows))
	for _, item := range rows {
		items = append(items, map[string]any{
			"creation_skill_version_id": item.SkillVersionID,
			"project_generation_mode":   item.GenerationMode,
			"reason":                    item.Reason,
			"outcome":                   item.Outcome,
			"count":                     item.Count,
		})
	}
	return map[string]any{"items": items}, nil
}

// sha256Hex 计算字符串的 SHA-256 十六进制摘要。
func sha256Hex(value string) string {
	return domainMustHex(value)
}

// domainMustHex 是 sha256Hex 的实现：输入恒为 canonical JSON，不会失败。
func domainMustHex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
