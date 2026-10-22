package service

// 模板市场契约的安全校验（对应 server/services/workflow_contracts.py）。
//
// 上传方可以是任意用户，所以这里拒绝一切"能碰到本机文件系统 / 外部网络"的表达：
// 绝对路径、URL、父目录穿越、外链 $ref，以及非法的 JSON Schema。

import (
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
)

// WorkflowContractIssue 是一条契约问题。
type WorkflowContractIssue struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

// WorkflowContractValidationError 收集全部问题后一次性抛出。
type WorkflowContractValidationError struct{ Issues []WorkflowContractIssue }

func (e *WorkflowContractValidationError) Error() string {
	messages := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		messages = append(messages, issue.Code+" at "+issue.Path)
	}
	return strings.Join(messages, "; ")
}

var workflowSchemaTypes = map[string]bool{
	"array": true, "boolean": true, "integer": true, "null": true, "number": true, "object": true, "string": true,
}

var workflowResourceKeys = map[string]bool{
	"resource_ref": true, "resource_refs": true, "resource_path": true,
	"asset_ref": true, "asset_refs": true, "asset_path": true,
	"media_asset_id": true, "media_asset_ids": true,
}

var workflowWindowsAbsoluteRe = regexp.MustCompile(`^(?:[A-Za-z]:[/\\]|\\\\)`)

// ValidateWorkflowTemplateContract 校验模板元数据、节点 schema、引用与图安全性。
//
// nodes / edges 由调用方注入 contract（Python 侧用关键字参数覆盖同名键），
// 允许在没有落库前先校验待提交的图。
func ValidateWorkflowTemplateContract(
	contract map[string]any, nodes []map[string]any, edges []map[string]any,
	allowedExecutorIDs, availableCapabilities map[string]bool,
) (map[string]any, error) {
	issues := make([]WorkflowContractIssue, 0)
	addIssue := func(code, path, message string) {
		issues = append(issues, WorkflowContractIssue{Code: code, Path: path, Message: message})
	}

	payload := map[string]any{}
	for key, value := range contract {
		payload[key] = value
	}
	nodeValues := make([]any, 0, len(nodes))
	for _, node := range nodes {
		nodeValues = append(nodeValues, node)
	}
	edgeValues := make([]any, 0, len(edges))
	for _, edge := range edges {
		edgeValues = append(edgeValues, edge)
	}
	if nodes != nil {
		payload["nodes"] = nodeValues
	}
	if edges != nil {
		payload["edges"] = edgeValues
	}

	generationMode := payload["generation_mode"]
	if generationMode != nil {
		if text, ok := generationMode.(string); !ok || !domain.WorkflowGenerationModes[text] {
			addIssue("invalid_generation_mode", "$.generation_mode", "generation_mode is unsupported")
		}
	}
	if payload["input_schema"] != nil {
		validateJSONSchema(payload["input_schema"], "$.input_schema", 0, addIssue)
	}
	for _, field := range []string{"risk_tags", "required_capabilities", "provider_requirements"} {
		if payload[field] != nil {
			validateStringSequence(payload[field], "$."+field, addIssue)
		}
	}
	for _, field := range []string{"copyright_declaration", "author", "license", "example_project", "cover_ref"} {
		if value := payload[field]; value != nil {
			if _, ok := value.(string); !ok {
				addIssue("invalid_contract", "$."+field, field+" must be a string")
			}
		}
	}
	for _, field := range []string{"estimated_episode_cost", "estimated_cost"} {
		if value := payload[field]; value != nil {
			if number, ok := contractFloat(value); !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
				addIssue("invalid_cost", "$."+field, "cost must be a finite non-negative number")
			}
		}
	}

	nodeList, _ := payload["nodes"].([]any)
	if payload["nodes"] != nil {
		if _, ok := payload["nodes"].([]any); !ok {
			addIssue("nodes_not_array", "$.nodes", "nodes must be an array")
			nodeList = nil
		}
	}
	edgeList, _ := payload["edges"].([]any)
	if payload["edges"] != nil {
		if _, ok := payload["edges"].([]any); !ok {
			addIssue("edges_not_array", "$.edges", "edges must be an array")
			edgeList = nil
		}
	}

	nodeIDSet := map[string]bool{}
	nodeIDs := make([]string, 0, len(nodeList))
	for index, raw := range nodeList {
		path := "$.nodes[" + itoa(index) + "]"
		node, ok := raw.(map[string]any)
		if !ok {
			addIssue("node_not_object", path, "node must be an object")
			continue
		}
		nodeID, ok := firstString(node, "node_key", "id", "node_id")
		if !ok || strings.TrimSpace(nodeID) == "" {
			addIssue("node_id_required", path+".id", "node id must be a non-empty string")
			continue
		}
		if nodeIDSet[nodeID] {
			addIssue("duplicate_node_id", path+".id", "duplicate node id: "+nodeID)
		} else {
			nodeIDSet[nodeID] = true
			nodeIDs = append(nodeIDs, nodeID)
		}
		for _, schemaField := range []string{"input_schema", "output_schema"} {
			if node[schemaField] != nil {
				validateJSONSchema(node[schemaField], path+"."+schemaField, 0, addIssue)
			}
		}
		validateNodeContract(node, path, addIssue, allowedExecutorIDs, availableCapabilities)
		validateResourceReferences(node, path, addIssue)
	}

	adjacency := map[string]map[string]bool{}
	indegree := map[string]int{}
	for id := range nodeIDSet {
		adjacency[id] = map[string]bool{}
		indegree[id] = 0
	}
	seenEdges := map[string]bool{}
	missingEndpoints := make([]any, 0)
	for index, raw := range edgeList {
		path := "$.edges[" + itoa(index) + "]"
		edge, ok := raw.(map[string]any)
		if !ok {
			addIssue("edge_not_object", path, "edge must be an object")
			continue
		}
		source := edgeEndpoint(edge, "source")
		target := edgeEndpoint(edge, "target")
		if source == "" || target == "" {
			addIssue("edge_endpoint_required", path, "edge needs source and target node ids")
			continue
		}
		missing := make([]string, 0, 2)
		for _, id := range []string{source, target} {
			if !nodeIDSet[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			missingEndpoints = append(missingEndpoints, map[string]any{
				"edge_index": index, "missing": missing,
			})
			addIssue("missing_edge_endpoint", path, "unknown node id: "+strings.Join(missing, ", "))
			continue
		}
		pair := source + "->" + target
		if seenEdges[pair] {
			addIssue("duplicate_edge", path, "duplicate edge: "+source+" -> "+target)
			continue
		}
		seenEdges[pair] = true
		adjacency[source][target] = true
		indegree[target]++
	}

	queue := make([]string, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		if indegree[id] == 0 {
			queue = append(queue, id)
		}
	}
	visited := map[string]bool{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		visited[current] = true
		targets := make([]string, 0, len(adjacency[current]))
		for target := range adjacency[current] {
			targets = append(targets, target)
		}
		sort.Strings(targets)
		for _, target := range targets {
			indegree[target]--
			if indegree[target] == 0 {
				queue = append(queue, target)
			}
		}
	}
	if len(visited) != len(nodeIDSet) {
		cycle := make([]string, 0, len(nodeIDs))
		for _, id := range nodeIDs {
			if !visited[id] {
				cycle = append(cycle, id)
			}
		}
		addIssue("workflow_cycle_detected", "$.edges", "workflow graph contains a cycle: "+strings.Join(cycle, ", "))
	}

	if len(issues) > 0 {
		return nil, &WorkflowContractValidationError{Issues: issues}
	}
	return map[string]any{
		"valid":             true,
		"node_count":        len(nodeIDs),
		"edge_count":        len(seenEdges),
		"missing_endpoints": missingEndpoints,
	}, nil
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func firstString(m map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if text, ok := m[key].(string); ok {
			return text, true
		}
	}
	return "", false
}

func contractFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	}
	return 0, false
}

func validateNodeContract(
	node map[string]any,
	path string,
	addIssue func(code, p, message string),
	allowedExecutorIDs, availableCapabilities map[string]bool,
) {
	if executorID := node["executor_id"]; executorID != nil {
		text, ok := executorID.(string)
		if !ok || strings.TrimSpace(text) == "" {
			addIssue("invalid_executor_id", path+".executor_id", "executor_id must be a non-empty string")
		} else if allowedExecutorIDs != nil && !allowedExecutorIDs[text] {
			addIssue("unknown_executor_id", path+".executor_id", "executor is not allowed: "+text)
		}
	}
	if capabilities := node["required_capabilities"]; capabilities != nil {
		validateStringSequence(capabilities, path+".required_capabilities", addIssue)
		if list, ok := capabilities.([]any); ok && availableCapabilities != nil {
			unavailable := make([]string, 0)
			seen := map[string]bool{}
			for _, item := range list {
				if text, isString := item.(string); isString && !availableCapabilities[text] && !seen[text] {
					seen[text] = true
					unavailable = append(unavailable, text)
				}
			}
			if len(unavailable) > 0 {
				sort.Strings(unavailable)
				addIssue("capability_unavailable", path+".required_capabilities",
					"capabilities are unavailable: "+strings.Join(unavailable, ", "))
			}
		}
	}
	if cost := node["estimated_cost"]; cost != nil {
		if number, ok := contractFloat(cost); !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
			addIssue("invalid_cost", path+".estimated_cost", "estimated_cost must be a finite non-negative number")
		}
	}
	for _, field := range []string{"cache_policy", "approval_policy"} {
		if value := node[field]; value != nil {
			if _, isString := value.(string); !isString {
				if _, isMap := value.(map[string]any); !isMap {
					addIssue("invalid_node_policy", path+"."+field, "policy must be a string or object")
				}
			}
		}
	}
}

