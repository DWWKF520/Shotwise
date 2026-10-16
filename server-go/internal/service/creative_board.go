// 语义创意板业务服务（对应 server/services/creative_boards.py）。
//
// 创意板数据与 workflow 执行解耦：边只描述内容关系，不涉及节点调度、端口、适配器或失败分支。
package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/repository"
)

// 语义画布允许的条目类型与关系集合（与 Python 侧常量对齐）。
var creativeBoardItemTypes = map[string]bool{
	"document": true, "character": true, "scene": true, "prop": true, "product": true,
	"media": true, "episode": true, "shot": true, "skill_action": true, "review": true, "final": true,
}

var creativeBoardRelations = map[string]bool{
	"reference": true, "derived_from": true, "belongs_to": true, "shot_order": true, "composite_input": true,
}

// CreativeBoardValidationError 表示创意板载荷违反语义画布约束（HTTP 400，detail 为原文）。
type CreativeBoardValidationError struct{ Msg string }

func (e *CreativeBoardValidationError) Error() string { return e.Msg }

// CreativeBoardNotFoundError 表示板不存在或不属于请求用户（HTTP 404）。
type CreativeBoardNotFoundError struct{ Ref string }

func (e *CreativeBoardNotFoundError) Error() string { return e.Ref }

// CreativeBoardConflictError 表示版本化更新命中了较旧的板 revision（HTTP 409）。
type CreativeBoardConflictError struct {
	CurrentRevision  int64
	CurrentUpdatedAt time.Time
}

func (e *CreativeBoardConflictError) Error() string { return "creative board revision conflict" }

// errBoardCAS 是事务内 revision CAS 失败信号；事务回滚后由上层转成冲突或 404。
var errBoardCAS = errors.New("creative board revision cas failed")

// CreativeBoardService 承载创意板的读写与版本管理。
type CreativeBoardService struct {
	db   *gorm.DB
	repo *repository.CreativeBoardRepo
}

// NewCreativeBoardService 构造服务。
func NewCreativeBoardService(gdb *gorm.DB) *CreativeBoardService {
	return &CreativeBoardService{db: gdb, repo: repository.NewCreativeBoardRepo(gdb)}
}

// --- 载荷渲染 ---

func creativeBoardPayload(
	board *repository.CreativeBoard,
	items []repository.CreativeBoardItem,
	edges []repository.CreativeBoardEdge,
) map[string]any {
	itemPayloads := make([]map[string]any, 0, len(items))
	for i := range items {
		item := &items[i]
		itemPayloads = append(itemPayloads, map[string]any{
			"id":               item.ID,
			"item_type":        item.ItemType,
			"resource_type":    item.ResourceType,
			"resource_id":      item.ResourceID,
			"position":         map[string]any{"x": item.PositionX, "y": item.PositionY},
			"size":             map[string]any{"width": item.Width, "height": item.Height},
			"group_id":         item.GroupID,
			"display_settings": parseJSONValue(item.DisplaySettingsJSON),
		})
	}
	edgePayloads := make([]map[string]any, 0, len(edges))
	for i := range edges {
		edge := &edges[i]
		edgePayloads = append(edgePayloads, map[string]any{
			"id":             edge.ID,
			"source_item_id": edge.SourceItemID,
			"target_item_id": edge.TargetItemID,
			"relation":       edge.Relation,
			"ordinal":        edge.Ordinal,
			"metadata":       parseJSONValue(edge.MetadataJSON),
		})
	}
	return map[string]any{
		"id":               board.ID,
		"project_id":       board.ProjectID,
		"name":             board.Name,
		"viewport":         parseJSONValue(board.ViewportJSON),
		"display_settings": parseJSONValue(board.DisplaySettingsJSON),
		"created_at":       isoformatTime(board.CreatedAt.Time),
		"updated_at":       isoformatTime(board.UpdatedAt.Time),
		"revision":         board.Revision,
		"items":            itemPayloads,
		"edges":            edgePayloads,
	}
}

func creativeBoardVersionPayload(version *repository.CreativeBoardVersion, includeSnapshot bool) map[string]any {
	payload := map[string]any{
		"id":             version.ID,
		"board_id":       version.BoardID,
		"version_number": version.VersionNumber,
		"version_name":   version.VersionName,
		"created_at":     isoformatTime(version.CreatedAt.Time),
	}
	if includeSnapshot {
		payload["snapshot"] = parseJSONValue(version.SnapshotJSON)
	}
	return payload
}

