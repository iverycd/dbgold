package typemap

import (
	"dbgold/datamigrate/source"
	"fmt"
	"strings"
)

// PostgreSQL/GaussDB/SeaBox 语法兼容,共用 DaMengToPG。
func init() {
	Register("dameng", "postgres", DaMengToPG)
	Register("dameng", "gaussdb", DaMengToPG)
	Register("dameng", "seabox", DaMengToPG)
	Register("dameng", "highgo", DaMengToPG)
}

// DaMengToPG 将达梦列的数据类型转换为 PostgreSQL 类型字符串。
// charInLength=true 时 char/varchar 长度单位使用 CHAR，useNvarchar2=true 时转为 nvarchar2（优先级更高）。
func DaMengToPG(col source.ColumnInfo, charInLength, useNvarchar2 bool) string {
	dt := strings.ToLower(col.DataType)
	switch dt {
	case "tinyint", "byte":
		return "smallint"
	case "smallint":
		return "smallint"
	case "int", "integer", "mediumint":
		return "int"
	case "bigint":
		return "bigint"
	case "real":
		return "real"
	case "float", "double", "double precision":
		if col.Precision > 0 {
			return fmt.Sprintf("decimal(%d,%d)", col.Precision, col.Scale)
		}
		return "decimal"
	case "numeric", "number", "decimal":
		if col.Precision > 0 {
			return fmt.Sprintf("decimal(%d,%d)", col.Precision, col.Scale)
		}
		return "decimal"
	case "char", "character":
		if useNvarchar2 {
			return fmt.Sprintf("nvarchar2(%d)", col.Length)
		}
		if charInLength {
			return fmt.Sprintf("char(%d char)", col.Length)
		}
		return fmt.Sprintf("char(%d)", col.Length)
	case "varchar", "varchar2":
		if useNvarchar2 {
			return fmt.Sprintf("nvarchar2(%d)", col.Length)
		}
		if charInLength {
			return fmt.Sprintf("varchar(%d char)", col.Length)
		}
		return fmt.Sprintf("varchar(%d)", col.Length)
	case "clob", "text", "longvarchar":
		return "text"
	case "binary", "varbinary":
		return "bytea"
	case "blob", "image", "longvarbinary", "bfile":
		return "bytea"
	case "date":
		return "date"
	case "time":
		return "time"
	case "timestamp", "datetime":
		return "timestamp"
	case "bit", "boolean":
		return "smallint"
	case "interval year to month":
		return "interval"
	case "interval day to second":
		return "interval"
	default:
		return dt
	}
}
