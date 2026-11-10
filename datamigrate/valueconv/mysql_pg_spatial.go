package valueconv

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strings"

	"dbgold/datamigrate/source"
)

const maxMySQLSpatialDepth = 64
const maxPostgresSpatialSRID = 999999

// MySQLSpatialHEXEWKB converts MySQL's four-byte little-endian SRID prefix
// followed by WKB to PostGIS's HEXEWKB text input. The coordinate bytes are
// preserved exactly; no text formatting, axis swap, or reprojection is applied.
func MySQLSpatialHEXEWKB(raw []byte, dataType string) (string, error) {
	if len(raw) < 9 {
		return "", fmt.Errorf("MySQL 空间值被截断：需要 SRID 和完整 WKB 头，实际 %d 字节", len(raw))
	}
	srid := binary.LittleEndian.Uint32(raw[:4])
	if srid > maxPostgresSpatialSRID {
		return "", fmt.Errorf("MySQL 空间值 SRID %d 超出目标可无损保存的范围 0..%d", srid, maxPostgresSpatialSRID)
	}
	wkb := raw[4:]
	parser := mysqlWKBParser{data: wkb}
	typ, err := parser.geometry(0)
	if err != nil {
		return "", fmt.Errorf("MySQL 空间值 WKB 无效: %w", err)
	}
	if parser.pos != len(wkb) {
		return "", fmt.Errorf("MySQL 空间值 WKB 末尾有 %d 个多余字节", len(wkb)-parser.pos)
	}
	if subtype, spatial := source.MySQLSpatialSubtype(dataType); spatial && subtype != "Geometry" {
		names := [...]string{"", "POINT", "LINESTRING", "POLYGON", "MULTIPOINT", "MULTILINESTRING", "MULTIPOLYGON", "GEOMETRYCOLLECTION"}
		if names[typ] != strings.ToUpper(subtype) {
			return "", fmt.Errorf("MySQL 空间列 %s 与 WKB 类型 %s 不匹配", dataType, names[typ])
		}
	}
	order, _ := mysqlWKBByteOrder(wkb[0]) // validated by geometry
	ewkb := make([]byte, len(raw))
	ewkb[0] = wkb[0]
	order.PutUint32(ewkb[1:5], typ|0x20000000)
	order.PutUint32(ewkb[5:9], srid)
	copy(ewkb[9:], wkb[5:])
	return hex.EncodeToString(ewkb), nil
}

// The validator walks the entire WKB without allocating from untrusted count
// fields. It validates structure only: it does not repair polygon topology or
// remove repeated coordinates, both of which would change source data.
type mysqlWKBParser struct {
	data []byte
	pos  int
}

func mysqlWKBByteOrder(flag byte) (binary.ByteOrder, error) {
	switch flag {
	case 0:
		return binary.BigEndian, nil
	case 1:
		return binary.LittleEndian, nil
	default:
		return nil, fmt.Errorf("非法字节序 %d", flag)
	}
}

func (p *mysqlWKBParser) take(n int) ([]byte, error) {
	if n < 0 || n > len(p.data)-p.pos {
		return nil, fmt.Errorf("位置 %d 数据被截断", p.pos)
	}
	b := p.data[p.pos : p.pos+n]
	p.pos += n
	return b, nil
}

func (p *mysqlWKBParser) count(order binary.ByteOrder, minBytes int) (uint32, error) {
	b, err := p.take(4)
	if err != nil {
		return 0, err
	}
	n := order.Uint32(b)
	if uint64(n) > uint64((len(p.data)-p.pos)/minBytes) {
		return 0, fmt.Errorf("位置 %d 的元素数量 %d 超出剩余数据长度", p.pos-4, n)
	}
	return n, nil
}

func (p *mysqlWKBParser) coordinates(order binary.ByteOrder, n uint32, allowEmpty bool) error {
	if uint64(n) > uint64((len(p.data)-p.pos)/16) {
		return fmt.Errorf("位置 %d 的坐标数量 %d 超出剩余数据长度", p.pos, n)
	}
	for range n {
		b, _ := p.take(16)
		x := math.Float64frombits(order.Uint64(b[:8]))
		y := math.Float64frombits(order.Uint64(b[8:]))
		if allowEmpty && math.IsNaN(x) && math.IsNaN(y) {
			continue
		}
		if math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) {
			return fmt.Errorf("位置 %d 包含非有限坐标", p.pos-16)
		}
	}
	return nil
}

func (p *mysqlWKBParser) geometry(depth int) (uint32, error) {
	if depth >= maxMySQLSpatialDepth {
		return 0, fmt.Errorf("几何集合嵌套超过 %d 层", maxMySQLSpatialDepth)
	}
	header, err := p.take(5)
	if err != nil {
		return 0, err
	}
	order, err := mysqlWKBByteOrder(header[0])
	if err != nil {
		return 0, err
	}
	typ := order.Uint32(header[1:])
	switch typ {
	case 1:
		err = p.coordinates(order, 1, true)
	case 2:
		var n uint32
		n, err = p.count(order, 16)
		if err == nil {
			err = p.coordinates(order, n, false)
		}
	case 3:
		var rings uint32
		rings, err = p.count(order, 4)
		for i := uint32(0); err == nil && i < rings; i++ {
			var n uint32
			n, err = p.count(order, 16)
			if err == nil {
				err = p.coordinates(order, n, false)
			}
		}
	case 4, 5, 6, 7:
		var n uint32
		n, err = p.count(order, 5)
		for i := uint32(0); err == nil && i < n; i++ {
			var child uint32
			child, err = p.geometry(depth + 1)
			if err == nil && typ != 7 && child != typ-3 {
				err = fmt.Errorf("集合类型 %d 包含不匹配的子类型 %d", typ, child)
			}
		}
	default:
		err = fmt.Errorf("不支持的 MySQL 二维 WKB 类型 %d", typ)
	}
	return typ, err
}
