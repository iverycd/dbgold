package cdc

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"dbgold/datamigrate/valueconv"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
)

// RowsEvent exposes ENUM/SET as ordinals/bitmaps and integers as signed
// numbers. Restore the values produced by the full MySQL reader before using
// its established DM value/date conversion. Source rows are never mutated.
func convertDamengBinlogRow(table *TableInfo, row []any) ([]any, error) {
	if table == nil || len(row) != len(table.Columns) || len(table.ColumnTypes) != len(table.Columns) {
		return nil, fmt.Errorf("CDC 行与源列元数据不匹配")
	}
	values := make([]any, len(row))
	converter := valueconv.NewDaMeng()
	for i, value := range row {
		dt := strings.ToUpper(table.ColumnTypes[i])
		definition := ""
		if i < len(table.ColumnDefinitions) {
			definition = table.ColumnDefinitions[i]
		}
		var err error
		if value != nil {
			switch dt {
			case "ENUM", "SET":
				value, err = damengEnumSetValue(value, definition, dt)
			case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT":
				if strings.Contains(strings.ToLower(definition), "unsigned") {
					value, err = damengUnsignedValue(value, dt)
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", table.Name, table.Columns[i], err)
		}
		if value != nil && damengMySQLCharacterType(dt) {
			charset := ""
			if i < len(table.ColumnCharsets) {
				charset = table.ColumnCharsets[i]
			}
			value, err = decodeDamengMySQLText(value, charset)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", table.Name, table.Columns[i], err)
			}
		}
		binary := strings.Contains(dt, "BLOB") || strings.Contains(dt, "BINARY") || dt == "BIT" || dt == "GEOMETRY"
		if !binary {
			switch text := value.(type) {
			case string:
				value = strings.ReplaceAll(text, "\x00", "")
			case []byte:
				value = strings.ReplaceAll(string(text), "\x00", "")
			}
		}
		value = converter.Convert(value, "mysql", dt)
		if dt == "DATE" || dt == "DATETIME" || dt == "TIMESTAMP" {
			value, err = valueconv.MySQLDamengDate(value, dt)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", table.Name, table.Columns[i], err)
			}
		}
		values[i] = value
	}
	return values, nil
}

func damengMySQLCharacterType(dt string) bool {
	switch dt {
	case "CHAR", "VARCHAR", "TINYTEXT", "TEXT", "MEDIUMTEXT", "LONGTEXT":
		return true
	}
	return false
}

func decodeDamengMySQLText(value any, charset string) (string, error) {
	var raw []byte
	switch v := value.(type) {
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	default:
		return "", fmt.Errorf("字符列不支持 binlog 值类型 %T", value)
	}
	charset = strings.ToLower(charset)
	if charset == "ascii" {
		for _, b := range raw {
			if b > 127 {
				return "", fmt.Errorf("非法 ASCII binlog 文本")
			}
		}
		return string(raw), nil
	}
	if charset == "" || charset == "utf8" || charset == "utf8mb3" || charset == "utf8mb4" {
		if !utf8.Valid(raw) {
			return "", fmt.Errorf("非法 UTF-8 binlog 文本")
		}
		return string(raw), nil
	}
	var codec encoding.Encoding
	switch charset {
	case "ucs2", "utf16":
		codec = unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM)
	case "utf16le":
		codec = unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)
	case "utf32":
		codec = utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM)
	default:
		aliases := map[string]string{"latin1": "windows-1252", "latin2": "ISO-8859-2", "latin5": "ISO-8859-9", "latin7": "ISO-8859-13", "cp1250": "windows-1250", "cp1251": "windows-1251", "cp1256": "windows-1256", "cp1257": "windows-1257", "sjis": "Shift_JIS", "cp932": "Shift_JIS", "ujis": "EUC-JP", "eucjpms": "EUC-JP", "euckr": "EUC-KR", "koi8r": "KOI8-R", "koi8u": "KOI8-U", "greek": "ISO-8859-7", "hebrew": "ISO-8859-8", "tis620": "TIS-620", "macroman": "macintosh"}
		name := charset
		if alias := aliases[charset]; alias != "" {
			name = alias
		}
		var err error
		codec, err = ianaindex.MIME.Encoding(name)
		if err != nil || codec == nil {
			return "", fmt.Errorf("暂不支持 MySQL binlog 字符集 %s", charset)
		}
	}
	decoded, err := codec.NewDecoder().Bytes(raw)
	if err != nil {
		return "", fmt.Errorf("解码 MySQL %s 文本: %w", charset, err)
	}
	// x/text decoders may substitute U+FFFD for invalid byte sequences without
	// returning an error. A round trip rejects those substitutions.
	roundtrip, err := codec.NewEncoder().Bytes(decoded)
	if err != nil || !bytes.Equal(raw, roundtrip) {
		return "", fmt.Errorf("MySQL %s binlog 文本不能无损解码", charset)
	}
	return string(decoded), nil
}

func damengUnsignedValue(value any, dt string) (any, error) {
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
		return damengUnsignedValue(int64(v), dt)
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
		// The DM driver casts uint64 to int64. Decimal text lets the target
		// reject an insufficient column range instead of storing a negative ID.
		return strconv.FormatUint(n, 10), nil
	}
	return int64(n), nil
}

func damengEnumSetValue(value any, definition, kind string) (any, error) {
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
