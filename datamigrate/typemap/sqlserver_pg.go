package typemap

import (
	"dbgold/datamigrate/source"
	"fmt"
	"strings"
)

// PostgreSQL/GaussDB/SeaBox 语法兼容,共用 SQLServerToPG。
func init() {
	Register("sqlserver", "postgres", SQLServerToPG)
	Register("sqlserver", "gaussdb", SQLServerToPG)
	Register("sqlserver", "seabox", SQLServerToPG)
	Register("sqlserver", "highgo", SQLServerToPG)
	Register("sqlserver", "kingbase", SQLServerToPG)
}

// SQLServerToPG 将 SQL Server 列的数据类型转换为 PostgreSQL 类型字符串。
// charInLength=true 时 char/varchar 长度单位使用 CHAR，useNvarchar2=true 时转为 nvarchar2（优先级更高）。
func SQLServerToPG(col source.ColumnInfo, charInLength, useNvarchar2 bool) string {
	dt := strings.ToLower(col.DataType)
	switch dt {
	case "tinyint", "smallint", "mediumint", "int":
		return "int"
	case "bigint":
		return "bigint"
	case "varchar", "nvarchar":
		if col.Length == -1 {
			return "text" // MAX 类型
		}
		if useNvarchar2 {
			return fmt.Sprintf("nvarchar2(%d)", col.Length)
		}
		if charInLength {
			return fmt.Sprintf("varchar(%d char)", col.Length)
		}
		return fmt.Sprintf("varchar(%d)", col.Length)
	case "char", "nchar":
		if useNvarchar2 {
			return fmt.Sprintf("nvarchar2(%d)", col.Length)
		}
		if charInLength {
			return fmt.Sprintf("char(%d char)", col.Length)
		}
		return fmt.Sprintf("char(%d)", col.Length)
	case "text", "ntext":
		return "text"
	case "varbinary", "binary", "image":
		return "bytea"
	case "bit":
		// SQL Server bit 是 0/1 整数，不是 PostgreSQL bit 类型
		return "smallint"
	case "uniqueidentifier":
		return "char(36)"
	case "smalldatetime", "datetime", "datetime2":
		return "timestamp"
	case "date":
		return "date"
	case "time":
		return "time"
	case "timestamp":
		// SQL Server timestamp 是 rowversion（行版本号），不是时间戳
		return "bytea"
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
		return "real"
	case "float":
		return "decimal"
	case "xml":
		return "text"
	case "geography", "geometry":
		return "text"
	case "hierarchyid":
		return "text"
	default:
		return dt
	}
}
