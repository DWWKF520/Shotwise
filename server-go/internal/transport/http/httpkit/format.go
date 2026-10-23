package httpkit

import (
	"time"
)

// IsoTimestamp 近似 Python datetime.isoformat()：小数秒按微秒输出，为 0 时省略。
func IsoTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.999999-07:00")
}

// ItoaInt 是索引转字符串（只用于校验项的 loc）。
func ItoaInt(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	negative := value < 0
	if negative {
		value = -value
	}
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	if negative {
		return "-" + digits
	}
	return digits
}
