package creation

// Shotwise 工作流领域原语（对应 lib/workflow.py）。
//
// 与 Python 侧一样刻意不依赖 gin / GORM：同一套校验与指纹规则要被 API、
// 调度器与智能体适配器共用。
//
// ## 关于 canonical JSON 的浮点形态
//
// Python 侧用 json.dumps(sort_keys=True, separators=(",",":")) 计算 graph_hash
// 与 input_fingerprint，而 json.dumps(1.0) == "1.0"。Go 标准库把 float64(1)
// 编码成 "1"，直接 Marshal 会算出与 Python 不同的哈希——同一个库里两种服务的
// 指纹就对不上。所以这里提供 workflowPyJSONValue / WorkflowCanonicalJSON，把整数值浮点
// 按 Python 的 repr 口径补 ".0"。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// 运行 / 节点 / 模板状态取值集合。
var (
	// WorkflowRunStatuses 是 workflow_runs.status 的取值集合。
	WorkflowRunStatuses = map[string]bool{
		"planned": true, "running": true, "paused": true,
		"waiting_review": true, "succeeded": true, "failed": true, "cancelled": true,
	}
	// WorkflowTemplateStatuses 是 workflow_templates.status 的取值集合。
	WorkflowTemplateStatuses = map[string]bool{
		"draft": true, "submitted": true, "under_review": true,
		"published": true, "rejected": true, "suspended": true,
	}
	// WorkflowContentModes 是 revision 的 content_mode 取值集合。
	WorkflowContentModes = map[string]bool{"manga": true, "drama": true, "narration": true, "ad": true}
	// WorkflowGenerationModes 是 revision 的 generation_mode 取值集合。
	WorkflowGenerationModes = map[string]bool{"storyboard": true, "reference_video": true}
	// WorkflowNodeStatuses 是 workflow_node_runs.status 的取值集合。
	WorkflowNodeStatuses = map[string]bool{
		"blocked": true, "ready": true, "queued": true, "running": true, "collecting": true,
		"succeeded": true, "retry_wait": true, "failed": true, "waiting_review": true,
		"skipped": true, "stale": true, "orphaned": true, "cancelled": true,
	}
	// WorkflowTerminalRunStatuses 是 run 的终态集合。
	WorkflowTerminalRunStatuses = map[string]bool{"succeeded": true, "failed": true, "cancelled": true}
	// WorkflowTerminalNodeStatuses 是 node run 的终态集合。
	WorkflowTerminalNodeStatuses = map[string]bool{
		"succeeded": true, "failed": true, "skipped": true, "stale": true, "orphaned": true, "cancelled": true,
	}
	// BuiltinWorkflowNodeTypes 首期只放行了这批已评审的内置节点类型。
	BuiltinWorkflowNodeTypes = map[string]bool{
		"source_import": true, "script_generate": true, "script_review": true,
		"character_reference": true, "storyboard_generate": true, "storyboard_review": true,
		"shot_image_generate": true, "shot_video_generate": true, "voice_generate": true,
		"subtitle_generate": true, "compose": true, "quality_check": true, "export": true,
		"image_input": true, "video_input": true, "loop": true, "branch": true, "param_adjust": true,
	}
)

// WorkflowQualityGateIDs 是可作为质量门禁的检查项。
var WorkflowQualityGateIDs = []string{
	"script_structure_complete",
	"character_references_consistent",
	"scene_references_exist",
	"storyboard_complete",
	"video_duration_legal",
	"subtitles_in_bounds",
	"audio_video_sync",
	"output_files_complete",
}

var workflowRunTransitions = map[string]map[string]bool{
	"planned":        {"running": true, "cancelled": true},
	"running":        {"paused": true, "waiting_review": true, "succeeded": true, "failed": true, "cancelled": true},
	"paused":         {"running": true, "cancelled": true},
	"waiting_review": {"running": true, "cancelled": true},
	"succeeded":      {},
	"failed":         {},
	"cancelled":      {},
}

