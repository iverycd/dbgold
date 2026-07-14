package typemap

import (
	"dbgold/datamigrate/source"
	"fmt"
	"strings"
)

func init() {
	Register("postgres", "dameng", PGToDameng)
}

// PGToDameng 将 PostgreSQL 列类型转换为达梦(Oracle 兼容模式)类型字符串。
// col.DataType 取自 pg 的 udt_name(int4/varchar/numeric/timestamptz/bytea 等)。
// charInLength=true 时 char/varchar 长度单位使用 CHAR；
// useNvarchar2=true 时 varchar/char 转为 NVARCHAR2(优先级更高)。
func PGToDameng(col source.ColumnInfo, charInLength, useNvarchar2 bool) string {
	dt := strings.ToLower(strings.TrimSpace(col.DataType))
	switch dt {
	case "int2", "smallint":
		return "SMALLINT"
	case "int4", "integer", "int":
		return "INT"
	case "int8", "bigint":
		return "BIGINT"
	case "numeric", "decimal":
		if col.Precision > 0 {
			return fmt.Sprintf("NUMBER(%d,%d)", col.Precision, col.Scale)
		}
		return "NUMBER"
	case "money":
		return "NUMBER(19,4)"
	case "float4", "real":
		return "FLOAT"
	case "float8", "double precision":
		return "DOUBLE"
	case "bool", "boolean":
		// 达梦以 NUMBER(1) 承接布尔，值由 DaMengValueConverter 落地为 0/1
		return "NUMBER(1)"
	case "bpchar", "char", "character":
		if useNvarchar2 {
			return fmt.Sprintf("NVARCHAR2(%d)", charLen(col.Length))
		}
		if charInLength {
			return fmt.Sprintf("CHAR(%d CHAR)", charLen(col.Length))
		}
		return fmt.Sprintf("CHAR(%d)", charLen(col.Length))
	case "varchar", "character varying":
		if col.Length <= 0 {
			// 无长度约束的 varchar → 大文本
			return "TEXT"
		}
		if useNvarchar2 {
			return fmt.Sprintf("NVARCHAR2(%d)", col.Length)
		}
		if charInLength {
			return fmt.Sprintf("VARCHAR2(%d CHAR)", col.Length)
		}
		return fmt.Sprintf("VARCHAR2(%d)", col.Length)
	case "name":
		return "VARCHAR2(128)"
	case "text", "json", "jsonb", "xml":
		return "TEXT"
	case "uuid":
		return "VARCHAR2(36)"
	case "inet", "cidr":
		return "VARCHAR2(43)"
	case "macaddr", "macaddr8":
		return "VARCHAR2(23)"
	case "bit", "varbit":
		return "VARCHAR2(64)"
	case "bytea":
		return "BLOB"
	case "date":
		return "DATE"
	case "time", "timetz":
		return "TIME"
	case "timestamp", "timestamptz", "datetime":
		return "TIMESTAMP"
	case "interval":
		return "VARCHAR2(64)"
	default:
		// 数组(udt_name 以 _ 开头)及其它未知类型统一落到大文本，避免目标建表报未知类型
		return "TEXT"
	}
}

// charLen 保证 char 类型长度至少为 1（pg bpchar 无显式长度时 character_maximum_length 为 0）。
func charLen(n int64) int64 {
	if n <= 0 {
		return 1
	}
	return n
}