// boardSnapshotFromPayload 从板载荷里摘出快照字段（用于建版本）。
func boardSnapshotFromPayload(payload map[string]any) map[string]any {
	return map[string]any{
		"name":             payload["name"],
		"viewport":         payload["viewport"],
		"display_settings": payload["display_settings"],
		"created_at":       payload["created_at"],
		"items":            payload["items"],
		"edges":            payload["edges"],
	}
}

// boardJSON 把对象/值序列化为库里的 JSON 文本；空 map 按 Python 口径落 "{}"。
func boardJSON(value any) (string, error) {
	if value == nil {
		value = map[string]any{}
	}
	b, err := domain.CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func boardMapOrEmpty(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

func newBoardRef() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }

// --- 读取 ---

// loadBoardPayload 读取归属校验通过的板的完整载荷。
func (s *CreativeBoardService) loadBoardPayload(ctx context.Context, board *repository.CreativeBoard) (map[string]any, error) {
	items, err := s.repo.ListItems(ctx, board.ID)
	if err != nil {
		return nil, err
	}
	edges, err := s.repo.ListEdges(ctx, board.ID)
	if err != nil {
		return nil, err
	}
	return creativeBoardPayload(board, items, edges), nil
}

// CreateBoard 新建一块语义创意板。
func (s *CreativeBoardService) CreateBoard(
	ctx context.Context, userID, projectID, name string, viewport, displaySettings map[string]any,
) (map[string]any, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return nil, &CreativeBoardValidationError{Msg: "board name must not be empty"}
	}
	viewportJSON, err := boardJSON(viewport)
	if err != nil {
		return nil, err
	}
	displayJSON, err := boardJSON(displaySettings)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	board := &repository.CreativeBoard{
		ID:                  newBoardRef(),
		UserID:              userID,
		ProjectID:           projectID,
		Name:                trimmed,
		ViewportJSON:        viewportJSON,
		DisplaySettingsJSON: displayJSON,
		CreatedAt:           db.NewFlexibleTime(now),
		UpdatedAt:           db.NewFlexibleTime(now),
		Revision:            1,
	}
	if err := s.repo.Create(ctx, board); err != nil {
		return nil, err
	}
	return s.loadBoardPayload(ctx, board)
}

// ListBoards 列出项目下的板。
func (s *CreativeBoardService) ListBoards(ctx context.Context, userID, projectID string) (map[string]any, error) {
	boards, err := s.repo.ListBoards(ctx, userID, projectID)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(boards))
	for i := range boards {
		board := &boards[i]
		items = append(items, map[string]any{
			"id":         board.ID,
			"project_id": board.ProjectID,
			"name":       board.Name,
			"updated_at": isoformatTime(board.UpdatedAt.Time),
			"revision":   board.Revision,
		})
	}
	return map[string]any{"items": items}, nil
}

// GetBoard 读取单块板。
func (s *CreativeBoardService) GetBoard(ctx context.Context, boardID, userID string) (map[string]any, error) {
	board, err := s.repo.GetOwnedBoard(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	if board == nil {
		return nil, &CreativeBoardNotFoundError{Ref: boardID}
	}
	return s.loadBoardPayload(ctx, board)
}

// --- 写入辅助 ---

// currentOrConflict 事务回滚后构造权威的冲突 / 未找到错误。
func (s *CreativeBoardService) currentOrConflict(
	ctx context.Context, boardID, userID string, expected *int64,
) error {
	board, err := s.repo.GetOwnedBoard(ctx, boardID, userID)
	if err != nil {
		return err
	}
	if board == nil || expected == nil {
		return &CreativeBoardNotFoundError{Ref: boardID}
	}
	return &CreativeBoardConflictError{CurrentRevision: board.Revision, CurrentUpdatedAt: board.UpdatedAt.Time}
}

// requireOwned 在事务外做归属校验（读取路径）。
func (s *CreativeBoardService) requireOwned(
	ctx context.Context, boardID, userID string,
) (*repository.CreativeBoard, error) {
	board, err := s.repo.GetOwnedBoard(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	if board == nil {
		return nil, &CreativeBoardNotFoundError{Ref: boardID}
	}
	return board, nil
}

// UpdateBoard 更新板元数据（名字 / viewport / 展示设置）。
func (s *CreativeBoardService) UpdateBoard(
	ctx context.Context, boardID, userID string, name *string, viewport, displaySettings map[string]any, expected *int64,
) (map[string]any, error) {
	board, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		if trimmed == "" {
			return nil, &CreativeBoardValidationError{Msg: "board name must not be empty"}
		}
		values["name"] = trimmed
	}
	if viewport != nil {
		encoded, err := boardJSON(viewport)
		if err != nil {
			return nil, err
		}
		values["viewport_json"] = encoded
	}
	if displaySettings != nil {
		encoded, err := boardJSON(displaySettings)
		if err != nil {
			return nil, err
		}
		values["display_settings_json"] = encoded
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ok, err := s.repo.WithTx(tx).AdvanceRevision(ctx, board.ID, userID, expected, values, time.Now().UTC())
		if err != nil {
			return err
		}
		if !ok {
			return errBoardCAS
		}
		return nil
	})
	if errors.Is(err, errBoardCAS) {
		return nil, s.currentOrConflict(ctx, boardID, userID, expected)
	}
	if err != nil {
		return nil, err
	}
	updated, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	return s.loadBoardPayload(ctx, updated)
}

