package cdc

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// MySQL's row decoder returns ENUM ordinals, SET bitmaps and signed integer
// bit patterns. Restore the same values that a MySQL SQL query would return.
func decodeMySQLBinlogValue(table *TableInfo, column int, value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	definition := ""
	if column < len(table.ColumnDefinitions) {
		definition = table.ColumnDefinitions[column]
	}
	kind := strings.ToUpper(table.ColumnTypes[column])
	switch kind {
	case "ENUM", "SET":
		return mysqlEnumSetBinlogValue(value, definition, kind)
	case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT":
		if strings.Contains(strings.ToLower(definition), "unsigned") {
			return mysqlUnsignedBinlogValue(value, kind)
		}
	}
	return value, nil
}

func cdcBinlogValueColumns(table *TableInfo) []string {
	var columns []string
	for i, kind := range table.ColumnTypes {
		definition := ""
		if i < len(table.ColumnDefinitions) {
			definition = table.ColumnDefinitions[i]
		}
		if strings.EqualFold(kind, "ENUM") || strings.EqualFold(kind, "SET") || strings.Contains(strings.ToLower(definition), "unsigned") {
			columns = append(columns, table.Columns[i])
		}
	}
	return columns
}

func mysqlUnsignedBinlogValue(value any, dt string) (any, error) {
	var n uint64
	switch v := value.(type) {
	case int8:
		n = uint64(uint8(v))
	case int16:
		n = uint64(uint16(v))
	case int32:
		n = uint64(uint32(v))
		if dt == "MEDIUMINT" {
			n &= 0xffffff
		}
	case int64:
		n = uint64(v)
		switch dt {
		case "TINYINT":
			n &= 0xff
		case "SMALLINT":
			n &= 0xffff
		case "MEDIUMINT":
			n &= 0xffffff
		case "INT", "INTEGER":
			n &= 0xffffffff
		}
	case int:
		return mysqlUnsignedBinlogValue(int64(v), dt)
	case uint8:
		n = uint64(v)
	case uint16:
		n = uint64(v)
	case uint32:
		n = uint64(v)
	case uint64:
		n = v
	case string, []byte:
		return value, nil
	default:
		return nil, fmt.Errorf("UNSIGNED 列不支持 binlog 值类型 %T", value)
	}
	if n > math.MaxInt64 {
		// Decimal text binds the value exactly even through drivers that cast
		// uint64 to int64. The target type still enforces column capacity.
		return strconv.FormatUint(n, 10), nil
	}
	return int64(n), nil
}

func mysqlEnumSetBinlogValue(value any, definition, kind string) (any, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	if text, ok := value.([]byte); ok {
		return string(text), nil
	}
	var n uint64
	switch v := value.(type) {
	case int64:
		n = uint64(v)
	case uint64:
		n = v
	case int:
		n = uint64(v)
	default:
		return nil, fmt.Errorf("%s 列不支持 binlog 值类型 %T", kind, value)
	}
	members, err := mysqlEnumSetMembers(definition, kind)
	if err != nil {
		return nil, err
	}
	if kind == "ENUM" {
		if n == 0 {
			return "", nil
		}
		if n > uint64(len(members)) {
			return nil, fmt.Errorf("ENUM 序号 %d 超出定义", n)
		}
		return members[n-1], nil
	}
	if len(members) > 64 || (len(members) < 64 && n>>len(members) != 0) {
		return nil, fmt.Errorf("SET 位图超出定义")
	}
	selected := make([]string, 0, len(members))
	for i, member := range members {
		if n&(uint64(1)<<i) != 0 {
			selected = append(selected, member)
		}
	}
	return strings.Join(selected, ","), nil
}

// COLUMN_TYPE contains quoted MySQL string literals, not comma-separated
// labels: commas, doubled quotes and backslash escapes may be inside labels.
func mysqlEnumSetMembers(definition, kind string) ([]string, error) {
	prefix := strings.ToLower(kind) + "("
	if !strings.HasPrefix(strings.ToLower(definition), prefix) || !strings.HasSuffix(definition, ")") {
		return nil, fmt.Errorf("缺少有效的 %s COLUMN_TYPE 元数据", kind)
	}
	text := definition[len(prefix) : len(definition)-1]
	var result []string
	for i := 0; i < len(text); {
		if text[i] != '\'' {
			return nil, fmt.Errorf("无效的 %s 成员定义", kind)
		}
		i++
		var member strings.Builder
		closed := false
		for i < len(text) {
			c := text[i]
			i++
			if c == '\'' {
				if i < len(text) && text[i] == '\'' {
					member.WriteByte('\'')
					i++
					continue
				}
				closed = true
				break
			}
			if c == '\\' && i < len(text) {
				c = text[i]
				i++
				switch c {
				case '0':
					c = 0
				case 'b':
					c = '\b'
				case 'n':
					c = '\n'
				case 'r':
					c = '\r'
				case 't':
					c = '\t'
				case 'Z':
					c = 26
				}
			}
			member.WriteByte(c)
		}
		if !closed {
			return nil, fmt.Errorf("未闭合的 %s 成员定义", kind)
		}
		result = append(result, member.String())
		if i == len(text) {
			break
		}
		if text[i] != ',' {
			return nil, fmt.Errorf("无效的 %s 成员分隔符", kind)
		}
		i++
		if i == len(text) {
			return nil, fmt.Errorf("无效的 %s 尾部分隔符", kind)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("%s 成员定义为空", kind)
	}
	return result, nil
}
