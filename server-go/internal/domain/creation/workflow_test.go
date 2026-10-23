package creation

import (
	"errors"
	"testing"
)

// 参考值由 Python 侧 lib/workflow.py（canonical_json / graph_hash / input_fingerprint）
// 对同一组输入实算得出，Go 侧必须逐字节对齐——两套服务共用同一个库，
// 指纹对不上就意味着 Python 建的 revision 在 Go 侧认不出来。

const (
	referenceCanonicalJSON = `{"n":2,"s":"中文","weight":1.0}`
	referenceGraphHashAll  = "98574b9834a579eb2206f1eb6ff1fa84aa1c19857da5bf85127c6c076ec5438e"
	referenceGraphHashExec = "3b370eae930eb07298e6bb3b9f40d78f9e6690310ab346c2a6101b4211050738"
	referenceFingerprint   = "280f54ced2a9cf7723d10fc19404ccc69dd95278a50b31338739f8b5de229021"
)

func referenceNodes() []map[string]any {
	return []map[string]any{
		{
			"node_key": "a", "node_type": "a", "node_type_version": "1", "config_schema_version": "1",
			"config": map[string]any{}, "ui_position": map[string]any{"x": 0, "y": 0}, "weight": 2.0,
		},
		{
			"node_key": "b", "node_type": "b", "node_type_version": "1", "config_schema_version": "1",
			"config": map[string]any{}, "ui_position": map[string]any{"x": 240, "y": 0}, "weight": 1.0,
		},
	}
}

func referenceEdges() []map[string]any {
	return []map[string]any{
		{"edge_key": "a-b", "source_node_key": "a", "target_node_key": "b", "on_failure": "stop", "priority": 0},
	}
}

func TestWorkflowCanonicalJSONMatchesPython(t *testing.T) {
	encoded, err := WorkflowCanonicalJSON(map[string]any{"weight": 1.0, "n": 2, "s": "中文"})
	if err != nil {
		t.Fatalf("canonical json: %v", err)
	}
	if encoded != referenceCanonicalJSON {
		t.Fatalf("canonical json mismatch:\n got %s\nwant %s", encoded, referenceCanonicalJSON)
	}
}

func TestWorkflowGraphHashMatchesPython(t *testing.T) {
	withLayout, err := WorkflowGraphHash(referenceNodes(), referenceEdges(), true)
	if err != nil {
		t.Fatalf("graph hash: %v", err)
	}
	if withLayout != referenceGraphHashAll {
		t.Fatalf("graph hash (layout) mismatch:\n got %s\nwant %s", withLayout, referenceGraphHashAll)
	}
	execOnly, err := WorkflowGraphHash(referenceNodes(), referenceEdges(), false)
	if err != nil {
		t.Fatalf("graph hash: %v", err)
	}
	if execOnly != referenceGraphHashExec {
		t.Fatalf("graph hash (exec) mismatch:\n got %s\nwant %s", execOnly, referenceGraphHashExec)
	}
}

func TestWorkflowInputFingerprintNormalizesPrompt(t *testing.T) {
	fingerprint, err := WorkflowInputFingerprint(map[string]any{"prompt": "a\r\nb\n", "x": 1.0})
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if fingerprint != referenceFingerprint {
		t.Fatalf("fingerprint mismatch:\n got %s\nwant %s", fingerprint, referenceFingerprint)
	}
}

func TestWorkflowValidateGraphRejectsCyclesAndUnknownEdges(t *testing.T) {
	nodes := []map[string]any{
		{"node_key": "a", "node_type": "script_generate"},
		{"node_key": "b", "node_type": "export"},
	}
	if err := WorkflowValidateGraph(nodes, referenceEdges()); err != nil {
		t.Fatalf("valid graph rejected: %v", err)
	}

	cyclic := []map[string]any{
		{"edge_key": "1", "source_node_key": "a", "target_node_key": "b"},
		{"edge_key": "2", "source_node_key": "b", "target_node_key": "a"},
	}
	err := WorkflowValidateGraph(nodes, cyclic)
	var validation *WorkflowValidationError
	if !errors.As(err, &validation) || validation.Code != "workflow_cycle_detected" {
		t.Fatalf("cycle not detected: %v", err)
	}

	dangling := []map[string]any{{"edge_key": "1", "source_node_key": "a", "target_node_key": "ghost"}}
	err = WorkflowValidateGraph(nodes, dangling)
	if !errors.As(err, &validation) || validation.Code != "workflow_input_invalid" {
		t.Fatalf("dangling edge not rejected: %v", err)
	}
	if validation.Params["reason"] != "edge_reference" {
		t.Fatalf("unexpected reason: %v", validation.Params)
	}
}

