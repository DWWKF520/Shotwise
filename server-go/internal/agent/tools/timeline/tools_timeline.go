package timeline

import "github.com/shotwise/shotwise/internal/agent/tools"

// 时间线计划的 5 个内部工具（对应 server/agent_runtime/sdk_tools/episode_editing.py）。
//
// 这组工具只做「读项目/剧本/媒体清单 → 组装计划文档 → 落库」：不修改项目数据、不入队任务、
// 不调用 FFmpeg、不渲染。计划持久化经 media.MediaAssemblyService 走 assembly_plans /
// assembly_plan_revisions 两张表。
//
// 回包约定：与 Python 的 _result(payload, message) 对齐——message 是给模型看的一句话，
// payload 是结构化 JSON。Go 的工具信封只承载单段文本，故把两者拼在同一段里
// （message 换行 + 缩进 JSON）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	domainmedia "github.com/shotwise/shotwise/internal/domain/media"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service/media"
)

const (
	episodeEditingContractVersion   = "episode-editing/v1"
	episodeManifestContractVersion  = "episode-media-manifest/v1"
	timelinePersistenceReadOnly     = "read_only"
	timelinePersistenceDatabase     = "database"
	timelineGetManifestName         = "get_episode_media_manifest"
	timelineCreatePlanName          = "create_timeline_plan"
	timelineGetPlanName             = "get_timeline_plan"
	timelineUpdatePlanName          = "update_timeline_plan"
	timelineValidatePlanName        = "validate_timeline_plan"
	timelineCreatePlanDefaultSpeed  = 1.0
	timelineAudioDefaultPolicy      = "duck"
	timelineAudioDefaultDuckingDB   = -12.0
	timelineDefaultTransitionSecond = 0.5
	timelineFallbackDurationSeconds = 0.001
)

// timelineAllowedAudioPolicies 是计划级音频策略白名单（对应 Python _ALLOWED_AUDIO_POLICIES）。
var timelineAllowedAudioPolicies = map[string]bool{"duck": true, "mute": true}

// timelineAllowedPlanPatches 是 update_timeline_plan 允许更新的 plan 字段（对应 Python _ALLOWED_PLAN_PATCHES）。
var timelineAllowedPlanPatches = map[string]bool{
	"director_review": true, "timeline_items": true, "audio": true, "subtitles": true,
	"intro": true, "outro": true, "cover": true, "render_profile": true,
}

// timelinePlanPatchOrder 是 patch 应用的固定顺序：Go map 无序，固定次序保证同一 patch 得到同一结果与同一首个错误。
var timelinePlanPatchOrder = []string{
	"timeline_items", "audio", "director_review", "subtitles", "intro", "outro", "cover", "render_profile",
}

// TimelinePlanService 是时间线计划持久化能力（*media.MediaAssemblyService 满足）。
type TimelinePlanService interface {
	CreatePlan(ctx context.Context, p media.CreatePlanParams) (map[string]any, error)
	GetPlan(ctx context.Context, planID, userID string) (map[string]any, error)
	CreateRevision(ctx context.Context, planID string, p media.CreateRevisionParams) (map[string]any, error)
}

// timelineToolset 是单个项目的时间线工具上下文。
type timelineToolset struct {
	project    string
	projectDir string
	plans      TimelinePlanService
	userID     string
}

// NewTools 构造项目绑定的时间线工具集；plans 为 nil 时返回 (nil, nil)。
// userID 固定取 repository.DefaultUserID（单用户部署形态）。
func NewTools(project, projectDir string, plans TimelinePlanService) ([]tool.BaseTool, error) {
	if plans == nil {
		return nil, nil
	}
	defaults := timelineToolset{
		project:    project,
		projectDir: projectDir,
		plans:      plans,
		userID:     repository.DefaultUserID,
	}
	built := make([]tool.BaseTool, 0, 5)
	ctrs := []struct {
		name string
		ctor func() (tool.InvokableTool, error)
	}{
		{timelineGetManifestName, func() (tool.InvokableTool, error) { return newGetEpisodeMediaManifestTool(defaults) }},
		{timelineCreatePlanName, func() (tool.InvokableTool, error) { return newCreateTimelinePlanTool(defaults) }},
		{timelineGetPlanName, func() (tool.InvokableTool, error) { return newGetTimelinePlanTool(defaults) }},
		{timelineUpdatePlanName, func() (tool.InvokableTool, error) { return newUpdateTimelinePlanTool(defaults) }},
		{timelineValidatePlanName, func() (tool.InvokableTool, error) { return newValidateTimelinePlanTool(defaults) }},
	}
	for _, c := range ctrs {
		builtTool, err := c.ctor()
		if err != nil {
			return nil, fmt.Errorf("timeline: build %s: %w", c.name, err)
		}
		built = append(built, builtTool)
	}
	return built, nil
}

// --- 工具入参 ---

type getEpisodeMediaManifestIn struct {
	Script *string `json:"script,omitempty" jsonschema:"description=剧本文件名；省略时使用当前剧集。"`
}

type createTimelinePlanIn struct {
	Script         *string        `json:"script,omitempty" jsonschema:"description=剧本文件名；省略时使用当前剧集。"`
	DirectorReview map[string]any `json:"director_review,omitempty" jsonschema:"description=导演审阅结果对象（inspect_director_review 输出）；省略时记为 pending 待审。"`
	TimelineItems  []any          `json:"timeline_items,omitempty" jsonschema:"description=时间线条目数组；省略时按媒体清单顺序生成默认条目（cut + duck）。"`
}

