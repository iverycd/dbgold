package valueconv

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
	"github.com/shopspring/decimal"
	goora "github.com/sijms/go-ora/v2"
	"github.com/sijms/go-ora/v2/converters"
)

// PGOracleNumber validates significant digits and exponent before binding.
// go-ora's Number constructor alone does not reject Oracle overflow/rounding.
func PGOracleNumber(s string, precision, scale int64) (*goora.Number, error) {
	n, err := decimal.NewFromString(s)
	if err != nil {
		return nil, fmt.Errorf("非有限或非法 NUMERIC")
	}
	if n.IsZero() {
		return goora.NewNumberFromString("0")
	}
	if !n.IsZero() {
		digits := strings.TrimPrefix(n.Coefficient().String(), "-")
		exponent := int64(n.Exponent()) + int64(len(digits)) - 1
		if len(strings.TrimRight(digits, "0")) > 38 || exponent < -130 || exponent > 125 {
			return nil, fmt.Errorf("数值超出 Oracle NUMBER 的 38 位有效数字或指数范围，拒绝舍入/下溢")
		}
	}
	if precision > 0 {
		if precision > 38 || scale < -84 || scale > 127 {
			return nil, fmt.Errorf("目标 NUMBER 精度无效")
		}
		// Shift is exact and handles negative scales as well as scale > precision.
		scaled := n.Shift(int32(scale))
		if !scaled.Equal(scaled.Truncate(0)) || scaled.Abs().GreaterThanOrEqual(decimal.New(1, int32(precision))) {
			return nil, fmt.Errorf("数值超出 NUMBER(%d,%d)，拒绝舍入或截断", precision, scale)
		}
	}
	// Scientific input avoids go-ora retaining leading fractional zero pairs in
	// its NUMBER buffer (which can exceed Oracle's wire capacity for tiny values).
	if n.IsZero() {
		return goora.NewNumberFromString("0")
	}
	coefficient := n.Coefficient().String()
	trimmed := strings.TrimRight(coefficient, "0")
	exponent := int64(n.Exponent()) + int64(len(coefficient)-len(trimmed))
	return goora.NewNumberFromString(trimmed + "e" + strconv.FormatInt(exponent, 10))
}

