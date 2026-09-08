package valueconv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dbgold/datamigrate/source"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// PGMySQLValue is used by both full-data preflight and the actual copy. Errors
// carry column identity at the caller, never the potentially sensitive value.
func PGMySQLValue(v any, c source.ColumnInfo) (any, error) {
	if v == nil {
		if !c.IsNullable {
			return nil, fmt.Errorf("NOT NULL 列含 NULL")
		}
		return nil, nil
	}
	dt := strings.ToLower(c.DataType)
	str := func() string {
		if b, ok := v.([]byte); ok {
			return string(b)
		}
		return fmt.Sprint(v)
	}
	switch dt {
	case "bytea":
		if b, ok := v.([]byte); ok {
			return b, nil
		}
		return nil, fmt.Errorf("bytea 未以二进制返回")
	case "bool", "boolean":
		if b, ok := v.(bool); ok {
			if b {
				return int64(1), nil
			}
			return int64(0), nil
		}
		switch strings.ToLower(str()) {
		case "t", "true", "1":
			return int64(1), nil
		case "f", "false", "0":
			return int64(0), nil
		}
		return nil, fmt.Errorf("非法布尔值")
	case "int2", "smallint", "int4", "integer", "int", "int8", "bigint":
		bits := 64
		if dt == "int2" || dt == "smallint" {
			bits = 16
		}
		if dt == "int4" || dt == "integer" || dt == "int" {
			bits = 32
		}
		n, e := strconv.ParseInt(str(), 10, bits)
		if e != nil {
			return nil, fmt.Errorf("整数越界")
		}
		return n, nil
	case "numeric", "decimal":
		s := str()
		rat, ok := new(big.Rat).SetString(s)
		if !ok {
			return nil, fmt.Errorf("非有限或非法 NUMERIC")
		}
		factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(c.Scale), nil)
		scaled := new(big.Rat).Mul(rat, new(big.Rat).SetInt(factor))
		if !scaled.IsInt() {
			return nil, fmt.Errorf("小数精度损失")
		}
		limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(c.Precision), nil)
		if new(big.Int).Abs(scaled.Num()).Cmp(limit) >= 0 {
			return nil, fmt.Errorf("DECIMAL 越界")
		}
		return rat.FloatString(int(c.Scale)), nil
	case "float4", "float8", "real", "double precision":
		n, e := strconv.ParseFloat(str(), 64)
		if e != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, fmt.Errorf("非有限浮点值")
		}
		if dt == "float4" || dt == "real" {
			if math.IsInf(float64(float32(n)), 0) {
				return nil, fmt.Errorf("FLOAT 越界")
			}
			n = float64(float32(n))
		}
		if n == 0 {
			n = 0
		} // Canonicalize negative zero for MySQL key comparisons.
		return n, nil
	case "timestamp", "timestamptz", "date":
		var t time.Time
		if x, ok := v.(time.Time); ok {
			t = x
		} else {
			var err error
			for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999-07", "2006-01-02 15:04:05.999999999", "2006-01-02"} {
				t, err = time.Parse(layout, str())
				if err == nil {
					break
				}
			}
			if err != nil {
				return nil, fmt.Errorf("无法表示的日期时间（含 infinity/BC）")
			}
		}
		if dt == "timestamptz" {
			t = t.UTC()
		}
		if t.Year() < 1000 || t.Year() > 9999 {
			return nil, fmt.Errorf("日期超出 MySQL 1000..9999 范围")
		}
		if t.Nanosecond()%1000 != 0 {
			return nil, fmt.Errorf("时间精度超过微秒")
		}
		if dt == "date" {
			return t.Format("2006-01-02"), nil
		}
		return t.Format("2006-01-02 15:04:05.000000"), nil
	case "time":
		s := str()
		if t, ok := v.(time.Time); ok {
			s = t.Format("15:04:05.000000")
		}
		if s == "24:00:00" {
			return s, nil
		}
		parsed, e := time.Parse("15:04:05.999999999", s)
		if e != nil {
			return nil, fmt.Errorf("非法 TIME")
		}
		if parsed.Nanosecond()%1000 != 0 {
			return nil, fmt.Errorf("TIME 精度超过微秒")
		}
		return parsed.Format("15:04:05.000000"), nil
	case "uuid":
		s := str()
		if !uuidPattern.MatchString(s) {
			return nil, fmt.Errorf("非法 UUID")
		}
		return strings.ToLower(s), nil
	case "json", "jsonb":
		s := str()
		if !utf8.ValidString(s) || !json.Valid([]byte(s)) {
			return nil, fmt.Errorf("非法 JSON/UTF-8")
		}
		dec := json.NewDecoder(bytes.NewBufferString(s))
		dec.UseNumber()
		var obj any
		if e := dec.Decode(&obj); e != nil {
			return nil, fmt.Errorf("JSON 解析失败")
		}
		if e := checkJSONNumbers(obj); e != nil {
			return nil, e
		}
		return s, nil
	default:
		s := str()
		if !utf8.ValidString(s) {
			return nil, fmt.Errorf("非法 UTF-8")
		}
		if c.Length > 0 && int64(utf8.RuneCountInString(s)) > c.Length {
			return nil, fmt.Errorf("字符长度越界")
		}
		return s, nil
	}
}
func checkJSONNumbers(v any) error {
	switch x := v.(type) {
	case json.Number:
		s := x.String()
		if !strings.ContainsAny(s, ".eE") {
			if _, e := strconv.ParseInt(s, 10, 64); e == nil {
				return nil
			}
			if _, e := strconv.ParseUint(s, 10, 64); e == nil {
				return nil
			}
		}
		n, e := strconv.ParseFloat(s, 64)
		if e != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return fmt.Errorf("JSON 数值超出 MySQL 范围")
		}
		// Reject decimal rounding rather than silently store a different JSON number.
		a, ok := new(big.Rat).SetString(s)
		b, ok2 := new(big.Rat).SetString(strconv.FormatFloat(n, 'g', -1, 64))
		if !ok || !ok2 || a.Cmp(b) != 0 {
			return fmt.Errorf("JSON 数值转换会损失精度")
		}
	case []any:
		for _, v := range x {
			if e := checkJSONNumbers(v); e != nil {
				return e
			}
		}
	case map[string]any:
		for _, v := range x {
			if e := checkJSONNumbers(v); e != nil {
				return e
			}
		}
	}
	return nil
}
