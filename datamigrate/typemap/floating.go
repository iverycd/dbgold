package typemap

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dbgold/datamigrate/source"
)

// SQL Server FLOAT defaults to binary64. Only a positive precision up to 24
// explicitly identifies binary32; missing precision must never narrow a column.
func singlePrecisionFloat(precision int64) bool {
	return precision > 0 && precision <= 24
}

// DM's FLOAT/REAL names can describe either binary floats or Oracle-compatible
// decimal storage. Only actual source metadata authorizes the binary mapping.
func damengBinaryFloatType(col source.ColumnInfo, realType, doubleType string) (string, bool) {
	switch col.DamengFloatStorage {
	case "real":
		return realType, true
	case "double":
		return doubleType, true
	default:
		return "", false
	}
}

var mysqlFloatDeclaration = regexp.MustCompile(`(?i)^\s*(float|double(?:\s+precision)?|real)(?:\s*\(\s*([0-9]+)\s*(?:,\s*([0-9]+)\s*)?\))?((?:\s+(?:unsigned|zerofill))*)\s*$`)

// MySQL's NUMERIC_PRECISION/SCALE do not tell us whether FLOAT(M,D) was
// declared. Preserve explicit sizes and modifiers only from COLUMN_TYPE.
func mysqlFloatingDeclaration(col source.ColumnInfo) string {
	base := "double"
	if strings.EqualFold(strings.TrimSpace(col.DataType), "float") {
		base = "float"
	}
	m := mysqlFloatDeclaration.FindStringSubmatch(col.FormattedType)
	if m == nil {
		return base
	}
	declared := strings.ToLower(strings.Join(strings.Fields(m[1]), " "))
	if (declared == "float" && base != "float" && (m[2] == "" || m[3] != "")) ||
		(strings.HasPrefix(declared, "double") && base != "double") {
		return base
	}
	result := base
	if m[2] != "" {
		precision, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			return base
		}
		if m[3] != "" {
			scale, err := strconv.ParseInt(m[3], 10, 64)
			if err != nil || precision < 1 || precision > 255 || scale > 30 || scale > precision {
				return base
			}
			result += fmt.Sprintf("(%d,%d)", precision, scale)
		} else {
			// FLOAT(p) is normalized to FLOAT/DOUBLE by MySQL itself. The
			// canonical DATA_TYPE determines width; p is not a decimal size.
			if declared != "float" || precision > 53 || (precision <= 24) != (base == "float") {
				return base
			}
		}
	}
	seen := map[string]bool{}
	for _, modifier := range strings.Fields(strings.ToLower(m[4])) {
		if seen[modifier] {
			return base
		}
		seen[modifier] = true
		result += " " + modifier
	}
	return result
}
