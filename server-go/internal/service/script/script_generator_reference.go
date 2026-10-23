// 剧本生成器的参考视频（reference_video）与广告（ad）两条 step2 路径（对应
// lib/script_generator.py 的参考分支与 ad 分支）。
//
// 参考路径：LLM 只出书写层正文，unit 数与顺序、台词行逐字、镜头数都由 step1 定稿；保结构
// diff 违约时把已付费的产出连同报告落隔离草稿，由晋升工具重判。ad 路径：单视频平铺 shots[]，
// 供应商 storyboard 路径枚举单镜头时长、reference 路径取 1-15 自由整数。
package script

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/shotwise/shotwise/internal/service"
	"os"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/service/reference"
)

// ScriptReferenceContext 是参考路径 step2 所需的运行时能力上下文。
type ScriptReferenceContext struct {
	// Caps 是参考拆分档位与上限（含两套逐 unit 档位）。
	Caps reference.ReferenceSplitCaps
	// DurationCtx 是项目能力 context（provider/model/resolution/全集档位），用于逐 unit 取档。
	DurationCtx service.ProjectDurationContext
}

// WithReferenceContext 注入参考路径能力上下文。
func WithReferenceContext(rc ScriptReferenceContext) ScriptGenOption {
	return func(g *ScriptGenerator) { g.refCtx = &rc }
}

// --- ad 路径 ---

// composeAD 构造 ad 分支的 (prompt, response_schema)。
func (g *ScriptGenerator) composeAD(episode int, genMode string) (string, map[string]any, error) {
	if genMode == string(project.GenerationReferenceVideo) {
		prompt, err := g.buildADPrompt(episode, genMode, nil)
		if err != nil {
			return "", nil, err
		}
		return prompt, adReferenceEpisodeScriptSchema(), nil
	}
	schema, err := adEpisodeScriptSchema(g.supportedDurs)
	if err != nil {
		return "", nil, err
	}
	prompt, err := g.buildADPrompt(episode, genMode, g.supportedDurs)
	if err != nil {
		return "", nil, err
	}
	return prompt, schema, nil
}

// buildADPrompt 构建广告/短片模式 prompt：brief + 产品信息 + 审定配比表，不读 step1 中间文件。
func (g *ScriptGenerator) buildADPrompt(episode int, genMode string, supported []int) (string, error) {
	if g.pf.TargetDuration == nil || *g.pf.TargetDuration <= 0 {
		return "", fmt.Errorf("广告/短片项目缺少合法的 target_duration（正整数秒），当前为 %v", g.pf.TargetDuration)
	}
	return BuildAdPrompt(AdPromptOptions{
		ProjectOverview:    g.pf.Overview,
		Style:              g.pf.Style,
		StyleDescription:   "",
		Characters:         &g.pf.Characters,
		Scenes:             &g.pf.Scenes,
		Props:              &g.pf.Props,
		Products:           &g.pf.Products,
		Brief:              g.pf.Brief,
		TargetDuration:     *g.pf.TargetDuration,
		GenerationMode:     genMode,
		SupportedDurations: supported,
		Episode:            episode,
		AspectRatio:        g.aspectRatio(),
		TargetLanguage:     targetLanguageOf(g.pf),
		SpeechRateOverride: service.ProjectSpeechRateOverride(g.pf),
	})
}

// generateAD 是 ad 单段生成（平铺 shots[]）。
func (g *ScriptGenerator) generateAD(ctx context.Context, episode int, instructions string) (string, error) {
	if g.gen == nil {
		return "", fmt.Errorf("文本生成器未初始化")
	}
	prompt, schema, err := g.composeAD(episode, g.generationMode())
	if err != nil {
		return "", err
	}
	prompt = AppendUserInstructions(prompt, instructions)
	response, err := g.gen.Generate(ctx, prompt, schema, defaultMaxOutputTokens)
	if err != nil {
		return "", err
	}
	script, err := parseEpisodeScriptResponse(response, episode)
	if err != nil {
		return "", err
	}
	return g.finish(script, episode, nil, nil)
}

