// Package app 装配 gin 引擎、进程级单例与 HTTP 路由。
package app

import (
	"context"
	servicereference "github.com/shotwise/shotwise/internal/service/reference"
	servicescript "github.com/shotwise/shotwise/internal/service/script"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/agent"
	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/queue"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/admin"
	"github.com/shotwise/shotwise/internal/service/asset"
	"github.com/shotwise/shotwise/internal/service/creative"
	"github.com/shotwise/shotwise/internal/service/generation"
	"github.com/shotwise/shotwise/internal/service/media"
	"github.com/shotwise/shotwise/internal/service/publishing"
	"github.com/shotwise/shotwise/internal/textgen"
	"github.com/shotwise/shotwise/internal/transport/http/agentconfig"
	"github.com/shotwise/shotwise/internal/transport/http/agentrt"
	httpasset "github.com/shotwise/shotwise/internal/transport/http/asset"
	"github.com/shotwise/shotwise/internal/transport/http/assistant"
	httpcreative "github.com/shotwise/shotwise/internal/transport/http/creative"
	"github.com/shotwise/shotwise/internal/transport/http/files"
	httpgeneration "github.com/shotwise/shotwise/internal/transport/http/generation"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"
	httpmedia "github.com/shotwise/shotwise/internal/transport/http/media"
	"github.com/shotwise/shotwise/internal/transport/http/project"
	"github.com/shotwise/shotwise/internal/transport/http/providers"
	"github.com/shotwise/shotwise/internal/transport/http/script"
	"github.com/shotwise/shotwise/internal/transport/http/system"
	"gorm.io/gorm"
)

// Runtime 持有进程级单例，对应 Python 侧挂在 app.state 上的那批对象。
type Runtime struct {
	Config  *config.Config
	Log     *slog.Logger
	DB      *gorm.DB
	Dialect db.Dialect
	Store   *service.ProjectStore
	Assets  *asset.AssetService
	Hub     *service.EventHub
	Auth    *auth.Service
	Agent   *agent.Runtime
	// Assistant 是持久会话编排层（meta + 事件日志 + client_key 幂等 + rewrite 分叉）。
	// main.go 装配；nil 时 assistant 路由组整组跳过（与 Agent 同口径）。
	Assistant *agent.AssistantService
	Queue     *queue.Queue
	// Worker 是进程内生成 worker；为 nil 时队列可观测性省掉泳道与 ETA 容量分母。
	Worker *queue.Worker
}

// New 构造引擎并完成路由注册。
//
// 中间件注册顺序即执行顺序（gin 的 Use 是尾插），与 Python 侧「注册顺序相反」
// 的 add_middleware 语义不同，这里按最终外层→内层排列：
//
//	Recovery → SPA no-cache → 请求日志 → CORS → 路由
func New(rt *Runtime) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()

	r.Use(gin.Recovery())
	r.Use(spaNoCacheMiddleware())
	r.Use(requestLoggingMiddleware(rt.Log))
	r.Use(corsMiddleware(rt.Config.CORSOrigins))

	// SPA 静态服务兜底未匹配路由；dist 不完整时保持 JSON 404。
	if !registerSPA(r, config.ResolveFrontendDist(), rt.Log) {
		r.NoRoute(notFound)
	}
	r.NoMethod(methodNotAllowed)

	r.GET("/health", system.Health)

	v1 := r.Group("/api/v1")
	registerAuth(v1, rt)
	registerProjects(v1, rt)
	registerMemories(v1, rt)
	registerReferenceVideos(v1, rt)
	registerMediaAssembly(v1, rt)
	registerJianyingAssembly(v1, rt)
	registerMediaRendering(v1, rt)
	registerCreativeBoards(v1, rt)
	registerCreativeContext(v1, rt)
	registerPublishing(v1, rt)
	registerMediaAssets(v1, rt)
	registerScriptReview(v1, rt)
	registerWorkflows(v1, rt)
	registerCreationPlans(v1, rt)
	registerDraftPromotions(v1, rt)
	registerPromptRegistry(v1, rt)
	registerAgent(v1, rt)
	registerTasks(v1, rt)
	registerGenerate(v1, rt)
	registerEpisodeDuration(v1, rt)
	registerBatches(v1, rt)
	registerGrids(v1, rt)
	registerEndFrames(v1, rt)
	registerShotUploads(v1, rt)
	registerOnboardingAndKeys(v1, rt)
	registerFeatures(v1, rt)
	registerProviders(v1, rt)
	registerCustomProviders(v1, rt)
	registerAgentConfig(v1, rt)
	registerSystem(v1, rt)
	registerSystemConfig(v1, rt)
	registerCreative(v1, rt)
	registerProjectSourceExport(v1, rt)
	registerProjectArchive(v1, rt)
	registerAssistant(v1, rt)
	registerFileRoutes(v1, rt)
	registerProjectExtras(v1, rt)
	registerGlobalAssets(v1, rt)
	registerCharacterRelations(v1, rt)
	registerCharacterExtras(v1, rt)

	return r
}

