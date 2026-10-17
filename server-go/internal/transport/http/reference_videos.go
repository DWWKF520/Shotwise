package http

// 参考生视频 unit 路由（对应 server/routers/reference_videos.py）。
//
// Mount prefix: /api/v1/projects/:name/reference-videos
//
// 读时派生口径：ad 项目的 unit 是 shots 的派生索引（reference_units，stale 读时注入），
// narration/drama 的 unit 内容自包含（video_units）。写路径一律经
// ProjectStore.UpdateEpisodeScript（脚本锁 → 项目锁，锁内复核 episode→script 绑定）。

import (
	"context"
	"errors"
	"net/http"

	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/i18n"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
)

// ReferenceVideoResolver 是参考生视频 handler 的解析依赖（*config.Resolver 满足）：
// 能力视图（时长档位 / 预览声音档 / 音频开关判定）+ 视频后端解析闸。
type ReferenceVideoResolver interface {
	service.VideoCapabilityResolver
	service.VideoBackendResolver
}

// ReferenceVideoDeps 是参考生视频 handler 的依赖。
type ReferenceVideoDeps struct {
	Store    *service.ProjectStore
	Resolver ReferenceVideoResolver
	Queue    ReferenceVideoQueue
	Hub      *service.EventHub
}

// ReferenceVideoQueue 是参考生视频 handler 的入队能力（*queue.Queue 满足）。
type ReferenceVideoQueue interface {
	EnqueueTask(ctx context.Context, p repository.EnqueueParams) (repository.EnqueueResult, error)
}

// referenceVideoTransitions 是 transition_to_next 的合法枚举。
var referenceVideoTransitions = map[string]bool{"cut": true, "fade": true, "dissolve": true}

// referenceVideoRefTypes 是 references[].type 的合法枚举。
var referenceVideoRefTypes = map[string]bool{"character": true, "scene": true, "prop": true}

// RegisterReferenceVideos 注册参考生视频 unit 路由。
//
// 路由树注意：/units/reorder（静态）与 /units/:unit_id（参数）同层，gin 按静态优先匹配；
// /units/:unit_id/duration-precheck、/units/:unit_id/generate、/units/:unit_id/upload-video
// 与 /units/:unit_id 段数不同，不冲突。
func RegisterReferenceVideos(group *gin.RouterGroup, d ReferenceVideoDeps) {
	base := "/projects/:name/reference-videos/episodes/:episode"
	group.GET(base+"/units", listReferenceUnits(d))
	group.POST(base+"/derive-units", deriveReferenceUnits(d))
	group.POST(base+"/units", addReferenceUnit(d))
	group.POST(base+"/units/reorder", reorderReferenceUnits(d))
	group.PATCH(base+"/units/:unit_id", patchReferenceUnit(d))
	group.DELETE(base+"/units/:unit_id", deleteReferenceUnit(d))
	group.GET(base+"/units/:unit_id/duration-precheck", precheckReferenceUnit(d))
	group.POST(base+"/script-preview", previewReferenceScript(d))
	group.POST(base+"/units/:unit_id/generate", generateReferenceUnit(d))
	group.POST(base+"/units/:unit_id/upload-video", uploadReferenceUnitVideo(d))
}

// episodeParam 解析 :episode；非法时写出 FastAPI 风格的 422。
func episodeParam(c *gin.Context) (int, bool) {
	raw := c.Param("episode")
	value, err := strconv.Atoi(raw)
	if err != nil {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"path", "episode"}, Msg: "value is not a valid integer", Type: "int_parsing"},
		}})
		return 0, false
	}
	return value, true
}

// requireExistingProject 项目不存在时写出 404 并返回 false。
func requireExistingProject(c *gin.Context, store *service.ProjectStore) bool {
	if !store.Exists(c.Param("name")) {
		apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "project_not_found"))
		return false
	}
	return true
}

// decodeReferences 解析 references 字段：缺失返回空切片，格式非法写出 422。
func decodeReferences(c *gin.Context, raw any) ([]domain.AdReference, bool) {
	if raw == nil {
		return []domain.AdReference{}, true
	}
	items, ok := raw.([]any)
	if !ok {
		abortFieldConstraint(c, "references", "list_type")
		return nil, false
	}
	refs := make([]domain.AdReference, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			abortFieldConstraint(c, "references", "model_attributes_type")
			return nil, false
		}
		refType, _ := entry["type"].(string)
		if !referenceVideoRefTypes[refType] {
			abortFieldConstraint(c, "references.type", "string_pattern_mismatch")
			return nil, false
		}
		name, ok := entry["name"].(string)
		if !ok {
			abortFieldConstraint(c, "references.name", "string_type")
			return nil, false
		}
		refs = append(refs, domain.AdReference{Type: refType, Name: domain.NormalizeAssetName(name)})
	}
	return refs, true
}

