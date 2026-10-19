package service

// Shotwise 工作流服务（对应 server/services/workflows.py）。
//
// 覆盖定义 / 版本 / 运行 / 事件日志四组语义。模板市场、审核、预算与智能体 patch
// 在 workflow_templates.go，安全契约校验在 workflow_contracts.go。
//
// 两条不变式照抄 Python 侧：
//  1. 已发布 revision 永不被改动——revert / patch / upgrade 都是"新建 draft + 发布"；
//  2. 每个状态跃迁都同时写 project_event_log，事件是审计与断点续传的唯一凭据。

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
)

// WorkflowNotFoundError 表示资源不存在或不属于请求用户（HTTP 404）。
type WorkflowNotFoundError struct {
	Code   string
	Params map[string]any
}

func (e *WorkflowNotFoundError) Error() string { return e.Code }

// WorkflowConflictError 表示与资源当前状态冲突（HTTP 409）。
type WorkflowConflictError struct {
	Code   string
	Params map[string]any
}

func (e *WorkflowConflictError) Error() string { return e.Code }

func workflowNotFound(code string, params map[string]any) error {
	if params == nil {
		params = map[string]any{}
	}
	return &WorkflowNotFoundError{Code: code, Params: params}
}

func workflowConflict(code string, params map[string]any) error {
	if params == nil {
		params = map[string]any{}
	}
	return &WorkflowConflictError{Code: code, Params: params}
}

// workflowActiveRunStatuses 参与 plan_run 去重的非终态。
var workflowActiveRunStatuses = []string{"planned", "running", "paused", "waiting_review"}

// workflowPassedNodeStatuses 重试时可沿用的"已通过"节点状态。
var workflowPassedNodeStatuses = map[string]bool{"succeeded": true, "skipped": true}

// WorkflowService 是工作流领域服务。
type WorkflowService struct{ db *gorm.DB }

// NewWorkflowService 构造工作流服务。
func NewWorkflowService(gdb *gorm.DB) *WorkflowService { return &WorkflowService{db: gdb} }

// --- JSON 小工具 ---

// wfJSON 输出 canonical JSON；序列化失败时退回 fallback（调用点均为可选字段）。
func wfJSON(value any, fallback string) string {
	encoded, err := domain.WorkflowCanonicalJSON(value)
	if err != nil {
		return fallback
	}
	return encoded
}

// wfDecode 解析 JSON 列；空串或解析失败返回 fallback。
func wfDecode(raw string, fallback any) any {
	if raw == "" {
		return fallback
	}
	var out any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return fallback
	}
	return out
}

// wfDecodePtr 解析可空 JSON 列；NULL 或解析失败返回 fallback。
func wfDecodePtr(raw *string, fallback any) any {
	if raw == nil || *raw == "" {
		return fallback
	}
	return wfDecode(*raw, fallback)
}

func wfDecodeMap(raw string) map[string]any {
	if value, ok := wfDecode(raw, map[string]any{}).(map[string]any); ok {
		return value
	}
	return map[string]any{}
}

func wfDecodeList(raw string) []any {
	if value, ok := wfDecode(raw, []any{}).([]any); ok {
		return value
	}
	return []any{}
}

func wfNow() time.Time { return time.Now().UTC() }

func wfISO(value db.FlexibleTime) any {
	if !value.Valid {
		return nil
	}
	return value.Time.UTC().Format(time.RFC3339Nano)
}

// --- 图视图 ---

type workflowGraph struct {
	Revision *repository.WorkflowRevision
	Nodes    []map[string]any
	Edges    []map[string]any
}

func wfNodeView(row repository.WorkflowNode) map[string]any {
	return map[string]any{
		"node_key":              row.NodeKey,
		"node_type":             row.NodeType,
		"node_type_version":     row.NodeTypeVersion,
		"config_schema_version": row.ConfigSchemaVersion,
		"config":                wfDecode(row.ConfigJSON, map[string]any{}),
		"ui_position":           wfDecodePtr(row.UiPositionJSON, nil),
		"weight":                row.Weight,
		"retry_policy":          wfDecodePtr(row.RetryPolicyJSON, nil),
		"approval_policy":       wfDecodePtr(row.ApprovalPolicyJSON, nil),
		"input_schema":          wfDecode(row.InputSchemaJSON, map[string]any{}),
		"output_schema":         wfDecode(row.OutputSchemaJSON, map[string]any{}),
		"executor_id":           row.ExecutorID,
		"required_capabilities": wfDecode(row.RequiredCapabilitiesJSON, []any{}),
		"estimated_cost":        row.EstimatedCost,
		"cache_policy":          row.CachePolicy,
	}
}

func wfEdgeView(row repository.WorkflowEdge) map[string]any {
	return map[string]any{
		"edge_key":        row.EdgeKey,
		"source_node_key": row.SourceNodeKey,
		"target_node_key": row.TargetNodeKey,
		"condition":       wfDecodePtr(row.ConditionJSON, nil),
		"on_failure":      row.OnFailure,
		"priority":        row.Priority,
	}
}

// loadGraph 读取 revision 及其节点/边；actorID 非空时要求定义归属该用户。
func loadGraph(ctx context.Context, tx *gorm.DB, revisionID, actorID string) (*workflowGraph, error) {
	var revision repository.WorkflowRevision
	if err := tx.Where("id = ?", revisionID).Take(&revision).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, workflowNotFound("workflow_revision_not_found", map[string]any{"id": revisionID})
		}
		return nil, err
	}
	var definition repository.WorkflowDefinition
	if err := tx.Where("id = ?", revision.DefinitionID).Take(&definition).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, workflowNotFound("workflow_revision_not_found", map[string]any{"id": revisionID})
		}
		return nil, err
	}
	if actorID != "" && definition.UserID != actorID {
		return nil, workflowNotFound("workflow_revision_not_found", map[string]any{"id": revisionID})
	}
	var nodeRows []repository.WorkflowNode
	if err := tx.Where("revision_id = ?", revisionID).Order("node_key").Find(&nodeRows).Error; err != nil {
		return nil, err
	}
	var edgeRows []repository.WorkflowEdge
	if err := tx.Where("revision_id = ?", revisionID).Order("edge_key").Find(&edgeRows).Error; err != nil {
		return nil, err
	}
	nodes := make([]map[string]any, 0, len(nodeRows))
	for _, row := range nodeRows {
		nodes = append(nodes, wfNodeView(row))
	}
	edges := make([]map[string]any, 0, len(edgeRows))
	for _, row := range edgeRows {
		edges = append(edges, wfEdgeView(row))
	}
	return &workflowGraph{Revision: &revision, Nodes: nodes, Edges: edges}, nil
}

// loadDefinition 读取定义并校验归属；不存在或不属于该用户一律按 404 处理。
func loadDefinition(ctx context.Context, tx *gorm.DB, definitionID, actorID string) (*repository.WorkflowDefinition, error) {
	var definition repository.WorkflowDefinition
	if err := tx.Where("id = ?", definitionID).Take(&definition).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, workflowNotFound("workflow_not_found", map[string]any{"id": definitionID})
		}
		return nil, err
	}
	if definition.UserID != actorID {
		return nil, workflowNotFound("workflow_not_found", map[string]any{"id": definitionID})
	}
	return &definition, nil
}

