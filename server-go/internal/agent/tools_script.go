package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/domain"
)

// 剧集剧本编辑工具（对应 server/agent_runtime/sdk_tools/patch_script.py 与 patch_episode_meta.py）。
//
// 把 agent 对 scripts/*.json 的编辑收归这组工具：批量字段编辑 + 结构性增删拆 + 顶层元数据。
// 所有写入都经 service.ProjectStore.UpdateScript，因此自动继承剧本锁、结构「不更坏」守卫、
// metadata 重算与 project.json 集元数据同步。

// ScriptStore 是剧本锁内读-改-写能力（*service.ProjectStore 满足）。
type ScriptStore interface {
	UpdateScript(project, scriptFilename string, mutate func(script map[string]any) error) error
}

// WithScriptEditing 注入剧本编辑工具的项目依赖；未注入时不注册这组工具。
func WithScriptEditing(scripts ScriptStore) RuntimeOption {
	return func(r *Runtime) {
		r.scriptStore = scripts
	}
}

// scriptRegenTriggerFields 是改了之后需紧接着重新生成图/视频的字段前缀（工具不自动作废旧资产）。
var scriptRegenTriggerFields = []string{"image_prompt", "video_prompt"}

// --- patch_episode_script ---

type patchEpisodeScriptIn struct {
	Script string                    `json:"script" jsonschema:"description=剧本文件名（纯文件名，如 episode_1.json）；单集单文件，多集编辑每集一次调用"`
	Edits  map[string]map[string]any `json:"edits" jsonschema:"description={ 分镜id: { 字段路径: 新值 } } 映射；至少一个分镜、每个分镜至少一个字段"`
}

// newPatchEpisodeScriptTool 构造 patch_episode_script：一次调用改多分镜 × 多字段，全批原子。
func newPatchEpisodeScriptTool(project string, scripts ScriptStore) (tool.InvokableTool, error) {
	return toolutils.InferTool("patch_episode_script",
		"批量编辑一个剧本文件里多个分镜的多个字段：传 script（单文件名）+ edits 映射，形如 {分镜id: {字段路径: 新值}}。"+
			"分镜 id 由数据形状判别（segment_id/scene_id/unit_id/shot_id）。字段支持点分嵌套路径（如 image_prompt.scene、duration_seconds）。"+
			"all-or-nothing 原子：任一编辑非法（id 未命中 / 字段路径不存在 / 改 generated_assets 或分镜 id）→ 整批零落盘。"+
			"纯字段 setter，不触碰已生成资产——批量改了 image_prompt / video_prompt 后须紧接着重新生成对应分镜的图/视频。"+
			"叶子字段不存在会被创建（允许补 LLM 漏写的 optional 字段）。",
		func(ctx context.Context, in patchEpisodeScriptIn) (string, error) {
			if err := validateScriptFilename(in.Script); err != nil {
				return denyEnvelope(err.Error()), nil
			}
			if len(in.Edits) == 0 {
				return denyEnvelope("edits 必须是非空 { 分镜id: { 字段路径: 值 } } 映射"), nil
			}
			if scripts == nil {
				return denyEnvelope("script editing not configured"), nil
			}

			var regenIDs []string
			var lines []string
			err := scripts.UpdateScript(project, in.Script, func(script map[string]any) error {
				// 分镜 id 排序遍历，让报错与汇总文本稳定可复现。
				ids := sortedKeys(in.Edits)
				for _, id := range ids {
					fieldMap := in.Edits[id]
					if len(fieldMap) == 0 {
						return fmt.Errorf("分镜 %s 的编辑必须是非空 { 字段路径: 值 } 映射", id)
					}
					fields := sortedKeys(fieldMap)
					for _, field := range fields {
						if err := domain.PatchScriptField(script, id, field, fieldMap[field]); err != nil {
							return fmt.Errorf("分镜 %s 的字段 %s：%w", id, field, err)
						}
						if isRegenTriggerField(field) && !contains(regenIDs, id) {
							regenIDs = append(regenIDs, id)
						}
					}
					lines = append(lines, fmt.Sprintf("  %s: %s", id, strings.Join(fields, ", ")))
				}
				return nil
			})
			if err != nil {
				return denyEnvelope(err.Error()), nil
			}

			out := []string{fmt.Sprintf("✅ 已更新 %d 个分镜的字段：", len(lines))}
			out = append(out, lines...)
			if len(regenIDs) > 0 {
				out = append(out, fmt.Sprintf(
					"⚠️  改了 image_prompt / video_prompt 的分镜（%s）须紧接着重新生成对应图/视频，否则会留下「新 prompt + 旧画面」的陈旧。",
					strings.Join(regenIDs, ", ")))
			}
			return okEnvelope(strings.Join(out, "\n")), nil
		})
}

