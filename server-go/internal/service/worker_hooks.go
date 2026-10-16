// worker 钩子的服务层适配（对应 lib/generation_worker.py 的注入点）。
//
// worker（internal/queue）只负责调度与终态收口，具体能力经这些钩子注入：中断记账收口、
// 失败结构化编码、瞬态判定、执行身份投影与执行前资产复检。本文件是唯一把 service 层实现
// 接到 worker 端口上的地方——worker 包不反向依赖 service。
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/queue"
	"github.com/shotwise/shotwise/internal/repository"
)

// ----------------------------------------------------------------------
// 中断记账收口
// ----------------------------------------------------------------------

// LedgerSettler 把 *Ledger 的中断收口能力适配成 queue.InterruptedSettler。
//
// Ledger 的同名方法语义与 worker 端口一致（启动收口 + 取消收口），只是返回签名不同：
// Ledger.ResumeFailed 额外回报受影响行数，端口只要求 error。
type LedgerSettler struct{ ledger *Ledger }

// NewLedgerSettler 构造中断记账收口适配器。
func NewLedgerSettler(l *Ledger) *LedgerSettler { return &LedgerSettler{ledger: l} }

// SettleInterruptedCalls 把 startedBefore 之前仍 pending 的记账行翻失败，返回条数。
func (s *LedgerSettler) SettleInterruptedCalls(ctx context.Context, startedBefore time.Time) (int, error) {
	return s.ledger.SettleInterrupted(ctx, startedBefore)
}

// ResumeFailed 按 api_call_id 幂等翻一条 pending 记账行为失败（取消收口，不重扣费）。
func (s *LedgerSettler) ResumeFailed(ctx context.Context, callID int64) error {
	_, err := s.ledger.ResumeFailed(ctx, callID)
	return err
}

// 编译期断言：适配器满足 worker 端口。
var _ queue.InterruptedSettler = (*LedgerSettler)(nil)

// ----------------------------------------------------------------------
// 失败编码
// ----------------------------------------------------------------------

// EncodeTaskFailure 把已知失败码 + 参数编码为落库的 error_message。
//
// 包装 domain.EncodeFailure（落库机器码，读侧按 Accept-Language 渲染）；码未登记或载荷无法
// 编码时退化为 “[code]”。与 Python 侧 encode_failure 的差异仅此一条降级——Python 会抛
// KeyError，但其调用点经 _try_encode_failure 包成 None 再降级，净效果同为本函数形态，且绝不
// 因编码失败把任务卡死在 running。
func EncodeTaskFailure(code string, params map[string]any) string {
	encoded, err := domain.EncodeFailure(code, params)
	if err != nil {
		return "[" + code + "]"
	}
	return encoded
}

// failureCarrier 是「带 code/params 的能力类错误」的统一投影。
type failureCarrier struct {
	code   provider.FailureCode
	params map[string]any
}

// asFailureCarrier 从错误里提取结构化失败坐标（code + params）。
//
// 覆盖 worker 执行路径会抛出的三类：图片/视频后端能力错误（provider.CapabilityError）、
// 视频解析闸错误（config.VideoBucketCapabilityError）、剧本编辑错误（domain.ScriptEditError）。
// 认不出返回 ok=false，交调用方降级。
func asFailureCarrier(err error) (failureCarrier, bool) {
	var capability *provider.CapabilityError
	if errors.As(err, &capability) {
		return failureCarrier{code: capability.Code, params: capability.Params}, true
	}
	var bucketCap *config.VideoBucketCapabilityError
	if errors.As(err, &bucketCap) {
		params := map[string]any{}
		for k, v := range bucketCap.Params() {
			params[k] = v
		}
		return failureCarrier{code: bucketCap.Code, params: params}, true
	}
	var scriptEdit *domain.ScriptEditError
	if errors.As(err, &scriptEdit) {
		return failureCarrier{code: provider.FailureCode(scriptEdit.Key), params: scriptEdit.Params}, true
	}
	return failureCarrier{}, false
}

// EncodeTaskError 把执行异常编码为落库的 error_message。
//
// 能力类与剧本编辑类异常走 code + params 结构化编码（编不出来时退回文本，绝不丢原因）；
// 其余异常沿用文本并裁剪到落库上限。normal（processTask）与 resume（processResumeTask）两条
// 独立的执行路径共用本函数，避免同一处理漂移成两份。
//
// 落库只存机器码，本地化文案由读侧按 Accept-Language 渲染——worker 后台无 request 上下文，
// 在这里渲染会把失败原因锁死成单一语言。
func EncodeTaskError(err error) string {
	if err == nil {
		return ""
	}
	if carrier, ok := asFailureCarrier(err); ok {
		if encoded, encodeErr := domain.EncodeFailure(string(carrier.code), carrier.params); encodeErr == nil {
			return encoded
		}
		return domain.BoundReason(err.Error(), domain.MaxErrorMessageLen)
	}
	return domain.BoundReason(err.Error(), domain.MaxErrorMessageLen)
}

