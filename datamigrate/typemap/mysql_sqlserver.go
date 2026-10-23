package typemap

import (
	"dbgold/datamigrate/source"
	"fmt"
	"strings"
)

func init()                                                  { Register("mysql", "sqlserver", MySQLToSQLServer) }
func MySQLToSQLServer(c source.ColumnInfo, _, _ bool) string { t, _ := SQLServerType(c); return t }

// SQLServerType targets SQL Server 2008 R2. Unsupported semantics are errors,
// not raw source type names or silently downgraded text.
func SQLServerType(c source.ColumnInfo) (string, error) {
	unsigned := strings.Contains(strings.ToLower(c.FormattedType), "unsigned")
	switch strings.ToLower(strings.TrimSpace(c.DataType)) {
	case "tinyint":
		if unsigned {
			return "TINYINT", nil
		}
		return "SMALLINT", nil
	case "smallint":
		if unsigned {
			return "INT", nil
		}
		return "SMALLINT", nil
	case "mediumint":
		return "INT", nil
	case "int", "integer":
		if unsigned {
			return "BIGINT", nil
		}
		return "INT", nil
	case "bigint":
		if unsigned {
			return "DECIMAL(20,0)", nil
		}
		return "BIGINT", nil
	case "bool", "boolean":
		return "SMALLINT", nil
	case "year":
		return "SMALLINT", nil
	case "bit":
		if c.Precision <= 1 {
			return "BIT", nil
		}
		if c.Precision <= 64 {
			return "DECIMAL(20,0)", nil
		}
	case "decimal", "numeric":
		if c.Precision > 0 && c.Precision <= 38 && c.Scale >= 0 && c.Scale <= c.Precision {
			return fmt.Sprintf("DECIMAL(%d,%d)", c.Precision, c.Scale), nil
		}
	case "float":
		return "REAL", nil
	case "double", "double precision", "real":
		return "FLOAT(53)", nil
	case "char", "varchar":
		if c.Length > 0 {
			// NVARCHAR counts UTF-16 units; reserve two for each MySQL character.
			if c.Length <= 2000 {
				return fmt.Sprintf("NVARCHAR(%d)", c.Length*2), nil
			}
			return "NVARCHAR(MAX)", nil
		}
	case "tinytext", "text", "mediumtext", "longtext":
		return "NVARCHAR(MAX)", nil
	case "binary", "varbinary":
		if c.Length > 0 {
			if c.Length <= 8000 {
				return fmt.Sprintf("VARBINARY(%d)", c.Length), nil
			}
			return "VARBINARY(MAX)", nil
		}
	case "tinyblob", "blob", "mediumblob", "longblob":
		return "VARBINARY(MAX)", nil
	case "date":
		return "DATE", nil
	case "datetime", "timestamp", "time":
		if c.DateTimePrecision >= 0 && c.DateTimePrecision <= 6 {
			t := "DATETIME2"
			if strings.EqualFold(c.DataType, "time") {
				t = "TIME"
			}
			return fmt.Sprintf("%s(%d)", t, c.DateTimePrecision), nil
		}
	}
	return "", fmt.Errorf("列 %s: MySQL %s (%s, precision=%d scale=%d) 无法等价迁移到 SQL Server 2008 R2", c.Name, c.DataType, c.FormattedType, c.Precision, c.Scale)
}