func isRegenTriggerField(fieldPath string) bool {
	head := fieldPath
	if idx := strings.Index(fieldPath, "."); idx >= 0 {
		head = fieldPath[:idx]
	}
	return contains(scriptRegenTriggerFields, head)
}

// --- insert_segment ---

type insertSegmentIn struct {
	Script  string         `json:"script" jsonschema:"description=剧本文件名（纯文件名）"`
	AfterID string         `json:"after_id" jsonschema:"description=在此分镜 id 之后插入"`
	Item    map[string]any `json:"item" jsonschema:"description=新分镜的完整内容对象（id 由系统分配，generated_assets 会清空）"`
}

// newInsertSegmentTool 构造 insert_segment。
func newInsertSegmentTool(project string, scripts ScriptStore) (tool.InvokableTool, error) {
	return toolutils.InferTool("insert_segment",
		"在指定分镜 id 之后插入一个新分镜（segment/scene/unit）。新分镜由你提供完整内容，其 id 由系统分配"+
			"（派生自锚点 id 的稳定后缀，不重排其余分镜），资产为空待生成。reference 模式插入的是 video_unit（含 shots）。",
		func(ctx context.Context, in insertSegmentIn) (string, error) {
			if err := validateScriptFilename(in.Script); err != nil {
				return denyEnvelope(err.Error()), nil
			}
			if scripts == nil {
				return denyEnvelope("script editing not configured"), nil
			}
			var ids []string
			var idsErr error
			err := scripts.UpdateScript(project, in.Script, func(script map[string]any) error {
				if err := domain.InsertScriptSegment(script, in.AfterID, in.Item); err != nil {
					return err
				}
				ids, idsErr = domain.ScriptItemIDs(script)
				return idsErr
			})
			if err != nil {
				return denyEnvelope(err.Error()), nil
			}
			return okEnvelope(fmt.Sprintf("✅ 已在 %s 之后插入新分镜\n当前分镜顺序: %s",
				in.AfterID, strings.Join(ids, ", "))), nil
		})
}

// --- remove_segment ---

type removeSegmentIn struct {
	Script string `json:"script" jsonschema:"description=剧本文件名（纯文件名）"`
	ID     string `json:"id" jsonschema:"description=要删除的分镜 id"`
}

// newRemoveSegmentTool 构造 remove_segment。
func newRemoveSegmentTool(project string, scripts ScriptStore) (tool.InvokableTool, error) {
	return toolutils.InferTool("remove_segment",
		"按 id 删除一个分镜（segment/scene/unit）。其余分镜的 id 不变、不重排，被删分镜的已生成资产随之失效。"+
			"reference 模式删除的是 video_unit。",
		func(ctx context.Context, in removeSegmentIn) (string, error) {
			if err := validateScriptFilename(in.Script); err != nil {
				return denyEnvelope(err.Error()), nil
			}
			if scripts == nil {
				return denyEnvelope("script editing not configured"), nil
			}
			var ids []string
			var idsErr error
			err := scripts.UpdateScript(project, in.Script, func(script map[string]any) error {
				if err := domain.RemoveScriptSegment(script, in.ID); err != nil {
					return err
				}
				ids, idsErr = domain.ScriptItemIDs(script)
				return idsErr
			})
			if err != nil {
				return denyEnvelope(err.Error()), nil
			}
			return okEnvelope(fmt.Sprintf("✅ 已删除分镜 %s\n当前分镜顺序: %s", in.ID, strings.Join(ids, ", "))), nil
		})
}