// IsTransientTaskError 判定异常是否属于上游瞬态失败。
//
// 与供应商泳道健康度同源：只有瞬态失败计健康账，其余（能力拒绝、参数非法、用户取消）不折减
// 并发。直接转调 provider.IsTransientUpstreamError，不各写一份词汇表。
func IsTransientTaskError(err error) bool {
	return provider.IsTransientUpstreamError(err)
}

// ----------------------------------------------------------------------
// 执行身份投影
// ----------------------------------------------------------------------

// NewProviderExtractor 构造认领后的 provider 身份投影，仅供限流槽路由。
//
// 取值优先级：
//  1. task 持久化的 ProviderID 列——入队派生 / worker 回队刷新写入的投影值，最贴近实际执行身份；
//  2. payload 里提交时锁定的能力桶身份键（video_provider_<bucket>，见 queue.pinVideoExecutionModel），
//     值为 “provider/model”，取 provider 段；
//  3. 按 task 媒体车道用 config.Resolver 解析（video / audio / image），取 provider_id。
//
// 解析失败返回空串（worker 会回退 DefaultProvider），绝不 panic、绝不冒泡阻断认领循环——队列里
// 可能残留指向已删除 / 不可读项目的任务，claim 过滤不能因此停摆。
func NewProviderExtractor(store *ProjectStore, resolver *config.Resolver) queue.ProviderExtractor {
	return func(ctx context.Context, task *repository.TaskData) (providerID string) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("provider 身份投影 panic，回退默认供应商", "task_id", taskIDOf(task), "panic", r)
				providerID = ""
			}
		}()
		if task == nil {
			return ""
		}
		// 1. 持久化的 provider_id 列。
		if task.ProviderID != nil && *task.ProviderID != "" {
			return *task.ProviderID
		}
		lane := queue.MediaLane(task.MediaType, task.TaskType)
		// 2. 视频任务 payload 里锁定的能力桶身份。
		if lane == provider.MediaVideo {
			if pinned := pinnedVideoProvider(task.Payload); pinned != "" {
				return pinned
			}
		}
		// 3. 按车道用 resolver 解析。
		if resolver == nil {
			return ""
		}
		project, err := loadProjectForExtraction(store, task.ProjectName)
		if err != nil {
			return ""
		}
		resolved, err := resolveProviderForLane(ctx, resolver, project, task)
		if err != nil {
			return ""
		}
		return resolved.ProviderID
	}
}

// loadProjectForExtraction 取项目上下文；无项目名或读取失败时返回 (nil, nil)/(nil, err)。
//
// 返回 nil 项目是合法输入：resolver 的层叠解析在无项目上下文时退到全局层。
func loadProjectForExtraction(store *ProjectStore, projectName string) (*domain.ProjectFile, error) {
	if projectName == "" || store == nil {
		return nil, nil
	}
	project, err := store.Get(projectName)
	if err != nil {
		slog.Debug("身份投影读取项目失败", "project", projectName, "err", err)
		return nil, err
	}
	return project, nil
}

// resolveProviderForLane 按媒体车道解析执行身份。
func resolveProviderForLane(
	ctx context.Context, resolver *config.Resolver, project *domain.ProjectFile, task *repository.TaskData,
) (config.ProviderModel, error) {
	switch queue.MediaLane(task.MediaType, task.TaskType) {
	case provider.MediaVideo:
		if bucket, ok := provider.VideoBucketForTaskType(task.TaskType); ok {
			return resolver.ResolveVideoBackend(ctx, project, task.Payload, &bucket)
		}
		return resolver.ResolveVideoBackend(ctx, project, task.Payload, nil)
	case provider.MediaAudio:
		return resolver.ResolveAudioBackend(ctx, project, task.Payload)
	default:
		// image_edit 必然 i2i 且入队即知；其余 image 任务认领时拿不到真实 capability，
		// 取 t2i 作代表性 provider（执行层会独立精确再解析一次）。
		capability := config.ImageCapT2I
		if task.TaskType == "image_edit" {
			capability = config.ImageCapI2I
		}
		return resolver.ResolveImageBackend(ctx, project, task.Payload, capability)
	}
}

// pinnedVideoProvider 从 payload 的能力桶锁定键取 provider 段（“provider/model” 的前者）。
func pinnedVideoProvider(payload map[string]any) string {
	for _, bucket := range provider.VideoBucketOrder {
		raw, ok := payload["video_provider_"+string(bucket)]
		if !ok {
			continue
		}
		value, ok := raw.(string)
		if !ok || value == "" {
			continue
		}
		if pid, _, found := strings.Cut(value, "/"); found && pid != "" {
			return pid
		}
	}
	return ""
}

// ----------------------------------------------------------------------
// 执行前复检
// ----------------------------------------------------------------------

