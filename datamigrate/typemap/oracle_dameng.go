package typemap

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dbgold/datamigrate/source"
)

func init() { Register("oracle", "dameng", OracleToDameng) }

var oracleDamengSizes = regexp.MustCompile(`\(\s*[0-9]+\s*(?:,\s*[+-]?[0-9]+\s*)?\)`)
var oracleDamengSpatialType = regexp.MustCompile(`^SYSGEO\.ST_[A-Z][A-Z0-9_]*$`)

// OracleDamengBaseType removes precision annotations without losing TZ/INTERVAL qualifiers.
func OracleDamengBaseType(s string) string {
	return strings.Join(strings.Fields(oracleDamengSizes.ReplaceAllString(strings.ToUpper(strings.TrimSpace(s)), "")), " ")
}

func OracleDamengTimestampPrecision(c source.ColumnInfo) int64 {
	if m := oracleDamengSizes.FindString(c.DataType); m != "" {
		p, _ := strconv.ParseInt(strings.TrimSpace(m[1:len(m)-1]), 10, 64)
		return p
	}
	return c.DateTimePrecision
}

func OracleDamengIntervalPrecisions(c source.ColumnInfo) (int64, int64) {
	p, s := c.Precision, c.Scale
	if p == 0 {
		p = 2
	}
	ms := oracleDamengSizes.FindAllString(c.DataType, -1)
	if len(ms) > 0 {
		p, _ = strconv.ParseInt(strings.TrimSpace(ms[0][1:len(ms[0])-1]), 10, 64)
	}
	if len(ms) > 1 {
		s, _ = strconv.ParseInt(strings.TrimSpace(ms[1][1:len(ms[1])-1]), 10, 64)
	}
	return p, s
}

// OracleToDameng follows the DM migration mapping. FLOAT uses the target's
// approximate numeric semantics; NUMBER remains an exact decimal type.
func OracleToDameng(c source.ColumnInfo, charLength, national bool) string {
	base := OracleDamengBaseType(source.OracleDamengSourceType(c))
	if oracleDamengSpatialType.MatchString(base) {
		return "SYSGEO2." + strings.TrimPrefix(base, "SYSGEO.")
	}
	switch base {
	case "NUMBER", "NUMERIC", "DECIMAL", "DEC":
		if c.Precision > 0 {
			return fmt.Sprintf("NUMBER(%d,%d)", c.Precision, c.Scale)
		}
		return "NUMBER"
	case "FLOAT", "REAL", "DOUBLE PRECISION":
		p := c.Precision
		if m := oracleDamengSizes.FindString(c.DataType); m != "" {
			p, _ = strconv.ParseInt(strings.TrimSpace(m[1:len(m)-1]), 10, 64)
		}
		if p >= 1 && p <= 126 {
			return fmt.Sprintf("FLOAT(%d)", p)
		}
		return "FLOAT"
	case "INTEGER", "INT", "SMALLINT":
		return "NUMBER(38,0)"
	case "BINARY_FLOAT":
		return "FLOAT"
	case "BINARY_DOUBLE":
		return "DOUBLE"
	case "CHAR", "CHARACTER", "VARCHAR", "VARCHAR2", "CHAR VARYING", "CHARACTER VARYING", "NCHAR", "NATIONAL CHAR", "NATIONAL CHARACTER", "NVARCHAR2", "NCHAR VARYING", "NATIONAL CHAR VARYING", "NATIONAL CHARACTER VARYING":
		length := c.Length
		kind := "VARCHAR2"
		if base == "CHAR" || base == "CHARACTER" {
			kind = "CHAR"
		}
		if national {
			return fmt.Sprintf("NVARCHAR(%d CHAR)", length)
		}
		switch base {
		case "NCHAR", "NATIONAL CHAR", "NATIONAL CHARACTER":
			return fmt.Sprintf("NCHAR(%d CHAR)", length)
		case "NVARCHAR2", "NCHAR VARYING", "NATIONAL CHAR VARYING", "NATIONAL CHARACTER VARYING":
			return fmt.Sprintf("NVARCHAR(%d CHAR)", length)
		}
		if charLength || strings.EqualFold(c.CharUsed, "C") {
			return fmt.Sprintf("%s(%d CHAR)", kind, length)
		}
		if c.DataLength > 0 {
			length = c.DataLength
		}
		return fmt.Sprintf("%s(%d)", kind, length)
	case "CLOB", "NCLOB":
		return "CLOB"
	case "LONG":
		return "TEXT"
	case "LONG RAW":
		return "LONGVARBINARY"
	case "BLOB":
		return "BLOB"
	case "BFILE":
		return "BFILE"
	case "XMLTYPE", "SYS.XMLTYPE":
		return "XMLTYPE"
	case "SDE.ST_GEOMETRY", "MDSYS.SDO_GEOMETRY", "SDO_GEOMETRY":
		return "SYSGEO2.ST_GEOMETRY"
	case "ROWID":
		return "BINARY(18)"
	case "UROWID":
		return "VARBINARY(4000)"
	case "RAW":
		length := c.DataLength
		if length == 0 {
			length = c.Length
		}
		return fmt.Sprintf("VARBINARY(%d)", length)
	case "DATE":
		return "TIMESTAMP(0)"
	case "TIMESTAMP", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE":
		qualifier := strings.TrimPrefix(base, "TIMESTAMP")
		if base == "TIMESTAMP WITH LOCAL TIME ZONE" {
			qualifier = " WITH TIME ZONE"
		}
		return fmt.Sprintf("TIMESTAMP(%d)%s", OracleDamengTimestampPrecision(c), qualifier)
	case "INTERVAL YEAR TO MONTH":
		p, _ := OracleDamengIntervalPrecisions(c)
		return fmt.Sprintf("INTERVAL YEAR(%d) TO MONTH", p)
	case "INTERVAL DAY TO SECOND":
		p, s := OracleDamengIntervalPrecisions(c)
		return fmt.Sprintf("INTERVAL DAY(%d) TO SECOND(%d)", p, s)
	}
	return ""
}

