package media

// 媒体装配计划的确定性契约（对应 lib/media_assembly/plan.py 与 source_snapshot.py）。
//
// 纯函数：只校验文档结构与生命周期转移、构造规范化 source snapshot、计算内容指纹，
// 不读文件、不调用媒体工具。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// PlanStatuses 是装配计划的生命周期状态集合。
var PlanStatuses = map[string]bool{
	"draft": true, "confirmed": true, "preview_pending": true, "preview_ready": true,
	"render_pending": true, "rendering": true, "completed": true, "stale": true, "failed": true,
}

// runningPlanStatuses 是「进行中」状态：此时不得写入新 revision（对应 Python is_running_status）。
var runningPlanStatuses = map[string]bool{
	"preview_pending": true, "render_pending": true, "rendering": true,
}

// SupportedTimelineTransitions 是可声明的时间线转场类型。
var SupportedTimelineTransitions = map[string]bool{"cut": true, "fade": true, "dissolve": true, "wipe": true}

// RenderableTimelineTransitions 是实际渲染出重叠的转场（cut 之外）。
var RenderableTimelineTransitions = map[string]bool{"fade": true, "dissolve": true, "wipe": true}

// allowedPlanTransitions 是生命周期转移白名单。
var allowedPlanTransitions = map[string]map[string]bool{
	"draft":           {"confirmed": true, "stale": true},
	"confirmed":       {"draft": true, "preview_pending": true, "stale": true},
	"preview_pending": {"preview_ready": true, "failed": true, "stale": true},
	"preview_ready":   {"confirmed": true, "render_pending": true, "stale": true},
	"render_pending":  {"rendering": true, "confirmed": true, "stale": true},
	"rendering":       {"completed": true, "failed": true, "stale": true},
	"completed":       {"draft": true, "stale": true},
	"stale":           {"draft": true, "confirmed": true},
	"failed":          {"draft": true, "confirmed": true, "stale": true},
}

// AssemblyPlanValidationError 表示装配计划违反确定性契约；Errors 是逐条问题详情。
type AssemblyPlanValidationError struct{ Errors []map[string]any }

func (e *AssemblyPlanValidationError) Error() string { return "assembly plan validation failed" }

// AssemblyPlanTransitionError 表示计划生命周期转移不被允许。
type AssemblyPlanTransitionError struct{ Msg string }

func (e *AssemblyPlanTransitionError) Error() string { return e.Msg }

// AssertTransition 校验 current → target 是否为合法生命周期转移。
func AssertTransition(current, target string) error {
	if !PlanStatuses[current] || !PlanStatuses[target] {
		return &AssemblyPlanTransitionError{Msg: fmt.Sprintf("unknown assembly plan status: %q -> %q", current, target)}
	}
	if !allowedPlanTransitions[current][target] {
		return &AssemblyPlanTransitionError{Msg: fmt.Sprintf("invalid assembly plan transition: %q -> %q", current, target)}
	}
	return nil
}

// IsRunningStatus 报告状态是否属于「进行中」集合。
func IsRunningStatus(status string) bool { return runningPlanStatuses[status] }

// PackagingSectionEnabled 报告 packaging 分节是否参与渲染。
// 分节默认启用，只有显式 enabled=false 才停用——校验、时长复核与渲染共用同一条规则。
func PackagingSectionEnabled(section any) bool {
	m, ok := asMapping(section)
	if !ok {
		return false
	}
	if v, present := m["enabled"]; present {
		if b, isBool := v.(bool); isBool && !b {
			return false
		}
	}
	return true
}

// --- 校验辅助 ---

func vErr(path, code, message string) map[string]any {
	return map[string]any{"path": path, "code": code, "message": message}
}

