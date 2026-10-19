// 角色关系图的数据契约与 AI/人工合并规则（纯逻辑）。对应 lib/character_relations.py。
//
// 关系数据挂在项目级：它描述故事而不是单个角色资产。校验与合并语义集中在这里，
// 供路由、AI 分析与改名/删角色流程共用同一套规则。
package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RelationshipTypes 是关系边的类型枚举（与 Python RELATIONSHIP_TYPES 同源）。
var RelationshipTypes = []string{
	"family", "romance", "marriage", "friend", "ally", "enemy",
	"mentor", "subordinate", "rival", "interest", "custom",
}

// 关系图分析状态取值。
const (
	RelationStatusIdle      = "idle"
	RelationStatusAnalyzing = "analyzing"
	RelationStatusReady     = "ready"
	RelationStatusFailed    = "failed"
)

// 关系边来源取值。
const (
	RelationOriginAI     = "ai"
	RelationOriginManual = "manual"
)

// 字符串字段上限：超限时截断而不是报错——project.json 可被用户手工编辑，
// 一个超长的 label 不该让整张关系图读不出来。Python 侧靠写入时的校验保证存量数据不越界。
const (
	relationMaxNameLen        = 120
	relationMaxTypeLen        = 32
	relationMaxLabelLen       = 80
	relationMaxDescriptionLen = 1000
	relationMaxIDLen          = 128
	relationMaxErrorLen       = 1000
	relationMaxFileLen        = 255
	relationMaxExcerptLen     = 500
	relationMaxEvidenceCount  = 8
)

// relationTopKeys 是关系图的顶层字段白名单（对应 pydantic extra="forbid"）。
var relationTopKeys = map[string]bool{
	"revision": true, "analysis_status": true, "analyzed_at": true,
	"source_fingerprint": true, "error": true, "edges": true,
	"suppressed_pairs": true, "node_positions": true,
}

// RelationEvidence 是支撑一条 AI 关系的紧凑剧本引用。
type RelationEvidence struct {
	Episode    *int    `json:"episode"`
	ScriptFile *string `json:"script_file"`
	SceneID    *string `json:"scene_id"`
	Excerpt    string  `json:"excerpt"`
}

// RelationEdge 是关系图中的一条可见关系线。
type RelationEdge struct {
	ID             string             `json:"id"`
	Source         string             `json:"source"`
	Target         string             `json:"target"`
	Type           string             `json:"type"`
	Label          string             `json:"label"`
	Directed       bool               `json:"directed"`
	Description    string             `json:"description"`
	Origin         string             `json:"origin"`
	ManualOverride bool               `json:"manual_override"`
	Confidence     *float64           `json:"confidence"`
	Evidence       []RelationEvidence `json:"evidence"`
}

// RelationPosition 是一个角色节点的画布坐标。
type RelationPosition struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// SuppressedPair 是被用户删除、重新分析时不应复活的 AI 关系对。
type SuppressedPair struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	Directed bool   `json:"directed"`
}

// RelationsData 是落盘的角色关系图。
type RelationsData struct {
	Revision          int
	AnalysisStatus    string
	AnalyzedAt        *string
	SourceFingerprint *string
	Error             *string
	Edges             []RelationEdge
	SuppressedPairs   []SuppressedPair
	NodePositions     map[string]RelationPosition
}

// relationPair 是关系对的身份，刻意与关系类型无关。
type relationPair struct {
	source   string
	target   string
	directed bool
}

// relationFullKey 在关系对之上叠加类型，用于探测同对同类型的重复边。
type relationFullKey struct {
	pair relationPair
	kind string
}

// relationPairKey 返回一对角色的规范身份：名称先落 NFC 坐标系，非有向对按名称排序。
func relationPairKey(source, target string, directed bool) relationPair {
	sourceKey := NormalizeAssetName(source)
	targetKey := NormalizeAssetName(target)
	if !directed && sourceKey > targetKey {
		sourceKey, targetKey = targetKey, sourceKey
	}
	return relationPair{source: sourceKey, target: targetKey, directed: directed}
}