var workflowNodeTransitions = map[string]map[string]bool{
	"blocked":        {"ready": true, "skipped": true, "cancelled": true},
	"ready":          {"queued": true, "waiting_review": true, "cancelled": true, "stale": true},
	"queued":         {"running": true, "retry_wait": true, "failed": true, "cancelled": true, "orphaned": true},
	"running":        {"collecting": true, "retry_wait": true, "failed": true, "cancelled": true, "orphaned": true},
	"collecting":     {"succeeded": true, "retry_wait": true, "failed": true, "cancelled": true, "orphaned": true},
	"retry_wait":     {"ready": true, "cancelled": true, "failed": true},
	"waiting_review": {"ready": true, "cancelled": true, "stale": true},
	"succeeded":      {},
	"failed":         {},
	"skipped":        {},
	"stale":          {},
	"orphaned":       {},
	"cancelled":      {},
}

var workflowTemplateTransitions = map[string]map[string]bool{
	"draft":        {"submitted": true},
	"submitted":    {"draft": true, "under_review": true},
	"under_review": {"published": true, "rejected": true, "draft": true},
	"published":    {"suspended": true},
	"rejected":     {"draft": true, "submitted": true},
	"suspended":    {"published": true},
}

// WorkflowValidationError 是工作流校验失败；Code 是机读码，Params 是渲染参数。
type WorkflowValidationError struct {
	Code   string
	Params map[string]any
}

func (e *WorkflowValidationError) Error() string { return e.Code }

// NewWorkflowValidationError 构造校验错误；params 为 nil 时给空 map，方便调用方直接塞字段。
func NewWorkflowValidationError(code string, params map[string]any) *WorkflowValidationError {
	if params == nil {
		params = map[string]any{}
	}
	return &WorkflowValidationError{Code: code, Params: params}
}

// --- canonical JSON ---

// workflowPyFloat 按 Python float 的 repr 口径格式化浮点：整数值补 ".0"，
// 大/小量级走科学计数法（Python 在 1e16 / 1e-4 处切换）。
//
// 与 task_failure.go 的 formatPyFloat 不同：那边服务于 default=str 的异常文本，
// 这里必须与 json.dumps 逐字节一致（1.0 → "1.0"）。
func workflowPyFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	abs := math.Abs(f)
	if abs != 0 && (abs >= 1e16 || abs < 1e-4) {
		return strconv.FormatFloat(f, 'e', -1, 64)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// workflowPyJSONValue 递归把浮点换成 Python 形态的 json.Number，其它类型原样透传。
func workflowPyJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = workflowPyJSONValue(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = workflowPyJSONValue(item)
		}
		return out
	case []map[string]any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = workflowPyJSONValue(item)
		}
		return out
	case float64:
		return json.Number(workflowPyFloat(typed))
	case float32:
		return json.Number(workflowPyFloat(float64(typed)))
	case int:
		return json.Number(strconv.FormatInt(int64(typed), 10))
	case int64:
		return json.Number(strconv.FormatInt(typed, 10))
	default:
		return value
	}
}

// WorkflowCanonicalJSON 与 Python 的 canonical_json 逐字节对齐（ensure_ascii=False）。
func WorkflowCanonicalJSON(value any) (string, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(workflowPyJSONValue(value)); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// WorkflowNormalizePrompt 只做架构允许的那一种 prompt 归一化。
func WorkflowNormalizePrompt(value string) string {
	normalized := norm.NFC.String(strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n"))
	return strings.TrimRight(normalized, "\n")
}

// WorkflowInputFingerprint 返回生成输入快照的 SHA-256 指纹。
func WorkflowInputFingerprint(payload map[string]any) (string, error) {
	normalized := make(map[string]any, len(payload))
	for key, value := range payload {
		normalized[key] = value
	}
	for _, key := range []string{"prompt", "negative_prompt", "canonical_prompt"} {
		if text, ok := normalized[key].(string); ok {
			normalized[key] = WorkflowNormalizePrompt(text)
		}
	}
	encoded, err := WorkflowCanonicalJSON(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:]), nil
}

// WorkflowGraphHash 计算图哈希；includeLayout 为 false 时剔除 UI 坐标。
func WorkflowGraphHash(nodes, edges []map[string]any, includeLayout bool) (string, error) {
	nodeValues := make([]any, 0, len(nodes))
	for _, node := range nodes {
		item := make(map[string]any, len(node))
		for key, value := range node {
			item[key] = value
		}
		if !includeLayout {
			delete(item, "ui_position")
			delete(item, "ui_position_json")
		}
		nodeValues = append(nodeValues, item)
	}
	sort.SliceStable(nodeValues, func(i, j int) bool {
		return nodeKeyValue(nodeValues[i]) < nodeKeyValue(nodeValues[j])
	})
	edgeValues := make([]any, 0, len(edges))
	for _, edge := range edges {
		edgeValues = append(edgeValues, edge)
	}
	sort.SliceStable(edgeValues, func(i, j int) bool {
		return edgeKeyValue(edgeValues[i]) < edgeKeyValue(edgeValues[j])
	})
	encoded, err := WorkflowCanonicalJSON(map[string]any{"nodes": nodeValues, "edges": edgeValues})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:]), nil
}

