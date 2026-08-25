package valueconv

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"dbgold/internal/jdbcbridge"
	mssql "github.com/microsoft/go-mssqldb"
)

// OscarValueConverter selects exact JDBC wire types for values that cannot be
// represented safely by the bridge's generic Go type mapping.
type OscarValueConverter struct{}

func NewOscar() *OscarValueConverter { return &OscarValueConverter{} }

func (c *OscarValueConverter) Convert(value interface{}, srcType, dbTypeName string) interface{} {
	if value == nil {
		return nil
	}
	dt := strings.ToUpper(dbTypeName)
	if srcType == "mysql" {
		if b, ok := value.([]byte); ok {
			switch dt {
			case "BIT":
				return bytesToInt(b)
			case "GEOMETRY":
				if len(b) >= 4 {
					b = b[4:]
				}
				return hex.EncodeToString(b)
			}
		}
	}
	if srcType == "sqlserver" {
		switch dt {
		case "BIT":
			switch v := value.(type) {
			case bool:
				return v
			case int64:
				return v != 0
			}
		case "UNIQUEIDENTIFIER":
			if id, ok := value.(mssql.UniqueIdentifier); ok {
				return id.String()
			}
		case "MONEY", "SMALLMONEY":
			if b, ok := value.([]byte); ok {
				return jdbcbridge.Decimal(string(b))
			}
		case "XML":
			if b, ok := value.([]byte); ok {
				return string(b)
			}
		}
	}
	if t, ok := value.(time.Time); ok {
		if srcType == "oracle" && dt == "DATE" {
			return jdbcbridge.Timestamp(t.Format("2006-01-02 15:04:05.999999999"))
		}
		switch dt {
		case "DATE":
			return jdbcbridge.Date(t.Format("2006-01-02"))
		case "TIME":
			return jdbcbridge.Time(t.Format("15:04:05.999999999"))
		default:
			return jdbcbridge.Timestamp(t.Format("2006-01-02 15:04:05.999999999"))
		}
	}
	// Several readers expose exact numerics as []byte. Preserve their textual
	// representation instead of converting through float64.
	if isExactNumericType(dt) {
		switch v := value.(type) {
		case []byte:
			return jdbcbridge.Decimal(string(v))
		case string:
			return jdbcbridge.Decimal(v)
		case fmt.Stringer:
			return jdbcbridge.Decimal(v.String())
		}
	}
	return value
}

func isExactNumericType(dt string) bool {
	return dt == "DECIMAL" || dt == "NUMERIC" || dt == "NUMBER" || dt == "MONEY" || dt == "SMALLMONEY"
}