// resolveRelationCharacter 把关系边上的名字解析到项目里的真实 key。
func resolveRelationCharacter(characters AssetBucket, name string) (string, error) {
	resolved, ok := characters.ResolveKey(name)
	if !ok {
		return "", fmt.Errorf("relationship references unknown character: %s", name)
	}
	return resolved, nil
}

// NormalizeCharacterRelations 校验落盘数据，并把每条引用解析到项目 key。
//
// value 为 nil 时按空图处理。未知字段、非法状态、自环、重复 id、同对同类型的重复边都报错。
func NormalizeCharacterRelations(value map[string]any, characters AssetBucket) (RelationsData, error) {
	for key := range value {
		if !relationTopKeys[key] {
			return RelationsData{}, fmt.Errorf("character relations: 未知字段 %q", key)
		}
	}
	out := RelationsData{
		AnalysisStatus: RelationStatusIdle,
		NodePositions:  map[string]RelationPosition{},
	}
	if v, ok := value["revision"]; ok {
		n, ok := relationInt(v)
		if !ok || n < 0 {
			return RelationsData{}, fmt.Errorf("character relations: revision 必须是非负整数，收到 %v", v)
		}
		out.Revision = n
	}
	if v, ok := value["analysis_status"]; ok {
		s, ok := v.(string)
		if !ok || !isRelationStatus(s) {
			return RelationsData{}, fmt.Errorf("character relations: analysis_status 非法，收到 %v", v)
		}
		out.AnalysisStatus = s
	}
	if out.AnalyzedAt, _ = relationOptionalString(value["analyzed_at"]); !relationIsNil(value["analyzed_at"]) {
		if _, ok := value["analyzed_at"].(string); !ok {
			return RelationsData{}, fmt.Errorf("character relations: analyzed_at 必须是字符串或 null")
		}
	}
	if out.SourceFingerprint, _ = relationOptionalString(value["source_fingerprint"]); !relationIsNil(value["source_fingerprint"]) {
		if _, ok := value["source_fingerprint"].(string); !ok {
			return RelationsData{}, fmt.Errorf("character relations: source_fingerprint 必须是字符串或 null")
		}
	}
	if raw, ok := value["error"]; ok && !relationIsNil(raw) {
		s, isStr := raw.(string)
		if !isStr {
			return RelationsData{}, fmt.Errorf("character relations: error 必须是字符串或 null")
		}
		s = truncateRunes(s, relationMaxErrorLen)
		out.Error = &s
	}

	ids := map[string]bool{}
	relationKeys := map[relationFullKey]bool{}
	rawEdges, err := relationList(value["edges"], "edges")
	if err != nil {
		return RelationsData{}, err
	}
	for _, raw := range rawEdges {
		edge, err := ParseRelationEdge(raw)
		if err != nil {
			return RelationsData{}, err
		}
		source, err := resolveRelationCharacter(characters, edge.Source)
		if err != nil {
			return RelationsData{}, err
		}
		target, err := resolveRelationCharacter(characters, edge.Target)
		if err != nil {
			return RelationsData{}, err
		}
		if NormalizeAssetName(source) == NormalizeAssetName(target) {
			return RelationsData{}, fmt.Errorf("a relationship cannot connect a character to itself")
		}
		if ids[edge.ID] {
			return RelationsData{}, fmt.Errorf("duplicate relationship id: %s", edge.ID)
		}
		ids[edge.ID] = true
		full := relationFullKey{pair: relationPairKey(source, target, edge.Directed), kind: edge.Type}
		if relationKeys[full] {
			return RelationsData{}, fmt.Errorf("duplicate relationship for the same character pair and type")
		}
		relationKeys[full] = true
		edge.Source, edge.Target = source, target
		out.Edges = append(out.Edges, edge)
	}

	seenSuppressed := map[relationPair]bool{}
	rawSuppressed, err := relationList(value["suppressed_pairs"], "suppressed_pairs")
	if err != nil {
		return RelationsData{}, err
	}
	for _, raw := range rawSuppressed {
		pair, err := parseSuppressedPair(raw)
		if err != nil {
			return RelationsData{}, err
		}
		source, err := resolveRelationCharacter(characters, pair.Source)
		if err != nil {
			return RelationsData{}, err
		}
		target, err := resolveRelationCharacter(characters, pair.Target)
		if err != nil {
			return RelationsData{}, err
		}
		if NormalizeAssetName(source) == NormalizeAssetName(target) {
			continue
		}
		key := relationPairKey(source, target, pair.Directed)
		if seenSuppressed[key] {
			continue
		}
		seenSuppressed[key] = true
		out.SuppressedPairs = append(out.SuppressedPairs, SuppressedPair{Source: source, Target: target, Directed: pair.Directed})
	}

	positions, err := relationPositions(value["node_positions"])
	if err != nil {
		return RelationsData{}, err
	}
	for name, pos := range positions {
		if resolved, ok := characters.ResolveKey(name); ok {
			out.NodePositions[resolved] = pos
		}
	}
	return out, nil
}