// parseEpisodeScriptResponse 解析剧本响应为 dict（title 缺失/空白兜底），供 ad 单段路径使用。
//
// 校验失败降级返回原始数据，与 Python `_parse_response` 同口径——落盘前的结构校验由写盘入口把关。
func parseEpisodeScriptResponse(responseText string, episode int) (map[string]any, error) {
	root, err := service.ParseJSONObject(responseText)
	if err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	if title, ok := root["title"].(string); !ok || strings.TrimSpace(title) == "" {
		root["title"] = fmt.Sprintf("第%d集", episode)
	}
	return root, nil
}

// --- 参考路径 ---

// buildReferencePrompt 构建参考路径 step2 prompt（视觉展开）。
func (g *ScriptGenerator) buildReferencePrompt(units []map[string]any, rc *ScriptReferenceContext, episode int) (string, error) {
	return BuildReferenceVideoPrompt(ReferenceVideoPromptOptions{
		ProjectOverview:  g.pf.Overview,
		Style:            g.pf.Style,
		StyleDescription: "",
		Characters:       &g.pf.Characters,
		Scenes:           &g.pf.Scenes,
		Props:            &g.pf.Props,
		Step1Units:       units,
		MaxRefs:          rc.Caps.MaxRefs,
		Episode:          episode,
		AspectRatio:      g.aspectRatio(),
		TargetLanguage:   targetLanguageOf(g.pf),
	})
}

// generateReferenceStep2 是参考视频两段式 step2。
func (g *ScriptGenerator) generateReferenceStep2(ctx context.Context, episode int, instructions string) (string, error) {
	if g.gen == nil {
		return "", fmt.Errorf("文本生成器未初始化")
	}
	rc, err := g.requireReferenceContext()
	if err != nil {
		return "", err
	}
	units, err := g.loadReferenceStep1(episode, rc.Caps.RawDurations)
	if err != nil {
		return "", err
	}
	prompt, err := g.buildReferencePrompt(units, rc, episode)
	if err != nil {
		return "", err
	}
	prompt = AppendUserInstructions(prompt, instructions)
	if err := g.assertReferenceStep1Ready(units, rc); err != nil {
		return "", err
	}
	response, err := g.gen.Generate(ctx, prompt, reference.ReferenceStep2FlatScriptSchema(), defaultMaxOutputTokens)
	if err != nil {
		return "", err
	}
	script, err := reference.MergeReferenceVisual(units, response, episode, g.pf, rc.Caps.MaxRefs)
	if err != nil {
		if len(reference.ViolationItems(err)) > 0 {
			return "", quarantineReferenceStep2(g.dir, episode, response, err)
		}
		return "", err
	}
	unitDurations := referenceUnitDurationMap(units, episode)
	resultPath, err := g.finish(script, episode, unitDurations, rc)
	if err != nil {
		if len(reference.ViolationItems(err)) > 0 {
			return "", quarantineReferenceStep2(g.dir, episode, response, err)
		}
		return "", err
	}
	return resultPath, nil
}

// referenceUnitDurationMap 构造「改写后 unit_id → step1 确认时长」的映射。
func referenceUnitDurationMap(units []map[string]any, episode int) map[string]int {
	out := map[string]int{}
	for _, unit := range units {
		id, _ := unit["unit_id"].(string)
		out[fmt.Sprint(rewriteEpisodePrefix(id, episode))] = service.IntValueOrZero(unit["duration_seconds"])
	}
	return out
}

// quarantineReferenceStep2 把违约的 step2 产出与报告落隔离草稿，返回携带报告的违约异常。
func quarantineReferenceStep2(projectDir string, episode int, responseText string, err error) error {
	content, contentErr := reference.ReferenceStep2FlatContent(responseText, episode)
	if contentErr != nil {
		return contentErr
	}
	report, reportErr := reference.QuarantineAndReport(projectDir, episode, reference.QuarantineKindStep2, content, reference.ViolationItems(err), nil, nil)
	if reportErr != nil {
		return reportErr
	}
	return &reference.DraftViolation{Code: "quarantined", Message: report}
}

