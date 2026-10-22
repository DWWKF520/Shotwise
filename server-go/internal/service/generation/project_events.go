// 生成成功事件（对应 server/services/generation_tasks.py 的
// emit_generation_success_batch + compute_affected_fingerprints，以及 lib/project_change_hints.py
// 的批广播语义）。
//
// 把 worker 终态里成功的事件转成项目变更批并广播：前端据 change 的 entity_type/action 弹通知并
// 自动导航，据 change.asset_fingerprints 做 cache-bust。失败的终态不发变更事件——与 Python 侧只
// 在成功时调用 emit_generation_success_batch 一致。
//
// 变更事件文案的 zh/en/vi 三语化尚未做：本轮按 Python 对齐，文案本身就是中文字面量模板
// （label_tpl 里的「分镜」「角色」「宫格」等）。三语化需与 Python 侧同步推进，不在此处引入半套 i18n。
package generation

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/queue"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"gorm.io/gorm"
)

// taskChangeSpec 是「任务类型 → 完成事件」的固定映射项
// （对应 Python _TASK_CHANGE_SPECS 的 tuple：(entity_type, action, label_tpl, include_script_episode)）。
type taskChangeSpec struct {
	entityType           string
	action               string
	labelTemplate        string // 含单个 %s 占位，套入 resource_id
	includeScriptEpisode bool
}

// skeletonDrivenTaskActions 是骨架驱动的任务类型 → 完成事件 action。
//
// entity_type 与条目名词按项目剧本当前骨架种类动态解析（与分镜级事件同一判定），不按 task_type
// 恒定硬编码——避免恒发 segment/「分镜」而与分镜级事件（project_events.py）名词不一致。
var skeletonDrivenTaskActions = map[string]string{
	"storyboard":      "storyboard_ready",
	"video":           "video_ready",
	"reference_video": "reference_video_ready",
}

// skeletonTaskLabelNouns 是骨架驱动任务里覆盖骨架名词的条目文案。
//
// reference_video 沿用「参考视频」措辞（区别于分镜级事件的骨架名词「视频单元」）；storyboard /
// video 未列出，回退到骨架名词本身（分镜/场景/镜头）。
var skeletonTaskLabelNouns = map[string]string{
	"reference_video": "参考视频",
}

// taskChangeSpecs 是任务类型 → 固定变更规格的唯一真相源。
//
// tts / grid / grid_split / voice_sample 在此登记；三类项目级资产（character / scene / prop）与
// product 的规格由 domain.AssetSpecs 派生（action 恒为 updated，label 为「<类型名>「{}」设计图」）。
var taskChangeSpecs = func() map[string]taskChangeSpec {
	specs := map[string]taskChangeSpec{
		"tts":          {"segment", "tts_ready", "旁白「%s」", true},
		"grid":         {"grid", "grid_ready", "宫格「%s」", true},
		"grid_split":   {"grid", "grid_split_done", "宫格「%s」切分", true},
		"voice_sample": {"character", "voice_sample_ready", "「%s」试听样本", false},
	}
	for kind, spec := range domain.AssetSpecs {
		specs[string(kind)] = taskChangeSpec{string(kind), "updated", spec.LabelZh + "「%s」设计图", false}
	}
	return specs
}()



// affectedFingerPath 是待算指纹的一对路径：rel 为项目相对路径（指纹 key），abs 为绝对路径（stat 用）。
type affectedFingerPath struct{ rel, abs string }

