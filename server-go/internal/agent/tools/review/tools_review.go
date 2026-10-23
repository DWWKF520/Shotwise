package review

import "github.com/shotwise/shotwise/internal/agent/tools"

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// inspect_director_review：只读审阅已生成的视频单元（对应 sdk_tools/director_review.py）。
//
// 返回多模态工具结果：文本部件给出确定性发现项与媒体探测 JSON，每张抽帧图片前附一条
// 「剧本 / unit / 位置 frame」标签文本，供导演 Skill 逐帧查看。不生成、不合成、不持久化任何文件。

const (
	inspectDirectorReviewName = "inspect_director_review"
	inspectDirectorReviewDesc = "只读审阅当前项目已生成的视频单元：检查缺失、顺序、媒体可读性、时长和音频信息，并返回首帧/中帧/尾帧供导演 Skill 实际查看。不会自动重生成、合成视频或保存审阅结果。"

	reviewDurationToleranceSeconds = 0.75
	reviewFrameMIME                = "image/jpeg"
)

var reviewUnitNumberRE = regexp.MustCompile(`(?i)U([0-9]+)$`)

// reviewFrame 是输出 JSON 里的一帧描述（对应 Python _frame dict）。
type reviewFrame struct {
	Position         string  `json:"position"`
	TimestampSeconds float64 `json:"timestamp_seconds"`
	Included         bool    `json:"included"`
}

// inspectDirectorReviewIn 与 Python 工具的 input_schema 保持一致。可选字段一律 omitempty，
// 指针型用于区分「未传」与零值（include_frames 缺省为 true）。
type inspectDirectorReviewIn struct {
	Script              *string  `json:"script,omitempty" jsonschema:"description=剧本文件名，例如 episode_1.json；scope=project 时省略。"`
	Scope               *string  `json:"scope,omitempty" jsonschema:"description=审阅范围：episode（默认）、project 或 units。"`
	UnitIDs             []string `json:"unit_ids,omitempty" jsonschema:"description=scope=units 时指定要审阅的 unit。"`
	IncludeFrames       *bool    `json:"include_frames,omitempty" jsonschema:"description=是否抽取帧图片，省略为 true。"`
	Sampling            *string  `json:"sampling,omitempty" jsonschema:"description=抽帧采样：start_middle_end（默认，首/中/尾）或 start_middle。"`
	IncludeShortSamples *bool    `json:"include_short_samples,omitempty" jsonschema:"description=是否额外抽取 25%、75% 和尾部短采样帧，省略为 false。"`
}

// reviewFinding 是确定性发现项（字段与 Python _finding 一一对应）。
type reviewFinding struct {
	Code           string   `json:"code"`
	Severity       string   `json:"severity"`
	UnitIDs        []string `json:"unit_ids"`
	Title          string   `json:"title"`
	Reason         string   `json:"reason"`
	Recommendation string   `json:"recommendation"`
	Action         string   `json:"action"`
	Confidence     float64  `json:"confidence"`
}

func reviewFindingOf(code, severity string, unitIDs []string, title, reason, recommendation, action string) reviewFinding {
	return reviewFinding{
		Code: code, Severity: severity, UnitIDs: unitIDs, Title: title, Reason: reason,
		Recommendation: recommendation, Action: action, Confidence: 1.0,
	}
}

// reviewedUnit 是单个视频单元的审阅结果。
type reviewedUnit struct {
	UnitID                 string            `json:"unit_id"`
	PlannedDurationSeconds *float64          `json:"planned_duration_seconds"`
	Shots                  []string          `json:"shots"`
	References             []tools.ReviewRef `json:"references"`
	GeneratedStatus        any               `json:"generated_status"`
	ReviewStatus           string            `json:"review_status"`
	VideoPath              *string           `json:"video_path"`
	MediaProbe             map[string]any    `json:"media_probe"`
	Frames                 []reviewFrame     `json:"frames"`
	DeterministicFindings  []reviewFinding   `json:"deterministic_findings"`
}

// reviewFrameData 是一张已抽取的帧图片（仅存在内存中，随结果返回）。
type reviewFrameData struct {
	scriptName string
	unitID     string
	position   string
	data       []byte
}

