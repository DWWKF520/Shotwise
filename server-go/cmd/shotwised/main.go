// Command shotwised 是 SHOTWISE 的 Go 服务入口。
//
// 启动顺序与 Python 侧 lifespan 对齐：
//
//	assert_no_provider_secrets → 沙箱探测 → 认证就绪 → 数据库迁移 → 默认用户
//	→ 事件服务 → 生成 worker（先注册取消回调再启动）→ 监听
//
// 关停顺序相反。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/shotwise/shotwise/internal/agent"
	"github.com/shotwise/shotwise/internal/agent/checkpoint"
	"github.com/shotwise/shotwise/internal/agent/eventlog"
	"github.com/shotwise/shotwise/internal/app"
	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/pricing"
	"github.com/shotwise/shotwise/internal/queue"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/textgen"
	"github.com/shotwise/shotwise/migrations"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// 必须在任何子进程（ffmpeg / agent 工具 / 沙箱）启动之前：
	// fork 出去的子进程会继承父进程环境，provider 密钥一旦在 env 里就会泄漏。
	if err := config.AssertNoProviderSecrets(); err != nil {
		return err
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}

	gdb, dialect, err := db.Open(cfg.DatabaseURL, gormLogLevel(cfg.LogLevel))
	if err != nil {
		return err
	}
	res, err := db.Migrate(gdb, dialect, migrations.FS())
	if err != nil {
		return err
	}
	for _, v := range res.Stamped {
		log.Info("migration stamped onto existing schema", "version", v)
	}
	for _, v := range res.Applied {
		log.Info("migration applied", "version", v)
	}

	users := repository.NewUserRepository(gdb)
	if err := users.EnsureDefaultUser(cfg.AuthUsername); err != nil {
		return err
	}

	authSvc := auth.New(cfg.TokenSecret, cfg.AuthEnabled, users)
	if cfg.AuthPassword != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(cfg.AuthPassword), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		authSvc.SetCredentials(cfg.AuthUsername, hash)
	}

	// 智能体运行时：用 Eino 重建，取代 Claude Agent SDK 的子进程模型。
	// checkpoint 与事件日志都落 SQL（agent_checkpoints / agent_session_event_log）：
	// 前者供中断恢复，后者是权威 transcript，SSE 按 seq 游标续传依赖它。
	cpStore, err := checkpoint.NewSQLStore(gdb)
	if err != nil {
		return err
	}
	store := service.NewProjectStore(cfg.DataDir)
	assets := service.NewAssetService(store)

	// 生成队列与 worker：所有生成类工具（资产 / 分镜 / 配音 / 图片编辑 / 宫格 / 视频 / 分集规划）
	// 与任务 API 都依赖它。ledger 走内置定价目录（pricing.BuiltinCatalog），无目录时结算无法
	// 在真实调用上完成。worker 的执行层是 TaskDispatcher（按 task_type 路由到各生成路径）。
	ledger := service.NewLedger(gdb, pricing.NewCalculator(pricing.BuiltinCatalog()))
	images := service.NewImageGenerator(gdb, ledger)
	audioGen := service.NewAudioGenerator(gdb, ledger)
	videos := service.NewVideoGenerator(gdb, ledger)
	dispatcher := service.NewTaskDispatcher(store, images)
	dispatcher.SetAudioGenerator(audioGen)
	dispatcher.SetVideoGenerator(videos)
	// 项目变更事件的订阅总线：worker 终态经它广播给前端，须先于 queue 构造。
	hub := service.NewEventHub()
	q := queue.NewQueue(gdb,
		queue.WithProjectLoader(store),
		// 生成完成的终态广播：只发成功事件，带受影响文件的指纹供前端 cache-bust。
		queue.WithTerminalEmitter(service.NewTerminalEmitter(hub, store, gdb)))
	worker := queue.NewWorker(q, dispatcher,
		// 中断记账收口：启动时翻残留 pending 行为失败，取消时翻该任务的 pending 行。
		queue.WithInterruptedSettler(service.NewLedgerSettler(ledger)),
		// 失败编码走结构化 "[code] {params}"，读侧据此渲染多语言文案。
		queue.WithFailureEncoder(service.EncodeTaskFailure),
		queue.WithTaskErrorEncoder(service.EncodeTaskError),
		// 健康账的熔断只对上游瞬态失败计数，取消与业务失败不动健康度。
		queue.WithTransientClassifier(service.IsTransientTaskError),
		// 限流槽路由用的执行身份投影；解析失败由 worker 兜底 DefaultProvider，不阻断认领循环。
		queue.WithProviderExtractor(service.NewProviderExtractor(store, config.NewResolver(gdb))),
		// 执行前复检引用资产，收窄「准入与执行之间」的项目 / 剧本漂移窗口。
		queue.WithPreflight(service.NewWorkerPreflight(store)),
		// 重启自愈：按提交时锁定的身份轮询下载，绝不重新 submit（重复 submit 等于重复计费）。
		queue.WithResumeExecutor(service.NewResumeExecutor(dispatcher)))
	// 取消回调先注册再启动 worker：取消 API 才能把进程内取消信号同步投递给在途执行。
	q.SetCancelCallback(worker.RequestCancel)
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel() // 兜底：中途 return 时也停掉 worker（关停路径另显式调用一次）
	go worker.Run(workerCtx)

	agentRT, err := agent.NewRuntime(context.Background(), agent.ModelConfig{
		APIKey:  cfg.LLMAPIKey,
		Model:   cfg.LLMModel,
		BaseURL: cfg.LLMBaseURL,
	}, cfg.DataDir, cpStore, eventlog.NewSQLStore(gdb),
		agent.WithIdleTimeout(cfg.AgentIdleTimeout),
		agent.WithProjectWriters(assets, store), agent.WithAssetProjects(store),
		agent.WithVideoCapabilities(config.NewResolver(gdb)),
		agent.WithRelationAnalysis(func(ctx context.Context, pf *domain.ProjectFile) (agent.RelationTextGenerator, error) {
			return textgen.NewGenerator(ctx, gdb, pf, textgen.TaskScript)
		}, store),
		agent.WithScriptEditing(store), agent.WithAssetRename(assets),
		agent.WithTextGenerationTools(func(ctx context.Context, pf *domain.ProjectFile) (service.TextCompletion, error) {
			return textgen.NewGenerator(ctx, gdb, pf, textgen.TaskScript)
		}, store),
		agent.WithScriptReviewTools(store),
		agent.WithReferenceTools(func(ctx context.Context, pf *domain.ProjectFile) (service.TextCompletion, error) {
			return textgen.NewGenerator(ctx, gdb, pf, textgen.TaskScript)
		}, store, config.NewResolver(gdb)),
		agent.WithTimelineTools(service.NewMediaAssemblyService(gdb)),
		agent.WithMemoryProposals(service.NewMemoryService(gdb)),
		// 生成类工具依赖上面装配好的 GenerationQueue 与 worker。
		agent.WithAssetGeneration(q, store),
		agent.WithGenerationTools(q, store),
		agent.WithImageEditTools(q, store, config.NewResolver(gdb)),
		agent.WithGridTools(q, store, config.NewResolver(gdb), service.NewGridService(store)),
		agent.WithEpisodePlanning(store, func(ctx context.Context, pf *domain.ProjectFile) (service.TextCompletion, error) {
			return textgen.NewGenerator(ctx, gdb, pf, textgen.TaskScript)
		}),
		agent.WithVideoGeneration(q, store, config.NewResolver(gdb), store))
	if err != nil {
		return err
	}

	rt := &app.Runtime{
		Config:  cfg,
		Log:     log,
		DB:      gdb,
		Dialect: dialect,
		Store:   store,
		Hub:     hub,
		Auth:    authSvc,
		Agent:   agentRT,
		Assets:  assets,
		Queue:   q,
	}

	engine := app.New(rt)
	srv := &http.Server{
		Addr:              cfg.ListenHost + ":" + strconv.Itoa(cfg.ListenPort),
		Handler:           engine,
		ReadHeaderTimeout: 30 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr, "dialect", dialect, "data_dir", cfg.DataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-stop:
	}

	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// 停 worker（等在途执行收尾），再停会话 loop，最后停 HTTP。
	workerCancel()
	worker.Stop(ctx)
	agentRT.Close()
	if err := srv.Shutdown(ctx); err != nil {
		return err
	}
	return closeDB(gdb)
}

func closeDB(gdb *gorm.DB) error {
	pool, err := gdb.DB()
	if err != nil {
		return nil
	}
	return pool.Close()
}

func gormLogLevel(level string) logger.LogLevel {
	switch level {
	case "debug":
		return logger.Info
	case "silent", "error":
		return logger.Error
	case "warn":
		return logger.Warn
	default:
		return logger.Warn
	}
}
