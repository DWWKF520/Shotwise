package creative

// 数据库承载的官方 Creation Skill 目录与清单同步器
// （对应 server/services/creation_skill_catalog.py）。
//
// 目录里已发布的版本是"冻结"的：同步只补缺失的发布，绝不改动既有行；
// 一旦落库内容与内置清单不符，直接报错而不是静默改写。

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/domain/creation"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
)

// CreationSkillManifestConflictError 表示清单试图改动已冻结的发布。
type CreationSkillManifestConflictError struct{ Message string }

func (e *CreationSkillManifestConflictError) Error() string { return e.Message }

// CreationSkillCatalog 是官方创作 Skill 目录服务。
type CreationSkillCatalog struct {
	db        *gorm.DB
	workflows *WorkflowService
}

// NewCreationSkillCatalog 构造目录服务。
func NewCreationSkillCatalog(gdb *gorm.DB, workflows *WorkflowService) *CreationSkillCatalog {
	return &CreationSkillCatalog{db: gdb, workflows: workflows}
}

func catalogJSON(value any) string {
	encoded, err := creation.WorkflowCanonicalJSON(value)
	if err != nil {
		return "{}"
	}
	return encoded
}

func catalogDecode(raw string, fallback any) any {
	if raw == "" {
		return fallback
	}
	var out any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return fallback
	}
	return out
}

