// 参考生视频 step1 拆分的运行时能力、schema、扁平产出校验、晋升与 step2 保结构合并
// （对应 server/agent_runtime/sdk_tools/text_generation.py 的参考工具链辅助与
// lib/script_generator.py 的参考路径合并）。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/provider"
)

// ReferenceSplitCaps 是 rv 拆分用的视频能力：两套逐 unit 档位 + 派生上限 + 声音输入档。
//
// ReferenceDurations / TextDurations 是带 / 不带 `@` 引用的 unit 各自的生效档位，Durations 是
// 二者并集——schema 枚举与 prompt 候选集合取并集，因为落在任一套内的时长都可能合法。
type ReferenceSplitCaps struct {
	DefaultDuration    *int
	Durations          []int
	RawDurations       []int
	ReferenceDurations []int
	TextDurations      []int
	MaxDuration        int
	MaxRefs            *int
	Voice              VoiceRenderSettings
}

// TiersFor 返回该引用状态下生效的档位。
func (c ReferenceSplitCaps) TiersFor(hasReferences bool) []int {
	if hasReferences {
		return c.ReferenceDurations
	}
	return c.TextDurations
}

// FetchReferenceSplitCaps 解析参考视频拆分所需的视频能力；能力未知时停止，不伪造时长档位。
func FetchReferenceSplitCaps(
	ctx context.Context, r VideoCapabilityResolver, project *domain.ProjectFile,
) (ReferenceSplitCaps, error) {
	if r == nil || project == nil {
		return ReferenceSplitCaps{}, fmt.Errorf("video_capabilities_unresolved: 视频能力不可解析")
	}
	view, err := r.VideoCapabilitiesForProjectBucket(ctx, project, provider.VideoBucketForGenerationMode(project.GenerationMode))
	if err != nil {
		return ReferenceSplitCaps{}, fmt.Errorf("video_capabilities_unresolved: %w", err)
	}
	durations := append([]int(nil), view.SupportedDurations...)
	if len(durations) == 0 {
		return ReferenceSplitCaps{}, fmt.Errorf("video_capabilities_unresolved: 模型 %s/%s 未声明 supported_durations", view.ProviderID, view.ModelID)
	}
	withRefs, withoutRefs := ReferenceUnitDurationTiers(ctx, r, project, view, durations)
	union := normalizeDurations(append(append([]int(nil), withRefs...), withoutRefs...))
	if len(union) == 0 {
		return ReferenceSplitCaps{}, fmt.Errorf("video_capabilities_unresolved: 参考路线时长档位为空")
	}
	maxDuration, _ := maxDurationOf(union)
	var maxRefs *int
	if view.Caps.MaxReferenceImages > 0 {
		value := view.Caps.MaxReferenceImages
		maxRefs = &value
	}
	defaultDuration := project.DefaultDuration
	if defaultDuration != nil && !containsInt(union, *defaultDuration) {
		defaultDuration = nil
	}
	caps := ResolveProjectVideoCaps(ctx, r, project, provider.VideoBucketForGenerationMode(project.GenerationMode))
	return ReferenceSplitCaps{
		DefaultDuration:    defaultDuration,
		Durations:          union,
		RawDurations:       normalizeDurations(durations),
		ReferenceDurations: normalizeDurations(withRefs),
		TextDurations:      normalizeDurations(withoutRefs),
		MaxDuration:        maxDuration,
		MaxRefs:            maxRefs,
		Voice:              VoiceRenderSettingsFromCaps(caps, project),
	}, nil
}

// referenceStep1FlatUnitAllowed 是扁平 step1 unit 的字段白名单（对应 extra="forbid"）。
var referenceStep1FlatUnitAllowed = []string{"duration_seconds", "source_text", "text"}

// BuildReferenceUnitsStep1Schema 构造 unit 时长被 supported_durations 枚举硬约束的 step1 JSON Schema。
func BuildReferenceUnitsStep1Schema(durations []int) (map[string]any, error) {
	normalized := normalizeDurations(durations)
	if len(normalized) == 0 {
		return nil, fmt.Errorf("supported_durations 为空，无法构造 duration 枚举约束")
	}
	unit := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"duration_seconds": durationConstraint(normalized),
			"source_text":      map[string]any{"type": "string", "minLength": 1},
			"text":             map[string]any{"type": "string", "minLength": 1},
		},
		"required":             []any{"duration_seconds", "source_text", "text"},
		"additionalProperties": false,
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"units": map[string]any{"type": "array", "minItems": 1, "items": unit},
		},
		"required": []any{"units"},
	}, nil
}