// appendEvent 写入 project_event_log 并回填自增 seq。
func appendEvent(
	ctx context.Context, tx *gorm.DB, params workflowEventParams,
) (*repository.ProjectEventLog, error) {
	event := repository.ProjectEventLog{
		EventID:          uuid.NewString(),
		WorkspaceID:      params.WorkspaceID,
		ProjectID:        params.ProjectID,
		AggregateType:    params.AggregateType,
		AggregateID:      params.AggregateID,
		AggregateVersion: params.AggregateVersion,
		EventType:        params.EventType,
		EventVersion:     1,
		PayloadJSON:      wfJSON(params.Payload, "{}"),
		ActorType:        "user",
		ActorID:          params.ActorID,
		TraceID:          params.TraceID,
		CreatedAt:        db.NewFlexibleTime(wfNow()),
	}
	if err := tx.Create(&event).Error; err != nil {
		return nil, err
	}
	return &event, nil
}

type workflowEventParams struct {
	WorkspaceID      string
	ProjectID        string
	AggregateType    string
	AggregateID      string
	AggregateVersion int64
	EventType        string
	Payload          map[string]any
	ActorID          string
	TraceID          *string
}

// --- 定义 ---

// WorkflowDefinitionParams 是创建定义的入参。
type WorkflowDefinitionParams struct {
	WorkspaceID string
	ProjectID   string
	Name        string
	ActorID     string
}

// CreateDefinition 创建一条工作流定义并写事件。
func (s *WorkflowService) CreateDefinition(ctx context.Context, p WorkflowDefinitionParams) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result, err := s.createDefinition(ctx, tx, p)
		out = result
		return err
	})
	return out, err
}

func (s *WorkflowService) createDefinition(
	ctx context.Context, tx *gorm.DB, p WorkflowDefinitionParams,
) (map[string]any, error) {
	now := db.NewFlexibleTime(wfNow())
	definition := repository.WorkflowDefinition{
		ID:          uuid.NewString(),
		UserID:      p.ActorID,
		WorkspaceID: p.WorkspaceID,
		ProjectID:   p.ProjectID,
		Name:        p.Name,
		Scope:       "project",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := tx.Create(&definition).Error; err != nil {
		return nil, err
	}
	event, err := appendEvent(ctx, tx, workflowEventParams{
		WorkspaceID:      p.WorkspaceID,
		ProjectID:        p.ProjectID,
		AggregateType:    "workflow_definition",
		AggregateID:      definition.ID,
		AggregateVersion: 1,
		EventType:        "workflow.definition.created",
		Payload:          map[string]any{"name": p.Name},
		ActorID:          p.ActorID,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": definition.ID, "version": 1, "event_cursor": event.Seq}, nil
}

// --- 版本 ---

// WorkflowRevisionParams 是创建版本的入参。TemplateLock 为 nil 时列写 NULL。
type WorkflowRevisionParams struct {
	DefinitionID   string
	Nodes          []map[string]any
	Edges          []map[string]any
	TemplateLock   any
	ActorID        string
	ContentMode    string
	GenerationMode string
	InputSchema    any
}

// CreateRevision 校验图与模式后落一条 draft 版本及其节点/边。
func (s *WorkflowService) CreateRevision(ctx context.Context, p WorkflowRevisionParams) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result, err := s.createRevision(ctx, tx, p)
		out = result
		return err
	})
	return out, err
}

func (s *WorkflowService) createRevision(
	ctx context.Context, tx *gorm.DB, p WorkflowRevisionParams,
) (map[string]any, error) {
	if err := domain.WorkflowValidateGraph(p.Nodes, p.Edges); err != nil {
		return nil, err
	}
	if err := domain.WorkflowValidateModes(p.ContentMode, p.GenerationMode); err != nil {
		return nil, err
	}
	if err := domain.WorkflowValidateNodeContracts(p.Nodes); err != nil {
		return nil, err
	}
	definition, err := loadDefinition(ctx, tx, p.DefinitionID, p.ActorID)
	if err != nil {
		return nil, err
	}

	var maxRevision int64
	if err := tx.Model(&repository.WorkflowRevision{}).
		Where("definition_id = ?", p.DefinitionID).
		Select("COALESCE(MAX(revision_no), 0)").Scan(&maxRevision).Error; err != nil {
		return nil, err
	}
	revisionNo := maxRevision + 1

	graphHash, err := domain.WorkflowGraphHash(p.Nodes, p.Edges, true)
	if err != nil {
		return nil, err
	}
	executionHash, err := domain.WorkflowGraphHash(p.Nodes, p.Edges, false)
	if err != nil {
		return nil, err
	}
	revision := repository.WorkflowRevision{
		ID:              uuid.NewString(),
		DefinitionID:    definition.ID,
		RevisionNo:      revisionNo,
		Status:          "draft",
		ContentMode:     p.ContentMode,
		GenerationMode:  p.GenerationMode,
		InputSchemaJSON: wfJSON(p.InputSchema, "{}"),
		GraphHash:       graphHash,
		ExecutionHash:   executionHash,
		CreatedBy:       p.ActorID,
		CreatedAt:       db.NewFlexibleTime(wfNow()),
	}
	if p.TemplateLock != nil {
		encoded := wfJSON(p.TemplateLock, "{}")
		revision.TemplateLockJSON = &encoded
	}
	if revision.InputSchemaJSON == "" {
		revision.InputSchemaJSON = "{}"
	}
	if err := tx.Create(&revision).Error; err != nil {
		return nil, err
	}

	for _, node := range p.Nodes {
		nodeType := domain.WorkflowMapString(node, "node_type")
		if nodeType == "" {
			nodeType = domain.WorkflowMapString(node, "node_key")
		}
		row := repository.WorkflowNode{
			ID:                       uuid.NewString(),
			RevisionID:               revision.ID,
			NodeKey:                  domain.WorkflowMapString(node, "node_key"),
			NodeType:                 nodeType,
			NodeTypeVersion:          wfStringDefault(node, "node_type_version", "1"),
			ConfigSchemaVersion:      wfStringDefault(node, "config_schema_version", "1"),
			ConfigJSON:               wfJSON(wfValue(node, "config", map[string]any{}), "{}"),
			Weight:                   domain.WorkflowMapFloat(node, "weight", 1.0),
			InputSchemaJSON:          wfJSON(wfValue(node, "input_schema", map[string]any{}), "{}"),
			OutputSchemaJSON:         wfJSON(wfValue(node, "output_schema", map[string]any{}), "{}"),
			ExecutorID:               wfStringDefault(node, "executor_id", "builtin"),
			RequiredCapabilitiesJSON: wfJSON(wfValue(node, "required_capabilities", []any{}), "[]"),
			EstimatedCost:            domain.WorkflowMapFloat(node, "estimated_cost", 0),
			CachePolicy:              wfStringDefault(node, "cache_policy", "reuse"),
		}
		if value, ok := node["ui_position"]; ok && value != nil {
			encoded := wfJSON(value, "{}")
			row.UiPositionJSON = &encoded
		}
		if value, ok := node["retry_policy"]; ok && value != nil {
			encoded := wfJSON(value, "{}")
			row.RetryPolicyJSON = &encoded
		}
		if value, ok := node["approval_policy"]; ok && value != nil {
			encoded := wfJSON(value, "{}")
			row.ApprovalPolicyJSON = &encoded
		}
		if err := tx.Create(&row).Error; err != nil {
			return nil, err
		}
	}
	for _, edge := range p.Edges {
		row := repository.WorkflowEdge{
			ID:            uuid.NewString(),
			RevisionID:    revision.ID,
			EdgeKey:       domain.WorkflowMapString(edge, "edge_key"),
			SourceNodeKey: domain.WorkflowMapString(edge, "source_node_key"),
			TargetNodeKey: domain.WorkflowMapString(edge, "target_node_key"),
			OnFailure:     wfStringDefault(edge, "on_failure", "stop"),
			Priority:      int64(domain.WorkflowMapInt(edge, "priority", 0)),
		}
		if value, ok := edge["condition"]; ok && value != nil {
			encoded := wfJSON(value, "{}")
			row.ConditionJSON = &encoded
		}
		if err := tx.Create(&row).Error; err != nil {
			return nil, err
		}
	}

	event, err := appendEvent(ctx, tx, workflowEventParams{
		WorkspaceID:      definition.WorkspaceID,
		ProjectID:        definition.ProjectID,
		AggregateType:    "workflow_revision",
		AggregateID:      revision.ID,
		AggregateVersion: 1,
		EventType:        "workflow.revision.created",
		Payload: map[string]any{
			"definition_id": definition.ID,
			"revision_no":   revisionNo,
			"content_mode":  p.ContentMode,
		},
		ActorID: p.ActorID,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":              revision.ID,
		"revision_no":     revisionNo,
		"status":          revision.Status,
		"graph_hash":      revision.GraphHash,
		"execution_hash":  revision.ExecutionHash,
		"content_mode":    revision.ContentMode,
		"generation_mode": revision.GenerationMode,
		"version":         1,
		"event_cursor":    event.Seq,
	}, nil
}

func wfValue(m map[string]any, key string, fallback any) any {
	if value, ok := m[key]; ok && value != nil {
		return value
	}
	return fallback
}

func wfStringDefault(m map[string]any, key, fallback string) string {
	if value := domain.WorkflowMapString(m, key); value != "" {
		return value
	}
	return fallback
}

// ValidateRevision 复算图校验，返回版本指纹与模式。
func (s *WorkflowService) ValidateRevision(ctx context.Context, revisionID, actorID string) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		graph, err := loadGraph(ctx, tx, revisionID, actorID)
		if err != nil {
			return err
		}
		if err := domain.WorkflowValidateGraph(graph.Nodes, graph.Edges); err != nil {
			return err
		}
		out = map[string]any{
			"valid":           true,
			"revision_id":     graph.Revision.ID,
			"graph_hash":      graph.Revision.GraphHash,
			"execution_hash":  graph.Revision.ExecutionHash,
			"content_mode":    graph.Revision.ContentMode,
			"generation_mode": graph.Revision.GenerationMode,
			"input_schema":    wfDecode(graph.Revision.InputSchemaJSON, map[string]any{}),
		}
		return nil
	})
	return out, err
}

