// 资产类型规格与名称规范化。对应 lib/asset_types.ASSET_SPECS 与 validate_asset_name。
//
// project.json 是四类资产目录的唯一真相源；characters/scenes/props/products 共用同一套
// 抽象，新增类型只需在 AssetSpecs 注册。
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// AssetKind 是资产类型。四类资产共用一套抽象。
type AssetKind string

const (
	AssetCharacter AssetKind = "character"
	AssetScene     AssetKind = "scene"
	AssetProp      AssetKind = "prop"
	AssetProduct   AssetKind = "product"
)

// AssetSpec 描述一类资产的存储契约。
type AssetSpec struct {
	Kind              AssetKind
	BucketKey         string // project.json 上的键名，也是版本历史的 resource_type
	Subdir            string // 项目目录下的子目录
	SheetKey          string // 定妆图/设计稿字段名
	ExtraStringFields []string
	ExtraListFields   []string
	InGlobalLibrary   bool
}

// AssetSpecs 是资产规格的单一真相源。
var AssetSpecs = map[AssetKind]AssetSpec{
	AssetCharacter: {
		Kind: AssetCharacter, BucketKey: "characters", Subdir: "characters", SheetKey: "character_sheet",
		// voice_updated_at 不在可写字段里：它只由系统在 reference_audio 真正变更时机械戳写，
		// 开放任意覆写会让 voice_notice_dismissed_at 的等值校验被绕过。
		ExtraStringFields: []string{"voice_style", "reference_image", "reference_audio", "character_avatar", "voice_notice_dismissed_at"},
		InGlobalLibrary:   true,
	},
	AssetScene: {
		Kind: AssetScene, BucketKey: "scenes", Subdir: "scenes", SheetKey: "scene_sheet",
		InGlobalLibrary: true,
	},
	AssetProp: {
		Kind: AssetProp, BucketKey: "props", Subdir: "props", SheetKey: "prop_sheet",
		InGlobalLibrary: true,
	},
	AssetProduct: {
		Kind: AssetProduct, BucketKey: "products", Subdir: "products", SheetKey: "product_sheet",
		ExtraStringFields: []string{"brand"},
		ExtraListFields:   []string{"reference_images", "selling_points"},
		// 全局资产库是单图列模型，多图列表型的 product 暂不进入。
		InGlobalLibrary: false,
	},
}

// AssetTypeOrder 是路由注册与遍历的稳定顺序。
var AssetTypeOrder = []AssetKind{AssetCharacter, AssetScene, AssetProp, AssetProduct}

// Spec 返回资产规格，未知类型返回 false。
func Spec(kind AssetKind) (AssetSpec, bool) {
	s, ok := AssetSpecs[kind]
	return s, ok
}

// WritableStringFields 是 PATCH 可写的字符串字段集合（description + sheet + extra）。
func (s AssetSpec) WritableStringFields() []string {
	return append([]string{"description", s.SheetKey}, s.ExtraStringFields...)
}

// WritableListFields 是 PATCH 可写的列表字段集合。
func (s AssetSpec) WritableListFields() []string {
	return s.ExtraListFields
}

// PathStringFields 是随资产名迁移的字符串路径字段。
//
// 名单是硬编码的四项，不是「所有 extra 字符串字段」：voice_style 是配音风格文本、
// voice_notice_dismissed_at 是 UI 状态，都与文件名无关，改名时不该碰。
// 列表型路径字段只有 reference_images 一项（多图序列），由 RewriteEntryPaths 单独处理。
func (s AssetSpec) PathStringFields() []string {
	return []string{s.SheetKey, "character_avatar", "reference_image", "reference_audio"}
}

// MigratedDirs 是该类型文件迁移覆盖的目录（相对项目根）：本级 + refs / refs_audio。
func (s AssetSpec) MigratedDirs() []string {
	return []string{s.Subdir, s.Subdir + "/refs", s.Subdir + "/refs_audio"}
}

// SequencedRefs 标记 refs 子目录下是否按「名_序号」累积命名（仅多图资产放行）。
func (s AssetSpec) SequencedRefs() bool {
	return containsString(s.ExtraListFields, "reference_images")
}

func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// --- 资产名 ---

// ErrInvalidAssetName 资产名非法。对外文案走 asset_invalid_name，不回传原因细节。
var ErrInvalidAssetName = errors.New("invalid asset name")

// illegalAssetNameChars 是既不能进文件路径也不能进单段路由参数的字符。
var illegalAssetNameChars = []string{"/", "\\", "\x00", ":", "*", "?", "\"", "<", ">", "|", "]"}

// windowsReservedBasenames 是 Windows 保留设备名；带扩展名同样保留（CON.backup 亦拒）。
var windowsReservedBasenames = func() map[string]bool {
	out := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}
	for i := 1; i <= 9; i++ {
		out[fmt.Sprintf("COM%d", i)] = true
		out[fmt.Sprintf("LPT%d", i)] = true
	}
	return out
}()

