package source

import (
	"fmt"
	"regexp"
	"strings"
)

var oscarDamengTypeModifier = regexp.MustCompile(`\(\s*-?\d+(?:\s*,\s*-?\d+)?(?:\s+(?:CHAR|BYTE))?\s*\)`)

// OscarDamengColumnType normalizes the catalog's type spelling for the
// migration's mapping and wire values. Quoted "char" is an internal one-byte
// type distinct from SQL CHAR, and BOOL is distinct from a bit string.
func OscarDamengColumnType(raw string) string {
	typ := strings.ToUpper(strings.TrimSpace(raw))
	if strings.HasSuffix(typ, "[]") || strings.HasSuffix(typ, " ARRAY") {
		return "ARRAY"
	}
	typ = strings.Join(strings.Fields(oscarDamengTypeModifier.ReplaceAllString(typ, " ")), " ")
	switch typ {
	case "CHARACTER", "BPCHAR":
		return "CHAR"
	case "CHARACTER VARYING", "CHAR VARYING", "VARCHAR2":
		return "VARCHAR"
	case "BIT VARYING":
		return "VARBIT"
	case "BINARY VARYING":
		return "VARBINARY"
	case "INT1":
		return "TINYINT"
	case "INT2":
		return "SMALLINT"
	case "INT4", "INTEGER":
		return "INT"
	case "INT8":
		return "BIGINT"
	case "DECIMAL", "NUMBER":
		return "NUMERIC"
	case "FLOAT4":
		return "REAL"
	case "FLOAT8", "DOUBLE":
		return "DOUBLE PRECISION"
	case "TIME WITHOUT TIME ZONE":
		return "TIME"
	case "TIMETZ":
		return "TIME WITH TIME ZONE"
	case "TIMESTAMP WITHOUT TIME ZONE", "DATETIME":
		return "TIMESTAMP"
	case "TIMESTAMPTZ":
		return "TIMESTAMP WITH TIME ZONE"
	case "INTERVALYTM", "YMINTERVAL", "INTERVAL YEAR TO MONTH":
		return "INTERVAL YEAR TO MONTH"
	case "INTERVALDTS", "DSINTERVAL", "INTERVAL DAY TO SECOND":
		return "INTERVAL DAY TO SECOND"
	case "BOOLEAN":
		return "BOOL"
	case "XML", "SYS.XMLTYPE":
		return "XMLTYPE"
	default:
		return typ
	}
}

func OscarDamengUnboundedVarchar(typ string, length int64) bool {
	switch OscarDamengColumnType(typ) {
	case "VARCHAR", "NVARCHAR", "NVARCHAR2":
		return length == 0 || length == -1
	}
	return false
}

// OscarDamengBitBytes represents each digit as one binary octet. This
// preserves the width and leading zeroes of both fixed and variable bit strings.
func OscarDamengBitBytes(bits string) ([]byte, error) {
	value := make([]byte, len(bits))
	for i := range bits {
		if bits[i] != '0' && bits[i] != '1' {
			return nil, fmt.Errorf("神通位串包含非 0/1 字符")
		}
		value[i] = bits[i] - '0'
	}
	return value, nil
}