// ReferenceStep2FlatScriptSchema 是 step2 扁平产出的 response_schema（title + 与 step1 等长同序的 unit 正文）。
func ReferenceStep2FlatScriptSchema() map[string]any {
	unit := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "minLength": 1},
		},
		"required":             []any{"text"},
		"additionalProperties": false,
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{"type": "string"},
			"units": map[string]any{"type": "array", "minItems": 1, "items": unit},
		},
		"required": []any{"title", "units"},
	}
}

// ParseReferenceStep1Flat 解析并校验 step1 扁平响应，返回归一化后的 units。
//
// 与 response_schema 同口径：units 非空、每个 unit 字段恰好为 duration_seconds / source_text /
// text，时长落在档位内。违约 fail-loud，不把未校验内容当成功产物落盘。
func ParseReferenceStep1Flat(responseText string, durations []int) ([]map[string]any, error) {
	root, err := parseJSONObject(responseText)
	if err != nil {
		return nil, fmt.Errorf("step1 拆分内容%s", strings.TrimPrefix(err.Error(), "JSON 解析失败: "))
	}
	rawUnits, ok := root["units"].([]any)
	if !ok || len(rawUnits) == 0 {
		return nil, fmt.Errorf("step1 拆分内容结构异常：units 必须是非空的 unit 对象数组")
	}
	allowed := map[int]bool{}
	for _, d := range normalizeDurations(durations) {
		allowed[d] = true
	}
	units := make([]map[string]any, 0, len(rawUnits))
	for index, raw := range rawUnits {
		unit, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("step1 拆分内容结构异常：units.%d 必须是对象", index)
		}
		if err := rejectUnknownKeys(unit, referenceStep1FlatUnitAllowed, fmt.Sprintf("units.%d", index)); err != nil {
			return nil, fmt.Errorf("step1 拆分内容不符合产出结构：%w", err)
		}
		duration := coerceDigitInt(unit["duration_seconds"])
		if duration == 0 {
			duration = intValueOrZero(unit["duration_seconds"])
		}
		if !allowed[duration] {
			return nil, fmt.Errorf("step1 拆分内容不符合产出结构：units.%d.duration_seconds 须取自模型档位 %v",
				index, normalizeDurations(durations))
		}
		sourceText, ok := unit["source_text"].(string)
		if !ok || sourceText == "" {
			return nil, fmt.Errorf("step1 拆分内容不符合产出结构：units.%d.source_text 必填且非空", index)
		}
		text, ok := unit["text"].(string)
		if !ok || text == "" {
			return nil, fmt.Errorf("step1 拆分内容不符合产出结构：units.%d.text 必填且非空", index)
		}
		units = append(units, map[string]any{
			"duration_seconds": duration, "source_text": sourceText, "text": text,
		})
	}
	return units, nil
}

// ValidateUnitDurationTier 按该 unit 的引用状态判时长是否落在生效档位内，出档抛 DraftViolation。
func ValidateUnitDurationTier(label string, duration int, hasReferences bool, caps ReferenceSplitCaps) error {
	tiers := caps.TiersFor(hasReferences)
	if containsInt(tiers, duration) {
		return nil
	}
	state := "无 `@` 资产引用"
	remedy := "；请改取该档位内的时长"
	if hasReferences {
		state = "带 `@` 资产引用"
		remedy = "；请改取该档位内的时长，或把次要资产融入描述文字、不用 `@` 引用"
	}
	return violation(label, "duration_off_tier", nil,
		"%s 时长 %ds 不在%s的 unit 的生效档位 %v 内%s", label, duration, state, tiers, remedy)
}