// decodeOptionalDuration 解析可选的 duration_seconds；缺失 / null 返回 nil，非法写出 422。
func decodeOptionalDuration(c *gin.Context, raw any, present bool) (*int, bool) {
	if !present || raw == nil {
		return nil, true
	}
	value, ok := jsonInt(raw)
	if !ok || value < 1 {
		abortFieldConstraint(c, "duration_seconds", "greater_than_equal")
		return nil, false
	}
	return &value, true
}

// decodeOptionalTransition 解析可选的 transition_to_next；缺失 / null 返回 nil，非法写出 422。
func decodeOptionalTransition(c *gin.Context, raw any, present bool) (*string, bool) {
	if !present || raw == nil {
		return nil, true
	}
	value, ok := raw.(string)
	if !ok || !referenceVideoTransitions[value] {
		abortFieldConstraint(c, "transition_to_next", "string_pattern_mismatch")
		return nil, false
	}
	return &value, true
}

// jsonInt 把 JSON 数字解析为整数（非整数 / 布尔不算）。
func jsonInt(raw any) (int, bool) {
	switch v := raw.(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, false
		}
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

func listReferenceUnits(d ReferenceVideoDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		project := c.Param("name")
		if !requireExistingProject(c, d.Store) {
			return
		}
		pf, script, _, err := d.Store.LoadEpisodeScript(project, episode)
		if err != nil {
			abortReferenceVideoErr(c, err)
			return
		}
		if pf.ContentMode == string(domain.ContentAd) {
			c.JSON(http.StatusOK, gin.H{"units": service.AnnotateAdUnitStaleness(script, script["reference_units"])})
			return
		}
		units, _ := script["video_units"].([]any)
		if units == nil {
			units = []any{}
		}
		c.JSON(http.StatusOK, gin.H{"units": units})
	}
}

func deriveReferenceUnits(d ReferenceVideoDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		project := c.Param("name")
		if !requireExistingProject(c, d.Store) {
			return
		}
		pf, _, _, err := d.Store.LoadEpisodeScript(project, episode)
		if err != nil {
			abortReferenceVideoErr(c, err)
			return
		}
		if pf.ContentMode != string(domain.ContentAd) {
			abortReferenceVideoErr(c, &service.AdOnlyDeriveError{})
			return
		}
		maxUnitDuration := service.ResolveMaxUnitDuration(c.Request.Context(), d.Resolver, pf)

		requireAd := true
		var annotated []any
		err = d.Store.UpdateEpisodeScript(project, service.EpisodeScriptResolverFor(episode, &requireAd, nil),
			func(script map[string]any) error {
				units := domain.SyncAdReferenceUnits(script, episode, maxUnitDuration)
				annotated = service.AnnotateAdUnitStaleness(script, service.ToAnyList(units))
				return nil
			})
		if err != nil {
			abortReferenceVideoErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"units": annotated})
	}
}

func addReferenceUnit(d ReferenceVideoDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		project := c.Param("name")
		if !requireExistingProject(c, d.Store) {
			return
		}
		body, ok := decodeBody(c)
		if !ok {
			return
		}
		prompt, ok := requiredString(c, body, "prompt")
		if !ok {
			return
		}
		refs, ok := decodeReferences(c, body["references"])
		if !ok {
			return
		}
		rawDuration, hasDuration := body["duration_seconds"]
		duration, ok := decodeOptionalDuration(c, rawDuration, hasDuration)
		if !ok {
			return
		}
		rawTransition, hasTransition := body["transition_to_next"]
		transition, ok := decodeOptionalTransition(c, rawTransition, hasTransition)
		if !ok {
			return
		}
		note := optionalBodyString(body, "note")

		// 时长未给出时按项目能力解析默认档位（异步 IO / 能力解析不进项目锁）。
		if duration == nil {
			pf, _, _, err := d.Store.LoadEpisodeScript(project, episode)
			if err != nil {
				abortReferenceVideoErr(c, err)
				return
			}
			bucket := provider.ReferenceVideoBucket(len(refs) > 0)
			dctx := service.ResolveProjectDurationContext(c.Request.Context(), d.Resolver, pf, bucket)
			resolved := service.DefaultUnitDuration(dctx, pf, len(refs) > 0)
			duration = &resolved
		}
		finalDuration := *duration
		transitionValue := "cut"
		if transition != nil {
			transitionValue = *transition
		}

		requireAd := false
		var unit map[string]any
		err := d.Store.UpdateEpisodeScript(project, service.EpisodeScriptResolverFor(episode, &requireAd, refs),
			func(script map[string]any) error {
				unit = service.BuildReferenceUnit(
					service.NextUnitID(script, episode), prompt, refs, finalDuration, transitionValue, note)
				units, _ := script["video_units"].([]any)
				script["video_units"] = append(units, unit)
				return nil
			})
		if err != nil {
			abortReferenceVideoErr(c, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{"unit": unit})
	}
}

