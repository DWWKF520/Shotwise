package creative

import (
	"context"
	"errors"
	"testing"

	"github.com/shotwise/shotwise/internal/repository"
)

func newBoard(t *testing.T, svc *CreativeBoardService) map[string]any {
	t.Helper()
	board, err := svc.CreateBoard(
		context.Background(), repository.DefaultUserID, "p1", "Board A",
		map[string]any{"zoom": 1.0}, map[string]any{"theme": "dark"},
	)
	if err != nil {
		t.Fatalf("create board: %v", err)
	}
	return board
}

func TestCreativeBoardCreateAndAddItem(t *testing.T) {
	svc := NewCreativeBoardService(newServiceTestDB(t))
	ctx := context.Background()
	user := repository.DefaultUserID

	board := newBoard(t, svc)
	boardID, _ := board["id"].(string)
	if board["revision"] != int64(1) {
		t.Fatalf("new board revision = %v", board["revision"])
	}

	added, err := svc.AddItem(ctx, boardID, user, "character", "character", "林岚",
		map[string]any{"x": 12, "y": 34}, map[string]any{"width": 300, "height": 200}, nil, nil, nil)
	if err != nil {
		t.Fatalf("add item: %v", err)
	}
	if added["revision"] != int64(2) {
		t.Fatalf("item add should bump revision, got %v", added["revision"])
	}

	fetched, err := svc.GetBoard(ctx, boardID, user)
	if err != nil {
		t.Fatalf("get board: %v", err)
	}
	items, _ := fetched["items"].([]map[string]any)
	if len(items) != 1 {
		t.Fatalf("board items = %v", fetched["items"])
	}
	if items[0]["resource_id"] != "林岚" {
		t.Fatalf("item payload = %v", items[0])
	}
	position, _ := items[0]["position"].(map[string]any)
	if position["x"] != int64(12) {
		t.Fatalf("position = %v", position)
	}
}

func TestCreativeBoardAddItemRejectsUnknownType(t *testing.T) {
	svc := NewCreativeBoardService(newServiceTestDB(t))
	board := newBoard(t, svc)
	boardID, _ := board["id"].(string)

	_, err := svc.AddItem(context.Background(), boardID, repository.DefaultUserID, "bogus", "character", "x",
		nil, nil, nil, nil, nil)
	var validation *CreativeBoardValidationError
	if !errors.As(err, &validation) || validation.Msg != "unsupported board item type" {
		t.Fatalf("want validation error, got %v", err)
	}
}

func TestCreativeBoardRevisionConflict(t *testing.T) {
	svc := NewCreativeBoardService(newServiceTestDB(t))
	ctx := context.Background()
	user := repository.DefaultUserID
	board := newBoard(t, svc)
	boardID, _ := board["id"].(string)

	stale := int64(99)
	_, err := svc.UpdateBoard(ctx, boardID, user, nil, map[string]any{"zoom": 2.0}, nil, &stale)
	var conflict *CreativeBoardConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	if conflict.CurrentRevision != 1 {
		t.Fatalf("conflict current_revision = %d", conflict.CurrentRevision)
	}
}

func TestCreativeBoardItemUpdateAndEdgeLifecycle(t *testing.T) {
	svc := NewCreativeBoardService(newServiceTestDB(t))
	ctx := context.Background()
	user := repository.DefaultUserID
	board := newBoard(t, svc)
	boardID, _ := board["id"].(string)

	first, err := svc.AddItem(ctx, boardID, user, "character", "character", "A", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("add first: %v", err)
	}
	second, err := svc.AddItem(ctx, boardID, user, "scene", "scene", "B", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("add second: %v", err)
	}
	aID, _ := first["id"].(string)
	bID, _ := second["id"].(string)

	// 移动 A 的位置。
	updated, err := svc.UpdateItem(ctx, boardID, aID, user,
		map[string]any{"x": 100}, nil, nil, false, nil, nil)
	if err != nil {
		t.Fatalf("update item: %v", err)
	}
	items, _ := updated["items"].([]map[string]any)
	var moved map[string]any
	for _, item := range items {
		if item["id"] == aID {
			moved = item
		}
	}
	position, _ := moved["position"].(map[string]any)
	if position["x"] != int64(100) {
		t.Fatalf("moved position = %v", position)
	}

	edge, err := svc.AddEdge(ctx, boardID, user, aID, bID, "reference", nil, nil, nil)
	if err != nil {
		t.Fatalf("add edge: %v", err)
	}
	edgeID, _ := edge["id"].(string)

	// 重复边直接返回已有边，不新增。
	again, err := svc.AddEdge(ctx, boardID, user, aID, bID, "reference", nil, nil, nil)
	if err != nil {
		t.Fatalf("duplicate edge: %v", err)
	}
	if again["id"] != edgeID {
		t.Fatalf("duplicate edge should reuse id: %v vs %v", again["id"], edgeID)
	}

	// 非法 relation。
	_, err = svc.AddEdge(ctx, boardID, user, aID, bID, "bogus", nil, nil, nil)
	var validation *CreativeBoardValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("want validation error, got %v", err)
	}

	// 删除条目会连带删除相连边。
	result, err := svc.DeleteItem(ctx, boardID, aID, user, nil)
	if err != nil {
		t.Fatalf("delete item: %v", err)
	}
	if result["deleted"] != true {
		t.Fatalf("delete result = %v", result)
	}
	fetched, err := svc.GetBoard(ctx, boardID, user)
	if err != nil {
		t.Fatalf("get board: %v", err)
	}
	if edges, _ := fetched["edges"].([]map[string]any); len(edges) != 0 {
		t.Fatalf("edges should be cascaded away, got %v", edges)
	}
}