// PublishRevision 把 draft 版本发布为定义当前激活版本。
func (s *WorkflowService) PublishRevision(ctx context.Context, revisionID, actorID string) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result, err := s.publishRevision(ctx, tx, revisionID, actorID)
		out = result
		return err
	})
	return out, err
}

func (s *WorkflowService) publishRevision(
	ctx context.Context, tx *gorm.DB, revisionID, actorID string,
) (map[string]any, error) {
	graph, err := loadGraph(ctx, tx, revisionID, actorID)
	if err != nil {
		return nil, err
	}
	if graph.Revision.Status != "draft" {
		return nil, workflowConflict("workflow_revision_immutable", map[string]any{
			"id": revisionID, "status": graph.Revision.Status,
		})
	}
	if err := domain.WorkflowValidateGraph(graph.Nodes, graph.Edges); err != nil {
		return nil, err
	}
	definition, err := loadDefinition(ctx, tx, graph.Revision.DefinitionID, actorID)
	if err != nil {
		return nil, err
	}
	revisionIDCopy := revisionID
	if err := tx.Model(&repository.WorkflowRevision{}).
		Where("id = ?", revisionID).Update("status", "published").Error; err != nil {
		return nil, err
	}
	if err := tx.Model(&repository.WorkflowDefinition{}).
		Where("id = ?", definition.ID).
		Updates(map[string]any{
			"active_revision_id": revisionIDCopy,
			"updated_at":         db.NewFlexibleTime(wfNow()),
		}).Error; err != nil {
		return nil, err
	}
	event, err := appendEvent(ctx, tx, workflowEventParams{
		WorkspaceID:      definition.WorkspaceID,
		ProjectID:        definition.ProjectID,
		AggregateType:    "workflow_revision",
		AggregateID:      revisionID,
		AggregateVersion: 2,
		EventType:        "workflow.revision.published",
		Payload:          map[string]any{"definition_id": definition.ID, "revision_no": graph.Revision.RevisionNo},
		ActorID:          actorID,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id": revisionID, "status": "published", "version": 2, "event_cursor": event.Seq,
	}, nil
}

// ListRevisions 列出定义的版本历史（新在前），标注是否激活。
func (s *WorkflowService) ListRevisions(ctx context.Context, definitionID, actorID string) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		definition, err := loadDefinition(ctx, tx, definitionID, actorID)
		if err != nil {
			return err
		}
		var rows []repository.WorkflowRevision
		if err := tx.Where("definition_id = ?", definitionID).
			Order("revision_no DESC").Find(&rows).Error; err != nil {
			return err
		}
		items := make([]any, 0, len(rows))
		for _, row := range rows {
			isActive := definition.ActiveRevisionID != nil && *definition.ActiveRevisionID == row.ID
			items = append(items, map[string]any{
				"id":              row.ID,
				"revision_no":     row.RevisionNo,
				"status":          row.Status,
				"graph_hash":      row.GraphHash,
				"execution_hash":  row.ExecutionHash,
				"content_mode":    row.ContentMode,
				"generation_mode": row.GenerationMode,
				"is_active":       isActive,
				"created_by":      row.CreatedBy,
				"created_at":      wfISO(row.CreatedAt),
			})
		}
		out = map[string]any{"items": items}
		return nil
	})
	return out, err
}

// RevertRevision 用历史图新建并发布一个版本。
func (s *WorkflowService) RevertRevision(
	ctx context.Context, definitionID, revisionID, actorID string,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := loadDefinition(ctx, tx, definitionID, actorID); err != nil {
			return err
		}
		var source repository.WorkflowRevision
		if err := tx.Where("id = ?", revisionID).Take(&source).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return workflowNotFound("workflow_revision_not_found", map[string]any{"id": revisionID})
			}
			return err
		}
		if source.DefinitionID != definitionID {
			return workflowNotFound("workflow_revision_not_found", map[string]any{"id": revisionID})
		}
		graph, err := loadGraph(ctx, tx, revisionID, actorID)
		if err != nil {
			return err
		}
		var templateLock any
		if source.TemplateLockJSON != nil {
			templateLock = wfDecode(*source.TemplateLockJSON, nil)
		}
		created, err := s.createRevision(ctx, tx, WorkflowRevisionParams{
			DefinitionID:   definitionID,
			Nodes:          graph.Nodes,
			Edges:          graph.Edges,
			TemplateLock:   templateLock,
			ActorID:        actorID,
			ContentMode:    source.ContentMode,
			GenerationMode: source.GenerationMode,
			InputSchema:    wfDecode(source.InputSchemaJSON, map[string]any{}),
		})
		if err != nil {
			return err
		}
		published, err := s.publishRevision(ctx, tx, created["id"].(string), actorID)
		if err != nil {
			return err
		}
		out = map[string]any{"reverted_from": revisionID, "revision_id": created["id"]}
		for key, value := range published {
			out[key] = value
		}
		return nil
	})
	return out, err
}

