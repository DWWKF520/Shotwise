// 生成前资产引用校验。对应 lib/generation_preflight.py。
//
// generate_storyboards 在入队前先跑一遍：任何一个待入队分镜引用了「未登记」或
// 「已登记但无可用设计图」的角色/场景/道具，都阻断整个批次。不可先入队前面的图片、
// 再发现后续资产缺失——那会留下半批已生成、半批失败的不一致状态。
//
// 只覆盖三类待检查资产（character / scene / prop）；产品与角色衍生形态不在
// generate_storyboards 的关注范围内。
package service

import (
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// PreflightIssue 的问题种类。判定规则：
//   - Unregistered：名字（NFC 归一后）在对应资产桶里解析不到落盘 key。
//   - MissingSheet：解析到了 key，但该条目的设计图字段为空，或指向的路径越界/文件不存在。
const (
	// PreflightKindUnregistered 引用的资产名未在 project.json 对应桶中登记。
	PreflightKindUnregistered = "unregistered"
	// PreflightKindMissingSheet 资产已登记但其设计图缺失或不可用。
	PreflightKindMissingSheet = "missing_sheet"
)

// PreflightIssue 是一条引用问题：字段名、资产名、问题种类与资产类型。
type PreflightIssue struct {
	Field     string           // characters_in_segment / scenes / props 等
	AssetName string           // NFC 归一后的资产名
	Kind      string           // PreflightKindUnregistered 或 PreflightKindMissingSheet
	AssetType domain.AssetKind // character / scene / prop
}

// PreflightResult 是一次引用校验的结果（可含多条问题）。
type PreflightResult struct {
	Issues []PreflightIssue
}

// ItemReference 是从分镜条目里提取出的一条类型化引用。
type ItemReference struct {
	Kind  domain.AssetKind
	Field string
	Name  string
}

// ValidateItemAssetReferences 校验一个分镜条目引用的三类资产（对应 Python 同名函数）。
//
// fields 给出三类资产在条目里的字段名；charField 为空表示该条目没有角色引用字段。
// 条目中非列表值、非字符串元素、空白名一律跳过，与 Python collect_item_references 同语义。
func ValidateItemAssetReferences(project *domain.ProjectFile, projectDir string, item map[string]any,
	fields domain.StoryboardItemFields) PreflightResult {
	return ValidateAssetReferences(project, projectDir, collectItemReferences(item, fields))
}

// MergePreflightResults 合并多次校验结果（对应 Python merge_preflight_results）。
//
// 保留首次出现顺序，并按 (种类, 类型, 名) 去重：同一资产在多个分镜里被重复引用时
// 只报告一次。
func MergePreflightResults(results ...PreflightResult) PreflightResult {
	var merged PreflightResult
	seen := map[string]bool{}
	for _, result := range results {
		for _, issue := range result.Issues {
			key := issueKey(issue)
			if seen[key] {
				continue
			}
			seen[key] = true
			merged.Issues = append(merged.Issues, issue)
		}
	}
	return merged
}

// I18NParams 把校验结果展开成 generation_asset_preflight_failed 的渲染参数。
//
// 三类问题分别逗号拼接、空类别用「—」占位（与 Python 侧 _raise_asset_preflight 同形）。
// Go 侧尚无「缺少角色衍生形态」这一类，missing_variants 恒为占位符。
func (r PreflightResult) I18NParams() map[string]any {
	var unregistered, missingSheet []string
	for _, issue := range r.Issues {
		label := issue.AssetName
		switch issue.Kind {
		case PreflightKindUnregistered:
			unregistered = append(unregistered, label)
		case PreflightKindMissingSheet:
			missingSheet = append(missingSheet, label)
		}
	}
	return map[string]any{
		"unregistered":     joinLabelsOrDash(unregistered),
		"missing_images":   joinLabelsOrDash(missingSheet),
		"missing_variants": "—",
	}
}

// FormatPreflightError 把校验结果渲染成给模型看的错误文案；无问题时返回空串。
//
// 两类问题分别以「种类:名」条目罗列，空类别用「—」占位，与 Python format_preflight_error
// 的排版一致；Go 侧不含角色衍生形态一类，故只渲染两段。
func FormatPreflightError(result PreflightResult) string {
	if len(result.Issues) == 0 {
		return ""
	}
	var unregistered, missingSheet []string
	for _, issue := range result.Issues {
		label := string(issue.AssetType) + ":" + issue.AssetName
		switch issue.Kind {
		case PreflightKindUnregistered:
			unregistered = append(unregistered, label)
		case PreflightKindMissingSheet:
			missingSheet = append(missingSheet, label)
		}
	}
	return "生成前检查失败。未登记资产：" + joinLabelsOrDash(unregistered) +
		"；缺少设计图：" + joinLabelsOrDash(missingSheet)
}

// collectItemReferences 从分镜条目里按资产类型收集引用名，按 (类型, 名) 去重。
func collectItemReferences(item map[string]any, fields domain.StoryboardItemFields) []ItemReference {
	pairs := []struct {
		kind  domain.AssetKind
		field string
	}{
		{domain.AssetCharacter, fields.CharField},
		{domain.AssetScene, fields.SceneField},
		{domain.AssetProp, fields.PropField},
	}
	var references []ItemReference
	seen := map[string]bool{}
	for _, pair := range pairs {
		if pair.field == "" {
			continue
		}
		names := StringList(item[pair.field])
		for _, raw := range names {
			if strings.TrimSpace(raw) == "" {
				continue
			}
			name := domain.NormalizeAssetName(raw)
			key := string(pair.kind) + "\x00" + name
			if seen[key] {
				continue
			}
			seen[key] = true
			references = append(references, ItemReference{Kind: pair.kind, Field: pair.field, Name: name})
		}
	}
	return references
}

// ValidateAssetReferences 逐条检查「是否登记」与「设计图是否可用」，两条问题各自去重。
func ValidateAssetReferences(project *domain.ProjectFile, projectDir string, references []ItemReference) PreflightResult {
	var result PreflightResult
	seen := map[string]bool{}
	for _, ref := range references {
		spec, ok := domain.Spec(ref.Kind)
		if !ok || strings.TrimSpace(ref.Name) == "" {
			continue
		}
		dedup := string(ref.Kind) + "\x00" + ref.Name

		bucket, bucketOK := bucketRef(project, ref.Kind)
		key, found := "", false
		if bucketOK && bucket != nil {
			key, found = bucket.ResolveKey(ref.Name)
		}
		if !found {
			if !seen[dedup] {
				seen[dedup] = true
				result.Issues = append(result.Issues, PreflightIssue{
					Field: ref.Field, AssetName: ref.Name,
					Kind: PreflightKindUnregistered, AssetType: ref.Kind,
				})
			}
			continue
		}

		entry := (*bucket)[key]
		sheet := entry.MustString(spec.SheetKey)
		if sheet == "" || !fs.SafeExists(projectDir, sheet) {
			if !seen[dedup] {
				seen[dedup] = true
				result.Issues = append(result.Issues, PreflightIssue{
					Field: ref.Field, AssetName: ref.Name,
					Kind: PreflightKindMissingSheet, AssetType: ref.Kind,
				})
			}
		}
	}
	return result
}

// bucketRef 取资产桶；project 为 nil 时按空桶处理，避免 nil 解引用。
func bucketRef(project *domain.ProjectFile, kind domain.AssetKind) (*domain.AssetBucket, bool) {
	if project == nil {
		return nil, false
	}
	return project.BucketRef(kind)
}

// StringList 把条目字段读成字符串列表；非列表或含非字符串元素时按空处理，
// 与 Python 侧对畸形数据的跳过语义一致。
func StringList(value any) []string {
	switch v := value.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, element := range v {
			if s, ok := element.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	default:
		return nil
	}
}

// issueKey 是合并去重的身份键：同种类、同类型、同名视为同一条问题。
func issueKey(issue PreflightIssue) string {
	return issue.Kind + "\x00" + string(issue.AssetType) + "\x00" + issue.AssetName
}

// joinLabelsOrDash 用「, 」连接标签；空列表返回占位符「—」。
func joinLabelsOrDash(labels []string) string {
	if len(labels) == 0 {
		return "—"
	}
	return strings.Join(labels, ", ")
}
