package source

import (
	"fmt"
	"strings"
)

// FormatColumnType describes original source metadata for reports. It never
// applies a target dialect's type mapping or modifies source identifiers.
func FormatColumnType(c ColumnInfo) string {
	typ := strings.TrimSpace(c.FormattedType)
	if typ == "" {
		typ = strings.TrimSpace(c.DataType)
	}
	if typ == "" || strings.Contains(typ, "(") {
		return typ
	}
	switch strings.ToUpper(typ) {
	case "VARCHAR", "VARCHAR2", "NVARCHAR", "NVARCHAR2", "CHAR", "NCHAR", "CHARACTER", "CHARACTER VARYING", "BINARY", "VARBINARY", "RAW", "BIT", "VARBIT", "BIT VARYING":
		if c.Length < 0 {
			return typ + "(max)"
		}
		if c.Length > 0 {
			semantics := ""
			if strings.EqualFold(typ, "VARCHAR2") || strings.EqualFold(typ, "CHAR") {
				switch c.CharUsed {
				case "C":
					semantics = " CHAR"
				case "B":
					semantics = " BYTE"
				}
			}
			return fmt.Sprintf("%s(%d%s)", typ, c.Length, semantics)
		}
	case "NUMBER", "NUMERIC", "DECIMAL", "DEC":
		if c.Precision > 0 {
			return fmt.Sprintf("%s(%d,%d)", typ, c.Precision, c.Scale)
		}
	case "DATETIME2", "DATETIMEOFFSET", "TIME", "TIMESTAMP":
		if c.DateTimePrecision > 0 {
			return fmt.Sprintf("%s(%d)", typ, c.DateTimePrecision)
		}
	case "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE", "TIME WITH TIME ZONE":
		if c.DateTimePrecision > 0 {
			parts := strings.SplitN(typ, " ", 2)
			return fmt.Sprintf("%s(%d) %s", parts[0], c.DateTimePrecision, parts[1])
		}
	}
	if c.TypeKind == "user_defined" && c.TypeSchema != "" && !strings.Contains(typ, ".") {
		return c.TypeSchema + "." + typ
	}
	return typ
}