// registerReferenceVideos 参考生视频 unit 路由（对应 Python 侧无 feature gate 的挂载）。
func registerReferenceVideos(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpgeneration.RegisterReferenceVideos(authed, httpgeneration.ReferenceVideoDeps{
		Store:    rt.Store,
		Resolver: config.NewResolver(rt.DB),
		Queue:    rt.Queue,
		Hub:      rt.Hub,
	})
}

// registerMediaAssets 项目媒体库路由（受 media_library 开关约束）。
func registerMediaAssets(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	gated := authed.Group("", httpkit.RequireFeature("media_library"))
	httpmedia.RegisterMediaAssets(gated, httpmedia.MediaAssetsDeps{Store: rt.Store})
}

// registerCreativeContext 创建上下文解析路由（受 context_agent 开关约束）。
func registerCreativeContext(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	gated := authed.Group("", httpkit.RequireFeature("context_agent"))
	httpcreative.RegisterCreativeContext(gated, httpcreative.CreativeContextDeps{
		Context: creative.NewCreativeContextService(rt.DB, rt.Store),
		Store:   rt.Store,
	})
}

// registerPublishing 发布账号与发布任务路由。
func registerPublishing(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpmedia.RegisterPublishing(authed, httpmedia.PublishingDeps{
		Publishing: publishing.NewPublishingService(rt.DB),
		Events:     publishing.NewPublishJobEvents(rt.Hub, rt.Store),
	})
}

// registerCreativeBoards 创意板路由（受 creative_board 开关约束）。
func registerCreativeBoards(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	gated := authed.Group("", httpkit.RequireFeature("creative_board"))
	httpcreative.RegisterCreativeBoards(gated, httpcreative.CreativeBoardDeps{
		Boards: creative.NewCreativeBoardService(rt.DB),
	})
}

// registerMediaAssembly 媒体装配计划路由。
func registerMediaAssembly(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpmedia.RegisterMediaAssembly(authed, httpmedia.MediaAssemblyDeps{
		Plans: media.NewMediaAssemblyService(rt.DB),
	})
}

// registerMediaRendering 渲染任务路由（对应 Python 侧无 feature gate 的挂载）。
func registerMediaRendering(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpmedia.RegisterMediaRendering(authed, httpmedia.MediaRenderingDeps{
		Renders: media.NewMediaRenderingService(rt.DB, rt.Store),
	})
}

// registerJianyingAssembly 剪映草稿导出路由（对应 Python 侧无 feature gate 的挂载）。
func registerJianyingAssembly(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpmedia.RegisterJianyingAssembly(authed, httpmedia.JianyingAssemblyDeps{
		Exports: media.NewJianyingExportService(rt.DB, rt.Store),
	})
}

// registerMemories 长期记忆路由。
func registerMemories(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpasset.RegisterMemories(authed, httpasset.MemoryDeps{
		Store:    rt.Store,
		Memories: service.NewMemoryService(rt.DB),
	})
}