type getTimelinePlanIn struct {
	PlanID string `json:"plan_id" jsonschema:"description=正式时间线计划 id"`
}

type updateTimelinePlanIn struct {
	PlanID string         `json:"plan_id" jsonschema:"description=正式时间线计划 id"`
	Patch  map[string]any `json:"patch" jsonschema:"description=允许更新的 plan 字段对象（director_review/timeline_items/audio/subtitles/intro/outro/cover/render_profile）"`
}

type validateTimelinePlanIn struct {
	PlanID string `json:"plan_id" jsonschema:"description=正式时间线计划 id"`
}

// --- 工具构造 ---

func newGetEpisodeMediaManifestTool(t timelineToolset) (tool.InvokableTool, error) {
	return toolutils.InferTool(timelineGetManifestName,
		"只读获取剧集视频单元媒体清单、顺序、路径和媒体探测信息；不修改项目、不创建计划、不渲染。",
		func(ctx context.Context, in getEpisodeMediaManifestIn) (string, error) {
			manifest, err := t.buildManifest(ctx, in.Script)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineGetManifestName, err, nil)), nil
			}
			items := itemListOf(manifest["items"])
			message := fmt.Sprintf("已读取 %v 的 %d 个视频单元媒体清单。", manifest["script"], len(items))
			return expectOK(timelineGetManifestName, message, manifest)
		})
}

func newCreateTimelinePlanTool(t timelineToolset) (tool.InvokableTool, error) {
	return toolutils.InferTool(timelineCreatePlanName,
		"创建正式持久化的成片时间线计划；默认采用 duck 保留视频环境声，只做计划和校验前准备，不调用 FFmpeg、不渲染。",
		func(ctx context.Context, in createTimelinePlanIn) (string, error) {
			manifest, err := t.buildManifest(ctx, in.Script)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineCreatePlanName, err, nil)), nil
			}
			draft, err := normalizeTimelinePlan("pending", manifest, boxMap(in.DirectorReview), boxSlice(in.TimelineItems))
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineCreatePlanName, err, nil)), nil
			}
			document, err := serviceDocument(draft, manifest)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineCreatePlanName, err, nil)), nil
			}
			payload, err := t.plans.CreatePlan(ctx, media.CreatePlanParams{
				UserID:         t.userID,
				ProjectName:    t.project,
				Name:           fmt.Sprintf("%v assembly", manifest["script"]),
				Scope:          "episode",
				EpisodeNumber:  manifestEpisodePtr(manifest),
				SourceManifest: manifest,
				Timeline:       documentTimeline(document),
				Audio:          documentMap(document, "audio"),
				Subtitle:       documentMap(document, "subtitle"),
				Packaging:      documentMap(document, "packaging"),
				OutputProfile:  documentMap(document, "output_profile"),
			})
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineCreatePlanName, err, assemblyValidationLog(err))), nil
			}
			plan, err := planFromService(payload)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineCreatePlanName, err, nil)), nil
			}
			message := fmt.Sprintf("已创建正式时间线计划 %v（database，revision %v）。", plan["plan_id"], plan["revision"])
			return expectOK(timelineCreatePlanName, message, plan)
		})
}

func newGetTimelinePlanTool(t timelineToolset) (tool.InvokableTool, error) {
	return toolutils.InferTool(timelineGetPlanName,
		"读取当前用户拥有的正式持久化时间线计划；不会执行渲染。",
		func(ctx context.Context, in getTimelinePlanIn) (string, error) {
			planID, err := requirePlanID(in.PlanID)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineGetPlanName, err, nil)), nil
			}
			payload, err := t.plans.GetPlan(ctx, planID, t.userID)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineGetPlanName, err, nil)), nil
			}
			plan, err := planFromService(payload)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineGetPlanName, err, nil)), nil
			}
			return expectOK(timelineGetPlanName, fmt.Sprintf("已读取正式时间线计划 %s。", planID), plan)
		})
}

func newUpdateTimelinePlanTool(t timelineToolset) (tool.InvokableTool, error) {
	return toolutils.InferTool(timelineUpdatePlanName,
		"更新正式持久化时间线计划的允许字段并创建 immutable revision；不渲染。",
		func(ctx context.Context, in updateTimelinePlanIn) (string, error) {
			planID, err := requirePlanID(in.PlanID)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineUpdatePlanName, err, nil)), nil
			}
			if len(in.Patch) == 0 {
				return tools.DenyEnvelope(toolFailure(timelineUpdatePlanName, errors.New("patch 必须是非空对象"), nil)), nil
			}
			manifest, err := t.buildManifest(ctx, nil)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineUpdatePlanName, err, nil)), nil
			}
			currentPayload, err := t.plans.GetPlan(ctx, planID, t.userID)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineUpdatePlanName, err, nil)), nil
			}
			current, err := planFromService(currentPayload)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineUpdatePlanName, err, nil)), nil
			}
			if err := applyPlanPatch(current, in.Patch); err != nil {
				return tools.DenyEnvelope(toolFailure(timelineUpdatePlanName, err, nil)), nil
			}
			document, err := serviceDocument(current, manifest)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineUpdatePlanName, err, nil)), nil
			}
			payload, err := t.plans.CreateRevision(ctx, planID, media.CreateRevisionParams{
				UserID:           t.userID,
				SourceManifest:   manifest,
				Timeline:         documentTimeline(document),
				Audio:            documentMap(document, "audio"),
				Subtitle:         documentMap(document, "subtitle"),
				Packaging:        documentMap(document, "packaging"),
				OutputProfile:    documentMap(document, "output_profile"),
				ExpectedRevision: payloadRevisionNumber(currentPayload),
			})
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineUpdatePlanName, err, assemblyValidationLog(err))), nil
			}
			message := fmt.Sprintf("已更新正式时间线计划 %s（revision %v）。", planID, payload["current_revision_number"])
			return expectOK(timelineUpdatePlanName, message, mustPlanFromService(timelineUpdatePlanName, payload))
		})
}