// ComputeAffectedFingerprints 计算任务受影响文件的 mtime 指纹（对应 Python compute_affected_fingerprints）。
//
// 返回「项目相对路径 → mtime 纳秒」的映射；文件不存在的不进表。grid / grid_split 额外纳入
// frame_chain 里的 cell 路径并做项目内路径安全校验——记录是磁盘上的 JSON，image_path 不可直接
// 信任（绝对路径或 ../ 会越出项目目录，把任意服务器文件的存在性/mtime 暴露给前端）。
func ComputeAffectedFingerprints(
	store *service.ProjectStore, projectName, taskType, resourceID string,
) map[string]int64 {
	result := map[string]int64{}
	if store == nil {
		return result
	}
	projectDir, err := store.Dir(projectName)
	if err != nil {
		return result
	}

	var paths []affectedFingerPath
	add := func(rel string) {
		paths = append(paths, affectedFingerPath{rel: rel, abs: filepath.Join(projectDir, filepath.FromSlash(rel))})
	}

	switch taskType {
	case "storyboard":
		add("storyboards/scene_" + resourceID + ".png")
	case "video":
		add("videos/scene_" + resourceID + ".mp4")
		add("thumbnails/scene_" + resourceID + ".jpg")
	case "character":
		add("characters/" + resourceID + ".png")
		avatar := ""
		if project, err := store.Get(projectName); err == nil {
			if key, ok := project.Characters.ResolveKey(resourceID); ok {
				avatar = project.Characters[key].MustString("character_avatar")
			}
		}
		if avatar != "" {
			add(avatar)
		} else {
			add("characters/" + resourceID + "_avatar.png")
		}
	case "scene":
		add("scenes/" + resourceID + ".png")
	case "prop":
		add("props/" + resourceID + ".png")
	case "product":
		add("products/" + resourceID + ".png")
	case "grid", "grid_split":
		add("grids/" + resourceID + ".png")
		paths = append(paths, gridCellFingerPaths(projectDir, resourceID)...)
	case "reference_video":
		add("reference_videos/" + resourceID + ".mp4")
		add("reference_videos/thumbnails/" + resourceID + ".jpg")
	case "tts":
		if rel, err := service.ResourceRelativePath("audio", resourceID); err == nil {
			add(rel)
		}
	}

	for _, p := range paths {
		if st, err := os.Stat(p.abs); err == nil {
			result[p.rel] = st.ModTime().UnixNano()
		}
	}
	return result
}

// gridCellFingerPaths 取宫格 frame_chain 里 cell 的项目内相对路径（越界 / 不存在 / 脏数据一概跳过）。
func gridCellFingerPaths(projectDir, gridID string) []affectedFingerPath {
	manager, err := service.NewGridManager(projectDir)
	if err != nil {
		return nil
	}
	grid, ok, err := manager.Get(gridID)
	if err != nil || !ok || grid == nil {
		return nil
	}
	var out []affectedFingerPath
	for _, frame := range grid.FrameChain {
		if frame.ImagePath == nil || *frame.ImagePath == "" {
			continue
		}
		raw := *frame.ImagePath
		// 记录是磁盘上的 JSON：绝对路径会覆盖左操作数、../ 会越出项目目录，把任意服务器文件的
		// 存在性/mtime 暴露给前端。绝对路径显式拒绝（Go 的 SafeJoin 拼接语义不会丢弃前缀），
		// 越界由 SafeJoin 的包含校验兜住。
		if filepath.IsAbs(raw) {
			slog.Warn("跳过绝对路径的宫格 cell", "path", raw)
			continue
		}
		candidate := fs.TrySafeJoin(projectDir, false, filepath.FromSlash(raw))
		if candidate == "" {
			slog.Warn("跳过越出项目目录的宫格 cell 路径", "path", raw)
			continue
		}
		rel, err := filepath.Rel(projectDir, candidate)
		if err != nil {
			continue
		}
		// 指纹 key 用归一化后的项目相对路径（posix 正斜杠）：原始字符串若是项目内的绝对路径，
		// 会把服务器路径泄漏给前端且匹配不上前端的资源 key。
		out = append(out, affectedFingerPath{rel: filepath.ToSlash(rel), abs: candidate})
	}
	return out
}

// NewTerminalEmitter 构造 worker 终态事件发射器。
//
// 只把 status == succeeded 的事件转成变更批广播；失败 / 取消的终态不发（与 Python 只在成功时
// 调用 emit_generation_success_batch 一致）。终态事件本身不带 resource_id / payload，故按 TaskID
// 回读一次任务投影；回读失败只记日志并跳过该事件，绝不阻断其余事件的广播。
//
// batch_id / fingerprint / generated_at 的产生口径：
//   - batch_id：32 位十六进制（与 Python uuid4().hex 同形，复用 repository.NewTaskID）；
//   - fingerprint：project.json 的 mtime（service.ProjectFingerprint，口径与项目事件流首帧一致）；
//   - generated_at：UTC RFC3339（与 Python _utc_now_iso 的 "%Y-%m-%dT%H:%M:%SZ" 同形）。
func NewTerminalEmitter(hub *service.EventHub, store *service.ProjectStore, db *gorm.DB) queue.TerminalEmitter {
	repo := repository.NewTaskRepo(db)
	return func(events []repository.TerminalEvent) {
		for _, event := range events {
			if event.Status != repository.TaskStatusSucceeded {
				continue
			}
			task, err := repo.Get(context.Background(), event.TaskID)
			if err != nil || task == nil {
				slog.Warn("读取终态任务失败，跳过变更事件", "task_id", event.TaskID, "err", err)
				continue
			}
			EmitGenerationSuccessBatch(hub, store, task)
		}
	}
}

