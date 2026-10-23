package creative

// 模板市场、审核、预算与智能体 patch（对应 server/services/workflows.py 的下半部分）。
//
// 与 Python 侧一致的两条硬约束：
//  1. 已发布 revision 不可变——patch / upgrade 都落新 draft 再发布；
//  2. 预算结算只允许在预留额度内做减法，任何"释放已关闭预留"的重复调用都幂等。

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain/creation"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
)

// workflowTemplateTypes 是模板市场允许的类型。
var workflowTemplateTypes = map[string]bool{"manga": true, "short_drama": true}

// WorkflowTemplateParams 是创建 / 更新模板草稿的入参。
type WorkflowTemplateParams struct {
	Name           string
	Description    string
	TemplateType   string
	Contract       map[string]any
	Nodes          []map[string]any
	Edges          []map[string]any
	ContentMode    string
	GenerationMode string
	CoverRef       *string
	ActorID        string
}

// validateTemplateContract 校验市场元数据 + 图安全性；问题列表会带在错误参数里。
func validateTemplateContract(contract map[string]any, nodes, edges []map[string]any) error {
	if _, err := ValidateWorkflowTemplateContract(contract, nodes, edges, nil, nil); err != nil {
		var validation *WorkflowContractValidationError
		if errors.As(err, &validation) {
			issues := make([]any, 0, len(validation.Issues))
			for _, issue := range validation.Issues {
				issues = append(issues, map[string]any{
					"code": issue.Code, "path": issue.Path, "message": issue.Message,
				})
			}
			return creation.NewWorkflowValidationError("workflow_template_invalid", map[string]any{
				"reason": "contract", "issues": issues,
			})
		}
		return err
	}
	if raw := contract["aspect_ratios"]; raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return creation.NewWorkflowValidationError("workflow_template_invalid", map[string]any{"reason": "aspect_ratios"})
		}
		for _, item := range list {
			text, isString := item.(string)
			if !isString || (text != "16:9" && text != "9:16" && text != "1:1") {
				return creation.NewWorkflowValidationError("workflow_template_invalid",
					map[string]any{"reason": "aspect_ratios"})
			}
		}
	}
	for _, field := range []string{"copyright_declaration", "author", "license", "example_project", "cover_ref"} {
		if value := contract[field]; value != nil {
			if _, ok := value.(string); !ok {
				return creation.NewWorkflowValidationError("workflow_template_invalid", map[string]any{"reason": field})
			}
		}
	}
	return nil
}

// templatePayload 渲染模板及其用量统计。
func templatePayload(
	template *repository.WorkflowTemplate, stats *repository.WorkflowUsageStat,
) map[string]any {
	payload := map[string]any{
		"id":                    template.ID,
		"scope":                 "marketplace",
		"name_key":              template.Name,
		"description_key":       template.Description,
		"name":                  template.Name,
		"description":           template.Description,
		"cover_ref":             nilAsAny(template.CoverRef),
		"template_type":         template.TemplateType,
		"status":                template.Status,
		"draft_revision_id":     nilAsAny(template.DraftRevisionID),
		"published_revision_id": nilAsAny(template.PublishedRevisionID),
		"contract":              wfDecode(template.ContractJSON, map[string]any{}),
		"created_at":            wfISO(template.CreatedAt),
		"updated_at":            wfISO(template.UpdatedAt),
		"published_at":          wfISO(template.PublishedAt),
		"stats":                 usageStatsView(stats),
	}
	return payload
}

func usageStatsView(stats *repository.WorkflowUsageStat) map[string]any {
	if stats == nil {
		return map[string]any{
			"views": 0, "derivations": 0, "run_count": 0, "successful_run_count": 0,
			"success_rate": 0.0, "average_cost": 0.0, "average_duration_seconds": 0.0, "rating": nil,
		}
	}
	successRate := 0.0
	averageCost := 0.0
	averageDuration := 0.0
	if stats.RunCount > 0 {
		successRate = float64(stats.SuccessfulRunCount) / float64(stats.RunCount)
		averageCost = stats.TotalCost / float64(stats.RunCount)
		averageDuration = stats.TotalDurationSeconds / float64(stats.RunCount)
	}
	var rating any
	if stats.RatingCount > 0 {
		rating = stats.RatingTotal / float64(stats.RatingCount)
	}
	return map[string]any{
		"views":                    stats.Views,
		"derivations":              stats.Derivations,
		"run_count":                stats.RunCount,
		"successful_run_count":     stats.SuccessfulRunCount,
		"success_rate":             successRate,
		"average_cost":             averageCost,
		"average_duration_seconds": averageDuration,
		"rating":                   rating,
	}
}

// CreateTemplateDraft 创建模板草稿及其承载图的工作流定义。
func (s *WorkflowService) CreateTemplateDraft(ctx context.Context, p WorkflowTemplateParams) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if !workflowTemplateTypes[p.TemplateType] {
			return creation.NewWorkflowValidationError("workflow_template_invalid",
				map[string]any{"reason": "template_type"})
		}
		if err := validateTemplateContract(p.Contract, p.Nodes, p.Edges); err != nil {
			return err
		}
		if err := creation.WorkflowValidateGraph(p.Nodes, p.Edges); err != nil {
			return err
		}
		now := db.NewFlexibleTime(wfNow())
		templateID := uuid.NewString()
		template := repository.WorkflowTemplate{
			ID:           templateID,
			UserID:       p.ActorID,
			Name:         p.Name,
			Description:  p.Description,
			CoverRef:     p.CoverRef,
			TemplateType: p.TemplateType,
			Status:       "draft",
			ContractJSON: wfJSON(p.Contract, "{}"),
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := tx.Create(&template).Error; err != nil {
			return err
		}
		definition := repository.WorkflowDefinition{
			ID:          uuid.NewString(),
			UserID:      p.ActorID,
			WorkspaceID: "marketplace",
			ProjectID:   "template:" + templateID,
			Name:        p.Name,
			Scope:       "template",
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := tx.Create(&definition).Error; err != nil {
			return err
		}
		inputSchema := any(map[string]any{})
		if value, ok := p.Contract["input_schema"]; ok && value != nil {
			inputSchema = value
		}
		revision, err := s.createRevision(ctx, tx, WorkflowRevisionParams{
			DefinitionID:   definition.ID,
			Nodes:          p.Nodes,
			Edges:          p.Edges,
			TemplateLock:   map[string]any{"template_id": templateID, "template_schema_version": 1},
			ActorID:        p.ActorID,
			ContentMode:    p.ContentMode,
			GenerationMode: p.GenerationMode,
			InputSchema:    inputSchema,
		})
		if err != nil {
			return err
		}
		revisionID := revision["id"].(string)
		if err := tx.Model(&repository.WorkflowTemplate{}).
			Where("id = ?", templateID).Update("draft_revision_id", revisionID).Error; err != nil {
			return err
		}
		template.DraftRevisionID = &revisionID
		stats := repository.WorkflowUsageStat{TemplateID: templateID}
		if err := tx.Create(&stats).Error; err != nil {
			return err
		}
		out = templatePayload(&template, &stats)
		return nil
	})
	return out, err
}

