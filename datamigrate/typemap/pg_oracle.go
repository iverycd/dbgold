package typemap

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

func init() {
	for _, product := range []string{"postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		Register(product, "oracle", func(c source.ColumnInfo, _ bool, national bool) string {
			t, _ := PGToOracleChecked(c, national, 4, 4)
			return t
		})
	}
}

// PGOracleBaseType normalizes catalog aliases without changing object names.
func PGOracleBaseType(raw string) string {
	switch s := strings.ToLower(strings.TrimSpace(raw)); s {
	case "smallint":
		return "int2"
	case "integer", "int":
		return "int4"
	case "bigint":
		return "int8"
	case "real":
		return "float4"
	case "double precision":
		return "float8"
	case "decimal":
		return "numeric"
	case "boolean":
		return "bool"
	case "character varying":
		return "varchar"
	case "char", "character":
		return "bpchar"
	case "timestamp without time zone":
		return "timestamp"
	case "timestamp with time zone":
		return "timestamptz"
	case "bit varying":
		return "varbit"
	default:
		return s
	}
}

// PGToOracleChecked has no unknown-type fallback. Character widths come from
// the actual target character sets; fixed CHAR must never become a LOB.
func PGToOracleChecked(c source.ColumnInfo, national bool, charBytes, ncharBytes int) (string, error) {
	if c.TypeKind != "" && c.TypeKind != "b" {
		return "", fmt.Errorf("列 %s 的 enum/domain/复合类型不能自动迁移到 Oracle", c.Name)
	}
	if c.TypeSchema != "" && c.TypeSchema != "pg_catalog" && c.TypeSchema != "sys_catalog" {
		return "", fmt.Errorf("列 %s 的扩展类型 %s.%s 不能自动迁移到 Oracle", c.Name, c.TypeSchema, c.DataType)
	}
	if c.Generated != "" {
		return "", fmt.Errorf("列 %s 的生成表达式不能自动迁移到 Oracle", c.Name)
	}
	text := func(n int64, fixed bool) (string, error) {
		width, limit, name := max(charBytes, 1), int64(4000), "VARCHAR2"
		if fixed {
			limit, name = 2000, "CHAR"
		}
		if national {
			if fixed {
				return "", fmt.Errorf("列 %s 的 bpchar 不能映射 NCHAR：补充字符的填充长度语义不同", c.Name)
			}
			width, name = max(ncharBytes, 1), "NVARCHAR2"
			if fixed {
				name = "NCHAR"
			}
		}
		if n > limit/int64(width) || fixed && n <= 0 {
			return "", fmt.Errorf("列 %s 字符长度 %d 超出 Oracle 字符集安全容量，不能丢失长度约束", c.Name, n)
		}
		if n <= 0 {
			if national {
				return "NCLOB", nil
			}
			return "CLOB", nil
		}
		if national {
			// National strings count a supplementary character as two code
			// units. A matching LENGTH4 CHECK retains the PG code-point limit.
			return fmt.Sprintf("%s(%d)", name, 2*n), nil
		}
		return fmt.Sprintf("%s(%d CHAR)", name, n), nil
	}
	switch PGOracleBaseType(c.DataType) {
	case "int2":
		return "NUMBER(5,0)", nil
	case "int4":
		return "NUMBER(10,0)", nil
	case "int8":
		return "NUMBER(19,0)", nil
	case "bool":
		return "NUMBER(1,0)", nil
	case "numeric":
		if c.Precision == 0 && c.Scale == 0 {
			return "NUMBER", nil
		}
		if c.Precision < 1 || c.Precision > 38 || c.Scale < -84 || c.Scale > 127 {
			return "", fmt.Errorf("列 %s numeric(%d,%d) 超出 Oracle NUMBER 精度范围", c.Name, c.Precision, c.Scale)
		}
		return fmt.Sprintf("NUMBER(%d,%d)", c.Precision, c.Scale), nil
	case "float4":
		return "BINARY_FLOAT", nil
	case "float8":
		return "BINARY_DOUBLE", nil
	case "bpchar":
		return text(max(c.Length, 1), true)
	case "varchar":
		return text(c.Length, false)
	case "text":
		if national {
			return "NCLOB", nil
		}
		return "CLOB", nil
	case "json", "jsonb", "xml":
		return "CLOB", nil
	case "uuid":
		return "VARCHAR2(36 CHAR)", nil
	case "inet", "cidr":
		return "VARCHAR2(49 CHAR)", nil
	case "macaddr":
		return "VARCHAR2(17 CHAR)", nil
	case "macaddr8":
		return "VARCHAR2(23 CHAR)", nil
	case "bit", "varbit":
		// Bits are ASCII, so the national text option is unnecessary.
		if PGOracleBaseType(c.DataType) == "bit" {
			c.Length = max(c.Length, 1)
		}
		if national {
			return PGToOracleChecked(c, false, charBytes, ncharBytes)
		}
		return text(c.Length, false)
	case "bytea":
		return "BLOB", nil
	case "date":
		return "DATE", nil
	case "timestamp", "timestamptz":
		if c.DateTimePrecision < 0 || c.DateTimePrecision > 6 {
			return "", fmt.Errorf("列 %s 时间精度必须为 0..6", c.Name)
		}
		t := fmt.Sprintf("TIMESTAMP(%d)", c.DateTimePrecision)
		if PGOracleBaseType(c.DataType) == "timestamptz" {
			t += " WITH TIME ZONE"
		}
		return t, nil
	default:
		return "", fmt.Errorf("列 %s 的 PG 类型 %s 不支持迁移到 Oracle", c.Name, c.DataType)
	}
}
