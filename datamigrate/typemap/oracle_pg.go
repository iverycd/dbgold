package typemap

import (
	"dbgold/datamigrate/source"
	"fmt"
	"strings"
)

// PostgreSQL/GaussDB/SeaBox 语法兼容,共用 OracleToPG。
func init() {
	Register("oracle", "postgres", OracleToPG)
	Register("oracle", "gaussdb", OracleToPG)
	Register("oracle", "seabox", OracleToPG)
	Register("oracle", "highgo", OracleToPG)
}

// OracleToPG 将 Oracle 列的数据类型转换为 PostgreSQL 类型字符串。
// charInLength=true 时 char/varchar 长度单位使用 CHAR，useNvarchar2=true 时转为 nvarchar2（优先级更高）。
func OracleToPG(col source.ColumnInfo, charInLength, useNvarchar2 bool) string {
	dt := strings.ToUpper(strings.TrimSpace(col.DataType))

	// 处理带括号的复合类型，如 "TIMESTAMP(6)"、"INTERVAL YEAR(2) TO MONTH"
	base := dt
	if idx := strings.Index(dt, "("); idx != -1 {
		base = strings.TrimSpace(dt[:idx])
	}

	switch base {
	case "NUMBER", "NUMERIC", "DECIMAL":
		if col.Precision > 0 && col.Scale == 0 {
			switch {
			case col.Precision <= 4:
				return "smallint"
			case col.Precision <= 9:
				return "int"
			case col.Precision <= 18:
				return "bigint"
			default:
				return fmt.Sprintf("decimal(%d,0)", col.Precision)
			}
		}
		if col.Precision > 0 {
			return fmt.Sprintf("decimal(%d,%d)", col.Precision, col.Scale)
		}
		return "decimal"

	case "FLOAT", "BINARY_DOUBLE", "DOUBLE PRECISION":
		return "double precision"

	case "BINARY_FLOAT", "REAL":
		return "real"

	case "SMALLINT", "INTEGER", "INT":
		return "int"

	case "CHAR", "NCHAR", "CHARACTER":
		if useNvarchar2 {
			return fmt.Sprintf("nvarchar2(%d)", col.Length)
		}
		if charInLength {
			return fmt.Sprintf("char(%d char)", col.Length)
		}
		return fmt.Sprintf("char(%d)", col.Length)

	case "VARCHAR2", "NVARCHAR2", "VARCHAR":
		if useNvarchar2 {
			return fmt.Sprintf("nvarchar2(%d)", col.Length)
		}
		if charInLength {
			return fmt.Sprintf("varchar(%d char)", col.Length)
		}
		return fmt.Sprintf("varchar(%d)", col.Length)

	case "CLOB", "NCLOB", "LONG":
		return "text"

	case "BLOB", "RAW", "LONG RAW":
		return "bytea"

	// Oracle DATE 含时分秒，映射到 timestamp
	case "DATE":
		return "timestamp"

	case "TIMESTAMP":
		return "timestamp"

	case "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE":
		return "timestamptz"

	case "INTERVAL YEAR TO MONTH":
		return "interval"

	case "INTERVAL DAY TO SECOND":
		return "interval"

	case "BOOLEAN":
		return "boolean"

	case "XMLTYPE":
		return "text"

	default:
		return strings.ToLower(dt)
	}
}