// registerScriptReview step1→step2 web 审核 gate 路由（对应 Python 侧无 feature gate 的挂载）。
func registerScriptReview(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	script.RegisterScriptReview(authed, script.ScriptReviewDeps{
		Store:   rt.Store,
		Reviews: servicescript.NewScriptReviewService(rt.Store, config.NewResolver(rt.DB)),
	})
}

// registerWorkflows Shotwise Flow 路由（对应 Python 侧无 feature gate 的挂载）。
func registerWorkflows(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpcreative.RegisterWorkflows(authed, httpcreative.WorkflowDeps{
		Workflows: creative.NewWorkflowService(rt.DB),
	})
}

// registerCreationPlans 创作计划路由（受 creation_plan + official_creation_skills 双开关约束）。
//
// 与 Python 侧 router 级 dependencies 等价：任一开关关闭，整组端点 404 feature_disabled。
func registerCreationPlans(v1 *gin.RouterGroup, rt *Runtime) {
	workflows := creative.NewWorkflowService(rt.DB)
	authed := v1.Group("", rt.Auth.Middleware())
	gated := authed.Group("",
		httpkit.RequireFeature("creation_plan"),
		httpkit.RequireFeature("official_creation_skills"),
	)
	httpcreative.RegisterCreationPlans(gated, httpcreative.CreationPlanDeps{
		Store: rt.Store,
		Plans: creative.NewCreationPlanService(rt.DB, rt.Dialect, workflows, config.NewResolver(rt.DB)),
	})
}

// registerDraftPromotions 草稿晋升路由（对应 Python 侧无 feature gate 的挂载）。
func registerDraftPromotions(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	script.RegisterDraftPromotions(authed, script.DraftPromotionDeps{
		Store: rt.Store,
		Drafts: domain.NewDraftPromotionService(
			servicereference.NewFileDraftPromotionRepository(rt.Store),
			servicereference.ScriptStructureDraftValidator(),
		),
	})
}

// registerPromptRegistry 提示词模板只读路由（对应 Python 侧无 feature gate 的挂载）。
func registerPromptRegistry(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpcreative.RegisterPromptRegistry(authed, httpcreative.PromptRegistryDeps{})
}

// registerTasks 任务队列 / 队列可观测性 / 版本路由。
func registerTasks(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	if rt.Queue != nil {
		httpgeneration.RegisterTasks(authed, httpgeneration.TaskDeps{Queue: rt.Queue})
	}
	var snapshotter generation.WorkerSnapshotter
	if rt.Worker != nil {
		snapshotter = rt.Worker
	}
	httpgeneration.RegisterQueueObservability(authed, httpgeneration.QueueObservabilityDeps{
		Obs: generation.NewQueueObservability(rt.DB, snapshotter),
	})
	httpgeneration.RegisterVersions(authed, httpgeneration.VersionDeps{
		Versions: service.NewVersionService(rt.Store),
	})
}

// registerGenerate 生成入队路由（分镜图 / 视频 / 旁白 / 资产设计图 / 图片编辑 / 试听样本）。
func registerGenerate(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	resolver := config.NewResolver(rt.DB)
	httpgeneration.RegisterGenerate(authed, httpgeneration.GenerateDeps{
		Store: rt.Store,
		Generate: generation.NewGenerateService(
			rt.Store, rt.Queue, resolver, generation.NewAudioGenerator(rt.DB, nil)),
	})
}

// registerEpisodeDuration 分集目标时长规划路由。
//
// 档位解析与生成路径同源（同一个 config.Resolver），否则预览给出的时长会在提交时被
// 后端按另一套档位静默夹取。
func registerEpisodeDuration(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpgeneration.RegisterEpisodeDuration(authed, httpgeneration.EpisodeDurationDeps{
		Store:    rt.Store,
		Duration: servicescript.NewEpisodeDurationService(rt.Store, config.NewResolver(rt.DB)),
	})
}

