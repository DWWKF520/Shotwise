package assets

import "github.com/shotwise/shotwise/internal/agent/tools"

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	domainproject "github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/repository"
)

// assetTypeOrder 是 type 省略时的处理顺序（与 Python ALL_TYPES 一致）。
var assetTypeOrder = []domainproject.AssetKind{domainproject.AssetCharacter, domainproject.AssetScene, domainproject.AssetProp, domainproject.AssetProduct}

// assetLabels 是面向模型的中文类型名（agent 侧字符串，不走 i18n）。
var assetLabels = map[domainproject.AssetKind]string{
	domainproject.AssetCharacter: "角色", domainproject.AssetScene: "场景", domainproject.AssetProp: "道具", domainproject.AssetProduct: "产品",
}

type generateAssetsIn struct {
	Type       string   `json:"type,omitempty" jsonschema:"description=资产类型 character/scene/prop/product；省略则依次处理全部类型"`
	Names      []string `json:"names,omitempty" jsonschema:"description=目标资产名称列表；必须配合 type 使用"`
	All        bool     `json:"all,omitempty" jsonschema:"description=处理该类型全部待生成资产（与 names 互斥）"`
	Regenerate bool     `json:"regenerate,omitempty" jsonschema:"description=强制重新生成已有设计图"`
}

// assetSpec 是待入队的单个资产任务。
type assetSpec struct {
	name   string
	prompt string
}

// NewGenerateAssetsTool 构造 generate_assets 工具，project 名闭包绑定。
func NewGenerateAssetsTool(project string, q tools.AssetQueue, projects tools.AssetProjects) (tool.InvokableTool, error) {
	return toolutils.InferTool("generate_assets",
		"批量生成角色/场景/道具/产品设计图；等待每个任务完成后汇总结果。type 省略则按 角色→场景→道具→产品 依次处理；names 指定具体名称（必须同时给 type）；all=true 为该类型全部待生成资产。",
		func(ctx context.Context, in generateAssetsIn) (string, error) {
			if len(in.Names) > 0 && in.Type == "" {
				return tools.DenyEnvelope("names 必须配合 type 使用"), nil
			}
			if len(in.Names) > 0 && in.All {
				return tools.DenyEnvelope("all 与 names 互斥，不能同时使用"), nil
			}
			kinds := assetTypeOrder
			if in.Type != "" {
				kind := domainproject.AssetKind(in.Type)
				if _, ok := assetLabels[kind]; !ok {
					return tools.DenyEnvelope(fmt.Sprintf("type 必须是 character/scene/prop/product 之一，收到 %q", in.Type)), nil
				}
				kinds = []domainproject.AssetKind{kind}
			}
			if q == nil || projects == nil {
				return tools.DenyEnvelope("asset generation not configured"), nil
			}
			names := tools.DedupStrings(in.Names)
			text, failed, err := runGenerateAssets(ctx, project, kinds, names, in.Regenerate, q, projects)
			if err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			if failed > 0 {
				return tools.DenyEnvelope(text), nil
			}
			return tools.OKEnvelope(text), nil
		})
}

