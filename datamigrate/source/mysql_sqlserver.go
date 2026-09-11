package source

import (
	"context"
	"strconv"
	"strings"
)

// MySQLSQLServerReader shares the lossless MySQL metadata/byte reader, never
// the historical PostgreSQL view and value rewriting path.
type MySQLSQLServerReader struct{ *MySQLReader }

func NewMySQLForSQLServer(dsn, database string, pool ConnPoolConfig) (*MySQLSQLServerReader, error) {
	r, err := NewMySQLForOracle(dsn, database, pool)
	if err != nil {
		return nil, err
	}
	return &MySQLSQLServerReader{r}, nil
}
func (r *MySQLSQLServerReader) GetViews(ctx context.Context) ([]ViewInfo, error) {
	return r.MySQLReader.GetViewsForTarget(ctx, "oracle")
}
func (r *MySQLSQLServerReader) GetViewsForTarget(ctx context.Context, _ string) ([]ViewInfo, error) {
	return r.GetViews(ctx)
}
func (r *MySQLSQLServerReader) GetIndexes(ctx context.Context) ([]IndexInfo, error) {
	out, err := r.oracleIndexes(ctx)
	for i := range out {
		out[i].Unsupported = strings.ReplaceAll(out[i].Unsupported, "Oracle", "SQL Server 2008 R2")
	}
	return out, err
}
func (r *MySQLSQLServerReader) GetForeignKeys(ctx context.Context) ([]FKInfo, error) {
	out, err := r.oracleForeignKeys(ctx)
	for i := range out {
		out[i].Unsupported = strings.ReplaceAll(out[i].Unsupported, "Oracle", "SQL Server 2008 R2")
	}
	return out, err
}

func (r *MySQLSQLServerReader) GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error) {
	info, e := r.oracleTableInfo(ctx, table)
	if e != nil {
		return nil, e
	}
	for i := range info.Columns {
		c := &info.Columns[i]
		if strings.EqualFold(c.DataType, "bit") {
			s := strings.ToLower(c.FormattedType)
			if strings.HasPrefix(s, "bit(") {
				end := strings.IndexByte(s, ')')
				if end > 4 {
					n, err := strconv.ParseInt(s[4:end], 10, 64)
					if err == nil {
						c.Precision = n
					}
				}
			}
		}
	}
	return info, nil
}

// MySQL decimal division/AVG precision is session-configurable. Carry it into
// view generation rather than silently assuming the default of four digits.
func (r *MySQLSQLServerReader) SQLServerViewPrecision(ctx context.Context) (int, error) {
	var n int
	err := r.queryRowContext(ctx, "SELECT @@SESSION.div_precision_increment").Scan(&n)
	return n, err
}
