package project

import "github.com/shotwise/shotwise/internal/agent/tools"

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/domain"
)

// AssetWriter 是 patch_project 资产分支所需的写入能力（由 service.AssetService 满足）。
type AssetWriter interface {
	Upsert(project string, kind domain.AssetKind, rawName string, fields map[string]any) (created bool, err error)
}

// ProjectWriter 是 patch_project 设置/概述分支所需的 project.json 读改写能力（由 service.ProjectStore 满足）。
type ProjectWriter interface {
	Update(name string, mutate func(*domain.ProjectFile) error) error
}


// patchSettingsWhitelist 是顶层 settings 白名单（与 Python _SETTINGS_WHITELIST 一致）。
var patchSettingsWhitelist = []string{
	"episode_target_units", "source_language", "brief",
	"planning_window_chars", "planning_max_episodes", "narration_voice", "narration_speed",
}

// patchOverviewFields 是项目概述可编辑字段（merge 语义）。
var patchOverviewFields = []string{"synopsis", "genre", "theme", "world_setting"}

type patchProjectIn struct {
	Table    string                    `json:"table,omitempty" jsonschema:"description=资产表：characters/scenes/props/products（资产 upsert 分支，需与 entries 同时给出）"`
	Entries  map[string]map[string]any `json:"entries,omitempty" jsonschema:"description=资产 upsert：{名称: {description 等字段}}；已存在则合并改字段"`
	Settings map[string]any            `json:"settings,omitempty" jsonschema:"description=顶层设置写入：白名单字段 episode_target_units/source_language/brief/planning_window_chars/planning_max_episodes/narration_voice/narration_speed，值为 null 时清除"`
	Overview map[string]any            `json:"overview,omitempty" jsonschema:"description=项目概述 merge 编辑：synopsis/genre/theme/world_setting，只改传入字段"`
}

// NewPatchProjectTool 构造 patch_project 工具，project 名闭包绑定（防 prompt injection 改项目）。
func NewPatchProjectTool(project string, assets AssetWriter, projects ProjectWriter) (tool.InvokableTool, error) {
	return toolutils.InferTool("patch_project",
		"新增或修改 project.json：(1) 资产 upsert（table+entries）；(2) 顶层 settings 写入；(3) 项目概述 overview 编辑。三种形态三选一。",
		func(ctx context.Context, in patchProjectIn) (string, error) {
			hasUpsert := in.Table != "" || in.Entries != nil
			hasSettings := in.Settings != nil
			hasOverview := in.Overview != nil
			n := 0
			for _, b := range []bool{hasUpsert, hasSettings, hasOverview} {
				if b {
					n++
				}
			}
			switch {
			case n > 1:
				return tools.DenyEnvelope("table/entries、settings、overview 三选一，不能同时给出多个"), nil
			case n == 0:
				return tools.DenyEnvelope("必须提供 table+entries（资产 upsert）、settings（顶层字段）或 overview（项目概述）之一"), nil
			}

			var text string
			var err error
			switch {
			case hasOverview:
				text, err = applyOverview(projects, project, in.Overview)
			case hasSettings:
				text, err = applySettings(projects, project, in.Settings)
			default:
				text, err = applyUpsert(assets, project, in.Table, in.Entries)
			}
			if err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			return tools.OKEnvelope(text), nil
		})
}