// runGenerateAssets 对每个类型：构造待入队 spec → 全部入队 → 逐个等待终态 → 汇总。
// 返回汇总文本与失败数量。
func runGenerateAssets(ctx context.Context, project string, kinds []domainproject.AssetKind, names []string,
	regenerate bool, q tools.AssetQueue, projects tools.AssetProjects) (string, int, error) {
	pf, err := projects.Get(project)
	if err != nil {
		return "", 0, err
	}
	dir, err := projects.Dir(project)
	if err != nil {
		return "", 0, err
	}

	var warnings, details []string
	var succeeded, failed int
	for _, kind := range kinds {
		specs := buildAssetSpecs(pf, dir, kind, names, regenerate, &warnings)
		if len(specs) == 0 {
			continue
		}
		taskIDs := make([]string, 0, len(specs))
		for _, s := range specs {
			res, err := q.EnqueueTask(ctx, repository.EnqueueParams{
				ProjectName: project,
				TaskType:    string(kind),
				MediaType:   "image",
				ResourceID:  s.name,
				Payload:     map[string]any{"prompt": s.prompt},
				Source:      "agent",
			})
			if err != nil {
				return "", 0, fmt.Errorf("enqueue %s %q: %w", assetLabels[kind], s.name, err)
			}
			taskIDs = append(taskIDs, res.TaskID)
		}
		for i, taskID := range taskIDs {
			label := assetLabels[kind]
			outcome, err := tools.AwaitAssetTask(ctx, q, taskID)
			if err != nil {
				return "", 0, err
			}
			if outcome.Status == repository.TaskStatusSucceeded {
				succeeded++
				file := fmt.Sprintf("%s/%s.png", string(kind)+"s", specs[i].name)
				if fp, ok := outcome.Result["file_path"].(string); ok && fp != "" {
					file = fp
				}
				version := ""
				if v, ok := outcome.Result["version"]; ok && v != nil {
					version = fmt.Sprintf(" (v%v)", v)
				}
				details = append(details, fmt.Sprintf("  ✓ %s '%s' → %s%s", label, specs[i].name, file, version))
				continue
			}
			failed++
			msg := ""
			if outcome.ErrorMessage != nil {
				msg = *outcome.ErrorMessage
			}
			if msg == "" {
				msg = outcome.Status
			}
			details = append(details, fmt.Sprintf("  ✗ %s '%s': %s", label, specs[i].name, msg))
		}
	}

	var body []string
	body = append(body, warnings...)
	if succeeded+failed > 0 {
		body = append(body, fmt.Sprintf("generate_assets summary: %d succeeded, %d failed", succeeded, failed))
	} else {
		body = append(body, "✅ 没有需要生成的资产")
	}
	body = append(body, details...)
	return strings.Join(body, "\n"), failed, nil
}