// AddItem 新增画布条目并推进板版本。
func (s *CreativeBoardService) AddItem(
	ctx context.Context, boardID, userID string, itemType, resourceType, resourceID string,
	position, size map[string]any, groupID *string, displaySettings map[string]any, expected *int64,
) (map[string]any, error) {
	board, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	if !creativeBoardItemTypes[itemType] {
		return nil, &CreativeBoardValidationError{Msg: "unsupported board item type"}
	}
	if strings.TrimSpace(resourceID) == "" {
		return nil, &CreativeBoardValidationError{Msg: "resource_id must not be empty"}
	}
	displayJSON, err := boardJSON(displaySettings)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	item := &repository.CreativeBoardItem{
		ID:                  newBoardRef(),
		BoardID:             board.ID,
		ItemType:            itemType,
		ResourceType:        resourceType,
		ResourceID:          resourceID,
		PositionX:           int64(mapInt(position, "x", 0)),
		PositionY:           int64(mapInt(position, "y", 0)),
		Width:               int64(mapAtLeastOne(size, "width", 280)),
		Height:              int64(mapAtLeastOne(size, "height", 180)),
		GroupID:             groupID,
		DisplaySettingsJSON: displayJSON,
		CreatedAt:           db.NewFlexibleTime(now),
		UpdatedAt:           db.NewFlexibleTime(now),
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := s.repo.WithTx(tx)
		if err := repo.Create(ctx, item); err != nil {
			return err
		}
		ok, err := repo.AdvanceRevision(ctx, board.ID, userID, expected, nil, now)
		if err != nil {
			return err
		}
		if !ok {
			return errBoardCAS
		}
		return nil
	})
	if errors.Is(err, errBoardCAS) {
		return nil, s.currentOrConflict(ctx, boardID, userID, expected)
	}
	if err != nil {
		return nil, err
	}
	updated, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":            item.ID,
		"item_type":     item.ItemType,
		"resource_type": item.ResourceType,
		"resource_id":   item.ResourceID,
		"revision":      updated.Revision,
		"updated_at":    isoformatTime(updated.UpdatedAt.Time),
	}, nil
}

// UpdateItem 更新条目位置 / 尺寸 / 分组 / 展示设置。
func (s *CreativeBoardService) UpdateItem(
	ctx context.Context, boardID, itemID, userID string, position, size map[string]any,
	groupID *string, updateGroupID bool, displaySettings map[string]any, expected *int64,
) (map[string]any, error) {
	board, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	var item *repository.CreativeBoardItem
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := s.repo.WithTx(tx)
		item, err = repo.GetItem(ctx, board.ID, itemID)
		if err != nil {
			return err
		}
		if item == nil {
			return &CreativeBoardNotFoundError{Ref: itemID}
		}
		if position != nil {
			item.PositionX = int64(mapInt(position, "x", int(item.PositionX)))
			item.PositionY = int64(mapInt(position, "y", int(item.PositionY)))
		}
		if size != nil {
			item.Width = int64(mapAtLeastOne(size, "width", int(item.Width)))
			item.Height = int64(mapAtLeastOne(size, "height", int(item.Height)))
		}
		if groupID != nil || updateGroupID {
			item.GroupID = groupID
		}
		if displaySettings != nil {
			encoded, err := boardJSON(displaySettings)
			if err != nil {
				return err
			}
			item.DisplaySettingsJSON = encoded
		}
		item.UpdatedAt = db.NewFlexibleTime(time.Now().UTC())
		if err := repo.SaveItem(ctx, item); err != nil {
			return err
		}
		ok, err := repo.AdvanceRevision(ctx, board.ID, userID, expected, nil, item.UpdatedAt.Time)
		if err != nil {
			return err
		}
		if !ok {
			return errBoardCAS
		}
		return nil
	})
	if errors.Is(err, errBoardCAS) {
		return nil, s.currentOrConflict(ctx, boardID, userID, expected)
	}
	if err != nil {
		return nil, err
	}
	updated, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	return s.loadBoardPayload(ctx, updated)
}