func TestCreativeBoardSnapshotVersionAndCopy(t *testing.T) {
	svc := NewCreativeBoardService(newServiceTestDB(t))
	ctx := context.Background()
	user := repository.DefaultUserID
	board := newBoard(t, svc)
	boardID, _ := board["id"].(string)

	snapshot := map[string]any{
		"name": "service.Snapshot Board",
		"items": []any{
			map[string]any{"id": "i1", "item_type": "character", "resource_type": "character", "resource_id": "A"},
			map[string]any{"id": "i2", "item_type": "scene", "resource_type": "scene", "resource_id": "B"},
		},
		"edges": []any{
			map[string]any{"id": "e1", "source_item_id": "i1", "target_item_id": "i2", "relation": "reference"},
		},
	}
	expected := int64(1)
	replaced, err := svc.ReplaceSnapshot(ctx, boardID, user, snapshot, &expected)
	if err != nil {
		t.Fatalf("replace snapshot: %v", err)
	}
	if replaced["name"] != "service.Snapshot Board" || replaced["revision"] != int64(2) {
		t.Fatalf("replaced board = %v", replaced)
	}

	version, err := svc.CreateVersion(ctx, boardID, user, "v1", nil)
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	if version["version_number"] != int64(1) {
		t.Fatalf("version = %v", version)
	}

	versions, err := svc.ListVersions(ctx, boardID, user)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if items, _ := versions["items"].([]map[string]any); len(items) != 1 {
		t.Fatalf("versions items = %v", versions["items"])
	}

	versionID, _ := version["id"].(string)
	restored, err := svc.RestoreVersion(ctx, boardID, versionID, user, 2)
	if err != nil {
		t.Fatalf("restore version: %v", err)
	}
	if restored["revision"] != int64(3) {
		t.Fatalf("restore should bump revision: %v", restored["revision"])
	}

	copied, err := svc.CopyBoard(ctx, boardID, user, nil, nil)
	if err != nil {
		t.Fatalf("copy board: %v", err)
	}
	if copied["name"] != "service.Snapshot Board copy" {
		t.Fatalf("copy name = %v", copied["name"])
	}
	copiedItems, _ := copied["items"].([]map[string]any)
	if len(copiedItems) != 2 {
		t.Fatalf("copied items = %v", copied["items"])
	}
}

func TestCreativeBoardSnapshotRejectsInvalidRelation(t *testing.T) {
	svc := NewCreativeBoardService(newServiceTestDB(t))
	board := newBoard(t, svc)
	boardID, _ := board["id"].(string)
	snapshot := map[string]any{
		"name": "Bad",
		"items": []any{
			map[string]any{"id": "i1", "item_type": "character", "resource_type": "character", "resource_id": "A"},
			map[string]any{"id": "i2", "item_type": "scene", "resource_type": "scene", "resource_id": "B"},
		},
		"edges": []any{
			map[string]any{"id": "e1", "source_item_id": "i1", "target_item_id": "i2", "relation": "nope"},
		},
	}
	expected := int64(1)
	_, err := svc.ReplaceSnapshot(context.Background(), boardID, repository.DefaultUserID, snapshot, &expected)
	var validation *CreativeBoardValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("want validation error, got %v", err)
	}
}

func TestCreativeBoardDelete(t *testing.T) {
	svc := NewCreativeBoardService(newServiceTestDB(t))
	ctx := context.Background()
	user := repository.DefaultUserID
	board := newBoard(t, svc)
	boardID, _ := board["id"].(string)

	result, err := svc.DeleteBoard(ctx, boardID, user, nil)
	if err != nil {
		t.Fatalf("delete board: %v", err)
	}
	if result["deleted"] != true {
		t.Fatalf("delete result = %v", result)
	}
	var notFound *CreativeBoardNotFoundError
	if _, err := svc.GetBoard(ctx, boardID, user); !errors.As(err, &notFound) {
		t.Fatalf("want not found after delete, got %v", err)
	}
}
