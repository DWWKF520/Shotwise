package repository

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/migrations"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// bookkeepingTables 是 Go 侧自己维护的表，不属于业务模型清单。
// agent_checkpoints 由 agent/checkpoint 存储经 AutoMigrate 内联建表（DDL 尚未收编进迁移体系）。
var bookkeepingTables = map[string]bool{"schema_migrations": true, "alembic_version": true, "agent_checkpoints": true}

// TestModelsCoverMigratedSchema 校验模型 ↔ 0001_init 基线双向一致：
// 每张表都有模型、每个字段都有列。CI 上就能跑，不依赖本地开发库。
func TestModelsCoverMigratedSchema(t *testing.T) {
	gdb := openMigratedDB(t, filepath.Join(t.TempDir(), "baseline.db"))
	assertModelsMatchDatabase(t, gdb)
}

// TestModelsReadExistingDatabase 是 P1 的完成判据：既有 projects/.shotwise.db
// （Alembic 建出来的，时间列混用 TEXT / DATETIME）必须能被逐表读出。
// 该文件是本地开发数据，不存在时跳过。
func TestModelsReadExistingDatabase(t *testing.T) {
	path := existingDatabasePath()
	if path == "" {
		t.Skip("projects/.shotwise.db not present locally")
	}
	gdb, dialect, err := db.Open("file:"+filepath.ToSlash(path)+"?mode=ro", logger.Silent)
	if err != nil {
		t.Fatalf("open %s read-only: %v", path, err)
	}
	t.Cleanup(func() {
		if pool, err := gdb.DB(); err == nil {
			_ = pool.Close()
		}
	})
	if dialect != db.SQLite {
		t.Fatalf("unexpected dialect %q", dialect)
	}
	assertModelsMatchDatabase(t, gdb)
}

func openMigratedDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+path, logger.Silent)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Migrate(gdb, dialect, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if pool, err := gdb.DB(); err == nil {
			_ = pool.Close()
		}
	})
	return gdb
}

// existingDatabasePath 从当前目录逐级上溯找 projects/.shotwise.db，
// 这样在 server-go/ 或仓库根目录下运行 go test 都能命中本地开发库。
func existingDatabasePath() string {
	const rel = "projects" + string(filepath.Separator) + ".shotwise.db"
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, rel)
		if st, err := os.Stat(candidate); err == nil && st.Size() > 0 {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func assertModelsMatchDatabase(t *testing.T, gdb *gorm.DB) {
	t.Helper()

	models := AllModels()
	tables := map[string]reflect.Type{}
	for _, m := range models {
		typ := reflect.TypeOf(m).Elem()
		name := m.(interface{ TableName() string }).TableName()
		if _, dup := tables[name]; dup {
			t.Fatalf("duplicate table name %q from %s", name, typ)
		}
		tables[name] = typ
	}

	dbTables, err := gdb.Migrator().GetTables()
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	for _, name := range dbTables {
		if bookkeepingTables[name] {
			continue
		}
		if _, ok := tables[name]; !ok {
			t.Errorf("table %q has no model in AllModels()", name)
		}
	}

	for name, typ := range tables {
		want := modelColumns(t, typ)
		got, err := tableColumns(gdb, name)
		if err != nil {
			t.Errorf("table %q: read columns: %v", name, err)
			continue
		}
		if diff := symmetricDiff(want, got); len(diff) > 0 {
			t.Errorf("table %q column mismatch, model=%v db=%v diff=%v", name, want, got, diff)
			continue
		}
		// 真读一行：验证列能被对应 Go 类型 Scan（时间列在此暴露 TEXT/INTEGER/REAL 漂移）
		if err := readOneRow(gdb, name, typ, want); err != nil {
			t.Errorf("table %q: read row: %v", name, err)
		}
	}
}

// modelColumns 取字段上的 column tag；没有 tag 的字段说明模型写漏了，
// 因为列名真相源是 DDL，不能靠 GORM 的命名推断。
func modelColumns(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var cols []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("gorm")
		col := ""
		for _, part := range strings.Split(tag, ";") {
			if v, ok := strings.CutPrefix(part, "column:"); ok {
				col = v
			}
		}
		if col == "" {
			t.Fatalf("field %s.%s has no explicit column tag", typ.Name(), f.Name)
		}
		cols = append(cols, col)
	}
	sort.Strings(cols)
	return cols
}

func tableColumns(gdb *gorm.DB, table string) ([]string, error) {
	rows, err := gdb.Table(table).Select("*").Limit(1).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	sort.Strings(cols)
	return cols, nil
}

func readOneRow(gdb *gorm.DB, table string, typ reflect.Type, cols []string) error {
	ptr := reflect.New(typ)
	// 双引号标识符在两库都合法，且能护住 key / value 这类保留字列名。
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = strconv.Quote(c)
	}
	return gdb.Table(table).Select(strings.Join(quoted, ", ")).Limit(1).Find(ptr.Interface()).Error
}

func symmetricDiff(a, b []string) []string {
	inB := map[string]bool{}
	for _, v := range b {
		inB[v] = true
	}
	var out []string
	for _, v := range a {
		if !inB[v] {
			out = append(out, "-"+v)
		}
	}
	inA := map[string]bool{}
	for _, v := range a {
		inA[v] = true
	}
	for _, v := range b {
		if !inA[v] {
			out = append(out, "+"+v)
		}
	}
	return out
}