// UpdateTemplateDraft 更新草稿（仅 draft / rejected 可编辑），落一条新 revision。
func (s *WorkflowService) UpdateTemplateDraft(
	ctx context.Context, templateID string, p WorkflowTemplateParams,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		template, err := loadOwnedTemplate(ctx, tx, templateID, p.ActorID)
		if err != nil {
			return err
		}
		if template.Status != "draft" && template.Status != "rejected" {
			return creation.NewWorkflowValidationError("workflow_template_not_editable",
				map[string]any{"status": template.Status})
		}
		if !workflowTemplateTypes[p.TemplateType] {
			return creation.NewWorkflowValidationError("workflow_template_invalid",
				map[string]any{"reason": "template_type"})
		}
		if err := validateTemplateContract(p.Contract, p.Nodes, p.Edges); err != nil {
			return err
		}
		if err := creation.WorkflowValidateGraph(p.Nodes, p.Edges); err != nil {
			return err
		}
		if template.DraftRevisionID == nil || *template.DraftRevisionID == "" {
			return creation.NewWorkflowValidationError("workflow_template_invalid",
				map[string]any{"reason": "missing_revision"})
		}
		var currentRevision repository.WorkflowRevision
		if err := tx.Where("id = ?", *template.DraftRevisionID).Take(&currentRevision).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return workflowNotFound("workflow_revision_not_found", map[string]any{"id": *template.DraftRevisionID})
			}
			return err
		}
		inputSchema := any(map[string]any{})
		if value, ok := p.Contract["input_schema"]; ok && value != nil {
			inputSchema = value
		}
		revision, err := s.createRevision(ctx, tx, WorkflowRevisionParams{
			DefinitionID:   currentRevision.DefinitionID,
			Nodes:          p.Nodes,
			Edges:          p.Edges,
			TemplateLock:   map[string]any{"template_id": template.ID, "template_schema_version": 1},
			ActorID:        p.ActorID,
			ContentMode:    p.ContentMode,
			GenerationMode: p.GenerationMode,
			InputSchema:    inputSchema,
		})
		if err != nil {
			return err
		}
		revisionID := revision["id"].(string)
		submittedAt := db.FlexibleTime{}
		if err := tx.Model(&repository.WorkflowTemplate{}).Where("id = ?", templateID).Updates(map[string]any{
			"name":              p.Name,
			"description":       p.Description,
			"cover_ref":         p.CoverRef,
			"template_type":     p.TemplateType,
			"contract_json":     wfJSON(p.Contract, "{}"),
			"draft_revision_id": revisionID,
			"status":            "draft",
			"submitted_at":      submittedAt,
			"updated_at":        db.NewFlexibleTime(wfNow()),
		}).Error; err != nil {
			return err
		}
		template.Name = p.Name
		template.Description = p.Description
		template.CoverRef = p.CoverRef
		template.TemplateType = p.TemplateType
		template.ContractJSON = wfJSON(p.Contract, "{}")
		template.DraftRevisionID = &revisionID
		template.Status = "draft"
		stats, err := loadUsageStats(ctx, tx, template.ID, false)
		if err != nil {
			return err
		}
		out = templatePayload(template, stats)
		return nil
	})
	return out, err
}

func loadOwnedTemplate(
	ctx context.Context, tx *gorm.DB, templateID, actorID string,
) (*repository.WorkflowTemplate, error) {
	var template repository.WorkflowTemplate
	if err := tx.Where("id = ?", templateID).Take(&template).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, workflowNotFound("workflow_template_not_found", map[string]any{"id": templateID})
		}
		return nil, err
	}
	if template.UserID != actorID {
		return nil, workflowNotFound("workflow_template_not_found", map[string]any{"id": templateID})
	}
	return &template, nil
}

// loadUsageStats 取模板用量统计；create 为真时缺失就建一条零值行。
func loadUsageStats(
	ctx context.Context, tx *gorm.DB, templateID string, create bool,
) (*repository.WorkflowUsageStat, error) {
	var stats repository.WorkflowUsageStat
	err := tx.Where("template_id = ?", templateID).Take(&stats).Error
	if err == nil {
		return &stats, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if !create {
		return nil, nil
	}
	stats = repository.WorkflowUsageStat{TemplateID: templateID}
	if err := tx.Create(&stats).Error; err != nil {
		return nil, err
	}
	return &stats, nil
}

// GetTemplate 返回模板详情；public 为真时允许读取他人已发布模板。
func (s *WorkflowService) GetTemplate(
	ctx context.Context, templateID, actorID string, public bool,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var template repository.WorkflowTemplate
		if err := tx.Where("id = ?", templateID).Take(&template).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return workflowNotFound("workflow_template_not_found", map[string]any{"id": templateID})
			}
			return err
		}
		if public {
			if template.Status != "published" && template.UserID != actorID {
				return workflowNotFound("workflow_template_not_found", map[string]any{"id": templateID})
			}
		} else if template.UserID != actorID {
			return workflowNotFound("workflow_template_not_found", map[string]any{"id": templateID})
		}
		stats, err := loadUsageStats(ctx, tx, templateID, false)
		if err != nil {
			return err
		}
		payload := templatePayload(&template, stats)
		revisionID := template.PublishedRevisionID
		if revisionID == nil || *revisionID == "" {
			revisionID = template.DraftRevisionID
		}
		if revisionID != nil && *revisionID != "" {
			graph, err := loadGraph(ctx, tx, *revisionID, "")
			if err != nil {
				return err
			}
			payload["revision"] = revisionView(graph)
		}
		out = payload
		return nil
	})
	return out, err
}