// GetWorkflow 返回定义及其激活版本 / 未发布草稿。
func (s *WorkflowService) GetWorkflow(ctx context.Context, definitionID, actorID string) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		definition, err := loadDefinition(ctx, tx, definitionID, actorID)
		if err != nil {
			return err
		}
		result := map[string]any{
			"id":                 definition.ID,
			"workspace_id":       definition.WorkspaceID,
			"project_id":         definition.ProjectID,
			"name":               definition.Name,
			"scope":              definition.Scope,
			"active_revision_id": nilAsAny(definition.ActiveRevisionID),
		}
		var activeRevisionNo *int64
		if definition.ActiveRevisionID != nil && *definition.ActiveRevisionID != "" {
			graph, err := loadGraph(ctx, tx, *definition.ActiveRevisionID, actorID)
			if err != nil {
				return err
			}
			no := graph.Revision.RevisionNo
			activeRevisionNo = &no
			result["active_revision"] = revisionView(graph)
		}
		var draft repository.WorkflowRevision
		err = tx.Where("definition_id = ? AND status = ?", definitionID, "draft").
			Order("revision_no DESC").Limit(1).Take(&draft).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && (activeRevisionNo == nil || draft.RevisionNo > *activeRevisionNo) {
			graph, err := loadGraph(ctx, tx, draft.ID, actorID)
			if err != nil {
				return err
			}
			result["draft_revision"] = revisionView(graph)
		}
		out = result
		return nil
	})
	return out, err
}

func nilAsAny(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func revisionView(graph *workflowGraph) map[string]any {
	return map[string]any{
		"id":              graph.Revision.ID,
		"revision_no":     graph.Revision.RevisionNo,
		"status":          graph.Revision.Status,
		"graph_hash":      graph.Revision.GraphHash,
		"execution_hash":  graph.Revision.ExecutionHash,
		"content_mode":    graph.Revision.ContentMode,
		"generation_mode": graph.Revision.GenerationMode,
		"input_schema":    wfDecode(graph.Revision.InputSchemaJSON, map[string]any{}),
		"template_lock":   wfDecodePtr(graph.Revision.TemplateLockJSON, nil),
		"nodes":           graph.Nodes,
		"edges":           graph.Edges,
	}
}

// --- 运行 ---

// WorkflowPlanRunParams 是规划一次运行的入参。
type WorkflowPlanRunParams struct {
	RevisionID       string
	WorkspaceID      string
	ProjectID        string
	Mode             string
	InputSnapshot    map[string]any
	ScriptRevisionID *string
	ActorID          string
	EpisodeID        *string
	BudgetLimit      *float64
	// OfficialRevision 表示 revision 由官方 Skill 铸造：它的工作流定义归属 system，
	// 普通用户按自己身份启动计划时必须跳过归属校验（否则恒 404）。
	// 计划行本身仍按 user_id 收敛，越权访问不会发生。
	OfficialRevision bool
}

// PlanRun 按输入指纹去重后创建一条 planned 运行及其节点运行行。
func (s *WorkflowService) PlanRun(ctx context.Context, p WorkflowPlanRunParams) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result, err := s.planRun(ctx, tx, p)
		out = result
		return err
	})
	return out, err
}

func (s *WorkflowService) planRun(ctx context.Context, tx *gorm.DB, p WorkflowPlanRunParams) (map[string]any, error) {
	if p.Mode != "auto" && p.Mode != "manual" && p.Mode != "hybrid" {
		return nil, domain.NewWorkflowValidationError("workflow_input_invalid", map[string]any{"reason": "mode"})
	}
	if p.BudgetLimit != nil && *p.BudgetLimit < 0 {
		return nil, domain.NewWorkflowValidationError("workflow_input_invalid", map[string]any{"reason": "budget_limit"})
	}
	graphActorID := p.ActorID
	if p.OfficialRevision {
		graphActorID = ""
	}
	graph, err := loadGraph(ctx, tx, p.RevisionID, graphActorID)
	if err != nil {
		return nil, err
	}
	if graph.Revision.Status != "published" {
		return nil, workflowConflict("workflow_revision_not_published", map[string]any{"id": p.RevisionID})
	}
	// 官方 Skill 的 revision 挂在 system 名下的 marketplace 定义上：既不属于发起
	// 用户，也不在计划的工作区 / 项目里，两项归属校验都要跳过。
	if !p.OfficialRevision {
		definition, err := loadDefinition(ctx, tx, graph.Revision.DefinitionID, p.ActorID)
		if err != nil {
			return nil, err
		}
		if definition.WorkspaceID != p.WorkspaceID || definition.ProjectID != p.ProjectID {
			return nil, workflowNotFound("workflow_revision_not_found", map[string]any{"id": p.RevisionID})
		}
	}
	if mode := snapshotGenerationMode(p.InputSnapshot); mode != "" && mode != graph.Revision.GenerationMode {
		return nil, domain.NewWorkflowValidationError("workflow_generation_mode_mismatch", map[string]any{
			"project_generation_mode":  mode,
			"workflow_generation_mode": graph.Revision.GenerationMode,
		})
	}
	fingerprint, err := domain.WorkflowInputFingerprint(map[string]any{
		"schema_version":          1,
		"workspace_id":            p.WorkspaceID,
		"project_id":              p.ProjectID,
		"workflow_execution_hash": graph.Revision.ExecutionHash,
		"script_revision_id":      nilAsAny(p.ScriptRevisionID),
		"input":                   p.InputSnapshot,
	})
	if err != nil {
		return nil, err
	}
	var existing repository.WorkflowRun
	err = tx.Where(
		"user_id = ? AND workspace_id = ? AND project_id = ? AND input_fingerprint = ? AND status IN ?",
		p.ActorID, p.WorkspaceID, p.ProjectID, fingerprint, workflowActiveRunStatuses,
	).Order("created_at DESC").Limit(1).Take(&existing).Error
	if err == nil {
		return map[string]any{
			"id": existing.ID, "status": existing.Status, "version": existing.Version, "deduped": true,
		}, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	now := db.NewFlexibleTime(wfNow())
	traceID := uuid.NewString()
	progress := 0.0
	run := repository.WorkflowRun{
		ID:                 uuid.NewString(),
		UserID:             p.ActorID,
		WorkflowRevisionID: graph.Revision.ID,
		WorkspaceID:        p.WorkspaceID,
		ProjectID:          p.ProjectID,
		ScriptRevisionID:   p.ScriptRevisionID,
		EpisodeID:          p.EpisodeID,
		BudgetLimit:        p.BudgetLimit,
		Status:             "planned",
		Mode:               p.Mode,
		ExecutionHash:      graph.Revision.ExecutionHash,
		InputFingerprint:   fingerprint,
		InputSnapshotJSON:  wfJSON(p.InputSnapshot, "{}"),
		Progress:           &progress,
		Version:            1,
		ControlGeneration:  0,
		TraceID:            traceID,
		CreatedBy:          p.ActorID,
		CreatedAt:          now,
	}
	graphSnapshot := graph.Revision.ID
	run.GraphSnapshotRef = &graphSnapshot
	if err := tx.Create(&run).Error; err != nil {
		return nil, err
	}
	targets := map[string]bool{}
	for _, edge := range graph.Edges {
		targets[domain.WorkflowMapString(edge, "target_node_key")] = true
	}
	for _, node := range graph.Nodes {
		key := domain.WorkflowMapString(node, "node_key")
		status := "ready"
		if targets[key] {
			status = "blocked"
		}
		nodeProgress := 0.0
		source := "unknown"
		row := repository.WorkflowNodeRun{
			ID:             uuid.NewString(),
			WorkflowRunID:  run.ID,
			NodeKey:        key,
			AttemptNo:      1,
			Status:         status,
			Progress:       &nodeProgress,
			ProgressSource: &source,
			FencingToken:   0,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := tx.Create(&row).Error; err != nil {
			return nil, err
		}
	}
	event, err := appendEvent(ctx, tx, workflowEventParams{
		WorkspaceID:      p.WorkspaceID,
		ProjectID:        p.ProjectID,
		AggregateType:    "workflow_run",
		AggregateID:      run.ID,
		AggregateVersion: 1,
		EventType:        "workflow.run.planned",
		Payload: map[string]any{
			"revision_id":       graph.Revision.ID,
			"mode":              p.Mode,
			"episode_id":        nilAsAny(p.EpisodeID),
			"budget_limit":      nilAsAnyFloat(p.BudgetLimit),
			"input_fingerprint": fingerprint,
		},
		ActorID: p.ActorID,
		TraceID: &traceID,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":           run.ID,
		"status":       run.Status,
		"version":      1,
		"episode_id":   nilAsAny(p.EpisodeID),
		"budget_limit": nilAsAnyFloat(p.BudgetLimit),
		"event_cursor": event.Seq,
		"deduped":      false,
	}, nil
}

func nilAsAnyFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

// snapshotGenerationMode 递归寻找快照里显式声明的项目生成模式。
func snapshotGenerationMode(snapshot map[string]any) string {
	return findGenerationMode(snapshot)
}

func findGenerationMode(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		if explicit, ok := typed["project_generation_mode"].(string); ok && explicit != "" {
			return explicit
		}
		for _, key := range []string{"project_context", "project_snapshot"} {
			if nested, ok := typed[key].(map[string]any); ok {
				if mode, ok := nested["generation_mode"].(string); ok && mode != "" {
					return mode
				}
			}
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if mode := findGenerationMode(typed[key]); mode != "" {
				return mode
			}
		}
	case []any:
		for _, child := range typed {
			if mode := findGenerationMode(child); mode != "" {
				return mode
			}
		}
	}
	return ""
}

// FindWorkflowRunForCreationPlan 找出此前为该不可变创作计划建过的运行。
func (s *WorkflowService) FindWorkflowRunForCreationPlan(
	ctx context.Context, creationPlanID, projectID, actorID string,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result, err := s.findRunForPlan(ctx, tx, creationPlanID, projectID, actorID)
		out = result
		return err
	})
	return out, err
}