// RelationsPayload 把关系图转成可写入 project.json 的 JSON 结构。
func RelationsPayload(data RelationsData) map[string]any {
	payload := map[string]any{
		"revision":           data.Revision,
		"analysis_status":    data.AnalysisStatus,
		"analyzed_at":        nil,
		"source_fingerprint": nil,
		"error":              nil,
		"edges":              []any{},
		"suppressed_pairs":   []any{},
		"node_positions":     map[string]any{},
	}
	if data.AnalyzedAt != nil {
		payload["analyzed_at"] = *data.AnalyzedAt
	}
	if data.SourceFingerprint != nil {
		payload["source_fingerprint"] = *data.SourceFingerprint
	}
	if data.Error != nil {
		payload["error"] = *data.Error
	}
	edges := make([]any, 0, len(data.Edges))
	for _, edge := range data.Edges {
		edges = append(edges, relationEdgePayload(edge))
	}
	payload["edges"] = edges
	suppressed := make([]any, 0, len(data.SuppressedPairs))
	for _, pair := range data.SuppressedPairs {
		suppressed = append(suppressed, map[string]any{
			"source": pair.Source, "target": pair.Target, "directed": pair.Directed,
		})
	}
	payload["suppressed_pairs"] = suppressed
	positions := map[string]any{}
	for name, pos := range data.NodePositions {
		positions[name] = map[string]any{"x": pos.X, "y": pos.Y}
	}
	payload["node_positions"] = positions
	return payload
}

// relationEdgePayload 展开单条边（evidence 逐项展开，缺省值为 null）。
func relationEdgePayload(edge RelationEdge) map[string]any {
	evidence := make([]any, 0, len(edge.Evidence))
	for _, e := range edge.Evidence {
		evidence = append(evidence, map[string]any{
			"episode":     relationDeref(e.Episode),
			"script_file": relationDeref(e.ScriptFile),
			"scene_id":    relationDeref(e.SceneID),
			"excerpt":     e.Excerpt,
		})
	}
	return map[string]any{
		"id": edge.ID, "source": edge.Source, "target": edge.Target, "type": edge.Type,
		"label": edge.Label, "directed": edge.Directed, "description": edge.Description,
		"origin": edge.Origin, "manual_override": edge.ManualOverride,
		"confidence": relationDeref(edge.Confidence), "evidence": evidence,
	}
}

// relationDeref 把可选值指针展开成裸值（nil 指针展开为 nil）。
//
// 展开后是值而不是指针：MergeAIRelationships 会把自己展开的 payload 再解析一遍，
// 指针落进 map 会被解析层按「类型不符」拒掉。
func relationDeref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// MarkAnalysisStarted 把关系图标记为分析中。
func MarkAnalysisStarted(existing map[string]any, characters AssetBucket) (RelationsData, error) {
	data, err := NormalizeCharacterRelations(existing, characters)
	if err != nil {
		return RelationsData{}, err
	}
	data.AnalysisStatus = RelationStatusAnalyzing
	data.Error = nil
	return data, nil
}

