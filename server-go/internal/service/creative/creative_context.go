package creative

import "github.com/shotwise/shotwise/internal/service"

// 创意板创建上下文的持久化装配（对应 server/routers/creative_context.py 的
// _load_persisted_context：从 DB 加载 board / plan / run 并组装 payload，
// fail-closed 解析本身在 internal/domain/creative_context.go）。

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/repository"
	"gorm.io/gorm"
)

// CreativeContextService 承载创建上下文解析的持久化装配。
type CreativeContextService struct {
	boards *repository.CreativeBoardRepo
	store  *service.ProjectStore
	db     *gorm.DB
}

// NewCreativeContextService 构造服务。
func NewCreativeContextService(gdb *gorm.DB, store *service.ProjectStore) *CreativeContextService {
	return &CreativeContextService{boards: repository.NewCreativeBoardRepo(gdb), store: store, db: gdb}
}

// CreativeContextRequest 是两个端点共用的请求参数（对应 CreativeContextRequest /
// ContextReferenceResolutionRequest 的交集与并集字段）。
type CreativeContextRequest struct {
	CreativeBoardID        string
	EpisodeID              *string
	ShotID                 *string
	SelectedResources      []domain.SelectedResource
	ContextReferences      []domain.ContextReference
	CurrentSkillID         *string
	CreationPlanID         string
	WorkflowRunID          *string
	Previewed              *bool
	Confirmed              bool
	QualityGatePassed      *bool
	ApprovalStatus         *string
	AllowGateBypass        bool
	GenerationMode         *string
	GenerationModeOverride *string
}

// ResolveCreationContext 装配持久化上下文并解析（对应 resolve_context 端点）。
func (s *CreativeContextService) ResolveCreationContext(
	ctx context.Context, projectID, userID string, req CreativeContextRequest,
) (map[string]any, error) {
	project, err := s.projectMap(projectID)
	if err != nil {
		return nil, err
	}
	board, boardItems, err := s.loadBoard(ctx, projectID, userID, req.CreativeBoardID)
	if err != nil {
		return nil, err
	}
	planPayload, err := s.loadPlanPayload(ctx, userID, req.CreationPlanID)
	if err != nil {
		return nil, err
	}
	runPayload, err := s.loadRunPayload(ctx, projectID, userID, req.WorkflowRunID)
	if err != nil {
		return nil, err
	}
	boardPayload := map[string]any{
		"id": board.ID, "project_id": board.ProjectID, "items": boardItems,
	}
	return domain.ResolveCreationContext(domain.ResolveCreationContextParams{
		ProjectID:              projectID,
		Project:                project,
		SelectedResources:      req.SelectedResources,
		CreativeBoardID:        req.CreativeBoardID,
		EpisodeID:              req.EpisodeID,
		ShotID:                 req.ShotID,
		CurrentSkillID:         req.CurrentSkillID,
		CreationPlanID:         req.CreationPlanID,
		WorkflowRunID:          req.WorkflowRunID,
		Board:                  boardPayload,
		BoardItems:             boardItems,
		MediaAssets:            nil,
		CreationPlan:           planPayload,
		WorkflowRun:            runPayload,
		Previewed:              req.Previewed,
		Confirmed:              req.Confirmed,
		QualityGatePassed:      req.QualityGatePassed,
		ApprovalStatus:         req.ApprovalStatus,
		AllowGateBypass:        req.AllowGateBypass,
		GenerationMode:         req.GenerationMode,
		GenerationModeOverride: req.GenerationModeOverride,
		ContextReferences:      req.ContextReferences,
	})
}

// ResolveContextReferences 在 Creation service.Plan 存在之前解析自然语言引用
// （对应 resolve_references 端点）。
func (s *CreativeContextService) ResolveContextReferences(
	ctx context.Context, projectID, userID string, req CreativeContextRequest,
) (map[string]any, error) {
	project, err := s.projectMap(projectID)
	if err != nil {
		return nil, err
	}
	_, boardItems, err := s.loadBoard(ctx, projectID, userID, req.CreativeBoardID)
	if err != nil {
		return nil, err
	}
	return domain.ResolveContextReferences(domain.ResolveContextReferencesParams{
		ProjectID:         projectID,
		Project:           project,
		References:        req.ContextReferences,
		SelectedResources: req.SelectedResources,
		BoardItems:        boardItems,
		MediaAssets:       nil,
		EpisodeID:         req.EpisodeID,
		ShotID:            req.ShotID,
	})
}