func newValidateTimelinePlanTool(t timelineToolset) (tool.InvokableTool, error) {
	return toolutils.InferTool(timelineValidatePlanName,
		"对正式持久化时间线计划执行确定性校验并写入新 revision：导演审阅、单元完整性、顺序、媒体可用性和音频策略；不渲染。",
		func(ctx context.Context, in validateTimelinePlanIn) (string, error) {
			planID, err := requirePlanID(in.PlanID)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineValidatePlanName, err, nil)), nil
			}
			manifest, err := t.buildManifest(ctx, nil)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineValidatePlanName, err, nil)), nil
			}
			currentPayload, err := t.plans.GetPlan(ctx, planID, t.userID)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineValidatePlanName, err, nil)), nil
			}
			current, err := planFromService(currentPayload)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineValidatePlanName, err, nil)), nil
			}
			validation := timelineValidation(current, manifest)
			document, err := serviceDocument(current, manifest)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineValidatePlanName, err, nil)), nil
			}
			payload, err := t.plans.CreateRevision(ctx, planID, media.CreateRevisionParams{
				UserID:             t.userID,
				SourceManifest:     manifest,
				Timeline:           documentTimeline(document),
				Audio:              documentMap(document, "audio"),
				Subtitle:           documentMap(document, "subtitle"),
				Packaging:          documentMap(document, "packaging"),
				OutputProfile:      documentMap(document, "output_profile"),
				ValidationMetadata: validation,
				ExpectedRevision:   payloadRevisionNumber(currentPayload),
			})
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineValidatePlanName, err, assemblyValidationLog(err))), nil
			}
			plan, err := planFromService(payload)
			if err != nil {
				return tools.DenyEnvelope(toolFailure(timelineValidatePlanName, err, nil)), nil
			}
			plan["validation"] = validation
			if valid, _ := validation["valid"].(bool); valid {
				plan["status"] = "validated"
			} else {
				plan["status"] = "draft"
			}
			message := "时间线校验完成：存在阻塞问题。"
			if valid, _ := validation["valid"].(bool); valid {
				message = "时间线校验完成：通过。"
			}
			return expectOK(timelineValidatePlanName, message, plan)
		})
}

// --- 清单构建 ---

// buildManifest 组装剧集媒体清单（对应 _build_manifest）。
func (t timelineToolset) buildManifest(ctx context.Context, scriptArg *string) (map[string]any, error) {
	project, err := tools.LoadRawProject(t.projectDir)
	if err != nil {
		return nil, err
	}
	scriptName, err := tools.ChooseScript(project, scriptArg)
	if err != nil {
		return nil, err
	}
	script, err := tools.LoadScript(t.projectDir, scriptName)
	if err != nil {
		return nil, err
	}
	collection, rawItems := tools.ItemCollection(script, project)

	items := make([]any, 0, len(rawItems))
	for i, rawItem := range rawItems {
		order := i + 1
		itemID := tools.ReviewItemID(rawItem)
		if itemID == "" {
			itemID = fmt.Sprintf("unknown-%d", order)
		}
		assets, _ := tools.AsMap(rawItem["generated_assets"])
		mediaPath, relPath, pathErr := tools.VideoPathFor(t.projectDir, assets)
		if pathErr != nil {
			mediaPath, relPath = "", ""
		}
		var mediaProbe map[string]any
		if mediaPath != "" {
			mediaProbe = tools.ProbeMedia(ctx, mediaPath)
		} else {
			mediaProbe = map[string]any{
				"exists": false, "readable": false, "probe_available": false, "has_video_stream": false,
				"has_audio_stream": nil, "duration_seconds": nil, "width": nil, "height": nil,
			}
		}
		planned, hasPlanned := tools.PlannedDuration(rawItem)
		items = append(items, map[string]any{
			"order":                    order,
			"unit_id":                  itemID,
			"planned_duration_seconds": tools.OptionalFloat(planned, hasPlanned),
			"description":              tools.ReviewItemTexts(rawItem),
			"references":               tools.ReviewItemReferences(rawItem),
			"source":                   map[string]any{"path": nilIfBlank(relPath), "kind": "generated_video"},
			"asset_status":             assets["status"],
			"version":                  itemVersion(assets, rawItem),
			"file_fingerprint":         fileFingerprint(mediaPath),
			"media_probe":              mediaProbe,
		})
	}

	encoded, err := domainmedia.CanonicalJSON(map[string]any{
		"script": scriptName, "collection": collection, "items": items,
	})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	return map[string]any{
		"contract_version": episodeManifestContractVersion,
		"persistence":      timelinePersistenceReadOnly,
		"project_manifest": map[string]any{
			"name":            t.project,
			"generation_mode": project["generation_mode"],
			"content_mode":    project["content_mode"],
			"current_episode": project["current_episode"],
			"episodes":        mapValueOr(project, "episodes", []any{}),
		},
		"script":          scriptName,
		"episode":         episodeNumberValue(project, scriptName),
		"content_mode":    tools.ResolveContentMode(script, project),
		"generation_mode": project["generation_mode"],
		"collection":      collection,
		"fingerprint":     hex.EncodeToString(sum[:]),
		"items":           items,
	}, nil
}