// MarkAnalysisFailed 把关系图标记为失败并记录原因。
func MarkAnalysisFailed(existing map[string]any, characters AssetBucket, message string) (RelationsData, error) {
	data, err := NormalizeCharacterRelations(existing, characters)
	if err != nil {
		return RelationsData{}, err
	}
	data.AnalysisStatus = RelationStatusFailed
	truncated := truncateRunes(message, relationMaxErrorLen)
	data.Error = &truncated
	return data, nil
}

// MergeAIRelationships 用新一轮 AI 结果替换未被人工触碰的 AI 边，
// 保留人工新增/修改的边与被删除对的抑制记录。
func MergeAIRelationships(
	existing map[string]any, characters AssetBucket, aiEdges []RelationEdge, sourceFingerprint string,
) (RelationsData, error) {
	current, err := NormalizeCharacterRelations(existing, characters)
	if err != nil {
		return RelationsData{}, err
	}

	var manualEdges []RelationEdge
	protected := map[relationPair]bool{}
	for _, edge := range current.Edges {
		if edge.Origin == RelationOriginManual || edge.ManualOverride {
			manualEdges = append(manualEdges, edge)
			protected[relationPairKey(edge.Source, edge.Target, edge.Directed)] = true
		}
	}
	suppressed := map[relationPair]bool{}
	for _, pair := range current.SuppressedPairs {
		suppressed[relationPairKey(pair.Source, pair.Target, pair.Directed)] = true
	}

	var merged []RelationEdge
	seen := map[relationFullKey]bool{}
	for _, raw := range aiEdges {
		edge := raw
		edge.ID = uuid.NewString()
		edge.Origin = RelationOriginAI
		edge.ManualOverride = false
		// 单边走一次完整校验：名字解析、自环、类型枚举、evidence 上限都在这里兜住。
		single, err := NormalizeCharacterRelations(
			map[string]any{"edges": []any{relationEdgePayload(edge)}}, characters,
		)
		if err != nil {
			return RelationsData{}, err
		}
		if len(single.Edges) == 0 {
			continue
		}
		normalized := single.Edges[0]
		pair := relationPairKey(normalized.Source, normalized.Target, normalized.Directed)
		full := relationFullKey{pair: pair, kind: normalized.Type}
		if protected[pair] || suppressed[pair] || seen[full] {
			continue
		}
		seen[full] = true
		merged = append(merged, normalized)
	}

	now := formatRelationTimestamp(time.Now())
	current.Revision++
	current.AnalysisStatus = RelationStatusReady
	current.AnalyzedAt = &now
	current.SourceFingerprint = &sourceFingerprint
	current.Error = nil
	current.Edges = append(manualEdges, merged...)
	return current, nil
}

// RelationshipSourceFingerprint 对「应当让上一次 AI 分析失效」的数据求摘要。
//
// 摘要只在同一运行时内自比（写入后由下次分析比对），不追求与 Python 逐字节一致。
func RelationshipSourceFingerprint(project map[string]any, scripts []map[string]any) string {
	overview, _ := project["overview"].(map[string]any)
	if overview == nil {
		overview = map[string]any{}
	}
	characters, _ := project["characters"].(map[string]any)
	if characters == nil {
		characters = map[string]any{}
	}
	payload := map[string]any{"overview": overview, "characters": characters, "scripts": scripts}
	return sha256Hex(canonicalJSON(payload))
}

// canonicalJSON 输出键序稳定、不转义 HTML 的紧凑 JSON。
func canonicalJSON(value any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// relationIsNil 判定值是否为 nil，含「接口里装着类型化 nil 指针」的情形
// （MergeAIRelationships 回读自己展开的 payload 时，*float64 / *int 的 nil 会以此形态出现）。
func relationIsNil(raw any) bool {
	if raw == nil {
		return true
	}
	v := reflect.ValueOf(raw)
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice:
		return v.IsNil()
	}
	return false
}

// --- 解析辅助 ---

