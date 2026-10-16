package domain

import (
	"reflect"
	"testing"
)

func strPtr(s string) *string { return &s }

func boolPtr(b bool) *bool { return &b }

func resolutionCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	resolutionErr, ok := err.(*CreativeContextResolutionError)
	if !ok {
		t.Fatalf("expected *CreativeContextResolutionError, got %T", err)
	}
	return resolutionErr.Code
}

func resolutionCodeOf(t *testing.T, result map[string]any, err error) string {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got payload %v", result)
	}
	return resolutionCode(t, err)
}

func baseCreativeProject() map[string]any {
	return map[string]any{
		"id":              "proj-1",
		"content_mode":    "drama",
		"generation_mode": "storyboard",
		"grid_storyboard": false,
		"characters":      []any{map[string]any{"id": "char-1", "name": "林晚", "gender": "female"}},
		"scenes":          []any{map[string]any{"id": "scene-1", "name": "码头"}},
		"episodes": []any{
			map[string]any{
				"id": "ep-1", "name": "第一集",
				"shots": []any{map[string]any{"id": "shot-1", "name": "开场"}, map[string]any{"id": "shot-2"}},
			},
		},
	}
}

func validCreationPlan() map[string]any {
	return map[string]any{
		"id":         "plan-1",
		"project_id": "proj-1",
		"status":     "previewed",
		"skill_id":   "novel-to-drama",
	}
}

func documentSelection() []SelectedResource {
	return []SelectedResource{{ID: "doc-1", ResourceType: "document"}}
}

func TestResolveContextReferencesRequiresProjectID(t *testing.T) {
	_out, _err := ResolveContextReferences(ResolveContextReferencesParams{
		ProjectID: "  ",
		Project:   map[string]any{},
	})
	errCode := resolutionCodeOf(t, _out, _err)
	if errCode != "project_id_required" {
		t.Fatalf("code = %s", errCode)
	}
}

func TestResolveContextReferencesProjectMismatch(t *testing.T) {
	project := baseCreativeProject()
	project["id"] = "other"
	_out, _err := ResolveContextReferences(ResolveContextReferencesParams{
		ProjectID:  "proj-1",
		Project:    project,
		References: []ContextReference{{Text: "林晚"}},
	})
	errCode := resolutionCodeOf(t, _out, _err)
	if errCode != "project_context_mismatch" {
		t.Fatalf("code = %s", errCode)
	}
}

func TestResolveContextReferencesEmptyText(t *testing.T) {
	_out, _err := ResolveContextReferences(ResolveContextReferencesParams{
		ProjectID:  "proj-1",
		Project:    baseCreativeProject(),
		References: []ContextReference{{Text: "   "}},
	})
	errCode := resolutionCodeOf(t, _out, _err)
	if errCode != "context_reference_required" {
		t.Fatalf("code = %s", errCode)
	}
}

func TestResolveContextReferencesUnresolved(t *testing.T) {
	_, err := ResolveContextReferences(ResolveContextReferencesParams{
		ProjectID:  "proj-1",
		Project:    baseCreativeProject(),
		References: []ContextReference{{Text: "不存在的角色"}},
	})
	resolutionErr, ok := err.(*CreativeContextResolutionError)
	if !ok {
		t.Fatalf("expected resolution error, got %v", err)
	}
	if resolutionErr.Code != "context_reference_unresolved" {
		t.Fatalf("code = %s", resolutionErr.Code)
	}
	if resolutionErr.Details["reference"] != "不存在的角色" {
		t.Fatalf("details = %v", resolutionErr.Details)
	}
}