func patchReferenceUnit(d ReferenceVideoDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		project := c.Param("name")
		if !requireExistingProject(c, d.Store) {
			return
		}
		body, ok := decodeBody(c)
		if !ok {
			return
		}
		unitID := c.Param("unit_id")

		refs, refsPresent := body["references"]
		var resolvedRefs []domain.AdReference
		if refsPresent && refs != nil {
			var valid bool
			resolvedRefs, valid = decodeReferences(c, refs)
			if !valid {
				return
			}
		}
		rawDuration, hasDuration := body["duration_seconds"]
		duration, ok := decodeOptionalDuration(c, rawDuration, hasDuration)
		if !ok {
			return
		}
		rawTransition, hasTransition := body["transition_to_next"]
		transition, ok := decodeOptionalTransition(c, rawTransition, hasTransition)
		if !ok {
			return
		}
		prompt, promptPresent := body["prompt"]
		note := optionalBodyString(body, "note")
		var promptText *string
		if promptPresent && prompt != nil {
			text, isString := prompt.(string)
			if !isString {
				abortFieldConstraint(c, "prompt", "string_type")
				return
			}
			promptText = &text
		}

		requireAd := false
		var resolverRefs []domain.AdReference
		if refsPresent && refs != nil {
			resolverRefs = resolvedRefs
		}
		var unit map[string]any
		err := d.Store.UpdateEpisodeScript(project, service.EpisodeScriptResolverFor(episode, &requireAd, resolverRefs),
			func(script map[string]any) error {
				target, findErr := service.FindUnit(script, unitID)
				if findErr != nil {
					return findErr
				}
				if resolverRefs != nil {
					refList := make([]any, 0, len(resolvedRefs))
					for _, ref := range resolvedRefs {
						refList = append(refList, map[string]any{"type": ref.Type, "name": ref.Name})
					}
					target["references"] = refList
				}
				if promptText != nil {
					shots, _ := service.ParsePrompt(*promptText)
					shotsAny := make([]any, 0, len(shots))
					for _, shot := range shots {
						shotsAny = append(shotsAny, map[string]any{"text": shot["text"]})
					}
					target["shots"] = shotsAny
				}
				if duration != nil {
					target["duration_seconds"] = *duration
				}
				if transition != nil {
					target["transition_to_next"] = *transition
				}
				if note != nil {
					target["note"] = *note
				}
				unit = target
				return nil
			})
		if err != nil {
			abortReferenceVideoErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"unit": unit})
	}
}

