package valueconv

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dbgold/datamigrate/source"
	"gitee.com/chunanyong/dm"
)

// OscarDameng converts the typed JDBC wire values to DM parameters. Keep
// source decimal, temporal and binary values out of implicit Go conversions.
func OscarDameng(value any, typ string) (any, error) {
	if value == nil {
		return nil, nil
	}
	base := source.OscarDamengColumnType(typ)
	switch base {
	case "BOOLEAN", "BOOL":
		if b, ok := value.(bool); ok {
			if b {
				return int64(1), nil
			}
			return int64(0), nil
		}
	case "BIT", "VARBIT", "BIT VARYING":
		if bits, ok := value.(string); ok {
			return source.OscarDamengBitBytes(bits)
		}
	case "DECIMAL", "NUMERIC", "NUMBER", "DEC":
		if raw, ok := value.(string); ok {
			return oracleDamengDecimal(raw)
		}
	case "FLOAT", "REAL", "FLOAT4", "FLOAT8", "DOUBLE", "DOUBLE PRECISION":
		if f, ok := value.(float64); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f, nil
		}
	case "DATE", "TIME", "TIME WITH TIME ZONE", "TIMESTAMP", "TIMESTAMP WITH TIME ZONE":
		if raw, ok := value.(string); ok {
			return oscarDamengTemporal(raw, base)
		}
	case "INTERVAL YEAR TO MONTH", "INTERVAL DAY TO SECOND":
		if raw, ok := value.(string); ok {
			return oscarDamengInterval(raw, base)
		}
	case "BLOB", "BINARY", "VARBINARY":
		if b, ok := value.([]byte); ok {
			if base == "BLOB" {
				return dm.NewBlob(b), nil
			}
			return b, nil
		}
	case "TEXT", "ARRAY", "CLOB", "XMLTYPE":
		if s, ok := value.(string); ok {
			if base == "XMLTYPE" {
				return OracleDameng(s, base)
			}
			return dm.NewClob(s), nil
		}
	case "TINYINT", "INT1", "SMALLINT", "INT2", "INT", "INTEGER", "INT4", "BIGINT", "INT8", "SMALLSERIAL", "SERIAL", "BIGSERIAL":
		if n, ok := value.(int64); ok {
			return n, nil
		}
	case "CHAR", `"CHAR"`, "CHARACTER", "BPCHAR", "NCHAR", "VARCHAR", "VARCHAR2", "CHARACTER VARYING", "NVARCHAR", "NVARCHAR2", "NAME":
		if s, ok := value.(string); ok {
			return s, nil
		}
	}
	return nil, fmt.Errorf("神通类型 %s 的 JDBC 值 %T 无法保真写入达梦", typ, value)
}

func OscarDamengPlaceholder(typ string) string {
	switch source.OscarDamengColumnType(typ) {
	case "DATE":
		return "CAST(TO_DATE(?, 'YYYY-MM-DD') AS DATE)"
	case "TIME":
		return "CAST(TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS.FF9') AS TIME(6))"
	case "TIME WITH TIME ZONE":
		return "CAST(CAST(? AS VARCHAR(64)) AS TIME(6) WITH TIME ZONE)"
	case "TIMESTAMP", "TIMESTAMP WITH TIME ZONE", "XMLTYPE":
		return OracleDamengPlaceholder(source.OscarDamengColumnType(typ))
	case "INTERVAL YEAR TO MONTH":
		return "TO_YMINTERVAL(?)"
	case "INTERVAL DAY TO SECOND":
		return "TO_DSINTERVAL(?)"
	default:
		return "?"
	}
}

var oscarDamengTimezone = regexp.MustCompile(`(?i)(Z|[+-][0-9]{1,2}(?::?[0-9]{2})?(?::[0-9]{2})?)$`)
var oscarDamengYearMonth = regexp.MustCompile(`^([+-]?[0-9]{1,9})-([0-9]{1,2})$`)
var oscarDamengDaySecond = regexp.MustCompile(`^([+-]?[0-9]{1,9}) ([0-9]{1,2}):([0-9]{1,2}):([0-9]{1,2})(?:\.([0-9]{1,9}))?$`)

