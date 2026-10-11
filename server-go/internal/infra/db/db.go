// Package db 管理数据库连接与方言差异。
//
// 必须复刻的连接层差异（Python 侧踩过坑）：
//   - SQLite：WAL / busy_timeout=30000 / foreign_keys=ON
//   - PostgreSQL：池参数 pool_size=10, max_overflow=20, pool_recycle=3600
//   - SQLite 上时间戳列实际是 TEXT（Alembic 的类型统一迁移在 SQLite 上跳过了改类型），
//     因此读时必须同时接受 TEXT(ISO8601) / INTEGER(epoch) / REAL(julian)。
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Dialect 标识底层数据库类型。
type Dialect string

const (
	SQLite    Dialect = "sqlite"
	Postgres  Dialect = "postgres"
)

// Open 按 URL 打开连接。支持 sqlite://<path>、file:<path>、postgres://...、postgresql://...
func Open(databaseURL string, logLevel logger.LogLevel) (*gorm.DB, Dialect, error) {
	var dial gorm.Dialector
	var dialect Dialect

	switch {
	case strings.HasPrefix(databaseURL, "sqlite://"):
		path := strings.TrimPrefix(databaseURL, "sqlite://")
		dial = sqlite.Open(path)
		dialect = SQLite
	case strings.HasPrefix(databaseURL, "file:"):
		dial = sqlite.Open(databaseURL)
		dialect = SQLite
	case strings.HasPrefix(databaseURL, "postgres://"), strings.HasPrefix(databaseURL, "postgresql://"):
		dial = postgres.Open(databaseURL)
		dialect = Postgres
	default:
		return nil, "", fmt.Errorf("unsupported DATABASE_URL scheme: %s", databaseURL)
	}

	gdb, err := gorm.Open(dial, &gorm.Config{
		Logger:                                   logger.Default.LogMode(logLevel),
		TranslateError:                           true,
		PrepareStmt:                              false,
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		return nil, "", fmt.Errorf("open database: %w", err)
	}

	pool, err := gdb.DB()
	if err != nil {
		return nil, "", err
	}
	configurePool(pool, dialect)

	if dialect == SQLite {
		if err := applySQLitePragmas(gdb); err != nil {
			return nil, "", err
		}
	}
	return gdb, dialect, nil
}

func configurePool(pool *sql.DB, dialect Dialect) {
	pool.SetConnMaxLifetime(time.Hour)
	if dialect == Postgres {
		pool.SetMaxOpenConns(30) // pool_size 10 + max_overflow 20
		pool.SetMaxIdleConns(10)
	} else {
		// SQLite 的写并发上限是 1，多开连接只会增加 SQLITE_BUSY 概率。
		pool.SetMaxOpenConns(4)
		pool.SetMaxIdleConns(2)
	}
}

func applySQLitePragmas(gdb *gorm.DB) error {
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=30000",
		"PRAGMA foreign_keys=ON",
	} {
		if err := gdb.Exec(p).Error; err != nil {
			return fmt.Errorf("apply %q: %w", p, err)
		}
	}
	return nil
}

// IsUniqueViolation 判断是否为唯一约束冲突。
// 任务去重要靠它：Python 侧是在 enqueue 时捕获 IntegrityError 回查既有任务。
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// gorm 的 TranslateError 会把驱动错误包成 gorm.ErrDuplicatedKey。
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value violates unique constraint") ||
		strings.Contains(msg, "SQLITE_CONSTRAINT_UNIQUE")
}