// NewWorkerPreflight 构造执行前资产复检钩子（对应 lib/generation_worker.py::_preflight_task_assets
// 的 storyboard / video / reference_video 三支）。
//
// 准入校验挡不住「入队到执行之间 project.json / 剧本被编辑」的窗口，故执行前再核对一次：
//   - storyboard / video：校验剧本条目引用的角色/场景/道具是否登记且有可用设计图；
//   - reference_video：校验 unit 引用的资产（ad 按成员镜头现算，其余按 unit 声明的 references）。
//
// 其余 task_type 一律放行（返回 nil）——Python 侧还覆盖 grid，宫格复检随宫格执行路径在 Go 侧
// 迁移时补齐。缺 queue 元数据（项目名 / resource_id / script_file）的历史或测试任务同样放行。
func NewWorkerPreflight(store *ProjectStore) queue.Preflight {
	return func(ctx context.Context, task *repository.TaskData) error {
		if store == nil || task == nil {
			return nil
		}
		switch task.TaskType {
		case "storyboard", "video", "reference_video":
		default:
			return nil
		}
		scriptFile := payloadString(task.Payload, "script_file")
		if scriptFile == "" && task.ScriptFile != nil {
			scriptFile = *task.ScriptFile
		}
		if task.ProjectName == "" || task.ResourceID == "" || scriptFile == "" {
			return nil
		}

		project, err := store.Get(task.ProjectName)
		if err != nil {
			return err
		}
		projectDir, err := store.Dir(task.ProjectName)
		if err != nil {
			return err
		}
		script, err := store.ReadScriptJSON(task.ProjectName, scriptFile)
		if err != nil {
			return err
		}

		if task.TaskType == "reference_video" {
			return preflightReferenceUnit(project, projectDir, script, task.ResourceID)
		}
		return preflightStoryboardItem(project, projectDir, script, task.ResourceID)
	}
}

// preflightStoryboardItem 校验单个剧本条目引用的三类资产。
//
// 用 domain.StoryboardItems 取条目与资产引用字段名（与执行路径同源）；骨架数组键存在但类型非数组
// 的畸形剧本会退化为空条目，进而以「找不到分镜/场景」失败——仍阻断任务，只是文案与 Python 侧的
// ScriptEditError 不同。
func preflightStoryboardItem(
	project *domain.ProjectFile, projectDir string, script map[string]any, resourceID string,
) error {
	items, idField, fields := domain.StoryboardItems(script)
	item, ok := findPreflightItem(items, idField, resourceID)
	if !ok {
		return fmt.Errorf("生成前检查失败：找不到分镜/场景 %s", resourceID)
	}
	if formatted := FormatPreflightError(ValidateItemAssetReferences(project, projectDir, item, fields)); formatted != "" {
		return errors.New(formatted)
	}
	return nil
}

// preflightReferenceUnit 校验单个参考单元引用的资产。
//
// ad 从成员镜头现算参考集（索引里的 references 只是可能落后的展示缓存），其余按 unit 声明的
// references；两条路径委托既有 reference_preflight.go 的统一入口。
func preflightReferenceUnit(
	project *domain.ProjectFile, projectDir string, script map[string]any, resourceID string,
) error {
	isAd := scriptContentMode(script, project) == string(domain.ContentAd)
	key := "video_units"
	if isAd {
		key = "reference_units"
	}
	unit, ok := findReferenceUnit(script, key, resourceID)
	if !ok {
		return fmt.Errorf("生成前检查失败：找不到参考单元 %s", resourceID)
	}
	scriptArg := script
	if !isAd {
		scriptArg = nil
	}
	errorText, err := ValidateReferenceUnitsPreflight(
		project, projectDir, []map[string]any{unit}, scriptArg, nil,
	)
	if err != nil {
		return err
	}
	if errorText != "" {
		return errors.New(errorText)
	}
	return nil
}

// findPreflightItem 按 id 字段或 scene_id 别名匹配条目（对应 Python 预检里的双键比对）。
func findPreflightItem(items []any, idField, resourceID string) (map[string]any, bool) {
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if pythonStrLike(item[idField]) == resourceID || pythonStrLike(item["scene_id"]) == resourceID {
			return item, true
		}
	}
	return nil, false
}

// findReferenceUnit 在给定键的 unit 数组里按 unit_id 匹配。
func findReferenceUnit(script map[string]any, key, resourceID string) (map[string]any, bool) {
	list, ok := script[key].([]any)
	if !ok {
		return nil, false
	}
	for _, raw := range list {
		unit, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := unit["unit_id"].(string); id == resourceID {
			return unit, true
		}
	}
	return nil, false
}

// pythonStrLike 复刻 Python str() 的文本形态（nil 记空串，布尔按 True/False）。
func pythonStrLike(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%v", v)
	default:
		return fmt.Sprint(v)
	}
}

// taskIDOf 读 task id，nil 安全。
func taskIDOf(task *repository.TaskData) string {
	if task == nil {
		return ""
	}
	return task.TaskID
}
