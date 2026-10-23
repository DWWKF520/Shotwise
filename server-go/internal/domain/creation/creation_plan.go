package creation

// 单次创作执行计划的不可变领域模型（对应 lib/creation_plan.py）。
//
// 项目独占 content_mode / generation_mode / grid_storyboard：CreationPlan 只能
// 快照这三个值，不能提供替代值。这是领域层的硬边界——compile 接口刻意不接受
// 任何 *_override 入参，也不接受 generation_mode 参数。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// CreationPlanError 是项目上下文无法产出合法计划时的错误。
type CreationPlanError struct{ Message string }

func (e *CreationPlanError) Error() string { return e.Message }

func creationPlanErrorf(format string, args ...any) error {
	return &CreationPlanError{Message: fmt.Sprintf(format, args...)}
}

// CreationPlanForbiddenOverrides 是计划拒绝接受的模式覆盖键。
var CreationPlanForbiddenOverrides = []string{
	"generation_mode_override", "content_mode_override", "grid_storyboard_override",
}

// ProjectContextSnapshot 是从项目捕获的只读生成规则。
type ProjectContextSnapshot struct {
	ProjectID      string
	ContentMode    string
	GenerationMode string
	GridStoryboard bool
	AspectRatio    string
	Style          string
	ModelConfig    map[string]any
}

// ProjectContextSnapshotFromProject 从项目映射捕获其独占字段。
func ProjectContextSnapshotFromProject(projectID string, project map[string]any) (*ProjectContextSnapshot, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, creationPlanErrorf("project_id must be a non-empty string")
	}
	contentMode, ok := project["content_mode"].(string)
	if !ok && project["content_mode"] != nil {
		return nil, creationPlanErrorf("Project content_mode must be a string")
	}
	if contentMode == "" {
		contentMode = "drama"
	}
	generationMode, isString := project["generation_mode"].(string)
	if !isString {
		return nil, creationPlanErrorf("Project generation_mode must be a string")
	}
	gridStoryboard, isBool := project["grid_storyboard"].(bool)
	if !isBool && project["grid_storyboard"] != nil {
		return nil, creationPlanErrorf("Project grid_storyboard must be a boolean")
	}
	aspectRatio := ""
	if value, ok := project["aspect_ratio"].(string); ok {
		aspectRatio = value
	} else if project["aspect_ratio"] != nil {
		return nil, creationPlanErrorf("Project aspect_ratio must be a string")
	}
	style := ""
	if value, ok := project["style"].(string); ok {
		style = value
	} else if project["style"] != nil {
		return nil, creationPlanErrorf("Project style must be a string")
	}
	rawModelConfig := project["model_settings"]
	if rawModelConfig == nil {
		rawModelConfig = project["model_config"]
	}
	if rawModelConfig == nil {
		rawModelConfig = map[string]any{}
	}
	modelConfig, err := freezeCreationValue(rawModelConfig)
	if err != nil {
		return nil, err
	}
	frozen, isMap := modelConfig.(map[string]any)
	if !isMap {
		return nil, creationPlanErrorf("Project model_config must be a mapping")
	}
	snapshot := &ProjectContextSnapshot{
		ProjectID:      projectID,
		ContentMode:    contentMode,
		GenerationMode: generationMode,
		GridStoryboard: gridStoryboard,
		AspectRatio:    aspectRatio,
		Style:          style,
		ModelConfig:    frozen,
	}
	if err := snapshot.validate(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *ProjectContextSnapshot) validate() error {
	if strings.TrimSpace(s.ProjectID) == "" {
		return creationPlanErrorf("project_id must be a non-empty string")
	}
	if !WorkflowContentModes[s.ContentMode] {
		return creationPlanErrorf("unsupported content_mode: %s", s.ContentMode)
	}
	if !WorkflowGenerationModes[s.GenerationMode] {
		return creationPlanErrorf("unsupported generation_mode: %s", s.GenerationMode)
	}
	return nil
}

// ToDict 输出可比较、可持久化的字典视图。
func (s *ProjectContextSnapshot) ToDict() map[string]any {
	return map[string]any{
		"project_id":      s.ProjectID,
		"content_mode":    s.ContentMode,
		"generation_mode": s.GenerationMode,
		"grid_storyboard": s.GridStoryboard,
		"aspect_ratio":    s.AspectRatio,
		"style":           s.Style,
		"model_config":    thawCreationValue(s.ModelConfig),
	}
}

// CompatibilityCheck 是计划编译时记录的兼容性结论。
type CompatibilityCheck struct {
	Compatible bool
	Reasons    []string
}