// registerBatches 批量生成路由。
//
// 仓储与适配器都按「项目 + 调用者」现场构造：批次号是全局主键，围栏必须落在查询条件上，
// 否则同一批次号可以被跨项目/跨用户读到。
func registerBatches(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpgeneration.RegisterBatches(authed, httpgeneration.BatchDeps{
		Store: rt.Store,
		NewOrchestrator: func(projectName, userID string) *generation.BatchOrchestrator {
			repo := repository.NewTaskRepo(rt.DB)
			adapter := generation.NewBatchTaskAdapter(repo, rt.Queue, projectName)
			return generation.NewBatchOrchestrator(
				generation.NewGormBatchRepository(rt.DB, userID, projectName), adapter)
		},
	})
}

// registerGrids 宫格图路由。
//
// 档位门控（4×4 / 5×5 是否放行）与执行侧必须同源：这里注入的 resolver 与生成路径是
// 同一个 config.Resolver，否则预览按 4K 放行、执行按 2K 落地会让格数对不上。
func registerGrids(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpgeneration.RegisterGrids(authed, httpgeneration.GridDeps{
		Store: rt.Store,
		Grids: generation.NewGridTaskService(
			rt.Store, rt.Queue, config.NewResolver(rt.DB), service.NewGridService(rt.Store)),
	})
}

// registerEndFrames 镜头尾帧路由。
func registerEndFrames(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpmedia.RegisterEndFrames(authed, httpmedia.EndFrameDeps{
		Store:    rt.Store,
		EndFrame: asset.NewEndFrameService(rt.Store),
	})
}

// registerShotUploads 镜头级分镜图 / 视频上传路由。
//
// 与宫格联合图上传分开挂：那条只覆盖联合图、不动分镜格；这条覆盖单格并对齐生成链路的
// 「版本 + 剧本回写 + 指纹」三件套。
func registerShotUploads(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpmedia.RegisterShotUploads(authed, httpmedia.ShotUploadDeps{
		Store:   rt.Store,
		Uploads: media.NewShotUploadService(rt.Store),
	})
}

// registerOnboardingAndKeys 首次引导状态与 API Key 管理。
//
// API Key 的增删查必须走 JWT 身份（路由层再加一道 auth.RequireAdmin）：
// 一把泄漏的 key 若能自我续期 / 自我吊销他人，等于把凭证管理面变成了提权面。
func registerOnboardingAndKeys(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	system.RegisterOnboarding(authed, system.OnboardingDeps{
		Config: config.NewConfigService(rt.DB),
	})
	system.RegisterAPIKeys(authed, system.APIKeyDeps{DB: rt.DB})
}

// registerFeatures 放量诊断路由。
//
// 走 authed：诊断面不含密钥，但会暴露「哪些开关开着」，仍然不该对匿名请求开放。
func registerFeatures(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpcreative.RegisterFeatures(authed, httpcreative.FeaturesDeps{
		Plans: creative.NewCreationPlanService(
			rt.DB, rt.Dialect, creative.NewWorkflowService(rt.DB), config.NewResolver(rt.DB)),
	})
}

// registerProviders 供应商管理路由。
//
// 已收敛范围：openai / claude（文本）、ark 的 seedance（视频）与 seedream（图片）、
// openai video / openai image。管理面本身不按厂商写分支——目录直接读注册表，
// 所以后续加厂商不需要改这层。
func registerProviders(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	providers.RegisterProviders(authed, providers.ProviderDeps{
		Providers: admin.NewProviderAdminService(rt.DB),
	})
}

// registerCustomProviders 自定义供应商路由。
//
// 与内置供应商管理面同级：自定义供应商的凭据存在 custom_provider 表上，不走
// provider_config / provider_credential，两条链路互不参照。
func registerCustomProviders(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	// 显式判空再装箱：rt.Worker 是 *queue.Worker，直接塞进接口会得到非 nil 的
	// typed-nil，服务的 nil 判断挡不住，调用时才在解引用处炸。
	var limits admin.CustomCapacityRefresher
	if rt.Worker != nil {
		limits = rt.Worker
	}
	providers.RegisterCustomProviders(authed, providers.CustomProviderDeps{
		Providers: admin.NewCustomProviderService(rt.DB, rt.Store, limits),
	})
}