// episodeNumberValue 取剧集元数据里的集号；非整数（含布尔）返回 nil。
func episodeNumberValue(project map[string]any, scriptName string) any {
	meta := tools.EpisodeMetaFor(project, scriptName)
	if n, ok := integerOf(meta["episode"]); ok {
		return n
	}
	return nil
}

// manifestEpisodePtr 把清单里的集号转成创建计划所需的指针值。
func manifestEpisodePtr(manifest map[string]any) *int64 {
	if n, ok := integerOf(manifest["episode"]); ok {
		return &n
	}
	return nil
}

// itemVersion 取单元版本：assets 里 version/media_version/video_version/video_clip_version 首个非空，
// 否则回退单元自身的 version（对应 Python 的生成器表达式 + next 默认值）。
func itemVersion(assets, rawItem map[string]any) any {
	for _, key := range []string{"version", "media_version", "video_version", "video_clip_version"} {
		if v, present := assets[key]; present && v != nil {
			return v
		}
	}
	return rawItem["version"]
}

// fileFingerprint 计算媒体文件的指纹：大小、mtime（纳秒）与全文 sha256。
// 文件缺失或不可读时对应字段留空，不报错（对应 _file_fingerprint）。
func fileFingerprint(path string) map[string]any {
	if path == "" {
		return map[string]any{"exists": false, "size": nil, "mtime_ns": nil, "sha256": nil}
	}
	st, err := os.Stat(path)
	if err != nil {
		return map[string]any{"exists": false, "size": nil, "mtime_ns": nil, "sha256": nil}
	}
	size := st.Size()
	mtime := st.ModTime().UnixNano()
	file, err := os.Open(path)
	if err != nil {
		return map[string]any{"exists": true, "size": size, "mtime_ns": mtime, "sha256": nil}
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return map[string]any{"exists": true, "size": size, "mtime_ns": mtime, "sha256": nil}
	}
	return map[string]any{
		"exists": true, "size": size, "mtime_ns": mtime, "sha256": hex.EncodeToString(digest.Sum(nil)),
	}
}

// --- 计划归一 ---

// manifestTimelineItems 按清单顺序生成默认时间线条目（对应 _manifest_timeline_items）。
func manifestTimelineItems(manifest map[string]any) []any {
	items := itemListOf(manifest["items"])
	out := make([]any, 0, len(items))
	for _, raw := range items {
		item, ok := tools.AsMap(raw)
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"order":              item["order"],
			"unit_id":            item["unit_id"],
			"source":             deepCopyJSON(item["source"]),
			"trim":               map[string]any{"start_seconds": 0.0, "end_seconds": nil},
			"speed":              timelineCreatePlanDefaultSpeed,
			"transition_to_next": "cut",
			"audio_policy":       timelineAudioDefaultPolicy,
		})
	}
	return out
}

// validateTimelineItem 归一 Agent 面向的紧凑时间线条目（对应 _validate_timeline_item）。
//
// 播放顺序由数组位置决定；同时兼容旧会话发出的 1-based order + video_clip + 绝对
// start_seconds/end_seconds + 标量 transition 形态。
func validateTimelineItem(raw any, index int) (map[string]any, error) {
	item, ok := tools.AsMap(raw)
	if !ok {
		return nil, fmt.Errorf("timeline_items[%d] 必须是对象", index)
	}
	unitID, ok := tools.AsString(item["unit_id"])
	if !ok || strings.TrimSpace(unitID) == "" {
		return nil, fmt.Errorf("timeline_items[%d].unit_id 必须是非空字符串", index)
	}

	normalized := deepCopyMap(item)
	normalized["unit_id"] = strings.TrimSpace(unitID)
	normalized["order"] = index + 1

	if _, present := normalized["source"]; !present {
		if clip, ok := tools.AsString(normalized["video_clip"]); ok && strings.TrimSpace(clip) != "" {
			normalized["source"] = map[string]any{"path": strings.TrimSpace(clip), "kind": "generated_video"}
		}
	}

	if _, present := normalized["trim"]; !present {
		_, hasStart := normalized["start_seconds"]
		_, hasEnd := normalized["end_seconds"]
		if hasStart || hasEnd {
			normalized["trim"] = map[string]any{
				"start_seconds": mapValueOr(normalized, "start_seconds", 0.0),
				"end_seconds":   mapValueOr(normalized, "end_seconds", nil),
			}
		}
	}

	if _, present := normalized["transition_to_next"]; !present {
		if transition, hasTransition := normalized["transition"]; hasTransition {
			switch value := transition.(type) {
			case string:
				normalized["transition_to_next"] = value
			case map[string]any:
				if name, ok := tools.AsString(value["type"]); ok && strings.TrimSpace(name) != "" {
					normalized["transition_to_next"] = strings.TrimSpace(name)
				}
			default:
				return nil, fmt.Errorf("timeline_items[%d].transition 必须是字符串或对象", index)
			}
		}
	}
	setDefaultKey(normalized, "transition_to_next", "cut")
	setDefaultKey(normalized, "audio_policy", timelineAudioDefaultPolicy)
	return normalized, nil
}

