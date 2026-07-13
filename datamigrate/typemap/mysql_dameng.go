package typemap

import (
	"dbgold/datamigrate/source"
	"fmt"
	"strings"
)

func init() {
	Register("mysql", "dameng", MySQLToDameng)
}

// MySQLToDameng 将 MySQL 列的数据类型转换为达梦(Oracle 兼容模式)类型字符串。
// 达梦 Oracle 兼容模式使用 NUMBER/VARCHAR2/CLOB/BLOB 等 Oracle 风格类型。
// charInLength=true 时 char/varchar 长度单位使用 CHAR；
// useNvarchar2=true 时 varchar/char 转为 NVARCHAR2(优先级更高)。
func MySQLToDameng(col source.ColumnInfo, charInLength, useNvarchar2 bool) string {
	dt := strings.ToLower(strings.TrimSpace(col.DataType))
	switch dt {
	case "tinyint":
		return "TINYINT"
	case "smallint":
		return "SMALLINT"
	case "mediumint":
		// 达梦没有 mediumint，用 INT 承接（范围更大，不会溢出）
		return "INT"
	case "int", "integer":
		return "INT"
	case "bigint":
		return "BIGINT"
	case "decimal", "numeric":
		if col.Precision > 0 {
			return fmt.Sprintf("NUMBER(%d,%d)", col.Precision, col.Scale)
		}
		return "NUMBER"
	case "real":
		return "BINARY_DOUBLE"
	case "float", "double":
		if col.Precision > 0 {
			return fmt.Sprintf("DECIMAL(%d,%d)", col.Precision, col.Scale)
		}
		return "DECIMAL"
	case "char":
		if useNvarchar2 {
			return fmt.Sprintf("NVARCHAR2(%d)", col.Length)
		}
		if charInLength {
			return fmt.Sprintf("CHAR(%d CHAR)", col.Length)
		}
		return fmt.Sprintf("CHAR(%d)", col.Length)
	case "varchar":
		if useNvarchar2 {
			return fmt.Sprintf("NVARCHAR2(%d)", col.Length)
		}
		if charInLength {
			return fmt.Sprintf("VARCHAR2(%d CHAR)", col.Length)
		}
		return fmt.Sprintf("VARCHAR2(%d)", col.Length)
	case "tinytext":
		return "VARCHAR2(4000)"
	case "text", "mediumtext", "longtext":
		return "TEXT"
	case "json":
		return "CLOB"
	case "date":
		return "DATE"
	case "datetime", "timestamp":
		return "TIMESTAMP"
	case "time":
		return "TIME"
	case "year":
		return "NUMBER(4)"
	case "tinyblob", "blob", "mediumblob", "longblob", "binary", "varbinary":
		return "BLOB"
	case "bit":
		return "NUMBER(1)"
	case "enum":
		return "VARCHAR2(255)"
	case "set":
		return "VARCHAR2(1024)"
	default:
		return strings.ToUpper(dt)
	}
}