func (c CompatibilityCheck) validate() error {
	seen := map[string]bool{}
	for _, reason := range c.Reasons {
		if seen[reason] {
			return creationPlanErrorf("compatibility reasons must be unique and ordered")
		}
		seen[reason] = true
	}
	if c.Compatible && len(c.Reasons) > 0 {
		return creationPlanErrorf("a compatible plan cannot contain incompatibility reasons")
	}
	if !c.Compatible && len(c.Reasons) == 0 {
		return creationPlanErrorf("an incompatible plan must record at least one reason")
	}
	return nil
}

// CreationPlan 是不可变、确定性的单 Skill 执行计划。
type CreationPlan struct {
	PlanID           string
	SkillID          string
	WorkflowRevision string
	ProjectContext   *ProjectContextSnapshot
	SkillInputs      map[string]any
	Compatibility    CompatibilityCheck
	Fingerprint      string
	SkillMaintainer  string
}

// CompileCreationPlanParams 是 CompileCreationPlan 的入参。
type CompileCreationPlanParams struct {
	PlanID                   string
	SkillID                  string
	WorkflowRevision         string
	ProjectContext           *ProjectContextSnapshot
	SkillInputs              map[string]any
	SupportedContentModes    map[string]bool
	SupportedGenerationModes map[string]bool
}

// CompileCreationPlan 从项目快照与 Skill 自有输入编译计划。
//
// generation_mode 刻意不是入参：调用方只能从 ProjectContextSnapshot 取值，
// 从接口上杜绝第二个真相源。
func CompileCreationPlan(p CompileCreationPlanParams) (*CreationPlan, error) {
	if strings.TrimSpace(p.PlanID) == "" {
		return nil, creationPlanErrorf("plan_id must be a non-empty string")
	}
	if strings.TrimSpace(p.SkillID) == "" {
		return nil, creationPlanErrorf("skill_id must be a non-empty string")
	}
	if strings.TrimSpace(p.WorkflowRevision) == "" {
		return nil, creationPlanErrorf("workflow_revision must be a non-empty string")
	}
	frozenInputs, err := freezeCreationValue(p.SkillInputs)
	if err != nil {
		return nil, err
	}
	inputs, isMap := frozenInputs.(map[string]any)
	if !isMap {
		return nil, creationPlanErrorf("skill_inputs must be a mapping")
	}
	forbidden := make([]string, 0)
	for _, key := range CreationPlanForbiddenOverrides {
		if _, exists := p.SkillInputs[key]; exists {
			forbidden = append(forbidden, key)
		}
	}
	if len(forbidden) > 0 {
		sort.Strings(forbidden)
		return nil, creationPlanErrorf("CreationPlan does not accept mode overrides: %s", strings.Join(forbidden, ", "))
	}
	reasons := make([]string, 0, 2)
	if p.SupportedContentModes != nil && !p.SupportedContentModes[p.ProjectContext.ContentMode] {
		reasons = append(reasons, "content_mode_not_supported")
	}
	if p.SupportedGenerationModes != nil && !p.SupportedGenerationModes[p.ProjectContext.GenerationMode] {
		reasons = append(reasons, "generation_mode_not_supported")
	}
	compatibility := CompatibilityCheck{Compatible: len(reasons) == 0, Reasons: reasons}
	if err := compatibility.validate(); err != nil {
		return nil, err
	}
	fingerprint, err := creationPlanFingerprint(
		p.SkillID, p.WorkflowRevision, p.ProjectContext, inputs, compatibility,
	)
	if err != nil {
		return nil, err
	}
	return &CreationPlan{
		PlanID:           p.PlanID,
		SkillID:          p.SkillID,
		WorkflowRevision: p.WorkflowRevision,
		ProjectContext:   p.ProjectContext,
		SkillInputs:      inputs,
		Compatibility:    compatibility,
		Fingerprint:      fingerprint,
		SkillMaintainer:  "official",
	}, nil
}

// IsCompatible 报告计划是否兼容当前项目。
func (p *CreationPlan) IsCompatible() bool { return p.Compatibility.Compatible }

// RequireCompatible 不兼容时报错。
func (p *CreationPlan) RequireCompatible() error {
	if p.IsCompatible() {
		return nil
	}
	return creationPlanErrorf("CreationPlan is incompatible: %s", strings.Join(p.Compatibility.Reasons, ", "))
}

// AssertProjectMatches 项目任何独占设置变化后拒绝启动旧计划。
func (p *CreationPlan) AssertProjectMatches(project map[string]any) error {
	current, err := ProjectContextSnapshotFromProject(p.ProjectContext.ProjectID, project)
	if err != nil {
		return err
	}
	equal, err := snapshotsEqual(current, p.ProjectContext)
	if err != nil {
		return err
	}
	if !equal {
		return creationPlanErrorf("Project changed after CreationPlan preview; compile a new plan")
	}
	return nil
}