type reviewEpisodeSummary struct {
	UnitCount                int     `json:"unit_count"`
	ReviewedUnitCount        int     `json:"reviewed_unit_count"`
	CompletedCount           int     `json:"completed_count"`
	MissingCount             int     `json:"missing_count"`
	FailedCount              int     `json:"failed_count"`
	PlannedDurationSeconds   float64 `json:"planned_duration_seconds"`
	AvailableDurationSeconds float64 `json:"available_duration_seconds"`
}

type reviewEpisode struct {
	Script     string               `json:"script"`
	Collection string               `json:"collection"`
	Summary    reviewEpisodeSummary `json:"summary"`
	Units      []reviewedUnit       `json:"units"`
}

type reviewTopSummary struct {
	EpisodeCount             int     `json:"episode_count"`
	UnitCount                int     `json:"unit_count"`
	ReviewedUnitCount        int     `json:"reviewed_unit_count"`
	MissingCount             int     `json:"missing_count"`
	FailedCount              int     `json:"failed_count"`
	PlannedDurationSeconds   float64 `json:"planned_duration_seconds"`
	AvailableDurationSeconds float64 `json:"available_duration_seconds"`
}

type inspectDirectorReviewResult struct {
	Status                string            `json:"status"`
	Project               map[string]string `json:"project"`
	Scope                 string            `json:"scope"`
	Summary               reviewTopSummary  `json:"summary"`
	Episodes              []reviewEpisode   `json:"episodes"`
	DeterministicFindings []reviewFinding   `json:"deterministic_findings"`
	Limitations           []string          `json:"limitations"`
}

// reviewRun 承载单个剧本审阅的共享上下文。
type reviewRun struct {
	projectDir          string
	includeFrames       bool
	includeShortSamples bool
	sampling            string
	isAd                bool
	adShots             map[string]map[string]any
}

// NewInspectDirectorReviewTool 构造 inspect_director_review 多模态工具，project 名闭包绑定。
func NewInspectDirectorReviewTool(project string, projects tools.AssetProjects) (tool.EnhancedInvokableTool, error) {
	return toolutils.InferEnhancedTool(inspectDirectorReviewName, inspectDirectorReviewDesc,
		func(ctx context.Context, in inspectDirectorReviewIn) (*schema.ToolResult, error) {
			if projects == nil {
				return tools.DenyResult(inspectDirectorReviewName + " 失败: asset projects not configured"), nil
			}
			res, err := runInspectDirectorReview(ctx, project, projects, in)
			if err != nil {
				return tools.DenyResult(fmt.Sprintf("%s 失败: %s", inspectDirectorReviewName, err.Error())), nil
			}
			return res, nil
		})
}

type inspectArgs struct {
	scope               string
	includeFrames       bool
	includeShortSamples bool
	sampling            string
	unitIDs             map[string]bool
}

// parseInspectArgs 按 Python 工具的校验顺序校验入参。
func parseInspectArgs(in inspectDirectorReviewIn) (inspectArgs, error) {
	args := inspectArgs{scope: "episode", includeFrames: true, sampling: "start_middle_end"}
	if in.Scope != nil {
		args.scope = *in.Scope
	}
	if args.scope != "episode" && args.scope != "project" && args.scope != "units" {
		return args, errors.New("scope 只支持 episode、project 或 units")
	}
	if in.IncludeFrames != nil {
		args.includeFrames = *in.IncludeFrames
	}
	if in.IncludeShortSamples != nil {
		args.includeShortSamples = *in.IncludeShortSamples
	}
	if in.Sampling != nil {
		args.sampling = *in.Sampling
	}
	if args.sampling != "start_middle_end" && args.sampling != "start_middle" {
		return args, errors.New("sampling 只支持 start_middle_end 或 start_middle")
	}
	if in.UnitIDs != nil {
		if len(in.UnitIDs) == 0 {
			return args, errors.New("unit_ids 必须是非空字符串数组")
		}
		args.unitIDs = map[string]bool{}
		for _, id := range in.UnitIDs {
			if id == "" {
				return args, errors.New("unit_ids 必须是非空字符串数组")
			}
			args.unitIDs[id] = true
		}
	}
	if args.scope == "units" && args.unitIDs == nil {
		return args, errors.New("scope=units 时必须传入 unit_ids")
	}
	if args.scope != "units" && args.unitIDs != nil {
		return args, errors.New("只有 scope=units 时才能传 unit_ids")
	}
	return args, nil
}