// loadReferenceStep1 加载并校验 reference_video step1 结构化中间文件 step1_reference_units.json。
func (g *ScriptGenerator) loadReferenceStep1(episode int, supportedDurations []int) ([]map[string]any, error) {
	drafts := service.EpisodeDraftsDir(g.dir, episode)
	step1JSON := filepath.Join(drafts, service.ReferenceVideoStep1Filename)
	if reference.QuarantineExists(g.dir, episode, reference.QuarantineKindStep1) {
		return nil, fmt.Errorf("第 %d 集 step1 有违约产物待处置（%s），step2 生成已中止；"+
			"请先修复该草稿并经 %s 晋升为正式 step1",
			episode, reference.QuarantinePath(g.dir, episode, reference.QuarantineKindStep1), reference.PromoteToolName)
	}
	if _, err := os.Stat(step1JSON); err != nil {
		legacy := filepath.Join(drafts, service.ReferenceVideoStep1LegacyFilename)
		if _, legacyErr := os.Stat(legacy); legacyErr == nil {
			return nil, fmt.Errorf("仅找到结构化前的旧拆分表 %s，未找到 %s；请重跑 split_reference_video_units 产出结构化 %s",
				legacy, step1JSON, service.ReferenceVideoStep1Filename)
		}
		return nil, fmt.Errorf("未找到 Step 1 中间文件: %s；generation_mode=reference_video 期望该文件，请先完成 video_unit 拆分", step1JSON)
	}

	// 与 Web 端保存 / 确认共享同一把 per-path 锁：迁移的读改写与它们相互互斥。
	var root map[string]any
	var migratedProject *project.ProjectFile
	var migrationWarnings []service.MigrationWarning
	migrateErr := reference.Step1WriteLock(g.dir, episode, func(step1Path string) error {
		// 顺序不变量：审阅 gate 的判定在更早的 step2 工具入口完成，迁移在其后运行且可能
		// 改写时长。先记下迁移前的放行状态，供迁移后判断放行依据是否已失效。放行状态与
		// 草稿在同一临界区内读取，两者才描述同一时刻——锁外读则并发的保存/确认会让它
		// 描述另一份草稿的审阅结果。
		gatePassedBefore := !GateBlocksStep2(g.dir, g.pf, episode)
		content, err := readJSONObjectFile(step1Path)
		if err != nil {
			return fmt.Errorf("step1_reference_units.json 解析失败: %w", err)
		}
		// 存量草稿的 per-shot 时长一次性收编到 unit 级并回写落盘（二次加载不再触发）。
		// 此处持有模型档位，收编结果直接取档，与下方的枚举校验对齐。
		migratedProject, migrationWarnings, err = reference.MigrateStep1DraftInPlace(
			g.store, g.projectName, content, episode, supportedDurations,
		)
		if err != nil {
			return err
		}
		root = content
		if !gatePassedBefore {
			return nil
		}
		// 迁移带 warnings 说明 clamp 改写了实际秒数，那是内容变更、审阅确认随之失效。而
		// gate 放行据的是改写前的状态：不在此处补判，生成就会拿着用户从未过目的秒数走完
		// 付费的 step2，落盘之后才在下次加载被拦下。
		if len(migrationWarnings) > 0 && migratedProject != nil &&
			GateBlocksStep2(g.dir, migratedProject, episode) {
			return fmt.Errorf("第 %d 集 step1 时长已按当前模型档位收编改写（%s），"+
				"改写后的内容尚未经审阅确认，step2 生成已中止；请在 Web 端审阅确认本集 step1 后重新生成",
				episode, reference.RenderMigrationWarnings(migrationWarnings))
		}
		return nil
	})
	if migrateErr != nil {
		return nil, migrateErr
	}

	rawUnits, ok := root["units"].([]any)
	if !ok || len(rawUnits) == 0 {
		return nil, fmt.Errorf("step1_reference_units.json units 为空")
	}
	units, err := normalizeReferenceStep1Units(rawUnits)
	if err != nil {
		return nil, fmt.Errorf("step1_reference_units.json 结构校验失败: %w", err)
	}

	ids := make([]string, 0, len(units))
	for _, unit := range units {
		ids = append(ids, fmt.Sprint(unit["unit_id"]))
	}
	if dupes := duplicateStrings(ids); len(dupes) > 0 {
		return nil, fmt.Errorf("step1_reference_units.json unit_id 重复: %v", dupes)
	}
	rewritten := make([]string, 0, len(ids))
	for _, id := range ids {
		rewritten = append(rewritten, fmt.Sprint(rewriteEpisodePrefix(id, episode)))
	}
	if dupes := duplicateStrings(rewritten); len(dupes) > 0 {
		return nil, fmt.Errorf("step1_reference_units.json unit_id 改写到 episode=%d 后重复: %v", episode, dupes)
	}
	allowed := map[int]bool{}
	for _, d := range supportedDurations {
		allowed[d] = true
	}
	var bad []int
	for _, unit := range units {
		duration := service.IntValueOrZero(unit["duration_seconds"])
		if !allowed[duration] {
			bad = append(bad, duration)
		}
	}
	if len(bad) > 0 {
		bad = uniqueSortedInts(bad)
		return nil, fmt.Errorf("step1_reference_units.json unit 时长非法（不在 %v 内）: %v", service.NormalizeDurations(supportedDurations), bad)
	}
	return units, nil
}