// buildAssetSpecs 复刻 Python _build_specs：names 模式按名解析并校验描述、跳过已有设计图；
// 无 names 时取全部待生成（无 sheet 或 sheet 文件缺失）。
func buildAssetSpecs(pf *domainproject.ProjectFile, dir string, kind domainproject.AssetKind, names []string,
	regenerate bool, warnings *[]string) []assetSpec {
	spec, ok := domainproject.Spec(kind)
	if !ok {
		return nil
	}
	bucket, ok := pf.BucketRef(kind)
	if !ok || bucket == nil {
		return nil
	}
	label := assetLabels[kind]

	var out []assetSpec
	if len(names) > 0 {
		seen := map[string]bool{}
		for _, raw := range names {
			key, found := bucket.ResolveKey(raw)
			if !found {
				*warnings = append(*warnings, fmt.Sprintf("⚠️  %s '%s' 不存在于 project.json 中，跳过", label, raw))
				continue
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			entry := (*bucket)[key]
			desc := entry.Description()
			if strings.TrimSpace(desc) == "" {
				*warnings = append(*warnings, fmt.Sprintf("⚠️  %s '%s' 缺少描述，跳过", label, raw))
				continue
			}
			if !regenerate && hasAvailableSheet(dir, entry, spec.SheetKey) {
				*warnings = append(*warnings, fmt.Sprintf("⚠️  %s '%s' 已有设计图，跳过；如需重生成请传 regenerate=true", label, raw))
				continue
			}
			out = append(out, assetSpec{name: key, prompt: desc})
		}
		return out
	}

	keys := make([]string, 0, len(*bucket))
	for k := range *bucket {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := (*bucket)[key]
		if hasAvailableSheet(dir, entry, spec.SheetKey) {
			continue
		}
		desc := ""
		if entry != nil {
			desc = entry.Description()
		}
		if strings.TrimSpace(desc) == "" {
			*warnings = append(*warnings, fmt.Sprintf("⚠️  %s '%s' 缺少描述，跳过", label, key))
			continue
		}
		out = append(out, assetSpec{name: key, prompt: desc})
	}
	return out
}

// hasAvailableSheet 判定条目是否已有可用设计图（字段非空且文件存在于项目内）。
func hasAvailableSheet(dir string, entry *domainproject.AssetEntry, sheetKey string) bool {
	if entry == nil {
		return false
	}
	sheet := entry.MustString(sheetKey)
	if sheet == "" {
		return false
	}
	p, err := fs.SafeResolve(dir, sheet)
	if err != nil {
		return false
	}
	_, statErr := os.Stat(p)
	return statErr == nil
}

// assetEmoji 是 list_pending_assets 输出中的类型图标（与 Python 侧 _EMOJI 一致）。
var assetEmoji = map[domainproject.AssetKind]string{
	domainproject.AssetCharacter: "🧑", domainproject.AssetScene: "🏠", domainproject.AssetProp: "📦", domainproject.AssetProduct: "🛍️",
}

type listPendingAssetsIn struct {
	Type string `json:"type,omitempty" jsonschema:"description=资产类型 character/scene/prop/product；省略则汇总所有类型"`
}

// NewListPendingAssetsTool 列出无可用设计图的资产（只读，对应 Python list_pending_assets）。
func NewListPendingAssetsTool(project string, projects tools.AssetProjects) (tool.InvokableTool, error) {
	return toolutils.InferTool("list_pending_assets",
		"列出项目内待生成设计图的角色/场景/道具/产品。type 省略则汇总所有类型。",
		func(ctx context.Context, in listPendingAssetsIn) (string, error) {
			kinds := assetTypeOrder
			if in.Type != "" {
				kind := domainproject.AssetKind(in.Type)
				if _, ok := assetLabels[kind]; !ok {
					return tools.DenyEnvelope(fmt.Sprintf("type 必须是 character/scene/prop/product 之一，收到 %q", in.Type)), nil
				}
				kinds = []domainproject.AssetKind{kind}
			}
			if projects == nil {
				return tools.DenyEnvelope("asset projects not configured"), nil
			}
			pf, err := projects.Get(project)
			if err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			dir, err := projects.Dir(project)
			if err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			return tools.OKEnvelope(renderPendingAssets(project, kinds, pf, dir)), nil
		})
}

func renderPendingAssets(project string, kinds []domainproject.AssetKind, pf *domainproject.ProjectFile, dir string) string {
	var lines []string
	total := 0
	for _, kind := range kinds {
		label := assetLabels[kind]
		pending := pendingAssetNames(pf, dir, kind)
		if len(pending) == 0 {
			lines = append(lines, fmt.Sprintf("✅ 项目 '%s' 所有%s都已有设计图", project, label))
			continue
		}
		total += len(pending)
		lines = append(lines, fmt.Sprintf("\n📋 待生成的%s (%d 个):", label, len(pending)))
		for _, p := range pending {
			lines = append(lines, fmt.Sprintf("  %s %s — %s", assetEmoji[kind], p.name, previewText(p.desc, 60)))
		}
	}
	if len(kinds) == len(assetTypeOrder) && total == 0 {
		lines = append(lines, fmt.Sprintf("\n✅ 项目 '%s' 所有资产均已有设计图", project))
	}
	return strings.Join(lines, "\n")
}

type pendingAsset struct {
	name string
	desc string
}

// pendingAssetNames 返回指定类型中无可用设计图的条目（按名称排序）。
func pendingAssetNames(pf *domainproject.ProjectFile, dir string, kind domainproject.AssetKind) []pendingAsset {
	spec, ok := domainproject.Spec(kind)
	if !ok {
		return nil
	}
	bucket, ok := pf.BucketRef(kind)
	if !ok || bucket == nil {
		return nil
	}
	keys := make([]string, 0, len(*bucket))
	for k := range *bucket {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []pendingAsset
	for _, key := range keys {
		entry := (*bucket)[key]
		if hasAvailableSheet(dir, entry, spec.SheetKey) {
			continue
		}
		desc := ""
		if entry != nil {
			desc = entry.Description()
		}
		out = append(out, pendingAsset{name: key, desc: desc})
	}
	return out
}

// previewText 按字符（非字节）截断，超出时追加省略号。
func previewText(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}