func runInspectDirectorReview(ctx context.Context, project string, projects tools.AssetProjects, in inspectDirectorReviewIn) (*schema.ToolResult, error) {
	args, err := parseInspectArgs(in)
	if err != nil {
		return nil, err
	}
	dir, err := projects.Dir(project)
	if err != nil {
		return nil, err
	}
	raw, err := tools.LoadRawProject(dir)
	if err != nil {
		return nil, err
	}

	var scriptNames []string
	if args.scope == "project" {
		scriptNames = tools.ScriptCandidates(raw)
		if len(scriptNames) == 0 {
			return nil, errors.New("project.json 中没有可用剧本")
		}
	} else {
		name, err := tools.ChooseScript(raw, in.Script)
		if err != nil {
			return nil, err
		}
		scriptNames = []string{name}
	}

	run := reviewRun{
		projectDir:          dir,
		includeFrames:       args.includeFrames,
		includeShortSamples: args.includeShortSamples,
		sampling:            args.sampling,
	}
	var episodes []reviewEpisode
	var allFindings []reviewFinding
	var frames []reviewFrameData
	for _, name := range scriptNames {
		script, err := tools.LoadScript(dir, name)
		if err != nil {
			return nil, err
		}
		episode, findings, epFrames, err := reviewEpisodeOf(ctx, run, raw, script, name, args.unitIDs)
		if err != nil {
			return nil, err
		}
		episodes = append(episodes, episode)
		allFindings = append(allFindings, findings...)
		frames = append(frames, epFrames...)
	}

	summary := reviewTopSummary{EpisodeCount: len(episodes)}
	var planned, available float64
	for _, ep := range episodes {
		summary.UnitCount += ep.Summary.UnitCount
		summary.ReviewedUnitCount += ep.Summary.ReviewedUnitCount
		summary.MissingCount += ep.Summary.MissingCount
		summary.FailedCount += ep.Summary.FailedCount
		planned += ep.Summary.PlannedDurationSeconds
		available += ep.Summary.AvailableDurationSeconds
	}
	summary.PlannedDurationSeconds = tools.RoundReview3(planned)
	summary.AvailableDurationSeconds = tools.RoundReview3(available)

	result := inspectDirectorReviewResult{
		Status:                "ok",
		Project:               map[string]string{"name": project},
		Scope:                 args.scope,
		Summary:               summary,
		Episodes:              episodes,
		DeterministicFindings: allFindings,
		Limitations: []string{
			"必须由导演 Skill 实际查看返回的图像后判断镜头语言、人物/场景/道具连续性和故事节奏。",
			"音频流缺失只记录为 info，不作为阻塞问题。",
			"本次审阅不会合成最终视频，也不会自动重生成或保存审阅结果。",
		},
	}
	resultJSON, err := tools.EncodeReviewJSON(result)
	if err != nil {
		return nil, err
	}
	summaryText := fmt.Sprintf("导演审阅素材检查完成：%d 个视频单元，发现 %d 条确定性问题。请逐个查看带有 unit/位置标签的首、中、尾帧，再输出导演审阅报告。",
		summary.ReviewedUnitCount, len(allFindings))

	parts := []schema.ToolOutputPart{
		{Type: schema.ToolPartTypeText, Text: tools.OKEnvelope(summaryText)},
		{Type: schema.ToolPartTypeText, Text: resultJSON},
	}
	for _, fd := range frames {
		b64 := base64.StdEncoding.EncodeToString(fd.data)
		parts = append(parts,
			schema.ToolOutputPart{Type: schema.ToolPartTypeText,
				Text: fmt.Sprintf("%s / %s / %s frame", fd.scriptName, fd.unitID, fd.position)},
			schema.ToolOutputPart{Type: schema.ToolPartTypeImage, Image: &schema.ToolOutputImage{
				MessagePartCommon: schema.MessagePartCommon{Base64Data: &b64, MIMEType: reviewFrameMIME},
			}},
		)
	}
	return &schema.ToolResult{Parts: parts}, nil
}