// DeleteItem 删除条目及其相连边。
func (s *CreativeBoardService) DeleteItem(
	ctx context.Context, boardID, itemID, userID string, expected *int64,
) (map[string]any, error) {
	board, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := s.repo.WithTx(tx)
		item, err := repo.GetItem(ctx, board.ID, itemID)
		if err != nil {
			return err
		}
		if item == nil {
			return &CreativeBoardNotFoundError{Ref: itemID}
		}
		if err := repo.DeleteEdgesTouching(ctx, board.ID, item.ID); err != nil {
			return err
		}
		if err := repo.DeleteItem(ctx, item); err != nil {
			return err
		}
		ok, err := repo.AdvanceRevision(ctx, board.ID, userID, expected, nil, now)
		if err != nil {
			return err
		}
		if !ok {
			return errBoardCAS
		}
		return nil
	})
	if errors.Is(err, errBoardCAS) {
		return nil, s.currentOrConflict(ctx, boardID, userID, expected)
	}
	if err != nil {
		return nil, err
	}
	updated, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"deleted":    true,
		"item_id":    itemID,
		"revision":   updated.Revision,
		"updated_at": isoformatTime(updated.UpdatedAt.Time),
	}, nil
}

// AddEdge 新增语义边（重复关系直接返回已有边）。
func (s *CreativeBoardService) AddEdge(
	ctx context.Context, boardID, userID, sourceID, targetID, relation string, ordinal *int64,
	metadata map[string]any, expected *int64,
) (map[string]any, error) {
	board, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	if !creativeBoardRelations[relation] {
		return nil, &CreativeBoardValidationError{Msg: "unsupported board edge relation"}
	}
	if sourceID == targetID {
		return nil, &CreativeBoardValidationError{Msg: "board edge endpoints must differ"}
	}
	ids, err := s.repo.ListItemIDs(ctx, board.ID)
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(ids))
	for _, id := range ids {
		known[id] = true
	}
	if !known[sourceID] || !known[targetID] {
		return nil, &CreativeBoardValidationError{Msg: "board edge endpoints must belong to the board"}
	}
	existing, err := s.repo.FindEdge(ctx, board.ID, sourceID, targetID, relation)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if expected != nil && board.Revision != *expected {
			return nil, &CreativeBoardConflictError{
				CurrentRevision: board.Revision, CurrentUpdatedAt: board.UpdatedAt.Time,
			}
		}
		return map[string]any{
			"id":             existing.ID,
			"source_item_id": existing.SourceItemID,
			"target_item_id": existing.TargetItemID,
			"relation":       existing.Relation,
			"revision":       board.Revision,
			"updated_at":     isoformatTime(board.UpdatedAt.Time),
		}, nil
	}
	metadataJSON, err := boardJSON(metadata)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	edge := &repository.CreativeBoardEdge{
		ID:           newBoardRef(),
		BoardID:      board.ID,
		SourceItemID: sourceID,
		TargetItemID: targetID,
		Relation:     relation,
		Ordinal:      ordinal,
		MetadataJSON: metadataJSON,
		CreatedAt:    db.NewFlexibleTime(now),
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := s.repo.WithTx(tx)
		if err := repo.Create(ctx, edge); err != nil {
			return err
		}
		ok, err := repo.AdvanceRevision(ctx, board.ID, userID, expected, nil, now)
		if err != nil {
			return err
		}
		if !ok {
			return errBoardCAS
		}
		return nil
	})
	if errors.Is(err, errBoardCAS) {
		return nil, s.currentOrConflict(ctx, boardID, userID, expected)
	}
	if err != nil {
		return nil, err
	}
	updated, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":             edge.ID,
		"source_item_id": edge.SourceItemID,
		"target_item_id": edge.TargetItemID,
		"relation":       edge.Relation,
		"revision":       updated.Revision,
		"updated_at":     isoformatTime(updated.UpdatedAt.Time),
	}, nil
}