// NormalizeAssetName 把名字归一到比对坐标系（Unicode NFC）。
//
// 资产名判等必须先归一：NFC（网页表单与登记侧主形态）与 NFD（macOS 文件系统、部分输入法）
// 屏幕显示相同、字节不同，vi 等语种尤其容易同时出现。归一只收敛编码形式，不改字。
func NormalizeAssetName(name string) string {
	return norm.NFC.String(name)
}

// ValidateAssetName 校验并规范化（strip + NFC）资产名，非法时返回 ErrInvalidAssetName。
//
// 名字会被拼进文件路径与单段路由参数，必须在边界就拒掉路径不安全的形式，
// 否则后续按名访问（PATCH/DELETE/rename）全部失效。
func ValidateAssetName(raw string) (string, error) {
	name := NormalizeAssetName(strings.TrimSpace(raw))
	if name == "" {
		return "", ErrInvalidAssetName
	}
	if strings.Contains(name, "..") {
		return "", ErrInvalidAssetName
	}
	for _, illegal := range illegalAssetNameChars {
		if strings.Contains(name, illegal) {
			return "", ErrInvalidAssetName
		}
	}
	for _, r := range name {
		if r < 32 || r == 127 {
			return "", ErrInvalidAssetName
		}
	}
	if strings.HasSuffix(name, ".") {
		return "", ErrInvalidAssetName
	}
	base := name
	if i := strings.Index(base, "."); i >= 0 {
		base = base[:i]
	}
	if windowsReservedBasenames[strings.ToUpper(base)] {
		return "", ErrInvalidAssetName
	}
	return name, nil
}

// --- 资产条目 ---

// AssetEntry 承载一个资产条目。
//
// 用 map 而不是定长结构体：四类资产的字段集不同（character 有 voice_style、product 有
// reference_images），且创建请求是 extra="allow" 语义。定长结构体会在写回时静默丢掉
// 对端（Python 服务、上传流程）写入的未知字段，因此原样保留全部键。
type AssetEntry struct {
	fields map[string]any
}

// NewAssetEntry 构造空条目。
func NewAssetEntry() *AssetEntry { return &AssetEntry{fields: map[string]any{}} }

// WrapAssetEntry 用已有 map 构造条目；nil 时返回空条目。
func WrapAssetEntry(fields map[string]any) *AssetEntry {
	if fields == nil {
		return NewAssetEntry()
	}
	return &AssetEntry{fields: fields}
}

// Fields 返回底层 map（同一份数据，不拷贝）。
func (e *AssetEntry) Fields() map[string]any {
	if e == nil {
		return nil
	}
	if e.fields == nil {
		e.fields = map[string]any{}
	}
	return e.fields
}

// Clone 深拷贝条目，供 dry_run 在副本上试算。
func (e *AssetEntry) Clone() *AssetEntry {
	return WrapAssetEntry(cloneMap(e.Fields()))
}

// String 读取字符串字段；缺失或非字符串返回 ok=false。
func (e *AssetEntry) String(name string) (string, bool) {
	v, ok := e.Fields()[name].(string)
	return v, ok
}

// MustString 读取字符串字段，缺失时返回空串。
func (e *AssetEntry) MustString(name string) string {
	v, _ := e.String(name)
	return v
}

// SetString 写入字符串字段。
func (e *AssetEntry) SetString(name, value string) { e.Fields()[name] = value }