// reviewEpisodeOf 审阅单个剧本（对应 _review_episode）。unitIDs 非 nil 时只审阅指定 unit，
// 未在剧本中出现的请求 id 以占位单元参与审阅（产出 video_missing）。
func reviewEpisodeOf(ctx context.Context, run reviewRun, project, script map[string]any, scriptName string,
	unitIDs map[string]bool) (reviewEpisode, []reviewFinding, []reviewFrameData, error) {
	collection, rawItems := tools.ItemCollection(script, project)
	var selected []map[string]any
	for _, item := range rawItems {
		if unitIDs == nil || unitIDs[tools.ReviewItemID(item)] {
			selected = append(selected, item)
		}
	}
	if unitIDs != nil {
		found := map[string]bool{}
		for _, item := range selected {
			found[tools.ReviewItemID(item)] = true
		}
		var missing []string
		for id := range unitIDs {
			if !found[id] {
				missing = append(missing, id)
			}
		}
		sort.Strings(missing)
		for _, id := range missing {
			selected = append(selected, map[string]any{"unit_id": id, "generated_assets": map[string]any{}})
		}
	}

	isAd := tools.ResolveContentMode(script, project) == "ad" && project["generation_mode"] == "reference_video"
	run.isAd = isAd
	if isAd {
		run.adShots = adShotsByID(script)
	}

	var units []reviewedUnit
	var findings []reviewFinding
	var frames []reviewFrameData
	for _, item := range selected {
		unit, unitFrames, itemFindings, err := reviewItem(ctx, run, item)
		if err != nil {
			return reviewEpisode{}, nil, nil, err
		}
		for i := range unitFrames {
			unitFrames[i].scriptName = scriptName
		}
		units = append(units, unit)
		findings = append(findings, itemFindings...)
		frames = append(frames, unitFrames...)
	}

	ids := make([]string, 0, len(units))
	for _, u := range units {
		ids = append(ids, u.UnitID)
	}
	sequence := sequenceFindings(ids)
	findings = append(findings, sequence...)
	for _, f := range sequence {
		affected := map[string]bool{}
		for _, id := range f.UnitIDs {
			affected[id] = true
		}
		for i := range units {
			if !affected[units[i].UnitID] {
				continue
			}
			units[i].DeterministicFindings = append(units[i].DeterministicFindings, f)
			if units[i].ReviewStatus == "keep" {
				units[i].ReviewStatus = "manual_review"
			}
		}
	}

	var plannedTotal, availableTotal float64
	var completed, missing, failed int
	for _, u := range units {
		if u.PlannedDurationSeconds != nil {
			plannedTotal += *u.PlannedDurationSeconds
		}
		if tools.BoolIs(u.MediaProbe, "has_video_stream", true) {
			completed++
			if d, ok := tools.AsNumber(u.MediaProbe["duration_seconds"]); ok {
				availableTotal += d
			}
		}
		switch u.ReviewStatus {
		case "missing":
			missing++
		case "technical_error", "stale":
			failed++
		}
	}
	scriptDuration, hasScriptDuration := tools.AsNumber(script["duration_seconds"])
	planned := plannedTotal
	if hasScriptDuration {
		planned = scriptDuration
	}
	episode := reviewEpisode{
		Script:     scriptName,
		Collection: collection,
		Summary: reviewEpisodeSummary{
			UnitCount:                len(rawItems),
			ReviewedUnitCount:        len(units),
			CompletedCount:           completed,
			MissingCount:             missing,
			FailedCount:              failed,
			PlannedDurationSeconds:   tools.RoundReview3(planned),
			AvailableDurationSeconds: tools.RoundReview3(availableTotal),
		},
		Units: units,
	}
	return episode, findings, frames, nil
}

