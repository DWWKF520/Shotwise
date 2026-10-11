package db

// 迁移执行器。
//
// 设计约束（来自 docs/go-migration/02-data-layer.md）：
//   - 不复刻 Alembic 的 78 个迁移，而是按实测 DDL 一次性建 0001_init 基线；
//   - 版本按文件名字典序执行（编号零填充），每个版本一个事务；
//   - 记账表 schema_migrations 只存已应用的版本；
//   - 灰度期数据库可能是 Alembic 建出来的（表已齐全），此时只 stamp 基线、不跑 DDL。

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// AlembicHeadRevision 是 0001_init 基线所对应的 Python 侧 Alembic head。
// 既有库的 alembic_version 不等于它时拒绝 stamp —— 此时基线 DDL 与该库并不等价，
// 静默跳过会让 Go 服务跑在一个自己没校验过的 schema 上。
const AlembicHeadRevision = "20261008_render_job_leases"

// ErrNothingToRollback 表示没有可回滚的已应用版本。
var ErrNothingToRollback = errors.New("db: no applied migration to roll back")

// SchemaMigration 是迁移记账表。这张表由 Go 侧独占，因此可以走 AutoMigrate。
type SchemaMigration struct {
	Version   string       `gorm:"primaryKey;column:version;size:128"`
	AppliedAt FlexibleTime `gorm:"column:applied_at;not null"`
	// Stamped 为真表示这一版是登记而非执行出来的（表已由 Alembic 建好）。
	// down 依赖它拒绝在 Python 管的库上重放 DROP。
	Stamped bool `gorm:"column:stamped;not null;default:false"`
}

func (SchemaMigration) TableName() string { return "schema_migrations" }

// Migration 是一个已加载版本在本方言下的完整脚本。
type Migration struct {
	Version string
	Up      []string
	Down    []string
}

// MigrationState 是 status 输出的单项。
type MigrationState struct {
	Version string
	Applied bool
}

// suffix 返回该方言的覆盖文件名后缀。
func (d Dialect) suffix() string {
	if d == SQLite {
		return "sqlite"
	}
	return "pg"
}

// LoadMigrations 读取 fsys 中属于该方言的迁移，按版本升序合并主体与覆盖文件。
func LoadMigrations(fsys fs.FS, dialect Dialect) ([]Migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("glob migrations: %w", err)
	}
	if len(names) == 0 {
		return nil, errors.New("db: no migration files found")
	}

	type script struct{ up, down []string }
	byVersion := map[string]*struct {
		base    script
		overlay script
	}{}

	for _, name := range names {
		version, isOverlay, ok := splitMigrationName(path.Base(name), dialect.suffix())
		if !ok {
			continue // 其他方言的覆盖文件
		}
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		upSrc, downSrc := splitMigrateSections(string(raw))
		s := script{up: splitStatements(upSrc), down: splitStatements(downSrc)}
		if len(s.up) == 0 && len(s.down) == 0 {
			return nil, fmt.Errorf("migration %s has no statements", name)
		}
		entry := byVersion[version]
		if entry == nil {
			entry = &struct {
				base    script
				overlay script
			}{}
			byVersion[version] = entry
		}
		if isOverlay {
			entry.overlay = s
		} else {
			entry.base = s
		}
	}

	versions := make([]string, 0, len(byVersion))
	for v := range byVersion {
		versions = append(versions, v)
	}
	sort.Strings(versions)

	out := make([]Migration, 0, len(versions))
	for _, v := range versions {
		e := byVersion[v]
		out = append(out, Migration{
			Version: v,
			Up:      append(append([]string{}, e.base.up...), e.overlay.up...),
			Down:    append(append([]string{}, e.base.down...), e.overlay.down...),
		})
	}
	return out, nil
}

// splitMigrationName 解析文件名，返回版本号与是否为当前方言的覆盖文件。
// 非当前方言的覆盖文件返回 ok=false。
func splitMigrationName(file, dialectSuffix string) (version string, isOverlay, ok bool) {
	stem := strings.TrimSuffix(file, ".sql")
	if strings.HasSuffix(stem, "."+dialectSuffix) {
		return strings.TrimSuffix(stem, "."+dialectSuffix), true, true
	}
	for _, other := range []string{"sqlite", "pg"} {
		if other != dialectSuffix && strings.HasSuffix(stem, "."+other) {
			return "", false, false
		}
	}
	return stem, false, true
}