// normalizeReferenceStep1Units 校验并归一 step1 units 形状。
func normalizeReferenceStep1Units(rawUnits []any) ([]map[string]any, error) {
	units := make([]map[string]any, 0, len(rawUnits))
	for index, raw := range rawUnits {
		unit, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("units.%d 必须是对象", index)
		}
		unitID, ok := unit["unit_id"].(string)
		if !ok || unitID == "" {
			return nil, fmt.Errorf("units.%d.unit_id 必填且非空", index)
		}
		shots, ok := unit["shots"].([]any)
		if !ok || len(shots) == 0 || len(shots) > reference.MaxShotsPerUnit {
			return nil, fmt.Errorf("units.%d.shots 必须是 1-%d 个镜头对象", index, reference.MaxShotsPerUnit)
		}
		normalizedShots := make([]any, 0, len(shots))
		for si, rawShot := range shots {
			shot, ok := rawShot.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("units.%d.shots.%d 必须是对象", index, si)
			}
			if _, ok := shot["text"].(string); !ok {
				return nil, fmt.Errorf("units.%d.shots.%d.text 必填且为字符串", index, si)
			}
			normalizedShots = append(normalizedShots, map[string]any{"text": shot["text"]})
		}
		duration, ok := reference.IntValueOK(unit["duration_seconds"])
		if !ok || duration < 1 || duration > 300 {
			return nil, fmt.Errorf("units.%d.duration_seconds 必须是 1-300 的整数", index)
		}
		refs, _ := unit["references"].([]any)
		if refs == nil {
			refs = []any{}
		}
		units = append(units, map[string]any{
			"unit_id":          unitID,
			"shots":            normalizedShots,
			"duration_seconds": duration,
			"references":       refs,
			"source_text":      service.StringValue(unit["source_text"], ""),
		})
	}
	return units, nil
}

// assertReferenceStep1Ready 对 step1 现值做全部预判：时长档位仍生效 + 正文按机器口径合法。
func (g *ScriptGenerator) assertReferenceStep1Ready(units []map[string]any, rc *ScriptReferenceContext) error {
	for _, unit := range units {
		duration := service.IntValueOrZero(unit["duration_seconds"])
		offTiers := g.unitDurationOffEveryTier(duration, rc)
		if offTiers != nil {
			return fmt.Errorf("unit %v 已确认时长 %ds 不在当前生效档位 %v 内；通常是模型或分辨率配置变化让档位收窄导致，"+
				"请调整配置回原档位，或重新拆分该集 step1 并重新审阅确认",
				unit["unit_id"], duration, uniqueSortedInts(offTiers))
		}
	}
	return g.assertReferenceStep1TextValid(units, rc.Caps.MaxRefs)
}