// DeleteEdge 删除单条语义边。
func (s *CreativeBoardService) DeleteEdge(
	ctx context.Context, boardID, edgeID, userID string, expected *int64,
) (map[string]any, error) {
	board, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := s.repo.WithTx(tx)
		edge, err := repo.GetEdge(ctx, board.ID, edgeID)
		if err != nil {
			return err
		}
		if edge == nil {
			return &CreativeBoardNotFoundError{Ref: edgeID}
		}
		if err := repo.DeleteEdge(ctx, edge); err != nil {
			return err
		}
		ok, err := repo.AdvanceRevision(ctx, board.ID, userID, expected, nil, now)
		if err != nil {
			return err
		}
		if !ok {
			return errBoardCAS
		}
		return nil
	})
	if errors.Is(err, errBoardCAS) {
		return nil, s.currentOrConflict(ctx, boardID, userID, expected)
	}
	if err != nil {
		return nil, err
	}
	updated, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"deleted":    true,
		"edge_id":    edgeID,
		"revision":   updated.Revision,
		"updated_at": isoformatTime(updated.UpdatedAt.Time),
	}, nil
}

// DeleteBoard 删除整块板及其条目 / 边。
func (s *CreativeBoardService) DeleteBoard(
	ctx context.Context, boardID, userID string, expected *int64,
) (map[string]any, error) {
	board, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	revision, updatedAt := board.Revision, board.UpdatedAt.Time
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := s.repo.WithTx(tx)
		if err := repo.DeleteEdgesByBoard(ctx, board.ID); err != nil {
			return err
		}
		if err := repo.DeleteItemsByBoard(ctx, board.ID); err != nil {
			return err
		}
		ok, err := repo.DeleteBoardRow(ctx, board.ID, userID, expected)
		if err != nil {
			return err
		}
		if !ok {
			return errBoardCAS
		}
		return nil
	})
	if errors.Is(err, errBoardCAS) {
		return nil, s.currentOrConflict(ctx, boardID, userID, expected)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"deleted":    true,
		"board_id":   boardID,
		"revision":   revision,
		"updated_at": isoformatTime(updatedAt),
	}, nil
}

// ReplaceSnapshot 原子替换整板快照并推进版本。
func (s *CreativeBoardService) ReplaceSnapshot(
	ctx context.Context, boardID, userID string, snapshot map[string]any, expected *int64,
) (map[string]any, error) {
	board, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	normalised, err := normaliseBoardSnapshot(snapshot, board.CreatedAt.Time)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := s.repo.WithTx(tx)
		if err := repo.DeleteEdgesByBoard(ctx, board.ID); err != nil {
			return err
		}
		if err := repo.DeleteItemsByBoard(ctx, board.ID); err != nil {
			return err
		}
		for _, item := range normalised.Items {
			item.BoardID = board.ID
			item.CreatedAt = db.NewFlexibleTime(now)
			item.UpdatedAt = db.NewFlexibleTime(now)
			if err := repo.Create(ctx, item); err != nil {
				return err
			}
		}
		for _, edge := range normalised.Edges {
			edge.BoardID = board.ID
			edge.CreatedAt = db.NewFlexibleTime(now)
			if err := repo.Create(ctx, edge); err != nil {
				return err
			}
		}
		values := map[string]any{
			"name":                  normalised.Name,
			"viewport_json":         normalised.ViewportJSON,
			"display_settings_json": normalised.DisplaySettingsJSON,
			"created_at":            db.NewFlexibleTime(normalised.CreatedAt),
		}
		ok, err := repo.AdvanceRevision(ctx, board.ID, userID, expected, values, now)
		if err != nil {
			return err
		}
		if !ok {
			return errBoardCAS
		}
		return nil
	})
	if errors.Is(err, errBoardCAS) {
		return nil, s.currentOrConflict(ctx, boardID, userID, expected)
	}
	if err != nil {
		return nil, err
	}
	updated, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	return s.loadBoardPayload(ctx, updated)
}