// ListCreatorTemplates 列出作者自己的模板，附带审核历史。
func (s *WorkflowService) ListCreatorTemplates(ctx context.Context, actorID string) (map[string]any, error) {
	var rows []repository.WorkflowTemplate
	if err := s.db.WithContext(ctx).Where("user_id = ?", actorID).
		Order("updated_at DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]any, 0, len(rows))
	for i := range rows {
		payload, err := s.GetTemplate(ctx, rows[i].ID, actorID, false)
		if err != nil {
			return nil, err
		}
		var reviews []repository.WorkflowMarketplaceReview
		if err := s.db.WithContext(ctx).Where("template_id = ?", rows[i].ID).
			Order("created_at DESC").Limit(20).Find(&reviews).Error; err != nil {
			return nil, err
		}
		history := make([]any, 0, len(reviews))
		for _, review := range reviews {
			history = append(history, map[string]any{
				"id":          review.ID,
				"revision_id": review.RevisionID,
				"status":      review.Status,
				"decision":    review.Decision,
				"comment":     review.Comment,
				"reviewer_id": review.ReviewerID,
				"created_at":  wfISO(review.CreatedAt),
			})
		}
		payload["review_history"] = history
		payload["reviews"] = history
		items = append(items, payload)
	}
	return map[string]any{"items": items}, nil
}

// SubmitTemplate 把草稿送审。
func (s *WorkflowService) SubmitTemplate(ctx context.Context, templateID, actorID string) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		template, err := loadOwnedTemplate(ctx, tx, templateID, actorID)
		if err != nil {
			return err
		}
		if err := creation.WorkflowTemplateTransition(template.Status, "submitted"); err != nil {
			return err
		}
		if template.DraftRevisionID == nil || *template.DraftRevisionID == "" {
			return creation.NewWorkflowValidationError("workflow_template_invalid",
				map[string]any{"reason": "missing_revision"})
		}
		graph, err := loadGraph(ctx, tx, *template.DraftRevisionID, actorID)
		if err != nil {
			return err
		}
		if err := validateTemplateContract(wfDecodeMap(template.ContractJSON), graph.Nodes, graph.Edges); err != nil {
			return err
		}
		if err := creation.WorkflowValidateGraph(graph.Nodes, graph.Edges); err != nil {
			return err
		}
		if err := tx.Model(&repository.WorkflowTemplate{}).Where("id = ?", templateID).Updates(map[string]any{
			"status":       "submitted",
			"submitted_at": db.NewFlexibleTime(wfNow()),
			"updated_at":   db.NewFlexibleTime(wfNow()),
		}).Error; err != nil {
			return err
		}
		out = map[string]any{"id": templateID, "status": "submitted", "revision_id": *template.DraftRevisionID}
		return nil
	})
	return out, err
}

// WithdrawTemplate 撤回送审，回到草稿。
func (s *WorkflowService) WithdrawTemplate(ctx context.Context, templateID, actorID string) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		template, err := loadOwnedTemplate(ctx, tx, templateID, actorID)
		if err != nil {
			return err
		}
		if err := creation.WorkflowTemplateTransition(template.Status, "draft"); err != nil {
			return err
		}
		if err := tx.Model(&repository.WorkflowTemplate{}).Where("id = ?", templateID).Updates(map[string]any{
			"status":     "draft",
			"updated_at": db.NewFlexibleTime(wfNow()),
		}).Error; err != nil {
			return err
		}
		out = map[string]any{"id": templateID, "status": "draft"}
		return nil
	})
	return out, err
}

// ReviewTemplateParams 是管理员审核入参。
type ReviewTemplateParams struct {
	TemplateID string
	ReviewerID string
	Decision   string
	Comment    string
}

// ReviewTemplate 管理员审核：start 开审，其余落决定并推进模板状态。
func (s *WorkflowService) ReviewTemplate(ctx context.Context, p ReviewTemplateParams) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var template repository.WorkflowTemplate
		if err := tx.Where("id = ?", p.TemplateID).Take(&template).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return workflowNotFound("workflow_template_not_found", map[string]any{"id": p.TemplateID})
			}
			return err
		}
		if p.Decision == "start" {
			if err := creation.WorkflowTemplateTransition(template.Status, "under_review"); err != nil {
				return err
			}
			if template.DraftRevisionID == nil || *template.DraftRevisionID == "" {
				return creation.NewWorkflowValidationError("workflow_template_invalid",
					map[string]any{"reason": "missing_revision"})
			}
			if err := tx.Create(&repository.WorkflowMarketplaceReview{
				ID:         uuid.NewString(),
				TemplateID: template.ID,
				RevisionID: *template.DraftRevisionID,
				Status:     "under_review",
				Decision:   "start",
				Comment:    p.Comment,
				ReviewerID: p.ReviewerID,
				CreatedAt:  db.NewFlexibleTime(wfNow()),
			}).Error; err != nil {
				return err
			}
			if err := tx.Model(&repository.WorkflowTemplate{}).Where("id = ?", p.TemplateID).Updates(map[string]any{
				"status":     "under_review",
				"updated_at": db.NewFlexibleTime(wfNow()),
			}).Error; err != nil {
				return err
			}
			out = map[string]any{
				"id": p.TemplateID, "status": "under_review", "decision": p.Decision, "comment": p.Comment,
			}
			return nil
		}
		target := map[string]string{
			"approve": "published", "reject": "rejected", "changes_requested": "draft",
		}[p.Decision]
		if target == "" {
			return creation.NewWorkflowValidationError("workflow_template_invalid", map[string]any{"reason": "decision"})
		}
		if template.Status == "submitted" {
			template.Status = "under_review"
		} else if template.Status != "under_review" {
			return creation.NewWorkflowValidationError("workflow_template_invalid_transition",
				map[string]any{"status": template.Status, "target": "under_review"})
		}
		template.Status = "under_review"
		if template.DraftRevisionID == nil || *template.DraftRevisionID == "" {
			return creation.NewWorkflowValidationError("workflow_template_invalid",
				map[string]any{"reason": "missing_revision"})
		}
		revisionID := *template.DraftRevisionID
		if err := tx.Create(&repository.WorkflowMarketplaceReview{
			ID:         uuid.NewString(),
			TemplateID: template.ID,
			RevisionID: revisionID,
			Status:     "under_review",
			Decision:   p.Decision,
			Comment:    p.Comment,
			ReviewerID: p.ReviewerID,
			CreatedAt:  db.NewFlexibleTime(wfNow()),
		}).Error; err != nil {
			return err
		}
		updates := map[string]any{"updated_at": db.NewFlexibleTime(wfNow())}
		switch target {
		case "published":
			var revision repository.WorkflowRevision
			if err := tx.Where("id = ?", revisionID).Take(&revision).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return workflowNotFound("workflow_revision_not_found", map[string]any{"id": revisionID})
				}
				return err
			}
			var definition repository.WorkflowDefinition
			if err := tx.Where("id = ?", revision.DefinitionID).Take(&definition).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return workflowNotFound("workflow_revision_not_found", map[string]any{"id": revisionID})
				}
				return err
			}
			graph, err := loadGraph(ctx, tx, revisionID, "")
			if err != nil {
				return err
			}
			if err := validateTemplateContract(wfDecodeMap(template.ContractJSON), graph.Nodes, graph.Edges); err != nil {
				return err
			}
			if err := creation.WorkflowValidateGraph(graph.Nodes, graph.Edges); err != nil {
				return err
			}
			if err := tx.Model(&repository.WorkflowRevision{}).
				Where("id = ?", revisionID).Update("status", "published").Error; err != nil {
				return err
			}
			if err := tx.Model(&repository.WorkflowDefinition{}).
				Where("id = ?", definition.ID).Update("active_revision_id", revisionID).Error; err != nil {
				return err
			}
			updates["published_revision_id"] = revisionID
			updates["status"] = "published"
			updates["published_at"] = db.NewFlexibleTime(wfNow())
		case "rejected":
			updates["status"] = "rejected"
		default:
			updates["status"] = "draft"
		}
		if err := tx.Model(&repository.WorkflowTemplate{}).Where("id = ?", p.TemplateID).Updates(updates).Error; err != nil {
			return err
		}
		out = map[string]any{
			"id":       p.TemplateID,
			"status":   updates["status"],
			"decision": p.Decision,
			"comment":  p.Comment,
		}
		return nil
	})
	return out, err
}