func catalogStringList(raw string) []string {
	value, ok := catalogDecode(raw, []any{}).([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(value))
	for _, item := range value {
		if text, isString := item.(string); isString {
			out = append(out, text)
		}
	}
	sort.Strings(out)
	return out
}

// manifestPayload 由内置清单算出可比对载荷。
func manifestPayload(skill *creation.CreationSkillDefinition) map[string]any {
	version := skill.LatestVersion
	compatibility := version.Compatibility
	var gridStoryboards any
	if compatibility.GridStoryboards != nil && compatibility.HasGridStoryboards() {
		values := make([]any, 0, 2)
		if compatibility.GridStoryboards[false] {
			values = append(values, false)
		}
		if compatibility.GridStoryboards[true] {
			values = append(values, true)
		}
		gridStoryboards = values
	}
	return map[string]any{
		"version_id":                       version.ID,
		"version":                          version.Version,
		"title":                            version.Title,
		"summary":                          version.Summary,
		"category":                         version.Category,
		"workflow_template_revision_alias": version.WorkflowTemplateRevisionAlias,
		"expected_outputs":                 version.ExpectedOutputs,
		"review_required":                  version.ReviewRequired,
		"estimated_cost_hint":              nilAsAnyString(version.EstimatedCostHint),
		"content_modes":                    sortedStringKeys(compatibility.ContentModes),
		"generation_modes":                 sortedStringKeys(compatibility.GenerationModes),
		"required_inputs":                  sortedStringKeys(compatibility.RequiredInputs),
		"scopes":                           sortedStringKeys(compatibility.Scopes),
		"grid_storyboards":                 gridStoryboards,
	}
}

func sortedStringKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// storedPayload 由落库行算出可比对载荷。
func storedPayload(
	version *repository.CreationSkillVersion, compatibility *repository.CreationSkillCompatibility,
) map[string]any {
	var gridStoryboards any
	if compatibility.GridStoryboardsJSON != nil {
		raw := catalogDecode(*compatibility.GridStoryboardsJSON, []any{})
		if list, ok := raw.([]any); ok {
			values := make([]any, 0, len(list))
			hasFalse, hasTrue := false, false
			for _, item := range list {
				if flag, isBool := item.(bool); isBool {
					if flag {
						hasTrue = true
					} else {
						hasFalse = true
					}
				}
			}
			if hasFalse {
				values = append(values, false)
			}
			if hasTrue {
				values = append(values, true)
			}
			gridStoryboards = values
		}
	}
	return map[string]any{
		"version_id":                       version.ID,
		"version":                          version.Version,
		"title":                            version.Title,
		"summary":                          version.Summary,
		"category":                         version.Category,
		"workflow_template_revision_alias": version.WorkflowTemplateRevisionAlias,
		"expected_outputs":                 catalogDecode(version.ExpectedOutputsJSON, []any{}),
		"review_required":                  version.ReviewRequired,
		"estimated_cost_hint":              nilAsAnyString(version.EstimatedCostHint),
		"content_modes":                    catalogStringList(compatibility.ContentModesJSON),
		"generation_modes":                 catalogStringList(compatibility.GenerationModesJSON),
		"required_inputs":                  catalogStringList(compatibility.RequiredInputsJSON),
		"scopes":                           catalogStringList(compatibility.ScopesJSON),
		"grid_storyboards":                 gridStoryboards,
	}
}

func payloadEqual(left, right map[string]any) bool {
	encodedLeft, err := creation.WorkflowCanonicalJSON(left)
	if err != nil {
		return false
	}
	encodedRight, err := creation.WorkflowCanonicalJSON(right)
	if err != nil {
		return false
	}
	return encodedLeft == encodedRight
}

// skillFromRecords 由三张表的行重建 Skill 定义。
func skillFromRecords(
	definition *repository.CreationSkillDefinition,
	version *repository.CreationSkillVersion,
	compatibility *repository.CreationSkillCompatibility,
) *creation.CreationSkillDefinition {
	var gridStoryboards map[bool]bool
	if compatibility.GridStoryboardsJSON != nil {
		gridStoryboards = map[bool]bool{}
		for _, item := range catalogDecode(*compatibility.GridStoryboardsJSON, []any{}).([]any) {
			if flag, ok := item.(bool); ok {
				gridStoryboards[flag] = true
			}
		}
	}
	scopes := map[string]bool{}
	for _, item := range catalogStringList(compatibility.ScopesJSON) {
		scopes[item] = true
	}
	if len(scopes) == 0 {
		scopes[creation.CreationScopeProject] = true
	}
	skillVersion := creation.CreationSkillVersion{
		ID:                            version.ID,
		SkillID:                       version.SkillID,
		Version:                       version.Version,
		Title:                         version.Title,
		Summary:                       version.Summary,
		Category:                      version.Category,
		WorkflowTemplateRevisionAlias: version.WorkflowTemplateRevisionAlias,
		Compatibility: creation.NewCreationSkillCompatibility(
			stringSetFromList(catalogStringList(compatibility.ContentModesJSON)),
			stringSetFromList(catalogStringList(compatibility.GenerationModesJSON)),
			stringSetFromList(catalogStringList(compatibility.RequiredInputsJSON)),
			scopes,
			gridStoryboards,
		),
		ExpectedOutputs:    catalogStringList(version.ExpectedOutputsJSON),
		ReviewRequired:     version.ReviewRequired,
		EstimatedCostHint:  version.EstimatedCostHint,
		WorkflowRevisionID: version.WorkflowRevisionID,
	}
	return &creation.CreationSkillDefinition{
		ID:            definition.ID,
		Slug:          definition.Slug,
		LatestVersion: skillVersion,
		Official:      true,
		Active:        definition.Active,
	}
}

func stringSetFromList(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}

// GetPersistedCreationSkillVersion 解析供计划编译使用的已发布版本。
func (c *CreationSkillCatalog) GetPersistedCreationSkillVersion(
	ctx context.Context, skillVersionID string,
) (*creation.CreationSkillDefinition, *creation.CreationSkillVersion, error) {
	var version repository.CreationSkillVersion
	if err := c.db.WithContext(ctx).Where("id = ?", skillVersionID).Take(&version).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if version.Status != "published" {
		return nil, nil, nil
	}
	var definition repository.CreationSkillDefinition
	if err := c.db.WithContext(ctx).Where("id = ? AND active = ?", version.SkillID, true).
		Take(&definition).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var compatibility repository.CreationSkillCompatibility
	if err := c.db.WithContext(ctx).Where("skill_version_id = ?", version.ID).
		Take(&compatibility).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	// 版本必须绑定一条已发布的工作流 revision，否则该 Skill 不可执行。
	if version.WorkflowRevisionID == nil || *version.WorkflowRevisionID == "" {
		return nil, nil, nil
	}
	var revision repository.WorkflowRevision
	if err := c.db.WithContext(ctx).Where("id = ?", *version.WorkflowRevisionID).Take(&revision).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if revision.Status != "published" {
		return nil, nil, nil
	}
	skill := skillFromRecords(&definition, &version, &compatibility)
	return skill, &skill.LatestVersion, nil
}

// ensureOfficialWorkflowRevision 解析或发布官方 Skill 版本绑定的不可变工作流 revision。
func (c *CreationSkillCatalog) ensureOfficialWorkflowRevision(
	ctx context.Context, tx *gorm.DB, skill *creation.CreationSkillDefinition, currentRevisionID string,
) (string, error) {
	if currentRevisionID != "" && !strings.HasPrefix(currentRevisionID, "official:") {
		var revision repository.WorkflowRevision
		if err := tx.Where("id = ?", currentRevisionID).Take(&revision).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return "", &CreationSkillManifestConflictError{Message: "Creation Skill " + skill.ID +
					" is bound to missing Workflow Revision " + currentRevisionID}
			}
			return "", err
		}
		if revision.Status != "published" {
			return "", &CreationSkillManifestConflictError{Message: "Creation Skill " + skill.ID +
				" is bound to unpublished Workflow Revision " + currentRevisionID}
		}
		return currentRevisionID, nil
	}
	contentMode := "drama"
	for _, mode := range sortedStringKeys(skill.LatestVersion.Compatibility.ContentModes) {
		contentMode = mode
		break
	}
	generationMode := "storyboard"
	for _, mode := range sortedStringKeys(skill.LatestVersion.Compatibility.GenerationModes) {
		generationMode = mode
		break
	}
	nodes, edges := LegacyLinearGraph()
	definition, err := c.workflows.createDefinition(ctx, tx, WorkflowDefinitionParams{
		WorkspaceID: "official",
		ProjectID:   "__official_creation_skills__",
		Name:        "Official Creation Skill: " + skill.ID + " v" + intToString(skill.LatestVersion.Version),
		ActorID:     "system",
	})
	if err != nil {
		return "", err
	}
	revision, err := c.workflows.createRevision(ctx, tx, WorkflowRevisionParams{
		DefinitionID:   definition["id"].(string),
		ActorID:        "system",
		ContentMode:    contentMode,
		GenerationMode: generationMode,
		InputSchema: map[string]any{
			"type":                 "object",
			"required":             sortedStringKeys(skill.LatestVersion.Compatibility.RequiredInputs),
			"properties":           map[string]any{},
			"additionalProperties": true,
		},
		TemplateLock: map[string]any{
			"kind":                             "official_creation_skill",
			"skill_id":                         skill.ID,
			"skill_version_id":                 skill.LatestVersion.ID,
			"workflow_template_revision_alias": skill.LatestVersion.WorkflowTemplateRevisionAlias,
		},
		Nodes: nodes,
		Edges: edges,
	})
	if err != nil {
		return "", err
	}
	revisionID := revision["id"].(string)
	if _, err := c.workflows.publishRevision(ctx, tx, revisionID, "system"); err != nil {
		return "", err
	}
	return revisionID, nil
}