func nodeKeyValue(value any) string {
	if m, ok := value.(map[string]any); ok {
		return WorkflowMapString(m, "node_key")
	}
	return ""
}

func edgeKeyValue(value any) string {
	if m, ok := value.(map[string]any); ok {
		return WorkflowMapString(m, "edge_key")
	}
	return ""
}

// WorkflowMapString 取 map 里的字符串字段；缺失或非字符串返回空串。
func WorkflowMapString(m map[string]any, key string) string {
	if text, ok := m[key].(string); ok {
		return text
	}
	return ""
}

// WorkflowMapFloat 取 map 里的数值字段；缺失或不可解析返回 fallback。
func WorkflowMapFloat(m map[string]any, key string, fallback float64) float64 {
	switch typed := m[key].(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case json.Number:
		if parsed, err := typed.Float64(); err == nil {
			return parsed
		}
	}
	return fallback
}

// WorkflowMapInt 取 map 里的整数字段。
func WorkflowMapInt(m map[string]any, key string, fallback int) int {
	switch typed := m[key].(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return int(parsed)
		}
	}
	return fallback
}

// WorkflowMapBool 取 map 里的布尔字段。
func WorkflowMapBool(m map[string]any, key string, fallback bool) bool {
	if flag, ok := m[key].(bool); ok {
		return flag
	}
	return fallback
}

// --- 图校验 ---

// WorkflowValidateGraph 校验节点身份、边引用、可达性与环。
func WorkflowValidateGraph(nodes, edges []map[string]any) error {
	keys := make([]string, 0, len(nodes))
	known := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		key := WorkflowMapString(node, "node_key")
		if key == "" || known[key] {
			return NewWorkflowValidationError("workflow_input_invalid", map[string]any{"reason": "node_key"})
		}
		known[key] = true
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return NewWorkflowValidationError("workflow_input_invalid", map[string]any{"reason": "node_key"})
	}

	seen := make(map[string]bool, len(edges))
	outgoing := map[string]map[string]bool{}
	incoming := map[string]map[string]bool{}
	for _, edge := range edges {
		key := WorkflowMapString(edge, "edge_key")
		if key != "" {
			if seen[key] {
				return NewWorkflowValidationError("workflow_input_invalid", map[string]any{"reason": "edge_key"})
			}
			seen[key] = true
		}
		source := WorkflowMapString(edge, "source_node_key")
		target := WorkflowMapString(edge, "target_node_key")
		if source == "" || target == "" || !known[source] || !known[target] {
			return NewWorkflowValidationError("workflow_input_invalid", map[string]any{"reason": "edge_reference"})
		}
		if source == target {
			return NewWorkflowValidationError("workflow_cycle_detected", map[string]any{"node_key": source})
		}
		if outgoing[source] == nil {
			outgoing[source] = map[string]bool{}
		}
		outgoing[source][target] = true
		if incoming[target] == nil {
			incoming[target] = map[string]bool{}
		}
		incoming[target][source] = true
	}

	roots := make([]string, 0, len(keys))
	for _, key := range keys {
		if len(incoming[key]) == 0 {
			roots = append(roots, key)
		}
	}
	if len(roots) == 0 {
		return NewWorkflowValidationError("workflow_cycle_detected", nil)
	}
	visited := map[string]bool{}
	queue := append([]string{}, roots...)
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if visited[key] {
			continue
		}
		visited[key] = true
		for target := range outgoing[key] {
			queue = append(queue, target)
		}
	}
	if len(visited) != len(known) {
		return NewWorkflowValidationError("workflow_input_invalid", map[string]any{"reason": "unreachable_node"})
	}

	indegree := make(map[string]int, len(keys))
	for _, key := range keys {
		indegree[key] = len(incoming[key])
	}
	queue = queue[:0]
	for _, key := range keys {
		if indegree[key] == 0 {
			queue = append(queue, key)
		}
	}
	processed := 0
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		processed++
		for target := range outgoing[key] {
			indegree[target]--
			if indegree[target] == 0 {
				queue = append(queue, target)
			}
		}
	}
	if processed != len(known) {
		return NewWorkflowValidationError("workflow_cycle_detected", nil)
	}
	return nil
}

