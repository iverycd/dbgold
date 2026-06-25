package typemap

import (
	"dbgold/datamigrate/source"
	"fmt"
	"strings"
)

func init() {
	Register("sqlserver", "mysql", SQLServerToMySQL)
}

// SQLServerToMySQL 将 SQL Server 列类型映射为 MySQL 类型字符串(目标 MySQL 5.7+,utf8mb4)。
func SQLServerToMySQL(col source.ColumnInfo, charInLength, useNvarchar2 bool) string {
	dt := strings.ToLower(strings.TrimSpace(col.DataType))
	switch dt {
	case "tinyint", "smallint":
		return "smallint"
	case "mediumint", "int":
		return "int"
	case "bigint":
		return "bigint"
	case "varchar", "nvarchar":
		if col.Length == -1 { // MAX 类型
			return "longtext"
		}
		n := col.Length
		if n <= 0 {
			n = 1
		}
		if n > mysqlVarcharMaxChars {
			return "text"
		}
		return fmt.Sprintf("varchar(%d)", n)
	case "char", "nchar":
		n := col.Length
		if n <= 0 {
			n = 1
		}
		if n > mysqlCharMaxChars {
			return fmt.Sprintf("varchar(%d)", n)
		}
		return fmt.Sprintf("char(%d)", n)
	case "text", "ntext":
		return "longtext"
	case "varbinary", "binary", "image":
		if dt == "image" || col.Length == -1 {
			return "longblob"
		}
		n := col.Length
		if n <= 0 {
			n = 1
		}
		return fmt.Sprintf("%s(%d)", dt, n)
	case "bit":
		// SQL Server bit 是 0/1 整数
		return "tinyint(1)"
	case "uniqueidentifier":
		return "char(36)"
	case "smalldatetime", "datetime", "datetime2":
		return "datetime(6)"
	case "date":
		return "date"
	case "time":
		return "time"
	case "datetimeoffset":
		return "datetime(6)"
	case "timestamp":
		// SQL Server timestamp 是 rowversion(行版本号),不是时间戳
		return "varbinary(8)"
	case "numeric", "decimal":
		if col.Precision > 0 {
			return fmt.Sprintf("decimal(%d,%d)", col.Precision, col.Scale)
		}
		return "decimal"
	case "money":
		return "decimal(19,4)"
	case "smallmoney":
		return "decimal(10,4)"
	case "real":
		return "float"
	case "float":
		return "double"
	case "xml":
		return "longtext"
	case "geography", "geometry", "hierarchyid":
		return "longtext"
	default:
		return dt
	}
}
