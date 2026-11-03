package datamigrate

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
)

// oracleMySQLIndexPart describes the maximum key contribution, without the
// record/NULL/variable-length bookkeeping that does not count toward key length.
type oracleMySQLIndexPart struct {
	fixedBytes int64
	units      int64
	unitBytes  int64
	mandatory  bool // TEXT/BLOB always need an explicit prefix.
}

var oracleMySQLIndexType = regexp.MustCompile(`^([a-z]+)(?:\s*\(\s*([0-9]+)\s*(?:,\s*([0-9]+)\s*)?\))?(?:\s+(?:unsigned|zerofill))*$`)

// planOracleMySQLIndex only adapts ordinary indexes whose target types are
// understood. A full key of unknown type remains MySQL's responsibility. The
// returned source-named copy is safe to share with reporting and repair plans.
func planOracleMySQLIndex(idx source.IndexInfo, columns []target.MySQLIndexColumn, limit int) (source.IndexInfo, string, error) {
	idx.Columns = slices.Clone(idx.Columns)
	idx.PrefixLengths = slices.Clone(idx.PrefixLengths)
	idx.Descending = slices.Clone(idx.Descending)
	idx.Attnums = slices.Clone(idx.Attnums)
	if idx.IsPrimary || idx.IsUnique || idx.Unsupported != "" || len(idx.Columns) == 0 {
		return idx, "", nil
	}
	if limit <= 0 {
		return idx, "", fmt.Errorf("索引 %s 的目标键长上限无效: %d", idx.IndexName, limit)
	}
	byName := make(map[string]target.MySQLIndexColumn, len(columns))
	for _, c := range columns {
		byName[c.Name] = c
	}
	parts := make([]oracleMySQLIndexPart, len(idx.Columns))
	total, fixed, minimum := int64(0), int64(0), int64(0)
	mandatory := false
	for i, name := range idx.Columns {
		c, found := byName[name]
		if !found {
			return idx, "", fmt.Errorf("索引 %s 缺少列 %s 的目标元数据", idx.IndexName, name)
		}
		part, known, err := oracleMySQLIndexColumnPart(c)
		if err != nil {
			return idx, "", fmt.Errorf("索引 %s 列 %s: %w", idx.IndexName, name, err)
		}
		if !known {
			return idx, "", nil
		}
		if part.units > 0 && i < len(idx.PrefixLengths) && idx.PrefixLengths[i] > 0 {
			part.units = min(part.units, int64(idx.PrefixLengths[i]))
		}
		parts[i] = part
		if part.units == 0 {
			fixed += part.fixedBytes
			total += part.fixedBytes
		} else {
			if part.units > (math.MaxInt64-total)/part.unitBytes {
				return idx, "", fmt.Errorf("索引 %s 键长计算溢出", idx.IndexName)
			}
			total += part.units * part.unitBytes
			minimum += part.unitBytes
		}
		mandatory = mandatory || part.mandatory && (i >= len(idx.PrefixLengths) || idx.PrefixLengths[i] <= 0)
	}
	if total <= int64(limit) && !mandatory {
		return idx, "", nil
	}
	if fixed+minimum > int64(limit) {
		return idx, "", fmt.Errorf("索引 %s 的完整固定类型列及最短前缀需要 %d 字节，超过目标 %d 字节上限", idx.IndexName, fixed+minimum, limit)
	}

	// Give each adjustable part one complete character/byte first. Equal shares
	// of the remaining bytes are water-filled, returning short columns' unused
	// budgets to longer columns. Source order resolves the small rounding residue.
	allocated := make([]int64, len(parts))
	active := make([]int, 0, len(parts))
	budget := int64(limit) - fixed - minimum
	for i, part := range parts {
		if part.units > 0 {
			allocated[i] = 1
			if part.units > 1 {
				active = append(active, i)
			}
		}
	}
	for len(active) > 0 && budget > 0 {
		share := budget / int64(len(active))
		remaining := make([]int, 0, len(active))
		capped := false
		for _, i := range active {
			part := parts[i]
			capacity := (part.units - allocated[i]) * part.unitBytes
			if capacity <= share {
				allocated[i] = part.units
				budget -= capacity
				capped = true
			} else {
				remaining = append(remaining, i)
			}
		}
		active = remaining
		if capped {
			continue
		}
		for _, i := range active {
			units := share / parts[i].unitBytes
			allocated[i] += units
			budget -= units * parts[i].unitBytes
		}
		for budget > 0 {
			progress := false
			for _, i := range active {
				if allocated[i] < parts[i].units && budget >= parts[i].unitBytes {
					allocated[i]++
					budget -= parts[i].unitBytes
					progress = true
				}
			}
			if !progress {
				break
			}
		}
		break
	}
	prefixes := make([]int, len(idx.Columns))
	var descriptions []string
	for i, part := range parts {
		if part.units == 0 {
			continue
		}
		existing := i < len(idx.PrefixLengths) && idx.PrefixLengths[i] > 0
		if allocated[i] < part.units || part.mandatory || existing {
			prefixes[i] = int(allocated[i])
			descriptions = append(descriptions, fmt.Sprintf("%s(%d)", idx.Columns[i], prefixes[i]))
		}
	}
	if slices.Equal(prefixes, idx.PrefixLengths) {
		return idx, "", nil
	}
	idx.PrefixLengths = prefixes
	warning := fmt.Sprintf("索引 %s 自动调整为前缀索引（%s）；字段内容与容量保持完整，可能影响排序和覆盖查询", idx.IndexName, strings.Join(descriptions, ", "))
	return idx, warning, nil
}

