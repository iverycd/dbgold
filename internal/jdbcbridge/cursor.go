package jdbcbridge

import (
	"context"
	"errors"
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

// OpenOscarDamengCursor uses the Oscar driver's native string representation
// for the selected zero-based columns, leaving the existing codecs in place
// for fixed-point numbers, payload streams, and other ordinary values.
func (m *Manager) OpenOscarDamengCursor(ctx context.Context, session uint64, query string, textColumns []int) (uint64, error) {
	if len(textColumns) == 0 {
		return m.OpenCursor(ctx, session, query)
	}
	if len(textColumns) > 65536 {
		return 0, fmt.Errorf("too many Oscar text cursor columns")
	}
	e := &encoder{}
	e.uint64(session)
	e.string(query)
	e.uint32(uint32(len(textColumns)))
	for _, i := range textColumns {
		if i < 0 || i >= 65536 {
			return 0, fmt.Errorf("invalid Oscar text cursor column %d", i)
		}
		e.uint32(uint32(i))
	}
	b, err := m.requestExisting(ctx, opOscarDamengCursorOpen, e.Bytes())
	if err != nil {
		var remote *BridgeError
		if errors.As(err, &remote) && remote.Message == fmt.Sprintf("unsupported operation %d", opOscarDamengCursorOpen) {
			return 0, fmt.Errorf("Oscar JDBC Bridge JAR 版本旧，达梦迁移需要部署当前程序附带的 JAR: %w", err)
		}
		return 0, err
	}
	return newDecoder(b).uint64()
}

// OpenBoundCursor binds catalog filters as values, never as SQL identifiers.
func (m *Manager) OpenBoundCursor(ctx context.Context, session uint64, query string, args ...string) (uint64, error) {
	e := &encoder{}
	e.uint64(session)
	e.string(query)
	e.uint32(uint32(len(args)))
	for _, arg := range args {
		e.string(arg)
	}
	b, err := m.requestExisting(ctx, opBoundCursorOpen, e.Bytes())
	if err != nil {
		return 0, err
	}
	return newDecoder(b).uint64()
}

type MigrationColumn struct {
	Name, Type, AutoIncrement, Generated, Comment string
	Size, Scale, OctetLength, Ordinal             int64
	SQLType                                       int32
	Nullable                                      bool
	Default                                       *string
}

func (m *Manager) MigrationColumns(ctx context.Context, session uint64, schema, table string) ([]MigrationColumn, error) {
	e := &encoder{}
	e.uint64(session)
	e.string(schema)
	e.string(table)
	b, err := m.requestExisting(ctx, opMigrationColumns, e.Bytes())
	if err != nil {
		return nil, err
	}
	d := newDecoder(b)
	n, err := d.uint32()
	if err != nil {
		return nil, err
	}
	if n == 0 || n > 65536 {
		return nil, fmt.Errorf("invalid bridge migration column count %d", n)
	}
	columns := make([]MigrationColumn, n)
	for i := range columns {
		c := &columns[i]
		for _, field := range []*string{&c.Name, &c.Type, &c.AutoIncrement, &c.Generated, &c.Comment} {
			if *field, err = d.string(); err != nil {
				return nil, err
			}
		}
		for _, field := range []*int64{&c.Size, &c.Scale, &c.OctetLength, &c.Ordinal} {
			if *field, err = d.int64(); err != nil {
				return nil, err
			}
		}
		if c.SQLType, err = d.int32(); err != nil {
			return nil, err
		}
		if c.Nullable, err = d.bool(); err != nil {
			return nil, err
		}
		hasDefault, err := d.bool()
		if err != nil {
			return nil, err
		}
		if hasDefault {
			value, err := d.string()
			if err != nil {
				return nil, err
			}
			c.Default = &value
		}
	}
	if d.Len() != 0 {
		return nil, fmt.Errorf("trailing bridge migration column bytes")
	}
	return columns, nil
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

// MaxMigrationCursorRowBytes leaves room for the framed protocol header.
const MaxMigrationCursorRowBytes = maxFrameSize - 1024

// FetchMigrationCursor keeps ordinary batches bounded and allows a larger
// individual row to cross the bridge alone, subject to available JVM heap.
func (m *Manager) FetchMigrationCursor(ctx context.Context, cursor uint64, maxRows, maxBytes int) ([][]any, bool, error) {
	if maxRows <= 0 || maxRows > 10000 || maxBytes <= 0 || maxBytes > MaxMigrationCursorRowBytes {
		return nil, false, fmt.Errorf("invalid migration cursor fetch limit")
	}
	e := &encoder{}
	e.uint64(cursor)
	e.uint32(uint32(maxRows))
	e.uint32(uint32(maxBytes))
	b, err := m.requestExisting(ctx, opMigrationCursorFetch, e.Bytes())
	if err != nil {
		return nil, false, err
	}
	rows, done, err := decodeCursorRows(b, maxRows, MaxMigrationCursorRowBytes)
	if err == nil && len(b) > maxBytes && len(rows) != 1 {
		return nil, false, fmt.Errorf("bridge migration response exceeded batch limit without an isolated row")
	}
	return rows, done, err
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