// ParseRelationEdge 校验并展开一条边。未知字段报错（对应 extra="forbid"）。
func ParseRelationEdge(raw any) (RelationEdge, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return RelationEdge{}, fmt.Errorf("character relations: edges 元素必须是对象")
	}
	for key := range m {
		switch key {
		case "id", "source", "target", "type", "label", "directed", "description",
			"origin", "manual_override", "confidence", "evidence":
		default:
			return RelationEdge{}, fmt.Errorf("relationship edge: 未知字段 %q", key)
		}
	}
	edge := RelationEdge{
		ID: uuid.NewString(), Type: "custom", Origin: RelationOriginAI,
		Evidence: []RelationEvidence{},
	}
	if v, ok := m["id"]; ok {
		s, isStr := v.(string)
		if !isStr || s == "" {
			return RelationEdge{}, fmt.Errorf("relationship edge: id 必须是非空字符串")
		}
		edge.ID = truncateRunes(s, relationMaxIDLen)
	}
	if edge.Source, ok = relationRequiredString(m["source"], "source"); !ok {
		return RelationEdge{}, fmt.Errorf("relationship edge: source 必须是非空字符串")
	}
	if edge.Target, ok = relationRequiredString(m["target"], "target"); !ok {
		return RelationEdge{}, fmt.Errorf("relationship edge: target 必须是非空字符串")
	}
	edge.Source = truncateRunes(edge.Source, relationMaxNameLen)
	edge.Target = truncateRunes(edge.Target, relationMaxNameLen)
	if v, ok := m["type"]; ok {
		s, isStr := v.(string)
		if !isStr {
			return RelationEdge{}, fmt.Errorf("relationship edge: type 必须是字符串")
		}
		s = strings.TrimSpace(s)
		if s != "" {
			if !containsString(RelationshipTypes, s) {
				return RelationEdge{}, fmt.Errorf("unknown relationship type: %s", s)
			}
			edge.Type = truncateRunes(s, relationMaxTypeLen)
		}
	}
	if v, ok := m["label"]; ok {
		edge.Label = truncateRunes(relationOptionalText(v), relationMaxLabelLen)
	}
	if v, ok := m["description"]; ok {
		edge.Description = truncateRunes(relationOptionalText(v), relationMaxDescriptionLen)
	}
	if v, ok := m["directed"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return RelationEdge{}, fmt.Errorf("relationship edge: directed 必须是布尔值")
		}
		edge.Directed = b
	}
	if v, ok := m["origin"]; ok {
		s, isStr := v.(string)
		if !isStr || (s != RelationOriginAI && s != RelationOriginManual) {
			return RelationEdge{}, fmt.Errorf("relationship edge: origin 只能是 ai 或 manual")
		}
		edge.Origin = s
	}
	if v, ok := m["manual_override"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return RelationEdge{}, fmt.Errorf("relationship edge: manual_override 必须是布尔值")
		}
		edge.ManualOverride = b
	}
	if v, ok := m["confidence"]; ok && !relationIsNil(v) {
		f, isNum := relationFloat(v)
		if !isNum || f < 0 || f > 1 {
			return RelationEdge{}, fmt.Errorf("relationship edge: confidence 必须是 0 到 1 之间的数或 null")
		}
		edge.Confidence = &f
	}
	evidence, err := relationList(m["evidence"], "evidence")
	if err != nil {
		return RelationEdge{}, err
	}
	if len(evidence) > relationMaxEvidenceCount {
		return RelationEdge{}, fmt.Errorf("relationship edge: evidence 最多 %d 条", relationMaxEvidenceCount)
	}
	for _, item := range evidence {
		parsed, err := parseRelationEvidence(item)
		if err != nil {
			return RelationEdge{}, err
		}
		edge.Evidence = append(edge.Evidence, parsed)
	}
	return edge, nil
}

