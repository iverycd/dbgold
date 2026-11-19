package cdc

import (
	"bytes"
	"fmt"
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
		value, err := decodeMySQLBinlogValue(table, i, value)
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
