package cdc

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
)

type damengJSONPathPart struct {
	key   string
	index int
	array bool
}
type damengJSONOpaque struct {
	parts            []damengJSONPathPart
	sqlType          string
	typ              byte
	precision, scale int
}
type damengJSONPatch struct {
	Path    string
	SQLType string
	Value   string
}
type damengJSONDocument struct {
	Text    string
	Patches []damengJSONPatch
}

func (d damengJSONDocument) key() string { data, _ := json.Marshal(d); return string(data) }
func (d damengJSONDocument) query() (string, []any) {
	query := damengSourceJSONExpression
	args := []any{d.Text}
	if len(d.Patches) == 0 {
		return query, args
	}
	var parts []string
	for _, patch := range d.Patches {
		value := "?"
		if patch.SQLType != "" {
			value = "CAST(? AS " + patch.SQLType + ")"
		}
		parts = append(parts, "?,"+value)
		args = append(args, patch.Path, patch.Value)
	}
	return "JSON_SET(" + query + "," + strings.Join(parts, ",") + ")", args
}

// Only structural offsets and opaque metadata are read here; go-mysql keeps
// responsibility for decoding the actual decimal and temporal payload values.
func collectDamengJSONOpaque(typ byte, data []byte, depth int, path []damengJSONPathPart) ([]damengJSONOpaque, error) {
	if depth > 128 {
		return nil, fmt.Errorf("JSON binary 嵌套过深")
	}
	if typ == replication.JSONB_OPAQUE {
		if len(data) < 2 {
			return nil, fmt.Errorf("JSON opaque 值损坏")
		}
		length, n := binary.Uvarint(data[1:])
		if n <= 0 || n > 5 || length > uint64(len(data)-1-n) {
			return nil, fmt.Errorf("JSON opaque 长度损坏")
		}
		payload := data[1+n : 1+n+int(length)]
		item := damengJSONOpaque{parts: append([]damengJSONPathPart(nil), path...), typ: data[0]}
		switch item.typ {
		case gomysql.MYSQL_TYPE_NEWDECIMAL:
			if len(payload) < 3 {
				return nil, fmt.Errorf("JSON DECIMAL 元数据不完整")
			}
			item.precision, item.scale = int(payload[0]), int(payload[1])
			if item.precision < 1 || item.precision > 81 || item.scale > 30 || item.scale > item.precision {
				return nil, fmt.Errorf("JSON DECIMAL 精度无效")
			}
			// Binary JSON stores the internal decimal precision, which can be
			// rounded to base-10^9 groups (e.g. 66 for SQL DECIMAL(65,30)).
			// The SQL cast precision is bounded only after decoding the value.
			item.sqlType = fmt.Sprintf("DECIMAL(%d,%d)", min(item.precision, 65), item.scale)
			digitsToBytes := [...]int{0, 1, 1, 2, 2, 3, 3, 4, 4, 4}
			integer := item.precision - item.scale
			expected := 2 + integer/9*4 + digitsToBytes[integer%9] + item.scale/9*4 + digitsToBytes[item.scale%9]
			if len(payload) != expected {
				return nil, fmt.Errorf("JSON DECIMAL 二进制长度无效")
			}
		case gomysql.MYSQL_TYPE_DATE:
			item.sqlType = "DATE"
		case gomysql.MYSQL_TYPE_DATETIME, gomysql.MYSQL_TYPE_TIMESTAMP:
			item.sqlType = "DATETIME(6)"
		case gomysql.MYSQL_TYPE_TIME:
			item.sqlType = "TIME(6)"
		default:
			return nil, fmt.Errorf("暂不支持 JSON opaque 类型 %d，原检查点保留", item.typ)
		}
		if item.typ != gomysql.MYSQL_TYPE_NEWDECIMAL && len(payload) != 8 {
			return nil, fmt.Errorf("JSON 时间扩展值长度无效")
		}
		return []damengJSONOpaque{item}, nil
	}
	object := typ == replication.JSONB_SMALL_OBJECT || typ == replication.JSONB_LARGE_OBJECT
	array := typ == replication.JSONB_SMALL_ARRAY || typ == replication.JSONB_LARGE_ARRAY
	if !object && !array {
		return nil, nil
	}
	width := 2
	if typ == replication.JSONB_LARGE_OBJECT || typ == replication.JSONB_LARGE_ARRAY {
		width = 4
	}
	if len(data) < 2*width {
		return nil, fmt.Errorf("JSON binary 容器头损坏")
	}
	read := func(b []byte) uint64 {
		if width == 2 {
			return uint64(binary.LittleEndian.Uint16(b))
		}
		return uint64(binary.LittleEndian.Uint32(b))
	}
	count, size := read(data[:width]), read(data[width:2*width])
	entry := width + 1
	headerEntry := entry
	if object {
		headerEntry += width + 2
	}
	if size > uint64(len(data)) || size < uint64(2*width) || count > (size-uint64(2*width))/uint64(headerEntry) {
		return nil, fmt.Errorf("JSON binary 容器长度损坏")
	}
	start := 2 * width
	if object {
		start += int(count) * (width + 2)
	}
	var result []damengJSONOpaque
	for i := 0; i < int(count); i++ {
		at := start + i*entry
		childType := data[at]
		inline := childType == replication.JSONB_LITERAL || childType == replication.JSONB_INT16 || childType == replication.JSONB_UINT16 || (width == 4 && (childType == replication.JSONB_INT32 || childType == replication.JSONB_UINT32))
		if inline {
			continue
		}
		offset := read(data[at+1 : at+entry])
		if offset >= size {
			return nil, fmt.Errorf("JSON binary 子值位置损坏")
		}
		part := damengJSONPathPart{array: true, index: i}
		if object {
			keyEntry := 2*width + i*(width+2)
			keyOffset := read(data[keyEntry : keyEntry+width])
			keyLength := uint64(binary.LittleEndian.Uint16(data[keyEntry+width : keyEntry+width+2]))
			if keyOffset > size || keyLength > size-keyOffset {
				return nil, fmt.Errorf("JSON binary 键位置损坏")
			}
			part = damengJSONPathPart{key: string(data[int(keyOffset):int(keyOffset+keyLength)])}
		}
		items, err := collectDamengJSONOpaque(childType, data[int(offset):int(size)], depth+1, append(path, part))
		if err != nil {
			return nil, err
		}
		result = append(result, items...)
	}
	return result, nil
}