func deleteReferenceUnit(d ReferenceVideoDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		project := c.Param("name")
		if !requireExistingProject(c, d.Store) {
			return
		}
		unitID := c.Param("unit_id")

		requireAd := false
		err := d.Store.UpdateEpisodeScript(project, service.EpisodeScriptResolverFor(episode, &requireAd, nil),
			func(script map[string]any) error {
				units, _ := script["video_units"].([]any)
				kept := make([]any, 0, len(units))
				removed := false
				for _, raw := range units {
					unit, isMap := raw.(map[string]any)
					if isMap {
						if id, _ := unit["unit_id"].(string); id == unitID {
							removed = true
							continue
						}
					}
					kept = append(kept, raw)
				}
				if !removed {
					return &service.ReferenceUnitNotFoundError{UnitID: unitID}
				}
				script["video_units"] = kept
				return nil
			})
		if err != nil {
			abortReferenceVideoErr(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func reorderReferenceUnits(d ReferenceVideoDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		project := c.Param("name")
		if !requireExistingProject(c, d.Store) {
			return
		}
		body, ok := decodeBody(c)
		if !ok {
			return
		}
		requested, ok := decodeUnitIDs(c, body["unit_ids"])
		if !ok {
			return
		}

		requireAd := false
		var reordered []any
		err := d.Store.UpdateEpisodeScript(project, service.EpisodeScriptResolverFor(episode, &requireAd, nil),
			func(script map[string]any) error {
				units, _ := script["video_units"].([]any)
				if kind := service.FullPermutationError(service.UnitIDs(units), requested); kind != "" {
					return &service.ReorderValidationError{Kind: kind}
				}
				byID := map[string]any{}
				for _, raw := range units {
					if unit, isMap := raw.(map[string]any); isMap {
						if id, _ := unit["unit_id"].(string); id != "" {
							byID[id] = unit
						}
					}
				}
				reordered = make([]any, 0, len(requested))
				for _, id := range requested {
					reordered = append(reordered, byID[id])
				}
				script["video_units"] = reordered
				return nil
			})
		if err != nil {
			abortReferenceVideoErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"units": reordered})
	}
}

// decodeUnitIDs 解析 reorder 的 unit_ids（必填字符串数组）。
func decodeUnitIDs(c *gin.Context, raw any) ([]string, bool) {
	items, ok := raw.([]any)
	if !ok {
		abortFieldConstraint(c, "unit_ids", "list_type")
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		value, isString := item.(string)
		if !isString {
			abortFieldConstraint(c, "unit_ids", "string_type")
			return nil, false
		}
		out = append(out, value)
	}
	return out, true
}

func precheckReferenceUnit(d ReferenceVideoDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		project := c.Param("name")
		if !requireExistingProject(c, d.Store) {
			return
		}
		unitID := c.Param("unit_id")

		pf, script, _, err := d.Store.LoadEpisodeScript(project, episode)
		if err != nil {
			abortReferenceVideoErr(c, err)
			return
		}

		var unit map[string]any
		var adShots []map[string]any
		if pf.ContentMode == string(domain.ContentAd) {
			unit, err = service.FindAdUnit(script, unitID)
			if err != nil {
				abortReferenceVideoErr(c, err)
				return
			}
			adShots, err = domain.ResolveAdUnitShots(script, unit)
			if err != nil {
				apperr.Abort(c, apperr.NewText(http.StatusConflict, "ref_ad_stale_index", nil))
				return
			}
		} else {
			unit, err = service.FindUnit(script, unitID)
			if err != nil {
				abortReferenceVideoErr(c, err)
				return
			}
		}

		bucket := provider.VideoBucket(domain.ReferenceUnitVideoBucket(unit, adShots))
		dctx := service.ResolveProjectDurationContext(c.Request.Context(), d.Resolver, pf, bucket)
		slot, slotErr := service.PrecheckUnit(dctx, unit, adShots)
		if slotErr != nil {
			apperr.Abort(c, apperr.Internal(slotErr))
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"needs_confirmation": slot.NeedsConfirmation(),
			"script_duration":    slot.TotalSeconds,
			"request_duration":   slot.Seconds,
			"adjustment":         slot.Adjustment,
		})
	}
}