func intToString(value int64) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	negative := value < 0
	if negative {
		value = -value
	}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

// SyncOfficialCreationSkills 补齐缺失的清单发布，不改动已冻结的发布。
func (c *CreationSkillCatalog) SyncOfficialCreationSkills(ctx context.Context) (int, error) {
	changed := 0
	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		manifestByID := map[string]*creation.CreationSkillDefinition{}
		for i := range creation.OfficialCreationSkills {
			manifestByID[creation.OfficialCreationSkills[i].ID] = &creation.OfficialCreationSkills[i]
		}
		var existing []repository.CreationSkillDefinition
		if err := tx.Where("official = ?", true).Find(&existing).Error; err != nil {
			return err
		}
		for i := range existing {
			if _, ok := manifestByID[existing[i].ID]; !ok && existing[i].Active {
				if err := tx.Model(&repository.CreationSkillDefinition{}).
					Where("id = ?", existing[i].ID).Update("active", false).Error; err != nil {
					return err
				}
				changed++
			}
		}
		for _, skill := range creation.OfficialCreationSkills {
			current := skill
			var definition repository.CreationSkillDefinition
			err := tx.Where("id = ?", current.ID).Take(&definition).Error
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				definition = repository.CreationSkillDefinition{
					ID:        current.ID,
					Slug:      current.Slug,
					Official:  true,
					Active:    current.Active,
					CreatedAt: db.NewFlexibleTime(wfNow()),
					UpdatedAt: db.NewFlexibleTime(wfNow()),
				}
				if err := tx.Create(&definition).Error; err != nil {
					return err
				}
				changed++
			case err != nil:
				return err
			default:
				if definition.Slug != current.Slug || !definition.Official {
					return &CreationSkillManifestConflictError{
						Message: "Creation Skill definition " + current.ID + " conflicts with the manifest",
					}
				}
			}

			var version repository.CreationSkillVersion
			err = tx.Where("id = ?", current.LatestVersion.ID).Take(&version).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				revisionID, err := c.ensureOfficialWorkflowRevision(ctx, tx, &current, "")
				if err != nil {
					return err
				}
				row := repository.CreationSkillVersion{
					ID:                            current.LatestVersion.ID,
					SkillID:                       current.ID,
					Version:                       current.LatestVersion.Version,
					Title:                         current.LatestVersion.Title,
					Summary:                       current.LatestVersion.Summary,
					Category:                      current.LatestVersion.Category,
					WorkflowTemplateRevisionAlias: current.LatestVersion.WorkflowTemplateRevisionAlias,
					WorkflowRevisionID:            &revisionID,
					ExpectedOutputsJSON:           catalogJSON(current.LatestVersion.ExpectedOutputs),
					ReviewRequired:                current.LatestVersion.ReviewRequired,
					EstimatedCostHint:             current.LatestVersion.EstimatedCostHint,
					Status:                        "published",
					FrozenAt:                      db.NewFlexibleTime(wfNow()),
				}
				if err := tx.Create(&row).Error; err != nil {
					return err
				}
				gridJSON := any(nil)
				if current.LatestVersion.Compatibility.HasGridStoryboards() {
					gridJSON = manifestPayload(&current)["grid_storyboards"]
				}
				compatibility := repository.CreationSkillCompatibility{
					SkillVersionID:      current.LatestVersion.ID,
					ContentModesJSON:    catalogJSON(sortedStringKeys(current.LatestVersion.Compatibility.ContentModes)),
					GenerationModesJSON: catalogJSON(sortedStringKeys(current.LatestVersion.Compatibility.GenerationModes)),
					RequiredInputsJSON:  catalogJSON(sortedStringKeys(current.LatestVersion.Compatibility.RequiredInputs)),
					ScopesJSON:          catalogJSON(sortedStringKeys(current.LatestVersion.Compatibility.Scopes)),
				}
				if gridJSON != nil {
					encoded := catalogJSON(gridJSON)
					compatibility.GridStoryboardsJSON = &encoded
				}
				if err := tx.Create(&compatibility).Error; err != nil {
					return err
				}
				changed++
				continue
			}
			if err != nil {
				return err
			}
			var compatibility repository.CreationSkillCompatibility
			err = tx.Where("skill_version_id = ?", version.ID).Take(&compatibility).Error
			if errors.Is(err, gorm.ErrRecordNotFound) ||
				(err == nil && !payloadEqual(storedPayload(&version, &compatibility), manifestPayload(&current))) {
				return &CreationSkillManifestConflictError{
					Message: "Frozen Creation Skill version " + version.ID + " differs from the manifest",
				}
			}
			if err != nil {
				return err
			}
			currentRevision := ""
			if version.WorkflowRevisionID != nil {
				currentRevision = *version.WorkflowRevisionID
			}
			revisionID, err := c.ensureOfficialWorkflowRevision(ctx, tx, &current, currentRevision)
			if err != nil {
				return err
			}
			if currentRevision != revisionID {
				if err := tx.Model(&repository.CreationSkillVersion{}).
					Where("id = ?", version.ID).Update("workflow_revision_id", revisionID).Error; err != nil {
					return err
				}
				changed++
			}
		}
		return nil
	})
	return changed, err
}

