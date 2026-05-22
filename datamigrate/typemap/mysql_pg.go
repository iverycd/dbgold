package typemap

import (
	"fmt"
	"strings"
	"dbgold/datamigrate/source"
)

// MySQLToPG 将 MySQL 列的数据类型转换为 PostgreSQL 类型字符串
func MySQLToPG(col source.ColumnInfo) string {
	dt := strings.ToLower(col.DataType)
	switch dt {
	case "tinyint", "smallint", "mediumint", "int":
		return "int"
	case "bigint":
		return "bigint"
	case "float", "double":
		return "double precision"
	case "decimal", "numeric":
		if col.Precision > 0 {
			return fmt.Sprintf("decimal(%d,%d)", col.Precision, col.Scale)
		}
		return "decimal"
	case "char":
		return fmt.Sprintf("char(%d)", col.Length)
	case "varchar":
		return fmt.Sprintf("varchar(%d)", col.Length)
	case "tinytext", "text", "mediumtext", "longtext":
		return "text"
	case "datetime", "timestamp":
		return "timestamp"
	case "date":
		return "date"
	case "time":
		return "time"
	case "tinyblob", "blob", "mediumblob", "longblob", "binary", "varbinary":
		return "bytea"
	case "json":
		return "jsonb"
	case "enum":
		return "varchar(255)"
	case "set":
		return "text"
	case "year":
		return "int"
	case "bit":
		return "bit"
	default:
		return dt
	}
}