// CollectReferenceFlatViolations 逐 unit 收齐 step1 扁平产出的全部违约（不在首个违约处中断）。
func CollectReferenceFlatViolations(
	flatUnits []map[string]any, project *domain.ProjectFile,
	episode int, novelText string, caps ReferenceSplitCaps, sourceLanguage string,
) []DraftViolation {
	speechRateOverride := ProjectSpeechRateOverride(project)
	var violations []DraftViolation
	for index, flat := range flatUnits {
		label := fmt.Sprintf("unit E%dU%02d", episode, index+1)
		duration := intValueOrZero(flat["duration_seconds"])
		sourceText, _ := flat["source_text"].(string)
		text, _ := flat["text"].(string)
		checkTextAndTier := func() error {
			_, refs, err := validateUnitText(label, text, project, caps.MaxRefs)
			if err != nil {
				return err
			}
			return ValidateUnitDurationTier(label, duration, len(refs) > 0, caps)
		}
		violations = append(violations, collectViolations(
			func() error { return validateSourceTextAnchor(label, sourceText, novelText) },
			checkTextAndTier,
			func() error { return validateDialogueLoad(label, text, duration, sourceLanguage, speechRateOverride) },
		)...)
	}
	return violations
}

// BuildReferenceUnitsFromFlat 把已校验通过的扁平产出派生为落盘的结构化 unit 表。
//
// LLM 只写内容，机器写结构：unit_id 按数组序号编号、shots 由 `镜头N：` 切分、references 由
// `@[名称]` 派生（首现顺序即参考图编号）。
func BuildReferenceUnitsFromFlat(
	flatUnits []map[string]any, project *domain.ProjectFile, episode int, maxRefs *int,
) ([]map[string]any, error) {
	units := make([]map[string]any, 0, len(flatUnits))
	for index, flat := range flatUnits {
		unitID := fmt.Sprintf("E%dU%02d", episode, index+1)
		text, _ := flat["text"].(string)
		shots, refs, err := validateUnitText("unit "+unitID, text, project, maxRefs)
		if err != nil {
			return nil, err
		}
		shotsAny := make([]any, 0, len(shots))
		for _, shot := range shots {
			shotsAny = append(shotsAny, map[string]any{"text": shot["text"]})
		}
		units = append(units, map[string]any{
			"unit_id":          unitID,
			"shots":            shotsAny,
			"duration_seconds": intValueOrZero(flat["duration_seconds"]),
			"references":       renderUnitReferences(refs),
			"source_text":      stringValue(flat["source_text"], ""),
		})
	}
	return units, nil
}

// ReferenceResultText 渲染晋升 / 拆分成功后回给 agent 的摘要：落盘统计 + 容忍 warning。
func ReferenceResultText(step1Path string, units []map[string]any, warningLines []string, action string) string {
	shotCount := 0
	totalSeconds := 0
	maxUnitRefs := 0
	for _, unit := range units {
		shots, _ := unit["shots"].([]any)
		shotCount += len(shots)
		totalSeconds += intValueOrZero(unit["duration_seconds"])
		refs, _ := unit["references"].([]any)
		if len(refs) > maxUnitRefs {
			maxUnitRefs = len(refs)
		}
	}
	text := fmt.Sprintf("✅ 参考视频单元%s（结构化 step1）已保存: %s\n"+
		"📊 生成统计: %d 个 unit / %d 个 shot，总时长 %d 秒；单 unit references 最多 %d 个",
		action, step1Path, len(units), shotCount, totalSeconds, maxUnitRefs)
	if len(warningLines) > 0 {
		text += "\n⚠️ 声音降级提示（不阻断，产物已落盘）:\n- " + strings.Join(warningLines, "\n- ")
	}
	return text
}

// renderShotsTextList 把磁盘形态的 shots（[]any）还原为书写层正文；非对象条目按空正文保留 header。
func renderShotsTextList(shots []any) string {
	blocks := make([]string, 0, len(shots))
	for index, raw := range shots {
		body := ""
		if m, ok := raw.(map[string]any); ok {
			if text, ok := m["text"].(string); ok {
				body = text
			}
		}
		if body == "" {
			blocks = append(blocks, fmt.Sprintf("镜头%d：", index+1))
			continue
		}
		head, rest := body, ""
		if i := strings.Index(body, "\n"); i >= 0 {
			head, rest = body[:i], body[i+1:]
		}
		block := fmt.Sprintf("镜头%d：%s", index+1, head)
		if rest != "" {
			block += "\n" + rest
		}
		blocks = append(blocks, block)
	}
	return strings.Join(blocks, "\n")
}