func buildDamengJSONDocument(text string, opaque []damengJSONOpaque) (damengJSONDocument, error) {
	doc := damengJSONDocument{Text: text}
	if len(opaque) == 0 {
		return doc, nil
	}
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return doc, err
	}
	for _, item := range opaque {
		value := decoded
		path := "$"
		for _, part := range item.parts {
			if part.array {
				array, ok := value.([]any)
				if !ok || part.index < 0 || part.index >= len(array) {
					return doc, fmt.Errorf("JSON opaque 数组位置不匹配")
				}
				value = array[part.index]
				path += "[" + strconv.Itoa(part.index) + "]"
			} else {
				object, ok := value.(map[string]any)
				if !ok {
					return doc, fmt.Errorf("JSON opaque 对象位置不匹配")
				}
				value, ok = object[part.key]
				if !ok {
					return doc, fmt.Errorf("JSON opaque 键不存在")
				}
				quoted, _ := json.Marshal(part.key)
				path += "." + string(quoted)
			}
		}
		text, ok := value.(string)
		if !ok {
			return doc, fmt.Errorf("JSON opaque 解码值类型无效: %T", value)
		}
		patch := damengJSONPatch{Path: path, SQLType: item.sqlType, Value: text}
		if item.typ == gomysql.MYSQL_TYPE_NEWDECIMAL {
			if !validDamengJSONDecimal(text, min(item.precision, 65), item.scale) {
				return doc, fmt.Errorf("JSON DECIMAL 值不符合原始精度")
			}
		} else {
			// Temporal JSON values are rendered as strings by MySQL. Reproduce
			// that representation directly: CAST under a different sql_mode may
			// turn a historical zero/ALLOW_INVALID_DATES value into SQL NULL.
			patch.SQLType = ""
			switch item.typ {
			case gomysql.MYSQL_TYPE_DATE:
				if len(text) < 10 {
					return doc, fmt.Errorf("JSON DATE 解码值无效")
				}
				patch.Value = text[:10]
			case gomysql.MYSQL_TYPE_DATETIME, gomysql.MYSQL_TYPE_TIMESTAMP, gomysql.MYSQL_TYPE_TIME:
				if !strings.Contains(text, ".") {
					patch.Value = text + ".000000"
				}
			}
		}
		doc.Patches = append(doc.Patches, patch)
	}
	return doc, nil
}

func validDamengJSONDecimal(text string, precision, scale int) bool {
	text = strings.TrimPrefix(text, "-")
	parts := strings.Split(text, ".")
	if len(parts) > 2 || len(parts) == 0 || parts[0] == "" {
		return false
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if len(fraction) != scale || len(strings.TrimLeft(parts[0], "0")) > precision-scale {
		return false
	}
	for _, digit := range parts[0] + fraction {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}