// CreateVersion 记录一块板的命名版本快照。
func (s *CreativeBoardService) CreateVersion(
	ctx context.Context, boardID, userID, versionName string, expected *int64,
) (map[string]any, error) {
	board, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	if expected != nil && board.Revision != *expected {
		return nil, &CreativeBoardConflictError{CurrentRevision: board.Revision, CurrentUpdatedAt: board.UpdatedAt.Time}
	}
	trimmed := strings.TrimSpace(versionName)
	if trimmed == "" {
		return nil, &CreativeBoardValidationError{Msg: "version name must not be empty"}
	}
	latest, err := s.repo.LatestVersion(ctx, board.ID)
	if err != nil {
		return nil, err
	}
	var nextNumber int64 = 1
	if latest != nil {
		nextNumber = latest.VersionNumber + 1
	}
	snapshot, err := s.loadBoardPayload(ctx, board)
	if err != nil {
		return nil, err
	}
	snapshotJSON, err := boardJSON(boardSnapshotFromPayload(snapshot))
	if err != nil {
		return nil, err
	}
	version := &repository.CreativeBoardVersion{
		ID:            newBoardRef(),
		BoardID:       board.ID,
		VersionNumber: nextNumber,
		VersionName:   trimmed,
		SnapshotJSON:  snapshotJSON,
		CreatedAt:     db.NewFlexibleTime(time.Now().UTC()),
	}
	if err := s.repo.Create(ctx, version); err != nil {
		return nil, err
	}
	return creativeBoardVersionPayload(version, true), nil
}

// ListVersions 列出版本列表（不含快照）。
func (s *CreativeBoardService) ListVersions(ctx context.Context, boardID, userID string) (map[string]any, error) {
	board, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	versions, err := s.repo.ListVersions(ctx, board.ID)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(versions))
	for i := range versions {
		items = append(items, creativeBoardVersionPayload(&versions[i], false))
	}
	return map[string]any{"items": items}, nil
}

// GetVersion 读取单个版本（含快照）。
func (s *CreativeBoardService) GetVersion(
	ctx context.Context, boardID, versionID, userID string,
) (map[string]any, error) {
	board, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	version, err := s.repo.GetVersion(ctx, board.ID, versionID)
	if err != nil {
		return nil, err
	}
	if version == nil {
		return nil, &CreativeBoardNotFoundError{Ref: versionID}
	}
	return creativeBoardVersionPayload(version, true), nil
}

// RestoreVersion 用某个版本快照替换当前板内容。
func (s *CreativeBoardService) RestoreVersion(
	ctx context.Context, boardID, versionID, userID string, expectedRevision int64,
) (map[string]any, error) {
	version, err := s.GetVersion(ctx, boardID, versionID, userID)
	if err != nil {
		return nil, err
	}
	snapshot, _ := version["snapshot"].(map[string]any)
	return s.ReplaceSnapshot(ctx, boardID, userID, snapshot, &expectedRevision)
}