func oscarDamengTemporal(raw, base string) (any, error) {
	raw = strings.TrimSpace(strings.Replace(raw, "T", " ", 1))
	if base == "DATE" {
		t, err := time.Parse("2006-01-02", raw)
		if err != nil {
			// Some JDBC implementations format a date as midnight timestamp.
			t, err = time.Parse("2006-01-02 15:04:05.999999999", raw)
			if err != nil || t.Hour() != 0 || t.Minute() != 0 || t.Second() != 0 || t.Nanosecond() != 0 {
				return nil, fmt.Errorf("无效 DATE 或日期包含非零时间 %q", raw)
			}
		}
		if _, err := OracleDameng(t, "DATE"); err != nil {
			return nil, err
		}
		return t.Format("2006-01-02"), nil
	}
	withTimeOnly := base == "TIME" || base == "TIME WITH TIME ZONE"
	if withTimeOnly {
		raw = "2000-01-01 " + raw
	}
	dt := "TIMESTAMP"
	if strings.HasSuffix(base, "WITH TIME ZONE") {
		zone := oscarDamengTimezone.FindStringIndex(raw)
		if zone == nil || zone[0] <= len("2000-01-01 00:00:00")-1 {
			return nil, fmt.Errorf("%s 缺少明确时区偏移 %q", base, raw)
		}
		offset, err := oscarDamengOffset(raw[zone[0]:])
		if err != nil {
			return nil, err
		}
		raw = strings.TrimSpace(raw[:zone[0]]) + " " + offset
		dt = "TIMESTAMP WITH TIME ZONE"
	}
	result, err := OracleDameng(raw, dt)
	if err == nil && withTimeOnly {
		// OracleDameng returns exactly nine digits for FF9. TIME supports six.
		canonical := result.(string)
		if canonical[26:29] != "000" {
			return nil, fmt.Errorf("%s 超过达梦 TIME 的六位小数秒精度", base)
		}
		if dt == "TIMESTAMP WITH TIME ZONE" {
			return canonical[11:26] + canonical[29:], nil
		}
	}
	return result, err
}

func oscarDamengOffset(raw string) (string, error) {
	if strings.EqualFold(raw, "Z") {
		return "+00:00", nil
	}
	parts := strings.Split(raw[1:], ":")
	if len(parts) == 1 && len(parts[0]) > 2 {
		n := len(parts[0]) - 2
		parts = []string{parts[0][:n], parts[0][n:]}
	}
	if len(parts) == 3 && parts[2] != "00" {
		return "", fmt.Errorf("达梦时区偏移须为整分钟: %q", raw)
	}
	h, _ := strconv.Atoi(parts[0])
	m := 0
	if len(parts) > 1 {
		m, _ = strconv.Atoi(parts[1])
	}
	if m >= 60 || raw[0] == '-' && h > 12 || raw[0] == '+' && (h > 14 || h == 14 && m != 0) {
		return "", fmt.Errorf("时区偏移超出达梦范围: %q", raw)
	}
	return fmt.Sprintf("%c%02d:%02d", raw[0], h, m), nil
}

func oscarDamengInterval(raw, base string) (string, error) {
	raw = strings.TrimSpace(raw)
	if base == "INTERVAL YEAR TO MONTH" {
		if m := oscarDamengYearMonth.FindStringSubmatch(raw); m != nil {
			month, _ := strconv.Atoi(m[2])
			if month <= 11 {
				return fmt.Sprintf("%s-%02d", m[1], month), nil
			}
		}
	} else if m := oscarDamengDaySecond.FindStringSubmatch(raw); m != nil {
		hour, _ := strconv.Atoi(m[2])
		minute, _ := strconv.Atoi(m[3])
		second, _ := strconv.Atoi(m[4])
		fraction := strings.TrimRight(m[5], "0")
		if hour < 24 && minute < 60 && second < 60 && len(fraction) <= 6 {
			canonical := fmt.Sprintf("%s %02d:%02d:%02d", m[1], hour, minute, second)
			if fraction != "" {
				canonical += "." + fraction
			}
			return canonical, nil
		}
	}
	return "", fmt.Errorf("无效或超出达梦精度的 %s 值 %q", base, raw)
}