// previewReferenceScript 是分镜文稿的读时派生预览：shots / references / utterances +
// 降级可见性 warning（只读、不落盘——文稿是唯一真相，其余都是机械派生物）。
func previewReferenceScript(d ReferenceVideoDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		project := c.Param("name")
		if !requireExistingProject(c, d.Store) {
			return
		}
		body, ok := decodeBody(c)
		if !ok {
			return
		}
		prompt := ""
		if raw, present := body["prompt"]; present && raw != nil {
			text, isString := raw.(string)
			if !isString {
				abortFieldConstraint(c, "prompt", "string_type")
				return
			}
			prompt = text
		}

		pf, _, _, err := d.Store.LoadEpisodeScript(project, episode)
		if err != nil {
			abortReferenceVideoErr(c, err)
			return
		}
		// 声音相关 warning 依赖该集视频后端的能力，与执行层同一份解析出口；
		// 能力解析失败按 soft 降级，只是少发这几条提示。
		bucket := provider.VideoBucketForGenerationMode(pf.GenerationMode)
		caps := service.ResolveProjectVideoCaps(c.Request.Context(), d.Resolver, pf, bucket)
		var maxRefImages *int
		if caps.Resolved && caps.View.Caps.MaxReferenceImages > 0 {
			limit := caps.View.Caps.MaxReferenceImages
			maxRefImages = &limit
		}
		preview := service.BuildScriptPreview(prompt, pf, service.VoiceRenderSettingsFromCaps(caps, pf), maxRefImages)

		loc := i18n.FromRequest(c.Request)
		shots := make([]gin.H, 0, len(preview.Shots))
		for index, shot := range preview.Shots {
			text, _ := shot["text"].(string)
			shots = append(shots, gin.H{"index": index + 1, "text": text})
		}
		utterances := make([]gin.H, 0, len(preview.Utterances))
		for _, u := range preview.Utterances {
			utterances = append(utterances, gin.H{
				"shot_index": u.ShotIndex, "kind": u.Kind, "speaker": u.Speaker, "text": u.Text,
			})
		}
		warnings := make([]gin.H, 0, len(preview.Warnings))
		for _, w := range preview.Warnings {
			key, _ := w["key"].(string)
			params, _ := w["params"].(map[string]any)
			warnings = append(warnings, gin.H{"key": key, "message": i18n.T(loc, key, params)})
		}
		c.JSON(http.StatusOK, gin.H{
			"shots":      shots,
			"references": preview.References,
			"utterances": utterances,
			"warnings":   warnings,
		})
	}
}

// generateReferenceUnit 把 unit 的成片生成任务入队（202）。
func generateReferenceUnit(d ReferenceVideoDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		project := c.Param("name")
		if !requireExistingProject(c, d.Store) {
			return
		}
		unitID := c.Param("unit_id")

		pf, script, scriptFile, err := d.Store.LoadEpisodeScript(project, episode)
		if err != nil {
			abortReferenceVideoErr(c, err)
			return
		}
		isAd := pf.ContentMode == string(domain.ContentAd)
		var unit map[string]any
		var unitShots []map[string]any
		var guardPrompt string
		if isAd {
			// 按持久化索引水合成员镜头（重生成单个 unit 不重新派生，分组可复现）；
			// 索引悬空（镜头被删后未重新派生）→ 409 提示重新派生
			unit, err = service.FindAdUnit(script, unitID)
			if err != nil {
				abortReferenceVideoErr(c, err)
				return
			}
			unitShots, err = domain.ResolveAdUnitShots(script, unit)
			if err != nil {
				apperr.Abort(c, apperr.NewText(http.StatusConflict, "ref_ad_stale_index", nil))
				return
			}
			var style *string
			if pf.Style != "" {
				styleValue := pf.Style
				style = &styleValue
			}
			guardPrompt = domain.RenderAdUnitPrompt(unitShots, style)
		} else {
			unit, err = service.FindUnit(script, unitID)
			if err != nil {
				abortReferenceVideoErr(c, err)
				return
			}
			guardPrompt = domain.AssembleShotsText(unit["shots"])
		}

		projectDir, err := d.Store.Dir(project)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		// ad 参考集由镜头字段继承而不是人工挑选，缺图在执行层按软口径跳过并记 warning，
		// 提交期只硬拦未登记资产；narration/drama 按 unit 声明的 references 全量校验。
		var refs []domain.AdReference
		if isAd {
			refs = domain.AdUnitReferences(unitShots)
		} else {
			refs = unitTypedReferences(unit)
		}
		preflight := service.ValidateTypedReferences(pf, projectDir, refs, !isAd)
		if len(preflight.Issues) > 0 {
			var unregistered, missingImages []string
			for _, issue := range preflight.Issues {
				label := string(issue.AssetType) + ":" + issue.AssetName
				if issue.Kind == service.PreflightKindUnregistered {
					unregistered = append(unregistered, label)
				} else {
					missingImages = append(missingImages, label)
				}
			}
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "generation_asset_preflight_failed", map[string]any{
				"unregistered":     joinOrDash(unregistered),
				"missing_images":   joinOrDash(missingImages),
				"missing_variants": "—",
			}))
			return
		}

		// 空提示词的结构校验在提交入口当场拒绝，与 SDK 入队路径同一守卫口径。
		if strings.TrimSpace(guardPrompt) == "" {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "prompt_text_empty", nil))
			return
		}

		// 参考生视频按镜头是否携带参考图分流定桶（docs/adr/0054）：有参考图 → r2v，
		// 无参考图的退化镜头降级 → i2v。解析闸让能力缺失 / 悬空引用在提交入口即返回
		// 修复指引，而非任务面板里的异步失败。
		bucket := provider.VideoBucket(domain.ReferenceUnitVideoBucket(unit, unitShots))
		if err := service.RequireVideoBucketCapability(c.Request.Context(), d.Resolver, pf, bucket); err != nil {
			abortReferenceVideoErr(c, err)
			return
		}
		if err := service.AssertAudioSwitchSupported(c.Request.Context(), d.Resolver, pf, bucket); err != nil {
			abortReferenceVideoErr(c, err)
			return
		}

		userID := ""
		if user := auth.Current(c); user != nil {
			userID = user.ID
		}
		result, err := d.Queue.EnqueueTask(c.Request.Context(), repository.EnqueueParams{
			ProjectName: project,
			TaskType:    "reference_video",
			MediaType:   "video",
			ResourceID:  unitID,
			Payload:     map[string]any{"prompt": guardPrompt, "script_file": scriptFile},
			ScriptFile:  &scriptFile,
			Source:      "webui",
			UserID:      userID,
		})
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"task_id": result.TaskID, "deduped": result.Deduped})
	}
}