// SetTemplateSuspended 上架 / 下架已发布模板。
func (s *WorkflowService) SetTemplateSuspended(
	ctx context.Context, templateID, reviewerID string, suspended bool,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var template repository.WorkflowTemplate
		if err := tx.Where("id = ?", templateID).Take(&template).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return workflowNotFound("workflow_template_not_found", map[string]any{"id": templateID})
			}
			return err
		}
		target := "published"
		if suspended {
			target = "suspended"
		}
		if err := creation.WorkflowTemplateTransition(template.Status, target); err != nil {
			return err
		}
		if err := tx.Model(&repository.WorkflowTemplate{}).Where("id = ?", templateID).Updates(map[string]any{
			"status":     target,
			"updated_at": db.NewFlexibleTime(wfNow()),
		}).Error; err != nil {
			return err
		}
		out = map[string]any{"id": templateID, "status": target, "actor_id": reviewerID}
		return nil
	})
	return out, err
}

// ListMarketplace 列出市场在售模板，附带可直接导入的 nodes / edges。
func (s *WorkflowService) ListMarketplace(
	ctx context.Context, templateType string, limit int,
) (map[string]any, error) {
	if templateType != "" && !workflowTemplateTypes[templateType] {
		return nil, creation.NewWorkflowValidationError("workflow_template_invalid",
			map[string]any{"reason": "template_type"})
	}
	var rows []repository.WorkflowTemplate
	query := s.db.WithContext(ctx).Where("status = ?", "published")
	if templateType != "" {
		query = query.Where("template_type = ?", templateType)
	}
	if err := query.Order("published_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]any, 0, len(rows))
	for i := range rows {
		detail, err := s.GetTemplate(ctx, rows[i].ID, "", true)
		if err != nil {
			return nil, err
		}
		revision, _ := detail["revision"].(map[string]any)
		delete(detail, "revision")
		detail["template_lock"] = map[string]any{
			"template_id":             rows[i].ID,
			"source_revision_id":      nilAsAny(rows[i].PublishedRevisionID),
			"template_schema_version": 1,
		}
		if revision != nil {
			detail["nodes"] = revision["nodes"]
			detail["edges"] = revision["edges"]
		} else {
			detail["nodes"] = []any{}
			detail["edges"] = []any{}
		}
		items = append(items, detail)
	}
	return map[string]any{"items": items}, nil
}

// RecordTemplateView 累计浏览量。
func (s *WorkflowService) RecordTemplateView(ctx context.Context, templateID string) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var template repository.WorkflowTemplate
		if err := tx.Where("id = ?", templateID).Take(&template).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return workflowNotFound("workflow_template_not_found", map[string]any{"id": templateID})
			}
			return err
		}
		if template.Status != "published" {
			return workflowNotFound("workflow_template_not_found", map[string]any{"id": templateID})
		}
		stats, err := loadUsageStats(ctx, tx, templateID, true)
		if err != nil {
			return err
		}
		if err := tx.Model(&repository.WorkflowUsageStat{}).Where("template_id = ?", templateID).
			Update("views", stats.Views+1).Error; err != nil {
			return err
		}
		out = map[string]any{"template_id": templateID, "views": stats.Views + 1}
		return nil
	})
	return out, err
}

// RateTemplate 给已发布模板打分；作者不能给自己打分。
func (s *WorkflowService) RateTemplate(
	ctx context.Context, templateID, actorID string, rating float64,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if rating < 1 || rating > 5 {
			return creation.NewWorkflowValidationError("workflow_template_invalid", map[string]any{"reason": "rating"})
		}
		var template repository.WorkflowTemplate
		if err := tx.Where("id = ?", templateID).Take(&template).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return workflowNotFound("workflow_template_not_found", map[string]any{"id": templateID})
			}
			return err
		}
		if template.Status != "published" {
			return workflowNotFound("workflow_template_not_found", map[string]any{"id": templateID})
		}
		if template.UserID == actorID {
			return creation.NewWorkflowValidationError("workflow_template_invalid",
				map[string]any{"reason": "author_rating"})
		}
		stats, err := loadUsageStats(ctx, tx, templateID, true)
		if err != nil {
			return err
		}
		total := stats.RatingTotal + rating
		count := stats.RatingCount + 1
		if err := tx.Model(&repository.WorkflowUsageStat{}).Where("template_id = ?", templateID).Updates(map[string]any{
			"rating_total": total,
			"rating_count": count,
		}).Error; err != nil {
			return err
		}
		out = map[string]any{
			"template_id":  templateID,
			"rating":       total / float64(count),
			"rating_count": count,
		}
		return nil
	})
	return out, err
}

// GetTemplateUpgrade 对比当前激活版本与模板最新版本，给出可升级性与差异。
func (s *WorkflowService) GetTemplateUpgrade(
	ctx context.Context, definitionID, actorID string,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result, err := s.getTemplateUpgradeTx(ctx, tx, definitionID, actorID)
		out = result
		return err
	})
	return out, err
}

