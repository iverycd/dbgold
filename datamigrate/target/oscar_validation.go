package target

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"dbgold/datamigrate/validation"
	"dbgold/internal/jdbcbridge"
)

// Keep validation optional: existing writers and bridge implementations do not
// gain a mandatory new method on the migration interfaces.
type oscarValidationBridge interface {
	DescribeTable(context.Context, uint64, string, string) (jdbcbridge.TableMetadata, error)
	OpenCursor(context.Context, uint64, string) (uint64, error)
	FetchCursor(context.Context, uint64, int, int) ([][]any, bool, error)
	CloseCursor(context.Context, uint64) error
}

func (w *OscarWriter) validationBridge() (oscarValidationBridge, error) {
	b, ok := w.bridge.(oscarValidationBridge)
	if !ok {
		return nil, fmt.Errorf("Oscar JDBC bridge does not support lossless validation scans")
	}
	return b, nil
}
func (w *OscarWriter) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	b, err := w.validationBridge()
	if err != nil {
		return validation.Table{}, err
	}
	m, err := b.DescribeTable(ctx, w.session, w.schema, table)
	if err != nil {
		return validation.Table{}, err
	}
	out := validation.Table{PrimaryKey: m.PrimaryKey}
	for _, raw := range m.Columns {
		c := validation.Column{Name: raw.Name, DatabaseType: raw.TypeName, Precision: raw.Precision, Scale: raw.Scale, TimePrecision: raw.Scale, Length: raw.Length}
		validation.ClassifyColumn("oscar", &c)
		if c.Kind == validation.Bits {
			c.Kind = validation.Unsupported
		}
		out.Columns = append(out.Columns, c)
	}
	return out, nil
}
func (w *OscarWriter) OpenValidationScan(ctx context.Context, table string, columns []validation.Column) (validation.RowStream, error) {
	b, err := w.validationBridge()
	if err != nil {
		return nil, err
	}
	actual, err := w.DescribeValidationTable(ctx, table)
	if err != nil {
		return nil, err
	}
	expr := make([]string, len(columns))
	for i, c := range columns {
		if c.Kind == validation.Unsupported {
			return nil, fmt.Errorf("column %s type %s has no lossless JDBC validation serializer", c.Name, c.DatabaseType)
		}
		found := false
		for _, got := range actual.Columns {
			if c.Name == got.Name {
				found = true
				if !reflect.DeepEqual(c, got) {
					return nil, fmt.Errorf("column %s metadata changed", c.Name)
				}
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("column %s disappeared", c.Name)
		}
		expr[i] = validation.QuoteValidationIdent("oscar", c.Name)
	}
	q := "SELECT " + strings.Join(expr, ",") + " FROM " + w.dia.QualifyTable(w.schema, table)
	id, err := b.OpenCursor(ctx, w.session, q)
	if err != nil {
		return nil, err
	}
	return &oscarValidationStream{bridge: b, id: id}, nil
}

type oscarValidationStream struct {
	bridge       oscarValidationBridge
	id           uint64
	rows         [][]any
	done, closed bool
}

func (s *oscarValidationStream) Next(ctx context.Context) ([]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed {
		return nil, io.EOF
	}
	if len(s.rows) == 0 {
		if s.done {
			return nil, io.EOF
		}
		var err error
		s.rows, s.done, err = s.bridge.FetchCursor(ctx, s.id, 128, 8<<20)
		if err != nil {
			return nil, err
		}
		if len(s.rows) == 0 {
			if s.done {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("Oscar JDBC cursor returned no rows without completion")
		}
	}
	row := s.rows[0]
	s.rows[0] = nil
	s.rows = s.rows[1:]
	return row, nil
}
func (s *oscarValidationStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.rows = nil
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.bridge.CloseCursor(ctx, s.id)
}