// FlattenReferenceStep1Units 把正式 step1 的结构化 unit 表还原为隔离草稿装的扁平书写层。
//
// unit_id / shots / references 不进草稿（机器派生物）。盘上 unit 不合形状时不 fail-loud：
// 字段缺失或类型不符时原样带过，交由晋升侧的 schema 重判逐条报告。
func FlattenReferenceStep1Units(units []any) []map[string]any {
	flat := make([]map[string]any, 0, len(units))
	for _, raw := range units {
		unit, ok := raw.(map[string]any)
		if !ok {
			flat = append(flat, map[string]any{"duration_seconds": nil, "source_text": "", "text": ""})
			continue
		}
		shots, _ := unit["shots"].([]any)
		text := renderShotsTextList(shots)
		parsedShots, _ := parsePrompt(text)
		if len(parsedShots) != len(shots) {
			text = ""
		}
		duration, present := unit["duration_seconds"]
		if !present {
			duration = nil
		}
		flat = append(flat, map[string]any{
			"duration_seconds": duration,
			"source_text":      stringValue(unit["source_text"], ""),
			"text":             text,
		})
	}
	return flat
}

// ReferenceDraftRevalidation 是 step1 隔离草稿读时重判的结果。
type ReferenceDraftRevalidation struct {
	Violations   []DraftViolation
	FlatUnits    []map[string]any
	Caps         ReferenceSplitCaps
	SchemaFailed bool
}

// RevalidateReferenceStep1Draft 按产出时那套校验器全量重判 step1 隔离草稿，只读、不写盘、不清草稿。
func RevalidateReferenceStep1Draft(
	ctx context.Context, projectDir string, project *domain.ProjectFile, episode int,
	draft *QuarantinedDraft, resolver VideoCapabilityResolver,
) (ReferenceDraftRevalidation, error) {
	if draft == nil {
		return ReferenceDraftRevalidation{}, fmt.Errorf("隔离草稿缺失")
	}
	if _, ok := draft.Meta["source"]; !ok {
		return ReferenceDraftRevalidation{}, fmt.Errorf(
			"隔离草稿 %s 的 meta.source 缺失（产出时记录的源文范围）；"+
				"请恢复该字段（指定源文时为其相对路径，按整个 source/ 产出时为 null）后重试", draft.Path)
	}
	source, _ := draft.Meta["source"].(string)
	sourceText, err := ResolveEpisodeSource(projectDir, episode, source, project)
	if err != nil {
		return ReferenceDraftRevalidation{}, err
	}
	caps, err := FetchReferenceSplitCaps(ctx, resolver, project)
	if err != nil {
		return ReferenceDraftRevalidation{}, err
	}

	rawUnits, ok := draft.Content["units"].([]any)
	var violations []DraftViolation
	var flatUnits []map[string]any
	if !ok || len(rawUnits) == 0 {
		violations = []DraftViolation{{Code: "schema_invalid",
			Message: fmt.Sprintf("隔离草稿的 content.units 必须是非空的 unit 对象数组（当前为 %T）", draft.Content["units"])}}
	} else {
		flatUnits, err = ParseReferenceStep1FlatFromUnits(rawUnits, caps.Durations)
		if err != nil {
			violations = []DraftViolation{{Code: "schema_invalid",
				Message: fmt.Sprintf("隔离草稿的 content 不符合 step1 产出结构：%s；"+
					"每个 unit 须有非空 source_text / text，且 duration_seconds 取自模型档位 %v",
					err.Error(), caps.Durations)}}
		}
	}
	if len(violations) > 0 {
		return ReferenceDraftRevalidation{Violations: violations, Caps: caps, SchemaFailed: true}, nil
	}
	violations = CollectReferenceFlatViolations(flatUnits, project, episode, sourceText.Text, caps, project.SourceLanguage)
	return ReferenceDraftRevalidation{Violations: violations, FlatUnits: flatUnits, Caps: caps, SchemaFailed: false}, nil
}