// findRunForPlan 是 FindWorkflowRunForCreationPlan 的事务内版本。
func (s *WorkflowService) findRunForPlan(
	ctx context.Context, tx *gorm.DB, creationPlanID, projectID, actorID string,
) (map[string]any, error) {
	var rows []repository.WorkflowRun
	err := tx.
		Where("user_id = ? AND project_id = ?", actorID, projectID).
		Order("created_at DESC").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	for i := range rows {
		snapshot, ok := wfDecode(rows[i].InputSnapshotJSON, map[string]any{}).(map[string]any)
		if !ok {
			continue
		}
		if value, _ := snapshot["creation_plan_id"].(string); value == creationPlanID {
			return map[string]any{
				"id": rows[i].ID, "status": rows[i].Status, "version": rows[i].Version, "deduped": true,
			}, nil
		}
	}
	return nil, nil
}

// RequestWorkflowRunExecution 给已在跑的运行打一个幂等的派发审计标记。
//
// 运行行本身就是持久的工作项，事件只是审计标记：进程重启后轮询执行器仍能从
// running 行恢复，不需要第二次派发请求。
func (s *WorkflowService) RequestWorkflowRunExecution(
	ctx context.Context, runID, actorID string,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result, err := s.requestExecution(ctx, tx, runID, actorID)
		out = result
		return err
	})
	return out, err
}

// requestExecution 是 RequestWorkflowRunExecution 的事务内版本。
//
// 创作计划启动要在同一事务里「建运行 → 跃迁 → 派发」，否则派发读不到未提交的运行行，
// 会误报 workflow_run_not_found。
func (s *WorkflowService) requestExecution(
	ctx context.Context, tx *gorm.DB, runID, actorID string,
) (map[string]any, error) {
	var out map[string]any
	execErr := func() error {
		var run repository.WorkflowRun
		if err := tx.Where("id = ?", runID).Take(&run).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return workflowNotFound("workflow_run_not_found", map[string]any{"id": runID})
			}
			return err
		}
		if run.UserID != actorID {
			return workflowNotFound("workflow_run_not_found", map[string]any{"id": runID})
		}
		if run.Status != "running" {
			out = map[string]any{
				"id":              run.ID,
				"status":          run.Status,
				"dispatch_status": "not_running",
				"executor":        "workflow_executor_loop",
			}
			return nil
		}
		var existing repository.ProjectEventLog
		err := tx.Where(
			"aggregate_type = ? AND aggregate_id = ? AND event_type = ?",
			"workflow_run", run.ID, "workflow.run.dispatch_requested",
		).Limit(1).Take(&existing).Error
		already := err == nil
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if !already {
			if _, err := appendEvent(ctx, tx, workflowEventParams{
				WorkspaceID:      run.WorkspaceID,
				ProjectID:        run.ProjectID,
				AggregateType:    "workflow_run",
				AggregateID:      run.ID,
				AggregateVersion: run.Version,
				EventType:        "workflow.run.dispatch_requested",
				Payload:          map[string]any{"status": run.Status, "executor": "workflow_executor_loop"},
				ActorID:          actorID,
				TraceID:          &run.TraceID,
			}); err != nil {
				return err
			}
		}
		dispatch := "queued"
		if already {
			dispatch = "already_queued"
		}
		out = map[string]any{
			"id":              run.ID,
			"status":          run.Status,
			"dispatch_status": dispatch,
			"executor":        "workflow_executor_loop",
		}
		return nil
	}()
	if execErr != nil {
		return nil, execErr
	}
	return out, nil
}

// TransitionWorkflowRun 按期望版本做乐观并发的状态跃迁。
func (s *WorkflowService) TransitionWorkflowRun(
	ctx context.Context, runID, target string, expectedVersion int64, actorID string,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result, err := s.transitionRun(ctx, tx, runID, target, expectedVersion, actorID)
		out = result
		return err
	})
	return out, err
}