// --- split_segment ---

type splitSegmentIn struct {
	Script string           `json:"script" jsonschema:"description=剧本文件名（纯文件名）"`
	ID     string           `json:"id" jsonschema:"description=要拆分的分镜 id"`
	Parts  []map[string]any `json:"parts" jsonschema:"description=拆分后各部分的完整内容对象（≥2 个，id 由系统分配）"`
}

// newSplitSegmentTool 构造 split_segment。
func newSplitSegmentTool(project string, scripts ScriptStore) (tool.InvokableTool, error) {
	return toolutils.InferTool("split_segment",
		"把一个分镜按你提供的各部分内容拆成多个（≥2 份）。首份保留原 id 且 generated_assets 不动（锚点延续）；"+
			"其余分配稳定的派生 id 且 generated_assets 清空，需重新生成。只想微调原分镜内容请用 patch_episode_script。"+
			"reference 模式下 unit 的 duration_seconds 是独立字段，拆分后每份都要给出符合模型档位的值。",
		func(ctx context.Context, in splitSegmentIn) (string, error) {
			if err := validateScriptFilename(in.Script); err != nil {
				return denyEnvelope(err.Error()), nil
			}
			if scripts == nil {
				return denyEnvelope("script editing not configured"), nil
			}
			var ids []string
			var idsErr error
			err := scripts.UpdateScript(project, in.Script, func(script map[string]any) error {
				if err := domain.SplitScriptSegment(script, in.ID, in.Parts); err != nil {
					return err
				}
				ids, idsErr = domain.ScriptItemIDs(script)
				return idsErr
			})
			if err != nil {
				return denyEnvelope(err.Error()), nil
			}
			return okEnvelope(fmt.Sprintf("✅ 已把分镜 %s 拆为 %d 份\n当前分镜顺序: %s",
				in.ID, len(in.Parts), strings.Join(ids, ", "))), nil
		})
}

// --- patch_episode_meta ---

// scriptMetaWhitelist 是 agent 可编辑的剧本顶层字段白名单。新增字段时在此追加并补值校验。
var scriptMetaWhitelist = []string{"title"}

type patchEpisodeMetaIn struct {
	Script string `json:"script" jsonschema:"description=剧本文件名（纯文件名，如 episode_1.json）"`
	Field  string `json:"field" jsonschema:"description=要编辑的顶层字段名，必须在白名单内 title"`
	Value  string `json:"value" jsonschema:"description=新值（title 为非空字符串）"`
}

// newPatchEpisodeMetaTool 构造 patch_episode_meta：改剧本顶层元数据并镜像到 project.json 分集列表。
func newPatchEpisodeMetaTool(project string, scripts ScriptStore) (tool.InvokableTool, error) {
	return toolutils.InferTool("patch_episode_meta",
		"编辑剧本的顶层元数据字段（非分镜级）。本期仅支持 field=title 改分集标题——分集标题以剧本顶层 title 为唯一真相源，"+
			"改后自动镜像到 project.json 供 WebUI 分集列表显示。改某个分镜内部字段请用 patch_episode_script。",
		func(ctx context.Context, in patchEpisodeMetaIn) (string, error) {
			if err := validateScriptFilename(in.Script); err != nil {
				return denyEnvelope(err.Error()), nil
			}
			if !contains(scriptMetaWhitelist, in.Field) {
				return denyEnvelope(fmt.Sprintf("field %q 不在白名单 %v 内", in.Field, scriptMetaWhitelist)), nil
			}
			if strings.TrimSpace(in.Value) == "" {
				return denyEnvelope(fmt.Sprintf("%s 必须是非空字符串", in.Field)), nil
			}
			if scripts == nil {
				return denyEnvelope("script editing not configured"), nil
			}
			newValue := strings.TrimSpace(in.Value)
			err := scripts.UpdateScript(project, in.Script, func(script map[string]any) error {
				script[in.Field] = newValue
				return nil
			})
			if err != nil {
				return denyEnvelope(err.Error()), nil
			}
			return okEnvelope(fmt.Sprintf("✅ 已更新分集%s为「%s」", in.Field, newValue)), nil
		})
}
