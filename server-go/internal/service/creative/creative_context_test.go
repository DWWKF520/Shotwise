package creative

import "github.com/shotwise/shotwise/internal/service"

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/shotwise/shotwise/internal/domain/creation"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/repository"
	"gorm.io/gorm"
)

// newCreativeContextEnv 构造真实 DB + 项目目录的创建上下文测试环境。
func newCreativeContextEnv(t *testing.T) (*CreativeContextService, *CreativeBoardService, *gorm.DB) {
	t.Helper()
	gdb := newServiceTestDB(t)
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "Project One", project.ContentDrama, project.GenerationStoryboard, ""); err != nil {
		t.Fatalf("create project: %v", err)
	}
	boards := NewCreativeBoardService(gdb)
	return NewCreativeContextService(gdb, store), boards, gdb
}

func insertCreativeContextPlan(t *testing.T, gdb *gorm.DB, id, status, skillID string, estimatedCost float64) {
	t.Helper()
	plan := repository.CreationPlan{
		ID: id, UserID: repository.DefaultUserID, ProjectID: "p1",
		SkillID: skillID, Status: status, EstimatedCost: estimatedCost,
		Fingerprint: "fp-" + id,
	}
	if err := gdb.Create(&plan).Error; err != nil {
		t.Fatalf("insert plan: %v", err)
	}
}

func creativeContextResolutionCode(t *testing.T, err error) string {
	t.Helper()
	var resolutionErr *creation.CreativeContextResolutionError
	if !errors.As(err, &resolutionErr) {
		t.Fatalf("want resolution error, got %v", err)
	}
	return resolutionErr.Code
}

