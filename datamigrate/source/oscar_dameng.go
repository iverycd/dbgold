package source

import (
	"context"
	"fmt"
	"strings"

	"dbgold/internal/jdbcbridge"
)

// OscarDamengReader keeps the Oscar catalog and connection while selecting
// the projections and identity metadata needed for a Dameng target.
type OscarDamengReader struct {
	*OscarReader
}

func WrapOscarForDameng(r Reader) Reader {
	if oscar, ok := r.(*OscarReader); ok {
		return &OscarDamengReader{OscarReader: oscar}
	}
	return r
}

func oscarDamengType(col jdbcbridge.MigrationColumn) string {
	switch col.SQLType {
	case 2003: // java.sql.Types.ARRAY
		return "ARRAY"
	case 2009: // java.sql.Types.SQLXML
		return "XMLTYPE"
	case 2013: // java.sql.Types.TIME_WITH_TIMEZONE
		return "TIME WITH TIME ZONE"
	case 2014: // java.sql.Types.TIMESTAMP_WITH_TIMEZONE
		return "TIMESTAMP WITH TIME ZONE"
	}
	return OscarDamengColumnType(oscarMigrationColumnType(col))
}

func (r *OscarDamengReader) GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error) {
	raw, err := r.columns(ctx, table)
	if err != nil {
		return nil, err
	}
	info := &TableDDLInfo{TableName: table, Columns: make([]ColumnInfo, 0, len(raw))}
	for _, c := range raw {
		col := oscarColumnInfo(c)
		col.DataType = oscarDamengType(c)
		identity, err := r.damengIdentity(ctx, table, c)
		if err != nil {
			return info, err
		}
		if identity != nil {
			col.Extra, col.Default, col.IdentityInfo = "auto_increment", nil, identity
		}
		info.Columns = append(info.Columns, col)
	}
	return info, nil
}

type oscarDamengProjection struct {
	names, types, expressions []string
	columns                   []jdbcbridge.MigrationColumn
	textColumns               []int
}

func newOscarDamengProjection(cols []jdbcbridge.MigrationColumn) oscarDamengProjection {
	p := oscarDamengProjection{
		names: make([]string, len(cols)), types: make([]string, len(cols)),
		expressions: make([]string, len(cols)), columns: cols,
	}
	for i, col := range cols {
		typ := oscarDamengType(col)
		expr := oscarQuoteIdent(col.Name)
		switch typ {
		case "BIT", "VARBIT", "ARRAY":
			// Use the server's textual form for bit width and array bounds,
			// dimensions, NULL elements, and escaping. JDBC BIT can be read
			// as a single boolean, and getArray can discard the lower bound.
			expr = "CAST(" + expr + " AS TEXT)"
			p.textColumns = append(p.textColumns, i)
		case `"CHAR"`, "NAME", "XMLTYPE",
			"TIME WITH TIME ZONE", "TIMESTAMP WITH TIME ZONE",
			"INTERVAL YEAR TO MONTH", "INTERVAL DAY TO SECOND":
			p.textColumns = append(p.textColumns, i)
		}
		if OscarDamengUnboundedVarchar(typ, col.Size) {
			// Its target is TEXT. Bind empty and large values as CLOBs, while
			// preserving the VARCHAR declaration in the source table metadata.
			typ = "TEXT"
		}
		p.names[i], p.types[i], p.expressions[i] = col.Name, typ, expr
	}
	return p
}

func (p oscarDamengProjection) query(from string) string {
	return "SELECT " + strings.Join(p.expressions, ",") + " FROM " + from
}

func (r *OscarDamengReader) openDamengCursor(ctx context.Context, p oscarDamengProjection, query string) (uint64, error) {
	if len(p.textColumns) == 0 {
		return r.bridge.OpenCursor(ctx, r.session, query)
	}
	bridge, ok := r.bridge.(interface {
		OpenOscarDamengCursor(context.Context, uint64, string, []int) (uint64, error)
	})
	if !ok {
		return 0, fmt.Errorf("神通 JDBC Bridge 缺少达梦迁移所需的文本游标")
	}
	return bridge.OpenOscarDamengCursor(ctx, r.session, query, p.textColumns)
}

func (p oscarDamengProjection) checkRows(table string, rows [][]any) error {
	for _, row := range rows {
		if len(row) != len(p.names) {
			return fmt.Errorf("神通表 %s 返回列数与固定投影不一致", table)
		}
		for _, i := range p.textColumns {
			if row[i] == nil {
				continue
			}
			value, ok := row[i].(string)
			if !ok {
				return fmt.Errorf("神通表 %s 列 %s 的 %s 未以原始文本返回", table, p.names[i], p.types[i])
			}
			if p.types[i] != "BIT" && p.types[i] != "VARBIT" {
				continue
			}
			size := p.columns[i].Size
			if strings.Trim(value, "01") != "" || p.types[i] == "BIT" && int64(len(value)) != max(1, size) ||
				p.types[i] == "VARBIT" && size > 0 && int64(len(value)) > size {
				return fmt.Errorf("神通表 %s 列 %s 的 %s 值不是完整位串", table, p.names[i], p.types[i])
			}
		}
	}
	return nil
}

func (r *OscarDamengReader) scanDamengRows(ctx context.Context, table string, p oscarDamengProjection, query string, batch int, consume func([][]any) error) error {
	id, err := r.openDamengCursor(ctx, p, query)
	if err != nil {
		return fmt.Errorf("打开神通源表 %s 的数据游标: %w", table, err)
	}
	return r.scanOpenedCursor(ctx, id, batch, r.bridge.FetchMigrationCursor, func(rows [][]any) error {
		if err := p.checkRows(table, rows); err != nil {
			return err
		}
		return consume(rows)
	})
}

// StreamOscarDamengTable reads every source row through one cursor, including
// tables with duplicate rows and tables lacking a stable key.
func (r *OscarDamengReader) StreamOscarDamengTable(ctx context.Context, table string, batch int, consume func([]string, []string, [][]any) error) error {
	if batch <= 0 || consume == nil {
		return fmt.Errorf("神通流式读取需要正数批次大小和回调")
	}
	raw, err := r.columns(ctx, table)
	if err != nil {
		return err
	}
	p := newOscarDamengProjection(raw)
	return r.scanDamengRows(ctx, table, p, p.query(r.table(table)), min(batch, oscarCursorRows), func(rows [][]any) error {
		return consume(p.names, p.types, rows)
	})
}

func (r *OscarDamengReader) ReadPage(ctx context.Context, table string, pkCols []string, offset, limit int64) ([]string, []string, [][]any, error) {
	if offset < 0 || limit <= 0 {
		return nil, nil, nil, fmt.Errorf("神通数据分页边界无效")
	}
	raw, err := r.columns(ctx, table)
	if err != nil {
		return nil, nil, nil, err
	}
	p := newOscarDamengProjection(raw)
	query := p.query(r.table(table))
	if len(pkCols) > 0 {
		query += " ORDER BY " + oscarProjection(pkCols)
	}
	query += fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)
	var rows [][]any
	err = r.scanDamengRows(ctx, table, p, query, oscarCursorRows, func(chunk [][]any) error {
		rows = append(rows, chunk...)
		return nil
	})
	return p.names, p.types, rows, err
}

func (r *OscarDamengReader) StreamPartitionRows(ctx context.Context, table string, batch int, consume func([]string, []string, [][]any) error) error {
	return r.StreamOscarDamengTable(ctx, table, batch, consume)
}

var _ Reader = (*OscarDamengReader)(nil)
var _ PartitionReader = (*OscarDamengReader)(nil)
