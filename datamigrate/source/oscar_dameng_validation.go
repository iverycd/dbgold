package source

import (
	"context"
	"fmt"

	"dbgold/datamigrate/validation"
	"dbgold/internal/jdbcbridge"
)

func oscarDamengValidationColumn(col jdbcbridge.MigrationColumn) validation.Column {
	typ := oscarDamengType(col)
	c := validation.Column{
		Name: col.Name, DatabaseType: typ, Precision: col.Size, Length: col.Size,
		Scale: max(col.Scale, 0), TimePrecision: max(col.Scale, 0),
	}
	validation.ClassifyColumn("oscar", &c)
	switch typ {
	case "BIT", "VARBIT":
		c.Kind = validation.Binary
	case "NAME", `"CHAR"`, "ARRAY":
		c.Kind = validation.Text
	case "SERIAL", "BIGSERIAL":
		c.Kind = validation.Number
	case "XMLTYPE", "TIME WITH TIME ZONE", "INTERVAL YEAR TO MONTH", "INTERVAL DAY TO SECOND":
		// The ordinary Dameng SQL scanner has no matching canonical value
		// for these storage types. Report an explicit unsupported column.
		c.Kind = validation.Unsupported
	}
	return c
}

func (r *OscarDamengReader) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	raw, err := r.columns(ctx, table)
	if err != nil {
		return validation.Table{}, err
	}
	primary, err := r.GetPrimaryKey(ctx, table)
	if err != nil {
		return validation.Table{}, err
	}
	out := validation.Table{PrimaryKey: primary, Columns: make([]validation.Column, len(raw))}
	for i, col := range raw {
		out.Columns[i] = oscarDamengValidationColumn(col)
	}
	return out, nil
}

func (r *OscarDamengReader) OpenValidationScan(ctx context.Context, table string, columns []validation.Column) (validation.RowStream, error) {
	return r.openDamengValidation(ctx, table, r.table(table), columns)
}

func (r *OscarDamengReader) OpenPartitionValidationScan(ctx context.Context, table string, location validation.PartitionLocation, columns []validation.Column) (validation.RowStream, error) {
	from, err := r.oscarValidationPartition(ctx, table, location)
	if err != nil {
		return nil, err
	}
	return r.openDamengValidation(ctx, table, from, columns)
}

func (r *OscarDamengReader) openDamengValidation(ctx context.Context, table, from string, columns []validation.Column) (validation.RowStream, error) {
	if len(columns) == 0 {
		return nil, fmt.Errorf("神通表 %s 缺少内容校验列", table)
	}
	raw, err := r.columns(ctx, table)
	if err != nil {
		return nil, err
	}
	catalog := make(map[string]jdbcbridge.MigrationColumn, len(raw))
	for _, col := range raw {
		catalog[col.Name] = col
	}
	projection := make([]jdbcbridge.MigrationColumn, len(columns))
	var bits []int
	for i, col := range columns {
		original, ok := catalog[col.Name]
		if !ok || oscarDamengValidationColumn(original) != col {
			return nil, fmt.Errorf("神通表 %s 的列 %s 元数据发生变化", table, col.Name)
		}
		if col.Kind == validation.Unsupported {
			return nil, fmt.Errorf("神通列 %s.%s 类型 %s 缺少可用的达梦内容校验规则", table, col.Name, col.DatabaseType)
		}
		if col.DatabaseType == "BIT" || col.DatabaseType == "VARBIT" {
			bits = append(bits, i)
		}
		projection[i] = original
	}
	p := newOscarDamengProjection(projection)
	id, err := r.openDamengCursor(ctx, p, p.query(from))
	if err != nil {
		return nil, err
	}
	return &oscarValidationCursor{
		reader: r.OscarReader, id: id, width: len(columns), fetch: r.bridge.FetchMigrationCursor,
		adjust: func(rows [][]any) error {
			if err := p.checkRows(table, rows); err != nil {
				return err
			}
			for _, row := range rows {
				for _, i := range bits {
					if row[i] == nil {
						continue
					}
					value, err := OscarDamengBitBytes(row[i].(string))
					if err != nil {
						return err
					}
					row[i] = value
				}
			}
			return nil
		},
	}, nil
}

var _ validation.Scanner = (*OscarDamengReader)(nil)
var _ validation.PartitionScanner = (*OscarDamengReader)(nil)
