// Package app 装配 gin 引擎、进程级单例与 HTTP 路由。
package app

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/agent"
	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/queue"
	"github.com/shotwise/shotwise/internal/textgen"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	transport "github.com/shotwise/shotwise/internal/transport/http"
	"gorm.io/gorm"
)

// Runtime 持有进程级单例，对应 Python 侧挂在 app.state 上的那批对象。
type Runtime struct {
	Config  *config.Config
	Log     *slog.Logger
	DB      *gorm.DB
	Dialect db.Dialect
	Store   *service.ProjectStore
	Assets  *service.AssetService
	Hub     *service.EventHub
	Auth    *auth.Service
	Agent   *agent.Runtime
	Queue   *queue.Queue
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

	r.NoRoute(notFound)
	r.NoMethod(methodNotAllowed)

	r.GET("/health", transport.Health)

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
	registerAgentConfig(v1, rt)
	registerSystem(v1, rt)
	registerSystemConfig(v1, rt)
	registerCreative(v1, rt)
	registerProjectSourceExport(v1, rt)
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
	transport.RegisterReferenceVideos(authed, transport.ReferenceVideoDeps{
		Store:    rt.Store,
		Resolver: config.NewResolver(rt.DB),
		Queue:    rt.Queue,
		Hub:      rt.Hub,
	})
}

// registerMediaAssets 项目媒体库路由（受 media_library 开关约束）。
func registerMediaAssets(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	gated := authed.Group("", transport.RequireFeature("media_library"))
	transport.RegisterMediaAssets(gated, transport.MediaAssetsDeps{Store: rt.Store})
}

// registerCreativeContext 创建上下文解析路由（受 context_agent 开关约束）。
func registerCreativeContext(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	gated := authed.Group("", transport.RequireFeature("context_agent"))
	transport.RegisterCreativeContext(gated, transport.CreativeContextDeps{
		Context: service.NewCreativeContextService(rt.DB, rt.Store),
		Store:   rt.Store,
	})
}

// registerPublishing 发布账号与发布任务路由。
func registerPublishing(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterPublishing(authed, transport.PublishingDeps{
		Publishing: service.NewPublishingService(rt.DB),
		Events:     service.NewPublishJobEvents(rt.Hub, rt.Store),
	})
}

// registerCreativeBoards 创意板路由（受 creative_board 开关约束）。
func registerCreativeBoards(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	gated := authed.Group("", transport.RequireFeature("creative_board"))
	transport.RegisterCreativeBoards(gated, transport.CreativeBoardDeps{
		Boards: service.NewCreativeBoardService(rt.DB),
	})
}

// registerMediaAssembly 媒体装配计划路由。
func registerMediaAssembly(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterMediaAssembly(authed, transport.MediaAssemblyDeps{
		Plans: service.NewMediaAssemblyService(rt.DB),
	})
}

// registerMediaRendering 渲染任务路由（对应 Python 侧无 feature gate 的挂载）。
func registerMediaRendering(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterMediaRendering(authed, transport.MediaRenderingDeps{
		Renders: service.NewMediaRenderingService(rt.DB, rt.Store),
	})
}

// registerJianyingAssembly 剪映草稿导出路由（对应 Python 侧无 feature gate 的挂载）。
func registerJianyingAssembly(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterJianyingAssembly(authed, transport.JianyingAssemblyDeps{
		Exports: service.NewJianyingExportService(rt.DB, rt.Store),
	})
}

// registerMemories 长期记忆路由。
func registerMemories(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterMemories(authed, transport.MemoryDeps{
		Store:    rt.Store,
		Memories: service.NewMemoryService(rt.DB),
	})
}

// registerScriptReview step1→step2 web 审核 gate 路由（对应 Python 侧无 feature gate 的挂载）。
func registerScriptReview(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterScriptReview(authed, transport.ScriptReviewDeps{
		Store:   rt.Store,
		Reviews: service.NewScriptReviewService(rt.Store, config.NewResolver(rt.DB)),
	})
}

// registerWorkflows Shotwise Flow 路由（对应 Python 侧无 feature gate 的挂载）。
func registerWorkflows(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterWorkflows(authed, transport.WorkflowDeps{
		Workflows: service.NewWorkflowService(rt.DB),
	})
}

