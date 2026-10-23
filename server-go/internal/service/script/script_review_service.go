// step1→step2 web 审核 gate 的服务层（对应 server/services/script_review.py::ScriptReviewService）：
// 审阅状态读取、结构化中间态编辑、确认动作。
//
// 纯 gate 逻辑（适用性 / 指纹 / 状态派生）在 script_review.go；本层叠加 service.ProjectStore 持久化
// （确认指纹落 project.json episodes[i].step1_review）与结构化内容的校验、落盘。
//
// 确认触发 step2 的语义是「放行」而非「服务端 launcher」：step2（剧本视觉生成）由 agent 的
// generate_episode_script 工具执行，本服务只负责把审核状态翻到 confirmed；该工具读时经
// GateBlocksStep2 校验，pending 时拒绝、confirmed 后放行。
//
// Python 侧的 async 结构（asyncio.to_thread + 阻塞文件锁）在 Go 侧直接同步化；「档位表先于
// 取锁解析」的顺序约束保留——caps 解析走 resolver 查询，锁内不做能力解析。
package script

import (
	"context"
	"errors"
	"fmt"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/reference"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/provider"
)

// gate 领域错误码（对应 Python ScriptReviewError.code 的取值域）。
const (
	ScriptReviewCodeNotApplicable   = "not_applicable"
	ScriptReviewCodeNoStep1         = "no_step1"
	ScriptReviewCodeInvalidContent  = "invalid_content"
	ScriptReviewCodeEpisodeNotFound = "episode_not_found"
	ScriptReviewCodeQuarantined     = "quarantined"
	ScriptReviewCodeConflict        = "conflict"
)

// ScriptReviewError 是 gate 操作的领域错误。Code 供 transport 映射 HTTP 状态与 i18n key；
// Message 为技术细节。
type ScriptReviewError struct {
	Code    string
	Message string
}