// asMapping 把 v 当作对象；非对象时返回 false（调用方决定是否记错误）。
func asMapping(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// checkedMapping 对应 Python _mapping：非对象时追加 object_required 错误。
func checkedMapping(v any, path string, errs *[]map[string]any) map[string]any {
	m, ok := asMapping(v)
	if !ok {
		*errs = append(*errs, vErr(path, "object_required", "expected an object"))
		return nil
	}
	return m
}

// anyList 把 v 当作序列：仅接受 JSON 数组形态（[]any / []map[string]any）。
func anyList(v any) ([]any, bool) {
	switch list := v.(type) {
	case []any:
		return list, true
	case []map[string]any:
		out := make([]any, len(list))
		for i, item := range list {
			out[i] = item
		}
		return out, true
	}
	return nil, false
}

// nonEmptyString 校验非空（去空白）字符串。
func nonEmptyString(v any, path string, errs *[]map[string]any) bool {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		*errs = append(*errs, vErr(path, "string_required", "expected a non-empty string"))
		return false
	}
	return true
}

// numberValue 对应 Python _non_negative_number 的数值判定：排除布尔，接受数字。
func numberValue(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	}
	return 0, false
}

func nonNegativeNumber(v any, path string, errs *[]map[string]any) bool {
	f, ok := numberValue(v)
	if !ok || f < 0 {
		*errs = append(*errs, vErr(path, "non_negative_number_required", "expected a non-negative number"))
		return false
	}
	return true
}

func positiveNumber(v any, path string, errs *[]map[string]any) bool {
	f, ok := numberValue(v)
	if !ok || f <= 0 {
		*errs = append(*errs, vErr(path, "positive_number_required", "expected a positive number"))
		return false
	}
	return true
}

// integerValue 判定整数值（对应 Python isinstance(int)）：接受整型与取整浮点。
func integerValue(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case int32:
		return int64(x), true
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return int64(x), true
		}
	}
	return 0, false
}

// --- 时间线校验 ---

func validateTimeline(value any, errs *[]map[string]any) {
	items, ok := anyList(value)
	if !ok {
		*errs = append(*errs, vErr("timeline", "list_required", "timeline must be a list"))
		return
	}
	if len(items) == 0 {
		*errs = append(*errs, vErr("timeline", "not_empty", "timeline must contain at least one item"))
		return
	}

	seenIDs := map[string]bool{}
	seenOrders := map[int64]bool{}
	for index, rawItem := range items {
		path := fmt.Sprintf("timeline[%d]", index)
		item := checkedMapping(rawItem, path, errs)
		if item == nil {
			continue
		}
		itemID := item["id"]
		if nonEmptyString(itemID, path+".id", errs) {
			id := itemID.(string)
			if seenIDs[id] {
				*errs = append(*errs, vErr(path+".id", "duplicate", "timeline item ids must be unique"))
			}
			seenIDs[id] = true
		}
		order, ok := integerValue(item["order"])
		switch {
		case !ok || order < 0:
			*errs = append(*errs, vErr(path+".order", "non_negative_integer_required", "order must be a non-negative integer"))
		case seenOrders[order]:
			*errs = append(*errs, vErr(path+".order", "duplicate", "timeline order values must be unique"))
		default:
			seenOrders[order] = true
		}
		nonEmptyString(item["kind"], path+".kind", errs)
		nonEmptyString(item["source_ref"], path+".source_ref", errs)

		duration := item["duration_seconds"]
		if positiveNumber(duration, path+".duration_seconds", errs) {
			dur, _ := numberValue(duration)
			trimStart := valueOr(item, "trim_start_seconds", 0)
			trimEnd := valueOr(item, "trim_end_seconds", 0)
			startOK := nonNegativeNumber(trimStart, path+".trim_start_seconds", errs)
			endOK := nonNegativeNumber(trimEnd, path+".trim_end_seconds", errs)
			if startOK && endOK {
				start, _ := numberValue(trimStart)
				end, _ := numberValue(trimEnd)
				if start+end >= dur {
					*errs = append(*errs, vErr(path, "empty_after_trim", "trimmed duration must remain positive"))
				}
			}
		}
		policy := valueOr(item, "audio_policy", "duck")
		if p, ok := policy.(string); !ok || (p != "keep" && p != "duck" && p != "mute") {
			*errs = append(*errs, vErr(path+".audio_policy", "invalid_audio_policy", "audio_policy must be keep, duck, or mute"))
		}
		if transition, present := item["transition"]; present && transition != nil {
			transitionObj := checkedMapping(transition, path+".transition", errs)
			if transitionObj != nil {
				transitionType := transitionObj["type"]
				if nonEmptyString(transitionType, path+".transition.type", errs) {
					tt := transitionType.(string)
					if !SupportedTimelineTransitions[tt] {
						*errs = append(*errs, vErr(path+".transition.type", "unsupported_transition",
							"transition type must be cut, fade, dissolve, or wipe"))
					}
				}
				transitionDuration := valueOr(transitionObj, "duration_seconds", 0)
				tt, _ := transitionType.(string)
				if tt == "cut" {
					nonNegativeNumber(transitionDuration, path+".transition.duration_seconds", errs)
				} else if positiveNumber(transitionDuration, path+".transition.duration_seconds", errs) {
					td, _ := numberValue(transitionDuration)
					dur, durOK := numberValue(duration)
					if durOK && td >= dur {
						*errs = append(*errs, vErr(path+".transition.duration_seconds", "transition_too_long",
							"transition must be shorter than the item"))
					}
				}
			}
		}
	}

	if len(seenOrders) > 0 {
		complete := int64(len(items))
		var all bool = true
		for i := int64(0); i < complete; i++ {
			if !seenOrders[i] {
				all = false
				break
			}
		}
		if int64(len(seenOrders)) != complete || !all {
			*errs = append(*errs, vErr("timeline", "non_contiguous_order", "timeline order must be contiguous from zero"))
		}
	}
}