func TestWorkflowNodeContractsRejectCustomExecutor(t *testing.T) {
	nodes := []map[string]any{{"node_key": "a", "node_type": "script_generate", "executor_id": "third-party"}}
	err := WorkflowValidateNodeContracts(nodes)
	var validation *WorkflowValidationError
	if !errors.As(err, &validation) || validation.Code != "workflow_executor_not_allowed" {
		t.Fatalf("custom executor not rejected: %v", err)
	}
}

func TestWorkflowApplyPatchSetConfig(t *testing.T) {
	nodes := []map[string]any{
		{"node_key": "a", "node_type": "script_generate", "config": map[string]any{"model": map[string]any{"id": "old"}}},
	}
	target := "a"
	path := "model.id"
	patch := WorkflowPatch{
		BaseRevisionID: "rev",
		Scope:          "episode",
		Operations: []WorkflowPatchOperation{
			{Operation: "set_config", TargetNode: &target, Path: &path, After: "new"},
		},
	}
	next, _, err := WorkflowApplyPatchToGraph(nodes, nil, patch)
	if err != nil {
		t.Fatalf("apply patch: %v", err)
	}
	config, _ := next[0]["config"].(map[string]any)
	model, _ := config["model"].(map[string]any)
	if model["id"] != "new" {
		t.Fatalf("config not patched: %v", config)
	}
	if _, ok := nodes[0]["config"].(map[string]any)["model"].(map[string]any)["id"]; !ok {
		t.Fatal("source graph mutated")
	}
	if original, _ := nodes[0]["config"].(map[string]any)["model"].(map[string]any)["id"].(string); original != "old" {
		t.Fatalf("source graph mutated: %v", original)
	}
}

func TestWorkflowValidatePatchRequiresConfirmationForDestructiveOps(t *testing.T) {
	nodes := []map[string]any{
		{"node_key": "a", "node_type": "script_generate", "estimated_cost": 0.0},
		{"node_key": "b", "node_type": "export", "estimated_cost": 0.0},
	}
	edges := []map[string]any{{"edge_key": "a-b", "source_node_key": "a", "target_node_key": "b"}}
	target := "b"
	patch := WorkflowPatch{
		BaseRevisionID: "rev",
		Scope:          "episode",
		Operations:     []WorkflowPatchOperation{{Operation: "remove_node", TargetNode: &target}},
	}
	_, err := WorkflowValidatePatch(nodes, edges, patch, nil, false, nil)
	var validation *WorkflowValidationError
	if !errors.As(err, &validation) || validation.Code != "workflow_patch_confirmation_required" {
		t.Fatalf("destructive patch not gated: %v", err)
	}
}

func TestWorkflowTemplateAndRunTransitions(t *testing.T) {
	if err := WorkflowTemplateTransition("draft", "published"); err == nil {
		t.Fatal("draft -> published must be rejected")
	}
	if err := WorkflowTemplateTransition("draft", "submitted"); err != nil {
		t.Fatalf("draft -> submitted must be allowed: %v", err)
	}
	if err := WorkflowTransitionRun("planned", "running"); err != nil {
		t.Fatalf("planned -> running must be allowed: %v", err)
	}
	if err := WorkflowTransitionRun("succeeded", "running"); err == nil {
		t.Fatal("terminal run must not transition")
	}
}

func TestWorkflowTopologicalOrder(t *testing.T) {
	order, err := WorkflowTopologicalOrder(referenceNodes(), referenceEdges())
	if err != nil {
		t.Fatalf("topological order: %v", err)
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("unexpected order: %v", order)
	}
}