// normalizePlanAudio 归一计划级音频段（对应 _normalize_audio）。
func normalizePlanAudio(audio any) (map[string]any, error) {
	if audio == nil {
		return map[string]any{
			"unit_audio_policy": timelineAudioDefaultPolicy, "tracks": []any{}, "ducking_db": timelineAudioDefaultDuckingDB,
		}, nil
	}
	m, ok := tools.AsMap(audio)
	if !ok {
		return nil, errors.New("audio 必须是对象")
	}
	result := deepCopyMap(m)
	policy := mapValueOr(result, "unit_audio_policy", timelineAudioDefaultPolicy)
	if p, ok := policy.(string); !ok || !timelineAllowedAudioPolicies[p] {
		return nil, errors.New("audio.unit_audio_policy 只支持 duck 或 mute")
	}
	result["unit_audio_policy"] = policy
	if tracks, present := result["tracks"]; present {
		if _, ok := tools.AsList(tracks); !ok {
			return nil, errors.New("audio.tracks 必须是数组")
		}
	}
	setDefaultKey(result, "ducking_db", timelineAudioDefaultDuckingDB)
	return result, nil
}

// applyPlanPatch 把 patch 应用到计划上（对应 _apply_patch）。
func applyPlanPatch(plan, patch map[string]any) error {
	var unknown []string
	for key := range patch {
		if !timelineAllowedPlanPatches[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("不允许更新的 plan 字段：%s", pyStrList(unknown))
	}
	for _, key := range timelinePlanPatchOrder {
		value, present := patch[key]
		if !present {
			continue
		}
		switch key {
		case "timeline_items":
			items, ok := tools.AsList(value)
			if !ok {
				return errors.New("timeline_items 必须是数组")
			}
			normalized := make([]any, 0, len(items))
			for index, item := range items {
				normalizedItem, err := validateTimelineItem(item, index)
				if err != nil {
					return err
				}
				normalized = append(normalized, normalizedItem)
			}
			plan[key] = normalized
		case "audio":
			normalizedAudio, err := normalizePlanAudio(value)
			if err != nil {
				return err
			}
			plan[key] = normalizedAudio
		case "director_review":
			m, ok := tools.AsMap(value)
			if !ok {
				return errors.New("director_review 必须是对象")
			}
			plan[key] = deepCopyMap(m)
		default:
			m, ok := tools.AsMap(value)
			if !ok {
				return fmt.Errorf("%s 必须是对象", key)
			}
			plan[key] = deepCopyMap(m)
		}
	}
	return nil
}

// normalizeTimelinePlan 组装 Agent 面向的计划草稿（对应 _normalize_plan）。
func normalizeTimelinePlan(
	planID string, manifest map[string]any, directorReviewResult any, timelineItems any,
) (map[string]any, error) {
	var normalizedItems []any
	switch {
	case timelineItems == nil:
		normalizedItems = manifestTimelineItems(manifest)
	case isListValue(timelineItems):
		raw := itemListOf(timelineItems)
		normalizedItems = make([]any, 0, len(raw))
		for index, item := range raw {
			normalizedItem, err := validateTimelineItem(item, index)
			if err != nil {
				return nil, err
			}
			normalizedItems = append(normalizedItems, normalizedItem)
		}
	default:
		return nil, errors.New("timeline_items 必须是数组")
	}

	review := directorReviewResult
	if review == nil {
		review = map[string]any{"status": "pending", "source": "inspect_director_review", "required": true}
	}
	reviewMap, ok := tools.AsMap(review)
	if !ok {
		return nil, errors.New("director_review 必须是对象")
	}
	defaultAudio, err := normalizePlanAudio(nil)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"contract_version":     episodeEditingContractVersion,
		"persistence":          timelinePersistenceDatabase,
		"plan_id":              planID,
		"revision":             1,
		"status":               "draft",
		"script":               manifest["script"],
		"episode":              manifest["episode"],
		"content_mode":         manifest["content_mode"],
		"generation_mode":      manifest["generation_mode"],
		"manifest_fingerprint": manifest["fingerprint"],
		"director_review":      deepCopyMap(reviewMap),
		"timeline_items":       normalizedItems,
		"audio":                defaultAudio,
		"subtitles":            map[string]any{"mode": "burn_in", "cues": []any{}, "source": "script"},
		"intro":                map[string]any{"enabled": false, "kind": "title_card", "duration_seconds": 0.0},
		"outro":                map[string]any{"enabled": false, "kind": "end_card", "duration_seconds": 0.0},
		"cover":                map[string]any{"enabled": false, "source": nil},
		"render_profile": map[string]any{
			"preview": map[string]any{"width": 640, "height": 360},
			"final":   map[string]any{"width": 1920, "height": 1080},
		},
		"validation": nil,
	}, nil
}

// --- 确定性校验 ---