// ToDict 输出序列化视图；源项目不受影响。
func (p *CreationPlan) ToDict() map[string]any {
	reasons := make([]any, 0, len(p.Compatibility.Reasons))
	for _, reason := range p.Compatibility.Reasons {
		reasons = append(reasons, reason)
	}
	return map[string]any{
		"plan_id":           p.PlanID,
		"skill_id":          p.SkillID,
		"workflow_revision": p.WorkflowRevision,
		"project_context":   p.ProjectContext.ToDict(),
		"skill_inputs":      thawCreationValue(p.SkillInputs),
		"compatibility": map[string]any{
			"compatible": p.Compatibility.Compatible,
			"reasons":    reasons,
		},
		"fingerprint":      p.Fingerprint,
		"skill_maintainer": p.SkillMaintainer,
	}
}

func snapshotsEqual(a, b *ProjectContextSnapshot) (bool, error) {
	left, err := WorkflowCanonicalJSON(a.ToDict())
	if err != nil {
		return false, err
	}
	right, err := WorkflowCanonicalJSON(b.ToDict())
	if err != nil {
		return false, err
	}
	return left == right, nil
}

func creationPlanFingerprint(
	skillID, workflowRevision string,
	context *ProjectContextSnapshot,
	inputs map[string]any,
	compatibility CompatibilityCheck,
) (string, error) {
	reasons := make([]any, 0, len(compatibility.Reasons))
	for _, reason := range compatibility.Reasons {
		reasons = append(reasons, reason)
	}
	payload := map[string]any{
		"skill_id":          skillID,
		"workflow_revision": workflowRevision,
		"project_context":   context.ToDict(),
		"skill_inputs":      thawCreationValue(inputs),
		"compatibility": map[string]any{
			"compatible": compatibility.Compatible,
			"reasons":    reasons,
		},
	}
	encoded, err := WorkflowCanonicalJSON(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:]), nil
}

// freezeCreationValue 校验输入只包含 JSON 原生类型，并深拷贝一份。
//
// Python 侧用 tuple 承载冻结映射以保证不可变；Go 侧靠"编译后不再改动"这条约定，
// 但键必须是 string、值必须是 JSON 原生类型，否则 canonical JSON 会漂移。
func freezeCreationValue(value any) (any, error) {
	switch typed := value.(type) {
	case nil, bool, string:
		return value, nil
	case int:
		return value, nil
	case int64:
		return value, nil
	case float64:
		return value, nil
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			frozen, err := freezeCreationValue(item)
			if err != nil {
				return nil, err
			}
			out[key] = frozen
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			frozen, err := freezeCreationValue(item)
			if err != nil {
				return nil, err
			}
			out = append(out, frozen)
		}
		return out, nil
	case []string:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, item)
		}
		return out, nil
	default:
		// 具名 / 泛型容器（map[string]string、[]string 之类）按 JSON 的形状展开，
		// 与 Python 侧「只要是 Mapping / Sequence 就冻结」的口径一致。
		reflected := reflect.ValueOf(value)
		switch reflected.Kind() {
		case reflect.Map:
			if reflected.Type().Key().Kind() != reflect.String {
				return nil, creationPlanErrorf("skill inputs contain unsupported value: %T", value)
			}
			out := make(map[string]any, reflected.Len())
			iter := reflected.MapRange()
			for iter.Next() {
				frozen, err := freezeCreationValue(iter.Value().Interface())
				if err != nil {
					return nil, err
				}
				out[iter.Key().String()] = frozen
			}
			return out, nil
		case reflect.Slice, reflect.Array:
			out := make([]any, 0, reflected.Len())
			for i := 0; i < reflected.Len(); i++ {
				frozen, err := freezeCreationValue(reflected.Index(i).Interface())
				if err != nil {
					return nil, err
				}
				out = append(out, frozen)
			}
			return out, nil
		}
		return nil, creationPlanErrorf("skill inputs contain unsupported value: %T", value)
	}
}

// thawCreationValue 把冻结值还原成普通 JSON 值（当前实现即原样返回）。
func thawCreationValue(value any) any { return value }

// CreationPlanStableID 由稳定载荷算出计划 ID。
func CreationPlanStableID(payload map[string]any) (string, error) {
	encoded, err := WorkflowCanonicalJSON(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(encoded))
	return "cp_" + hex.EncodeToString(sum[:]), nil
}
