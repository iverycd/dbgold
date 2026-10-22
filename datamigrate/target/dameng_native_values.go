package target

import (
	"fmt"
	"math"
	"reflect"

	"dbgold/datamigrate/damengnative"
	"gitee.com/chunanyong/dm"
)

// ValidateDamengNativeValue is pure apart from reading an already fetched UDT's
// attributes/LOB value. It never opens a destination connection or writes SQL.
func ValidateDamengNativeValue(column damengnative.Column, value any) error {
	if err := ValidateDamengNativeColumn(column); err != nil {
		return err
	}
	if !column.Writable {
		return nil
	}
	if _, err := DamengNativeValueSize(value); err != nil {
		return err
	}
	_, _, err := nativeCopyValue(column, value, column.TypeSchema)
	return err
}

// DamengNativeValueSize traverses source UDT attributes so nested LOBs count
// against the same batch byte budget as ordinary scalar columns.
func DamengNativeValueSize(value any) (int, error) { return nativeValueSize(value, 0) }
func nativeValueSize(value any, depth int) (int, error) {
	if depth > 64 {
		return 0, fmt.Errorf("达梦对象值嵌套超过 64 层，不能安全复制")
	}
	switch v := value.(type) {
	case nil:
		return 0, nil
	case string:
		return len(v), nil
	case []byte:
		return len(v), nil
	case *dm.DmBlob:
		if v == nil || !v.Valid {
			return 0, nil
		}
		length, err := v.GetLength()
		if err != nil {
			return 0, err
		}
		if length < 0 || length > math.MaxInt32 {
			return 0, fmt.Errorf("对象 BLOB 超出驱动可无损读取的长度")
		}
		return int(length), nil
	case *dm.DmClob:
		if v == nil || !v.Valid {
			return 0, nil
		}
		length, err := v.GetLength()
		if err != nil {
			return 0, err
		}
		if length < 0 || length > math.MaxInt32 {
			return 0, fmt.Errorf("对象 CLOB 超出驱动可无损读取的长度")
		}
		return int(length) * 4, nil
	case dm.DmBlob:
		return nativeValueSize(&v, depth)
	case dm.DmClob:
		return nativeValueSize(&v, depth)
	case *dm.DmStruct:
		if v == nil || !v.Valid {
			return 0, nil
		}
		attributes, err := v.GetAttributes()
		if err != nil {
			return 0, err
		}
		size := 32
		for _, a := range attributes {
			n, err := nativeValueSize(a, depth+1)
			if err != nil {
				return 0, err
			}
			size += n
		}
		return size, nil
	case *dm.DmArray:
		if v == nil || !v.Valid {
			return 0, nil
		}
		values, err := v.GetArray()
		if err != nil {
			return 0, err
		}
		if values == nil {
			return 32, nil
		}
		rv := reflect.ValueOf(values)
		if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
			return 0, fmt.Errorf("达梦数组元素无法读取: %T", values)
		}
		size := 32
		for i := 0; i < rv.Len(); i++ {
			n, err := nativeValueSize(rv.Index(i).Interface(), depth+1)
			if err != nil {
				return 0, err
			}
			size += n
		}
		return size, nil
	default:
		return 32, nil
	}
}
