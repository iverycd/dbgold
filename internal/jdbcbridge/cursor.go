package jdbcbridge

import (
	"context"
	"fmt"
	"io"
	"math"
)

// ColumnMetadata is JDBC's actual catalog type, not the migration's mapped type.
type ColumnMetadata struct {
	Name, TypeName           string
	Precision, Scale, Length int64
	SQLType                  int32
}
type TableMetadata struct {
	Columns    []ColumnMetadata
	PrimaryKey []string
}

func (m *Manager) DescribeTable(ctx context.Context, session uint64, schema, table string) (TableMetadata, error) {
	e := &encoder{}
	e.uint64(session)
	e.string(schema)
	e.string(table)
	b, err := m.requestExisting(ctx, opDescribeTable, e.Bytes())
	if err != nil {
		return TableMetadata{}, err
	}
	return decodeTableMetadata(b)
}
func decodeTableMetadata(b []byte) (TableMetadata, error) {
	d := newDecoder(b)
	n, err := d.uint32()
	if err != nil {
		return TableMetadata{}, err
	}
	if n > 65536 {
		return TableMetadata{}, fmt.Errorf("invalid bridge metadata column count %d", n)
	}
	out := TableMetadata{}
	for i := uint32(0); i < n; i++ {
		var c ColumnMetadata
		if c.Name, err = d.string(); err != nil {
			return out, err
		}
		if c.TypeName, err = d.string(); err != nil {
			return out, err
		}
		if c.SQLType, err = d.int32(); err != nil {
			return out, err
		}
		if c.Precision, err = d.int64(); err != nil {
			return out, err
		}
		if c.Scale, err = d.int64(); err != nil {
			return out, err
		}
		if c.Length, err = d.int64(); err != nil {
			return out, err
		}
		out.Columns = append(out.Columns, c)
	}
	n, err = d.uint32()
	if err != nil {
		return out, err
	}
	if n > uint32(len(out.Columns)) {
		return out, fmt.Errorf("invalid bridge primary key count")
	}
	for i := uint32(0); i < n; i++ {
		name, e := d.string()
		if e != nil {
			return out, e
		}
		out.PrimaryKey = append(out.PrimaryKey, name)
	}
	if d.Len() != 0 {
		return out, fmt.Errorf("trailing bridge metadata bytes")
	}
	return out, nil
}
func (m *Manager) OpenCursor(ctx context.Context, session uint64, query string) (uint64, error) {
	e := &encoder{}
	e.uint64(session)
	e.string(query)
	b, err := m.requestExisting(ctx, opCursorOpen, e.Bytes())
	if err != nil {
		return 0, err
	}
	return newDecoder(b).uint64()
}
func (m *Manager) FetchCursor(ctx context.Context, cursor uint64, maxRows, maxBytes int) ([][]any, bool, error) {
	if maxRows <= 0 || maxRows > 10000 || maxBytes <= 0 || maxBytes > maxFrameSize-1024 {
		return nil, false, fmt.Errorf("invalid cursor fetch limit")
	}
	e := &encoder{}
	e.uint64(cursor)
	e.uint32(uint32(maxRows))
	e.uint32(uint32(maxBytes))
	b, err := m.requestExisting(ctx, opCursorFetch, e.Bytes())
	if err != nil {
		return nil, false, err
	}
	return decodeCursorRows(b, maxRows, maxBytes)
}
func decodeCursorRows(b []byte, maxRows, maxBytes int) ([][]any, bool, error) {
	if len(b) > maxBytes+16 {
		return nil, false, fmt.Errorf("bridge cursor response exceeds byte limit")
	}
	d := newDecoder(b)
	done, err := d.bool()
	if err != nil {
		return nil, false, err
	}
	n, err := d.uint32()
	if err != nil {
		return nil, false, err
	}
	if n > uint32(maxRows) {
		return nil, false, fmt.Errorf("bridge cursor response exceeds row limit")
	}
	out := make([][]any, 0, n)
	for i := uint32(0); i < n; i++ {
		cols, err := d.uint32()
		if err != nil {
			return nil, false, err
		}
		if cols > 65536 || uint64(cols) > uint64(d.Len()) {
			return nil, false, fmt.Errorf("invalid cursor column count")
		}
		row := make([]any, int(cols))
		for j := range row {
			row[j], err = decodeCursorValue(d)
			if err != nil {
				return nil, false, err
			}
		}
		out = append(out, row)
	}
	if d.Len() != 0 {
		return nil, false, fmt.Errorf("trailing bridge cursor bytes")
	}
	return out, done, nil
}
func decodeCursorValue(d *decoder) (any, error) {
	kind, err := d.byte()
	if err != nil {
		return nil, err
	}
	switch kind {
	case valueNull:
		return nil, nil
	case valueBool:
		return d.bool()
	case valueInt64:
		return d.int64()
	case valueFloat64:
		v, e := d.uint64()
		return math.Float64frombits(v), e
	case valueDecimal, valueString, valueDate, valueTime, valueTimestamp:
		return d.string()
	case valueBytes:
		n, e := d.uint32()
		if e != nil {
			return nil, e
		}
		if uint64(n) > uint64(d.Len()) {
			return nil, io.ErrUnexpectedEOF
		}
		b := make([]byte, int(n))
		_, e = io.ReadFull(d, b)
		return b, e
	default:
		return nil, fmt.Errorf("unsupported cursor value kind %d", kind)
	}
}
func (m *Manager) CloseCursor(ctx context.Context, cursor uint64) error {
	_, err := m.requestExisting(ctx, opCursorClose, sessionBody(cursor))
	return err
}