// assertReferenceStep1TextValid 按机器产物的严格口径预判 step1 各 unit 正文，违约时把定位指回 step1。
func (g *ScriptGenerator) assertReferenceStep1TextValid(units []map[string]any, maxRefs *int) error {
	sourceLanguage := g.pf.SourceLanguage
	speechRateOverride := service.ProjectSpeechRateOverride(g.pf)
	for _, unit := range units {
		label := fmt.Sprintf("step1 的 unit %v", unit["unit_id"])
		storedShots, _ := unit["shots"].([]any)
		text := reference.RenderShotsTextList(storedShots)
		err := func() error {
			parsedShots, _, err := reference.ValidateUnitText(label, text, g.pf, maxRefs)
			if err != nil {
				return err
			}
			if len(parsedShots) != len(storedShots) {
				return reference.Violation(label, "shot_count_changed", nil,
					"%s 落盘的 %d 个镜头在书写层解析回 %d 个；镜头正文里不能再嵌 `镜头N：` 行，请把它拆成独立镜头",
					label, len(storedShots), len(parsedShots))
			}
			return reference.ValidateDialogueLoad(label, text, service.IntValueOrZero(unit["duration_seconds"]), sourceLanguage, speechRateOverride)
		}()
		if err != nil {
			violations := reference.ViolationItems(err)
			message := ""
			code := ""
			if len(violations) > 0 {
				message = violations[0].Message
				code = violations[0].Code
			}
			return reference.Violation(label, code, nil,
				"%s；这段正文来自 step1（拆分产出或手工编辑），step2 会逐字保留它，"+
					"请先在 Web 端修正该 unit 的 step1 正文或时长并重新审阅确认", message)
		}
	}
	return nil
}

// unitDurationOffEveryTier 时长在带图与不带图两种档位下都出局时返回带图档位，任一合法则返回 nil。
func (g *ScriptGenerator) unitDurationOffEveryTier(duration int, rc *ScriptReferenceContext) []int {
	if g.unitDurationOffTier(duration, true, rc) == nil {
		return nil
	}
	if g.unitDurationOffTier(duration, false, rc) == nil {
		return nil
	}
	return service.EffectiveReferenceDurations(
		rc.DurationCtx.ProviderID, rc.DurationCtx.ModelName, rc.DurationCtx.SupportedDurations,
		rc.DurationCtx.Resolution, true,
	)
}

// unitDurationOffTier 时长落在该 unit 生效档位之外时返回该档位集，落在内则返回 nil。
func (g *ScriptGenerator) unitDurationOffTier(duration int, hasReferences bool, rc *ScriptReferenceContext) []int {
	tiers := service.EffectiveReferenceDurations(
		rc.DurationCtx.ProviderID, rc.DurationCtx.ModelName, rc.DurationCtx.SupportedDurations,
		rc.DurationCtx.Resolution, hasReferences,
	)
	if len(tiers) == 0 {
		return nil
	}
	if service.ResolveDurationSlot(float64(duration), tiers).Seconds == duration {
		return nil
	}
	return tiers
}