func (e *ScriptReviewError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

func scriptReviewErr(code string) *ScriptReviewError {
	return &ScriptReviewError{Code: code}
}

// ScriptReviewService 封装 step1→step2 审核 gate 的读写。transport 与测试经此操作 gate，
// 不直接碰文件 / project.json。
type ScriptReviewService struct {
	store    *service.ProjectStore
	resolver service.VideoCapabilityResolver
}

// NewScriptReviewService 构造审阅门服务。
func NewScriptReviewService(store *service.ProjectStore, resolver service.VideoCapabilityResolver) *ScriptReviewService {
	return &ScriptReviewService{store: store, resolver: resolver}
}

// resolveStep1Kind 返回该集 step1 变体；不适用 gate（无结构化 step1，如 ad）时返回 not_applicable
// 领域错误（对应 Python _resolve_step1_model 的变体判定半边；内容模型即 ValidateStep1Content）。
func (s *ScriptReviewService) resolveStep1Kind(pf *project.ProjectFile) (string, error) {
	kind := Step1Kind(pf)
	if kind == "" {
		return "", scriptReviewErr(ScriptReviewCodeNotApplicable)
	}
	return kind, nil
}

// requireEpisode 校验该集已在 project.json episodes[] 登记，返回（必要时已自愈的）project。
//
// 与 Confirm 的写入前置一致：避免 GetState 把未登记分集误报成 no_step1、SaveContent 给未登记
// 分集写出永远无法与 project.json 关联的孤儿 step1 文件。
//
// 条目缺失时不立即拒绝：若该集的派生文件 source/episode_N.txt 实际存在（用户绕过分集规划器、
// 手动预拆分上传的存量场景），先用 RegisterOrphanEpisodeEntries 自愈补建条目再重新校验，
// 而非直接判死锁——手动预拆分与账本为空同时出现时，唯一的登记来源就是这次自愈。派生文件也不
// 存在时（真正缺失的集号）不自愈，直接报 episode_not_found。
func (s *ScriptReviewService) requireEpisode(
	projectName string, pf *project.ProjectFile, episode int,
) (*project.ProjectFile, error) {
	if service.FindEpisodeEntry(pf, episode) != nil {
		return pf, nil
	}
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	if _, discovered := DiscoverEpisodeFiles(projectDir)[episode]; !discovered {
		return nil, scriptReviewErr(ScriptReviewCodeEpisodeNotFound)
	}
	healed, err := s.registerOrphanEpisodes(projectName)
	if err != nil {
		return nil, err
	}
	if service.FindEpisodeEntry(healed, episode) == nil {
		return nil, scriptReviewErr(ScriptReviewCodeEpisodeNotFound)
	}
	return healed, nil
}

// registerOrphanEpisodes 在项目锁内跑一次 RegisterOrphanEpisodeEntries 并落盘，返回自愈后的
// project（对应 Python _register_orphan_episodes）。已登记的集号在函数内部即被跳过，重复触发
// 不会重写既有条目或产生重复集号。
func (s *ScriptReviewService) registerOrphanEpisodes(projectName string) (*project.ProjectFile, error) {
	var healed *project.ProjectFile
	err := s.store.Update(projectName, func(pf *project.ProjectFile) error {
		healed = pf
		projectDir, err := s.store.Dir(projectName)
		if err != nil {
			return err
		}
		RegisterOrphanEpisodeEntries(projectDir, pf)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return healed, nil
}

// resolveCapsBestEffort 做视频能力查询，解析失败时退回零值 view 而非冒穿（对应 Python
// _resolve_caps_best_effort：缺 caps 只是让下游退到 registry / 不收窄这两个既有降级口径，
// 解析异常直接冒穿会让用户连草稿都加载不了）。
func (s *ScriptReviewService) resolveCapsBestEffort(
	ctx context.Context, projectName string, pf *project.ProjectFile,
) config.VideoCapabilityView {
	caps := service.ResolveProjectVideoCaps(ctx, s.resolver, pf, provider.VideoBucketForGenerationMode(pf.GenerationMode))
	if !caps.Resolved {
		slog.Info("video_capabilities 解析异常，审阅门退回不带 caps 的解析", "project", projectName)
		return config.VideoCapabilityView{}
	}
	return caps.View
}

// resolveSupportedDurations 返回收窄前的时长档位全集；非 reference_video 变体或解析不到型号时
// nil（对应 Python _resolve_supported_durations）。
//
// caps 先解析、再交 config.ResolveRawSupportedDurations：registry 那一级只收录内建供应商，
// 自定义供应商（custom- 前缀）的档位表只有 caps（DB 驱动的能力查询）给得出。caps 解析失败时
// 退回 registry 解析，不阻断 gate：缺档位表只是回到结构区间 clamp。
//
// 调用方须在取 step1 文件锁之前完成本解析（Python 侧锁是跨进程阻塞文件锁、不可跨 await 持有；
// Go 侧保留该顺序以维持与 caps 查询的时序语义不变）。
func (s *ScriptReviewService) resolveSupportedDurations(
	ctx context.Context, projectName string, pf *project.ProjectFile,
) []int {
	if Step1Kind(pf) != step1KindReferenceVideo {
		return nil
	}
	view := s.resolveCapsBestEffort(ctx, projectName, pf)
	return config.ResolveRawSupportedDurations(pf, &view)
}

// readStep1Migrated 读结构化 step1，并对参考生视频草稿做一次性时长收编迁移；返回
// (内容, 最新 project)。调用方须已持有该文件的排他锁（withStep1FileLock / reference.Step1WriteLock
// 同一把 per-path 锁）——本函数只做读改写，不自行加锁。
//
// supportedDurations 由调用方经 resolveSupportedDurations 解析后传入（本函数跑在锁内，不做
// 能力解析），与 step2 生成侧同源：迁移幂等一次性、谁先跑谁定终局。为 nil（项目未配置视频
// 型号）时退回结构区间 clamp。
func (s *ScriptReviewService) readStep1Migrated(
	projectName string, pf *project.ProjectFile, episode int, path string, supportedDurations []int,
) (map[string]any, *project.ProjectFile, error) {
	content, _ := service.ReadJSONObjectOrNone(path)
	if content == nil || Step1Kind(pf) != step1KindReferenceVideo {
		return content, pf, nil
	}
	updated, _, err := reference.MigrateStep1DraftInPlace(s.store, projectName, content, episode, supportedDurations)
	if err != nil {
		return nil, nil, err
	}
	if updated != nil {
		return content, updated, nil
	}
	return content, pf, nil
}

// withStep1FileLock 取该 step1 文件的 per-path 排他锁（对应 Python ProjectManager.file_lock）。
// 与 reference.Step1WriteLock 同键：drama / narration 草稿的保存、迁移读改写与参考生视频的写盘出口
// 相互互斥。锁不可重入——已在临界区内的调用方不得再套。
func withStep1FileLock(path string, fn func() error) error {
	var err error
	reference.Step1Locks.WithLock(path, func() { err = fn() })
	return err
}

// GetState 返回该集审核状态 + 结构化中间态内容（供 web 渲染）。
//
// content 为解析后的结构化 step1（drama: {title, scenes[]}；narration: {segments[]}；
// reference_video: {units[]}）；不适用 gate 或 step1 缺失 / 损坏时为 nil。
//
// supportedDurations 只在 reference_video 变体下非 nil：unit 时长是档位枚举而非自由秒数，
// web 侧要按它渲染选择项。取值与读时迁移同源（同一次 resolveSupportedDurations），两处不同源
// 的话，gate 里能选的档位会与迁移收编到的档位不一致。
func (s *ScriptReviewService) GetState(ctx context.Context, projectName string, episode int) (map[string]any, error) {
	pf, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	supportedDurations := s.resolveSupportedDurations(ctx, projectName, pf)
	return s.getStateSync(projectName, pf, episode, supportedDurations)
}

// getStateSync 是 GetState 的主体：分集自愈、读时迁移、状态与指纹派生。
//
// pf 与 supportedDurations 由 GetState 预先取好传入——迁移不改动视频型号配置，档位表在迁移
// 前后描述的是同一件事，无需在锁内重算。
func (s *ScriptReviewService) getStateSync(
	projectName string, pf *project.ProjectFile, episode int, supportedDurations []int,
) (map[string]any, error) {
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	path, ok := Step1Path(projectDir, pf, episode)
	if ok {
		// 适用 gate（drama / narration / reference_video）才要求分集已登记；
		// not_applicable（ad）与分集存在性无关，保持原样返回。
		pf, err = s.requireEpisode(projectName, pf, episode)
		if err != nil {
			return nil, err
		}
	}
	var content map[string]any
	var fingerprint any
	status := ""
	if ok {
		// 迁移可能回写 step1 与确认记录；指纹与状态在同一把锁内、迁移之后取，三者才据
		// 同一份落盘内容派生——锁外取则并发 SaveContent 会让指纹描述另一份内容。
		err = withStep1FileLock(path, func() error {
			var updated *project.ProjectFile
			content, updated, err = s.readStep1Migrated(projectName, pf, episode, path, supportedDurations)
			if err != nil {
				return err
			}
			pf = updated
			live, err := service.ContentFingerprint(path)
			if err != nil {
				return err
			}
			if live != nil {
				fingerprint = *live
			}
			status = ReviewStatus(projectDir, pf, episode)
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		status = ReviewStatus(projectDir, pf, episode)
	}
	var confirmedAt any
	if review := service.StoredReview(pf, episode); review != nil && review.ConfirmedAt != "" {
		confirmedAt = review.ConfirmedAt
	}
	var contentValue any
	if content != nil {
		contentValue = content
	}
	return map[string]any{
		"episode":             episode,
		"content_mode":        pf.ContentMode,
		"status":              status,
		"fingerprint":         fingerprint,
		"confirmed_at":        confirmedAt,
		"content":             contentValue,
		"supported_durations": supportedDurations,
	}, nil
}

// GetQuarantineInfo 返回 reference_video 变体的隔离草稿信息（供 web 审核 gate 呈现违约行内
// 锚定），不适用 / 无隔离草稿时返回 nil。
//
// 读时按产出时那套校验器全量重算（reference.RevalidateReferenceStep1Draft，晋升工具同一份代码），
// 不信任草稿里 violations 的上一轮快照——隔离期间源文或模型配置可能已变，报告要对现值负责。
// 本方法与 GetState 各自读盘、互不依赖，由 transport 在同一次请求内合并两者的返回。
//
// content 校验通过时返回收编后的扁平产出（时长已按当前档位收编），未通过时返回草稿原样
// 内容 + 违约列表，供呈现层原样展示 agent 手改的那份文本。meta 被改坏以致无从重算时，把
// 「无法重算」本身作为一条违约返回——报告一律对现值负责。
func (s *ScriptReviewService) GetQuarantineInfo(
	ctx context.Context, projectName string, episode int,
) (map[string]any, error) {
	pf, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	if Step1Kind(pf) != step1KindReferenceVideo {
		return nil, nil
	}
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	quarantinePath, ok := Step1QuarantinePath(projectDir, pf, episode)
	if !ok || !step1FilePresent(quarantinePath) {
		return nil, nil
	}
	draft, err := reference.ReadQuarantine(projectDir, episode, reference.QuarantineKindStep1)
	if err != nil {
		var unsupported *reference.UnsupportedQuarantineSchemaError
		if errors.As(err, &unsupported) {
			slog.Warn("隔离草稿 schema 版本过新",
				"project", projectName, "episode", episode, "version", unsupported.Version)
			return map[string]any{
				"content": nil,
				"violations": []map[string]any{{
					"code": "quarantine_schema_unsupported", "label": "",
					"message": unsupported.Error(), "line": nil,
					"schema_version": unsupported.Version,
				}},
			}, nil
		}
		return nil, err
	}
	if draft == nil {
		if !step1FilePresent(quarantinePath) {
			// 存在性检查与读取之间的窗口内，agent 的晋升 / 重拆分工具把文件清掉了（正式内容
			// 已写入、隔离态合法结束）：不是信封损坏，是这次读跨越了「清除」那一刻，按「无
			// 隔离草稿」处理，不误报损坏。
			return nil, nil
		}
		// 文件存在但信封形状坏（非法 JSON / 顶层非对象 / content 非对象）：reference.ReadQuarantine 按
		// 「无隔离草稿」返回 nil 是它自己的读取口径（供 Confirm 的存在性判断照常阻塞），但
		// 呈现层不能照单全收——那会让面板看起来「干净」，实际隔离草稿仍在阻塞确认。
		return map[string]any{
			"content": nil,
			"violations": []map[string]any{{
				"code": "quarantine_unreadable", "label": "",
				"message": "隔离草稿文件已损坏或格式不符，无法解析", "line": nil,
			}},
		}, nil
	}
	revalidation, err := reference.RevalidateReferenceStep1Draft(ctx, projectDir, pf, episode, draft, s.resolver)
	if err != nil {
		// meta.source 缺失等草稿被改坏的情形：把重算失败本身报成一条无 unit 归属的违约，
		// 呈现层落聚合区。gate 不崩，用户也不会看到一份与现值脱钩的旧报告。异常文本含
		// draft.Path，只记日志、不回传给调用方——面向用户的 message 不能带内部路径。
		slog.Warn("隔离草稿重算失败", "project", projectName, "episode", episode, "error", err)
		return map[string]any{
			"content": draft.Content,
			"violations": []map[string]any{{
				"code": "quarantine_unreadable", "label": "",
				"message": "隔离草稿的产出上下文缺失或损坏，无法重新校验", "line": nil,
			}},
		}, nil
	}
	var content any = draft.Content
	if !revalidation.SchemaFailed {
		content = map[string]any{"units": revalidation.FlatUnits}
	}
	return map[string]any{"content": content, "violations": reference.ViolationEntries(revalidation.Violations)}, nil
}

// step1FilePresent 判定路径上有常规文件在场。
func step1FilePresent(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// GetReferenceDurationTiers 返回 reference_video 变体逐 unit 生效时长档位：
// {with_references, without_references}；非 reference_video 变体或无法解析到型号时返回 nil。
//
// 与 GetState.supportedDurations（收窄前的结构区间全集，供读时迁移的存量草稿 clamp）取自
// 同一份 ResolveRawSupportedDurations 结果，但用途不同源：那个决定「这个秒数结构上合不合法」，
// 这个决定「现在选它，_assert_reference_step1_ready 会不会接受」——分辨率 / 参考图联动约束
// 只影响后者。clamp 保持用全集，避免收窄后把结构合法但当前档位表之外的存量秒数误判非法。
func (s *ScriptReviewService) GetReferenceDurationTiers(
	ctx context.Context, projectName string, episode int,
) (map[string]any, error) {
	pf, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	if Step1Kind(pf) != step1KindReferenceVideo {
		return nil, nil
	}
	view := s.resolveCapsBestEffort(ctx, projectName, pf)
	raw := config.ResolveRawSupportedDurations(pf, &view)
	if raw == nil {
		return nil, nil
	}
	withRefs, withoutRefs := service.ReferenceUnitDurationTiers(ctx, s.resolver, pf, view, raw)
	return map[string]any{
		"with_references":    service.SortedUniqueInts(withRefs),
		"without_references": service.SortedUniqueInts(withoutRefs),
	}, nil
}

// SaveContent 校验并落盘编辑后的结构化中间态（手动或 agent 编辑后回写），返回最新状态
// （重新待审）——内容变更使指纹漂移，GetState 据此自动回到 pending_review，保存即重新需要确认。
//
// baseFingerprint 是编辑方读取内容（GetState）时拿到的指纹：给定时在锁内与盘上现值比对，
// 不一致（编辑期间另一写入方已改过 step1）报 conflict、不落盘——后写方拿 409 冲突提示去刷新
// 合并，先写方的内容不被静默覆盖。nil 不比对（无基线的直连调用）。
func (s *ScriptReviewService) SaveContent(
	ctx context.Context, projectName string, episode int, content any, baseFingerprint *string,
) (map[string]any, error) {
	if err := s.saveContentSync(projectName, episode, content, baseFingerprint); err != nil {
		return nil, err
	}
	return s.GetState(ctx, projectName, episode)
}

// saveContentSync 是 SaveContent 的主体：结构校验、references 重派生、基线比对、落盘。
func (s *ScriptReviewService) saveContentSync(
	projectName string, episode int, content any, baseFingerprint *string,
) error {
	pf, err := s.store.Get(projectName)
	if err != nil {
		return err
	}
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return err
	}
	path, ok := Step1Path(projectDir, pf, episode)
	if !ok {
		return scriptReviewErr(ScriptReviewCodeNotApplicable)
	}
	pf, err = s.requireEpisode(projectName, pf, episode)
	if err != nil {
		return err
	}
	kind, err := s.resolveStep1Kind(pf)
	if err != nil {
		return err
	}
	validated, err := ValidateStep1Content(kind, content)
	if err != nil {
		return &ScriptReviewError{Code: ScriptReviewCodeInvalidContent, Message: err.Error()}
	}
	// 入参的 nil 表示「调用方无基线、不比对」；比对语义里的 nil 另有含义（取基线时文件不存在），
	// 两者在此一次性转换，三个变体共用同一个 expected。
	expected := reference.UncheckedFingerprintExpectation()
	if baseFingerprint != nil {
		expected = reference.BaselineFingerprint(baseFingerprint)
	}
	if kind == step1KindReferenceVideo {
		// references 是从 shot 文本机械派生的字段：编辑正文后随之重派生，避免正文与 references
		// 漂移（step2 会用陈旧 [图N] 映射生成）。机械变换、不校验能力上限。
		rederiveUnitReferences(validated["units"], pf)
		// 写盘经单一出口：锁、基线比对、step2 隔离草稿清理都在 reference.WriteStep1Locked 一处。
		err = reference.Step1WriteLock(projectDir, episode, func(string) error {
			_, err := reference.WriteStep1Locked(projectDir, episode, validated, expected, true)
			return err
		})
	} else {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		// 与 readStep1Migrated 共享同一把 per-path 锁：保存与迁移的读改写相互互斥。
		// 基线比对同 rv 走 reference.AssertBaseFingerprint，两者的冲突判据不分叉。
		err = withStep1FileLock(path, func() error {
			if err := reference.AssertBaseFingerprint(path, expected); err != nil {
				return err
			}
			return fs.WriteJSONAtomic(path, validated)
		})
	}
	var conflict *reference.Step1WriteConflict
	if errors.As(err, &conflict) {
		return &ScriptReviewError{Code: ScriptReviewCodeConflict, Message: conflict.Error()}
	}
	return err
}

// Confirm 把该集审核状态翻到 confirmed（记录当前 step1 内容指纹），放行 step2。
//
// 无 step1 / 不适用 / 集条目缺失 / step1 内容结构非法 / 有违约产物待处置（隔离草稿在场）时
// 返回 ScriptReviewError，由 transport 映射 4xx。
//
// 档位表先于加锁解析（同 GetState）：确认路径同样要对存量草稿做一次读时收编，收编用的档位表
// 须与 gate 面板呈现的那份同源，否则确认会把面板上选不到的秒数固化到盘上。
func (s *ScriptReviewService) Confirm(ctx context.Context, projectName string, episode int) (map[string]any, error) {
	pf, err := s.store.Get(projectName)
	if err != nil {
		return nil, err
	}
	supportedDurations := s.resolveSupportedDurations(ctx, projectName, pf)
	if err := s.confirmSync(projectName, pf, episode, supportedDurations); err != nil {
		return nil, err
	}
	return s.GetState(ctx, projectName, episode)
}

// confirmSync 是 Confirm 的主体：隔离态校验、读时收编、结构校验、指纹落盘。
func (s *ScriptReviewService) confirmSync(
	projectName string, pf *project.ProjectFile, episode int, supportedDurations []int,
) error {
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return err
	}
	path, ok := Step1Path(projectDir, pf, episode)
	if !ok {
		return scriptReviewErr(ScriptReviewCodeNotApplicable)
	}
	pf, err = s.requireEpisode(projectName, pf, episode)
	if err != nil {
		return err
	}
	// 隔离草稿在场时拒绝确认：正式 step1 此刻仍是上一版（或不存在），确认它等于替用户认可
	// 一份他没看过的内容，而刚产出的那份违约正文还在隔离草稿里等 agent 处置。
	if quarantinePath, ok := Step1QuarantinePath(projectDir, pf, episode); ok && step1FilePresent(quarantinePath) {
		return &ScriptReviewError{
			Code:    ScriptReviewCodeQuarantined,
			Message: fmt.Sprintf("step1 违约产物待处置: %s", quarantinePath),
		}
	}
	// 存量草稿先做时长收编再校验：agent / 直连调用可能不经 GetState 就确认。同一把
	// per-path 锁覆盖读改写全程（含下方 reference_video 分支自己的落盘），避免中途被
	// SaveContent 或迁移的写入插队——readStep1Migrated 要求调用方已持锁。
	fingerprint := ""
	err = withStep1FileLock(path, func() error {
		var updated *project.ProjectFile
		var content map[string]any
		content, updated, err = s.readStep1Migrated(projectName, pf, episode, path, supportedDurations)
		if err != nil {
			return err
		}
		pf = updated
		if content == nil {
			if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
				return scriptReviewErr(ScriptReviewCodeNoStep1)
			}
		}
		// 确认前按 step1 变体模型校验 step1 结构：content 为 nil（非法 JSON / 非对象）同样会
		// 在此被校验拒绝为 invalid_content，不会被仅凭「文件存在」放行。
		kind, err := s.resolveStep1Kind(pf)
		if err != nil {
			return err
		}
		validated, err := ValidateStep1Content(kind, content)
		if err != nil {
			return &ScriptReviewError{Code: ScriptReviewCodeInvalidContent, Message: err.Error()}
		}
		if kind == step1KindReferenceVideo {
			// references 是从 shot 正文机械派生的字段（同 SaveContent）：agent / 人工可能绕过
			// SaveContent 直改 step1 正文后直接确认，故确认前重派生并落盘。指纹按刚写盘的
			// 这份对象直接算，确认记录与实际写入内容一致。
			rederiveUnitReferences(validated["units"], pf)
			// 单一写盘出口（已持同一把 per-path 锁，不再套 reference.Step1WriteLock）；同临界区读改写
			// 无并发窗口，不做基线比对。重派生真的改了内容时，step2 隔离草稿的基底随之失效，
			// 由出口按变更清理。
			if _, err := reference.WriteStep1Locked(
				projectDir, episode, validated, reference.UncheckedFingerprintExpectation(), true,
			); err != nil {
				return err
			}
			fingerprint = service.ContentFingerprintOfData(validated)
		} else {
			// 指纹从校验通过的 content 派生，不二次读盘：确认记录须对应这里校验过的内容。
			fingerprint = service.ContentFingerprintOfData(content)
		}
		return nil
	})
	if err != nil {
		return err
	}

	confirmedAt := time.Now().UTC().Format(time.RFC3339)
	return s.store.Update(projectName, func(pf *project.ProjectFile) error {
		if !service.ApplyConfirmation(pf, episode, fingerprint, confirmedAt) {
			return scriptReviewErr(ScriptReviewCodeEpisodeNotFound)
		}
		return nil
	})
}