// WorkflowNodeGraphEdges 由边构造出/入邻接表，键一律按 validate_graph 的字符串化口径。
func WorkflowNodeGraphEdges(edges []map[string]any) (map[string]map[string]bool, map[string]map[string]bool) {
	outgoing := map[string]map[string]bool{}
	incoming := map[string]map[string]bool{}
	for _, edge := range edges {
		source := WorkflowMapString(edge, "source_node_key")
		target := WorkflowMapString(edge, "target_node_key")
		if source == "" || target == "" {
			continue
		}
		if outgoing[source] == nil {
			outgoing[source] = map[string]bool{}
		}
		outgoing[source][target] = true
		if incoming[target] == nil {
			incoming[target] = map[string]bool{}
		}
		incoming[target][source] = true
	}
	return outgoing, incoming
}

// WorkflowValidateModes 校验内容模式与生成模式。
func WorkflowValidateModes(contentMode, generationMode string) error {
	if !WorkflowContentModes[contentMode] {
		return NewWorkflowValidationError("workflow_input_invalid", map[string]any{"reason": "content_mode"})
	}
	if !WorkflowGenerationModes[generationMode] {
		return NewWorkflowValidationError("workflow_input_invalid", map[string]any{"reason": "generation_mode"})
	}
	return nil
}

// WorkflowValidateNodeContracts 首期只放行已评审的内置 runtime 节点。
func WorkflowValidateNodeContracts(nodes []map[string]any) error {
	for _, node := range nodes {
		nodeType := WorkflowMapString(node, "node_type")
		executorID := WorkflowMapString(node, "executor_id")
		if executorID == "" {
			executorID = "builtin"
		}
		cachePolicy := WorkflowMapString(node, "cache_policy")
		if cachePolicy == "" {
			cachePolicy = "reuse"
		}
		if !BuiltinWorkflowNodeTypes[nodeType] {
			return NewWorkflowValidationError("workflow_node_not_allowed", map[string]any{"node_type": nodeType})
		}
		if executorID != "builtin" {
			return NewWorkflowValidationError("workflow_executor_not_allowed", map[string]any{"executor_id": executorID})
		}
		if cachePolicy != "reuse" && cachePolicy != "refresh" && cachePolicy != "never" {
			return NewWorkflowValidationError("workflow_input_invalid", map[string]any{"reason": "cache_policy"})
		}
		if WorkflowMapFloat(node, "estimated_cost", 0) < 0 {
			return NewWorkflowValidationError("workflow_input_invalid", map[string]any{"reason": "estimated_cost"})
		}
	}
	return nil
}

// WorkflowTransitionRun 校验 run 状态跃迁。
func WorkflowTransitionRun(status, target string) error {
	if !WorkflowRunStatuses[status] || !WorkflowRunStatuses[target] || !workflowRunTransitions[status][target] {
		return NewWorkflowValidationError("workflow_invalid_transition", map[string]any{
			"entity": "run", "status": status, "target": target,
		})
	}
	return nil
}

// WorkflowTransitionNode 校验 node run 状态跃迁。
func WorkflowTransitionNode(status, target string) error {
	if !WorkflowNodeStatuses[status] || !WorkflowNodeStatuses[target] || !workflowNodeTransitions[status][target] {
		return NewWorkflowValidationError("workflow_invalid_transition", map[string]any{
			"entity": "node", "status": status, "target": target,
		})
	}
	return nil
}

// WorkflowTemplateTransition 校验模板状态跃迁。
func WorkflowTemplateTransition(status, target string) error {
	if !WorkflowTemplateStatuses[status] || !WorkflowTemplateTransitionsOk(status, target) {
		return NewWorkflowValidationError("workflow_template_invalid_transition", map[string]any{
			"status": status, "target": target,
		})
	}
	return nil
}