// timelineValidation 对计划与媒体清单做确定性校验（对应 _validation）。
func timelineValidation(plan, manifest map[string]any) map[string]any {
	issues := []map[string]any{}

	review, _ := tools.AsMap(plan["director_review"])
	reviewStatus := ""
	if review != nil {
		reviewStatus, _ = tools.AsString(review["status"])
	}
	if reviewStatus != "approved" && reviewStatus != "keep" {
		issues = append(issues, map[string]any{
			"code": "director_review_required", "severity": "blocker", "message": "导演审阅尚未通过",
		})
	}

	manifestItems := itemListOf(manifest["items"])
	manifestIDs := make([]string, 0, len(manifestItems))
	for _, raw := range manifestItems {
		item, ok := tools.AsMap(raw)
		if !ok {
			continue
		}
		if id, ok := tools.AsString(item["unit_id"]); ok {
			manifestIDs = append(manifestIDs, id)
		}
	}

	seen := map[string]bool{}
	timelineIDs := make([]string, 0)
	for _, raw := range itemListOf(plan["timeline_items"]) {
		item, ok := tools.AsMap(raw)
		if !ok {
			continue
		}
		unitID, ok := tools.AsString(item["unit_id"])
		if !ok {
			continue
		}
		timelineIDs = append(timelineIDs, unitID)
		if seen[unitID] {
			issues = append(issues, map[string]any{
				"code": "duplicate_unit_id", "severity": "blocker", "unit_id": unitID, "message": "时间线存在重复视频单元",
			})
		}
		seen[unitID] = true
		if !containsString(manifestIDs, unitID) {
			issues = append(issues, map[string]any{
				"code": "unit_not_in_manifest", "severity": "blocker", "unit_id": unitID, "message": "视频单元不属于当前媒体清单",
			})
		}
	}
	for _, unitID := range manifestIDs {
		if !seen[unitID] {
			issues = append(issues, map[string]any{
				"code": "unit_missing_from_plan", "severity": "blocker", "unit_id": unitID, "message": "媒体清单中的视频单元未加入时间线",
			})
		}
	}
	if len(timelineIDs) > 0 && !hasIssueCode(issues, "unit_not_in_manifest") && !stringSlicesEqual(timelineIDs, manifestIDs) {
		issues = append(issues, map[string]any{
			"code": "order_mismatch", "severity": "warning", "message": "时间线顺序与媒体清单顺序不同，请确认是否为有意调整",
		})
	}

	for _, raw := range manifestItems {
		item, ok := tools.AsMap(raw)
		if !ok {
			continue
		}
		probe, _ := tools.AsMap(item["media_probe"])
		if !tools.BoolIs(probe, "has_video_stream", true) {
			unitID, _ := tools.AsString(item["unit_id"])
			issues = append(issues, map[string]any{
				"code": "media_missing_or_unreadable", "severity": "blocker", "unit_id": unitID, "message": "视频单元缺少可用视频流",
			})
		}
	}

	valid := true
	for _, issue := range issues {
		if severity, _ := tools.AsString(issue["severity"]); severity == "blocker" {
			valid = false
			break
		}
	}
	return map[string]any{"valid": valid, "issues": issues}
}

// assemblyValidationLog 渲染结构化领域校验问题（对应 _assembly_validation_log）。
func assemblyValidationLog(err error) []string {
	var validationErr *domainmedia.AssemblyPlanValidationError
	if !errors.As(err, &validationErr) {
		return nil
	}
	messages := make([]string, 0, len(validationErr.Errors))
	for _, issue := range validationErr.Errors {
		path := mapValueOr(issue, "path", "plan")
		code := mapValueOr(issue, "code", "validation_error")
		message := mapValueOr(issue, "message", "invalid value")
		messages = append(messages, fmt.Sprintf("%v: %v - %v", path, code, message))
	}
	if len(messages) == 0 {
		return []string{"plan: validation_error - 未提供具体校验详情"}
	}
	return messages
}

// --- 服务文档 ---

// serviceTimeline 把紧凑时间线条目转成装配域时间线（对应 _service_timeline）。
func serviceTimeline(items []any, manifest map[string]any) ([]map[string]any, error) {
	manifestByID := map[string]map[string]any{}
	for _, raw := range itemListOf(manifest["items"]) {
		item, ok := tools.AsMap(raw)
		if !ok {
			continue
		}
		if id, ok := tools.AsString(item["unit_id"]); ok {
			manifestByID[id] = item
		}
	}

	result := make([]map[string]any, 0, len(items))
	for index, raw := range items {
		item, ok := tools.AsMap(raw)
		if !ok {
			continue
		}
		unitID, ok := tools.AsString(item["unit_id"])
		if !ok {
			return nil, fmt.Errorf("timeline_items[%d].unit_id 缺失", index)
		}
		source, _ := tools.AsMap(item["source"])
		sourceItem := manifestByID[unitID]
		manifestSource, _ := tools.AsMap(sourceItem["source"])
		// 紧凑契约可能只用 unit_id 标识片段：先回退到清单里的媒体路径，再回退到 unit id，
		// 让落盘的时间线引用真实文件而不是单元名。
		sourceRef, _ := tools.AsString(source["path"])
		if sourceRef == "" {
			sourceRef, _ = tools.AsString(manifestSource["path"])
		}
		if sourceRef == "" {
			sourceRef = unitID
		}

		probe, _ := tools.AsMap(sourceItem["media_probe"])
		durationRaw := item["duration_seconds"]
		duration, ok := tools.AsNumber(durationRaw)
		if !ok || duration <= 0 {
			duration = firstDurationNumber(sourceItem["planned_duration_seconds"], probe["duration_seconds"])
		}

		trim, _ := tools.AsMap(item["trim"])
		startRaw := mapValueOr(trim, "start_seconds", 0.0)
		start, ok := tools.AsNumber(startRaw)
		if !ok {
			return nil, fmt.Errorf("timeline_items[%d].trim.start_seconds 必须是数字", index)
		}
		endTrim := 0.0
		if end := trim["end_seconds"]; end != nil {
			endValue, ok := tools.AsNumber(end)
			if !ok {
				return nil, fmt.Errorf("timeline_items[%d].trim.end_seconds 必须是数字", index)
			}
			endTrim = math.Max(0.0, duration-endValue)
		}

		transitionName, _ := tools.AsString(mapValueOr(item, "transition_to_next", "cut"))
		if !domainmedia.SupportedTimelineTransitions[transitionName] {
			return nil, fmt.Errorf("timeline_items[%d].transition_to_next must be cut, fade, dissolve, or wipe", index)
		}
		var transition any
		if transitionName != "cut" {
			transitionDuration := item["transition_duration_seconds"]
			if rawTransition, ok := tools.AsMap(item["transition"]); ok {
				if v, present := rawTransition["duration_seconds"]; present {
					transitionDuration = v
				}
			}
			seconds, ok := tools.AsNumber(transitionDuration)
			if !ok || seconds <= 0 {
				seconds = timelineDefaultTransitionSecond
			}
			transition = map[string]any{"type": transitionName, "duration_seconds": seconds}
		}

		result = append(result, map[string]any{
			"id":                 fmt.Sprintf("%s:%d", unitID, index),
			"source_unit_id":     unitID,
			"order":              index,
			"kind":               "video_unit",
			"source_ref":         sourceRef,
			"duration_seconds":   duration,
			"trim_start_seconds": start,
			"trim_end_seconds":   endTrim,
			"audio_policy":       mapValueOr(item, "audio_policy", timelineAudioDefaultPolicy),
			"transition":         transition,
		})
	}
	return result, nil
}

