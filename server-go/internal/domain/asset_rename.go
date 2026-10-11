// 资产重命名的级联改写（纯逻辑，不碰文件系统）。对应 lib/asset_rename.py。
//
// 覆盖面与 lib/data_validator 的引用扫描口径对齐：骨架的引用数组、references[].name、
// speaker、@[名] mention、旧式剧本内嵌角色表。只识别结构、不校验语义——结构校验由写盘
// 入口的「不更坏」守卫兜底。
package domain

import (
	"path"
	"strings"
)

// listFieldsByType 是各资产类型在剧本/草稿骨架里的「名称列表」引用字段。
// 列表内只有 str 元素才是名称引用：drama 顶层 scenes 是场景 dict 列表，
// 与 narration 分段的场景名列表同 key 不同形，按元素类型即可区分。
var listFieldsByType = map[AssetKind]map[string]bool{
	AssetCharacter: {"characters_in_segment": true, "characters_in_scene": true, "characters_in_shot": true},
	AssetScene:     {"scenes": true},
	AssetProp:      {"props": true},
	AssetProduct:   {"products_in_shot": true},
}

// mentionNameSplit 拆 “角色/衍生slug“：mention 语法里只有一层斜杠有意义。
func mentionNameSplit(name string) (string, string) {
	i := strings.Index(name, "/")
	if i < 0 {
		return name, ""
	}
	characterName, slug := name[:i], name[i+1:]
	if characterName == "" || slug == "" || strings.Contains(slug, "/") {
		return name, ""
	}
	return characterName, slug
}

func isList(v any) bool {
	_, ok := v.([]any)
	return ok
}

// RewriteMentions 把文本中指向 oldName 的 @ 引用改写为 @[newName]，返回新文本与改写数。
//
// 识别 @[名称] 与旧式裸 @名称 两形态，改写时统一升格为包裹形式；判等走 NFC 坐标系
// （正文可能以 NFD 落盘）；不做其他归一——重命名只该改名字，不该顺带改正文编码形式。
// 已是目标形式的不计数。
func RewriteMentions(text, oldName, newName string) (string, int) {
	target := NormalizeAssetName(oldName)
	runes := []rune(text)
	var (
		out   []rune
		last  int
		count int
	)
	for _, m := range iterMentions(runes) {
		characterName, variantSlug := mentionNameSplit(m.name)
		if NormalizeAssetName(characterName) != target {
			continue
		}
		replacement := "@[" + newName + "]"
		if variantSlug != "" {
			replacement = "@[" + newName + "/" + variantSlug + "]"
		}
		if string(runes[m.start:m.end]) == replacement {
			continue
		}
		out = append(out, runes[last:m.start]...)
		out = append(out, []rune(replacement)...)
		last = m.end
		count++
	}
	if count == 0 {
		return text, 0
	}
	out = append(out, runes[last:]...)
	return string(out), count
}

type mentionSpan struct {
	start, end int
	name       string
}

// iterMentions 定位文本里的 mention 区间。
//
// @ 左侧是 ASCII 单词字符时视为邮箱/id 片段；包裹形态不跨行；花括号包裹刻意不认
// （编辑器只写 @[名称]，运行时契约保持在单一包裹形态上）。
func iterMentions(runes []rune) []mentionSpan {
	var out []mentionSpan
	for i := 0; i < len(runes); {
		if runes[i] != '@' {
			i++
			continue
		}
		if i > 0 && IsASCIIWordChar(runes[i-1]) {
			i++
			continue
		}
		if i+1 >= len(runes) {
			i++
			continue
		}
		if runes[i+1] == '[' {
			close := -1
			for j := i + 2; j < len(runes); j++ {
				if runes[j] == '\n' || runes[j] == '\r' {
					break
				}
				if runes[j] == ']' {
					close = j
					break
				}
			}
			if close < 0 {
				i++
				continue
			}
			out = append(out, mentionSpan{start: i, end: close + 1, name: string(runes[i+2 : close])})
			i = close + 1
			continue
		}
		j := i + 1
		for j < len(runes) && isLegacyMentionChar(runes[j]) {
			j++
		}
		if j <= i+1 {
			i++
			continue
		}
		out = append(out, mentionSpan{start: i, end: j, name: string(runes[i+1 : j])})
		i = j
	}
	return out
}

func isLegacyMentionChar(r rune) bool {
	if r == '_' || IsASCIIWordChar(r) {
		return true
	}
	return r >= '一' && r <= '鿿'
}

func nameMatches(value any, target, newName string) bool {
	s, ok := value.(string)
	return ok && NormalizeAssetName(s) == target && s != newName
}

