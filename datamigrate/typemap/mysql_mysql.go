package typemap

import (
	"dbgold/datamigrate/source"
	"fmt"
	"strings"
)

func init() {
	Register("mysql", "mysql", MySQLToMySQL)
}

// MySQLToMySQL 同构映射:MySQL → MySQL,基本原样保留类型,补全长度/精度。
// charInLength / useNvarchar2 对 MySQL 目标无意义,保留签名以符合 Mapper 接口。
func MySQLToMySQL(col source.ColumnInfo, charInLength, useNvarchar2 bool) string {
	dt := strings.ToLower(strings.TrimSpace(col.DataType))
	switch dt {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint":
		if dt == "integer" {
			return "int"
		}
		return dt
	case "float", "double", "double precision":
		if dt == "double precision" {
			return "double"
		}
		if col.Precision > 0 {
			return fmt.Sprintf("%s(%d,%d)", dt, col.Precision, col.Scale)
		}
		return dt
	case "decimal", "numeric":
		if col.Precision > 0 {
			return fmt.Sprintf("decimal(%d,%d)", col.Precision, col.Scale)
		}
		return "decimal"
	case "char":
		n := col.Length
		if n <= 0 {
			n = 1
		}
		if n > mysqlCharMaxChars {
			return fmt.Sprintf("varchar(%d)", n)
		}
		return fmt.Sprintf("char(%d)", n)
	case "varchar":
		n := col.Length
		if n <= 0 {
			n = 1
		}
		if n > mysqlVarcharMaxChars {
			return "text"
		}
		return fmt.Sprintf("varchar(%d)", n)
	case "tinytext", "text", "mediumtext", "longtext":
		return dt
	case "tinyblob", "blob", "mediumblob", "longblob":
		return dt
	case "binary", "varbinary":
		n := col.Length
		if n <= 0 {
			n = 1
		}
		return fmt.Sprintf("%s(%d)", dt, n)
	case "date":
		return "date"
	case "time":
		return "time"
	case "datetime", "timestamp":
		return "datetime(6)"
	case "year":
		return "int"
	case "json":
		return "json"
	case "bit":
		if col.Length > 0 {
			return fmt.Sprintf("bit(%d)", col.Length)
		}
		return "bit"
	case "enum", "set":
		// 原始枚举/集合定义不在 ColumnInfo 中,降级为 varchar 保值
		return "varchar(255)"
	case "geometry", "point", "linestring", "polygon":
		return "longtext"
	default:
		return dt
	}
}