// registerAgentConfig Agent 凭证与预设目录路由。
//
// 收敛范围：预设只给 anthropic（claude）与 openai 两家 + 自定义哨兵；凭证仍按
// (user, sdk_type) 维度互斥激活，所以 claude 与 openai 可以各激活一条。
func registerAgentConfig(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	agentconfig.RegisterAgentConfig(authed, agentconfig.AgentConfigDeps{
		Credentials: admin.NewAgentCredentialService(rt.DB),
	})
}

// registerSystem 系统级路由（诊断日志打包，管理员限定）。
func registerSystem(v1 *gin.RouterGroup, rt *Runtime) {
	admin := v1.Group("", rt.Auth.Middleware(), auth.RequireAdmin())
	system.RegisterSystem(admin, system.SystemDeps{Config: rt.Config})
}

// registerAssistant 助手会话路由（持久会话契约：meta + 事件日志 + client_key 幂等）。
//
// authed 走 Bearer；entries/stream 是 EventSource 直发请求，走 flexible
// 鉴权（?token=）。assistant 为 nil（未装配智能体运行时）时整组跳过。
func registerAssistant(v1 *gin.RouterGroup, rt *Runtime) {
	if rt.Assistant == nil {
		return
	}
	authed := v1.Group("", rt.Auth.Middleware())
	sse := v1.Group("", rt.Auth.FlexibleMiddleware())
	assistant.RegisterAssistant(authed, sse, assistant.AssistantDeps{
		Assistant: rt.Assistant,
		Store:     rt.Store,
	})
}

// registerFileRoutes 文件管理路由。
//
// 公开端点（项目静态文件 / 全局资产）不带鉴权：前端 <img>/<video> 直发请求带不了
// Authorization header；两者都有 SafeJoin 防护与 nosniff。
func registerFileRoutes(v1 *gin.RouterGroup, rt *Runtime) {
	deps := files.FilesDeps{
		Store:  rt.Store,
		Files:  servicescript.NewFileService(rt.Store),
		Review: servicescript.NewScriptReviewService(rt.Store, config.NewResolver(rt.DB)),
	}
	files.RegisterPublicFiles(v1, deps)
	authed := v1.Group("", rt.Auth.Middleware())
	files.RegisterFiles(authed, deps)
	files.RegisterFileUploads(authed, files.FileUploadDeps{Store: rt.Store, DB: rt.DB})
}

// registerProjectExtras 项目扩展端点（剧本编辑 / 分集元数据 / 概述 / 视频能力）。
func registerProjectExtras(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	project.RegisterProjectExtras(authed, project.ProjectExtraDeps{
		Store: rt.Store,
		Extra: servicescript.NewProjectExtraService(rt.Store, rt.Queue, config.NewResolver(rt.DB)),
	})
}

// registerGlobalAssets 全局资产库路由（跨项目复用的 character / scene / prop）。
func registerGlobalAssets(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpasset.RegisterAssetsGlobal(authed, httpasset.AssetsDeps{
		Assets: asset.NewGlobalAssetService(
			repository.NewGlobalAssetRepository(rt.DB), rt.Store),
	})
}

// registerCharacterRelations 角色关系图路由。
//
// analyze 复用 agent 包的同一份分析实现（与 SDK 工具零分叉）；工厂每轮现取，
// 保证用当轮的文本后端配置。
func registerCharacterRelations(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpasset.RegisterCharacterRelations(authed, httpasset.CharacterRelationsDeps{
		Store: rt.Store,
		RelationFactory: func(ctx context.Context, pf *domain.ProjectFile) (agent.RelationTextGenerator, error) {
			return textgen.NewGenerator(ctx, rt.DB, pf, textgen.TaskScript)
		},
	})
}

// registerCharacterExtras 角色头像与变体路由。
func registerCharacterExtras(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpasset.RegisterCharacterExtras(authed, httpasset.CharacterDeps{
		Variants: asset.NewCharacterVariants(rt.Store),
	})
}