// WorkflowTemplateTransitionsOk 报告模板从 status 能否走到 target。
func WorkflowTemplateTransitionsOk(status, target string) bool {
	return workflowTemplateTransitions[status][target]
}

// WorkflowTopologicalOrder 用 Kahn 算法返回依赖序（根在前），有环时报 workflow_cycle_detected。
func WorkflowTopologicalOrder(nodes, edges []map[string]any) ([]string, error) {
	if err := WorkflowValidateGraph(nodes, edges); err != nil {
		return nil, err
	}
	outgoing, _ := WorkflowNodeGraphEdges(edges)
	indegree := make(map[string]int, len(nodes))
	order := make([]string, 0, len(nodes))
	for _, node := range nodes {
		key := WorkflowMapString(node, "node_key")
		indegree[key] = 0
		order = append(order, key)
	}
	for _, targets := range outgoing {
		for target := range targets {
			if _, ok := indegree[target]; ok {
				indegree[target]++
			}
		}
	}
	queue := make([]string, 0, len(order))
	for _, key := range order {
		if indegree[key] == 0 {
			queue = append(queue, key)
		}
	}
	ordered := make([]string, 0, len(order))
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		ordered = append(ordered, key)
		for target := range outgoing[key] {
			indegree[target]--
			if indegree[target] == 0 {
				queue = append(queue, target)
			}
		}
	}
	if len(ordered) != len(indegree) {
		return nil, NewWorkflowValidationError("workflow_cycle_detected", nil)
	}
	return ordered, nil
}

// WorkflowUpstreamOf 返回每个 node_key 的可达上游闭包。
func WorkflowUpstreamOf(nodes, edges []map[string]any) (map[string]map[string]bool, error) {
	_, incoming := WorkflowNodeGraphEdges(edges)
	result := make(map[string]map[string]bool, len(nodes))
	for _, node := range nodes {
		result[WorkflowMapString(node, "node_key")] = map[string]bool{}
	}
	order, err := WorkflowTopologicalOrder(nodes, edges)
	if err != nil {
		return nil, err
	}
	for _, key := range order {
		seen := map[string]bool{}
		for source := range incoming[key] {
			seen[source] = true
			for upstream := range result[source] {
				seen[upstream] = true
			}
		}
		result[key] = seen
	}
	return result, nil
}

// WorkflowQualityGateReport 把门禁事实归一成可暂停、可行动的报告。
func WorkflowQualityGateReport(facts map[string]any, required []string) map[string]any {
	checks := required
	if len(checks) == 0 {
		checks = WorkflowQualityGateIDs
	}
	results := map[string]any{}
	failures := make([]any, 0, len(checks))
	for _, gateID := range checks {
		raw, ok := facts[gateID]
		if !ok {
			raw = false
		}
		passed := false
		message := gateID
		suggestion := "检查该门禁所需的输入和上游产物"
		if detail, isMap := raw.(map[string]any); isMap {
			passed = WorkflowMapBool(detail, "ok", false)
			if text := WorkflowMapString(detail, "message"); text != "" {
				message = text
			}
			if text := WorkflowMapString(detail, "suggestion"); text != "" {
				suggestion = text
			}
		} else if flag, isBool := raw.(bool); isBool {
			passed = flag
		}
		results[gateID] = map[string]any{"passed": passed, "message": message, "suggestion": suggestion}
		if !passed {
			failures = append(failures, map[string]any{
				"gate": gateID, "message": message, "suggestion": suggestion,
			})
		}
	}
	return map[string]any{"passed": len(failures) == 0, "results": results, "failures": failures}
}

// --- Patch ---

// WorkflowPatchOperations 是 patch.operation 的合法取值。
var WorkflowPatchOperations = map[string]bool{
	"set_config": true, "add_node": true, "remove_node": true, "add_edge": true, "remove_edge": true,
}

// WorkflowPatchScopes 是 patch.scope 的合法取值。
var WorkflowPatchScopes = map[string]bool{"shot": true, "scene": true, "episode": true}

// WorkflowPatchOperation 是智能体提出的一项安全、可审计的图/配置变更。
type WorkflowPatchOperation struct {
	Operation            string  `json:"operation"`
	TargetNode           *string `json:"target_node"`
	Path                 *string `json:"path"`
	Before               any     `json:"before"`
	After                any     `json:"after"`
	EstimatedCostDelta   float64 `json:"estimated_cost_delta"`
	RequiresConfirmation bool    `json:"requires_confirmation"`
}