// --- 音频校验 ---

func validateAudio(value any, errs *[]map[string]any) {
	audio, ok := asMapping(value)
	if !ok {
		*errs = append(*errs, vErr("audio", "object_required", "audio must be an object"))
		return
	}
	tracksRaw, present := audio["tracks"]
	if !present {
		tracksRaw = []any{}
	}
	tracks, ok := anyList(tracksRaw)
	if !ok {
		*errs = append(*errs, vErr("audio.tracks", "list_required", "audio.tracks must be a list"))
		return
	}
	seenIDs := map[string]bool{}
	for index, rawTrack := range tracks {
		path := fmt.Sprintf("audio.tracks[%d]", index)
		track := checkedMapping(rawTrack, path, errs)
		if track == nil {
			continue
		}
		trackID := track["id"]
		if nonEmptyString(trackID, path+".id", errs) {
			id := trackID.(string)
			if seenIDs[id] {
				*errs = append(*errs, vErr(path+".id", "duplicate", "audio track ids must be unique"))
			}
			seenIDs[id] = true
		}
		nonEmptyString(track["kind"], path+".kind", errs)
		nonEmptyString(track["source_ref"], path+".source_ref", errs)
		nonNegativeNumber(valueOr(track, "start_seconds", 0), path+".start_seconds", errs)
		volume := valueOr(track, "volume", 1.0)
		if f, isNum := numberValue(volume); !isNum || f < 0 {
			*errs = append(*errs, vErr(path+".volume", "invalid_volume", "volume must be a non-negative number"))
		}
		for _, key := range []string{"fade_in_seconds", "fade_out_seconds"} {
			nonNegativeNumber(valueOr(track, key, 0), path+"."+key, errs)
		}
	}
}

// --- 字幕校验 ---

