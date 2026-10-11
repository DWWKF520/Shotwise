package db

import (
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/shotwise/shotwise/migrations"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openTempSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, dialect, err := Open("sqlite://"+filepath.Join(t.TempDir(), "t.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open temp sqlite: %v", err)
	}
	if dialect != SQLite {
		t.Fatalf("dialect = %q, want %q", dialect, SQLite)
	}
	t.Cleanup(func() {
		if pool, err := gdb.DB(); err == nil {
			_ = pool.Close()
		}
	})
	return gdb
}

func TestSplitStatements(t *testing.T) {
	src := strings.Join([]string{
		`-- 注释里有 ; 分号`,
		`CREATE TABLE a (x VARCHAR DEFAULT 'semo;colon' NOT NULL);`,
		`/* 块注释 ; 也不切 */ INSERT INTO a VALUES ('a''b');`,
		`INSERT INTO a VALUES ("quoted;ident");`,
		``,
	}, "\n")

	got := splitStatements(src)
	if len(got) != 3 {
		t.Fatalf("got %d statements, want 3: %#v", len(got), got)
	}
	joined := strings.Join(got, "\n")
	for _, frag := range []string{"semo;colon", "'a''b'", `"quoted;ident"`} {
		if !strings.Contains(joined, frag) {
			t.Errorf("statement lost literal %q: %#v", frag, got)
		}
	}
}

func TestSplitMigrateSections(t *testing.T) {
	up, down := splitMigrateSections("-- head comment\nSELECT 1;\n-- +migrate Down\nSELECT 2;\n")
	if strings.TrimSpace(up) != "-- head comment\nSELECT 1;" {
		t.Errorf("up = %q", up)
	}
	if strings.TrimSpace(down) != "SELECT 2;" {
		t.Errorf("down = %q", down)
	}
}

func TestLoadMigrationsMergesDialectOverlay(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_a.sql":        &fstest.MapFile{Data: []byte("-- +migrate Up\nCREATE TABLE t (x INT);\n")},
		"0001_a.sqlite.sql": &fstest.MapFile{Data: []byte("-- +migrate Up\nCREATE INDEX i ON t (x);\n")},
		"0001_a.pg.sql":     &fstest.MapFile{Data: []byte("-- +migrate Up\nCREATE INDEX p ON t (x);\n")},
		"0002_b.sql":        &fstest.MapFile{Data: []byte("-- +migrate Up\nCREATE TABLE u (x INT);\n")},
		"README.md":         &fstest.MapFile{Data: []byte("ignored")},
		"0003_d.pg.sql":     &fstest.MapFile{Data: []byte("-- +migrate Up\nCREATE TABLE v (x INT);\n")},
	}

	sqliteMigs, err := LoadMigrations(fsys, SQLite)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(sqliteMigs) != 2 {
		t.Fatalf("got %d migrations, want 2", len(sqliteMigs))
	}
	if sqliteMigs[0].Version != "0001_a" {
		t.Errorf("version = %q", sqliteMigs[0].Version)
	}
	if len(sqliteMigs[0].Up) != 2 || !strings.Contains(sqliteMigs[0].Up[1], "CREATE INDEX i") {
		t.Errorf("sqlite overlay not merged in order: %#v", sqliteMigs[0].Up)
	}

	pgMigs, err := LoadMigrations(fsys, Postgres)
	if err != nil {
		t.Fatalf("load pg: %v", err)
	}
	if !strings.Contains(pgMigs[0].Up[1], "CREATE INDEX p") {
		t.Errorf("pg overlay wrong: %#v", pgMigs[0].Up)
	}
	if len(pgMigs) != 3 || pgMigs[2].Version != "0003_d" || len(pgMigs[2].Up) != 1 {
		t.Errorf("pg migration set wrong: %#v", pgMigs)
	}
}

