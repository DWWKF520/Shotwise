// 重启自愈与断点续传执行层（对应 server/services/resume_executor.py::execute_resume_video_task）。
//
// 只做「按持久化身份轮询 + 下载 + 结算 + 回写」——绝不重新 submit（ADR 0007：重复 submit 等于
// 重复计费）。不走 executeVideoTask / executeReferenceVideoTask 常规流水线：provider 端 job 早已
// 在跑，本地 storyboard / 参考资产是否存在、能力闸是否仍通过，都不该把已提交给供应商的付费任务
// 卡成幽灵任务；本层仅复用两条常规路径的 finalize helper 回写产物。
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/queue"
	"github.com/shotwise/shotwise/internal/repository"
)

// ResumeExecutor 实现 queue.ResumeExecutor：worker 的 _process_resume_task 直接调它。
//
// 依赖注入的是 TaskDispatcher（承载 store 与 video generator）：resume 与常规路径共用同一份
// finalize helper（finalizeVideoTask / finalizeReferenceVideoUnit），避免复制粘贴一份产物回写。
type ResumeExecutor struct {
	dispatcher *TaskDispatcher
}

// NewResumeExecutor 构造；dispatcher 未注入视频生成器时 resume 显式失败（不静默放行）。
func NewResumeExecutor(dispatcher *TaskDispatcher) *ResumeExecutor {
	return &ResumeExecutor{dispatcher: dispatcher}
}

// 编译期契约：可直接作为 worker 的重启自愈执行层注入（queue.WithResumeExecutor 的形参）。
var _ queue.ResumeExecutor = (*ResumeExecutor)(nil)

// ResumeVideo 是重启自愈入口：解析持久化身份 → 回放提交域名 → 轮询下载 → 结算 → finalize。
//
// 身份锁定：backend 按提交时锁进 payload 的能力桶键解析（ResolvePinned），而非按当前项目配置
// 重解析——否则任务提交后到重启前用户改过配置，会拿旧 job_id 去新 provider 轮询。
func (r *ResumeExecutor) ResumeVideo(
	ctx context.Context, task *repository.TaskData, jobID string,
) (map[string]any, error) {
	// 非视频任务类型不承诺续跑（orphan 扫描只把 video 交到这里，该分支仅在脏数据时可达）。
	if task.TaskType != "video" && task.TaskType != "reference_video" {
		return nil, fmt.Errorf("%w: task_type=%s", queue.ErrResumeUnsupported, task.TaskType)
	}
	dispatcher := r.dispatcher
	if dispatcher == nil || dispatcher.videos == nil {
		return nil, fmt.Errorf("resume 需要已注入的视频生成器")
	}
	scriptFile := payloadString(task.Payload, "script_file")
	if scriptFile == "" {
		return nil, fmt.Errorf("script_file is required for resume")
	}

	project, err := dispatcher.store.Get(task.ProjectName)
	if err != nil {
		return nil, err
	}
	projectDir, err := dispatcher.store.Dir(task.ProjectName)
	if err != nil {
		return nil, err
	}

	target, err := dispatcher.videos.ResolvePinned(ctx, project, task.Payload, task.TaskID)
	if err != nil {
		return nil, err
	}
	providerID := target.ProviderModel.ProviderID

	// 提交端点回放：自定义供应商的 provider_endpoint 承载 endpoint 标识，比对判定协议是否被换掉；
	// 内置供应商承载的是请求域名，走下方 submitted_base_url 消费分支。
	submittedEndpoint := derefText(task.ProviderEndpoint)
	currentEndpoint := ""
	if custom, ok := target.Backend.(provider.CustomVideoBackend); ok {
		currentEndpoint = custom.Endpoint()
	}
	if err := ensureResumeEndpointUnchanged(submittedEndpoint, currentEndpoint, jobID, providerID); err != nil {
		return nil, err
	}

	resourceType := "videos"
	if task.TaskType == "reference_video" {
		resourceType = "reference_videos"
	}

	out, err := dispatcher.videos.ResumeGenerate(ctx, target, ResumeGenerateParams{
		ProjectName:      task.ProjectName,
		ProjectDir:       projectDir,
		ResourceType:     resourceType,
		ResourceID:       task.ResourceID,
		Prompt:           payloadString(task.Payload, "prompt"),
		AspectRatio:      resumeAspectRatio(project, task.TaskType),
		DurationSeconds:  resumeDurationSeconds(task.Payload, project),
		Resolution:       payloadString(task.Payload, "resolution"),
		TaskID:           task.TaskID,
		APICallID:        payloadAPICallID(task.Payload),
		SubmittedBaseURL: submittedBaseURLForResume(submittedEndpoint, currentEndpoint),
		Seed:             payloadInt64(task.Payload, "seed"),
		ServiceTier:      resumeServiceTier(task.Payload),
		GenerateAudio:    payloadOptionalBool(task.Payload, "generate_audio"),
	}, jobID)
	if err != nil {
		var expired *provider.ResumeExpiredError
		switch {
		case errors.As(err, &expired):
			// 过期翻 pending → failed，零费用不重扣（与 Python resume_failed 同口径）；补账失败
			// 只记日志，不吞过期这一终止原因本身。
			r.settleResumeFailed(ctx, task)
			return nil, fmt.Errorf("%w: %s", queue.ErrResumeExpired, expired.Error())
		case errors.Is(err, provider.ErrResumeUnsupported):
			return nil, fmt.Errorf("%w: %s", queue.ErrResumeUnsupported, err.Error())
		default:
			return nil, err
		}
	}

	// 产物回写：复用两条常规路径的同一份 finalize（缩略图 + 剧本 generated_assets + 版本登记）。
	// resume 无 warnings，来源签名按现算口径（signatureCompute）写回。
	if task.TaskType == "reference_video" {
		return dispatcher.finalizeReferenceVideoUnit(task, projectDir, scriptFile, out, nil, signatureCompute, "")
	}
	return dispatcher.finalizeVideoTask(task, projectDir, scriptFile, out)
}