// applyUpsert 资产分支：逐条 upsert，字段限定在 spec 允许范围内（sheet 字段由生成流水线回写，禁止手动设置）。
func applyUpsert(assets AssetWriter, project, table string, entries map[string]map[string]any) (string, error) {
	kind, ok := tools.PatchTableKinds[table]
	if !ok {
		return "", fmt.Errorf("table 必须是 characters/scenes/props/products 之一，收到 %q", table)
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("entries 必须是非空 { 名称: 字段对象 } 映射")
	}
	if assets == nil {
		return "", fmt.Errorf("asset writer not configured")
	}
	allowed := allowedAssetFields(kind)

	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	var added, merged []string
	var dropped []string
	for _, name := range names {
		fields := map[string]any{}
		for k, v := range entries[name] {
			if allowed[k] {
				fields[k] = v
			} else {
				dropped = append(dropped, name+"."+k)
			}
		}
		created, err := assets.Upsert(project, kind, name, fields)
		if err != nil {
			return "", fmt.Errorf("%s %q: %w", table, name, err)
		}
		if created {
			added = append(added, name)
		} else {
			merged = append(merged, name)
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ %s: ", table)
	var parts []string
	if len(added) > 0 {
		parts = append(parts, fmt.Sprintf("新增 %d 个: %s", len(added), strings.Join(added, ", ")))
	}
	if len(merged) > 0 {
		parts = append(parts, fmt.Sprintf("合并改字段 %d 个: %s", len(merged), strings.Join(merged, ", ")))
	}
	sb.WriteString(strings.Join(parts, "; "))
	if len(dropped) > 0 {
		fmt.Fprintf(&sb, "\n⚠️  以下字段不在 agent 可编辑范围，已忽略 → %s", strings.Join(dropped, ", "))
		sb.WriteString("\n   说明: reference_image 由用户上传/系统管理；*_sheet 由资产生成流水线回写，不可手动设置。")
	}
	return sb.String(), nil
}

// allowedAssetFields 返回 agent 可通过 patch_project 写入的资产字段：description + spec 登记的额外字段，排除 sheet 字段。
func allowedAssetFields(kind domain.AssetKind) map[string]bool {
	allow := map[string]bool{"description": true}
	if spec, ok := domain.Spec(kind); ok {
		for _, f := range spec.ExtraStringFields {
			allow[f] = true
		}
		for _, f := range spec.ExtraListFields {
			allow[f] = true
		}
		delete(allow, spec.SheetKey)
	}
	return allow
}

// applyOverview 概述分支：字段白名单 + 字符串类型校验，merge 语义写入 project.json。
func applyOverview(projects ProjectWriter, project string, overview map[string]any) (string, error) {
	if len(overview) == 0 {
		return "", fmt.Errorf("overview 必须是非空 { 字段名: 值 } 映射")
	}
	values := make(map[string]string, len(overview))
	for _, key := range tools.SortedKeys(overview) {
		if !tools.Contains(patchOverviewFields, key) {
			return "", fmt.Errorf("overview 字段 %q 不在白名单 %v 内", key, patchOverviewFields)
		}
		s, ok := overview[key].(string)
		if !ok {
			return "", fmt.Errorf("overview 字段 %q 的值必须是字符串，收到 %v", key, overview[key])
		}
		values[key] = s
	}
	if projects == nil {
		return "", fmt.Errorf("project writer not configured")
	}

	set := map[string]string{}
	err := projects.Update(project, func(pf *domain.ProjectFile) error {
		if pf.Overview == nil {
			pf.Overview = &domain.Overview{}
		}
		for key, value := range values {
			cur := overviewField(pf.Overview, key)
			if *cur == value {
				set[key] = "noop"
				continue
			}
			*cur = value
			set[key] = "set"
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return renderDiagnostics("overview", set), nil
}

func overviewField(o *domain.Overview, key string) *string {
	switch key {
	case "synopsis":
		return &o.Synopsis
	case "genre":
		return &o.Genre
	case "theme":
		return &o.Theme
	default:
		return &o.WorldSetting
	}
}

// applySettings 设置分支：先整体校验（失败不落盘），再在项目锁内 RMW。
func applySettings(projects ProjectWriter, project string, settings map[string]any) (string, error) {
	if len(settings) == 0 {
		return "", fmt.Errorf("settings 必须是非空 { 字段名: 值 } 映射")
	}
	setters := make(map[string]func(*domain.ProjectFile) error, len(settings))
	for _, key := range tools.SortedKeys(settings) {
		if !tools.Contains(patchSettingsWhitelist, key) {
			return "", fmt.Errorf("settings 字段 %q 不在白名单 %v 内", key, patchSettingsWhitelist)
		}
		set, err := settingSetter(key, settings[key])
		if err != nil {
			return "", err
		}
		setters[key] = set
	}
	if projects == nil {
		return "", fmt.Errorf("project writer not configured")
	}

	diag := map[string]string{}
	err := projects.Update(project, func(pf *domain.ProjectFile) error {
		// brief 仅广告项目可用：与 Python 一致，只要 key 出现（含 null）即门控。
		if _, ok := settings["brief"]; ok && pf.ContentMode != string(domain.ContentAd) {
			return fmt.Errorf("brief 仅广告/短片项目（content_mode=ad）可用")
		}
		for _, key := range tools.SortedKeys(setters) {
			if err := setters[key](pf); err != nil {
				return err
			}
			diag[key] = "set"
			if settings[key] == nil {
				diag[key] = "clear"
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return renderDiagnostics("settings", diag), nil
}

// settingSetter 返回单个设置项的写入函数，并在此完成类型校验（值 null 表示清除）。
func settingSetter(key string, value any) (func(*domain.ProjectFile) error, error) {
	switch key {
	case "episode_target_units", "planning_window_chars", "planning_max_episodes":
		if value == nil {
			return func(pf *domain.ProjectFile) error { *intField(pf, key) = nil; return nil }, nil
		}
		n, err := positiveInt(key, value)
		if err != nil {
			return nil, err
		}
		return func(pf *domain.ProjectFile) error {
			target := intField(pf, key)
			*target = &n
			return nil
		}, nil
	case "source_language":
		if value == nil {
			return func(pf *domain.ProjectFile) error { pf.SourceLanguage = ""; return nil }, nil
		}
		s, ok := value.(string)
		if !ok || !tools.Contains([]string{"zh", "en", "vi"}, s) {
			return nil, fmt.Errorf("source_language 必须是 [zh en vi] 之一或 null，收到 %v", value)
		}
		return func(pf *domain.ProjectFile) error { pf.SourceLanguage = s; return nil }, nil
	case "brief":
		if value == nil {
			return func(pf *domain.ProjectFile) error { pf.Brief = ""; return nil }, nil
		}
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("brief 必须是字符串或 null，收到 %v", value)
		}
		return func(pf *domain.ProjectFile) error { pf.Brief = s; return nil }, nil
	case "narration_voice":
		if value == nil {
			return func(pf *domain.ProjectFile) error { pf.NarrationVoice = ""; return nil }, nil
		}
		s, ok := value.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("narration_voice 必须是非空字符串或 null，收到 %v", value)
		}
		return func(pf *domain.ProjectFile) error { pf.NarrationVoice = s; return nil }, nil
	case "narration_speed":
		if value == nil {
			return func(pf *domain.ProjectFile) error { pf.NarrationSpeed = nil; return nil }, nil
		}
		v, err := positiveFloat(value)
		if err != nil {
			return nil, fmt.Errorf("narration_speed 必须是正的有限数值或 null，收到 %v", value)
		}
		return func(pf *domain.ProjectFile) error { pf.NarrationSpeed = &v; return nil }, nil
	}
	return nil, fmt.Errorf("settings 字段 %q 缺类型校验", key)
}

// positiveInt 接受正整数（JSON 数字或其字符串形态，模型常把数字加引号）。
func positiveInt(key string, value any) (int, error) {
	fail := fmt.Errorf("%s 必须是正整数或 null，收到 %v", key, value)
	switch v := value.(type) {
	case float64:
		if v != math.Trunc(v) || v < 1 || v > math.MaxInt32 {
			return 0, fail
		}
		return int(v), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1 {
			return 0, fail
		}
		return n, nil
	}
	return 0, fail
}

// positiveFloat 接受正的有限数值（数字或其字符串形态）。
func positiveFloat(value any) (float64, error) {
	var f float64
	switch v := value.(type) {
	case float64:
		f = v
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, err
		}
		f = parsed
	default:
		return 0, fmt.Errorf("not a number")
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0, fmt.Errorf("not positive finite")
	}
	return f, nil
}

func intField(pf *domain.ProjectFile, key string) **int {
	switch key {
	case "episode_target_units":
		return &pf.EpisodeTargetUnits
	case "planning_window_chars":
		return &pf.PlanningWindowChars
	default:
		return &pf.PlanningMaxEpisodes
	}
}

func renderDiagnostics(scope string, ops map[string]string) string {
	keys := tools.SortedKeys(ops)
	var set, clear, noop []string
	for _, k := range keys {
		switch ops[k] {
		case "set":
			set = append(set, k)
		case "clear":
			clear = append(clear, k)
		default:
			noop = append(noop, k)
		}
	}
	var parts []string
	if len(set) > 0 {
		parts = append(parts, "已更新 "+strings.Join(set, ", "))
	}
	if len(clear) > 0 {
		parts = append(parts, "已清除 "+strings.Join(clear, ", "))
	}
	if len(noop) > 0 {
		parts = append(parts, "无变更 "+strings.Join(noop, ", "))
	}
	summary := "无变更"
	if len(parts) > 0 {
		summary = strings.Join(parts, "; ")
	}
	return fmt.Sprintf("✅ %s: %s", scope, summary)
}

