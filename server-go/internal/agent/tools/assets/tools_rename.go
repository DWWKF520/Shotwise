package assets

import "github.com/shotwise/shotwise/internal/agent/tools"

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service/asset"
)

// rename_asset 工具（对应 server/agent_runtime/sdk_tools/rename_asset.py）。
//
// 与 patch_project 的分工：后者的章程限定 project.json 字段写入，而重命名是跨全部剧集剧本
// + step1 草稿 + 文件系统的级联事务（ADR 0057），经 AssetService.Rename 在项目锁内一次完成。
// 改名不影响全局资产库。

// AssetRenamer 是级联改名能力（*asset.AssetService 满足）。
type AssetRenamer interface {
	Rename(project string, kind domain.AssetKind, oldName, newName string, dryRun bool) (asset.RenameReport, error)
}

type renameAssetIn struct {
	Table   string `json:"table" jsonschema:"description=资产表：characters/scenes/props/products"`
	OldName string `json:"old_name" jsonschema:"description=现有资产名"`
	NewName string `json:"new_name" jsonschema:"description=新资产名"`
}

// NewRenameAssetTool 构造 rename_asset。
func NewRenameAssetTool(project string, renamer AssetRenamer) (tool.InvokableTool, error) {
	return toolutils.InferTool("rename_asset",
		"级联重命名资产：一次改齐资产表 key、全部剧集剧本与 step1 草稿的名称引用"+
			"（引用数组 / speaker / @[名称] mention）以及按名命名的关联文件与版本历史。"+
			"新名与既有同类型资产冲突时整体拒绝、不落盘。改名不影响全局资产库。"+
			"用 patch_project 改名等于「新名新建 + 旧名残留」，改名一律用本工具。",
		func(ctx context.Context, in renameAssetIn) (string, error) {
			kind, ok := tools.PatchTableKinds[in.Table]
			if !ok {
				return tools.DenyEnvelope(fmt.Sprintf("table 必须是 characters/scenes/props/products 之一，收到 %q", in.Table)), nil
			}
			if renamer == nil {
				return tools.DenyEnvelope("asset rename not configured"), nil
			}
			report, err := renamer.Rename(project, kind, in.OldName, in.NewName, false)
			if err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			return tools.OKEnvelope(fmt.Sprintf(
				"已把 %s 资产 %s 重命名为 %s：更新 %d 集共 %d 处引用，迁移 %d 个关联文件。",
				report.Table, report.OldName, report.NewName,
				report.Episodes, report.References, report.Files)), nil
		})
}
