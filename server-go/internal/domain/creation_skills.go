package domain

// 官方创作 Skill 目录（对应 lib/creation_skills.py）。
//
// Creation Skills 是产品能力，不是 agent_runtime_profile/.claude/skills 下的
// Agent Skill；首期没有用户创作、发布或市场界面，目录为内置静态数据。

import (
	"sort"
	"strings"
)

// CreationScope 是 Skill 的作用域取值。
const (
	CreationScopeProject   = "project"
	CreationScopeEpisode   = "episode"
	CreationScopeSelection = "selection"
)

// CreationSkillCompatibility 描述一个创作 Skill 接受的项目模式与输入。
type CreationSkillCompatibility struct {
	ContentModes    map[string]bool
	GenerationModes map[string]bool
	RequiredInputs  map[string]bool
	Scopes          map[string]bool
	// GridStoryboards 非 nil 时限定项目宫格开关取值。
	GridStoryboards    map[bool]bool
	hasGridStoryboards bool
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

func boolSet(values []bool) map[bool]bool {
	out := make(map[bool]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

func sortedBoolSetKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Check 返回不兼容原因；兼容时返回空。
func (c *CreationSkillCompatibility) Check(project map[string]any, availableInputs map[string]bool) string {
	if !c.ContentModes[projectString(project, "content_mode")] {
		return "content_mode_incompatible"
	}
	if !c.GenerationModes[projectString(project, "generation_mode")] {
		return "generation_mode_incompatible"
	}
	if c.hasGridStoryboards && !c.GridStoryboards[projectBool(project, "grid_storyboard")] {
		return "grid_storyboard_incompatible"
	}
	var missing []string
	for input := range c.RequiredInputs {
		if !availableInputs[input] {
			missing = append(missing, input)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return "missing_inputs:" + strings.Join(missing, ",")
	}
	return ""
}

// Explain 输出稳定的、不含用户内容的兼容性结果（供 UI 与分析用）。
func (c *CreationSkillCompatibility) Explain(project map[string]any, availableInputs map[string]bool) map[string]any {
	contentMode := project["content_mode"]
	generationMode := project["generation_mode"]
	var missing []string
	for input := range c.RequiredInputs {
		if !availableInputs[input] {
			missing = append(missing, input)
		}
	}
	sort.Strings(missing)
	var reasons []string
	if !c.ContentModes[projectString(project, "content_mode")] {
		reasons = append(reasons, "content_mode_incompatible")
	}
	if !c.GenerationModes[projectString(project, "generation_mode")] {
		reasons = append(reasons, "generation_mode_incompatible")
	}
	if c.hasGridStoryboards && !c.GridStoryboards[projectBool(project, "grid_storyboard")] {
		reasons = append(reasons, "grid_storyboard_incompatible")
	}
	if len(missing) > 0 {
		reasons = append(reasons, "missing_inputs:"+strings.Join(missing, ","))
	}
	var supportedGrid any
	if c.hasGridStoryboards {
		values := make([]any, 0, len(c.GridStoryboards))
		if c.GridStoryboards[false] {
			values = append(values, false)
		}
		if c.GridStoryboards[true] {
			values = append(values, true)
		}
		supportedGrid = values
	}
	requiresNewProject := false
	for _, reason := range reasons {
		if reason == "generation_mode_incompatible" {
			requiresNewProject = true
		}
	}
	return map[string]any{
		"compatible":                 len(reasons) == 0,
		"project_content_mode":       contentMode,
		"project_generation_mode":    generationMode,
		"supported_content_modes":    sortedBoolSetKeys(c.ContentModes),
		"supported_generation_modes": sortedBoolSetKeys(c.GenerationModes),
		"grid_storyboard":            project["grid_storyboard"],
		"supported_grid_storyboards": supportedGrid,
		"reasons":                    reasons,
		"requires_new_project":       requiresNewProject,
	}
}

func projectString(project map[string]any, key string) string {
	if s, ok := project[key].(string); ok {
		return s
	}
	return ""
}

func projectBool(project map[string]any, key string) bool {
	if b, ok := project[key].(bool); ok {
		return b
	}
	return false
}

// CreationSkillVersion 是钉在发布模板别名上的不可变官方版本。
type CreationSkillVersion struct {
	ID                            string
	SkillID                       string
	Version                       int64
	Title                         string
	Summary                       string
	Category                      string
	WorkflowTemplateRevisionAlias string
	Compatibility                 *CreationSkillCompatibility
	ExpectedOutputs               []string
	ReviewRequired                bool
	EstimatedCostHint             *string
	WorkflowRevisionID            *string
}

// CreationSkillDefinition 是一个官方产品能力的稳定公开身份。
type CreationSkillDefinition struct {
	ID            string
	Slug          string
	LatestVersion CreationSkillVersion
	Official      bool
	Active        bool
}

// CompatibilityReport 构造结构化的不兼容响应，不暴露用户内容。
func CompatibilityReport(
	skill *CreationSkillDefinition, project map[string]any, availableInputs map[string]bool,
) map[string]any {
	result := skill.LatestVersion.Compatibility.Explain(project, availableInputs)
	var conflictReason any
	if reasons, ok := result["reasons"].([]string); ok && len(reasons) > 0 {
		conflictReason = reasons[0]
	}
	result["skill_id"] = skill.ID
	result["skill_version_id"] = skill.LatestVersion.ID
	result["alternative_skills"] = []string{}
	result["conflict_reason"] = conflictReason
	return result
}

// SkillCheck 是 ListOfficialCreationSkills 的一项：Skill 及其兼容结果。
type SkillCheck struct {
	Skill  *CreationSkillDefinition
	Reason string // 空串表示兼容
}

// ListOfficialCreationSkills 列出激活的官方 Skill 及其兼容性结果。
func ListOfficialCreationSkills(project map[string]any, availableInputs map[string]bool) []SkillCheck {
	out := make([]SkillCheck, 0, len(OfficialCreationSkills))
	for i := range OfficialCreationSkills {
		skill := &OfficialCreationSkills[i]
		if !skill.Active {
			continue
		}
		out = append(out, SkillCheck{Skill: skill, Reason: skill.LatestVersion.Compatibility.Check(project, availableInputs)})
	}
	return out
}

// OfficialCreationSkills 是内置的官方创作 Skill 目录（顺序与 Python 侧一致）。
var OfficialCreationSkills = buildOfficialCreationSkills()

type skillSpec struct {
	id             string
	title          string
	summary        string
	category       string
	contentModes   []string
	genModes       []string
	requiredInputs []string
	scopes         []string
	outputs        []string
	reviewRequired bool
	grid           []bool
}

func buildOfficialCreationSkills() []CreationSkillDefinition {
	specs := []skillSpec{
		{
			id: "novel-to-drama", title: "小说转连续短剧",
			summary:        "将文稿编排为角色、场景、分镜和剧集视频。",
			category:       "剧集",
			contentModes:   []string{"drama"},
			genModes:       []string{"storyboard"},
			requiredInputs: []string{"document"},
			scopes:         []string{CreationScopeProject},
			outputs:        []string{"script", "characters", "scenes", "storyboards", "videos"},
		},
		{
			id: "novel-to-narration", title: "小说转旁白解说",
			summary:        "将文稿整理为旁白驱动的视频剧集。",
			category:       "解说",
			contentModes:   []string{"narration"},
			genModes:       []string{"storyboard"},
			requiredInputs: []string{"document"},
			scopes:         []string{CreationScopeProject},
			outputs:        []string{"script", "storyboards", "narration", "videos"},
		},
		{
			id: "ad-brief-to-video", title: "广告 Brief 转分镜视频",
			summary:        "根据 Brief 与商品素材生成广告镜头。",
			category:       "广告",
			contentModes:   []string{"ad"},
			genModes:       []string{"storyboard"},
			requiredInputs: []string{"brief"},
			scopes:         []string{CreationScopeProject},
			outputs:        []string{"shots", "storyboards", "videos"},
		},
		{
			id: "reference-image-video", title: "参考图生成视频",
			summary:        "使用项目资产图作为参考生成视频单元。",
			category:       "视频",
			contentModes:   []string{"drama", "narration", "ad"},
			genModes:       []string{"reference_video"},
			requiredInputs: []string{"image"},
			scopes:         []string{CreationScopeEpisode, CreationScopeSelection},
			outputs:        []string{"videos"},
		},
		{
			id: "grid-storyboard-batch", title: "宫格分镜批量生成",
			summary:        "按项目宫格策略批量生成分镜。",
			category:       "分镜",
			contentModes:   []string{"drama", "narration"},
			genModes:       []string{"storyboard"},
			requiredInputs: []string{"document"},
			scopes:         []string{CreationScopeEpisode},
			outputs:        []string{"storyboards"},
			grid:           []bool{true},
		},
		{
			id: "character-consistency", title: "角色定妆与一致性测试",
			summary:        "生成角色定妆图和一致性参考。",
			category:       "角色",
			contentModes:   []string{"drama", "narration", "ad"},
			genModes:       []string{"storyboard", "reference_video"},
			requiredInputs: []string{"image"},
			scopes:         []string{CreationScopeSelection},
			outputs:        []string{"images"},
			reviewRequired: true,
		},
		{
			id: "redraw-storyboard", title: "选中分镜重绘",
			summary:        "按选中的分镜和参考重绘画面。",
			category:       "分镜",
			contentModes:   []string{"drama", "narration", "ad"},
			genModes:       []string{"storyboard"},
			requiredInputs: []string{"image"},
			scopes:         []string{CreationScopeSelection},
			outputs:        []string{"storyboards"},
		},
		{
			id: "redub-video", title: "选中视频重新配音",
			summary:        "为选中的视频重新生成旁白或对白音轨。",
			category:       "音频",
			contentModes:   []string{"drama", "narration", "ad"},
			genModes:       []string{"storyboard", "reference_video"},
			requiredInputs: []string{"video"},
			scopes:         []string{CreationScopeSelection},
			outputs:        []string{"audio", "videos"},
		},
		{
			id: "recommended-story-starter", title: "一键故事起步",
			summary:        "从一份文稿快速整理出可继续创作的故事、角色和分镜起点。",
			category:       "推荐",
			contentModes:   []string{"drama", "narration", "ad"},
			genModes:       []string{"storyboard"},
			requiredInputs: []string{"document"},
			scopes:         []string{CreationScopeProject},
			outputs:        []string{"script", "characters", "scenes", "storyboards"},
		},
		{
			id: "recommended-idea-to-video", title: "灵感到成片路线",
			summary:        "把一段创作灵感整理成清晰的视频制作路线和首批镜头。",
			category:       "推荐",
			contentModes:   []string{"drama", "narration", "ad"},
			genModes:       []string{"storyboard", "reference_video"},
			requiredInputs: nil,
			scopes:         []string{CreationScopeProject},
			outputs:        []string{"script", "shots", "videos"},
		},
		{
			id: "cinematic-shot-design", title: "电影感镜头设计",
			summary:        "为故事场景规划景别、机位、运动和镜头之间的视觉节奏。",
			category:       "专业影视",
			contentModes:   []string{"drama", "narration", "ad"},
			genModes:       []string{"storyboard"},
			requiredInputs: []string{"document"},
			scopes:         []string{CreationScopeEpisode},
			outputs:        []string{"shots", "storyboards"},
		},
		{
			id: "character-look-development", title: "角色视觉开发",
			summary:        "围绕角色参考图生成定妆方向，并检查主要视觉特征的一致性。",
			category:       "专业影视",
			contentModes:   []string{"drama", "narration", "ad"},
			genModes:       []string{"storyboard", "reference_video"},
			requiredInputs: []string{"image"},
			scopes:         []string{CreationScopeSelection},
			outputs:        []string{"images"},
			reviewRequired: true,
		},
		{
			id: "commerce-product-film", title: "商品广告片",
			summary:        "根据广告 Brief 和商品素材规划完整的产品展示视频。",
			category:       "商业广告",
			contentModes:   []string{"ad"},
			genModes:       []string{"storyboard"},
			requiredInputs: []string{"brief", "image"},
			scopes:         []string{CreationScopeProject},
			outputs:        []string{"shots", "storyboards", "videos"},
			reviewRequired: true,
		},
		{
			id: "commerce-vertical-ad", title: "竖屏转化广告",
			summary:        "把卖点和行动号召组织成适合移动端投放的竖屏广告镜头。",
			category:       "商业广告",
			contentModes:   []string{"ad"},
			genModes:       []string{"storyboard"},
			requiredInputs: []string{"brief"},
			scopes:         []string{CreationScopeProject},
			outputs:        []string{"shots", "storyboards", "videos"},
		},
		{
			id: "short-drama-hook", title: "短剧开场钩子",
			summary:        "强化短剧前几场的冲突、悬念和追更动机，快速建立故事节奏。",
			category:       "短剧爽剧",
			contentModes:   []string{"drama"},
			genModes:       []string{"storyboard"},
			requiredInputs: []string{"document"},
			scopes:         []string{CreationScopeProject},
			outputs:        []string{"script", "storyboards", "videos"},
		},
		{
			id: "short-drama-cliffhanger", title: "爽剧反转分镜",
			summary:        "围绕反转、升级和结尾悬念拆解可连续生成的短剧镜头。",
			category:       "短剧爽剧",
			contentModes:   []string{"drama"},
			genModes:       []string{"storyboard"},
			requiredInputs: []string{"document"},
			scopes:         []string{CreationScopeEpisode},
			outputs:        []string{"script", "shots", "storyboards"},
		},
		{
			id: "anime-world-builder", title: "动漫世界观设定",
			summary:        "从故事文稿提炼角色、场景和视觉规则，建立可持续扩展的动漫世界。",
			category:       "动漫游戏",
			contentModes:   []string{"drama", "narration"},
			genModes:       []string{"storyboard"},
			requiredInputs: []string{"document"},
			scopes:         []string{CreationScopeProject},
			outputs:        []string{"characters", "scenes", "images"},
		},
		{
			id: "music-video-beatboard", title: "音乐 MV 节拍分镜",
			summary:        "按歌曲段落和情绪变化规划音乐 MV 的视觉节拍与镜头段落。",
			category:       "音乐MV",
			contentModes:   []string{"ad"},
			genModes:       []string{"storyboard"},
			requiredInputs: []string{"brief"},
			scopes:         []string{CreationScopeProject},
			outputs:        []string{"shots", "storyboards", "videos"},
		},
		{
			id: "creator-short-form", title: "自媒体短视频脚本",
			summary:        "将内容素材整理成适合自媒体发布的旁白、镜头和节奏方案。",
			category:       "自媒体创作",
			contentModes:   []string{"narration"},
			genModes:       []string{"storyboard"},
			requiredInputs: []string{"document"},
			scopes:         []string{CreationScopeProject},
			outputs:        []string{"script", "storyboards", "narration", "videos"},
		},
		{
			id: "universal-content-recut", title: "内容再创作助手",
			summary:        "把现有内容改造成另一种适合当前项目的表达结构和视频方案。",
			category:       "通用技能",
			contentModes:   []string{"drama", "narration", "ad"},
			genModes:       []string{"storyboard", "reference_video"},
			requiredInputs: nil,
			scopes:         []string{CreationScopeProject, CreationScopeEpisode},
			outputs:        []string{"script", "shots", "videos"},
		},
		{
			id: "discover-reference-look", title: "参考视觉探索",
			summary:        "从选中的参考图出发探索不同的视觉方向，并生成可继续制作的视频单元。",
			category:       "发现",
			contentModes:   []string{"drama", "narration", "ad"},
			genModes:       []string{"reference_video"},
			requiredInputs: []string{"image"},
			scopes:         []string{CreationScopeSelection},
			outputs:        []string{"videos"},
		},
	}
	out := make([]CreationSkillDefinition, 0, len(specs))
	for _, spec := range specs {
		version := CreationSkillVersion{
			ID:                            spec.id + ":v1",
			SkillID:                       spec.id,
			Version:                       1,
			Title:                         spec.title,
			Summary:                       spec.summary,
			Category:                      spec.category,
			WorkflowTemplateRevisionAlias: "official:" + spec.id,
			Compatibility: &CreationSkillCompatibility{
				ContentModes:    stringSet(spec.contentModes),
				GenerationModes: stringSet(spec.genModes),
				RequiredInputs:  stringSet(spec.requiredInputs),
				Scopes:          stringSet(spec.scopes),
			},
			ExpectedOutputs: spec.outputs,
			ReviewRequired:  spec.reviewRequired,
		}
		if spec.grid != nil {
			version.Compatibility.GridStoryboards = boolSet(spec.grid)
			version.Compatibility.hasGridStoryboards = true
		}
		out = append(out, CreationSkillDefinition{
			ID: spec.id, Slug: spec.id, LatestVersion: version, Official: true, Active: true,
		})
	}
	return out
}

// CreationSkillByID 按 ID 查官方目录（对应 _skill_by_id 的查找部分）。
func CreationSkillByID(skillID string) *CreationSkillDefinition {
	for i := range OfficialCreationSkills {
		if OfficialCreationSkills[i].ID == skillID {
			return &OfficialCreationSkills[i]
		}
	}
	return nil
}