// MigrateResult 区分「跑了 DDL」与「只登记基线」，两者对运维的含义不同。
type MigrateResult struct {
	Applied []string
	Stamped []string
}

// Migrate 应用所有未应用的版本。
func Migrate(gdb *gorm.DB, dialect Dialect, fsys fs.FS) (MigrateResult, error) {
	migrations, err := LoadMigrations(fsys, dialect)
	if err != nil {
		return MigrateResult{}, err
	}
	if err := ensureLedger(gdb); err != nil {
		return MigrateResult{}, err
	}

	var result MigrateResult
	applied, err := appliedVersions(gdb)
	if err != nil {
		return MigrateResult{}, err
	}
	if len(applied) == 0 {
		rev, err := alembicRevision(gdb)
		if err != nil {
			return MigrateResult{}, err
		}
		if rev != "" {
			if rev != AlembicHeadRevision {
				return MigrateResult{}, fmt.Errorf("db: alembic revision %q is not the baseline head %q; run `alembic upgrade head` first", rev, AlembicHeadRevision)
			}
			if len(migrations) == 0 {
				return MigrateResult{}, errors.New("db: no migrations to stamp")
			}
			base := migrations[0]
			if err := recordVersion(gdb, base.Version, true); err != nil {
				return MigrateResult{}, err
			}
			result.Stamped = append(result.Stamped, base.Version)
			applied = []string{base.Version}
			migrations = migrations[1:]
		}
	}

	for _, m := range migrations {
		if contains(applied, m.Version) {
			continue
		}
		if err := runInTx(gdb, m.Up, func(tx *gorm.DB) error {
			return tx.Create(&SchemaMigration{Version: m.Version, AppliedAt: NewFlexibleTime(time.Now().UTC())}).Error
		}); err != nil {
			return result, fmt.Errorf("apply migration %s: %w", m.Version, err)
		}
		result.Applied = append(result.Applied, m.Version)
	}
	return result, nil
}

// ensureLedger 建出记账表。status / down 也要先调它：既有库可能还没有这张表。
func ensureLedger(gdb *gorm.DB) error {
	if err := gdb.AutoMigrate(&SchemaMigration{}); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	return nil
}

// MigrateStatus 返回每个版本的 Applied 状态（按版本升序）。
func MigrateStatus(gdb *gorm.DB, dialect Dialect, fsys fs.FS) ([]MigrationState, error) {
	migrations, err := LoadMigrations(fsys, dialect)
	if err != nil {
		return nil, err
	}
	if err := ensureLedger(gdb); err != nil {
		return nil, err
	}
	applied, err := appliedVersions(gdb)
	if err != nil {
		return nil, err
	}
	out := make([]MigrationState, 0, len(migrations))
	for _, m := range migrations {
		out = append(out, MigrationState{Version: m.Version, Applied: contains(applied, m.Version)})
	}
	return out, nil
}

// MigrateDown 回滚最近一个已应用版本，返回其版本号。
func MigrateDown(gdb *gorm.DB, dialect Dialect, fsys fs.FS) (string, error) {
	migrations, err := LoadMigrations(fsys, dialect)
	if err != nil {
		return "", err
	}
	if err := ensureLedger(gdb); err != nil {
		return "", err
	}
	applied, err := appliedVersions(gdb)
	if err != nil {
		return "", err
	}
	if len(applied) == 0 {
		return "", ErrNothingToRollback
	}

	target := applied[len(applied)-1]
	var row SchemaMigration
	if err := gdb.Where("version = ?", target).First(&row).Error; err != nil {
		return "", fmt.Errorf("read ledger for %s: %w", target, err)
	}
	if row.Stamped {
		return "", fmt.Errorf("db: version %s was stamped onto an existing Alembic database, its down script would drop Python-managed tables", target)
	}
	for _, m := range migrations {
		if m.Version != target {
			continue
		}
		if len(m.Down) == 0 {
			return "", fmt.Errorf("db: migration %s has no down script", m.Version)
		}
		if err := runInTx(gdb, m.Down, func(tx *gorm.DB) error {
			return tx.Where("version = ?", m.Version).Delete(&SchemaMigration{}).Error
		}); err != nil {
			return "", fmt.Errorf("roll back migration %s: %w", m.Version, err)
		}
		return m.Version, nil
	}
	return "", fmt.Errorf("db: applied version %s is not present in the migration files", target)
}

