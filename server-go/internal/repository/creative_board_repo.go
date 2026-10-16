package repository

// 语义创意板仓储（对应 server/services/creative_boards.py 内的读写原语）。
// 创意板数据与 workflow 执行解耦：边只描述内容关系，不涉及节点调度。

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// CreativeBoardRepo 读写 creative_boards 及其 items / edges / versions。
type CreativeBoardRepo struct{ db *gorm.DB }

// NewCreativeBoardRepo 构造仓储。
func NewCreativeBoardRepo(gdb *gorm.DB) *CreativeBoardRepo { return &CreativeBoardRepo{db: gdb} }

// WithTx 返回绑定到给定事务的仓储副本。
func (r *CreativeBoardRepo) WithTx(tx *gorm.DB) *CreativeBoardRepo { return &CreativeBoardRepo{db: tx} }

// GetOwnedBoard 仅在板属于请求用户时返回；不存在或非本人所有返回 (nil, nil)。
func (r *CreativeBoardRepo) GetOwnedBoard(ctx context.Context, boardID, userID string) (*CreativeBoard, error) {
	var board CreativeBoard
	err := r.db.WithContext(ctx).Where("id = ? AND user_id = ?", boardID, userID).Take(&board).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &board, nil
}

// ListBoards 按 updated_at 倒序列出项目下的板。
func (r *CreativeBoardRepo) ListBoards(ctx context.Context, userID, projectID string) ([]CreativeBoard, error) {
	var boards []CreativeBoard
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND project_id = ?", userID, projectID).
		Order("updated_at DESC").Find(&boards).Error
	return boards, err
}

// ListItems 按 created_at 升序列出板内条目。
func (r *CreativeBoardRepo) ListItems(ctx context.Context, boardID string) ([]CreativeBoardItem, error) {
	var items []CreativeBoardItem
	err := r.db.WithContext(ctx).Where("board_id = ?", boardID).Order("created_at").Find(&items).Error
	return items, err
}

// ListEdges 按 created_at 升序列出板内边。
func (r *CreativeBoardRepo) ListEdges(ctx context.Context, boardID string) ([]CreativeBoardEdge, error) {
	var edges []CreativeBoardEdge
	err := r.db.WithContext(ctx).Where("board_id = ?", boardID).Order("created_at").Find(&edges).Error
	return edges, err
}

