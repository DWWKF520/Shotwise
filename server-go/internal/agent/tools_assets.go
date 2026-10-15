package agent

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/repository"
)

// AssetQueue 是 generate_assets 的任务队列能力（*queue.Queue 满足）。
type AssetQueue interface {
	EnqueueTask(ctx context.Context, p repository.EnqueueParams) (repository.EnqueueResult, error)
	GetTask(ctx context.Context, taskID string) (*repository.TaskData, error)
	IsWorkerOnline(ctx context.Context, name string) (bool, error)
}

// AssetProjects 是 generate_assets 读取项目与定位项目目录的能力（*service.ProjectStore 满足）。
type AssetProjects interface {
	Get(name string) (*domain.ProjectFile, error)
	Dir(name string) (string, error)
}

// WithAssetGeneration 注入 generate_assets 的队列与项目依赖；未注入时不注册该工具。
func WithAssetGeneration(q AssetQueue, projects AssetProjects) RuntimeOption {
	return func(r *Runtime) {
		r.assetQueue = q
		r.assetProjects = projects
	}
}

const (
	assetPollInterval = 1 * time.Second
	// assetWorkerOfflineGrace 是等待期间 worker 离线的容忍时长，超过即失败（与 Python WorkerOfflineError 对齐）。
	assetWorkerOfflineGrace = 30 * time.Second
	// assetDefaultLease 是 worker 租约名（与 queue.DefaultLeaseName 一致）。
	assetDefaultLease = "default"
)

// assetTypeOrder 是 type 省略时的处理顺序（与 Python ALL_TYPES 一致）。
var assetTypeOrder = []domain.AssetKind{domain.AssetCharacter, domain.AssetScene, domain.AssetProp, domain.AssetProduct}

// assetLabels 是面向模型的中文类型名（agent 侧字符串，不走 i18n）。
var assetLabels = map[domain.AssetKind]string{
	domain.AssetCharacter: "角色", domain.AssetScene: "场景", domain.AssetProp: "道具", domain.AssetProduct: "产品",
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

// newGenerateAssetsTool 构造 generate_assets 工具，project 名闭包绑定。
func newGenerateAssetsTool(project string, q AssetQueue, projects AssetProjects) (tool.InvokableTool, error) {
	return toolutils.InferTool("generate_assets",
		"批量生成角色/场景/道具/产品设计图；等待每个任务完成后汇总结果。type 省略则按 角色→场景→道具→产品 依次处理；names 指定具体名称（必须同时给 type）；all=true 为该类型全部待生成资产。",
		func(ctx context.Context, in generateAssetsIn) (string, error) {
			if len(in.Names) > 0 && in.Type == "" {
				return denyEnvelope("names 必须配合 type 使用"), nil
			}
			if len(in.Names) > 0 && in.All {
				return denyEnvelope("all 与 names 互斥，不能同时使用"), nil
			}
			kinds := assetTypeOrder
			if in.Type != "" {
				kind := domain.AssetKind(in.Type)
				if _, ok := assetLabels[kind]; !ok {
					return denyEnvelope(fmt.Sprintf("type 必须是 character/scene/prop/product 之一，收到 %q", in.Type)), nil
				}
				kinds = []domain.AssetKind{kind}
			}
			if q == nil || projects == nil {
				return denyEnvelope("asset generation not configured"), nil
			}
			names := dedupStrings(in.Names)
			text, failed, err := runGenerateAssets(ctx, project, kinds, names, in.Regenerate, q, projects)
			if err != nil {
				return denyEnvelope(err.Error()), nil
			}
			if failed > 0 {
				return denyEnvelope(text), nil
			}
			return okEnvelope(text), nil
		})
}

// runGenerateAssets 对每个类型：构造待入队 spec → 全部入队 → 逐个等待终态 → 汇总。
// 返回汇总文本与失败数量。
func runGenerateAssets(ctx context.Context, project string, kinds []domain.AssetKind, names []string,
	regenerate bool, q AssetQueue, projects AssetProjects) (string, int, error) {
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
			outcome, err := awaitAssetTask(ctx, q, taskID)
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
func buildAssetSpecs(pf *domain.ProjectFile, dir string, kind domain.AssetKind, names []string,
	regenerate bool, warnings *[]string) []assetSpec {
	spec, ok := domain.Spec(kind)
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
func hasAvailableSheet(dir string, entry *domain.AssetEntry, sheetKey string) bool {
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

// awaitAssetTask 轮询任务至终态；worker 离线超过宽限期即失败。ctx 取消立即返回。
func awaitAssetTask(ctx context.Context, q AssetQueue, taskID string) (repository.TaskData, error) {
	var offlineSince time.Time
	for {
		task, err := q.GetTask(ctx, taskID)
		if err != nil {
			return repository.TaskData{}, err
		}
		if task == nil {
			return repository.TaskData{}, fmt.Errorf("task not found: %s", taskID)
		}
		if _, terminal := repository.TerminalTaskStatuses[task.Status]; terminal {
			return *task, nil
		}

		online, err := q.IsWorkerOnline(ctx, assetDefaultLease)
		if err != nil {
			return repository.TaskData{}, err
		}
		if online {
			offlineSince = time.Time{}
		} else {
			if offlineSince.IsZero() {
				offlineSince = time.Now()
			} else if time.Since(offlineSince) >= assetWorkerOfflineGrace {
				return repository.TaskData{}, fmt.Errorf("queue worker offline while waiting for task '%s'", taskID)
			}
		}

		select {
		case <-ctx.Done():
			return repository.TaskData{}, ctx.Err()
		case <-time.After(assetPollInterval):
		}
	}
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