func runInTx(gdb *gorm.DB, stmts []string, finalize func(*gorm.DB) error) error {
	tx := gdb.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	for _, stmt := range stmts {
		if err := tx.Exec(stmt).Error; err != nil {
			tx.Rollback()
			return fmt.Errorf("statement %q: %w", firstLine(stmt), err)
		}
	}
	if finalize != nil {
		if err := finalize(tx); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit().Error
}

func recordVersion(gdb *gorm.DB, version string, stamped bool) error {
	return gdb.Create(&SchemaMigration{
		Version:   version,
		AppliedAt: NewFlexibleTime(time.Now().UTC()),
		Stamped:   stamped,
	}).Error
}

func appliedVersions(gdb *gorm.DB) ([]string, error) {
	var rows []SchemaMigration
	if err := gdb.Order("version").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Version)
	}
	sort.Strings(out)
	return out, nil
}

// alembicRevision 返回既有库的 Alembic 版本号；表不存在或无行时返回空串。
func alembicRevision(gdb *gorm.DB) (string, error) {
	if !gdb.Migrator().HasTable("alembic_version") {
		return "", nil
	}
	var rows []struct {
		VersionNum string `gorm:"column:version_num"`
	}
	if err := gdb.Table("alembic_version").Select("version_num").Scan(&rows).Error; err != nil {
		return "", fmt.Errorf("read alembic_version: %w", err)
	}
	if len(rows) == 0 {
		return "", nil
	}
	if len(rows) > 1 {
		return "", fmt.Errorf("db: alembic_version has %d rows (unmerged heads?), cannot baseline", len(rows))
	}
	return rows[0].VersionNum, nil
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func firstLine(stmt string) string {
	if i := strings.IndexAny(stmt, "\r\n"); i >= 0 {
		return strings.TrimSpace(stmt[:i]) + " …"
	}
	return strings.TrimSpace(stmt)
}

// splitMigrateSections 按 `-- +migrate Up` / `-- +migrate Down` 标记切段。
// 没有标记的内容归入 Up，与 goose 的默认语义一致。
func splitMigrateSections(src string) (up, down string) {
	lines := strings.Split(src, "\n")
	var upBuf, downBuf strings.Builder
	section := "up"
	for _, line := range lines {
		if m := migrateSectionRE.FindStringSubmatch(line); m != nil {
			section = strings.ToLower(m[1])
			continue
		}
		if section == "down" {
			downBuf.WriteString(line)
			downBuf.WriteByte('\n')
		} else {
			upBuf.WriteString(line)
			upBuf.WriteByte('\n')
		}
	}
	return upBuf.String(), downBuf.String()
}

var migrateSectionRE = regexp.MustCompile(`(?im)^\s*--\s*\+migrate\s+(up|down)\s*$`)

// splitStatements 按语句分隔符切分，跳过字符串字面量、标识符引用与注释。
// 两边驱动都不允许一次 Exec 混多条语句（pgx 走 prepared、modernc 亦受限），
// 所以必须在 Go 侧切准；用 regexp 或 strings.Split(";") 会在
// DEFAULT 'a;b'、$$ 之类字面量里断错。
func splitStatements(src string) []string {
	var (
		out   []string
		cur   strings.Builder
		quote byte
	)
	b := []byte(src)
	for i := 0; i < len(b); {
		c := b[i]
		if quote != 0 {
			cur.WriteByte(c)
			if c == quote {
				// SQL 里连续两个同种引号是转义，不结束字面量
				if i+1 < len(b) && b[i+1] == quote {
					cur.WriteByte(quote)
					i += 2
					continue
				}
				quote = 0
			}
			i++
			continue
		}
		switch {
		case c == '-' && i+1 < len(b) && b[i+1] == '-':
			if j := bytes.IndexByte(b[i:], '\n'); j >= 0 {
				i += j // 保留换行，便于按行定位错误
				continue
			}
			i = len(b)
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			end := i + 2
			for end+1 < len(b) && !(b[end] == '*' && b[end+1] == '/') {
				end++
			}
			if end+1 < len(b) {
				i = end + 2
			} else {
				i = len(b)
			}
		case c == '\'' || c == '"':
			quote = c
			cur.WriteByte(c)
			i++
		case c == ';':
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
			i++
		default:
			cur.WriteByte(c)
			i++
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}