// WorkflowPatch 是智能体对某个基线 revision 提出的变更集。
type WorkflowPatch struct {
	BaseRevisionID string                   `json:"base_revision_id"`
	Operations     []WorkflowPatchOperation `json:"operations"`
	Scope          string                   `json:"scope"`
	Rerun          bool                     `json:"rerun"`
	Reason         string                   `json:"reason"`
}

// OpString 返回操作的字符串形式（空指针视为空串）。
func (o WorkflowPatchOperation) OpString(field *string) string {
	if field == nil {
		return ""
	}
	return *field
}

// Validate 复刻 Pydantic 的字段约束；失败返回 *WorkflowValidationError。
func (p *WorkflowPatch) Validate() error {
	if p.BaseRevisionID == "" {
		return NewWorkflowValidationError("workflow_patch_invalid", map[string]any{"reason": "base_revision_id"})
	}
	if len(p.Operations) == 0 || len(p.Operations) > 100 {
		return NewWorkflowValidationError("workflow_patch_invalid", map[string]any{"reason": "operations"})
	}
	if p.Scope == "" {
		p.Scope = "episode"
	}
	if !WorkflowPatchScopes[p.Scope] {
		return NewWorkflowValidationError("workflow_patch_invalid", map[string]any{"reason": "scope"})
	}
	for i := range p.Operations {
		if !WorkflowPatchOperations[p.Operations[i].Operation] {
			return NewWorkflowValidationError("workflow_patch_invalid", map[string]any{"reason": "operation"})
		}
	}
	return nil
}

// setPath 按点分路径写入嵌套 map，缺失的中间层自动补对象。
func setPath(value map[string]any, path string, newValue any) error {
	parts := make([]string, 0, 4)
	for _, part := range strings.Split(path, ".") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return NewWorkflowValidationError("workflow_patch_invalid", map[string]any{"reason": "path"})
	}
	cursor := value
	for _, part := range parts[:len(parts)-1] {
		child, ok := cursor[part].(map[string]any)
		if !ok {
			child = map[string]any{}
			cursor[part] = child
		}
		cursor = child
	}
	cursor[parts[len(parts)-1]] = newValue
	return nil
}

// workflowCloneNodes 深拷贝节点/边，避免改动已发布图。
func workflowCloneNodes(nodes []map[string]any) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		encoded, err := WorkflowCanonicalJSON(node)
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader([]byte(encoded)))
		decoder.UseNumber()
		var clone map[string]any
		if err := decoder.Decode(&clone); err != nil {
			return nil, err
		}
		out = append(out, clone)
	}
	return out, nil
}