// serviceDocument 组装装配域计划文档（对应 _service_document）。
func serviceDocument(plan, manifest map[string]any) (map[string]any, error) {
	packaging := map[string]any{
		"director_review": deepCopyJSON(mapValueOr(plan, "director_review", map[string]any{})),
		"intro":           deepCopyJSON(mapValueOr(plan, "intro", map[string]any{})),
		"outro":           deepCopyJSON(mapValueOr(plan, "outro", map[string]any{})),
		"cover":           deepCopyJSON(mapValueOr(plan, "cover", map[string]any{})),
	}
	var renderProfile map[string]any
	if profile, ok := tools.AsMap(plan["render_profile"]); ok {
		renderProfile = deepCopyMap(profile)
	} else {
		renderProfile = map[string]any{"format": "mp4"}
	}
	setDefaultKey(renderProfile, "format", "mp4")
	timeline, err := serviceTimeline(itemListOf(plan["timeline_items"]), manifest)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"source_manifest": manifest,
		"timeline":        timeline,
		"audio":           deepCopyJSON(mapValueOr(plan, "audio", map[string]any{})),
		"subtitle":        deepCopyJSON(mapValueOr(plan, "subtitles", map[string]any{})),
		"packaging":       packaging,
		"output_profile":  renderProfile,
	}, nil
}

// documentTimeline 从装配文档取时间线（类型为 []map[string]any）。
func documentTimeline(document map[string]any) []map[string]any {
	timeline, _ := document["timeline"].([]map[string]any)
	return timeline
}

// documentMap 从装配文档取对象字段。
func documentMap(document map[string]any, key string) map[string]any {
	m, _ := tools.AsMap(document[key])
	return m
}

// --- 服务信封还原 ---

// planFromService 把服务信封还原为 Agent 面向的计划（对应 _plan_from_service）。
func planFromService(payload map[string]any) (map[string]any, error) {
	if payload == nil {
		return nil, errors.New("计划信封缺失")
	}
	revision, _ := tools.AsMap(payload["current_revision"])
	source, _ := tools.AsMap(revision["source_snapshot"])
	items := itemListOf(source["items"])
	packaging, _ := tools.AsMap(revision["packaging"])
	timeline := itemListOf(revision["timeline"])

	sourceByID := map[string]map[string]any{}
	for _, raw := range items {
		item, ok := tools.AsMap(raw)
		if !ok {
			continue
		}
		if id, ok := tools.AsString(item["unit_id"]); ok {
			sourceByID[id] = item
		}
	}

	timelineItems := make([]any, 0, len(timeline))
	for index, raw := range timeline {
		item, ok := tools.AsMap(raw)
		if !ok {
			continue
		}
		unitIDValue := item["source_unit_id"]
		if !tools.Truthy(unitIDValue) {
			unitIDValue = item["id"]
		}
		unitID := fmt.Sprintf("%v", unitIDValue)
		sourceItem := sourceByID[unitID]
		sourceInfo, _ := tools.AsMap(sourceItem["source"])
		transition, _ := tools.AsMap(item["transition"])
		transitionName := "cut"
		if transition != nil {
			if name, ok := tools.AsString(transition["type"]); ok {
				transitionName = name
			}
		}
		timelineItems = append(timelineItems, map[string]any{
			"order":              index + 1,
			"unit_id":            unitIDValue,
			"source":             deepCopyJSON(sourceInfo),
			"trim":               map[string]any{"start_seconds": mapValueOr(item, "trim_start_seconds", 0.0), "end_seconds": nil},
			"speed":              timelineCreatePlanDefaultSpeed,
			"transition_to_next": transitionName,
			"audio_policy":       mapValueOr(item, "audio_policy", timelineAudioDefaultPolicy),
		})
	}

	outputProfile, _ := tools.AsMap(revision["output_profile"])
	episode := mapValueOr(payload, "episode_number", source["episode"])
	fingerprint := source["manifest_fingerprint"]
	if !tools.Truthy(fingerprint) {
		fingerprint = revision["source_fingerprint"]
	}
	review := packaging["director_review"]
	if review == nil {
		review = map[string]any{"status": "pending", "required": true}
	}
	return map[string]any{
		"contract_version":     episodeEditingContractVersion,
		"persistence":          timelinePersistenceDatabase,
		"plan_id":              payload["id"],
		"revision":             payload["current_revision_number"],
		"status":               payload["status"],
		"script":               source["script"],
		"episode":              episode,
		"content_mode":         source["content_mode"],
		"generation_mode":      source["generation_mode"],
		"manifest_fingerprint": fingerprint,
		"director_review":      deepCopyJSON(review),
		"timeline_items":       timelineItems,
		"audio":                deepCopyJSON(mapValueOr(revision, "audio", map[string]any{})),
		"subtitles":            deepCopyJSON(mapValueOr(revision, "subtitle", map[string]any{})),
		"intro":                deepCopyJSON(mapValueOr(packaging, "intro", map[string]any{})),
		"outro":                deepCopyJSON(mapValueOr(packaging, "outro", map[string]any{})),
		"cover":                deepCopyJSON(mapValueOr(packaging, "cover", map[string]any{})),
		"render_profile":       deepCopyJSON(outputProfile),
		"validation":           deepCopyJSON(revision["validation"]),
		"source_snapshot":      source,
	}, nil
}

