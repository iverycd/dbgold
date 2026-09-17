package typemap

import (
	"dbgold/datamigrate/source"
	"fmt"
	"strings"
)

func init() {
	for _, s := range []string{"postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		Register(s, "mysql", PGToMySQL)
	}
}

// PGToMySQL returns no fallback for unsupported types. The planner uses the
// checked form and must reject a table before any target mutation.
func PGToMySQL(c source.ColumnInfo, _, _ bool) string { t, _ := PGToMySQLChecked(c); return t }
func PGToMySQLChecked(c source.ColumnInfo) (string, error) {
	if c.TypeKind != "" && c.TypeKind != "b" {
		return "", fmt.Errorf("类型 %s (%s) 不支持自动转换", c.FormattedType, c.TypeKind)
	}
	if c.TypeSchema != "" && c.TypeSchema != "pg_catalog" && c.TypeSchema != "sys_catalog" {
		return "", fmt.Errorf("扩展类型 %s.%s 不支持", c.TypeSchema, c.DataType)
	}
	if c.Generated != "" {
		return "", fmt.Errorf("生成列首版不自动转换")
	}
	switch strings.ToLower(c.DataType) {
	case "int2", "smallint":
		return "SMALLINT", nil
	case "int4", "integer", "int":
		return "INT", nil
	case "int8", "bigint":
		return "BIGINT", nil
	case "float4", "real":
		return "FLOAT", nil
	case "float8", "double precision":
		return "DOUBLE", nil
	case "numeric", "decimal":
		if c.Precision <= 0 || c.Precision > 65 || c.Scale < 0 || c.Scale > 30 || c.Scale > c.Precision {
			return "", fmt.Errorf("numeric(%d,%d) 超出 MySQL DECIMAL 范围或无界", c.Precision, c.Scale)
		}
		return fmt.Sprintf("DECIMAL(%d,%d)", c.Precision, c.Scale), nil
	case "bool", "boolean":
		return "TINYINT(1)", nil
	case "bit":
		if c.Length < 1 || c.Length > 64 {
			return "", fmt.Errorf("bit(%d) 超出 MySQL BIT 支持的 1..64 位范围", c.Length)
		}
		return fmt.Sprintf("BIT(%d)", c.Length), nil
	case "uuid":
		return "CHAR(36)", nil
	case "bytea":
		return "LONGBLOB", nil
	case "text":
		return "LONGTEXT", nil
	case "varchar", "character varying":
		if c.Length <= 0 {
			return "LONGTEXT", nil
		}
		if c.Length > 65535 {
			return "", fmt.Errorf("VARCHAR(%d) 超出 MySQL 最大字符容量，不能自动丢失长度约束", c.Length)
		}
		if c.PGMySQLLongText {
			return "LONGTEXT", nil
		}
		return fmt.Sprintf("VARCHAR(%d)", c.Length), nil
	case "bpchar", "char", "character":
		n := c.Length
		if n <= 0 {
			n = 1
		}
		if n > 255 {
			return "", fmt.Errorf("CHAR(%d) 超出 MySQL 上限", n)
		}
		return fmt.Sprintf("CHAR(%d)", n), nil
	case "json", "jsonb":
		return "JSON", nil
	case "date":
		return "DATE", nil
	case "time", "timestamp", "timestamptz":
		if c.DateTimePrecision < 0 || c.DateTimePrecision > 6 {
			return "", fmt.Errorf("时间精度超出 MySQL 范围")
		}
		if strings.EqualFold(c.DataType, "timestamptz") {
			return "DATETIME(6)", nil
		}
		if c.DataType == "time" {
			return fmt.Sprintf("TIME(%d)", c.DateTimePrecision), nil
		}
		return fmt.Sprintf("DATETIME(%d)", c.DateTimePrecision), nil
	default:
		return "", fmt.Errorf("类型 %q 不支持自动转换；请调整源定义或排除整表", c.DataType)
	}
}