func (s *WorkflowService) transitionRun(
	ctx context.Context, tx *gorm.DB, runID, target string, expectedVersion int64, actorID string,
) (map[string]any, error) {
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
	if run.Version != expectedVersion {
		return nil, workflowConflict("workflow_version_conflict", map[string]any{
			"expected": expectedVersion, "actual": run.Version,
		})
	}
	previousStatus := run.Status
	if err := domain.WorkflowTransitionRun(previousStatus, target); err != nil {
		return nil, err
	}
	now := db.NewFlexibleTime(wfNow())
	controlGeneration := run.ControlGeneration
	if target == "paused" || target == "cancelled" ||
		(target == "running" && (previousStatus == "paused" || previousStatus == "waiting_review")) {
		controlGeneration++
	}
	updates := map[string]any{
		"status":             target,
		"version":            run.Version + 1,
		"control_generation": controlGeneration,
	}
	if target == "running" && !run.StartedAt.Valid {
		updates["started_at"] = now
	}
	if domain.WorkflowTerminalRunStatuses[target] {
		updates["finished_at"] = now
	}
	if err := tx.Model(&repository.WorkflowRun{}).Where("id = ?", runID).Updates(updates).Error; err != nil {
		return nil, err
	}
	event, err := appendEvent(ctx, tx, workflowEventParams{
		WorkspaceID:      run.WorkspaceID,
		ProjectID:        run.ProjectID,
		AggregateType:    "workflow_run",
		AggregateID:      run.ID,
		AggregateVersion: run.Version + 1,
		EventType:        "workflow.run." + target,
		Payload:          map[string]any{"status": target, "control_generation": controlGeneration},
		ActorID:          actorID,
		TraceID:          &run.TraceID,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":                 run.ID,
		"status":             target,
		"version":            run.Version + 1,
		"control_generation": controlGeneration,
		"event_cursor":       event.Seq,
	}, nil
}

// GetRun 返回运行详情及其节点运行。
func (s *WorkflowService) GetRun(ctx context.Context, runID, actorID string) (map[string]any, error) {
	var run repository.WorkflowRun
	if err := s.db.WithContext(ctx).Where("id = ?", runID).Take(&run).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, workflowNotFound("workflow_run_not_found", map[string]any{"id": runID})
		}
		return nil, err
	}
	if run.UserID != actorID {
		return nil, workflowNotFound("workflow_run_not_found", map[string]any{"id": runID})
	}
	var nodeRuns []repository.WorkflowNodeRun
	if err := s.db.WithContext(ctx).Where("workflow_run_id = ?", runID).
		Order("created_at, node_key").Find(&nodeRuns).Error; err != nil {
		return nil, err
	}
	nodes := make([]any, 0, len(nodeRuns))
	for _, row := range nodeRuns {
		nodes = append(nodes, map[string]any{
			"id":              row.ID,
			"node_key":        row.NodeKey,
			"attempt_no":      row.AttemptNo,
			"status":          row.Status,
			"progress":        nilAsAnyFloat(row.Progress),
			"progress_source": nilAsAnyString(row.ProgressSource),
			"phase_code":      nilAsAnyString(row.PhaseCode),
			"error_code":      nilAsAnyString(row.ErrorCode),
			"error_params":    wfDecodePtr(row.ErrorParamsJSON, map[string]any{}),
			"output_refs":     wfDecodePtr(row.OutputRefsJSON, map[string]any{}),
			"fencing_token":   row.FencingToken,
		})
	}
	return map[string]any{
		"id":                   run.ID,
		"workflow_revision_id": run.WorkflowRevisionID,
		"workspace_id":         run.WorkspaceID,
		"project_id":           run.ProjectID,
		"status":               run.Status,
		"mode":                 run.Mode,
		"progress":             nilAsAnyFloat(run.Progress),
		"version":              run.Version,
		"control_generation":   run.ControlGeneration,
		"input_fingerprint":    run.InputFingerprint,
		"episode_id":           nilAsAnyString(run.EpisodeID),
		"budget_limit":         nilAsAnyFloat(run.BudgetLimit),
		"spent_amount":         run.SpentAmount,
		"reserved_amount":      run.ReservedAmount,
		"remaining_amount":     remainingBudget(run.BudgetLimit, run.SpentAmount, run.ReservedAmount, false),
		"nodes":                nodes,
	}, nil
}

func nilAsAnyString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// remainingBudget 计算剩余预算；无上限时为 nil。clamp 为真时下限截到 0。
func remainingBudget(limit *float64, spent, reserved float64, clamp bool) any {
	if limit == nil {
		return nil
	}
	value := *limit - spent - reserved
	if clamp && value < 0 {
		value = 0
	}
	return value
}

// ListRuns 列出项目的运行（新在前）。
func (s *WorkflowService) ListRuns(
	ctx context.Context, projectID, actorID string, limit int,
) (map[string]any, error) {
	var rows []repository.WorkflowRun
	if err := s.db.WithContext(ctx).Where("user_id = ? AND project_id = ?", actorID, projectID).
		Order("created_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{
			"id":                   row.ID,
			"workflow_revision_id": row.WorkflowRevisionID,
			"status":               row.Status,
			"mode":                 row.Mode,
			"progress":             nilAsAnyFloat(row.Progress),
			"version":              row.Version,
			"control_generation":   row.ControlGeneration,
			"created_at":           wfISO(row.CreatedAt),
			"episode_id":           nilAsAnyString(row.EpisodeID),
			"budget_limit":         nilAsAnyFloat(row.BudgetLimit),
			"started_at":           wfISO(row.StartedAt),
			"finished_at":          wfISO(row.FinishedAt),
		})
	}
	return map[string]any{"items": items}, nil
}

// ListProjectEvents 回放项目事件日志。
func (s *WorkflowService) ListProjectEvents(
	ctx context.Context, projectID, actorID string, after int64, limit int,
) (map[string]any, error) {
	var rows []repository.ProjectEventLog
	if err := s.db.WithContext(ctx).
		Where("actor_id = ? AND project_id = ? AND seq > ?", actorID, projectID, after).
		Order("seq").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{
			"seq":               row.Seq,
			"event_id":          row.EventID,
			"aggregate_type":    row.AggregateType,
			"aggregate_id":      row.AggregateID,
			"aggregate_version": row.AggregateVersion,
			"event_type":        row.EventType,
			"event_version":     row.EventVersion,
			"payload":           wfDecode(row.PayloadJSON, map[string]any{}),
			"trace_id":          nilAsAnyString(row.TraceID),
			"created_at":        wfISO(row.CreatedAt),
		})
	}
	cursor := after
	if len(rows) > 0 {
		cursor = rows[len(rows)-1].Seq
	}
	return map[string]any{"items": items, "cursor": cursor}, nil
}

// ListNodeLogs 返回某个节点的日志行（按 seq 升序）。
func (s *WorkflowService) ListNodeLogs(
	ctx context.Context, runID, nodeKey, actorID string, limit int,
) (map[string]any, error) {
	var run repository.WorkflowRun
	if err := s.db.WithContext(ctx).Where("id = ?", runID).Take(&run).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, workflowNotFound("workflow_run_not_found", map[string]any{"id": runID})
		}
		return nil, err
	}
	if run.UserID != actorID {
		return nil, workflowNotFound("workflow_run_not_found", map[string]any{"id": runID})
	}
	var rows []repository.ProjectEventLog
	if err := s.db.WithContext(ctx).
		Where("actor_id = ? AND project_id = ? AND aggregate_id = ? AND event_type = ?",
			actorID, run.ProjectID, run.ID, "workflow.node_log").
		Order("seq DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]any, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		payload, ok := wfDecode(rows[i].PayloadJSON, map[string]any{}).(map[string]any)
		if !ok {
			continue
		}
		if domain.WorkflowMapString(payload, "node_key") != nodeKey {
			continue
		}
		level := domain.WorkflowMapString(payload, "level")
		if level == "" {
			level = "info"
		}
		items = append(items, map[string]any{
			"seq":        rows[i].Seq,
			"level":      level,
			"line":       domain.WorkflowMapString(payload, "line"),
			"created_at": wfISO(rows[i].CreatedAt),
		})
	}
	return map[string]any{"items": items}, nil
}

