package typemap

import (
	"dbgold/datamigrate/source"
	"fmt"
	"strings"
)

func init() {
	for _, product := range []string{"oracle", "postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		Register(product, "sqlserver", func(c source.ColumnInfo, _, _ bool) string {
			mapped, _ := SQLServerTypeForSource(c, product)
			return mapped
		})
	}
}

func IsSQLServerUnboundedDecimal(c source.ColumnInfo, src string) bool {
	if src == "dameng" {
		t := strings.ToLower(c.DataType)
		return c.DamengFloatStorage == "decimal" || c.Precision <= 0 && (t == "number" || t == "numeric" || t == "decimal")
	}
	if source.IsPGFamily(src) {
		return c.Precision <= 0 && PGOracleBaseType(c.DataType) == "numeric"
	}
	if src == "oracle" {
		switch oracleSQLServerBaseType(c.DataType) {
		case "number", "numeric", "decimal":
			return c.Precision <= 0
		case "float", "real", "double precision":
			return true
		}
	}
	return false
}

// SQLServerTypeForSource uses a checked mapping for target DDL and parameter casts.
func SQLServerTypeForSource(c source.ColumnInfo, src string) (string, error) {
	switch {
	case src == "" || src == "mysql":
		return SQLServerType(c)
	case src == "oracle":
		return oracleSQLServerType(c)
	case src == "dameng":
		return damengSQLServerType(c)
	case source.IsPGFamily(src):
		return pgSQLServerType(c)
	default:
		return "", fmt.Errorf("不支持 %s 迁移到 SQL Server", src)
	}
}

func oracleSQLServerBaseType(raw string) string {
	t := strings.ToLower(strings.TrimSpace(raw))
	start, end := strings.IndexByte(t, '('), strings.IndexByte(t, ')')
	if start >= 0 && end > start {
		t = t[:start] + t[end+1:]
	}
	return strings.Join(strings.Fields(t), " ")
}

func oracleSQLServerType(c source.ColumnInfo) (string, error) {
	switch oracleSQLServerBaseType(c.DataType) {
	case "number", "numeric", "decimal":
		return sqlServerDecimal(c, c.Precision, c.Scale)
	case "float", "real", "double precision":
		return sqlServerDecimal(c, 0, 0)
	case "integer", "int", "smallint":
		// Oracle implements these ANSI synonyms as NUMBER(38,0).
		return "DECIMAL(38,0)", nil
	case "binary_float":
		return "REAL", nil
	case "binary_double":
		return "FLOAT(53)", nil
	case "boolean":
		return "BIT", nil
	case "char", "nchar", "character", "varchar", "varchar2", "nvarchar2":
		return sqlServerUnicode(c.Length), nil
	case "clob", "nclob", "long", "xmltype", "sys.xmltype":
		return "NVARCHAR(MAX)", nil
	case "raw":
		n := c.DataLength
		if n <= 0 {
			n = c.Length
		}
		if n > 0 && n <= 8000 {
			return fmt.Sprintf("VARBINARY(%d)", n), nil
		}
		return "VARBINARY(MAX)", nil
	case "blob", "long raw":
		return "VARBINARY(MAX)", nil
	case "date":
		return "DATETIME2(0)", nil
	case "timestamp":
		return sqlServerTemporal(c, "DATETIME2")
	case "timestamp with time zone", "timestamp with local time zone":
		return sqlServerTemporal(c, "DATETIMEOFFSET")
	}
	return "", fmt.Errorf("列 %s: Oracle 类型 %s 无法自动迁移到 SQL Server", c.Name, c.DataType)
}

func pgSQLServerType(c source.ColumnInfo) (string, error) {
	if c.TypeKind != "" && c.TypeKind != "b" {
		return "", fmt.Errorf("列 %s: PG 用户定义类型 %s 无法自动迁移到 SQL Server", c.Name, c.DataType)
	}
	if c.TypeSchema != "" && c.TypeSchema != "pg_catalog" && c.TypeSchema != "sys_catalog" {
		return "", fmt.Errorf("列 %s: PG 扩展类型 %s.%s 无法自动迁移到 SQL Server", c.Name, c.TypeSchema, c.DataType)
	}
	switch PGOracleBaseType(c.DataType) {
	case "int2", "smallserial", "serial2":
		return "SMALLINT", nil
	case "int4", "serial", "serial4":
		return "INT", nil
	case "int8", "bigserial", "serial8":
		return "BIGINT", nil
	case "numeric":
		return sqlServerDecimal(c, c.Precision, c.Scale)
	case "bool":
		return "BIT", nil
	case "float4":
		return "REAL", nil
	case "float8":
		return "FLOAT(53)", nil
	case "bpchar", "varchar":
		return sqlServerUnicode(c.Length), nil
	case "text", "json", "jsonb", "xml":
		return "NVARCHAR(MAX)", nil
	case "bytea":
		return "VARBINARY(MAX)", nil
	case "uuid":
		return "UNIQUEIDENTIFIER", nil
	case "date":
		return "DATE", nil
	case "time", "time without time zone":
		return sqlServerTemporal(c, "TIME")
	case "timestamp":
		return sqlServerTemporal(c, "DATETIME2")
	case "timestamptz":
		return sqlServerTemporal(c, "DATETIMEOFFSET")
	}
	return "", fmt.Errorf("列 %s: PG 类型 %s 无法自动迁移到 SQL Server", c.Name, c.DataType)
}

func sqlServerDecimal(c source.ColumnInfo, precision, scale int64) (string, error) {
	if c.SQLServerPrecision != 0 {
		precision, scale = c.SQLServerPrecision, c.SQLServerScale
	} else if precision <= 0 {
		return "", fmt.Errorf("列 %s: 无限定精度的 %s 缺少 SQL Server 精度预检结果", c.Name, c.DataType)
	} else if scale < 0 {
		precision -= scale
		scale = 0
	} else if scale > precision {
		precision = scale
	}
	if precision < 1 || precision > 38 || scale < 0 || scale > precision {
		return "", fmt.Errorf("列 %s: 精度 %d 标度 %d 超出 SQL Server DECIMAL 范围", c.Name, precision, scale)
	}
	return fmt.Sprintf("DECIMAL(%d,%d)", precision, scale), nil
}

func sqlServerUnicode(length int64) string {
	if length <= 0 || length > 2000 {
		return "NVARCHAR(MAX)"
	}
	return fmt.Sprintf("NVARCHAR(%d)", length*2)
}

func sqlServerTemporal(c source.ColumnInfo, typ string) (string, error) {
	if c.DateTimePrecision < 0 || c.DateTimePrecision > 9 {
		return "", fmt.Errorf("列 %s: 无效的时间精度 %d", c.Name, c.DateTimePrecision)
	}
	// Values at source precision 8 or 9 are accepted only if their final
	// fractional digits are zero. The Writer checks before issuing the INSERT.
	return fmt.Sprintf("%s(%d)", typ, min(c.DateTimePrecision, 7)), nil
}