// getTemplateUpgradeTx 是 GetTemplateUpgrade 的事务内版本，供 UpgradeWorkflowTemplate
// 在同一事务里先比对再落新版本，避免两次读之间模板被改动。
func (s *WorkflowService) getTemplateUpgradeTx(
	ctx context.Context, tx *gorm.DB, definitionID, actorID string,
) (map[string]any, error) {
	var out map[string]any
	upgradeErr := func() error {
		definition, err := loadDefinition(ctx, tx, definitionID, actorID)
		if err != nil {
			return err
		}
		if definition.ActiveRevisionID == nil || *definition.ActiveRevisionID == "" {
			out = map[string]any{"available": false, "reason": "no_active_revision"}
			return nil
		}
		currentGraph, err := loadGraph(ctx, tx, *definition.ActiveRevisionID, actorID)
		if err != nil {
			return err
		}
		templateLock := wfDecodePtr(currentGraph.Revision.TemplateLockJSON, map[string]any{})
		lock, _ := templateLock.(map[string]any)
		templateID, _ := lock["template_id"].(string)
		sourceRevisionID, _ := lock["source_revision_id"].(string)
		if templateID == "" || sourceRevisionID == "" {
			out = map[string]any{"available": false, "reason": "not_template_derived"}
			return nil
		}
		var template repository.WorkflowTemplate
		err = tx.Where("id = ?", templateID).Take(&template).Error
		templateMissing := errors.Is(err, gorm.ErrRecordNotFound)
		if err != nil && !templateMissing {
			return err
		}
		var latest *repository.WorkflowRevision
		if !templateMissing && template.PublishedRevisionID != nil && *template.PublishedRevisionID != "" {
			var row repository.WorkflowRevision
			if err := tx.Where("id = ?", *template.PublishedRevisionID).Take(&row).Error; err == nil {
				latest = &row
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		if templateMissing || latest == nil || template.Status != "published" {
			out = map[string]any{
				"available":                  false,
				"reason":                     "template_unavailable",
				"template_id":                templateID,
				"current_revision_id":        currentGraph.Revision.ID,
				"current_source_revision_id": sourceRevisionID,
			}
			return nil
		}
		if latest.ID == sourceRevisionID {
			out = map[string]any{
				"available":                  false,
				"reason":                     "up_to_date",
				"template_id":                template.ID,
				"current_revision_id":        currentGraph.Revision.ID,
				"current_source_revision_id": sourceRevisionID,
				"latest_revision_id":         latest.ID,
				"latest_revision_no":         latest.RevisionNo,
			}
			return nil
		}
		latestGraph, err := loadGraph(ctx, tx, latest.ID, "")
		if err != nil {
			return err
		}
		currentByKey := map[string]map[string]any{}
		for _, node := range currentGraph.Nodes {
			currentByKey[creation.WorkflowMapString(node, "node_key")] = node
		}
		latestByKey := map[string]map[string]any{}
		for _, node := range latestGraph.Nodes {
			latestByKey[creation.WorkflowMapString(node, "node_key")] = node
		}
		currentEdgesByKey := map[string]map[string]any{}
		for _, edge := range currentGraph.Edges {
			currentEdgesByKey[creation.WorkflowMapString(edge, "edge_key")] = edge
		}
		latestEdgesByKey := map[string]map[string]any{}
		for _, edge := range latestGraph.Edges {
			latestEdgesByKey[creation.WorkflowMapString(edge, "edge_key")] = edge
		}
		changed, added, removed := diffKeys(currentByKey, latestByKey)
		_, addedEdges, removedEdges := diffKeys(currentEdgesByKey, latestEdgesByKey)
		reasons := make([]string, 0, 3)
		if currentGraph.Revision.ContentMode != latestGraph.Revision.ContentMode {
			reasons = append(reasons, "content_mode_changed")
		}
		if currentGraph.Revision.GenerationMode != latestGraph.Revision.GenerationMode {
			reasons = append(reasons, "generation_mode_changed")
		}
		if err := creation.WorkflowValidateGraph(latestGraph.Nodes, latestGraph.Edges); err != nil {
			reasons = append(reasons, "latest_revision_invalid")
		} else if err := creation.WorkflowValidateNodeContracts(latestGraph.Nodes); err != nil {
			reasons = append(reasons, "latest_revision_invalid")
		}
		out = map[string]any{
			"available":                  true,
			"template_id":                template.ID,
			"current_revision_id":        currentGraph.Revision.ID,
			"current_source_revision_id": sourceRevisionID,
			"latest_revision_id":         latest.ID,
			"latest_revision_no":         latest.RevisionNo,
			"compatible":                 len(reasons) == 0,
			"compatibility_reasons":      reasons,
			"estimated_cost_delta":       sumEstimatedCost(latestGraph.Nodes) - sumEstimatedCost(currentGraph.Nodes),
			"changes": map[string]any{
				"added_nodes":   added,
				"removed_nodes": removed,
				"changed_nodes": changed,
				"added_edges":   addedEdges,
				"removed_edges": removedEdges,
			},
		}
		return nil
	}()
	if upgradeErr != nil {
		return nil, upgradeErr
	}
	return out, nil
}

// diffKeys 返回 (changed, added, removed)，全部有序。
func diffKeys(current, latest map[string]map[string]any) (changed []string, added []string, removed []string) {
	changed = make([]string, 0)
	added = make([]string, 0)
	removed = make([]string, 0)
	for key := range current {
		if other, ok := latest[key]; ok {
			left, _ := creation.WorkflowCanonicalJSON(current[key])
			right, _ := creation.WorkflowCanonicalJSON(other)
			if left != right {
				changed = append(changed, key)
			}
		} else {
			removed = append(removed, key)
		}
	}
	for key := range latest {
		if _, ok := current[key]; !ok {
			added = append(added, key)
		}
	}
	sort.Strings(changed)
	sort.Strings(added)
	sort.Strings(removed)
	return changed, added, removed
}

func sumEstimatedCost(nodes []map[string]any) float64 {
	total := 0.0
	for _, node := range nodes {
		total += creation.WorkflowMapFloat(node, "estimated_cost", 0)
	}
	return total
}

// UpgradeWorkflowTemplate 用模板最新版本新建一个已发布版本。
func (s *WorkflowService) UpgradeWorkflowTemplate(
	ctx context.Context, definitionID, actorID string, confirmed bool,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if !confirmed {
			return creation.NewWorkflowValidationError("workflow_template_upgrade_confirmation_required", nil)
		}
		upgrade, err := s.getTemplateUpgradeTx(ctx, tx, definitionID, actorID)
		if err != nil {
			return err
		}
		available, _ := upgrade["available"].(bool)
		if !available {
			reason, _ := upgrade["reason"].(string)
			return workflowConflict("workflow_template_upgrade_unavailable", map[string]any{"reason": reason})
		}
		if compatible, _ := upgrade["compatible"].(bool); !compatible {
			return workflowConflict("workflow_template_upgrade_incompatible", map[string]any{
				"reasons": upgrade["compatibility_reasons"],
			})
		}
		currentRevisionID, _ := upgrade["current_revision_id"].(string)
		latestRevisionID, _ := upgrade["latest_revision_id"].(string)
		currentGraph, err := loadGraph(ctx, tx, currentRevisionID, actorID)
		if err != nil {
			return err
		}
		latestGraph, err := loadGraph(ctx, tx, latestRevisionID, "")
		if err != nil {
			return err
		}
		templateLock, _ := wfDecodePtr(currentGraph.Revision.TemplateLockJSON, map[string]any{}).(map[string]any)
		if templateLock == nil {
			templateLock = map[string]any{}
		}
		templateLock["source_revision_id"] = latestGraph.Revision.ID
		templateLock["upgraded_from_revision_id"] = currentGraph.Revision.ID
		templateLock["upgraded_at"] = wfNow().Format(time.RFC3339Nano)
		revision, err := s.createRevision(ctx, tx, WorkflowRevisionParams{
			DefinitionID:   currentGraph.Revision.DefinitionID,
			Nodes:          latestGraph.Nodes,
			Edges:          latestGraph.Edges,
			TemplateLock:   templateLock,
			ActorID:        actorID,
			ContentMode:    latestGraph.Revision.ContentMode,
			GenerationMode: latestGraph.Revision.GenerationMode,
			InputSchema:    wfDecode(latestGraph.Revision.InputSchemaJSON, map[string]any{}),
		})
		if err != nil {
			return err
		}
		published, err := s.publishRevision(ctx, tx, revision["id"].(string), actorID)
		if err != nil {
			return err
		}
		out = map[string]any{"upgrade": upgrade, "revision": published}
		return nil
	})
	return out, err
}

// DeriveTemplateParams 是由市场模板派生项目工作流的入参。
type DeriveTemplateParams struct {
	TemplateID  string
	WorkspaceID string
	ProjectID   string
	Name        string
	ActorID     string
}