// ParseReferenceStep1FlatFromUnits 校验已解析的 units 数组（供隔离草稿重判）。
func ParseReferenceStep1FlatFromUnits(rawUnits []any, durations []int) ([]map[string]any, error) {
	allowed := map[int]bool{}
	for _, d := range normalizeDurations(durations) {
		allowed[d] = true
	}
	units := make([]map[string]any, 0, len(rawUnits))
	for index, raw := range rawUnits {
		unit, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("units.%d 必须是对象", index)
		}
		if err := rejectUnknownKeys(unit, referenceStep1FlatUnitAllowed, fmt.Sprintf("units.%d", index)); err != nil {
			return nil, err
		}
		duration := coerceDigitInt(unit["duration_seconds"])
		if !allowed[duration] {
			return nil, fmt.Errorf("units.%d.duration_seconds 须取自模型档位 %v", index, normalizeDurations(durations))
		}
		sourceText, ok := unit["source_text"].(string)
		if !ok || sourceText == "" {
			return nil, fmt.Errorf("units.%d.source_text 必填且非空", index)
		}
		text, ok := unit["text"].(string)
		if !ok || text == "" {
			return nil, fmt.Errorf("units.%d.text 必填且非空", index)
		}
		units = append(units, map[string]any{
			"duration_seconds": duration, "source_text": sourceText, "text": text,
		})
	}
	return units, nil
}

// RenderStep1ConflictReport 渲染晋升遇乐观并发冲突时回给 agent 的结构化报告：最新内容 + 合并指引。
func RenderStep1ConflictReport(episode int, draft *QuarantinedDraft, conflict *Step1WriteConflict) string {
	latestBlock := "当前正式文件不存在或不是合法的 step1 JSON，无法附上最新内容；请自行读取该文件确认。"
	if conflict != nil && conflict.CurrentContent != nil {
		if units, ok := conflict.CurrentContent["units"].([]any); ok && len(units) > 0 {
			latest, _ := marshalIndented(map[string]any{"units": FlattenReferenceStep1Units(units)})
			latestBlock = "当前正式 step1 的最新内容（扁平书写层，与草稿 content 同形）：\n" + latest
		}
	}
	expectedLiteral := jsonLiteral(conflictExpected(conflict))
	actualLiteral := jsonLiteral(conflictActual(conflict))
	draftPath := "<未知路径>"
	if draft != nil {
		draftPath = draft.Path
	}
	return "❌ 晋升中止（并发冲突）：正式 step1 在本草稿产出后已被其他写入方（如 Web 端保存）修改，" +
		"直接晋升会覆盖对方的修改，本次未写盘、草稿仍在场。\n" +
		"草稿基线指纹: " + expectedLiteral + "；盘上现值指纹: " + actualLiteral + "\n\n" +
		latestBlock + "\n\n" +
		"处置：对照上方最新内容与草稿 " + draftPath + " 的 content.units，把对方的修改合并进草稿；" +
		"合并完成后把草稿 meta.base_fingerprint 更新为 " + actualLiteral + "，" +
		"再调用 " + PromoteToolName + "（{\"episode\": " + fmt.Sprint(episode) + "}）重新晋升。"
}

func conflictExpected(conflict *Step1WriteConflict) *string {
	if conflict == nil {
		return nil
	}
	return conflict.Expected
}

func conflictActual(conflict *Step1WriteConflict) *string {
	if conflict == nil {
		return nil
	}
	return conflict.Actual
}

// ReferenceStep2FlatContent 把 step2 响应还原成隔离草稿要装的扁平形状 {title, units: [{text}]}。
func ReferenceStep2FlatContent(responseText string, episode int) (map[string]any, error) {
	root, err := parseJSONObject(responseText)
	if err != nil {
		return nil, err
	}
	title, _ := root["title"].(string)
	if strings.TrimSpace(title) == "" {
		title = fmt.Sprintf("第%d集", episode)
	}
	rawUnits, ok := root["units"].([]any)
	if !ok || len(rawUnits) == 0 {
		return nil, fmt.Errorf("step2 视觉展开结构校验失败：units 必须是非空数组")
	}
	units := make([]any, 0, len(rawUnits))
	for index, raw := range rawUnits {
		unit, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("step2 视觉展开结构校验失败：units.%d 必须是对象", index)
		}
		text, ok := unit["text"].(string)
		if !ok || text == "" {
			return nil, fmt.Errorf("step2 视觉展开结构校验失败：units.%d.text 必填且非空", index)
		}
		units = append(units, map[string]any{"text": text})
	}
	return map[string]any{"title": title, "units": units}, nil
}