// registerCreationPlans 创作计划路由（受 creation_plan + official_creation_skills 双开关约束）。
//
// 与 Python 侧 router 级 dependencies 等价：任一开关关闭，整组端点 404 feature_disabled。
func registerCreationPlans(v1 *gin.RouterGroup, rt *Runtime) {
	workflows := service.NewWorkflowService(rt.DB)
	authed := v1.Group("", rt.Auth.Middleware())
	gated := authed.Group("",
		transport.RequireFeature("creation_plan"),
		transport.RequireFeature("official_creation_skills"),
	)
	transport.RegisterCreationPlans(gated, transport.CreationPlanDeps{
		Store: rt.Store,
		Plans: service.NewCreationPlanService(rt.DB, rt.Dialect, workflows, config.NewResolver(rt.DB)),
	})
}

// registerDraftPromotions 草稿晋升路由（对应 Python 侧无 feature gate 的挂载）。
func registerDraftPromotions(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterDraftPromotions(authed, transport.DraftPromotionDeps{
		Store: rt.Store,
		Drafts: domain.NewDraftPromotionService(
			service.NewFileDraftPromotionRepository(rt.Store),
			service.ScriptStructureDraftValidator(),
		),
	})
}

// registerPromptRegistry 提示词模板只读路由（对应 Python 侧无 feature gate 的挂载）。
func registerPromptRegistry(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterPromptRegistry(authed, transport.PromptRegistryDeps{})
}

// registerTasks 任务队列 / 队列可观测性 / 版本路由。
func registerTasks(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	if rt.Queue != nil {
		transport.RegisterTasks(authed, transport.TaskDeps{Queue: rt.Queue})
	}
	var snapshotter service.WorkerSnapshotter
	if rt.Worker != nil {
		snapshotter = rt.Worker
	}
	transport.RegisterQueueObservability(authed, transport.QueueObservabilityDeps{
		Obs: service.NewQueueObservability(rt.DB, snapshotter),
	})
	transport.RegisterVersions(authed, transport.VersionDeps{
		Versions: service.NewVersionService(rt.Store),
	})
}

// registerGenerate 生成入队路由（分镜图 / 视频 / 旁白 / 资产设计图 / 图片编辑 / 试听样本）。
func registerGenerate(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	resolver := config.NewResolver(rt.DB)
	transport.RegisterGenerate(authed, transport.GenerateDeps{
		Store: rt.Store,
		Generate: service.NewGenerateService(
			rt.Store, rt.Queue, resolver, service.NewAudioGenerator(rt.DB, nil)),
	})
}

// registerEpisodeDuration 分集目标时长规划路由。
//
// 档位解析与生成路径同源（同一个 config.Resolver），否则预览给出的时长会在提交时被
// 后端按另一套档位静默夹取。
func registerEpisodeDuration(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterEpisodeDuration(authed, transport.EpisodeDurationDeps{
		Store:    rt.Store,
		Duration: service.NewEpisodeDurationService(rt.Store, config.NewResolver(rt.DB)),
	})
}

// registerBatches 批量生成路由。
//
// 仓储与适配器都按「项目 + 调用者」现场构造：批次号是全局主键，围栏必须落在查询条件上，
// 否则同一批次号可以被跨项目/跨用户读到。
func registerBatches(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterBatches(authed, transport.BatchDeps{
		Store: rt.Store,
		NewOrchestrator: func(projectName, userID string) *service.BatchOrchestrator {
			repo := repository.NewTaskRepo(rt.DB)
			adapter := service.NewBatchTaskAdapter(repo, rt.Queue, projectName)
			return service.NewBatchOrchestrator(
				service.NewGormBatchRepository(rt.DB, userID, projectName), adapter)
		},
	})
}

// registerGrids 宫格图路由。
//
// 档位门控（4×4 / 5×5 是否放行）与执行侧必须同源：这里注入的 resolver 与生成路径是
// 同一个 config.Resolver，否则预览按 4K 放行、执行按 2K 落地会让格数对不上。
func registerGrids(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterGrids(authed, transport.GridDeps{
		Store: rt.Store,
		Grids: service.NewGridTaskService(
			rt.Store, rt.Queue, config.NewResolver(rt.DB), service.NewGridService(rt.Store)),
	})
}

// registerEndFrames 镜头尾帧路由。
func registerEndFrames(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterEndFrames(authed, transport.EndFrameDeps{
		Store:    rt.Store,
		EndFrame: service.NewEndFrameService(rt.Store),
	})
}

