package typemap

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

func init() {
	for _, src := range []string{"dameng", "sqlserver"} {
		Register(src, "oracle", func(c source.ColumnInfo, _, national bool) string {
			t, _ := OracleTypeForSource(c, src, national, 4, 4)
			return t
		})
	}
	Register("dameng", "sqlserver", func(c source.ColumnInfo, _, _ bool) string {
		t, _ := SQLServerTypeForSource(c, "dameng")
		return t
	})
}

// OracleTypeForSource is shared by planning, target validation and binding.
// Unknown types never fall back to source SQL.
func OracleTypeForSource(c source.ColumnInfo, src string, national bool, charBytes, ncharBytes int) (string, error) {
	if source.IsPGFamily(src) {
		return PGToOracleChecked(c, national, charBytes, ncharBytes)
	}
	if src == "mysql" || src == "" {
		return OracleType(c, national, charBytes, ncharBytes)
	}
	if src != "dameng" && src != "sqlserver" {
		return "", fmt.Errorf("不支持 %s → Oracle", src)
	}
	fail := func() (string, error) {
		return "", fmt.Errorf("列 %s 的 %s 类型 %s 无法自动迁移到 Oracle", c.Name, src, c.DataType)
	}
	if c.Generated != "" || c.TypeKind != "" {
		return fail()
	}
	t := strings.ToLower(strings.TrimSpace(c.DataType))
	text := func(fixed, nchar bool) (string, error) {
		kind, lob, limit, width := "VARCHAR2", "CLOB", int64(4000), max(charBytes, 1)
		if fixed {
			kind, limit = "CHAR", 2000
		}
		if national || nchar {
			kind, lob, width = "NVARCHAR2", "NCLOB", max(ncharBytes, 1)
			if fixed {
				kind = "NCHAR"
			}
		}
		if c.Length <= 0 || c.Length > limit/int64(width) {
			return lob, nil
		}
		unit := " CHAR"
		if kind == "NVARCHAR2" || kind == "NCHAR" {
			unit = ""
		}
		return fmt.Sprintf("%s(%d%s)", kind, c.Length, unit), nil
	}
	stamp := func(zone bool) (string, error) {
		p := c.DateTimePrecision
		if p < 0 || p > 9 {
			return fail()
		}
		s := fmt.Sprintf("TIMESTAMP(%d)", p)
		if zone {
			s += " WITH TIME ZONE"
		}
		return s, nil
	}
	if src == "sqlserver" {
		switch t {
		case "timestamp", "rowversion":
			return "RAW(8)", nil
		case "uniqueidentifier":
			return "VARCHAR2(36 CHAR)", nil
		case "money":
			return "NUMBER(19,4)", nil
		case "smallmoney":
			return "NUMBER(10,4)", nil
		case "datetime":
			return "TIMESTAMP(7)", nil
		case "smalldatetime":
			return "TIMESTAMP(0)", nil
		case "float":
			if c.Precision > 0 && c.Precision <= 24 {
				return "BINARY_FLOAT", nil
			}
			return "BINARY_DOUBLE", nil
		}
	}
	switch t {
	case "tinyint", "byte":
		return "NUMBER(3,0)", nil
	case "smallint":
		return "NUMBER(5,0)", nil
	case "int", "integer", "mediumint":
		return "NUMBER(10,0)", nil
	case "bigint":
		return "NUMBER(19,0)", nil
	case "bit", "boolean":
		return "NUMBER(1,0)", nil
	case "number", "numeric", "decimal", "dec":
		if c.Precision == 0 {
			return "NUMBER", nil
		}
		if c.Precision < 1 || c.Precision > 38 || c.Scale < -84 || c.Scale > 127 {
			return fail()
		}
		return fmt.Sprintf("NUMBER(%d,%d)", c.Precision, c.Scale), nil
	case "float", "real", "double", "double precision", "binary_float", "binary_double":
		if src == "dameng" && c.DamengFloatStorage == "" {
			return fail()
		}
		if src == "dameng" && c.DamengFloatStorage == "decimal" {
			c.DataType, c.Precision, c.Scale = "decimal", 0, 0
			return OracleTypeForSource(c, src, national, charBytes, ncharBytes)
		}
		if t == "real" || t == "binary_float" || c.DamengFloatStorage == "real" {
			return "BINARY_FLOAT", nil
		}
		return "BINARY_DOUBLE", nil
	case "char", "character":
		return text(true, false)
	case "nchar":
		return text(true, true)
	case "varchar", "varchar2":
		return text(false, false)
	case "nvarchar", "nvarchar2":
		return text(false, true)
	case "clob", "text", "longvarchar", "xml":
		return "CLOB", nil
	case "nclob", "ntext":
		return "NCLOB", nil
	case "binary", "varbinary", "blob", "image", "longvarbinary":
		return "BLOB", nil
	case "raw":
		if n := max(c.DataLength, c.Length); n > 0 && n <= 2000 {
			return fmt.Sprintf("RAW(%d)", n), nil
		}
		return "BLOB", nil
	case "date":
		return "DATE", nil
	case "time":
		return "VARCHAR2(18 CHAR)", nil
	case "timestamp", "datetime", "datetime2":
		return stamp(false)
	case "datetimeoffset", "timestamp with time zone":
		return stamp(true)
	}
	return fail()
}

func damengSQLServerType(c source.ColumnInfo) (string, error) {
	t := strings.ToLower(strings.TrimSpace(c.DataType))
	if (t == "timestamp" || t == "datetime" || t == "time" || t == "timestamp with time zone") && (c.DateTimePrecision < 0 || c.DateTimePrecision > 7) {
		return "", fmt.Errorf("列 %s 时间精度超出 SQL Server 0..7 范围", c.Name)
	}
	if c.Generated != "" || c.TypeKind != "" {
		return "", fmt.Errorf("列 %s 的生成列/自定义类型需人工适配", c.Name)
	}
	switch t {
	case "tinyint", "byte", "smallint":
		return "SMALLINT", nil
	case "int", "integer", "mediumint":
		return "INT", nil
	case "bigint":
		return "BIGINT", nil
	case "bit", "boolean":
		return "BIT", nil
	case "float", "real", "double", "double precision":
		if c.DamengFloatStorage == "" {
			return "", fmt.Errorf("列 %s 缺少浮点存储描述", c.Name)
		}
		if c.DamengFloatStorage == "decimal" {
			return sqlServerDecimal(c, 0, 0)
		}
		if c.DamengFloatStorage == "real" || t == "real" {
			return "REAL", nil
		}
		return "FLOAT(53)", nil
	case "date":
		return "DATE", nil
	case "time":
		return sqlServerTemporal(c, "TIME")
	case "timestamp", "datetime":
		return sqlServerTemporal(c, "DATETIME2")
	case "timestamp with time zone":
		return sqlServerTemporal(c, "DATETIMEOFFSET")
	case "binary", "varbinary", "blob", "image", "longvarbinary":
		return "VARBINARY(MAX)", nil
	case "text", "ntext", "longvarchar":
		return "NVARCHAR(MAX)", nil
	case "nvarchar":
		c.DataType = "nvarchar2"
	}
	return oracleSQLServerType(c)
}
