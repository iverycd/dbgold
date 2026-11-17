package typemap

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

func init() {
	Register("oscar", "mysql", func(col source.ColumnInfo, _, _ bool) string {
		mapped, _ := OscarToMySQLChecked(col)
		return mapped
	})
}

// OscarToMySQLChecked returns a common MySQL 5.7/8.0 storage declaration.
func OscarToMySQLChecked(col source.ColumnInfo) (string, error) {
	if col.Generated != "" {
		return "", fmt.Errorf("生成列尚无等价 MySQL 定义")
	}
	typ := strings.ToLower(strings.Join(strings.Fields(col.DataType), " "))
	switch typ {
	case "tinyint", "smallint", "bigint":
		return typ, nil
	case "int2", "smallserial":
		return "smallint", nil
	case "int", "integer", "int4", "serial":
		return "int", nil
	case "int8", "bigserial":
		return "bigint", nil
	case "real", "float4":
		return "float", nil
	case "float", "double", "double precision", "float8":
		return "double", nil
	case "numeric", "decimal", "number", "dec":
		if col.Precision < 1 || col.Precision > 65 || col.Scale < 0 || col.Scale > 30 || col.Scale > col.Precision {
			return "", fmt.Errorf("%s(%d,%d) 超出 MySQL DECIMAL 的精度或标度范围", typ, col.Precision, col.Scale)
		}
		return fmt.Sprintf("decimal(%d,%d)", col.Precision, col.Scale), nil
	case "boolean", "bool":
		return "tinyint(1)", nil
	case "bit":
		width := col.Length
		if width == 0 {
			width = col.Precision
		}
		if width == 1 {
			return "bit(1)", nil
		}
		return "", fmt.Errorf("JDBC 尚不能无损读取 %s(%d)", typ, width)
	case "char", "character", "bpchar", "nchar", "national character":
		return oscarMySQLLength(typ, "char", col.Length, 255)
	case "varchar", "varchar2", "nvarchar", "nvarchar2", "character varying", "national character varying":
		return oscarMySQLLength(typ, "varchar", col.Length, 16383)
	case "text", "clob", "nclob", "longvarchar", "longnvarchar":
		return "longtext", nil
	case "binary":
		return oscarMySQLLength(typ, "binary", col.Length, 255)
	case "varbinary", "raw":
		return oscarMySQLLength(typ, "varbinary", col.Length, 65535)
	case "blob", "longvarbinary", "bytea", "long raw":
		return "longblob", nil
	case "date":
		return "date", nil
	case "time", "time without time zone":
		return oscarMySQLTime(typ, "time", col.DateTimePrecision)
	case "timestamp", "timestamp without time zone", "datetime":
		return oscarMySQLTime(typ, "datetime", col.DateTimePrecision)
	default:
		return "", fmt.Errorf("Oscar 类型 %q 尚无经校验的 MySQL 映射", col.DataType)
	}
}

func oscarMySQLLength(src, dst string, length, maximum int64) (string, error) {
	if length < 1 || length > maximum {
		return "", fmt.Errorf("%s(%d) 超出 MySQL %s 的长度范围 1..%d", src, length, dst, maximum)
	}
	return fmt.Sprintf("%s(%d)", dst, length), nil
}

func oscarMySQLTime(src, dst string, precision int64) (string, error) {
	if precision < 0 || precision > 6 {
		return "", fmt.Errorf("%s(%d) 的小数秒精度超出 MySQL 支持的 0..6", src, precision)
	}
	if precision == 0 {
		return dst, nil
	}
	return fmt.Sprintf("%s(%d)", dst, precision), nil
}