func ValidateOracleDamengColumn(c source.ColumnInfo) error {
	fail := func(s string) error { return fmt.Errorf("列 %s (%s): %s", c.Name, c.DataType, s) }
	if c.Generated != "" {
		return fail("暂不支持虚拟列或生成列")
	}
	if c.TypeKind != "" || c.TypeSchema != "" {
		t := source.OracleDamengSourceType(c)
		supported := t == "SYS.XMLTYPE" || t == "MDSYS.SDO_GEOMETRY" || t == "SDE.ST_GEOMETRY" || oracleDamengSpatialType.MatchString(t)
		if !supported || c.TypeKind != "" && c.TypeKind != "user_defined" {
			return fail("暂不支持此用户定义类型")
		}
	}
	if c.Identity != "" && !strings.EqualFold(strings.TrimSpace(c.Identity), "BY DEFAULT ON NULL") {
		return fail("仅支持 BY DEFAULT ON NULL 身份列，禁止改变生成语义")
	}
	if OracleToDameng(c, false, false) == "" {
		return fail("不支持迁移到达梦，禁止自动降级")
	}
	switch OracleDamengBaseType(c.DataType) {
	case "NUMBER", "NUMERIC", "DECIMAL", "DEC":
		if c.Precision < 0 || c.Precision > 38 || c.Scale < 0 || c.Precision > 0 && c.Scale > c.Precision || c.Precision == 0 && c.Scale != 0 {
			return fail("数值精度/标度超出达梦范围（不支持负标度或标度大于精度）")
		}
	case "CHAR", "CHARACTER", "VARCHAR", "VARCHAR2", "CHAR VARYING", "CHARACTER VARYING", "NCHAR", "NATIONAL CHAR", "NATIONAL CHARACTER", "NVARCHAR2", "NCHAR VARYING", "NATIONAL CHAR VARYING", "NATIONAL CHARACTER VARYING":
		if c.Length < 1 || c.Length > 32767 || c.DataLength > 32767 {
			return fail("字符长度超出达梦支持范围")
		}
	case "RAW":
		length := c.DataLength
		if length == 0 {
			length = c.Length
		}
		if length < 1 || length > 32767 {
			return fail("RAW 长度超出达梦支持范围")
		}
	case "TIMESTAMP", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE":
		p := OracleDamengTimestampPrecision(c)
		if p < 0 || p > 9 {
			return fail("时间精度必须在 0 到 9 之间")
		}
	case "INTERVAL YEAR TO MONTH", "INTERVAL DAY TO SECOND":
		p, s := OracleDamengIntervalPrecisions(c)
		if p < 1 || p > 9 || s < 0 || s > 6 {
			return fail("区间引导精度须为 1 到 9，秒精度不得超过 6；禁止截断")
		}
	}
	return nil
}
