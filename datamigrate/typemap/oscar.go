package typemap

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

func init() {
	Register("mysql", "oscar", MySQLToOscar)
	Register("sqlserver", "oscar", SQLServerToOscar)
	Register("oracle", "oscar", OracleToOscar)
	Register("dameng", "oscar", DaMengToOscar)
}

func sized(name string, n int64, fallback int64) string {
	if n <= 0 {
		n = fallback
	}
	return fmt.Sprintf("%s(%d)", name, n)
}

func decimalType(col source.ColumnInfo) string {
	if col.Precision > 0 {
		return fmt.Sprintf("DECIMAL(%d,%d)", col.Precision, col.Scale)
	}
	return "DECIMAL"
}

// MySQLToOscar maps MySQL types to types accepted by Oscar's JDBC/SQL layer.
func MySQLToOscar(col source.ColumnInfo, _ bool, useNvarchar2 bool) string {
	dt := strings.ToLower(strings.TrimSpace(col.DataType))
	switch dt {
	case "tinyint", "smallint":
		return "SMALLINT"
	case "mediumint", "int", "integer", "year":
		return "INTEGER"
	case "bigint":
		return "BIGINT"
	case "decimal", "numeric", "float", "double":
		return decimalType(col)
	case "real":
		return "REAL"
	case "char":
		if useNvarchar2 {
			return sized("NVARCHAR2", col.Length, 1)
		}
		return sized("CHAR", col.Length, 1)
	case "varchar", "enum":
		if useNvarchar2 {
			return sized("NVARCHAR2", col.Length, 255)
		}
		return sized("VARCHAR", col.Length, 255)
	case "tinytext", "text", "mediumtext", "longtext", "json", "xml", "set", "geometry":
		return "CLOB"
	case "tinyblob", "blob", "mediumblob", "longblob":
		return "BLOB"
	case "binary", "varbinary":
		if col.Length > 0 {
			return sized("VARBINARY", col.Length, 255)
		}
		return "BLOB"
	case "bit":
		// MySQL BIT may contain up to 64 bits; BIGINT preserves BIT(n), not just BIT(1).
		return "BIGINT"
	case "boolean", "bool":
		return "BOOLEAN"
	case "date":
		return "DATE"
	case "time":
		return "TIME"
	case "datetime", "timestamp":
		return "TIMESTAMP"
	default:
		return strings.ToUpper(dt)
	}
}

func SQLServerToOscar(col source.ColumnInfo, _ bool, useNvarchar2 bool) string {
	dt := strings.ToLower(strings.TrimSpace(col.DataType))
	switch dt {
	case "tinyint", "smallint":
		return "SMALLINT"
	case "int", "integer":
		return "INTEGER"
	case "bigint":
		return "BIGINT"
	case "numeric", "decimal":
		return decimalType(col)
	case "money":
		return "DECIMAL(19,4)"
	case "smallmoney":
		return "DECIMAL(10,4)"
	case "real":
		return "REAL"
	case "float":
		return "DECIMAL"
	case "char", "nchar":
		if useNvarchar2 || dt == "nchar" {
			return sized("NVARCHAR2", col.Length, 1)
		}
		return sized("CHAR", col.Length, 1)
	case "varchar", "nvarchar":
		if col.Length == -1 {
			return "CLOB"
		}
		if useNvarchar2 || dt == "nvarchar" {
			return sized("NVARCHAR2", col.Length, 255)
		}
		return sized("VARCHAR", col.Length, 255)
	case "text", "ntext", "xml", "geography", "geometry", "hierarchyid":
		return "CLOB"
	case "binary", "varbinary":
		if col.Length > 0 && col.Length != -1 {
			return sized("VARBINARY", col.Length, 255)
		}
		return "BLOB"
	case "image", "timestamp", "rowversion":
		return "BLOB"
	case "bit":
		return "BOOLEAN"
	case "uniqueidentifier":
		return "VARCHAR(36)"
	case "date":
		return "DATE"
	case "time":
		return "TIME"
	case "smalldatetime", "datetime", "datetime2", "datetimeoffset":
		return "TIMESTAMP"
	default:
		return strings.ToUpper(dt)
	}
}

func OracleToOscar(col source.ColumnInfo, _ bool, useNvarchar2 bool) string {
	dt := strings.ToUpper(strings.TrimSpace(col.DataType))
	base := dt
	if i := strings.IndexByte(base, '('); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	switch base {
	case "NUMBER", "NUMERIC", "DECIMAL":
		if col.Precision > 0 && col.Scale == 0 {
			switch {
			case col.Precision <= 4:
				return "SMALLINT"
			case col.Precision <= 9:
				return "INTEGER"
			case col.Precision <= 18:
				return "BIGINT"
			}
		}
		return decimalType(col)
	case "FLOAT", "BINARY_DOUBLE", "DOUBLE PRECISION":
		return "DECIMAL"
	case "BINARY_FLOAT", "REAL":
		return "REAL"
	case "SMALLINT":
		return "SMALLINT"
	case "INTEGER", "INT":
		return "INTEGER"
	case "CHAR", "NCHAR", "CHARACTER":
		if useNvarchar2 || base == "NCHAR" {
			return sized("NVARCHAR2", col.Length, 1)
		}
		return sized("CHAR", col.Length, 1)
	case "VARCHAR", "VARCHAR2", "NVARCHAR2":
		if useNvarchar2 || base == "NVARCHAR2" {
			return sized("NVARCHAR2", col.Length, 255)
		}
		return sized("VARCHAR", col.Length, 255)
	case "CLOB", "NCLOB", "LONG", "XMLTYPE":
		return "CLOB"
	case "BLOB", "RAW", "LONG RAW", "BFILE":
		return "BLOB"
	case "DATE", "TIMESTAMP", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE":
		return "TIMESTAMP"
	case "BOOLEAN":
		return "BOOLEAN"
	default:
		return dt
	}
}

func DaMengToOscar(col source.ColumnInfo, _ bool, useNvarchar2 bool) string {
	dt := strings.ToLower(strings.TrimSpace(col.DataType))
	switch dt {
	case "tinyint", "byte", "smallint":
		return "SMALLINT"
	case "int", "integer", "mediumint":
		return "INTEGER"
	case "bigint":
		return "BIGINT"
	case "numeric", "number", "decimal", "float", "double", "double precision":
		return decimalType(col)
	case "real":
		return "REAL"
	case "char", "character", "nchar":
		if useNvarchar2 || dt == "nchar" {
			return sized("NVARCHAR2", col.Length, 1)
		}
		return sized("CHAR", col.Length, 1)
	case "varchar", "varchar2", "nvarchar2":
		if useNvarchar2 || dt == "nvarchar2" {
			return sized("NVARCHAR2", col.Length, 255)
		}
		return sized("VARCHAR", col.Length, 255)
	case "clob", "text", "longvarchar", "xml", "xmltype", "geometry":
		return "CLOB"
	case "blob", "image", "longvarbinary", "bfile":
		return "BLOB"
	case "binary", "varbinary":
		if col.Length > 0 {
			return sized("VARBINARY", col.Length, 255)
		}
		return "BLOB"
	case "date":
		return "DATE"
	case "time":
		return "TIME"
	case "timestamp", "datetime":
		return "TIMESTAMP"
	case "bit", "boolean":
		return "BOOLEAN"
	default:
		return strings.ToUpper(dt)
	}
}