// List 读取字符串列表；只要有一个元素不是 string 就返回 ok=false（脏数据按缺失处理，
// 不让它渗进生成链路做路径拼接）。
func (e *AssetEntry) List(name string) ([]string, bool) {
	raw, ok := e.Fields()[name].([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// SetList 写入字符串列表。
func (e *AssetEntry) SetList(name string, values []string) {
	list := make([]any, 0, len(values))
	for _, v := range values {
		list = append(list, v)
	}
	e.Fields()[name] = list
}

// Description 是条目描述。
func (e *AssetEntry) Description() string { return e.MustString("description") }

// Sheet 是该类型的定妆图/设计稿路径字段。
func (e *AssetEntry) Sheet(spec AssetSpec) string { return e.MustString(spec.SheetKey) }

// UnmarshalJSON 原样收下全部键。
func (e *AssetEntry) UnmarshalJSON(data []byte) error {
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	e.fields = fields
	return nil
}

// MarshalJSON 按 description 优先、其余键字典序输出，保证重复写入不产生抖动。
//
// 注意：Go 的 map 无插入序，资产在 project.json 里的排列会稳定为字典序；
// 这是切到 Go 写入后的一次性重排，之后保持幂等。
func (e AssetEntry) MarshalJSON() ([]byte, error) {
	return marshalOrdered(e.fields, "description")
}

// marshalOrdered 先输出 priority 键、其余按字典序输出。值统一交给 encoding/json 编码，
// 不自己拼字符串。
func marshalOrdered(fields map[string]any, priority ...string) ([]byte, error) {
	if fields == nil {
		return []byte("{}"), nil
	}
	var order []string
	seen := map[string]bool{}
	for _, k := range priority {
		if _, ok := fields[k]; ok && !seen[k] {
			order = append(order, k)
			seen[k] = true
		}
	}
	for _, k := range sortedKeys(fields) {
		if !seen[k] {
			order = append(order, k)
		}
	}
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range order {
		if i > 0 {
			sb.WriteByte(',')
		}
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		value, err := json.Marshal(fields[k])
		if err != nil {
			return nil, err
		}
		sb.Write(key)
		sb.WriteByte(':')
		sb.Write(value)
	}
	sb.WriteByte('}')
	return []byte(sb.String()), nil
}

// AssetBucket 是「资产名 → 条目」的桶。
type AssetBucket map[string]*AssetEntry

// ResolveKey 按比对坐标系（NFC）解析真实落盘 key，未命中返回 ok=false。
//
// 存量 key 可能是任一编码形式，不能拿归一后的名字直接下标，否则会对着同一个视觉名字
// 新建第二条资产或漏判冲突。多形式并存时后写入的胜出（与读侧合并方向一致）。
func (b AssetBucket) ResolveKey(name string) (string, bool) {
	target := NormalizeAssetName(name)
	found := ""
	ok := false
	for key := range b {
		if NormalizeAssetName(key) == target {
			found, ok = key, true
		}
	}
	return found, ok
}

// RekeyEquivalent 把桶中与 oldName 视觉等价的全部 key 一并改成 newName，返回改名后的条目。
//
// 只改 ResolveKey 选中的那一条，另一条等价 key 会顶着旧名带着失效的路径字段留在桶里。
func (b AssetBucket) RekeyEquivalent(oldName, newName string) (*AssetEntry, bool) {
	if _, ok := b.ResolveKey(oldName); !ok {
		return nil, false
	}
	target := NormalizeAssetName(oldName)
	rekeyed := AssetBucket{}
	var winner *AssetEntry
	for key, value := range b {
		if NormalizeAssetName(key) == target {
			rekeyed[newName] = value
			winner = value
		} else {
			rekeyed[key] = value
		}
	}
	for k := range b {
		delete(b, k)
	}
	for k, v := range rekeyed {
		b[k] = v
	}
	return winner, true
}

// --- 角色关系图 ---

// RenameRelationReferences 把关系图里指向 oldName 的节点身份改成 newName。
//
// 关系边与角色资产共享名称标识：改名若不同步这里，画布会在改名成功后留下悬空边。
func RenameRelationReferences(relations map[string]any, oldName, newName string) map[string]any {
	if relations == nil {
		return nil
	}
	renamed := func(name string) string {
		if NormalizeAssetName(name) == NormalizeAssetName(oldName) {
			return newName
		}
		return name
	}
	for _, key := range []string{"edges", "suppressed_pairs"} {
		if list, ok := relations[key].([]any); ok {
			for _, item := range list {
				edge, ok := item.(map[string]any)
				if !ok {
					continue
				}
				for _, side := range []string{"source", "target"} {
					if s, ok := edge[side].(string); ok {
						edge[side] = renamed(s)
					}
				}
			}
		}
	}
	if positions, ok := relations["node_positions"].(map[string]any); ok {
		rekeyed := map[string]any{}
		for name, pos := range positions {
			rekeyed[renamed(name)] = pos
		}
		for k := range positions {
			delete(positions, k)
		}
		for k, v := range rekeyed {
			positions[k] = v
		}
	}
	return relations
}

// RemoveRelationReferences 删除角色时一并移除其全部入边、出边、抑制记录与节点坐标，
// 避免关系图保留无法渲染的孤儿节点。
func RemoveRelationReferences(relations map[string]any, name string) map[string]any {
	if relations == nil {
		return nil
	}
	key := NormalizeAssetName(name)
	for _, field := range []string{"edges", "suppressed_pairs"} {
		list, ok := relations[field].([]any)
		if !ok {
			continue
		}
		kept := list[:0]
		for _, item := range list {
			edge, ok := item.(map[string]any)
			if !ok {
				kept = append(kept, item)
				continue
			}
			source, _ := edge["source"].(string)
			target, _ := edge["target"].(string)
			if NormalizeAssetName(source) == key || NormalizeAssetName(target) == key {
				continue
			}
			kept = append(kept, item)
		}
		relations[field] = kept
	}
	if positions, ok := relations["node_positions"].(map[string]any); ok {
		for nodeName := range positions {
			if NormalizeAssetName(nodeName) == key {
				delete(positions, nodeName)
			}
		}
	}
	return relations
}

// --- 小工具 ---

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func cloneMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneMap(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = cloneValue(item)
		}
		return out
	case AssetBucket:
		out := AssetBucket{}
		for k, entry := range t {
			out[k] = entry.Clone()
		}
		return out
	default:
		return v
	}
}

// IsASCIIWordChar 判定 @mention 左邻是否为 ASCII 单词字符（命中则视为邮箱/id 片段而非引用）。
func IsASCIIWordChar(r rune) bool {
	return r == '_' || (r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)))
}
