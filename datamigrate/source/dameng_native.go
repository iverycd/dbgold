package source

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/damengnative"
)

// DaMengNativeReader uses the original DM catalog and values. The embedded
// reader satisfies the historical interface, but native execution uses the
// snapshot and one streaming cursor, never cross-vendor DDL or page cleaners.
type DaMengNativeReader struct{ *DaMengReader }

func NewDaMengNative(dsn, schema string, pool ConnPoolConfig) (*DaMengNativeReader, error) {
	db, err := sql.Open("dm", dsn)
	if err != nil {
		return nil, err
	}
	pool.applyTo(db)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	owner, err := damengnative.ResolveSchema(context.Background(), db, schema)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &DaMengNativeReader{DaMengReader: &DaMengReader{db: db, schema: owner}}, nil
}
func (r *DaMengNativeReader) MigrationSchema() string { return r.schema }
func (r *DaMengNativeReader) SnapshotDamengNative(ctx context.Context) (*damengnative.Snapshot, error) {
	return damengnative.ReadCatalog(ctx, r.db, r.schema, true)
}

func (r *DaMengNativeReader) ReadDamengNativeLinkDDL(ctx context.Context, name string) (string, error) {
	var ddl sql.NullString
	if err := r.db.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('DB_LINK',?,?)`, name, r.schema).Scan(&ddl); err != nil {
		// Some servers include SQL/password fragments in metadata errors.
		return "", fmt.Errorf("重新读取源 DBLink 定义失败")
	}
	if !ddl.Valid || strings.TrimSpace(ddl.String) == "" {
		return "", fmt.Errorf("源 DBLink 原生定义为空")
	}
	return damengnative.StripClientDelimiter(ddl.String), nil
}

func (r *DaMengNativeReader) CountRows(ctx context.Context, table string) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+damengnative.Qualified(r.schema, table)).Scan(&n)
	return n, err
}
func (r *DaMengNativeReader) GetViews(ctx context.Context) ([]ViewInfo, error) {
	return r.GetViewsForTarget(ctx, "dameng")
}
func (r *DaMengNativeReader) GetViewsForTarget(ctx context.Context, _ string) ([]ViewInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT VIEW_NAME,DBMS_METADATA.GET_DDL('VIEW',VIEW_NAME,?) FROM ALL_VIEWS WHERE OWNER=? ORDER BY VIEW_NAME`, r.schema, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ViewInfo
	for rows.Next() {
		var v ViewInfo
		if err = rows.Scan(&v.ViewName, &v.Definition); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *DaMengNativeReader) GetPrimaryKeys(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT c.TABLE_NAME,c.CONSTRAINT_NAME,cc.COLUMN_NAME FROM ALL_CONSTRAINTS c JOIN ALL_CONS_COLUMNS cc ON c.OWNER=cc.OWNER AND c.CONSTRAINT_NAME=cc.CONSTRAINT_NAME WHERE c.OWNER=? AND c.CONSTRAINT_TYPE='P' ORDER BY c.TABLE_NAME,c.CONSTRAINT_NAME,cc.POSITION`, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	positions := map[string]int{}
	var out []IndexInfo
	for rows.Next() {
		var table, name, column string
		if err = rows.Scan(&table, &name, &column); err != nil {
			return nil, err
		}
		key := table + "\x00" + name
		at, ok := positions[key]
		if !ok {
			at = len(out)
			positions[key] = at
			out = append(out, IndexInfo{TableName: table, IndexName: name, IsPrimary: true, IsUnique: true})
		}
		out[at].Columns = append(out[at].Columns, column)
	}
	return out, rows.Err()
}

func (r *DaMengNativeReader) OpenDamengNativeScan(ctx context.Context, t damengnative.Table) (damengnative.Scan, error) {
	if t.Schema != r.schema {
		return nil, fmt.Errorf("源表 schema 与已冻结达梦 schema 不一致")
	}
	name := t.SourceName
	if name == "" {
		name = t.Name
	}
	var projection []string
	for _, c := range t.Columns {
		if !c.Writable {
			continue
		}
		q := damengnative.Quote(c.Name)
		// DECIMAL is converted to exact decimal text by the server. This avoids
		// driver float conversion and preserves negative precision-38 numbers.
		base := strings.ToUpper(strings.TrimSpace(c.DataType))
		if at := strings.IndexByte(base, '('); at >= 0 {
			base = strings.TrimSpace(base[:at])
		}
		if c.TypeSchema == "" {
			switch base {
			case "NUMBER", "NUMERIC", "DEC", "DECIMAL", "FLOAT":
				q = "CAST(" + q + " AS VARCHAR(256))"
			case "ROWID":
				// DM's Go driver returns the physical ROWID payload as int64.
				// Use the documented server encoding, preserving site/partition
				// components rather than rebuilding addresses in application code.
				q = "ROWIDTOCHAR(" + q + ")"
			}
		}
		projection = append(projection, q+" AS "+damengnative.Quote(c.Name))
	}
	if len(projection) == 0 {
		return nil, fmt.Errorf("源表 %s 没有可写入列", name)
	}
	rows, err := r.db.QueryContext(ctx, "SELECT "+strings.Join(projection, ", ")+" FROM "+damengnative.Qualified(r.schema, name))
	if err != nil {
		return nil, err
	}
	return &damengNativeScan{rows: rows, width: len(projection)}, nil
}

type damengNativeScan struct {
	rows   *sql.Rows
	width  int
	values []any
	err    error
}

func (s *damengNativeScan) Next() bool {
	if s.err != nil {
		return false
	}
	if !s.rows.Next() {
		return false
	}
	s.values = make([]any, s.width)
	ptrs := make([]any, s.width)
	for i := range ptrs {
		ptrs[i] = &s.values[i]
	}
	s.err = s.rows.Scan(ptrs...)
	return s.err == nil
}
func (s *damengNativeScan) Values() ([]any, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.values == nil {
		return nil, fmt.Errorf("Values 必须在成功 Next 后调用")
	}
	out := make([]any, len(s.values))
	for i, v := range s.values {
		if b, ok := v.([]byte); ok {
			out[i] = append([]byte(nil), b...)
		} else {
			out[i] = v
		}
	}
	return out, nil
}
func (s *damengNativeScan) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.rows.Err()
}
func (s *damengNativeScan) Close() error { return s.rows.Close() }

// Legacy callers also receive untouched values; in particular text NUL bytes
// and quoted identifiers must never pass through DaMengReader.ReadPage.
func (r *DaMengNativeReader) ReadPage(ctx context.Context, table string, pk []string, offset, limit int64) ([]string, []string, [][]any, error) {
	query := "SELECT * FROM " + damengnative.Qualified(r.schema, table)
	if len(pk) > 0 {
		keys := make([]string, len(pk))
		for i, k := range pk {
			keys[i] = damengnative.Quote(k)
		}
		query += " ORDER BY " + strings.Join(keys, ", ")
	}
	query += fmt.Sprintf(" OFFSET %d ROWS FETCH NEXT %d ROWS ONLY", offset, limit)
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, nil, err
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, nil, nil, err
	}
	names := make([]string, len(types))
	for i, c := range types {
		names[i] = c.DatabaseTypeName()
	}
	var data [][]any
	for rows.Next() {
		v := make([]any, len(cols))
		p := make([]any, len(cols))
		for i := range p {
			p[i] = &v[i]
		}
		if err = rows.Scan(p...); err != nil {
			return nil, nil, nil, err
		}
		for i, value := range v {
			if b, ok := value.([]byte); ok {
				v[i] = append([]byte(nil), b...)
			}
		}
		data = append(data, v)
	}
	return cols, names, data, rows.Err()
}