func TestMigrateAppliesBaselineAndIsIdempotent(t *testing.T) {
	gdb := openTempSQLite(t)

	applied, err := Migrate(gdb, SQLite, migrations.FS())
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(applied.Applied) != 1 || applied.Applied[0] != "0001_init" {
		t.Fatalf("applied = %#v, want [0001_init]", applied)
	}
	if len(applied.Stamped) != 0 {
		t.Fatalf("stamped = %#v, want none on a fresh database", applied.Stamped)
	}

	var count int64
	if err := gdb.Table("tasks").Count(&count).Error; err != nil {
		t.Fatalf("tasks table not created: %v", err)
	}

	again, err := Migrate(gdb, SQLite, migrations.FS())
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(again.Applied)+len(again.Stamped) != 0 {
		t.Errorf("re-running applied %#v, want none", again)
	}

	states, err := MigrateStatus(gdb, SQLite, migrations.FS())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(states) != 1 || !states[0].Applied {
		t.Errorf("status = %#v, want 0001_init applied", states)
	}
}

func TestMigrateDownDropsBaseline(t *testing.T) {
	gdb := openTempSQLite(t)
	if _, err := Migrate(gdb, SQLite, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	version, err := MigrateDown(gdb, SQLite, migrations.FS())
	if err != nil {
		t.Fatalf("down: %v", err)
	}
	if version != "0001_init" {
		t.Fatalf("rolled back %q", version)
	}
	if gdb.Migrator().HasTable("tasks") {
		t.Error("tasks survived the down script")
	}
	var rows []SchemaMigration
	if err := gdb.Find(&rows).Error; err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("schema_migrations still holds %#v", rows)
	}

	if _, err := MigrateDown(gdb, SQLite, migrations.FS()); err == nil {
		t.Error("expected ErrNothingToRollback on empty ledger")
	}
}

// Alembic 建出来的库表已齐全，首跑只 stamp 基线、不重放 DDL。
func TestMigrateStampsAlembicDatabase(t *testing.T) {
	gdb := openTempSQLite(t)
	if err := gdb.Exec("CREATE TABLE alembic_version (version_num VARCHAR(128) NOT NULL, PRIMARY KEY (version_num))").Error; err != nil {
		t.Fatalf("create alembic_version: %v", err)
	}
	if err := gdb.Exec("INSERT INTO alembic_version VALUES (?)", AlembicHeadRevision).Error; err != nil {
		t.Fatalf("insert revision: %v", err)
	}

	res, err := Migrate(gdb, SQLite, migrations.FS())
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Errorf("applied %#v, want none (baseline is stamped, not replayed)", res.Applied)
	}
	if len(res.Stamped) != 1 || res.Stamped[0] != "0001_init" {
		t.Errorf("stamped = %#v, want [0001_init]", res.Stamped)
	}
	if gdb.Migrator().HasTable("tasks") {
		t.Error("baseline DDL was replayed onto an Alembic database")
	}
	var rows []SchemaMigration
	if err := gdb.Find(&rows).Error; err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if len(rows) != 1 || rows[0].Version != "0001_init" || !rows[0].Stamped {
		t.Errorf("ledger = %#v, want 0001_init stamped", rows)
	}

	// 基线是登记出来的，它的 down 会删掉 Python 侧还在用的表，必须拒绝。
	if _, err := MigrateDown(gdb, SQLite, migrations.FS()); err == nil {
		t.Error("expected down to refuse a stamped baseline")
	} else if !strings.Contains(err.Error(), "Alembic") {
		t.Errorf("down error = %v, want it to name the Alembic-managed schema", err)
	}
}

// 版本号不等于基线 head 时拒绝 stamp：静默跳过等于让服务跑在自己没校验过的 schema 上。
func TestMigrateRejectsStaleAlembicRevision(t *testing.T) {
	gdb := openTempSQLite(t)
	if err := gdb.Exec("CREATE TABLE alembic_version (version_num VARCHAR(128) NOT NULL, PRIMARY KEY (version_num))").Error; err != nil {
		t.Fatalf("create alembic_version: %v", err)
	}
	if err := gdb.Exec("INSERT INTO alembic_version VALUES ('some_older_head')").Error; err != nil {
		t.Fatalf("insert revision: %v", err)
	}

	if _, err := Migrate(gdb, SQLite, migrations.FS()); err == nil {
		t.Fatal("expected failure on non-baseline alembic revision")
	} else if !strings.Contains(err.Error(), "alembic upgrade head") {
		t.Errorf("error should tell the operator to upgrade, got: %v", err)
	}
}
