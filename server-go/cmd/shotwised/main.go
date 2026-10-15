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
		}, store))
	if err != nil {
		return err
	}

	rt := &app.Runtime{
		Config:  cfg,
		Log:     log,
		DB:      gdb,
		Dialect: dialect,
		Store:   store,
		Hub:     service.NewEventHub(),
		Auth:    authSvc,
		Agent:   agentRT,
		Assets:  assets,
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
	// 先停会话 loop，再停 HTTP：loop 收口时不再往已关的连接写事件。
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