func validateJSONSchema(value any, path string, depth int, addIssue func(code, p, message string)) {
	if depth > 32 {
		addIssue("schema_too_deep", path, "schema nesting exceeds the supported limit")
		return
	}
	schema, ok := value.(map[string]any)
	if !ok {
		addIssue("schema_not_object", path, "JSON Schema must be an object")
		return
	}
	if raw := schema["type"]; raw != nil {
		valid := false
		if text, isString := raw.(string); isString && workflowSchemaTypes[text] {
			valid = true
		}
		if list, isList := raw.([]any); isList && len(list) > 0 {
			valid = true
			for _, item := range list {
				text, isString := item.(string)
				if !isString || !workflowSchemaTypes[text] {
					valid = false
					break
				}
			}
		}
		if !valid {
			addIssue("invalid_schema_type", path+".type", "type contains an unsupported JSON Schema type")
		}
	}
	if properties := schema["properties"]; properties != nil {
		props, isMap := properties.(map[string]any)
		if !isMap {
			addIssue("schema_properties_not_object", path+".properties", "properties must be an object")
		} else {
			names := make([]string, 0, len(props))
			for name := range props {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if name == "" {
					addIssue("invalid_schema_property", path+".properties", "property names must be strings")
					continue
				}
				validateJSONSchema(props[name], path+".properties."+name, depth+1, addIssue)
			}
		}
	}
	if required := schema["required"]; required != nil {
		validateStringSequence(required, path+".required", addIssue)
		if list, isList := required.([]any); isList {
			if props, isMap := schema["properties"].(map[string]any); isMap {
				for _, raw := range list {
					if name, isString := raw.(string); isString {
						if _, exists := props[name]; !exists {
							addIssue("schema_required_property_missing", path+".required",
								"unknown required property: "+name)
						}
					}
				}
			}
		}
	}
	if items := schema["items"]; items != nil {
		if _, isList := items.([]any); isList {
			addIssue("schema_items_array_unsupported", path+".items", "tuple item schemas are not supported")
		} else {
			validateJSONSchema(items, path+".items", depth+1, addIssue)
		}
	}
	if enum := schema["enum"]; enum != nil {
		list, isList := enum.([]any)
		if !isList || len(list) == 0 {
			addIssue("invalid_schema_enum", path+".enum", "enum must be a non-empty array")
		}
	}
	if ref := schema["$ref"]; ref != nil {
		text, isString := ref.(string)
		if !isString || !strings.HasPrefix(text, "#") {
			addIssue("external_schema_ref", path+".$ref", "external schema references are not allowed")
		}
	}
}

func validateResourceReferences(value any, path string, addIssue func(code, p, message string)) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			childPath := path + "." + key
			if workflowResourceKeys[key] {
				validateResourceValue(typed[key], childPath, addIssue)
			} else {
				switch typed[key].(type) {
				case map[string]any, []any:
					validateResourceReferences(typed[key], childPath, addIssue)
				}
			}
		}
	case []any:
		for index, child := range typed {
			validateResourceReferences(child, path+"["+itoa(index)+"]", addIssue)
		}
	}
}

func validateResourceValue(value any, path string, addIssue func(code, p, message string)) {
	var refs []any
	switch typed := value.(type) {
	case string:
		refs = []any{typed}
	case []any:
		refs = typed
	default:
		addIssue("invalid_resource_reference", path, "resource reference must be a string or array")
		return
	}
	_, isList := value.([]any)
	for index, raw := range refs {
		refPath := path
		if isList {
			refPath = path + "[" + itoa(index) + "]"
		}
		text, ok := raw.(string)
		if !ok || strings.TrimSpace(text) == "" {
			addIssue("invalid_resource_reference", refPath, "resource reference must be a non-empty string")
			continue
		}
		normalized := strings.ReplaceAll(text, "\\", "/")
		if workflowWindowsAbsoluteRe.MatchString(text) || strings.HasPrefix(normalized, "/") ||
			strings.Contains(normalized, "://") {
			addIssue("unsafe_resource_reference", refPath, "absolute paths and URLs are not allowed")
		}
		for _, part := range strings.Split(normalized, "/") {
			if part == ".." {
				addIssue("unsafe_resource_reference", refPath, "parent traversal is not allowed")
				break
			}
		}
	}
}

func validateStringSequence(value any, path string, addIssue func(code, p, message string)) {
	list, ok := value.([]any)
	if !ok {
		addIssue("string_array_required", path, "value must be an array of non-empty strings")
		return
	}
	for _, item := range list {
		text, isString := item.(string)
		if !isString || strings.TrimSpace(text) == "" {
			addIssue("string_array_required", path, "value must be an array of non-empty strings")
			return
		}
	}
}

var workflowEdgeEndpointAliases = map[string][]string{
	"source": {"source_node_key", "source", "from", "source_node_id"},
	"target": {"target_node_key", "target", "to", "target_node_id"},
}

func edgeEndpoint(edge map[string]any, side string) string {
	for _, key := range workflowEdgeEndpointAliases[side] {
		if text, ok := edge[key].(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}