// RetryRunFromNode 在失败节点处分叉出一条新运行，保留上游已通过产物。
func (s *WorkflowService) RetryRunFromNode(
	ctx context.Context, runID, nodeKey, actorID string, start bool,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var sourceRun repository.WorkflowRun
		if err := tx.Where("id = ?", runID).Take(&sourceRun).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return workflowNotFound("workflow_run_not_found", map[string]any{"id": runID})
			}
			return err
		}
		if sourceRun.UserID != actorID {
			return workflowNotFound("workflow_run_not_found", map[string]any{"id": runID})
		}
		if sourceRun.Status != "failed" && sourceRun.Status != "paused" {
			return workflowConflict("workflow_retry_invalid_status", map[string]any{"status": sourceRun.Status})
		}
		graph, err := loadGraph(ctx, tx, sourceRun.WorkflowRevisionID, actorID)
		if err != nil {
			return err
		}
		present := map[string]bool{}
		for _, node := range graph.Nodes {
			present[domain.WorkflowMapString(node, "node_key")] = true
		}
		if !present[nodeKey] {
			return workflowNotFound("workflow_node_not_found", map[string]any{"node_key": nodeKey})
		}
		var sourceRows []repository.WorkflowNodeRun
		if err := tx.Where("workflow_run_id = ?", sourceRun.ID).
			Order("node_key, attempt_no DESC").Find(&sourceRows).Error; err != nil {
			return err
		}
		latest := map[string]repository.WorkflowNodeRun{}
		for _, row := range sourceRows {
			if _, exists := latest[row.NodeKey]; !exists {
				latest[row.NodeKey] = row
			}
		}
		target, exists := latest[nodeKey]
		if !exists || (target.Status != "failed" && target.Status != "stale" && target.Status != "orphaned") {
			status := any(nil)
			if exists {
				status = target.Status
			}
			return workflowConflict("workflow_retry_node_invalid_status", map[string]any{
				"node_key": nodeKey, "status": status,
			})
		}
		outgoing, incoming := domain.WorkflowNodeGraphEdges(graph.Edges)
		descendants := map[string]bool{}
		queue := make([]string, 0, len(outgoing[nodeKey]))
		for key := range outgoing[nodeKey] {
			queue = append(queue, key)
		}
		sort.Strings(queue)
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			if descendants[current] {
				continue
			}
			descendants[current] = true
			next := make([]string, 0, len(outgoing[current]))
			for key := range outgoing[current] {
				next = append(next, key)
			}
			sort.Strings(next)
			queue = append(queue, next...)
		}

		now := db.NewFlexibleTime(wfNow())
		traceID := uuid.NewString()
		progress := 0.0
		retryRun := repository.WorkflowRun{
			ID:                 uuid.NewString(),
			UserID:             actorID,
			WorkflowRevisionID: graph.Revision.ID,
			WorkspaceID:        sourceRun.WorkspaceID,
			ProjectID:          sourceRun.ProjectID,
			ScriptRevisionID:   sourceRun.ScriptRevisionID,
			Status:             "planned",
			Mode:               sourceRun.Mode,
			ExecutionHash:      sourceRun.ExecutionHash,
			InputFingerprint:   sourceRun.InputFingerprint,
			GraphSnapshotRef:   sourceRun.GraphSnapshotRef,
			InputSnapshotJSON:  sourceRun.InputSnapshotJSON,
			Progress:           &progress,
			Version:            1,
			ControlGeneration:  0,
			TraceID:            traceID,
			CreatedBy:          actorID,
			CreatedAt:          now,
		}
		if err := tx.Create(&retryRun).Error; err != nil {
			return err
		}
		for _, node := range graph.Nodes {
			key := domain.WorkflowMapString(node, "node_key")
			previous, hasPrevious := latest[key]
			var status string
			switch {
			case key == nodeKey:
				status = "ready"
			case descendants[key]:
				status = "blocked"
			case hasPrevious && workflowPassedNodeStatuses[previous.Status]:
				status = previous.Status
			case len(incoming[key]) == 0:
				status = "ready"
			default:
				status = "blocked"
			}
			nodeProgress := 0.0
			progressSource := "checkpoint"
			attempt := int64(1)
			var phaseCode *string
			var outputRefs *string
			if hasPrevious && workflowPassedNodeStatuses[previous.Status] {
				if previous.Progress != nil {
					nodeProgress = *previous.Progress
				}
				if previous.ProgressSource != nil {
					progressSource = *previous.ProgressSource
				}
				phaseCode = previous.PhaseCode
				outputRefs = previous.OutputRefsJSON
			}
			if hasPrevious {
				attempt = previous.AttemptNo + 1
			}
			row := repository.WorkflowNodeRun{
				ID:             uuid.NewString(),
				WorkflowRunID:  retryRun.ID,
				NodeKey:        key,
				AttemptNo:      attempt,
				Status:         status,
				Progress:       &nodeProgress,
				ProgressSource: &progressSource,
				PhaseCode:      phaseCode,
				OutputRefsJSON: outputRefs,
				FencingToken:   0,
				CreatedAt:      now,
				UpdatedAt:      now,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		event, err := appendEvent(ctx, tx, workflowEventParams{
			WorkspaceID:      retryRun.WorkspaceID,
			ProjectID:        retryRun.ProjectID,
			AggregateType:    "workflow_run",
			AggregateID:      retryRun.ID,
			AggregateVersion: 1,
			EventType:        "workflow.run.retry_planned",
			Payload:          map[string]any{"source_run_id": sourceRun.ID, "retry_from": nodeKey},
			ActorID:          actorID,
			TraceID:          &traceID,
		})
		if err != nil {
			return err
		}
		result := map[string]any{
			"id":            retryRun.ID,
			"status":        retryRun.Status,
			"version":       1,
			"event_cursor":  event.Seq,
			"source_run_id": sourceRun.ID,
			"retry_from":    nodeKey,
		}
		if start {
			transitioned, err := s.transitionRun(ctx, tx, retryRun.ID, "running", 1, actorID)
			if err != nil {
				return err
			}
			for key, value := range transitioned {
				result[key] = value
			}
		}
		out = result
		return nil
	})
	return out, err
}

// --- 画布管理：列表 / 导出 / 导入 / 迁移 ---

// LegacyFlowNodeTypes 是画布化之前的线性链路节点类型顺序。
var LegacyFlowNodeTypes = []string{
	"source_import", "script_generate", "script_review", "character_reference",
	"storyboard_generate", "storyboard_review", "shot_image_generate", "shot_video_generate",
	"voice_generate", "subtitle_generate", "compose", "quality_check", "export",
}

// LegacyLinearGraph 把旧线性流程表达成 DAG，供迁移使用。
func LegacyLinearGraph() ([]map[string]any, []map[string]any) {
	nodes := make([]map[string]any, 0, len(LegacyFlowNodeTypes))
	edges := make([]map[string]any, 0, len(LegacyFlowNodeTypes)-1)
	for index, nodeType := range LegacyFlowNodeTypes {
		weight := 1.0
		if strings.Contains(nodeType, "generate") {
			weight = 2.0
		}
		nodes = append(nodes, map[string]any{
			"node_key":              nodeType,
			"node_type":             nodeType,
			"node_type_version":     "1",
			"config_schema_version": "1",
			"config":                map[string]any{},
			"ui_position":           map[string]any{"x": index * 240, "y": 0},
			"weight":                weight,
		})
	}
	for index := 0; index+1 < len(LegacyFlowNodeTypes); index++ {
		source := LegacyFlowNodeTypes[index]
		target := LegacyFlowNodeTypes[index+1]
		edges = append(edges, map[string]any{
			"edge_key":        source + "-" + target,
			"source_node_key": source,
			"target_node_key": target,
			"on_failure":      "stop",
			"priority":        0,
		})
	}
	return nodes, edges
}

// ListDefinitions 列出项目的画布定义。
func (s *WorkflowService) ListDefinitions(ctx context.Context, projectID, actorID string) (map[string]any, error) {
	var rows []repository.WorkflowDefinition
	if err := s.db.WithContext(ctx).
		Where("user_id = ? AND project_id = ?", actorID, projectID).
		Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{
			"id":                 row.ID,
			"name":               row.Name,
			"scope":              row.Scope,
			"active_revision_id": nilAsAny(row.ActiveRevisionID),
			"created_at":         wfISO(row.CreatedAt),
			"updated_at":         wfISO(row.UpdatedAt),
		})
	}
	return map[string]any{"items": items}, nil
}

