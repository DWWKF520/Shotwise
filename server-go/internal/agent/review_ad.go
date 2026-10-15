package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
)

// ad 参考单元陈旧判定（对应 lib/reference_video/ad_units.py 的 is_ad_unit_stale 及其依赖）。
// 签名以 Python 落盘的 source_signature 为比较基准，因此规范化 JSON 必须与 Python
// json.dumps(sort_keys=True, ensure_ascii=False, separators=(",", ":")) 逐字节一致。

// adReferenceRef 是 unit 参考集中的一条参考（类型 + 原始名称）。
type adReferenceRef struct {
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

// adShotsByID 按 shot_id 索引 script.shots；非 dict 与缺 shot_id 的脏条目跳过。
func adShotsByID(script map[string]any) map[string]map[string]any {
	byID := map[string]map[string]any{}
	shots, _ := asList(script["shots"])
	for _, raw := range shots {
		shot, ok := asMap(raw)
		if !ok {
			continue
		}
		if id, ok := asString(shot["shot_id"]); ok && id != "" {
			byID[id] = shot
		}
	}
	return byID
}

// adUnitReferences 计算成员镜头参考的并集（产品在前，类型内按归一名首现去重）。
func adUnitReferences(shots []map[string]any) []adReferenceRef {
	var refs []adReferenceRef
	for _, f := range adReferenceFields {
		seen := map[string]bool{}
		for _, shot := range shots {
			names, ok := asList(shot[f.field])
			if !ok {
				continue
			}
			for _, raw := range names {
				name, ok := asString(raw)
				if !ok || name == "" {
					continue
				}
				canonical := domain.NormalizeAssetName(name)
				if seen[canonical] {
					continue
				}
				seen[canonical] = true
				refs = append(refs, adReferenceRef{Type: f.refType, Name: name})
			}
		}
	}
	return refs
}

// adSourceSignature 计算 unit 的编排 + 参考集签名（与 Python _source_signature 同口径）。
func adSourceSignature(byID map[string]map[string]any, unit map[string]any) string {
	shotIDs, _ := asList(unit["shot_ids"])
	var memberIDs []string
	var members []map[string]any
	for _, raw := range shotIDs {
		sid, ok := asString(raw)
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

	var b strings.Builder
	b.WriteString(`{"references":[`)
	for i, ref := range adUnitReferences(members) {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("[" + pyJSONString(ref.Type) + "," + pyJSONString(domain.NormalizeAssetName(ref.Name)) + "]")
	}
	b.WriteString(`],"shot_ids":[`)
	for i, sid := range memberIDs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(pyJSONString(sid))
	}
	b.WriteString("]}")

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// adUnitStale 判定产物签名与当前编排签名是否偏离（同 Python _is_stale）：
// 无成片或无签名的存量产物视为非 stale。
func adUnitStale(byID map[string]map[string]any, unit map[string]any) bool {
	assets, _ := asMap(unit["generated_assets"])
	if clip, _ := asString(assets["video_clip"]); clip == "" {
		return false
	}
	recorded, _ := asString(assets["source_signature"])
	if recorded == "" {
		return false
	}
	return recorded != adSourceSignature(byID, unit)
}

// pyJSONString 按 Python json.dumps(ensure_ascii=False) 的字符串转义输出：
// 只转义引号、反斜杠与控制字符，其余 Unicode 原样保留。
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				b.WriteString(fmt.Sprintf(`\u%04x`, r))
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