// DeriveTemplate 把已发布模板实例化成项目内的一条工作流。
func (s *WorkflowService) DeriveTemplate(ctx context.Context, p DeriveTemplateParams) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var template repository.WorkflowTemplate
		if err := tx.Where("id = ?", p.TemplateID).Take(&template).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return workflowConflict("workflow_template_not_published", map[string]any{"id": p.TemplateID})
			}
			return err
		}
		if template.Status != "published" || template.PublishedRevisionID == nil || *template.PublishedRevisionID == "" {
			return workflowConflict("workflow_template_not_published", map[string]any{"id": p.TemplateID})
		}
		sourceGraph, err := loadGraph(ctx, tx, *template.PublishedRevisionID, "")
		if err != nil {
			return err
		}
		definition, err := s.createDefinition(ctx, tx, WorkflowDefinitionParams{
			WorkspaceID: p.WorkspaceID, ProjectID: p.ProjectID, Name: p.Name, ActorID: p.ActorID,
		})
		if err != nil {
			return err
		}
		revision, err := s.createRevision(ctx, tx, WorkflowRevisionParams{
			DefinitionID: definition["id"].(string),
			Nodes:        sourceGraph.Nodes,
			Edges:        sourceGraph.Edges,
			TemplateLock: map[string]any{
				"template_id":        template.ID,
				"source_revision_id": sourceGraph.Revision.ID,
				"derived_at":         wfNow().Format(time.RFC3339Nano),
			},
			ActorID:        p.ActorID,
			ContentMode:    sourceGraph.Revision.ContentMode,
			GenerationMode: sourceGraph.Revision.GenerationMode,
			InputSchema:    wfDecode(sourceGraph.Revision.InputSchemaJSON, map[string]any{}),
		})
		if err != nil {
			return err
		}
		if _, err := s.publishRevision(ctx, tx, revision["id"].(string), p.ActorID); err != nil {
			return err
		}
		if err := tx.Model(&repository.WorkflowUsageStat{}).Where("template_id = ?", template.ID).
			Update("derivations", gorm.Expr("derivations + 1")).Error; err != nil {
			return err
		}
		out = map[string]any{
			"definition_id": definition["id"],
			"revision_id":   revision["id"],
			"template_id":   template.ID,
		}
		return nil
	})
	return out, err
}

// ListPendingTemplateReviews 列出待审模板（含风险标签与静态校验）。
func (s *WorkflowService) ListPendingTemplateReviews(
	ctx context.Context, templateType, riskTag string, limit int,
) (map[string]any, error) {
	var rows []repository.WorkflowTemplate
	if err := s.db.WithContext(ctx).Where("status IN ?", []string{"submitted", "under_review"}).
		Order("submitted_at ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]any, 0, len(rows))
	for i := range rows {
		contract := wfDecodeMap(rows[i].ContractJSON)
		tags := make([]string, 0)
		if raw, ok := contract["risk_tags"].([]any); ok {
			for _, item := range raw {
				if text, isString := item.(string); isString {
					tags = append(tags, text)
				}
			}
		}
		if riskTag != "" && !workflowContains(tags, riskTag) {
			continue
		}
		detail, err := s.GetTemplate(ctx, rows[i].ID, rows[i].UserID, false)
		if err != nil {
			return nil, err
		}
		var reviews []repository.WorkflowMarketplaceReview
		if err := s.db.WithContext(ctx).Where("template_id = ?", rows[i].ID).
			Order("created_at DESC").Limit(10).Find(&reviews).Error; err != nil {
			return nil, err
		}
		history := make([]any, 0, len(reviews))
		for _, review := range reviews {
			history = append(history, map[string]any{
				"id":          review.ID,
				"decision":    review.Decision,
				"comment":     review.Comment,
				"reviewer_id": review.ReviewerID,
				"created_at":  wfISO(review.CreatedAt),
			})
		}
		detail["risk_tags"] = tags
		detail["static_validation"] = staticTemplateValidation(detail)
		detail["reviews"] = history
		items = append(items, detail)
	}
	return map[string]any{"items": items}, nil
}

// staticTemplateValidation 对详情里的图做一次离线一致性检查。
func staticTemplateValidation(payload map[string]any) map[string]any {
	graph := payload
	if value, ok := payload["active_revision"].(map[string]any); ok {
		graph = value
	} else if value, ok := payload["revision"].(map[string]any); ok {
		graph = value
	}
	nodeKeys := map[string]bool{}
	nodes := make([]map[string]any, 0)
	if raw, ok := graph["nodes"].([]any); ok {
		for _, item := range raw {
			if entry, isMap := item.(map[string]any); isMap {
				nodes = append(nodes, entry)
				key := firstNodeKey(entry)
				nodeKeys[key] = true
			}
		}
	}
	edges := make([]map[string]any, 0)
	if raw, ok := graph["edges"].([]any); ok {
		for _, item := range raw {
			if entry, isMap := item.(map[string]any); isMap {
				edges = append(edges, entry)
			}
		}
	}
	missingEndpoints := make([]any, 0)
	for _, edge := range edges {
		source := edgeEndpoint(edge, "source")
		target := edgeEndpoint(edge, "target")
		if !nodeKeys[source] || !nodeKeys[target] {
			missingEndpoints = append(missingEndpoints, edge)
		}
	}
	return map[string]any{
		"valid":             len(nodes) > 0 && len(missingEndpoints) == 0,
		"node_count":        len(nodes),
		"edge_count":        len(edges),
		"missing_endpoints": missingEndpoints,
	}
}

func firstNodeKey(node map[string]any) string {
	for _, key := range []string{"node_key", "key", "id"} {
		if text, ok := node[key].(string); ok && text != "" {
			return text
		}
	}
	return ""
}

func workflowContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// --- 智能体 patch ---

// ValidatePatchForRun 对运行基线做一次 dry-run 校验，不落库。
func (s *WorkflowService) ValidatePatchForRun(
	ctx context.Context, runID string, patch creation.WorkflowPatch, actorID string,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		run, graph, err := loadRunGraph(ctx, tx, runID, actorID)
		if err != nil {
			return err
		}
		if patch.BaseRevisionID != graph.Revision.ID {
			return workflowConflict("workflow_patch_stale_revision", map[string]any{
				"expected": graph.Revision.ID, "actual": patch.BaseRevisionID,
			})
		}
		completed, err := completedPatchNodes(ctx, tx, runID)
		if err != nil {
			return err
		}
		preview, err := creation.WorkflowValidatePatch(
			graph.Nodes, graph.Edges, patch, patchRemainingBudget(run), true, completed,
		)
		if err != nil {
			return err
		}
		if !config.AutoOptimizationEnabled() {
			preview["requires_confirmation"] = true
		}
		out = preview
		return nil
	})
	return out, err
}

