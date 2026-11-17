package source

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"

	"dbgold/datamigrate/validation"
)

func (r *OscarReader) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	raw, err := r.columns(ctx, table)
	if err != nil {
		return validation.Table{}, err
	}
	primary, err := r.GetPrimaryKey(ctx, table)
	if err != nil {
		return validation.Table{}, err
	}
	out := validation.Table{PrimaryKey: primary}
	for _, col := range raw {
		c := validation.Column{
			Name: col.Name, DatabaseType: oscarMigrationColumnType(col), Precision: col.Size,
			Scale: max(col.Scale, 0), Length: col.Size, TimePrecision: max(col.Scale, 0),
		}
		validation.ClassifyColumn("oscar", &c)
		if c.Kind == validation.Bits && (!strings.EqualFold(col.Type, "BIT") || col.Size != 1) {
			c.Kind = validation.Unsupported
		}
		out.Columns = append(out.Columns, c)
	}
	return out, nil
}

func (r *OscarReader) OpenValidationScan(ctx context.Context, table string, cols []validation.Column) (validation.RowStream, error) {
	return r.openOscarValidation(ctx, table, r.table(table), cols)
}

func (r *OscarReader) OpenPartitionValidationScan(ctx context.Context, table string, location validation.PartitionLocation, cols []validation.Column) (validation.RowStream, error) {
	spec, err := r.PartitionMetadata(ctx, table)
	if err != nil {
		return nil, err
	}
	if spec == nil || spec.Unsupported != "" || location.Native || location.Name == "" {
		return nil, fmt.Errorf("神通源表 %s 缺少可核验的原生叶分区身份", table)
	}
	var from string
	err = oscarVisitLeaves(spec, false, func(p Partition, nested bool) error {
		if p.Name == location.Name && nested == location.Subpartition &&
			(location.Schema == "" || p.Schema == location.Schema) && (location.Ordinal == 0 || p.Ordinal == location.Ordinal) {
			from = r.partitionFrom(table, p, nested)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if from == "" {
		return nil, fmt.Errorf("神通源表 %s 的原生叶分区 %s 发生变化", table, location.Name)
	}
	return r.openOscarValidation(ctx, table, from, cols)
}

func (r *OscarReader) openOscarValidation(ctx context.Context, table, from string, cols []validation.Column) (validation.RowStream, error) {
	actual, err := r.DescribeValidationTable(ctx, table)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("神通表 %s 缺少内容校验列", table)
	}
	catalog := make(map[string]validation.Column, len(actual.Columns))
	for _, c := range actual.Columns {
		catalog[c.Name] = c
	}
	names := make([]string, 0, len(cols))
	bits := make([]int, 0)
	for i, c := range cols {
		if !reflect.DeepEqual(catalog[c.Name], c) {
			return nil, fmt.Errorf("神通表 %s 的列 %s 元数据发生变化", table, c.Name)
		}
		if c.Kind == validation.Unsupported {
			return nil, fmt.Errorf("神通列 %s.%s 类型 %s 缺少无损校验读取能力", table, c.Name, c.DatabaseType)
		}
		if c.Kind == validation.Bits {
			bits = append(bits, i)
		}
		names = append(names, c.Name)
	}
	id, err := r.bridge.OpenCursor(ctx, r.session, "SELECT "+oscarProjection(names)+" FROM "+from)
	if err != nil {
		return nil, err
	}
	return &oscarValidationCursor{reader: r, id: id, width: len(cols), bits: bits}, nil
}

type oscarValidationCursor struct {
	reader *OscarReader
	id     uint64
	width  int
	bits   []int
	rows   [][]any
	done   bool
}

func (s *oscarValidationCursor) Next(ctx context.Context) ([]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.id == 0 {
		return nil, io.EOF
	}
	if len(s.rows) == 0 {
		if s.done {
			return nil, io.EOF
		}
		var err error
		s.rows, s.done, err = s.reader.bridge.FetchCursor(ctx, s.id, oscarCursorRows, oscarCursorBytes)
		if err != nil {
			return nil, err
		}
		if len(s.rows) == 0 {
			if s.done {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("神通内容校验游标未返回数据或完成状态")
		}
	}
	row := s.rows[0]
	s.rows[0] = nil
	s.rows = s.rows[1:]
	if len(row) != s.width {
		return nil, fmt.Errorf("神通内容校验列数与冻结投影不一致")
	}
	for _, i := range s.bits {
		if row[i] == nil {
			continue
		}
		b, err := oscarBoolean(row[i])
		if err != nil {
			return nil, err
		}
		if b {
			row[i] = "1"
		} else {
			row[i] = "0"
		}
	}
	return row, nil
}

func (s *oscarValidationCursor) Close() error {
	if s.id == 0 {
		return nil
	}
	id := s.id
	s.id, s.rows = 0, nil
	return s.reader.closeCursor(id)
}

var _ validation.Scanner = (*OscarReader)(nil)
var _ validation.PartitionScanner = (*OscarReader)(nil)