func TestCreativeContextResolveSuccess(t *testing.T) {
	svc, boards, gdb := newCreativeContextEnv(t)
	ctx := context.Background()
	user := repository.DefaultUserID

	board, err := boards.CreateBoard(ctx, user, "p1", "Board", nil, nil)
	if err != nil {
		t.Fatalf("create board: %v", err)
	}
	boardID, _ := board["id"].(string)
	if _, err := boards.AddItem(ctx, boardID, user, "document", "document", "doc-1",
		nil, nil, nil, map[string]any{"name": "原始文稿"}, nil); err != nil {
		t.Fatalf("add item: %v", err)
	}
	insertCreativeContextPlan(t, gdb, "plan-1", "previewed", "novel-to-drama", 0)

	out, err := svc.ResolveCreationContext(ctx, "p1", user, CreativeContextRequest{
		CreativeBoardID:   boardID,
		CreationPlanID:    "plan-1",
		SelectedResources: []creation.SelectedResource{{ID: "doc-1", ResourceType: "document"}},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if out["current_skill_id"] != "novel-to-drama" {
		t.Fatalf("current_skill_id = %v", out["current_skill_id"])
	}
	if out["generation_mode"] != "storyboard" {
		t.Fatalf("generation_mode = %v", out["generation_mode"])
	}
	gates, _ := out["gates"].(map[string]any)
	if gates["previewed"] != true || gates["confirmed"] != true {
		t.Fatalf("gates = %v", gates)
	}
	selected, _ := out["selected_resources"].([]any)
	if len(selected) != 1 || selected[0].(map[string]any)["id"] != "doc-1" {
		t.Fatalf("selected_resources = %v", selected)
	}
}

func TestCreativeContextResolveBoardAndPlanErrors(t *testing.T) {
	svc, boards, gdb := newCreativeContextEnv(t)
	ctx := context.Background()
	user := repository.DefaultUserID

	board, err := boards.CreateBoard(ctx, user, "p1", "Board", nil, nil)
	if err != nil {
		t.Fatalf("create board: %v", err)
	}
	boardID, _ := board["id"].(string)
	if _, err := boards.AddItem(ctx, boardID, user, "document", "document", "doc-1", nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("add item: %v", err)
	}
	insertCreativeContextPlan(t, gdb, "plan-1", "previewed", "novel-to-drama", 0)

	_, err = svc.ResolveCreationContext(ctx, "p1", user, CreativeContextRequest{
		CreativeBoardID:   "board-404",
		CreationPlanID:    "plan-1",
		SelectedResources: []creation.SelectedResource{{ID: "doc-1", ResourceType: "document"}},
	})
	if code := creativeContextResolutionCode(t, err); code != "creative_board_not_found" {
		t.Fatalf("code = %s", code)
	}

	_, err = svc.ResolveCreationContext(ctx, "p1", user, CreativeContextRequest{
		CreativeBoardID:   boardID,
		CreationPlanID:    "plan-404",
		SelectedResources: []creation.SelectedResource{{ID: "doc-1", ResourceType: "document"}},
	})
	if code := creativeContextResolutionCode(t, err); code != "creation_plan_not_found" {
		t.Fatalf("code = %s", code)
	}

	// 他人 board / plan 一律按 not_found 处理（fingerprint 全局唯一，需区分）。
	other := repository.CreationPlan{ID: "plan-other", UserID: "user-x", ProjectID: "p1", Status: "previewed", Fingerprint: "fp-other"}
	if err := gdb.Create(&other).Error; err != nil {
		t.Fatalf("insert other plan: %v", err)
	}
	_, err = svc.ResolveCreationContext(ctx, "p1", user, CreativeContextRequest{
		CreativeBoardID:   boardID,
		CreationPlanID:    "plan-other",
		SelectedResources: []creation.SelectedResource{{ID: "doc-1", ResourceType: "document"}},
	})
	if code := creativeContextResolutionCode(t, err); code != "creation_plan_not_found" {
		t.Fatalf("code = %s", code)
	}
}

func TestCreativeContextResolveRunPayloadFromRevision(t *testing.T) {
	svc, boards, gdb := newCreativeContextEnv(t)
	ctx := context.Background()
	user := repository.DefaultUserID

	board, err := boards.CreateBoard(ctx, user, "p1", "Board", nil, nil)
	if err != nil {
		t.Fatalf("create board: %v", err)
	}
	boardID, _ := board["id"].(string)
	if _, err := boards.AddItem(ctx, boardID, user, "document", "document", "doc-1", nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("add item: %v", err)
	}
	insertCreativeContextPlan(t, gdb, "plan-1", "previewed", "novel-to-drama", 0)

	definition := repository.WorkflowDefinition{ID: "wf-1", UserID: user, ProjectID: "p1", Name: "wf"}
	if err := gdb.Create(&definition).Error; err != nil {
		t.Fatalf("insert definition: %v", err)
	}
	revision := repository.WorkflowRevision{
		ID: "rev-1", DefinitionID: "wf-1", RevisionNo: 1, Status: "active",
		GraphHash: "graph-hash-1", ExecutionHash: "exec-hash-1", ContentMode: "drama", GenerationMode: "storyboard",
	}
	if err := gdb.Create(&revision).Error; err != nil {
		t.Fatalf("insert revision: %v", err)
	}
	run := repository.WorkflowRun{
		ID: "run-1", UserID: user, WorkflowRevisionID: "rev-1", ProjectID: "p1", Status: "running",
	}
	if err := gdb.Create(&run).Error; err != nil {
		t.Fatalf("insert run: %v", err)
	}

	out, err := svc.ResolveCreationContext(ctx, "p1", user, CreativeContextRequest{
		CreativeBoardID:   boardID,
		CreationPlanID:    "plan-1",
		WorkflowRunID:     strPtrValue("run-1"),
		SelectedResources: []creation.SelectedResource{{ID: "doc-1", ResourceType: "document"}},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	gates, _ := out["gates"].(map[string]any)
	runInfo, _ := gates["workflow_run"].(map[string]any)
	if runInfo["id"] != "run-1" || runInfo["status"] != "running" {
		t.Fatalf("workflow_run = %v", runInfo)
	}

	// revision 与项目 generation_mode 不一致 → generation_mode_changed。
	if err := gdb.Model(&repository.WorkflowRevision{}).Where("id = ?", "rev-1").
		Update("generation_mode", "reference_video").Error; err != nil {
		t.Fatalf("update revision: %v", err)
	}
	_, err = svc.ResolveCreationContext(ctx, "p1", user, CreativeContextRequest{
		CreativeBoardID:   boardID,
		CreationPlanID:    "plan-1",
		WorkflowRunID:     strPtrValue("run-1"),
		SelectedResources: []creation.SelectedResource{{ID: "doc-1", ResourceType: "document"}},
	})
	if code := creativeContextResolutionCode(t, err); code != "generation_mode_changed" {
		t.Fatalf("code = %s", code)
	}
}

func TestCreativeContextPlanPreviewMerge(t *testing.T) {
	svc, boards, gdb := newCreativeContextEnv(t)
	ctx := context.Background()
	user := repository.DefaultUserID

	board, err := boards.CreateBoard(ctx, user, "p1", "Board", nil, nil)
	if err != nil {
		t.Fatalf("create board: %v", err)
	}
	boardID, _ := board["id"].(string)
	if _, err := boards.AddItem(ctx, boardID, user, "document", "document", "doc-1", nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("add item: %v", err)
	}
	// draft 状态本会触发 creation_plan_preview_required，但 preview JSON 里的
	// previewed=true 覆盖后放行（对应 plan_payload.update(preview)）。
	preview, _ := json.Marshal(map[string]any{"previewed": true})
	plan := repository.CreationPlan{
		ID: "plan-1", UserID: user, ProjectID: "p1", SkillID: "novel-to-drama",
		Status: "draft", PreviewJSON: string(preview), Fingerprint: "fp-plan-1",
	}
	if err := gdb.Create(&plan).Error; err != nil {
		t.Fatalf("insert plan: %v", err)
	}

	out, err := svc.ResolveCreationContext(ctx, "p1", user, CreativeContextRequest{
		CreativeBoardID:   boardID,
		CreationPlanID:    "plan-1",
		SelectedResources: []creation.SelectedResource{{ID: "doc-1", ResourceType: "document"}},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if out["current_skill_id"] != "novel-to-drama" {
		t.Fatalf("current_skill_id = %v", out["current_skill_id"])
	}
}

func TestCreativeContextResolveReferencesFlow(t *testing.T) {
	svc, boards, _ := newCreativeContextEnv(t)
	ctx := context.Background()
	user := repository.DefaultUserID

	board, err := boards.CreateBoard(ctx, user, "p1", "Board", nil, nil)
	if err != nil {
		t.Fatalf("create board: %v", err)
	}
	boardID, _ := board["id"].(string)
	if _, err := boards.AddItem(ctx, boardID, user, "document", "document", "doc-1",
		nil, nil, nil, map[string]any{"name": "原始文稿"}, nil); err != nil {
		t.Fatalf("add item: %v", err)
	}

	out, err := svc.ResolveContextReferences(ctx, "p1", user, CreativeContextRequest{
		CreativeBoardID:   boardID,
		SelectedResources: []creation.SelectedResource{{ID: "doc-1", ResourceType: "document"}},
		ContextReferences: []creation.ContextReference{{Text: "原始文稿"}},
	})
	if err != nil {
		t.Fatalf("resolve references: %v", err)
	}
	resolutions, _ := out["references"].([]any)
	if len(resolutions) != 1 {
		t.Fatalf("references = %v", resolutions)
	}
	resolution := resolutions[0].(map[string]any)
	if resolution["id"] != "doc-1" || resolution["resource_type"] != "document" {
		t.Fatalf("resolution = %v", resolution)
	}
	selected, _ := out["selected_resources"].([]any)
	if len(selected) != 1 {
		t.Fatalf("selected_resources = %v", selected)
	}
}

func TestCreativeContextResolveReferencesBoardMismatch(t *testing.T) {
	svc, boards, _ := newCreativeContextEnv(t)
	ctx := context.Background()
	// 在另一个项目下建板。
	board, err := boards.CreateBoard(ctx, repository.DefaultUserID, "p2", "Other", nil, nil)
	if err != nil {
		t.Fatalf("create board: %v", err)
	}
	boardID, _ := board["id"].(string)

	_, err = svc.ResolveContextReferences(ctx, "p1", repository.DefaultUserID, CreativeContextRequest{
		CreativeBoardID: boardID,
		ContextReferences: []creation.ContextReference{
			{Text: "任意"},
		},
	})
	if code := creativeContextResolutionCode(t, err); code != "creative_board_project_mismatch" {
		t.Fatalf("code = %s", code)
	}
}

func strPtrValue(s string) *string { return &s }