// CopyBoard 复制一块板（可选择某个版本作为源）。
func (s *CreativeBoardService) CopyBoard(
	ctx context.Context, boardID, userID string, name *string, versionID *string,
) (map[string]any, error) {
	board, err := s.requireOwned(ctx, boardID, userID)
	if err != nil {
		return nil, err
	}
	var source map[string]any
	if versionID == nil {
		payload, err := s.loadBoardPayload(ctx, board)
		if err != nil {
			return nil, err
		}
		source = boardSnapshotFromPayload(payload)
	} else {
		version, err := s.GetVersion(ctx, boardID, *versionID, userID)
		if err != nil {
			return nil, err
		}
		source, _ = version["snapshot"].(map[string]any)
	}
	copyName := ""
	if name != nil {
		copyName = strings.TrimSpace(*name)
	} else {
		sourceName, _ := source["name"].(string)
		copyName = sourceName + " copy"
	}
	if copyName == "" {
		return nil, &CreativeBoardValidationError{Msg: "board name must not be empty"}
	}
	normalised, err := normaliseBoardSnapshot(source, board.CreatedAt.Time)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	newBoard := &repository.CreativeBoard{
		ID:                  newBoardRef(),
		UserID:              userID,
		ProjectID:           board.ProjectID,
		Name:                copyName,
		ViewportJSON:        normalised.ViewportJSON,
		DisplaySettingsJSON: normalised.DisplaySettingsJSON,
		CreatedAt:           db.NewFlexibleTime(now),
		UpdatedAt:           db.NewFlexibleTime(now),
		Revision:            1,
	}
	itemIDs := make(map[string]string, len(normalised.Items))
	for _, item := range normalised.Items {
		itemIDs[item.ID] = newBoardRef()
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := s.repo.WithTx(tx)
		if err := repo.Create(ctx, newBoard); err != nil {
			return err
		}
		for _, item := range normalised.Items {
			clone := *item
			clone.ID = itemIDs[item.ID]
			clone.BoardID = newBoard.ID
			clone.CreatedAt = db.NewFlexibleTime(now)
			clone.UpdatedAt = db.NewFlexibleTime(now)
			if err := repo.Create(ctx, &clone); err != nil {
				return err
			}
		}
		for _, edge := range normalised.Edges {
			clone := *edge
			clone.ID = newBoardRef()
			clone.BoardID = newBoard.ID
			clone.SourceItemID = itemIDs[edge.SourceItemID]
			clone.TargetItemID = itemIDs[edge.TargetItemID]
			clone.CreatedAt = db.NewFlexibleTime(now)
			if err := repo.Create(ctx, &clone); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.loadBoardPayload(ctx, newBoard)
}

// --- 内部工具 ---

func mapInt(m map[string]any, key string, fallback int) int {
	if m == nil {
		return fallback
	}
	raw, present := m[key]
	if !present || raw == nil {
		return fallback
	}
	switch v := raw.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return fallback
}

func mapAtLeastOne(m map[string]any, key string, fallback int) int {
	value := mapInt(m, key, fallback)
	if value < 1 {
		return 1
	}
	return value
}

// normalisedBoard 是校验归一后的快照内容；BoardID 由调用方补齐。
type normalisedBoard struct {
	Name                string
	ViewportJSON        string
	DisplaySettingsJSON string
	CreatedAt           time.Time
	Items               []*repository.CreativeBoardItem
	Edges               []*repository.CreativeBoardEdge
}

// normaliseBoardSnapshot 校验并把快照归一为落库行（对应 Python 的 _normalise_snapshot）。
func normaliseBoardSnapshot(snapshot map[string]any, fallbackCreatedAt time.Time) (*normalisedBoard, error) {
	name, _ := snapshot["name"].(string)
	if strings.TrimSpace(name) == "" {
		return nil, &CreativeBoardValidationError{Msg: "board name must not be empty"}
	}
	rawItems, ok := anyListOrEmpty(snapshot["items"])
	if !ok {
		return nil, &CreativeBoardValidationError{Msg: "snapshot items and edges must be lists"}
	}
	rawEdges, ok := anyListOrEmpty(snapshot["edges"])
	if !ok {
		return nil, &CreativeBoardValidationError{Msg: "snapshot items and edges must be lists"}
	}

	items := make([]*repository.CreativeBoardItem, 0, len(rawItems))
	itemIDs := make(map[string]bool, len(rawItems))
	for _, raw := range rawItems {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, &CreativeBoardValidationError{Msg: "snapshot items must be objects"}
		}
		itemID := stringField(entry, "id")
		if itemID == "" {
			itemID = newBoardRef()
		}
		if itemIDs[itemID] {
			return nil, &CreativeBoardValidationError{Msg: "snapshot item ids must be unique"}
		}
		itemType := stringField(entry, "item_type")
		if !creativeBoardItemTypes[itemType] {
			return nil, &CreativeBoardValidationError{Msg: "unsupported board item type"}
		}
		resourceType := stringField(entry, "resource_type")
		if strings.TrimSpace(resourceType) == "" {
			return nil, &CreativeBoardValidationError{Msg: "resource_type must not be empty"}
		}
		resourceID := stringField(entry, "resource_id")
		if strings.TrimSpace(resourceID) == "" {
			return nil, &CreativeBoardValidationError{Msg: "resource_id must not be empty"}
		}
		position, _ := entry["position"].(map[string]any)
		size, _ := entry["size"].(map[string]any)
		if entry["position"] != nil && position == nil {
			return nil, &CreativeBoardValidationError{Msg: "snapshot item position and size must be objects"}
		}
		if entry["size"] != nil && size == nil {
			return nil, &CreativeBoardValidationError{Msg: "snapshot item position and size must be objects"}
		}
		displaySettings, _ := entry["display_settings"].(map[string]any)
		displayJSON, err := boardJSON(displaySettings)
		if err != nil {
			return nil, err
		}
		itemIDs[itemID] = true
		items = append(items, &repository.CreativeBoardItem{
			ID:                  itemID,
			ItemType:            itemType,
			ResourceType:        resourceType,
			ResourceID:          resourceID,
			PositionX:           int64(mapInt(position, "x", 0)),
			PositionY:           int64(mapInt(position, "y", 0)),
			Width:               int64(mapAtLeastOne(size, "width", 280)),
			Height:              int64(mapAtLeastOne(size, "height", 180)),
			GroupID:             optionalStringField(entry, "group_id"),
			DisplaySettingsJSON: displayJSON,
		})
	}

	edges := make([]*repository.CreativeBoardEdge, 0, len(rawEdges))
	edgeKeys := make(map[string]bool, len(rawEdges))
	edgeIDs := make(map[string]bool, len(rawEdges))
	for _, raw := range rawEdges {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, &CreativeBoardValidationError{Msg: "snapshot edges must be objects"}
		}
		source := stringField(entry, "source_item_id")
		target := stringField(entry, "target_item_id")
		relation := stringField(entry, "relation")
		edgeID := stringField(entry, "id")
		if edgeID == "" {
			edgeID = newBoardRef()
		}
		if source == "" || target == "" || source == target {
			return nil, &CreativeBoardValidationError{Msg: "board edge endpoints must differ"}
		}
		if !itemIDs[source] || !itemIDs[target] {
			return nil, &CreativeBoardValidationError{Msg: "board edge endpoints must belong to the board"}
		}
		if !creativeBoardRelations[relation] {
			return nil, &CreativeBoardValidationError{Msg: "unsupported board edge relation"}
		}
		key := source + "\x00" + target + "\x00" + relation
		if edgeKeys[key] || edgeIDs[edgeID] {
			return nil, &CreativeBoardValidationError{Msg: "snapshot edge ids and relationships must be unique"}
		}
		edgeKeys[key] = true
		edgeIDs[edgeID] = true
		metadata, _ := entry["metadata"].(map[string]any)
		metadataJSON, err := boardJSON(metadata)
		if err != nil {
			return nil, err
		}
		edges = append(edges, &repository.CreativeBoardEdge{
			ID:           edgeID,
			SourceItemID: source,
			TargetItemID: target,
			Relation:     relation,
			Ordinal:      optionalIntField(entry, "ordinal"),
			MetadataJSON: metadataJSON,
		})
	}

	createdAt, err := snapshotCreatedAt(snapshot["created_at"], fallbackCreatedAt)
	if err != nil {
		return nil, err
	}
	viewportJSON, err := boardJSON(asMapOrNil(snapshot["viewport"]))
	if err != nil {
		return nil, err
	}
	displayJSON, err := boardJSON(asMapOrNil(snapshot["display_settings"]))
	if err != nil {
		return nil, err
	}
	return &normalisedBoard{
		Name:                strings.TrimSpace(name),
		ViewportJSON:        viewportJSON,
		DisplaySettingsJSON: displayJSON,
		CreatedAt:           createdAt,
		Items:               items,
		Edges:               edges,
	}, nil
}