// ConvertPGOracle retains source constraints and is deliberately separate from
// the existing MySQL converter.
func ConvertPGOracle(v any, src source.ColumnInfo, dst OracleColumn, codec converters.IStringConverter) (any, string, error) {
	if v == nil {
		if !src.IsNullable || !dst.Nullable {
			return nil, "", fmt.Errorf("非空列不能接收 NULL")
		}
		return nil, "", nil
	}
	dt, target := typemap.PGOracleBaseType(src.DataType), strings.ToUpper(dst.DataType)
	s := OracleText(v)
	fail := func(message string) (any, string, error) { return nil, "", fmt.Errorf("%s", message) }
	switch dt {
	case "int2", "int4", "int8", "bool", "numeric":
		if target != "NUMBER" {
			return fail("PG 数值列必须写入 Oracle NUMBER")
		}
		switch dt {
		case "int2", "int4", "int8":
			bits := map[string]int{"int2": 16, "int4": 32, "int8": 64}[dt]
			n, err := strconv.ParseInt(s, 10, bits)
			if err != nil {
				return fail("源整数范围越界")
			}
			s = strconv.FormatInt(n, 10)
		case "bool":
			switch strings.ToLower(s) {
			case "true", "t", "1":
				s = "1"
			case "false", "f", "0":
				s = "0"
			default:
				return fail("非法 PG 布尔值")
			}
		case "numeric":
			if _, err := PGOracleNumber(s, src.Precision, src.Scale); err != nil {
				return nil, "", err
			}
		}
		precision := dst.Precision
		if precision == 0 && (dst.ScaleKnown || dst.Scale != 0) {
			precision = 38
		}
		n, err := PGOracleNumber(s, precision, dst.Scale)
		return n, "", err
	case "float4", "float8":
		if target != "BINARY_FLOAT" && target != "BINARY_DOUBLE" {
			return fail("PG 浮点列必须写入 Oracle BINARY_FLOAT/BINARY_DOUBLE")
		}
		bits := 64
		if dt == "float4" {
			bits = 32
		}
		n, err := strconv.ParseFloat(s, bits)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return fail("非有限或越界 PG 浮点值")
		}
		if target == "BINARY_FLOAT" && float64(float32(n)) != n {
			return fail("目标 BINARY_FLOAT 无法无损保留源浮点值")
		}
		return ConvertOracle(n, dt, dst, codec)
	case "date", "timestamp", "timestamptz":
		t, err := PGOracleTime(v, src)
		if err != nil {
			return nil, "", err
		}
		if dt == "timestamptz" {
			if !strings.Contains(target, "WITH TIME ZONE") || strings.Contains(target, "LOCAL") {
				return fail("timestamptz 必须写入 TIMESTAMP WITH TIME ZONE")
			}
			if err := pgOracleTimePrecision(t, dst.Scale); err != nil {
				return nil, "", err
			}
			return goora.TimeStampTZ(t.UTC()), "", nil
		}
		if target != "DATE" && (!strings.HasPrefix(target, "TIMESTAMP") || strings.Contains(target, "TIME ZONE")) {
			return fail("源无时区日期不能隐式转换为目标时区类型")
		}
		if target == "DATE" && t.Nanosecond() != 0 {
			return fail("DATE 无法保留小数秒")
		}
		if target != "DATE" {
			if err := pgOracleTimePrecision(t, dst.Scale); err != nil {
				return nil, "", err
			}
		}
		return goora.TimeStamp(t), "", nil
	case "bytea":
		if target != "BLOB" {
			return fail("bytea 必须写入 BLOB")
		}
		return ConvertOracle(v, "bytea", dst, codec)
	case "text", "varchar", "bpchar", "json", "jsonb", "xml", "uuid", "inet", "cidr", "macaddr", "macaddr8", "bit", "varbit":
		if !utf8.ValidString(s) {
			return fail("源文本不是有效 UTF-8")
		}
		if src.Length > 0 && int64(utf8.RuneCountInString(s)) > src.Length {
			return fail("文本超过源列字符长度约束")
		}
		switch dt {
		case "json", "jsonb":
			if !json.Valid([]byte(s)) {
				return fail("非法 JSON")
			}
		case "uuid":
			if !uuidPattern.MatchString(s) {
				return fail("非法 UUID")
			}
		case "inet", "cidr":
			if strings.Contains(s, "/") {
				prefix, err := netip.ParsePrefix(s)
				if err != nil {
					return fail("非法网络地址")
				}
				if dt == "cidr" && prefix != prefix.Masked() {
					return fail("CIDR 含非零主机位")
				}
			} else if _, err := netip.ParseAddr(s); err != nil {
				return fail("非法网络地址")
			}
		case "macaddr", "macaddr8":
			mac, err := net.ParseMAC(s)
			want := 6
			if dt == "macaddr8" {
				want = 8
			}
			if err != nil || len(mac) != want {
				return fail("非法 MAC 地址")
			}
		case "bit", "varbit":
			if strings.Trim(s, "01") != "" || dt == "bit" && int64(len(s)) != max(src.Length, 1) {
				return fail("位串长度或内容不符合源类型")
			}
		}
		switch target {
		case "VARCHAR2", "NVARCHAR2", "CHAR", "NCHAR", "CLOB", "NCLOB":
		default:
			return fail("PG 文本列的目标类型不兼容")
		}
		if s == "" && target != "CLOB" && target != "NCLOB" {
			if !src.IsNullable || !dst.Nullable {
				return fail("非空列不能将空字符串转换为 NULL")
			}
			return nil, "空字符串转 NULL", nil
		}
		if target == "NVARCHAR2" || target == "NCHAR" {
			units := 0
			for _, r := range s {
				units++
				if r > 0xffff {
					units++
				}
			}
			if dst.CharLength > 0 && int64(units) > dst.CharLength {
				return fail("文本超过目标国家字符集代码单元长度")
			}
		}
		return ConvertOracle(s, "TEXT", dst, codec)
	default:
		return fail("PG 类型尚不支持 Oracle 数据转换")
	}
}

func pgOracleTimePrecision(t time.Time, precision int64) error {
	if precision < 0 || precision > 9 || t.Nanosecond()%int(math.Pow10(9-int(precision))) != 0 {
		return fmt.Errorf("时间值超过目标 TIMESTAMP 精度")
	}
	return nil
}

// PGOracleTime rejects infinity, BC and out-of-range values without a NULL fallback.
func PGOracleTime(v any, c source.ColumnInfo) (time.Time, error) {
	dt := typemap.PGOracleBaseType(c.DataType)
	t, ok := v.(time.Time)
	if !ok {
		layouts := []string{"2006-01-02 15:04:05.999999999", "2006-01-02"}
		if dt == "timestamptz" {
			layouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999-07", "2006-01-02 15:04:05.999999999-07:00:00"}
		}
		var err error
		for _, layout := range layouts {
			t, err = time.Parse(layout, OracleText(v))
			if err == nil {
				break
			}
		}
		if err != nil {
			return time.Time{}, fmt.Errorf("日期时间不可表示（不支持 infinity/BC/隐式时区）")
		}
	}
	if dt == "timestamptz" {
		t = t.UTC()
	}
	if t.Year() < 1 || t.Year() > 9999 {
		return time.Time{}, fmt.Errorf("日期超出 Oracle 迁移支持的 AD 1..9999 范围")
	}
	if dt == "date" {
		if t.Hour() != 0 || t.Minute() != 0 || t.Second() != 0 || t.Nanosecond() != 0 {
			return time.Time{}, fmt.Errorf("DATE 源值含时间部分")
		}
	} else if err := pgOracleTimePrecision(t, c.DateTimePrecision); err != nil {
		return time.Time{}, err
	}
	return t, nil
}
