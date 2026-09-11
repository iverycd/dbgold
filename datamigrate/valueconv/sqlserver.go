package valueconv

import (
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
	"fmt"
	"github.com/shopspring/decimal"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// ConvertSQLServer validates before binding, so SQL Server implicit casts can
// never truncate text, round decimals, or normalize invalid MySQL dates.
func ConvertSQLServer(v interface{}, c source.ColumnInfo) (interface{}, error) {
	if _, e := typemap.SQLServerType(c); e != nil {
		return nil, e
	}
	if v == nil {
		if !c.IsNullable {
			return nil, fmt.Errorf("列 %s 不允许 NULL", c.Name)
		}
		return nil, nil
	}
	s := OracleText(v)
	dt := strings.ToLower(c.DataType)
	fail := func() (interface{}, error) {
		return nil, fmt.Errorf("列 %s (%s): 数据超出范围或不能无损转换", c.Name, c.DataType)
	}
	switch dt {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "year", "bool", "boolean", "bit":
		n := new(big.Int)
		if dt == "bit" {
			if b, ok := v.([]byte); ok {
				n.SetBytes(b)
			} else {
				if _, ok := n.SetString(s, 10); !ok {
					return fail()
				}
			}
		} else {
			if _, ok := n.SetString(s, 10); !ok {
				return fail()
			}
		}
		bits := map[string]uint{"tinyint": 8, "smallint": 16, "mediumint": 24, "int": 32, "integer": 32, "bigint": 64, "year": 16, "bool": 8, "boolean": 8, "bit": uint(max(c.Precision, 1))}[dt]
		unsigned := dt == "bit" || strings.Contains(strings.ToLower(c.FormattedType), "unsigned")
		hi := new(big.Int).Lsh(big.NewInt(1), bits)
		lo := new(big.Int)
		if !unsigned {
			hi.Rsh(hi, 1)
			lo.Neg(new(big.Int).Set(hi))
		}
		hi.Sub(hi, big.NewInt(1))
		if n.Cmp(lo) < 0 || n.Cmp(hi) > 0 {
			return fail()
		}
		if dt == "bit" && c.Precision <= 1 {
			return n.Sign() != 0, nil
		}
		if n.IsInt64() {
			return n.Int64(), nil
		}
		return n.String(), nil
	case "decimal", "numeric":
		d, e := decimal.NewFromString(s)
		if e != nil {
			return fail()
		}
		if !d.Equal(d.Truncate(int32(c.Scale))) || d.Abs().GreaterThanOrEqual(decimal.New(1, int32(c.Precision-c.Scale))) {
			return fail()
		}
		if strings.Contains(strings.ToLower(c.FormattedType), "unsigned") && d.IsNegative() {
			return fail()
		}
		return d.StringFixed(int32(c.Scale)), nil
	case "float", "double", "real":
		bits := 64
		if dt == "float" {
			bits = 32
		}
		f, e := strconv.ParseFloat(s, bits)
		if e != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return fail()
		}
		return f, nil
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext":
		if !utf8.ValidString(s) {
			return fail()
		}
		if c.Length > 0 && int64(utf8.RuneCountInString(s)) > c.Length {
			return fail()
		}
		// Defensive bound also protects callers constructing metadata by hand.
		if int64(len(utf16.Encode([]rune(s)))) > 2147483647/2 {
			return fail()
		}
		return s, nil
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
		b, ok := v.([]byte)
		if !ok {
			return fail()
		}
		if c.Length > 0 && int64(len(b)) > c.Length {
			return fail()
		}
		return append([]byte{}, b...), nil
	case "date", "datetime", "timestamp", "time":
		if t, ok := v.(time.Time); ok {
			if dt == "date" {
				s = t.Format("2006-01-02")
			} else if dt == "time" {
				s = t.Format("15:04:05.999999999")
			} else {
				s = t.Format("2006-01-02 15:04:05.999999999")
			}
		}
		layout := "2006-01-02 15:04:05.999999999"
		if dt == "date" {
			layout = "2006-01-02"
		}
		if dt == "time" {
			layout = "15:04:05.999999999"
		}
		t, e := time.Parse(layout, s)
		if e != nil || dt != "time" && t.Year() < 1 {
			return fail()
		}
		unit := int(math.Pow10(9 - int(c.DateTimePrecision)))
		if dt != "date" && t.Nanosecond()%unit != 0 {
			return fail()
		}
		if dt == "date" {
			return t.Format("2006-01-02"), nil
		}
		if dt == "time" {
			return t.Format("15:04:05.9999999"), nil
		}
		return t.Format("2006-01-02T15:04:05.9999999"), nil
	}
	return fail()
}
