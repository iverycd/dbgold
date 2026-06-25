package typemap

import (
	"dbgold/datamigrate/source"
	"fmt"
	"strings"
)

func init() {
	Register("dameng", "mysql", DaMengToMySQL)
}

// DaMengToMySQL 将达梦列类型映射为 MySQL 类型字符串(目标 MySQL 5.7+,utf8mb4)。
func DaMengToMySQL(col source.ColumnInfo, charInLength, useNvarchar2 bool) string {
	dt := strings.ToLower(strings.TrimSpace(col.DataType))
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
		return "float"
	case "float", "double", "double precision":
		if col.Precision > 0 {
			return fmt.Sprintf("decimal(%d,%d)", col.Precision, col.Scale)
		}
		return "double"
	case "numeric", "number", "decimal":
		if col.Precision > 0 {
			scale := col.Scale
			if scale < 0 {
				scale = 0
			}
			return fmt.Sprintf("decimal(%d,%d)", col.Precision, scale)
		}
		return "decimal"
	case "char", "character":
		n := col.Length
		if n <= 0 {
			n = 1
		}
		if n > mysqlCharMaxChars {
			return fmt.Sprintf("varchar(%d)", n)
		}
		return fmt.Sprintf("char(%d)", n)
	case "varchar", "varchar2":
		n := col.Length
		if n <= 0 {
			n = 1
		}
		if n > mysqlVarcharMaxChars {
			return "text"
		}
		return fmt.Sprintf("varchar(%d)", n)
	case "clob", "text", "longvarchar":
		return "longtext"
	case "binary", "varbinary":
		n := col.Length
		if n <= 0 {
			n = 1
		}
		return fmt.Sprintf("%s(%d)", dt, n)
	case "blob", "image", "longvarbinary", "bfile":
		return "longblob"
	case "date":
		return "date"
	case "time":
		return "time"
	case "timestamp", "datetime":
		return "datetime(6)"
	case "bit", "boolean":
		return "tinyint(1)"
	case "interval year to month", "interval day to second":
		return "varchar(50)"
	default:
		return dt
	}
}
