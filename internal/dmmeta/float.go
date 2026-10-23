// Package dmmeta interprets actual DM column descriptors without changing
// connection settings or relying on the configured compatibility mode.
package dmmeta

import (
	"database/sql"
	"fmt"
	"strings"
)

// IsFloatType identifies catalog declarations whose physical representation
// must be inspected: NUMBER_MODE can make even REAL decimal-backed.
func IsFloatType(dataType string) bool {
	switch baseType(dataType) {
	case "REAL", "FLOAT", "DOUBLE", "DOUBLE PRECISION":
		return true
	default:
		return false
	}
}

// FloatStorage returns decimal, real (binary32), or double (binary64) from an
// actual column descriptor. DM's ScanType incorrectly reports some FLOAT-backed
// DOUBLE columns as float32, so it must not be used to determine their width.
func FloatStorage(column *sql.ColumnType, precision int64) (string, error) {
	if column == nil {
		return "", fmt.Errorf("缺少达梦浮点列实际类型元数据")
	}
	if _, _, ok := column.DecimalSize(); ok {
		return "decimal", nil
	}
	switch baseType(column.DatabaseTypeName()) {
	case "NUMBER", "NUMERIC", "DECIMAL", "DEC":
		return "decimal", nil
	case "REAL", "BINARY_FLOAT":
		return "real", nil
	case "DOUBLE", "DOUBLE PRECISION", "BINARY_DOUBLE":
		return "double", nil
	case "FLOAT":
		if precision < 0 || precision > 126 {
			return "", fmt.Errorf("达梦 FLOAT 精度 %d 不在 1 到 126 之间", precision)
		}
		if precision > 0 && precision <= 24 {
			return "real", nil
		}
		// Zero means the catalog omitted precision. Only after the descriptor
		// proves a native FLOAT do we apply DM's default binary64 width.
		return "double", nil
	default:
		return "", fmt.Errorf("达梦列实际类型 %q 不是可支持的浮点或十进制类型", column.DatabaseTypeName())
	}
}

func baseType(dataType string) string {
	if at := strings.IndexByte(dataType, '('); at >= 0 {
		dataType = dataType[:at]
	}
	return strings.ToUpper(strings.Join(strings.Fields(dataType), " "))
}