// RewritePayloadReferences 就地把剧本/草稿 payload 中指向 oldName 的名称引用改为 newName，
// 返回改写处数。
func RewritePayloadReferences(payload map[string]any, kind AssetKind, oldName, newName string) int {
	target := NormalizeAssetName(oldName)
	listFields := listFieldsByType[kind]
	count := 0

	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			for key, value := range v {
				if listFields[key] {
					if list, ok := value.([]any); ok {
						for i, item := range list {
							if nameMatches(item, target, newName) {
								list[i] = newName
								count++
							} else {
								walk(item)
							}
						}
						continue
					}
				}
				if key == "references" {
					if list, ok := value.([]any); ok {
						for _, item := range list {
							ref, ok := item.(map[string]any)
							if !ok {
								continue
							}
							if ref["type"] == string(kind) && nameMatches(ref["name"], target, newName) {
								ref["name"] = newName
								count++
							}
						}
						continue
					}
				}
				if key == "speaker" && kind == AssetCharacter && nameMatches(value, target, newName) {
					v[key] = newName
					count++
					continue
				}
				if (key == "shots" || key == "units") && isList(value) {
					for _, item := range value.([]any) {
						// 参考路线 shot 只有 text（改写 mention）；ad shot 还带引用数组与
						// video_prompt.dialogue，继续下钻由通用规则处理。unit 一并认：
						// 隔离草稿装的是扁平书写层产物，mention 直接落在 units[].text 上。
						if m, ok := item.(map[string]any); ok {
							if text, ok := m["text"].(string); ok {
								rewritten, n := RewriteMentions(text, oldName, newName)
								if n > 0 {
									m["text"] = rewritten
									count += n
								}
							}
						}
						walk(item)
					}
					continue
				}
				walk(value)
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		}
	}

	if kind == AssetCharacter {
		// 旧式剧本把角色表镜像内嵌在顶层 characters dict，收编整簇等价 key 并同步路径字段，
		// 避免旧名以镜像形式残留。
		if embedded, ok := payload["characters"].(map[string]any); ok {
			count += rekeyEquivalentDict(embedded, oldName, newName, AssetSpecs[kind])
		}
	}
	walk(payload)
	return count
}

// rekeyEquivalentDict 在裸 dict 形态的桶里做与 AssetBucket.RekeyEquivalent 同语义的收编，
// 返回改写数（key 变化 + 路径字段同步）。
func rekeyEquivalentDict(bucket map[string]any, oldName, newName string, spec AssetSpec) int {
	target := NormalizeAssetName(oldName)
	count := 0
	for key := range bucket {
		if NormalizeAssetName(key) != target {
			continue
		}
		value := bucket[key]
		delete(bucket, key)
		bucket[newName] = value
		if key != newName {
			count++
		}
		if entry, ok := value.(map[string]any); ok {
			count += RewriteEntryPaths(WrapAssetEntry(entry), spec, oldName, newName)
		}
	}
	return count
}

// RenamedFileStem 按「文件 stem = 资产名」不变式推导改名后的 stem。
//
// 默认只认 stem 精确等于旧名（sheet / 参考图 / 参考音频）。allowSequence 额外放行
// 「旧名_{序号}」形态——仅多图序列资产的 refs 子目录可用：资产名本身允许下划线加数字，
// 在普通目录放行会把兄弟资产「旧名_2」的文件一并卷走。
func RenamedFileStem(stem, oldName, newName string, allowSequence bool) (string, bool) {
	normalized := NormalizeAssetName(stem)
	target := NormalizeAssetName(oldName)
	if normalized == target {
		return newName, true
	}
	if allowSequence && strings.HasPrefix(normalized, target+"_") {
		if suffix := normalized[len(target)+1:]; isAllDigits(suffix) {
			return newName + "_" + suffix, true
		}
	}
	return "", false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// RenamedRelPath 把路径字段值里的文件 stem 从旧名改为新名；与旧名无关时返回 ok=false。
func RenamedRelPath(rel, oldName, newName string, allowSequence bool) (string, bool) {
	normalized := strings.ReplaceAll(rel, "\\", "/")
	dir, base := path.Split(normalized)
	stem, ext := splitStemExt(base)
	renamed, ok := RenamedFileStem(stem, oldName, newName, allowSequence)
	if !ok {
		return "", false
	}
	return dir + renamed + ext, true
}

// splitStemExt 按 POSIX 语义拆 stem/扩展名：前导点属于 stem（.DS_Store 无扩展名），
// 这与 Go 的 path.Ext 行为不同，必须自己切。
func splitStemExt(base string) (stem, ext string) {
	dot := strings.LastIndex(base, ".")
	if dot <= 0 {
		return base, ""
	}
	return base[:dot], base[dot:]
}

// RewriteEntryPaths 同步条目里按名命名的路径字段，返回改写数。
//
// 改写范围与文件迁移范围逐维对齐：只动落在该类型目录本级及 refs / refs_audio 子目录下、
// 且 stem 命中旧名的值。「旧名_{序号}」只在多图序列资产的 refs 子目录放行。
// 用户手动指到别处的路径（如 thumbnails/旧名.png）不动——那里的文件不在迁移范围内，
// 改了字段反而把一条有效引用改成悬空。
func RewriteEntryPaths(entry *AssetEntry, spec AssetSpec, oldName, newName string) int {
	migrated := map[string]bool{}
	for _, d := range spec.MigratedDirs() {
		migrated[d] = true
	}
	sequencedRefs := spec.SequencedRefs()
	refsDir := spec.Subdir + "/refs"

	rewrite := func(value string, sequencedField bool) (string, bool) {
		normalized := strings.ReplaceAll(value, "\\", "/")
		parent := path.Dir(normalized)
		if !migrated[parent] {
			return "", false
		}
		allowSequence := sequencedField && sequencedRefs && parent == refsDir
		renamed, ok := RenamedRelPath(normalized, oldName, newName, allowSequence)
		if !ok || renamed == value {
			return "", false
		}
		return renamed, true
	}

	count := 0
	for _, field := range spec.PathStringFields() {
		value, ok := entry.String(field)
		if !ok || value == "" {
			continue
		}
		if renamed, changed := rewrite(value, false); changed {
			entry.SetString(field, renamed)
			count++
		}
	}
	if images, ok := entry.Fields()["reference_images"].([]any); ok {
		for i, item := range images {
			value, ok := item.(string)
			if !ok || value == "" {
				continue
			}
			if renamed, changed := rewrite(value, true); changed {
				images[i] = renamed
				count++
			}
		}
	}
	return count
}
