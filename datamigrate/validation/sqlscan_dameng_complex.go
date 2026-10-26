package validation

import (
	"context"
	"encoding/binary"
	"math"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"gitee.com/chunanyong/dm"
)

// Only the DM scanner constructs these cells, directly from the driver's native
// values. A JSON/text cast cannot stand in for an object: it may omit NULLs,
// coerce numbers or rewrite LOBs. Runtime class IDs intentionally do not enter
// the content encoding; the native migration separately proves type definitions.
type damengComplexCell []byte

func isDamengComplexType(name string) bool {
	if !strings.HasPrefix(name, "class") || len(name) == len("class") {
		return false
	}
	for _, r := range name[len("class"):] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

type damengComplexEncoder struct {
	ctx    context.Context
	limit  int
	data   []byte
	active map[any]bool
}

func encodeDamengComplex(ctx context.Context, value any, limit int) ([]byte, error) {
	switch value.(type) {
	case *dm.DmStruct, *dm.DmArray:
	default:
		return nil, unsupported("原生复杂列返回了 %T", value)
	}
	if limit > MaxValueBytes {
		limit = MaxValueBytes
	}
	e := damengComplexEncoder{ctx: ctx, limit: limit, active: make(map[any]bool)}
	if err := e.node(value, 0); err != nil {
		return nil, err
	}
	return e.data, nil
}

func (e *damengComplexEncoder) append(data ...byte) error {
	if len(data) > e.limit-len(e.data) {
		return unsupported("复杂值超过本行剩余校验字节预算")
	}
	e.data = append(e.data, data...)
	return nil
}

func (e *damengComplexEncoder) scalar(tag byte, data []byte) error {
	var header [11]byte
	header[0] = tag
	n := binary.PutUvarint(header[1:], uint64(len(data)))
	if len(data)+n+1 > e.limit-len(e.data) {
		return unsupported("复杂值超过本行剩余校验字节预算")
	}
	if err := e.append(header[:n+1]...); err != nil {
		return err
	}
	return e.append(data...)
}

func (e *damengComplexEncoder) containerHeader(tag byte, count int) error {
	var header [11]byte
	header[0] = tag
	n := binary.PutUvarint(header[1:], uint64(count))
	return e.append(header[:n+1]...)
}

func (e *damengComplexEncoder) container(tag byte, values []any, depth int) error {
	if err := e.containerHeader(tag, len(values)); err != nil {
		return err
	}
	for _, value := range values {
		if err := e.node(value, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (e *damengComplexEncoder) node(value any, depth int) error {
	if err := e.ctx.Err(); err != nil {
		return err
	}
	if depth > 128 {
		return unsupported("复杂值嵌套超过校验上限")
	}
	switch v := value.(type) {
	case nil:
		return e.append(0)
	case *dm.DmStruct:
		if v == nil || !v.Valid {
			return e.append(0)
		}
		if e.active[v] {
			return unsupported("复杂对象包含循环引用")
		}
		e.active[v] = true
		defer delete(e.active, v)
		values, err := v.GetAttributes()
		if err != nil {
			return err
		}
		return e.container(1, values, depth)
	case *dm.DmArray:
		if v == nil || !v.Valid {
			return e.append(0)
		}
		if e.active[v] {
			return unsupported("复杂集合包含循环引用")
		}
		e.active[v] = true
		defer delete(e.active, v)
		items, err := v.GetArray()
		if err != nil {
			return err
		}
		count := 0
		var array reflect.Value
		if items != nil {
			array = reflect.ValueOf(items)
			if array.Kind() != reflect.Slice && array.Kind() != reflect.Array {
				return unsupported("原生集合返回了 %T", items)
			}
			count = array.Len()
			if count > e.limit-len(e.data) {
				return unsupported("复杂集合元素数超过校验预算")
			}
		}
		if err := e.containerHeader(2, count); err != nil {
			return err
		}
		for i := 0; i < count; i++ {
			if err := e.node(array.Index(i).Interface(), depth+1); err != nil {
				return err
			}
		}
		return nil
	case string:
		if len(v) > e.limit-len(e.data) {
			return unsupported("复杂文本超过校验预算")
		}
		if !utf8.ValidString(v) {
			return unsupported("复杂文本不是有效 UTF-8")
		}
		return e.scalar(3, []byte(v))
	case []byte:
		return e.scalar(4, v)
	case bool:
		if v {
			return e.append(8, 1)
		}
		return e.append(8, 0)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		s, err := scalarText(v)
		if err != nil {
			return err
		}
		return e.scalar(5, []byte(s))
	case float32:
		var bits [4]byte
		binary.BigEndian.PutUint32(bits[:], math.Float32bits(v))
		return e.scalar(6, bits[:])
	case float64:
		var bits [8]byte
		binary.BigEndian.PutUint64(bits[:], math.Float64bits(v))
		return e.scalar(7, bits[:])
	case time.Time:
		// Preserve wall time and numeric timezone as stored by the native codec.
		return e.scalar(9, []byte(v.Format(time.RFC3339Nano)))
	case *dm.DmDecimal:
		if v == nil || !v.Valid {
			return e.append(0)
		}
		s, err := canonicalNumber(v.String(), Column{Kind: Number})
		if err != nil {
			return err
		}
		return e.scalar(10, []byte(s))
	case dm.DmDecimal:
		return e.node(&v, depth)
	case *dm.DmClob:
		if v == nil || !v.Valid {
			return e.append(0)
		}
		length, err := v.GetLength()
		if err != nil {
			return err
		}
		if length < 0 || length > int64(e.limit-len(e.data)) {
			return unsupported("复杂 CLOB 长度超过校验预算")
		}
		var data []byte
		for pos := int64(1); pos <= length; {
			if err := e.ctx.Err(); err != nil {
				return err
			}
			count := min(int64(4096), length-pos+1)
			text, err := v.ReadString(int(pos), int(count))
			if err != nil {
				return err
			}
			if int64(utf8.RuneCountInString(text)) != count || !utf8.ValidString(text) {
				return unsupported("复杂 CLOB 未完整返回原始 Unicode 文本")
			}
			if len(text) > e.limit-len(e.data)-len(data) {
				return unsupported("复杂 CLOB 字节数超过校验预算")
			}
			data = append(data, text...)
			pos += count
		}
		return e.scalar(3, data)
	case dm.DmClob:
		return e.node(&v, depth)
	case *dm.DmBlob:
		if v == nil || !v.Valid {
			return e.append(0)
		}
		length, err := v.GetLength()
		if err != nil {
			return err
		}
		if length < 0 || length > int64(e.limit-len(e.data)) {
			return unsupported("复杂 BLOB 长度超过校验预算")
		}
		data := make([]byte, int(length))
		if length > 0 {
			n, err := v.ReadAt(1, data)
			if err != nil {
				return err
			}
			if n != len(data) {
				return unsupported("复杂 BLOB 读取不完整")
			}
		}
		return e.scalar(4, data)
	case dm.DmBlob:
		return e.node(&v, depth)
	case *dm.DmIntervalDT:
		if v == nil || !v.Valid {
			return e.append(0)
		}
		return e.scalar(11, []byte(v.String()))
	case dm.DmIntervalDT:
		return e.node(&v, depth)
	case *dm.DmIntervalYM:
		if v == nil || !v.Valid {
			return e.append(0)
		}
		return e.scalar(12, []byte(v.String()))
	case dm.DmIntervalYM:
		return e.node(&v, depth)
	default:
		return unsupported("复杂成员类型 %T 没有无损内容编码器", value)
	}
}