func validateSubtitles(value any, errs *[]map[string]any) {
	subtitle, ok := asMapping(value)
	if !ok {
		*errs = append(*errs, vErr("subtitle", "object_required", "subtitle must be an object"))
		return
	}
	cuesRaw, present := subtitle["cues"]
	if !present {
		cuesRaw = []any{}
	}
	cues, ok := anyList(cuesRaw)
	if !ok {
		*errs = append(*errs, vErr("subtitle.cues", "list_required", "subtitle.cues must be a list"))
		return
	}
	previousEnd := 0.0
	for index, rawCue := range cues {
		path := fmt.Sprintf("subtitle.cues[%d]", index)
		cue := checkedMapping(rawCue, path, errs)
		if cue == nil {
			continue
		}
		nonEmptyString(cue["id"], path+".id", errs)
		nonEmptyString(cue["text"], path+".text", errs)
		startRaw := cue["start_seconds"]
		endRaw := cue["end_seconds"]
		startOK := nonNegativeNumber(startRaw, path+".start_seconds", errs)
		endOK := positiveNumber(endRaw, path+".end_seconds", errs)
		if startOK && endOK {
			start, _ := numberValue(startRaw)
			end, _ := numberValue(endRaw)
			if end <= start {
				*errs = append(*errs, vErr(path, "invalid_interval", "subtitle end must be after start"))
			}
			if start < previousEnd {
				*errs = append(*errs, vErr(path, "overlap", "subtitle cues must not overlap"))
			}
			previousEnd = math.Max(previousEnd, end)
		}
	}
}

// --- packaging 校验 ---

func validatePackaging(value any, errs *[]map[string]any) {
	packaging := checkedMapping(value, "packaging", errs)
	if packaging == nil {
		return
	}
	if cover, present := packaging["cover"]; present && cover != nil {
		coverConfig := checkedMapping(cover, "packaging.cover", errs)
		if coverConfig != nil && PackagingSectionEnabled(coverConfig) {
			nonEmptyString(coverConfig["source_ref"], "packaging.cover.source_ref", errs)
			positiveNumber(coverConfig["duration_seconds"], "packaging.cover.duration_seconds", errs)
			fit := valueOr(coverConfig, "fit", "cover")
			if s, ok := fit.(string); !ok || (s != "cover" && s != "contain") {
				*errs = append(*errs, vErr("packaging.cover.fit", "invalid_fit", "cover fit must be cover or contain"))
			}
		}
	}
	for _, name := range []string{"intro", "outro"} {
		section, present := packaging[name]
		if !present || section == nil {
			continue
		}
		config := checkedMapping(section, "packaging."+name, errs)
		if config == nil || !PackagingSectionEnabled(config) {
			continue
		}
		nonEmptyString(config["text"], "packaging."+name+".text", errs)
		positiveNumber(config["duration_seconds"], "packaging."+name+".duration_seconds", errs)
	}
}

// --- 输出档校验 ---

func validateOutputProfile(value any, errs *[]map[string]any) {
	profile := checkedMapping(value, "output_profile", errs)
	if profile == nil {
		return
	}
	format := valueOr(profile, "format", "mp4")
	if s, ok := format.(string); !ok || s != "mp4" {
		*errs = append(*errs, vErr("output_profile.format", "unsupported_format", "only mp4 output is supported"))
	}
	for _, key := range []string{"width", "height"} {
		if raw, present := profile[key]; present {
			if v, ok := integerValue(raw); !ok || v <= 0 {
				*errs = append(*errs, vErr("output_profile."+key, "positive_integer_required", key+" must be positive"))
			}
		}
	}
	if raw, present := profile["fps"]; present {
		positiveNumber(raw, "output_profile.fps", errs)
	}
}

// ValidatePlanDocument 在不读文件、不调用媒体工具的前提下校验一份计划文档。
// 校验通过时返回 {"valid": true, "errors": [], "warnings": []}；失败时返回 *AssemblyPlanValidationError。
func ValidatePlanDocument(sourceSnapshot, timeline, audio, subtitle, packaging, outputProfile any) (map[string]any, error) {
	var errs []map[string]any
	checkedMapping(sourceSnapshot, "source_snapshot", &errs)
	validateTimeline(timeline, &errs)
	validateAudio(audio, &errs)
	validateSubtitles(subtitle, &errs)
	validatePackaging(packaging, &errs)
	validateOutputProfile(outputProfile, &errs)
	if len(errs) > 0 {
		return nil, &AssemblyPlanValidationError{Errors: errs}
	}
	return map[string]any{"valid": true, "errors": []any{}, "warnings": []any{}}, nil
}

