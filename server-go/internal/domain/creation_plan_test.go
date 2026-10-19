package domain

import (
	"testing"
)

func creationProject() map[string]any {
	return map[string]any{
		"content_mode":    "drama",
		"generation_mode": "storyboard",
		"grid_storyboard": false,
		"aspect_ratio":    "9:16",
		"style":           "写实",
		"model_settings":  map[string]any{"openai/gpt": map[string]string{"resolution": "1080p"}},
	}
}

func TestProjectContextSnapshotFromProjectRejectsBadModes(t *testing.T) {
	if _, err := ProjectContextSnapshotFromProject("p1", map[string]any{
		"generation_mode": "telepathy",
	}); err == nil {
		t.Fatal("unsupported generation_mode must be rejected")
	}
	if _, err := ProjectContextSnapshotFromProject("", creationProject()); err == nil {
		t.Fatal("empty project_id must be rejected")
	}
	snapshot, err := ProjectContextSnapshotFromProject("p1", creationProject())
	if err != nil {
		t.Fatalf("valid project must compile: %v", err)
	}
	if snapshot.ContentMode != "drama" || snapshot.GenerationMode != "storyboard" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func TestCompileCreationPlanRejectsModeOverrides(t *testing.T) {
	context, err := ProjectContextSnapshotFromProject("p1", creationProject())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	_, err = CompileCreationPlan(CompileCreationPlanParams{
		PlanID:           "cp_1",
		SkillID:          "novel-to-drama",
		WorkflowRevision: "rev-1",
		ProjectContext:   context,
		SkillInputs: map[string]any{
			"generation_mode_override": "reference_video",
		},
	})
	if err == nil {
		t.Fatal("mode override must be rejected")
	}
	if got := err.Error(); got == "" {
		t.Fatal("error must carry a message")
	}
}

func TestCompileCreationPlanFingerprintIsDeterministic(t *testing.T) {
	context, err := ProjectContextSnapshotFromProject("p1", creationProject())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	params := CompileCreationPlanParams{
		PlanID:           "cp_1",
		SkillID:          "novel-to-drama",
		WorkflowRevision: "rev-1",
		ProjectContext:   context,
		SkillInputs: map[string]any{
			"resource_ids":   []string{"doc-1"},
			"resource_types": []string{"document"},
			"estimated_cost": 1.0,
		},
		SupportedContentModes:    map[string]bool{"drama": true},
		SupportedGenerationModes: map[string]bool{"storyboard": true},
	}
	first, err := CompileCreationPlan(params)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	second, err := CompileCreationPlan(params)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if first.Fingerprint != second.Fingerprint {
		t.Fatalf("fingerprint must be deterministic: %s vs %s", first.Fingerprint, second.Fingerprint)
	}
	if !first.IsCompatible() {
		t.Fatalf("drama/storyboard project must be compatible: %v", first.Compatibility)
	}
	if err := first.RequireCompatible(); err != nil {
		t.Fatalf("require compatible: %v", err)
	}
}

func TestCompileCreationPlanRecordsModeReasons(t *testing.T) {
	context, err := ProjectContextSnapshotFromProject("p1", creationProject())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	plan, err := CompileCreationPlan(CompileCreationPlanParams{
		PlanID:                   "cp_2",
		SkillID:                  "reference-image-video",
		WorkflowRevision:         "rev-2",
		ProjectContext:           context,
		SkillInputs:              map[string]any{},
		SupportedGenerationModes: map[string]bool{"reference_video": true},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if plan.IsCompatible() {
		t.Fatal("storyboard project must be incompatible with a reference_video-only skill")
	}
	if len(plan.Compatibility.Reasons) != 1 || plan.Compatibility.Reasons[0] != "generation_mode_not_supported" {
		t.Fatalf("unexpected reasons: %v", plan.Compatibility.Reasons)
	}
	if err := plan.RequireCompatible(); err == nil {
		t.Fatal("require compatible must fail")
	}
}

func TestCreationPlanAssertProjectMatches(t *testing.T) {
	context, err := ProjectContextSnapshotFromProject("p1", creationProject())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	plan, err := CompileCreationPlan(CompileCreationPlanParams{
		PlanID:           "cp_3",
		SkillID:          "novel-to-drama",
		WorkflowRevision: "rev-3",
		ProjectContext:   context,
		SkillInputs:      map[string]any{},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if err := plan.AssertProjectMatches(creationProject()); err != nil {
		t.Fatalf("unchanged project must match: %v", err)
	}
	mutated := creationProject()
	mutated["style"] = "水墨"
	if err := plan.AssertProjectMatches(mutated); err == nil {
		t.Fatal("changed project must be rejected")
	}
}

func TestCreationPlanStableIDPrefix(t *testing.T) {
	id, err := CreationPlanStableID(map[string]any{"a": 1})
	if err != nil {
		t.Fatalf("stable id: %v", err)
	}
	if len(id) != 67 || id[:3] != "cp_" {
		t.Fatalf("unexpected plan id: %s", id)
	}
}