// uploadReferenceUnitVideo 上传 unit 成片，复用生成链路的 finalize 与版本管理。
func uploadReferenceUnitVideo(d ReferenceVideoDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		project := c.Param("name")
		if !requireExistingProject(c, d.Store) {
			return
		}
		unitID := c.Param("unit_id")

		header, err := c.FormFile("file")
		if err != nil {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"body", "file"}, Msg: "field required", Type: "value_error.missing"},
			}})
			return
		}
		maxBytes, err := service.ValidateUploadVideo(header.Filename, &header.Size)
		if err != nil {
			abortReferenceVideoErr(c, err)
			return
		}
		relativePath, err := service.ReferenceVideoRelativePath(unitID)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}

		pf, script, _, err := d.Store.LoadEpisodeScript(project, episode)
		if err != nil {
			abortReferenceVideoErr(c, err)
			return
		}
		if _, err = service.FindUnitForProject(pf, script, unitID); err != nil {
			abortReferenceVideoErr(c, err)
			return
		}
		projectDir, err := d.Store.Dir(project)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		// 路径遍历防护：unit_id 拼出的绝对路径不得逃出项目目录（与 versions 路由对齐）
		target, err := fs.SafeJoin(projectDir, false, relativePath)
		if err != nil {
			apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "invalid_resource_id",
				map[string]any{"resource_id": unitID}))
			return
		}

		versions := service.NewVersions(projectDir)
		if _, err = versions.EnsureCurrentTracked("reference_videos", unitID, "", target, nil); err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		file, err := header.Open()
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		err = service.SaveUploadedVideoStream(file, target, maxBytes)
		file.Close()
		if err != nil {
			abortReferenceVideoErr(c, err)
			return
		}

		// 上传流可达数百 MB、耗时数秒，期间 episode→script 绑定可能被并发重绑
		// （PATCH / agent 同步剧本）。落盘后重解析绑定，确保元数据写进当前生效的剧本；
		// unit 被并发删除时在此 404（落盘后绑定重查到锁内写回之间的窄竞态留给 finalize）。
		pf2, script2, scriptFile2, err := d.Store.LoadEpisodeScript(project, episode)
		if err != nil {
			abortReferenceVideoErr(c, err)
			return
		}
		if _, err = service.FindUnitForProject(pf2, script2, unitID); err != nil {
			abortReferenceVideoErr(c, err)
			return
		}

		version, err := service.RecordUploadVersion(versions, unitID, target, header.Filename)
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		if err = service.FinalizeUploadedReferenceVideoUnit(
			d.Store, project, scriptFile2, projectDir, unitID, target, version, versions,
		); err != nil {
			abortReferenceVideoErr(c, err)
			return
		}
		// 上传完成与 AI 生成同源广播项目变更事件；返回的指纹直接复用进响应体。
		fingerprints := service.EmitGenerationSuccessBatch(d.Hub, d.Store, &repository.TaskData{
			ProjectName: project,
			TaskType:    "reference_video",
			ResourceID:  unitID,
			Payload:     map[string]any{"script_file": scriptFile2},
		})
		c.JSON(http.StatusOK, gin.H{
			"success":            true,
			"path":               relativePath,
			"version":            version,
			"asset_fingerprints": fingerprints,
		})
	}
}