func TestResolveContextReferencesExactName(t *testing.T) {
	out, err := ResolveContextReferences(ResolveContextReferencesParams{
		ProjectID:  "proj-1",
		Project:    baseCreativeProject(),
		References: []ContextReference{{Text: "林晚"}},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	resolutions, _ := out["references"].([]any)
	if len(resolutions) != 1 {
		t.Fatalf("resolutions = %v", resolutions)
	}
	resolution := resolutions[0].(map[string]any)
	if resolution["id"] != "char-1" || resolution["resource_type"] != "character" {
		t.Fatalf("resolution = %v", resolution)
	}
	selected := out["selected_resources"].([]any)
	if len(selected) != 1 || selected[0].(map[string]any)["id"] != "char-1" {
		t.Fatalf("selected = %v", selected)
	}
	if out["disambiguated"] != true {
		t.Fatalf("disambiguated = %v", out["disambiguated"])
	}
}

func TestResolveContextReferencesAmbiguous(t *testing.T) {
	project := baseCreativeProject()
	project["characters"] = []any{
		map[string]any{"id": "char-1", "name": "林晚"},
		map[string]any{"id": "char-2", "name": "林晚"},
	}
	_, err := ResolveContextReferences(ResolveContextReferencesParams{
		ProjectID:  "proj-1",
		Project:    project,
		References: []ContextReference{{Text: "林晚"}},
	})
	resolutionErr, ok := err.(*CreativeContextResolutionError)
	if !ok {
		t.Fatalf("expected resolution error, got %v", err)
	}
	if resolutionErr.Code != "ambiguous_context_reference" {
		t.Fatalf("code = %s", resolutionErr.Code)
	}
	candidates, _ := resolutionErr.Details["candidates"].([]any)
	if len(candidates) != 2 {
		t.Fatalf("candidates = %v", candidates)
	}
}

func TestResolveContextReferencesExpectedTypeFilters(t *testing.T) {
	project := baseCreativeProject()
	project["characters"] = []any{map[string]any{"id": "char-1", "name": "码头"}}
	boardItems := []any{
		map[string]any{"resource_id": "scene-1", "resource_type": "scene", "name": "码头"},
	}
	out, err := ResolveContextReferences(ResolveContextReferencesParams{
		ProjectID:  "proj-1",
		Project:    project,
		References: []ContextReference{{Text: "码头", ExpectedType: strPtr("scene")}},
		BoardItems: boardItems,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	resolution := out["references"].([]any)[0].(map[string]any)
	if resolution["resource_type"] != "scene" || resolution["id"] != "scene-1" {
		t.Fatalf("resolution = %v", resolution)
	}
}

func TestResolveContextReferencesCurrentShotUsesShotID(t *testing.T) {
	out, err := ResolveContextReferences(ResolveContextReferencesParams{
		ProjectID:  "proj-1",
		Project:    baseCreativeProject(),
		References: []ContextReference{{Text: "当前镜头"}},
		ShotID:     strPtr("shot-2"),
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out["shot_id"] != "shot-2" {
		t.Fatalf("shot_id = %v", out["shot_id"])
	}
	resolution := out["references"].([]any)[0].(map[string]any)
	if resolution["id"] != "shot-2" || resolution["resource_type"] != "shot" {
		t.Fatalf("resolution = %v", resolution)
	}
}

func TestResolveContextReferencesShotBackfillsEpisode(t *testing.T) {
	out, err := ResolveContextReferences(ResolveContextReferencesParams{
		ProjectID:  "proj-1",
		Project:    baseCreativeProject(),
		References: []ContextReference{{Text: "开场"}},
		BoardItems: []any{
			map[string]any{"resource_id": "shot-1", "resource_type": "shot", "name": "开场"},
		},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out["shot_id"] != "shot-1" {
		t.Fatalf("shot_id = %v", out["shot_id"])
	}
	if out["episode_id"] != "ep-1" {
		t.Fatalf("episode_id = %v", out["episode_id"])
	}
}

func TestResolveContextReferencesShotEpisodeConflict(t *testing.T) {
	_out, _err := ResolveContextReferences(ResolveContextReferencesParams{
		ProjectID:  "proj-1",
		Project:    baseCreativeProject(),
		References: []ContextReference{{Text: "开场"}},
		EpisodeID:  strPtr("ep-9"),
		BoardItems: []any{
			map[string]any{"resource_id": "shot-1", "resource_type": "shot", "name": "开场", "episode_id": "ep-1"},
		},
	})
	errCode := resolutionCodeOf(t, _out, _err)
	if errCode != "context_reference_conflict" {
		t.Fatalf("code = %s", errCode)
	}
}

func TestResolveContextReferencesFemalePronoun(t *testing.T) {
	project := baseCreativeProject()
	project["characters"] = []any{
		map[string]any{"id": "char-1", "name": "林晚", "gender": "female"},
		map[string]any{"id": "char-2", "name": "陈默", "gender": "male"},
	}
	out, err := ResolveContextReferences(ResolveContextReferencesParams{
		ProjectID:  "proj-1",
		Project:    project,
		References: []ContextReference{{Text: "她"}},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	resolution := out["references"].([]any)[0].(map[string]any)
	if resolution["id"] != "char-1" {
		t.Fatalf("resolution = %v", resolution)
	}
}

func TestResolveContextReferencesFemalePronounAmbiguousWithoutGender(t *testing.T) {
	project := baseCreativeProject()
	project["characters"] = []any{
		map[string]any{"id": "char-1", "name": "林晚"},
		map[string]any{"id": "char-2", "name": "陈默"},
	}
	_out, _err := ResolveContextReferences(ResolveContextReferencesParams{
		ProjectID:  "proj-1",
		Project:    project,
		References: []ContextReference{{Text: "她"}},
	})
	errCode := resolutionCodeOf(t, _out, _err)
	if errCode != "ambiguous_context_reference" {
		t.Fatalf("code = %s", errCode)
	}
}

func TestResolveContextReferencesBoardItemLabel(t *testing.T) {
	boardItems := []any{
		map[string]any{"id": "asset-9", "resource_type": "image", "name": "黄昏海面"},
	}
	out, err := ResolveContextReferences(ResolveContextReferencesParams{
		ProjectID:  "proj-1",
		Project:    baseCreativeProject(),
		References: []ContextReference{{Text: "黄昏海面"}},
		BoardItems: boardItems,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	resolution := out["references"].([]any)[0].(map[string]any)
	if resolution["id"] != "asset-9" || resolution["resource_type"] != "image" {
		t.Fatalf("resolution = %v", resolution)
	}
	ids, _ := out["selected_media_asset_ids"].([]any)
	if len(ids) != 1 || ids[0] != "asset-9" {
		t.Fatalf("selected_media_asset_ids = %v", ids)
	}
}

func TestResolveContextReferencesSelectedResourceAlias(t *testing.T) {
	out, err := ResolveContextReferences(ResolveContextReferencesParams{
		ProjectID:  "proj-1",
		Project:    baseCreativeProject(),
		References: []ContextReference{{Text: "这个视频"}},
		SelectedResources: []SelectedResource{
			{ID: "vid-1", ResourceType: "video"},
			{ID: "vid-2", ResourceType: "video"},
		},
	})
	errCode := resolutionCode(t, err)
	if errCode != "ambiguous_context_reference" {
		t.Fatalf("code = %s (%v)", errCode, out)
	}
}

func TestResolveCreationContextMissingProjectIDReturnsErrorPayload(t *testing.T) {
	out, err := ResolveCreationContext(ResolveCreationContextParams{ProjectID: " "})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out["status"] != "error" {
		t.Fatalf("status = %v", out["status"])
	}
	errorPayload := out["error"].(map[string]any)
	if errorPayload["code"] != "missing_project_id" {
		t.Fatalf("error = %v", errorPayload)
	}
}

func TestResolveCreationContextWorkflowRunRequiresPlan(t *testing.T) {
	_out, _err := ResolveCreationContext(ResolveCreationContextParams{
		ProjectID:         "proj-1",
		Project:           baseCreativeProject(),
		SelectedResources: documentSelection(),
		WorkflowRunID:     strPtr("run-1"),
	})
	errCode := resolutionCodeOf(t, _out, _err)
	if errCode != "workflow_run_requires_creation_plan" {
		t.Fatalf("code = %s", errCode)
	}
}

func TestResolveCreationContextResourceSelectionRequired(t *testing.T) {
	_out, _err := ResolveCreationContext(ResolveCreationContextParams{
		ProjectID: "proj-1",
		Project:   baseCreativeProject(),
	})
	errCode := resolutionCodeOf(t, _out, _err)
	if errCode != "resource_selection_required" {
		t.Fatalf("code = %s", errCode)
	}
}

func TestResolveCreationContextResourceValidation(t *testing.T) {
	cases := []struct {
		name     string
		resource SelectedResource
		want     string
	}{
		{"empty_id", SelectedResource{ID: " ", ResourceType: "document"}, "resource_id_required"},
		{"dup", SelectedResource{ID: "doc-1", ResourceType: "document"}, "ambiguous_duplicate_resource"},
		{"unknown_type", SelectedResource{ID: "doc-2", ResourceType: "hologram"}, "unknown_resource_type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resources := append(documentSelection(), tc.resource)
			if tc.name == "empty_id" {
				resources = []SelectedResource{tc.resource}
			}
			_out, _err := ResolveCreationContext(ResolveCreationContextParams{
				ProjectID:         "proj-1",
				Project:           baseCreativeProject(),
				SelectedResources: resources,
			})
			errCode := resolutionCodeOf(t, _out, _err)
			if errCode != tc.want {
				t.Fatalf("code = %s, want %s", errCode, tc.want)
			}
		})
	}
}

func TestResolveCreationContextEpisodeValidation(t *testing.T) {
	_out, _err := ResolveCreationContext(ResolveCreationContextParams{
		ProjectID:         "proj-1",
		Project:           baseCreativeProject(),
		SelectedResources: documentSelection(),
		EpisodeID:         strPtr("ep-404"),
	})
	errCode := resolutionCodeOf(t, _out, _err)
	if errCode != "episode_not_found" {
		t.Fatalf("code = %s", errCode)
	}

	_out, _err = ResolveCreationContext(ResolveCreationContextParams{
		ProjectID:         "proj-1",
		Project:           baseCreativeProject(),
		SelectedResources: documentSelection(),
		ShotID:            strPtr("shot-404"),
		EpisodeID:         strPtr("ep-1"),
	})
	errCode = resolutionCodeOf(t, _out, _err)
	if errCode != "shot_not_found" {
		t.Fatalf("code = %s", errCode)
	}

	_out, _err = ResolveCreationContext(ResolveCreationContextParams{
		ProjectID:         "proj-1",
		Project:           baseCreativeProject(),
		SelectedResources: documentSelection(),
		ShotID:            strPtr("shot-1"),
	})
	errCode = resolutionCodeOf(t, _out, _err)
	if errCode != "episode_id_required_for_shot" {
		t.Fatalf("code = %s", errCode)
	}
}

func TestResolveCreationContextBoardValidation(t *testing.T) {
	_out, _err := ResolveCreationContext(ResolveCreationContextParams{
		ProjectID: "proj-1",
		Project:   baseCreativeProject(),
		SelectedResources: []SelectedResource{
			{ID: "doc-8", ResourceType: "document"},
			{ID: "doc-9", ResourceType: "document"},
		},
		CreativeBoardID: "board-1",
		Board:           map[string]any{"id": "board-1", "project_id": "proj-1"},
		BoardItems: []any{
			map[string]any{"id": "item-1", "resource_id": "doc-9"},
			map[string]any{"id": "item-2", "resource_id": "doc-9"},
		},
	})
	errCode := resolutionCodeOf(t, _out, _err)
	if errCode != "resource_not_on_creative_board" {
		t.Fatalf("code = %s", errCode)
	}

	_out, _err = ResolveCreationContext(ResolveCreationContextParams{
		ProjectID:         "proj-1",
		Project:           baseCreativeProject(),
		SelectedResources: []SelectedResource{{ID: "doc-9", ResourceType: "document"}},
		CreativeBoardID:   "board-1",
		Board:             map[string]any{"id": "board-1", "project_id": "proj-1"},
		BoardItems: []any{
			map[string]any{"id": "item-1", "resource_id": "doc-9"},
			map[string]any{"id": "item-2", "resource_id": "doc-9"},
		},
	})
	errCode = resolutionCodeOf(t, _out, _err)
	if errCode != "ambiguous_board_resource" {
		t.Fatalf("code = %s", errCode)
	}
}

func TestResolveCreationContextBoardIDRequired(t *testing.T) {
	_out, _err := ResolveCreationContext(ResolveCreationContextParams{
		ProjectID:         "proj-1",
		Project:           baseCreativeProject(),
		SelectedResources: documentSelection(),
	})
	errCode := resolutionCodeOf(t, _out, _err)
	if errCode != "creative_board_id_required" {
		t.Fatalf("code = %s", errCode)
	}
}

func TestResolveCreationContextPlanRequired(t *testing.T) {
	_out, _err := ResolveCreationContext(ResolveCreationContextParams{
		ProjectID:         "proj-1",
		Project:           baseCreativeProject(),
		SelectedResources: documentSelection(),
		CreativeBoardID:   "board-1",
	})
	errCode := resolutionCodeOf(t, _out, _err)
	if errCode != "creation_plan_required" {
		t.Fatalf("code = %s", errCode)
	}
}

func TestResolveCreationContextMediaAssetValidation(t *testing.T) {
	_out, _err := ResolveCreationContext(ResolveCreationContextParams{
		ProjectID:         "proj-1",
		Project:           baseCreativeProject(),
		SelectedResources: []SelectedResource{{ID: "asset-404", ResourceType: "image"}},
		CreativeBoardID:   "board-1",
		CreationPlanID:    "plan-1",
		MediaAssets:       []any{map[string]any{"id": "asset-1", "kind": "image"}},
	})
	errCode := resolutionCodeOf(t, _out, _err)
	if errCode != "media_asset_not_found" {
		t.Fatalf("code = %s", errCode)
	}

	_out, _err = ResolveCreationContext(ResolveCreationContextParams{
		ProjectID:         "proj-1",
		Project:           baseCreativeProject(),
		SelectedResources: []SelectedResource{{ID: "asset-1", ResourceType: "video"}},
		CreativeBoardID:   "board-1",
		CreationPlanID:    "plan-1",
		MediaAssets:       []any{map[string]any{"id": "asset-1", "kind": "image"}},
	})
	errCode = resolutionCodeOf(t, _out, _err)
	if errCode != "media_asset_kind_mismatch" {
		t.Fatalf("code = %s", errCode)
	}
}

func TestResolveCreationContextGateErrors(t *testing.T) {
	base := func() ResolveCreationContextParams {
		return ResolveCreationContextParams{
			ProjectID:         "proj-1",
			Project:           baseCreativeProject(),
			SelectedResources: documentSelection(),
			CreativeBoardID:   "board-1",
			CreationPlanID:    "plan-1",
		}
	}
	cases := []struct {
		name   string
		mutate func(p *ResolveCreationContextParams)
		want   string
	}{
		{"bypass", func(p *ResolveCreationContextParams) { p.AllowGateBypass = true }, "gate_bypass_forbidden"},
		{"details_required", func(p *ResolveCreationContextParams) { p.CreationPlan = nil }, "creation_plan_details_required"},
		{"context_mismatch", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["id"] = "plan-2"
			p.CreationPlan = plan
		}, "creation_plan_context_mismatch"},
		{"project_mismatch", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["project_id"] = "proj-2"
			p.CreationPlan = plan
		}, "creation_plan_project_mismatch"},
		{"unavailable_cancelled", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["status"] = "cancelled"
			p.CreationPlan = plan
		}, "creation_plan_unavailable"},
		{"preview_required", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["status"] = "draft"
			p.CreationPlan = plan
		}, "creation_plan_preview_required"},
		{"mode_override_forbidden", func(p *ResolveCreationContextParams) {
			p.CreationPlan = validCreationPlan()
			p.GenerationMode = strPtr("storyboard")
		}, "generation_mode_override_forbidden"},
		{"snapshot_project_mismatch", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["project_snapshot"] = `{"project_id": "proj-2"}`
			p.CreationPlan = plan
		}, "creation_plan_project_mismatch"},
		{"snapshot_changed", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["project_snapshot"] = `{"content_mode": "narration"}`
			p.CreationPlan = plan
		}, "project_snapshot_changed"},
		{"snapshot_generation_mode_changed", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["project_snapshot"] = `{"generation_mode": "reference_video"}`
			p.CreationPlan = plan
		}, "generation_mode_changed"},
		{"skill_mismatch", func(p *ResolveCreationContextParams) {
			p.CreationPlan = validCreationPlan()
			p.CurrentSkillID = strPtr("redraw-storyboard")
		}, "creation_plan_skill_mismatch"},
		{"skill_required", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			delete(plan, "skill_id")
			p.CreationPlan = plan
		}, "creation_plan_skill_required"},
		{"unknown_skill", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["skill_id"] = "no-such-skill"
			p.CreationPlan = plan
		}, "unknown_creation_skill"},
		{"skill_incompatible", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["skill_id"] = "ad-brief-to-video"
			p.CreationPlan = plan
		}, "creation_skill_incompatible"},
		{"persisted_incompatible_report", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["compatibility_report"] = map[string]any{"compatible": false, "reasons": []string{"stale"}}
			p.CreationPlan = plan
		}, "creation_skill_incompatible"},
		{"resources_mismatch", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["resource_ids"] = []any{map[string]any{"id": "doc-999"}}
			p.CreationPlan = plan
		}, "creation_plan_resources_mismatch"},
		{"parameters_override_forbidden", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["parameters"] = map[string]any{"content_mode_override": "ad"}
			p.CreationPlan = plan
		}, "gate_bypass_forbidden"},
		{"confirmation_required", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["estimated_cost"] = 12.5
			p.CreationPlan = plan
		}, "confirmation_required"},
		{"quality_gate_failed", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["quality_gate"] = false
			p.CreationPlan = plan
		}, "quality_gate_failed"},
		{"quality_gate_required", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["quality_gate"] = map[string]any{"required": true}
			p.CreationPlan = plan
		}, "quality_gate_required"},
		{"quality_gate_pending_ok", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["quality_gate"] = map[string]any{"required": false}
			p.CreationPlan = plan
		}, ""},
		{"approval_required_by_status", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["approval"] = `"pending"`
			p.CreationPlan = plan
		}, "approval_required"},
		{"approval_required_by_review_points", func(p *ResolveCreationContextParams) {
			plan := validCreationPlan()
			plan["review_points"] = []any{"check-1"}
			p.CreationPlan = plan
		}, "approval_required"},
		{"workflow_run_details_required", func(p *ResolveCreationContextParams) {
			p.CreationPlan = validCreationPlan()
			p.WorkflowRunID = strPtr("run-1")
		}, "workflow_run_details_required"},
		{"workflow_run_context_mismatch", func(p *ResolveCreationContextParams) {
			p.CreationPlan = validCreationPlan()
			p.WorkflowRunID = strPtr("run-1")
			p.WorkflowRun = map[string]any{"id": "run-2", "project_id": "proj-1"}
		}, "workflow_run_context_mismatch"},
		{"workflow_run_project_mismatch", func(p *ResolveCreationContextParams) {
			p.CreationPlan = validCreationPlan()
			p.WorkflowRunID = strPtr("run-1")
			p.WorkflowRun = map[string]any{"id": "run-1", "project_id": "proj-2"}
		}, "workflow_run_project_mismatch"},
		{"workflow_run_quality_failed", func(p *ResolveCreationContextParams) {
			p.CreationPlan = validCreationPlan()
			p.WorkflowRunID = strPtr("run-1")
			p.WorkflowRun = map[string]any{"id": "run-1", "project_id": "proj-1", "error_code": "quality_gate_failed"}
		}, "quality_gate_failed"},
		{"workflow_run_waiting_review", func(p *ResolveCreationContextParams) {
			p.CreationPlan = validCreationPlan()
			p.WorkflowRunID = strPtr("run-1")
			p.WorkflowRun = map[string]any{"id": "run-1", "project_id": "proj-1", "status": "waiting_review"}
		}, "approval_required"},
		{"workflow_run_mode_changed", func(p *ResolveCreationContextParams) {
			p.CreationPlan = validCreationPlan()
			p.WorkflowRunID = strPtr("run-1")
			p.WorkflowRun = map[string]any{"id": "run-1", "project_id": "proj-1", "status": "running", "generation_mode": "reference_video"}
		}, "generation_mode_changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := base()
			params.CreationPlan = validCreationPlan()
			tc.mutate(&params)
			out, err := ResolveCreationContext(params)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if out["current_skill_id"] == nil {
					t.Fatal("expected success payload")
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error %s, got payload %v", tc.want, out)
			}
			errCode := resolutionCode(t, err)
			if errCode != tc.want {
				t.Fatalf("code = %s, want %s", errCode, tc.want)
			}
		})
	}
}