// ApplyPatchForRun 授权后落一个新 draft 版本；start 为真时立即发布并规划运行。
func (s *WorkflowService) ApplyPatchForRun(
	ctx context.Context, runID string, patch creation.WorkflowPatch, actorID string, confirmed, start bool,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		run, graph, err := loadRunGraph(ctx, tx, runID, actorID)
		if err != nil {
			return err
		}
		if patch.BaseRevisionID != graph.Revision.ID {
			return workflowConflict("workflow_patch_stale_revision", map[string]any{
				"expected": graph.Revision.ID, "actual": patch.BaseRevisionID,
			})
		}
		completed, err := completedPatchNodes(ctx, tx, runID)
		if err != nil {
			return err
		}
		preview, err := creation.WorkflowValidatePatch(
			graph.Nodes, graph.Edges, patch, patchRemainingBudget(run), confirmed, completed,
		)
		if err != nil {
			return err
		}
		if requires, _ := preview["requires_confirmation"].(bool); requires && !confirmed {
			return creation.NewWorkflowValidationError("workflow_patch_confirmation_required",
				map[string]any{"operation": "patch"})
		}
		nextNodes, nextEdges, err := creation.WorkflowApplyPatchToGraph(graph.Nodes, graph.Edges, patch)
		if err != nil {
			return err
		}
		templateLock, _ := wfDecodePtr(graph.Revision.TemplateLockJSON, map[string]any{}).(map[string]any)
		if templateLock == nil {
			templateLock = map[string]any{}
		}
		templateLock["parent_revision_id"] = graph.Revision.ID
		templateLock["patch_reason"] = patch.Reason
		templateLock["patch_scope"] = patch.Scope
		created, err := s.createRevision(ctx, tx, WorkflowRevisionParams{
			DefinitionID:   graph.Revision.DefinitionID,
			Nodes:          nextNodes,
			Edges:          nextEdges,
			TemplateLock:   templateLock,
			ActorID:        actorID,
			ContentMode:    graph.Revision.ContentMode,
			GenerationMode: graph.Revision.GenerationMode,
			InputSchema:    wfDecode(graph.Revision.InputSchemaJSON, map[string]any{}),
		})
		if err != nil {
			return err
		}
		response := map[string]any{
			"revision_id":          created["id"],
			"status":               created["status"],
			"parent_revision_id":   graph.Revision.ID,
			"affected_nodes":       preview["affected_nodes"],
			"estimated_cost_delta": preview["estimated_cost_delta"],
		}
		if start {
			published, err := s.publishRevision(ctx, tx, created["id"].(string), actorID)
			if err != nil {
				return err
			}
			planned, err := s.planRun(ctx, tx, WorkflowPlanRunParams{
				RevisionID:       created["id"].(string),
				WorkspaceID:      run.WorkspaceID,
				ProjectID:        run.ProjectID,
				ScriptRevisionID: run.ScriptRevisionID,
				Mode:             run.Mode,
				InputSnapshot:    wfDecodeMap(run.InputSnapshotJSON),
				ActorID:          actorID,
				EpisodeID:        run.EpisodeID,
				BudgetLimit:      run.BudgetLimit,
			})
			if err != nil {
				return err
			}
			response["published"] = published
			response["run"] = planned
		}
		out = response
		return nil
	})
	return out, err
}

func loadRunGraph(
	ctx context.Context, tx *gorm.DB, runID, actorID string,
) (*repository.WorkflowRun, *workflowGraph, error) {
	var run repository.WorkflowRun
	if err := tx.Where("id = ?", runID).Take(&run).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, workflowNotFound("workflow_run_not_found", map[string]any{"id": runID})
		}
		return nil, nil, err
	}
	if run.UserID != actorID {
		return nil, nil, workflowNotFound("workflow_run_not_found", map[string]any{"id": runID})
	}
	graph, err := loadGraph(ctx, tx, run.WorkflowRevisionID, actorID)
	if err != nil {
		return nil, nil, err
	}
	return &run, graph, nil
}

func patchRemainingBudget(run *repository.WorkflowRun) *float64 {
	if run.BudgetLimit == nil {
		return nil
	}
	remaining := *run.BudgetLimit - run.SpentAmount - run.ReservedAmount
	return &remaining
}

func completedPatchNodes(ctx context.Context, tx *gorm.DB, runID string) (map[string]bool, error) {
	var rows []repository.WorkflowNodeRun
	if err := tx.Where("workflow_run_id = ?", runID).Find(&rows).Error; err != nil {
		return nil, err
	}
	completed := map[string]bool{}
	for _, row := range rows {
		if workflowPassedNodeStatuses[row.Status] {
			completed[row.NodeKey] = true
		}
	}
	return completed, nil
}

// --- 预算 ---

// ReserveRunBudget 在运行剩余额度内预留预算。
func (s *WorkflowService) ReserveRunBudget(
	ctx context.Context, runID, actorID string, amount float64,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if !isFinite(amount) || amount <= 0 {
			return creation.NewWorkflowValidationError("workflow_input_invalid",
				map[string]any{"reason": "budget_reservation"})
		}
		run, err := loadOwnedRun(ctx, tx, runID, actorID)
		if err != nil {
			return err
		}
		remaining := patchRemainingBudget(run)
		if remaining != nil && amount > *remaining {
			return creation.NewWorkflowValidationError("workflow_budget_exceeded",
				map[string]any{"requested": amount, "remaining": *remaining})
		}
		reservation := repository.BudgetReservation{
			ID:              uuid.NewString(),
			WorkspaceID:     run.WorkspaceID,
			WorkflowRunID:   run.ID,
			Currency:        "USD",
			EstimatedAmount: amount,
			ReservedAmount:  amount,
			SettledAmount:   0,
			Status:          "reserved",
		}
		if err := tx.Create(&reservation).Error; err != nil {
			return err
		}
		reserved := run.ReservedAmount + amount
		if err := tx.Model(&repository.WorkflowRun{}).
			Where("id = ?", runID).Update("reserved_amount", reserved).Error; err != nil {
			return err
		}
		out = map[string]any{
			"reservation_id":   reservation.ID,
			"run_id":           run.ID,
			"reserved_amount":  reserved,
			"remaining_amount": remainingBudget(run.BudgetLimit, run.SpentAmount, reserved, false),
		}
		return nil
	})
	return out, err
}