// DeactivateCreationSkill 停用 Skill，保留定义与冻结历史。
func (c *CreationSkillCatalog) DeactivateCreationSkill(
	ctx context.Context, skillID, actorRole string,
) (bool, error) {
	if actorRole != "admin" && actorRole != "official" {
		return false, &CreationSkillForbiddenError{Code: "creation_skill_maintenance_forbidden"}
	}
	result := c.db.WithContext(ctx).Model(&repository.CreationSkillDefinition{}).
		Where("id = ?", skillID).Update("active", false)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// CreationSkillForbiddenError 表示角色无权维护 Skill 目录。
type CreationSkillForbiddenError struct{ Code string }

func (e *CreationSkillForbiddenError) Error() string { return e.Code }

// ListCreationSkills 返回激活的已发布版本及其兼容性报告。
func (c *CreationSkillCatalog) ListCreationSkills(
	ctx context.Context, project map[string]any, availableInputs map[string]bool,
) ([]map[string]any, error) {
	var versions []repository.CreationSkillVersion
	if err := c.db.WithContext(ctx).Where("status = ?", "published").
		Order("version DESC").Find(&versions).Error; err != nil {
		return nil, err
	}
	var definitions []repository.CreationSkillDefinition
	if err := c.db.WithContext(ctx).Where("active = ?", true).Find(&definitions).Error; err != nil {
		return nil, err
	}
	activeByID := map[string]repository.CreationSkillDefinition{}
	for _, definition := range definitions {
		activeByID[definition.ID] = definition
	}
	var compatibilities []repository.CreationSkillCompatibility
	if err := c.db.WithContext(ctx).Find(&compatibilities).Error; err != nil {
		return nil, err
	}
	compatByVersion := map[string]repository.CreationSkillCompatibility{}
	for _, compatibility := range compatibilities {
		compatByVersion[compatibility.SkillVersionID] = compatibility
	}
	revisionIDs := make([]string, 0, len(versions))
	for _, version := range versions {
		if version.WorkflowRevisionID != nil && *version.WorkflowRevisionID != "" {
			revisionIDs = append(revisionIDs, *version.WorkflowRevisionID)
		}
	}
	publishedRevisions := map[string]bool{}
	if len(revisionIDs) > 0 {
		var revisions []repository.WorkflowRevision
		if err := c.db.WithContext(ctx).Where("id IN ? AND status = ?", revisionIDs, "published").
			Find(&revisions).Error; err != nil {
			return nil, err
		}
		for _, revision := range revisions {
			publishedRevisions[revision.ID] = true
		}
	}

	// 同一 Skill 只取版本号最大的一条。
	latest := map[string]repository.CreationSkillVersion{}
	order := make([]string, 0, len(versions))
	for _, version := range versions {
		if _, ok := latest[version.SkillID]; !ok {
			latest[version.SkillID] = version
			order = append(order, version.SkillID)
		}
	}
	sort.Strings(order)
	items := make([]map[string]any, 0, len(order))
	for _, skillID := range order {
		version := latest[skillID]
		definition, ok := activeByID[skillID]
		if !ok {
			continue
		}
		compatibility, ok := compatByVersion[version.ID]
		if !ok {
			continue
		}
		if version.WorkflowRevisionID == nil || !publishedRevisions[*version.WorkflowRevisionID] {
			continue
		}
		def := definition
		ver := version
		comp := compatibility
		skill := skillFromRecords(&def, &ver, &comp)
		report := creation.CompatibilityReportWithAlternatives(
			skill, project, availableInputs, alternativeSkillIDs(skill, project, availableInputs),
		)
		items = append(items, map[string]any{
			"id":                   definition.ID,
			"slug":                 definition.Slug,
			"title":                version.Title,
			"summary":              version.Summary,
			"version":              version.Version,
			"version_id":           version.ID,
			"workflow_revision_id": nilAsAnyString(version.WorkflowRevisionID),
			"category":             version.Category,
			"inputs":               sortedStringKeys(skill.LatestVersion.Compatibility.RequiredInputs),
			"outputs":              skill.LatestVersion.ExpectedOutputs,
			"review_required":      version.ReviewRequired,
			"estimated_cost_hint":  nilAsAnyString(version.EstimatedCostHint),
			"compatibility":        report,
		})
	}
	return items, nil
}

// alternativeSkillIDs 列出当前项目可用的其它官方 Skill。
func alternativeSkillIDs(
	skill *creation.CreationSkillDefinition, project map[string]any, availableInputs map[string]bool,
) []string {
	alternatives := make([]string, 0)
	for i := range creation.OfficialCreationSkills {
		other := &creation.OfficialCreationSkills[i]
		if other.ID == skill.ID || !other.Active {
			continue
		}
		if other.LatestVersion.Compatibility.Check(project, availableInputs) == "" {
			alternatives = append(alternatives, other.ID)
		}
	}
	sort.Strings(alternatives)
	return alternatives
}

// ListCreationSkillVersions 列出某个 Skill 的全部历史版本（含停用）。
func (c *CreationSkillCatalog) ListCreationSkillVersions(ctx context.Context, skillID string) ([]map[string]any, error) {
	var rows []repository.CreationSkillVersion
	if err := c.db.WithContext(ctx).Where("skill_id = ?", skillID).
		Order("version DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		current := row
		items = append(items, map[string]any{
			"id":                   row.ID,
			"skill_id":             row.SkillID,
			"workflow_revision_id": nilAsAnyString(row.WorkflowRevisionID),
			"version":              row.Version,
			"title":                row.Title,
			"summary":              row.Summary,
			"category":             row.Category,
			"status":               row.Status,
			"frozen_at":            wfISO(current.FrozenAt),
		})
	}
	return items, nil
}