// mustPlanFromService 把服务信封还原为计划，失败时回退为空计划（仅在已成功写入后调用）。
func mustPlanFromService(name string, payload map[string]any) map[string]any {
	plan, err := planFromService(payload)
	if err != nil {
		return map[string]any{"plan_id": payload["id"], "revision": payload["current_revision_number"], "error": err.Error()}
	}
	return plan
}

// payloadRevisionNumber 取服务信封里的 current_revision_number，作为乐观并发期望值。
func payloadRevisionNumber(payload map[string]any) *int64 {
	if n, ok := integerOf(payload["current_revision_number"]); ok {
		return &n
	}
	return nil
}

// --- 小工具 ---

// expectOK 渲染成功回包：message 一行 + 缩进 JSON 的结构化内容。
func expectOK(name, message string, payload any) (string, error) {
	encoded, err := tools.EncodeReviewJSON(payload)
	if err != nil {
		return tools.DenyEnvelope(toolFailure(name, err, nil)), nil
	}
	return tools.OKEnvelope(message + "\n" + encoded), nil
}

// toolFailure 渲染工具失败文案：`{name} 失败: {err}`，附加可选的行级细节。
func toolFailure(name string, err error, log []string) string {
	message := fmt.Sprintf("%s 失败: %s", name, err.Error())
	if len(log) == 0 {
		return message
	}
	return message + "\n" + strings.Join(log, "\n")
}

// requirePlanID 校验工具入参里的 plan_id。
func requirePlanID(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("plan_id 必须是非空字符串")
	}
	return trimmed, nil
}

// itemListOf 把 JSON 数组（[]any 或 []map[string]any）归一为 []any。
func itemListOf(v any) []any {
	switch list := v.(type) {
	case []any:
		return list
	case []map[string]any:
		out := make([]any, len(list))
		for i, item := range list {
			out[i] = item
		}
		return out
	}
	return nil
}

func isListValue(v any) bool {
	switch v.(type) {
	case []any, []map[string]any:
		return true
	}
	return false
}

// boxSlice 把 nil 切片归一为 nil 接口：避免「缺席」被装箱成非 nil 接口后误判为「传了空数组」。
func boxSlice(v []any) any {
	if v == nil {
		return nil
	}
	return v
}

// boxMap 同 boxSlice：nil 映射归一为 nil 接口。
func boxMap(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}

func mapValueOr(m map[string]any, key string, fallback any) any {
	if v, present := m[key]; present {
		return v
	}
	return fallback
}

func setDefaultKey(m map[string]any, key string, value any) {
	if _, present := m[key]; !present {
		m[key] = value
	}
}

// integerOf 取整数值（Go 的 int 系列或取整浮点）。
func integerOf(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case float64:
		if n == math.Trunc(n) && !math.IsInf(n, 0) {
			return int64(n), true
		}
	}
	return 0, false
}

// firstDurationNumber 取第一个正数时长，都没有时回退到兜底常量。
func firstDurationNumber(values ...any) float64 {
	for _, v := range values {
		if f, ok := tools.AsNumber(v); ok && f > 0 {
			return f
		}
	}
	return timelineFallbackDurationSeconds
}

func containsString(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasIssueCode(issues []map[string]any, code string) bool {
	for _, issue := range issues {
		if c, _ := tools.AsString(issue["code"]); c == code {
			return true
		}
	}
	return false
}

// deepCopyJSON 深拷贝任意 JSON 值（对应 Python copy.deepcopy）。
func deepCopyJSON(v any) any {
	if v == nil {
		return nil
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return v
	}
	return out
}

// deepCopyMap 深拷贝一个对象。
func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyJSON(v)
	}
	return out
}

// nilIfBlank 空串归一为 nil（JSON null），非空原样返回。
func nilIfBlank(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// pyStrList 按 Python repr 的形态渲染字符串列表：['a', 'b']。
func pyStrList(values []string) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, "'"+strings.ReplaceAll(v, "'", "\\'")+"'")
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