// parseRelationEvidence 校验并展开一条证据引用。
func parseRelationEvidence(raw any) (RelationEvidence, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return RelationEvidence{}, fmt.Errorf("relationship evidence 必须是对象")
	}
	for key := range m {
		switch key {
		case "episode", "script_file", "scene_id", "excerpt":
		default:
			return RelationEvidence{}, fmt.Errorf("relationship evidence: 未知字段 %q", key)
		}
	}
	out := RelationEvidence{}
	if v, ok := m["episode"]; ok && !relationIsNil(v) {
		n, isInt := relationInt(v)
		if !isInt || n < 1 {
			return RelationEvidence{}, fmt.Errorf("relationship evidence: episode 必须是不小于 1 的整数或 null")
		}
		out.Episode = &n
	}
	if s, err := relationOptionalBoundedString(m["script_file"], relationMaxFileLen); err != nil {
		return RelationEvidence{}, err
	} else {
		out.ScriptFile = s
	}
	if s, err := relationOptionalBoundedString(m["scene_id"], relationMaxFileLen); err != nil {
		return RelationEvidence{}, err
	} else {
		out.SceneID = s
	}
	if v, ok := m["excerpt"]; ok {
		out.Excerpt = truncateRunes(relationOptionalText(v), relationMaxExcerptLen)
	}
	return out, nil
}

// parseSuppressedPair 校验并展开一条抑制记录。
func parseSuppressedPair(raw any) (SuppressedPair, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return SuppressedPair{}, fmt.Errorf("suppressed_pairs 元素必须是对象")
	}
	for key := range m {
		switch key {
		case "source", "target", "directed":
		default:
			return SuppressedPair{}, fmt.Errorf("suppressed pair: 未知字段 %q", key)
		}
	}
	out := SuppressedPair{}
	var okSource, okTarget bool
	if out.Source, okSource = relationRequiredString(m["source"], "source"); !okSource {
		return SuppressedPair{}, fmt.Errorf("suppressed pair: source 必须是非空字符串")
	}
	if out.Target, okTarget = relationRequiredString(m["target"], "target"); !okTarget {
		return SuppressedPair{}, fmt.Errorf("suppressed pair: target 必须是非空字符串")
	}
	out.Source = truncateRunes(out.Source, relationMaxNameLen)
	out.Target = truncateRunes(out.Target, relationMaxNameLen)
	if v, ok := m["directed"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return SuppressedPair{}, fmt.Errorf("suppressed pair: directed 必须是布尔值")
		}
		out.Directed = b
	}
	return out, nil
}

// relationPositions 解析节点坐标表；未知字段报错。
func relationPositions(raw any) (map[string]RelationPosition, error) {
	out := map[string]RelationPosition{}
	if relationIsNil(raw) {
		return out, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("character relations: node_positions 必须是对象")
	}
	for name, value := range m {
		pos, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("node position %q 必须是对象", name)
		}
		for key := range pos {
			if key != "x" && key != "y" {
				return nil, fmt.Errorf("node position: 未知字段 %q", key)
			}
		}
		parsed := RelationPosition{}
		if v, ok := pos["x"]; ok {
			f, isNum := relationFloat(v)
			if !isNum {
				return nil, fmt.Errorf("node position %q: x 必须是数值", name)
			}
			parsed.X = f
		}
		if v, ok := pos["y"]; ok {
			f, isNum := relationFloat(v)
			if !isNum {
				return nil, fmt.Errorf("node position %q: y 必须是数值", name)
			}
			parsed.Y = f
		}
		out[name] = parsed
	}
	return out, nil
}

// relationList 取一个列表字段；缺省为空列表，形态不符报错。
func relationList(raw any, field string) ([]any, error) {
	if relationIsNil(raw) {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("character relations: %s 必须是数组", field)
	}
	return list, nil
}

// relationRequiredString 取必填字符串并去除首尾空白。
func relationRequiredString(raw any, field string) (string, bool) {
	s, ok := raw.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	return s, s != ""
}

// relationOptionalText 取可选文本，非字符串按空串处理。
func relationOptionalText(raw any) string {
	s, ok := raw.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// relationOptionalString 取可选字符串（null / 缺省为 nil）。
func relationOptionalString(raw any) (*string, bool) {
	s, ok := raw.(string)
	if !ok {
		return nil, false
	}
	return &s, true
}

// relationOptionalBoundedString 取可选字符串并截断到上限；类型不符报错。
func relationOptionalBoundedString(raw any, limit int) (*string, error) {
	if relationIsNil(raw) {
		return nil, nil
	}
	s, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("relationship evidence: 字段必须是字符串或 null")
	}
	s = truncateRunes(s, limit)
	return &s, nil
}