// settleResumeFailed 在 provider 端 job 过期时把提交时持久化的 pending 记账行翻 failed：让
// usage 报表不堆积无终态行，cost 零不重扣。缺锚点或写入失败只记日志，绝不阻断 mark_failed 落库。
func (r *ResumeExecutor) settleResumeFailed(ctx context.Context, task *repository.TaskData) {
	if r.dispatcher == nil || r.dispatcher.videos == nil {
		return
	}
	ledger := r.dispatcher.videos.ledger
	if ledger == nil {
		return
	}
	callID := payloadAPICallID(task.Payload)
	if callID == nil {
		return
	}
	if _, err := ledger.ResumeFailed(ctx, *callID); err != nil {
		slog.Error("resume 过期补账失败", "task_id", task.TaskID, "call_id", *callID, "err", err)
	}
}

// ensureResumeEndpointUnchanged 提交本 job 的 endpoint 与当下解析出的 endpoint 不同即显式失败。
//
// 内置供应商无 endpoint 维度（其持久化值是请求域名，走 submittedBaseURLForResume 分支），两侧任一
// 为空即跳过比对，与存量未持久化 endpoint 的任务行为一致。endpoint 决定协议，换 endpoint 等于换
// backend：拿新协议 backend 轮旧协议下创建的 job 会误读响应，把仍在跑仍在计费的远端 job 标失败。
func ensureResumeEndpointUnchanged(submitted, current, jobID, providerID string) error {
	if submitted == "" || current == "" || submitted == current {
		return nil
	}
	provErr := &provider.ResumeEndpointChangedError{
		JobID:             jobID,
		Provider:          providerID,
		SubmittedEndpoint: submitted,
		CurrentEndpoint:   current,
	}
	return fmt.Errorf("%w: %s", queue.ErrResumeEndpointChanged, provErr.Error())
}

// submittedBaseURLForResume 返回内置供应商提交本 job 时的请求域名，供 backend 回放轮询；无从判定
// 时返回空串（不注入）。
//
// current 非空即当下是自定义供应商，其持久化值是 endpoint 标识、归比对闸消费，不拿它拼 URL——
// 否则会把 404（可归因为 job 过期）换成更难归因的连接错误。域名形态再确认一次，拦住「任务由
// 自定义供应商提交、在途把模型行改成内置供应商」这类跨类切换。
func submittedBaseURLForResume(submitted, current string) string {
	if current != "" {
		return ""
	}
	lower := strings.ToLower(submitted)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return submitted
	}
	return ""
}

// resumeAspectRatio 解析 resume 请求比例：video 路径与常规路径同源（get_aspect_ratio(project,
// "videos")），reference_video 路径直接取项目顶层 aspect_ratio、缺省 9:16。
func resumeAspectRatio(project *domain.ProjectFile, taskType string) string {
	if taskType == "reference_video" {
		if project.AspectRatio != "" {
			return project.AspectRatio
		}
		return "9:16"
	}
	return aspectRatioFor(project, "videos")
}

// resumeDurationSeconds 按 resume 口径归一申请秒数：payload > 项目默认 > 8，可浮点解析则截断取整。
//
// 与常规路径的 videoDurationSeconds（非整数秒 fail loud）不同：resume 只用于让 backend 轮询/下载
// 已提交的 job，脏数据（"8.0" / 不可解析）不该让在跑的付费任务卡死，故回退到 8。
func resumeDurationSeconds(payload map[string]any, project *domain.ProjectFile) int {
	var raw any
	if value, present := payload["duration_seconds"]; present && refPresenceTruthy(value) {
		raw = value
	} else if project != nil && project.DefaultDuration != nil && *project.DefaultDuration != 0 {
		raw = *project.DefaultDuration
	} else {
		raw = 8
	}
	if seconds, ok := lenientIntSeconds(raw); ok {
		return seconds
	}
	return 8
}

// lenientIntSeconds 把外部输入按 int(float(x)) 语义归一为整数秒（截断小数），不可解析返回 false。
func lenientIntSeconds(raw any) (int, bool) {
	switch v := raw.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	default:
		return 0, false
	}
}

// resumeServiceTier 读 payload 里的服务档位，缺失按 default（与常规路径同口径）。
func resumeServiceTier(payload map[string]any) string {
	if tier := payloadNestedString(payload, "video_provider_settings", "service_tier"); tier != "" {
		return tier
	}
	return "default"
}

// payloadOptionalBool 读 payload 里的 bool 字段：仅在显式提供 bool 时返回非 nil 指针，
// 缺省 / 非 bool 一律 nil 交调用方走默认（与 Python 的 isinstance(x, bool) 门控一致）。
func payloadOptionalBool(payload map[string]any, key string) *bool {
	if value, ok := payload[key].(bool); ok {
		out := value
		return &out
	}
	return nil
}

// payloadAPICallID 读 payload["api_call_id"] 并归一为整数：JSON 解出为 float64 / 字符串脏数据
// 按可解析口径转换，不可解析返回 nil（不补账），与 Python 的 int() 容错口径一致。
func payloadAPICallID(payload map[string]any) *int64 {
	raw, present := payload["api_call_id"]
	if !present || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case float64:
		if v != float64(int64(v)) {
			return nil
		}
		id := int64(v)
		return &id
	case int64:
		id := v
		return &id
	case int:
		id := int64(v)
		return &id
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return nil
		}
		return &parsed
	default:
		return nil
	}
}