// GetItem 取板内单条目；不存在返回 (nil, nil)。
func (r *CreativeBoardRepo) GetItem(ctx context.Context, boardID, itemID string) (*CreativeBoardItem, error) {
	var item CreativeBoardItem
	err := r.db.WithContext(ctx).Where("id = ? AND board_id = ?", itemID, boardID).Take(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// GetEdge 取板内单边；不存在返回 (nil, nil)。
func (r *CreativeBoardRepo) GetEdge(ctx context.Context, boardID, edgeID string) (*CreativeBoardEdge, error) {
	var edge CreativeBoardEdge
	err := r.db.WithContext(ctx).Where("id = ? AND board_id = ?", edgeID, boardID).Take(&edge).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &edge, nil
}

// FindEdge 按 (source, target, relation) 查重；无重复返回 (nil, nil)。
func (r *CreativeBoardRepo) FindEdge(
	ctx context.Context, boardID, sourceID, targetID, relation string,
) (*CreativeBoardEdge, error) {
	var edge CreativeBoardEdge
	err := r.db.WithContext(ctx).Where(
		"board_id = ? AND source_item_id = ? AND target_item_id = ? AND relation = ?",
		boardID, sourceID, targetID, relation,
	).Take(&edge).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &edge, nil
}

// ListItemIDs 列出板内全部条目 id。
func (r *CreativeBoardRepo) ListItemIDs(ctx context.Context, boardID string) ([]string, error) {
	var ids []string
	err := r.db.WithContext(ctx).Model(&CreativeBoardItem{}).
		Where("board_id = ?", boardID).Pluck("id", &ids).Error
	return ids, err
}

// Create 写入一行（板 / 条目 / 边 / 版本）。
func (r *CreativeBoardRepo) Create(ctx context.Context, row any) error {
	return r.db.WithContext(ctx).Create(row).Error
}

// SaveItem 落库单条目的当前字段。
func (r *CreativeBoardRepo) SaveItem(ctx context.Context, item *CreativeBoardItem) error {
	return r.db.WithContext(ctx).Model(item).
		Select("item_type", "resource_type", "resource_id", "position_x", "position_y",
			"width", "height", "group_id", "display_settings_json", "updated_at").
		Updates(item).Error
}

// DeleteItem 删除单条目。
func (r *CreativeBoardRepo) DeleteItem(ctx context.Context, item *CreativeBoardItem) error {
	return r.db.WithContext(ctx).Delete(item).Error
}

// DeleteEdgesTouching 删除与某条目相连的所有边。
func (r *CreativeBoardRepo) DeleteEdgesTouching(ctx context.Context, boardID, itemID string) error {
	return r.db.WithContext(ctx).
		Where("board_id = ? AND (source_item_id = ? OR target_item_id = ?)", boardID, itemID, itemID).
		Delete(&CreativeBoardEdge{}).Error
}

// DeleteEdge 删除单边。
func (r *CreativeBoardRepo) DeleteEdge(ctx context.Context, edge *CreativeBoardEdge) error {
	return r.db.WithContext(ctx).Delete(edge).Error
}

// DeleteItemsByBoard 删除板内全部条目。
func (r *CreativeBoardRepo) DeleteItemsByBoard(ctx context.Context, boardID string) error {
	return r.db.WithContext(ctx).Where("board_id = ?", boardID).Delete(&CreativeBoardItem{}).Error
}

// DeleteEdgesByBoard 删除板内全部边。
func (r *CreativeBoardRepo) DeleteEdgesByBoard(ctx context.Context, boardID string) error {
	return r.db.WithContext(ctx).Where("board_id = ?", boardID).Delete(&CreativeBoardEdge{}).Error
}

// DeleteBoardRow 删除板行；expected 非 nil 时做 revision CAS；命中一行返回 true。
func (r *CreativeBoardRepo) DeleteBoardRow(
	ctx context.Context, boardID, userID string, expected *int64,
) (bool, error) {
	q := r.db.WithContext(ctx).Where("id = ? AND user_id = ?", boardID, userID)
	if expected != nil {
		q = q.Where("revision = ?", *expected)
	}
	result := q.Delete(&CreativeBoard{})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// AdvanceRevision 用 revision CAS 推进板版本号，可顺带写其它字段；
// expected 非 nil 时要求 revision 等于该值；命中一行返回 true。
func (r *CreativeBoardRepo) AdvanceRevision(
	ctx context.Context, boardID, userID string, expected *int64, values map[string]any, now time.Time,
) (bool, error) {
	updates := map[string]any{
		"updated_at": now,
		"revision":   gorm.Expr("revision + 1"),
	}
	for k, v := range values {
		updates[k] = v
	}
	q := r.db.WithContext(ctx).Model(&CreativeBoard{}).Where("id = ? AND user_id = ?", boardID, userID)
	if expected != nil {
		q = q.Where("revision = ?", *expected)
	}
	result := q.Updates(updates)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// LatestVersion 取版本号最大的版本；无版本返回 (nil, nil)。
func (r *CreativeBoardRepo) LatestVersion(ctx context.Context, boardID string) (*CreativeBoardVersion, error) {
	var version CreativeBoardVersion
	err := r.db.WithContext(ctx).Where("board_id = ?", boardID).
		Order("version_number DESC").Limit(1).Take(&version).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &version, nil
}

// ListVersions 按 created_at 倒序（再按版本号倒序）列出。
func (r *CreativeBoardRepo) ListVersions(ctx context.Context, boardID string) ([]CreativeBoardVersion, error) {
	var versions []CreativeBoardVersion
	err := r.db.WithContext(ctx).Where("board_id = ?", boardID).
		Order("created_at DESC, version_number DESC").Find(&versions).Error
	return versions, err
}

// GetVersion 取板内单版本；不存在返回 (nil, nil)。
func (r *CreativeBoardRepo) GetVersion(ctx context.Context, boardID, versionID string) (*CreativeBoardVersion, error) {
	var version CreativeBoardVersion
	err := r.db.WithContext(ctx).Where("id = ? AND board_id = ?", versionID, boardID).Take(&version).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &version, nil
}
