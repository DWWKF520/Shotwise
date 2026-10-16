// 参考生视频 unit 的纯函数域（对应 lib/reference_video/units.py、ad_units.py 与 shot_parser 的
// 拼接函数）。不读磁盘、不依赖项目状态：定桶判据、ad 派生分组、成员镜头水合、来源签名与 stale
// 判定都收敛在这里，供入队工具、执行期与审阅读侧共用同一份口径。
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// AdUnitMaxShots 是单个 ad video_unit 最多容纳的镜头数，与 “ReferenceVideoUnit.shots“ 的
// max_length=4 同口径（一个 unit 是一次视频生成调用的最小粒度）。
const AdUnitMaxShots = 4

// AdReference 是 unit 参考集中的一条参考（原始类型 + 原始名称）。
type AdReference struct {
	Type string
	Name string
}

// adReferenceFields 是镜头字段到参考类型的映射，顺序即注入优先级（产品绝对优先）。
var adReferenceFields = []struct{ field, refType string }{
	{"products_in_shot", "product"},
	{"characters_in_shot", "character"},
	{"scenes", "scene"},
	{"props", "prop"},
}

// GeneratedAssets 归一化访问条目的 generated_assets 容器：脏数据（非对象）按空对象处理，
// 让调用方走各自「未生成」分支而非抛错中断整批。
func GeneratedAssets(item map[string]any) map[string]any {
	assets, ok := item["generated_assets"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return assets
}

// EmptyGeneratedAssets 返回新建 unit 的默认 produced-assets 容器（字段全 nil、status=pending），
// 对应 Python GeneratedAssets().model_dump()。
func EmptyGeneratedAssets() map[string]any {
	return map[string]any{
		"storyboard_image":      nil,
		"storyboard_last_image": nil,
		"grid_id":               nil,
		"grid_cell_index":       nil,
		"video_clip":            nil,
		"video_thumbnail":       nil,
		"video_uri":             nil,
		"narration_audio":       nil,
		"status":                "pending",
		"video_generated_at":    nil,
		"source_signature":      nil,
	}
}

// AdShotDurationSeconds 取 ad 单镜头时长（秒）的脏数据归一口径：非对象条目、非正整数时长
// （布尔不算）一律按 0 计。
func AdShotDurationSeconds(shot any) int {
	m, ok := shot.(map[string]any)
	if !ok {
		return 0
	}
	if n, ok := positiveInt(m["duration_seconds"]); ok {
		return n
	}
	return 0
}

// AdScriptTotalDuration 求 ad 剧本 shots 总时长（秒）：脏数据按 0 计、不抛。
func AdScriptTotalDuration(shots []map[string]any) int {
	total := 0
	for _, shot := range shots {
		total += AdShotDurationSeconds(shot)
	}
	return total
}

// positiveInt 取真正的正整数（布尔与非整数值不算，JSON 数字的整数值算整数）。
func positiveInt(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		if v != float64(int(v)) || v <= 0 {
			return 0, false
		}
		return int(v), true
	case int:
		if v <= 0 {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// AdShotsByID 按 shot_id 索引 script.shots（内容唯一真相）；脏条目（非对象 / 缺 shot_id）跳过。
func AdShotsByID(script map[string]any) map[string]map[string]any {
	byID := map[string]map[string]any{}
	shots, _ := script["shots"].([]any)
	for _, raw := range shots {
		shot, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := shot["shot_id"].(string)
		if id != "" {
			byID[id] = shot
		}
	}
	return byID
}

// AdUnitReferences 计算成员镜头参考的并集（产品在前，类型内按归一名首现去重）。
func AdUnitReferences(shots []map[string]any) []AdReference {
	var refs []AdReference
	for _, f := range adReferenceFields {
		seen := map[string]bool{}
		for _, shot := range shots {
			names, ok := shot[f.field].([]any)
			if !ok {
				continue
			}
			for _, raw := range names {
				name, ok := raw.(string)
				if !ok || name == "" {
					continue
				}
				canonical := NormalizeAssetName(name)
				if seen[canonical] {
					continue
				}
				seen[canonical] = true
				refs = append(refs, AdReference{Type: f.refType, Name: name})
			}
		}
	}
	return refs
}

// AdUnitReferencesMaps 把参考集渲染成 “[{"type","name"}]“ 结构（剧本里持久化的形态）。
func AdUnitReferencesMaps(shots []map[string]any) []any {
	refs := AdUnitReferences(shots)
	out := make([]any, 0, len(refs))
	for _, ref := range refs {
		out = append(out, map[string]any{"type": ref.Type, "name": ref.Name})
	}
	return out
}

// AdSourceSignature 计算 unit 的编排 + 参考集签名（sha256 十六进制），对应 Python
// _source_signature——规范化 JSON 必须与 Python json.dumps(sort_keys=True, ensure_ascii=False,
// separators=(",", ":")) 逐字节一致（经 CanonicalJSON 输出）。
func AdSourceSignature(byID map[string]map[string]any, unit map[string]any) string {
	shotIDs, _ := unit["shot_ids"].([]any)
	memberIDs := make([]any, 0, len(shotIDs))
	var members []map[string]any
	for _, raw := range shotIDs {
		sid, ok := raw.(string)
		if !ok {
			continue
		}
		shot, found := byID[sid]
		if !found {
			continue
		}
		memberIDs = append(memberIDs, sid)
		members = append(members, shot)
	}
	references := make([]any, 0, 4)
	for _, ref := range AdUnitReferences(members) {
		references = append(references, []any{ref.Type, NormalizeAssetName(ref.Name)})
	}
	payload := map[string]any{"shot_ids": memberIDs, "references": references}
	encoded, err := CanonicalJSON(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// AdUnitSourceSignature 按剧本现算 unit 的来源签名。
func AdUnitSourceSignature(script map[string]any, unit map[string]any) string {
	return AdSourceSignature(AdShotsByID(script), unit)
}

// AdUnitStaleByID 判定产物签名与当前编排签名是否偏离（无成片或无签名的存量产物视为非 stale）。
func AdUnitStaleByID(byID map[string]map[string]any, unit map[string]any) bool {
	assets := GeneratedAssets(unit)
	clip, _ := assets["video_clip"].(string)
	if clip == "" {
		return false
	}
	recorded, _ := assets["source_signature"].(string)
	if recorded == "" {
		return false
	}
	return recorded != AdSourceSignature(byID, unit)
}

// AdUnitStale 按剧本现算 stale。
func AdUnitStale(script map[string]any, unit map[string]any) bool {
	return AdUnitStaleByID(AdShotsByID(script), unit)
}

// AdStaleUnitIDs 返回偏离当前编排的 unit_id 清单（无 unit_id 的脏条目跳过）。
func AdStaleUnitIDs(script map[string]any, units []any) []string {
	byID := AdShotsByID(script)
	var out []string
	for _, raw := range units {
		u, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := u["unit_id"].(string)
		if id == "" {
			continue
		}
		if AdUnitStaleByID(byID, u) {
			out = append(out, id)
		}
	}
	return out
}

// DeriveAdReferenceUnits 把 ad 剧本的 shots 按顺序派生为 video_unit 轻量索引（纯函数）。
//
// 分组只取连续镜头、不重排；每 unit 最多 AdUnitMaxShots 个 shot；maxUnitDuration（供应商单次
// 生成时长上限，秒）给定时 unit 内镜头时长之和不超过该上限（单镜头自身超上限时独立成 unit）。
func DeriveAdReferenceUnits(shots []any, episode int, maxUnitDuration *int) []map[string]any {
	if maxUnitDuration != nil && *maxUnitDuration <= 0 {
		maxUnitDuration = nil
	}

	var groups [][]map[string]any
	var current []map[string]any
	currentDuration := 0
	for _, raw := range shots {
		shot, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := shot["shot_id"].(string); id == "" {
			continue
		}
		duration := AdShotDurationSeconds(shot)
		overCount := len(current) >= AdUnitMaxShots
		overDuration := maxUnitDuration != nil && len(current) > 0 && currentDuration+duration > *maxUnitDuration
		if overCount || overDuration {
			groups = append(groups, current)
			current = nil
			currentDuration = 0
		}
		current = append(current, shot)
		currentDuration += duration
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}

	out := make([]map[string]any, 0, len(groups))
	for n, group := range groups {
		ids := make([]any, 0, len(group))
		for _, shot := range group {
			ids = append(ids, shot["shot_id"])
		}
		out = append(out, map[string]any{
			"unit_id":    fmt.Sprintf("E%dU%d", episode, n+1),
			"shot_ids":   ids,
			"references": AdUnitReferencesMaps(group),
		})
	}
	return out
}

// MergeAdReferenceUnits 把新派生的分组与剧本中已持久化的索引合并（不改入参）。
//
// 按 unit_id 沿用旧条目的 generated_assets，从不清空产物指针。
func MergeAdReferenceUnits(existing any, derived []map[string]any) []map[string]any {
	existingByID := map[string]map[string]any{}
	if list, ok := existing.([]any); ok {
		for _, raw := range list {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := entry["unit_id"].(string); id != "" {
				existingByID[id] = entry
			}
		}
	}

	merged := make([]map[string]any, 0, len(derived))
	for _, unit := range derived {
		var assets map[string]any
		if prev, ok := existingByID[unit["unit_id"].(string)]; ok {
			assets = cloneAnyMap(GeneratedAssets(prev))
		}
		if len(assets) == 0 {
			assets = EmptyGeneratedAssets()
		}
		entry := map[string]any{}
		for k, v := range unit {
			entry[k] = v
		}
		entry["generated_assets"] = assets
		merged = append(merged, entry)
	}
	return merged
}

// SyncAdReferenceUnits 从 shots 重新派生分组并写回 “script["reference_units"]“，返回合并后索引。
func SyncAdReferenceUnits(script map[string]any, episode int, maxUnitDuration *int) []map[string]any {
	shots, _ := script["shots"].([]any)
	derived := DeriveAdReferenceUnits(shots, episode, maxUnitDuration)
	merged := MergeAdReferenceUnits(script["reference_units"], derived)
	script["reference_units"] = toAnyList(merged)
	return merged
}

// ResolveAdUnitShots 按索引条目的 shot_ids 从 shots 水合成员镜头（保持索引顺序）。
//
// 任一 shot_id 在 shots 中不存在（索引已过期）时返回错误，调用方应提示重新派生分组。
func ResolveAdUnitShots(script map[string]any, unit map[string]any) ([]map[string]any, error) {
	byID := AdShotsByID(script)
	var resolved []map[string]any
	var missing []string
	ids, _ := unit["shot_ids"].([]any)
	for _, raw := range ids {
		sid, _ := raw.(string)
		shot, found := byID[sid]
		if !found {
			missing = append(missing, sid)
			continue
		}
		resolved = append(resolved, shot)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("分组索引已过期：unit %v 引用的镜头 %s 不存在，请重新派生分组",
			unit["unit_id"], strings.Join(missing, ", "))
	}
	return resolved, nil
}

// ReferenceVideoBucket 是参考生视频镜头的能力桶：有参考图 → r2v；无参考图的退化镜头 → i2v。
func ReferenceVideoBucket(withReferences bool) string {
	if withReferences {
		return "r2v"
	}
	return "i2v"
}

// ReferenceUnitVideoBucket 是 unit 的能力桶（读侧近似判据）。ad 路径传入水合后的成员镜头，
// 参考集从镜头现算；narration/drama 的 unit 内容自包含，传 nil。
func ReferenceUnitVideoBucket(unit map[string]any, adShots []map[string]any) string {
	if adShots != nil {
		return ReferenceVideoBucket(len(AdUnitReferences(adShots)) > 0)
	}
	if unit == nil {
		return ReferenceVideoBucket(false)
	}
	return ReferenceVideoBucket(refTruthy(unit["references"]))
}

// refTruthy 复刻 Python bool(value) 的真值判定（脏数据稳健）。
func refTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

// FindReferenceUnit 在剧本中定位参考生视频 unit：ad 在 reference_units 派生索引，其余在
// video_units。
func FindReferenceUnit(script map[string]any, unitID string, isAd bool) (map[string]any, bool) {
	key := "video_units"
	if isAd {
		key = "reference_units"
	}
	units, _ := script[key].([]any)
	for _, raw := range units {
		u, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := u["unit_id"].(string); id == unitID {
			return u, true
		}
	}
	return nil, false
}

// AssembleShotsText 把 unit.shots[*].text 拼接为单一原始 prompt（三段论渲染之前的书写层文本）。
//
// 对畸形数据防御性归一化：非对象 shot 跳过；text 缺失或非字符串按空串处理。
func AssembleShotsText(shots any) string {
	list, ok := shots.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(list))
	for _, raw := range list {
		s, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		text, _ := s["text"].(string)
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n")
}

// ShotPromptText 取单镜头的画面描述文本：静态画面 + 动作 + 运镜 + 环境音 + 台词。
//
// 口播文案（voiceover_text）刻意不进画面生成 prompt。
func ShotPromptText(shot map[string]any) string {
	imagePrompt, _ := shot["image_prompt"].(map[string]any)
	videoPrompt, _ := shot["video_prompt"].(map[string]any)
	text := func(m map[string]any, key string) string {
		if m == nil {
			return ""
		}
		s, _ := m[key].(string)
		return strings.TrimSpace(s)
	}

	var parts []string
	if scene := text(imagePrompt, "scene"); scene != "" {
		parts = append(parts, scene)
	}
	if action := text(videoPrompt, "action"); action != "" {
		parts = append(parts, action)
	}
	if camera := text(videoPrompt, "camera_motion"); camera != "" {
		parts = append(parts, "运镜："+camera)
	}
	if audio := text(videoPrompt, "ambiance_audio"); audio != "" {
		parts = append(parts, "环境音："+audio)
	}
	if dialogue, ok := videoPrompt["dialogue"].([]any); ok {
		for _, raw := range dialogue {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			speaker := text(entry, "speaker")
			if speaker != "" {
				// 归一到与音色绑定声明相同的坐标系：<X> 与台词音色参考必须字节一致。
				speaker = NormalizeAssetName(speaker)
			}
			line := text(entry, "line")
			if line != "" && speaker != "" {
				parts = append(parts, fmt.Sprintf("<%s>说 {%s}", speaker, line))
			}
		}
	}
	return strings.Join(parts, "；")
}

// RenderAdUnitPrompt 把 unit 的成员镜头渲染为多镜头视频生成 prompt。
//
// 每个镜头一行 “Shot {n} ({duration}s): {画面描述}“；项目风格以 “Style:“ 头行注入。
// 所有镜头都无画面内容时返回空串，让入队守卫当场拒绝。
func RenderAdUnitPrompt(shots []map[string]any, style *string) string {
	var lines []string
	for n, shot := range shots {
		text := ShotPromptText(shot)
		if text == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("Shot %d (%ds): %s", n+1, AdShotDurationSeconds(shot), text))
	}
	if len(lines) == 0 {
		return ""
	}
	if style != nil {
		if normalized := normalizeStyleValue(*style); normalized != "" {
			lines = insertFront(lines, "Style: "+normalized)
		}
	}
	return strings.Join(lines, "\n")
}

// normalizeStyleValue 去掉风格值开头的「画风：」前缀并 strip 两端空白（幂等）。
func normalizeStyleValue(style string) string {
	s := strings.TrimSpace(style)
	for _, prefix := range []string{"画风：", "画风:"} {
		if strings.HasPrefix(s, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(s, prefix))
		}
	}
	return s
}

func insertFront(list []string, item string) []string {
	out := make([]string, 0, len(list)+1)
	out = append(out, item)
	return append(out, list...)
}

// pyJSONString 已由 task_failure.go 提供（ensure_ascii=False 转义口径）；cloneAnyMap 由
// media_assembly.go 提供——本模块复用同包既有实现，不重复定义。

func toAnyList(in []map[string]any) []any {
	out := make([]any, 0, len(in))
	for _, item := range in {
		out = append(out, item)
	}
	return out
}