// WorkflowApplyPatchToGraph 把已校验的 patch 应用到图副本上。
//
// 已发布行永不被改动：调用方拿到返回的图后自行决定如何持久化（通常是新建 draft）。
func WorkflowApplyPatchToGraph(
	nodes, edges []map[string]any, patch WorkflowPatch,
) ([]map[string]any, []map[string]any, error) {
	nextNodes, err := workflowCloneNodes(nodes)
	if err != nil {
		return nil, nil, err
	}
	nextEdges, err := workflowCloneNodes(edges)
	if err != nil {
		return nil, nil, err
	}
	byKey := map[string]map[string]any{}
	for _, node := range nextNodes {
		byKey[WorkflowMapString(node, "node_key")] = node
	}
	for _, operation := range patch.Operations {
		target := operation.OpString(operation.TargetNode)
		switch operation.Operation {
		case "set_config":
			node, ok := byKey[target]
			if !ok {
				return nil, nil, NewWorkflowValidationError("workflow_patch_invalid", map[string]any{"reason": "unknown_node"})
			}
			config, ok := node["config"].(map[string]any)
			if !ok {
				config = map[string]any{}
				node["config"] = config
			}
			if err := setPath(config, operation.OpString(operation.Path), operation.After); err != nil {
				return nil, nil, err
			}
		case "add_node":
			candidate, ok := operation.After.(map[string]any)
			if !ok || WorkflowMapString(candidate, "node_key") == "" {
				return nil, nil, NewWorkflowValidationError("workflow_patch_invalid", map[string]any{"reason": "node_payload"})
			}
			key := WorkflowMapString(candidate, "node_key")
			if _, exists := byKey[key]; exists {
				return nil, nil, NewWorkflowValidationError("workflow_input_invalid", map[string]any{"reason": "node_key"})
			}
			cloned, err := workflowCloneNodes([]map[string]any{candidate})
			if err != nil {
				return nil, nil, err
			}
			byKey[key] = cloned[0]
			nextNodes = append(nextNodes, cloned[0])
		case "remove_node":
			if _, exists := byKey[target]; !exists {
				return nil, nil, NewWorkflowValidationError("workflow_patch_invalid", map[string]any{"reason": "unknown_node"})
			}
			kept := make([]map[string]any, 0, len(nextNodes))
			for _, node := range nextNodes {
				if WorkflowMapString(node, "node_key") != target {
					kept = append(kept, node)
				}
			}
			nextNodes = kept
			delete(byKey, target)
			keptEdges := make([]map[string]any, 0, len(nextEdges))
			for _, edge := range nextEdges {
				source := WorkflowMapString(edge, "source_node_key")
				destination := WorkflowMapString(edge, "target_node_key")
				if source != target && destination != target {
					keptEdges = append(keptEdges, edge)
				}
			}
			nextEdges = keptEdges
		case "add_edge":
			candidate, ok := operation.After.(map[string]any)
			if !ok {
				return nil, nil, NewWorkflowValidationError("workflow_patch_invalid", map[string]any{"reason": "edge_payload"})
			}
			cloned, err := workflowCloneNodes([]map[string]any{candidate})
			if err != nil {
				return nil, nil, err
			}
			nextEdges = append(nextEdges, cloned[0])
		case "remove_edge":
			edgeKey := target
			if edgeKey == "" {
				edgeKey = operation.OpString(operation.Path)
			}
			keptEdges := make([]map[string]any, 0, len(nextEdges))
			for _, edge := range nextEdges {
				if WorkflowMapString(edge, "edge_key") != edgeKey {
					keptEdges = append(keptEdges, edge)
				}
			}
			nextEdges = keptEdges
		}
	}
	if err := WorkflowValidateGraph(nextNodes, nextEdges); err != nil {
		return nil, nil, err
	}
	if err := WorkflowValidateNodeContracts(nextNodes); err != nil {
		return nil, nil, err
	}
	return nextNodes, nextEdges, nil
}

// workflowQualityGateNodeKeys 找出图里的质量门禁节点。
func workflowQualityGateNodeKeys(nodes []map[string]any) map[string]bool {
	keys := map[string]bool{}
	for _, node := range nodes {
		nodeType := strings.ToLower(WorkflowMapString(node, "node_type"))
		config, _ := node["config"].(map[string]any)
		if strings.Contains(nodeType, "quality") || WorkflowMapBool(config, "quality_gate", false) {
			if key := WorkflowMapString(node, "node_key"); key != "" {
				keys[key] = true
			}
		}
	}
	return keys
}

// ensureQualityGatesIntact 禁止 patch 摘掉质量门禁。
func ensureQualityGatesIntact(nodes, edges []map[string]any, patch WorkflowPatch) error {
	qualityKeys := workflowQualityGateNodeKeys(nodes)
	if len(qualityKeys) == 0 {
		return nil
	}
	edgeByKey := map[string]map[string]any{}
	for _, edge := range edges {
		edgeByKey[WorkflowMapString(edge, "edge_key")] = edge
	}
	for _, operation := range patch.Operations {
		target := operation.OpString(operation.TargetNode)
		switch operation.Operation {
		case "remove_node":
			if qualityKeys[target] {
				return NewWorkflowValidationError("workflow_quality_gate_required", map[string]any{"node_key": target})
			}
		case "remove_edge":
			edge := edgeByKey[target]
			if edge != nil {
				source := WorkflowMapString(edge, "source_node_key")
				destination := WorkflowMapString(edge, "target_node_key")
				if qualityKeys[source] || qualityKeys[destination] {
					return NewWorkflowValidationError("workflow_quality_gate_required", map[string]any{"node_key": target})
				}
			}
		case "set_config":
			if !qualityKeys[target] {
				continue
			}
			path := strings.ToLower(strings.ReplaceAll(operation.OpString(operation.Path), "-", "_"))
			parts := strings.Split(path, ".")
			leaf := parts[len(parts)-1]
			if leaf == "disabled" && operation.After == true {
				return NewWorkflowValidationError("workflow_quality_gate_required", map[string]any{"node_key": target})
			}
			if strings.Contains(path, "quality_gate") && operation.After == false {
				return NewWorkflowValidationError("workflow_quality_gate_required", map[string]any{"node_key": target})
			}
		}
	}
	return nil
}

