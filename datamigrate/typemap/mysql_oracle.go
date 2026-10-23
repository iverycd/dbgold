package typemap

import (
	"fmt"
	"math"
	"strings"

	"dbgold/datamigrate/source"
)

func init() { Register("mysql", "oracle", MySQLToOracle) }

// MySQLToOracle uses conservative Unicode capacities when no live target is
// available. The Oracle dialect supplies the actual target character widths.
func MySQLToOracle(c source.ColumnInfo, _ bool, national bool) string {
	t, _ := OracleType(c, national, 4, 4)
	return t
}

func OracleType(c source.ColumnInfo, national bool, charBytes, ncharBytes int) (string, error) {
	dt := strings.ToLower(strings.TrimSpace(c.DataType))
	integer := func(p int) (string, error) { return fmt.Sprintf("NUMBER(%d,0)", p), nil }
	textType := func(fixed bool) (string, error) {
		if c.Length <= 0 {
			return "", fmt.Errorf("列 %s 缺少字符长度", c.Name)
		}
		width, limit, name, lob := max(charBytes, 1), int64(4000), "VARCHAR2", "CLOB"
		if fixed {
			name, limit = "CHAR", 2000
		}
		if national {
			width, name, lob, limit = max(ncharBytes, 1), "NVARCHAR2", "NCLOB", 4000
		}
		if c.Length > limit/int64(width) {
			return lob, nil
		}
		if national {
			return fmt.Sprintf("%s(%d)", name, c.Length), nil
		}
		return fmt.Sprintf("%s(%d CHAR)", name, c.Length), nil
	}
	switch dt {
	case "tinyint":
		return integer(3)
	case "smallint":
		return integer(5)
	case "mediumint":
		return integer(8)
	case "int", "integer":
		return integer(10)
	case "bigint":
		return integer(20)
	case "bool", "boolean":
		return integer(1)
	case "bit":
		bits := c.Precision
		if bits <= 0 {
			bits = 1
		}
		if bits > 64 {
			return "", fmt.Errorf("列 %s BIT 精度超过 64", c.Name)
		}
		return integer(int(math.Ceil(float64(bits) * math.Log10(2))))
	case "decimal", "numeric":
		if c.Precision < 1 || c.Precision > 38 || c.Scale < 0 || c.Scale > c.Precision {
			return "", fmt.Errorf("列 %s DECIMAL(%d,%d) 超出 Oracle 38 位精度范围", c.Name, c.Precision, c.Scale)
		}
		return fmt.Sprintf("NUMBER(%d,%d)", c.Precision, c.Scale), nil
	case "float":
		return "BINARY_FLOAT", nil
	case "double", "double precision", "real":
		return "BINARY_DOUBLE", nil
	case "char":
		return textType(true)
	case "varchar", "enum", "set":
		return textType(false)
	case "tinytext", "text", "mediumtext", "longtext", "json":
		return "CLOB", nil
	case "date":
		return "DATE", nil
	case "datetime", "timestamp":
		if c.DateTimePrecision < 0 || c.DateTimePrecision > 6 {
			return "", fmt.Errorf("列 %s 时间精度无效", c.Name)
		}
		return fmt.Sprintf("TIMESTAMP(%d)", c.DateTimePrecision), nil
	case "time":
		return "VARCHAR2(32 CHAR)", nil
	case "year":
		return integer(4)
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob", "geometry", "point", "linestring", "polygon", "multipoint", "multilinestring", "multipolygon", "geometrycollection":
		return "BLOB", nil
	default:
		return "", fmt.Errorf("列 %s 的 MySQL 类型 %s 暂不支持迁移到 Oracle", c.Name, c.DataType)
	}
}