// registerSystemConfig 系统配置路由（GET 公开、PATCH 管理员），与诊断日志分开挂。
func registerSystemConfig(v1 *gin.RouterGroup, rt *Runtime) {
	system.RegisterSystemConfig(v1, system.SystemConfigDeps{
		Config: admin.NewSystemConfigService(rt.DB, config.NewConfigService(rt.DB)),
	})
}

// registerCreative 创作稿文本生成端点。
func registerCreative(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	httpcreative.RegisterCreative(authed, httpcreative.CreativeDeps{Store: rt.Store, DB: rt.DB})
}

// registerProjectSourceExport 源文上传、概述生成与导出。
//
// export/token 走 authed；GET /export 走 download_token（浏览器原生下载带不了
// Authorization header），token 的 purpose/project 双校验绑定单个项目。
func registerProjectSourceExport(v1 *gin.RouterGroup, rt *Runtime) {
	deps := project.NewSourceExportDeps(rt.Store, rt.Auth, rt.DB)
	authed := v1.Group("", rt.Auth.Middleware())
	project.RegisterProjectSourceExport(v1, authed, deps)
}

// registerProjectArchive 项目 ZIP 导入端点。
func registerProjectArchive(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	project.RegisterProjectArchive(authed, project.ArchiveDeps{
		Archive: service.NewProjectArchiveService(rt.Store),
	})
}

// registerAuth 认证路由。status/token 必须公开（拿 token 之前就要可达）。
func registerAuth(v1 *gin.RouterGroup, rt *Runtime) {
	v1.GET("/auth/status", system.AuthStatus(rt.Auth))
	v1.POST("/auth/token", system.AuthToken(rt.Auth))

	authed := v1.Group("", rt.Auth.Middleware())
	authed.POST("/auth/logout", system.AuthLogout())
	authed.GET("/auth/verify", system.AuthVerify())
}

// registerProjects 项目路由。
//
// 注意 gin 的路由树限制：/projects/:name/events/stream 与 /projects/:name
// 前缀不冲突，但未来加入 /projects/:name/script-scenes/:id 时必须核对
// 与 /projects/:name/scenes/:entry 的匹配优先级（Python 侧 projects.py:1050
// 明确注释过这个陷阱）。
func registerProjects(v1 *gin.RouterGroup, rt *Runtime) {
	deps := project.ProjectDeps{Store: rt.Store, Hub: rt.Hub}

	authed := v1.Group("", rt.Auth.Middleware())
	authed.GET("/projects", project.ListProjects(deps))
	authed.POST("/projects", project.CreateProject(deps))
	authed.GET("/projects/:name", project.GetProject(deps))
	authed.PATCH("/projects/:name", project.UpdateProject(deps))
	authed.DELETE("/projects/:name", project.DeleteProject(deps))

	httpasset.RegisterAssets(authed, httpasset.AssetDeps{Assets: rt.Assets, Store: rt.Store})

	// SSE 走 flexible 鉴权：EventSource 无法自定义 header，仅支持 ?token=。
	sse := v1.Group("", rt.Auth.FlexibleMiddleware())
	sse.GET("/projects/:name/events/stream", project.ProjectEventStream(deps))
}

// registerAgent 智能体运行时路由（含 SSE 流式问答）。
func registerAgent(v1 *gin.RouterGroup, rt *Runtime) {
	if rt.Agent == nil {
		return
	}
	authed := v1.Group("", rt.Auth.Middleware())
	agentrt.RegisterAgentRoutes(authed, rt.Agent)

	// entry SSE 走 flexible 鉴权：EventSource 无法自定义 header，仅支持 ?token=。
	sse := v1.Group("", rt.Auth.FlexibleMiddleware())
	agentrt.RegisterAgentStreamRoutes(sse, rt.Agent)
}

func notFound(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"detail": "Not Found"})
}

func methodNotAllowed(c *gin.Context) {
	c.JSON(http.StatusMethodNotAllowed, gin.H{"detail": "Method Not Allowed"})
}
