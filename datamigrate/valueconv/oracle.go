package valueconv

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	goora "github.com/sijms/go-ora/v2"
	"github.com/sijms/go-ora/v2/converters"
)

type MigrationWarning struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

// OracleColumn is read from ALL_TAB_COLUMNS, also for data-only migration.
type OracleColumn struct {
	Name, DataType                           string
	Nullable                                 bool
	Precision, Scale, DataLength, CharLength int64
	CharUsed                                 string
}

func OracleText(v interface{}) string {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(v)
}

func OracleZeroDate(s string) bool {
	return len(s) >= 10 && s[4] == '-' && s[7] == '-' && (s[:4] == "0000" || s[5:7] == "00" || s[8:10] == "00")
}

// ConvertOracle never relies on Oracle's NLS implicit numeric/date conversion.
// warning is returned to the Writer, which records it only after commit.
func ConvertOracle(v interface{}, srcType string, c OracleColumn, codec converters.IStringConverter) (out interface{}, warning string, err error) {
	null := func(reason string) (interface{}, string, error) {
		if !c.Nullable {
			return nil, "", fmt.Errorf("非空列不能接收 %s", reason)
		}
		return nil, reason + "转 NULL", nil
	}
	if v == nil {
		if !c.Nullable {
			return nil, "", fmt.Errorf("非空列不能接收 NULL")
		}
		return nil, "", nil
	}
	dt := strings.ToUpper(c.DataType)
	s := OracleText(v)
	switch {
	case dt == "NUMBER":
		if strings.EqualFold(srcType, "BIT") {
			if b, ok := v.([]byte); ok {
				s = new(big.Int).SetBytes(b).String()
			}
		}
		if b, ok := v.(bool); ok {
			if b {
				s = "1"
			} else {
				s = "0"
			}
		}
		n, e := decimal.NewFromString(s)
		if e != nil {
			return nil, "", fmt.Errorf("无效数值: %w", e)
		}
		if c.Precision > 0 {
			if !n.Truncate(int32(c.Scale)).Equal(n) || n.Abs().GreaterThanOrEqual(decimal.New(1, int32(c.Precision-c.Scale))) {
				return nil, "", fmt.Errorf("数值超出 NUMBER(%d,%d)，拒绝舍入或截断", c.Precision, c.Scale)
			}
		}
		num, e := goora.NewNumberFromString(n.String())
		return num, "", e
	case dt == "BINARY_FLOAT" || dt == "BINARY_DOUBLE":
		bits := 64
		if dt == "BINARY_FLOAT" {
			bits = 32
		}
		n, e := strconv.ParseFloat(s, bits)
		if e != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return nil, "", fmt.Errorf("浮点值超出目标范围")
		}
		// go-ora binds ordinary Go floats as NUMBER, whose exponent cannot
		// represent the complete DOUBLE range. Bind scientific text and use an
		// explicit Oracle format model in INSERT instead (independent of NLS).
		precision := 16
		if bits == 32 {
			precision = 8
		}
		return strconv.FormatFloat(n, 'e', precision, bits), "", nil
	case dt == "DATE" || strings.HasPrefix(dt, "TIMESTAMP"):
		var t time.Time
		if raw, ok := v.(time.Time); ok {
			if raw.IsZero() {
				return null("零日期")
			}
			t = raw
		} else {
			if OracleZeroDate(s) {
				return null("零日期")
			}
			if s == "" {
				return null("空日期")
			}
			layout := "2006-01-02 15:04:05.999999999"
			if len(s) == 10 {
				layout = "2006-01-02"
			}
			var e error
			t, e = time.ParseInLocation(layout, s, time.UTC)
			if e != nil {
				return nil, "", fmt.Errorf("无效日期，拒绝自动归一化: %w", e)
			}
		}
		if dt == "DATE" {
			if t.Nanosecond() != 0 {
				return nil, "", fmt.Errorf("DATE 无法保留小数秒")
			}
			// Bind without a time zone so session time zone conversion cannot
			// change the source wall-clock date during assignment to DATE.
			return goora.TimeStamp(t), "", nil
		}
		if c.Scale >= 0 && c.Scale < 9 && t.Nanosecond()%int(math.Pow10(9-int(c.Scale))) != 0 {
			return nil, "", fmt.Errorf("时间值超过目标 TIMESTAMP 精度")
		}
		return goora.TimeStamp(t), "", nil
	case dt == "BLOB":
		b, ok := v.([]byte)
		if !ok {
			return nil, "", fmt.Errorf("BLOB 源值不是二进制")
		}
		return goora.Blob{Data: b}, "", nil
	case dt == "CLOB" || dt == "NCLOB" || dt == "VARCHAR2" || dt == "NVARCHAR2" || dt == "CHAR" || dt == "NCHAR":
		if s == "" {
			switch strings.ToUpper(srcType) {
			case "CHAR", "VARCHAR", "ENUM", "SET":
				return null("空字符串")
			}
		}
		if !utf8.ValidString(s) {
			return nil, "", fmt.Errorf("源文本不是有效 UTF-8")
		}
		if codec != nil && codec.Decode(codec.Encode(s)) != s {
			return nil, "", fmt.Errorf("文本含目标字符集无法表示的字符")
		}
		if dt == "CLOB" {
			return goora.Clob{String: s, Valid: true}, "", nil
		}
		if dt == "NCLOB" {
			return goora.NClob{String: s, Valid: true}, "", nil
		}
		if s == "" {
			return null("空字符串")
		}
		if c.CharLength > 0 && c.CharUsed == "C" && int64(utf8.RuneCountInString(s)) > c.CharLength {
			return nil, "", fmt.Errorf("文本超过目标字符长度 %d", c.CharLength)
		}
		byteLen := len(s)
		if codec != nil {
			byteLen = len(codec.Encode(s))
		}
		if c.DataLength > 0 && int64(byteLen) > c.DataLength {
			return nil, "", fmt.Errorf("文本超过目标字节长度 %d", c.DataLength)
		}
		if dt == "NVARCHAR2" || dt == "NCHAR" {
			return goora.NVarChar(s), "", nil
		}
		return s, "", nil
	default:
		return nil, "", fmt.Errorf("不支持写入 Oracle 类型 %s", c.DataType)
	}
}