// MergeReferenceVisual 是参考路径 step2 合并：LLM 只出书写层正文，其余字段机械沿用 step1 / 从正文派生。
//
// 保结构 diff 在此落地：unit 数与顺序、台词规范行逐字、每 unit 的镜头数都由 step1 定稿。逐 unit 的
// 违约收齐后一次抛出（*DraftViolations），供调用方把整份产出连同报告落到隔离草稿。
func MergeReferenceVisual(
	step1Units []map[string]any, responseText string, episode int, project *domain.ProjectFile, maxRefs *int,
) (map[string]any, error) {
	root, err := parseJSONObject(responseText)
	if err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	title, _ := root["title"].(string)
	if strings.TrimSpace(title) == "" {
		title = fmt.Sprintf("第%d集", episode)
	}
	rawUnits, ok := root["units"].([]any)
	if !ok {
		return nil, fmt.Errorf("step2 视觉展开结构校验失败：units 必须是非空数组")
	}
	flatUnits := make([]map[string]any, 0, len(rawUnits))
	for index, raw := range rawUnits {
		unit, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("step2 视觉展开结构校验失败：units.%d 必须是对象", index)
		}
		text, ok := unit["text"].(string)
		if !ok || text == "" {
			return nil, fmt.Errorf("step2 视觉展开结构校验失败：units.%d.text 必填且非空", index)
		}
		flatUnits = append(flatUnits, map[string]any{"text": text})
	}
	if len(flatUnits) != len(step1Units) {
		return nil, &DraftViolations{Items: []DraftViolation{{Code: "unit_count_changed",
			Message: fmt.Sprintf("step2 产出的 unit 数（%d）与 step1 已确认的（%d）不一致；"+
				"step2 只做视觉展开，不得合并、拆分或增删 unit", len(flatUnits), len(step1Units))}}}
	}

	var videoUnits []any
	var violations []DraftViolation
	for i, step1Unit := range step1Units {
		unitID, _ := step1Unit["unit_id"].(string)
		label := "unit " + unitID
		storedShots, _ := step1Unit["shots"].([]any)
		step1Text := renderShotsTextList(storedShots)
		text, _ := flatUnits[i]["text"].(string)

		shots, refs, err := validateUnitText(label, text, project, maxRefs)
		if err != nil {
			violations = append(violations, violationItems(err)...)
			continue
		}
		if err := assertDialoguePreserved(label, step1Text, text); err != nil {
			violations = append(violations, violationItems(err)...)
			continue
		}
		if len(shots) != len(storedShots) {
			violations = append(violations, DraftViolation{Code: "shot_count_changed", Label: label,
				Message: fmt.Sprintf("%s 的镜头数被改动（step1 有 %d 个，step2 产出 %d 个）；step2 只做视觉展开，镜头数须保持不变",
					label, len(storedShots), len(shots))})
			continue
		}
		shotsAny := make([]any, 0, len(shots))
		for _, shot := range shots {
			shotsAny = append(shotsAny, map[string]any{"text": shot["text"]})
		}
		duration, present := step1Unit["duration_seconds"]
		if !present {
			duration = nil
		}
		videoUnits = append(videoUnits, map[string]any{
			"unit_id":          unitID,
			"shots":            shotsAny,
			"references":       renderUnitReferences(refs),
			"duration_seconds": duration,
		})
	}
	if len(violations) > 0 {
		return nil, &DraftViolations{Items: violations}
	}
	return map[string]any{"title": title, "video_units": videoUnits}, nil
}

// marshalIndented 渲染 2 空格缩进的 JSON（失败时返回错误字符串，供报告回显）。
func marshalIndented(value any) (string, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// jsonLiteral 按 JSON 字面量渲染可空字符串（null 字面量用于「文件已被删除」的现值指纹）。
func jsonLiteral(value *string) string {
	if value == nil {
		return "null"
	}
	raw, err := json.Marshal(*value)
	if err != nil {
		return "null"
	}
	return string(raw)
}