// WorkflowPatchRequiresConfirmation 判断一个 patch 是否需要用户显式确认。
func WorkflowPatchRequiresConfirmation(nodes []map[string]any, patch WorkflowPatch, completedNodes map[string]bool) bool {
	if patch.Rerun {
		return true
	}
	confirmationPaths := map[string]bool{
		"provider": true, "supplier": true, "model": true,
		"backend": true, "engine": true, "executor": true, "format": true,
	}
	for _, operation := range patch.Operations {
		if operation.RequiresConfirmation {
			return true
		}
		switch operation.Operation {
		case "add_node", "remove_node", "add_edge", "remove_edge":
			return true
		}
		if operation.EstimatedCostDelta > 0 {
			return true
		}
		target := operation.OpString(operation.TargetNode)
		if target != "" && completedNodes[target] {
			return true
		}
		if operation.Operation == "set_config" {
			path := strings.ToLower(strings.ReplaceAll(operation.OpString(operation.Path), "-", "_"))
			parts := strings.Split(path, ".")
			if confirmationPaths[parts[len(parts)-1]] {
				return true
			}
		}
	}
	return false
}

// WorkflowAffectedNodes 返回变更节点及其下游闭包（局部重跑用）。
func WorkflowAffectedNodes(nodes, edges []map[string]any, patch WorkflowPatch) ([]string, error) {
	if err := WorkflowValidateGraph(nodes, edges); err != nil {
		return nil, err
	}
	outgoing, _ := WorkflowNodeGraphEdges(edges)
	result := map[string]bool{}
	queue := make([]string, 0, len(patch.Operations))
	for _, operation := range patch.Operations {
		target := operation.OpString(operation.TargetNode)
		if target != "" && !result[target] {
			result[target] = true
			queue = append(queue, target)
		}
	}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		for next := range outgoing[key] {
			if !result[next] {
				result[next] = true
				queue = append(queue, next)
			}
		}
	}
	keys := make([]string, 0, len(result))
	for key := range result {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// WorkflowValidatePatch 校验智能体 patch，不改动已发布 revision。
func WorkflowValidatePatch(
	nodes, edges []map[string]any,
	patch WorkflowPatch,
	remainingBudget *float64,
	allowDestructive bool,
	completedNodes map[string]bool,
) (map[string]any, error) {
	if err := WorkflowValidateGraph(nodes, edges); err != nil {
		return nil, err
	}
	if err := ensureQualityGatesIntact(nodes, edges, patch); err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, node := range nodes {
		known[WorkflowMapString(node, "node_key")] = true
	}
	totalDelta := 0.0
	for _, operation := range patch.Operations {
		if operation.Operation == "set_config" && operation.OpString(operation.TargetNode) == "" {
			return nil, NewWorkflowValidationError("workflow_patch_invalid", map[string]any{"reason": "target_node"})
		}
		target := operation.OpString(operation.TargetNode)
		if target != "" && !known[target] && operation.Operation != "add_node" {
			return nil, NewWorkflowValidationError("workflow_patch_invalid", map[string]any{
				"reason": "unknown_node", "node_key": target,
			})
		}
		if (operation.Operation == "remove_node" || operation.Operation == "remove_edge") && !allowDestructive {
			return nil, NewWorkflowValidationError("workflow_patch_confirmation_required", map[string]any{
				"operation": operation.Operation,
			})
		}
		totalDelta += operation.EstimatedCostDelta
	}
	if remainingBudget != nil && totalDelta > *remainingBudget {
		return nil, NewWorkflowValidationError("workflow_budget_exceeded", map[string]any{
			"estimated_delta": totalDelta, "remaining": *remainingBudget,
		})
	}
	affected, err := WorkflowAffectedNodes(nodes, edges, patch)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"valid":                 true,
		"affected_nodes":        affected,
		"estimated_cost_delta":  totalDelta,
		"requires_confirmation": WorkflowPatchRequiresConfirmation(nodes, patch, completedNodes),
	}, nil
}