// relationInt 接受 JSON 数字或数字字符串（模型常把数字加引号）。
func relationInt(raw any) (int, bool) {
	switch v := raw.(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, false
		}
		return int(v), true
	case int:
		return v, true
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err != nil {
			return 0, false
		}
		if fmt.Sprintf("%d", n) != strings.TrimSpace(v) {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// relationFloat 接受 JSON 数字或数字字符串。
func relationFloat(raw any) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%g", &f); err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func isRelationStatus(s string) bool {
	switch s {
	case RelationStatusIdle, RelationStatusAnalyzing, RelationStatusReady, RelationStatusFailed:
		return true
	}
	return false
}

// 关系图的长度上限统一走 task_failure.go 的 truncateRunes（按字符截断）。

// formatRelationTimestamp 输出与 Python datetime.isoformat() 同形的 UTC 时间戳。
func formatRelationTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

// userVisibleEdge 返回边的「用户可见五元组」：这五项任一被用户改动即视为人工编辑。
func userVisibleEdge(edge RelationEdge) string {
	return strings.Join([]string{edge.Source, edge.Target, edge.Type, edge.Label, boolText(edge.Directed), edge.Description}, "\x00")
}

func boolText(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

// ApplyManualRelationships 把画布快照落库并识别用户的编辑 / 删除。
//
// 与 AI 合并规则（MergeAIRelationships）互补的两条语义：
//   - 未改动的 AI 边保持 AI 所有；编辑过或新建的边转为 manual（manual_override=true），
//     后续 AI 刷新不得覆盖；
//   - 删除一条 AI 边写入 pair 级 suppressed 记录，重新分析时该对不复活。
//
// submittedEdges 是画布提交的全量边（已按 ParseRelationEdge 解析）。
func ApplyManualRelationships(
	existing map[string]any, characters AssetBucket, submittedEdges []RelationEdge,
) (RelationsData, error) {
	current, err := NormalizeCharacterRelations(existing, characters)
	if err != nil {
		return RelationsData{}, err
	}
	oldByID := map[string]RelationEdge{}
	for _, edge := range current.Edges {
		oldByID[edge.ID] = edge
	}
	submittedIDs := map[string]bool{}
	normalized := make([]RelationEdge, 0, len(submittedEdges))
	for _, edge := range submittedEdges {
		submittedIDs[edge.ID] = true
		previous, known := oldByID[edge.ID]
		switch {
		case !known:
			// 新建边一律归人工。
			edge.Origin = "manual"
			edge.ManualOverride = true
			edge.Confidence = nil
		case userVisibleEdge(previous) != userVisibleEdge(edge):
			// 编辑过：转人工，但保留原 AI 置信度供 UI 展示。
			edge.Origin = "manual"
			edge.ManualOverride = true
			edge.Confidence = previous.Confidence
		default:
			edge = previous
		}
		normalized = append(normalized, edge)
	}

	suppressions := map[relationPair]SuppressedPair{}
	for _, pair := range current.SuppressedPairs {
		suppressions[relationPairKey(pair.Source, pair.Target, pair.Directed)] = pair
	}
	for _, previous := range current.Edges {
		if submittedIDs[previous.ID] || previous.Origin != "ai" || previous.ManualOverride {
			continue
		}
		// 删除的纯 AI 边：登记抑制对，防止下次分析静默复活。
		suppressions[relationPairKey(previous.Source, previous.Target, previous.Directed)] = SuppressedPair{
			Source:   previous.Source,
			Target:   previous.Target,
			Directed: previous.Directed,
		}
	}
	currentPairs := map[relationPair]bool{}
	for _, edge := range normalized {
		currentPairs[relationPairKey(edge.Source, edge.Target, edge.Directed)] = true
	}
	filtered := make([]SuppressedPair, 0, len(suppressions))
	for key, pair := range suppressions {
		if !currentPairs[key] {
			filtered = append(filtered, pair)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Source != filtered[j].Source {
			return filtered[i].Source < filtered[j].Source
		}
		return filtered[i].Target < filtered[j].Target
	})
	current.Revision++
	current.Edges = normalized
	current.SuppressedPairs = filtered
	return current, nil
}