// ExportDefinition 把定义（含激活版本）序列化成可导入的 JSON。
func (s *WorkflowService) ExportDefinition(ctx context.Context, definitionID, actorID string) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		definition, err := loadDefinition(ctx, tx, definitionID, actorID)
		if err != nil {
			return err
		}
		if definition.ActiveRevisionID == nil || *definition.ActiveRevisionID == "" {
			out = map[string]any{
				"schema_version":  2,
				"name":            definition.Name,
				"nodes":           []any{},
				"edges":           []any{},
				"template_lock":   nil,
				"content_mode":    "drama",
				"generation_mode": "storyboard",
				"input_schema":    map[string]any{},
			}
			return nil
		}
		graph, err := loadGraph(ctx, tx, *definition.ActiveRevisionID, actorID)
		if err != nil {
			return err
		}
		out = map[string]any{
			"schema_version":  2,
			"name":            definition.Name,
			"nodes":           graph.Nodes,
			"edges":           graph.Edges,
			"template_lock":   wfDecodePtr(graph.Revision.TemplateLockJSON, nil),
			"content_mode":    graph.Revision.ContentMode,
			"generation_mode": graph.Revision.GenerationMode,
			"input_schema":    wfDecode(graph.Revision.InputSchemaJSON, map[string]any{}),
		}
		return nil
	})
	return out, err
}

// WorkflowImportParams 是导入工作流 JSON 的入参。
type WorkflowImportParams struct {
	WorkspaceID    string
	ProjectID      string
	Name           string
	Nodes          []map[string]any
	Edges          []map[string]any
	TemplateLock   any
	ActorID        string
	ContentMode    string
	GenerationMode string
	InputSchema    any
}

// ImportDefinition 由导出的 JSON 直接建出一条已发布定义。
func (s *WorkflowService) ImportDefinition(ctx context.Context, p WorkflowImportParams) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := domain.WorkflowValidateGraph(p.Nodes, p.Edges); err != nil {
			return err
		}
		definition, err := s.createDefinition(ctx, tx, WorkflowDefinitionParams{
			WorkspaceID: p.WorkspaceID, ProjectID: p.ProjectID, Name: p.Name, ActorID: p.ActorID,
		})
		if err != nil {
			return err
		}
		revision, err := s.createRevision(ctx, tx, WorkflowRevisionParams{
			DefinitionID:   definition["id"].(string),
			Nodes:          p.Nodes,
			Edges:          p.Edges,
			TemplateLock:   p.TemplateLock,
			ActorID:        p.ActorID,
			ContentMode:    p.ContentMode,
			GenerationMode: p.GenerationMode,
			InputSchema:    p.InputSchema,
		})
		if err != nil {
			return err
		}
		if _, err := s.publishRevision(ctx, tx, revision["id"].(string), p.ActorID); err != nil {
			return err
		}
		out = map[string]any{"definition_id": definition["id"], "revision_id": revision["id"]}
		return nil
	})
	return out, err
}

// MigrateProject 为遗留项目幂等地创建一条画布工作流。
func (s *WorkflowService) MigrateProject(
	ctx context.Context, workspaceID, projectID, actorID string,
) (map[string]any, error) {
	var out map[string]any
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing repository.WorkflowDefinition
		err := tx.Where("user_id = ? AND project_id = ? AND workspace_id = ?", actorID, projectID, workspaceID).
			Limit(1).Take(&existing).Error
		if err == nil {
			out = map[string]any{"migrated": false, "definition_id": existing.ID}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		definition, err := s.createDefinition(ctx, tx, WorkflowDefinitionParams{
			WorkspaceID: workspaceID,
			ProjectID:   projectID,
			Name:        projectID + " production",
			ActorID:     actorID,
		})
		if err != nil {
			return err
		}
		nodes, edges := LegacyLinearGraph()
		revision, err := s.createRevision(ctx, tx, WorkflowRevisionParams{
			DefinitionID:   definition["id"].(string),
			Nodes:          nodes,
			Edges:          edges,
			TemplateLock:   map[string]any{"template_schema_version": 1},
			ActorID:        actorID,
			ContentMode:    "drama",
			GenerationMode: "storyboard",
			InputSchema:    map[string]any{},
		})
		if err != nil {
			return err
		}
		if _, err := s.publishRevision(ctx, tx, revision["id"].(string), actorID); err != nil {
			return err
		}
		out = map[string]any{
			"migrated": true, "definition_id": definition["id"], "revision_id": revision["id"],
		}
		return nil
	})
	return out, err
}

// --- 内置模板 ---

// ListBuiltinTemplates 返回简单视图与画布视图共用的内置模板。
func ListBuiltinTemplates() map[string]any {
	definitions := []struct {
		ID        string
		NameKey   string
		DescKey   string
		NodeTypes []string
	}{
		{
			ID: "novel-to-manga", NameKey: "flow_template_novel_to_manga",
			DescKey: "flow_template_novel_to_manga_desc", NodeTypes: LegacyFlowNodeTypes,
		},
		{
			ID: "storyboard-to-video", NameKey: "flow_template_storyboard",
			DescKey: "flow_template_storyboard_desc",
			NodeTypes: []string{
				"script_generate", "script_review", "character_reference", "storyboard_generate",
				"quality_check", "shot_image_generate", "shot_video_generate", "compose", "export",
			},
		},
		{
			ID: "reference-to-video", NameKey: "flow_template_reference",
			DescKey: "flow_template_reference_desc",
			NodeTypes: []string{
				"source_import", "script_review", "reference_video_generate", "quality_check", "export",
			},
		},
	}
	items := make([]any, 0, len(definitions))
	for _, definition := range definitions {
		nodes, edges := templateGraph(definition.NodeTypes)
		generationMode := "storyboard"
		if definition.ID == "reference-to-video" {
			generationMode = "reference_video"
		}
		items = append(items, map[string]any{
			"id":              definition.ID,
			"scope":           "official",
			"name_key":        definition.NameKey,
			"description_key": definition.DescKey,
			"template_lock": map[string]any{
				"template_schema_version": 1, "template_id": definition.ID,
			},
			"generation_mode": generationMode,
			"nodes":           nodes,
			"edges":           edges,
		})
	}
	return map[string]any{"items": items}
}

func templateGraph(nodeTypes []string) ([]map[string]any, []map[string]any) {
	nodes := make([]map[string]any, 0, len(nodeTypes))
	edges := make([]map[string]any, 0, len(nodeTypes))
	for index, nodeType := range nodeTypes {
		weight := 1.0
		if strings.Contains(nodeType, "generate") {
			weight = 2.0
		}
		nodes = append(nodes, map[string]any{
			"node_key":              nodeType,
			"node_type":             nodeType,
			"node_type_version":     "1",
			"config_schema_version": "1",
			"config":                map[string]any{},
			"ui_position":           map[string]any{"x": index * 260, "y": (index % 3) * 80},
			"weight":                weight,
		})
	}
	for index := 0; index+1 < len(nodeTypes); index++ {
		source := nodeTypes[index]
		target := nodeTypes[index+1]
		edges = append(edges, map[string]any{
			"edge_key":        source + "-" + target,
			"source_node_key": source,
			"target_node_key": target,
			"on_failure":      "stop",
			"priority":        0,
		})
	}
	return nodes, edges
}
