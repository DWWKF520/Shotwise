package db

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FlexibleTime 兼容 SQLite 上时间戳列的类型漂移。
//
// Python 侧的时间戳统一迁移（b942e8c5d545）在 SQLite 上跳过了改列类型，
// 因此 tasks/api_calls/worker_lease/agent_sessions 等表的时间列实际是 TEXT，
// 而 PostgreSQL 上同一列是 TIMESTAMPTZ。读时必须同时接受：
//
//	TEXT    "2026-03-17T08:00:00+00:00" / "2026-03-17 08:00:00"
//	INTEGER Unix epoch（秒）
//	REAL    Julian day
//	[]byte  上述任意一种的原始字节
//
// 写时统一输出 RFC3339Nano 的 UTC 字符串，两库都能正确排序。
type FlexibleTime struct {
	time.Time
	Valid bool
}

// NewFlexibleTime 从 time.Time 构造。
func NewFlexibleTime(t time.Time) FlexibleTime {
	return FlexibleTime{Time: t.UTC(), Valid: !t.IsZero()}
}

func (ft FlexibleTime) IsZero() bool { return !ft.Valid }

// Scan 实现 sql.Scanner。
func (ft *FlexibleTime) Scan(src any) error {
	if src == nil {
		ft.Valid = false
		return nil
	}
	switch v := src.(type) {
	case time.Time:
		ft.Time = v.UTC()
		ft.Valid = true
		return nil
	case string:
		return ft.parseString(v)
	case []byte:
		return ft.parseString(string(v))
	case int64:
		ft.Time = time.Unix(v, 0).UTC()
		ft.Valid = true
		return nil
	case float64:
		// SQLite 的 Julian day（含小数），出现于历史脏数据。
		ft.Time = julianToTime(v)
		ft.Valid = true
		return nil
	default:
		return fmt.Errorf("FlexibleTime: unsupported source type %T", src)
	}
}

func (ft *FlexibleTime) parseString(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		ft.Valid = false
		return nil
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			ft.Time = t.UTC()
			ft.Valid = true
			return nil
		}
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		ft.Time = julianToTime(n)
		ft.Valid = true
		return nil
	}
	return fmt.Errorf("FlexibleTime: unparseable value %q", s)
}

// Value 实现 driver.Valuer。
func (ft FlexibleTime) Value() (driver.Value, error) {
	if !ft.Valid {
		return nil, nil
	}
	return ft.Time.UTC().Format(time.RFC3339Nano), nil
}

var _ driver.Valuer = FlexibleTime{}
var _ sql.Scanner = (*FlexibleTime)(nil)

// julianToTime 把 SQLite Julian day 转成 UTC 时间。
// 参考 SQLite 的 julianday：Unix epoch = 2440587.5。
func julianToTime(d float64) time.Time {
	return time.Unix(int64((d-2440587.5)*86400), 0).UTC()
}

// ErrNotSupported 供上层识别。
var ErrNotSupported = errors.New("not supported")