// unitTypedReferences 从 unit.references 收集类型化引用对：非对象条目与非字符串字段
// 跳过（空字符串仍是合法 str，与 Python 的 isinstance 收集口径一致）。
func unitTypedReferences(unit map[string]any) []domain.AdReference {
	list, _ := unit["references"].([]any)
	refs := make([]domain.AdReference, 0, len(list))
	for _, raw := range list {
		entry, isMap := raw.(map[string]any)
		if !isMap {
			continue
		}
		refType, typeOK := entry["type"].(string)
		refName, nameOK := entry["name"].(string)
		if typeOK && nameOK {
			refs = append(refs, domain.AdReference{Type: refType, Name: refName})
		}
	}
	return refs
}

// joinOrDash 用 ", " 连接标签；空列表返回占位符 "—"（对齐 Python ", ".join(...) or "—"）。
func joinOrDash(labels []string) string {
	if len(labels) == 0 {
		return "—"
	}
	return strings.Join(labels, ", ")
}

// reorderDetailKeys 把 reorder 校验错误种类映射到 i18n key。
var reorderDetailKeys = map[string]string{
	"length":    "ref_unit_ids_length_mismatch",
	"duplicate": "ref_duplicate_unit_ids",
	"mismatch":  "ref_unit_ids_mismatch",
}

// abortReferenceVideoErr 把参考生视频领域错误映射成统一 {"detail": ...} 信封。
func abortReferenceVideoErr(c *gin.Context, err error) {
	var (
		episodeMissing *service.EpisodeNotFoundError
		notRefMode     *service.NotReferenceVideoModeError
		scriptMissing  *service.EpisodeScriptMissingError
		unregistered   *service.UnregisteredReferencesError
		adOnly         *service.AdOnlyDeriveError
		adDerived      *service.AdUnitsDerivedError
		unitMissing    *service.ReferenceUnitNotFoundError
		reorderErr     *service.ReorderValidationError
		structErr      *service.ScriptStructureValidationError
		uploadErr      *service.UploadValidationError
		bucketCapErr   *config.VideoBucketCapabilityError
		audioSwitchErr *service.AudioSwitchConflictError
	)
	switch {
	case errors.As(err, &episodeMissing):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "ref_episode_not_found",
			map[string]any{"episode": episodeMissing.Episode}))
	case errors.As(err, &notRefMode):
		apperr.Abort(c, apperr.NewText(http.StatusConflict, "ref_not_reference_video_mode", nil))
	case errors.As(err, &scriptMissing):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "ref_script_missing", nil))
	case errors.As(err, &unregistered):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "ref_not_registered",
			map[string]any{"missing": strings.Join(unregistered.Missing, ", ")}))
	case errors.As(err, &adOnly):
		apperr.Abort(c, apperr.NewText(http.StatusConflict, "ref_derive_ad_only", nil))
	case errors.As(err, &adDerived):
		apperr.Abort(c, apperr.NewText(http.StatusConflict, "ref_ad_units_derived", nil))
	case errors.As(err, &unitMissing):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "ref_unit_not_found",
			map[string]any{"unit_id": unitMissing.UnitID}))
	case errors.As(err, &reorderErr):
		key := reorderDetailKeys[reorderErr.Kind]
		if key == "" {
			key = "ref_unit_ids_mismatch"
		}
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, key, nil))
	case errors.As(err, &uploadErr):
		apperr.Abort(c, apperr.NewText(uploadErr.Status, uploadErr.Key, uploadErr.Params))
	case errors.As(err, &bucketCapErr):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, string(bucketCapErr.Code), map[string]any{
			"provider": bucketCapErr.ProviderID, "model": bucketCapErr.ModelID,
		}))
	case errors.As(err, &audioSwitchErr):
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, "video_audio_switch_not_supported", map[string]any{
			"provider": audioSwitchErr.ProviderID, "model": audioSwitchErr.ModelID,
		}))
	case errors.Is(err, service.ErrEpisodeScriptRebound):
		apperr.Abort(c, apperr.NewText(http.StatusConflict, "ref_script_rebound", nil))
	case errors.As(err, &structErr):
		apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "script_validation_failed",
			map[string]any{"details": structErr.Error()}))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}