package valueconv

import (
	"fmt"
	"strings"
	"time"
)

const MySQLDamengZeroDateReplacement = "1900-01-01"

// MySQLDamengZeroDate recognizes complete and partial zero dates. Callers must
// restrict this check to date/datetime/timestamp columns, never text columns.
func MySQLDamengZeroDate(s string) bool {
	if len(s) < 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i := 0; i < 10; i++ {
		if i != 4 && i != 7 && (s[i] < '0' || s[i] > '9') {
			return false
		}
	}
	if s[:4] != "0000" && s[5:7] != "00" && s[8:10] != "00" {
		return false
	}
	if len(s) == 10 {
		return true
	}
	if len(s) > 26 {
		return false
	}
	// Invalid time text is an error, not another zero-date representation.
	_, err := time.Parse("2006-01-02 15:04:05.999999", MySQLDamengZeroDateReplacement+s[10:])
	return err == nil
}

// MySQLDamengDate preserves wall-clock values and microseconds. The dedicated
// Reader disables parseTime so partial zero dates are not normalized by MySQL's
// driver and valid 0001-01-01 values are distinguishable from zero dates.
func MySQLDamengDate(v interface{}, dt string) (interface{}, error) {
	if v == nil {
		return nil, nil
	}
	dt = strings.ToUpper(dt)
	if dt != "DATE" && dt != "DATETIME" && dt != "TIMESTAMP" {
		return v, nil
	}
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case []byte:
		s = string(x)
	case time.Time:
		if x.Nanosecond()%1000 != 0 {
			return nil, fmt.Errorf("%s 值超过 6 位小数秒", dt)
		}
		s = x.Format("2006-01-02 15:04:05.000000")
		if dt == "DATE" {
			s = x.Format("2006-01-02")
		}
	default:
		return nil, fmt.Errorf("%s 驱动值类型不支持: %T", dt, v)
	}
	if MySQLDamengZeroDate(s) {
		s = MySQLDamengZeroDateReplacement
		if dt != "DATE" {
			s += " 00:00:00"
		}
	}
	layout, format := "2006-01-02", "2006-01-02"
	if dt != "DATE" {
		layout, format = "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05.000000"
		if len(s) > 26 {
			return nil, fmt.Errorf("%s 值超过 6 位小数秒", dt)
		}
	}
	t, err := time.Parse(layout, s)
	if err != nil {
		return nil, fmt.Errorf("无效 %s 日期 %q: %w", dt, s, err)
	}
	return t.Format(format), nil
}

func MySQLDamengPlaceholder(dt string) string {
	switch strings.ToUpper(dt) {
	case "DATE":
		return "TO_DATE(?, 'YYYY-MM-DD')"
	case "DATETIME", "TIMESTAMP":
		return "TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS.FF6')"
	default:
		return "?"
	}
}