// reviewItem 审阅单个视频单元（对应 _review_item）。返回的发现项是单元自身的原始发现，
// 不含之后由序列发现追加的条目。
func reviewItem(ctx context.Context, run reviewRun, item map[string]any) (reviewedUnit, []reviewFrameData, []reviewFinding, error) {
	itemID := tools.ReviewItemID(item)
	if itemID == "" {
		itemID = "unknown"
	}
	assets, _ := tools.AsMap(item["generated_assets"])
	planned, hasPlanned := tools.PlannedDuration(item)
	var findings []reviewFinding
	status := "keep"

	if s, _ := tools.AsString(assets["status"]); s == "failed" {
		status = "technical_error"
		findings = append(findings, reviewFindingOf("generation_failed", "blocker", []string{itemID},
			"视频单元生成失败", "generated_assets.status 标记为 failed。",
			"确认失败原因后仅重生成该视频单元。", "regenerate"))
	}

	videoPath, relPath, pathErr := tools.VideoPathFor(run.projectDir, assets)
	if pathErr != nil {
		status = "technical_error"
		findings = append(findings, reviewFindingOf("video_unreadable", "blocker", []string{itemID},
			"视频路径不安全或不可读", "video_clip 未能解析: PathTraversalError。",
			"修复该单元的视频产物路径后再审阅。", "technical_error"))
	}

	var media map[string]any
	if videoPath == "" {
		if status == "keep" {
			status = "missing"
		}
		findings = append(findings, reviewFindingOf("video_missing", "blocker", []string{itemID},
			"缺少视频片段", "该单元没有 generated_assets.video_clip。",
			"补齐该镜头的视频单元后再进行最终合成。", "missing"))
		media = map[string]any{"exists": false, "readable": false, "probe_available": tools.LookPathOK("ffprobe")}
	} else {
		media = tools.ProbeMedia(ctx, videoPath)
		switch {
		case tools.BoolIs(media, "exists", false):
			if status == "keep" {
				status = "missing"
			}
			findings = append(findings, reviewFindingOf("video_missing", "blocker", []string{itemID},
				"视频文件不存在", fmt.Sprintf("记录路径 %s 没有对应文件。", relPath),
				"补齐或重生成该视频单元。", "missing"))
		case tools.BoolIs(media, "probe_available", false):
			if status == "keep" {
				status = "manual_review"
			}
			finding := reviewFindingOf("probe_unavailable", "info", []string{itemID},
				"无法探测视频元数据", "当前环境没有 ffprobe；文件存在但无法确认时长和音视频流。",
				"安装 ffmpeg/ffprobe 后重新审阅，或由导演手动确认。", "manual_review")
			finding.Confidence = 0.5
			findings = append(findings, finding)
		case tools.BoolIs(media, "readable", false):
			status = "technical_error"
			reason, _ := tools.AsString(media["error"])
			if reason == "" {
				reason = "媒体探测失败。"
			}
			findings = append(findings, reviewFindingOf("video_unreadable", "blocker", []string{itemID},
				"视频文件不可读", reason, "先修复或重新生成该视频片段。", "regenerate"))
		case tools.BoolIs(media, "has_video_stream", false):
			status = "technical_error"
			findings = append(findings, reviewFindingOf("video_unreadable", "blocker", []string{itemID},
				"视频没有视频流", "媒体文件存在，但 ffprobe 没有发现视频流。",
				"重新生成该片段。", "regenerate"))
		}

		actual, hasActual := tools.AsNumber(media["duration_seconds"])
		if hasPlanned && hasActual && math.Abs(planned-actual) > reviewDurationToleranceSeconds {
			if status == "keep" {
				status = "manual_review"
			}
			findings = append(findings, reviewFindingOf("duration_mismatch", "high", []string{itemID},
				"成片时长与规划不一致",
				fmt.Sprintf("规划 %ss，媒体 %ss。", formatReviewG(planned), formatReviewG(actual)),
				"确认节奏是否可接受；必要时只重生成该片段。", "manual_review"))
		}
		if tools.BoolIs(media, "has_audio_stream", false) {
			findings = append(findings, reviewFindingOf("audio_missing_info", "info", []string{itemID},
				"视频没有音频流", "当前阶段只记录为信息项，不阻断导演审阅。",
				"暂不要求重生成；最终成片阶段再单独处理声音。", "keep"))
		}
		if run.isAd && adUnitStale(run.adShots, item) {
			status = "stale"
			findings = append(findings, reviewFindingOf("stale_source", "high", []string{itemID},
				"视频片段来源已过期", "广告 unit 的镜头或参考集已经与成片来源签名不一致。",
				"确认修改是否有意；需要时只重生成该 unit。", "regenerate"))
		}
	}

	var frames []reviewFrame
	var frameData []reviewFrameData
	if actual, ok := tools.AsNumber(media["duration_seconds"]); ok && run.includeFrames && tools.BoolIs(media, "has_video_stream", true) {
		type samplePos struct {
			name string
			ts   float64
		}
		positions := []samplePos{{"start", 0}, {"middle", actual / 2}}
		if run.sampling == "start_middle_end" {
			positions = append(positions, samplePos{"end", math.Max(0, actual-0.1)})
		}
		if run.includeShortSamples {
			positions = append(positions,
				samplePos{"sample_25", actual * 0.25},
				samplePos{"sample_75", actual * 0.75},
				samplePos{"sample_end", math.Max(0, actual-0.2)},
			)
		}
		dir, err := os.MkdirTemp("", "shotwise-director-review-")
		if err != nil {
			return reviewedUnit{}, nil, nil, err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		seen := map[float64]bool{}
		for i, p := range positions {
			ts := tools.RoundReview3(math.Max(0, p.ts))
			if seen[ts] {
				continue
			}
			seen[ts] = true
			data, ok := tools.ExtractFrame(ctx, videoPath, ts, dir, i)
			frames = append(frames, reviewFrame{Position: p.name, TimestampSeconds: ts, Included: ok})
			if ok {
				frameData = append(frameData, reviewFrameData{unitID: itemID, position: p.name, data: data})
			}
		}
	}
	if frames == nil {
		frames = []reviewFrame{}
	}
	if findings == nil {
		findings = []reviewFinding{}
	}

	var generatedStatus any = "pending"
	if v, ok := assets["status"]; ok {
		generatedStatus = v
	}
	unit := reviewedUnit{
		UnitID:                 itemID,
		PlannedDurationSeconds: tools.OptionalFloat(planned, hasPlanned),
		Shots:                  tools.ReviewItemTexts(item),
		References:             tools.ReviewItemReferences(item),
		GeneratedStatus:        generatedStatus,
		ReviewStatus:           status,
		MediaProbe:             media,
		Frames:                 frames,
		DeterministicFindings:  append([]reviewFinding{}, findings...),
	}
	if relPath != "" {
		unit.VideoPath = &relPath
	}
	return unit, frameData, findings, nil
}

// sequenceFindings 根据单元 ID 序列发现重复、顺序异常与序号缺口（对应 _sequence_findings）。
func sequenceFindings(ids []string) []reviewFinding {
	var findings []reviewFinding

	counts := map[string]int{}
	for _, id := range ids {
		counts[id]++
	}
	var duplicates []string
	for id, n := range counts {
		if n > 1 {
			duplicates = append(duplicates, id)
		}
	}
	sort.Strings(duplicates)
	if len(duplicates) > 0 {
		findings = append(findings, reviewFindingOf("duplicate_unit", "blocker", duplicates,
			"发现重复的视频单元", fmt.Sprintf("重复 ID：%s。", strings.Join(duplicates, ", ")),
			"修正剧本 unit 顺序或 ID 后再审阅。", "manual_review"))
	}

	var numbers []int
	for _, id := range ids {
		m := reviewUnitNumberRE.FindStringSubmatch(id)
		if m == nil {
			continue
		}
		if n, err := strconv.Atoi(m[1]); err == nil {
			numbers = append(numbers, n)
		}
	}
	if len(numbers) > 1 && !sort.IntsAreSorted(numbers) {
		findings = append(findings, reviewFindingOf("out_of_order", "high", append([]string{}, ids...),
			"视频单元顺序异常", "unit 在剧本中的顺序与其 U 序号不一致。",
			"先确认镜头顺序，必要时调整剧本顺序，而不是直接合成。", "manual_review"))
	}
	if len(numbers) > 0 {
		minN, maxN := numbers[0], numbers[0]
		present := map[int]bool{}
		for _, n := range numbers {
			present[n] = true
			minN = min(minN, n)
			maxN = max(maxN, n)
		}
		if minN == 1 {
			var missing []int
			for n := 1; n <= maxN; n++ {
				if !present[n] {
					missing = append(missing, n)
				}
			}
			if len(missing) > 0 {
				missingIDs := make([]string, 0, len(missing))
				missingNums := make([]string, 0, len(missing))
				for _, n := range missing {
					missingIDs = append(missingIDs, fmt.Sprintf("U%02d", n))
					missingNums = append(missingNums, strconv.Itoa(n))
				}
				findings = append(findings, reviewFindingOf("missing_unit", "blocker", missingIDs,
					"视频单元序号存在缺口", fmt.Sprintf("缺少 U 序号：%s。", strings.Join(missingNums, ", ")),
					"补齐或确认缺失镜头后再进行任何合成。", "missing"))
			}
		}
	}
	return findings
}

func formatReviewG(v float64) string {
	return strconv.FormatFloat(v, 'g', 6, 64)
}