// SettleRunBudget 用预留额度抵扣实际花费。
func (s *WorkflowService) SettleRunBudget(
	ctx context.Context, reservationID, actorID string, amount float64,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if !isFinite(amount) || amount < 0 {
			return creation.NewWorkflowValidationError("workflow_input_invalid",
				map[string]any{"reason": "settled_amount"})
		}
		var reservation repository.BudgetReservation
		if err := tx.Where("id = ?", reservationID).Take(&reservation).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return workflowNotFound("workflow_budget_reservation_not_found",
					map[string]any{"id": reservationID})
			}
			return err
		}
		run, err := loadOwnedRun(ctx, tx, reservation.WorkflowRunID, actorID)
		if err != nil {
			return err
		}
		if reservation.Status == "released" || reservation.Status == "settled" {
			if amount == 0 {
				out = budgetSnapshot(reservation, run)
				return nil
			}
			return creation.NewWorkflowValidationError("workflow_budget_reservation_closed",
				map[string]any{"id": reservation.ID})
		}
		if amount > reservation.ReservedAmount {
			return creation.NewWorkflowValidationError("workflow_input_invalid",
				map[string]any{"reason": "settled_amount"})
		}
		if run.BudgetLimit != nil && run.SpentAmount+amount > *run.BudgetLimit+1e-9 {
			return creation.NewWorkflowValidationError("workflow_budget_exceeded",
				map[string]any{"spent": run.SpentAmount + amount, "limit": *run.BudgetLimit})
		}
		reserved := reservation.ReservedAmount - amount
		settled := reservation.SettledAmount + amount
		status := "reserved"
		if reserved <= 1e-9 {
			reserved = 0
			status = "settled"
		}
		if err := tx.Model(&repository.BudgetReservation{}).Where("id = ?", reservationID).Updates(map[string]any{
			"reserved_amount": reserved,
			"settled_amount":  settled,
			"status":          status,
		}).Error; err != nil {
			return err
		}
		runReserved := math.Max(0, run.ReservedAmount-amount)
		spent := run.SpentAmount + amount
		if err := tx.Model(&repository.WorkflowRun{}).Where("id = ?", run.ID).Updates(map[string]any{
			"reserved_amount": runReserved,
			"spent_amount":    spent,
		}).Error; err != nil {
			return err
		}
		out = map[string]any{
			"reservation_id":   reservation.ID,
			"spent_amount":     spent,
			"reserved_amount":  runReserved,
			"remaining_amount": remainingBudget(run.BudgetLimit, spent, runReserved, false),
		}
		return nil
	})
	return out, err
}

// ReleaseRunBudget 释放未结算的预留；对已关闭预留且无金额请求时幂等。
func (s *WorkflowService) ReleaseRunBudget(
	ctx context.Context, reservationID, actorID string, amount *float64,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if amount != nil && (!isFinite(*amount) || *amount < 0) {
			return creation.NewWorkflowValidationError("workflow_input_invalid",
				map[string]any{"reason": "released_amount"})
		}
		var reservation repository.BudgetReservation
		if err := tx.Where("id = ?", reservationID).Take(&reservation).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return workflowNotFound("workflow_budget_reservation_not_found",
					map[string]any{"id": reservationID})
			}
			return err
		}
		run, err := loadOwnedRun(ctx, tx, reservation.WorkflowRunID, actorID)
		if err != nil {
			return err
		}
		outstanding := math.Max(0, reservation.ReservedAmount)
		if outstanding == 0 {
			if amount != nil && *amount != 0 {
				return creation.NewWorkflowValidationError("workflow_budget_reservation_closed",
					map[string]any{"id": reservation.ID})
			}
			out = budgetSnapshot(reservation, run)
			return nil
		}
		releaseAmount := outstanding
		if amount != nil {
			releaseAmount = *amount
		}
		if releaseAmount > outstanding {
			return creation.NewWorkflowValidationError("workflow_input_invalid",
				map[string]any{"reason": "released_amount"})
		}
		reserved := math.Max(0, outstanding-releaseAmount)
		status := "reserved"
		if reserved <= 1e-9 {
			reserved = 0
			status = "released"
		}
		if err := tx.Model(&repository.BudgetReservation{}).Where("id = ?", reservationID).Updates(map[string]any{
			"reserved_amount": reserved,
			"status":          status,
		}).Error; err != nil {
			return err
		}
		runReserved := math.Max(0, run.ReservedAmount-releaseAmount)
		if err := tx.Model(&repository.WorkflowRun{}).
			Where("id = ?", run.ID).Update("reserved_amount", runReserved).Error; err != nil {
			return err
		}
		out = map[string]any{
			"reservation_id":   reservation.ID,
			"spent_amount":     run.SpentAmount,
			"reserved_amount":  runReserved,
			"remaining_amount": remainingBudget(run.BudgetLimit, run.SpentAmount, runReserved, false),
		}
		return nil
	})
	return out, err
}

// ReleaseRunBudgets 释放终态运行的所有未结算预留，供 worker 崩溃后重试。
func (s *WorkflowService) ReleaseRunBudgets(ctx context.Context, runID, actorID string) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		run, err := loadOwnedRun(ctx, tx, runID, actorID)
		if err != nil {
			return err
		}
		var reservations []repository.BudgetReservation
		if err := tx.Where("workflow_run_id = ?", runID).Find(&reservations).Error; err != nil {
			return err
		}
		released := 0.0
		for _, reservation := range reservations {
			outstanding := math.Max(0, reservation.ReservedAmount)
			if outstanding == 0 {
				continue
			}
			if err := tx.Model(&repository.BudgetReservation{}).Where("id = ?", reservation.ID).Updates(map[string]any{
				"reserved_amount": 0,
				"status":          "released",
			}).Error; err != nil {
				return err
			}
			released += outstanding
		}
		runReserved := math.Max(0, run.ReservedAmount-released)
		if err := tx.Model(&repository.WorkflowRun{}).
			Where("id = ?", runID).Update("reserved_amount", runReserved).Error; err != nil {
			return err
		}
		out = map[string]any{
			"run_id":           run.ID,
			"released_amount":  released,
			"spent_amount":     run.SpentAmount,
			"reserved_amount":  runReserved,
			"remaining_amount": remainingBudget(run.BudgetLimit, run.SpentAmount, runReserved, false),
		}
		return nil
	})
	return out, err
}

func loadOwnedRun(ctx context.Context, tx *gorm.DB, runID, actorID string) (*repository.WorkflowRun, error) {
	var run repository.WorkflowRun
	if err := tx.Where("id = ?", runID).Take(&run).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, workflowNotFound("workflow_run_not_found", map[string]any{"id": runID})
		}
		return nil, err
	}
	if run.UserID != actorID {
		return nil, workflowNotFound("workflow_run_not_found", map[string]any{"id": runID})
	}
	return &run, nil
}

func budgetSnapshot(reservation repository.BudgetReservation, run *repository.WorkflowRun) map[string]any {
	return map[string]any{
		"reservation_id":   reservation.ID,
		"spent_amount":     run.SpentAmount,
		"reserved_amount":  run.ReservedAmount,
		"remaining_amount": remainingBudget(run.BudgetLimit, run.SpentAmount, run.ReservedAmount, false),
	}
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// ReservationByID 读取预算预留；不存在时按 404 处理。
func (s *WorkflowService) ReservationByID(ctx context.Context, reservationID string) (*repository.BudgetReservation, error) {
	var reservation repository.BudgetReservation
	if err := s.db.WithContext(ctx).Where("id = ?", reservationID).Take(&reservation).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, workflowNotFound("workflow_budget_reservation_not_found",
				map[string]any{"id": reservationID})
		}
		return nil, err
	}
	return &reservation, nil
}