func TestResolveCreationContextSuccess(t *testing.T) {
	plan := validCreationPlan()
	plan["estimated_cost"] = 5.0
	plan["confirmed"] = true
	out, err := ResolveCreationContext(ResolveCreationContextParams{
		ProjectID:         "proj-1",
		Project:           baseCreativeProject(),
		SelectedResources: documentSelection(),
		CreativeBoardID:   "board-1",
		Board:             map[string]any{"id": "board-1", "project_id": "proj-1"},
		BoardItems:        []any{map[string]any{"id": "item-1", "resource_id": "doc-1"}},
		CreationPlanID:    "plan-1",
		CreationPlan:      plan,
		WorkflowRunID:     strPtr("run-1"),
		WorkflowRun:       map[string]any{"id": "run-1", "project_id": "proj-1", "status": "running"},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out["current_skill_id"] != "novel-to-drama" {
		t.Fatalf("current_skill_id = %v", out["current_skill_id"])
	}
	if out["creative_board_id"] != "board-1" {
		t.Fatalf("creative_board_id = %v", out["creative_board_id"])
	}
	if out["workflow_run_id"] != "run-1" {
		t.Fatalf("workflow_run_id = %v", out["workflow_run_id"])
	}
	if out["generation_mode"] != "storyboard" {
		t.Fatalf("generation_mode = %v", out["generation_mode"])
	}
	gates := out["gates"].(map[string]any)
	if gates["confirmed"] != true || gates["requires_confirmation"] != true {
		t.Fatalf("gates = %v", gates)
	}
	runInfo := gates["workflow_run"].(map[string]any)
	if runInfo["id"] != "run-1" || runInfo["status"] != "running" {
		t.Fatalf("workflow_run = %v", runInfo)
	}
	available, _ := out["available_skills"].([]any)
	if len(available) == 0 {
		t.Fatal("available_skills is empty")
	}
	found := false
	for _, raw := range available {
		if raw.(map[string]any)["id"] == "novel-to-drama" {
			found = true
		}
	}
	if !found {
		t.Fatalf("novel-to-drama not in available_skills: %v", available)
	}
	if out["requires_creation_plan_preview"] != true || out["disambiguated"] != true {
		t.Fatalf("payload flags wrong: %v", out)
	}
}

func TestResolveCreationContextContextReferencesMerge(t *testing.T) {
	plan := validCreationPlan()
	plan["resource_ids"] = []any{map[string]any{"id": "doc-1"}, map[string]any{"id": "char-1"}}
	out, err := ResolveCreationContext(ResolveCreationContextParams{
		ProjectID:         "proj-1",
		Project:           baseCreativeProject(),
		SelectedResources: documentSelection(),
		CreativeBoardID:   "board-1",
		CreationPlanID:    "plan-1",
		CreationPlan:      plan,
		ContextReferences: []ContextReference{{Text: "林晚"}},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	selected := out["selected_resources"].([]any)
	if len(selected) != 2 {
		t.Fatalf("selected_resources = %v", selected)
	}
	resolutions, _ := out["context_reference_resolutions"].([]any)
	if len(resolutions) != 1 {
		t.Fatalf("context_reference_resolutions = %v", resolutions)
	}
	if resolutions[0].(map[string]any)["id"] != "char-1" {
		t.Fatalf("resolution = %v", resolutions[0])
	}
}

func TestResolveCreationContextEpisodeFromReferenceBackfill(t *testing.T) {
	plan := validCreationPlan()
	out, err := ResolveCreationContext(ResolveCreationContextParams{
		ProjectID:         "proj-1",
		Project:           baseCreativeProject(),
		SelectedResources: documentSelection(),
		CreativeBoardID:   "board-1",
		CreationPlanID:    "plan-1",
		CreationPlan:      plan,
		BoardItems: []any{
			map[string]any{"resource_id": "shot-1", "resource_type": "shot", "name": "开场"},
		},
		ContextReferences: []ContextReference{{Text: "开场"}},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out["episode_id"] != "ep-1" || out["shot_id"] != "shot-1" {
		t.Fatalf("episode/shot = %v / %v", out["episode_id"], out["shot_id"])
	}
}

func TestGateResult(t *testing.T) {
	cases := []struct {
		name     string
		value    any
		result   *bool
		required bool
	}{
		{"bool_true", true, boolPtr(true), true},
		{"bool_false", false, boolPtr(false), true},
		{"json_encoded_approved", `"approved"`, boolPtr(true), true},
		{"json_encoded_pending", `"pending"`, boolPtr(false), true},
		{"plain_string_not_json", "approved", nil, false},
		{"map_passed_required", map[string]any{"passed": true, "required": true}, boolPtr(true), true},
		{"map_enabled", map[string]any{"enabled": true}, nil, true},
		{"map_ok", map[string]any{"ok": false, "required": true}, boolPtr(false), true},
		{"map_not_required", map[string]any{"passed": true, "required": false}, boolPtr(true), false},
		{"json_string", `{"passed": true, "required": true}`, boolPtr(true), true},
		{"nil", nil, nil, false},
		{"number", float64(1), nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, required := gateResult(tc.value)
			if required != tc.required {
				t.Fatalf("required = %v, want %v", required, tc.required)
			}
			if tc.result == nil {
				if result != nil {
					t.Fatalf("result = %v, want nil", *result)
				}
				return
			}
			if result == nil || *result != *tc.result {
				t.Fatalf("result = %v, want %v", result, *tc.result)
			}
		})
	}
}

func TestCreationSkillCompatibilityCheck(t *testing.T) {
	skill := CreationSkillByID("grid-storyboard-batch")
	if skill == nil {
		t.Fatal("grid-storyboard-batch not found")
	}
	project := map[string]any{
		"content_mode": "drama", "generation_mode": "storyboard", "grid_storyboard": false,
	}
	if reason := skill.LatestVersion.Compatibility.Check(project, map[string]bool{"document": true}); reason != "grid_storyboard_incompatible" {
		t.Fatalf("reason = %s", reason)
	}
	project["grid_storyboard"] = true
	if reason := skill.LatestVersion.Compatibility.Check(project, map[string]bool{"document": true}); reason != "" {
		t.Fatalf("reason = %s", reason)
	}
	if reason := skill.LatestVersion.Compatibility.Check(project, nil); reason != "missing_inputs:document" {
		t.Fatalf("reason = %s", reason)
	}
}

func TestCompatibilityReportExplain(t *testing.T) {
	skill := CreationSkillByID("novel-to-drama")
	report := CompatibilityReport(skill, map[string]any{
		"content_mode": "ad", "generation_mode": "reference_video",
	}, map[string]bool{"document": true})
	if report["compatible"] != false {
		t.Fatalf("compatible = %v", report["compatible"])
	}
	reasons := report["reasons"].([]string)
	if !reflect.DeepEqual(reasons, []string{"content_mode_incompatible", "generation_mode_incompatible"}) {
		t.Fatalf("reasons = %v", reasons)
	}
	if report["requires_new_project"] != true {
		t.Fatalf("requires_new_project = %v", report["requires_new_project"])
	}
	if report["skill_id"] != "novel-to-drama" || report["skill_version_id"] != "novel-to-drama:v1" {
		t.Fatalf("report ids = %v", report)
	}
	if report["conflict_reason"] != "content_mode_incompatible" {
		t.Fatalf("conflict_reason = %v", report["conflict_reason"])
	}
	supported := report["supported_content_modes"].([]string)
	if !reflect.DeepEqual(supported, []string{"drama"}) {
		t.Fatalf("supported_content_modes = %v", supported)
	}
}

func TestListOfficialCreationSkills(t *testing.T) {
	project := map[string]any{
		"content_mode": "drama", "generation_mode": "storyboard",
	}
	checks := ListOfficialCreationSkills(project, map[string]bool{"document": true})
	if len(checks) != len(OfficialCreationSkills) {
		t.Fatalf("checks = %d, want %d", len(checks), len(OfficialCreationSkills))
	}
	compatible := 0
	for _, check := range checks {
		if check.Reason == "" {
			compatible++
		}
	}
	if compatible == 0 {
		t.Fatal("no compatible skills for drama+storyboard+document")
	}
	if CreationSkillByID("no-such-skill") != nil {
		t.Fatal("unexpected skill found")
	}
}
