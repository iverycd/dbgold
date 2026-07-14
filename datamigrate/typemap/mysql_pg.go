package typemap

import (
	"dbgold/datamigrate/source"
	"fmt"
	"strings"
)

// PostgreSQL/GaussDB/SeaBox 语法兼容,共用 MySQLToPG。
func init() {
	Register("mysql", "postgres", MySQLToPG)
	Register("mysql", "gaussdb", MySQLToPG)
	Register("mysql", "seabox", MySQLToPG)
	Register("mysql", "highgo", MySQLToPG)
	Register("mysql", "kingbase", MySQLToPG)
}

// MySQLToPG 将 MySQL 列的数据类型转换为 PostgreSQL 类型字符串。
// charInLength=true 时 char/varchar 长度单位使用 CHAR，useNvarchar2=true 时 varchar/char 转为 nvarchar2（优先级更高）。
func MySQLToPG(col source.ColumnInfo, charInLength, useNvarchar2 bool) string {
	dt := strings.ToLower(col.DataType)
	switch dt {
	case "tinyint", "smallint", "mediumint", "int":
		return "int"
	case "bigint":
		return "bigint"
	case "float", "double":
		if col.Precision > 0 {
			return fmt.Sprintf("decimal(%d,%d)", col.Precision, col.Scale)
		}
		return "decimal"
	case "decimal", "numeric":
		if col.Precision > 0 {
			return fmt.Sprintf("decimal(%d,%d)", col.Precision, col.Scale)
		}
		return "decimal"
	case "char":
		if useNvarchar2 {
			return fmt.Sprintf("nvarchar2(%d)", col.Length)
		}
		if charInLength {
			return fmt.Sprintf("char(%d char)", col.Length)
		}
		return fmt.Sprintf("char(%d)", col.Length)
	case "varchar":
		if useNvarchar2 {
			return fmt.Sprintf("nvarchar2(%d)", col.Length)
		}
		if charInLength {
			return fmt.Sprintf("varchar(%d char)", col.Length)
		}
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