// snapshotCreatedAt 解析快照 created_at：缺失回落 fallback；非法抛校验错误。
func snapshotCreatedAt(value any, fallback time.Time) (time.Time, error) {
	if value == nil {
		return fallback, nil
	}
	text, ok := value.(string)
	if !ok {
		return time.Time{}, &CreativeBoardValidationError{Msg: "snapshot created_at must be an ISO timestamp"}
	}
	if text == "" {
		return fallback, nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999", "2006-01-02T15:04:05"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, &CreativeBoardValidationError{Msg: "snapshot created_at must be an ISO timestamp"}
}

func anyListOrEmpty(value any) ([]any, bool) {
	switch v := value.(type) {
	case nil:
		return []any{}, true
	case []any:
		return v, true
	case []map[string]any:
		// 快照可能来自服务内部构造的载荷（未经 JSON 往返），条目是具名切片。
		list := make([]any, 0, len(v))
		for _, item := range v {
			list = append(list, item)
		}
		return list, true
	}
	return nil, false
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func optionalStringField(m map[string]any, key string) *string {
	raw, present := m[key]
	if !present || raw == nil {
		return nil
	}
	s, ok := raw.(string)
	if !ok {
		return nil
	}
	return &s
}

func optionalIntField(m map[string]any, key string) *int64 {
	raw, present := m[key]
	if !present || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case float64:
		n := int64(v)
		return &n
	case int64:
		return &v
	case int:
		n := int64(v)
		return &n
	}
	return nil
}

func asMapOrNil(value any) map[string]any {
	if value == nil {
		return nil
	}
	m, _ := value.(map[string]any)
	return m
}