// projectMap 把 project.json 读成通用 map（等价 Python 侧 load_project 返回 dict）。
func (s *CreativeContextService) projectMap(projectID string) (map[string]any, error) {
	pf, err := s.store.Get(projectID)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(pf)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// loadBoard 加载属主匹配的创意板与条目（对应 _load_persisted_context 的 board 段）。
func (s *CreativeContextService) loadBoard(
	ctx context.Context, projectID, userID, boardID string,
) (*repository.CreativeBoard, []any, error) {
	board, err := s.boards.GetOwnedBoard(ctx, boardID, userID)
	if err != nil {
		return nil, nil, err
	}
	if board == nil {
		return nil, nil, &domain.CreativeContextResolutionError{
			Code: "creative_board_not_found", Details: map[string]any{"creative_board_id": boardID},
		}
	}
	if board.ProjectID != projectID {
		return nil, nil, &domain.CreativeContextResolutionError{Code: "creative_board_project_mismatch"}
	}
	items, err := s.boards.ListItems(ctx, board.ID)
	if err != nil {
		return nil, nil, err
	}
	payload := make([]any, 0, len(items))
	for i := range items {
		payload = append(payload, boardItemPayload(items[i]))
	}
	return board, payload, nil
}

// boardItemPayload 把板条目转成通用 map；仅保留 domain 候选构建读取的字段
// （Python 侧直接把 ORM 对象传进解析器，未定义属性经 getattr 取 None）。
func boardItemPayload(item repository.CreativeBoardItem) map[string]any {
	return map[string]any{
		"id":                    item.ID,
		"board_id":              item.BoardID,
		"item_type":             item.ItemType,
		"resource_type":         item.ResourceType,
		"resource_id":           item.ResourceID,
		"display_settings_json": item.DisplaySettingsJSON,
	}
}

// loadPlanPayload 加载 CreationPlan 并组装解析器 payload（对应 plan_payload 段）。
func (s *CreativeContextService) loadPlanPayload(
	ctx context.Context, userID, planID string,
) (map[string]any, error) {
	var plan repository.CreationPlan
	err := s.db.WithContext(ctx).Where("id = ? AND user_id = ?", planID, userID).Take(&plan).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, &domain.CreativeContextResolutionError{
			Code: "creation_plan_not_found", Details: map[string]any{"creation_plan_id": planID},
		}
	}
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"id":                        plan.ID,
		"project_id":                plan.ProjectID,
		"status":                    plan.Status,
		"skill_id":                  nilIfEmpty(plan.SkillID),
		"creation_skill_version_id": nilIfEmpty(plan.CreationSkillVersionID),
		"project_snapshot":          creativeJSONValue(plan.ProjectSnapshotJSON),
		"resource_ids":              creativeJSONValue(plan.ResourceIdsJSON),
		"parameters":                creativeJSONValue(plan.ParametersJSON),
		"preview_json":              nilIfEmpty(plan.PreviewJSON),
		"estimated_cost":            plan.EstimatedCost,
		"previewed":                 creativePlanPreviewStatuses[plan.Status],
	}
	if preview, ok := creativeJSONValue(plan.PreviewJSON).(map[string]any); ok {
		for key, value := range preview {
			payload[key] = value
		}
	}
	return payload, nil
}

// creativePlanPreviewStatuses 对应 plan_payload 里 previewed 字段的 status 集合。
var creativePlanPreviewStatuses = map[string]bool{
	"previewed": true, "confirmed": true, "started": true, "running": true, "succeeded": true,
}

// loadRunPayload 加载 WorkflowRun（可选）及其 revision 的 generation_mode。
func (s *CreativeContextService) loadRunPayload(
	ctx context.Context, projectID, userID string, runID *string,
) (map[string]any, error) {
	if runID == nil || *runID == "" {
		return nil, nil
	}
	var run repository.WorkflowRun
	err := s.db.WithContext(ctx).Where("id = ? AND user_id = ?", *runID, userID).Take(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, &domain.CreativeContextResolutionError{
			Code: "workflow_run_not_found", Details: map[string]any{"workflow_run_id": *runID},
		}
	}
	if err != nil {
		return nil, err
	}
	if run.ProjectID != projectID {
		return nil, &domain.CreativeContextResolutionError{Code: "workflow_run_project_mismatch"}
	}
	payload := map[string]any{
		"id": run.ID, "project_id": run.ProjectID, "status": run.Status,
		// workflow_runs 表没有 error_code 列，Python 侧 getattr 恒 None。
		"error_code": nil,
	}
	var revision repository.WorkflowRevision
	if err := s.db.WithContext(ctx).Where("id = ?", run.WorkflowRevisionID).Take(&revision).Error; err == nil {
		payload["generation_mode"] = revision.GenerationMode
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	return payload, nil
}

// creativeJSONValue 对应 _json_value：空串或非法 JSON 返回 nil。
func creativeJSONValue(value string) any {
	if value == "" {
		return nil
	}
	var out any
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		return nil
	}
	return out
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
