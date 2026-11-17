package valueconv

import (
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"
)

// SQLServerDecimalDigits returns the minimum exact integer and fractional
// capacities needed for a source value, without treating exponent notation or
// insignificant zeroes as extra digits.
func SQLServerDecimalDigits(v any) (integer, fraction int, err error) {
	if v == nil {
		return 0, 0, nil
	}
	n, err := decimal.NewFromString(strings.TrimSpace(OracleText(v)))
	if err != nil {
		return 0, 0, fmt.Errorf("无效或非有限的十进制值")
	}
	integer, fraction = sqlServerDecimalDigits(n)
	return integer, fraction, nil
}

func sqlServerDecimalDigits(n decimal.Decimal) (integer, fraction int) {
	if n.IsZero() {
		return 0, 0
	}
	digits := strings.TrimPrefix(n.Coefficient().String(), "-")
	shift := int(n.Exponent())
	return max(len(digits)+shift, 0), max(-(shift + len(digits) - len(strings.TrimRight(digits, "0"))), 0)
}

// ConvertSQLServerForSource checks the exact mapped type before database/sql
// binds a parameter. Explicit target casts can otherwise round or truncate.
func ConvertSQLServerForSource(v any, c source.ColumnInfo, src string) (any, error) {
	if src == "" || src == "mysql" {
		return ConvertSQLServer(v, c)
	}
	target, err := typemap.SQLServerTypeForSource(c, src)
	if err != nil {
		return nil, err
	}
	if v == nil {
		if !c.IsNullable {
			return nil, fmt.Errorf("列 %s 不允许 NULL", c.Name)
		}
		return nil, nil
	}
	typ, args := sqlServerTypeParts(target)
	s := OracleText(v)
	fail := func() (any, error) {
		return nil, fmt.Errorf("列 %s (%s): 数据超出 %s 范围或不能无损转换", c.Name, c.DataType, target)
	}
	switch typ {
	case "BIT":
		switch strings.ToLower(s) {
		case "t", "true", "1":
			return true, nil
		case "f", "false", "0":
			return false, nil
		}
	case "SMALLINT", "INT", "BIGINT":
		bits := map[string]int{"SMALLINT": 16, "INT": 32, "BIGINT": 64}[typ]
		if n, err := strconv.ParseInt(s, 10, bits); err == nil {
			return n, nil
		}
	case "DECIMAL":
		n, err := decimal.NewFromString(strings.TrimSpace(s))
		if err != nil {
			return fail()
		}
		integer, fraction := sqlServerDecimalDigits(n)
		if integer <= args[0]-args[1] && fraction <= args[1] {
			if n.IsZero() {
				return "0", nil
			}
			return n.StringFixed(int32(args[1])), nil
		}
	case "REAL", "FLOAT":
		bits, minMagnitude := 64, 0x1p-1022
		if typ == "REAL" {
			bits, minMagnitude = 32, 0x1p-126
		}
		f, err := strconv.ParseFloat(s, bits)
		if err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			if math.Abs(f) >= minMagnitude {
				return f, nil
			}
			if f == 0 {
				if exact, err := decimal.NewFromString(s); err == nil && exact.IsZero() {
					return f, nil
				}
			}
		}
	case "NVARCHAR":
		if !utf8.ValidString(s) || c.Length > 0 && int64(utf8.RuneCountInString(s)) > c.Length {
			return fail()
		}
		limit := 2147483647 / 2
		if len(args) != 0 {
			limit = args[0]
		}
		units := 0
		for _, r := range s {
			units++
			if r > 0xffff {
				units++
			}
		}
		if units <= limit {
			return s, nil
		}
	case "VARBINARY":
		b, ok := v.([]byte)
		if ok && len(b) <= 2147483647 && (len(args) == 0 || len(b) <= args[0]) {
			return append([]byte{}, b...), nil
		}
	case "UNIQUEIDENTIFIER":
		if b, ok := v.([]byte); ok && len(b) == 16 {
			h := hex.EncodeToString(b)
			s = h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
		}
		if uuidPattern.MatchString(s) {
			return strings.ToLower(s), nil
		}
	case "DATE", "DATETIME2", "DATETIMEOFFSET", "TIME":
		precision := 0
		if len(args) != 0 {
			precision = args[0]
		}
		if out, ok := convertSQLServerTemporal(v, s, typ, precision); ok {
			return out, nil
		}
	}
	return fail()
}

func sqlServerTypeParts(t string) (string, []int) {
	name, tail, ok := strings.Cut(t, "(")
	if !ok || tail == "MAX)" {
		return name, nil
	}
	var args []int
	for _, raw := range strings.Split(strings.TrimSuffix(tail, ")"), ",") {
		n, _ := strconv.Atoi(raw)
		args = append(args, n)
	}
	return name, args
}

func convertSQLServerTemporal(v any, s, typ string, precision int) (string, bool) {
	const dateLayout = "2006-01-02"
	const timeLayout = "15:04:05.9999999"
	const stampLayout = "2006-01-02T15:04:05.9999999"
	t, ok := v.(time.Time)
	if !ok {
		var layouts []string
		switch typ {
		case "DATE":
			layouts = []string{dateLayout}
		case "TIME":
			layouts = []string{timeLayout}
		case "DATETIME2":
			layouts = []string{stampLayout, "2006-01-02 15:04:05.999999999", dateLayout}
		case "DATETIMEOFFSET":
			layouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999 Z07:00", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999Z07"}
		}
		for _, layout := range layouts {
			var err error
			t, err = time.Parse(layout, s)
			if err == nil {
				ok = true
				break
			}
		}
		if !ok {
			return "", false
		}
	}
	if typ != "TIME" && (t.Year() < 1 || t.Year() > 9999) || t.Nanosecond()%int(math.Pow10(9-precision)) != 0 {
		return "", false
	}
	switch typ {
	case "DATE":
		return t.Format(dateLayout), t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0
	case "TIME":
		return t.Format(timeLayout), true
	case "DATETIMEOFFSET":
		_, offset := t.Zone()
		utcYear := t.UTC().Year()
		if offset%60 != 0 || offset < -14*60*60 || offset > 14*60*60 || utcYear < 1 || utcYear > 9999 {
			return "", false
		}
		return t.Format(stampLayout + "-07:00"), true
	default:
		return t.Format(stampLayout), true
	}
}