// overrideReferenceUnitDurations 按 step1 确认值机械覆盖 unit 时长，按落地后的最终 references 状态取档。
func (g *ScriptGenerator) overrideReferenceUnitDurations(
	script map[string]any, kind, idField string, refDurations map[string]int, rewrittenOutputIDs []string,
	rc *ScriptReferenceContext,
) error {
	if dupes := duplicateStrings(rewrittenOutputIDs); len(dupes) > 0 {
		return fmt.Errorf("reference_video 输出 unit_id 重复: %v", dupes)
	}
	outputSet := service.StringSet(rewrittenOutputIDs)
	expectedSet := map[string]bool{}
	for id := range refDurations {
		expectedSet[id] = true
	}
	var missing []string
	for id := range expectedSet {
		if !outputSet[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("reference_video 输出缺少 step1 已确认的 unit_id: %v", uniqueSortedStrings(missing))
	}
	var unknown []string
	for id := range outputSet {
		if !expectedSet[id] {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("reference_video 输出包含 step1 之外的未知 unit_id: %v", uniqueSortedStrings(unknown))
	}

	items, _ := script[kind].([]any)
	for _, item := range items {
		unit, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := unit[idField].(string)
		target, ok := refDurations[id]
		if !ok {
			continue
		}
		hasRefs := service.RefPresenceTruthy(unit["references"])
		if unitTiers := g.unitDurationOffTier(target, hasRefs, rc); unitTiers != nil {
			return reference.Violation("unit "+id, "duration_off_tier", nil,
				"unit %s 已确认时长 %ds 不在当前生效档位 %v 内；通常是该次生成给该 unit 新增/去掉了引用"+
					"导致，请调整该 unit 正文里的 `@` 引用使其回到该档位；若引用本就该是这样，"+
					"说明模型能力已变化，需要重新拆分该集 step1",
				id, target, uniqueSortedInts(unitTiers))
		}
		unit["duration_seconds"] = target
	}
	return nil
}

// PromoteReferenceStep2Draft 按产出时那套校验器全量重判 step2 隔离草稿，通过则晋升为正式剧本并清除草稿。
//
// 重判用的是 reference.MergeReferenceVisual 本身，不是它的简化副本：晋升口径与产出口径必须同一份代码。
func (g *ScriptGenerator) PromoteReferenceStep2Draft(episode int) (string, error) {
	rc, err := g.requireReferenceContext()
	if err != nil {
		return "", err
	}
	draft, err := reference.ReadQuarantine(g.dir, episode, reference.QuarantineKindStep2)
	if err != nil {
		return "", err
	}
	if draft == nil {
		return "", fmt.Errorf("第 %d 集没有可晋升的 step2 隔离草稿（%s 缺失或内容不是合法信封）",
			episode, reference.QuarantinePath(g.dir, episode, reference.QuarantineKindStep2))
	}
	units, err := g.loadReferenceStep1(episode, rc.Caps.RawDurations)
	if err != nil {
		return "", err
	}
	if err := g.assertReferenceStep1Ready(units, rc); err != nil {
		return "", err
	}
	contentJSON, err := json.Marshal(draft.Content)
	if err != nil {
		return "", err
	}
	script, mergeErr := reference.MergeReferenceVisual(units, string(contentJSON), episode, g.pf, rc.Caps.MaxRefs)
	if mergeErr == nil {
		script, mergeErr = func() (map[string]any, error) {
			if err := g.addMetadata(script, episode, referenceUnitDurationMap(units, episode), rc); err != nil {
				return nil, err
			}
			return script, nil
		}()
	}
	if mergeErr != nil {
		violations := reference.ViolationItems(mergeErr)
		if len(violations) == 0 {
			violations = []reference.DraftViolation{{Code: "schema_invalid",
				Message: fmt.Sprintf("隔离草稿的 content 不符合 step2 产出结构：%s", mergeErr.Error())}}
		}
		report, qerr := reference.QuarantineAndReport(g.dir, episode, reference.QuarantineKindStep2, draft.Content, violations, nil, draft.Extra)
		if qerr != nil {
			return "", qerr
		}
		return "", &reference.DraftViolation{Code: "quarantined", Message: report}
	}
	filename := EpisodeScriptFilename(episode)
	if err := g.store.SaveScript(g.projectName, filename, script); err != nil {
		return "", err
	}
	if err := reference.ClearQuarantine(g.dir, episode, reference.QuarantineKindStep2); err != nil {
		return "", err
	}
	return filepath.Join(g.dir, "scripts", filename), nil
}