// registerShotUploads 镜头级分镜图 / 视频上传路由。
//
// 与宫格联合图上传分开挂：那条只覆盖联合图、不动分镜格；这条覆盖单格并对齐生成链路的
// 「版本 + 剧本回写 + 指纹」三件套。
func registerShotUploads(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterShotUploads(authed, transport.ShotUploadDeps{
		Store:   rt.Store,
		Uploads: service.NewShotUploadService(rt.Store),
	})
}

// registerOnboardingAndKeys 首次引导状态与 API Key 管理。
//
// API Key 的增删查必须走 JWT 身份（路由层再加一道 auth.RequireAdmin）：
// 一把泄漏的 key 若能自我续期 / 自我吊销他人，等于把凭证管理面变成了提权面。
func registerOnboardingAndKeys(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterOnboarding(authed, transport.OnboardingDeps{
		Config: config.NewConfigService(rt.DB),
	})
	transport.RegisterAPIKeys(authed, transport.APIKeyDeps{DB: rt.DB})
}

// registerFeatures 放量诊断路由。
//
// 走 authed：诊断面不含密钥，但会暴露「哪些开关开着」，仍然不该对匿名请求开放。
func registerFeatures(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterFeatures(authed, transport.FeaturesDeps{
		Plans: service.NewCreationPlanService(
			rt.DB, rt.Dialect, service.NewWorkflowService(rt.DB), config.NewResolver(rt.DB)),
	})
}

// registerProviders 供应商管理路由。
//
// 已收敛范围：openai / claude（文本）、ark 的 seedance（视频）与 seedream（图片）、
// openai video / openai image。管理面本身不按厂商写分支——目录直接读注册表，
// 所以后续加厂商不需要改这层。
func registerProviders(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterProviders(authed, transport.ProviderDeps{
		Providers: service.NewProviderAdminService(rt.DB),
	})
}

// registerAgentConfig Agent 凭证与预设目录路由。
//
// 收敛范围：预设只给 anthropic（claude）与 openai 两家 + 自定义哨兵；凭证仍按
// (user, sdk_type) 维度互斥激活，所以 claude 与 openai 可以各激活一条。
func registerAgentConfig(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterAgentConfig(authed, transport.AgentConfigDeps{
		Credentials: service.NewAgentCredentialService(rt.DB),
	})
}

// registerSystem 系统级路由（诊断日志打包，管理员限定）。
func registerSystem(v1 *gin.RouterGroup, rt *Runtime) {
	admin := v1.Group("", rt.Auth.Middleware(), auth.RequireAdmin())
	transport.RegisterSystem(admin, transport.SystemDeps{Config: rt.Config})
}

// registerAssistant 助手会话路由。
//
// 目前只注册 Python 侧已下线的 410 接口；会话型端点（send / rewrite / 列表 / 详情）需要
// 先把持久会话存储（meta + 事件日志 + client_key 幂等）迁过来，半兼容的 200 比 404 更伤。
func registerAssistant(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterAssistant(authed, transport.AssistantDeps{})
}

// registerFileRoutes 文件管理路由。
//
// 公开端点（项目静态文件 / 全局资产）不带鉴权：前端 <img>/<video> 直发请求带不了
// Authorization header；两者都有 SafeJoin 防护与 nosniff。
func registerFileRoutes(v1 *gin.RouterGroup, rt *Runtime) {
	deps := transport.FilesDeps{
		Store:  rt.Store,
		Files:  service.NewFileService(rt.Store),
		Review: service.NewScriptReviewService(rt.Store, config.NewResolver(rt.DB)),
	}
	transport.RegisterPublicFiles(v1, deps)
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterFiles(authed, deps)
}

// registerProjectExtras 项目扩展端点（剧本编辑 / 分集元数据 / 概述 / 视频能力）。
func registerProjectExtras(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterProjectExtras(authed, transport.ProjectExtraDeps{
		Store: rt.Store,
		Extra: service.NewProjectExtraService(rt.Store, rt.Queue, config.NewResolver(rt.DB)),
	})
}

// registerGlobalAssets 全局资产库路由（跨项目复用的 character / scene / prop）。
func registerGlobalAssets(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterAssetsGlobal(authed, transport.AssetsDeps{
		Assets: service.NewGlobalAssetService(
			repository.NewGlobalAssetRepository(rt.DB), rt.Store),
	})
}

