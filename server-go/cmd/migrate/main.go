// Command migrate 管理数据库迁移：up / down / status。
//
// 用法：
//
//	migrate [up|down|status]
//
// 数据库连接取自 DATABASE_URL（缺省为数据目录下的 SQLite 库），与主服务同源。
// 首次对 Alembic 建出来的既有库执行 up 时只 stamp 基线，不重放 DDL。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/migrations"
	"gorm.io/gorm/logger"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	flag.Parse()
	cmd := "up"
	if a := flag.Arg(0); a != "" {
		cmd = a
	}
	if cmd != "up" && cmd != "down" && cmd != "status" {
		return fmt.Errorf("unknown command %q (want up | down | status)", cmd)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	gdb, dialect, err := db.Open(cfg.DatabaseURL, logger.Warn)
	if err != nil {
		return err
	}
	pool, err := gdb.DB()
	if err == nil {
		defer pool.Close() //nolint:errcheck // 只读命令行工具，关闭失败不影响结果
	}

	switch cmd {
	case "up":
		res, err := db.Migrate(gdb, dialect, migrations.FS())
		if err != nil {
			return err
		}
		for _, v := range res.Stamped {
			fmt.Printf("stamped %s (schema already present from Alembic)\n", v)
		}
		for _, v := range res.Applied {
			fmt.Printf("applied %s\n", v)
		}
		if len(res.Stamped)+len(res.Applied) == 0 {
			fmt.Printf("database %s is up to date (%s)\n", cfg.DatabaseURL, dialect)
		}
		return nil
	case "down":
		version, err := db.MigrateDown(gdb, dialect, migrations.FS())
		if err != nil {
			return err
		}
		fmt.Printf("rolled back %s\n", version)
		return nil
	default:
		states, err := db.MigrateStatus(gdb, dialect, migrations.FS())
		if err != nil {
			return err
		}
		for _, s := range states {
			mark := "pending"
			if s.Applied {
				mark = "applied"
			}
			fmt.Printf("%-40s %s\n", s.Version, mark)
		}
		return nil
	}
}
