package typemap

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

func init() { Register("oscar", "dameng", OscarToDameng) }

func OscarToDameng(c source.ColumnInfo, _, _ bool) string {
	mapped, _ := OscarToDamengChecked(c)
	return mapped
}

// OscarToDamengChecked rejects definitions that would narrow the source type.
func OscarToDamengChecked(c source.ColumnInfo) (string, error) {
	fail := func(reason string) (string, error) {
		return "", fmt.Errorf("列 %s (%s): %s", c.Name, c.DataType, reason)
	}
	if c.Generated != "" || c.TypeKind != "" && c.TypeKind != "builtin" {
		return fail("达梦迁移暂不支持生成列或用户定义类型")
	}
	base := source.OscarDamengColumnType(c.DataType)
	if c.Extra == "auto_increment" {
		switch base {
		case "INT", "INTEGER", "INT4", "SERIAL", "BIGINT", "INT8", "BIGSERIAL":
		default:
			return fail("达梦 IDENTITY 需要 INT 或 BIGINT 列")
		}
		if c.IdentityInfo == nil || c.IdentityInfo.Increment <= 0 || c.IsNullable {
			return fail("缺少可核验的自增起始值、递增步长或非空属性")
		}
	}
	switch base {
	case "CHAR", "CHARACTER", "BPCHAR", "NCHAR":
		if c.Length > 32767 || c.Length < 0 {
			return fail("定长字符长度超出达梦范围")
		}
		return fmt.Sprintf("CHAR(%d CHAR)", max(c.Length, 1)), nil
	case "VARCHAR", "VARCHAR2", "CHARACTER VARYING", "NVARCHAR2", "NVARCHAR":
		if source.OscarDamengUnboundedVarchar(base, c.Length) {
			return "TEXT", nil
		}
		if c.Length < 0 || c.Length > 32767 {
			return fail("变长字符长度超出达梦范围")
		}
		return fmt.Sprintf("VARCHAR(%d CHAR)", c.Length), nil
	case `"CHAR"`:
		return "VARCHAR(1 CHAR)", nil
	case "NAME":
		return "VARCHAR(127 CHAR)", nil
	case "TEXT", "ARRAY":
		return "TEXT", nil
	case "BOOLEAN", "BOOL":
		return "BIT", nil
	case "BIT", "VARBIT", "BIT VARYING", "BINARY", "VARBINARY":
		length := c.Length
		if length == 0 && (base == "BIT" || base == "BINARY") {
			length = 1
		}
		if length < 1 || length > 32767 {
			return fail("需要正数长度且达梦二进制列最多支持 32767 字节")
		}
		if base == "BIT" || base == "BINARY" {
			return fmt.Sprintf("BINARY(%d)", length), nil
		}
		return fmt.Sprintf("VARBINARY(%d)", length), nil
	case "TINYINT", "INT1":
		return "TINYINT", nil
	case "SMALLINT", "INT2", "SMALLSERIAL":
		return "SMALLINT", nil
	case "INT", "INTEGER", "INT4", "SERIAL":
		return "INT", nil
	case "BIGINT", "INT8", "BIGSERIAL":
		return "BIGINT", nil
	case "DECIMAL", "NUMERIC", "NUMBER", "DEC":
		if c.Precision == 0 {
			return "NUMERIC", nil
		}
		if c.Precision < 0 || c.Precision > 38 || c.Scale < 0 || c.Scale > c.Precision {
			return fail("数值精度或标度超出达梦范围")
		}
		return fmt.Sprintf("NUMERIC(%d,%d)", c.Precision, c.Scale), nil
	case "REAL", "FLOAT4":
		return "FLOAT(24)", nil
	case "DOUBLE", "DOUBLE PRECISION", "FLOAT8":
		return "FLOAT(53)", nil
	case "FLOAT":
		if c.Precision > 0 && c.Precision <= 24 {
			return "FLOAT(24)", nil
		}
		return "FLOAT(53)", nil
	case "TIME", "TIME WITH TIME ZONE", "TIMESTAMP", "TIMESTAMP WITH TIME ZONE":
		parts := strings.SplitN(base, " ", 2)
		maxPrecision := int64(9)
		if parts[0] == "TIME" {
			maxPrecision = 6
		}
		p := OscarDamengTimePrecision(c)
		if p > maxPrecision {
			return fail("小数秒精度超出达梦范围")
		}
		name := fmt.Sprintf("%s(%d)", parts[0], p)
		if len(parts) == 2 {
			name += " " + parts[1]
		}
		return name, nil
	case "INTERVAL YEAR TO MONTH", "INTERVAL DAY TO SECOND":
		p, s := OscarDamengIntervalPrecisions(c)
		if p < 1 || p > 9 || s < 0 || s > 6 {
			return fail("区间引导精度或秒精度超出达梦范围")
		}
		if base == "INTERVAL YEAR TO MONTH" {
			return fmt.Sprintf("INTERVAL YEAR(%d) TO MONTH", p), nil
		}
		return fmt.Sprintf("INTERVAL DAY(%d) TO SECOND(%d)", p, s), nil
	case "DATE", "BLOB", "CLOB", "XMLTYPE":
		return base, nil
	}
	return fail("尚无达梦类型映射")
}

func OscarDamengTimePrecision(c source.ColumnInfo) int64 {
	return max(OracleDamengTimestampPrecision(c), 0)
}

func OscarDamengIntervalPrecisions(c source.ColumnInfo) (int64, int64) {
	// JDBC COLUMN_SIZE can describe the rendered value, not the leading field.
	// Only an explicit declaration can safely narrow the leading field.
	if OracleDamengBaseType(c.FormattedType) == source.OscarDamengColumnType(c.DataType) {
		c.DataType = c.FormattedType
	}
	c.Precision, c.Scale = 9, max(c.Scale, 0)
	return OracleDamengIntervalPrecisions(c)
}