// EmitGenerationSuccessBatch 把一次成功的生成 / 上传转成一条项目变更并广播，返回受影响文件指纹
// （调用方可直接复用，免二次计算）。对应 Python emit_generation_success_batch。
//
// image_edit 完成事件按 payload.resource_type 派发到既有 spec 表（storyboard 走骨架驱动、四类资产
// 走 ASSET_SPECS 派生表），entity / action / 指纹与生成路径一致，前端既有的 SSE 指纹刷新零改动
// 即可覆盖编辑完成。认不出的 task_type 不发事件（返回空指纹表）。
func EmitGenerationSuccessBatch(hub *service.EventHub, store *service.ProjectStore, task *repository.TaskData) map[string]int64 {
	if task == nil {
		return map[string]int64{}
	}
	taskType := task.TaskType
	if taskType == "image_edit" {
		taskType = payloadString(task.Payload, "resource_type")
	}
	scriptFile := payloadString(task.Payload, "script_file")
	// 单次加载剧本，骨架种类与 episode 共用；缺失 / 损坏时 script 为 nil，各消费点各自兜底。
	var script map[string]any
	if store != nil && scriptFile != "" {
		if loaded, err := store.ReadScriptJSON(task.ProjectName, scriptFile); err == nil {
			script = loaded
		}
	}

	var change service.Change
	var includeScriptEpisode bool
	if action, ok := skeletonDrivenTaskActions[taskType]; ok {
		kind := "video_units"
		if taskType != "reference_video" {
			if script != nil {
				kind = domain.ResolveScriptKind(script)
			} else {
				kind = "segments"
			}
		}
		entityType := domain.ScriptSkeletonEntityTypes[kind]
		if entityType == "" {
			entityType = "segment"
		}
		noun := skeletonTaskLabelNouns[taskType]
		if noun == "" {
			noun = domain.ScriptSkeletonItemNouns[kind]
		}
		if noun == "" {
			noun = "分镜"
		}
		change = service.Change{
			EntityType: entityType,
			Action:     action,
			EntityID:   task.ResourceID,
			Label:      fmt.Sprintf(noun+"「%s」", task.ResourceID),
			Focus:      nil,
			Important:  true,
		}
		includeScriptEpisode = true
	} else {
		spec, ok := taskChangeSpecs[taskType]
		if !ok {
			return map[string]int64{}
		}
		change = service.Change{
			EntityType: spec.entityType,
			Action:     spec.action,
			EntityID:   task.ResourceID,
			Label:      fmt.Sprintf(spec.labelTemplate, task.ResourceID),
			Focus:      nil,
			Important:  true,
		}
		includeScriptEpisode = spec.includeScriptEpisode
	}

	change.AssetFingerprints = ComputeAffectedFingerprints(store, task.ProjectName, taskType, task.ResourceID)
	if includeScriptEpisode {
		change.ScriptFile = scriptFile
		change.Episode = episodeFromScript(script)
	}

	if hub == nil {
		return change.AssetFingerprints
	}
	projectDir := ""
	if store != nil {
		if dir, err := store.Dir(task.ProjectName); err == nil {
			projectDir = dir
		}
	}
	hub.Publish(service.ChangeBatch{
		ProjectName: task.ProjectName,
		BatchID:     repository.NewTaskID(),
		Fingerprint: service.ProjectFingerprint(projectDir),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Source:      "worker",
		Changes:     []service.Change{change},
	})
	return change.AssetFingerprints
}

// episodeFromScript 读剧本的 episode 字段（对应 Python _episode_from_script：非整数记 nil）。
func episodeFromScript(script map[string]any) *int {
	if script == nil {
		return nil
	}
	switch v := script["episode"].(type) {
	case float64:
		if v == float64(int(v)) {
			n := int(v)
			return &n
		}
	case int:
		n := v
		return &n
	case int64:
		n := int(v)
		return &n
	}
	return nil
}