// registerCharacterRelations 角色关系图路由。
//
// analyze 复用 agent 包的同一份分析实现（与 SDK 工具零分叉）；工厂每轮现取，
// 保证用当轮的文本后端配置。
func registerCharacterRelations(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterCharacterRelations(authed, transport.CharacterRelationsDeps{
		Store: rt.Store,
		RelationFactory: func(ctx context.Context, pf *domain.ProjectFile) (agent.RelationTextGenerator, error) {
			return textgen.NewGenerator(ctx, rt.DB, pf, textgen.TaskScript)
		},
	})
}

// registerCharacterExtras 角色头像与变体路由。
func registerCharacterExtras(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterCharacterExtras(authed, transport.CharacterDeps{
		Variants: service.NewCharacterVariants(rt.Store),
	})
}

// registerSystemConfig 系统配置路由（GET 公开、PATCH 管理员），与诊断日志分开挂。
func registerSystemConfig(v1 *gin.RouterGroup, rt *Runtime) {
	transport.RegisterSystemConfig(v1, transport.SystemConfigDeps{
		Config: service.NewSystemConfigService(rt.DB, config.NewConfigService(rt.DB)),
	})
}

// registerCreative 创作稿文本生成端点。
func registerCreative(v1 *gin.RouterGroup, rt *Runtime) {
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterCreative(authed, transport.CreativeDeps{Store: rt.Store, DB: rt.DB})
}

// registerProjectSourceExport 源文上传、概述生成与导出。
//
// export/token 走 authed；GET /export 走 download_token（浏览器原生下载带不了
// Authorization header），token 的 purpose/project 双校验绑定单个项目。
func registerProjectSourceExport(v1 *gin.RouterGroup, rt *Runtime) {
	deps := transport.NewSourceExportDeps(rt.Store, rt.Auth, rt.DB)
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterProjectSourceExport(v1, authed, deps)
}

// registerAuth 认证路由。status/token 必须公开（拿 token 之前就要可达）。
func registerAuth(v1 *gin.RouterGroup, rt *Runtime) {
	v1.GET("/auth/status", transport.AuthStatus(rt.Auth))
	v1.POST("/auth/token", transport.AuthToken(rt.Auth))

	authed := v1.Group("", rt.Auth.Middleware())
	authed.POST("/auth/logout", transport.AuthLogout())
	authed.GET("/auth/verify", transport.AuthVerify())
}

// registerProjects 项目路由。
//
// 注意 gin 的路由树限制：/projects/:name/events/stream 与 /projects/:name
// 前缀不冲突，但未来加入 /projects/:name/script-scenes/:id 时必须核对
// 与 /projects/:name/scenes/:entry 的匹配优先级（Python 侧 projects.py:1050
// 明确注释过这个陷阱）。
func registerProjects(v1 *gin.RouterGroup, rt *Runtime) {
	deps := transport.ProjectDeps{Store: rt.Store, Hub: rt.Hub}

	authed := v1.Group("", rt.Auth.Middleware())
	authed.GET("/projects", transport.ListProjects(deps))
	authed.POST("/projects", transport.CreateProject(deps))
	authed.GET("/projects/:name", transport.GetProject(deps))
	authed.PATCH("/projects/:name", transport.UpdateProject(deps))
	authed.DELETE("/projects/:name", transport.DeleteProject(deps))

	transport.RegisterAssets(authed, transport.AssetDeps{Assets: rt.Assets, Store: rt.Store})

	// SSE 走 flexible 鉴权：EventSource 无法自定义 header，仅支持 ?token=。
	sse := v1.Group("", rt.Auth.FlexibleMiddleware())
	sse.GET("/projects/:name/events/stream", transport.ProjectEventStream(deps))
}

// registerAgent 智能体运行时路由（含 SSE 流式问答）。
func registerAgent(v1 *gin.RouterGroup, rt *Runtime) {
	if rt.Agent == nil {
		return
	}
	authed := v1.Group("", rt.Auth.Middleware())
	transport.RegisterAgentRoutes(authed, rt.Agent)

	// entry SSE 走 flexible 鉴权：EventSource 无法自定义 header，仅支持 ?token=。
	sse := v1.Group("", rt.Auth.FlexibleMiddleware())
	transport.RegisterAgentStreamRoutes(sse, rt.Agent)
}

func notFound(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"detail": "Not Found"})
}

func methodNotAllowed(c *gin.Context) {
	c.JSON(http.StatusMethodNotAllowed, gin.H{"detail": "Method Not Allowed"})
}