// --- source snapshot ---

// BuildSourceSnapshot 把实时项目/媒体清单归一为稳定的快照文档。
// 未知字段原样保留以兼容旧快照；缺失字段显式保留为空，不编造取值。
func BuildSourceSnapshot(projectName string, sourceManifest map[string]any) map[string]any {
	result := make(map[string]any, len(sourceManifest)+8)
	for key, value := range sourceManifest {
		result[key] = value
	}
	items, _ := anyList(sourceManifest["items"])
	snapped := make([]any, 0, len(items))
	for _, item := range items {
		if m, ok := asMapping(item); ok {
			snapped = append(snapped, snapshotItem(m))
		}
	}
	result["snapshot_version"] = "media-assembly-source/v1"
	result["project"] = map[string]any{"name": projectName}
	result["script"] = sourceManifest["script"]
	result["episode"] = sourceManifest["episode"]
	result["content_mode"] = sourceManifest["content_mode"]
	result["generation_mode"] = sourceManifest["generation_mode"]
	result["collection"] = sourceManifest["collection"]
	result["manifest_contract_version"] = sourceManifest["contract_version"]
	result["manifest_fingerprint"] = sourceManifest["fingerprint"]
	result["items"] = snapped
	return result
}

func snapshotItem(item map[string]any) map[string]any {
	source, ok := asMapping(item["source"])
	if !ok {
		source = map[string]any{}
	}
	mediaProbe, ok := asMapping(item["media_probe"])
	if !ok {
		mediaProbe = map[string]any{}
	}
	fileFingerprint, ok := asMapping(item["file_fingerprint"])
	if !ok {
		fileFingerprint = map[string]any{
			"exists":   boolOr(mediaProbe["exists"], false),
			"size":     nil,
			"mtime_ns": nil,
			"sha256":   nil,
		}
	}
	return map[string]any{
		"order":                    item["order"],
		"unit_id":                  item["unit_id"],
		"description":              item["description"],
		"references":               valueOr(item, "references", []any{}),
		"planned_duration_seconds": item["planned_duration_seconds"],
		"asset_status":             item["asset_status"],
		"source":                   map[string]any{"kind": source["kind"], "path": source["path"]},
		"version":                  item["version"],
		"file_fingerprint":         cloneAnyMap(fileFingerprint),
		"media_probe":              cloneAnyMap(mediaProbe),
	}
}

// SourceFingerprint 计算计划所代表素材的稳定指纹（规范化 JSON 的 sha256）。
func SourceFingerprint(sourceSnapshot map[string]any) (string, error) {
	encoded, err := CanonicalJSON(sourceSnapshot)
	if err != nil {
		return "", &AssemblyPlanValidationError{Errors: []map[string]any{
			vErr("source_snapshot", "not_json", "source_snapshot must be JSON serializable"),
		}}
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// --- 规范化 JSON ---

// CanonicalJSON 输出紧凑、键有序、不转义非 ASCII 的 JSON：
// separator 为 "," / ":"，等价 Python json.dumps(ensure_ascii=False, sort_keys=True, separators=(",", ":"))。
//
// 与 Python 的已知差异：数值格式化由 Go 的 encoding/json 决定（例如从 JSON 读回的整数
// 与浮点在本实现里同为 float64，恒输出无小数点的整数形态），故跨语言逐字节比对不成立；
// 在本服务内同源计算、自比较，指纹语义（确定性 + 内容敏感）不受影响。
func CanonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// --- 通用小工具 ---

func valueOr(m map[string]any, key string, fallback any) any {
	if v, present := m[key]; present {
		return v
	}
	return fallback
}

// boolOr 取布尔值；缺失或类型不符时回退到 fallback。
func boolOr(v any, fallback bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}

// cloneAnyMap 浅拷贝一个对象，避免调用方后续改动污染快照。
func cloneAnyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