func oracleMySQLIndexColumnPart(c target.MySQLIndexColumn) (oracleMySQLIndexPart, bool, error) {
	part := oracleMySQLIndexPart{}
	m := oracleMySQLIndexType.FindStringSubmatch(strings.ToLower(strings.TrimSpace(c.DataType)))
	if m == nil {
		return part, false, nil
	}
	first, second := int64(0), int64(0)
	var err error
	if m[2] != "" {
		first, err = strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			return part, true, fmt.Errorf("目标类型长度无效: %s", c.DataType)
		}
	}
	if m[3] != "" {
		second, err = strconv.ParseInt(m[3], 10, 64)
		if err != nil {
			return part, true, fmt.Errorf("目标类型标度无效: %s", c.DataType)
		}
	}
	typ := m[1]
	textBytes := int64(0)
	switch typ {
	case "tinytext", "tinyblob":
		textBytes = 255
	case "text", "blob":
		textBytes = 65535
	case "mediumtext", "mediumblob":
		textBytes = 16777215
	case "longtext", "longblob":
		textBytes = 4294967295
	}
	if textBytes > 0 || typ == "char" || typ == "varchar" || typ == "binary" || typ == "varbinary" {
		part.unitBytes = 1
		binary := typ == "binary" || typ == "varbinary" || strings.HasSuffix(typ, "blob")
		if !binary {
			if c.MaxBytes < 1 {
				return part, true, fmt.Errorf("目标字符集最大字节数缺失")
			}
			part.unitBytes = int64(c.MaxBytes)
		}
		if textBytes > 0 {
			part.units = textBytes / part.unitBytes
			if c.Length > 0 {
				part.units = min(part.units, c.Length)
			}
			part.mandatory = true
		} else {
			part.units = c.Length
			if m[2] != "" {
				part.units = first
			}
		}
		if part.units < 1 {
			return part, true, fmt.Errorf("目标字符串或二进制列长度无效")
		}
		return part, true, nil
	}
	switch typ {
	case "tinyint", "bool", "boolean":
		part.fixedBytes = 1
	case "smallint":
		part.fixedBytes = 2
	case "mediumint":
		part.fixedBytes = 3
	case "int", "integer":
		part.fixedBytes = 4
	case "bigint":
		part.fixedBytes = 8
	case "float":
		part.fixedBytes = 4
		if m[2] != "" && m[3] == "" && first > 24 {
			part.fixedBytes = 8
		}
	case "double", "real":
		part.fixedBytes = 8
	case "decimal", "numeric":
		precision, scale := c.Precision, c.Scale
		if m[2] != "" {
			precision, scale = first, second
		} else if precision == 0 {
			precision, scale = 10, 0
		}
		if precision < 1 || precision > 65 || scale < 0 || scale > 30 || scale > precision {
			return part, true, fmt.Errorf("目标 DECIMAL 精度或标度无效")
		}
		remainder := [...]int64{0, 1, 1, 2, 2, 3, 3, 4, 4}
		integerDigits := precision - scale
		part.fixedBytes = integerDigits/9*4 + remainder[integerDigits%9] + scale/9*4 + remainder[scale%9]
	case "bit":
		bits := first
		if m[2] == "" {
			bits = 1
		}
		if bits < 1 || bits > 64 {
			return part, true, fmt.Errorf("目标 BIT 位数无效")
		}
		part.fixedBytes = (bits + 7) / 8
	case "date":
		part.fixedBytes = 3
	case "year":
		part.fixedBytes = 1
	case "time", "datetime", "timestamp":
		precision := c.DateTimePrecision
		if m[2] != "" {
			precision = first
		}
		if precision < 0 || precision > 6 {
			return part, true, fmt.Errorf("目标时间小数位数无效")
		}
		part.fixedBytes = map[string]int64{"time": 3, "datetime": 5, "timestamp": 4}[typ] + (precision+1)/2
	default:
		return part, false, nil
	}
	return part, true, nil
}
